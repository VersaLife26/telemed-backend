package signal

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// The response shape below is Cloudflare's documented one: iceServers with a
// STUN entry whose "urls" is a bare string and a TURN entry whose "urls" is an
// array. Decoding into []string alone fails on the first, and the failure
// would read as "TURN unavailable" rather than as a parsing bug.
const cloudflareBody = `{"iceServers":[
  {"urls":"stun:stun.cloudflare.com:3478"},
  {"urls":["turn:turn.cloudflare.com:3478?transport=udp","turns:turn.cloudflare.com:5349?transport=tcp"],
   "username":"u-123","credential":"c-456"}
]}`

func newFakeCloudflare(t *testing.T, status int, body string, calls *int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*calls++
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization = %q", got)
		}
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		if _, ok := req["ttl"]; !ok {
			t.Errorf("request body has no ttl: %v", req)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// withEndpoint points the provider at a test server by overriding the URL it
// builds. The endpoint is a package const, so the test swaps the client's
// transport instead of the address.
func withEndpoint(c *CloudflareTURN, srv *httptest.Server) {
	base := srv.URL
	c.client = &http.Client{
		Timeout:   2 * time.Second,
		Transport: rewriteTransport{base: base, inner: srv.Client().Transport},
	}
}

type rewriteTransport struct {
	base  string
	inner http.RoundTripper
}

func (t rewriteTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	u, err := r.URL.Parse(t.base)
	if err != nil {
		return nil, err
	}
	r = r.Clone(r.Context())
	r.URL.Scheme, r.URL.Host = u.Scheme, u.Host
	inner := t.inner
	if inner == nil {
		inner = http.DefaultTransport
	}
	return inner.RoundTrip(r)
}

func TestCloudflareTURNParsesBothURLShapes(t *testing.T) {
	calls := 0
	srv := newFakeCloudflare(t, http.StatusCreated, cloudflareBody, &calls)

	c := NewCloudflareTURN("key-1", "test-token", 24*time.Hour, nil, zerolog.Nop())
	withEndpoint(c, srv)

	servers, err := c.Servers(context.Background(), "patient-1")
	if err != nil {
		t.Fatalf("Servers: %v", err)
	}
	if len(servers) != 2 {
		t.Fatalf("got %d ice servers, want 2: %+v", len(servers), servers)
	}
	if len(servers[0].URLs) != 1 || servers[0].URLs[0] != "stun:stun.cloudflare.com:3478" {
		t.Errorf("stun entry = %+v, want the bare-string url promoted to a slice", servers[0])
	}
	if len(servers[1].URLs) != 2 {
		t.Errorf("turn entry urls = %v, want 2", servers[1].URLs)
	}
	// Without these the entry is a STUN server wearing a TURN URL, and every
	// CGNAT call fails with no error anywhere.
	if servers[1].Username != "u-123" || servers[1].Credential != "c-456" {
		t.Errorf("turn credential lost: %+v", servers[1])
	}
}

func TestCloudflareTURNCachesBetweenJoins(t *testing.T) {
	calls := 0
	srv := newFakeCloudflare(t, http.StatusCreated, cloudflareBody, &calls)

	c := NewCloudflareTURN("key-1", "test-token", 24*time.Hour, nil, zerolog.Nop())
	withEndpoint(c, srv)

	// Hub.ICEServers runs on every join, and a join includes every reconnect.
	// Uncached, this would be a cross-internet call on the recovery path of a
	// call that is already failing.
	for i := 0; i < 5; i++ {
		if _, err := c.Servers(context.Background(), "patient-1"); err != nil {
			t.Fatalf("Servers: %v", err)
		}
	}
	if calls != 1 {
		t.Errorf("made %d API calls for 5 joins, want 1", calls)
	}
}

func TestCloudflareTURNServesStaleCredentialWhenTheAPIIsDown(t *testing.T) {
	calls := 0
	fail := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if fail {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(cloudflareBody))
	}))
	defer srv.Close()

	c := NewCloudflareTURN("key-1", "test-token", 24*time.Hour, nil, zerolog.Nop())
	withEndpoint(c, srv)

	if _, err := c.Servers(context.Background(), "p"); err != nil {
		t.Fatalf("warm the cache: %v", err)
	}

	// Expire the cache, then break the API.
	c.mu.Lock()
	c.until = time.Now().Add(-time.Hour)
	c.mu.Unlock()
	fail = true

	servers, err := c.Servers(context.Background(), "p")
	if err != nil {
		t.Fatalf("a stale credential must be preferred to failing: %v", err)
	}
	// A slightly stale TURN credential may still work. STUN only definitely
	// does not, for the users TURN exists for.
	if len(servers) != 2 || servers[1].Username != "u-123" {
		t.Errorf("did not serve the stale credential: %+v", servers)
	}
}

func TestCloudflareTURNReportsFailureWithNoCache(t *testing.T) {
	calls := 0
	srv := newFakeCloudflare(t, http.StatusInternalServerError, `{}`, &calls)

	c := NewCloudflareTURN("key-1", "test-token", time.Hour,
		[]string{"stun:stun.cloudflare.com:3478"}, zerolog.Nop())
	withEndpoint(c, srv)

	servers, err := c.Servers(context.Background(), "p")
	if err == nil {
		t.Fatal("expected an error when there is no credential and no cache")
	}
	// It still returns STUN so the caller can decide to try anyway, rather
	// than handing back nothing at all.
	if len(servers) != 1 || len(servers[0].URLs) != 1 {
		t.Errorf("degraded result = %+v, want STUN only", servers)
	}
}
