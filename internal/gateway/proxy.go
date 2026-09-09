package gateway

import (
	"context"
	"errors"
	"net/http"
	"net/http/httputil"
	"time"

	"telemed/internal/platform/httpx"
	"telemed/internal/platform/observability"
)

// errUpstreamStatus is a sentinel passed to Metrics.ObserveDependency so a
// 5xx response (which never produces a Go error from RoundTrip) still counts
// as an error in the per-upstream error-rate metric, not just a slow success.
var errUpstreamStatus = errors.New("upstream returned 5xx")

type proxyTimingKey struct{}

type proxyTiming struct {
	state BreakerState
	start time.Time
}

func withProxyTiming(ctx context.Context, state BreakerState, start time.Time) context.Context {
	return context.WithValue(ctx, proxyTimingKey{}, proxyTiming{state: state, start: start})
}

func proxyTimingFrom(ctx context.Context) proxyTiming {
	if t, ok := ctx.Value(proxyTimingKey{}).(proxyTiming); ok {
		return t
	}
	return proxyTiming{state: StateClosed, start: time.Now()}
}

// newReverseProxy builds a path-preserving reverse proxy for one upstream.
// Backend services own the same /api/v1/... path space the gateway does, so
// forwarding only ever rewrites scheme and host, never the path.
//
// Every response outcome (transport error, 5xx, or success) feeds back into
// the upstream's circuit breaker, which is what lets Allow/Report drive real
// state transitions rather than a breaker nothing ever reports to.
func newReverseProxy(up *Upstream, metrics *observability.Metrics) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Transport: up.Client.Transport,

		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(up.BaseURL)
			pr.SetXForwarded()
			injectTrustedHeaders(pr.Out, pr.In)
			stripHopByHopHeaders(pr.Out.Header)
		},

		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			var maxErr *http.MaxBytesError
			if errors.As(err, &maxErr) {
				// The client's fault, not the upstream's: do not trip the
				// breaker over a request nobody downstream ever saw in full.
				httpx.Error(w, r, httpx.NewError(http.StatusRequestEntityTooLarge,
					httpx.CodeBadRequest, "request body exceeds the limit for this route"))
				return
			}

			t := proxyTimingFrom(r.Context())
			up.Breaker.Report(context.WithoutCancel(r.Context()), t.state, false)
			if metrics != nil {
				// DependencyUp is deliberately NOT written here: its documented
				// contract ("1 when a downstream answered its last health
				// probe") belongs to the periodic /health/ready check in
				// main.go. Writing it per-request too, under a differently
				// shaped label ("doctor-service" vs the health check's
				// "upstream:doctor-service"), created two divergent series for
				// the same upstream -- caught by hand while smoke-testing a
				// live boot, not by any unit test. Real-time per-request
				// upstream health is GatewayMetrics.CircuitState instead,
				// driven by Breaker.OnState on every Allow/Report.
				metrics.ObserveDependency(up.Name, r.Method, t.start, err)
			}
			httpx.Error(w, r, httpx.NewError(http.StatusServiceUnavailable,
				httpx.CodeUnavailable, up.Name+" is temporarily unavailable").WithCause(err))
		},

		ModifyResponse: func(resp *http.Response) error {
			t := proxyTimingFrom(resp.Request.Context())
			success := resp.StatusCode < http.StatusInternalServerError
			up.Breaker.Report(context.WithoutCancel(resp.Request.Context()), t.state, success)
			if metrics != nil {
				var obsErr error
				if !success {
					obsErr = errUpstreamStatus
				}
				metrics.ObserveDependency(up.Name, resp.Request.Method, t.start, obsErr)
			}
			return nil
		},
	}
}
