import "server-only";
import { connection } from "next/server";
import type { HistoryResponse, IncidentsResponse, StatusResponse } from "./types";

const API_URL = process.env.UPCHECK_API_URL ?? "http://localhost:8484";

// 서버 렌더링이라 방문자 시간대는 알 수 없다. 운영자가 정한 시간대 하나로 보여준다.
export const TIMEZONE = process.env.UPCHECK_TIMEZONE ?? "Asia/Seoul";

export class ApiError extends Error {
  constructor(
    message: string,
    readonly status: number,
    options?: ErrorOptions,
  ) {
    super(message, options);
    this.name = "ApiError";
  }
}

async function getJSON<T>(path: string): Promise<T> {
  await connection();

  let res: Response;
  try {
    res = await fetch(`${API_URL}${path}`, {
      cache: "no-store",
      signal: AbortSignal.timeout(5000),
    });
  } catch (err) {
    throw new ApiError(`upcheck API 에 연결할 수 없습니다`, 503, { cause: err });
  }

  if (!res.ok) {
    let msg = `upcheck API 오류 (HTTP ${res.status})`;
    try {
      const body = (await res.json()) as { error?: string };
      if (body.error) msg = body.error;
    } catch {
      // 본문이 JSON 이 아니면 상태 코드만 전한다
    }
    throw new ApiError(msg, res.status);
  }
  return res.json() as Promise<T>;
}

export function getStatus() {
  return getJSON<StatusResponse>("/api/status");
}

export function getHistory(id: number, days = 90) {
  const q = new URLSearchParams({ days: String(days), tz: TIMEZONE });
  return getJSON<HistoryResponse>(`/api/monitors/${id}/history?${q}`);
}

export function getIncidents(limit = 10) {
  return getJSON<IncidentsResponse>(`/api/incidents?limit=${limit}`);
}
