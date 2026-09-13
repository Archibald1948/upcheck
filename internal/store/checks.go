package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/Archibald1948/upcheck/internal/config"
)

// CheckRow 는 checks 테이블에 넣을 한 행이다.
type CheckRow struct {
	MonitorID  int64
	CheckedAt  time.Time
	OK         bool
	StatusCode int
	LatencyMS  int64
	Error      string
}

// SyncMonitors 는 설정 파일의 모니터를 DB에 반영하고 이름→id 맵을 돌려준다.
//
// 설정이 원본(source of truth)이고 DB는 그걸 따라간다.
// 설정에서 사라진 모니터는 지우지 않고 enabled=0 으로만 바꾼다.
// 지워버리면 ON DELETE CASCADE 로 과거 체크 기록까지 날아가기 때문이다.
func (s *Store) SyncMonitors(ctx context.Context, monitors []config.Monitor) (map[string]int64, error) {
	ids := make(map[string]int64, len(monitors))

	err := s.tx(ctx, func(tx *sql.Tx) error {
		// ON CONFLICT ... DO UPDATE 는 SQLite 의 upsert 문법이다.
		// name 에 UNIQUE 제약이 있어서 충돌 시 갱신으로 넘어간다.
		// created_at 은 갱신하지 않는다 — 처음 등록된 시각을 유지한다.
		stmt, err := tx.PrepareContext(ctx, `
			INSERT INTO monitors
				(name, type, target, interval_sec, timeout_ms, expected_status, keyword, enabled, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, 1, ?)
			ON CONFLICT(name) DO UPDATE SET
				type            = excluded.type,
				target          = excluded.target,
				interval_sec    = excluded.interval_sec,
				timeout_ms      = excluded.timeout_ms,
				expected_status = excluded.expected_status,
				keyword         = excluded.keyword,
				enabled         = 1
			RETURNING id`)
		if err != nil {
			return fmt.Errorf("모니터 upsert 준비 실패: %w", err)
		}
		// Prepare 한 문장은 반드시 닫아야 한다. 안 닫으면 커서가 남는다.
		defer stmt.Close()

		now := unix(time.Now())
		for _, m := range monitors {
			var id int64
			// RETURNING 이 있으므로 QueryRow 로 받는다.
			err := stmt.QueryRowContext(ctx,
				m.Name, m.Type, m.Target, m.IntervalSec, m.TimeoutMS,
				m.ExpectedStatus, m.Keyword, now,
			).Scan(&id)
			if err != nil {
				return fmt.Errorf("모니터 %q 저장 실패: %w", m.Name, err)
			}
			ids[m.Name] = id
		}

		// 설정에 없는 모니터는 비활성으로 내린다.
		// 이름 목록을 IN 절에 넣으려면 플레이스홀더를 개수만큼 만들어야 한다.
		if len(monitors) > 0 {
			names := make([]any, 0, len(monitors))
			placeholders := make([]byte, 0, len(monitors)*2)
			for i, m := range monitors {
				names = append(names, m.Name)
				if i > 0 {
					placeholders = append(placeholders, ',')
				}
				placeholders = append(placeholders, '?')
			}
			q := "UPDATE monitors SET enabled = 0 WHERE name NOT IN (" + string(placeholders) + ")"
			if _, err := tx.ExecContext(ctx, q, names...); err != nil {
				return fmt.Errorf("비활성 처리 실패: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return ids, nil
}

// InsertChecks 는 체크 결과를 한 트랜잭션으로 몰아 넣는다.
//
// 한 행마다 따로 넣으면 트랜잭션마다 디스크 동기화가 일어나 아주 느리다.
// 묶어서 넣으면 동기화가 한 번이다. 배치 크기가 커질수록 행당 비용이 준다.
func (s *Store) InsertChecks(ctx context.Context, rows []CheckRow) error {
	if len(rows) == 0 {
		return nil
	}

	return s.tx(ctx, func(tx *sql.Tx) error {
		// 같은 SQL 을 여러 번 실행할 때는 Prepare 가 이득이다.
		// SQL 파싱과 실행 계획 수립을 한 번만 한다.
		stmt, err := tx.PrepareContext(ctx, `
			INSERT INTO checks (monitor_id, checked_at, ok, status_code, latency_ms, error)
			VALUES (?, ?, ?, ?, ?, ?)`)
		if err != nil {
			return fmt.Errorf("체크 삽입 준비 실패: %w", err)
		}
		defer stmt.Close()

		for _, r := range rows {
			// SQLite 에는 BOOLEAN 이 없다. 0/1 정수로 저장한다.
			// Go의 bool 을 그대로 넘겨도 드라이버가 바꿔주지만,
			// 스키마가 INTEGER 라는 걸 드러내려고 명시적으로 변환한다.
			okVal := 0
			if r.OK {
				okVal = 1
			}
			if _, err := stmt.ExecContext(ctx,
				r.MonitorID, unix(r.CheckedAt), okVal, r.StatusCode, r.LatencyMS, r.Error,
			); err != nil {
				return fmt.Errorf("체크 저장 실패(monitor_id=%d): %w", r.MonitorID, err)
			}
		}
		return nil
	})
}

// CountChecks 는 저장된 체크 행 수를 센다. 주로 테스트와 운영 확인용이다.
func (s *Store) CountChecks(ctx context.Context) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM checks`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("체크 개수 조회 실패: %w", err)
	}
	return n, nil
}
