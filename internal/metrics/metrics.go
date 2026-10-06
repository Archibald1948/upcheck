// Package metrics 는 Prometheus 가 긁어갈 지표를 모은다.
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
// 전역 레지스트리는 같은 지표를 두 번 등록하면 panic 이라 테스트끼리 간섭한다.
func New() *Metrics {
	m := &Metrics{reg: prometheus.NewRegistry()}

	// URL 이나 에러 메시지를 라벨로 넣으면 카디널리티가 무한히 늘어난다.
	m.checksTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "upcheck_checks_total",
		Help: "체크 실행 횟수",
	}, []string{"monitor", "type", "result"})

	m.checkDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "upcheck_check_duration_seconds",
		Help:    "체크 응답시간",
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

	m.reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	return m
}

// Registry 는 /metrics 핸들러가 쓸 레지스트리다.
func (m *Metrics) Registry() *prometheus.Registry { return m.reg }

// ObserveCheck 은 체크 결과 하나를 지표에 반영한다.
func (m *Metrics) ObserveCheck(res checker.Result) {
	result := "fail"
	up := 0.0
	if res.OK {
		result = "ok"
		up = 1
	}

	m.checksTotal.WithLabelValues(res.Monitor, res.Type, result).Inc()
	m.monitorUp.WithLabelValues(res.Monitor, res.Type).Set(up)

	// 실패한 체크의 응답시간은 대부분 타임아웃이라 분위수를 왜곡하므로 넣지 않는다.
	if res.OK {
		m.checkDuration.WithLabelValues(res.Monitor, res.Type).Observe(res.Latency.Seconds())
	}

	if res.CertDaysLeft != nil {
		m.certDaysLeft.WithLabelValues(res.Monitor).Set(float64(*res.CertDaysLeft))
	}
}

// AddGauge 는 오르내리는 값을 그때그때 읽어 가는 게이지를 등록한다.
// fn 은 스크레이프 goroutine 에서 불리므로 원자적으로 읽을 수 있는 값만 넘겨야 한다.
func (m *Metrics) AddGauge(name, help string, fn func() float64) {
	m.reg.MustRegister(prometheus.NewGaugeFunc(
		prometheus.GaugeOpts{Name: name, Help: help}, fn,
	))
}

// AddCounter 는 줄어들지 않는 누적값을 등록한다. rate() 는 카운터에만 쓸 수 있다.
func (m *Metrics) AddCounter(name, help string, fn func() float64) {
	m.reg.MustRegister(prometheus.NewCounterFunc(
		prometheus.CounterOpts{Name: name, Help: help}, fn,
	))
}
