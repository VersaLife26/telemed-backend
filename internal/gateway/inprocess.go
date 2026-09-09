package gateway

import (
	"context"
	"net/http"

	"telemed/internal/platform/httpx"
	"telemed/internal/platform/observability"
)

// NewInProcessUpstream describes a domain that lives in this process.
//
// The route table does not change and neither does anything composed around a
// route -- the auth class, the rate limit, the timeout, the body cap, the
// breaker. What changes is the last inch: instead of a reverse proxy opening a
// connection to http://user-service:8081, the request is handed to that
// domain's own http.Handler.
//
// Client and BaseURL stay nil. Nothing reads them on this path, and leaving
// them nil means a future edit that reintroduces a dial fails loudly at the
// nil pointer rather than quietly sending mesh traffic to the zero URL.
func NewInProcessUpstream(name string, handler http.Handler, breaker *Breaker, maxInFlight int) *Upstream {
	return &Upstream{
		Name:    name,
		Breaker: breaker,
		Handler: handler,
		sem:     newSemaphore(maxInFlight),
	}
}

// newSemaphore returns nil for a non-positive size, meaning "unbounded".
func newSemaphore(n int) chan struct{} {
	if n <= 0 {
		return nil
	}
	return make(chan struct{}, n)
}

// newInProcessProxy is the in-process counterpart of newReverseProxy: it runs
// the domain's handler and reports the same outcome to the same breaker and
// the same dependency metric, so an operator's dashboards do not change shape
// when a domain moves in-process.
//
// WHY THERE IS A PER-DOMAIN LIMIT HERE
// Nine processes each had their own goroutine budget, their own load-shed
// threshold and their own memory limit, so a payment stampede degraded
// payments. One process shares one budget, and without this a flood of slow
// payment calls would starve OTP verification -- a domain that was previously
// unaffected. The gateway's global LoadShed still caps total in-flight work;
// this caps one domain's share of it, which is the property the split
// processes used to provide for free.
//
// A refusal here is the same 503 the breaker returns, for the same reason
// (this domain cannot take more work right now), so no caller learns a new
// failure mode.
func newInProcessProxy(up *Upstream, metrics *observability.Metrics) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if up.sem != nil {
			select {
			case up.sem <- struct{}{}:
				defer func() { <-up.sem }()
			default:
				httpx.Error(w, r, httpx.NewError(http.StatusServiceUnavailable,
					httpx.CodeUnavailable, up.Name+" is at capacity"))
				return
			}
		}

		t := proxyTimingFrom(r.Context())
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		up.Handler.ServeHTTP(rec, r)

		// Same rule as ModifyResponse on the proxying path: 5xx is a failure,
		// everything else is a success. A panic never reaches here -- the
		// server's Recoverer has already turned it into a 500 -- so it is
		// counted through the recorded status, exactly as a 500 from an
		// upstream was.
		success := rec.status < http.StatusInternalServerError
		up.Breaker.Report(context.WithoutCancel(r.Context()), t.state, success)
		if metrics != nil {
			var obsErr error
			if !success {
				obsErr = errUpstreamStatus
			}
			metrics.ObserveDependency(up.Name, r.Method, t.start, obsErr)
		}
	})
}

// statusRecorder remembers the status code so the breaker can be told about it.
type statusRecorder struct {
	http.ResponseWriter
	status  int
	written bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if !s.written {
		s.status = code
		s.written = true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	s.written = true
	return s.ResponseWriter.Write(b)
}

// Flush forwards to the wrapped writer when it supports flushing.
//
// Load-bearing rather than tidy: the consultation surface streams, and a
// wrapper that swallows Flush turns a stream into a response that arrives all
// at once at the end.
func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

var _ http.Flusher = (*statusRecorder)(nil)
