import Link from "next/link";
import { getHistory, getIncidents, getStatus, TIMEZONE } from "@/lib/api";
import { OVERALL, periodUptime, STATE_LABEL, STATE_LEVEL } from "@/lib/status";
import { formatDateTime, formatUptime } from "@/lib/format";
import { StatusIcon } from "@/components/StatusIcon";
import { Legend, UptimeBar } from "@/components/UptimeBar";
import { IncidentList } from "@/components/IncidentList";
import { AutoRefresh } from "@/components/AutoRefresh";
import styles from "./page.module.css";

export default async function StatusPage() {
  const [status, incidents] = await Promise.all([getStatus(), getIncidents(8)]);

  // 모니터 수만큼 요청이 나간다.
  const histories = await Promise.all(status.monitors.map((m) => getHistory(m.id, 90)));

  const overall = OVERALL[status.overall];

  return (
    <div>
      <AutoRefresh seconds={60} />

      <div className={styles.banner} data-level={overall.level} role="status">
        <StatusIcon level={overall.level} size={20} />
        {overall.label}
        <span className={styles.updated}>
          {formatDateTime(status.generated_at, TIMEZONE)} 기준
        </span>
      </div>

      <h2 className="section-title">
        일별 가용성 <span className="muted" style={{ fontWeight: 400, fontSize: 13 }}>· {TIMEZONE} 기준</span>
      </h2>

      <div className={`card ${styles.monitors}`}>
        {status.monitors.length === 0 && <p className="muted">등록된 모니터가 없습니다.</p>}

        {status.monitors.map((m, i) => {
          const days = histories[i].days;
          const level = STATE_LEVEL[m.status];
          return (
            <section key={m.id} className={styles.monitor} aria-labelledby={`m-${m.id}`}>
              <div className={styles.monitorHead}>
                <StatusIcon level={level} />
                <Link id={`m-${m.id}`} href={`/monitors/${m.id}`} className={styles.monitorName}>
                  {m.name}
                </Link>
                <span className={styles.state}>{STATE_LABEL[m.status]}</span>
                {/* 좁은 화면에선 바가 30일만 보여서 기간을 같이 적는다 */}
                <span className={styles.periodUptime}>
                  <span className="muted">{days.length}일</span> {formatUptime(periodUptime(days))}
                </span>
              </div>
              <UptimeBar days={days} name={m.name} />
            </section>
          );
        })}

        {status.monitors.length > 0 && (
          <div className={styles.legendRow}>
            <Legend />
          </div>
        )}
      </div>

      <h2 className="section-title">최근 장애</h2>
      <div className="card" style={{ paddingBlock: 6 }}>
        <IncidentList incidents={incidents.incidents} timeZone={TIMEZONE} />
      </div>
    </div>
  );
}
