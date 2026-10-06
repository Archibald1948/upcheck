package scheduler

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Archibald1948/upcheck/internal/checker"
	"github.com/Archibald1948/upcheck/internal/config"
)

// quietLogger 는 테스트 출력이 경고로 뒤덮이지 않게 로그를 버린다.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// concurrencyServer 는 동시에 처리 중인 요청 수의 최댓값을 기록하는 테스트 서버다.
type concurrencyServer struct {
	*httptest.Server
	current atomic.Int64
	max     atomic.Int64
	total   atomic.Int64
}

func newConcurrencyServer(delay time.Duration) *concurrencyServer {
	cs := &concurrencyServer{}
	cs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cs.total.Add(1)
		now := cs.current.Add(1)
		defer cs.current.Add(-1)

		for {
			peak := cs.max.Load()
			if now <= peak || cs.max.CompareAndSwap(peak, now) {
				break
			}
		}

		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
		w.Write([]byte("ok"))
	}))
	return cs
}

// makeMonitors 는 같은 URL 을 보는 모니터 n개를 만든다.
func makeMonitors(n int, url string, interval time.Duration) []config.Monitor {
	ms := make([]config.Monitor, n)
	for i := range ms {
		ms[i] = config.Monitor{
			Name:           fmt.Sprintf("m%02d", i),
			Type:           "http",
			Target:         url,
			IntervalSec:    int(interval.Seconds()),
			TimeoutMS:      2000,
			ExpectedStatus: 200,
		}
		// Ticker 는 0 이하 주기를 받으면 panic 한다. 최소 1초로 맞춘다.
		if ms[i].IntervalSec < 1 {
			ms[i].IntervalSec = 1
		}
	}
	return ms
}

// ─────────────── 핵심: 동시성 상한 ───────────────

// TestPoolBoundsConcurrency 는 워커풀이 동시 요청 수를 workers 이하로
// 제한하는지 서버 쪽에서 확인한다.
func TestPoolBoundsConcurrency(t *testing.T) {
	srv := newConcurrencyServer(100 * time.Millisecond)
	defer srv.Close()

	const workers = 4
	c := checker.New()
	defer c.Close()

	p := NewPool(c, workers, WithLogger(quietLogger()), WithTick(10*time.Millisecond))

	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()

	var got int
	for range p.Run(ctx, makeMonitors(40, srv.URL, time.Second)) {
		got++
	}

	if got == 0 {
		t.Fatal("결과가 하나도 없다")
	}
	if max := srv.max.Load(); max > workers {
		t.Errorf("동시 요청 최대 %d개 — workers=%d 를 넘었다", max, workers)
	}
	t.Logf("모니터 40개 / workers %d → 동시 요청 최대 %d개, 체크 %d회",
		workers, srv.max.Load(), got)
}

// TestTickerDoesNotBoundConcurrency 는 Ticker 방식에 동시 요청 상한이 없음을 확인한다.
func TestTickerDoesNotBoundConcurrency(t *testing.T) {
	srv := newConcurrencyServer(150 * time.Millisecond)
	defer srv.Close()

	const monitors = 30
	c := checker.New()
	defer c.Close()

	ts := NewTickerScheduler(c, quietLogger())

	ctx, cancel := context.WithTimeout(context.Background(), 800*time.Millisecond)
	defer cancel()

	for range ts.Run(ctx, makeMonitors(monitors, srv.URL, time.Second)) {
	}

	// 정확히 30이라고 단언하면 스케줄링에 따라 흔들릴 수 있으니 여유를 둔다.
	if max := srv.max.Load(); max < monitors/2 {
		t.Errorf("동시 요청 최대 %d개 — 모니터 %d개가 동시에 출발했어야 한다", max, monitors)
	}
	t.Logf("모니터 %d개 → 동시 요청 최대 %d개 (상한 없음)", monitors, srv.max.Load())
}

// ─────────────── 종료와 goroutine 누수 ───────────────

// TestNoGoroutineLeak 은 스케줄러를 취소한 뒤 goroutine 수가 원래대로 돌아오는지 본다.
func TestNoGoroutineLeak(t *testing.T) {
	impls := []struct {
		name string
		make func(*checker.Checker) Scheduler
	}{
		{"pool", func(c *checker.Checker) Scheduler {
			return NewPool(c, 4, WithLogger(quietLogger()), WithTick(10*time.Millisecond))
		}},
		{"ticker", func(c *checker.Checker) Scheduler {
			return NewTickerScheduler(c, quietLogger())
		}},
	}

	for _, impl := range impls {
		t.Run(impl.name, func(t *testing.T) {
			srv := newConcurrencyServer(20 * time.Millisecond)
			defer srv.Close()

			baseline := goroutineCount(t, 0)

			c := checker.New()
			s := impl.make(c)

			ctx, cancel := context.WithCancel(context.Background())
			results := s.Run(ctx, makeMonitors(20, srv.URL, time.Second))

			<-results

			cancel()

			drained := make(chan struct{})
			go func() {
				defer close(drained)
				for range results {
				}
			}()

			select {
			case <-drained:
			case <-time.After(5 * time.Second):
				t.Fatal("5초 안에 결과 채널이 닫히지 않았다 (goroutine 이 안 빠져나옴)")
			}

			c.Close() // 유휴 커넥션과 거기 딸린 goroutine 정리

			got := goroutineCount(t, baseline)
			if got > baseline+2 {
				buf := make([]byte, 1<<16)
				n := runtime.Stack(buf, true)
				t.Errorf("goroutine 누수: 시작 %d개 → 종료 후 %d개\n%s", baseline, got, buf[:n])
			}
			t.Logf("goroutine: 시작 %d개 → 종료 후 %d개", baseline, got)
		})
	}
}

// goroutineCount 는 goroutine 수가 want 이하로 잦아들 때까지 기다렸다가 센다.
// want=0 이면 그냥 현재 값을 센다.
func goroutineCount(t *testing.T, want int) int {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	var n int
	for {
		runtime.GC()
		n = runtime.NumGoroutine()
		if want == 0 || n <= want+2 || time.Now().After(deadline) {
			return n
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestShutdownIsPrompt 는 체크 도중 취소가 빨리 먹히는지 본다.
func TestShutdownIsPrompt(t *testing.T) {
	srv := newConcurrencyServer(3 * time.Second)
	defer srv.Close()

	c := checker.New()
	defer c.Close()
	p := NewPool(c, 4, WithLogger(quietLogger()), WithTick(10*time.Millisecond))

	ctx, cancel := context.WithCancel(context.Background())
	results := p.Run(ctx, makeMonitors(10, srv.URL, time.Second))

	time.Sleep(200 * time.Millisecond)

	start := time.Now()
	cancel()
	for range results {
	}
	elapsed := time.Since(start)

	if elapsed > 2*time.Second {
		t.Errorf("종료에 %v 걸렸다 — 진행 중인 요청이 취소되지 않았다", elapsed)
	}
	t.Logf("취소 후 %v 만에 종료", elapsed.Round(time.Millisecond))
}

// ─────────────── 스케줄링 동작 ───────────────

// TestPoolChecksEveryMonitor 는 과부하 상황에서도 모든 모니터가
// 최소 한 번은 체크되는지 본다.
func TestPoolChecksEveryMonitor(t *testing.T) {
	srv := newConcurrencyServer(80 * time.Millisecond)
	defer srv.Close()

	const monitors = 30
	c := checker.New()
	defer c.Close()

	p := NewPool(c, 2, WithLogger(quietLogger()), WithTick(10*time.Millisecond))

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	seen := make(map[string]int)
	for res := range p.Run(ctx, makeMonitors(monitors, srv.URL, time.Second)) {
		seen[res.Monitor]++
	}

	if len(seen) != monitors {
		t.Errorf("모니터 %d개 중 %d개만 체크됐다 — 굶은 모니터가 있다", monitors, len(seen))
	}

	st := p.Stats()
	t.Logf("체크된 모니터 %d/%d · 예약 %d · 큐가득미룸 %d · 최대지연 %v",
		len(seen), monitors, st.Scheduled, st.Deferred, st.MaxLag.Round(time.Millisecond))
}

// TestPoolSkipsInflightMonitor 는 이전 체크가 안 끝난 모니터를
// 큐에 중복으로 넣지 않는지 본다.
func TestPoolSkipsInflightMonitor(t *testing.T) {
	srv := newConcurrencyServer(1500 * time.Millisecond)
	defer srv.Close()

	c := checker.New()
	defer c.Close()
	p := NewPool(c, 8, WithLogger(quietLogger()), WithTick(10*time.Millisecond))

	ctx, cancel := context.WithTimeout(context.Background(), 2500*time.Millisecond)
	defer cancel()

	for range p.Run(ctx, makeMonitors(1, srv.URL, time.Second)) {
	}

	if st := p.Stats(); st.Skipped == 0 {
		t.Errorf("체크가 주기보다 오래 걸렸는데 건너뛴 기록이 없다: %+v", st)
	} else {
		t.Logf("진행 중이라 건너뜀 %d회 (의도된 동작)", st.Skipped)
	}
}

// ─────────────── 단위 테스트 ───────────────

func TestAdvance(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	cases := []struct {
		name           string
		due, now, want time.Time
	}{
		{
			name: "정상 — due 기준으로 더한다 (drift 방지)",
			due:  base, now: base.Add(100 * time.Millisecond),
			want: base.Add(time.Second),
		},
		{
			name: "너무 밀림 — now 기준으로 리셋",
			due:  base, now: base.Add(time.Hour),
			want: base.Add(time.Hour + time.Second),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := advance(tc.due, time.Second, tc.now); !got.Equal(tc.want) {
				t.Errorf("advance() = %v, 기대값 %v", got, tc.want)
			}
		})
	}
}

func TestObserveLagKeepsMaximum(t *testing.T) {
	var c counters
	for _, d := range []time.Duration{50, 300, 120, 80} {
		c.observeLag(d * time.Millisecond)
	}
	if got := c.snapshot().MaxLag; got != 300*time.Millisecond {
		t.Errorf("MaxLag = %v, 기대값 300ms", got)
	}
}

// TestCountersAreRaceFree 는 -race 로 돌릴 때 의미가 있다.
func TestCountersAreRaceFree(t *testing.T) {
	var c counters
	done := make(chan struct{})

	for range 8 {
		go func() {
			defer func() { done <- struct{}{} }()
			for i := range 500 {
				c.scheduled.Add(1)
				c.observeLag(time.Duration(i) * time.Microsecond)
			}
		}()
	}
	for range 8 {
		<-done
	}

	if got := c.snapshot().Scheduled; got != 4000 {
		t.Errorf("Scheduled = %d, 기대값 4000", got)
	}
}

func TestSchedulerNames(t *testing.T) {
	c := checker.New()
	defer c.Close()

	if name := NewPool(c, 8).Name(); name == "" {
		t.Error("Pool.Name() 이 비어 있다")
	}
	if name := NewTickerScheduler(c, nil).Name(); name == "" {
		t.Error("TickerScheduler.Name() 이 비어 있다")
	}
}
