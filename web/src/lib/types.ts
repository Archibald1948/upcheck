// internal/api/types.go 와 맞춰야 한다.

export type Overall = "operational" | "degraded" | "outage" | "unknown";
export type MonitorState = "up" | "down" | "unknown";

export interface StatusResponse {
  generated_at: string;
  overall: Overall;
  monitors: MonitorStatus[];
}

export interface MonitorStatus {
  id: number;
  name: string;
  type?: string;
  status: MonitorState;
  last_checked_at: string | null;
  uptime: { "24h": number | null; "7d": number | null; "30d": number | null };
  latency: { p50_ms: number | null; p95_ms: number | null; approx: boolean };
  // 아래는 upcheck -expose-details 일 때만 온다
  target?: string;
  last_error?: string;
  warning?: string;
}

export interface Day {
  date: string; // "2026-09-14"
  uptime: number | null; // 기록이 없으면 null — 0 과 다르다
  checks: number;
  failed: number;
  p50_ms: number | null;
  p95_ms: number | null;
}

export interface Incident {
  id: number;
  monitor_id: number;
  monitor_name: string;
  started_at: string;
  resolved_at: string | null; // 진행 중이면 null
  duration_sec: number;
  cause?: string;
}

export interface HistoryResponse {
  monitor: { id: number; name: string };
  timezone: string;
  days: Day[];
  incidents: Incident[];
}

export interface IncidentsResponse {
  incidents: Incident[];
}
