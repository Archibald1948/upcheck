import Link from "next/link";
import type { Incident } from "@/lib/types";
import { formatDateTime, formatDuration } from "@/lib/format";
import { StatusIcon } from "./StatusIcon";
import styles from "./IncidentList.module.css";

interface Props {
  incidents: Incident[];
  timeZone: string;
  /** 모니터 상세 페이지에서는 모니터 이름이 반복되므로 숨긴다 */
  showMonitor?: boolean;
  emptyText?: string;
}

/** 장애 타임라인. 서버 컴포넌트라 시각 포맷을 여기서 끝낸다. */
export function IncidentList({ incidents, timeZone, showMonitor = true, emptyText = "기록된 장애가 없습니다." }: Props) {
  if (incidents.length === 0) {
    return <p className={styles.empty}>{emptyText}</p>;
  }

  return (
    <ol className={styles.list}>
      {incidents.map((inc) => {
        const ongoing = inc.resolved_at === null;
        return (
          <li key={inc.id} className={styles.item}>
            <span className={styles.icon}>
              <StatusIcon level={ongoing ? "critical" : "good"} size={16} />
            </span>
            <div>
              <div className={styles.title}>
                {showMonitor ? (
                  <>
                    <Link href={`/monitors/${inc.monitor_id}`}>{inc.monitor_name}</Link>
                    {ongoing ? <span className={styles.ongoing}>진행 중</span> : <span className="sr-only"> (해소됨)</span>}
                  </>
                ) : (
                  // 모니터 이름이 반복되지 않는 곳에서는 "장애"라고만 쓰면
                  // 초록 체크 아이콘과 뜻이 엇갈린다. 상태를 제목에 담는다.
                  <>
                    {ongoing ? "진행 중인 장애" : "해소된 장애"}
                    {ongoing && <span className={styles.ongoing}>진행 중</span>}
                  </>
                )}
              </div>
              <div className={styles.when}>
                <time dateTime={inc.started_at}>{formatDateTime(inc.started_at, timeZone)}</time>
                {inc.resolved_at && (
                  <>
                    {" → "}
                    <time dateTime={inc.resolved_at}>{formatDateTime(inc.resolved_at, timeZone)}</time>
                  </>
                )}
              </div>
            </div>
            <span className={styles.duration}>{formatDuration(inc.duration_sec)}</span>
            {inc.cause && <div className={styles.cause}>{inc.cause}</div>}
          </li>
        );
      })}
    </ol>
  );
}
