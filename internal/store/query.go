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
	// 백분위수는 합칠 수 없어서(p95들의 평균은 p95가 아니다) 정확하지 않다.
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

// rollupFrontier 는 롤업이 끝난 지점을 돌려준다.
//
// 이 시각보다 앞은 checks_hourly 에 접혀 있고, 뒤는 checks 에 원본으로 있다.
// 두 테이블을 이어 붙일 때 이 경계를 쓰면 중복도 누락도 없다.
//
// 롤업이 한 번도 안 돌았으면 0이라 전부 원본에서 읽는다.
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
//
// 롤업 구간과 원본 구간을 경계로 나눠 더한다.
// 개수 합은 결합법칙이 성립해서, 접어 둔 값을 더해도 **정확히** 같은 값이 나온다.
//
// 다만 경계가 정시 단위라, 구간 시작점이 정시가 아니면 첫 칸에
// since 이전 데이터가 최대 59분까지 섞인다. 24시간/7일/30일 같은
// 긴 구간에서는 무시할 만한 오차다.
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
//
// 구간이 전부 원본에 남아 있으면 표본을 직접 정렬해 **정확한** 값을 낸다.
// 롤업 구간이 섞이면 근사할 수밖에 없어서 Approx 를 true 로 표시한다.
func (s *Store) Latency(ctx context.Context, monitorID int64, since, now time.Time) (Latency, error) {
	frontier, err := s.rollupFrontier(ctx)
	if err != nil {
		return Latency{}, err
	}

	sinceUnix := unix(since)

	// 구간 전체가 원본에 있다 → 정확히 계산한다.
	if sinceUnix >= frontier {
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

	// 롤업 구간이 섞였다 → 칸별 백분위수를 개수로 가중평균한다.
	//
	// 엄밀히는 틀린 계산이다. p95 들의 평균은 전체의 p95 가 아니다.
	// 정확히 하려면 칸마다 히스토그램이나 t-digest 를 저장해야 하는데,
	// 이 프로젝트 규모에서는 과하다. 대신 근사임을 값에 담아 전달한다.
	lat := Latency{Approx: true}

	rows, err := s.db.QueryContext(ctx, `
		SELECT total, latency_p50, latency_p95, latency_max
		FROM checks_hourly
		WHERE monitor_id = ? AND hour >= ? AND hour < ?`,
		monitorID, unix(truncHour(since)), frontier)
	if err != nil {
		return Latency{}, fmt.Errorf("롤업 응답시간 조회 실패: %w", err)
	}
	defer rows.Close()

	var sum50, sum95, weight int64
	var maxMS int64
	for rows.Next() {
		var total, p50, p95, mx int64
		if err := rows.Scan(&total, &p50, &p95, &mx); err != nil {
			return Latency{}, fmt.Errorf("롤업 응답시간 읽기 실패: %w", err)
		}
		sum50 += p50 * total
		sum95 += p95 * total
		weight += total
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
//
// MAX(checked_at) 이 아니라 MAX(id) 로 찾는다. id 는 단조 증가라
// 같은 초에 두 번 체크돼도 순서가 명확하다.
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

	// 행이 없는 건 에러가 아니라 "아직 체크 안 함"이다.
	// sql.ErrNoRows 를 errors.Is 로 구분해 호출부에 nil 을 준다.
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
//
// 스펙 M2가 요구한 "업타임 % (24h / 7d / 30d), 응답시간 p50 / p95, 현재 상태".
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
