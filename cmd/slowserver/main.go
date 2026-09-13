// Command slowserver — 일부러 느리게 응답하는 로컬 테스트 서버.
//
// 두 가지 일을 한다.
//  1. 지연 시간을 내가 정하는 엔드포인트를 제공한다 (M0 측정용)
//  2. 동시에 들어온 요청 수를 세어 최댓값을 기록한다 (M1 비교용)
//
// 2번이 핵심이다. 스케줄러 두 방식이 "실제로 몇 개를 동시에 때리는지"를
// 클라이언트 말이 아니라 서버 쪽에서 직접 잰다.
//
//	go run ./cmd/slowserver -delay 200ms
//
// 엔드포인트:
//
//	/slow        설정된 delay 만큼 기다렸다가 200 OK
//	/slow?ms=500 이번 요청만 500ms 지연
//	/flaky       3초 주기로 200 <-> 503 을 오간다 (M3 플래핑 테스트용)
//	/status/503  지정한 상태 코드를 그대로 반환
//	/stats       동시성 통계 (JSON)
//	/stats/reset 통계 초기화
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// concurrency 는 동시에 처리 중인 요청 수를 추적한다.
//
// 여러 요청 goroutine 이 동시에 건드리므로 전부 atomic 이어야 한다.
// net/http 는 요청마다 goroutine 을 하나씩 띄우기 때문이다.
type concurrency struct {
	current atomic.Int64
	max     atomic.Int64
	total   atomic.Int64
}

// enter 는 요청 시작을 기록하고, 끝날 때 부를 정리 함수를 돌려준다.
func (c *concurrency) enter() func() {
	c.total.Add(1)
	now := c.current.Add(1)

	// 최댓값 갱신 — CAS 루프.
	// "읽고 비교하고 쓰기"를 그냥 하면 그 사이에 다른 요청이 끼어들 수 있다.
	for {
		peak := c.max.Load()
		if now <= peak {
			break
		}
		if c.max.CompareAndSwap(peak, now) {
			break
		}
	}

	return func() { c.current.Add(-1) }
}

func main() {
	addr := flag.String("addr", ":8080", "listen 주소")
	delay := flag.Duration("delay", 200*time.Millisecond, "기본 응답 지연")
	flag.Parse()

	// log/slog 는 Go 표준 구조적 로깅이다. fmt.Println 과 달리
	// 키=값 형태로 찍혀서 나중에 기계가 파싱하기 좋다.
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))

	var conc concurrency
	startedAt := time.Now()

	mux := http.NewServeMux()

	mux.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) {
		defer conc.enter()()
		// ↑ enter() 가 정리 함수를 반환하고, defer 가 그걸 함수 끝에 부른다.
		//   defer f()() 는 "지금 f()를 실행해서 얻은 함수를 나중에 실행"이다.

		d := *delay
		if ms := r.URL.Query().Get("ms"); ms != "" {
			if n, err := strconv.Atoi(ms); err == nil {
				d = time.Duration(n) * time.Millisecond
			}
		}

		// 클라이언트가 도중에 요청을 끊으면(타임아웃/취소) r.Context() 가 닫힌다.
		// 그걸 무시하고 계속 자면 서버 goroutine 이 낭비된다.
		select {
		case <-time.After(d):
		case <-r.Context().Done():
			return
		}
		fmt.Fprintf(w, "ok after %v\n", d)
	})

	// 3초 주기로 up/down 이 바뀌는 엔드포인트.
	mux.HandleFunc("/flaky", func(w http.ResponseWriter, r *http.Request) {
		defer conc.enter()()
		if (time.Now().Unix()/3)%2 == 0 {
			fmt.Fprintln(w, "ok")
			return
		}
		http.Error(w, "일시적 장애", http.StatusServiceUnavailable)
	})

	mux.HandleFunc("/status/", func(w http.ResponseWriter, r *http.Request) {
		defer conc.enter()()
		code, err := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/status/"))
		if err != nil || code < 100 || code > 599 {
			code = http.StatusOK
		}
		w.WriteHeader(code)
		fmt.Fprintf(w, "status %d\n", code)
	})

	// /stats 는 통계를 읽기만 하므로 스스로를 집계에 넣지 않는다.
	mux.HandleFunc("/stats", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// 구조체 태그로 JSON 키 이름을 정한다. (docs/02 참고)
		json.NewEncoder(w).Encode(struct {
			Current   int64   `json:"current"`
			Max       int64   `json:"max_concurrent"`
			Total     int64   `json:"total_requests"`
			UptimeSec float64 `json:"uptime_sec"`
		}{
			Current:   conc.current.Load(),
			Max:       conc.max.Load(),
			Total:     conc.total.Load(),
			UptimeSec: time.Since(startedAt).Seconds(),
		})
	})

	mux.HandleFunc("/stats/reset", func(w http.ResponseWriter, r *http.Request) {
		conc.max.Store(conc.current.Load())
		conc.total.Store(0)
		fmt.Fprintln(w, "reset")
	})

	log.Info("slowserver 시작", "addr", *addr, "delay", *delay)

	srv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second, // Slowloris 방어
	}
	if err := srv.ListenAndServe(); err != nil {
		log.Error("서버 종료", "err", err)
		os.Exit(1)
	}
}
