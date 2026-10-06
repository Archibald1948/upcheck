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
	Type       string
	CheckedAt  time.Time
	OK         bool
	StatusCode int
	LatencyMS  int64
	Error      string

	// Warning 은 실패는 아니지만 알아둘 것이다 (tls 인증서 만료 임박 등).
	Warning string
}

// SyncMonitors 는 설정 파일의 모니터를 DB에 반영하고 이름→id 맵을 돌려준다.
// 설정에서 사라진 모니터는 CASCADE 로 기록이 날아가지 않게 enabled=0 으로만 바꾼다.
func (s *Store) SyncMonitors(ctx context.Context, monitors []config.Monitor) (map[string]int64, error) {
	ids := make(map[string]int64, len(monitors))

	err := s.tx(ctx, func(tx *sql.Tx) error {
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
		defer stmt.Close()

		now := unix(time.Now())
		for _, m := range monitors {
			var id int64
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
func (s *Store) InsertChecks(ctx context.Context, rows []CheckRow) error {
	if len(rows) == 0 {
		return nil
	}

	return s.tx(ctx, func(tx *sql.Tx) error {
		stmt, err := tx.PrepareContext(ctx, `
			INSERT INTO checks (monitor_id, type, checked_at, ok, status_code, latency_ms, error, warning)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`)
		if err != nil {
			return fmt.Errorf("체크 삽입 준비 실패: %w", err)
		}
		defer stmt.Close()

		for _, r := range rows {
			okVal := 0
			if r.OK {
				okVal = 1
			}
			typ := r.Type
			if typ == "" {
				typ = "http"
			}
			if _, err := stmt.ExecContext(ctx,
				r.MonitorID, typ, unix(r.CheckedAt), okVal, r.StatusCode, r.LatencyMS, r.Error, r.Warning,
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
