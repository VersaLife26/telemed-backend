package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	platmw "telemed/internal/platform/middleware"
)

// TestStripForgedHeaders_ClientCannotForgePrincipal is the single test the
// brief calls out by name: a client sending X-Telemed-User-ID must not have
// it reach the upstream. Without this, any client sets the header to any
// UUID it likes and every downstream service that trusts it treats the
// request as that user.
func TestStripForgedHeaders_ClientCannotForgePrincipal(t *testing.T) {
	forgedUserID := uuid.New().String()

	var sawHeader string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawHeader = r.Header.Get(HeaderUserID)
	})

	handler := StripForgedHeaders(next)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/users/me", http.NoBody)
	req.Header.Set(HeaderUserID, forgedUserID)
	req.Header.Set(HeaderRoles, "admin,super_admin")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if sawHeader != "" {
		t.Fatalf("forged %s survived StripForgedHeaders: got %q, want empty", HeaderUserID, sawHeader)
	}
}

func TestStripForgedHeaders_StripsEveryTrustedHeader(t *testing.T) {
	var remaining []string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, h := range trustedHeaders {
			if r.Header.Get(h) != "" {
				remaining = append(remaining, h)
			}
		}
	})
	handler := StripForgedHeaders(next)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/users/me", http.NoBody)
	for _, h := range trustedHeaders {
		req.Header.Set(h, "forged-value")
	}
	handler.ServeHTTP(httptest.NewRecorder(), req)

	if len(remaining) != 0 {
		t.Fatalf("trusted headers survived stripping: %v", remaining)
	}
}

// TestInjectTrustedHeaders_LegitimatePrincipalIsForwarded proves stripping
// does not also break the legitimate path: once the gateway itself has
// verified a token and attached a Principal to the request context, the
// outbound (proxied) request must carry the verified identity.
func TestInjectTrustedHeaders_LegitimatePrincipalIsForwarded(t *testing.T) {
	userID := uuid.New()
	principal := platmw.Principal{UserID: userID, Roles: []platmw.Role{platmw.RolePatient}}

	in := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/users/me", http.NoBody)
	in = in.WithContext(platmw.WithPrincipal(in.Context(), principal))

	out := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://upstream.internal/api/v1/users/me", http.NoBody)
	injectTrustedHeaders(out, in)

	if got := out.Header.Get(HeaderUserID); got != userID.String() {
		t.Fatalf("X-Telemed-User-ID = %q, want %q", got, userID.String())
	}
	if got := out.Header.Get(HeaderRoles); got != "patient" {
		t.Fatalf("X-Telemed-Roles = %q, want %q", got, "patient")
	}
}

// TestInjectTrustedHeaders_ForgedInboundHeaderDoesNotSurviveFullPipeline
// chains strip -> (simulated auth) -> inject the way router.go actually
// composes them, and asserts the header the upstream sees is derived solely
// from the verified principal, never from whatever the client sent.
func TestInjectTrustedHeaders_ForgedInboundHeaderDoesNotSurviveFullPipeline(t *testing.T) {
	forgedID := uuid.New()
	realID := uuid.New()

	var outHeader string
	core := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// r.Context(), not t.Context(): the outbound request stands in for the
		// one the reverse proxy builds, and it must inherit the inbound
		// request'''s context (deadline, cancellation, trace) exactly as
		// production does.
		out := httptest.NewRequestWithContext(r.Context(), http.MethodGet, "http://upstream.internal"+r.URL.Path, http.NoBody)
		injectTrustedHeaders(out, r)
		outHeader = out.Header.Get(HeaderUserID)
	})

	// Simulates RequireAuth: attaches the REAL, verified principal,
	// completely independent of whatever the client's headers said.
	simulateAuth := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p := platmw.Principal{UserID: realID, Roles: []platmw.Role{platmw.RolePatient}}
			next.ServeHTTP(w, r.WithContext(platmw.WithPrincipal(r.Context(), p)))
		})
	}

	handler := StripForgedHeaders(simulateAuth(core))

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/users/me", http.NoBody)
	req.Header.Set(HeaderUserID, forgedID.String()) // attacker's attempt
	handler.ServeHTTP(httptest.NewRecorder(), req)

	if outHeader != realID.String() {
		t.Fatalf("upstream saw X-Telemed-User-ID=%q, want the verified id %q (forged id was %q)",
			outHeader, realID.String(), forgedID.String())
	}
}

func TestStripHopByHopHeaders(t *testing.T) {
	h := http.Header{}
	h.Set("Connection", "Keep-Alive, X-Custom")
	h.Set("Keep-Alive", "timeout=5")
	h.Set("Proxy-Authorization", "Basic xyz")
	h.Set("Te", "trailers")
	h.Set("X-Custom", "should also go, named in Connection")
	h.Set("X-Telemed-User-ID", "should NOT be touched by this function -- StripForgedHeaders' job")
	h.Set("Content-Type", "application/json")

	stripHopByHopHeaders(h)

	for _, name := range []string{"Connection", "Keep-Alive", "Proxy-Authorization", "Te", "X-Custom"} {
		if h.Get(name) != "" {
			t.Errorf("hop-by-hop header %q survived stripping", name)
		}
	}
	if h.Get("Content-Type") != "application/json" {
		t.Error("stripHopByHopHeaders must not touch end-to-end headers")
	}
}
