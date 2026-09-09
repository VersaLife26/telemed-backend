// Package observability wires Prometheus metrics and OpenTelemetry tracing.
//
// The metric names here are the platform's operational contract: dashboards,
// alerts, and runbooks all reference them. Renaming one silently breaks every
// alert that mentions it, so treat these strings as an API.
package observability

import (
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics bundles the standard instrument set every service exposes.
type Metrics struct {
	registry *prometheus.Registry

	HTTPRequests *prometheus.CounterVec
	HTTPDuration *prometheus.HistogramVec
	HTTPInFlight prometheus.Gauge

	// Business counters. Services register their own on top of these via
	// Registry(); these three exist everywhere because every runbook uses them.
	EventsPublished *prometheus.CounterVec
	EventsConsumed  *prometheus.CounterVec
	OutboxBacklog   prometheus.Gauge

	DependencyUp       *prometheus.GaugeVec
	DependencyDuration *prometheus.HistogramVec
}

// NewMetrics builds a private registry (never the global default, which would
// leak metrics between tests) preloaded with Go runtime and process collectors.
func NewMetrics(service string) *Metrics {
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	labels := prometheus.Labels{"service": service}

	m := &Metrics{
		registry: reg,
		HTTPRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "telemed_http_requests_total", ConstLabels: labels,
			Help: "Total HTTP requests by route, method and status class.",
		}, []string{"method", "route", "status"}),
		HTTPDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "telemed_http_request_duration_seconds", ConstLabels: labels,
			Help: "HTTP request latency. Buckets are tuned for the p95<500ms SLO.",
			// Buckets straddle the 500ms SLO so the alert query needs no
			// interpolation across a wide bucket.
			Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 0.75, 1, 2.5, 5, 10},
		}, []string{"method", "route"}),
		HTTPInFlight: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "telemed_http_in_flight_requests", ConstLabels: labels,
			Help: "HTTP requests currently being served.",
		}),
		EventsPublished: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "telemed_events_published_total", ConstLabels: labels,
			Help: "Domain events relayed from the outbox to NATS.",
		}, []string{"subject", "result"}),
		EventsConsumed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "telemed_events_consumed_total", ConstLabels: labels,
			Help: "Domain events processed by this service's subscribers.",
		}, []string{"subject", "result"}),
		OutboxBacklog: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "telemed_outbox_backlog", ConstLabels: labels,
			Help: "Unpublished rows in outbox_events. Sustained growth means the relay is stuck.",
		}),
		DependencyUp: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "telemed_dependency_up", ConstLabels: labels,
			Help: "1 when a downstream dependency answered its last health probe.",
		}, []string{"dependency"}),
		DependencyDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "telemed_dependency_duration_seconds", ConstLabels: labels,
			Help:    "Latency of calls to downstream dependencies.",
			Buckets: prometheus.DefBuckets,
		}, []string{"dependency", "operation", "result"}),
	}

	reg.MustRegister(
		m.HTTPRequests, m.HTTPDuration, m.HTTPInFlight,
		m.EventsPublished, m.EventsConsumed, m.OutboxBacklog,
		m.DependencyUp, m.DependencyDuration,
	)
	return m
}

// Registry exposes the registry so a service can register domain metrics.
func (m *Metrics) Registry() *prometheus.Registry { return m.registry }

// Handler serves the Prometheus scrape endpoint.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{
		EnableOpenMetrics: true,
		// A scrape must never take a service down.
		Timeout: 10 * time.Second,
	})
}

// Middleware records request count, latency and concurrency. Route labels are
// the chi pattern or the constant "unmatched" -- see routeLabel -- so
// cardinality stays bounded no matter how many UUIDs the clients send.
func (m *Metrics) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/metrics" {
			next.ServeHTTP(w, r)
			return
		}
		m.HTTPInFlight.Inc()
		defer m.HTTPInFlight.Dec()

		start := time.Now()
		ww := chimw.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)

		route := routeLabel(r)
		m.HTTPRequests.WithLabelValues(r.Method, route, strconv.Itoa(ww.Status())).Inc()
		m.HTTPDuration.WithLabelValues(r.Method, route).Observe(time.Since(start).Seconds())
	})
}

// routeLabel is the only thing that may become a Prometheus "route" label
// value.
//
// It returns chi's matched pattern, or the constant "unmatched" -- never
// anything derived from the URL. chi only fills the pattern in during routing,
// so every request that matched no route has an empty pattern, and the
// previous fallback to r.URL.Path meant an anonymous caller chose the label
// value. That is two separate problems on an endpoint that is served
// unauthenticated on every service listener:
//
//   - Disclosure. On this platform an unmatched path is routinely
//     /api/v1/records/<document-uuid> or
//     /api/v1/verify/prescriptions/<prescription-uuid>. Prometheus keeps the
//     child series forever and /metrics hands it to anyone who asks.
//   - Exhaustion. Each distinct label value allocates two permanent child
//     metrics that are never evicted, so a few thousand requests to
//     /a1, /a2, /a3 ... is a memory-exhaustion DoS with no credential.
func routeLabel(r *http.Request) string {
	if rctx := chi.RouteContext(r.Context()); rctx != nil {
		if pattern := rctx.RoutePattern(); pattern != "" {
			return pattern
		}
	}
	return "unmatched"
}

// ObserveDependency times a downstream call and records success or failure.
func (m *Metrics) ObserveDependency(dependency, operation string, start time.Time, err error) {
	result := "ok"
	if err != nil {
		result = "error"
	}
	m.DependencyDuration.WithLabelValues(dependency, operation, result).Observe(time.Since(start).Seconds())
}
