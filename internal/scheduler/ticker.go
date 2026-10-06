package scheduler

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/Archibald1948/upcheck/internal/checker"
	"github.com/Archibald1948/upcheck/internal/config"
)

// TickerScheduler 는 "모니터마다 time.Ticker goroutine 하나" 방식이다. Pool 과 비교용이다.
type TickerScheduler struct {
	checker  *checker.Checker
	log      *slog.Logger
	counters counters
}

func NewTickerScheduler(c *checker.Checker, log *slog.Logger) *TickerScheduler {
	if log == nil {
		log = slog.Default()
	}
	return &TickerScheduler{checker: c, log: log}
}

func (t *TickerScheduler) Name() string { return "ticker(모니터당 goroutine)" }
func (t *TickerScheduler) Stats() Stats { return t.counters.snapshot() }

// Run 은 모니터마다 goroutine 을 하나씩 띄운다.
func (t *TickerScheduler) Run(ctx context.Context, monitors []config.Monitor) <-chan checker.Result {
	results := make(chan checker.Result, len(monitors))

	var wg sync.WaitGroup
	for _, m := range monitors {
		wg.Add(1)
		go func() {
			defer wg.Done()
			t.loop(ctx, m, results)
		}()
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	return results
}

// loop 는 모니터 하나를 자기 주기대로 계속 체크한다.
func (t *TickerScheduler) loop(ctx context.Context, m config.Monitor, results chan<- checker.Result) {
	ticker := time.NewTicker(m.Interval())
	defer ticker.Stop()

	for {
		start := time.Now()
		t.counters.scheduled.Add(1)

		res := t.checker.Check(ctx, m)

		select {
		case results <- res:
		case <-ctx.Done():
			return
		}

		// Ticker 채널 버퍼는 1이라 밀린 tick 은 조용히 버려진다. 직접 세서 남긴다.
		if elapsed := time.Since(start); elapsed > m.Interval() {
			t.counters.skipped.Add(1)
			t.counters.observeLag(elapsed - m.Interval())
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

var _ Scheduler = (*TickerScheduler)(nil)
var _ Scheduler = (*Pool)(nil)
