// Command upcheck — 가용성 모니터링 데몬.
//
// 두 가지 모드가 있다.
//
//	-once      모니터를 순차적으로 한 번씩 체크하고 종료 (M0)
//	(기본)     스케줄러를 띄워 주기대로 계속 체크 (M1)
//
// 스케줄러는 두 구현 중 고를 수 있다 (-scheduler pool|ticker).
// 왜 pool 이 기본인지는 docs/08-M1-동시성.md 에 있다.
//
// package main 이고 func main() 이 있는 패키지만 실행 파일이 된다.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"runtime"
	"runtime/pprof"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/Archibald1948/upcheck/internal/alert"
	"github.com/Archibald1948/upcheck/internal/api"
	"github.com/Archibald1948/upcheck/internal/checker"
	"github.com/Archibald1948/upcheck/internal/collector"
	"github.com/Archibald1948/upcheck/internal/config"
	"github.com/Archibald1948/upcheck/internal/scheduler"
	"github.com/Archibald1948/upcheck/internal/store"
)

func main() {
	// main 에서는 에러가 나면 바로 종료해야 해서 로직을 run()으로 뺀다.
	// 이렇게 하면 defer 가 제대로 실행된다. (os.Exit 는 defer 를 무시한다)
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "오류: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		configPath = flag.String("config", "configs/monitors.yaml", "설정 파일 경로")
		once       = flag.Bool("once", false, "순차적으로 한 번만 체크하고 종료 (M0 동작)")
		schedName  = flag.String("scheduler", "pool", "스케줄러: pool | ticker")
		duration   = flag.Duration("duration", 0, "이 시간만큼 돌고 자동 종료 (0=무한)")
		quiet      = flag.Bool("quiet", false, "개별 결과를 출력하지 않는다 (측정용)")
		dumpGo     = flag.Bool("dump-goroutines", false, "종료 후 남은 goroutine 스택을 출력 (누수 추적)")
		dbPath     = flag.String("db", "upcheck.db", "SQLite 파일 경로")
		report     = flag.Bool("report", false, "체크하지 않고 저장된 현황만 출력")
		maintEvery = flag.Duration("maintain-every", 10*time.Minute, "롤업·정리 잡 주기")
		httpAddr   = flag.String("http", ":8484", "API 서버 주소 (빈 값이면 끔)")
		apiOnly    = flag.Bool("api-only", false, "체크하지 않고 저장된 데이터로 API 만 띄운다")
		expose     = flag.Bool("expose-details", false, "API 응답에 target 주소·에러 원문·경고를 포함한다")
	)
	flag.Parse()

	// signal.NotifyContext 는 SIGINT/SIGTERM 을 받으면 자동으로 취소되는
	// context 를 만들어준다. Ctrl+C 를 누르면 ctx.Done() 이 닫히고,
	// 그 ctx 를 쓰는 HTTP 요청이 즉시 끊긴다.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	apiOpts := api.Options{ExposeDetails: *expose, DefaultTimezone: time.Local}

	// -api-only 는 체크 없이 DB 를 읽어 API 만 띄운다.
	// 프론트엔드를 개발할 때 데몬 전체를 돌리지 않아도 된다.
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

	// -report 는 체크 없이 DB만 읽는다. 설정 파일도 필요 없다.
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

	// -duration 이 주어지면 그만큼 뒤에 스스로 취소된다.
	// 자식 context 라서 Ctrl+C(부모 취소)도 여전히 먹는다.
	if *duration > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *duration)
		defer cancel()
	}

	c := checker.New()
	// 종료 시 유휴 커넥션을 정리한다. 안 하면 커넥션과 거기 딸린
	// goroutine 이 IdleConnTimeout 까지 남는다.
	defer c.Close()

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

	// 설정의 모니터를 DB에 반영하고 이름→id 맵을 받는다.
	ids, err := st.SyncMonitors(ctx, monitors)
	if err != nil {
		return err
	}

	rules, dispatcher, err := alert.FromConfig(cfg.Alerts, log)
	if err != nil {
		return err
	}

	return runDaemon(ctx, st, ids, sched, monitors, log, daemonOpts{
		quiet:      *quiet,
		dumpGo:     *dumpGo,
		dbPath:     *dbPath,
		maintEvery: *maintEvery,
		rules:      rules,
		dispatcher: dispatcher,
		httpAddr:   *httpAddr,
		apiOpts:    apiOpts,
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
}

// ─────────────────────────── M0: 순차 ───────────────────────────

// runOnce 는 모니터를 하나씩 순차적으로 검사한다.
//
// 총 소요시간 = 모든 모니터의 응답시간 합계.
// 이 숫자가 M1(동시성)의 동기다. docs/05-M0-순차-체크.md 참고.
func runOnce(ctx context.Context, c *checker.Checker, monitors []config.Monitor) error {
	fmt.Printf("모니터 %d개 · 순차 체크 (M0)\n\n", len(monitors))

	var upCount int
	start := time.Now()

	for _, m := range monitors {
		// 종료 신호가 왔으면 남은 모니터는 건너뛴다.
		// select 에 default 가 있으면 "채널이 준비됐는지 슬쩍 보고
		// 아니면 그냥 지나간다"는 논블로킹 검사가 된다.
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

// ─────────────────────────── M1: 스케줄러 ───────────────────────────

// runDaemon 은 스케줄러를 띄우고 결과를 수집해 DB에 쌓는다.
//
//	scheduler → results → collector → SQLite
//	                  maintenance goroutine → 롤업 · 정리
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

	// 모니터 이름 → 주소. 알림 본문에 넣는다.
	targets := make(map[string]string, len(monitors))
	for _, m := range monitors {
		targets[m.Name] = m.Target
	}

	// ── 실패할 수 있는 준비를 goroutine 을 띄우기 전에 전부 끝낸다 ──
	//
	// 순서가 중요하다. goroutine 을 먼저 띄워 놓고 뒤에서 에러로 return 하면,
	// 그 goroutine 은 아무도 채널을 닫아주지 않아 영원히 기다린다.
	// (docs/06 누수 패턴 2번 — 닫히지 않는 채널에서 받기)

	engine := alert.NewEngine(st, ids, opts.rules, opts.dispatcher, log)
	// 진행 중이던 장애를 복원한다. 재시작할 때마다 같은 장애로
	// 알림이 다시 가는 걸 막는다.
	if err := engine.Restore(ctx); err != nil {
		return err
	}

	// API 서버 포트를 잡는다.
	// 포트 충돌 같은 기동 실패를 goroutine 안에서 만나면 조용히 묻혀서,
	// 체크는 도는데 상태 페이지만 안 뜨는 상태가 된다.
	var apiLn net.Listener
	if opts.httpAddr != "" {
		ln, err := api.Listen(opts.httpAddr)
		if err != nil {
			return err
		}
		apiLn = ln
	}

	// ── 여기서부터 goroutine 을 띄운다. 이후로는 에러로 일찍 return 하지 않는다 ──

	// 알림 발송을 별도 goroutine 으로 띄운다 (스펙 7절 함정).
	// 웹훅 서버가 느려도 체크 루프는 멈추면 안 된다.
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
		fmt.Printf("API http://localhost%s/api/status\n\n", opts.httpAddr)
	}

	start := time.Now()
	results := sched.Run(ctx, monitors)

	// 롤업·정리 잡을 별도 goroutine 으로 띄운다.
	//
	// 알림과 마찬가지로, 정리 잡이 느려도 모니터링은 계속돼야 한다.
	// ctx 가 취소되면 스스로 마지막 롤업을 한 번 돌리고 끝난다.
	maintDone := make(chan struct{})
	go func() {
		defer close(maintDone)
		st.RunMaintenance(ctx, opts.maintEvery, store.DefaultRetention)
	}()

	tally := newTally()

	// goroutine 수의 최댓값을 관찰한다.
	// 두 스케줄러의 차이가 여기서 그대로 드러난다.
	// pool 은 워커 수 + 상수, ticker 는 모니터 수에 비례한다.
	peakGoroutines := runtime.NumGoroutine()

	coll := collector.New(st, ids, log)

	// collector 가 results 를 끝까지 읽는다. 이 함수가 반환했다는 건
	// 채널이 닫혔다는 뜻이고, 곧 스케줄러가 완전히 정리됐다는 뜻이다.
	err := coll.Run(ctx, results, func(res checker.Result) {
		tally.add(res)
		// 상태 전이 판정. 알릴 게 있으면 dispatcher 큐로 들어간다.
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

	// 여기서 순서가 중요하다.
	// collector 가 반환했다 = 더 이상 결과가 없다 = engine 이 이벤트를 안 만든다.
	// 그제서야 이벤트 채널을 닫아야 발송 중인 알림을 잃지 않는다.
	// (보내는 쪽이 닫는다 — 스펙 5절 규칙 2번)
	opts.dispatcher.Close()
	<-dispatchDone

	// 정리 잡이 마지막 롤업까지 끝내기를 기다린다.
	<-maintDone

	// API 서버가 진행 중인 요청을 마치고 내려가기를 기다린다.
	// 이게 끝나기 전에 run() 이 반환하면 defer st.Close() 가 먼저 돌아서,
	// 응답하던 요청이 닫힌 DB 를 만난다.
	if apiDone != nil {
		if err := <-apiDone; err != nil {
			log.Error("API 서버 종료 중 오류", "err", err)
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

	// 종료 후 남은 goroutine 수. 스케줄러가 뒷정리를 제대로 했다면
	// 시작 시점 수준으로 돌아와 있어야 한다. (스펙 6절 완료 기준 1번)
	fmt.Printf("goroutine: 최대 %d개 · 종료 후 %d개\n", peakGoroutines, runtime.NumGoroutine())

	if opts.dumpGo {
		fmt.Println("\n=== 남은 goroutine 스택 ===")
		// pprof.Lookup("goroutine") 은 현재 살아 있는 모든 goroutine 을 준다.
		// 인자 1 은 사람이 읽을 수 있는 형식으로 출력하라는 뜻이다.
		// M6에서 /debug/pprof 로 이걸 HTTP 로 노출한다.
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

// ─────────────────────────── 저장된 현황 출력 ───────────────────────────

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

	// 근사치가 섞였으면 알려준다. 숫자만 보여주고 말면 오해한다.
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
//
// 경고는 실패가 아니라 업타임 표에는 안 나타난다.
// 따로 보여주지 않으면 인증서가 만료될 때까지 아무도 모른다.
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

// ─────────────────────────── 집계 ───────────────────────────

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
	// map 은 make 하지 않으면 nil 이고, nil 맵에 쓰면 panic 이 난다.
	return &tally{byMonitor: make(map[string]*monitorTally)}
}

func (t *tally) add(r checker.Result) {
	t.total++
	// 맵에서 없는 키를 읽으면 제로값(여기선 nil 포인터)이 나온다. panic 이 아니다.
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

	// 맵의 순회 순서는 매번 무작위다. 출력이 흔들리지 않게 키를 정렬한다.
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
	// Deferred 는 '버린 체크 수'가 아니라 '미룬 횟수'다.
	// 미뤄진 모니터는 다음 tick 에 다시 시도하므로, 과부하가 이어지면
	// 같은 모니터가 여러 번 세어진다. 절대값보다 '0이냐 아니냐'를 본다.
	fmt.Printf("\n스케줄러: 예약 %d · 큐가득미룸 %d · 진행중건너뜀 %d · 최대지연 %v\n",
		s.Scheduled, s.Deferred, s.Skipped, s.MaxLag.Round(time.Millisecond))
	if s.Deferred > 0 {
		fmt.Println("  → 큐가 가득 찬다. workers 를 늘리거나 interval 을 늘려야 한다.")
	}
	if s.Skipped > 0 {
		fmt.Println("  → 체크가 주기보다 오래 걸린다. timeout_ms 나 interval_sec 을 점검하자.")
	}
}

// ─────────────────────────── 출력 ───────────────────────────

func printResult(r checker.Result) {
	status := "UP  "
	if !r.OK {
		status = "DOWN"
	}

	detail := resultDetail(r)
	if r.Warning != "" {
		// 경고는 실패가 아니다. 상태는 UP 그대로 두고 문구만 덧붙인다.
		detail += "  ⚠ " + r.Warning
	}

	// %-24s 는 왼쪽 정렬 24칸, %8v 는 오른쪽 정렬 8칸.
	fmt.Printf("[%s] %-5s %-24s %8v  %s\n",
		status, r.Type, truncate(r.Monitor, 24), r.Latency.Round(time.Millisecond), detail)
}

// resultDetail 은 타입에 맞는 한 줄 설명을 만든다.
//
// http 는 상태 코드가 의미 있지만 tcp/tls/dns 는 그런 게 없다.
// 그런 타입에 "0" 을 찍으면 읽는 사람이 헷갈리므로,
// Prober 가 채워 준 Detail 을 쓴다.
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

// truncate 는 문자열이 길면 잘라낸다.
//
// []rune 으로 바꾸는 이유: Go의 string 은 UTF-8 바이트 열이라
// len(s) 는 '바이트 수'다. 한글 한 글자는 3바이트라서 바이트로 자르면 깨진다.
// rune 은 유니코드 코드포인트 하나를 뜻한다.
func truncate(s string, n int) string {
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	return string(rs[:n-1]) + "…"
}
