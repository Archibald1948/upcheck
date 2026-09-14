"use client";

import { useEffect, useMemo, useRef, useState } from "react";
import type { Day } from "@/lib/types";
import { formatDay, formatMs, formatShortDay } from "@/lib/format";
import styles from "./ResponseTimeChart.module.css";

// p50 과 p95 는 단위(ms)가 같아서 축 하나에 둔다. 축을 두 개 두지 않는다.
const SERIES = [
  { key: "p50_ms", label: "p50 (중앙값)", color: "var(--series-1)" },
  { key: "p95_ms", label: "p95", color: "var(--series-2)" },
] as const;

type SeriesKey = (typeof SERIES)[number]["key"];

const HEIGHT = 244;
const M = { top: 12, right: 88, bottom: 28, left: 44 }; // 오른쪽은 끝 라벨 자리

/**
 * 일별 응답시간 선 차트.
 *
 * 롤업된 날의 백분위수는 시간별 값을 가중평균한 근사치다(서버 docs/10 참고).
 * 추세를 보기엔 충분하지만 SLA 계산에 쓸 숫자는 아니다.
 */
export function ResponseTimeChart({ days }: { days: Day[] }) {
  const frameRef = useRef<HTMLDivElement>(null);
  const [width, setWidth] = useState(0);
  const [active, setActive] = useState<number | null>(null);

  // SVG 를 CSS 로 늘리면 글자까지 늘어난다. 실제 폭을 재서 그 폭으로 그린다.
  useEffect(() => {
    const el = frameRef.current;
    if (!el) return;
    const ro = new ResizeObserver(([entry]) => setWidth(Math.floor(entry.contentRect.width)));
    ro.observe(el);
    return () => ro.disconnect();
  }, []);

  const geo = useMemo(() => layout(days, width), [days, width]);
  const hasData = days.some((d) => d.p95_ms !== null);

  const indexAt = (clientX: number) => {
    const rect = frameRef.current!.getBoundingClientRect();
    const x = clientX - rect.left - M.left;
    const i = Math.round((x / geo.plotW) * (days.length - 1));
    return Math.min(days.length - 1, Math.max(0, i));
  };

  const onKeyDown = (e: React.KeyboardEvent) => {
    const cur = active ?? days.length - 1;
    const next = { ArrowLeft: cur - 1, ArrowRight: cur + 1, Home: 0, End: days.length - 1 }[e.key];
    if (next !== undefined) {
      e.preventDefault();
      setActive(Math.min(days.length - 1, Math.max(0, next)));
    } else if (e.key === "Escape") {
      setActive(null);
    }
  };

  return (
    <div>
      {/* 시리즈가 둘 이상이면 범례는 항상 있다. 색만으로 구분하게 두지 않는다. */}
      <div className={styles.legend}>
        {SERIES.map((s) => (
          <span key={s.key} className={styles.legendItem}>
            <span className={styles.lineKey} style={{ background: s.color }} aria-hidden />
            {s.label}
          </span>
        ))}
      </div>

      <div
        ref={frameRef}
        className={styles.frame}
        tabIndex={hasData ? 0 : -1}
        role="img"
        aria-label={`일별 응답시간 차트. 화살표 키로 날짜를 옮깁니다. 같은 값은 아래 표 보기에서도 볼 수 있습니다.`}
        onPointerMove={(e) => hasData && width && setActive(indexAt(e.clientX))}
        onPointerLeave={() => setActive(null)}
        onKeyDown={onKeyDown}
        onBlur={() => setActive(null)}
      >
        {!hasData ? (
          <div className={styles.empty}>응답시간 기록이 없습니다</div>
        ) : (
          width > 0 && (
            <>
              <svg width={width} height={HEIGHT} aria-hidden>
                {/* 격자: 1px 실선, 표면에서 한 단계만 벗어난 회색 */}
                {geo.ticks.map((t) => (
                  <g key={t}>
                    <line x1={M.left} x2={M.left + geo.plotW} y1={geo.y(t)} y2={geo.y(t)} stroke={t === 0 ? "var(--axis)" : "var(--grid)"} strokeWidth={1} shapeRendering="crispEdges" />
                    <text x={M.left - 8} y={geo.y(t)} dy="0.32em" textAnchor="end" className={styles.tick}>
                      {t >= 1000 ? `${t / 1000}s` : t}
                    </text>
                  </g>
                ))}

                {geo.xTicks.map((i) => (
                  <text key={i} x={geo.x(i)} y={HEIGHT - 8} textAnchor="middle" className={styles.tick}>
                    {formatShortDay(days[i].date)}
                  </text>
                ))}

                {/* 크로스헤어: 포인터가 선에 닿지 않아도 날짜를 잡는다 */}
                {active !== null && (
                  <line x1={geo.x(active)} x2={geo.x(active)} y1={M.top} y2={M.top + geo.plotH} stroke="var(--axis)" strokeWidth={1} shapeRendering="crispEdges" />
                )}

                {SERIES.map((s) => (
                  <path key={s.key} d={geo.paths[s.key]} fill="none" stroke={s.color} strokeWidth={2} strokeLinejoin="round" strokeLinecap="round" />
                ))}

                {/* 끝점 표시 + 직접 라벨. 두 라벨이 겹칠 만큼 가까우면 라벨은 빼고 범례·툴팁에 맡긴다. */}
                {geo.ends.map((e) => (
                  <g key={e.key}>
                    <circle cx={e.x} cy={e.y} r={4} fill={e.color} stroke="var(--surface)" strokeWidth={2} />
                    {geo.showEndLabels && (
                      <text x={e.x + 10} y={e.y} dy="0.32em" className={styles.endLabel}>
                        {e.short} {formatMs(e.value)}
                      </text>
                    )}
                  </g>
                ))}

                {active !== null &&
                  SERIES.map((s) => {
                    const v = days[active][s.key];
                    return v === null ? null : (
                      <circle key={s.key} cx={geo.x(active)} cy={geo.y(v)} r={4} fill={s.color} stroke="var(--surface)" strokeWidth={2} />
                    );
                  })}
              </svg>

              {active !== null && <Tooltip day={days[active]} x={geo.x(active)} width={width} />}
            </>
          )
        )}
      </div>

      <TableView days={days} />
    </div>
  );
}

function Tooltip({ day, x, width }: { day: Day; x: number; width: number }) {
  // 크로스헤어 오른쪽에 붙이되, 오른쪽 끝에서는 왼쪽으로 넘긴다.
  const style: React.CSSProperties = x > width * 0.6 ? { right: width - x + 12 } : { left: x + 12 };
  return (
    <div className={styles.tooltip} style={style}>
      {/* 한 툴팁에 모든 시리즈. 값이 먼저, 이름이 뒤. */}
      {[...SERIES].reverse().map((s) => (
        <div key={s.key} className={styles.tipRow}>
          <span className={styles.lineKey} style={{ background: s.color }} aria-hidden />
          <span className={styles.tipName}>{s.label}</span>
          <span className={styles.tipValue}>{formatMs(day[s.key])}</span>
        </div>
      ))}
      <div className={styles.tipDate}>{formatDay(day.date)}</div>
    </div>
  );
}

function TableView({ days }: { days: Day[] }) {
  const rows = days.filter((d) => d.p50_ms !== null).reverse();
  if (rows.length === 0) return null;
  return (
    <details className="table-view">
      <summary>표로 보기 ({rows.length}일)</summary>
      <div className="table-scroll">
        <table>
          <thead>
            <tr>
              <th scope="col">날짜</th>
              <th scope="col">p50</th>
              <th scope="col">p95</th>
            </tr>
          </thead>
          <tbody>
            {rows.map((d) => (
              <tr key={d.date}>
                <td>{formatDay(d.date)}</td>
                <td>{formatMs(d.p50_ms)}</td>
                <td>{formatMs(d.p95_ms)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </details>
  );
}

// ─────────────── 좌표 계산 ───────────────

function layout(days: Day[], width: number) {
  const plotW = Math.max(0, width - M.left - M.right);
  const plotH = HEIGHT - M.top - M.bottom;

  let maxV = 0;
  for (const d of days) maxV = Math.max(maxV, d.p95_ms ?? 0, d.p50_ms ?? 0);
  const ticks = niceTicks(maxV);
  const top = ticks[ticks.length - 1] || 1;

  const n = Math.max(1, days.length - 1);
  const x = (i: number) => M.left + (i / n) * plotW;
  const y = (v: number) => M.top + plotH - (v / top) * plotH;

  // 기록이 없는 날에서는 선을 끊는다. 이어 그리면 없는 값을 지어내는 셈이다.
  const paths = {} as Record<SeriesKey, string>;
  for (const s of SERIES) {
    let d = "";
    let pen = false;
    days.forEach((day, i) => {
      const v = day[s.key];
      if (v === null) {
        pen = false;
        return;
      }
      d += `${pen ? "L" : "M"}${x(i).toFixed(1)},${y(v).toFixed(1)}`;
      pen = true;
    });
    paths[s.key] = d;
  }

  const ends = SERIES.flatMap((s) => {
    for (let i = days.length - 1; i >= 0; i--) {
      const v = days[i][s.key];
      if (v !== null) return [{ key: s.key, color: s.color, short: s.key === "p50_ms" ? "p50" : "p95", value: v, x: x(i), y: y(v) }];
    }
    return [];
  });
  // 두 끝 라벨이 14px 안으로 붙으면 겹친다. 억지로 위아래로 밀면 선과 떨어져 오히려 헷갈린다.
  const showEndLabels = ends.length < 2 || Math.abs(ends[0].y - ends[1].y) >= 14;

  // x축 날짜 라벨은 폭에 맞춰 4~6개만
  const count = Math.max(2, Math.min(6, Math.floor(plotW / 110)));
  const xTicks = Array.from({ length: count }, (_, k) => Math.round((k / (count - 1)) * (days.length - 1)));

  return { plotW, plotH, ticks, x, y, paths, ends, showEndLabels, xTicks };
}

/** 0 부터 시작하는 깔끔한 눈금 (0, 100, 200 … / 0, 250, 500 …) */
function niceTicks(max: number): number[] {
  if (max <= 0) return [0, 100];
  const rough = max / 4;
  const pow = 10 ** Math.floor(Math.log10(rough));
  const step = [1, 2, 2.5, 5, 10].map((m) => m * pow).find((s) => s >= rough) ?? pow * 10;
  const out: number[] = [];
  for (let v = 0; v < max + step; v += step) out.push(v);
  return out;
}
