package checker

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/Archibald1948/upcheck/internal/config"
)

// ─────────────── tcp ───────────────

// listenTCP 는 연결을 받아 바로 닫는 서버를 띄운다.
func listenTCP(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0") // 포트 0 = OS가 빈 포트를 골라준다
	if err != nil {
		t.Fatalf("listen 실패: %v", err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return // 리스너가 닫히면 여기로 온다
			}
			conn.Close()
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return ln
}

func TestTCPOpenPort(t *testing.T) {
	ln := listenTCP(t)
	c := New()
	defer c.Close()

	res := c.Check(context.Background(), config.Monitor{
		Name: "열린포트", Type: config.TypeTCP, Target: ln.Addr().String(), TimeoutMS: 2000,
	})
	if !res.OK {
		t.Fatalf("열린 포트인데 실패: %v", res.Err)
	}
	if res.Latency <= 0 {
		t.Error("Latency 가 측정되지 않았다")
	}
	// tcp 는 상태 코드 개념이 없다
	if res.StatusCode != 0 {
		t.Errorf("StatusCode = %d, tcp 에서는 0 이어야 한다", res.StatusCode)
	}
}

func TestTCPClosedPort(t *testing.T) {
	// 리스너를 띄웠다가 바로 닫아서 '확실히 아무도 없는 포트'를 얻는다.
	ln := listenTCP(t)
	addr := ln.Addr().String()
	ln.Close()

	c := New()
	defer c.Close()

	res := c.Check(context.Background(), config.Monitor{
		Name: "닫힌포트", Type: config.TypeTCP, Target: addr, TimeoutMS: 2000,
	})
	if res.OK {
		t.Fatal("닫힌 포트인데 OK 로 판정됐다")
	}
	if res.Err == nil {
		t.Fatal("Err 가 nil 이다")
	}
}

func TestTCPRespectsCancel(t *testing.T) {
	c := New()
	defer c.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 시작 전에 이미 취소

	// 라우팅되지 않는 주소 — 취소가 없으면 오래 매달린다
	res := c.Check(ctx, config.Monitor{
		Name: "취소", Type: config.TypeTCP, Target: "10.255.255.1:9", TimeoutMS: 5000,
	})
	if res.OK {
		t.Fatal("취소됐는데 OK 로 판정됐다")
	}
	if res.Err == nil || res.Err.Error() != "취소됨(종료 중)" {
		t.Errorf("Err = %v, 기대값 '취소됨(종료 중)'", res.Err)
	}
}

// ─────────────── tls ───────────────

// newTLSServer 는 지정한 유효기간의 자체 서명 인증서로 TLS 서버를 띄운다.
//
// 인증서 만료 경고를 검증하려면 만료일을 내 마음대로 정할 수 있어야 한다.
// 실제 사이트를 쓰면 만료일이 언제 바뀔지 몰라 테스트가 불안정해진다.
func newTLSServer(t *testing.T, validFor time.Duration) (addr string, pool *x509.CertPool) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("키 생성 실패: %v", err)
	}

	tmpl := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "upcheck-test"},
		Issuer:                pkix.Name{CommonName: "upcheck-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(validFor),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:              []string{"localhost"},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}

	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("인증서 생성 실패: %v", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("인증서 파싱 실패: %v", err)
	}

	pool = x509.NewCertPool()
	pool.AddCert(leaf)

	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}},
	})
	if err != nil {
		t.Fatalf("TLS listen 실패: %v", err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			// 핸드셰이크를 끝낸 뒤에 닫아야 한다.
			//
			// tls.Listen 의 Accept 는 연결만 돌려주고 핸드셰이크는
			// 첫 Read/Write 때 지연 실행된다. 바로 Close 하면
			// 클라이언트는 인증서를 받아보지도 못하고 EOF 를 만난다.
			// 그러면 '인증서가 유효한가'를 검증할 수 없다.
			go func(c net.Conn) {
				defer c.Close()
				if tc, ok := c.(*tls.Conn); ok {
					ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
					defer cancel()
					_ = tc.HandshakeContext(ctx)
				}
			}(conn)
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return ln.Addr().String(), pool
}

// probeTLS 는 테스트용 CA 를 신뢰하도록 설정한 tlsProber 로 검사한다.
//
// 자체 서명 인증서라 시스템 신뢰 저장소에는 없다. 실제 코드에서는
// 기본 검증을 그대로 쓰지만, 테스트에서는 이 CA 만 추가로 믿게 한다.
func probeTLS(t *testing.T, m config.Monitor, pool *x509.CertPool) Result {
	t.Helper()
	p := &tlsProber{dialer: &net.Dialer{}, rootCAs: pool}
	ctx, cancel := context.WithTimeout(context.Background(), m.Timeout())
	defer cancel()
	return p.Probe(ctx, m)
}

func TestTLSValidCertificate(t *testing.T) {
	addr, pool := newTLSServer(t, 200*24*time.Hour) // 200일 남음

	res := probeTLS(t, config.Monitor{
		Name: "유효", Type: config.TypeTLS, Target: addr,
		TimeoutMS: 3000, CertWarnDays: 30,
	}, pool)

	if !res.OK {
		t.Fatalf("유효한 인증서인데 실패: %v", res.Err)
	}
	if res.Warning != "" {
		t.Errorf("200일 남았는데 경고가 나왔다: %s", res.Warning)
	}
}

// TestTLSExpiringSoonWarns 는 스펙 M4의 "30일 이하면 경고"를 검증한다.
func TestTLSExpiringSoonWarns(t *testing.T) {
	addr, pool := newTLSServer(t, 10*24*time.Hour) // 10일 남음

	res := probeTLS(t, config.Monitor{
		Name: "만료임박", Type: config.TypeTLS, Target: addr,
		TimeoutMS: 3000, CertWarnDays: 30,
	}, pool)

	// 핵심: 경고는 나오되 실패는 아니다.
	// 인증서가 10일 뒤에 만료돼도 서비스는 지금 멀쩡히 돌고 있다.
	if !res.OK {
		t.Fatalf("만료 임박은 실패가 아니어야 한다: %v", res.Err)
	}
	if res.Warning == "" {
		t.Fatal("만료 10일 전인데 경고가 없다")
	}
	if !strings.Contains(res.Warning, "9일") && !strings.Contains(res.Warning, "10일") {
		t.Errorf("경고에 남은 일수가 안 보인다: %s", res.Warning)
	}
	t.Logf("경고: %s", res.Warning)
}

func TestTLSExpiredCertificateFails(t *testing.T) {
	addr, pool := newTLSServer(t, -time.Hour) // 이미 만료

	res := probeTLS(t, config.Monitor{
		Name: "만료됨", Type: config.TypeTLS, Target: addr,
		TimeoutMS: 3000, CertWarnDays: 30,
	}, pool)

	// 만료된 인증서는 핸드셰이크 자체가 실패한다 = 진짜 장애다
	if res.OK {
		t.Fatal("만료된 인증서인데 OK 로 판정됐다")
	}
	if res.Err == nil {
		t.Fatal("Err 가 nil 이다")
	}
	t.Logf("실패 사유: %v", res.Err)
}

func TestTLSUntrustedCertificateFails(t *testing.T) {
	addr, _ := newTLSServer(t, 200*24*time.Hour)

	// pool 을 넘기지 않는다 = 시스템 기본 검증. 자체 서명이라 실패해야 한다.
	res := probeTLS(t, config.Monitor{
		Name: "미신뢰", Type: config.TypeTLS, Target: addr,
		TimeoutMS: 3000, CertWarnDays: 30,
	}, nil)

	if res.OK {
		t.Fatal("신뢰할 수 없는 인증서인데 OK 로 판정됐다")
	}
}

func TestTLSConnectionRefused(t *testing.T) {
	ln := listenTCP(t)
	addr := ln.Addr().String()
	ln.Close()

	c := New()
	defer c.Close()

	res := c.Check(context.Background(), config.Monitor{
		Name: "연결거부", Type: config.TypeTLS, Target: addr, TimeoutMS: 2000, CertWarnDays: 30,
	})
	if res.OK {
		t.Fatal("연결이 안 되는데 OK 로 판정됐다")
	}
}

// ─────────────── dns ───────────────

func TestDNSResolvesLocalhost(t *testing.T) {
	c := New()
	defer c.Close()

	// localhost 는 어디서든 127.0.0.1 로 풀린다. 외부 의존이 없다.
	res := c.Check(context.Background(), config.Monitor{
		Name: "localhost", Type: config.TypeDNS, Target: "localhost",
		Record: "A", Expect: []string{"127.0.0.1"}, TimeoutMS: 3000,
	})
	if !res.OK {
		t.Fatalf("localhost 해석 실패: %v", res.Err)
	}
}

func TestDNSWrongExpectation(t *testing.T) {
	c := New()
	defer c.Close()

	res := c.Check(context.Background(), config.Monitor{
		Name: "틀린기대", Type: config.TypeDNS, Target: "localhost",
		Record: "A", Expect: []string{"10.1.2.3"}, TimeoutMS: 3000,
	})
	if res.OK {
		t.Fatal("기대값이 다른데 OK 로 판정됐다")
	}
	if res.Err == nil {
		t.Fatal("Err 가 nil 이다")
	}
	// 에러 메시지에 실제 값과 기대값이 둘 다 보여야 진단이 된다
	msg := res.Err.Error()
	if !strings.Contains(msg, "127.0.0.1") || !strings.Contains(msg, "10.1.2.3") {
		t.Errorf("에러에 실제/기대 값이 다 안 보인다: %s", msg)
	}
	t.Logf("실패 사유: %s", msg)
}

func TestDNSNoExpectationJustResolves(t *testing.T) {
	c := New()
	defer c.Close()

	// Expect 가 비면 "해석만 되면 정상"
	res := c.Check(context.Background(), config.Monitor{
		Name: "해석만", Type: config.TypeDNS, Target: "localhost",
		Record: "A", TimeoutMS: 3000,
	})
	if !res.OK {
		t.Fatalf("해석되는데 실패: %v", res.Err)
	}
}

func TestDNSNonexistentHost(t *testing.T) {
	c := New()
	defer c.Close()

	res := c.Check(context.Background(), config.Monitor{
		Name: "없는호스트", Type: config.TypeDNS,
		Target: "this-host-does-not-exist-upcheck.invalid",
		Record: "A", TimeoutMS: 3000,
	})
	if res.OK {
		t.Fatal("없는 호스트인데 OK 로 판정됐다")
	}
}

// TestContainsFold 는 도메인 비교가 대소문자와 끝점(.)을 무시하는지 본다.
func TestContainsFold(t *testing.T) {
	list := []string{"Example.COM.", "1.2.3.4"}

	cases := map[string]bool{
		"example.com":  true, // 대소문자 무시
		"example.com.": true, // 끝점 무시
		"EXAMPLE.COM":  true,
		"1.2.3.4":      true,
		"other.com":    false,
	}
	for want, expect := range cases {
		if got := containsFold(list, want); got != expect {
			t.Errorf("containsFold(%q) = %v, 기대값 %v", want, got, expect)
		}
	}
}

func TestIPNetwork(t *testing.T) {
	if got := ipNetwork("AAAA"); got != "ip6" {
		t.Errorf("AAAA → %q, 기대값 ip6", got)
	}
	if got := ipNetwork("A"); got != "ip4" {
		t.Errorf("A → %q, 기대값 ip4", got)
	}
}
