// Package admin 은 운영용 엔드포인트를 제공한다.
//
//	GET /healthz          살아 있나 (liveness)
//	GET /readyz           일할 준비가 됐나 (readiness)
//	GET /metrics          Prometheus 지표
//	GET /debug/pprof/...  프로파일
//
// 공개 API(internal/api)와 **다른 포트**에 띄운다.
// pprof 는 힙 내용과 모든 goroutine 스택을 그대로 내보낸다. 거기에는
// 처리 중이던 요청 데이터나 설정값이 들어 있을 수 있다. 지표도 모니터 이름과
// 내부 구조를 드러낸다. 인터넷에 열어 둘 것이 아니다.
// 그래서 기본 주소가 127.0.0.1 이다.
package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"runtime"
	"runtime/debug"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Pinger 는 준비 상태를 확인할 수 있는 무엇이다.
//
// *store.Store 를 그대로 받지 않고 인터페이스로 좁힌 이유:
// admin 패키지가 저장소 전체를 알 필요가 없고, 테스트에서 가짜를 넣기도 쉽다.
// ("Accept interfaces, return structs" → docs/03)
type Pinger interface {
	Ping(ctx context.Context) error
}

// Server 는 운영 엔드포인트 묶음이다.
type Server struct {
	log       *slog.Logger
	registry  *prometheus.Registry
	ready     Pinger
	startedAt time.Time
	version   string
}

// New 는 운영 서버를 만든다. registry 나 ready 는 nil 이어도 된다.
func New(log *slog.Logger, registry *prometheus.Registry, ready Pinger) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{
		log:       log,
		registry:  registry,
		ready:     ready,
		startedAt: time.Now(),
		version:   buildVersion(),
	}
}

// buildVersion 은 빌드 정보를 읽어 버전 문자열을 만든다.
//
// -ldflags 로 변수를 주입하지 않아도 된다. Go 는 빌드할 때 모듈 버전과
// (git 저장소에서 빌드했다면) 커밋 해시를 바이너리에 넣어 둔다.
func buildVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	var rev, dirty string
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			if len(s.Value) > 7 {
				rev = s.Value[:7]
			} else {
				rev = s.Value
			}
		case "vcs.modified":
			if s.Value == "true" {
				dirty = "-dirty"
			}
		}
	}
	if rev == "" {
		return info.Main.Version // 모듈로 받았을 때는 v0.1.0 같은 값이 온다
	}
	return rev + dirty
}

// Handler 는 라우팅이 끝난 http.Handler 를 돌려준다.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)

	if s.registry != nil {
		// promhttp.HandlerFor 는 전역 레지스트리 대신 우리 것을 쓰게 한다.
		mux.Handle("GET /metrics", promhttp.HandlerFor(s.registry, promhttp.HandlerOpts{
			// 지표 수집 중 에러가 나면 로그로 남기고 500 을 준다.
			ErrorLog:      slog.NewLogLogger(s.log.Handler(), slog.LevelWarn),
			ErrorHandling: promhttp.HTTPErrorOnError,
		}))
	}

	registerPprof(mux)
	return mux
}

// registerPprof 는 프로파일 엔드포인트를 **우리 mux 에** 단다.
//
// 주의: net/http/pprof 는 blank import 든 아니든, **import 되는 것만으로**
// init() 이 돌면서 http.DefaultServeMux 에 자기 핸들러를 등록한다.
// (테스트로 확인했다 — TestPprofNotExposedByPublicAPI)
//
//	func init() {
//	    http.HandleFunc("GET /debug/pprof/", Index)
//	    ...
//	}
//
// 그러니 "DefaultServeMux 에 안 붙게 하는" 방법은 없다. 대신 지켜야 할 규칙은
// **어디서도 DefaultServeMux 를 서비스하지 않는 것**이다.
// http.ListenAndServe(addr, nil) 처럼 핸들러를 nil 로 넘기면 DefaultServeMux 가
// 쓰이고, 그 순간 pprof 가 그 포트에 전부 열린다.
// upcheck 는 API·운영 서버 모두 핸들러를 명시적으로 넘긴다.
func registerPprof(mux *http.ServeMux) {
	mux.HandleFunc("GET /debug/pprof/", pprof.Index)
	mux.HandleFunc("GET /debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("GET /debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("GET /debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("GET /debug/pprof/trace", pprof.Trace)

	// pprof.Index 가 이름 있는 프로파일(goroutine, heap, allocs,
	// goroutineleak 등)을 알아서 처리하므로 개별 등록은 필요 없다.
	// Go 1.27 에 새로 들어온 goroutineleak 도 여기에 포함된다:
	//   /debug/pprof/goroutineleak?debug=1
	// "아무도 깨워줄 수 없는" goroutine 만 추려 주므로,
	// 전체 goroutine 수를 눈으로 세는 것보다 판정이 명확하다.
}

// health 는 /healthz · /readyz 응답이다.
type health struct {
	Status     string  `json:"status"`
	Version    string  `json:"version"`
	UptimeSec  float64 `json:"uptime_sec"`
	Goroutines int     `json:"goroutines"`
	Error      string  `json:"error,omitempty"`
}

// handleHealthz 는 liveness 다. 프로세스가 살아서 요청을 처리하면 200.
//
// 여기서 DB를 확인하면 안 된다. liveness 가 실패하면 오케스트레이터가
// 컨테이너를 **재시작**하는데, DB 가 잠깐 느리다고 재시작하면 상황만 나빠진다.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.snapshot("ok", ""))
}

// handleReadyz 는 readiness 다. 실제로 일할 수 있는지 본다.
//
// 실패하면 로드밸런서가 트래픽만 빼고 재시작은 하지 않는다.
// 그래서 DB 확인은 이쪽이 맞는 자리다.
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if s.ready == nil {
		writeJSON(w, http.StatusOK, s.snapshot("ok", ""))
		return
	}

	// 확인이 길어지면 헬스체크 자체가 부하가 된다. 짧은 시한을 건다.
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	if err := s.ready.Ping(ctx); err != nil {
		s.log.Warn("준비 상태 확인 실패", "err", err)
		// 원인은 로그에만 남긴다. 응답에는 무슨 종류인지만 적는다.
		writeJSON(w, http.StatusServiceUnavailable, s.snapshot("unavailable", "저장소에 연결할 수 없습니다"))
		return
	}
	writeJSON(w, http.StatusOK, s.snapshot("ok", ""))
}

func (s *Server) snapshot(status, errMsg string) health {
	return health{
		Status:     status,
		Version:    s.version,
		UptimeSec:  time.Since(s.startedAt).Round(time.Second).Seconds(),
		Goroutines: runtime.NumGoroutine(),
		Error:      errMsg,
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

// Listen 은 주소를 잡는다. API 서버와 같은 이유로 Serve 와 나눠 두었다.
func Listen(addr string) (net.Listener, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("운영 서버 listen 실패 (%s): %w", addr, err)
	}
	return ln, nil
}

// Serve 는 ctx 가 취소될 때까지 운영 엔드포인트를 제공한다.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		// pprof 의 CPU 프로파일은 기본 30초를 수집한다.
		// WriteTimeout 을 그보다 짧게 잡으면 프로파일을 뜰 수 없다.
		WriteTimeout: 65 * time.Second,
		IdleTimeout:  120 * time.Second,
		BaseContext:  func(net.Listener) context.Context { return ctx },
		ErrorLog:     slog.NewLogLogger(s.log.Handler(), slog.LevelWarn),
	}
	s.log.Info("운영 서버 시작", "addr", ln.Addr().String(), "version", s.version)

	serveErr := make(chan error, 1)
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
		close(serveErr)
	}()

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("운영 서버 종료 실패: %w", err)
	}
	return <-serveErr
}
