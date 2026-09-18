package user

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func newTestIssuer(t *testing.T) *TokenIssuer {
	t.Helper()
	pemKey, err := GenerateEphemeralKeyPEM()
	if err != nil {
		t.Fatalf("GenerateEphemeralKeyPEM: %v", err)
	}
	issuer, err := NewTokenIssuer(pemKey, "test-key-1", "telemed-user-service", "telemed-api")
	if err != nil {
		t.Fatalf("NewTokenIssuer: %v", err)
	}
	return issuer
}

func TestTokenIssuer_IssueAndVerifyRoundtrip(t *testing.T) {
	issuer := newTestIssuer(t)
	email := "patient@example.lk"
	u := User{
		ID: uuid.New(), Phone: "+94771234567", Email: &email,
		Name: "Nimal Perera", Language: LanguageSinhala, Role: RolePatient,
	}

	token, expiresAt, err := issuer.IssueAccessToken(u)
	if err != nil {
		t.Fatalf("IssueAccessToken: %v", err)
	}
	if token == "" {
		t.Fatal("issued an empty token")
	}
	wantExpiry := time.Now().Add(AccessTokenTTL)
	if diff := wantExpiry.Sub(expiresAt).Abs(); diff > 5*time.Second {
		t.Errorf("expiresAt = %v, want close to %v", expiresAt, wantExpiry)
	}

	principal, err := issuer.Verify(token)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if principal.UserID != u.ID {
		t.Errorf("UserID = %v, want %v", principal.UserID, u.ID)
	}
	if principal.Role != RolePatient {
		t.Errorf("Role = %v, want %v", principal.Role, RolePatient)
	}
	if principal.Phone != u.Phone {
		t.Errorf("Phone = %v, want %v", principal.Phone, u.Phone)
	}
}

func TestTokenIssuer_DoctorIDClaim(t *testing.T) {
	issuer := newTestIssuer(t)
	doctorID := uuid.New()
	u := User{
		ID:    uuid.New(),
		Phone: "+94771234567",
		Name:  "Dr. Nimal",
		Role:  RoleDoctor,
	}

	// 1. Without doctorID: telemed_doctor_id is not set
	tokNoDoc, _, err := issuer.IssueAccessToken(u)
	if err != nil {
		t.Fatalf("IssueAccessToken: %v", err)
	}
	pNoDoc, err := issuer.Verify(tokNoDoc)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if pNoDoc.DoctorID != uuid.Nil {
		t.Errorf("DoctorID = %v, want nil", pNoDoc.DoctorID)
	}

	// 2. With doctorID: telemed_doctor_id is set and verified
	tokWithDoc, _, err := issuer.IssueAccessToken(u, doctorID)
	if err != nil {
		t.Fatalf("IssueAccessToken: %v", err)
	}
	pWithDoc, err := issuer.Verify(tokWithDoc)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if pWithDoc.DoctorID != doctorID {
		t.Errorf("DoctorID = %v, want %v", pWithDoc.DoctorID, doctorID)
	}
}

func TestTokenIssuer_EmailOnlyPreferredUsername(t *testing.T) {
	issuer := newTestIssuer(t)
	email := "ada@example.lk"
	u := User{ID: uuid.New(), Email: &email, Role: RolePatient, Language: LanguageEnglish}

	token, _, err := issuer.IssueAccessToken(u)
	if err != nil {
		t.Fatalf("IssueAccessToken: %v", err)
	}
	if _, err := issuer.Verify(token); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

func TestTokenIssuer_RejectsTamperedToken(t *testing.T) {
	issuer := newTestIssuer(t)
	u := User{ID: uuid.New(), Phone: "+94771234567", Role: RolePatient, Language: LanguageEnglish}

	token, _, err := issuer.IssueAccessToken(u)
	if err != nil {
		t.Fatalf("IssueAccessToken: %v", err)
	}

	// Flip a character in the middle of the signature segment. The very
	// last character of an unpadded base64url string can encode unused
	// padding bits that a bit flip does not actually change once decoded --
	// a middle character always corresponds to real signature bytes.
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d segments, want 3", len(parts))
	}
	tampered := parts[0] + "." + parts[1] + "." + flipMiddleChar(parts[2])

	if _, err := issuer.Verify(tampered); err == nil {
		t.Error("Verify accepted a token with a tampered signature")
	}
}

func TestTokenIssuer_RejectsForeignKey(t *testing.T) {
	issuerA := newTestIssuer(t)
	issuerB := newTestIssuer(t) // distinct RSA keypair, same issuer/audience strings

	u := User{ID: uuid.New(), Phone: "+94771234567", Role: RolePatient, Language: LanguageEnglish}
	token, _, err := issuerA.IssueAccessToken(u)
	if err != nil {
		t.Fatalf("IssueAccessToken: %v", err)
	}

	if _, err := issuerB.Verify(token); err == nil {
		t.Error("Verify accepted a token signed by a different key")
	}
}

func TestTokenIssuer_RejectsWrongAudience(t *testing.T) {
	pemKey, err := GenerateEphemeralKeyPEM()
	if err != nil {
		t.Fatalf("GenerateEphemeralKeyPEM: %v", err)
	}
	issuer, err := NewTokenIssuer(pemKey, "k1", "telemed-user-service", "telemed-api")
	if err != nil {
		t.Fatalf("NewTokenIssuer: %v", err)
	}
	verifier, err := NewTokenIssuer(pemKey, "k1", "telemed-user-service", "some-other-audience")
	if err != nil {
		t.Fatalf("NewTokenIssuer: %v", err)
	}

	u := User{ID: uuid.New(), Phone: "+94771234567", Role: RolePatient, Language: LanguageEnglish}
	token, _, err := issuer.IssueAccessToken(u)
	if err != nil {
		t.Fatalf("IssueAccessToken: %v", err)
	}
	if _, err := verifier.Verify(token); err == nil {
		t.Error("Verify accepted a token whose audience does not match")
	}
}

func TestTokenIssuer_PublicJWKSMatchesKeyID(t *testing.T) {
	issuer := newTestIssuer(t)
	jwks := issuer.PublicJWKS()
	if len(jwks.Keys) != 1 {
		t.Fatalf("PublicJWKS returned %d keys, want 1", len(jwks.Keys))
	}
	if jwks.Keys[0].Kid != "test-key-1" {
		t.Errorf("kid = %q, want %q", jwks.Keys[0].Kid, "test-key-1")
	}
	if jwks.Keys[0].Kty != "RSA" || jwks.Keys[0].Alg != "RS256" {
		t.Errorf("unexpected key type/alg: %+v", jwks.Keys[0])
	}
}

func TestNewOpaqueRefreshToken_HashIsDeterministicAndDistinctFromRaw(t *testing.T) {
	raw, hash, err := NewOpaqueRefreshToken()
	if err != nil {
		t.Fatalf("NewOpaqueRefreshToken: %v", err)
	}
	if raw == hash {
		t.Fatal("hash equals raw token -- refresh tokens must never be stored unhashed")
	}
	if HashRefreshToken(raw) != hash {
		t.Error("HashRefreshToken(raw) does not match the hash returned alongside it")
	}

	raw2, _, err := NewOpaqueRefreshToken()
	if err != nil {
		t.Fatalf("NewOpaqueRefreshToken: %v", err)
	}
	if raw == raw2 {
		t.Error("two calls produced the same raw token")
	}
}

func flipMiddleChar(s string) string {
	if len(s) < 2 {
		return s
	}
	b := []byte(s)
	i := len(b) / 2
	if b[i] == 'A' {
		b[i] = 'B'
	} else {
		b[i] = 'A'
	}
	return string(b)
}
