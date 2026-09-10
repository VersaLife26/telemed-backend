package user

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"telemed/internal/platform/middleware"
)

// A service token is what lets notification-service resolve a recipient when
// there is no OAuth identity provider on the mesh path. It is only useful if
// the RECEIVING side accepts it unchanged, so these tests assert the claims
// user-service's gRPC interceptor actually reads.
//
// They deliberately do NOT go through TokenIssuer.Verify. That is the verifier
// for USER tokens and it requires telemed_user_id -- correctly, because a user
// token without a user is meaningless. A service token has no user, and the
// verifier on the mesh path is middleware.Authenticator, which parses
// telemed_user_id best-effort and leaves Principal.UserID zero when it is
// absent. Asserting on the decoded claims tests that contract without standing
// up a JWKS endpoint to build an Authenticator against.
//
// newTestIssuer lives in token_test.go.

// serviceTokenClaims decodes the payload segment without verifying. The
// signature is exercised by TestIssueServiceTokenIsSignedWithTheAdvertisedKeyID
// and by every verifier on the platform; what matters here is the content.
func serviceTokenClaims(t *testing.T, token string) map[string]any {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("not a JWT: %q", token)
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	var claims map[string]any
	if err := json.Unmarshal(raw, &claims); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	return claims
}

func TestIssueServiceTokenAssertsTheServiceRole(t *testing.T) {
	ti := newTestIssuer(t)

	token, expiresAt, err := ti.IssueServiceToken("notification-service", 24*time.Hour)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if !expiresAt.After(time.Now().UTC().Add(23 * time.Hour)) {
		t.Errorf("expiresAt = %s, want ~24h out", expiresAt)
	}

	c := serviceTokenClaims(t, token)

	// Exactly one role, and it is the one RequireRole(RoleService) checks. A
	// token carrying anything else here is a token that authenticates and then
	// fails every gRPC method with PermissionDenied.
	realm, ok := c["realm_access"].(map[string]any)
	if !ok {
		t.Fatalf("no realm_access claim: %v", c)
	}
	roles, ok := realm["roles"].([]any)
	if !ok || len(roles) != 1 {
		t.Fatalf("realm_access.roles = %v, want exactly one role", realm["roles"])
	}
	if roles[0] != string(middleware.RoleService) {
		t.Errorf("role = %v, want %q", roles[0], middleware.RoleService)
	}

	// The issuer and audience must be the platform's own, or the key set
	// bound to this issuer is not the one a verifier will select.
	if c["iss"] != "telemed-user-service" {
		t.Errorf("iss = %v, want telemed-user-service", c["iss"])
	}
	aud, ok := c["aud"].([]any)
	if !ok || len(aud) != 1 || aud[0] != "telemed-api" {
		t.Errorf("aud = %v, want [telemed-api]", c["aud"])
	}
}

func TestIssueServiceTokenCarriesNoUserIdentity(t *testing.T) {
	ti := newTestIssuer(t)

	token, _, err := ti.IssueServiceToken("notification-service", time.Hour)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	c := serviceTokenClaims(t, token)

	// The subject is the service. A UUID here would put a user id that
	// resolves to nobody into every audit entry the mesh call produces.
	if c["sub"] != "notification-service" {
		t.Errorf("sub = %v, want notification-service", c["sub"])
	}
	// middleware.Authenticator leaves Principal.UserID zero when
	// telemed_user_id is absent or unparseable, which is what we want: a
	// service is not a user. What must never happen is a NON-empty value that
	// some handler then treats as a real account.
	if id, present := c["telemed_user_id"]; present && id != "" {
		t.Errorf("telemed_user_id = %v, want absent or empty", id)
	}
	for _, personal := range []string{"phone_number", "email"} {
		if v, present := c[personal]; present && v != "" {
			t.Errorf("service token carries %s = %v", personal, v)
		}
	}
}

func TestIssueServiceTokenRejectsNonsense(t *testing.T) {
	ti := newTestIssuer(t)

	if _, _, err := ti.IssueServiceToken("", time.Hour); err == nil {
		t.Error("expected an error for an empty service name")
	}
	// A zero or negative TTL would mint a token that is already expired, and
	// the failure would surface as an intermittent Unauthenticated in a
	// completely different service.
	for _, ttl := range []time.Duration{0, -time.Hour} {
		if _, _, err := ti.IssueServiceToken("notification-service", ttl); err == nil {
			t.Errorf("expected an error for ttl %s", ttl)
		}
	}
}

func TestIssueServiceTokenIsSignedWithTheAdvertisedKeyID(t *testing.T) {
	ti := newTestIssuer(t)

	token, _, err := ti.IssueServiceToken("notification-service", time.Hour)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("not a JWT: %q", token)
	}
	rawHeader, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatalf("decode header: %v", err)
	}
	var header map[string]any
	if err := json.Unmarshal(rawHeader, &header); err != nil {
		t.Fatalf("unmarshal header: %v", err)
	}

	// The kid must match the JWKS this service publishes, or a verifier that
	// selects keys by kid -- which is every verifier on this platform -- finds
	// no key and rejects a perfectly good token.
	jwks := ti.PublicJWKS()
	if len(jwks.Keys) == 0 {
		t.Fatal("issuer publishes no keys")
	}
	if header["kid"] != jwks.Keys[0].Kid {
		t.Errorf("token kid = %v, published kid = %q", header["kid"], jwks.Keys[0].Kid)
	}
}
