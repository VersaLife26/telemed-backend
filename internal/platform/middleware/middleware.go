package middleware

import (
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/rs/zerolog"

	"telemed/internal/platform/cache"
	"telemed/internal/platform/httpx"
	"telemed/internal/platform/logger"
)

// RequestLogger logs one structured line per request. It deliberately never
// logs the request body or query values that may carry PHI -- only method,
// route pattern, status, duration, and the caller's masked identity.
func RequestLogger(log zerolog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Health and metrics scrapes would otherwise drown the log.
			if r.URL.Path == "/health" || r.URL.Path == "/health/live" ||
				r.URL.Path == "/health/ready" || r.URL.Path == "/metrics" {
				next.ServeHTTP(w, r)
				return
			}

			start := time.Now()
			ww := chimw.NewWrapResponseWriter(w, r.ProtoMajor)
			reqID := chimw.GetReqID(r.Context())

			reqLog := log.With().Str("request_id", reqID).Logger()
			r = r.WithContext(logger.WithContext(r.Context(), reqLog))

			next.ServeHTTP(ww, r)

			// The route pattern, not the raw path: /doctors/{id} rather than
			// /doctors/9f8e... keeps identifiers out of the log and makes the
			// lines aggregatable. chi fills the pattern in during routing, so
			// it is only meaningful after next.ServeHTTP has returned.
			//
			// A request that matched no route has no pattern, and the fallback
			// used to be the raw path -- which on a 404 against
			// /api/v1/records/<uuid> is exactly the identifier this comment
			// promises to keep out. logger.SafePath keeps the shape and drops
			// the values.
			pattern := logger.SafePath(r.URL.Path)
			// contextcheck flags r.Context() here, but this is a read of the
			// request context AFTER the handler ran, to recover what chi
			// matched -- not the creation of a detached context.
			//nolint:contextcheck // reading the completed request's context, by design.
			if rctx := chi.RouteContext(r.Context()); rctx != nil && rctx.RoutePattern() != "" {
				pattern = rctx.RoutePattern()
			}

			ev := reqLog.Info()
			if ww.Status() >= 500 {
				ev = reqLog.Error()
			} else if ww.Status() >= 400 {
				ev = reqLog.Warn()
			}

			//nolint:contextcheck // same as above: reading, not deriving, a context.
			if p, ok := PrincipalFrom(r.Context()); ok {
				ev = ev.Str("user_id", logger.MaskID(p.UserID.String()))
				if len(p.Roles) > 0 {
					ev = ev.Str("role", string(p.Roles[0]))
				}
			}

			ev.Str("method", r.Method).
				Str("route", pattern).
				Int("status", ww.Status()).
				Int("bytes", ww.BytesWritten()).
				Dur("duration", time.Since(start)).
				Str("ip", clientIP(r)).
				Msg("http request")
		})
	}
}

// SecurityHeaders applies the baseline header set. These are cheap and close
// entire vulnerability classes, so they are unconditional.
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		h.Set("Permissions-Policy", "geolocation=(), microphone=(self), camera=(self)")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		// The API serves JSON only; a restrictive CSP costs nothing here and
		// neutralises any content-sniffing based XSS on error pages.
		h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		if requestIsHTTPS(r) {
			h.Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains; preload")
		}
		next.ServeHTTP(w, r)
	})
}

// requestIsHTTPS reports whether the ORIGINAL client request was over TLS.
//
// r.TLS alone is wrong everywhere this platform actually runs. Every service
// sits behind ingress (or the api-gateway) which terminates TLS and speaks
// plaintext HTTP to the pod, so r.TLS is nil on every request and the HSTS
// header above was never once emitted in production -- an app-level control
// that looked present in code review and did nothing. It was masked by
// ingress-nginx's default hsts:true, which is someone else's default and not
// a control this platform owns.
//
// Trusting the X-Forwarded-Proto header is safe HERE specifically, and the
// reasoning matters because trusting forwarded headers is usually a mistake:
// RFC 6797 s7.2 requires a user agent to IGNORE Strict-Transport-Security
// received over non-secure transport. So the worst a client can achieve by
// forging the header on a plaintext connection is to make us emit a header
// its own browser must discard. There is no downgrade and nothing to poison.
// Contrast X-Forwarded-For, which decides rate-limit buckets and audit rows
// and is therefore parsed only through the trusted-proxy logic in clientip.go.
func requestIsHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	// Only the first value: a proxy chain appends, and the left-most entry is
	// the scheme the client actually used.
	proto := r.Header.Get("X-Forwarded-Proto")
	if i := strings.IndexByte(proto, ','); i >= 0 {
		proto = proto[:i]
	}
	return strings.EqualFold(strings.TrimSpace(proto), "https")
}

// NoStore marks responses as uncacheable. Apply to every route that returns
// clinical or financial data so no proxy or browser retains a copy.
func NoStore(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, private")
		w.Header().Set("Pragma", "no-cache")
		next.ServeHTTP(w, r)
	})
}

// RateLimitConfig describes one limit bucket.
type RateLimitConfig struct {
	// Requests allowed per Window.
	Requests int
	Window   time.Duration
	// KeyFunc derives the bucket key. Defaults to the client IP.
	KeyFunc func(*http.Request) string
	// Name distinguishes buckets in Redis, e.g. "otp" vs "search".
	Name string
	// FailClosed decides what happens when Redis cannot answer.
	//
	// The default (false) passes the request through: for AUTHENTICATED
	// traffic that is right, because a Redis blip must not take down every
	// consultation on the platform to enforce a throttle.
	//
	// It is the WRONG default for an unauthenticated, DB-writing endpoint --
	// /webhooks/* is 256 KiB per request, anonymous, and the limiter is the
	// only throttle in front of it, so a Redis blip removed the control
	// entirely (SECURITY-REVIEW F24). Setting this returns 503 instead, which
	// is also the answer every payment provider already understands: they all
	// retry on 503 by design, so shedding here costs a delayed callback rather
	// than a lost one.
	FailClosed bool
}

// RateLimit enforces a fixed-window limit in Redis so the budget is shared
// across every replica. A per-process limiter would let an attacker multiply
// their allowance by the number of pods.
func RateLimit(c cache.Cache, cfg RateLimitConfig, log zerolog.Logger) func(http.Handler) http.Handler {
	if cfg.Window <= 0 {
		cfg.Window = time.Minute
	}
	if cfg.Requests <= 0 {
		cfg.Requests = 60
	}
	if cfg.Name == "" {
		cfg.Name = "default"
	}
	if cfg.KeyFunc == nil {
		cfg.KeyFunc = clientIP
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := fmt.Sprintf("ratelimit:%s:%s", cfg.Name, cfg.KeyFunc(r))
			n, err := c.Incr(r.Context(), key, cfg.Window)
			if err != nil {
				if cfg.FailClosed {
					// See RateLimitConfig.FailClosed. Shed rather than serve
					// an unthrottled unauthenticated write.
					log.Error().Err(err).Str("bucket", cfg.Name).
						Msg("rate limiter unavailable, failing closed")
					httpx.Error(w, r, httpx.ErrUnavailable)
					return
				}
				// Failing open is the deliberate choice for authenticated
				// traffic: a Redis blip must not take down consultations. The
				// error is logged so the operator still learns the limiter is
				// degraded.
				log.Error().Err(err).Str("bucket", cfg.Name).Msg("rate limiter unavailable, failing open")
				next.ServeHTTP(w, r)
				return
			}

			remaining := max(cfg.Requests-int(n), 0)
			w.Header().Set("X-RateLimit-Limit", strconv.Itoa(cfg.Requests))
			w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(remaining))
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(cfg.Window).Unix(), 10))

			if int(n) > cfg.Requests {
				w.Header().Set("Retry-After", strconv.Itoa(int(cfg.Window.Seconds())))
				httpx.Error(w, r, httpx.ErrRateLimited)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// ByPrincipal keys a rate limit on the authenticated user, falling back to IP
// for anonymous callers.
func ByPrincipal(r *http.Request) string {
	if p, ok := PrincipalFrom(r.Context()); ok && p.UserID != (Principal{}).UserID {
		return "user:" + p.UserID.String()
	}
	return "ip:" + clientIP(r)
}

// IPAllowlist restricts a route tree to a set of CIDRs. The admin surface runs
// behind this in addition to Cloudflare, because defence that lives only at the
// edge disappears the moment someone reaches the origin directly.
func IPAllowlist(cidrs []string, log zerolog.Logger) func(http.Handler) http.Handler {
	nets := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		if !strings.Contains(c, "/") {
			if strings.Contains(c, ":") {
				c += "/128"
			} else {
				c += "/32"
			}
		}
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			log.Error().Err(err).Str("cidr", c).Msg("ignoring malformed allowlist entry")
			continue
		}
		nets = append(nets, n)
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// An empty allowlist means "not configured", and this middleware
			// then serves the request. In dev that is normal.
			//
			// IT IS NOT A CONTROL ON ITS OWN. Nothing here can distinguish
			// "deliberately open" from "the environment variable was never
			// set", so a service that must not be open when unconfigured has
			// to assert that at BOOT, in its own Config.Validate, and refuse
			// to start. telemed-api-gateway and telemed-admin-service both do
			// (security review F17); a service that mounts IPAllowlist on a
			// sensitive surface and does not is relying on a control that is
			// not there.
			//
			// A previous version of this comment claimed "the readiness check
			// asserts that separately". No such check existed anywhere in the
			// platform, which is exactly how an unset variable turned a
			// documented control into decoration for months.
			//
			// Note also that a MALFORMED entry is logged and skipped above, so
			// a list of two typo'd CIDRs parses to an empty list and lands
			// here -- fail-open reached from a config that looks configured.
			// The boot-time check is the place to catch that too.
			if len(nets) == 0 {
				next.ServeHTTP(w, r)
				return
			}
			ip := net.ParseIP(clientIP(r))
			for _, n := range nets {
				if ip != nil && n.Contains(ip) {
					next.ServeHTTP(w, r)
					return
				}
			}
			log.Warn().Str("ip", clientIP(r)).Str("path", r.URL.Path).Msg("blocked by IP allowlist")
			httpx.Error(w, r, httpx.ErrForbidden)
		})
	}
}

// clientIP returns the address resolved by the RealIP middleware. It never
// parses forwarding headers itself -- see clientip.go for why reading
// X-Forwarded-For without knowing the peer is a spoofing hole.
func clientIP(r *http.Request) string { return ClientIPFrom(r) }

// ClientIP is the exported form, for services that need the caller address in
// a handler or an audit record.
func ClientIP(r *http.Request) string { return ClientIPFrom(r) }
