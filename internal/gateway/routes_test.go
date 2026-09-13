package gateway

import (
	"sort"
	"strings"
	"testing"

	platmw "telemed/internal/platform/middleware"
)

// wantPublicRoutes is the exact, exhaustive allowlist of routes that bypass
// authentication. This test does not trust the route table to describe itself
// correctly -- it enumerates the loaded table and asserts the set of routes
// marked "public" is exactly this set, no more and no less. Adding a new route
// and marking it public by copy-paste accident fails this test; so does
// someone quietly tightening a route that must stay public.
//
// AGENT-BRIEF's list is "/auth/otp/*, doctor search, prescription
// verification, webhooks". Entries below go beyond that literal text.
// Each is deliberate, and the reason is recorded here rather than in a commit
// message nobody will find in 2031:
//
//   - auth-refresh, auth-logout. A client calls these precisely BECAUSE its
//     access token has expired; requiring one would make session renewal
//     impossible and is a contradiction, not a policy. The refresh token is
//     itself the credential, and user-service verifies it, detects replay of a
//     rotated token by revoking the whole session family, and refuses a
//     suspended account. These are on the same footing as OTP verify, which is
//     already on the brief's list: unauthenticated by necessity, hardened
//     upstream. logout-all is NOT here -- it acts on the whole account rather
//     than on the token presented, so it requires a live access token.
//
//   - auth-register-email, auth-login-email, auth-oauth-google. Same footing
//     as OTP: they ARE the credential exchange. user-service hashes passwords,
//     verifies Google ID tokens, rate-limits per email, and refuses suspended
//     accounts. Marking them authenticated would make first login impossible.
//
//   - doctor-reviews-get. Part of the same public doctor profile as
//     doctor-search and doctor-detail, which the brief does list. A patient
//     compares doctors by rating before creating an account, and
//     doctor-service serves the route under OptionalAuth by design. The
//     payload is published, moderated review text -- rating, comment,
//     publication state -- and carries no PHI. Its sibling doctor-reviews-post
//     is authenticated: reading opinions is public, writing one is not.
//
//   - doctor-apply, doctor-apply-documents, doctor-application-eligibility.
//     Public doctor onboarding collects details and credential images before
//     any account exists; eligibility gates OTP on the doctor portal after
//     admin approval. All three are rate-limited and carry no session.
//     Authenticated register remains for post-account document flows.
//
// Nothing else may be added without the same kind of justification. In
// particular, no route that returns or accepts patient data belongs here.
var wantPublicRoutes = map[string]struct{}{
	"otp-send":                       {},
	"otp-verify":                     {},
	"auth-register-email":            {},
	"auth-login-email":               {},
	"auth-oauth-google":              {},
	"auth-refresh":                   {},
	"auth-logout":                    {},
	"doctor-search":                  {},
	"doctor-detail":                  {},
	"doctor-reviews-get":             {},
	"doctor-apply":                   {},
	"doctor-apply-documents":         {},
	"doctor-application-eligibility": {},
	"prescription-verify-get":        {},
	"webhook-stripe":                 {},
	"webhook-payhere":                {},
	"webhook-dialog":                 {},
	"webhook-livekit":                {},
}

// testSurfacePrefix is the path prefix owned by the developer test surface.
//
// Its routes are public, and they are NOT listed in wantPublicRoutes, because
// enumerating them there would put them on the same footing as OTP verify --
// a considered, permanent exception to authentication. They are not that.
// The control that decides whether they exist is config.Base.TestModeEnabled
// (TELEMED_TEST_MODE), plus the composer dropping every one of these rules
// when the module was not built.
//
// What the tests below assert instead is the containment: every public route
// outside the reviewed allowlist must live under this prefix and point at this
// upstream. That is a stronger check than a list of eight names -- a new
// public route anywhere else still fails, and a test route that quietly grew a
// pattern outside /api/v1/test/ fails too.
const (
	testSurfacePrefix   = "/api/v1/test/"
	testSurfaceUpstream = "testkit-service"
)

// isTestSurface reports whether r belongs to the developer test surface.
func isTestSurface(r RouteRule) bool { return r.Upstream == testSurfaceUpstream }

func loadTestRoutes(t *testing.T) []RouteRule {
	t.Helper()
	rules, err := LoadRoutes("")
	if err != nil {
		t.Fatalf("LoadRoutes: %v", err)
	}
	return rules
}

func TestRouteTable_PublicAllowlistIsExact(t *testing.T) {
	rules := loadTestRoutes(t)

	got := map[string]struct{}{}
	for _, r := range PublicRoutes(rules) {
		got[r.Name] = struct{}{}
	}

	for name := range wantPublicRoutes {
		if _, ok := got[name]; !ok {
			t.Errorf("route %q should be public per AGENT-BRIEF but is not", name)
		}
	}
	for _, r := range PublicRoutes(rules) {
		if _, ok := wantPublicRoutes[r.Name]; ok {
			continue
		}
		if isTestSurface(r) {
			continue // asserted separately by TestRouteTable_TestSurfaceIsContained
		}
		t.Errorf("route %q is public but is NOT on the AGENT-BRIEF allowlist -- "+
			"an authenticated route may have been accidentally made public", r.Name)
	}
}

// TestRouteTable_TestSurfaceIsContained is what stands in for an allowlist
// entry per test route.
//
// The test surface is unauthenticated and hands back plaintext OTP codes, so
// the thing worth asserting is not "these eight names are expected" -- it is
// that the surface cannot spread. Every rule pointing at the test upstream
// must sit under /api/v1/test/, and nothing under /api/v1/test/ may point
// anywhere else. A test route that grew a pattern reaching into another
// domain, or a real route that acquired the test upstream, fails here.
func TestRouteTable_TestSurfaceIsContained(t *testing.T) {
	rules := loadTestRoutes(t)

	var seen int
	for _, r := range rules {
		underPrefix := strings.HasPrefix(r.Pattern, testSurfacePrefix)
		switch {
		case isTestSurface(r) && !underPrefix:
			t.Errorf("route %q is on the test upstream but its pattern %q is outside %s",
				r.Name, r.Pattern, testSurfacePrefix)
		case underPrefix && !isTestSurface(r):
			t.Errorf("route %q is under %s but points at upstream %q, not %q",
				r.Name, testSurfacePrefix, r.Upstream, testSurfaceUpstream)
		case isTestSurface(r):
			seen++
			if r.Auth != AuthPublic {
				t.Errorf("route %q is on the test surface but has auth %q; the surface exists to need no login",
					r.Name, r.Auth)
			}
		}
	}
	if seen == 0 {
		t.Error("no test-surface routes in the table; the containment check is asserting nothing")
	}
}

// TestRouteTable_EveryNonPublicRouteRequiresAuth is the same guarantee from
// the other direction: every rule not in the public set must be either
// "authenticated" or "admin" -- there is no third, implicit category.
func TestRouteTable_EveryNonPublicRouteRequiresAuth(t *testing.T) {
	rules := loadTestRoutes(t)
	for _, r := range rules {
		if _, isPublic := wantPublicRoutes[r.Name]; isPublic {
			continue
		}
		if isTestSurface(r) {
			continue
		}
		if r.Auth != AuthAuthenticated && r.Auth != AuthAdmin {
			t.Errorf("route %q has auth mode %q, want authenticated or admin (it is not on the public allowlist)",
				r.Name, r.Auth)
		}
	}
}

// adminPrefixExceptions are the auth-mode "admin" routes that legitimately
// live outside /api/v1/admin/*.
//
// The prefix rule is a NAMING convention, not a security boundary: router.go's
// composeMiddleware applies IPAllowlist, restrictOrigin and RequireTokenIssuer
// per rule, keyed on the auth mode, and never looks at the URL. An earlier
// version of this file asserted the opposite ("the IP allowlist only wraps
// that prefix") and used that to justify leaving payouts/run on auth mode
// "authenticated" -- which ran the platform's payout trigger with none of the
// three admin-network controls that every read-only /admin/analytics route
// already had. The route is now auth mode "admin"; the convention is kept by
// naming the exception here rather than by weakening the route.
var adminPrefixExceptions = map[string]string{
	"payouts-run": "payment-service registers POST /api/v1/payouts/run; moving the path " +
		"would be a client-visible API break, and the admin controls are applied by " +
		"auth mode rather than by prefix, so the path is cosmetic here.",
}

// TestRouteTable_AdminRoutesAreUnderAdminPrefix guards the belt-and-braces
// requirement: every "admin" auth-mode route lives under /api/v1/admin/*, or
// is named in adminPrefixExceptions with a reason.
func TestRouteTable_AdminRoutesAreUnderAdminPrefix(t *testing.T) {
	rules := loadTestRoutes(t)
	for _, r := range AdminRoutes(rules) {
		if hasPrefix(r.Pattern, "/api/v1/admin/") {
			continue
		}
		if _, excepted := adminPrefixExceptions[r.Name]; excepted {
			continue
		}
		t.Errorf("admin route %q has pattern %q, which is not under /api/v1/admin/ "+
			"and is not in adminPrefixExceptions", r.Name, r.Pattern)
	}
}

// TestRouteTable_PayoutRunIsOnTheAdminSurface pins the fix. Auth mode is what
// selects IPAllowlist + restrictOrigin + RequireTokenIssuer in
// composeMiddleware, so this one field is the difference between the payout
// trigger sitting on the admin network and being reachable from anywhere by
// anyone holding a token with a finance role claim.
func TestRouteTable_PayoutRunIsOnTheAdminSurface(t *testing.T) {
	for _, r := range loadTestRoutes(t) {
		if r.Name != "payouts-run" {
			continue
		}
		if r.Auth != AuthAdmin {
			t.Fatalf("payouts-run has auth mode %q, want %q: the payout trigger must "+
				"carry the admin IP allowlist, the admin origin restriction and the "+
				"admin issuer binding, none of which auth mode %q applies",
				r.Auth, AuthAdmin, r.Auth)
		}
		return
	}
	t.Fatal("payouts-run is not in the route table")
}

func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

// TestRouteTable_RoleNamesAreKnownRoles catches a typo'd role (e.g. "docter")
// that would silently make a route unreachable by anyone.
func TestRouteTable_RoleNamesAreKnownRoles(t *testing.T) {
	known := map[string]struct{}{
		string(platmw.RolePatient):    {},
		string(platmw.RoleDoctor):     {},
		string(platmw.RoleAdmin):      {},
		string(platmw.RoleSuperAdmin): {},
		string(platmw.RoleOps):        {},
		string(platmw.RoleFinance):    {},
		string(platmw.RoleSupport):    {},
		string(platmw.RoleService):    {},
	}
	rules := loadTestRoutes(t)
	for _, r := range rules {
		for _, role := range r.Roles {
			if _, ok := known[role]; !ok {
				t.Errorf("route %q references unknown role %q", r.Name, role)
			}
		}
	}
}

// TestRouteTable_UpstreamsAreInThePortTable asserts every route references
// one of the eight backend services in the brief's port table -- not a typo,
// not a ninth service that does not exist.
func TestRouteTable_UpstreamsAreInThePortTable(t *testing.T) {
	knownUpstreams := map[string]struct{}{
		"user-service": {}, "doctor-service": {}, "scheduling-service": {},
		"consultation-service": {}, "payment-service": {}, "notification-service": {},
		"record-service": {}, "admin-service": {},
		// Not in the brief's port table because it is not a backend service:
		// it is the developer test surface, which exists only in a process
		// that built it and is dropped from the table entirely otherwise.
		testSurfaceUpstream: {},
	}
	rules := loadTestRoutes(t)
	for _, r := range rules {
		if _, ok := knownUpstreams[r.Upstream]; !ok {
			t.Errorf("route %q references unknown upstream %q", r.Name, r.Upstream)
		}
	}
}

// TestRouteTable_NoDuplicateNames ensures route names are unique, since
// several tests and the health-check wiring use them as map keys.
func TestRouteTable_NoDuplicateNames(t *testing.T) {
	rules := loadTestRoutes(t)
	seen := map[string]bool{}
	for _, r := range rules {
		if seen[r.Name] {
			t.Errorf("duplicate route name %q", r.Name)
		}
		seen[r.Name] = true
	}
}

// TestRouteTable_EmbeddedAndRootCopyMatch guards against the go:embed copy
// (internal/gateway/routes.default.json) drifting from the canonical
// repo-root config/routes.json that ops actually edit.
func TestRouteTable_EmbeddedAndRootCopyMatch(t *testing.T) {
	embedded, err := LoadRoutes("")
	if err != nil {
		t.Fatalf("load embedded routes: %v", err)
	}
	fromDisk, err := LoadRoutes("../../config/routes.json")
	if err != nil {
		t.Fatalf("load config/routes.json: %v", err)
	}
	if len(embedded) != len(fromDisk) {
		t.Fatalf("embedded table has %d routes, config/routes.json has %d -- they have drifted, run `make openapi-sync`",
			len(embedded), len(fromDisk))
	}
	embNames := routeNames(embedded)
	diskNames := routeNames(fromDisk)
	for i := range embNames {
		if embNames[i] != diskNames[i] {
			t.Fatalf("route table drift at position %d: embedded=%q disk=%q", i, embNames[i], diskNames[i])
		}
	}
}

func routeNames(rules []RouteRule) []string {
	names := make([]string, len(rules))
	for i := range rules {
		names[i] = rules[i].Name
	}
	sort.Strings(names)
	return names
}

func TestLoadRoutes_RejectsInvalidAuthMode(t *testing.T) {
	_, err := LoadRoutes("testdata/invalid_auth.json")
	if err == nil {
		t.Fatal("expected an error loading a route table with an invalid auth mode")
	}
}

func TestLoadRoutes_RejectsEmptyTable(t *testing.T) {
	_, err := LoadRoutes("testdata/empty.json")
	if err == nil {
		t.Fatal("expected an error loading an empty route table")
	}
}

func TestLoadRoutes_FallsBackToEmbeddedWhenFileMissing(t *testing.T) {
	rules, err := LoadRoutes("testdata/does_not_exist.json")
	if err != nil {
		t.Fatalf("LoadRoutes with a missing override file should fall back, got error: %v", err)
	}
	if len(rules) == 0 {
		t.Fatal("expected the embedded default table")
	}
}
