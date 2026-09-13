// Package config 는 YAML 설정 파일을 읽어 모니터 목록으로 만든다.
//
// Go에서 package 이름은 보통 디렉터리 이름과 같게 맞춘다.
// 다른 파일에서는 import 후 config.Load(...) 처럼 "패키지명.식별자"로 쓴다.
package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Monitor 는 감시 대상 하나를 나타낸다.
//
// 백틱 안의 `yaml:"name"` 부분을 '구조체 태그(struct tag)'라고 한다.
// 컴파일러는 이걸 그냥 문자열로 달아두기만 하고, yaml 라이브러리가
// 리플렉션으로 읽어서 "YAML의 name 키를 이 필드에 넣어라"로 해석한다.
// 태그가 없으면 yaml.v3 는 필드명을 소문자로 바꾼 이름(intervalsec)을 찾으므로
// snake_case 키를 쓰려면 태그가 반드시 필요하다.
type Monitor struct {
	Name           string `yaml:"name"`
	Type           string `yaml:"type"`
	Target         string `yaml:"target"`
	IntervalSec    int    `yaml:"interval_sec"`
	TimeoutMS      int    `yaml:"timeout_ms"`
	ExpectedStatus int    `yaml:"expected_status"`
	Keyword        string `yaml:"keyword"`

	// Enabled 만 *bool(bool 포인터)인 이유:
	// Go의 bool 제로값은 false다. 그냥 bool로 두면
	// "사용자가 enabled: false 라고 쓴 것"과 "아예 안 쓴 것"을 구분할 수 없다.
	// 포인터로 두면 안 쓴 경우 nil 이 되어 "기본값 true"를 적용할 수 있다.
	Enabled *bool `yaml:"enabled"`
}

// Timeout 은 TimeoutMS(정수 밀리초)를 time.Duration 으로 바꿔준다.
//
// 수신자(receiver)가 m Monitor 인 '메서드'다. 포인터(*Monitor)가 아니라
// 값 수신자를 쓴 이유는 이 메서드가 Monitor 를 수정하지 않기 때문이다.
func (m Monitor) Timeout() time.Duration {
	return time.Duration(m.TimeoutMS) * time.Millisecond
}

// Interval 은 체크 주기를 time.Duration 으로 바꿔준다. (M1에서 스케줄러가 쓴다)
func (m Monitor) Interval() time.Duration {
	return time.Duration(m.IntervalSec) * time.Second
}

// IsEnabled 는 Enabled 가 지정되지 않았으면 true 로 본다.
func (m Monitor) IsEnabled() bool {
	return m.Enabled == nil || *m.Enabled
}

// Alerts 는 알림 설정이다.
type Alerts struct {
	// FailureThreshold 는 연속 몇 번 실패해야 down 으로 확정할지다.
	// 1로 두면 일시적인 네트워크 끊김에도 알림이 간다. 기본 3회.
	FailureThreshold int `yaml:"failure_threshold"`

	// SuccessThreshold 는 연속 몇 번 성공해야 up 으로 확정할지다.
	SuccessThreshold int `yaml:"success_threshold"`

	// Cooldown 은 같은 모니터에 다시 알림을 보내기까지의 최소 간격이다.
	// 플래핑하는 엔드포인트에서 알림 폭탄을 막는 마지막 방어선이다.
	//
	// time.Duration 을 YAML 에서 "5m" 처럼 쓰려면 문자열로 받아야 한다.
	// yaml.v3 는 Duration 을 모른다 — 나노초 정수로 해석해버린다.
	Cooldown string `yaml:"cooldown"`

	// 웹훅 주소. ${VAR} 로 환경변수를 참조할 수 있다.
	// 실제 주소를 설정 파일에 적어 커밋하지 말 것.
	Discord string `yaml:"discord_webhook"`
	Slack   string `yaml:"slack_webhook"`
}

// Config 는 설정 파일 전체의 모양이다.
type Config struct {
	Workers  int       `yaml:"workers"`
	Monitors []Monitor `yaml:"monitors"`
	Alerts   Alerts    `yaml:"alerts"`
}

// 기본값들. Go에는 상수 그룹을 묶는 const ( ... ) 문법이 있다.
const (
	defaultIntervalSec    = 60
	defaultTimeoutMS      = 5000
	defaultExpectedStatus = 200
	defaultWorkers        = 8

	defaultFailureThreshold = 3
	defaultSuccessThreshold = 1
	defaultCooldown         = 5 * time.Minute
)

// Load 는 path 의 YAML 파일을 읽어 Config 를 돌려준다.
//
// 반환 타입이 (*Config, error) 인 것에 주목. Go에는 예외(exception)가 없고
// 에러를 '마지막 반환값'으로 돌려주는 게 관례다. 호출부는 반드시 확인해야 한다.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		// %w 로 감싸면 원본 에러가 보존되어 나중에 errors.Is/As 로 확인할 수 있다.
		return nil, fmt.Errorf("설정 파일 읽기 실패: %w", err)
	}

	// var cfg Config 는 Config 의 '제로값'을 만든다.
	// Go는 선언만 해도 항상 0/""/nil 로 초기화되어 있다. 쓰레기값이 없다.
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("YAML 파싱 실패: %w", err)
	}

	if err := cfg.applyDefaultsAndValidate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// applyDefaultsAndValidate 는 빠진 값을 채우고 잘못된 값을 걸러낸다.
//
// 소문자로 시작하는 이름은 패키지 밖에서 보이지 않는다(unexported).
// Go의 접근 제어는 public/private 키워드가 아니라 '첫 글자 대소문자'다.
//
// 수신자가 *Config(포인터)인 이유: 필드를 실제로 수정해야 하기 때문이다.
// 값 수신자였다면 복사본만 고치고 원본은 그대로였을 것이다.
func (c *Config) applyDefaultsAndValidate() error {
	if c.Workers <= 0 {
		c.Workers = defaultWorkers
	}
	if err := c.Alerts.applyDefaults(); err != nil {
		return err
	}
	if len(c.Monitors) == 0 {
		return fmt.Errorf("모니터가 하나도 없다")
	}

	// range 로 슬라이스를 돌 때 i 는 인덱스, 두 번째 변수는 '값의 복사본'이다.
	// 복사본을 고쳐봐야 원본이 안 바뀌므로, 수정할 때는 c.Monitors[i] 로 접근한다.
	for i := range c.Monitors {
		m := &c.Monitors[i]

		if m.Name == "" {
			return fmt.Errorf("monitors[%d]: name 이 비어 있다", i)
		}
		if m.Target == "" {
			return fmt.Errorf("%s: target 이 비어 있다", m.Name)
		}
		if m.Type == "" {
			m.Type = "http" // M4에서 tcp/tls/dns 가 추가된다
		}
		if m.Type != "http" {
			return fmt.Errorf("%s: 아직 지원하지 않는 타입 %q", m.Name, m.Type)
		}
		if m.IntervalSec <= 0 {
			m.IntervalSec = defaultIntervalSec
		}
		if m.TimeoutMS <= 0 {
			m.TimeoutMS = defaultTimeoutMS
		}
		if m.ExpectedStatus == 0 {
			m.ExpectedStatus = defaultExpectedStatus
		}
	}
	return nil
}

// applyDefaults 는 알림 설정의 빈 값을 채우고 웹훅 주소의 환경변수를 푼다.
func (a *Alerts) applyDefaults() error {
	if a.FailureThreshold <= 0 {
		a.FailureThreshold = defaultFailureThreshold
	}
	if a.SuccessThreshold <= 0 {
		a.SuccessThreshold = defaultSuccessThreshold
	}
	if a.Cooldown == "" {
		a.Cooldown = defaultCooldown.String()
	}
	if _, err := a.CooldownDuration(); err != nil {
		return fmt.Errorf("alerts.cooldown: %w", err)
	}

	// os.ExpandEnv 는 "${VAR}" 와 "$VAR" 를 환경변수 값으로 바꾼다.
	// 없는 변수는 빈 문자열이 된다 — 그러면 그 채널은 그냥 꺼진 것으로 본다.
	a.Discord = os.ExpandEnv(a.Discord)
	a.Slack = os.ExpandEnv(a.Slack)
	return nil
}

// CooldownDuration 은 문자열 쿨다운을 time.Duration 으로 바꾼다.
func (a Alerts) CooldownDuration() (time.Duration, error) {
	if a.Cooldown == "" {
		return defaultCooldown, nil
	}
	d, err := time.ParseDuration(a.Cooldown)
	if err != nil {
		return 0, fmt.Errorf("%q 를 시간으로 해석할 수 없다 (예: 30s, 5m, 1h)", a.Cooldown)
	}
	if d < 0 {
		return 0, fmt.Errorf("쿨다운은 음수일 수 없다: %v", d)
	}
	return d, nil
}

// EnabledMonitors 는 켜져 있는 모니터만 추려서 돌려준다.
func (c *Config) EnabledMonitors() []Monitor {
	// 결과 슬라이스를 만들 때 make([]T, 길이, 용량) 으로 용량을 미리 잡아두면
	// append 할 때마다 배열을 새로 할당하는 일을 줄일 수 있다.
	out := make([]Monitor, 0, len(c.Monitors))
	for _, m := range c.Monitors {
		if m.IsEnabled() {
			out = append(out, m)
		}
	}
	return out
}
