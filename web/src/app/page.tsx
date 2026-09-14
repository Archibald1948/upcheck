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
  // 두 요청은 서로 기다릴 이유가 없다. 동시에 보낸다.
  const [status, incidents] = await Promise.all([getStatus(), getIncidents(8)]);

  // 모니터마다 90일 이력. 역시 동시에.
  // (모니터가 수십 개가 되면 요청이 그만큼 늘어난다. 그때는 여러 모니터를
  //  한 번에 주는 엔드포인트를 Go 쪽에 추가하는 게 맞다.)
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
                {/* 기간을 반드시 적는다. 좁은 화면에서는 바가 30일만 보이는데
                    숫자만 있으면 30일 값으로 읽힌다. */}
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
