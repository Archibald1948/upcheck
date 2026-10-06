// Package config 는 YAML 설정 파일을 읽어 모니터 목록으로 만든다.
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
	Expect []string `yaml:"expect"`
	// Resolver 는 쓸 DNS 서버다 (예: "8.8.8.8:53"). 비우면 시스템 기본값.
	Resolver string `yaml:"resolver"`

	// Enabled 는 *bool 이다. 안 쓴 경우 nil 이 되어 "기본값 true"를 적용할 수 있다.
	Enabled *bool `yaml:"enabled"`
}

// Timeout 은 TimeoutMS(정수 밀리초)를 time.Duration 으로 바꿔준다.
func (m Monitor) Timeout() time.Duration {
	return time.Duration(m.TimeoutMS) * time.Millisecond
}

// Interval 은 체크 주기를 time.Duration 으로 바꿔준다.
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
	FailureThreshold int `yaml:"failure_threshold"`

	// SuccessThreshold 는 연속 몇 번 성공해야 up 으로 확정할지다.
	SuccessThreshold int `yaml:"success_threshold"`

	// Cooldown 은 같은 모니터에 다시 알림을 보내기까지의 최소 간격이다.
	// yaml.v3 는 Duration 을 모른다 — 나노초 정수로 해석해버린다.
	Cooldown string `yaml:"cooldown"`

	// 웹훅 주소. ${VAR} 로 환경변수를 참조할 수 있다.
	Discord string `yaml:"discord_webhook"`
	Slack   string `yaml:"slack_webhook"`
}

// Config 는 설정 파일 전체의 모양이다.
type Config struct {
	Workers  int       `yaml:"workers"`
	Monitors []Monitor `yaml:"monitors"`
	Alerts   Alerts    `yaml:"alerts"`
}

// 기본값들.
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
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("설정 파일 읽기 실패: %w", err)
	}

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
var dnsRecordTypes = map[string]struct{}{
	"A": {}, "AAAA": {}, "CNAME": {}, "TXT": {}, "MX": {}, "NS": {},
}

// applyTypeDefaults 는 타입별 기본값을 채우고 target 모양을 검증한다.
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
		if _, _, err := net.SplitHostPort(m.Target); err != nil {
			return fmt.Errorf("tcp 타입의 target 은 host:port 여야 한다 (%q)", m.Target)
		}

	case TypeTLS:
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

// normalizeTLSTarget 은 tls target 을 host:port 로 맞춘다. 포트가 없으면 443.
func normalizeTLSTarget(target string) (string, error) {
	if target == "" {
		return "", fmt.Errorf("target 이 비어 있다")
	}

	// URL 형태로 적었으면 호스트만 뽑는다.
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
	out := make([]Monitor, 0, len(c.Monitors))
	for _, m := range c.Monitors {
		if m.IsEnabled() {
			out = append(out, m)
		}
	}
	return out
}
