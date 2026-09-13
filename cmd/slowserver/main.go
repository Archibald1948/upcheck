// Command slowserver — 일부러 느리게 응답하는 로컬 테스트 서버.
//
// M0가 얼마나 느린지 재려면 "응답이 느린 엔드포인트"가 많이 필요하다.
// 실제 외부 사이트를 50번 때리는 건 민폐이고 측정도 들쭉날쭉하므로
// 지연 시간을 내가 정할 수 있는 서버를 직접 띄운다.
//
//	go run ./cmd/slowserver -addr :8080 -delay 200ms
//
// 엔드포인트:
//
//	/slow        설정된 delay 만큼 기다렸다가 200 OK
//	/slow?ms=500 이번 요청만 500ms 지연
//	/flaky       3초 주기로 200 <-> 503 을 오간다 (M3 플래핑 테스트용)
//	/status/503  지정한 상태 코드를 그대로 반환
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

func main() {
	addr := flag.String("addr", ":8080", "listen 주소")
	delay := flag.Duration("delay", 200*time.Millisecond, "기본 응답 지연")
	flag.Parse()

	// log/slog 는 Go 표준 구조적 로깅이다. fmt.Println 과 달리
	// 키=값 형태로 찍혀서 나중에 기계가 파싱하기 좋다.
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))

	mux := http.NewServeMux()

	mux.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) {
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
		if (time.Now().Unix()/3)%2 == 0 {
			fmt.Fprintln(w, "ok")
			return
		}
		http.Error(w, "일시적 장애", http.StatusServiceUnavailable)
	})

	mux.HandleFunc("/status/", func(w http.ResponseWriter, r *http.Request) {
		code, err := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/status/"))
		if err != nil || code < 100 || code > 599 {
			code = http.StatusOK
		}
		w.WriteHeader(code)
		fmt.Fprintf(w, "status %d\n", code)
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
