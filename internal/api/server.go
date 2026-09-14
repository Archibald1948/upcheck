// Package api 는 상태 페이지가 읽을 HTTP JSON API 를 제공한다.
//
//	GET /api/status                          전체 현황
//	GET /api/monitors/{id}/history?days=90   일별 업타임 · 응답시간 · 장애 이력
//	GET /api/incidents?limit=20              최근 장애
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"strconv"
	"time"

	"github.com/Archibald1948/upcheck/internal/store"
)

// Options 는 API 동작 설정이다.
type Options struct {
	// ExposeDetails 가 false 면 target 주소, 에러 원문, 경고를 응답에서 뺀다.
	//
	// 상태 페이지는 공개용이다. "dial tcp 10.0.3.17:5432: connection refused"
	// 같은 에러 원문에는 내부 IP 와 포트, 쓰는 DB 종류가 다 들어 있다.
	// 외부에 보여줄 이유가 없는 정보다. 기본값을 '숨김'으로 둔다.
	ExposeDetails bool

	// DefaultTimezone 은 tz 파라미터가 없을 때 날짜를 자르는 시간대다.
	DefaultTimezone *time.Location
}

// Server 는 API 핸들러 묶음이다.
type Server struct {
	store *store.Store
	log   *slog.Logger
	opts  Options
	now   func() time.Time
}

// New 는 API 서버를 만든다.
func New(st *store.Store, log *slog.Logger, opts Options) *Server {
	if log == nil {
		log = slog.Default()
	}
	if opts.DefaultTimezone == nil {
		opts.DefaultTimezone = time.UTC
	}
	return &Server{store: st, log: log, opts: opts, now: time.Now}
}

// Handler 는 라우팅과 미들웨어가 붙은 http.Handler 를 돌려준다.
//
// Go 1.22 부터 표준 ServeMux 가 메서드와 경로 변수를 이해한다.
//
//	"GET /api/monitors/{id}/history"
//
// 그 전에는 이 한 줄을 위해 chi 나 gorilla/mux 를 들여야 했다.
// 스펙 3절이 "net/http (stdlib 라우팅으로 충분)"이라고 한 게 이것이다.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/status", s.handleStatus)
	mux.HandleFunc("GET /api/monitors/{id}/history", s.handleHistory)
	mux.HandleFunc("GET /api/incidents", s.handleIncidents)

	// 등록되지 않은 경로도 JSON 으로 답한다. 기본 404 는 text/plain 이라
	// 프론트엔드가 JSON 으로 파싱하다 엉뚱한 에러를 낸다.
	//
	// 패턴을 "/" 가 아니라 "GET /" 로 거는 게 중요하다.
	// "/" 는 모든 메서드를 받아서, POST /api/status 까지 여기로 빨려 들어와
	// 404 가 된다. ServeMux 는 "경로는 맞는데 메서드가 틀린" 요청에
	// 405 Method Not Allowed 를 주는데, 메서드 없는 catch-all 이 있으면
	// 그 판단을 할 기회 자체가 사라진다. (실제로 테스트에서 걸렸다)
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "없는 경로다")
	})

	// 미들웨어는 바깥쪽부터 감싼다. 요청은 recover → log → mux 순으로 지나간다.
	var h http.Handler = mux
	h = s.logRequests(h)
	h = s.recoverPanics(h)
	return h
}

// ─────────────────────── 핸들러 ───────────────────────

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	now := s.now()
	rows, err := s.store.Summary(r.Context(), now)
	if err != nil {
		s.internalError(w, r, err)
		return
	}

	resp := StatusResponse{
		GeneratedAt: now.UTC(),
		Monitors:    make([]MonitorStatus, 0, len(rows)),
	}
	for _, row := range rows {
		resp.Monitors = append(resp.Monitors, s.toMonitorStatus(row))
	}
	resp.Overall = overall(resp.Monitors)

	// 상태 페이지는 새로고침이 잦다. 짧게라도 캐시하게 해서
	// 사람이 몰렸을 때 요청마다 집계 질의가 도는 걸 줄인다.
	w.Header().Set("Cache-Control", "public, max-age=15")
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	// r.PathValue 는 패턴의 {id} 자리에 들어온 문자열을 준다 (Go 1.22+).
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "id 는 양의 정수여야 한다")
		return
	}

	days, err := intParam(r, "days", 90, 1, 90)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	loc := s.opts.DefaultTimezone
	if tz := r.URL.Query().Get("tz"); tz != "" {
		// LoadLocation 은 IANA 이름("Asia/Seoul")을 받는다.
		// 사용자 입력이라 반드시 에러를 확인한다.
		l, err := time.LoadLocation(tz)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("알 수 없는 시간대 %q", tz))
			return
		}
		loc = l
	}

	ctx := r.Context()
	m, err := s.store.Monitor(ctx, id)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if m == nil {
		writeError(w, http.StatusNotFound, "모니터를 찾을 수 없다")
		return
	}

	now := s.now()
	buckets, err := s.store.DailyHistory(ctx, id, days, loc, now)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	incidents, err := s.store.Incidents(ctx, id, 50)
	if err != nil {
		s.internalError(w, r, err)
		return
	}

	resp := HistoryResponse{
		Monitor:   MonitorRef{ID: m.ID, Name: m.Name},
		Timezone:  loc.String(),
		Days:      make([]Day, 0, len(buckets)),
		Incidents: make([]Incident, 0, len(incidents)),
	}
	for _, b := range buckets {
		resp.Days = append(resp.Days, toDay(b))
	}
	windowStart := now.AddDate(0, 0, -days)
	for _, inc := range incidents {
		// 요청한 기간보다 오래전에 끝난 장애는 뺀다.
		if inc.ResolvedAt != nil && inc.ResolvedAt.Before(windowStart) {
			continue
		}
		resp.Incidents = append(resp.Incidents, s.toIncident(inc.ID, inc.MonitorID, m.Name, inc, now))
	}

	w.Header().Set("Cache-Control", "public, max-age=60")
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleIncidents(w http.ResponseWriter, r *http.Request) {
	limit, err := intParam(r, "limit", 20, 1, 100)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	rows, err := s.store.RecentIncidents(r.Context(), limit)
	if err != nil {
		s.internalError(w, r, err)
		return
	}

	now := s.now()
	resp := IncidentsResponse{Incidents: make([]Incident, 0, len(rows))}
	for _, row := range rows {
		resp.Incidents = append(resp.Incidents,
			s.toIncident(row.ID, row.MonitorID, row.MonitorName, row.Incident, now))
	}

	w.Header().Set("Cache-Control", "public, max-age=15")
	writeJSON(w, http.StatusOK, resp)
}

// ─────────────────────── 변환 ───────────────────────

func (s *Server) toMonitorStatus(row store.MonitorStatus) MonitorStatus {
	ms := MonitorStatus{
		ID:     row.ID,
		Name:   row.Name,
		Status: "unknown",
		Uptime: UptimeWindows{
			H24: uptimePtr(row.Uptime24h),
			D7:  uptimePtr(row.Uptime7d),
			D30: uptimePtr(row.Uptime30d),
		},
		Latency: LatencySummary{
			P50MS:  msPtr(row.Latency24h.P50, row.Latency24h.Samples),
			P95MS:  msPtr(row.Latency24h.P95, row.Latency24h.Samples),
			Approx: row.Latency24h.Approx,
		},
	}

	if lc := row.LastCheck; lc != nil {
		ms.Type = lc.Type
		t := lc.CheckedAt.UTC()
		ms.LastCheckedAt = &t
		if lc.OK {
			ms.Status = "up"
		} else {
			ms.Status = "down"
		}
		if s.opts.ExposeDetails {
			ms.LastError = lc.Error
			ms.Warning = lc.Warning
		}
	}
	if s.opts.ExposeDetails {
		ms.Target = row.Target
	}
	return ms
}

func (s *Server) toIncident(id, monitorID int64, name string, inc store.Incident, now time.Time) Incident {
	out := Incident{
		ID:          id,
		MonitorID:   monitorID,
		MonitorName: name,
		StartedAt:   inc.StartedAt.UTC(),
		DurationSec: int64(inc.Duration(now).Seconds()),
	}
	if inc.ResolvedAt != nil {
		t := inc.ResolvedAt.UTC()
		out.ResolvedAt = &t
	}
	if s.opts.ExposeDetails {
		out.Cause = inc.Cause
	}
	return out
}

func toDay(b store.DayBucket) Day {
	d := Day{
		Date:   b.Date.Format(time.DateOnly),
		Checks: b.Total,
		Failed: b.Total - b.OK,
	}
	if b.HasData() {
		u := round2(b.Uptime())
		d.Uptime = &u
		if b.OK > 0 {
			p50, p95 := b.P50MS, b.P95MS
			d.P50MS, d.P95MS = &p50, &p95
		}
	}
	return d
}

// overall 은 전체 상태 한 줄을 정한다. 상태 페이지 맨 위 배너가 된다.
func overall(ms []MonitorStatus) string {
	var up, down int
	for _, m := range ms {
		switch m.Status {
		case "up":
			up++
		case "down":
			down++
		}
	}
	switch {
	case up == 0 && down == 0:
		return "unknown"
	case down == 0:
		return "operational"
	case up == 0:
		return "outage"
	default:
		return "degraded"
	}
}

func uptimePtr(u store.Uptime) *float64 {
	if u.Total == 0 {
		return nil
	}
	v := round2(u.Percent())
	return &v
}

func msPtr(d time.Duration, samples int64) *int64 {
	if samples == 0 {
		return nil
	}
	v := d.Milliseconds()
	return &v
}

// round2 는 소수 둘째 자리까지 남긴다.
// 99.99583333333 같은 값을 그대로 보내면 화면마다 자르는 방식이 달라진다.
func round2(v float64) float64 {
	return float64(int64(v*100+0.5)) / 100
}

// ─────────────────────── 응답 헬퍼 ───────────────────────

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// 헤더는 WriteHeader 전에 다 정해야 한다. 그 뒤에 Set 하면 무시된다.
	w.WriteHeader(status)

	enc := json.NewEncoder(w)
	// 기본값이면 <, >, & 를 < 로 바꾼다(HTML 에 끼워 넣을 때를 대비).
	// 순수 API 응답이라 끄는 게 읽기 좋다.
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v) // 헤더를 이미 보냈으니 여기서 실패해도 상태 코드를 못 바꾼다
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, ErrorResponse{Error: msg})
}

// internalError 는 500 을 돌려주되, 에러 원문은 로그에만 남긴다.
//
// DB 에러 메시지에는 테이블 이름이나 파일 경로가 들어 있다.
// 응답에 그대로 실으면 공격자에게 내부 구조를 알려주는 셈이다.
func (s *Server) internalError(w http.ResponseWriter, r *http.Request, err error) {
	// 클라이언트가 먼저 끊은 경우는 우리 잘못이 아니다. 경고로 남길 필요도 없다.
	if errors.Is(err, context.Canceled) {
		return
	}
	s.log.Error("API 처리 실패", "path", r.URL.Path, "err", err)
	writeError(w, http.StatusInternalServerError, "내부 오류")
}

// intParam 은 쿼리 파라미터를 정수로 읽고 범위를 검사한다.
func intParam(r *http.Request, name string, def, lo, hi int) (int, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return def, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s 는 정수여야 한다", name)
	}
	if v < lo || v > hi {
		return 0, fmt.Errorf("%s 는 %d~%d 사이여야 한다", name, lo, hi)
	}
	return v, nil
}

// ─────────────────────── 미들웨어 ───────────────────────

// statusRecorder 는 핸들러가 쓴 상태 코드를 기억한다.
//
// http.ResponseWriter 는 "무슨 상태 코드를 썼는지" 되물을 방법이 없다.
// 인터페이스를 감싸 WriteHeader 호출을 가로채는 것이 표준적인 방법이다.
// 구조체 임베딩 덕분에 나머지 메서드(Write, Header)는 그대로 위임된다.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		s.log.Debug("API 요청",
			"method", r.Method, "path", r.URL.Path,
			"status", rec.status, "elapsed", time.Since(start).Round(time.Microsecond))
	})
}

// recoverPanics 는 핸들러 panic 이 서버 전체를 죽이지 않게 한다.
//
// 사실 net/http 는 요청 goroutine 의 panic 을 스스로 잡아서 연결만 끊는다.
// 그래도 이 미들웨어를 두는 이유는 두 가지다.
//   - 클라이언트가 끊긴 연결 대신 500 JSON 을 받는다
//   - 스택을 우리 로거로 남긴다 (기본 동작은 표준 log 로 찍는다)
func (s *Server) recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				// http.ErrAbortHandler 는 net/http 가 "조용히 끊어라"는 신호로 쓰는 값이다.
				// 다시 던져서 원래 동작을 따르게 한다.
				if v == http.ErrAbortHandler {
					panic(v)
				}
				s.log.Error("API 핸들러 panic", "path", r.URL.Path, "panic", v, "stack", string(debug.Stack()))
				writeError(w, http.StatusInternalServerError, "내부 오류")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// ─────────────────────── 구동 ───────────────────────

// Listen 은 addr 에서 연결을 받을 준비만 한다.
//
// Serve 와 나눈 이유: 포트가 이미 쓰이고 있으면 goroutine 을 띄우기 전에
// 호출부가 동기적으로 에러를 받아야 한다. 합쳐 두고 goroutine 에서 부르면
// 기동 실패가 조용히 묻혀서, 체크는 도는데 상태 페이지만 안 뜨는
// 알아채기 어려운 상태가 된다.
func Listen(addr string) (net.Listener, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("API 서버 listen 실패 (%s): %w", addr, err)
	}
	return ln, nil
}

// ListenAndServe 는 Listen 과 Serve 를 이어서 부른다.
func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	ln, err := Listen(addr)
	if err != nil {
		return err
	}
	return s.Serve(ctx, ln)
}

// Serve 는 ln 으로 API 를 제공하고, ctx 가 취소되면 정리하고 반환한다.
//
// http.Server 의 타임아웃들을 전부 채운다. 기본값은 전부 0(무제한)이라,
// 느리게 헤더를 흘리는 클라이언트 몇 개로 연결을 다 붙잡을 수 있다(Slowloris).
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
		// 요청 ctx 가 서버 수명 ctx 를 물려받게 한다.
		// 종료가 시작되면 진행 중인 DB 질의도 함께 취소된다.
		BaseContext: func(net.Listener) context.Context { return ctx },
		ErrorLog:    slog.NewLogLogger(s.log.Handler(), slog.LevelWarn),
	}
	s.log.Info("API 서버 시작", "addr", ln.Addr().String())

	serveErr := make(chan error, 1)
	go func() {
		// Serve 는 Shutdown 이 불리면 http.ErrServerClosed 를 돌려준다.
		// 그건 정상 종료라 에러로 취급하지 않는다.
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

	// Shutdown 은 새 연결을 막고, 진행 중인 요청이 끝나기를 기다린다.
	// ctx 는 이미 취소됐으니 새 시한을 판다. 이 시한이 지나면 강제로 닫는다.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("API 서버 종료 실패: %w", err)
	}
	return <-serveErr
}
