package checker

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/Archibald1948/upcheck/internal/config"
)

// dnsProber 는 레코드가 기대한 값으로 해석되는지 본다.
//
// DNS 가 엉뚱한 곳을 가리키는 사고는 서버가 멀쩡해도 서비스를 죽인다.
// HTTP 체크로는 안 잡힌다 — 엉뚱한 서버가 200 을 잘 돌려주기 때문이다.
type dnsProber struct {
	// 시스템 기본 리졸버. 모니터마다 다른 리졸버를 쓰면 그때 새로 만든다.
	system *net.Resolver
}

func newDNSProber() *dnsProber { return &dnsProber{system: &net.Resolver{}} }

func (p *dnsProber) Type() string { return config.TypeDNS }

func (p *dnsProber) Probe(ctx context.Context, m config.Monitor) Result {
	var res Result

	resolver := p.system
	if m.Resolver != "" {
		resolver = customResolver(m.Resolver)
	}

	start := time.Now()
	got, err := lookup(ctx, resolver, m.Record, m.Target)
	res.Latency = time.Since(start)

	if err != nil {
		res.Err = classifyErr(ctx, err)
		return res
	}
	if len(got) == 0 {
		res.Err = fmt.Errorf("%s 레코드가 없다", m.Record)
		return res
	}

	sort.Strings(got) // 출력이 실행마다 흔들리지 않게
	res.Detail = m.Record + " " + strings.Join(got, " ")

	// Expect 가 비어 있으면 "해석만 되면 정상"이다.
	if len(m.Expect) == 0 {
		res.OK = true
		return res
	}

	// 기대값 중 하나라도 나오면 정상으로 본다.
	// 라운드로빈 DNS 처럼 여러 값이 번갈아 나오는 경우를 받아주려는 것이다.
	for _, want := range m.Expect {
		if containsFold(got, want) {
			res.OK = true
			return res
		}
	}

	res.Err = fmt.Errorf("%s 레코드가 [%s] — 기대값 [%s] 중 하나여야 한다",
		m.Record, strings.Join(got, " "), strings.Join(m.Expect, " "))
	return res
}

// customResolver 는 지정한 DNS 서버만 쓰는 리졸버를 만든다.
//
// PreferGo 를 켜야 Go 자체 DNS 구현이 쓰이고, 그래야 Dial 을 가로챌 수 있다.
// cgo 리졸버(OS 기본)는 서버를 골라 쓸 방법이 없다.
func customResolver(addr string) *net.Resolver {
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			// 세 번째 인자(원래 주소)를 버리고 우리가 지정한 서버로 보낸다.
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
	}
}

// lookup 은 레코드 종류에 맞는 조회를 하고 결과를 문자열로 돌려준다.
func lookup(ctx context.Context, r *net.Resolver, record, host string) ([]string, error) {
	switch record {
	case "A", "AAAA":
		ips, err := r.LookupIP(ctx, ipNetwork(record), host)
		if err != nil {
			return nil, err
		}
		out := make([]string, 0, len(ips))
		for _, ip := range ips {
			out = append(out, ip.String())
		}
		return out, nil

	case "CNAME":
		cname, err := r.LookupCNAME(ctx, host)
		if err != nil {
			return nil, err
		}
		return []string{strings.TrimSuffix(cname, ".")}, nil

	case "TXT":
		return r.LookupTXT(ctx, host)

	case "MX":
		mxs, err := r.LookupMX(ctx, host)
		if err != nil {
			return nil, err
		}
		out := make([]string, 0, len(mxs))
		for _, mx := range mxs {
			out = append(out, strings.TrimSuffix(mx.Host, "."))
		}
		return out, nil

	case "NS":
		nss, err := r.LookupNS(ctx, host)
		if err != nil {
			return nil, err
		}
		out := make([]string, 0, len(nss))
		for _, ns := range nss {
			out = append(out, strings.TrimSuffix(ns.Host, "."))
		}
		return out, nil

	default:
		return nil, fmt.Errorf("지원하지 않는 레코드 종류 %q", record)
	}
}

// ipNetwork 은 LookupIP 에 넘길 네트워크 이름이다.
func ipNetwork(record string) string {
	if record == "AAAA" {
		return "ip6"
	}
	return "ip4"
}

// containsFold 는 대소문자를 무시하고 값이 들어 있는지 본다.
//
// 도메인 이름은 대소문자를 구분하지 않는다. "Example.com" 과
// "example.com" 은 같은 이름이라, 설정에 대문자로 적었다고 실패하면 안 된다.
func containsFold(list []string, want string) bool {
	for _, v := range list {
		if strings.EqualFold(strings.TrimSuffix(v, "."), strings.TrimSuffix(want, ".")) {
			return true
		}
	}
	return false
}
