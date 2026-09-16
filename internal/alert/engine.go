package alert

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/Archibald1948/upcheck/internal/checker"
	"github.com/Archibald1948/upcheck/internal/store"
)

// status 는 확정된 모니터 상태다.
//
// iota 는 const 블록에서 0부터 자동으로 증가하는 값이다.
// 열거형이 없는 Go 에서 상수 묶음을 만들 때 쓴다.
type status int

const (
	statusUnknown status = iota // 아직 확정되지 않음 (기동 직후)
	statusUp
	statusDown
)

func (s status) String() string {
	switch s {
	case statusUp:
		return "up"
	case statusDown:
		return "down"
	default:
		return "unknown"
	}
}

// monitorState 는 모니터 하나의 판정 상태다.
type monitorState struct {
	confirmed       status // 확정된 현재 상태
	consecutiveFail int
	consecutiveOK   int

	// notified 는 "사용자에게 마지막으로 알린 상태"다.
	// confirmed 와 다르면 알릴 거리가 남아 있다는 뜻이다.
	notified   status
	notifiedAt time.Time

	// firstFailAt 은 현재 연속 실패가 시작된 시각이다.
	// 장애 시작 시각으로 쓴다 — 확정 시각(3번째 실패)이 아니라
	// 실제로 죽기 시작한 시각(1번째 실패)이 맞다.
	firstFailAt time.Time
	downSince   time.Time
	downCause   string
}

// Engine 은 체크 결과를 보고 알릴지 판정한다.
//
// 스펙 M3의 세 규칙을 담고 있다.
//  1. 상태 전이에만 발송 (매 체크마다 보내지 않는다)
//  2. 플래핑 방지 — 연속 N회 실패해야 down 확정
//  3. 쿨다운 — 같은 모니터에 X분 내 재발송 금지
//
// Observe 는 collector goroutine 하나에서만 불린다.
// 그래서 states 맵에 뮤텍스가 필요 없다. 호출 지점이 늘어나면 붙여야 한다.
type Engine struct {
	store *store.Store
	ids   map[string]int64
	rules Rules
	sink  EventSink
	log   *slog.Logger

	states map[string]*monitorState
	now    func() time.Time // 테스트에서 시간을 조작하려고 주입 가능하게 둔다

	// atomic 인 이유: Observe 는 collector goroutine 에서만 불리지만
	// Stats() 는 /metrics 스크레이프에서 불린다.
	sent       atomic.Int64
	suppressed atomic.Int64
}

// EventSink 는 판정된 이벤트를 받는 곳이다.
//
// Dispatcher 가 이걸 구현한다. 인터페이스로 끊어두면
// 테스트에서 가짜 sink 를 넣어 발송 없이 판정만 검증할 수 있다.
type EventSink interface {
	Send(ev Event) bool // 큐에 넣었으면 true, 가득 차서 버렸으면 false
}

// EngineOption 은 Engine 설정을 바꾼다.
type EngineOption func(*Engine)

// WithClock 은 시계를 갈아끼운다. 테스트에서 쿨다운을 검증할 때 쓴다.
func WithClock(now func() time.Time) EngineOption {
	return func(e *Engine) { e.now = now }
}

// NewEngine 은 판정 엔진을 만든다.
func NewEngine(s *store.Store, ids map[string]int64, rules Rules, sink EventSink, log *slog.Logger, opts ...EngineOption) *Engine {
	if log == nil {
		log = slog.Default()
	}
	e := &Engine{
		store:  s,
		ids:    ids,
		rules:  rules.normalize(),
		sink:   sink,
		log:    log,
		states: make(map[string]*monitorState),
		now:    time.Now,
	}
	for _, opt := range opts {
		opt(e)
	}
	return e
}

// Restore 는 DB에 열려 있는 장애를 읽어 상태를 복원한다.
//
// 이게 없으면 프로그램을 재시작할 때마다 이미 알린 장애로 알림이 다시 간다.
// 재시작이 잦은 환경(배포, 크래시 루프)에서 알림 폭탄의 원인이 된다.
func (e *Engine) Restore(ctx context.Context) error {
	if e.store == nil {
		return nil
	}
	for name, id := range e.ids {
		inc, err := e.store.OpenIncidentFor(ctx, id)
		if err != nil {
			return err
		}
		if inc == nil {
			continue
		}
		// 진행 중인 장애가 있다 = 이미 down 을 알렸다고 본다.
		e.states[name] = &monitorState{
			confirmed:   statusDown,
			notified:    statusDown,
			notifiedAt:  e.now(), // 재시작 직후 즉시 재발송하지 않도록
			downSince:   inc.StartedAt,
			downCause:   inc.Cause,
			firstFailAt: inc.StartedAt,
		}
		e.log.Info("진행 중인 장애를 복원했다", "monitor", name, "started_at", inc.StartedAt)
	}
	return nil
}

// Stats 는 발송/억제 횟수다.
type Stats struct {
	Sent       int64
	Suppressed int64
}

// Stats 는 언제든, 어느 goroutine 에서든 부를 수 있다.
func (e *Engine) Stats() Stats {
	return Stats{Sent: e.sent.Load(), Suppressed: e.suppressed.Load()}
}

// Observe 는 체크 결과 하나를 받아 상태를 갱신하고, 필요하면 이벤트를 낸다.
//
// collector 의 onResult 훅에서 불린다.
func (e *Engine) Observe(ctx context.Context, res checker.Result, target string) {
	st, ok := e.states[res.Monitor]
	if !ok {
		st = &monitorState{}
		e.states[res.Monitor] = st
	}

	prev := st.confirmed
	e.updateCounters(st, res)
	e.confirmStatus(st, res)

	// 확정 상태가 바뀌었으면 장애 이력을 남긴다.
	//
	// 알림을 실제로 보냈는지와 무관하게 기록한다.
	// 쿨다운으로 알림이 억제돼도 "무슨 일이 있었는가"는 남아야 한다.
	if st.confirmed != prev {
		e.recordIncident(ctx, res, st, prev)
	}

	e.maybeNotify(st, res, target)
}

// updateCounters 는 연속 성공/실패 횟수를 갱신한다.
func (e *Engine) updateCounters(st *monitorState, res checker.Result) {
	if res.OK {
		st.consecutiveOK++
		st.consecutiveFail = 0
		return
	}

	if st.consecutiveFail == 0 {
		// 이번 연속 실패의 첫 번째다. 장애 시작 시각으로 쓴다.
		st.firstFailAt = res.CheckedAt
	}
	st.consecutiveFail++
	st.consecutiveOK = 0
}

// confirmStatus 는 임계치를 넘었는지 보고 상태를 확정한다.
func (e *Engine) confirmStatus(st *monitorState, res checker.Result) {
	switch {
	case st.consecutiveFail >= e.rules.FailureThreshold:
		if st.confirmed != statusDown {
			st.downSince = st.firstFailAt
			st.downCause = errText(res)
		}
		st.confirmed = statusDown

	case st.consecutiveOK >= e.rules.SuccessThreshold:
		st.confirmed = statusUp
	}
	// 둘 다 아니면 이전 확정 상태를 유지한다.
	// 실패 1회로는 아직 down 이 아니고, 그렇다고 up 도 아니다.
}

// recordIncident 는 장애 시작/해소를 DB에 남긴다.
func (e *Engine) recordIncident(ctx context.Context, res checker.Result, st *monitorState, prev status) {
	if e.store == nil {
		return
	}
	id, ok := e.ids[res.Monitor]
	if !ok {
		return
	}

	switch st.confirmed {
	case statusDown:
		if _, err := e.store.OpenIncident(ctx, id, st.downSince, st.downCause); err != nil {
			// 기록에 실패해도 알림은 계속 가야 한다. 로그만 남긴다.
			e.log.Error("장애 기록 실패", "monitor", res.Monitor, "err", err)
		}
	case statusUp:
		if prev == statusDown {
			if err := e.store.ResolveIncident(ctx, id, res.CheckedAt); err != nil {
				e.log.Error("장애 해소 기록 실패", "monitor", res.Monitor, "err", err)
			}
		}
	}
}

// maybeNotify 는 세 가지 관문을 거쳐 발송 여부를 정한다.
func (e *Engine) maybeNotify(st *monitorState, res checker.Result, target string) {
	// 관문 1: 아직 확정 안 됨 — 알릴 게 없다.
	if st.confirmed == statusUnknown {
		return
	}

	// 관문 2: 이미 알린 상태와 같다 — 새 소식이 아니다.
	// 스펙 M3 "상태 전이에만 발송. 매 체크마다 보내면 안 됩니다."
	if st.confirmed == st.notified {
		return
	}

	// 기동 직후 첫 확정이 'up' 이면 조용히 넘어간다.
	// "잘 돌고 있다"는 건 알림거리가 아니다.
	// notifiedAt 은 일부러 갱신하지 않는다 — 갱신하면 기동 직후 장애가
	// 쿨다운에 걸려 억제된다.
	if st.notified == statusUnknown && st.confirmed == statusUp {
		st.notified = statusUp
		return
	}

	// 관문 3: 쿨다운.
	// 스펙 M3 "같은 모니터에 대해 X분 내 재발송 금지."
	//
	// 억제해도 st.notified 는 갱신하지 않는다. 그래서 쿨다운이 끝난 뒤
	// 다음 체크에서 다시 이 지점에 와서 '그때의 실제 상태'를 보낸다.
	// 알림을 잃는 게 아니라 미루는 것이고, 미룬 사이에 상태가 원래대로
	// 돌아갔으면 보낼 것도 없어진다. 이게 플래핑 방어의 핵심이다.
	now := e.now()
	if !st.notifiedAt.IsZero() && now.Sub(st.notifiedAt) < e.rules.Cooldown {
		e.suppressed.Add(1)
		e.log.Debug("쿨다운으로 알림 억제",
			"monitor", res.Monitor, "상태", st.confirmed.String(),
			"남은시간", (e.rules.Cooldown - now.Sub(st.notifiedAt)).Round(time.Second))
		return
	}

	ev := Event{
		Monitor: res.Monitor,
		Target:  target,
		At:      res.CheckedAt,
	}
	if st.confirmed == statusDown {
		ev.Kind = KindDown
		ev.Cause = st.downCause
		ev.FailCount = st.consecutiveFail
		ev.At = st.downSince
	} else {
		ev.Kind = KindUp
		ev.Downtime = res.CheckedAt.Sub(st.downSince)
		if st.downSince.IsZero() {
			ev.Downtime = 0
		}
	}

	st.notified = st.confirmed
	st.notifiedAt = now
	e.sent.Add(1)

	if e.sink != nil && !e.sink.Send(ev) {
		e.log.Warn("알림 큐가 가득 차 이벤트를 버렸다", "monitor", res.Monitor)
	}
}

// errText 는 실패 사유를 문자열로 꺼낸다.
func errText(res checker.Result) string {
	if res.Err != nil {
		return res.Err.Error()
	}
	return "알 수 없는 실패"
}
