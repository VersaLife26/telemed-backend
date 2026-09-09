package gateway

import (
	"context"
	"net/http"
	"time"

	"telemed/internal/platform/httpx"
)

// timeoutFor resolves a route's timeout class to a concrete duration. A
// single global timeout is wrong for a gateway that serves a JSON search
// endpoint and a 40MB prescription PDF through the same process.
func timeoutFor(class TimeoutClass) time.Duration {
	switch class {
	case TimeoutFast:
		return 5 * time.Second
	case TimeoutWrite:
		return 15 * time.Second
	case TimeoutUpload:
		return 60 * time.Second
	default:
		return 15 * time.Second
	}
}

func bodyLimitFor(class TimeoutClass, cfg Config) int64 {
	switch class {
	case TimeoutFast:
		return cfg.MaxBodyBytesFast
	case TimeoutUpload:
		return cfg.MaxBodyBytesUpload
	case TimeoutWrite:
		return cfg.MaxBodyBytesWrite
	default:
		return cfg.MaxBodyBytesWrite
	}
}

// routeProxyHandler builds the innermost handler for one route: enforce the
// body size cap, bound the whole round trip (including retries) by the
// route's timeout class, consult the upstream's circuit breaker, and only
// then hand off.
//
// proxy is an http.Handler rather than a *httputil.ReverseProxy because the
// same route table now serves both shapes: a reverse proxy when the domain is
// another process, and the domain's own handler when it is in this one. Every
// control above this line is identical either way, which is the point.
func routeProxyHandler(rule RouteRule, up *Upstream, proxy http.Handler, cfg Config) http.Handler {
	timeout := timeoutFor(rule.Timeout)
	limit := rule.BodyLimitBytes
	if limit <= 0 {
		limit = bodyLimitFor(rule.Timeout, cfg)
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Reject an oversized upload before it ever reaches the mesh: if the
		// client declared Content-Length, check it up front rather than
		// waiting for MaxBytesReader to trip mid-copy after a connection to
		// the upstream may already be open.
		if limit > 0 && r.ContentLength > limit {
			httpx.Error(w, r, httpx.NewError(http.StatusRequestEntityTooLarge,
				httpx.CodeBadRequest, "request body exceeds the limit for this route"))
			return
		}
		if limit > 0 {
			r.Body = http.MaxBytesReader(w, r.Body, limit)
		}

		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()

		allowed, state := up.Breaker.Allow(ctx)
		if !allowed {
			httpx.Error(w, r, httpx.NewError(http.StatusServiceUnavailable,
				httpx.CodeUnavailable, up.Name+" is temporarily unavailable (circuit open)"))
			return
		}

		ctx = withProxyTiming(ctx, state, time.Now())
		proxy.ServeHTTP(w, r.WithContext(ctx))
	})
}
