// Package scheduler 는 "언제 무엇을 체크할지" 를 정하고 실제 체크를 돌린다.
//
// 두 가지 구현이 들어 있다.
//
//	Pool   — 중앙 스케줄러 + 고정 개수 워커풀 (실제로 쓰는 것)
//	Ticker — 모니터마다 time.Ticker goroutine 하나 (비교용)
//
// 왜 둘 다 있는지, 왜 Pool 을 골랐는지는 docs/08-M1-동시성.md 에 있다.
package scheduler

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/gimseongsu/upcheck/internal/checker"
	"github.com/gimseongsu/upcheck/internal/config"
)

// Job 은 워커에게 넘기는 작업 하나다.
type Job struct {
	Monitor config.Monitor

	// Due 는 이 체크가 원래 실행됐어야 할 시각이다.
	// 실제 시작 시각과의 차이가 '지연(lag)' 이고,
	// 이게 계속 커지면 워커가 부족하다는 뜻이다.
	Due time.Time
}

// Scheduler 는 모니터들을 주기적으로 체크해 결과를 채널로 흘려보낸다.
//
// 반환 채널은 ctx 가 취소되고 내부 goroutine 이 전부 정리된 뒤에 닫힌다.
// 그래서 호출부는 "range 로 끝까지 읽으면 종료가 끝난 것"으로 볼 수 있다.
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
//
// 평범한 int64 를 여러 goroutine 에서 증가시키면 data race 다.
// x++ 는 "읽고 → 더하고 → 쓰기" 세 단계라서 중간에 끼어들 수 있기 때문이다.
// (go test -race 가 바로 잡아낸다)
//
// atomic.Int64 는 그 세 단계를 CPU 명령 하나로 처리해준다.
// 뮤텍스보다 가볍고, 단순 카운터에는 이게 맞다.
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

// observeLag 는 지금까지의 최대 지연을 갱신한다.
//
// "읽어서 비교하고 더 크면 쓴다"를 원자적으로 하려면 CAS 루프가 필요하다.
// CompareAndSwap(old, new) 는 "값이 아직 old 일 때만 new 로 바꾸고 true 반환".
// 그 사이 다른 goroutine 이 바꿔치웠으면 false 가 돌아오고, 다시 읽어서 재시도한다.
func (c *counters) observeLag(lag time.Duration) {
	ns := int64(lag)
	for {
		cur := c.maxLagNS.Load()
		if ns <= cur {
			return // 기록 경신이 아니면 할 일 없음
		}
		if c.maxLagNS.CompareAndSwap(cur, ns) {
			return
		}
		// 실패 = 그 찰나에 누가 바꿨다. 다시 읽고 재시도.
	}
}
