// Package admin 은 운영용 엔드포인트를 제공한다.
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
		mux.Handle("GET /metrics", promhttp.HandlerFor(s.registry, promhttp.HandlerOpts{
			ErrorLog:      slog.NewLogLogger(s.log.Handler(), slog.LevelWarn),
			ErrorHandling: promhttp.HTTPErrorOnError,
		}))
	}

	registerPprof(mux)
	return mux
}

// registerPprof 는 프로파일 엔드포인트를 **우리 mux 에** 단다.
//
// 주의: pprof 는 import 만으로 DefaultServeMux 에 등록된다.
// 어디서도 DefaultServeMux 를 서비스하지 않는 것(핸들러 nil 금지)이 규칙이다.
func registerPprof(mux *http.ServeMux) {
	mux.HandleFunc("GET /debug/pprof/", pprof.Index)
	mux.HandleFunc("GET /debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("GET /debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("GET /debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("GET /debug/pprof/trace", pprof.Trace)
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
// 여기서 DB를 확인하면 안 된다(실패 시 재시작된다).
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.snapshot("ok", ""))
}

// handleReadyz 는 readiness 다. 실제로 일할 수 있는지 본다.
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if s.ready == nil {
		writeJSON(w, http.StatusOK, s.snapshot("ok", ""))
		return
	}

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
