package scheduler

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/Archibald1948/upcheck/internal/checker"
	"github.com/Archibald1948/upcheck/internal/config"
)

// TickerScheduler 는 "모니터마다 time.Ticker goroutine 하나" 방식이다.
//
//	모니터 1 ──┐
//	모니터 2 ──┼──> 각자 goroutine + Ticker ──results──> 호출부
//	모니터 N ──┘
//
// 스펙 M1이 요구한 '설계 결정 지점'을 실제로 비교해 보려고 만들었다.
// 코드는 확실히 짧다. 큐도, 워커풀도, inflight 추적도 필요 없다.
//
// 하지만 채택하지 않았다. 이유는 docs/08-M1-동시성.md 에 측정값과 함께 정리했다.
// 요약하면: goroutine 수와 동시 HTTP 요청 수가 모니터 수에 그대로 비례한다.
// 모니터 1000개면 1000개가 동시에 나간다. 상한이 없다.
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
	// 버퍼를 모니터 수만큼 잡는다. 모든 모니터가 동시에 결과를 내밀 수 있으므로
	// 이보다 작으면 소비가 조금만 느려도 goroutine 들이 줄줄이 멈춰 선다.
	// ← 이것부터가 이미 "모니터 수에 비례하는 자원"이다.
	results := make(chan checker.Result, len(monitors))

	var wg sync.WaitGroup
	for _, m := range monitors {
		wg.Add(1)
		// Go 1.22 부터 range 의 반복 변수는 매 반복마다 새로 만들어진다.
		// 그 전에는 모든 goroutine 이 '마지막 m' 을 공유하는 유명한 버그가 있어서
		// go func(m config.Monitor){...}(m) 처럼 넘겨줘야 했다.
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

		// 이 방식의 구조적 약점:
		// 체크가 주기보다 오래 걸려도 Ticker 는 계속 째깍거린다.
		// Ticker 채널의 버퍼는 1이라 밀린 tick 은 조용히 버려진다.
		// 즉 "건너뛰었다"는 사실이 어디에도 기록되지 않는다.
		// Pool 쪽은 skipped/dropped 로 세어서 로그에 남긴다.
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

// 위 두 줄은 "컴파일 타임 인터페이스 검사" 관용구다.
// 값을 버리는(_) 변수 선언이라 런타임 비용이 0이고,
// Pool/TickerScheduler 가 Scheduler 를 만족하지 않으면 컴파일이 실패한다.
// 인터페이스를 구현한다고 어디에도 선언하지 않는 Go에서,
// "이 타입은 이 인터페이스를 만족해야 한다"는 의도를 코드로 남기는 방법이다.
