// Package checker 는 모니터 하나를 실제로 찔러보고 결과를 만든다.
//
// 타입별 검사는 Prober 인터페이스로 나뉘어 있다.
//
//	http  상태 코드 · 본문 키워드 · 리다이렉트 정책   (http.go)
//	tcp   포트 연결 가능 여부                        (tcp.go)
//	tls   인증서 만료까지 남은 일수                   (tls.go)
//	dns   레코드가 기대값으로 해석되는지              (dns.go)
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
	//
	// 같은 값이 Detail·Warning 문자열에도 들어가지만, 지표로 쓰려면 숫자가 필요하다.
	// 문자열에서 다시 파싱하는 건 문구를 손볼 때마다 조용히 깨진다.
	CertDaysLeft *int

	// Warning 은 "실패는 아니지만 알아둘 것"이다.
	// 지금은 tls 인증서 만료 임박에만 쓴다.
	//
	// 실패로 처리하지 않는 이유: 인증서가 30일 뒤에 만료돼도
	// 서비스는 지금 멀쩡히 돌고 있다. OK=false 로 만들면
	// 업타임 통계가 망가지고 한밤중에 가짜 장애 알림이 간다.
	Warning string
}

// Prober 는 체크 타입 하나를 구현한다.
//
// M3의 Notifier 와 같은 꼴이다. 새 타입을 추가하려면
// 이 인터페이스를 구현하고 New() 의 맵에 한 줄 넣으면 된다.
// 스케줄러와 collector 는 손댈 필요가 없다.
type Prober interface {
	// Probe 는 모니터를 검사한다.
	//
	// 결과에 Monitor/Type/CheckedAt 을 채울 필요는 없다 —
	// Checker 가 공통으로 채운다. 구현체는 OK/Err/Latency 등
	// 자기가 아는 것만 채우면 된다.
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
//
// 첫 번째 인자로 context.Context 를 받는 건 Go의 강한 관례다.
// 이 ctx 가 취소되면(= 프로그램 종료, 타임아웃) 진행 중인 작업도 즉시 끊긴다.
func (c *Checker) Check(ctx context.Context, m config.Monitor) Result {
	res := Result{
		Monitor:   m.Name,
		Type:      m.Type,
		CheckedAt: time.Now().UTC(), // 저장은 항상 UTC (스펙 7절 함정 4번)
	}

	prober, ok := c.probers[m.Type]
	if !ok {
		res.Err = fmt.Errorf("지원하지 않는 체크 타입 %q", m.Type)
		return res
	}

	// 이 체크만의 시한을 건다. 부모 ctx 가 먼저 취소되면 이쪽도 같이 취소된다.
	// (context 는 트리 구조라서 부모의 취소가 자식으로 전파된다)
	ctx, cancel := context.WithTimeout(ctx, m.Timeout())
	// defer 는 함수가 끝날 때 실행된다. cancel 을 부르지 않으면
	// context 내부 타이머가 남아서 goroutine/메모리가 샌다.
	defer cancel()

	out := prober.Probe(ctx, m)

	// Prober 가 채운 것만 옮겨 담는다. 공통 필드는 위에서 이미 채웠다.
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
		// 타입 단언으로 "Close 를 가진 것만" 골라낸다.
		// 모든 Prober 가 정리할 자원을 갖는 건 아니라서
		// Prober 인터페이스에 Close 를 넣지 않았다.
		if closer, ok := p.(interface{ Close() }); ok {
			closer.Close()
		}
	}
}

// classifyErr 은 하위 계층이 준 에러를 사람이 읽을 만한 문구로 바꾼다.
//
// 타임아웃과 "프로그램이 종료돼서 취소됨"은 원인이 전혀 다른데
// 둘 다 context 에러로 뭉뚱그려 오기 때문에 구분해준다.
func classifyErr(ctx context.Context, err error) error {
	// ctx.Err() 는 취소된 이유를 알려준다. 아직 살아 있으면 nil.
	switch ctx.Err() {
	case context.DeadlineExceeded:
		return fmt.Errorf("타임아웃")
	case context.Canceled:
		return fmt.Errorf("취소됨(종료 중)")
	}
	return err
}
