package config

import (
	"os"
	"path/filepath"
	"testing"
)

// writeTemp 은 임시 YAML 파일을 만들어 경로를 돌려준다.
func writeTemp(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "monitors.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("임시 파일 쓰기 실패: %v", err)
	}
	return path
}

func TestLoadAppliesDefaults(t *testing.T) {
	path := writeTemp(t, `
monitors:
  - name: 최소설정
    target: https://example.com
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load 실패: %v", err)
	}

	m := cfg.Monitors[0]
	checks := []struct {
		name string
		got  any
		want any
	}{
		{"Type", m.Type, "http"},
		{"IntervalSec", m.IntervalSec, defaultIntervalSec},
		{"TimeoutMS", m.TimeoutMS, defaultTimeoutMS},
		{"ExpectedStatus", m.ExpectedStatus, defaultExpectedStatus},
		{"Workers", cfg.Workers, defaultWorkers},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, 기대값 %v", c.name, c.got, c.want)
		}
	}
}

func TestLoadKeepsExplicitValues(t *testing.T) {
	path := writeTemp(t, `
workers: 32
monitors:
  - name: 명시설정
    target: https://example.com
    interval_sec: 5
    timeout_ms: 1500
    expected_status: 204
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load 실패: %v", err)
	}
	if cfg.Workers != 32 {
		t.Errorf("Workers = %d, 기대값 32", cfg.Workers)
	}
	m := cfg.Monitors[0]
	if m.IntervalSec != 5 || m.TimeoutMS != 1500 || m.ExpectedStatus != 204 {
		t.Errorf("명시한 값이 덮어써졌다: %+v", m)
	}
	if got := m.Timeout().Milliseconds(); got != 1500 {
		t.Errorf("Timeout() = %dms, 기대값 1500ms", got)
	}
}

func TestLoadRejectsBadConfig(t *testing.T) {
	cases := map[string]string{
		"모니터 없음":   "monitors: []",
		"이름 없음":    "monitors:\n  - target: https://example.com",
		"타깃 없음":    "monitors:\n  - name: x",
		"미지원 타입":   "monitors:\n  - name: x\n    target: y\n    type: tcp",
		"잘못된 YAML": "monitors: [[[",
	}

	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(writeTemp(t, content)); err == nil {
				t.Error("에러를 기대했는데 성공했다")
			}
		})
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load("이런파일은없다.yaml"); err == nil {
		t.Error("없는 파일인데 에러가 안 났다")
	}
}

func TestEnabledMonitors(t *testing.T) {
	path := writeTemp(t, `
monitors:
  - name: 기본값(켜짐)
    target: https://a.example
  - name: 명시적으로 켬
    target: https://b.example
    enabled: true
  - name: 끔
    target: https://c.example
    enabled: false
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load 실패: %v", err)
	}

	enabled := cfg.EnabledMonitors()
	if len(enabled) != 2 {
		t.Fatalf("켜진 모니터 %d개, 기대값 2개", len(enabled))
	}
	for _, m := range enabled {
		if m.Name == "끔" {
			t.Error("꺼둔 모니터가 포함됐다")
		}
	}
	// 원본은 그대로 3개여야 한다 (EnabledMonitors 가 원본을 건드리면 안 됨)
	if len(cfg.Monitors) != 3 {
		t.Errorf("원본 모니터가 %d개로 바뀌었다", len(cfg.Monitors))
	}
}
