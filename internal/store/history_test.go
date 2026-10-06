package store

import (
	"context"
	"testing"
	"time"
)

func TestDailyHistoryHasEveryDay(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	ids, _ := s.SyncMonitors(ctx, testMonitors("a"))

	now := time.Date(2026, 3, 10, 15, 0, 0, 0, time.UTC)
	// 3일 전 하루만 데이터가 있다
	seed(t, s, ids["a"], time.Date(2026, 3, 7, 0, 0, 0, 0, time.UTC), 24*time.Hour, 10*time.Minute, 0, 100)

	days, err := s.DailyHistory(ctx, ids["a"], 90, time.UTC, now)
	if err != nil {
		t.Fatal(err)
	}

	// 데이터가 없는 날도 빠짐없이 90칸이어야 한다
	if len(days) != 90 {
		t.Fatalf("칸 %d개, 기대값 90개", len(days))
	}
	// 오래된 날부터 오늘까지
	if !days[89].Date.Equal(time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("마지막 칸이 오늘이 아니다: %v", days[89].Date)
	}
	for i := 1; i < len(days); i++ {
		if !days[i].Date.After(days[i-1].Date) {
			t.Fatalf("날짜 순서가 틀렸다: [%d]=%v, [%d]=%v", i-1, days[i-1].Date, i, days[i].Date)
		}
	}

	var withData int
	for _, d := range days {
		if d.HasData() {
			withData++
			if d.Date.Day() != 7 {
				t.Errorf("데이터가 엉뚱한 날(%v)에 들어갔다", d.Date)
			}
		}
	}
	if withData != 1 {
		t.Errorf("데이터 있는 날 %d개, 기대값 1개", withData)
	}
}

// TestDailyHistoryAcrossRollupSeam 은 롤업 전후 일별 값이 같은지 본다.
func TestDailyHistoryAcrossRollupSeam(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	ids, _ := s.SyncMonitors(ctx, testMonitors("a"))
	id := ids["a"]

	now := time.Date(2026, 3, 10, 15, 30, 0, 0, time.UTC)
	seed(t, s, id, time.Date(2026, 3, 8, 0, 0, 0, 0, time.UTC), now.Sub(time.Date(2026, 3, 8, 0, 0, 0, 0, time.UTC)), 5*time.Minute, 11, 150)

	before, err := s.DailyHistory(ctx, id, 5, time.UTC, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Rollup(ctx, now); err != nil {
		t.Fatal(err)
	}
	after, err := s.DailyHistory(ctx, id, 5, time.UTC, now)
	if err != nil {
		t.Fatal(err)
	}

	for i := range before {
		if before[i].Total != after[i].Total || before[i].OK != after[i].OK {
			t.Errorf("%s: 롤업 전 %d/%d ≠ 후 %d/%d", before[i].Date.Format(time.DateOnly),
				before[i].OK, before[i].Total, after[i].OK, after[i].Total)
		}
	}
}

// TestDailyHistoryRespectsTimezone 은 날짜 경계가 요청한 시간대 자정인지 본다.
func TestDailyHistoryRespectsTimezone(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	ids, _ := s.SyncMonitors(ctx, testMonitors("a"))
	id := ids["a"]

	seoul, err := time.LoadLocation("Asia/Seoul")
	if err != nil {
		t.Skipf("시간대 데이터가 없다: %v", err)
	}

	at := time.Date(2026, 3, 9, 20, 0, 0, 0, time.UTC) // 서울 기준 3월 10일 05:00
	if err := s.InsertChecks(ctx, []CheckRow{{MonitorID: id, CheckedAt: at, OK: true, LatencyMS: 50}}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 3, 10, 6, 0, 0, 0, time.UTC)

	dayOf := func(loc *time.Location) int {
		days, err := s.DailyHistory(ctx, id, 3, loc, now)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range days {
			if d.HasData() {
				return d.Date.Day()
			}
		}
		return -1
	}

	if got := dayOf(time.UTC); got != 9 {
		t.Errorf("UTC 기준 %d일에 들어갔다, 기대값 9일", got)
	}
	if got := dayOf(seoul); got != 10 {
		t.Errorf("서울 기준 %d일에 들어갔다, 기대값 10일", got)
	}
}

func TestDailyHistoryUptime(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	ids, _ := s.SyncMonitors(ctx, testMonitors("a"))
	id := ids["a"]

	day := time.Date(2026, 3, 9, 0, 0, 0, 0, time.UTC)
	var rows []CheckRow
	for i := range 100 {
		rows = append(rows, CheckRow{
			MonitorID: id, CheckedAt: day.Add(time.Duration(i) * 10 * time.Minute),
			OK: i >= 5, LatencyMS: 120, // 5번 실패, 95번 성공
		})
	}
	if err := s.InsertChecks(ctx, rows); err != nil {
		t.Fatal(err)
	}

	days, err := s.DailyHistory(ctx, id, 2, time.UTC, time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}

	// 100번 × 10분 = 1000분 → 9일 00:00 ~ 16:30 이라 전부 9일에 들어간다
	if got := days[0].Uptime(); got != 95 {
		t.Errorf("9일 업타임 %.2f%%, 기대값 95%%", got)
	}
	if days[0].P50MS != 120 {
		t.Errorf("9일 p50 %dms, 기대값 120ms", days[0].P50MS)
	}
	if days[1].HasData() {
		t.Error("데이터가 없는 10일에 값이 들어갔다")
	}
}

func TestDailyHistoryRejectsBadDays(t *testing.T) {
	s := openTest(t)
	if _, err := s.DailyHistory(context.Background(), 1, 0, time.UTC, time.Now()); err == nil {
		t.Error("days=0 인데 에러가 없다")
	}
}

func TestMonitorLookup(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	ids, _ := s.SyncMonitors(ctx, testMonitors("a"))

	m, err := s.Monitor(ctx, ids["a"])
	if err != nil || m == nil || m.Name != "a" {
		t.Fatalf("Monitor() = %+v, %v", m, err)
	}

	missing, err := s.Monitor(ctx, 99999)
	if err != nil {
		t.Fatalf("없는 id 는 에러가 아니라 nil 이어야 한다: %v", err)
	}
	if missing != nil {
		t.Errorf("없는 id 인데 %+v", missing)
	}
}
