package alert

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// webhook 은 Discord/Slack 이 공유하는 발송 로직이다.
//
// 두 채널 모두 "JSON 을 POST 한다"는 점은 같고 본문 모양만 다르다.
// 공통부를 여기 두고, 본문 만들기만 payload 함수로 갈아끼운다.
type webhook struct {
	name    string
	url     string
	client  *http.Client
	payload func(Event) any
}

// Name 은 Notifier 인터페이스 구현이다.
func (w *webhook) Name() string { return w.name }

// newWebhookClient 는 알림 전용 HTTP 클라이언트를 만든다.
//
// checker 의 클라이언트를 재사용하지 않는다. 모니터링 트래픽과
// 알림 트래픽이 커넥션 풀을 공유하면 서로 영향을 준다.
func newWebhookClient() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxIdleConnsPerHost = 2
	return &http.Client{Transport: t}
}

// maxAttempts 는 발송 재시도 횟수다.
//
// 웹훅은 일시적으로 실패하는 일이 흔하다(429 rate limit, 502 등).
// 다만 여기 오래 매달리면 뒤의 알림이 밀리므로 몇 번만 시도한다.
const maxAttempts = 3

// Notify 는 이벤트를 웹훅으로 보낸다. Notifier 인터페이스 구현이다.
func (w *webhook) Notify(ctx context.Context, ev Event) error {
	body, err := json.Marshal(w.payload(ev))
	if err != nil {
		return fmt.Errorf("본문 생성 실패: %w", err)
	}

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if attempt > 1 {
			// 재시도 전에 잠깐 기다린다. 기다리는 중에도 취소는 즉시 먹어야 한다.
			wait := backoff(attempt, lastErr)
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return fmt.Errorf("재시도 대기 중 취소됨: %w", ctx.Err())
			}
		}

		retryable, err := w.post(ctx, body)
		if err == nil {
			return nil
		}
		lastErr = err
		if !retryable {
			return err
		}
	}
	return fmt.Errorf("%d회 시도 후 실패: %w", maxAttempts, lastErr)
}

// post 는 한 번 보낸다. 재시도할 만한 실패인지도 함께 알려준다.
func (w *webhook) post(ctx context.Context, body []byte) (retryable bool, err error) {
	// bytes.NewReader 로 감싸는 이유: 재시도할 때마다 본문을 처음부터
	// 다시 읽어야 하는데, Request.Body 는 한 번 읽으면 소진된다.
	// 매 시도마다 새 Reader 를 만들면 그 문제가 없다.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.url, bytes.NewReader(body))
	if err != nil {
		return false, fmt.Errorf("요청 생성 실패: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "upcheck/0.1")

	resp, err := w.client.Do(req)
	if err != nil {
		// 네트워크 오류는 대개 일시적이다. 단, ctx 취소면 재시도 의미가 없다.
		return ctx.Err() == nil, fmt.Errorf("요청 실패: %w", err)
	}
	defer resp.Body.Close()

	// 본문을 비워야 커넥션이 재사용된다. 실패 응답은 진단에 쓸 수 있게 조금 읽어 둔다.
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	io.Copy(io.Discard, resp.Body)

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return false, nil
	case resp.StatusCode == http.StatusTooManyRequests:
		return true, &httpError{Status: resp.StatusCode, RetryAfter: retryAfter(resp), Body: string(snippet)}
	case resp.StatusCode >= 500:
		return true, &httpError{Status: resp.StatusCode, Body: string(snippet)}
	default:
		// 4xx 는 우리가 잘못 보낸 것이다. 다시 보내도 똑같이 실패한다.
		return false, &httpError{Status: resp.StatusCode, Body: string(snippet)}
	}
}

// httpError 는 웹훅이 돌려준 실패 응답이다.
type httpError struct {
	Status     int
	RetryAfter time.Duration
	Body       string
}

func (e *httpError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("HTTP %d", e.Status)
	}
	return fmt.Sprintf("HTTP %d: %s", e.Status, e.Body)
}

// retryAfter 는 Retry-After 헤더를 읽는다. 없으면 0.
func retryAfter(resp *http.Response) time.Duration {
	v := resp.Header.Get("Retry-After")
	if v == "" {
		return 0
	}
	// 초 단위 정수 형식만 다룬다. HTTP 날짜 형식도 있지만 웹훅에서는 드물다.
	if sec, err := strconv.Atoi(v); err == nil && sec >= 0 {
		return time.Duration(sec) * time.Second
	}
	return 0
}

// backoff 는 재시도 대기 시간을 정한다.
//
// 서버가 Retry-After 로 말해주면 그걸 따르고, 아니면 지수적으로 늘린다.
// 지수 백오프는 "실패하는 서버를 계속 두들기지 않는다"는 기본 예의다.
func backoff(attempt int, lastErr error) time.Duration {
	var he *httpError
	if errors.As(lastErr, &he) && he.RetryAfter > 0 {
		if he.RetryAfter > 30*time.Second {
			return 30 * time.Second // 너무 오래 매달리지 않는다
		}
		return he.RetryAfter
	}
	// 2회차 200ms, 3회차 400ms
	return time.Duration(1<<(attempt-1)) * 100 * time.Millisecond
}

// ─────────────── Discord ───────────────

// NewDiscord 는 Discord 웹훅 Notifier 를 만든다.
//
// 반환 타입이 Notifier(인터페이스)가 아니라 *webhook(구체 타입)인 것에 주목.
// "Accept interfaces, return structs" — 받을 때 넓게 받고 돌려줄 때는 구체적으로.
func NewDiscord(url string) *webhook {
	return &webhook{
		name:    "discord",
		url:     url,
		client:  newWebhookClient(),
		payload: discordPayload,
	}
}

func discordPayload(ev Event) any {
	// Discord 임베드는 색을 10진 정수로 받는다.
	color := 0xE74C3C // 빨강
	if ev.Kind == KindUp {
		color = 0x2ECC71 // 초록
	}
	return map[string]any{
		"embeds": []map[string]any{{
			"title":       ev.Title(),
			"description": ev.Body(),
			"color":       color,
			"timestamp":   ev.At.UTC().Format(time.RFC3339),
		}},
	}
}

// ─────────────── Slack ───────────────

// NewSlack 은 Slack 웹훅 Notifier 를 만든다.
func NewSlack(url string) *webhook {
	return &webhook{
		name:    "slack",
		url:     url,
		client:  newWebhookClient(),
		payload: slackPayload,
	}
}

func slackPayload(ev Event) any {
	emoji := ":red_circle:"
	if ev.Kind == KindUp {
		emoji = ":large_green_circle:"
	}
	return map[string]any{
		"text": fmt.Sprintf("%s *%s*\n%s", emoji, ev.Title(), ev.Body()),
	}
}
