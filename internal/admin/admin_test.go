package admin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Archibald1948/upcheck/internal/api"
	"github.com/Archibald1948/upcheck/internal/checker"
	"github.com/Archibald1948/upcheck/internal/metrics"
)

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// fakePinger 는 준비 상태 확인 결과를 마음대로 정한다.
type fakePinger struct {
	err   error
	delay time.Duration
}

func (f fakePinger) Ping(ctx context.Context) error {
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return f.err
}

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

// TestHealthzIgnoresStore 는 liveness 가 저장소 상태와 무관한지 본다.
//
// liveness 실패는 컨테이너 재시작을 부른다. DB 가 잠깐 느리다고
// 재시작하면 상황만 나빠진다.
func TestHealthzIgnoresStore(t *testing.T) {
	s := New(quietLogger(), nil, fakePinger{err: errors.New("DB 죽음")})
	rec := get(t, s.Handler(), "/healthz")

	if rec.Code != http.StatusOK {
		t.Errorf("상태 코드 %d — DB 가 죽어도 liveness 는 200 이어야 한다", rec.Code)
	}
	var h health
	if err := json.Unmarshal(rec.Body.Bytes(), &h); err != nil {
		t.Fatal(err)
	}
	if h.Status != "ok" || h.Goroutines <= 0 {
		t.Errorf("응답이 이상하다: %+v", h)
	}
}

// TestReadyzChecksStore 는 readiness 가 저장소를 확인하는지 본다.
func TestReadyzChecksStore(t *testing.T) {
	t.Run("정상", func(t *testing.T) {
		s := New(quietLogger(), nil, fakePinger{})
		if rec := get(t, s.Handler(), "/readyz"); rec.Code != http.StatusOK {
			t.Errorf("상태 코드 %d, 200 이어야 한다", rec.Code)
		}
	})

	t.Run("저장소 실패", func(t *testing.T) {
		s := New(quietLogger(), nil, fakePinger{err: errors.New("no such table: monitors")})
		rec := get(t, s.Handler(), "/readyz")

		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("상태 코드 %d, 503 이어야 한다", rec.Code)
		}
		// 원인 문자열이 응답에 새면 안 된다. 테이블 이름은 내부 정보다.
		if strings.Contains(rec.Body.String(), "no such table") {
			t.Errorf("에러 원문이 응답에 새어 나갔다: %s", rec.Body.String())
		}
	})

	t.Run("저장소가 느림", func(t *testing.T) {
		// 확인이 길어지면 헬스체크 자체가 부하가 된다. 2초 시한이 걸려야 한다.
		s := New(quietLogger(), nil, fakePinger{delay: 5 * time.Second})
		start := time.Now()
		rec := get(t, s.Handler(), "/readyz")
		elapsed := time.Since(start)

		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("상태 코드 %d, 503 이어야 한다", rec.Code)
		}
		if elapsed > 3*time.Second {
			t.Errorf("확인에 %v 걸렸다 — 2초 시한이 안 걸렸다", elapsed)
		}
	})
}

func TestMetricsEndpoint(t *testing.T) {
	m := metrics.New()
	m.ObserveCheck(checker.Result{Monitor: "웹", Type: "http", OK: true, Latency: 120 * time.Millisecond})
	m.ObserveCheck(checker.Result{Monitor: "웹", Type: "http", OK: false, Latency: 5 * time.Second})
	days := 12
	m.ObserveCheck(checker.Result{Monitor: "인증서", Type: "tls", OK: true, Latency: 30 * time.Millisecond, CertDaysLeft: &days})

	s := New(quietLogger(), m.Registry(), nil)
	rec := get(t, s.Handler(), "/metrics")
	if rec.Code != http.StatusOK {
		t.Fatalf("상태 코드 %d", rec.Code)
	}
	body := rec.Body.String()

	want := []string{
		`upcheck_checks_total{monitor="웹",result="ok",type="http"} 1`,
		`upcheck_checks_total{monitor="웹",result="fail",type="http"} 1`,
		`upcheck_monitor_up{monitor="웹",type="http"} 0`,
		`upcheck_tls_cert_days_left{monitor="인증서"} 12`,
		// Go 런타임 지표가 공짜로 따라온다 — goroutine 누수 감시에 쓴다
		"go_goroutines",
	}
	for _, w := range want {
		if !strings.Contains(body, w) {
			t.Errorf("지표에 %q 가 없다", w)
		}
	}

	// 실패한 체크의 응답시간은 히스토그램에 넣지 않는다 (분위수 왜곡 방지).
	// 성공 1회만 세어져야 한다.
	if !strings.Contains(body, `upcheck_check_duration_seconds_count{monitor="웹",type="http"} 1`) {
		t.Error("응답시간 히스토그램이 성공 1회만 세지 않았다")
	}
}

// TestMetricsRegistryIsNotGlobal 은 New() 를 여러 번 불러도 안전한지 본다.
// 전역 레지스트리를 썼다면 두 번째 등록에서 panic 한다.
func TestMetricsRegistryIsNotGlobal(t *testing.T) {
	for range 3 {
		if m := metrics.New(); m.Registry() == nil {
			t.Fatal("레지스트리가 nil 이다")
		}
	}
}

func TestPprofOnOurMux(t *testing.T) {
	s := New(quietLogger(), nil, nil)
	h := s.Handler()

	for _, path := range []string{"/debug/pprof/", "/debug/pprof/goroutine?debug=1", "/debug/pprof/heap?debug=1"} {
		if rec := get(t, h, path); rec.Code != http.StatusOK {
			t.Errorf("%s → %d", path, rec.Code)
		}
	}

	// Go 1.27 에 새로 들어온 goroutineleak 프로파일 (스펙 6절 완료 기준)
	rec := get(t, h, "/debug/pprof/goroutineleak?debug=1")
	if rec.Code != http.StatusOK {
		t.Fatalf("goroutineleak → %d\n%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "goroutineleak profile") {
		t.Errorf("goroutineleak 프로파일 형식이 아니다: %.200s", rec.Body.String())
	}
}

// TestPprofNotExposedByPublicAPI 는 프로파일이 공개 API 에 새지 않는지 본다.
//
// net/http/pprof 는 import 되는 것만으로 init() 이 DefaultServeMux 에
// 핸들러를 등록한다. 그건 막을 수 없다 — 아래에서 실제로 200 이 나오는 걸 확인한다.
// 그러니 중요한 건 "우리 서버가 DefaultServeMux 를 쓰지 않는 것"이다.
func TestPprofNotExposedByPublicAPI(t *testing.T) {
	// 전역 mux 에는 실제로 붙어 있다 (net/http/pprof 의 init 때문)
	rec := httptest.NewRecorder()
	http.DefaultServeMux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/debug/pprof/", nil))
	if rec.Code != http.StatusOK {
		t.Logf("DefaultServeMux 응답 %d — net/http/pprof 의 init 동작이 바뀌었을 수 있다", rec.Code)
	}

	// 공개 API 는 자기 mux 만 서비스하므로 프로파일이 없어야 한다
	pub := api.New(nil, quietLogger(), api.Options{}).Handler()
	for _, path := range []string{"/debug/pprof/", "/debug/pprof/heap", "/metrics"} {
		rec := get(t, pub, path)
		if rec.Code != http.StatusNotFound {
			t.Errorf("공개 API 가 %s 를 노출한다 (%d)", path, rec.Code)
		}
	}
}

func TestVersionIsReported(t *testing.T) {
	s := New(quietLogger(), nil, nil)
	if s.version == "" {
		t.Error("버전이 비어 있다")
	}
	t.Logf("버전: %s", s.version)
}
