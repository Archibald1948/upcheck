package alert

import (
	"testing"
	"time"
)

func TestHumanDuration(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{14900 * time.Millisecond, "15초"}, // 버림이 아니라 반올림
		{0, "0초"},
		{45 * time.Second, "45초"},
		{59*time.Second + 600*time.Millisecond, "1분 0초"},
		{90 * time.Second, "1분 30초"},
		{time.Hour + 5*time.Minute, "1시간 5분"},
		{25 * time.Hour, "25시간 0분"},
	}
	for _, c := range cases {
		if got := humanDuration(c.d); got != c.want {
			t.Errorf("humanDuration(%v) = %q, 기대값 %q", c.d, got, c.want)
		}
	}
}

func TestEventTitleAndBody(t *testing.T) {
	down := Event{Monitor: "결제", Target: "https://x", Kind: KindDown, Cause: "타임아웃", FailCount: 3, At: time.Now()}
	if got := down.Title(); got == "" || !contains(got, "결제") {
		t.Errorf("Title() = %q", got)
	}
	if got := down.Body(); !contains(got, "타임아웃") || !contains(got, "3회") {
		t.Errorf("Body() = %q", got)
	}

	up := Event{Monitor: "결제", Target: "https://x", Kind: KindUp, Downtime: 90 * time.Second, At: time.Now()}
	if got := up.Body(); !contains(got, "1분 30초") {
		t.Errorf("Body() = %q", got)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestRulesNormalize(t *testing.T) {
	got := Rules{FailureThreshold: 0, SuccessThreshold: -1, Cooldown: -time.Second}.normalize()
	if got.FailureThreshold != DefaultRules.FailureThreshold ||
		got.SuccessThreshold != DefaultRules.SuccessThreshold ||
		got.Cooldown != DefaultRules.Cooldown {
		t.Errorf("잘못된 값이 기본값으로 안 바뀌었다: %+v", got)
	}

	// 쿨다운 0은 유효한 값이다 (알림을 즉시 보내고 싶을 때)
	if got := (Rules{FailureThreshold: 1, SuccessThreshold: 1, Cooldown: 0}).normalize(); got.Cooldown != 0 {
		t.Errorf("쿨다운 0이 %v 로 바뀌었다", got.Cooldown)
	}
}
