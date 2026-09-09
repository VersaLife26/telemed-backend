package audit

import (
	"context"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/rs/zerolog"

	adminmw "telemed/internal/platform/middleware"
)

// Middleware writes an audit_logs row for every state-changing request that
// completes successfully, without requiring each handler to remember to call
// anything. Mount it once, above every admin route:
//
//	r.Use(audit.Middleware(auditService, log))
//
// GET/HEAD/OPTIONS pass through untouched -- they cannot change state, and
// auditing every dashboard read would flood the table with noise that
// dilutes the entries that matter. Every other method that completes with a
// 2xx or 3xx status is audited using whatever the handler staged via
// audit.Stage. If nothing was staged, this middleware writes a fallback
// entry itself (action "unaudited.<method>.<route>") and logs a warning: a
// developer forgetting to stage a draft results in a loud, greppable gap in
// the audit trail, not a silent one.
func Middleware(svc *Service, log zerolog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// The box is installed for EVERY method, including reads.
			//
			// Reads still produce no fallback entry -- auditing every
			// dashboard GET would flood the table and dilute the entries that
			// matter -- but a read that deliberately stages a draft is
			// recorded. Some reads are the act: presigning a doctor's
			// credential documents, or opening a patient's own account of
			// their care on a dispute. Skipping GETs outright meant those
			// could not be audited at all without a second mechanism.
			ctx, _ := withBox(r.Context())
			r = r.WithContext(ctx)

			ww := chimw.NewWrapResponseWriter(w, r.ProtoMajor)
			next.ServeHTTP(ww, r)

			if ww.Status() >= 400 {
				// Failed requests (validation errors, 403s, 404s) changed
				// nothing and are already visible in the ordinary HTTP
				// access log (middleware.RequestLogger). Reserving
				// audit_logs for confirmed state changes keeps it the
				// authoritative "what happened" record rather than a
				// second, noisier copy of the access log.
				return
			}

			// The audit write must outlive the request that caused it.
			// r.Context() is cancelled the instant the client disconnects, so
			// a browser tab closed right after a successful PUT would cancel
			// the INSERT: the state change lands and nothing records who made
			// it. WithoutCancel keeps every value in the context -- request
			// id, logger, tracing span -- and drops only the cancellation.
			//nolint:contextcheck // context.WithoutCancel derives from r.Context(); contextcheck does not recognise it as a context.WithXXX constructor and detaching the cancellation is the point of the call.
			record(context.WithoutCancel(r.Context()), svc, log, r)
		})
	}
}

// record persists whatever the handler staged, under a context that is
// deliberately not the request's -- see the call site. It is a named function
// rather than an inline block so that context is a parameter, which is what
// makes "this call must not inherit the request's cancellation" explicit
// rather than a comment.
func record(ctx context.Context, svc *Service, log zerolog.Logger, r *http.Request) {
	principal, _ := adminmw.PrincipalFrom(ctx)
	reqID := chimw.GetReqID(ctx)
	pattern := routePattern(r)
	ip := adminmw.ClientIP(r)
	ua := r.UserAgent()

	drafts, _ := drain(ctx)

	if len(drafts) == 0 {
		if isRead(r.Method) {
			// An ordinary dashboard read. Nothing to record.
			return
		}
		drafts = []Draft{{
			Action:       "unaudited." + strings.ToLower(r.Method) + "." + normalizeRoute(pattern),
			ResourceType: pattern,
		}}
		log.Warn().Str("route", pattern).Str("method", r.Method).
			Msg("state-changing admin route completed without staging an audit entry; wrote fallback record")
	}

	for _, d := range drafts {
		if _, err := svc.Record(ctx, principal, d, ip, ua, reqID); err != nil {
			log.Error().Err(err).Str("action", d.Action).Msg("failed to write audit log entry")
		}
	}
}

// isRead reports whether a method cannot change state, and so gets no
// fallback audit entry when a handler stages nothing.
func isRead(method string) bool {
	return method == http.MethodGet || method == http.MethodHead || method == http.MethodOptions
}

func routePattern(r *http.Request) string {
	if rctx := chi.RouteContext(r.Context()); rctx != nil && rctx.RoutePattern() != "" {
		return rctx.RoutePattern()
	}
	return r.URL.Path
}

func normalizeRoute(pattern string) string {
	pattern = strings.Trim(pattern, "/")
	pattern = strings.ReplaceAll(pattern, "/", ".")
	pattern = strings.ReplaceAll(pattern, "{", "")
	pattern = strings.ReplaceAll(pattern, "}", "")
	return pattern
}
