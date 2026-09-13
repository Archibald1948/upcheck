package collector

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/Archibald1948/upcheck/internal/checker"
	"github.com/Archibald1948/upcheck/internal/config"
	"github.com/Archibald1948/upcheck/internal/store"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func setup(t *testing.T, names ...string) (*store.Store, map[string]int64) {
	t.Helper()
	ctx := context.Background()

	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "test.db"), quietLogger())
	if err != nil {
		t.Fatalf("store.Open 실패: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	ms := make([]config.Monitor, len(names))
	for i, n := range names {
		ms[i] = config.Monitor{
			Name: n, Type: "http", Target: "https://" + n + ".example",
			IntervalSec: 60, TimeoutMS: 5000, ExpectedStatus: 200,
		}
	}
	ids, err := s.SyncMonitors(ctx, ms)
	if err != nil {
		t.Fatalf("SyncMonitors 실패: %v", err)
	}
	return s, ids
}

func result(name string, ok bool, latency time.Duration) checker.Result {
	r := checker.Result{
		Monitor: name, CheckedAt: time.Now().UTC(), OK: ok, Latency: latency,
	}
	if ok {
		r.StatusCode = 200
	} else {
		r.StatusCode = 503
		r.Err = errors.New("상태 코드 503")
	}
	return r
}

// TestCollectorWritesAll 은 들어온 결과가 빠짐없이 저장되는지 본다.
func TestCollectorWritesAll(t *testing.T) {
	s, ids := setup(t, "a", "b")
	ctx := context.Background()

	results := make(chan checker.Result, 10)
	for i := range 10 {
		name := "a"
		if i%2 == 1 {
			name = "b"
		}
		results <- result(name, i%3 != 0, time.Duration(i+1)*10*time.Millisecond)
	}
	close(results)

	c := New(s, ids, quietLogger(), WithBatchSize(3))
	if err := c.Run(ctx, results, nil); err != nil {
		t.Fatalf("Run 실패: %v", err)
	}

	n, err := s.CountChecks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 10 {
		t.Errorf("저장된 행 %d개, 기대값 10개", n)
	}
	if st := c.Stats(); st.Written != 10 {
		t.Errorf("Written = %d, 기대값 10", st.Written)
	}
}

// TestCollectorFlushesOnCancelledContext 는 M2에서 데이터를 잃기
// 제일 쉬운 지점을 막았는지 본다.
//
// Ctrl+C 를 누르면 ctx 가 취소된 상태로 채널이 닫힌다.
// 마지막 배치를 취소된 ctx 로 저장하려 하면 즉시 실패해서 통째로 날아간다.
// collector 는 이때 새 시한을 파서 저장해야 한다.
func TestCollectorFlushesOnCancelledContext(t *testing.T) {
	s, ids := setup(t, "a")

	// 이미 취소된 context — 종료 직후 상황을 그대로 재현한다
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	results := make(chan checker.Result, 5)
	for range 5 {
		results <- result("a", true, 50*time.Millisecond)
	}
	close(results)

	// 배치 크기를 크게 잡아 중간 flush 없이 마지막에만 저장되게 한다
	c := New(s, ids, quietLogger(), WithBatchSize(100))
	if err := c.Run(ctx, results, nil); err != nil {
		t.Fatalf("Run 실패: %v", err)
	}

	n, err := s.CountChecks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 5 {
		t.Errorf("저장된 행 %d개, 기대값 5개 — 종료 시 마지막 배치를 잃었다", n)
	}
}

// TestCollectorFlushesOnInterval 은 배치가 안 차도 주기적으로 저장하는지 본다.
func TestCollectorFlushesOnInterval(t *testing.T) {
	s, ids := setup(t, "a")
	ctx := context.Background()

	results := make(chan checker.Result)
	c := New(s, ids, quietLogger(),
		WithBatchSize(1000),                    // 절대 안 찬다
		WithFlushInterval(50*time.Millisecond)) // 주기로만 저장된다

	done := make(chan error, 1)
	go func() { done <- c.Run(ctx, results, nil) }()

	results <- result("a", true, 10*time.Millisecond)
	results <- result("a", true, 20*time.Millisecond)

	// 주기 flush 가 돌 때까지 기다린다
	deadline := time.Now().Add(3 * time.Second)
	for {
		n, err := s.CountChecks(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if n == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("3초 안에 주기 저장이 일어나지 않았다 (저장된 행 %d개)", n)
		}
		time.Sleep(10 * time.Millisecond)
	}

	close(results)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// TestCollectorDropsUnknownMonitor 는 DB에 없는 모니터의 결과를
// 버리되 나머지는 계속 저장하는지 본다.
func TestCollectorDropsUnknownMonitor(t *testing.T) {
	s, ids := setup(t, "a")
	ctx := context.Background()

	results := make(chan checker.Result, 3)
	results <- result("a", true, 10*time.Millisecond)
	results <- result("없는모니터", true, 10*time.Millisecond)
	results <- result("a", true, 20*time.Millisecond)
	close(results)

	c := New(s, ids, quietLogger(), WithBatchSize(1))
	if err := c.Run(ctx, results, nil); err != nil {
		t.Fatalf("Run 실패: %v", err)
	}

	n, _ := s.CountChecks(ctx)
	if n != 2 {
		t.Errorf("저장된 행 %d개, 기대값 2개", n)
	}
	st := c.Stats()
	if st.Dropped != 1 {
		t.Errorf("Dropped = %d, 기대값 1", st.Dropped)
	}
	if st.Written != 2 {
		t.Errorf("Written = %d, 기대값 2", st.Written)
	}
}

// TestCollectorCallsHook 은 onResult 훅이 결과마다 불리는지 본다.
// M3의 상태 전이 판정이 여기에 붙는다.
func TestCollectorCallsHook(t *testing.T) {
	s, ids := setup(t, "a")
	ctx := context.Background()

	results := make(chan checker.Result, 3)
	for range 3 {
		results <- result("a", true, 10*time.Millisecond)
	}
	close(results)

	var seen int
	c := New(s, ids, quietLogger())
	err := c.Run(ctx, results, func(checker.Result) { seen++ })
	if err != nil {
		t.Fatal(err)
	}
	if seen != 3 {
		t.Errorf("훅이 %d번 불렸다, 기대값 3번", seen)
	}
}

// TestCollectorConvertsResult 는 체크 결과가 DB 행으로 제대로 옮겨지는지 본다.
func TestCollectorConvertsResult(t *testing.T) {
	s, ids := setup(t, "a")
	ctx := context.Background()

	results := make(chan checker.Result, 1)
	results <- checker.Result{
		Monitor: "a", CheckedAt: time.Now().UTC(), OK: false,
		StatusCode: 500, Latency: 1234 * time.Millisecond,
		Err: errors.New("서버 오류"),
	}
	close(results)

	c := New(s, ids, quietLogger())
	if err := c.Run(ctx, results, nil); err != nil {
		t.Fatal(err)
	}

	last, err := s.LastCheck(ctx, ids["a"])
	if err != nil || last == nil {
		t.Fatalf("LastCheck 실패: %v", err)
	}
	if last.OK {
		t.Error("OK 가 true 로 저장됐다")
	}
	if last.StatusCode != 500 {
		t.Errorf("StatusCode = %d, 기대값 500", last.StatusCode)
	}
	if last.LatencyMS != 1234 {
		t.Errorf("LatencyMS = %d, 기대값 1234", last.LatencyMS)
	}
	if last.Error != "서버 오류" {
		t.Errorf("Error = %q, 기대값 \"서버 오류\"", last.Error)
	}
}
