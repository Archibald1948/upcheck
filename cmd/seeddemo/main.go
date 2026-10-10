package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"os"
	"time"

	"github.com/Archibald1948/upcheck/internal/alert"
	"github.com/Archibald1948/upcheck/internal/checker"
	"github.com/Archibald1948/upcheck/internal/config"
	"github.com/Archibald1948/upcheck/internal/store"
)

type outage struct {
	daysAgo  float64       // 지금부터 며칠 전에 시작하나 (소수 가능)
	duration time.Duration // 얼마나 지속되나
	cause    string
}

type scenario struct {
	monitor  config.Monitor
	baseMS   int     // 평소 응답시간
	jitterMS int     // 응답시간 흔들림 폭
	blipRate float64 // 이유 없이 한 번 실패할 확률 (임계치 때문에 장애가 안 되어야 한다)
	outages  []outage
	slowDays []int // 이 날(며칠 전)은 응답이 3배 느리다
}

func scenarios() []scenario {
	return []scenario{
		{
			monitor: config.Monitor{Name: "웹사이트", Type: "http", Target: "https://www.example.com", ExpectedStatus: 200},
			baseMS:  180, jitterMS: 60, blipRate: 0.0002,
			outages: []outage{
				{daysAgo: 71.3, duration: 42 * time.Minute, cause: "상태 코드 502 (기대값 200)"},
				{daysAgo: 29.6, duration: 2*time.Hour + 10*time.Minute, cause: "타임아웃"},
			},
			slowDays: []int{29, 28},
		},
		{
			monitor: config.Monitor{Name: "API 서버", Type: "http", Target: "https://api.example.com/health", ExpectedStatus: 200},
			baseMS:  95, jitterMS: 30, blipRate: 0.0001,
			outages: []outage{
				{daysAgo: 55.1, duration: 18 * time.Minute, cause: "상태 코드 503 (기대값 200)"},
				{daysAgo: 12.4, duration: 35 * time.Minute, cause: "상태 코드 500 (기대값 200)"},
			},
		},
		{
			monitor: config.Monitor{Name: "결제 게이트웨이", Type: "http", Target: "https://pay.example.com/health", ExpectedStatus: 200},
			baseMS:  240, jitterMS: 90, blipRate: 0.0004,
			outages: []outage{
				// 짧게 여러 번 — 플래핑
				{daysAgo: 4.50, duration: 20 * time.Minute, cause: "타임아웃"},
				{daysAgo: 4.48, duration: 25 * time.Minute, cause: "타임아웃"},
				{daysAgo: 4.45, duration: 15 * time.Minute, cause: "타임아웃"},
			},
			slowDays: []int{4},
		},
		{
			monitor: config.Monitor{Name: "데이터베이스", Type: "tcp", Target: "db.internal:5432"},
			baseMS:  3, jitterMS: 2, blipRate: 0,
		},
		{
			monitor: config.Monitor{Name: "TLS 인증서", Type: "tls", Target: "www.example.com:443"},
			baseMS:  40, jitterMS: 15, blipRate: 0,
		},
		{
			monitor: config.Monitor{Name: "알림 워커", Type: "http", Target: "https://worker.internal/health", ExpectedStatus: 200},
			baseMS:  60, jitterMS: 20, blipRate: 0.0001,
			outages: []outage{
				{daysAgo: 43.0, duration: 6 * time.Hour, cause: "dial tcp: connection refused"},
				// 지금 진행 중인 장애 — 상태 페이지 상단 배너를 확인하려고
				{daysAgo: 0.02, duration: 24 * time.Hour, cause: "dial tcp: connection refused"},
			},
		},
	}
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "오류: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		dbPath   = flag.String("db", "demo.db", "만들 SQLite 파일")
		days     = flag.Int("days", 90, "며칠치를 만들까")
		interval = flag.Duration("interval", 5*time.Minute, "체크 간격")
		force    = flag.Bool("force", false, "파일이 이미 있으면 지우고 새로 만든다")
	)
	flag.Parse()

	if err := prepareFile(*dbPath, *force); err != nil {
		return err
	}

	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	st, err := store.Open(ctx, *dbPath, log)
	if err != nil {
		return err
	}
	defer st.Close()

	scs := scenarios()
	monitors := make([]config.Monitor, len(scs))
	for i, sc := range scs {
		m := sc.monitor
		m.IntervalSec = int(interval.Seconds())
		m.TimeoutMS = 5000
		monitors[i] = m
	}
	ids, err := st.SyncMonitors(ctx, monitors)
	if err != nil {
		return err
	}

	now := time.Now().UTC().Truncate(time.Minute)
	from := now.AddDate(0, 0, -*days)

	// 시뮬레이션 시각을 따라가는 시계. 쿨다운 계산이 가짜 시간 위에서 돌게 한다.
	simNow := from
	engine := alert.NewEngine(st, ids, alert.DefaultRules, nil, log,
		alert.WithClock(func() time.Time { return simNow }))

	// 시드 고정 — 매번 같은 데이터.
	rng := rand.New(rand.NewPCG(2026, 914))

	fmt.Printf("%d일 × %d개 모니터 × %v 간격 데모 데이터 생성\n", *days, len(scs), *interval)
	start := time.Now()

	const batchSize = 5000
	batch := make([]store.CheckRow, 0, batchSize)
	var total int

	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if err := st.InsertChecks(ctx, batch); err != nil {
			return err
		}
		total += len(batch)
		batch = batch[:0]
		return nil
	}

	// 모니터별로 몰아서 돌면 장애 이력의 id 순서가 시간 순서와 어긋난다.
	for at := from; !at.After(now); at = at.Add(*interval) {
		simNow = at
		for i, sc := range scs {
			res := simulate(sc, at, now, rng)
			engine.Observe(ctx, res, monitors[i].Target)

			row := store.CheckRow{
				MonitorID: ids[sc.monitor.Name], Type: sc.monitor.Type,
				CheckedAt: res.CheckedAt, OK: res.OK, StatusCode: res.StatusCode,
				LatencyMS: res.Latency.Milliseconds(),
			}
			if res.Err != nil {
				row.Error = res.Err.Error()
			}
			batch = append(batch, row)
			if len(batch) == batchSize {
				if err := flush(); err != nil {
					return err
				}
			}
		}
	}
	if err := flush(); err != nil {
		return err
	}
	fmt.Printf("  체크 %d행 저장 · %v\n", total, time.Since(start).Round(time.Millisecond))

	start = time.Now()
	roll, err := st.Rollup(ctx, now)
	if err != nil {
		return err
	}
	prune, err := st.Prune(ctx, now, store.DefaultRetention)
	if err != nil {
		return err
	}
	fmt.Printf("  롤업 %d칸 · 원본 %d행 정리 (7일 보관) · %v\n",
		roll.Buckets, prune.RawDeleted, time.Since(start).Round(time.Millisecond))

	incidents, err := st.RecentIncidents(ctx, 100)
	if err != nil {
		return err
	}
	var open int
	for _, inc := range incidents {
		if inc.ResolvedAt == nil {
			open++
		}
	}
	fmt.Printf("  장애 이력 %d건 (진행 중 %d건) — 판정 엔진이 기록\n", len(incidents), open)
	fmt.Printf("\n완료. 다음으로:\n  go run ./cmd/upcheck -db %s -api-only\n", *dbPath)
	return nil
}

// simulate 는 시각 at 에 모니터가 어땠을지 만들어 낸다.
func simulate(sc scenario, at, now time.Time, rng *rand.Rand) checker.Result {
	res := checker.Result{
		Monitor:   sc.monitor.Name,
		Type:      sc.monitor.Type,
		CheckedAt: at,
	}

	for _, o := range sc.outages {
		start := now.Add(-time.Duration(o.daysAgo * float64(24*time.Hour)))
		if !at.Before(start) && at.Before(start.Add(o.duration)) {
			res.Latency = 5 * time.Second
			res.Err = errors.New(o.cause)
			return res
		}
	}

	// 가끔 한 번씩 실패 — 임계치(3회) 때문에 장애로 확정되면 안 된다
	if rng.Float64() < sc.blipRate {
		res.Latency = 5 * time.Second
		res.Err = errors.New("타임아웃")
		return res
	}

	ms := sc.baseMS + rng.IntN(sc.jitterMS*2+1) - sc.jitterMS
	daysAgo := int(now.Sub(at).Hours() / 24)
	for _, d := range sc.slowDays {
		if d == daysAgo {
			ms *= 3
		}
	}
	res.OK = true
	res.StatusCode = 200
	if sc.monitor.Type != "http" {
		res.StatusCode = 0
	}
	res.Latency = time.Duration(max(ms, 1)) * time.Millisecond
	return res
}

// prepareFile 은 기존 파일을 실수로 덮어쓰지 않게 막는다. -force 를 줘야만 지운다.
func prepareFile(path string, force bool) error {
	if _, err := os.Stat(path); err == nil {
		if !force {
			return fmt.Errorf("%s 가 이미 있다. 지우고 새로 만들려면 -force", path)
		}
		for _, suffix := range []string{"", "-wal", "-shm"} {
			if err := os.Remove(path + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("기존 파일 삭제 실패: %w", err)
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("파일 확인 실패: %w", err)
	}
	return nil
}
