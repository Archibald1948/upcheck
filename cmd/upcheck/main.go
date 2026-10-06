// Command upcheck — 가용성 모니터링 데몬.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"runtime/pprof"
	"sort"
	"strings"
	"syscall"
	"time"

	// distroless 이미지엔 zoneinfo 가 없어 "unknown time zone" 으로 실패한다.
	_ "time/tzdata"

	"github.com/Archibald1948/upcheck/internal/admin"
	"github.com/Archibald1948/upcheck/internal/alert"
	"github.com/Archibald1948/upcheck/internal/api"
	"github.com/Archibald1948/upcheck/internal/checker"
	"github.com/Archibald1948/upcheck/internal/collector"
	"github.com/Archibald1948/upcheck/internal/config"
	"github.com/Archibald1948/upcheck/internal/metrics"
	"github.com/Archibald1948/upcheck/internal/scheduler"
	"github.com/Archibald1948/upcheck/internal/store"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "오류: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		configPath  = flag.String("config", "configs/monitors.yaml", "설정 파일 경로")
		once        = flag.Bool("once", false, "순차적으로 한 번만 체크하고 종료 (M0 동작)")
		schedName   = flag.String("scheduler", "pool", "스케줄러: pool | ticker")
		duration    = flag.Duration("duration", 0, "이 시간만큼 돌고 자동 종료 (0=무한)")
		quiet       = flag.Bool("quiet", false, "개별 결과를 출력하지 않는다 (측정용)")
		dumpGo      = flag.Bool("dump-goroutines", false, "종료 후 남은 goroutine 스택을 출력 (누수 추적)")
		dbPath      = flag.String("db", "upcheck.db", "SQLite 파일 경로")
		report      = flag.Bool("report", false, "체크하지 않고 저장된 현황만 출력")
		maintEvery  = flag.Duration("maintain-every", 10*time.Minute, "롤업·정리 잡 주기")
		httpAddr    = flag.String("http", ":8484", "API 서버 주소 (빈 값이면 끔)")
		apiOnly     = flag.Bool("api-only", false, "체크하지 않고 저장된 데이터로 API 만 띄운다")
		expose      = flag.Bool("expose-details", false, "API 응답에 target 주소·에러 원문·경고를 포함한다")
		adminAddr   = flag.String("admin", "127.0.0.1:8485", "운영 서버 주소 (healthz·metrics·pprof, 빈 값이면 끔)")
		healthcheck = flag.Bool("healthcheck", false, "운영 서버의 준비 상태를 확인하고 종료한다 (도커 HEALTHCHECK 용)")
	)
	flag.Parse()

	if *healthcheck {
		return runHealthcheck(*adminAddr)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	apiOpts := api.Options{ExposeDetails: *expose, DefaultTimezone: time.Local}

	if *apiOnly {
		if *httpAddr == "" {
			return fmt.Errorf("-api-only 에는 -http 주소가 필요하다")
		}
		st, err := store.Open(ctx, *dbPath, log)
		if err != nil {
			return err
		}
		defer st.Close()
		fmt.Printf("API 전용 모드 · DB %s · http://localhost%s/api/status · Ctrl+C 로 종료\n", *dbPath, *httpAddr)
		return api.New(st, log, apiOpts).ListenAndServe(ctx, *httpAddr)
	}

	if *report {
		st, err := store.Open(ctx, *dbPath, log)
		if err != nil {
			return err
		}
		defer st.Close()
		return printSummary(ctx, st)
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	monitors := cfg.EnabledMonitors()

	if *duration > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *duration)
		defer cancel()
	}

	c := checker.New()
	defer c.Close() // 안 닫으면 유휴 커넥션 goroutine 이 IdleConnTimeout 까지 남는다

	if *once {
		return runOnce(ctx, c, monitors)
	}

	var sched scheduler.Scheduler
	switch *schedName {
	case "pool":
		sched = scheduler.NewPool(c, cfg.Workers, scheduler.WithLogger(log))
	case "ticker":
		sched = scheduler.NewTickerScheduler(c, log)
	default:
		return fmt.Errorf("알 수 없는 스케줄러 %q (pool 또는 ticker)", *schedName)
	}

	st, err := store.Open(ctx, *dbPath, log)
	if err != nil {
		return err
	}
	defer st.Close()

	ids, err := st.SyncMonitors(ctx, monitors)
	if err != nil {
		return err
	}

	rules, dispatcher, err := alert.FromConfig(cfg.Alerts, log)
	if err != nil {
		return err
	}

	m := metrics.New()
	m.AddCounter("upcheck_checks_scheduled_total", "스케줄러가 큐에 넣은 작업 수",
		func() float64 { return float64(sched.Stats().Scheduled) })
	m.AddCounter("upcheck_checks_deferred_total", "큐가 가득 차 다음 tick 으로 미룬 횟수",
		func() float64 { return float64(sched.Stats().Deferred) })
	m.AddCounter("upcheck_checks_skipped_total", "이전 체크가 진행 중이라 건너뛴 횟수",
		func() float64 { return float64(sched.Stats().Skipped) })
	m.AddGauge("upcheck_scheduler_max_lag_seconds", "예정 시각 대비 가장 크게 밀린 정도",
		func() float64 { return sched.Stats().MaxLag.Seconds() })
	m.AddCounter("upcheck_notifications_sent_total", "알림 채널로 실제 발송된 수",
		func() float64 { return float64(dispatcher.Stats().Sent) })
	m.AddCounter("upcheck_notifications_failed_total", "알림 발송 실패 수",
		func() float64 { return float64(dispatcher.Stats().Failed) })
	m.AddCounter("upcheck_notifications_dropped_total", "알림 큐가 가득 차 버린 이벤트 수",
		func() float64 { return float64(dispatcher.Stats().Dropped) })

	return runDaemon(ctx, st, ids, sched, monitors, log, daemonOpts{
		quiet:      *quiet,
		dumpGo:     *dumpGo,
		dbPath:     *dbPath,
		maintEvery: *maintEvery,
		rules:      rules,
		dispatcher: dispatcher,
		httpAddr:   *httpAddr,
		apiOpts:    apiOpts,
		adminAddr:  *adminAddr,
		metrics:    m,
	})
}

type daemonOpts struct {
	quiet      bool
	dumpGo     bool
	dbPath     string
	maintEvery time.Duration
	rules      alert.Rules
	dispatcher *alert.Dispatcher
	httpAddr   string
	apiOpts    api.Options
	adminAddr  string
	metrics    *metrics.Metrics
}

// runHealthcheck 은 운영 서버의 /readyz 를 찔러 보고 종료 코드로 답한다.
func runHealthcheck(adminAddr string) error {
	if adminAddr == "" {
		return fmt.Errorf("-healthcheck 에는 -admin 주소가 필요하다")
	}
	// ":8485" 처럼 호스트가 비어 있으면 자기 자신으로 본다.
	host, port, err := net.SplitHostPort(adminAddr)
	if err != nil {
		return fmt.Errorf("잘못된 -admin 주소 %q: %w", adminAddr, err)
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	url := "http://" + net.JoinHostPort(host, port) + "/readyz"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("준비 상태 확인 실패: %w", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("준비되지 않음 (HTTP %d)", resp.StatusCode)
	}
	return nil
}

// runOnce 는 모니터를 하나씩 순차적으로 검사한다.
func runOnce(ctx context.Context, c *checker.Checker, monitors []config.Monitor) error {
	fmt.Printf("모니터 %d개 · 순차 체크 (M0)\n\n", len(monitors))

	var upCount int
	start := time.Now()

	for _, m := range monitors {
		select {
		case <-ctx.Done():
			fmt.Println("\n종료 신호를 받아 중단한다.")
			return nil
		default:
		}

		res := c.Check(ctx, m)
		if res.OK {
			upCount++
		}
		printResult(res)
	}

	elapsed := time.Since(start)
	fmt.Printf("\n총 %d개 중 %d개 정상 · 소요 %v\n",
		len(monitors), upCount, elapsed.Round(time.Millisecond))
	if len(monitors) > 0 {
		fmt.Printf("모니터당 평균 %v — 동시에 돌렸다면 가장 느린 하나만큼만 걸렸을 것이다.\n",
			(elapsed / time.Duration(len(monitors))).Round(time.Millisecond))
	}
	return nil
}

// runDaemon 은 스케줄러를 띄우고 결과를 수집해 DB에 쌓는다.
func runDaemon(
	ctx context.Context,
	st *store.Store,
	ids map[string]int64,
	sched scheduler.Scheduler,
	monitors []config.Monitor,
	log *slog.Logger,
	opts daemonOpts,
) error {
	fmt.Printf("모니터 %d개 · 스케줄러 %s · DB %s\n%s\nCtrl+C 로 종료\n\n",
		len(monitors), sched.Name(), opts.dbPath, alert.Describe(opts.rules, opts.dispatcher))

	targets := make(map[string]string, len(monitors))
	for _, m := range monitors {
		targets[m.Name] = m.Target
	}

	// 실패할 수 있는 준비는 goroutine 을 띄우기 전에 끝낸다. 뒤에서 return 하면 goroutine 이 샌다.
	engine := alert.NewEngine(st, ids, opts.rules, opts.dispatcher, log)
	// 재시작 때 진행 중 장애로 알림이 다시 가는 걸 막는다.
	if err := engine.Restore(ctx); err != nil {
		return err
	}

	var apiLn net.Listener
	if opts.httpAddr != "" {
		ln, err := api.Listen(opts.httpAddr)
		if err != nil {
			return err
		}
		apiLn = ln
	}

	var adminLn net.Listener
	if opts.adminAddr != "" {
		ln, err := admin.Listen(opts.adminAddr)
		if err != nil {
			if apiLn != nil {
				apiLn.Close()
			}
			return err
		}
		adminLn = ln
	}

	// 여기서부터 goroutine 을 띄운다. 이후로는 에러로 일찍 return 하지 않는다.

	dispatchDone := make(chan struct{})
	go func() {
		defer close(dispatchDone)
		opts.dispatcher.Run(ctx)
	}()

	var apiDone chan error
	if apiLn != nil {
		apiDone = make(chan error, 1)
		srv := api.New(st, log, opts.apiOpts)
		go func() { apiDone <- srv.Serve(ctx, apiLn) }()
		fmt.Printf("API   http://localhost%s/api/status\n", opts.httpAddr)
	}

	var adminDone chan error
	if adminLn != nil {
		adminDone = make(chan error, 1)
		srv := admin.New(log, opts.metrics.Registry(), st)
		go func() { adminDone <- srv.Serve(ctx, adminLn) }()
		fmt.Printf("운영  http://localhost%s/metrics · /healthz · /debug/pprof/\n", opts.adminAddr)
	}
	fmt.Println()

	start := time.Now()
	results := sched.Run(ctx, monitors)

	maintDone := make(chan struct{})
	go func() {
		defer close(maintDone)
		st.RunMaintenance(ctx, opts.maintEvery, store.DefaultRetention)
	}()

	tally := newTally()

	peakGoroutines := runtime.NumGoroutine()

	coll := collector.New(st, ids, log)
	opts.metrics.AddCounter("upcheck_checks_written_total", "DB에 저장된 체크 행 수",
		func() float64 { return float64(coll.Stats().Written) })
	opts.metrics.AddCounter("upcheck_check_write_failures_total", "저장에 실패한 체크 행 수",
		func() float64 { return float64(coll.Stats().Failed) })
	opts.metrics.AddCounter("upcheck_alerts_total", "판정 엔진이 발송하기로 정한 알림 수",
		func() float64 { return float64(engine.Stats().Sent) })
	opts.metrics.AddCounter("upcheck_alerts_suppressed_total", "쿨다운으로 억제된 알림 수",
		func() float64 { return float64(engine.Stats().Suppressed) })

	err := coll.Run(ctx, results, func(res checker.Result) {
		tally.add(res)
		opts.metrics.ObserveCheck(res)
		engine.Observe(ctx, res, targets[res.Monitor])

		if n := runtime.NumGoroutine(); n > peakGoroutines {
			peakGoroutines = n
		}
		if !opts.quiet {
			printResult(res)
		}
	})
	if err != nil {
		return err
	}

	// collector 가 반환한 뒤에야 닫아야 발송 중인 알림을 잃지 않는다.
	opts.dispatcher.Close()
	<-dispatchDone

	<-maintDone

	// 기다리지 않으면 defer st.Close() 가 먼저 돌아 응답 중인 요청이 닫힌 DB 를 만난다.
	if apiDone != nil {
		if err := <-apiDone; err != nil {
			log.Error("API 서버 종료 중 오류", "err", err)
		}
	}
	if adminDone != nil {
		if err := <-adminDone; err != nil {
			log.Error("운영 서버 종료 중 오류", "err", err)
		}
	}

	shutdown := time.Since(start)
	fmt.Printf("\n── 종료 (구동 %v) ──\n", shutdown.Round(time.Millisecond))
	tally.print()
	printStats(sched.Stats())

	printAlertStats(engine.Stats(), opts.dispatcher)

	cs := coll.Stats()
	fmt.Printf("DB: 저장 %d행", cs.Written)
	if cs.Failed > 0 {
		fmt.Printf(" · 저장실패 %d행", cs.Failed)
	}
	if cs.Dropped > 0 {
		fmt.Printf(" · 버림 %d행", cs.Dropped)
	}
	fmt.Println()

	fmt.Printf("goroutine: 최대 %d개 · 종료 후 %d개\n", peakGoroutines, runtime.NumGoroutine())

	if opts.dumpGo {
		fmt.Println("\n=== 남은 goroutine 스택 ===")
		_ = pprof.Lookup("goroutine").WriteTo(os.Stdout, 1)
	}

	fmt.Printf("\n누적 현황을 보려면: upcheck -db %s -report\n", opts.dbPath)
	return nil
}

func printAlertStats(es alert.Stats, d *alert.Dispatcher) {
	ds := d.Stats()
	if es.Sent == 0 && es.Suppressed == 0 && ds.Sent == 0 && ds.Failed == 0 {
		return
	}
	fmt.Printf("알림: 판정 %d건 · 쿨다운 억제 %d건", es.Sent, es.Suppressed)
	if d.Enabled() {
		fmt.Printf(" · 발송 %d · 실패 %d · 큐넘침 %d", ds.Sent, ds.Failed, ds.Dropped)
	}
	fmt.Println()
}

// printSummary 는 DB에 쌓인 집계를 보여준다. (-report)
func printSummary(ctx context.Context, st *store.Store) error {
	now := time.Now()
	rows, err := st.Summary(ctx, now)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		fmt.Println("등록된 모니터가 없다. 먼저 upcheck 를 한 번 구동하자.")
		return nil
	}

	total, err := st.CountChecks(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("모니터 %d개 · 보관 중인 원본 체크 %d행 · %s 기준\n\n",
		len(rows), total, now.Format("2006-01-02 15:04:05"))

	fmt.Printf("%-4s %-5s %-20s %9s %9s %9s %9s %9s\n",
		"상태", "타입", "모니터", "24h", "7d", "30d", "p50", "p95")
	fmt.Println(strings.Repeat("─", 84))

	for _, r := range rows {
		state, typ := "  ? ", "-"
		if r.LastCheck != nil {
			state = " UP "
			if !r.Up() {
				state = "DOWN"
			}
			typ = r.LastCheck.Type
		}
		fmt.Printf("%-4s %-5s %-20s %8s %9s %9s %9s %9s\n",
			state, typ, truncate(r.Name, 20),
			pct(r.Uptime24h), pct(r.Uptime7d), pct(r.Uptime30d),
			dur(r.Latency24h.P50), dur(r.Latency24h.P95))
	}

	printWarnings(rows)

	if err := printIncidents(ctx, st, now); err != nil {
		return err
	}

	for _, r := range rows {
		if r.Latency24h.Approx {
			fmt.Println("\n※ p50/p95 중 일부는 시간별 롤업에서 근사한 값이다.")
			fmt.Println("  백분위수는 합칠 수 없어서, 원본이 정리된 구간은 정확하지 않다.")
			fmt.Println("  업타임 %는 개수 합이라 롤업 구간도 정확하다.")
			break
		}
	}
	return nil
}

// printWarnings 는 지금 걸려 있는 경고를 모아 보여준다.
func printWarnings(rows []store.MonitorStatus) {
	var warned []store.MonitorStatus
	for _, r := range rows {
		if r.LastCheck != nil && r.LastCheck.Warning != "" {
			warned = append(warned, r)
		}
	}
	if len(warned) == 0 {
		return
	}

	fmt.Printf("\n경고 %d건\n", len(warned))
	fmt.Println(strings.Repeat("─", 84))
	for _, r := range warned {
		fmt.Printf("⚠  %-20s %s\n", truncate(r.Name, 20), r.LastCheck.Warning)
	}
}

// printIncidents 는 최근 장애 이력을 보여준다.
func printIncidents(ctx context.Context, st *store.Store, now time.Time) error {
	incidents, err := st.RecentIncidents(ctx, 10)
	if err != nil {
		return err
	}
	if len(incidents) == 0 {
		return nil
	}

	fmt.Printf("\n최근 장애 %d건\n", len(incidents))
	fmt.Println(strings.Repeat("─", 78))
	for _, i := range incidents {
		state := "해소"
		if i.ResolvedAt == nil {
			state = "진행중"
		}
		fmt.Printf("%-6s %-22s %s  (%v)  %s\n",
			state, truncate(i.MonitorName, 22),
			i.StartedAt.Local().Format("01-02 15:04:05"),
			i.Duration(now).Round(time.Second),
			truncate(i.Cause, 30))
	}
	return nil
}

// pct 는 업타임을 표시용 문자열로 바꾼다. 표본이 없으면 "-".
func pct(u store.Uptime) string {
	if u.Total == 0 {
		return "-"
	}
	return fmt.Sprintf("%.2f%%", u.Percent())
}

// dur 은 응답시간을 표시용 문자열로 바꾼다.
func dur(d time.Duration) string {
	if d == 0 {
		return "-"
	}
	return d.Round(time.Millisecond).String()
}

// monitorTally 는 모니터 하나의 누적 결과다.
type monitorTally struct {
	up, down   int
	latencySum time.Duration
	lastErr    error
}

type tally struct {
	byMonitor map[string]*monitorTally
	total     int
}

func newTally() *tally {
	return &tally{byMonitor: make(map[string]*monitorTally)}
}

func (t *tally) add(r checker.Result) {
	t.total++
	mt := t.byMonitor[r.Monitor]
	if mt == nil {
		mt = &monitorTally{}
		t.byMonitor[r.Monitor] = mt
	}
	if r.OK {
		mt.up++
		mt.latencySum += r.Latency
	} else {
		mt.down++
		mt.lastErr = r.Err
	}
}

func (t *tally) print() {
	if t.total == 0 {
		fmt.Println("수집된 결과가 없다.")
		return
	}

	names := make([]string, 0, len(t.byMonitor))
	for name := range t.byMonitor {
		names = append(names, name)
	}
	sort.Strings(names)

	fmt.Printf("체크 %d회 · 모니터 %d개\n\n", t.total, len(names))
	fmt.Printf("%-24s %6s %6s %9s %8s\n", "모니터", "UP", "DOWN", "업타임", "평균응답")
	for _, name := range names {
		mt := t.byMonitor[name]
		n := mt.up + mt.down
		uptime := float64(mt.up) / float64(n) * 100

		avg := "-"
		if mt.up > 0 {
			avg = (mt.latencySum / time.Duration(mt.up)).Round(time.Millisecond).String()
		}
		fmt.Printf("%-24s %6d %6d %8.1f%% %8s\n", truncate(name, 24), mt.up, mt.down, uptime, avg)
	}
}

func printStats(s scheduler.Stats) {
	// Deferred 는 '버린 체크 수'가 아니라 '미룬 횟수'다. 같은 모니터가 여러 번 세어질 수 있다.
	fmt.Printf("\n스케줄러: 예약 %d · 큐가득미룸 %d · 진행중건너뜀 %d · 최대지연 %v\n",
		s.Scheduled, s.Deferred, s.Skipped, s.MaxLag.Round(time.Millisecond))
	if s.Deferred > 0 {
		fmt.Println("  → 큐가 가득 찬다. workers 를 늘리거나 interval 을 늘려야 한다.")
	}
	if s.Skipped > 0 {
		fmt.Println("  → 체크가 주기보다 오래 걸린다. timeout_ms 나 interval_sec 을 점검하자.")
	}
}

func printResult(r checker.Result) {
	status := "UP  "
	if !r.OK {
		status = "DOWN"
	}

	detail := resultDetail(r)
	if r.Warning != "" {
		detail += "  ⚠ " + r.Warning
	}

	fmt.Printf("[%s] %-5s %-24s %8v  %s\n",
		status, r.Type, truncate(r.Monitor, 24), r.Latency.Round(time.Millisecond), detail)
}

// resultDetail 은 타입에 맞는 한 줄 설명을 만든다.
func resultDetail(r checker.Result) string {
	if !r.OK {
		return r.Err.Error()
	}
	if r.Detail != "" {
		return r.Detail
	}
	if r.Type == config.TypeHTTP {
		return fmt.Sprintf("%d", r.StatusCode)
	}
	return "정상"
}

// truncate 는 문자열이 길면 rune 단위로 잘라낸다 (바이트로 자르면 한글이 깨진다).
func truncate(s string, n int) string {
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	return string(rs[:n-1]) + "…"
}
