package checker

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Archibald1948/upcheck/internal/config"
)

// httpProber 는 HTTP 상태 코드와 본문 키워드를 검사한다.
//
// http.Client 를 필드로 들고 있는 게 핵심이다.
// 체크할 때마다 &http.Client{} 를 새로 만들면 커넥션 풀이 매번 버려져서
// 매 요청이 TCP 핸드셰이크 + TLS 핸드셰이크를 다시 한다. (스펙 7절 함정 1번)
type httpProber struct {
	client *http.Client
}

func newHTTPProber() *httpProber {
	// http.DefaultTransport 를 그대로 쓰지 않고 복사해서 튜닝한다.
	// DefaultTransport 는 MaxIdleConnsPerHost 가 2라서,
	// 같은 호스트를 여러 개 감시하면 유휴 커넥션이 금방 버려진다.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConns = 200
	transport.MaxIdleConnsPerHost = 10
	transport.IdleConnTimeout = 90 * time.Second

	return &httpProber{
		client: &http.Client{
			Transport: transport,
			// 리다이렉트를 따라가되 너무 깊어지면 멈춘다.
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 5 {
					return fmt.Errorf("리다이렉트가 너무 많다 (%d회)", len(via))
				}
				return nil
			},
			// 주의: 여기에 Timeout 을 걸지 않는다.
			// Client.Timeout 은 셧다운 시 즉시 취소가 안 된다.
			// 대신 요청마다 context 타임아웃을 건다. (스펙 7절 함정 3번)
		},
	}
}

func (p *httpProber) Type() string { return config.TypeHTTP }

// Close 는 유휴 커넥션을 정리한다.
//
// 안 불러도 IdleConnTimeout(90초) 이 지나면 알아서 닫히지만,
// 그때까지 커넥션과 그에 딸린 goroutine 이 남아 있다.
func (p *httpProber) Close() { p.client.CloseIdleConnections() }

func (p *httpProber) Probe(ctx context.Context, m config.Monitor) Result {
	var res Result

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.Target, nil)
	if err != nil {
		res.Err = fmt.Errorf("요청 생성 실패: %w", err)
		return res
	}
	req.Header.Set("User-Agent", "upcheck/0.1")

	start := time.Now()
	resp, err := p.client.Do(req)
	if err != nil {
		res.Latency = time.Since(start)
		res.Err = classifyErr(ctx, err)
		return res
	}
	// 응답 본문은 반드시 닫아야 한다. 안 닫으면 커넥션이 반납되지 않는다.
	defer resp.Body.Close()

	res.StatusCode = resp.StatusCode

	// 본문을 끝까지 읽어야 커넥션이 재사용된다. (스펙 7절 함정 2번)
	// 키워드 매칭이 필요하면 읽은 내용을 쓰고, 아니면 그냥 버린다.
	var body string
	if m.Keyword != "" {
		// 무한정 읽지 않도록 상한을 둔다. 1MB면 키워드 찾기엔 충분하다.
		b, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if readErr != nil {
			res.Latency = time.Since(start)
			res.Err = fmt.Errorf("본문 읽기 실패: %w", readErr)
			return res
		}
		body = string(b)
	}
	// 남은 본문을 비워 커넥션을 깨끗이 반납한다.
	_, _ = io.Copy(io.Discard, resp.Body)

	res.Latency = time.Since(start)

	// 판정: 상태 코드가 기대값과 같아야 하고, 키워드가 있으면 본문에 있어야 한다.
	if resp.StatusCode != m.ExpectedStatus {
		res.Err = fmt.Errorf("상태 코드 %d (기대값 %d)", resp.StatusCode, m.ExpectedStatus)
		return res
	}
	if m.Keyword != "" && !strings.Contains(body, m.Keyword) {
		res.Err = fmt.Errorf("본문에 키워드 %q 가 없다", m.Keyword)
		return res
	}

	res.OK = true
	return res
}
