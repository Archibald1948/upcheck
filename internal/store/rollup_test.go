package store

import (
	"context"
	"testing"
	"time"
)

// seed 는 [from, from+span) 구간에 일정 간격으로 체크를 심는다.
// failEvery 번째마다 실패로 만든다 (0이면 전부 성공).
func seed(t *testing.T, s *Store, monitorID int64, from time.Time, span, every time.Duration, failEvery int, latencyMS int64) int {
	t.Helper()

	var rows []CheckRow
	n := 0
	for at := from; at.Before(from.Add(span)); at = at.Add(every) {
		n++
		ok := failEvery == 0 || n%failEvery != 0
		rows = append(rows, CheckRow{
			MonitorID: monitorID, CheckedAt: at, OK: ok,
			StatusCode: 200, LatencyMS: latencyMS + int64(n%10),
		})
		if !ok {
			rows[len(rows)-1].StatusCode = 503
			rows[len(rows)-1].Error = "실패"
		}
	}
	if err := s.InsertChecks(context.Background(), rows); err != nil {
		t.Fatalf("시드 데이터 저장 실패: %v", err)
	}
	return len(rows)
}

// rawUptime 은 원본 테이블에서 직접 센 정답이다. 롤업 결과를 여기에 맞춰 본다.
func rawUptime(t *testing.T, s *Store, monitorID int64, from, to time.Time) (total, ok int64) {
	t.Helper()
	err := s.db.QueryRow(`
		SELECT COUNT(*), COALESCE(SUM(ok),0) FROM checks
		WHERE monitor_id = ? AND checked_at >= ? AND checked_at <= ?`,
		monitorID, unix(from), unix(to)).Scan(&total, &ok)
	if err != nil {
		t.Fatalf("원본 집계 실패: %v", err)
	}
	return total, ok
}

// TestRollupSkipsCurrentHour 는 진행 중인 시간을 접지 않는지 본다.
func TestRollupSkipsCurrentHour(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	ids, _ := s.SyncMonitors(ctx, testMonitors("a"))

	now := time.Date(2026, 3, 1, 10, 30, 0, 0, time.UTC)
	// 08:00~10:00 (완료된 2시간) + 10:00~10:30 (진행 중)
	seed(t, s, ids["a"], now.Add(-150*time.Minute), 150*time.Minute, time.Minute, 0, 100)

	res, err := s.Rollup(ctx, now)
	if err != nil {
		t.Fatalf("Rollup 실패: %v", err)
	}

	var hours []int64
	rows, _ := s.db.Query(`SELECT hour FROM checks_hourly ORDER BY hour`)
	defer rows.Close()
	for rows.Next() {
		var h int64
		rows.Scan(&h)
		hours = append(hours, h)
	}

	boundary := unix(truncHour(now))
	for _, h := range hours {
		if h >= boundary {
			t.Errorf("진행 중인 시간 %v 를 집계했다", fromUnix(h))
		}
	}
	if len(hours) == 0 {
		t.Fatal("완료된 시간이 하나도 집계되지 않았다")
	}
	t.Logf("집계된 시간 칸 %d개 (원본 %d행)", res.Buckets, res.Rows)
}

// TestRollupPreservesUptimeExactly 는 롤업이 업타임을 정확히 보존하는지 본다.
func TestRollupPreservesUptimeExactly(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	ids, _ := s.SyncMonitors(ctx, testMonitors("a"))
	id := ids["a"]

	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	from := now.Add(-6 * time.Hour)
	// 6시간, 30초 간격, 7번마다 실패
	seed(t, s, id, from, 6*time.Hour, 30*time.Second, 7, 200)

	wantTotal, wantOK := rawUptime(t, s, id, from, now)

	if _, err := s.Rollup(ctx, now); err != nil {
		t.Fatalf("Rollup 실패: %v", err)
	}

	// 롤업만으로 집계한 값
	var gotTotal, gotOK int64
	err := s.db.QueryRow(`
		SELECT COALESCE(SUM(total),0), COALESCE(SUM(ok_count),0) FROM checks_hourly
		WHERE monitor_id = ?`, id).Scan(&gotTotal, &gotOK)
	if err != nil {
		t.Fatal(err)
	}

	if gotTotal != wantTotal || gotOK != wantOK {
		t.Errorf("롤업 집계 (%d/%d) ≠ 원본 (%d/%d)", gotOK, gotTotal, wantOK, wantTotal)
	}
	t.Logf("원본 %d행 → 롤업 %d행, 업타임 %.4f%% 보존",
		wantTotal, countRows(t, s, "checks_hourly"),
		float64(gotOK)/float64(gotTotal)*100)
}

// TestRollupIsIdempotent 는 두 번 돌려도 값이 변하지 않는지 본다.
func TestRollupIsIdempotent(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	ids, _ := s.SyncMonitors(ctx, testMonitors("a"))

	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	seed(t, s, ids["a"], now.Add(-3*time.Hour), 3*time.Hour, time.Minute, 5, 150)

	first, err := s.Rollup(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := hourlySnapshot(t, s)

	second, err := s.Rollup(ctx, now)
	if err != nil {
		t.Fatal(err)
	}

	if got := hourlySnapshot(t, s); got != snapshot {
		t.Errorf("두 번째 롤업이 값을 바꿨다:\n1차 %s\n2차 %s", snapshot, got)
	}
	if first.Buckets != second.Buckets {
		t.Errorf("칸 수가 달라졌다: %d → %d", first.Buckets, second.Buckets)
	}
}

// ─────────────── 정리 ───────────────

// TestPruneWaitsForRollup 은 집계 안 된 원본을 지우지 않는지 본다.
func TestPruneWaitsForRollup(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	ids, _ := s.SyncMonitors(ctx, testMonitors("a"))

	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	// 30일 전 데이터 — 보관 기간(7일)을 한참 넘겼다
	seed(t, s, ids["a"], now.Add(-30*24*time.Hour), 3*time.Hour, time.Minute, 0, 100)
	before := countRows(t, s, "checks")

	// 롤업을 돌리지 않은 채 정리만 시도한다
	res, err := s.Prune(ctx, now, DefaultRetention)
	if err != nil {
		t.Fatalf("Prune 실패: %v", err)
	}

	if res.RawDeleted != 0 {
		t.Errorf("집계도 안 된 원본을 %d행 지웠다", res.RawDeleted)
	}
	if after := countRows(t, s, "checks"); after != before {
		t.Errorf("원본이 %d → %d 로 줄었다", before, after)
	}
}

// TestPruneAfterRollup 은 집계가 끝난 오래된 원본은 지우고,
// 롤업은 남기는지 본다.
func TestPruneAfterRollup(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	ids, _ := s.SyncMonitors(ctx, testMonitors("a"))
	id := ids["a"]

	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	// 30일 전 3시간치 (보관 기간 초과) + 최근 3시간치 (보관 기간 내)
	seed(t, s, id, now.Add(-30*24*time.Hour), 3*time.Hour, time.Minute, 0, 100)
	seed(t, s, id, now.Add(-3*time.Hour), 3*time.Hour, time.Minute, 0, 100)

	if err := s.Maintain(ctx, now, DefaultRetention); err != nil {
		t.Fatalf("Maintain 실패: %v", err)
	}

	// 오래된 원본은 사라지고
	var oldRaw int64
	s.db.QueryRow(`SELECT COUNT(*) FROM checks WHERE checked_at < ?`,
		unix(now.Add(-7*24*time.Hour))).Scan(&oldRaw)
	if oldRaw != 0 {
		t.Errorf("보관 기간 지난 원본이 %d행 남았다", oldRaw)
	}

	// 최근 원본은 남아 있어야 하고
	var recentRaw int64
	s.db.QueryRow(`SELECT COUNT(*) FROM checks WHERE checked_at >= ?`,
		unix(now.Add(-7*24*time.Hour))).Scan(&recentRaw)
	if recentRaw == 0 {
		t.Error("보관 기간 내 원본까지 지웠다")
	}

	// 롤업은 30일 전 것도 남아 있어야 한다 (롤업 보관은 90일)
	var oldHourly int64
	s.db.QueryRow(`SELECT COUNT(*) FROM checks_hourly WHERE hour < ?`,
		unix(now.Add(-7*24*time.Hour))).Scan(&oldHourly)
	if oldHourly == 0 {
		t.Error("30일 전 롤업이 사라졌다 — 90일은 보관해야 한다")
	}

	t.Logf("원본 %d행 남음 · 롤업 %d행 (그중 7일 이전 %d행)",
		recentRaw, countRows(t, s, "checks_hourly"), oldHourly)
}

// TestPruneDropsOldRollups 는 롤업 보관 기간이 지난 것도 지우는지 본다.
func TestPruneDropsOldRollups(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	ids, _ := s.SyncMonitors(ctx, testMonitors("a"))

	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	seed(t, s, ids["a"], now.Add(-100*24*time.Hour), 2*time.Hour, time.Minute, 0, 100)
	seed(t, s, ids["a"], now.Add(-2*time.Hour), 2*time.Hour, time.Minute, 0, 100)

	if err := s.Maintain(ctx, now, DefaultRetention); err != nil {
		t.Fatal(err)
	}

	var tooOld int64
	s.db.QueryRow(`SELECT COUNT(*) FROM checks_hourly WHERE hour < ?`,
		unix(now.Add(-90*24*time.Hour))).Scan(&tooOld)
	if tooOld != 0 {
		t.Errorf("90일 넘은 롤업이 %d행 남았다", tooOld)
	}
}

// ─────────────── 도우미 ───────────────

func countRows(t *testing.T, s *Store, table string) int64 {
	t.Helper()
	var n int64
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatalf("%s 개수 조회 실패: %v", table, err)
	}
	return n
}

func hourlySnapshot(t *testing.T, s *Store) string {
	t.Helper()
	var out string
	rows, err := s.db.Query(`
		SELECT monitor_id, hour, total, ok_count, latency_p50, latency_p95
		FROM checks_hourly ORDER BY monitor_id, hour`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var mid, h, tot, ok, p50, p95 int64
		rows.Scan(&mid, &h, &tot, &ok, &p50, &p95)
		out += formatBucket(mid, h, tot, ok, p50, p95)
	}
	return out
}

func formatBucket(mid, h, tot, ok, p50, p95 int64) string {
	return time.Unix(h, 0).UTC().Format("[01-02T15") +
		"|" + itoa(mid) + "|" + itoa(tot) + "/" + itoa(ok) +
		"|" + itoa(p50) + "/" + itoa(p95) + "]"
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}
