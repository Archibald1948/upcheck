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
// 커넥션 풀을 재사용하려고 http.Client 를 필드로 들고 있는다.
type httpProber struct {
	client *http.Client
}

func newHTTPProber() *httpProber {
	// DefaultTransport 는 MaxIdleConnsPerHost 가 2라서 복사해서 튜닝한다.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConns = 200
	transport.MaxIdleConnsPerHost = 10
	transport.IdleConnTimeout = 90 * time.Second

	return &httpProber{
		client: &http.Client{
			Transport: transport,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 5 {
					return fmt.Errorf("리다이렉트가 너무 많다 (%d회)", len(via))
				}
				return nil
			},
			// Timeout 은 걸지 않는다. 셧다운 시 즉시 취소되도록 요청마다 context 타임아웃을 쓴다.
		},
	}
}

func (p *httpProber) Type() string { return config.TypeHTTP }

// Close 는 유휴 커넥션을 정리한다.
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
	defer resp.Body.Close()

	res.StatusCode = resp.StatusCode

	var body string
	if m.Keyword != "" {
		// 1MB 상한
		b, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if readErr != nil {
			res.Latency = time.Since(start)
			res.Err = fmt.Errorf("본문 읽기 실패: %w", readErr)
			return res
		}
		body = string(b)
	}
	// 본문을 끝까지 읽어야 커넥션이 재사용된다.
	_, _ = io.Copy(io.Discard, resp.Body)

	res.Latency = time.Since(start)

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
