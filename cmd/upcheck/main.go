// Command upcheck — 가용성 모니터링 데몬.
//
// M0 단계: 설정을 읽어 모니터를 "순차적으로" 한 번씩 검사하고 결과를 출력한다.
// 일부러 동시성을 쓰지 않는다. 모니터 수를 늘렸을 때 얼마나 느려지는지
// 직접 재보는 게 M1(스케줄러 + 워커풀)의 동기이기 때문이다.
//
// package main 이고 func main() 이 있는 패키지만 실행 파일이 된다.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gimseongsu/upcheck/internal/checker"
	"github.com/gimseongsu/upcheck/internal/config"
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
	configPath := flag.String("config", "configs/monitors.yaml", "설정 파일 경로")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}

	monitors := cfg.EnabledMonitors()
	fmt.Printf("모니터 %d개 · 순차 체크 시작 (M0)\n\n", len(monitors))

	// signal.NotifyContext 는 SIGINT/SIGTERM 을 받으면 자동으로 취소되는
	// context 를 만들어준다. Ctrl+C 를 누르면 ctx.Done() 이 닫히고,
	// 그 ctx 를 쓰는 HTTP 요청이 즉시 끊긴다.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	c := checker.New()

	var upCount int
	start := time.Now()

	// ── 여기가 M0의 핵심이자 문제점 ──
	// 모니터를 하나씩, 앞의 것이 끝나야 다음 것을 시작한다.
	// 총 소요시간 = 모든 모니터의 응답시간 합계.
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
	fmt.Printf("\n총 %d개 중 %d개 정상 · 소요 %v\n", len(monitors), upCount, elapsed.Round(time.Millisecond))
	if len(monitors) > 0 {
		fmt.Printf("모니터당 평균 %v — 동시에 돌렸다면 가장 느린 하나만큼만 걸렸을 것이다.\n",
			(elapsed / time.Duration(len(monitors))).Round(time.Millisecond))
	}
	return nil
}

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
