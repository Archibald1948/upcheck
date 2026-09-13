package store

import (
	"context"
	"testing"
	"time"
)

// TestUptimeExactAcrossSeam 은 롤업 구간과 원본 구간을 이어 붙인 업타임이
// 원본만으로 센 값과 **정확히** 같은지 본다.
//
// 이게 M2의 핵심 계약이다. 오래된 데이터를 접어도 업타임 %는 안 변해야 한다.
func TestUptimeExactAcrossSeam(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	ids, _ := s.SyncMonitors(ctx, testMonitors("a"))
	id := ids["a"]

	// 구간 시작을 정시에 맞춘다.
	// 롤업 칸이 1시간 단위라, 중간에서 시작하면 첫 칸에 이전 데이터가 섞인다.
	now := time.Date(2026, 3, 1, 12, 30, 0, 0, time.UTC)
	from := time.Date(2026, 3, 1, 6, 0, 0, 0, time.UTC)

	seed(t, s, id, from, now.Sub(from), 30*time.Second, 9, 120)
	wantTotal, wantOK := rawUptime(t, s, id, from, now)

	// 롤업 전: 전부 원본에서 읽는다
	before, err := s.Uptime(ctx, id, from, now)
	if err != nil {
		t.Fatal(err)
	}
	if before.Total != wantTotal || before.OK != wantOK {
		t.Fatalf("롤업 전 업타임 (%d/%d) ≠ 원본 (%d/%d)",
			before.OK, before.Total, wantOK, wantTotal)
	}

	// 롤업 후: 12시 이전은 롤업에서, 12:00~12:30 은 원본에서 읽어 합친다
	if _, err := s.Rollup(ctx, now); err != nil {
		t.Fatal(err)
	}
	after, err := s.Uptime(ctx, id, from, now)
	if err != nil {
		t.Fatal(err)
	}

	if after.Total != wantTotal || after.OK != wantOK {
		t.Errorf("롤업 후 업타임 (%d/%d) ≠ 원본 (%d/%d) — 경계에서 중복이나 누락이 났다",
			after.OK, after.Total, wantOK, wantTotal)
	}
	t.Logf("업타임 %.4f%% (%d/%d) — 롤업 전후 동일", after.Percent(), after.OK, after.Total)
}

// TestUptimeSurvivesPrune 은 원본이 정리된 뒤에도 업타임이 유지되는지 본다.
func TestUptimeSurvivesPrune(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	ids, _ := s.SyncMonitors(ctx, testMonitors("a"))
	id := ids["a"]

	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	from := now.Add(-30 * 24 * time.Hour) // 30일 전

	// 30일 전 하루치를 심는다 (원본 보관 기간 7일을 넘김)
	seed(t, s, id, from, 24*time.Hour, 5*time.Minute, 8, 300)
	wantTotal, wantOK := rawUptime(t, s, id, from, now)

	if err := s.Maintain(ctx, now, DefaultRetention); err != nil {
		t.Fatal(err)
	}

	// 원본은 지워졌어야 한다
	if n := countRows(t, s, "checks"); n != 0 {
		t.Errorf("원본이 %d행 남았다 (정리됐어야 한다)", n)
	}

	// 그런데 업타임은 그대로여야 한다
	got, err := s.Uptime(ctx, id, from, now)
	if err != nil {
		t.Fatal(err)
	}
	if got.Total != wantTotal || got.OK != wantOK {
		t.Errorf("정리 후 업타임 (%d/%d) ≠ 정리 전 (%d/%d)",
			got.OK, got.Total, wantOK, wantTotal)
	}
	t.Logf("원본 %d행을 지우고도 업타임 %.4f%% 유지", wantTotal, got.Percent())
}

// TestLatencyExactWhenRawAvailable 은 원본이 남아 있으면
// 정확한 백분위수를 주고 Approx 가 false 인지 본다.
func TestLatencyExactWhenRawAvailable(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	ids, _ := s.SyncMonitors(ctx, testMonitors("a"))
	id := ids["a"]

	now := time.Date(2026, 3, 1, 12, 30, 0, 0, time.UTC)
	// 롤업을 돌리지 않으므로 전부 원본에서 읽는다
	var rows []CheckRow
	for i := range 100 {
		rows = append(rows, CheckRow{
			MonitorID: id, CheckedAt: now.Add(-time.Duration(i) * time.Second),
			OK: true, StatusCode: 200, LatencyMS: int64(i + 1), // 1..100ms
		})
	}
	if err := s.InsertChecks(ctx, rows); err != nil {
		t.Fatal(err)
	}

	lat, err := s.Latency(ctx, id, now.Add(-time.Hour), now)
	if err != nil {
		t.Fatal(err)
	}

	if lat.Approx {
		t.Error("원본이 다 있는데 Approx 가 true 다")
	}
	if lat.Samples != 100 {
		t.Errorf("표본 %d개, 기대값 100개", lat.Samples)
	}
	// 1..100 의 p50 = 50번째 = 50ms, p95 = 95번째 = 95ms
	if lat.P50 != 50*time.Millisecond {
		t.Errorf("p50 = %v, 기대값 50ms", lat.P50)
	}
	if lat.P95 != 95*time.Millisecond {
		t.Errorf("p95 = %v, 기대값 95ms", lat.P95)
	}
	if lat.Max != 100*time.Millisecond {
		t.Errorf("max = %v, 기대값 100ms", lat.Max)
	}
}

// TestLatencyMarkedApproxAfterRollup 은 롤업 구간이 섞이면
// Approx 가 true 로 표시되는지 본다.
//
// 백분위수는 합칠 수 없어서(p95 들의 평균은 p95 가 아니다) 근사일 수밖에 없다.
// 중요한 건 "근사라는 사실을 숨기지 않는 것"이다.
func TestLatencyMarkedApproxAfterRollup(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	ids, _ := s.SyncMonitors(ctx, testMonitors("a"))
	id := ids["a"]

	now := time.Date(2026, 3, 1, 12, 30, 0, 0, time.UTC)
	seed(t, s, id, now.Add(-6*time.Hour), 6*time.Hour, time.Minute, 0, 200)

	if _, err := s.Rollup(ctx, now); err != nil {
		t.Fatal(err)
	}

	lat, err := s.Latency(ctx, id, now.Add(-6*time.Hour), now)
	if err != nil {
		t.Fatal(err)
	}
	if !lat.Approx {
		t.Error("롤업 구간이 섞였는데 Approx 가 false 다")
	}
	if lat.P50 == 0 || lat.P95 == 0 {
		t.Errorf("근사값이라도 0이면 안 된다: %+v", lat)
	}
	t.Logf("근사 p50=%v p95=%v (표본 %d)", lat.P50, lat.P95, lat.Samples)
}

// TestLatencyIgnoresFailedChecks 는 실패한 체크의 시간이
// 백분위수에 섞이지 않는지 본다.
//
// 실패 체크의 '응답시간'은 타임아웃까지 걸린 시간이라, 섞으면 수치가 왜곡된다.
func TestLatencyIgnoresFailedChecks(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	ids, _ := s.SyncMonitors(ctx, testMonitors("a"))
	id := ids["a"]

	now := time.Date(2026, 3, 1, 12, 30, 0, 0, time.UTC)
	rows := []CheckRow{
		{MonitorID: id, CheckedAt: now.Add(-3 * time.Minute), OK: true, LatencyMS: 10},
		{MonitorID: id, CheckedAt: now.Add(-2 * time.Minute), OK: true, LatencyMS: 20},
		// 타임아웃 — 5초짜리. 섞이면 p95 가 5000ms 로 튄다.
		{MonitorID: id, CheckedAt: now.Add(-time.Minute), OK: false, LatencyMS: 5000, Error: "타임아웃"},
	}
	if err := s.InsertChecks(ctx, rows); err != nil {
		t.Fatal(err)
	}

	lat, err := s.Latency(ctx, id, now.Add(-time.Hour), now)
	if err != nil {
		t.Fatal(err)
	}
	if lat.Samples != 2 {
		t.Errorf("표본 %d개 — 성공한 2개만 세야 한다", lat.Samples)
	}
	if lat.Max != 20*time.Millisecond {
		t.Errorf("max = %v — 실패 체크의 5000ms 가 섞였다", lat.Max)
	}
}

// TestUptimeWithNoData 는 데이터가 없을 때 0으로 나누지 않는지 본다.
func TestUptimeWithNoData(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	ids, _ := s.SyncMonitors(ctx, testMonitors("a"))

	now := time.Now()
	u, err := s.Uptime(ctx, ids["a"], now.Add(-24*time.Hour), now)
	if err != nil {
		t.Fatal(err)
	}
	if u.Total != 0 || u.Percent() != 0 {
		t.Errorf("데이터가 없으면 0이어야 한다: %+v", u)
	}
}

// TestSummary 는 상태 페이지에 필요한 값이 한 번에 나오는지 본다.
func TestSummary(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	ids, _ := s.SyncMonitors(ctx, testMonitors("up", "down"))

	now := time.Date(2026, 3, 1, 12, 30, 0, 0, time.UTC)
	seed(t, s, ids["up"], now.Add(-2*time.Hour), 2*time.Hour, time.Minute, 0, 100)

	// down 은 마지막 체크가 실패
	if err := s.InsertChecks(ctx, []CheckRow{
		{MonitorID: ids["down"], CheckedAt: now.Add(-2 * time.Minute), OK: true, LatencyMS: 50},
		{MonitorID: ids["down"], CheckedAt: now.Add(-time.Minute), OK: false, StatusCode: 503, Error: "상태 코드 503"},
	}); err != nil {
		t.Fatal(err)
	}

	rows, err := s.Summary(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("모니터 %d개, 기대값 2개", len(rows))
	}

	byName := map[string]MonitorStatus{}
	for _, r := range rows {
		byName[r.Name] = r
	}

	if !byName["up"].Up() {
		t.Error("up 이 DOWN 으로 나온다")
	}
	if byName["down"].Up() {
		t.Error("down 이 UP 으로 나온다")
	}
	if p := byName["up"].Uptime24h.Percent(); p != 100 {
		t.Errorf("up 의 24h 업타임 %.2f%%, 기대값 100%%", p)
	}
	if p := byName["down"].Uptime24h.Percent(); p != 50 {
		t.Errorf("down 의 24h 업타임 %.2f%%, 기대값 50%%", p)
	}
	if byName["up"].Latency24h.P95 == 0 {
		t.Error("up 의 p95 가 0이다")
	}
}
