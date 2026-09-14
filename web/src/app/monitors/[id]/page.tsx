import Link from "next/link";
import { notFound } from "next/navigation";
import { ApiError, getHistory, getStatus, TIMEZONE } from "@/lib/api";
import { periodUptime, STATE_LABEL, STATE_LEVEL } from "@/lib/status";
import { formatDateTime, formatMs, formatUptime } from "@/lib/format";
import { StatusIcon } from "@/components/StatusIcon";
import { Legend, UptimeBar } from "@/components/UptimeBar";
import { ResponseTimeChart } from "@/components/ResponseTimeChart";
import { IncidentList } from "@/components/IncidentList";
import { AutoRefresh } from "@/components/AutoRefresh";
import styles from "./page.module.css";

export default async function MonitorPage({ params }: { params: Promise<{ id: string }> }) {
  // Next.js 15 부터 params 는 Promise 다. 동기적으로 꺼내면 안 된다.
  const { id: raw } = await params;
  const id = Number(raw);
  if (!Number.isInteger(id) || id <= 0) notFound();

  const [status, history] = await Promise.all([
    getStatus(),
    getHistory(id, 90).catch((err) => {
      // 없는 모니터는 에러 페이지가 아니라 404 로 보여준다
      if (err instanceof ApiError && err.status === 404) return null;
      throw err;
    }),
  ]);
  if (!history) notFound();

  const m = status.monitors.find((x) => x.id === id);
  if (!m) notFound(); // 비활성화된 모니터는 /api/status 에 없다

  const level = STATE_LEVEL[m.status];
  const days = history.days;

  const tiles = [
    { label: "24시간 가용성", value: formatUptime(m.uptime["24h"]) },
    { label: "7일 가용성", value: formatUptime(m.uptime["7d"]) },
    { label: "90일 가용성", value: formatUptime(periodUptime(days)) },
    { label: "24시간 p50", value: formatMs(m.latency.p50_ms) },
    { label: "24시간 p95", value: formatMs(m.latency.p95_ms) },
  ];

  return (
    <div>
      <AutoRefresh seconds={60} />

      <Link href="/" className={styles.back}>
        ← 전체 상태
      </Link>

      <div className={styles.head}>
        <h1 className={styles.name}>{m.name}</h1>
        <span className={styles.state}>
          <StatusIcon level={level} />
          {STATE_LABEL[m.status]}
        </span>
      </div>
      <div className={styles.sub}>
        {m.type && `${m.type.toUpperCase()} · `}
        {m.last_checked_at ? `마지막 확인 ${formatDateTime(m.last_checked_at, TIMEZONE)}` : "아직 확인 전"}
      </div>

      {m.warning && (
        <div className={styles.warning} role="note">
          <StatusIcon level="warning" />
          <span>{m.warning}</span>
        </div>
      )}

      <div className={styles.tiles}>
        {tiles.map((t) => (
          <div key={t.label} className={styles.tile}>
            <div className={styles.tileLabel}>{t.label}</div>
            <div className={styles.tileValue}>{t.value}</div>
          </div>
        ))}
      </div>
      {m.latency.approx && <p className={styles.note}>※ 24시간 응답시간 중 일부는 시간별 롤업에서 근사한 값입니다.</p>}

      <h2 className="section-title">
        일별 가용성 <span className="muted" style={{ fontWeight: 400, fontSize: 13 }}>· {history.timezone} 기준</span>
      </h2>
      <div className="card">
        <UptimeBar days={days} name={m.name} />
        <Legend />
        <DayTable days={days} />
      </div>

      <h2 className="section-title">일별 응답시간</h2>
      <div className="card">
        <ResponseTimeChart days={days} />
        <p className={styles.note}>
          하루 값은 시간별 백분위수를 체크 수로 가중평균한 근사치입니다. 추세를 보는 용도입니다.
        </p>
      </div>

      <h2 className="section-title">장애 이력</h2>
      <div className="card" style={{ paddingBlock: 6 }}>
        <IncidentList incidents={history.incidents} timeZone={TIMEZONE} showMonitor={false} emptyText="최근 90일 동안 장애가 없었습니다." />
      </div>
    </div>
  );
}

/** 업타임 바의 표 쌍둥이. 100% 가 아니었던 날만 모아 보여준다. */
function DayTable({ days }: { days: import("@/lib/types").Day[] }) {
  const rows = days.filter((d) => d.uptime !== null && d.uptime < 100).reverse();
  return (
    <details className="table-view">
      <summary>100% 가 아니었던 날 ({rows.length}일)</summary>
      {rows.length === 0 ? (
        <p className="muted">기록이 있는 모든 날이 100% 였습니다.</p>
      ) : (
        <div className="table-scroll">
          <table>
            <thead>
              <tr>
                <th scope="col">날짜</th>
                <th scope="col">가용성</th>
                <th scope="col">체크</th>
                <th scope="col">실패</th>
              </tr>
            </thead>
            <tbody>
              {rows.map((d) => (
                <tr key={d.date}>
                  <td>{d.date}</td>
                  <td>{formatUptime(d.uptime)}</td>
                  <td>{d.checks.toLocaleString("ko-KR")}</td>
                  <td>{d.failed.toLocaleString("ko-KR")}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </details>
  );
}
