// Package metrics 는 Prometheus 가 긁어갈 지표를 모은다.
//
// 지표는 "무슨 일이 일어났는지"를 숫자로 남기는 것이고, 로그와 달리
// 시간에 따른 추이를 볼 수 있다. 알림이 "지금 죽었다"를 알려준다면
// 지표는 "요즘 느려지고 있다"를 보여준다.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

	"github.com/Archibald1948/upcheck/internal/checker"
)

// Metrics 는 upcheck 가 내보내는 지표 묶음이다.
type Metrics struct {
	reg *prometheus.Registry

	checksTotal   *prometheus.CounterVec
	checkDuration *prometheus.HistogramVec
	monitorUp     *prometheus.GaugeVec
	certDaysLeft  *prometheus.GaugeVec
}

// New 는 전용 레지스트리에 지표를 등록해서 돌려준다.
//
// prometheus.DefaultRegisterer(전역)를 쓰지 않는 이유:
// 전역이라 테스트가 서로 간섭한다. 같은 지표를 두 번 등록하면 panic 이라,
// 테스트를 두 개만 돌려도 깨진다. 레지스트리를 직접 만들면 그럴 일이 없다.
func New() *Metrics {
	m := &Metrics{reg: prometheus.NewRegistry()}

	// 라벨은 카디널리티(값의 가짓수)를 곱한다.
	// monitor × type × result 라서 모니터 1000개여도 수천 개 수준이다.
	// URL 이나 에러 메시지를 라벨로 넣으면 무한히 늘어나 Prometheus 를 죽인다.
	m.checksTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "upcheck_checks_total",
		Help: "체크 실행 횟수",
	}, []string{"monitor", "type", "result"})

	m.checkDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "upcheck_check_duration_seconds",
		Help: "체크 응답시간",
		// 기본 버킷(5ms~10s)이 HTTP 응답시간에 그대로 맞는다.
		Buckets: prometheus.DefBuckets,
	}, []string{"monitor", "type"})

	m.monitorUp = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "upcheck_monitor_up",
		Help: "모니터의 마지막 체크 결과 (1=성공, 0=실패)",
	}, []string{"monitor", "type"})

	m.certDaysLeft = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "upcheck_tls_cert_days_left",
		Help: "TLS 인증서 만료까지 남은 일수",
	}, []string{"monitor"})

	m.reg.MustRegister(m.checksTotal, m.checkDuration, m.monitorUp, m.certDaysLeft)

	// Go 런타임과 프로세스 지표. goroutine 수, 힙 크기, 열린 파일 수 등이
	// 공짜로 따라온다. goroutine 누수 감시(스펙 6절 완료 기준)에 바로 쓰인다.
	m.reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	return m
}

// Registry 는 /metrics 핸들러가 쓸 레지스트리다.
func (m *Metrics) Registry() *prometheus.Registry { return m.reg }

// ObserveCheck 은 체크 결과 하나를 지표에 반영한다.
//
// collector 의 onResult 훅에서 불린다. 훅은 collector goroutine 하나에서만
// 불리지만, prometheus 지표는 원래 동시 호출에 안전하다.
func (m *Metrics) ObserveCheck(res checker.Result) {
	result := "fail"
	up := 0.0
	if res.OK {
		result = "ok"
		up = 1
	}

	m.checksTotal.WithLabelValues(res.Monitor, res.Type, result).Inc()
	m.monitorUp.WithLabelValues(res.Monitor, res.Type).Set(up)

	// 실패한 체크의 응답시간은 히스토그램에 넣지 않는다.
	// 대부분 타임아웃까지 걸린 시간이라 분위수를 왜곡한다.
	// (M2에서 p50/p95 를 성공만으로 계산한 것과 같은 이유)
	if res.OK {
		m.checkDuration.WithLabelValues(res.Monitor, res.Type).Observe(res.Latency.Seconds())
	}

	if res.CertDaysLeft != nil {
		m.certDaysLeft.WithLabelValues(res.Monitor).Set(float64(*res.CertDaysLeft))
	}
}

// AddGauge 는 오르내리는 값을 그때그때 읽어 가는 게이지를 등록한다.
//
// 스케줄러나 collector 처럼 이미 다른 곳에서 세고 있는 값은, 여기로 복사해
// 오는 대신 읽는 함수를 넘겨 둔다. Prometheus 가 긁어갈 때마다 fn 이 불린다.
// 덕분에 metrics 패키지가 scheduler·collector·alert 를 import 하지 않아도 된다.
//
// 주의: fn 은 스크레이프 goroutine 에서 불린다. 원자적으로 읽을 수 있는 값만 넘겨야 한다.
func (m *Metrics) AddGauge(name, help string, fn func() float64) {
	m.reg.MustRegister(prometheus.NewGaugeFunc(
		prometheus.GaugeOpts{Name: name, Help: help}, fn,
	))
}

// AddCounter 는 줄어들지 않는 누적값을 등록한다.
//
// 게이지와 타입을 구분하는 건 취향이 아니다. Prometheus 는 카운터에만
// rate() 를 쓸 수 있고, 값이 줄면 "프로세스가 재시작했다"로 해석한다.
// 누적 횟수를 게이지로 내보내면 그 계산이 전부 틀어진다.
func (m *Metrics) AddCounter(name, help string, fn func() float64) {
	m.reg.MustRegister(prometheus.NewCounterFunc(
		prometheus.CounterOpts{Name: name, Help: help}, fn,
	))
}
