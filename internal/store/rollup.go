package store

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"time"
)

// Retention 은 데이터를 얼마나 보관할지 정한다.
type Retention struct {
	Raw    time.Duration // checks(원본)를 얼마나 둘지
	Hourly time.Duration // checks_hourly(롤업)를 얼마나 둘지
}

// DefaultRetention 은 기본 보관 정책이다.
//
// 원본 7일: 최근 일주일은 개별 체크까지 들여다볼 수 있어야 장애를 파헤친다.
// 롤업 90일: M5의 상태 페이지가 90일 업타임 바를 그린다.
var DefaultRetention = Retention{
	Raw:    7 * 24 * time.Hour,
	Hourly: 90 * 24 * time.Hour,
}

// RollupResult 는 롤업 한 번의 결과다.
type RollupResult struct {
	Buckets int // 새로 만들거나 갱신한 (모니터, 시각) 칸 수
	Rows    int // 그 칸들이 접어 넣은 원본 행 수
}

// Rollup 은 아직 집계되지 않은 '완료된 시간'들을 checks_hourly 로 접는다.
//
// 현재 진행 중인 시간은 건드리지 않는다. 아직 체크가 더 들어올 수 있어서
// 지금 집계하면 반쪽짜리 값이 박힌다. 그래서 now 가 속한 정시가 경계다.
func (s *Store) Rollup(ctx context.Context, now time.Time) (RollupResult, error) {
	boundary := unix(truncHour(now))
	var res RollupResult

	// 어떤 (모니터, 시각) 칸에 집계할 원본이 남아 있는지 먼저 찾는다.
	//
	// checked_at / 3600 * 3600 은 정수 나눗셈이라 소수점이 버려진다.
	// 즉 "정시로 내림". INTEGER 로 저장한 덕에 이 한 줄로 끝난다.
	rows, err := s.db.QueryContext(ctx, `
		SELECT monitor_id, checked_at / 3600 * 3600 AS hour
		FROM checks
		WHERE checked_at < ?
		GROUP BY monitor_id, hour
		ORDER BY hour`, boundary)
	if err != nil {
		return res, fmt.Errorf("롤업 대상 조회 실패: %w", err)
	}

	type bucket struct {
		monitorID int64
		hour      int64
	}
	var buckets []bucket
	for rows.Next() {
		var b bucket
		if err := rows.Scan(&b.monitorID, &b.hour); err != nil {
			rows.Close()
			return res, fmt.Errorf("롤업 대상 읽기 실패: %w", err)
		}
		buckets = append(buckets, b)
	}
	// rows.Err() 는 순회 도중 난 에러를 알려준다.
	// Next() 가 false 를 반환한 이유가 "끝"인지 "에러"인지 구분하려면 꼭 확인해야 한다.
	if err := rows.Err(); err != nil {
		rows.Close()
		return res, fmt.Errorf("롤업 대상 순회 실패: %w", err)
	}
	rows.Close()

	if len(buckets) == 0 {
		return res, nil
	}

	err = s.tx(ctx, func(tx *sql.Tx) error {
		upsert, err := tx.PrepareContext(ctx, `
			INSERT INTO checks_hourly
				(monitor_id, hour, total, ok_count, latency_p50, latency_p95, latency_max)
			VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(monitor_id, hour) DO UPDATE SET
				total       = excluded.total,
				ok_count    = excluded.ok_count,
				latency_p50 = excluded.latency_p50,
				latency_p95 = excluded.latency_p95,
				latency_max = excluded.latency_max`)
		if err != nil {
			return fmt.Errorf("롤업 upsert 준비 실패: %w", err)
		}
		defer upsert.Close()

		for _, b := range buckets {
			// 백분위수는 SQL 로 구하기 번거롭다(SQLite 에 percentile 함수가 없다).
			// 한 시간 치 지연시간을 Go 로 가져와 정렬해서 계산한다.
			// 모니터 하나의 한 시간이면 많아야 수천 행이라 부담이 없다.
			lat, total, okCount, err := s.hourSamples(ctx, tx, b.monitorID, b.hour)
			if err != nil {
				return err
			}
			if total == 0 {
				continue
			}

			p50, p95, max := percentiles(lat)
			if _, err := upsert.ExecContext(ctx,
				b.monitorID, b.hour, total, okCount, p50, p95, max); err != nil {
				return fmt.Errorf("롤업 저장 실패: %w", err)
			}
			res.Buckets++
			res.Rows += total
		}
		return nil
	})
	if err != nil {
		return RollupResult{}, err
	}
	return res, nil
}

// hourSamples 는 한 시간 치의 성공 지연시간 목록과 전체/성공 개수를 가져온다.
//
// 지연시간은 성공한 체크만 모은다. 실패한 체크의 '응답시간'은
// 타임아웃까지 걸린 시간이라 p95 에 섞으면 수치가 왜곡된다.
func (s *Store) hourSamples(ctx context.Context, tx *sql.Tx, monitorID, hour int64) (lat []int64, total, okCount int, err error) {
	err = tx.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(SUM(ok), 0)
		FROM checks
		WHERE monitor_id = ? AND checked_at >= ? AND checked_at < ?`,
		monitorID, hour, hour+3600).Scan(&total, &okCount)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("시간별 집계 실패: %w", err)
	}

	rows, err := tx.QueryContext(ctx, `
		SELECT latency_ms
		FROM checks
		WHERE monitor_id = ? AND checked_at >= ? AND checked_at < ? AND ok = 1`,
		monitorID, hour, hour+3600)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("지연시간 조회 실패: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var ms int64
		if err := rows.Scan(&ms); err != nil {
			return nil, 0, 0, fmt.Errorf("지연시간 읽기 실패: %w", err)
		}
		lat = append(lat, ms)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, 0, fmt.Errorf("지연시간 순회 실패: %w", err)
	}
	return lat, total, okCount, nil
}

// percentiles 는 정렬되지 않은 표본에서 p50/p95/최댓값을 구한다.
//
// 입력 슬라이스를 그 자리에서 정렬한다(호출부가 재사용하지 않는 걸 전제).
// 백분위수 계산법은 여러 가지인데 여기서는 '가장 가까운 순위(nearest-rank)'를 쓴다.
// p95 = 오름차순으로 정렬했을 때 ceil(0.95 × n) 번째 값.
func percentiles(samples []int64) (p50, p95, max int64) {
	if len(samples) == 0 {
		return 0, 0, 0
	}
	slices.Sort(samples)
	return quantile(samples, 0.50), quantile(samples, 0.95), samples[len(samples)-1]
}

func quantile(sorted []int64, q float64) int64 {
	if len(sorted) == 0 {
		return 0
	}
	// nearest-rank: 순위는 1부터 시작하므로 인덱스로 바꿀 때 1을 뺀다.
	rank := int(float64(len(sorted))*q + 0.9999999)
	if rank < 1 {
		rank = 1
	}
	if rank > len(sorted) {
		rank = len(sorted)
	}
	return sorted[rank-1]
}

// PruneResult 는 정리 잡이 지운 행 수다.
type PruneResult struct {
	RawDeleted    int64
	HourlyDeleted int64
}

// Prune 은 보관 기간이 지난 데이터를 지운다.
//
// 원본(checks)을 지우기 전에 반드시 롤업이 끝나 있어야 한다.
// 안 그러면 집계도 못 한 데이터를 날려버린다. 그래서 Maintain 이
// Rollup → Prune 순서를 강제한다.
func (s *Store) Prune(ctx context.Context, now time.Time, r Retention) (PruneResult, error) {
	var res PruneResult

	rawCutoff := unix(now.Add(-r.Raw))
	hourlyCutoff := unix(now.Add(-r.Hourly))

	err := s.tx(ctx, func(tx *sql.Tx) error {
		// 롤업이 끝난 구간까지만 지운다. 아직 집계 안 된 원본은 남긴다.
		// COALESCE 는 값이 NULL 이면 대체값을 준다 — 롤업이 한 번도 안 돌았으면
		// MAX(hour) 가 NULL 이라 자칫 전부 지울 수 있다.
		var rolledUpTo int64
		if err := tx.QueryRowContext(ctx,
			`SELECT COALESCE(MAX(hour) + 3600, 0) FROM checks_hourly`).Scan(&rolledUpTo); err != nil {
			return fmt.Errorf("롤업 진행 지점 조회 실패: %w", err)
		}
		cutoff := min(rawCutoff, rolledUpTo)

		r1, err := tx.ExecContext(ctx, `DELETE FROM checks WHERE checked_at < ?`, cutoff)
		if err != nil {
			return fmt.Errorf("원본 정리 실패: %w", err)
		}
		res.RawDeleted, _ = r1.RowsAffected()

		r2, err := tx.ExecContext(ctx, `DELETE FROM checks_hourly WHERE hour < ?`, hourlyCutoff)
		if err != nil {
			return fmt.Errorf("롤업 정리 실패: %w", err)
		}
		res.HourlyDeleted, _ = r2.RowsAffected()
		return nil
	})
	if err != nil {
		return PruneResult{}, err
	}
	return res, nil
}

// Maintain 은 롤업과 정리를 올바른 순서로 한 번 돌린다.
func (s *Store) Maintain(ctx context.Context, now time.Time, r Retention) error {
	roll, err := s.Rollup(ctx, now)
	if err != nil {
		return err
	}
	prune, err := s.Prune(ctx, now, r)
	if err != nil {
		return err
	}

	if roll.Buckets > 0 || prune.RawDeleted > 0 || prune.HourlyDeleted > 0 {
		s.log.Info("정리 잡 완료",
			"롤업_칸", roll.Buckets, "접은_행", roll.Rows,
			"지운_원본", prune.RawDeleted, "지운_롤업", prune.HourlyDeleted)
	}
	return nil
}

// RunMaintenance 는 every 주기로 Maintain 을 반복한다. ctx 가 취소되면 끝난다.
//
// 별도 goroutine 으로 띄우라는 뜻이 아니라, 이 함수 자체를 goroutine 에서 부른다.
// 그래야 호출부가 goroutine 의 수명을 눈으로 볼 수 있다.
func (s *Store) RunMaintenance(ctx context.Context, every time.Duration, r Retention) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// 종료 직전에 한 번 더 돌려 마지막 구간을 접어 둔다.
			// ctx 는 이미 취소됐으므로 질의용으로 쓸 수 없다. 짧은 시한을 새로 판다.
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := s.Maintain(shutdownCtx, time.Now(), r); err != nil {
				s.log.Warn("종료 시 정리 잡 실패", "err", err)
			}
			return

		case <-ticker.C:
			if err := s.Maintain(ctx, time.Now(), r); err != nil {
				// 정리 잡이 실패해도 모니터링은 계속돼야 한다. 로그만 남긴다.
				s.log.Warn("정리 잡 실패", "err", err)
			}
		}
	}
}
