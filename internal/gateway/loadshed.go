package gateway

import (
	"math"
	"net/http"
	"sync/atomic"

	"github.com/go-chi/chi/v5"

	"telemed/internal/platform/httpx"
)

// LoadShed caps the number of requests concurrently being proxied. Above
// maxInFlight, a request is rejected immediately with 503 rather than accepted and
// left to queue behind a struggling upstream until its own timeout fires --
// the difference between one slow dependency and every dependency looking
// slow because the gateway itself is the bottleneck.
//
// maxInFlight is an int because that is what MAX_IN_FLIGHT parses to, and it is
// CLAMPED rather than converted. The counter is an int32 -- one atomic word
// is the point of this middleware -- and a plain int32(maxInFlight) truncates to the
// low 32 bits. MAX_IN_FLIGHT=4294967297 would then become a cap of 1 and the
// gateway would 503 its own traffic from the second concurrent request
// onwards, while MAX_IN_FLIGHT=2147483648 would become negative and disable
// shedding altogether. Both are silent: the operator typed a large number and
// got either a self-inflicted outage or no overload protection at all, with
// nothing in the logs either way. Config.Validate refuses those values at
// boot; this clamp is the second line, so the middleware is safe to call with
// any int whatever the caller did. (Named maxInFlight, not max: max is a
// predeclared identifier in Go 1.21+ and shadowing it in a file that also
// does bounds arithmetic is the sort of thing that reads fine and compiles
// into something else.)
func LoadShed(maxInFlight int, gm *GatewayMetrics) func(http.Handler) http.Handler {
	limit := int32(math.MaxInt32)
	if maxInFlight < math.MaxInt32 {
		limit = int32(maxInFlight) //nolint:gosec // G115: guarded by the comparison above; max < MaxInt32 fits int32 by construction, and negative values keep their documented "shedding disabled" meaning.
	}
	var inFlight int32
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n := atomic.AddInt32(&inFlight, 1)
			defer atomic.AddInt32(&inFlight, -1)
			if gm != nil {
				gm.InFlight.Set(float64(n))
			}

			if limit > 0 && n > limit {
				if gm != nil {
					gm.ShedTotal.WithLabelValues(shedRouteLabel(r)).Inc()
				}
				w.Header().Set("Retry-After", "1")
				httpx.Error(w, r, httpx.NewError(http.StatusServiceUnavailable,
					httpx.CodeUnavailable, "gateway is at capacity, try again shortly"))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// shedRouteLabel returns a BOUNDED label for the load-shed counter: the chi
// route pattern ("/api/v1/records/{recordID}"), never the raw path.
//
// This used to be r.URL.Path, which is two problems in one line. The label
// values then contained real resource identifiers -- prescription, document
// and consultation UUIDs -- and /metrics is served unauthenticated on the same
// listener the public ingress routes to, so those identifiers were readable by
// anyone. And because the path is attacker-chosen, an unauthenticated caller
// who could push the gateway past its in-flight cap could then plant unbounded
// distinct label values, which is how a Prometheus client's local map becomes a
// memory-exhaustion DoS.
//
// The pattern is populated by the time this runs: chi matches the route and
// fills RouteContext before invoking the handler this middleware wraps. The
// constant fallback keeps cardinality bounded even if that ever stops being
// true.
func shedRouteLabel(r *http.Request) string {
	if rctx := chi.RouteContext(r.Context()); rctx != nil {
		if p := rctx.RoutePattern(); p != "" {
			return p
		}
	}
	return "unmatched"
}
