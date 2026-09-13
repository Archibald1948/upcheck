package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Incident 는 장애 구간 하나다.
type Incident struct {
	ID         int64
	MonitorID  int64
	StartedAt  time.Time
	ResolvedAt *time.Time // nil = 아직 진행 중
	Cause      string
}

// Duration 은 장애가 지속된 시간이다. 진행 중이면 now 까지로 계산한다.
func (i Incident) Duration(now time.Time) time.Duration {
	if i.ResolvedAt != nil {
		return i.ResolvedAt.Sub(i.StartedAt)
	}
	return now.Sub(i.StartedAt)
}

// OpenIncident 는 장애 시작을 기록하고 id 를 돌려준다.
//
// 이미 열려 있는 장애가 있으면 새로 만들지 않고 그 id 를 준다.
// 재시작 직후처럼 메모리 상태가 비어 있을 때 중복 생성을 막는다.
func (s *Store) OpenIncident(ctx context.Context, monitorID int64, at time.Time, cause string) (int64, error) {
	if open, err := s.OpenIncidentFor(ctx, monitorID); err != nil {
		return 0, err
	} else if open != nil {
		return open.ID, nil
	}

	var id int64
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO incidents (monitor_id, started_at, resolved_at, cause)
		VALUES (?, ?, NULL, ?)
		RETURNING id`, monitorID, unix(at), cause).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("장애 기록 실패: %w", err)
	}
	return id, nil
}

// ResolveIncident 는 열려 있는 장애를 닫는다.
//
// 이미 닫혔거나 없으면 아무 일도 하지 않는다 (resolved_at IS NULL 조건).
// 같은 복구를 두 번 처리해도 시각이 덮어써지지 않는다.
func (s *Store) ResolveIncident(ctx context.Context, monitorID int64, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE incidents SET resolved_at = ?
		WHERE monitor_id = ? AND resolved_at IS NULL`, unix(at), monitorID)
	if err != nil {
		return fmt.Errorf("장애 해소 기록 실패: %w", err)
	}
	return nil
}

// OpenIncidentFor 는 모니터의 진행 중인 장애를 돌려준다. 없으면 (nil, nil).
//
// 프로그램 재시작 시 "이미 알림을 보낸 장애"를 복원하는 데 쓴다.
// 이게 없으면 재시작할 때마다 같은 장애로 알림이 다시 간다.
func (s *Store) OpenIncidentFor(ctx context.Context, monitorID int64) (*Incident, error) {
	var (
		inc       Incident
		startedAt int64
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT id, monitor_id, started_at, cause
		FROM incidents
		WHERE monitor_id = ? AND resolved_at IS NULL
		ORDER BY started_at DESC LIMIT 1`, monitorID).
		Scan(&inc.ID, &inc.MonitorID, &startedAt, &inc.Cause)

	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("진행 중 장애 조회 실패: %w", err)
	}
	inc.StartedAt = fromUnix(startedAt)
	return &inc, nil
}

// Incidents 는 모니터의 장애 이력을 최근 순으로 돌려준다.
func (s *Store) Incidents(ctx context.Context, monitorID int64, limit int) ([]Incident, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, monitor_id, started_at, resolved_at, cause
		FROM incidents
		WHERE monitor_id = ?
		ORDER BY started_at DESC
		LIMIT ?`, monitorID, limit)
	if err != nil {
		return nil, fmt.Errorf("장애 이력 조회 실패: %w", err)
	}
	defer rows.Close()

	var out []Incident
	for rows.Next() {
		var (
			inc        Incident
			startedAt  int64
			resolvedAt sql.NullInt64 // NULL 이 올 수 있으므로 Null 타입으로 받는다
		)
		if err := rows.Scan(&inc.ID, &inc.MonitorID, &startedAt, &resolvedAt, &inc.Cause); err != nil {
			return nil, fmt.Errorf("장애 행 읽기 실패: %w", err)
		}
		inc.StartedAt = fromUnix(startedAt)
		if resolvedAt.Valid {
			t := fromUnix(resolvedAt.Int64)
			inc.ResolvedAt = &t
		}
		out = append(out, inc)
	}
	return out, rows.Err()
}

// RecentIncidents 는 전체 모니터의 최근 장애를 모아 온다.
type IncidentWithMonitor struct {
	Incident
	MonitorName string
}

func (s *Store) RecentIncidents(ctx context.Context, limit int) ([]IncidentWithMonitor, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT i.id, i.monitor_id, i.started_at, i.resolved_at, i.cause, m.name
		FROM incidents i
		JOIN monitors m ON m.id = i.monitor_id
		ORDER BY i.started_at DESC
		LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("최근 장애 조회 실패: %w", err)
	}
	defer rows.Close()

	var out []IncidentWithMonitor
	for rows.Next() {
		var (
			iw         IncidentWithMonitor
			startedAt  int64
			resolvedAt sql.NullInt64
		)
		if err := rows.Scan(&iw.ID, &iw.MonitorID, &startedAt, &resolvedAt, &iw.Cause, &iw.MonitorName); err != nil {
			return nil, fmt.Errorf("장애 행 읽기 실패: %w", err)
		}
		iw.StartedAt = fromUnix(startedAt)
		if resolvedAt.Valid {
			t := fromUnix(resolvedAt.Int64)
			iw.ResolvedAt = &t
		}
		out = append(out, iw)
	}
	return out, rows.Err()
}
