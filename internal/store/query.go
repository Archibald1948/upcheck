package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Uptime 은 어떤 구간의 성공/전체 개수다.
type Uptime struct {
	Total int64
	OK    int64
}

// Percent 는 업타임 백분율이다. 표본이 없으면 0을 준다.
func (u Uptime) Percent() float64 {
	if u.Total == 0 {
		return 0
	}
	return float64(u.OK) / float64(u.Total) * 100
}

// Latency 는 응답시간 요약이다.
type Latency struct {
	P50, P95, Max time.Duration
	Samples       int64

	// Approx 가 true 면 롤업된 값에서 근사한 것이다.
	Approx bool
}

// MonitorStatus 는 상태 페이지에 뿌릴 모니터 하나의 현황이다.
type MonitorStatus struct {
	ID      int64
	Name    string
	Target  string
	Enabled bool

	LastCheck  *CheckRow // 한 번도 체크된 적 없으면 nil
	Uptime24h  Uptime
	Uptime7d   Uptime
	Uptime30d  Uptime
	Latency24h Latency
}

// Up 은 마지막 체크가 성공이었는지 알려준다.
func (m MonitorStatus) Up() bool { return m.LastCheck != nil && m.LastCheck.OK }

// rollupFrontier 는 롤업이 끝난 지점을 돌려준다. 롤업이 없으면 0.
func (s *Store) rollupFrontier(ctx context.Context) (int64, error) {
	var frontier int64
	err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(hour) + 3600, 0) FROM checks_hourly`).Scan(&frontier)
	if err != nil {
		return 0, fmt.Errorf("롤업 경계 조회 실패: %w", err)
	}
	return frontier, nil
}

// Uptime 은 [since, now] 구간의 업타임을 구한다.
// 첫 칸은 정시로 내려 읽어서 since 이전 데이터가 최대 59분 섞인다.
func (s *Store) Uptime(ctx context.Context, monitorID int64, since, now time.Time) (Uptime, error) {
	frontier, err := s.rollupFrontier(ctx)
	if err != nil {
		return Uptime{}, err
	}

	var u Uptime
	sinceHour := unix(truncHour(since))

	// 1) 롤업 구간: [since 의 정시, frontier)
	if sinceHour < frontier {
		err := s.db.QueryRowContext(ctx, `
			SELECT COALESCE(SUM(total), 0), COALESCE(SUM(ok_count), 0)
			FROM checks_hourly
			WHERE monitor_id = ? AND hour >= ? AND hour < ?`,
			monitorID, sinceHour, frontier).Scan(&u.Total, &u.OK)
		if err != nil {
			return Uptime{}, fmt.Errorf("롤업 업타임 조회 실패: %w", err)
		}
	}

	// 2) 원본 구간: [max(since, frontier), now]
	rawFrom := max(unix(since), frontier)
	var rawTotal, rawOK int64
	err = s.db.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(SUM(ok), 0)
		FROM checks
		WHERE monitor_id = ? AND checked_at >= ? AND checked_at <= ?`,
		monitorID, rawFrom, unix(now)).Scan(&rawTotal, &rawOK)
	if err != nil {
		return Uptime{}, fmt.Errorf("원본 업타임 조회 실패: %w", err)
	}

	u.Total += rawTotal
	u.OK += rawOK
	return u, nil
}

// Latency 는 [since, now] 구간의 응답시간 백분위수를 구한다.
// 롤업에만 남은 구간이 섞이면 근사하고 Approx 를 true 로 표시한다.
func (s *Store) Latency(ctx context.Context, monitorID int64, since, now time.Time) (Latency, error) {
	frontier, err := s.rollupFrontier(ctx)
	if err != nil {
		return Latency{}, err
	}

	sinceUnix := unix(since)

	rawComplete, err := s.rawCovers(ctx, monitorID, sinceUnix, frontier)
	if err != nil {
		return Latency{}, err
	}

	if rawComplete {
		samples, err := s.rawLatencies(ctx, monitorID, sinceUnix, unix(now))
		if err != nil {
			return Latency{}, err
		}
		p50, p95, mx := percentiles(samples)
		return Latency{
			P50:     time.Duration(p50) * time.Millisecond,
			P95:     time.Duration(p95) * time.Millisecond,
			Max:     time.Duration(mx) * time.Millisecond,
			Samples: int64(len(samples)),
		}, nil
	}

	// 롤업 구간이 섞였다 → 칸별 백분위수를 개수로 가중평균한다(근사).
	lat := Latency{Approx: true}

	// 가중치는 total 이 아니라 ok_count 다. 칸의 백분위수는 성공한 체크로만 계산됐다.
	rows, err := s.db.QueryContext(ctx, `
		SELECT ok_count, latency_p50, latency_p95, latency_max
		FROM checks_hourly
		WHERE monitor_id = ? AND hour >= ? AND hour < ? AND ok_count > 0`,
		monitorID, unix(truncHour(since)), frontier)
	if err != nil {
		return Latency{}, fmt.Errorf("롤업 응답시간 조회 실패: %w", err)
	}
	defer rows.Close()

	var sum50, sum95, weight int64
	var maxMS int64
	for rows.Next() {
		var okCount, p50, p95, mx int64
		if err := rows.Scan(&okCount, &p50, &p95, &mx); err != nil {
			return Latency{}, fmt.Errorf("롤업 응답시간 읽기 실패: %w", err)
		}
		sum50 += p50 * okCount
		sum95 += p95 * okCount
		weight += okCount
		maxMS = max(maxMS, mx)
	}
	if err := rows.Err(); err != nil {
		return Latency{}, fmt.Errorf("롤업 응답시간 순회 실패: %w", err)
	}

	// 원본으로 남아 있는 꼬리 구간도 한 칸처럼 취급해 섞는다.
	tail, err := s.rawLatencies(ctx, monitorID, frontier, unix(now))
	if err != nil {
		return Latency{}, err
	}
	if len(tail) > 0 {
		p50, p95, mx := percentiles(tail)
		n := int64(len(tail))
		sum50 += p50 * n
		sum95 += p95 * n
		weight += n
		maxMS = max(maxMS, mx)
	}

	if weight > 0 {
		lat.P50 = time.Duration(sum50/weight) * time.Millisecond
		lat.P95 = time.Duration(sum95/weight) * time.Millisecond
	}
	lat.Max = time.Duration(maxMS) * time.Millisecond
	lat.Samples = weight
	return lat, nil
}

// rawCovers 는 원본(checks)만으로 [since, now] 를 빠짐없이 덮는지 알려준다.
// 롤업은 원본을 지우지 않으므로 frontier 만 보면 안 된다.
func (s *Store) rawCovers(ctx context.Context, monitorID, since, frontier int64) (bool, error) {
	if since >= frontier {
		return true, nil
	}

	var minRaw sql.NullInt64
	if err := s.db.QueryRowContext(ctx,
		`SELECT MIN(checked_at) FROM checks WHERE monitor_id = ?`, monitorID).Scan(&minRaw); err != nil {
		return false, fmt.Errorf("원본 시작 시각 조회 실패: %w", err)
	}
	if !minRaw.Valid {
		return false, nil
	}

	// 원본이 정시가 아닌 시각부터 남아 있을 수 있어 칸의 끝(hour+3600)과 since 를 비교한다.
	var overlap int
	if err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM checks_hourly
		WHERE monitor_id = ? AND hour < ? AND hour + 3600 > ?`,
		monitorID, minRaw.Int64, since).Scan(&overlap); err != nil {
		return false, fmt.Errorf("원본 누락 구간 조회 실패: %w", err)
	}
	return overlap == 0, nil
}

// rawLatencies 는 성공한 체크의 지연시간만 모아 온다.
func (s *Store) rawLatencies(ctx context.Context, monitorID, from, to int64) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT latency_ms
		FROM checks
		WHERE monitor_id = ? AND checked_at >= ? AND checked_at <= ? AND ok = 1`,
		monitorID, from, to)
	if err != nil {
		return nil, fmt.Errorf("응답시간 조회 실패: %w", err)
	}
	defer rows.Close()

	var out []int64
	for rows.Next() {
		var ms int64
		if err := rows.Scan(&ms); err != nil {
			return nil, fmt.Errorf("응답시간 읽기 실패: %w", err)
		}
		out = append(out, ms)
	}
	return out, rows.Err()
}

// LastCheck 는 모니터의 가장 최근 체크를 돌려준다. 없으면 (nil, nil).
// 같은 초에 두 번 체크될 수 있어 checked_at 이 아니라 MAX(id) 로 찾는다.
func (s *Store) LastCheck(ctx context.Context, monitorID int64) (*CheckRow, error) {
	var (
		r         CheckRow
		checkedAt int64
		okInt     int
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT monitor_id, type, checked_at, ok, status_code, latency_ms, error, warning
		FROM checks
		WHERE id = (SELECT MAX(id) FROM checks WHERE monitor_id = ?)`,
		monitorID).Scan(&r.MonitorID, &r.Type, &checkedAt, &okInt, &r.StatusCode, &r.LatencyMS, &r.Error, &r.Warning)

	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("최근 체크 조회 실패: %w", err)
	}

	r.CheckedAt = fromUnix(checkedAt)
	r.OK = okInt != 0
	return &r, nil
}

// MonitorRow 는 monitors 테이블의 한 행이다.
type MonitorRow struct {
	ID      int64
	Name    string
	Target  string
	Enabled bool
}

// Monitors 는 등록된 모니터를 이름 순으로 돌려준다.
func (s *Store) Monitors(ctx context.Context, onlyEnabled bool) ([]MonitorRow, error) {
	q := `SELECT id, name, target, enabled FROM monitors`
	if onlyEnabled {
		q += ` WHERE enabled = 1`
	}
	q += ` ORDER BY name`

	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("모니터 목록 조회 실패: %w", err)
	}
	defer rows.Close()

	var out []MonitorRow
	for rows.Next() {
		var m MonitorRow
		var enabled int
		if err := rows.Scan(&m.ID, &m.Name, &m.Target, &enabled); err != nil {
			return nil, fmt.Errorf("모니터 행 읽기 실패: %w", err)
		}
		m.Enabled = enabled != 0
		out = append(out, m)
	}
	return out, rows.Err()
}

// Summary 는 활성 모니터 전체의 현황을 모아 온다.
func (s *Store) Summary(ctx context.Context, now time.Time) ([]MonitorStatus, error) {
	monitors, err := s.Monitors(ctx, true)
	if err != nil {
		return nil, err
	}

	out := make([]MonitorStatus, 0, len(monitors))
	for _, m := range monitors {
		st := MonitorStatus{ID: m.ID, Name: m.Name, Target: m.Target, Enabled: m.Enabled}

		if st.LastCheck, err = s.LastCheck(ctx, m.ID); err != nil {
			return nil, err
		}
		if st.Uptime24h, err = s.Uptime(ctx, m.ID, now.Add(-24*time.Hour), now); err != nil {
			return nil, err
		}
		if st.Uptime7d, err = s.Uptime(ctx, m.ID, now.Add(-7*24*time.Hour), now); err != nil {
			return nil, err
		}
		if st.Uptime30d, err = s.Uptime(ctx, m.ID, now.Add(-30*24*time.Hour), now); err != nil {
			return nil, err
		}
		if st.Latency24h, err = s.Latency(ctx, m.ID, now.Add(-24*time.Hour), now); err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, nil
}
