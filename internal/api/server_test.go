package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Archibald1948/upcheck/internal/config"
	"github.com/Archibald1948/upcheck/internal/store"
)

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// fixedNow 는 테스트의 기준 시각이다. 시간이 흘러도 결과가 같아야 한다.
var fixedNow = time.Date(2026, 9, 14, 6, 0, 0, 0, time.UTC)

type fixture struct {
	store *store.Store
	ids   map[string]int64
}

// newFixture 는 모니터 셋(정상/장애/미체크)과 데이터를 심은 저장소를 만든다.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()

	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "api.db"), quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	ids, err := st.SyncMonitors(ctx, []config.Monitor{
		{Name: "api", Type: "http", Target: "https://internal-api.corp:8443/health", IntervalSec: 60, TimeoutMS: 5000, ExpectedStatus: 200},
		{Name: "db", Type: "tcp", Target: "10.0.3.17:5432", IntervalSec: 60, TimeoutMS: 5000},
		{Name: "new", Type: "http", Target: "https://new.example", IntervalSec: 60, TimeoutMS: 5000, ExpectedStatus: 200},
	})
	if err != nil {
		t.Fatal(err)
	}

	var rows []store.CheckRow
	for i := range 60 {
		at := fixedNow.Add(-time.Duration(60-i) * time.Minute)
		rows = append(rows, store.CheckRow{MonitorID: ids["api"], Type: "http", CheckedAt: at, OK: true, StatusCode: 200, LatencyMS: int64(100 + i)})
		// db 는 마지막 10분 동안 죽어 있다
		ok := i < 50
		r := store.CheckRow{MonitorID: ids["db"], Type: "tcp", CheckedAt: at, OK: ok, LatencyMS: 5}
		if !ok {
			r.Error = "dial tcp 10.0.3.17:5432: connect: connection refused"
		}
		rows = append(rows, r)
	}
	if err := st.InsertChecks(ctx, rows); err != nil {
		t.Fatal(err)
	}
	if _, err := st.OpenIncident(ctx, ids["db"], fixedNow.Add(-10*time.Minute), "dial tcp 10.0.3.17:5432: connect: connection refused"); err != nil {
		t.Fatal(err)
	}
	return &fixture{store: st, ids: ids}
}

func (f *fixture) server(opts Options) http.Handler {
	s := New(f.store, quietLogger(), opts)
	s.now = func() time.Time { return fixedNow }
	return s.Handler()
}

// get 은 요청을 보내고 상태 코드와 본문을 돌려준다.
func get(t *testing.T, h http.Handler, path string) (int, []byte) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("%s: Content-Type = %q, JSON 이어야 한다", path, ct)
	}
	return rec.Code, rec.Body.Bytes()
}

func decode[T any](t *testing.T, body []byte) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatalf("JSON 해석 실패: %v\n%s", err, body)
	}
	return v
}

// ─────────────── /api/status ───────────────

func TestStatus(t *testing.T) {
	f := newFixture(t)
	code, body := get(t, f.server(Options{}), "/api/status")
	if code != http.StatusOK {
		t.Fatalf("상태 코드 %d\n%s", code, body)
	}

	resp := decode[StatusResponse](t, body)
	if resp.Overall != "degraded" {
		t.Errorf("overall = %q, 하나가 죽어 있으니 degraded 여야 한다", resp.Overall)
	}

	by := map[string]MonitorStatus{}
	for _, m := range resp.Monitors {
		by[m.Name] = m
	}

	if by["api"].Status != "up" || by["db"].Status != "down" || by["new"].Status != "unknown" {
		t.Errorf("상태가 틀렸다: api=%s db=%s new=%s", by["api"].Status, by["db"].Status, by["new"].Status)
	}
	if u := by["api"].Uptime.H24; u == nil || *u != 100 {
		t.Errorf("api 24h 업타임 = %v, 100 이어야 한다", u)
	}
	if u := by["db"].Uptime.H24; u == nil || *u > 84 || *u < 83 {
		t.Errorf("db 24h 업타임 = %v, 50/60 ≈ 83.33 이어야 한다", u)
	}
	if by["api"].Latency.P95MS == nil {
		t.Error("api p95 가 없다")
	}
}

// TestStatusNullForNoData 는 한 번도 체크 안 한 모니터가 0이 아니라
// null 로 나가는지 본다. 0% 는 "완전히 죽어 있었다"는 뜻이라 거짓말이다.
func TestStatusNullForNoData(t *testing.T) {
	f := newFixture(t)
	_, body := get(t, f.server(Options{}), "/api/status")

	var raw struct {
		Monitors []map[string]any `json:"monitors"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}
	for _, m := range raw.Monitors {
		if m["name"] != "new" {
			continue
		}
		uptime := m["uptime"].(map[string]any)
		if uptime["24h"] != nil {
			t.Errorf("데이터 없는 모니터의 24h 업타임이 %v — null 이어야 한다", uptime["24h"])
		}
		if m["last_checked_at"] != nil {
			t.Errorf("last_checked_at 이 %v — null 이어야 한다", m["last_checked_at"])
		}
		return
	}
	t.Fatal("new 모니터가 응답에 없다")
}

// TestDetailsHiddenByDefault 는 공개 상태 페이지에 내부 정보가
// 새지 않는지 본다. 이게 기본값이어야 한다.
func TestDetailsHiddenByDefault(t *testing.T) {
	f := newFixture(t)
	h := f.server(Options{}) // ExposeDetails: false

	for _, path := range []string{"/api/status", "/api/incidents", "/api/monitors/1/history?days=7"} {
		_, body := get(t, h, path)
		s := string(body)
		for _, secret := range []string{"10.0.3.17", "5432", "internal-api.corp", "connection refused"} {
			if strings.Contains(s, secret) {
				t.Errorf("%s 응답에 내부 정보 %q 가 새어 나갔다", path, secret)
			}
		}
	}
}

func TestDetailsExposedWhenEnabled(t *testing.T) {
	f := newFixture(t)
	_, body := get(t, f.server(Options{ExposeDetails: true}), "/api/status")

	resp := decode[StatusResponse](t, body)
	for _, m := range resp.Monitors {
		if m.Name == "db" {
			if m.Target != "10.0.3.17:5432" {
				t.Errorf("Target = %q", m.Target)
			}
			if !strings.Contains(m.LastError, "connection refused") {
				t.Errorf("LastError = %q", m.LastError)
			}
		}
	}
}

func TestOverall(t *testing.T) {
	ms := func(statuses ...string) []MonitorStatus {
		out := make([]MonitorStatus, len(statuses))
		for i, s := range statuses {
			out[i].Status = s
		}
		return out
	}
	cases := []struct {
		in   []MonitorStatus
		want string
	}{
		{ms("up", "up"), "operational"},
		{ms("up", "down"), "degraded"},
		{ms("down", "down"), "outage"},
		{ms(), "unknown"},
		{ms("unknown"), "unknown"},
		{ms("up", "unknown"), "operational"},
	}
	for _, c := range cases {
		if got := overall(c.in); got != c.want {
			t.Errorf("overall(%v) = %q, 기대값 %q", c.in, got, c.want)
		}
	}
}

// ─────────────── /api/monitors/{id}/history ───────────────

func TestHistory(t *testing.T) {
	f := newFixture(t)
	path := "/api/monitors/" + itoa(f.ids["db"]) + "/history?days=90&tz=Asia/Seoul"
	code, body := get(t, f.server(Options{}), path)
	if code != http.StatusOK {
		t.Fatalf("상태 코드 %d\n%s", code, body)
	}

	resp := decode[HistoryResponse](t, body)
	if len(resp.Days) != 90 {
		t.Fatalf("칸 %d개, 90개여야 한다", len(resp.Days))
	}
	if resp.Timezone != "Asia/Seoul" {
		t.Errorf("Timezone = %q", resp.Timezone)
	}

	// 06:00 UTC = 15:00 KST → 서울 기준 오늘은 9월 14일
	last := resp.Days[89]
	if last.Date != "2026-09-14" {
		t.Errorf("마지막 칸 날짜 %q, 2026-09-14 여야 한다", last.Date)
	}
	if last.Uptime == nil || last.Checks != 60 || last.Failed != 10 {
		t.Errorf("오늘 칸이 틀렸다: %+v", last)
	}
	// 데이터 없는 날은 null
	if resp.Days[0].Uptime != nil {
		t.Errorf("89일 전 칸에 값이 있다: %v", *resp.Days[0].Uptime)
	}
	if len(resp.Incidents) != 1 || resp.Incidents[0].ResolvedAt != nil {
		t.Errorf("진행 중인 장애 1건이어야 한다: %+v", resp.Incidents)
	}
}

func TestHistoryBadRequests(t *testing.T) {
	f := newFixture(t)
	h := f.server(Options{})

	cases := map[string]int{
		"/api/monitors/abc/history":               http.StatusBadRequest,
		"/api/monitors/0/history":                 http.StatusBadRequest,
		"/api/monitors/1/history?days=0":          http.StatusBadRequest,
		"/api/monitors/1/history?days=91":         http.StatusBadRequest,
		"/api/monitors/1/history?days=x":          http.StatusBadRequest,
		"/api/monitors/1/history?tz=Mars/Olympus": http.StatusBadRequest,
		"/api/monitors/99999/history":             http.StatusNotFound,
		"/api/없는경로":                               http.StatusNotFound,
	}
	for path, want := range cases {
		t.Run(path, func(t *testing.T) {
			code, body := get(t, h, path)
			if code != want {
				t.Errorf("상태 코드 %d, 기대값 %d\n%s", code, want, body)
			}
			if resp := decode[ErrorResponse](t, body); resp.Error == "" {
				t.Error("에러 메시지가 비어 있다")
			}
		})
	}
}

// TestMethodNotAllowed 는 GET 전용 경로에 POST 가 오면 405 인지 본다.
// Go 1.22 ServeMux 가 메서드 패턴으로 자동 처리한다.
//
// 처음엔 catch-all 을 "/" 로 걸어서 404 가 나왔다. 메서드 없는 패턴이
// 모든 요청을 가로채면 ServeMux 가 405 를 판단할 기회가 사라진다.
func TestMethodNotAllowed(t *testing.T) {
	f := newFixture(t)
	h := f.server(Options{})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/status", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /api/status → %d, 405 여야 한다", rec.Code)
	}
	// 405 응답에는 허용 메서드를 알려주는 Allow 헤더가 붙어야 한다
	if allow := rec.Header().Get("Allow"); !strings.Contains(allow, "GET") {
		t.Errorf("Allow 헤더 = %q, GET 이 있어야 한다", allow)
	}

	// HEAD 는 GET 패턴으로 처리된다
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodHead, "/api/status", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("HEAD /api/status → %d, 200 이어야 한다", rec.Code)
	}
}

// ─────────────── /api/incidents ───────────────

func TestIncidents(t *testing.T) {
	f := newFixture(t)
	code, body := get(t, f.server(Options{}), "/api/incidents?limit=5")
	if code != http.StatusOK {
		t.Fatalf("상태 코드 %d\n%s", code, body)
	}
	resp := decode[IncidentsResponse](t, body)
	if len(resp.Incidents) != 1 {
		t.Fatalf("장애 %d건, 1건이어야 한다", len(resp.Incidents))
	}
	inc := resp.Incidents[0]
	if inc.MonitorName != "db" {
		t.Errorf("MonitorName = %q", inc.MonitorName)
	}
	if inc.DurationSec != 600 {
		t.Errorf("DurationSec = %d, 진행 중이면 now 까지 600초여야 한다", inc.DurationSec)
	}
}

// ─────────────── 미들웨어 · 구동 ───────────────

func TestRecoverPanics(t *testing.T) {
	s := New(nil, quietLogger(), Options{})
	h := s.recoverPanics(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("일부러 낸 panic")
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("상태 코드 %d, 500 이어야 한다", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "일부러") {
		t.Error("panic 값이 응답에 새어 나갔다")
	}
}

// TestListenAndServeShutsDown 은 ctx 를 취소하면 서버가 정리되고 반환하는지 본다.
func TestListenAndServeShutsDown(t *testing.T) {
	f := newFixture(t)
	s := New(f.store, quietLogger(), Options{})

	// 빈 포트를 얻는다
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.ListenAndServe(ctx, addr) }()

	// 뜰 때까지 기다린다
	var resp *http.Response
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		resp, err = http.Get("http://" + addr + "/api/status")
		if err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("서버에 연결할 수 없다: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("상태 코드 %d", resp.StatusCode)
	}

	start := time.Now()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("정상 종료인데 에러: %v", err)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("ctx 취소 후 6초 안에 반환하지 않았다")
	}
	t.Logf("취소 후 %v 만에 종료", time.Since(start).Round(time.Millisecond))
}

func TestListenAndServePortInUse(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	f := newFixture(t)
	err = New(f.store, quietLogger(), Options{}).ListenAndServe(context.Background(), ln.Addr().String())
	if err == nil {
		t.Fatal("이미 쓰는 포트인데 에러가 없다")
	}
}

func itoa(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}
