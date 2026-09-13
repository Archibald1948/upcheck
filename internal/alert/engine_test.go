package alert

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

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// fakeSink 는 발송 없이 이벤트를 모아 둔다.
// 판정 로직만 떼어 검증하려는 것이다.
type fakeSink struct {
	events []Event
	full   bool // true 면 큐가 가득 찬 상황을 흉내낸다
}

func (f *fakeSink) Send(ev Event) bool {
	if f.full {
		return false
	}
	f.events = append(f.events, ev)
	return true
}

func (f *fakeSink) kinds() []Kind {
	out := make([]Kind, len(f.events))
	for i, e := range f.events {
		out[i] = e.Kind
	}
	return out
}

// clock 은 테스트에서 시간을 손으로 돌린다.
// 쿨다운을 검증하려면 실제로 5분을 기다릴 수 없다.
type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

type harness struct {
	engine *Engine
	sink   *fakeSink
	clock  *clock
	store  *store.Store
	ids    map[string]int64
}

func newHarness(t *testing.T, rules Rules) *harness {
	t.Helper()
	ctx := context.Background()

	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "test.db"), quietLogger())
	if err != nil {
		t.Fatalf("store.Open 실패: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	ids, err := s.SyncMonitors(ctx, []config.Monitor{{
		Name: "svc", Type: "http", Target: "https://svc.example",
		IntervalSec: 60, TimeoutMS: 5000, ExpectedStatus: 200,
	}})
	if err != nil {
		t.Fatalf("SyncMonitors 실패: %v", err)
	}

	c := &clock{t: time.Date(2026, 4, 1, 9, 0, 0, 0, time.UTC)}
	sink := &fakeSink{}
	e := NewEngine(s, ids, rules, sink, quietLogger(), WithClock(c.now))
	return &harness{engine: e, sink: sink, clock: c, store: s, ids: ids}
}

// feed 는 체크 결과를 n번 흘려 넣는다. 체크 시각은 시계를 따라간다.
func (h *harness) feed(t *testing.T, ok bool, n int, step time.Duration) {
	t.Helper()
	for range n {
		res := checker.Result{
			Monitor: "svc", CheckedAt: h.clock.now(), OK: ok, Latency: 50 * time.Millisecond,
		}
		if ok {
			res.StatusCode = 200
		} else {
			res.StatusCode = 503
			res.Err = errors.New("상태 코드 503 (기대값 200)")
		}
		h.engine.Observe(context.Background(), res, "https://svc.example")
		h.clock.advance(step)
	}
}

// ─────────────── 스펙 완료 기준 ───────────────

// TestDownThenUpSendsExactlyOnce 는 스펙 6절 완료 기준을 검증한다.
//
//	"일부러 죽인 엔드포인트에 대해 down 알림이 정확히 1회 발송되고,
//	 복구 시 up 알림 1회"
func TestDownThenUpSendsExactlyOnce(t *testing.T) {
	h := newHarness(t, Rules{FailureThreshold: 3, SuccessThreshold: 1, Cooldown: time.Minute})

	// 처음엔 정상. "잘 돌고 있다"는 알림거리가 아니다.
	h.feed(t, true, 5, time.Second)
	if len(h.sink.events) != 0 {
		t.Fatalf("정상 상태인데 알림이 %d건 나갔다: %v", len(h.sink.events), h.sink.kinds())
	}

	// 엔드포인트가 죽는다. 임계치(3회)를 넘으면 down 확정.
	h.feed(t, false, 10, time.Second)

	if got := h.sink.kinds(); len(got) != 1 || got[0] != KindDown {
		t.Fatalf("down 알림이 정확히 1건이어야 한다: %v", got)
	}

	// 쿨다운이 지나도 상태가 그대로면 다시 보내지 않는다.
	h.clock.advance(10 * time.Minute)
	h.feed(t, false, 10, time.Second)
	if got := h.sink.kinds(); len(got) != 1 {
		t.Fatalf("상태가 그대로인데 알림이 늘었다: %v", got)
	}

	// 복구. 쿨다운은 이미 지났으므로 즉시 나간다.
	h.feed(t, true, 5, time.Second)

	got := h.sink.kinds()
	if len(got) != 2 || got[0] != KindDown || got[1] != KindUp {
		t.Fatalf("down 1회 + up 1회여야 한다: %v", got)
	}

	// up 이벤트에 장애 지속 시간이 담겨야 한다
	up := h.sink.events[1]
	if up.Downtime <= 0 {
		t.Errorf("Downtime 이 %v 다", up.Downtime)
	}
	t.Logf("down → up 각 1회 · 장애 지속 %v", up.Downtime)
}

// TestFlappingDoesNotStorm 는 스펙 6절 완료 기준을 검증한다.
//
//	"3초에 한 번 껐다 켜지는(플래핑) 엔드포인트에 알림 폭탄이 발생하지 않음"
//
// slowserver 의 /flaky 를 그대로 흉내낸다: 3초 주기로 200 ↔ 503.
func TestFlappingDoesNotStorm(t *testing.T) {
	const cooldown = 5 * time.Minute
	h := newHarness(t, Rules{FailureThreshold: 3, SuccessThreshold: 1, Cooldown: cooldown})

	// 1초 주기로 체크. 3초마다 상태가 뒤집힌다. 10분 동안.
	const total = 600
	for i := range total {
		ok := (i/3)%2 == 0
		h.feed(t, ok, 1, time.Second)
	}

	got := len(h.sink.events)

	// 방어가 없었다면 상태 전이가 100회 가까이 일어난다.
	// 쿨다운이 5분이므로 10분 동안 많아야 2~3건이어야 한다.
	maxExpected := int(10*time.Minute/cooldown) + 1
	if got > maxExpected {
		t.Errorf("알림 폭탄: 10분 동안 %d건 (최대 %d건이어야 한다)", got, maxExpected)
	}
	if got == 0 {
		t.Error("플래핑 중인데 알림이 하나도 안 나갔다 — 너무 과하게 막았다")
	}

	st := h.engine.Stats()
	t.Logf("체크 %d회 · 발송 %d건 · 쿨다운 억제 %d건", total, got, st.Suppressed)
}

// ─────────────── 판정 규칙 ───────────────

// TestSingleFailureDoesNotAlert 는 일시적 실패 1회로 알림이 가지 않는지 본다.
func TestSingleFailureDoesNotAlert(t *testing.T) {
	h := newHarness(t, Rules{FailureThreshold: 3, SuccessThreshold: 1, Cooldown: time.Minute})

	h.feed(t, true, 3, time.Second)
	h.feed(t, false, 2, time.Second) // 임계치 3회에 못 미친다
	h.feed(t, true, 3, time.Second)

	if len(h.sink.events) != 0 {
		t.Errorf("연속 %d회 미만 실패로 알림이 나갔다: %v", 3, h.sink.kinds())
	}
}

func TestFailureThresholdIsRespected(t *testing.T) {
	// 임계치를 바꿔가며 확정 시점이 맞는지 본다
	for _, threshold := range []int{1, 2, 5} {
		t.Run("임계치"+itoa(threshold), func(t *testing.T) {
			h := newHarness(t, Rules{FailureThreshold: threshold, SuccessThreshold: 1, Cooldown: time.Hour})
			h.feed(t, true, 1, time.Second)

			// 임계치 직전까지는 조용해야 한다
			h.feed(t, false, threshold-1, time.Second)
			if len(h.sink.events) != 0 {
				t.Fatalf("%d회 실패에서 알림이 나갔다 (임계치 %d)", threshold-1, threshold)
			}

			// 임계치에 닿으면 나간다
			h.feed(t, false, 1, time.Second)
			if len(h.sink.events) != 1 {
				t.Fatalf("%d회 실패인데 알림이 %d건 (1건이어야)", threshold, len(h.sink.events))
			}
		})
	}
}

// TestDownAtStartupAlerts 는 기동 시점에 이미 죽어 있으면 알리는지 본다.
// "잘 돌고 있다"는 안 알리지만 "죽어 있다"는 알려야 한다.
func TestDownAtStartupAlerts(t *testing.T) {
	h := newHarness(t, Rules{FailureThreshold: 3, SuccessThreshold: 1, Cooldown: 5 * time.Minute})

	h.feed(t, false, 3, time.Second)

	if got := h.sink.kinds(); len(got) != 1 || got[0] != KindDown {
		t.Errorf("기동 직후 장애를 알려야 한다: %v", got)
	}
}

// TestCooldownDelaysButDoesNotLose 는 쿨다운이 알림을 버리는 게 아니라
// 미루는 것인지 확인한다.
func TestCooldownDelaysButDoesNotLose(t *testing.T) {
	const cooldown = 5 * time.Minute
	h := newHarness(t, Rules{FailureThreshold: 1, SuccessThreshold: 1, Cooldown: cooldown})

	h.feed(t, true, 1, time.Second)  // 조용히 up 확정
	h.feed(t, false, 1, time.Second) // down 알림 1건

	if len(h.sink.events) != 1 {
		t.Fatalf("down 알림이 %d건", len(h.sink.events))
	}

	// 쿨다운 안에 복구 — 억제된다
	h.feed(t, true, 1, time.Second)
	if len(h.sink.events) != 1 {
		t.Fatalf("쿨다운 안인데 up 이 나갔다: %v", h.sink.kinds())
	}

	// 쿨다운이 지나고 다음 체크가 오면, 그때의 실제 상태(up)를 보낸다
	h.clock.advance(cooldown + time.Second)
	h.feed(t, true, 1, time.Second)

	got := h.sink.kinds()
	if len(got) != 2 || got[1] != KindUp {
		t.Errorf("쿨다운이 끝나면 밀린 상태를 보내야 한다: %v", got)
	}
}

// ─────────────── 장애 이력 ───────────────

// TestIncidentRecorded 는 장애가 DB에 남는지 본다.
func TestIncidentRecorded(t *testing.T) {
	h := newHarness(t, Rules{FailureThreshold: 2, SuccessThreshold: 1, Cooldown: time.Minute})
	ctx := context.Background()

	h.feed(t, true, 1, time.Second)
	downStart := h.clock.now()
	h.feed(t, false, 4, time.Second)

	// 진행 중인 장애가 있어야 한다
	open, err := h.store.OpenIncidentFor(ctx, h.ids["svc"])
	if err != nil {
		t.Fatal(err)
	}
	if open == nil {
		t.Fatal("진행 중인 장애가 기록되지 않았다")
	}
	// 시작 시각은 '확정 시점'이 아니라 '첫 실패 시점'이어야 한다
	if !open.StartedAt.Equal(downStart) {
		t.Errorf("장애 시작 %v, 기대값 %v (첫 실패 시각)", open.StartedAt, downStart)
	}
	if open.Cause == "" {
		t.Error("장애 사유가 비어 있다")
	}

	// 복구하면 닫혀야 한다
	h.feed(t, true, 2, time.Second)
	open, err = h.store.OpenIncidentFor(ctx, h.ids["svc"])
	if err != nil {
		t.Fatal(err)
	}
	if open != nil {
		t.Error("복구했는데 장애가 닫히지 않았다")
	}

	all, err := h.store.Incidents(ctx, h.ids["svc"], 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("장애 이력 %d건, 기대값 1건", len(all))
	}
	if all[0].ResolvedAt == nil {
		t.Error("해소 시각이 기록되지 않았다")
	}
	t.Logf("장애 1건 기록 · 지속 %v · 사유 %q",
		all[0].Duration(h.clock.now()).Round(time.Second), all[0].Cause)
}

// TestIncidentRecordedEvenWhenSuppressed 는 알림이 억제돼도
// 장애 이력은 남는지 본다.
//
// 알림은 사람을 깨우는 것이고 이력은 사실의 기록이다. 둘은 별개여야 한다.
func TestIncidentRecordedEvenWhenSuppressed(t *testing.T) {
	h := newHarness(t, Rules{FailureThreshold: 1, SuccessThreshold: 1, Cooldown: time.Hour})
	ctx := context.Background()

	h.feed(t, true, 1, time.Second)
	h.feed(t, false, 1, time.Second) // down 알림 1건 (쿨다운 시작)
	h.feed(t, true, 1, time.Second)  // 복구 — 알림은 억제
	h.feed(t, false, 1, time.Second) // 또 장애 — 알림은 억제

	if n := len(h.sink.events); n != 1 {
		t.Fatalf("알림 %d건, 쿨다운으로 1건이어야 한다", n)
	}

	all, err := h.store.Incidents(ctx, h.ids["svc"], 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Errorf("장애 이력 %d건 — 알림이 억제돼도 2건 다 남아야 한다", len(all))
	}
}

// TestRestoreSkipsDuplicateAlert 는 재시작 시 이미 알린 장애로
// 다시 알림이 가지 않는지 본다.
func TestRestoreSkipsDuplicateAlert(t *testing.T) {
	h := newHarness(t, Rules{FailureThreshold: 2, SuccessThreshold: 1, Cooldown: time.Minute})
	ctx := context.Background()

	h.feed(t, true, 1, time.Second)
	h.feed(t, false, 3, time.Second)
	if len(h.sink.events) != 1 {
		t.Fatalf("down 알림 %d건", len(h.sink.events))
	}

	// 재시작을 흉내낸다: 같은 DB에 새 엔진
	sink2 := &fakeSink{}
	e2 := NewEngine(h.store, h.ids, Rules{FailureThreshold: 2, SuccessThreshold: 1, Cooldown: time.Minute},
		sink2, quietLogger(), WithClock(h.clock.now))
	if err := e2.Restore(ctx); err != nil {
		t.Fatal(err)
	}

	// 여전히 죽어 있다
	for range 5 {
		e2.Observe(ctx, checker.Result{
			Monitor: "svc", CheckedAt: h.clock.now(), OK: false,
			StatusCode: 503, Err: errors.New("상태 코드 503 (기대값 200)"),
		}, "https://svc.example")
		h.clock.advance(time.Second)
	}

	if len(sink2.events) != 0 {
		t.Errorf("재시작 후 같은 장애로 알림이 다시 갔다: %v", sink2.kinds())
	}

	// 장애 이력도 중복 생성되면 안 된다
	all, _ := h.store.Incidents(ctx, h.ids["svc"], 10)
	if len(all) != 1 {
		t.Errorf("장애 이력 %d건 — 재시작으로 중복 생성됐다", len(all))
	}
}

// TestSinkFullIsNotFatal 는 알림 큐가 가득 차도 판정이 계속 도는지 본다.
func TestSinkFullIsNotFatal(t *testing.T) {
	h := newHarness(t, Rules{FailureThreshold: 1, SuccessThreshold: 1, Cooldown: 0})
	h.sink.full = true

	h.feed(t, true, 1, time.Second)
	h.feed(t, false, 3, time.Second)
	h.feed(t, true, 3, time.Second)

	// 이벤트는 버려졌지만 장애 이력은 남아야 한다
	all, _ := h.store.Incidents(context.Background(), h.ids["svc"], 10)
	if len(all) == 0 {
		t.Error("큐가 가득 찼다고 장애 이력까지 빠졌다")
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
