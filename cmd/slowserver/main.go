// Command slowserver — 일부러 느리게 응답하는 로컬 테스트 서버.
//
//	/slow        설정된 delay 만큼 기다렸다가 200 OK
//	/slow?ms=500 이번 요청만 500ms 지연
//	/flaky       3초 주기로 200 <-> 503 을 오간다
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

	log := slog.New(slog.NewTextHandler(os.Stdout, nil))

	var conc concurrency
	startedAt := time.Now()

	mux := http.NewServeMux()

	mux.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) {
		defer conc.enter()()

		d := *delay
		if ms := r.URL.Query().Get("ms"); ms != "" {
			if n, err := strconv.Atoi(ms); err == nil {
				d = time.Duration(n) * time.Millisecond
			}
		}

		select {
		case <-time.After(d):
		case <-r.Context().Done():
			return
		}
		fmt.Fprintf(w, "ok after %v\n", d)
	})

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
