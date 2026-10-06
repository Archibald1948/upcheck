package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Archibald1948/upcheck/internal/config"
)

// TestRollupAtScale 은 모니터 100개 × 60초 주기에서 롤업이 얼마나 줄이는지 잰다.
func TestRollupAtScale(t *testing.T) {
	if testing.Short() {
		t.Skip("-short 모드에서는 건너뛴다")
	}

	const (
		monitorCount = 100
		interval     = time.Minute
		span         = 24 * time.Hour
	)

	dir := t.TempDir()
	path := filepath.Join(dir, "scale.db")
	ctx := context.Background()

	s, err := Open(ctx, path, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	names := make([]config.Monitor, monitorCount)
	for i := range names {
		names[i] = config.Monitor{
			Name: fmt.Sprintf("m%03d", i), Type: "http",
			Target: "https://example.test", IntervalSec: 60,
			TimeoutMS: 5000, ExpectedStatus: 200,
		}
	}
	ids, err := s.SyncMonitors(ctx, names)
	if err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC)
	from := now.Add(-span)

	start := time.Now()
	batch := make([]CheckRow, 0, 5000)
	var inserted int
	for _, m := range names {
		id := ids[m.Name]
		n := 0
		for at := from; at.Before(now); at = at.Add(interval) {
			n++
			batch = append(batch, CheckRow{
				MonitorID: id, CheckedAt: at, OK: n%97 != 0,
				StatusCode: 200, LatencyMS: int64(80 + n%120),
			})
			if len(batch) == cap(batch) {
				if err := s.InsertChecks(ctx, batch); err != nil {
					t.Fatal(err)
				}
				inserted += len(batch)
				batch = batch[:0]
			}
		}
	}
	if len(batch) > 0 {
		if err := s.InsertChecks(ctx, batch); err != nil {
			t.Fatal(err)
		}
		inserted += len(batch)
	}
	insertTime := time.Since(start)

	rawSize := dbSize(t, s, path)
	rawRows := countRows(t, s, "checks")

	start = time.Now()
	res, err := s.Rollup(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	rollupTime := time.Since(start)

	// 원본 정리 (보관 기간을 0으로 줘서 전부 접혔다고 가정)
	if _, err := s.Prune(ctx, now, Retention{Raw: 0, Hourly: 90 * 24 * time.Hour}); err != nil {
		t.Fatal(err)
	}
	// SQLite 는 DELETE 만으로는 파일 크기를 줄이지 않는다.
	if _, err := s.db.ExecContext(ctx, `VACUUM`); err != nil {
		t.Fatal(err)
	}

	hourlyRows := countRows(t, s, "checks_hourly")
	afterSize := dbSize(t, s, path)

	t.Logf("모니터 %d개 × %v 주기 × %v", monitorCount, interval, span)
	t.Logf("  원본      : %7d행 · %5.1f MB · 저장 %v", rawRows, mb(rawSize), insertTime.Round(time.Millisecond))
	t.Logf("  롤업      : %7d행 · 소요 %v (%d칸, %d행 접음)",
		hourlyRows, rollupTime.Round(time.Millisecond), res.Buckets, res.Rows)
	t.Logf("  정리 후   : %7d행 · %5.1f MB", hourlyRows, mb(afterSize))
	t.Logf("  → 행 %.0f분의 1, 용량 %.0f분의 1",
		float64(rawRows)/float64(hourlyRows), float64(rawSize)/float64(afterSize))

	if rawRows < 140_000 {
		t.Errorf("원본이 %d행 — 14만 행 언저리여야 한다", rawRows)
	}
	// 100개 모니터 × 24시간 = 2400칸
	if hourlyRows != monitorCount*24 {
		t.Errorf("롤업이 %d행 — %d행이어야 한다", hourlyRows, monitorCount*24)
	}
}

// dbSize 는 DB가 실제로 차지하는 크기를 잰다.
// wal_checkpoint(TRUNCATE) 로 WAL 내용을 본 파일에 반영하고 WAL 을 비운 뒤 잰다.
func dbSize(t *testing.T, s *Store, path string) int64 {
	t.Helper()
	if _, err := s.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatalf("WAL 체크포인트 실패: %v", err)
	}
	var total int64
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if fi, err := os.Stat(path + suffix); err == nil {
			total += fi.Size()
		}
	}
	return total
}

func mb(bytes int64) float64 { return float64(bytes) / (1024 * 1024) }
