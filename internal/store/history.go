package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// DayBucket 은 하루치 집계다. 상태 페이지의 업타임 바 한 칸이 된다.
type DayBucket struct {
	// Date 는 그 날의 자정이다 (요청한 시간대 기준).
	Date time.Time

	Total int64
	OK    int64

	// 응답시간은 시간별 칸을 성공 체크 수로 가중평균한 근사치다.
	// 하루치 원본을 다 들고 있지 않으므로 정확한 백분위수를 낼 수 없다.
	P50MS int64
	P95MS int64
}

// HasData 는 그 날 체크가 한 번이라도 있었는지 알려준다.
//
// "데이터 없음"과 "0% 업타임"은 전혀 다르다. 모니터를 등록하기 전 날이나
// 데몬이 꺼져 있던 날을 빨간 칸으로 그리면 거짓말이 된다.
func (d DayBucket) HasData() bool { return d.Total > 0 }

// Uptime 은 그 날의 업타임 백분율이다. 데이터가 없으면 0.
func (d DayBucket) Uptime() float64 {
	if d.Total == 0 {
		return 0
	}
	return float64(d.OK) / float64(d.Total) * 100
}

// hourAgg 는 한 시간치 집계다. 롤업 행이든 원본을 묶은 것이든 이 꼴로 모은다.
type hourAgg struct {
	hour     int64
	total    int64
	okCount  int64
	p50, p95 int64
}

// DailyHistory 는 최근 days 일의 일별 집계를 오래된 날부터 돌려준다.
//
// 날짜 경계는 loc 시간대의 자정이다. 한국 사용자에게 "9월 14일"은
// UTC 자정이 아니라 KST 자정부터다. UTC 로 자르면 오전 9시에 날이 바뀐다.
//
// 데이터가 없는 날도 빠짐없이 들어간다(Total=0). 화면에서 칸이 비지 않아야
// "그 날은 기록이 없다"를 보여줄 수 있다.
func (s *Store) DailyHistory(ctx context.Context, monitorID int64, days int, loc *time.Location, now time.Time) ([]DayBucket, error) {
	if days <= 0 {
		return nil, fmt.Errorf("days 는 1 이상이어야 한다: %d", days)
	}
	if loc == nil {
		loc = time.UTC
	}

	// 오늘 자정(loc 기준)에서 days-1 일 전 자정이 시작점이다.
	localNow := now.In(loc)
	today := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), 0, 0, 0, 0, loc)
	start := today.AddDate(0, 0, -(days - 1))

	frontier, err := s.rollupFrontier(ctx)
	if err != nil {
		return nil, err
	}

	hours, err := s.hourlyInRange(ctx, monitorID, start, frontier)
	if err != nil {
		return nil, err
	}
	rawHours, err := s.rawHoursInRange(ctx, monitorID, max(unix(start), frontier), unix(now))
	if err != nil {
		return nil, err
	}
	hours = append(hours, rawHours...)

	// 날짜별 칸을 먼저 전부 만들어 둔다. 데이터 없는 날도 자리를 차지해야 한다.
	buckets := make([]DayBucket, days)
	index := make(map[string]int, days)
	for i := range days {
		d := start.AddDate(0, 0, i)
		buckets[i].Date = d
		index[d.Format(time.DateOnly)] = i
	}

	// 시간 칸을 해당 날짜로 넣는다.
	//
	// AddDate 로 날짜를 만든 이유: 서머타임이 있는 시간대에서는 하루가 23시간이나
	// 25시간일 수 있다. 24*time.Hour 를 더하면 자정이 어긋난다.
	// (Asia/Seoul 은 서머타임이 없지만 코드는 시간대를 가리지 않아야 한다)
	type weighted struct{ sum50, sum95, weight int64 }
	lat := make([]weighted, days)

	for _, h := range hours {
		key := time.Unix(h.hour, 0).In(loc).Format(time.DateOnly)
		i, ok := index[key]
		if !ok {
			continue // 범위 밖 (시간대 경계에 걸친 칸)
		}
		buckets[i].Total += h.total
		buckets[i].OK += h.okCount
		if h.okCount > 0 {
			lat[i].sum50 += h.p50 * h.okCount
			lat[i].sum95 += h.p95 * h.okCount
			lat[i].weight += h.okCount
		}
	}
	for i := range buckets {
		if lat[i].weight > 0 {
			buckets[i].P50MS = lat[i].sum50 / lat[i].weight
			buckets[i].P95MS = lat[i].sum95 / lat[i].weight
		}
	}
	return buckets, nil
}

// hourlyInRange 는 롤업된 시간 칸을 [from, frontier) 에서 가져온다.
func (s *Store) hourlyInRange(ctx context.Context, monitorID int64, from time.Time, frontier int64) ([]hourAgg, error) {
	fromHour := unix(truncHour(from))
	if fromHour >= frontier {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT hour, total, ok_count, latency_p50, latency_p95
		FROM checks_hourly
		WHERE monitor_id = ? AND hour >= ? AND hour < ?
		ORDER BY hour`, monitorID, fromHour, frontier)
	if err != nil {
		return nil, fmt.Errorf("시간별 롤업 조회 실패: %w", err)
	}
	defer rows.Close()

	var out []hourAgg
	for rows.Next() {
		var h hourAgg
		if err := rows.Scan(&h.hour, &h.total, &h.okCount, &h.p50, &h.p95); err != nil {
			return nil, fmt.Errorf("시간별 롤업 읽기 실패: %w", err)
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// rawHoursInRange 는 아직 롤업 안 된 원본을 시간 단위로 묶는다.
//
// 롤업과 같은 방식(성공 체크만으로 nearest-rank 백분위수)으로 계산해야
// 두 구간을 이어 붙였을 때 값의 성격이 같다.
func (s *Store) rawHoursInRange(ctx context.Context, monitorID, from, to int64) ([]hourAgg, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT checked_at / 3600 * 3600 AS hour, ok, latency_ms
		FROM checks
		WHERE monitor_id = ? AND checked_at >= ? AND checked_at <= ?
		ORDER BY hour`, monitorID, from, to)
	if err != nil {
		return nil, fmt.Errorf("원본 시간별 조회 실패: %w", err)
	}
	defer rows.Close()

	var (
		out     []hourAgg
		cur     *hourAgg
		samples []int64
	)
	// flush 는 모아 둔 한 시간치를 마무리한다.
	// 클로저가 바깥 변수(cur, samples, out)를 직접 고친다.
	flush := func() {
		if cur == nil {
			return
		}
		cur.p50, cur.p95, _ = percentiles(samples)
		out = append(out, *cur)
		samples = samples[:0]
	}

	for rows.Next() {
		var (
			hour, latency int64
			okInt         int
		)
		if err := rows.Scan(&hour, &okInt, &latency); err != nil {
			return nil, fmt.Errorf("원본 시간별 읽기 실패: %w", err)
		}
		if cur == nil || cur.hour != hour {
			flush()
			cur = &hourAgg{hour: hour}
		}
		cur.total++
		if okInt != 0 {
			cur.okCount++
			samples = append(samples, latency)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("원본 시간별 순회 실패: %w", err)
	}
	flush()
	return out, nil
}

// Monitor 는 id 로 모니터 하나를 찾는다. 없으면 (nil, nil).
func (s *Store) Monitor(ctx context.Context, id int64) (*MonitorRow, error) {
	var (
		m       MonitorRow
		enabled int
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT id, name, target, enabled FROM monitors WHERE id = ?`, id).
		Scan(&m.ID, &m.Name, &m.Target, &enabled)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("모니터 조회 실패: %w", err)
	}
	m.Enabled = enabled != 0
	return &m, nil
}
