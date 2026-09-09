package gateway

import (
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog"

	"telemed/internal/platform/cache"
	"telemed/internal/platform/httpx"
	platmw "telemed/internal/platform/middleware"
	"telemed/internal/platform/observability"
)

// Deps is everything Mount needs to wire the route table onto a router.
type Deps struct {
	Config        Config
	Cache         cache.Cache
	Authenticator *platmw.Authenticator
	Metrics       *observability.Metrics
	GatewayMetric *GatewayMetrics
	Logger        zerolog.Logger
	Upstreams     map[string]*Upstream
	Routes        []RouteRule
}

// Mount registers every route in Deps.Routes onto r, plus a JSON 404 for
// anything under /api/v1 that matches no rule. Each rule is compiled into a
// single, fully-composed http.Handler; chi's own radix-tree router -- not a
// hand-rolled matcher -- resolves which one a request hits, including the
// static-beats-param-beats-wildcard precedence that makes
// "/doctors/{id}/slots" (scheduling-service) win over "/doctors/*"
// (doctor-service) for that one path shape while everything else falls
// through to the wildcard.
func Mount(r chi.Router, d Deps) error {
	// One dispatcher per upstream: a reverse proxy for a service across the
	// network, the domain's own handler when it is in this process.
	proxies := make(map[string]http.Handler, len(d.Upstreams))
	for name, up := range d.Upstreams {
		if up.Handler != nil {
			proxies[name] = newInProcessProxy(up, d.Metrics)
			continue
		}
		proxies[name] = newReverseProxy(up, d.Metrics)
	}

	// One shared in-flight counter for the whole gateway, not one per route:
	// the cap is on total concurrent proxied work, matching "an operator
	// should see the gateway shed load," not "route X shed load while route
	// Y had capacity to spare."
	shed := LoadShed(d.Config.MaxInFlight, d.GatewayMetric)

	// rangeValCopy is suppressed rather than "fixed" by indexing: this loop
	// runs once per route at process boot (64 rules, ~12 KB copied in total,
	// once), so the diagnostic describes no real cost here. The copy is also
	// load-bearing -- routeProxyHandler and composeMiddleware read the rule
	// while composing per-route handlers, and a value keeps a rule that ends
	// up captured by a closure from aliasing a later mutation of d.Routes.
	// Indexing would silence the linter and lose that.
	for _, rule := range d.Routes { //nolint:gocritic // rangeValCopy: boot-time, once per route; the copy is deliberate (see above).
		up, ok := d.Upstreams[rule.Upstream]
		if !ok {
			return fmt.Errorf("gateway: route %q references unknown upstream %q", rule.Name, rule.Upstream)
		}
		proxy := proxies[rule.Upstream]

		handler := routeProxyHandler(rule, up, proxy, d.Config)
		handler = composeMiddleware(rule, d, handler)
		handler = shed(handler)

		if len(rule.Methods) == 0 {
			r.Handle(rule.Pattern, handler)
			continue
		}
		for _, m := range rule.Methods {
			r.Method(m, rule.Pattern, handler)
		}
	}

	r.NotFound(func(w http.ResponseWriter, req *http.Request) {
		if d.GatewayMetric != nil {
			d.GatewayMetric.RouteNotFound.Inc()
		}
		httpx.Error(w, req, httpx.ErrNotFound)
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, req *http.Request) {
		httpx.Error(w, req, httpx.NewError(http.StatusMethodNotAllowed, httpx.CodeBadRequest, "method not allowed on this route"))
	})

	return nil
}

// composeMiddleware wraps core (the body-limit/timeout/breaker/proxy
// handler) with, from innermost to outermost: role check, suspension check,
// auth, rate limit, admin origin restriction, and header-forgery stripping.
// Header stripping is outermost on every single rule -- public,
// authenticated, and admin alike -- so there is exactly one code path that
// can forget it: none.
//
// The suspension check sits immediately inside auth, so it runs on a verified
// principal and before any role decision: a suspended account is refused
// whatever role it holds. It is deliberately NOT applied to public routes --
// those have no principal to check, and OTP login is how a reinstated user
// gets back in.
func composeMiddleware(rule RouteRule, d Deps, core http.Handler) http.Handler {
	h := core

	// Rate limiting is composed INSIDE authentication, and that placement is
	// the control rather than a detail of it.
	//
	// Every non-OTP tier keys on platmw.ByPrincipal, which returns
	// "user:<uuid>" when a verified principal is on the context and falls back
	// to "ip:<addr>" when there is not one. Middleware composed later runs
	// EARLIER, so while these limiters sat below the switch -- outermost --
	// ByPrincipal was evaluated before RequireAuth had put a principal
	// anywhere. Every authenticated route on the platform silently ran on the
	// IP fallback, and the per-user budget that the tier table describes did
	// not exist.
	//
	// That is a bypass in one direction and an outage in the other. One
	// account gets a fresh budget from every source address it can reach the
	// gateway from, and an IPv6 /64 hands out 2^64 of them; meanwhile every
	// subscriber behind one carrier-grade NAT -- which is how most of this
	// platform's users reach the internet -- shares a single bucket and
	// throttles each other.
	//
	// The trade this placement makes, stated rather than discovered later: a
	// request with a missing or invalid token is now refused by RequireAuth
	// before it is counted. It therefore costs no Redis round trip and no
	// legitimate user's budget, and it reaches no upstream and no database --
	// LoadShed and the read/write timeouts are what bound that traffic, which
	// is what they are for. Counting 401s was never protection; it only let an
	// anonymous flood exhaust the shared bucket of everyone behind the same
	// address.
	//
	// Public routes keep the limiter inside OptionalAuth, which never refuses
	// anything, so anonymous callers are still counted -- on the IP fallback,
	// which is the only key there is for them.
	limit := func(inner http.Handler) http.Handler {
		for _, mw := range buildRateLimiters(rule.RateLimit, rule.RateLimitKeyField, d.Cache, d.Logger) {
			inner = mw(inner)
		}
		return inner
	}

	switch rule.Auth {
	case AuthPublic:
		h = limit(h)
		h = platmw.OptionalAuth(d.Authenticator)(h)
	case AuthAuthenticated:
		if len(rule.Roles) > 0 {
			h = platmw.RequireRole(toRoles(rule.Roles)...)(h)
		}
		h = limit(h)
		h = platmw.RejectSuspended(d.Cache, d.Logger)(h)
		h = platmw.RequireAuth(d.Authenticator)(h)
	case AuthAdmin:
		roles := toRoles(rule.Roles)
		if len(roles) == 0 {
			roles = platmw.AdminRoles
		}
		h = platmw.RequireRole(roles...)(h)
		h = limit(h)
		// Runs after RequireAuth, so it reads a verified issuer rather than a
		// claimed one. Binding the issuer to its keys (ADR-012) stops
		// user-service forging "iss: keycloak"; it does not stop user-service
		// asserting an admin role under its own honest issuer. This does.
		h = RequireTokenIssuer(d.Config.AdminTokenIssuers(), d.Logger)(h)
		h = platmw.RejectSuspended(d.Cache, d.Logger)(h)
		h = platmw.RequireAuth(d.Authenticator)(h)
		h = platmw.IPAllowlist(d.Config.AdminIPAllowlist, d.Logger)(h)
		// Server-side rejection, not just a missing CORS header: a request
		// whose browser Origin is the patient or doctor frontend must never
		// reach the admin surface, even if IP and role happen to pass (an
		// ops engineer on the office VPN with the patient app open in
		// another tab, say). CORS alone only hides the response from that
		// tab's JavaScript; this actually refuses the request.
		h = restrictOrigin(d.Config.CORSPatientOrigins, d.Config.CORSDoctorOrigins)(h)
	}

	h = StripForgedHeaders(h)
	return h
}

func toRoles(names []string) []platmw.Role {
	if len(names) == 0 {
		return nil
	}
	roles := make([]platmw.Role, len(names))
	for i, n := range names {
		roles[i] = platmw.Role(n)
	}
	return roles
}

// restrictOrigin 403s a request whose Origin header names one of the
// disallowed frontends. Requests with no Origin header (every non-browser
// caller: curl, another service, a mobile app using plain HTTP calls rather
// than fetch/XHR) are unaffected -- Origin is a browser-only signal, so
// absence of it is not evidence of anything.
func restrictOrigin(disallowed ...[]string) func(http.Handler) http.Handler {
	blocked := map[string]struct{}{}
	for _, list := range disallowed {
		for _, o := range list {
			blocked[o] = struct{}{}
		}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if origin != "" {
				if _, isBlocked := blocked[origin]; isBlocked {
					httpx.Error(w, r, httpx.NewError(http.StatusForbidden, httpx.CodeForbidden,
						"this origin may not call the admin surface"))
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}
