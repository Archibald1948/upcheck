package alert

import (
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/Archibald1948/upcheck/internal/config"
)

// FromConfig 는 설정에서 판정 규칙과 발송기를 만든다.
// 웹훅 주소가 없으면 발송 채널이 없는 Dispatcher 를 돌려준다.
func FromConfig(a config.Alerts, log *slog.Logger, opts ...DispatcherOption) (Rules, *Dispatcher, error) {
	cooldown, err := a.CooldownDuration()
	if err != nil {
		return Rules{}, nil, err
	}

	rules := Rules{
		FailureThreshold: a.FailureThreshold,
		SuccessThreshold: a.SuccessThreshold,
		Cooldown:         cooldown,
	}.normalize()

	var notifiers []Notifier

	if a.Discord != "" {
		if err := validateWebhookURL("discord_webhook", a.Discord); err != nil {
			return Rules{}, nil, err
		}
		notifiers = append(notifiers, NewDiscord(a.Discord))
	}
	if a.Slack != "" {
		if err := validateWebhookURL("slack_webhook", a.Slack); err != nil {
			return Rules{}, nil, err
		}
		notifiers = append(notifiers, NewSlack(a.Slack))
	}

	return rules, NewDispatcher(log, notifiers, opts...), nil
}

// validateWebhookURL 은 주소가 쓸 만한지 시작 시점에 걸러낸다.
func validateWebhookURL(field, raw string) error {
	if strings.Contains(raw, "${") || strings.Contains(raw, "$") {
		return fmt.Errorf("alerts.%s: 환경변수가 치환되지 않았다 (%q)", field, raw)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("alerts.%s: 주소를 해석할 수 없다: %w", field, err)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return fmt.Errorf("alerts.%s: http(s) 주소여야 한다 (%q)", field, raw)
	}
	if u.Host == "" {
		return fmt.Errorf("alerts.%s: 호스트가 없다 (%q)", field, raw)
	}
	return nil
}

// Describe 는 알림 설정을 한 줄로 요약한다. 기동 로그에 쓴다.
func Describe(r Rules, d *Dispatcher) string {
	if !d.Enabled() {
		return fmt.Sprintf("알림 채널 없음 (판정과 장애 이력은 기록됨) · 실패 %d회로 down 확정", r.FailureThreshold)
	}
	return fmt.Sprintf("알림 %s · 실패 %d회로 down 확정 · 쿨다운 %v",
		strings.Join(d.Names(), ", "), r.FailureThreshold, r.Cooldown.Round(time.Second))
}
