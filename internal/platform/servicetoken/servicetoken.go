// Package servicetoken obtains and caches the machine-to-machine credential a
// service presents when it calls another service over gRPC.
//
// It exists because securing the mesh broke it. The F5 fix put an
// authenticating interceptor in front of every gRPC server -- correct, those
// surfaces answered anyone who could open a TCP connection -- but no client on
// the platform presented a credential, so every internal call started coming
// back Unauthenticated. Both servers and clients still built, started and
// passed their tests; the break was entirely at the call.
//
// The credential is a Keycloak client_credentials token carrying the realm
// role "service". That is deliberate reuse rather than a new mechanism: the
// realm already defines the role, telemed-api already has serviceAccounts
// enabled, and its service account already holds it. Every gRPC server already
// verifies Keycloak-issued tokens through the same Authenticator as its HTTP
// surface, so there is exactly one place where a signature becomes a
// principal.
package servicetoken

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// ErrNotConfigured is returned by New when no credentials were supplied.
//
// Callers decide what that means. A service whose only gRPC dependency is
// optional may log and continue; one that cannot work without the mesh should
// refuse to start, because a client with no credential does not fail at boot,
// it fails on every call afterwards -- which is precisely how this went
// unnoticed the first time.
var ErrNotConfigured = errors.New("servicetoken: no client credentials configured")

// Config describes the Keycloak client whose service account carries the
// "service" realm role.
type Config struct {
	// TokenURL is the realm's token endpoint, e.g.
	// https://keycloak/realms/telemedicine/protocol/openid-connect/token
	TokenURL     string
	ClientID     string
	ClientSecret string

	// StaticToken is a pre-minted bearer token used INSTEAD of the client
	// credentials flow. When set, TokenURL/ClientID/ClientSecret are ignored
	// and Token returns this value unchanged.
	//
	// It exists for a deployment with no OAuth identity provider on the mesh
	// path. The token is expected to be a long-lived JWT signed by one of the
	// platform's own issuers and carrying the "service" role, so the RECEIVING
	// side is unchanged: it still verifies a signature against a bound key set
	// and still requires RoleService. Nothing about the server's trust model
	// is relaxed -- only where the client gets its token from.
	//
	// The trade is real and worth stating: a static token cannot be rotated by
	// waiting for it to expire. Rotating it means changing it in both services
	// and restarting them.
	StaticToken string

	// RequireTLS reports whether the credential may only travel over a
	// secured connection. It defaults to true and should only be false for
	// local development: a bearer token on a plaintext link is readable by
	// anything on the path, and this one is a mesh-wide credential.
	RequireTLS bool

	// HTTPClient is injectable for tests. Nil means a client with a sane
	// timeout -- never http.DefaultClient, which has none, so a hung Keycloak
	// would pin a caller's goroutine indefinitely.
	HTTPClient *http.Client

	// Now is injectable so expiry logic is testable without sleeping.
	Now func() time.Time
}

// refreshMargin is how long before expiry a cached token is replaced.
//
// Tokens are typically minted with a 15-minute life. Renewing a minute early
// costs one extra request per token and removes the race where a token passes
// the cache check, spends its remaining milliseconds in flight, and arrives at
// the server already expired -- a failure that appears as an intermittent
// Unauthenticated under load and is miserable to diagnose.
const refreshMargin = 60 * time.Second

// Source hands out a cached service token and satisfies
// credentials.PerRPCCredentials, so it can be handed straight to
// grpc.WithPerRPCCredentials.
type Source struct {
	cfg    Config
	client *http.Client
	now    func() time.Time

	mu        sync.Mutex
	token     string
	expiresAt time.Time
}

// New validates the configuration and returns a Source. It does NOT contact
// Keycloak: a service must be able to start while its identity provider is
// briefly unavailable, and the first call will surface any real problem.
func New(cfg Config) (*Source, error) {
	if cfg.StaticToken == "" && (cfg.TokenURL == "" || cfg.ClientID == "") {
		return nil, ErrNotConfigured
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Source{cfg: cfg, client: client, now: now}, nil
}

// Token returns a valid access token, fetching a new one when the cached token
// is missing or within refreshMargin of expiry.
func (s *Source) Token(ctx context.Context) (string, error) {
	// A static token has no expiry this side can observe and nothing to
	// refresh, so it short-circuits before the mutex and the HTTP client.
	if s.cfg.StaticToken != "" {
		return s.cfg.StaticToken, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Holding the mutex across the HTTP call is intentional. Under a
	// thundering herd this serialises refreshes into one request rather than
	// letting every in-flight RPC mint its own token, and the call is already
	// bounded by the client's timeout.
	if s.token != "" && s.now().Before(s.expiresAt.Add(-refreshMargin)) {
		return s.token, nil
	}

	tok, ttl, err := s.fetch(ctx)
	if err != nil {
		// The stale token is deliberately NOT returned as a fallback. It is
		// either expired or about to be, and a server rejecting it produces a
		// confusing Unauthenticated instead of the plain "could not reach the
		// identity provider" the operator needs to see.
		return "", err
	}
	s.token, s.expiresAt = tok, s.now().Add(ttl)
	return tok, nil
}

func (s *Source) fetch(ctx context.Context) (string, time.Duration, error) {
	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	form.Set("client_id", s.cfg.ClientID)
	if s.cfg.ClientSecret != "" {
		form.Set("client_secret", s.cfg.ClientSecret)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.TokenURL,
		strings.NewReader(form.Encode()))
	if err != nil {
		return "", 0, fmt.Errorf("servicetoken: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := s.client.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("servicetoken: token request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Capped: an error page from a misconfigured proxy can be arbitrarily
	// large, and none of it belongs in a log line.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		return "", 0, fmt.Errorf("servicetoken: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		// The body is not included. A token endpoint's error body can echo the
		// request, and this request carries the client secret.
		return "", 0, fmt.Errorf("servicetoken: token endpoint returned %d", resp.StatusCode)
	}

	var parsed struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", 0, fmt.Errorf("servicetoken: decode response: %w", err)
	}
	if parsed.AccessToken == "" {
		return "", 0, errors.New("servicetoken: token endpoint returned no access_token")
	}

	ttl := time.Duration(parsed.ExpiresIn) * time.Second
	if ttl <= 0 {
		// A provider that omits expires_in gets a conservative assumption
		// rather than a token cached forever.
		ttl = 5 * time.Minute
	}
	return parsed.AccessToken, ttl, nil
}

// GetRequestMetadata satisfies credentials.PerRPCCredentials.
func (s *Source) GetRequestMetadata(ctx context.Context, _ ...string) (map[string]string, error) {
	tok, err := s.Token(ctx)
	if err != nil {
		return nil, err
	}
	return map[string]string{"authorization": "Bearer " + tok}, nil
}

// RequireTransportSecurity satisfies credentials.PerRPCCredentials.
//
// When true, gRPC refuses to attach the credential to an insecure connection
// rather than leaking it -- which is the behaviour to want for a mesh-wide
// bearer token.
func (s *Source) RequireTransportSecurity() bool { return s.cfg.RequireTLS }
