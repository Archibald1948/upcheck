package config

import (
	"testing"
)

// TestTypeValidation 은 타입별 target 형식 검증을 확인한다.
func TestTypeValidation(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string
		wantErr bool
	}{
		// http
		{"http 정상", "monitors:\n  - name: a\n    type: http\n    target: https://x.example", false},
		{"http 스킴 없음", "monitors:\n  - name: a\n    type: http\n    target: x.example", true},
		{"http 잘못된 스킴", "monitors:\n  - name: a\n    type: http\n    target: ftp://x.example", true},

		// tcp
		{"tcp 정상", "monitors:\n  - name: a\n    type: tcp\n    target: db.example:5432", false},
		{"tcp 포트 없음", "monitors:\n  - name: a\n    type: tcp\n    target: db.example", true},

		// tls
		{"tls 포트 없음(443 붙임)", "monitors:\n  - name: a\n    type: tls\n    target: x.example", false},
		{"tls 포트 있음", "monitors:\n  - name: a\n    type: tls\n    target: x.example:8443", false},
		{"tls URL 로 적음", "monitors:\n  - name: a\n    type: tls\n    target: https://x.example/path", false},

		// dns
		{"dns 정상", "monitors:\n  - name: a\n    type: dns\n    target: x.example", false},
		{"dns 레코드 지정", "monitors:\n  - name: a\n    type: dns\n    target: x.example\n    record: mx", false},
		{"dns 잘못된 레코드", "monitors:\n  - name: a\n    type: dns\n    target: x.example\n    record: SOA", true},
		{"dns target 이 URL", "monitors:\n  - name: a\n    type: dns\n    target: https://x.example", true},
		{"dns resolver 포트 없음", "monitors:\n  - name: a\n    type: dns\n    target: x.example\n    resolver: 8.8.8.8", true},
		{"dns resolver 정상", "monitors:\n  - name: a\n    type: dns\n    target: x.example\n    resolver: 8.8.8.8:53", false},

		// 알 수 없는 타입
		{"모르는 타입", "monitors:\n  - name: a\n    type: gopher\n    target: x", true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Load(writeTemp(t, c.yaml))
			if c.wantErr && err == nil {
				t.Error("에러를 기대했는데 통과했다")
			}
			if !c.wantErr && err != nil {
				t.Errorf("통과를 기대했는데 에러: %v", err)
			}
		})
	}
}

// TestTLSTargetNormalization 은 tls target 이 host:port 로 맞춰지는지 본다.
func TestTLSTargetNormalization(t *testing.T) {
	cases := map[string]string{
		"x.example":              "x.example:443",
		"x.example:8443":         "x.example:8443",
		"https://x.example":      "x.example:443",
		"https://x.example/path": "x.example:443",
		"https://x.example:9443": "x.example:9443",
	}
	for in, want := range cases {
		t.Run(in, func(t *testing.T) {
			cfg, err := Load(writeTemp(t,
				"monitors:\n  - name: a\n    type: tls\n    target: "+in))
			if err != nil {
				t.Fatalf("Load 실패: %v", err)
			}
			if got := cfg.Monitors[0].Target; got != want {
				t.Errorf("target = %q, 기대값 %q", got, want)
			}
		})
	}
}

// TestTypeDefaults 는 타입별 기본값이 채워지는지 본다.
func TestTypeDefaults(t *testing.T) {
	cfg, err := Load(writeTemp(t, `
monitors:
  - name: tls기본
    type: tls
    target: x.example
  - name: dns기본
    type: dns
    target: x.example
  - name: http기본
    target: https://x.example
`))
	if err != nil {
		t.Fatalf("Load 실패: %v", err)
	}

	if got := cfg.Monitors[0].CertWarnDays; got != defaultCertWarnDays {
		t.Errorf("CertWarnDays = %d, 기대값 %d", got, defaultCertWarnDays)
	}
	if got := cfg.Monitors[1].Record; got != defaultDNSRecord {
		t.Errorf("Record = %q, 기대값 %q", got, defaultDNSRecord)
	}
	// type 을 안 적으면 http
	if got := cfg.Monitors[2].Type; got != TypeHTTP {
		t.Errorf("Type = %q, 기대값 %q", got, TypeHTTP)
	}
	if got := cfg.Monitors[2].ExpectedStatus; got != defaultExpectedStatus {
		t.Errorf("ExpectedStatus = %d, 기대값 %d", got, defaultExpectedStatus)
	}
}

// TestDNSRecordIsUppercased 는 소문자로 적어도 받아주는지 본다.
func TestDNSRecordIsUppercased(t *testing.T) {
	cfg, err := Load(writeTemp(t,
		"monitors:\n  - name: a\n    type: dns\n    target: x.example\n    record: cname"))
	if err != nil {
		t.Fatalf("Load 실패: %v", err)
	}
	if got := cfg.Monitors[0].Record; got != "CNAME" {
		t.Errorf("Record = %q, 기대값 CNAME", got)
	}
}

// TestExpectedStatusOnlyForHTTP 는 http 가 아닌 타입에는
// expected_status 기본값이 안 붙는지 본다.
func TestExpectedStatusOnlyForHTTP(t *testing.T) {
	cfg, err := Load(writeTemp(t,
		"monitors:\n  - name: a\n    type: tcp\n    target: x.example:80"))
	if err != nil {
		t.Fatalf("Load 실패: %v", err)
	}
	if got := cfg.Monitors[0].ExpectedStatus; got != 0 {
		t.Errorf("tcp 모니터에 ExpectedStatus = %d 가 붙었다", got)
	}
}
