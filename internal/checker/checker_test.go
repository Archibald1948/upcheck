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

// httptest.NewServer 는 진짜 포트를 열어 로컬 HTTP 서버를 띄운다.
// 외부 네트워크에 의존하지 않으므로 테스트가 빠르고 안정적이다.

func TestCheckOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("hello upcheck"))
	}))
	defer srv.Close() // 테스트 끝나면 서버를 닫는다

	m := config.Monitor{
		Name: "ok", Target: srv.URL,
		TimeoutMS: 2000, ExpectedStatus: 200,
	}

	res := New().Check(context.Background(), m)
	if !res.OK {
		t.Fatalf("OK 를 기대했는데 실패: %v", res.Err)
	}
	if res.StatusCode != 200 {
		t.Errorf("StatusCode = %d, 기대값 200", res.StatusCode)
	}
	if res.Latency <= 0 {
		t.Error("Latency 가 측정되지 않았다")
	}
	// 시각은 UTC 로 저장돼야 한다 (스펙 7절 함정)
	if _, offset := res.CheckedAt.Zone(); offset != 0 {
		t.Errorf("CheckedAt 이 UTC 가 아니다: %v", res.CheckedAt)
	}
}

func TestCheckUnexpectedStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	res := New().Check(context.Background(), config.Monitor{
		Name: "503", Target: srv.URL, TimeoutMS: 2000, ExpectedStatus: 200,
	})

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

func TestCheckKeyword(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("<html><body>서비스 정상</body></html>"))
	}))
	defer srv.Close()

	c := New()
	base := config.Monitor{Target: srv.URL, TimeoutMS: 2000, ExpectedStatus: 200}

	t.Run("키워드 있음", func(t *testing.T) {
		m := base
		m.Name, m.Keyword = "found", "서비스 정상"
		if res := c.Check(context.Background(), m); !res.OK {
			t.Errorf("키워드가 있는데 실패: %v", res.Err)
		}
	})

	t.Run("키워드 없음", func(t *testing.T) {
		m := base
		m.Name, m.Keyword = "missing", "이런문구는없다"
		if res := c.Check(context.Background(), m); res.OK {
			t.Error("키워드가 없는데 OK 로 판정됐다")
		}
	})
}

// TestCheckTimeout 은 서버가 느릴 때 context 타임아웃이 도는지 확인한다.
func TestCheckTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(2 * time.Second):
		case <-r.Context().Done(): // 클라이언트가 끊으면 즉시 반환
		}
	}))
	defer srv.Close()

	start := time.Now()
	res := New().Check(context.Background(), config.Monitor{
		Name: "slow", Target: srv.URL, TimeoutMS: 200, ExpectedStatus: 200,
	})
	elapsed := time.Since(start)

	if res.OK {
		t.Fatal("타임아웃이 나야 하는데 OK 로 판정됐다")
	}
	// 200ms 타임아웃인데 2초를 기다렸다면 타임아웃이 안 걸린 것이다
	if elapsed > time.Second {
		t.Errorf("타임아웃이 동작하지 않았다: %v 소요", elapsed)
	}
	if res.Err == nil || res.Err.Error() != "타임아웃" {
		t.Errorf("Err = %v, 기대값 '타임아웃'", res.Err)
	}
}

// TestCheckCancelledContext 는 부모 context 취소(= Ctrl+C)가
// 진행 중인 요청을 즉시 끊는지 확인한다. M6의 graceful shutdown 기반이다.
func TestCheckCancelledContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(3 * time.Second):
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())

	// 50ms 뒤에 취소한다. time.AfterFunc 는 별도 goroutine 에서 함수를 부른다.
	time.AfterFunc(50*time.Millisecond, cancel)

	start := time.Now()
	res := New().Check(ctx, config.Monitor{
		Name: "cancel", Target: srv.URL, TimeoutMS: 5000, ExpectedStatus: 200,
	})
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

func TestCheckBadURL(t *testing.T) {
	res := New().Check(context.Background(), config.Monitor{
		Name: "bad", Target: "://잘못된주소", TimeoutMS: 1000, ExpectedStatus: 200,
	})
	if res.OK {
		t.Fatal("잘못된 URL 인데 OK 로 판정됐다")
	}
	if res.Err == nil {
		t.Fatal("Err 가 nil 이다")
	}
}

// TestCheckerReusesClient 는 Checker 가 http.Client 를 재사용하는지 확인한다.
// (스펙 7절 함정 1번 — 매 체크마다 클라이언트를 새로 만들면 안 된다)
func TestCheckerReusesClient(t *testing.T) {
	c := New()
	if c.client == nil {
		t.Fatal("client 가 nil 이다")
	}
	// Client.Timeout 은 비어 있어야 한다. context 로 타임아웃을 걸기 때문이다.
	// (스펙 7절 함정 3번)
	if c.client.Timeout != 0 {
		t.Errorf("Client.Timeout = %v, 0 이어야 한다 (context 로 제어)", c.client.Timeout)
	}
}

// TestClassifyErr 은 에러 분류 로직만 따로 본다.
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
		<-ctx.Done() // 시한이 지날 때까지 기다린다
		if got := classifyErr(ctx, orig); got.Error() != "타임아웃" {
			t.Errorf("got %v", got)
		}
	})
}
