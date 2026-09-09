package servicetoken

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func tokenServer(t *testing.T, calls *atomic.Int64, expiresIn int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if err := r.ParseForm(); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if r.Form.Get("grant_type") != "client_credentials" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"tok-` +
			r.Form.Get("client_id") + `","expires_in":` + strconv.Itoa(expiresIn) + `}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestTokenIsCachedAndReused(t *testing.T) {
	var calls atomic.Int64
	srv := tokenServer(t, &calls, 900)

	s, err := New(Config{TokenURL: srv.URL, ClientID: "telemed-api", ClientSecret: "s"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for range 5 {
		if _, err := s.Token(t.Context()); err != nil {
			t.Fatalf("Token: %v", err)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("token endpoint called %d times, want 1: every RPC minting its own "+
			"token would put Keycloak on the hot path of the mesh", got)
	}
}

// TestTokenIsRenewedBeforeItExpires pins the margin. A token that passes the
// cache check, spends its last milliseconds in flight and arrives expired is
// an intermittent Unauthenticated under load -- the worst kind to diagnose.
func TestTokenIsRenewedBeforeItExpires(t *testing.T) {
	var calls atomic.Int64
	srv := tokenServer(t, &calls, 90) // shorter than 2x refreshMargin

	now := time.Now()
	s, err := New(Config{
		TokenURL: srv.URL, ClientID: "telemed-api",
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := s.Token(t.Context()); err != nil {
		t.Fatalf("first: %v", err)
	}

	// 40s in: 50s of life left, more than the 60s margin? No -- 50 < 60, so it
	// must refresh.
	now = now.Add(40 * time.Second)
	if _, err := s.Token(t.Context()); err != nil {
		t.Fatalf("second: %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("calls = %d, want 2: a token inside the refresh margin must be replaced", got)
	}
}

func TestConcurrentCallersShareOneFetch(t *testing.T) {
	var calls atomic.Int64
	srv := tokenServer(t, &calls, 900)

	s, err := New(Config{TokenURL: srv.URL, ClientID: "telemed-api"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var wg sync.WaitGroup
	for range 25 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = s.Token(context.Background())
		}()
	}
	wg.Wait()
	if got := calls.Load(); got != 1 {
		t.Errorf("calls = %d, want 1: a thundering herd must collapse into one refresh", got)
	}
}

// TestAFailedRefreshDoesNotServeTheStaleToken states the deliberate choice.
// Returning the old token would turn "cannot reach Keycloak" into a confusing
// Unauthenticated from the far end, hiding the actual fault.
func TestAFailedRefreshDoesNotServeTheStaleToken(t *testing.T) {
	var fail atomic.Bool
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		if fail.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"tok","expires_in":90}`))
	}))
	t.Cleanup(srv.Close)

	now := time.Now()
	s, err := New(Config{TokenURL: srv.URL, ClientID: "c", Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := s.Token(t.Context()); err != nil {
		t.Fatalf("first: %v", err)
	}

	fail.Store(true)
	now = now.Add(45 * time.Second) // inside the refresh margin
	if _, err := s.Token(t.Context()); err == nil {
		t.Fatal("a failed refresh returned a token; the stale one must not be served")
	}
}

// TestTheSecretNeverReachesTheError guards a leak path: a token endpoint's
// error body can echo the request, and this request carries the client secret.
func TestTheSecretNeverReachesTheError(t *testing.T) {
	const secret = "super-secret-value"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.WriteHeader(http.StatusBadRequest)
		// A provider echoing the whole form back, which some do.
		_, _ = w.Write([]byte("bad request: " + r.Form.Encode()))
	}))
	t.Cleanup(srv.Close)

	s, err := New(Config{TokenURL: srv.URL, ClientID: "c", ClientSecret: secret})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = s.Token(t.Context())
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("the client secret appears in the error, which goes to logs: %q", err)
	}
}

func TestNewRefusesAnEmptyConfiguration(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("New accepted an empty config; a client with no credential does not " +
			"fail at boot, it fails on every call afterwards")
	}
}

func TestRequireTransportSecurityDefaultsToTheSafeAnswerWhenSet(t *testing.T) {
	s, err := New(Config{TokenURL: "http://x/y", ClientID: "c", RequireTLS: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !s.RequireTransportSecurity() {
		t.Error("RequireTLS was set but the credential would travel over plaintext")
	}
}
