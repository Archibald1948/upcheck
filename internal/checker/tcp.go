package checker

import (
	"context"
	"net"
	"time"

	"github.com/Archibald1948/upcheck/internal/config"
)

// tcpProber 는 포트에 연결이 되는지만 본다.
//
// HTTP 를 안 하는 것(데이터베이스, 메시지 큐, SMTP 등)을 감시할 때 쓴다.
// "연결이 받아들여졌다"까지만 확인하고 아무 데이터도 주고받지 않는다.
type tcpProber struct {
	dialer *net.Dialer
}

func newTCPProber() *tcpProber {
	// Dialer 에 Timeout 을 걸지 않는다. DialContext 에 넘기는 ctx 가
	// 이미 시한을 들고 있고, 그쪽이 셧다운 취소까지 함께 처리한다.
	return &tcpProber{dialer: &net.Dialer{}}
}

func (p *tcpProber) Type() string { return config.TypeTCP }

func (p *tcpProber) Probe(ctx context.Context, m config.Monitor) Result {
	var res Result

	start := time.Now()
	conn, err := p.dialer.DialContext(ctx, "tcp", m.Target)
	res.Latency = time.Since(start)

	if err != nil {
		res.Err = classifyErr(ctx, err)
		return res
	}
	// 연결을 붙잡고 있을 이유가 없다. 확인했으면 바로 닫는다.
	// 안 닫으면 감시 대상 서버의 연결 수가 계속 늘어난다.
	conn.Close()

	res.OK = true
	res.Detail = "연결됨"
	return res
}
