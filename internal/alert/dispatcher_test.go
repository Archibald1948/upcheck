package alert

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// recordingNotifier 는 받은 이벤트를 모아 두는 가짜 채널이다.
type recordingNotifier struct {
	name  string
	mu    sync.Mutex
	got   []Event
	err   error         // nil 이 아니면 항상 실패한다
	delay time.Duration // 발송이 느린 상황을 흉내낸다
	panic bool
}

func (r *recordingNotifier) Name() string { return r.name }

func (r *recordingNotifier) Notify(ctx context.Context, ev Event) error {
	if r.panic {
		panic("일부러 낸 panic")
	}
	if r.delay > 0 {
		select {
		case <-time.After(r.delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if r.err != nil {
		return r.err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, ev)
	return nil
}

func (r *recordingNotifier) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.got)
}

// runDispatcher 는 발송기를 goroutine 으로 띄우고 정리 함수를 돌려준다.
func runDispatcher(d *Dispatcher) func() {
	done := make(chan struct{})
	go func() {
		defer close(done)
		d.Run(context.Background())
	}()
	return func() {
		d.Close()
		<-done
	}
}

func TestDispatcherFansOutToAllNotifiers(t *testing.T) {
	a := &recordingNotifier{name: "a"}
	b := &recordingNotifier{name: "b"}
	d := NewDispatcher(quietLogger(), []Notifier{a, b})

	stop := runDispatcher(d)
	d.Send(downEvent())
	d.Send(upEvent())
	stop()

	if a.count() != 2 || b.count() != 2 {
		t.Errorf("채널별 수신 a=%d b=%d, 각 2건이어야 한다", a.count(), b.count())
	}
	if st := d.Stats(); st.Sent != 4 {
		t.Errorf("Sent = %d, 기대값 4 (이벤트 2 × 채널 2)", st.Sent)
	}
}

// TestSendNeverBlocks 는 큐가 가득 차도 Send 가 즉시 반환하는지 본다.
func TestSendNeverBlocks(t *testing.T) {
	// 발송 시한이 길면 뒷정리(큐 비우기)가 이벤트마다 시한을 기다려 테스트가 느려진다.
	slow := &recordingNotifier{name: "slow", delay: time.Hour}
	d := NewDispatcher(quietLogger(), []Notifier{slow},
		WithQueueSize(2), WithSendTimeout(50*time.Millisecond))

	stop := runDispatcher(d)
	defer stop()

	// 큐(2) + 처리 중(1) 보다 훨씬 많이 밀어 넣는다
	start := time.Now()
	var accepted, dropped int
	for range 100 {
		if d.Send(downEvent()) {
			accepted++
		} else {
			dropped++
		}
	}
	elapsed := time.Since(start)

	if elapsed > time.Second {
		t.Errorf("Send 100회에 %v 걸렸다 — 블로킹됐다", elapsed)
	}
	if dropped == 0 {
		t.Error("큐가 가득 찼는데 버려진 게 없다")
	}
	if st := d.Stats(); st.Dropped != int64(dropped) {
		t.Errorf("Dropped = %d, 기대값 %d", st.Dropped, dropped)
	}
	t.Logf("Send 100회 %v · 수용 %d · 버림 %d", elapsed.Round(time.Millisecond), accepted, dropped)
}

// TestDispatcherDrainsOnClose 는 종료 시 큐에 남은 알림을 마저 보내는지 본다.
func TestDispatcherDrainsOnClose(t *testing.T) {
	n := &recordingNotifier{name: "n", delay: 20 * time.Millisecond}
	d := NewDispatcher(quietLogger(), []Notifier{n}, WithQueueSize(32))

	stop := runDispatcher(d)
	for range 10 {
		d.Send(downEvent())
	}
	stop() // Close 후 Run 이 끝날 때까지 기다린다

	if n.count() != 10 {
		t.Errorf("수신 %d건 — 종료 시 큐를 다 비워야 한다", n.count())
	}
}

// TestOneFailingNotifierDoesNotBlockOthers 는 채널 하나가 실패해도
// 다른 채널은 정상 발송되는지 본다.
func TestOneFailingNotifierDoesNotBlockOthers(t *testing.T) {
	bad := &recordingNotifier{name: "bad", err: errors.New("웹훅 죽음")}
	good := &recordingNotifier{name: "good"}
	d := NewDispatcher(quietLogger(), []Notifier{bad, good})

	stop := runDispatcher(d)
	d.Send(downEvent())
	stop()

	if good.count() != 1 {
		t.Errorf("정상 채널이 %d건 받았다, 기대값 1건", good.count())
	}
	st := d.Stats()
	if st.Failed != 1 || st.Sent != 1 {
		t.Errorf("Sent=%d Failed=%d, 각 1이어야 한다", st.Sent, st.Failed)
	}
}

// TestNotifierPanicDoesNotKillProcess 는 채널이 panic 해도
// 발송기가 계속 도는지 본다.
func TestNotifierPanicDoesNotKillProcess(t *testing.T) {
	boom := &recordingNotifier{name: "boom", panic: true}
	good := &recordingNotifier{name: "good"}
	d := NewDispatcher(quietLogger(), []Notifier{boom, good})

	stop := runDispatcher(d)
	d.Send(downEvent())
	d.Send(upEvent()) // panic 후에도 다음 이벤트가 처리돼야 한다
	stop()

	if good.count() != 2 {
		t.Errorf("정상 채널이 %d건 받았다, 기대값 2건", good.count())
	}
	if st := d.Stats(); st.Failed != 2 {
		t.Errorf("Failed = %d, 기대값 2", st.Failed)
	}
}

// TestSlowNotifierIsCutOff 는 느린 채널이 시한에 걸려 잘리는지 본다.
func TestSlowNotifierIsCutOff(t *testing.T) {
	slow := &recordingNotifier{name: "slow", delay: 5 * time.Second}
	d := NewDispatcher(quietLogger(), []Notifier{slow}, WithSendTimeout(100*time.Millisecond))

	stop := runDispatcher(d)
	start := time.Now()
	d.Send(downEvent())
	stop()
	elapsed := time.Since(start)

	if elapsed > 2*time.Second {
		t.Errorf("발송 시한이 안 걸렸다: %v", elapsed)
	}
	if st := d.Stats(); st.Failed != 1 {
		t.Errorf("Failed = %d, 기대값 1", st.Failed)
	}
}

// TestDispatcherIsRaceFree 는 여러 goroutine 이 동시에 Send 해도
// 통계가 어긋나지 않는지 본다. -race 로 돌 때 의미가 있다.
func TestDispatcherIsRaceFree(t *testing.T) {
	n := &recordingNotifier{name: "n"}
	d := NewDispatcher(quietLogger(), []Notifier{n}, WithQueueSize(1024))
	stop := runDispatcher(d)

	var sent atomic.Int64
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				if d.Send(downEvent()) {
					sent.Add(1)
				}
			}
		}()
	}
	wg.Wait()
	stop()

	if got := int64(n.count()); got != sent.Load() {
		t.Errorf("수신 %d건 ≠ 수용 %d건", got, sent.Load())
	}
}

func TestDispatcherWithNoNotifiers(t *testing.T) {
	d := NewDispatcher(quietLogger(), nil)
	if d.Enabled() {
		t.Error("채널이 없는데 Enabled() 가 true 다")
	}
	stop := runDispatcher(d)
	d.Send(downEvent()) // 아무 일도 안 일어나야 한다
	stop()

	if st := d.Stats(); st.Sent != 0 || st.Failed != 0 {
		t.Errorf("발송 채널이 없는데 통계가 잡혔다: %+v", st)
	}
}
