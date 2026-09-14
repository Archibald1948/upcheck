import "server-only";
import { connection } from "next/server";
import type { HistoryResponse, IncidentsResponse, StatusResponse } from "./types";

// Go API 주소. 브라우저가 아니라 Next.js 서버만 이 주소를 부른다.
//
// NEXT_PUBLIC_ 을 붙이지 않은 이유:
//   - 붙이면 빌드 시점 값이 JS 번들에 박힌다. 도커 이미지 하나를 여러 환경에
//     배포할 때 주소를 바꿀 수 없다 (M6).
//   - 브라우저가 Go API 를 직접 부를 일이 없으니 공개할 이유도 없다.
//     Go API 는 내부망에만 열어 두면 된다. CORS 설정도 필요 없다.
const API_URL = process.env.UPCHECK_API_URL ?? "http://localhost:8484";

// 날짜를 자르는 기준 시간대. 서버에서 렌더링하므로 방문자 브라우저의
// 시간대는 알 수 없다. 상태 페이지는 운영자가 정한 시간대 하나로 보여준다.
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
  // connection() 은 "이 렌더링은 실제 요청이 올 때 해라"는 표시다.
  //
  // 이게 없으면 Next.js 가 `next build` 때 이 페이지를 미리 렌더링해 버린다.
  // 그러면 상태 페이지가 빌드한 순간의 상태로 굳고, 빌드할 때 Go API 가
  // 안 떠 있으면 빌드 자체가 실패한다.
  await connection();

  let res: Response;
  try {
    res = await fetch(`${API_URL}${path}`, {
      cache: "no-store",
      // Go API 가 멈춰 있으면 페이지 전체가 같이 멈춘다. 시한을 건다.
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
