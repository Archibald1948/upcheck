package api

import "time"

// StatusResponse 는 GET /api/status 응답이다.
type StatusResponse struct {
	GeneratedAt time.Time       `json:"generated_at"`
	Overall     string          `json:"overall"` // operational | degraded | outage | unknown
	Monitors    []MonitorStatus `json:"monitors"`
}

// MonitorStatus 는 모니터 하나의 현재 상태다.
type MonitorStatus struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Type   string `json:"type,omitempty"`
	Status string `json:"status"` // up | down | unknown

	LastCheckedAt *time.Time `json:"last_checked_at"`

	Uptime  UptimeWindows  `json:"uptime"`
	Latency LatencySummary `json:"latency"`

	// 아래는 공개 여부를 선택할 수 있는 운영 정보다 (Options.ExposeDetails).
	Target    string `json:"target,omitempty"`
	LastError string `json:"last_error,omitempty"`
	Warning   string `json:"warning,omitempty"`
}

// UptimeWindows 는 기간별 업타임 백분율이다. 데이터가 없는 기간은 null.
type UptimeWindows struct {
	H24 *float64 `json:"24h"`
	D7  *float64 `json:"7d"`
	D30 *float64 `json:"30d"`
}

// LatencySummary 는 최근 24시간 응답시간이다. 단위는 밀리초.
type LatencySummary struct {
	P50MS  *int64 `json:"p50_ms"`
	P95MS  *int64 `json:"p95_ms"`
	Approx bool   `json:"approx"`
}

// HistoryResponse 는 GET /api/monitors/{id}/history 응답이다.
type HistoryResponse struct {
	Monitor   MonitorRef `json:"monitor"`
	Timezone  string     `json:"timezone"`
	Days      []Day      `json:"days"`
	Incidents []Incident `json:"incidents"`
}

// MonitorRef 는 이력 응답에 붙는 모니터 요약이다.
type MonitorRef struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// Day 는 업타임 바의 한 칸이다.
type Day struct {
	Date   string   `json:"date"`   // "2026-09-14" — 시간대 기준 날짜
	Uptime *float64 `json:"uptime"` // 데이터가 없으면 null
	Checks int64    `json:"checks"`
	Failed int64    `json:"failed"`
	P50MS  *int64   `json:"p50_ms"`
	P95MS  *int64   `json:"p95_ms"`
}

// Incident 는 장애 구간 하나다.
type Incident struct {
	ID          int64      `json:"id"`
	MonitorID   int64      `json:"monitor_id"`
	MonitorName string     `json:"monitor_name"`
	StartedAt   time.Time  `json:"started_at"`
	ResolvedAt  *time.Time `json:"resolved_at"` // 진행 중이면 null
	DurationSec int64      `json:"duration_sec"`
	Cause       string     `json:"cause,omitempty"`
}

// IncidentsResponse 는 GET /api/incidents 응답이다.
type IncidentsResponse struct {
	Incidents []Incident `json:"incidents"`
}

// ErrorResponse 는 실패 응답이다.
type ErrorResponse struct {
	Error string `json:"error"`
}
