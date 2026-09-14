import type { MonitorState, Overall } from "./types";

// 업타임 칸 하나의 등급.
// 색은 dataviz 상태 팔레트(good/warning/serious/critical)를 쓰고,
// 색만으로 뜻을 전하지 않도록 항상 라벨과 함께 보여준다.
export type Level = "good" | "warning" | "serious" | "critical" | "none";

export function uptimeLevel(uptime: number | null): Level {
  if (uptime === null) return "none";
  if (uptime >= 99.9) return "good";
  if (uptime >= 99) return "warning";
  if (uptime >= 95) return "serious";
  return "critical";
}

export const LEVEL_LABEL: Record<Level, string> = {
  good: "정상",
  warning: "경미한 실패",
  serious: "부분 장애",
  critical: "주요 장애",
  none: "기록 없음",
};

// 범례에 쓸 설명. 기준값을 숨기지 않는다.
export const LEVEL_RANGE: Record<Level, string> = {
  good: "99.9% 이상",
  warning: "99% 이상",
  serious: "95% 이상",
  critical: "95% 미만",
  none: "",
};

export const STATE_LABEL: Record<MonitorState, string> = {
  up: "정상",
  down: "장애",
  unknown: "확인 전",
};

export const STATE_LEVEL: Record<MonitorState, Level> = {
  up: "good",
  down: "critical",
  unknown: "none",
};

export const OVERALL: Record<Overall, { label: string; level: Level }> = {
  operational: { label: "모든 시스템이 정상 작동 중입니다", level: "good" },
  degraded: { label: "일부 시스템에 장애가 있습니다", level: "serious" },
  outage: { label: "전체 시스템 장애", level: "critical" },
  unknown: { label: "상태를 확인하는 중입니다", level: "none" },
};

/**
 * 기간 전체 가용성. 칸별 백분율을 평균내지 않고 체크 수로 다시 계산한다.
 * 체크가 10번뿐인 날과 288번인 날을 같은 무게로 섞으면 틀린 값이 나온다.
 */
export function periodUptime(days: { checks: number; failed: number }[]): number | null {
  let checks = 0;
  let failed = 0;
  for (const d of days) {
    checks += d.checks;
    failed += d.failed;
  }
  if (checks === 0) return null;
  return Math.floor(((checks - failed) / checks) * 10000) / 100;
}
