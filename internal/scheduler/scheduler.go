// Package scheduler 는 "언제 무엇을 체크할지" 를 정하고 실제 체크를 돌린다.
package scheduler

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/Archibald1948/upcheck/internal/checker"
	"github.com/Archibald1948/upcheck/internal/config"
)

// Job 은 워커에게 넘기는 작업 하나다.
type Job struct {
	Monitor config.Monitor

	// Due 는 이 체크가 원래 실행됐어야 할 시각이다.
	Due time.Time
}

// Scheduler 는 모니터들을 주기적으로 체크해 결과를 채널로 흘려보낸다.
// 반환 채널은 ctx 가 취소되고 내부 goroutine 이 전부 정리된 뒤에 닫힌다.
type Scheduler interface {
	Run(ctx context.Context, monitors []config.Monitor) <-chan checker.Result
	Name() string
	Stats() Stats
}

// Stats 는 스케줄러가 어떻게 돌고 있는지 보여주는 숫자들이다.
type Stats struct {
	Scheduled int64         // 큐에 넣은 작업 수
	Deferred  int64         // 큐가 가득 차서 다음 tick 으로 미룬 횟수
	Skipped   int64         // 이전 체크가 아직 안 끝나서 건너뛴 수
	MaxLag    time.Duration // 예정 시각 대비 가장 크게 밀린 정도
}

// counters 는 여러 goroutine 이 동시에 건드리는 카운터 모음이다.
type counters struct {
	scheduled atomic.Int64
	deferred  atomic.Int64
	skipped   atomic.Int64
	maxLagNS  atomic.Int64 // time.Duration 은 int64 라 나노초로 저장한다
}

func (c *counters) snapshot() Stats {
	return Stats{
		Scheduled: c.scheduled.Load(),
		Deferred:  c.deferred.Load(),
		Skipped:   c.skipped.Load(),
		MaxLag:    time.Duration(c.maxLagNS.Load()),
	}
}

// observeLag 는 지금까지의 최대 지연을 CAS 루프로 갱신한다.
func (c *counters) observeLag(lag time.Duration) {
	ns := int64(lag)
	for {
		cur := c.maxLagNS.Load()
		if ns <= cur {
			return
		}
		if c.maxLagNS.CompareAndSwap(cur, ns) {
			return
		}
	}
}
