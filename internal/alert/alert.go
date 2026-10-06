// Package alert 는 상태 전이를 판정하고 알림을 보낸다.
package alert

import (
	"context"
	"fmt"
	"time"
)

// Kind 는 이벤트 종류다.
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
// 자르지 않고 반올림해야 -report 출력(Duration.Round)과 값이 맞는다.
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
type Notifier interface {
	Notify(ctx context.Context, ev Event) error
	Name() string
}

// Rules 는 알림 판정 규칙이다.
type Rules struct {
	// FailureThreshold 회 연속 실패해야 down 으로 확정한다.
	FailureThreshold int

	// SuccessThreshold 회 연속 성공해야 up 으로 확정한다.
	SuccessThreshold int

	// Cooldown 안에는 같은 모니터에 다시 보내지 않는다.
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
