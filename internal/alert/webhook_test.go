package alert

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Archibald1948/upcheck/internal/config"
)

func downEvent() Event {
	return Event{
		Monitor: "결제 API", Target: "https://pay.example/health",
		Kind: KindDown, At: time.Date(2026, 4, 1, 9, 0, 0, 0, time.UTC),
		Cause: "상태 코드 503 (기대값 200)", FailCount: 3,
	}
}

func upEvent() Event {
	return Event{
		Monitor: "결제 API", Target: "https://pay.example/health",
		Kind: KindUp, At: time.Date(2026, 4, 1, 9, 5, 0, 0, time.UTC),
		Downtime: 5 * time.Minute,
	}
}

// captureServer 는 받은 요청 본문을 모아 두는 테스트 서버다.
type captureServer struct {
	*httptest.Server
	bodies chan []byte
	calls  atomic.Int64
	status atomic.Int64 // 돌려줄 상태 코드
}

func newCaptureServer() *captureServer {
	cs := &captureServer{bodies: make(chan []byte, 16)}
	cs.status.Store(http.StatusNoContent)
	cs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cs.calls.Add(1)
		b, _ := io.ReadAll(r.Body)
		select {
		case cs.bodies <- b:
		default:
		}
		w.WriteHeader(int(cs.status.Load()))
	}))
	return cs
}

func (cs *captureServer) nextBody(t *testing.T) map[string]any {
	t.Helper()
	select {
	case b := <-cs.bodies:
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("본문이 JSON 이 아니다: %v\n%s", err, b)
		}
		return m
	case <-time.After(2 * time.Second):
		t.Fatal("2초 안에 요청이 오지 않았다")
		return nil
	}
}

// ─────────────── 발송 ───────────────

func TestDiscordPayload(t *testing.T) {
	srv := newCaptureServer()
	defer srv.Close()

	n := NewDiscord(srv.URL)
	if n.Name() != "discord" {
		t.Errorf("Name() = %q", n.Name())
	}
	if err := n.Notify(context.Background(), downEvent()); err != nil {
		t.Fatalf("Notify 실패: %v", err)
	}

	body := srv.nextBody(t)
	embeds, ok := body["embeds"].([]any)
	if !ok || len(embeds) != 1 {
		t.Fatalf("embeds 가 없다: %v", body)
	}
	e := embeds[0].(map[string]any)

	title, _ := e["title"].(string)
	if !strings.Contains(title, "결제 API") || !strings.Contains(title, "장애") {
		t.Errorf("제목이 이상하다: %q", title)
	}
	desc, _ := e["description"].(string)
	for _, want := range []string{"상태 코드 503", "연속 실패 3회", "pay.example"} {
		if !strings.Contains(desc, want) {
			t.Errorf("본문에 %q 가 없다:\n%s", want, desc)
		}
	}
}

func TestDiscordColorDiffersByKind(t *testing.T) {
	down := discordPayload(downEvent()).(map[string]any)["embeds"].([]map[string]any)[0]["color"]
	up := discordPayload(upEvent()).(map[string]any)["embeds"].([]map[string]any)[0]["color"]
	if down == up {
		t.Errorf("장애와 복구의 색이 같다: %v", down)
	}
}

func TestSlackPayload(t *testing.T) {
	srv := newCaptureServer()
	defer srv.Close()

	n := NewSlack(srv.URL)
	if n.Name() != "slack" {
		t.Errorf("Name() = %q", n.Name())
	}
	if err := n.Notify(context.Background(), upEvent()); err != nil {
		t.Fatalf("Notify 실패: %v", err)
	}

	body := srv.nextBody(t)
	text, _ := body["text"].(string)
	for _, want := range []string{"복구됨", "결제 API", "5분"} {
		if !strings.Contains(text, want) {
			t.Errorf("본문에 %q 가 없다:\n%s", want, text)
		}
	}
}

// TestWebhookRetriesOnServerError 는 5xx 에 재시도하는지 본다.
func TestWebhookRetriesOnServerError(t *testing.T) {
	srv := newCaptureServer()
	defer srv.Close()
	srv.status.Store(http.StatusBadGateway)

	n := NewDiscord(srv.URL)
	err := n.Notify(context.Background(), downEvent())
	if err == nil {
		t.Fatal("계속 502 인데 성공으로 반환됐다")
	}
	if got := srv.calls.Load(); got != maxAttempts {
		t.Errorf("요청 %d회, 기대값 %d회", got, maxAttempts)
	}
}

// TestWebhookDoesNotRetryOnClientError 는 4xx 에 재시도하지 않는지 본다.
// 우리가 잘못 보낸 것이라 다시 보내도 똑같이 실패한다.
func TestWebhookDoesNotRetryOnClientError(t *testing.T) {
	srv := newCaptureServer()
	defer srv.Close()
	srv.status.Store(http.StatusBadRequest)

	if err := NewSlack(srv.URL).Notify(context.Background(), downEvent()); err == nil {
		t.Fatal("400 인데 성공으로 반환됐다")
	}
	if got := srv.calls.Load(); got != 1 {
		t.Errorf("요청 %d회 — 4xx 는 재시도하면 안 된다", got)
	}
}

// TestWebhookRecoversAfterTransientFailure 는 첫 시도가 실패해도
// 다음 시도에서 성공하면 전체가 성공인지 본다.
func TestWebhookRecoversAfterTransientFailure(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	if err := NewDiscord(srv.URL).Notify(context.Background(), downEvent()); err != nil {
		t.Errorf("2회차에 성공했는데 실패로 반환됐다: %v", err)
	}
	if calls.Load() != 2 {
		t.Errorf("요청 %d회, 기대값 2회", calls.Load())
	}
}

// TestWebhookRespectsContextCancel 은 취소가 즉시 먹는지 본다.
//
// 핸들러 지연을 짧게 잡은 이유: httptest.Server.Close() 는 처리 중인
// 요청이 끝날 때까지 기다린다. 클라이언트가 끊어도 서버 쪽 goroutine 은
// 지연이 끝나야 반환하므로, 지연을 길게 잡으면 그만큼 테스트가 멈춰 선다.
// 검증 대상은 '클라이언트가 얼마나 빨리 포기하는가'지 서버가 아니다.
func TestWebhookRespectsContextCancel(t *testing.T) {
	const serverDelay = 800 * time.Millisecond
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(serverDelay):
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := NewSlack(srv.URL).Notify(ctx, downEvent())
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("취소됐는데 성공으로 반환됐다")
	}
	// 취소가 안 먹었다면 서버 지연만큼 기다렸을 것이다.
	if elapsed >= serverDelay {
		t.Errorf("취소에 %v 걸렸다 — 서버 지연(%v)을 다 기다린 셈이다", elapsed, serverDelay)
	}
	t.Logf("취소 후 %v 만에 반환", elapsed.Round(time.Millisecond))
}

// ─────────────── 설정 ───────────────

func TestFromConfigRejectsBadWebhook(t *testing.T) {
	cases := map[string]config.Alerts{
		"환경변수 미치환": {Discord: "${DISCORD_WEBHOOK}"},
		"스킴 없음":    {Slack: "hooks.slack.com/services/x"},
		"호스트 없음":   {Discord: "https://"},
		"잘못된 쿨다운":  {Cooldown: "5분"},
	}
	for name, a := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, err := FromConfig(a, quietLogger()); err == nil {
				t.Error("에러를 기대했는데 통과했다")
			}
		})
	}
}

func TestFromConfigWithNoWebhooks(t *testing.T) {
	// 웹훅이 없어도 판정은 돌아야 한다
	rules, d, err := FromConfig(config.Alerts{}, quietLogger())
	if err != nil {
		t.Fatalf("웹훅 없는 설정이 거부됐다: %v", err)
	}
	if d.Enabled() {
		t.Error("채널이 없는데 Enabled() 가 true 다")
	}
	if rules.FailureThreshold <= 0 || rules.Cooldown < 0 {
		t.Errorf("기본 규칙이 이상하다: %+v", rules)
	}
}

func TestFromConfigBuildsNotifiers(t *testing.T) {
	_, d, err := FromConfig(config.Alerts{
		Discord: "https://discord.com/api/webhooks/1/x",
		Slack:   "https://hooks.slack.com/services/x",
	}, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	names := d.Names()
	if len(names) != 2 {
		t.Fatalf("채널 %d개, 기대값 2개: %v", len(names), names)
	}
}
