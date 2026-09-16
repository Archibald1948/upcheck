package checker

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"time"

	"github.com/Archibald1948/upcheck/internal/config"
)

// tlsProber 는 TLS 핸드셰이크를 해보고 인증서 만료까지 남은 날을 센다.
//
// 인증서 만료는 예고된 장애다. 만료된 뒤에 알게 되면 이미 늦었다.
type tlsProber struct {
	dialer *net.Dialer

	// rootCAs 가 nil 이면 시스템 신뢰 저장소를 쓴다. 실제 운영에서는 항상 nil 이다.
	//
	// 테스트에서만 채운다. 인증서 만료 경고를 검증하려면 만료일을 마음대로
	// 정한 인증서가 필요한데, 그런 건 자체 서명일 수밖에 없어서
	// 시스템 저장소에는 없다. 실제 사이트를 쓰면 만료일이 언제 바뀔지 몰라
	// 테스트가 불안정해진다.
	rootCAs *x509.CertPool
}

func newTLSProber() *tlsProber { return &tlsProber{dialer: &net.Dialer{}} }

func (p *tlsProber) Type() string { return config.TypeTLS }

func (p *tlsProber) Probe(ctx context.Context, m config.Monitor) Result {
	var res Result

	host, _, err := net.SplitHostPort(m.Target)
	if err != nil {
		res.Err = fmt.Errorf("target 을 host:port 로 나눌 수 없다: %w", err)
		return res
	}

	start := time.Now()

	// tls.Dialer 는 TCP 연결과 핸드셰이크를 함께 한다.
	// ServerName 을 반드시 넘겨야 SNI 가 붙는다. 한 IP 에 여러 사이트가
	// 올라가 있으면 이게 없을 때 엉뚱한 인증서를 받는다.
	d := &tls.Dialer{
		NetDialer: p.dialer,
		Config: &tls.Config{
			ServerName: host,
			RootCAs:    p.rootCAs, // nil 이면 시스템 기본값
		},
	}

	conn, err := d.DialContext(ctx, "tcp", m.Target)
	res.Latency = time.Since(start)
	if err != nil {
		// 만료·이름 불일치·체인 오류가 전부 여기로 온다.
		// 기본 검증을 끄지 않았으므로 "핸드셰이크 성공 = 인증서 유효"다.
		res.Err = classifyErr(ctx, err)
		return res
	}
	defer conn.Close()

	// DialContext 는 net.Conn 을 돌려주지만 실제로는 *tls.Conn 이다.
	// 인증서를 보려면 구체 타입으로 꺼내야 한다.
	tlsConn, ok := conn.(*tls.Conn)
	if !ok {
		res.Err = fmt.Errorf("TLS 연결이 아니다")
		return res
	}

	certs := tlsConn.ConnectionState().PeerCertificates
	if len(certs) == 0 {
		res.Err = fmt.Errorf("서버가 인증서를 보내지 않았다")
		return res
	}
	// [0]이 서버 인증서(leaf)다. 그 뒤는 중간 CA 체인이다.
	leaf := certs[0]

	// 핸드셰이크가 성공했으니 이 시점에 인증서는 유효하다.
	res.OK = true

	daysLeft := int(time.Until(leaf.NotAfter).Hours() / 24)
	res.CertDaysLeft = &daysLeft
	res.Detail = fmt.Sprintf("만료 D-%d (%s)", daysLeft, leaf.NotAfter.Local().Format("2006-01-02"))

	if daysLeft <= m.CertWarnDays {
		res.Warning = fmt.Sprintf("인증서 만료까지 %d일 (%s, 발급자 %s)",
			daysLeft, leaf.NotAfter.Local().Format("2006-01-02"), issuerName(leaf.Issuer.CommonName))
	}
	return res
}

// issuerName 은 발급자 이름이 비어 있을 때를 대비한다.
func issuerName(cn string) string {
	if cn == "" {
		return "알 수 없음"
	}
	return cn
}
