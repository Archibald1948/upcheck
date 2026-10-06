package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/Archibald1948/upcheck/internal/config"
)

// openTest 는 임시 파일 DB 를 연다. WAL·PRAGMA 까지 검증하려고 ":memory:" 를 쓰지 않는다.
func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "test.db"), discardLogger())
	if err != nil {
		t.Fatalf("Open 실패: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func testMonitors(names ...string) []config.Monitor {
	ms := make([]config.Monitor, len(names))
	for i, n := range names {
		ms[i] = config.Monitor{
			Name: n, Type: "http", Target: "https://" + n + ".example",
			IntervalSec: 60, TimeoutMS: 5000, ExpectedStatus: 200,
		}
	}
	return ms
}

// ─────────────── 스키마 ───────────────

func TestOpenIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	ctx := context.Background()

	// 같은 파일을 두 번 열어도 마이그레이션이 다시 돌지 않아야 한다.
	for i := range 2 {
		s, err := Open(ctx, path, discardLogger())
		if err != nil {
			t.Fatalf("%d번째 Open 실패: %v", i+1, err)
		}
		s.Close()
	}
}

func TestWALEnabled(t *testing.T) {
	s := openTest(t)
	var mode string
	if err := s.db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatalf("journal_mode 조회 실패: %v", err)
	}
	if mode != "wal" {
		t.Errorf("journal_mode = %q, WAL 이어야 한다", mode)
	}
}

func TestForeignKeysEnabled(t *testing.T) {
	s := openTest(t)
	var on int
	if err := s.db.QueryRow(`PRAGMA foreign_keys`).Scan(&on); err != nil {
		t.Fatalf("foreign_keys 조회 실패: %v", err)
	}
	if on != 1 {
		t.Error("foreign_keys 가 꺼져 있다 — SQLite 는 기본값이 off 라 켜야 한다")
	}
}

// ─────────────── 모니터 동기화 ───────────────

func TestSyncMonitors(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	ids, err := s.SyncMonitors(ctx, testMonitors("a", "b"))
	if err != nil {
		t.Fatalf("SyncMonitors 실패: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("id 맵 크기 %d, 기대값 2", len(ids))
	}

	t.Run("다시 부르면 같은 id 를 준다", func(t *testing.T) {
		again, err := s.SyncMonitors(ctx, testMonitors("a", "b"))
		if err != nil {
			t.Fatalf("SyncMonitors 실패: %v", err)
		}
		for name, id := range ids {
			if again[name] != id {
				t.Errorf("%s 의 id 가 %d → %d 로 바뀌었다", name, id, again[name])
			}
		}
	})

	t.Run("설정이 바뀌면 갱신된다", func(t *testing.T) {
		m := testMonitors("a")[0]
		m.Target = "https://changed.example"
		m.IntervalSec = 999
		if _, err := s.SyncMonitors(ctx, []config.Monitor{m, testMonitors("b")[0]}); err != nil {
			t.Fatalf("SyncMonitors 실패: %v", err)
		}
		var target string
		var interval int
		err := s.db.QueryRow(`SELECT target, interval_sec FROM monitors WHERE name='a'`).
			Scan(&target, &interval)
		if err != nil {
			t.Fatal(err)
		}
		if target != "https://changed.example" || interval != 999 {
			t.Errorf("갱신 안 됨: target=%q interval=%d", target, interval)
		}
	})

	t.Run("설정에서 빠지면 지우지 않고 비활성화한다", func(t *testing.T) {
		if _, err := s.SyncMonitors(ctx, testMonitors("a")); err != nil {
			t.Fatalf("SyncMonitors 실패: %v", err)
		}
		var enabled int
		if err := s.db.QueryRow(`SELECT enabled FROM monitors WHERE name='b'`).Scan(&enabled); err != nil {
			t.Fatalf("b 가 삭제됐다 (비활성화여야 한다): %v", err)
		}
		if enabled != 0 {
			t.Error("b 가 비활성화되지 않았다")
		}

		active, err := s.Monitors(ctx, true)
		if err != nil {
			t.Fatal(err)
		}
		if len(active) != 1 || active[0].Name != "a" {
			t.Errorf("활성 모니터가 %v", active)
		}
	})
}

// ─────────────── 저장 ───────────────

func TestInsertChecks(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	ids, _ := s.SyncMonitors(ctx, testMonitors("a"))

	now := time.Now().UTC().Truncate(time.Second)
	rows := []CheckRow{
		{MonitorID: ids["a"], CheckedAt: now, OK: true, StatusCode: 200, LatencyMS: 120},
		{MonitorID: ids["a"], CheckedAt: now.Add(time.Second), OK: false, StatusCode: 503, LatencyMS: 90, Error: "상태 코드 503"},
	}
	if err := s.InsertChecks(ctx, rows); err != nil {
		t.Fatalf("InsertChecks 실패: %v", err)
	}

	n, err := s.CountChecks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("저장된 행 %d개, 기대값 2개", n)
	}

	last, err := s.LastCheck(ctx, ids["a"])
	if err != nil {
		t.Fatal(err)
	}
	if last == nil {
		t.Fatal("LastCheck 가 nil 이다")
	}
	if last.OK || last.StatusCode != 503 || last.Error != "상태 코드 503" {
		t.Errorf("마지막 체크가 틀렸다: %+v", last)
	}
	// 시각이 UTC 로 되돌아와야 한다
	if _, off := last.CheckedAt.Zone(); off != 0 {
		t.Errorf("CheckedAt 이 UTC 가 아니다: %v", last.CheckedAt)
	}
}

func TestInsertChecksEmpty(t *testing.T) {
	s := openTest(t)
	if err := s.InsertChecks(context.Background(), nil); err != nil {
		t.Errorf("빈 배치는 조용히 성공해야 한다: %v", err)
	}
}

func TestLastCheckWithNoData(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	ids, _ := s.SyncMonitors(ctx, testMonitors("a"))

	// 한 번도 체크 안 한 모니터는 에러가 아니라 nil 이어야 한다.
	last, err := s.LastCheck(ctx, ids["a"])
	if err != nil {
		t.Fatalf("에러가 아니라 nil 을 줘야 한다: %v", err)
	}
	if last != nil {
		t.Errorf("nil 이어야 하는데 %+v", last)
	}
}

// ─────────────── 백분위수 ───────────────

func TestQuantile(t *testing.T) {
	// nearest-rank: p 백분위 = 오름차순 ceil(p*n) 번째 값
	sorted := []int64{10, 20, 30, 40, 50, 60, 70, 80, 90, 100}

	cases := []struct {
		q    float64
		want int64
	}{
		{0.50, 50},  // ceil(0.5*10) = 5번째
		{0.95, 100}, // ceil(0.95*10) = 10번째
		{1.00, 100},
		{0.10, 10},
	}
	for _, c := range cases {
		if got := quantile(sorted, c.q); got != c.want {
			t.Errorf("quantile(%.2f) = %d, 기대값 %d", c.q, got, c.want)
		}
	}

	if got := quantile(nil, 0.5); got != 0 {
		t.Errorf("빈 표본은 0이어야 한다: %d", got)
	}
}

func TestPercentiles(t *testing.T) {
	// 정렬되지 않은 입력을 줘도 정렬해서 계산해야 한다
	p50, p95, mx := percentiles([]int64{50, 10, 90, 30, 70})
	if p50 != 50 {
		t.Errorf("p50 = %d, 기대값 50", p50)
	}
	if p95 != 90 {
		t.Errorf("p95 = %d, 기대값 90", p95)
	}
	if mx != 90 {
		t.Errorf("max = %d, 기대값 90", mx)
	}
}
