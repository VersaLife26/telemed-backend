package user

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// AccessTokenTTL and RefreshTokenTTL are the platform's session lifetimes
// (SDD section 8.1 / 12): short-lived access token, long-lived refresh token
// with rotation.
const (
	AccessTokenTTL  = 15 * time.Minute
	RefreshTokenTTL = 7 * 24 * time.Hour

	refreshTokenBytes = 32 // 256 bits of entropy, base64url encoded
)

// claims is the JWT payload this service mints. The shape intentionally
// matches what internal/platform/middleware.Authenticator expects to parse
// (realm_access.roles, telemed_user_id, phone_number, email, locale), so a
// token issued here verifies cleanly through the shared middleware on any
// other service, once that service's KEYCLOAK_JWKS_URL points at this
// service's /.well-known/jwks.json. See README "50-Year Maintenance" for why
// user-service, not Keycloak, is the signer on the OTP path.
type claims struct {
	jwt.RegisteredClaims
	RealmAccess struct {
		Roles []string `json:"roles"`
	} `json:"realm_access"`
	PreferredUsername string `json:"preferred_username"`
	PhoneNumber       string `json:"phone_number"`
	Email             string `json:"email"`
	Locale            string `json:"locale"`
	UserID            string `json:"telemed_user_id"`
}

// TokenIssuer signs and verifies the platform's access tokens with an RSA
// keypair owned by this service.
type TokenIssuer struct {
	privateKey *rsa.PrivateKey
	keyID      string
	issuer     string
	audience   string
}

// NewTokenIssuer builds an issuer from a PEM-encoded PKCS#1 or PKCS#8 RSA
// private key. keyID becomes the JWKS "kid" so a future key rotation can run
// two keys side by side during the overlap window.
func NewTokenIssuer(pemKey, keyID, issuer, audience string) (*TokenIssuer, error) {
	key, err := parseRSAPrivateKeyPEM(pemKey)
	if err != nil {
		return nil, err
	}
	return &TokenIssuer{privateKey: key, keyID: keyID, issuer: issuer, audience: audience}, nil
}

// GenerateEphemeralKeyPEM creates a fresh RSA-2048 key and returns it PEM
// encoded. Used only when no signing key is configured, which is refused
// outright in prod (see cmd/server/main.go) and is what lets a developer run
// the service with zero setup.
func GenerateEphemeralKeyPEM() (string, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return "", fmt.Errorf("user: generate ephemeral key: %w", err)
	}
	der := x509.MarshalPKCS1PrivateKey(key)
	block := &pem.Block{Type: "RSA PRIVATE KEY", Bytes: der}
	return string(pem.EncodeToMemory(block)), nil
}

func parseRSAPrivateKeyPEM(raw string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(raw))
	if block == nil {
		return nil, fmt.Errorf("user: no PEM block found in signing key")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("user: parse signing key: %w", err)
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("user: signing key is not RSA")
	}
	return key, nil
}

// IssueAccessToken mints a 15-minute RS256 access token for u.
func (t *TokenIssuer) IssueAccessToken(u User) (string, time.Time, error) {
	now := time.Now().UTC()
	expiresAt := now.Add(AccessTokenTTL)

	c := claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   u.ID.String(),
			Issuer:    t.issuer,
			Audience:  jwt.ClaimStrings{t.audience},
			ExpiresAt: jwt.NewNumericDate(expiresAt),
			IssuedAt:  jwt.NewNumericDate(now),
			ID:        uuid.NewString(),
		},
		PreferredUsername: preferredUsername(u),
		PhoneNumber:       u.Phone,
		Locale:            string(u.Language),
		UserID:            u.ID.String(),
	}
	c.RealmAccess.Roles = []string{string(u.Role)}
	if u.Email != nil {
		c.Email = *u.Email
	}

	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, c)
	tok.Header["kid"] = t.keyID

	signed, err := tok.SignedString(t.privateKey)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("user: sign access token: %w", err)
	}
	return signed, expiresAt, nil
}

func preferredUsername(u User) string {
	if u.Phone != "" {
		return u.Phone
	}
	if u.Email != nil && *u.Email != "" {
		return *u.Email
	}
	return u.ID.String()
}

// VerifiedPrincipal is the authenticated caller, extracted from a token this
// issuer signed.
type VerifiedPrincipal struct {
	UserID uuid.UUID
	Role   Role
	Phone  string
}

// Verify parses and validates a bearer token against this issuer's own
// public key, entirely in-process. user-service is the one service in the
// platform that must authenticate requests against tokens it minted itself,
// before it has (or can have) a running JWKS HTTP endpoint to fetch from --
// so, unlike every other service's use of middleware.Authenticator, this
// verification never leaves the process. Other services that verify these
// tokens do so via the standard JWKS-fetching middleware.Authenticator,
// pointed at GET /.well-known/jwks.json (see PublicJWKS).
func (t *TokenIssuer) Verify(raw string) (VerifiedPrincipal, error) {
	var c claims
	tok, err := jwt.ParseWithClaims(raw, &c, func(*jwt.Token) (any, error) {
		return &t.privateKey.PublicKey, nil
	},
		jwt.WithValidMethods([]string{"RS256"}),
		jwt.WithExpirationRequired(),
		jwt.WithLeeway(30*time.Second),
		jwt.WithIssuer(t.issuer),
		jwt.WithAudience(t.audience),
	)
	if err != nil {
		return VerifiedPrincipal{}, fmt.Errorf("user: verify token: %w", err)
	}
	if !tok.Valid {
		return VerifiedPrincipal{}, fmt.Errorf("user: token rejected")
	}

	id, err := uuid.Parse(c.UserID)
	if err != nil {
		return VerifiedPrincipal{}, fmt.Errorf("user: token missing telemed_user_id: %w", err)
	}
	var role Role
	if len(c.RealmAccess.Roles) > 0 {
		role = Role(c.RealmAccess.Roles[0])
	}
	return VerifiedPrincipal{UserID: id, Role: role, Phone: c.PhoneNumber}, nil
}

// JWK is the RFC 7517 JSON representation of the RSA public key, served at
// GET /.well-known/jwks.json so any service can verify tokens this issuer
// signs via the platform's standard JWKS-based Authenticator.
type JWK struct {
	Kty string `json:"kty"`
	Use string `json:"use"`
	Kid string `json:"kid"`
	Alg string `json:"alg"`
	N   string `json:"n"`
	E   string `json:"e"`
}

// JWKS is the RFC 7517 key set document.
type JWKS struct {
	Keys []JWK `json:"keys"`
}

// PublicJWKS returns the JWK set containing this issuer's current public key.
func (t *TokenIssuer) PublicJWKS() JWKS {
	pub := t.privateKey.PublicKey
	return JWKS{Keys: []JWK{{
		Kty: "RSA",
		Use: "sig",
		Kid: t.keyID,
		Alg: "RS256",
		N:   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		E:   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
	}}}
}

// NewOpaqueRefreshToken returns a cryptographically random refresh token
// alongside the SHA-256 hash that gets persisted. The raw value is returned
// to the client exactly once and never stored.
func NewOpaqueRefreshToken() (raw, hash string, err error) {
	buf := make([]byte, refreshTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", "", fmt.Errorf("user: generate refresh token: %w", err)
	}
	raw = base64.RawURLEncoding.EncodeToString(buf)
	return raw, HashRefreshToken(raw), nil
}

// HashRefreshToken returns the SHA-256 hex digest of a raw refresh token, the
// only form ever written to the database.
func HashRefreshToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}
