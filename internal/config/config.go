// Package config 는 YAML 설정 파일을 읽어 모니터 목록으로 만든다.
//
// Go에서 package 이름은 보통 디렉터리 이름과 같게 맞춘다.
// 다른 파일에서는 import 후 config.Load(...) 처럼 "패키지명.식별자"로 쓴다.
package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
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

	// ── tls 전용 ──
	// 인증서 만료가 이 일수 이하로 남으면 경고한다. 기본 30일.
	CertWarnDays int `yaml:"cert_warn_days"`

	// ── dns 전용 ──
	// Record 는 조회할 레코드 종류다 (A, AAAA, CNAME, TXT, MX, NS). 기본 A.
	Record string `yaml:"record"`
	// Expect 는 기대하는 응답 값들이다. 비어 있으면 "해석만 되면 정상"으로 본다.
	// 여러 개를 적으면 그중 하나라도 나오면 정상이다.
	Expect []string `yaml:"expect"`
	// Resolver 는 쓸 DNS 서버다 (예: "8.8.8.8:53"). 비우면 시스템 기본값.
	Resolver string `yaml:"resolver"`

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
	defaultCertWarnDays   = 30
	defaultDNSRecord      = "A"

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
			m.Type = TypeHTTP
		}
		if m.IntervalSec <= 0 {
			m.IntervalSec = defaultIntervalSec
		}
		if m.TimeoutMS <= 0 {
			m.TimeoutMS = defaultTimeoutMS
		}

		// 타입마다 필요한 필드와 target 모양이 다르다.
		if err := m.applyTypeDefaults(); err != nil {
			return fmt.Errorf("%s: %w", m.Name, err)
		}
	}
	return nil
}

// 지원하는 체크 타입.
const (
	TypeHTTP = "http"
	TypeTCP  = "tcp"
	TypeTLS  = "tls"
	TypeDNS  = "dns"
)

// dnsRecordTypes 는 지원하는 DNS 레코드 종류다.
//
// map[string]struct{} 는 '집합'이다. struct{} 는 크기가 0이라
// 값에 메모리를 쓰지 않는다.
var dnsRecordTypes = map[string]struct{}{
	"A": {}, "AAAA": {}, "CNAME": {}, "TXT": {}, "MX": {}, "NS": {},
}

// applyTypeDefaults 는 타입별 기본값을 채우고 target 모양을 검증한다.
//
// 잘못된 설정은 첫 체크가 실패할 때가 아니라 여기서 걸러야 한다.
// 오타 하나 때문에 한밤중에 가짜 장애 알림을 받으면 곤란하다.
func (m *Monitor) applyTypeDefaults() error {
	switch m.Type {
	case TypeHTTP:
		if m.ExpectedStatus == 0 {
			m.ExpectedStatus = defaultExpectedStatus
		}
		u, err := url.Parse(m.Target)
		if err != nil {
			return fmt.Errorf("target 을 URL 로 해석할 수 없다: %w", err)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return fmt.Errorf("http 타입의 target 은 http(s):// 로 시작해야 한다 (%q)", m.Target)
		}
		if u.Host == "" {
			return fmt.Errorf("target 에 호스트가 없다 (%q)", m.Target)
		}

	case TypeTCP:
		// tcp 는 포트를 반드시 적어야 한다. 기본 포트를 짐작할 근거가 없다.
		if _, _, err := net.SplitHostPort(m.Target); err != nil {
			return fmt.Errorf("tcp 타입의 target 은 host:port 여야 한다 (%q)", m.Target)
		}

	case TypeTLS:
		// tls 는 포트를 생략하면 443 으로 본다.
		if m.CertWarnDays <= 0 {
			m.CertWarnDays = defaultCertWarnDays
		}
		host, err := normalizeTLSTarget(m.Target)
		if err != nil {
			return err
		}
		m.Target = host

	case TypeDNS:
		if m.Record == "" {
			m.Record = defaultDNSRecord
		}
		m.Record = strings.ToUpper(m.Record)
		if _, ok := dnsRecordTypes[m.Record]; !ok {
			return fmt.Errorf("지원하지 않는 레코드 종류 %q (A, AAAA, CNAME, TXT, MX, NS)", m.Record)
		}
		if strings.ContainsAny(m.Target, "/:") {
			return fmt.Errorf("dns 타입의 target 은 호스트 이름이어야 한다 (%q)", m.Target)
		}
		if m.Resolver != "" {
			if _, _, err := net.SplitHostPort(m.Resolver); err != nil {
				return fmt.Errorf("resolver 는 host:port 여야 한다 (%q)", m.Resolver)
			}
		}

	default:
		return fmt.Errorf("알 수 없는 타입 %q (http, tcp, tls, dns)", m.Type)
	}
	return nil
}

// normalizeTLSTarget 은 tls target 을 host:port 로 맞춘다.
//
// "example.com" → "example.com:443"
// "example.com:8443" → 그대로
// "https://example.com/path" → "example.com:443" (URL 을 적어도 받아준다)
func normalizeTLSTarget(target string) (string, error) {
	if target == "" {
		return "", fmt.Errorf("target 이 비어 있다")
	}

	// URL 형태로 적었으면 호스트만 뽑는다. 흔한 실수라 에러 대신 받아준다.
	if strings.Contains(target, "://") {
		u, err := url.Parse(target)
		if err != nil || u.Host == "" {
			return "", fmt.Errorf("tls 타입의 target 을 해석할 수 없다 (%q)", target)
		}
		target = u.Host
	}

	if _, _, err := net.SplitHostPort(target); err == nil {
		return target, nil // 이미 포트가 있다
	}
	// 포트가 없으면 443 을 붙인다. 그래도 안 되면 형식이 잘못된 것이다.
	withPort := net.JoinHostPort(target, "443")
	if _, _, err := net.SplitHostPort(withPort); err != nil {
		return "", fmt.Errorf("tls 타입의 target 을 해석할 수 없다 (%q)", target)
	}
	return withPort, nil
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
