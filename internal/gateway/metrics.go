package gateway

import (
	"github.com/prometheus/client_golang/prometheus"

	"telemed/internal/platform/observability"
)

// circuitStateValue encodes BreakerState as a Prometheus gauge value:
// closed=0, half_open=1, open=2. Alerting rules key off ==2, not off a label
// string, so this ordering is part of the metric's contract once shipped.
func circuitStateValue(s BreakerState) float64 {
	switch s {
	case StateOpen:
		return 2
	case StateHalfOpen:
		return 1
	default:
		return 0
	}
}

// GatewayMetrics extends the platform's shared Metrics with the counters and
// gauges specific to a reverse-proxying gateway: retries and load-shedding
// have no equivalent in a domain service.
type GatewayMetrics struct {
	CircuitState  *prometheus.GaugeVec
	RetriesTotal  *prometheus.CounterVec
	ShedTotal     *prometheus.CounterVec
	InFlight      prometheus.Gauge
	RouteNotFound prometheus.Counter
}

// NewGatewayMetrics registers the gateway's extra instruments on the same
// private registry every telemed service already uses, so one /metrics
// endpoint carries both the generic HTTP metrics and these.
func NewGatewayMetrics(m *observability.Metrics) *GatewayMetrics {
	gm := &GatewayMetrics{
		CircuitState: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "telemed_gateway_circuit_state",
			Help: "Circuit breaker state per upstream: 0=closed 1=half_open 2=open.",
		}, []string{"upstream"}),
		RetriesTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "telemed_gateway_retries_total",
			Help: "Idempotent requests retried after a connection-level error, by upstream.",
		}, []string{"upstream"}),
		ShedTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "telemed_gateway_load_shed_total",
			Help: "Requests rejected with 503 because the in-flight cap was exceeded.",
		}, []string{"route"}),
		InFlight: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "telemed_gateway_proxy_in_flight",
			Help: "Requests currently being proxied to an upstream.",
		}),
		RouteNotFound: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "telemed_gateway_route_not_found_total",
			Help: "Requests under /api/v1 that matched no route table entry.",
		}),
	}
	m.Registry().MustRegister(gm.CircuitState, gm.RetriesTotal, gm.ShedTotal, gm.InFlight, gm.RouteNotFound)
	return gm
}

// CircuitStateSet records the current breaker state for one upstream. Safe
// to call from a Breaker.OnState callback on every Allow/Report -- setting a
// gauge is cheap and does not touch Redis.
func (gm *GatewayMetrics) CircuitStateSet(upstream string, state BreakerState) {
	gm.CircuitState.WithLabelValues(upstream).Set(circuitStateValue(state))
}
