// Package alert 는 상태 전이를 판정하고 알림을 보낸다.
//
// 세 부분으로 나뉜다.
//
//	Engine      체크 결과를 보고 "지금 알려야 하나"를 판정한다 (engine.go)
//	Dispatcher  판정된 이벤트를 별도 goroutine 에서 실제로 발송한다 (dispatcher.go)
//	Notifier    발송 채널 하나 (webhook.go 의 Discord / Slack)
//
// 판정과 발송을 분리한 이유는 스펙 7절 함정이다.
// 알림 서버가 느려도 모니터링은 계속돼야 한다.
package alert

import (
	"context"
	"fmt"
	"time"
)

// Kind 는 이벤트 종류다.
//
// 바탕 타입이 string 인 별도 타입을 만들었다. 그냥 string 을 쓰면
// 아무 문자열이나 들어올 수 있지만, 이렇게 하면 아래 상수들만 쓰게 된다.
type Kind string

const (
	KindDown Kind = "down" // up → down 확정
	KindUp   Kind = "up"   // down → up 확정 (복구)
)

// Event 는 발송할 알림 하나다.
type Event struct {
	Monitor string
	Target  string
	Kind    Kind
	At      time.Time

	// Cause 는 down 이벤트의 실패 사유다. up 이벤트에서는 비어 있다.
	Cause string

	// FailCount 는 down 확정까지 연속 실패한 횟수다.
	FailCount int

	// Downtime 은 up 이벤트에서 장애가 지속된 시간이다.
	Downtime time.Duration
}

// Title 은 알림 제목 줄이다.
func (e Event) Title() string {
	if e.Kind == KindUp {
		return fmt.Sprintf("복구됨: %s", e.Monitor)
	}
	return fmt.Sprintf("장애 발생: %s", e.Monitor)
}

// Body 는 알림 본문이다.
func (e Event) Body() string {
	at := e.At.Local().Format("2006-01-02 15:04:05 MST")
	if e.Kind == KindUp {
		return fmt.Sprintf("%s\n장애 지속 시간: %s\n복구 시각: %s",
			e.Target, humanDuration(e.Downtime), at)
	}
	return fmt.Sprintf("%s\n사유: %s\n연속 실패 %d회\n발생 시각: %s",
		e.Target, e.Cause, e.FailCount, at)
}

// humanDuration 은 지속 시간을 읽기 좋게 만든다.
//
// 먼저 초 단위로 반올림한다. int(d.Seconds()) 로 바로 자르면 14.9초가
// "14초"가 되는데, 같은 값을 Duration.Round 로 찍는 -report 출력은
// "15s"로 나와서 같은 장애가 두 자리에서 다르게 보인다.
func humanDuration(d time.Duration) string {
	d = d.Round(time.Second)
	sec := int(d.Seconds())
	switch {
	case sec < 60:
		return fmt.Sprintf("%d초", sec)
	case sec < 3600:
		return fmt.Sprintf("%d분 %d초", sec/60, sec%60)
	default:
		return fmt.Sprintf("%d시간 %d분", sec/3600, (sec%3600)/60)
	}
}

// Notifier 는 알림 발송 채널 하나다.
//
// 스펙 M3가 요구한 인터페이스. 새 채널(이메일, 텔레그램 등)을 추가하려면
// 이 두 메서드만 구현하면 되고, Engine 과 Dispatcher 는 손댈 필요가 없다.
type Notifier interface {
	Notify(ctx context.Context, ev Event) error
	Name() string
}

// Rules 는 알림 판정 규칙이다.
type Rules struct {
	// FailureThreshold 회 연속 실패해야 down 으로 확정한다.
	// 일시적인 네트워크 끊김 하나로 알림이 가는 걸 막는다.
	FailureThreshold int

	// SuccessThreshold 회 연속 성공해야 up 으로 확정한다.
	SuccessThreshold int

	// Cooldown 안에는 같은 모니터에 다시 보내지 않는다.
	// 플래핑 엔드포인트에서 알림 폭탄을 막는 마지막 방어선이다.
	Cooldown time.Duration
}

// DefaultRules 는 기본 판정 규칙이다.
var DefaultRules = Rules{
	FailureThreshold: 3,
	SuccessThreshold: 1,
	Cooldown:         5 * time.Minute,
}

// normalize 는 잘못된 값을 기본값으로 되돌린다.
func (r Rules) normalize() Rules {
	if r.FailureThreshold <= 0 {
		r.FailureThreshold = DefaultRules.FailureThreshold
	}
	if r.SuccessThreshold <= 0 {
		r.SuccessThreshold = DefaultRules.SuccessThreshold
	}
	if r.Cooldown < 0 {
		r.Cooldown = DefaultRules.Cooldown
	}
	return r
}
