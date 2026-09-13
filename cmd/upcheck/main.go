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
	"os"
	"os/signal"
	"runtime"
	"runtime/pprof"
	"sort"
	"syscall"
	"time"

	"github.com/Archibald1948/upcheck/internal/checker"
	"github.com/Archibald1948/upcheck/internal/config"
	"github.com/Archibald1948/upcheck/internal/scheduler"
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
	)
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	monitors := cfg.EnabledMonitors()

	// signal.NotifyContext 는 SIGINT/SIGTERM 을 받으면 자동으로 취소되는
	// context 를 만들어준다. Ctrl+C 를 누르면 ctx.Done() 이 닫히고,
	// 그 ctx 를 쓰는 HTTP 요청이 즉시 끊긴다.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

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

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	var sched scheduler.Scheduler
	switch *schedName {
	case "pool":
		sched = scheduler.NewPool(c, cfg.Workers, scheduler.WithLogger(log))
	case "ticker":
		sched = scheduler.NewTickerScheduler(c, log)
	default:
		return fmt.Errorf("알 수 없는 스케줄러 %q (pool 또는 ticker)", *schedName)
	}

	return runDaemon(ctx, sched, monitors, *quiet, *dumpGo)
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

// runDaemon 은 스케줄러를 띄우고 결과를 수집한다.
//
// 스펙 5절 다이어그램의 collector 자리다.
// M2에서 여기에 DB 저장과 상태 전이 판정이 붙는다.
func runDaemon(ctx context.Context, sched scheduler.Scheduler, monitors []config.Monitor, quiet, dumpGoroutines bool) error {
	fmt.Printf("모니터 %d개 · 스케줄러 %s · Ctrl+C 로 종료\n\n", len(monitors), sched.Name())

	start := time.Now()
	results := sched.Run(ctx, monitors)

	tally := newTally()

	// goroutine 수의 최댓값을 관찰한다.
	// 두 스케줄러의 차이가 여기서 그대로 드러난다.
	// pool 은 워커 수 + 상수, ticker 는 모니터 수에 비례한다.
	peakGoroutines := runtime.NumGoroutine()

	// range 로 채널을 읽으면 채널이 닫힐 때까지 계속 받는다.
	// 스케줄러는 ctx 가 취소되고 내부 goroutine 이 전부 정리된 뒤에 닫으므로,
	// 이 루프가 빠져나왔다는 건 곧 "종료가 깔끔하게 끝났다"는 뜻이다.
	for res := range results {
		tally.add(res)
		if n := runtime.NumGoroutine(); n > peakGoroutines {
			peakGoroutines = n
		}
		if !quiet {
			printResult(res)
		}
	}

	shutdown := time.Since(start)
	fmt.Printf("\n── 종료 (구동 %v) ──\n", shutdown.Round(time.Millisecond))
	tally.print()
	printStats(sched.Stats())

	// 종료 후 남은 goroutine 수. 스케줄러가 뒷정리를 제대로 했다면
	// 시작 시점 수준으로 돌아와 있어야 한다. (스펙 6절 완료 기준 1번)
	fmt.Printf("goroutine: 최대 %d개 · 종료 후 %d개\n", peakGoroutines, runtime.NumGoroutine())

	if dumpGoroutines {
		fmt.Println("\n=== 남은 goroutine 스택 ===")
		// pprof.Lookup("goroutine") 은 현재 살아 있는 모든 goroutine 을 준다.
		// 인자 1 은 사람이 읽을 수 있는 형식으로 출력하라는 뜻이다.
		// M6에서 /debug/pprof 로 이걸 HTTP 로 노출한다.
		_ = pprof.Lookup("goroutine").WriteTo(os.Stdout, 1)
	}
	return nil
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
	detail := fmt.Sprintf("%d", r.StatusCode)
	if !r.OK {
		status = "DOWN"
		detail = r.Err.Error()
	}
	// %-24s 는 왼쪽 정렬 24칸, %8v 는 오른쪽 정렬 8칸.
	fmt.Printf("[%s] %-24s %8v  %s\n",
		status, truncate(r.Monitor, 24), r.Latency.Round(time.Millisecond), detail)
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
