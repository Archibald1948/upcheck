package checker

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Archibald1948/upcheck/internal/config"
)

// httpMonitor 는 http 타입 모니터를 만든다.
func httpMonitor(name, target string) config.Monitor {
	return config.Monitor{
		Name: name, Type: config.TypeHTTP, Target: target,
		TimeoutMS: 2000, ExpectedStatus: 200,
	}
}

// ─────────────── 디스패치 ───────────────

func TestCheckerSupportsAllTypes(t *testing.T) {
	c := New()
	defer c.Close()

	want := []string{config.TypeHTTP, config.TypeTCP, config.TypeTLS, config.TypeDNS}
	got := c.Types()
	if len(got) != len(want) {
		t.Fatalf("지원 타입 %v, 기대값 %v", got, want)
	}
	for _, w := range want {
		if !contains(got, w) {
			t.Errorf("%q 타입이 등록되지 않았다", w)
		}
	}
}

func TestCheckerRejectsUnknownType(t *testing.T) {
	c := New()
	defer c.Close()

	res := c.Check(context.Background(), config.Monitor{
		Name: "x", Type: "gopher", Target: "x", TimeoutMS: 1000,
	})
	if res.OK {
		t.Fatal("모르는 타입인데 OK 로 판정됐다")
	}
	if res.Err == nil {
		t.Fatal("Err 가 nil 이다")
	}
}

// TestCheckFillsCommonFields 는 Checker 가 공통 필드를 채우는지 본다.
func TestCheckFillsCommonFields(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	c := New()
	defer c.Close()

	res := c.Check(context.Background(), httpMonitor("공통필드", srv.URL))

	if res.Monitor != "공통필드" {
		t.Errorf("Monitor = %q", res.Monitor)
	}
	if res.Type != config.TypeHTTP {
		t.Errorf("Type = %q", res.Type)
	}
	if _, offset := res.CheckedAt.Zone(); offset != 0 {
		t.Errorf("CheckedAt 이 UTC 가 아니다: %v", res.CheckedAt)
	}
}

// ─────────────── http ───────────────

func TestHTTPOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("hello upcheck"))
	}))
	defer srv.Close()

	c := New()
	defer c.Close()

	res := c.Check(context.Background(), httpMonitor("ok", srv.URL))
	if !res.OK {
		t.Fatalf("OK 를 기대했는데 실패: %v", res.Err)
	}
	if res.StatusCode != 200 {
		t.Errorf("StatusCode = %d, 기대값 200", res.StatusCode)
	}
	if res.Latency <= 0 {
		t.Error("Latency 가 측정되지 않았다")
	}
}

func TestHTTPUnexpectedStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	c := New()
	defer c.Close()

	res := c.Check(context.Background(), httpMonitor("503", srv.URL))
	if res.OK {
		t.Fatal("503 인데 OK 로 판정됐다")
	}
	if res.StatusCode != 503 {
		t.Errorf("StatusCode = %d, 기대값 503", res.StatusCode)
	}
	if res.Err == nil {
		t.Error("Err 가 설정되지 않았다")
	}
}

func TestHTTPKeyword(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("<html><body>서비스 정상</body></html>"))
	}))
	defer srv.Close()

	c := New()
	defer c.Close()

	t.Run("키워드 있음", func(t *testing.T) {
		m := httpMonitor("found", srv.URL)
		m.Keyword = "서비스 정상"
		if res := c.Check(context.Background(), m); !res.OK {
			t.Errorf("키워드가 있는데 실패: %v", res.Err)
		}
	})

	t.Run("키워드 없음", func(t *testing.T) {
		m := httpMonitor("missing", srv.URL)
		m.Keyword = "이런문구는없다"
		if res := c.Check(context.Background(), m); res.OK {
			t.Error("키워드가 없는데 OK 로 판정됐다")
		}
	})
}

// TestHTTPTimeout 은 서버가 느릴 때 context 타임아웃이 도는지 확인한다.
func TestHTTPTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(2 * time.Second):
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()

	c := New()
	defer c.Close()

	m := httpMonitor("slow", srv.URL)
	m.TimeoutMS = 200

	start := time.Now()
	res := c.Check(context.Background(), m)
	elapsed := time.Since(start)

	if res.OK {
		t.Fatal("타임아웃이 나야 하는데 OK 로 판정됐다")
	}
	if elapsed > time.Second {
		t.Errorf("타임아웃이 동작하지 않았다: %v 소요", elapsed)
	}
	if res.Err == nil || res.Err.Error() != "타임아웃" {
		t.Errorf("Err = %v, 기대값 '타임아웃'", res.Err)
	}
}

// TestHTTPCancelledContext 는 부모 context 취소(= Ctrl+C)가
// 진행 중인 요청을 즉시 끊는지 확인한다.
func TestHTTPCancelledContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(3 * time.Second):
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()

	c := New()
	defer c.Close()

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)

	m := httpMonitor("cancel", srv.URL)
	m.TimeoutMS = 5000

	start := time.Now()
	res := c.Check(ctx, m)
	elapsed := time.Since(start)

	if res.OK {
		t.Fatal("취소됐는데 OK 로 판정됐다")
	}
	if elapsed > time.Second {
		t.Errorf("취소가 전파되지 않았다: %v 소요", elapsed)
	}
	// 타임아웃(5초)이 아니라 '취소'로 분류돼야 한다
	if res.Err == nil || res.Err.Error() != "취소됨(종료 중)" {
		t.Errorf("Err = %v, 기대값 '취소됨(종료 중)'", res.Err)
	}
}

func TestHTTPBadURL(t *testing.T) {
	c := New()
	defer c.Close()

	res := c.Check(context.Background(), httpMonitor("bad", "://잘못된주소"))
	if res.OK {
		t.Fatal("잘못된 URL 인데 OK 로 판정됐다")
	}
	if res.Err == nil {
		t.Fatal("Err 가 nil 이다")
	}
}

// TestHTTPReusesClient 는 클라이언트 재사용과 Client.Timeout 미설정을 고정한다.
func TestHTTPReusesClient(t *testing.T) {
	p := newHTTPProber()
	if p.client == nil {
		t.Fatal("client 가 nil 이다")
	}
	if p.client.Timeout != 0 {
		t.Errorf("Client.Timeout = %v, 0 이어야 한다 (context 로 제어)", p.client.Timeout)
	}
	c := New()
	defer c.Close()
	first := c.probers[config.TypeHTTP]
	second := c.probers[config.TypeHTTP]
	if first != second {
		t.Error("Prober 가 매번 새로 만들어진다")
	}
}

// ─────────────── 공통 ───────────────

func TestClassifyErr(t *testing.T) {
	orig := errors.New("원본 에러")

	t.Run("살아있는 context", func(t *testing.T) {
		if got := classifyErr(context.Background(), orig); got != orig {
			t.Errorf("원본을 그대로 돌려줘야 한다: %v", got)
		}
	})

	t.Run("취소됨", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if got := classifyErr(ctx, orig); got.Error() != "취소됨(종료 중)" {
			t.Errorf("got %v", got)
		}
	})

	t.Run("시한 초과", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
		defer cancel()
		<-ctx.Done()
		if got := classifyErr(ctx, orig); got.Error() != "타임아웃" {
			t.Errorf("got %v", got)
		}
	})
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
