package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/rs/zerolog"

	platmw "telemed/internal/platform/middleware"
	"telemed/internal/platform/observability"
)

// echoUpstream is a fake backend service: it records the last request it
// received (headers included) and answers 200 with a trivial envelope. Every
// route table upstream in the router tests points at one of these unless a
// test needs different behaviour (a dead server, a slow one).
type echoUpstream struct {
	mu       sync.Mutex
	lastHdrs http.Header
	lastPath string
}

func newEchoUpstream(t *testing.T) (*httptest.Server, *echoUpstream) {
	t.Helper()
	e := &echoUpstream{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.mu.Lock()
		e.lastHdrs = r.Header.Clone()
		e.lastPath = r.URL.Path
		e.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":"ok"}`))
	}))
	t.Cleanup(srv.Close)
	return srv, e
}

func (e *echoUpstream) headers() http.Header {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.lastHdrs
}

// reset clears the recorded request so a table-driven test can assert "the
// upstream was never reached" on the next case rather than seeing the previous
// one's headers. Without it, "echo.headers() == nil" only ever means anything
// on the first subtest.
func (e *echoUpstream) reset() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.lastHdrs = nil
	e.lastPath = ""
}

// allUpstreamNames mirrors buildUpstreams in cmd/server/main.go.
//
// testSurfaceUpstream is included because the router tests mount the FULL
// table, and the composer only ever omits the test routes together with the
// upstream that serves them. Mounting the table with the routes present and
// the upstream missing is a state the composer cannot produce, so failing
// every router test on it would be testing an impossible configuration.
var allUpstreamNames = []string{
	"user-service", "doctor-service", "scheduling-service", "consultation-service",
	"payment-service", "notification-service", "record-service", "admin-service",
	testSurfaceUpstream,
}

func testConfig() Config {
	return Config{
		ServiceName:        "test-gateway",
		RedisURL:           "redis://unused",
		KeycloakJWKSURL:    "http://unused",
		MaxInFlight:        1000,
		MaxBodyBytesFast:   1 << 20,
		MaxBodyBytesWrite:  4 << 20,
		MaxBodyBytesUpload: 15 << 20,
		CORSPatientOrigins: []string{"https://patient.yourapp.lk"},
		CORSDoctorOrigins:  []string{"https://doctor.yourapp.lk"},
		CORSAdminOrigins:   []string{"https://admin.yourapp.lk"},
	}
}

// buildTestRouter wires a full gateway router where every upstream in the
// port table points at defaultUpstreamURL, except for any override supplied
// in urlOverrides (keyed by upstream name) -- used to make exactly one
// upstream dead or slow without rebuilding the whole table.
func buildTestRouter(t *testing.T, ti *testIdentity, cfg Config, defaultUpstreamURL string, urlOverrides map[string]string) (http.Handler, map[string]*Upstream, *fakeCache) {
	t.Helper()

	routes, err := LoadRoutes("")
	if err != nil {
		t.Fatalf("LoadRoutes: %v", err)
	}

	c := newFakeCache()
	metrics := observability.NewMetrics("test-gateway-" + uuid.NewString())
	gm := NewGatewayMetrics(metrics)

	upstreams := make(map[string]*Upstream, len(allUpstreamNames))
	for _, name := range allUpstreamNames {
		target := defaultUpstreamURL
		if o, ok := urlOverrides[name]; ok {
			target = o
		}
		breaker := NewBreaker(c, BreakerConfig{
			Name: name, FailureThreshold: 3,
			OpenDuration: 60 * time.Millisecond, ProbeGrace: 200 * time.Millisecond,
		}, zerolog.Nop())
		up, err := NewUpstream(name, target, breaker, 0, time.Millisecond, nil, zerolog.Nop())
		if err != nil {
			t.Fatalf("NewUpstream(%s): %v", name, err)
		}
		upstreams[name] = up
	}

	r := chi.NewRouter()
	if err := Mount(r, Deps{
		Config: cfg, Cache: c, Authenticator: ti.auth, Metrics: metrics,
		GatewayMetric: gm, Logger: zerolog.Nop(), Upstreams: upstreams, Routes: routes,
	}); err != nil {
		t.Fatalf("Mount: %v", err)
	}
	return r, upstreams, c
}

func TestRouter_PublicRouteReachesUpstreamWithoutAuth(t *testing.T) {
	ti := newTestIdentity(t)
	upSrv, echo := newEchoUpstream(t)
	r, _, _ := buildTestRouter(t, ti, testConfig(), upSrv.URL, nil)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/doctors", http.NoBody)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("public route without auth: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if echo.headers() == nil {
		t.Fatal("upstream was never called")
	}
}

func TestRouter_PublicWebhookBypassesAuth(t *testing.T) {
	ti := newTestIdentity(t)
	upSrv, echo := newEchoUpstream(t)
	r, _, _ := buildTestRouter(t, ti, testConfig(), upSrv.URL, nil)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/webhooks/stripe", http.NoBody)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("webhook without auth: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if echo.headers() == nil {
		t.Fatal("upstream was never called")
	}
}

func TestRouter_AuthenticatedRouteRejectsMissingToken(t *testing.T) {
	ti := newTestIdentity(t)
	upSrv, _ := newEchoUpstream(t)
	r, _, _ := buildTestRouter(t, ti, testConfig(), upSrv.URL, nil)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/users/me", http.NoBody)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestRouter_AuthenticatedRouteAcceptsValidToken(t *testing.T) {
	ti := newTestIdentity(t)
	upSrv, echo := newEchoUpstream(t)
	r, _, _ := buildTestRouter(t, ti, testConfig(), upSrv.URL, nil)

	userID := uuid.New()
	token := ti.sign(t, userID, []platmw.Role{platmw.RolePatient})

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/users/me", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := echo.headers().Get(HeaderUserID); got != userID.String() {
		t.Fatalf("upstream saw X-Telemed-User-ID=%q, want %q", got, userID.String())
	}
	if got := echo.headers().Get(HeaderRoles); got != "patient" {
		t.Fatalf("upstream saw X-Telemed-Roles=%q, want %q", got, "patient")
	}
}

// TestRouter_HeaderForgeryRejectedEndToEnd is the full-stack version of the
// header-forgery guarantee: a real request through the real chi router,
// real auth verification, real proxying. The forged identity must never
// reach the upstream; only the identity the JWT signature actually proves.
func TestRouter_HeaderForgeryRejectedEndToEnd(t *testing.T) {
	ti := newTestIdentity(t)
	upSrv, echo := newEchoUpstream(t)
	r, _, _ := buildTestRouter(t, ti, testConfig(), upSrv.URL, nil)

	realUserID := uuid.New()
	forgedUserID := uuid.New()
	token := ti.sign(t, realUserID, []platmw.Role{platmw.RolePatient})

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/users/me", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set(HeaderUserID, forgedUserID.String())
	req.Header.Set(HeaderRoles, "admin,super_admin")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := echo.headers().Get(HeaderUserID); got != realUserID.String() {
		t.Fatalf("upstream saw X-Telemed-User-ID=%q, want the verified id %q (forged id was %q)",
			got, realUserID.String(), forgedUserID.String())
	}
	if got := echo.headers().Get(HeaderRoles); got != "patient" {
		t.Fatalf("upstream saw X-Telemed-Roles=%q, want %q (forged roles were %q)", got, "patient", "admin,super_admin")
	}
}

func TestRouter_AdminRouteRequiresAdminRole(t *testing.T) {
	ti := newTestIdentity(t)
	upSrv, _ := newEchoUpstream(t)
	r, _, _ := buildTestRouter(t, ti, testConfig(), upSrv.URL, nil)

	patientToken := ti.sign(t, uuid.New(), []platmw.Role{platmw.RolePatient})
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/admin/doctors/pending", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+patientToken)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("patient role on admin route: status = %d, want 403", rec.Code)
	}

	adminToken := ti.sign(t, uuid.New(), []platmw.Role{platmw.RoleAdmin})
	req2 := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/admin/doctors/pending", http.NoBody)
	req2.Header.Set("Authorization", "Bearer "+adminToken)
	rec2 := httptest.NewRecorder()
	r.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("admin role on admin route: status = %d, body = %s", rec2.Code, rec2.Body.String())
	}
}

func TestRouter_AdminRouteEnforcesIPAllowlist(t *testing.T) {
	ti := newTestIdentity(t)
	upSrv, _ := newEchoUpstream(t)
	cfg := testConfig()
	cfg.AdminIPAllowlist = []string{"10.0.0.0/8"} // will not match httptest's default RemoteAddr
	r, _, _ := buildTestRouter(t, ti, cfg, upSrv.URL, nil)

	adminToken := ti.sign(t, uuid.New(), []platmw.Role{platmw.RoleAdmin})
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/admin/doctors/pending", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+adminToken)
	req.RemoteAddr = "203.0.113.7:54321" // a public IP, deliberately outside 10.0.0.0/8
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("admin role but IP not allowlisted: status = %d, want 403, body = %s", rec.Code, rec.Body.String())
	}
}

// TestRouter_AdminAllowlistIsNotBypassableByForgedXFF is the regression test
// for GHSA-3fxj-6jh8-hvhx. chi's middleware.RealIP -- which this gateway used
// to install -- rewrites r.RemoteAddr from X-Forwarded-For unconditionally,
// reading the LEFTMOST entry, which is the part of the list the client
// controls. With the admin surface guarded by an IP allowlist, that turned a
// documented security control into decoration: one header and any client is
// "inside" 10.0.0.0/8.
//
// platform/middleware.RealIP believes forwarding headers only when the TCP
// peer is itself a trusted proxy, and walks the list from the right. The two
// cases below are the whole contract: a forged header from an untrusted peer
// is ignored, and a real header from a trusted proxy is honoured.
func TestRouter_AdminAllowlistIsNotBypassableByForgedXFF(t *testing.T) {
	ti := newTestIdentity(t)
	upSrv, _ := newEchoUpstream(t)
	cfg := testConfig()
	// A public allowlist range, so "inside the allowlist" and "inside the
	// default trusted-proxy ranges" cannot be confused with one another.
	cfg.AdminIPAllowlist = []string{"198.51.100.0/24"}
	inner, _, _ := buildTestRouter(t, ti, cfg, upSrv.URL, nil)

	// Wrap in the same client-IP resolution server.New installs. nil means
	// the platform default: private ranges are trusted proxies, nothing else.
	r := platmw.RealIP(nil)(inner)

	adminToken := ti.sign(t, uuid.New(), []platmw.Role{platmw.RoleAdmin})

	cases := []struct {
		name       string
		remoteAddr string
		xff        string
		want       int
	}{
		{
			name:       "forged XFF from an untrusted peer is ignored",
			remoteAddr: "203.0.113.7:54321", // public: not a trusted proxy
			xff:        "198.51.100.9",      // claims to be inside the allowlist
			want:       http.StatusForbidden,
		},
		{
			name:       "forged XFF chain from an untrusted peer is still ignored",
			remoteAddr: "203.0.113.7:54321",
			xff:        "198.51.100.9, 203.0.113.7",
			want:       http.StatusForbidden,
		},
		{
			name:       "XFF from a trusted proxy is honoured",
			remoteAddr: "10.1.2.3:44444", // inside the default trusted ranges
			xff:        "198.51.100.9",
			want:       http.StatusOK,
		},
		{
			name:       "trusted proxy forwarding a non-allowlisted client is still blocked",
			remoteAddr: "10.1.2.3:44444",
			xff:        "203.0.113.7",
			want:       http.StatusForbidden,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequestWithContext(t.Context(),
				http.MethodGet, "/api/v1/admin/doctors/pending", http.NoBody)
			req.Header.Set("Authorization", "Bearer "+adminToken)
			req.Header.Set("X-Forwarded-For", tc.xff)
			req.RemoteAddr = tc.remoteAddr
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d, body = %s", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

func TestRouter_AdminRouteRejectsPatientOrigin(t *testing.T) {
	ti := newTestIdentity(t)
	upSrv, _ := newEchoUpstream(t)
	r, _, _ := buildTestRouter(t, ti, testConfig(), upSrv.URL, nil)

	adminToken := ti.sign(t, uuid.New(), []platmw.Role{platmw.RoleAdmin})
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/admin/doctors/pending", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+adminToken)
	req.Header.Set("Origin", "https://patient.yourapp.lk")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("admin route called with patient Origin: status = %d, want 403, body = %s", rec.Code, rec.Body.String())
	}
}

func TestRouter_CircuitBreakerOpensAndShortCircuits(t *testing.T) {
	ti := newTestIdentity(t)
	upSrv, _ := newEchoUpstream(t)

	deadSrv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := deadSrv.URL
	deadSrv.Close() // connection refused for every subsequent request

	r, upstreams, _ := buildTestRouter(t, ti, testConfig(), upSrv.URL, map[string]string{"user-service": deadURL})

	token := ti.sign(t, uuid.New(), []platmw.Role{platmw.RolePatient})

	for i := 0; i < 3; i++ {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/users/me", http.NoBody)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("failure %d against dead upstream: status = %d, want 503", i, rec.Code)
		}
	}

	if got := upstreams["user-service"].Breaker.Snapshot(context.Background()); got != StateOpen {
		t.Fatalf("breaker state = %v, want open after 3 consecutive failures", got)
	}

	// One more request: the breaker itself must short-circuit before a
	// connection attempt, and still answer 503 in the platform envelope.
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/users/me", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (circuit open)", rec.Code)
	}
}

func TestRouter_LoadSheddingCapsInFlight(t *testing.T) {
	ti := newTestIdentity(t)

	started := make(chan struct{})
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		<-release
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":"ok"}`))
	}))
	t.Cleanup(slow.Close)

	cfg := testConfig()
	cfg.MaxInFlight = 1
	r, _, _ := buildTestRouter(t, ti, cfg, slow.URL, nil)

	done := make(chan int, 1)
	go func() {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/doctors", http.NoBody)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		done <- rec.Code
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("first request never reached the upstream")
	}

	// The first request now holds the gateway's one unit of in-flight
	// capacity. A second, concurrent request must be shed immediately.
	req2 := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/doctors", http.NoBody)
	rec2 := httptest.NewRecorder()
	r.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusServiceUnavailable {
		t.Fatalf("second concurrent request: status = %d, want 503 (load shed)", rec2.Code)
	}

	close(release)
	select {
	case code := <-done:
		if code != http.StatusOK {
			t.Fatalf("first request: status = %d, want 200", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first request never completed after release")
	}
}

func TestRouter_UnknownPathReturnsJSONNotFound(t *testing.T) {
	ti := newTestIdentity(t)
	upSrv, _ := newEchoUpstream(t)
	r, _, _ := buildTestRouter(t, ti, testConfig(), upSrv.URL, nil)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/this/route/does/not/exist", http.NoBody)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct == "" {
		t.Fatal("expected a JSON envelope, got no Content-Type")
	}
}

// TestRouter_SpecificPatternBeatsWildcard proves the routing-precedence
// claim the gateway relies on instead of a hand-rolled matcher:
// /doctors/{id}/slots (scheduling-service) must win over the
// /doctors/{id} pattern (doctor-service) for that exact shape, and the
// broader /doctors/{id} pattern must still catch anything else under that
// segment. Chi's own radix-tree router resolves this by specificity
// (static > named param > wildcard); this test is what makes that claim
// verifiable rather than asserted in a comment.
func TestRouter_SpecificPatternBeatsWildcard(t *testing.T) {
	ti := newTestIdentity(t)

	schedulingSrv, schedulingEcho := newEchoUpstream(t)
	doctorSrv, doctorEcho := newEchoUpstream(t)

	r, _, _ := buildTestRouter(t, ti, testConfig(), doctorSrv.URL, map[string]string{
		"scheduling-service": schedulingSrv.URL,
	})

	token := ti.sign(t, uuid.New(), []platmw.Role{platmw.RolePatient})
	doctorID := uuid.New()

	// /api/v1/doctors/{doctorID}/slots must be routed to scheduling-service.
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/doctors/"+doctorID.String()+"/slots", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("doctors/{id}/slots: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if schedulingEcho.headers() == nil {
		t.Fatal("doctors/{id}/slots did not reach scheduling-service")
	}
	if doctorEcho.headers() != nil {
		t.Fatal("doctors/{id}/slots was routed to doctor-service instead of scheduling-service")
	}

	// /api/v1/doctors/{doctorID} (no /slots suffix) is public and must be
	// routed to doctor-service, not scheduling-service.
	req2 := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/doctors/"+doctorID.String(), http.NoBody)
	rec2 := httptest.NewRecorder()
	r.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("doctors/{id}: status = %d, body = %s", rec2.Code, rec2.Body.String())
	}
	if doctorEcho.headers() == nil {
		t.Fatal("doctors/{id} did not reach doctor-service")
	}
}

// TestRouter_AdminCatchallCoexistsWithSpecificAdminRoutes proves the
// admin-catchall wildcard does not swallow the three explicitly-typed admin
// routes registered alongside it.
func TestRouter_AdminCatchallCoexistsWithSpecificAdminRoutes(t *testing.T) {
	ti := newTestIdentity(t)
	upSrv, echo := newEchoUpstream(t)
	r, _, _ := buildTestRouter(t, ti, testConfig(), upSrv.URL, nil)

	adminToken := ti.sign(t, uuid.New(), []platmw.Role{platmw.RoleAdmin})

	for _, path := range []string{
		"/api/v1/admin/doctors/pending",         // specific
		"/api/v1/admin/analytics/revenue",       // specific
		"/api/v1/admin/disputes/some-random-id", // only the catchall covers this
	} {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, http.NoBody)
		req.Header.Set("Authorization", "Bearer "+adminToken)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status = %d, want 200 (admin-gated but reachable)", path, rec.Code)
		}
	}
	if echo.headers() == nil {
		t.Fatal("upstream was never reached")
	}
}
