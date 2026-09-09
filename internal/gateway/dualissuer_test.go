package gateway

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	platmw "telemed/internal/platform/middleware"
)

// testSigner is one token issuer: a private key, a real httptest JWKS
// endpoint serving its public half, and the "iss" claim it stamps.
type testSigner struct {
	key     *rsa.PrivateKey
	kid     string
	issuer  string
	jwksURL string
}

func newTestSigner(t *testing.T, kid, issuer string) *testSigner {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}
	body, err := json.Marshal(map[string]any{"keys": []any{map[string]any{
		"kty": "RSA",
		"kid": kid,
		"use": "sig",
		"alg": "RS256",
		"n":   base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
		"e":   base64.RawURLEncoding.EncodeToString(bigEndianBytes(key.E)),
	}}})
	if err != nil {
		t.Fatalf("marshal jwks: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	return &testSigner{key: key, kid: kid, issuer: issuer, jwksURL: srv.URL}
}

// sign mints a real RS256 token. issuerOverride, when non-empty, replaces the
// "iss" claim without changing the signing key -- which is exactly the attack
// the issuer allowlist exists to stop.
func (s *testSigner) sign(t *testing.T, roles []platmw.Role, issuerOverride string) string {
	t.Helper()

	iss := s.issuer
	if issuerOverride != "" {
		iss = issuerOverride
	}
	roleStrs := make([]string, len(roles))
	for i, r := range roles {
		roleStrs[i] = string(r)
	}
	userID := uuid.New()

	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"sub":             userID.String(),
		"iss":             iss,
		"aud":             "telemed-api",
		"iat":             time.Now().Unix(),
		"exp":             time.Now().Add(15 * time.Minute).Unix(),
		"telemed_user_id": userID.String(),
		"realm_access":    map[string]any{"roles": roleStrs},
	})
	tok.Header["kid"] = s.kid

	signed, err := tok.SignedString(s.key)
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return signed
}

// TestAuthenticator_AcceptsBothPlatformIssuers is the regression test for
// ADR-010 / INTEGRATION-FIXES item 8.
//
// The platform has two legitimate token issuers: telemed-user-service signs
// patient and doctor tokens (so phone-OTP login survives a Keycloak outage),
// and Keycloak signs admin tokens. This gateway was built against the
// single-issuer contract and would have rejected every patient and doctor
// token at the edge with a signature error -- the whole platform, dark, from
// one line of boot code.
//
// The fourth case is the reason the issuer allowlist is checked explicitly
// rather than left to the signature: both key sets are trusted, so a valid
// signature alone does not say which issuer minted the token.
func TestAuthenticator_AcceptsBothPlatformIssuers(t *testing.T) {
	userSigner := newTestSigner(t, "user-service-1", "telemed-user-service")
	kcSigner := newTestSigner(t, "keycloak-1", "https://kc.invalid/realms/telemedicine")

	cfg := Config{
		UserIssuer:      userSigner.issuer,
		UserJWKSURL:     userSigner.jwksURL,
		KeycloakIssuer:  kcSigner.issuer,
		KeycloakJWKSURL: kcSigner.jwksURL,
	}
	if got := len(cfg.JWKSURLs()); got != 2 {
		t.Fatalf("JWKSURLs() returned %d urls, want 2", got)
	}

	if got := len(cfg.IssuerKeys()); got != 2 {
		t.Fatalf("IssuerKeys() returned %d bindings, want 2", got)
	}

	auth, err := platmw.NewAuthenticatorFrom(context.Background(), platmw.AuthConfig{
		IssuerKeys: cfg.IssuerKeys(),
		Issuers:    cfg.TrustedIssuers(),
		Audience:   "telemed-api",
	})
	if err != nil {
		t.Fatalf("NewAuthenticatorFrom: %v", err)
	}

	cases := []struct {
		name    string
		token   string
		wantErr bool
	}{
		{
			name:  "patient token signed by user-service is accepted",
			token: userSigner.sign(t, []platmw.Role{platmw.RolePatient}, ""),
		},
		{
			name:  "admin token signed by keycloak is accepted",
			token: kcSigner.sign(t, []platmw.Role{platmw.RoleAdmin}, ""),
		},
		{
			name:    "a valid signature with an unlisted issuer is rejected",
			token:   userSigner.sign(t, []platmw.Role{platmw.RolePatient}, "https://attacker.invalid/"),
			wantErr: true,
		},
		{
			name:    "user-service key signing a token that claims to come from keycloak is rejected",
			token:   userSigner.sign(t, []platmw.Role{platmw.RoleSuperAdmin}, kcSigner.issuer),
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := auth.Verify(tc.token)
			if tc.wantErr && err == nil {
				t.Fatal("Verify accepted a token it must reject")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("Verify rejected a legitimate token: %v", err)
			}
		})
	}
}
