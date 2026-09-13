package alert

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

const (
	defaultQueueSize   = 64
	defaultSendTimeout = 10 * time.Second
	shutdownGrace      = 5 * time.Second
)

// Dispatcher 는 이벤트를 별도 goroutine 에서 발송한다.
//
// 스펙 7절 함정: "알림 발송이 체크 루프를 블로킹".
// 판정(Engine)과 발송(Dispatcher)을 버퍼 채널로 끊어 놓으면,
// 웹훅 서버가 10초씩 안 받아도 체크는 계속 돈다.
type Dispatcher struct {
	notifiers []Notifier
	events    chan Event
	log       *slog.Logger
	timeout   time.Duration

	sent    atomic.Int64
	failed  atomic.Int64
	dropped atomic.Int64
}

// DispatcherOption 은 Dispatcher 설정을 바꾼다.
type DispatcherOption func(*Dispatcher)

// WithQueueSize 는 버퍼 채널 크기를 정한다.
func WithQueueSize(n int) DispatcherOption {
	return func(d *Dispatcher) {
		if n > 0 {
			d.events = make(chan Event, n)
		}
	}
}

// WithSendTimeout 은 발송 하나에 허용할 시간을 정한다.
func WithSendTimeout(t time.Duration) DispatcherOption {
	return func(d *Dispatcher) {
		if t > 0 {
			d.timeout = t
		}
	}
}

// NewDispatcher 는 발송기를 만든다.
func NewDispatcher(log *slog.Logger, notifiers []Notifier, opts ...DispatcherOption) *Dispatcher {
	if log == nil {
		log = slog.Default()
	}
	d := &Dispatcher{
		notifiers: notifiers,
		events:    make(chan Event, defaultQueueSize),
		log:       log,
		timeout:   defaultSendTimeout,
	}
	for _, opt := range opts {
		opt(d)
	}
	return d
}

// Enabled 는 발송할 채널이 하나라도 있는지 알려준다.
func (d *Dispatcher) Enabled() bool { return len(d.notifiers) > 0 }

// Names 는 등록된 채널 이름들이다.
func (d *Dispatcher) Names() []string {
	out := make([]string, 0, len(d.notifiers))
	for _, n := range d.notifiers {
		out = append(out, n.Name())
	}
	return out
}

// Send 는 이벤트를 큐에 넣는다. 절대 블로킹하지 않는다.
//
// 큐가 가득 차면 버리고 false 를 돌려준다.
// 여기서 기다리면 판정 → collector → 스케줄러까지 줄줄이 멈춘다.
// 알림 몇 개를 잃는 것보다 모니터링이 멈추는 게 훨씬 나쁘다.
func (d *Dispatcher) Send(ev Event) bool {
	select {
	case d.events <- ev:
		return true
	default:
		d.dropped.Add(1)
		return false
	}
}

// Close 는 이벤트 채널을 닫는다. 보내는 쪽(Engine)이 끝난 뒤 불러야 한다.
func (d *Dispatcher) Close() { close(d.events) }

// DispatcherStats 는 발송 결과 집계다.
type DispatcherStats struct {
	Sent    int64
	Failed  int64
	Dropped int64
}

func (d *Dispatcher) Stats() DispatcherStats {
	return DispatcherStats{
		Sent: d.sent.Load(), Failed: d.failed.Load(), Dropped: d.dropped.Load(),
	}
}

// Run 은 이벤트 채널이 닫힐 때까지 발송을 반복한다.
//
// ctx 취소로 중간에 빠져나오지 않는다. 종료 중에도 큐에 남은 알림은
// 보내야 하기 때문이다. "서비스가 죽었다"는 알림을 종료하느라 잃으면 곤란하다.
// 대신 발송마다 자체 시한을 걸어 무한정 매달리지 않게 한다.
func (d *Dispatcher) Run(ctx context.Context) {
	for ev := range d.events {
		d.dispatch(ev)
	}
}

// dispatch 는 이벤트 하나를 모든 채널에 병렬로 보낸다.
//
// 순차로 보내면 느린 채널 하나가 뒤의 채널을 다 막는다.
func (d *Dispatcher) dispatch(ev Event) {
	// 상위 ctx 를 쓰지 않는 이유는 Run 의 주석에 있다.
	// 종료 중에도 발송은 시도하되, 이 시한 안에는 끝낸다.
	ctx, cancel := context.WithTimeout(context.Background(), d.timeout)
	defer cancel()

	var wg sync.WaitGroup
	for _, n := range d.notifiers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// 알림 채널 하나가 panic 해도 프로그램이 죽으면 안 된다.
			defer func() {
				if r := recover(); r != nil {
					d.failed.Add(1)
					d.log.Error("알림 발송 중 panic", "notifier", n.Name(), "panic", r)
				}
			}()

			if err := n.Notify(ctx, ev); err != nil {
				d.failed.Add(1)
				d.log.Error("알림 발송 실패",
					"notifier", n.Name(), "monitor", ev.Monitor, "kind", string(ev.Kind), "err", err)
				return
			}
			d.sent.Add(1)
			d.log.Info("알림 발송",
				"notifier", n.Name(), "monitor", ev.Monitor, "kind", string(ev.Kind))
		}()
	}
	wg.Wait()
}
