// Package checker 는 모니터 하나를 실제로 찔러보고 결과를 만든다.
package checker

import (
	"context"
	"fmt"
	"time"

	"github.com/Archibald1948/upcheck/internal/config"
)

// Result 는 체크 한 번의 결과다.
type Result struct {
	Monitor    string
	Type       string
	CheckedAt  time.Time
	OK         bool
	StatusCode int // http 전용. 다른 타입에서는 0
	Latency    time.Duration
	Err        error

	// Detail 은 타입별로 사람이 볼 한 줄 설명이다 (예: "A 8.8.8.8", "만료 D-123").
	// 화면 표시 전용이라 DB에는 저장하지 않는다.
	Detail string

	// CertDaysLeft 는 TLS 인증서 만료까지 남은 일수다. tls 타입에서만 채워진다.
	CertDaysLeft *int

	// Warning 은 "실패는 아니지만 알아둘 것"이다. 지금은 tls 인증서 만료 임박에만 쓴다.
	Warning string
}

// Prober 는 체크 타입 하나를 구현한다.
type Prober interface {
	// Probe 는 모니터를 검사한다. Monitor/Type/CheckedAt 은 Checker 가 채운다.
	Probe(ctx context.Context, m config.Monitor) Result

	// Type 은 이 Prober 가 담당하는 타입 이름이다.
	Type() string
}

// Checker 는 모니터의 타입을 보고 알맞은 Prober 에게 넘긴다.
type Checker struct {
	probers map[string]Prober
}

// New 는 기본 Prober 들을 등록한 Checker 를 만든다.
func New() *Checker {
	c := &Checker{probers: make(map[string]Prober)}
	c.register(newHTTPProber())
	c.register(newTCPProber())
	c.register(newTLSProber())
	c.register(newDNSProber())
	return c
}

func (c *Checker) register(p Prober) { c.probers[p.Type()] = p }

// Types 는 지원하는 타입 목록이다.
func (c *Checker) Types() []string {
	out := make([]string, 0, len(c.probers))
	for t := range c.probers {
		out = append(out, t)
	}
	return out
}

// Check 는 모니터 하나를 검사한다.
func (c *Checker) Check(ctx context.Context, m config.Monitor) Result {
	res := Result{
		Monitor:   m.Name,
		Type:      m.Type,
		CheckedAt: time.Now().UTC(), // 저장은 항상 UTC
	}

	prober, ok := c.probers[m.Type]
	if !ok {
		res.Err = fmt.Errorf("지원하지 않는 체크 타입 %q", m.Type)
		return res
	}

	ctx, cancel := context.WithTimeout(ctx, m.Timeout())
	defer cancel()

	out := prober.Probe(ctx, m)

	res.OK = out.OK
	res.StatusCode = out.StatusCode
	res.Latency = out.Latency
	res.Err = out.Err
	res.Detail = out.Detail
	res.Warning = out.Warning
	res.CertDaysLeft = out.CertDaysLeft
	return res
}

// Close 는 Prober 들이 쥐고 있는 자원을 정리한다. 종료 시 한 번 부른다.
func (c *Checker) Close() {
	for _, p := range c.probers {
		if closer, ok := p.(interface{ Close() }); ok {
			closer.Close()
		}
	}
}

// classifyErr 은 하위 계층이 준 에러를 사람이 읽을 만한 문구로 바꾼다.
// 타임아웃과 종료로 인한 취소를 구분한다.
func classifyErr(ctx context.Context, err error) error {
	switch ctx.Err() {
	case context.DeadlineExceeded:
		return fmt.Errorf("타임아웃")
	case context.Canceled:
		return fmt.Errorf("취소됨(종료 중)")
	}
	return err
}
