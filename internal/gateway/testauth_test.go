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

// testIdentity stands up a real Authenticator against a real (httptest)
// JWKS endpoint, and returns a function that mints tokens the Authenticator
// will actually verify. This exercises the genuine RS256 signature-check
// path rather than faking PrincipalFrom, which is what makes the router
// integration tests below trustworthy for the header-forgery guarantee.
type testIdentity struct {
	auth     *platmw.Authenticator
	jwksSrv  *httptest.Server
	key      *rsa.PrivateKey
	kid      string
	issuer   string
	audience string
}

func newTestIdentity(t *testing.T) *testIdentity {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}
	kid := "test-key-1"

	jwk := map[string]any{
		"kty": "RSA",
		"kid": kid,
		"use": "sig",
		"alg": "RS256",
		"n":   base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
		"e":   base64.RawURLEncoding.EncodeToString(bigEndianBytes(key.E)),
	}
	jwks := map[string]any{"keys": []any{jwk}}
	body, err := json.Marshal(jwks)
	if err != nil {
		t.Fatalf("marshal jwks: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	const issuer = "https://test-issuer.invalid/realms/telemedicine"
	const audience = "telemed-api"

	auth, err := platmw.NewSingleIssuerAuthenticator(context.Background(), srv.URL, issuer, audience)
	if err != nil {
		t.Fatalf("NewAuthenticator: %v", err)
	}

	return &testIdentity{auth: auth, jwksSrv: srv, key: key, kid: kid, issuer: issuer, audience: audience}
}

// sign mints a real, verifiable RS256 access token for userID carrying
// roles. The gateway's Authenticator.Verify will accept it exactly as it
// would accept a genuine Keycloak token.
func (ti *testIdentity) sign(t *testing.T, userID uuid.UUID, roles []platmw.Role) string {
	t.Helper()

	roleStrs := make([]string, len(roles))
	for i, r := range roles {
		roleStrs[i] = string(r)
	}

	claims := jwt.MapClaims{
		"sub":             userID.String(),
		"iss":             ti.issuer,
		"aud":             ti.audience,
		"iat":             time.Now().Unix(),
		"exp":             time.Now().Add(15 * time.Minute).Unix(),
		"telemed_user_id": userID.String(),
		"realm_access":    map[string]any{"roles": roleStrs},
	}

	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = ti.kid

	signed, err := tok.SignedString(ti.key)
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return signed
}

func bigEndianBytes(n int) []byte {
	if n == 0 {
		return []byte{0}
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte(n & 0xff)}, b...)
		n >>= 8
	}
	return b
}
