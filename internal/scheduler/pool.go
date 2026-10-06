package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/Archibald1948/upcheck/internal/checker"
	"github.com/Archibald1948/upcheck/internal/config"
)

// Pool 은 "중앙 스케줄러 + 고정 개수 워커풀" 방식이다.
type Pool struct {
	workers int
	tick    time.Duration
	checker *checker.Checker
	log     *slog.Logger

	counters counters

	// inflight 는 "지금 체크가 진행 중인 모니터" 집합이다. 같은 모니터의 체크가 큐에 쌓이는 걸 막는다.
	mu       sync.Mutex
	inflight map[string]struct{}
}

// defaultTick 은 스케줄러가 "체크할 때가 된 모니터가 있나" 를 훑는 주기다.
// 이 값이 곧 스케줄 정밀도의 하한이다.
const defaultTick = 250 * time.Millisecond

// maxStaggerWindow 는 기동 직후 첫 체크를 흩뿌릴 구간의 상한이다.
// 이 값이 곧 "모든 모니터의 첫 결과가 나오기까지 걸리는 최대 시간"이다.
const maxStaggerWindow = 5 * time.Second

// PoolOption 은 Pool 설정을 바꾸는 함수다.
type PoolOption func(*Pool)

// WithTick 은 스케줄러가 "체크할 때가 됐나" 를 확인하는 주기를 바꾼다.
func WithTick(d time.Duration) PoolOption {
	return func(p *Pool) { p.tick = d }
}

// WithLogger 는 로거를 갈아끼운다.
func WithLogger(l *slog.Logger) PoolOption {
	return func(p *Pool) { p.log = l }
}

// NewPool 은 워커 workers 개짜리 풀을 만든다.
func NewPool(c *checker.Checker, workers int, opts ...PoolOption) *Pool {
	if workers <= 0 {
		workers = 1
	}
	p := &Pool{
		workers:  workers,
		tick:     defaultTick,
		checker:  c,
		log:      slog.Default(),
		inflight: make(map[string]struct{}),
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

func (p *Pool) Name() string { return fmt.Sprintf("pool(workers=%d)", p.workers) }
func (p *Pool) Stats() Stats { return p.counters.snapshot() }

// Run 은 스케줄러와 워커들을 띄우고 결과 채널을 돌려준다.
func (p *Pool) Run(ctx context.Context, monitors []config.Monitor) <-chan checker.Result {
	jobs := make(chan Job, p.workers*2)
	results := make(chan checker.Result, p.workers*2)

	go func() {
		defer close(jobs) // 보내는 쪽이 닫는다 → 워커들이 루프를 빠져나온다
		p.schedule(ctx, monitors, jobs)
	}()

	var wg sync.WaitGroup
	for i := range p.workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.worker(ctx, i, jobs, results)
		}()
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	return results
}

// schedule 은 tick 마다 "지금 체크할 때가 된 모니터"를 찾아 큐에 넣는다.
func (p *Pool) schedule(ctx context.Context, monitors []config.Monitor, jobs chan<- Job) {
	// 첫 실행 시각을 흩어 놓는다(stagger). 전부 now 로 두면 기동 직후 워커풀이 포화된다.
	next := make([]time.Time, len(monitors))
	now := time.Now()
	n := len(monitors)
	for i := range next {
		var offset time.Duration
		if n > 1 {
			w := min(monitors[i].Interval(), maxStaggerWindow)
			offset = w * time.Duration(i) / time.Duration(n)
		}
		next[i] = now.Add(offset)
	}

	ticker := time.NewTicker(p.tick)
	defer ticker.Stop()

	pending := make([]dueItem, 0, n)

	for {
		now := time.Now()

		pending = pending[:0]
		for i := range monitors {
			if !now.Before(next[i]) {
				pending = append(pending, dueItem{idx: i, due: next[i]})
			}
		}

		// 가장 오래 밀린 것부터 처리한다. 없으면 과부하 때 인덱스가 뒤쪽인 모니터가 굶는다.
		slices.SortFunc(pending, func(a, b dueItem) int {
			return a.due.Compare(b.due)
		})

		for _, d := range pending {
			m := monitors[d.idx]

			if !p.acquire(m.Name) {
				// 직전 체크가 아직 안 끝났다. 이번 차례는 포기하고 다음 주기로.
				p.counters.skipped.Add(1)
				p.log.Warn("이전 체크가 진행 중이라 건너뜀", "monitor", m.Name)
				next[d.idx] = advance(d.due, m.Interval(), now)
				continue
			}

			// 논블로킹 전송. 여기서 기다리면 스케줄러가 멈춰 다른 모니터까지 밀린다.
			select {
			case jobs <- Job{Monitor: m, Due: d.due}:
				p.counters.scheduled.Add(1)
				next[d.idx] = advance(d.due, m.Interval(), now)

			default:
				p.release(m.Name)
				p.counters.deferred.Add(1)
				p.log.Warn("작업 큐가 가득 차 체크를 미룸",
					"monitor", m.Name, "workers", p.workers)
				// next 를 일부러 갱신하지 않는다. 다음 tick 정렬에서 앞자리를 차지하게 한다.
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// dueItem 은 이번 tick 에 실행할 때가 된 모니터 하나다.
type dueItem struct {
	idx int
	due time.Time
}

// advance 는 다음 실행 예정 시각을 계산한다.
// drift 를 막으려 due 기준으로 더하되, 너무 밀렸으면 now 기준으로 리셋한다.
func advance(due time.Time, interval time.Duration, now time.Time) time.Time {
	next := due.Add(interval)
	if next.Before(now) {
		return now.Add(interval)
	}
	return next
}

// worker 는 큐에서 작업을 꺼내 체크하고 결과를 내보낸다.
func (p *Pool) worker(ctx context.Context, id int, jobs <-chan Job, results chan<- checker.Result) {
	for {
		select {
		case <-ctx.Done():
			return

		case job, ok := <-jobs:
			if !ok {
				return
			}

			lag := time.Since(job.Due)
			p.counters.observeLag(lag)

			res := p.runCheck(ctx, job)
			p.release(job.Monitor.Name)

			// 보낼 때도 ctx 를 감시해야 소비자가 사라졌을 때 goroutine 이 새지 않는다.
			select {
			case results <- res:
			case <-ctx.Done():
				return
			}
		}
	}
}

// runCheck 은 체크 한 번을 돌리되 panic 이 나도 워커가 죽지 않게 감싼다.
func (p *Pool) runCheck(ctx context.Context, job Job) (res checker.Result) {
	defer func() {
		if r := recover(); r != nil {
			p.log.Error("체크 중 panic", "monitor", job.Monitor.Name, "panic", r)
			res = checker.Result{
				Monitor:   job.Monitor.Name,
				CheckedAt: time.Now().UTC(),
				Err:       fmt.Errorf("내부 오류(panic): %v", r),
			}
		}
	}()
	return p.checker.Check(ctx, job.Monitor)
}

// acquire 는 모니터를 '진행 중'으로 표시한다. 이미 진행 중이면 false.
func (p *Pool) acquire(name string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, busy := p.inflight[name]; busy {
		return false
	}
	p.inflight[name] = struct{}{}
	return true
}

func (p *Pool) release(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.inflight, name)
}
