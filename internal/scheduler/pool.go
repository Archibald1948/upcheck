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
//
//	스케줄러 goroutine 1개 ──jobs──> 워커 goroutine N개 ──results──> 호출부
//
// 핵심은 goroutine 수가 모니터 수와 무관하게 N+2 로 고정된다는 것이다.
// 모니터가 10개든 10000개든 동시에 나가는 HTTP 요청은 최대 N개다.
type Pool struct {
	workers int
	tick    time.Duration
	checker *checker.Checker
	log     *slog.Logger

	counters counters

	// inflight 는 "지금 체크가 진행 중인 모니터" 집합이다.
	//
	// 응답이 30초 걸리는 모니터를 10초 주기로 잡아두면, 이게 없을 때
	// 같은 모니터의 체크가 큐에 계속 쌓인다. 이미 도는 중이면 건너뛴다.
	//
	// 맵은 동시 접근에 안전하지 않다(동시에 쓰면 런타임이 프로그램을 죽인다).
	// 스케줄러 goroutine 이 넣고 워커 goroutine 이 빼므로 뮤텍스로 보호한다.
	mu       sync.Mutex
	inflight map[string]struct{}
}

// struct{} 는 크기가 0인 타입이다. 맵을 '집합'으로 쓸 때
// map[string]bool 대신 map[string]struct{} 를 쓰면 값에 메모리를 안 쓴다.

// defaultTick 은 스케줄러가 "체크할 때가 된 모니터가 있나" 를 훑는 주기다.
//
// 이 값이 곧 스케줄 정밀도의 하한이다. 1초로 두면 분산(stagger)을 아무리
// 촘촘히 해도 1초 단위로 뭉쳐 버린다. 반대로 너무 짧으면 헛돌기만 한다.
// 250ms 면 모니터 1000개를 훑어도 초당 비교 4000번이라 사실상 공짜다.
const defaultTick = 250 * time.Millisecond

// maxStaggerWindow 는 기동 직후 첫 체크를 흩뿌릴 구간의 상한이다.
//
// 부하를 고르게 펴는 것과, 기동하자마자 현황을 보는 것 사이의 절충이다.
// 이 값이 곧 "모든 모니터의 첫 결과가 나오기까지 걸리는 최대 시간"이다.
//
// 주기 전체에 걸쳐 흩는 게 부하 분산에는 가장 좋지만, 주기가 긴 모니터는
// 첫 결과를 그만큼 기다려야 한다. 그리고 순간적으로 몰리더라도
// 워커풀이 상한을 걸고 밀린 것부터 처리하므로 큰 문제가 되지 않는다.
const maxStaggerWindow = 5 * time.Second

// PoolOption 은 Pool 설정을 바꾸는 함수다.
//
// 가변 옵션이 필요할 때 Go에서 흔히 쓰는 '함수형 옵션' 패턴이다.
// 인자를 계속 늘리거나 설정 구조체를 따로 만들지 않아도 된다.
type PoolOption func(*Pool)

// WithTick 은 스케줄러가 "체크할 때가 됐나" 를 확인하는 주기를 바꾼다.
// 기본 defaultTick. 테스트에서는 짧게 준다.
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
	// opts 는 ...PoolOption — 가변 인자다. 함수 안에서는 슬라이스로 다룬다.
	for _, opt := range opts {
		opt(p)
	}
	return p
}

func (p *Pool) Name() string { return fmt.Sprintf("pool(workers=%d)", p.workers) }
func (p *Pool) Stats() Stats { return p.counters.snapshot() }

// Run 은 스케줄러와 워커들을 띄우고 결과 채널을 돌려준다.
//
// 채널을 닫는 책임은 '보내는 쪽'에 있다 (스펙 5절 규칙 2번).
//   - jobs   : 스케줄러만 보낸다  → 스케줄러가 닫는다
//   - results: 워커들이 보낸다    → 워커가 전부 끝난 뒤 닫는다
func (p *Pool) Run(ctx context.Context, monitors []config.Monitor) <-chan checker.Result {
	// 버퍼를 워커 수의 2배로 잡는다.
	// 0(언버퍼)이면 스케줄러가 워커를 붙잡을 때까지 멈춰 서고,
	// 너무 크면 오래된 작업이 큐에 쌓여 "지금 상태"가 아닌 걸 재게 된다.
	jobs := make(chan Job, p.workers*2)
	results := make(chan checker.Result, p.workers*2)

	// 스케줄러 goroutine 하나.
	go func() {
		defer close(jobs) // 보내는 쪽이 닫는다 → 워커들이 루프를 빠져나온다
		p.schedule(ctx, monitors, jobs)
	}()

	// 워커 N개.
	var wg sync.WaitGroup
	for i := range p.workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.worker(ctx, i, jobs, results)
		}()
	}

	// 워커가 전부 끝나야 results 를 닫을 수 있다.
	// wg.Wait() 은 블로킹이므로 별도 goroutine 에서 기다린다.
	go func() {
		wg.Wait()
		close(results)
	}()

	return results
}

// schedule 은 tick 마다 "지금 체크할 때가 된 모니터"를 찾아 큐에 넣는다.
//
// 모니터마다 Ticker 를 두는 대신 중앙에서 한 번씩 훑는다.
// 모니터 1000개를 1초마다 훑어도 비교 1000번이라 사실상 공짜다.
// (모니터가 수만 개가 되면 다음 실행 시각 기준 최소 힙으로 바꾸면 된다)
func (p *Pool) schedule(ctx context.Context, monitors []config.Monitor, jobs chan<- Job) {
	// 각 모니터의 첫 실행 시각.
	//
	// 전부 now 로 두면 시작하자마자 50개가 동시에 몰려 워커풀이 순간 포화된다.
	// 큐가 넘쳐서 체크가 밀리고, 주기가 같으니 그 뒤로도 계속 같이 몰린다.
	// 그래서 시작 시각을 조금씩 어긋나게 흩어 놓는다(stagger).
	//
	//   모니터 0: now + 0
	//   모니터 1: now + w/n
	//   모니터 2: now + 2w/n   ...   (w = 분산 구간)
	//
	// 한 번 어긋나 놓으면 그 뒤로는 각자 due+interval 로 진행하므로
	// 간격이 그대로 유지된다.
	//
	// 분산 구간 w 를 주기 전체로 잡지 않는 이유:
	// 주기가 60초인 모니터를 60초에 걸쳐 흩으면 마지막 모니터는
	// 첫 결과가 나오기까지 1분 가까이 걸린다. 기동 직후에 현황을 못 본다.
	// 그래서 maxStaggerWindow 로 상한을 둔다. 주기가 그보다 짧으면 주기를 쓴다.
	// 기동 후 최대 그 시간 안에 모든 모니터의 첫 결과가 나온다.
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
	defer ticker.Stop() // Ticker 는 멈추지 않으면 내부 자원이 샌다

	// pending 은 매 tick 마다 재사용한다. 루프 안에서 make 하면
	// 초당 네 번씩 쓰레기를 만든다. [:0] 은 길이만 0으로 만들고
	// 이미 확보한 메모리는 그대로 둔다.
	pending := make([]dueItem, 0, n)

	for {
		now := time.Now()

		pending = pending[:0]
		for i := range monitors {
			if !now.Before(next[i]) {
				pending = append(pending, dueItem{idx: i, due: next[i]})
			}
		}

		// 가장 오래 밀린 것부터 처리한다.
		//
		// 이게 없으면 과부하 때 인덱스가 뒤쪽인 모니터가 영영 굶는다.
		// 매 tick 마다 앞에서부터 큐를 채우니 뒤쪽은 항상 꽉 찬 큐를 만난다.
		// (실제로 50개 모니터를 과부하로 돌렸을 때 42개만 체크됐다)
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

			// 논블로킹 전송. 큐가 가득 찼다는 건 워커가 전부 바쁘다는 뜻이다.
			// 여기서 그냥 기다리면 스케줄러가 멈춰서 다른 모니터까지 밀린다.
			select {
			case jobs <- Job{Monitor: m, Due: d.due}:
				p.counters.scheduled.Add(1)
				next[d.idx] = advance(d.due, m.Interval(), now)

			default:
				p.release(m.Name)
				p.counters.deferred.Add(1)
				p.log.Warn("작업 큐가 가득 차 체크를 미룸",
					"monitor", m.Name, "workers", p.workers)
				// next 를 일부러 갱신하지 않는다.
				// 다음 tick 에도 '밀린 상태'로 남아 정렬에서 앞자리를 차지한다.
				// 한 번 밀린 모니터가 계속 뒤로 밀리는 걸 막는 장치다.
			}
		}

		// 다음 tick 을 기다리되, 그 사이 종료 신호가 오면 즉시 빠져나간다.
		// 이 select 가 없으면 종료까지 최대 tick 만큼 늦어진다.
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
//
// now 가 아니라 due 기준으로 더한다. now 기준으로 더하면
// 체크가 느릴수록 주기가 조금씩 늘어난다(drift).
//
// 다만 너무 오래 밀렸으면(노트북이 절전에서 깨어난 경우 등) 따라잡기를
// 포기하고 현재 시각 기준으로 리셋한다. 안 그러면 밀린 횟수만큼 폭주한다.
func advance(due time.Time, interval time.Duration, now time.Time) time.Time {
	next := due.Add(interval)
	if next.Before(now) {
		return now.Add(interval)
	}
	return next
}

// worker 는 큐에서 작업을 꺼내 체크하고 결과를 내보낸다.
func (p *Pool) worker(ctx context.Context, id int, jobs <-chan Job, results chan<- checker.Result) {
	// jobs 가 <-chan (수신 전용), results 가 chan<- (송신 전용) 인 점에 주목.
	// 방향을 타입으로 제한하면 실수로 반대로 쓰는 걸 컴파일러가 잡아준다.
	for {
		select {
		case <-ctx.Done():
			return

		case job, ok := <-jobs:
			// 채널이 닫히면 ok 가 false 다. 이게 워커의 정상 종료 경로다.
			if !ok {
				return
			}

			lag := time.Since(job.Due)
			p.counters.observeLag(lag)

			res := p.runCheck(ctx, job)
			p.release(job.Monitor.Name)

			// 결과를 보낼 때도 ctx 를 감시해야 한다.
			// 소비자가 사라졌는데 그냥 results <- res 로 보내면
			// 버퍼가 차는 순간 이 goroutine 이 영원히 멈춘다 = goroutine 누수.
			select {
			case results <- res:
			case <-ctx.Done():
				return
			}
		}
	}
}

// runCheck 은 체크 한 번을 돌리되 panic 이 나도 워커가 죽지 않게 감싼다.
//
// goroutine 안에서 난 panic 은 잡지 않으면 프로그램 전체를 죽인다.
// 모니터 하나가 이상해서 데몬이 통째로 내려가면 안 된다.
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
