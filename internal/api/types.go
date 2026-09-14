package api

import "time"

// 이 파일은 API 응답의 모양이다.
//
// 저장소 타입(store.MonitorStatus 등)을 그대로 JSON 으로 내보내지 않는다.
//   - 저장소 타입은 내부 사정에 따라 바뀐다. 그대로 내보내면 필드 이름 하나
//     바꿀 때마다 상태 페이지가 깨진다. API 모양은 따로 고정해야 한다.
//   - 공개하면 안 되는 필드(내부 호스트 주소 등)가 실수로 새어 나간다.

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

	// 포인터 필드는 JSON 에서 null 이 될 수 있다.
	// 한 번도 체크 안 한 모니터의 "마지막 체크 시각"은 없는 것이지
	// 0001-01-01 이 아니다.
	LastCheckedAt *time.Time `json:"last_checked_at"`

	Uptime  UptimeWindows  `json:"uptime"`
	Latency LatencySummary `json:"latency"`

	// 아래는 공개 여부를 선택할 수 있는 운영 정보다 (Options.ExposeDetails).
	// omitempty 라 비어 있으면 JSON 에서 아예 빠진다.
	Target    string `json:"target,omitempty"`
	LastError string `json:"last_error,omitempty"`
	Warning   string `json:"warning,omitempty"`
}

// UptimeWindows 는 기간별 업타임 백분율이다.
//
// *float64 인 이유: 데이터가 없는 기간은 null 이어야 한다.
// float64 로 두면 0 이 되는데, 0% 는 "완전히 죽어 있었다"는 뜻이라 거짓말이다.
type UptimeWindows struct {
	H24 *float64 `json:"24h"`
	D7  *float64 `json:"7d"`
	D30 *float64 `json:"30d"`
}

// LatencySummary 는 최근 24시간 응답시간이다. 단위는 밀리초.
//
// time.Duration 을 그대로 내보내지 않는다. encoding/json 은 Duration 을
// 나노초 정수로 찍는데, 받는 쪽(JavaScript)은 그게 나노초인지 알 길이 없다.
// 단위를 필드 이름(_ms)에 박아 두는 게 오해가 없다.
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
