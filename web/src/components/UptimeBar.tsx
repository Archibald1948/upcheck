"use client";

import { useCallback, useRef, useState } from "react";
import type { Day } from "@/lib/types";
import { LEVEL_LABEL, LEVEL_RANGE, uptimeLevel, type Level } from "@/lib/status";
import { formatDay, formatNumber, formatUptime } from "@/lib/format";
import { StatusIcon } from "./StatusIcon";
import styles from "./UptimeBar.module.css";

const MOBILE_DAYS = 30;

interface Props {
  days: Day[];
  name: string;
  /** 범례를 보여줄지. 목록에서는 맨 아래 한 번만 보여준다. */
  showLegend?: boolean;
}

/**
 * Statuspage 스타일 90일 업타임 바.
 *
 * - 칸마다 마우스를 올리거나 키보드로 옮겨가면 그 날의 값을 보여준다.
 * - 키보드는 칸 90개를 Tab 으로 하나씩 지나가게 하지 않는다. 바 전체가
 *   Tab 한 번이고, 안에서는 ←/→/Home/End 로 움직인다(roving tabindex).
 * - 툴팁이 유일한 통로가 아니다. 같은 값을 아래 표 보기에서도 읽을 수 있다.
 */
export function UptimeBar({ days, name, showLegend = false }: Props) {
  const [active, setActive] = useState<number | null>(null);
  const [focusIndex, setFocusIndex] = useState(days.length - 1);
  const cellRefs = useRef<(HTMLDivElement | null)[]>([]);

  // 좁은 화면에서 숨겨진 칸으로는 키보드 포커스가 가면 안 된다.
  const firstVisible = useCallback(() => {
    if (typeof window === "undefined") return 0;
    return window.matchMedia("(max-width: 640px)").matches ? Math.max(0, days.length - MOBILE_DAYS) : 0;
  }, [days.length]);

  const move = (to: number) => {
    const i = Math.min(days.length - 1, Math.max(firstVisible(), to));
    setFocusIndex(i);
    setActive(i);
    cellRefs.current[i]?.focus();
  };

  const onKeyDown = (e: React.KeyboardEvent) => {
    const i = focusIndex;
    const keys: Record<string, number> = {
      ArrowLeft: i - 1,
      ArrowRight: i + 1,
      Home: firstVisible(),
      End: days.length - 1,
    };
    if (e.key in keys) {
      e.preventDefault();
      move(keys[e.key]);
    } else if (e.key === "Escape") {
      setActive(null);
    }
  };

  const day = active !== null ? days[active] : null;

  return (
    <div>
      <div className={styles.wrap}>
        <div
          className={styles.bar}
          role="group"
          aria-label={`${name} 최근 ${days.length}일 가용성. 화살표 키로 날짜를 옮깁니다.`}
          onKeyDown={onKeyDown}
          onPointerLeave={() => setActive(null)}
        >
          {days.map((d, i) => {
            const level = uptimeLevel(d.uptime);
            const older = i < days.length - MOBILE_DAYS;
            return (
              <div
                key={d.date}
                ref={(el) => {
                  cellRefs.current[i] = el;
                }}
                className={[styles.cell, styles[level], older ? styles.older : "", active === i ? styles.active : ""].join(" ")}
                tabIndex={i === focusIndex ? 0 : -1}
                role="img"
                aria-label={`${formatDay(d.date)}: ${LEVEL_LABEL[level]}${d.uptime !== null ? `, 가용성 ${formatUptime(d.uptime)}` : ""}`}
                onPointerEnter={() => setActive(i)}
                onFocus={() => {
                  setFocusIndex(i);
                  setActive(i);
                }}
                onBlur={() => setActive(null)}
              />
            );
          })}
        </div>

        {day && active !== null && <Tooltip day={day} index={active} total={days.length} />}
      </div>

      <div className={styles.scale} aria-hidden>
        <span className={styles.long}>{days.length}일 전</span>
        <span className={styles.short}>{Math.min(MOBILE_DAYS, days.length)}일 전</span>
        <span>오늘</span>
      </div>

      {showLegend && <Legend />}
    </div>
  );
}

function Tooltip({ day, index, total }: { day: Day; index: number; total: number }) {
  const level = uptimeLevel(day.uptime);
  // 칸 위치를 따라가되 가장자리에서는 바깥으로 삐져나가지 않게 붙인다.
  const pct = ((index + 0.5) / total) * 100;
  const style: React.CSSProperties =
    pct < 20 ? { left: 0 } : pct > 80 ? { right: 0 } : { left: `${pct}%`, transform: "translateX(-50%)" };

  return (
    <div className={styles.tooltip} style={style} role="status">
      {/* 값이 먼저, 라벨이 뒤 — 읽는 사람은 이미 어느 칸인지 알고 숫자를 원한다 */}
      <div className={styles.tipValue}>
        <StatusIcon level={level} size={14} />
        {day.uptime !== null ? formatUptime(day.uptime) : "기록 없음"}
      </div>
      <div className={styles.tipLabel}>
        {LEVEL_LABEL[level]}
        {day.checks > 0 && ` · 체크 ${formatNumber(day.checks)}회${day.failed ? `, 실패 ${formatNumber(day.failed)}회` : ""}`}
      </div>
      <div className={styles.tipDate}>{formatDay(day.date)}</div>
    </div>
  );
}

export function Legend() {
  const levels: Level[] = ["good", "warning", "serious", "critical", "none"];
  return (
    <div className={styles.legend}>
      {levels.map((l) => (
        <span key={l} className={styles.legendItem}>
          <span className={`${styles.swatch} ${styles[l]}`} aria-hidden />
          {LEVEL_LABEL[l]}
          {LEVEL_RANGE[l] && <span className={styles.legendRange}>{LEVEL_RANGE[l]}</span>}
        </span>
      ))}
    </div>
  );
}
