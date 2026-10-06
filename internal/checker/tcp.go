package checker

import (
	"context"
	"net"
	"time"

	"github.com/Archibald1948/upcheck/internal/config"
)

// tcpProber 는 포트에 연결이 되는지만 본다.
type tcpProber struct {
	dialer *net.Dialer
}

func newTCPProber() *tcpProber {
	// Timeout 은 ctx 가 들고 있다.
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
	conn.Close()

	res.OK = true
	res.Detail = "연결됨"
	return res
}
