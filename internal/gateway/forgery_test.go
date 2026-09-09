package gateway

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	platmw "telemed/internal/platform/middleware"
)

// ---------------------------------------------------------------------------
// A two-issuer fixture that is the real thing, not a mock.
//
// Two RSA keypairs, two live JWKS endpoints, and ONE Authenticator configured
// exactly the way cmd/server/main.go configures it in production: IssuerKeys
// binding each issuer to the single key set permitted to sign for it. Every
// assertion below therefore exercises the genuine RS256 verification path.
// ---------------------------------------------------------------------------

// signer mints real tokens with one key, and can stamp any "iss" it likes --
// which is precisely the attacker capability ADR-012 is about.
type signer struct {
	key    *rsa.PrivateKey
	kid    string
	issuer string
	jwks   string
}

func newSigner(t *testing.T, kid, issuer string) *signer {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}
	body, err := json.Marshal(map[string]any{"keys": []any{map[string]any{
		"kty": "RSA", "kid": kid, "use": "sig", "alg": "RS256",
		"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
		"e": base64.RawURLEncoding.EncodeToString(bigEndianBytes(key.E)),
	}}})
	if err != nil {
		t.Fatalf("marshal jwks: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	return &signer{key: key, kid: kid, issuer: issuer, jwks: srv.URL}
}

// mint signs a token with this signer's key. claimedIssuer, when non-empty,
// overrides the "iss" claim WITHOUT changing the signing key: the forgery.
func (s *signer) mint(t *testing.T, userID uuid.UUID, roles []platmw.Role, claimedIssuer string) string {
	t.Helper()

	iss := s.issuer
	if claimedIssuer != "" {
		iss = claimedIssuer
	}
	roleStrs := make([]string, len(roles))
	for i, r := range roles {
		roleStrs[i] = string(r)
	}
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

const (
	userIssuer = "telemed-user-service"
	kcIssuer   = "https://kc.invalid/realms/telemedicine"
)

// dualIssuerFixture is the platform's real auth shape: user-service signs
// patient and doctor tokens, Keycloak signs admin tokens, one Authenticator
// accepts both while binding each to its own key set.
type dualIssuerFixture struct {
	user     *signer
	keycloak *signer
	identity *testIdentity // carries the shared dual-issuer Authenticator
	cfg      Config
}

func newDualIssuerFixture(t *testing.T) *dualIssuerFixture {
	t.Helper()

	user := newSigner(t, "user-service-1", userIssuer)
	kc := newSigner(t, "keycloak-1", kcIssuer)

	cfg := testConfig()
	cfg.UserIssuer, cfg.UserJWKSURL = user.issuer, user.jwks
	cfg.KeycloakIssuer, cfg.KeycloakJWKSURL = kc.issuer, kc.jwks
	cfg.KeycloakAudience = "telemed-api"
	cfg.AdminIPAllowlist = []string{"192.0.2.0/24"}

	auth, err := platmw.NewAuthenticatorFrom(context.Background(), platmw.AuthConfig{
		IssuerKeys: cfg.IssuerKeys(),
		Issuers:    cfg.TrustedIssuers(),
		Audience:   "telemed-api",
	})
	if err != nil {
		t.Fatalf("NewAuthenticatorFrom: %v", err)
	}

	return &dualIssuerFixture{
		user: user, keycloak: kc, cfg: cfg,
		// buildTestRouter only reads .auth off the identity.
		identity: &testIdentity{auth: auth, key: kc.key, kid: kc.kid, issuer: kc.issuer, audience: "telemed-api"},
	}
}

// adminIPRequest builds a request that already satisfies the admin IP
// allowlist, so a 403 in these tests is never the allowlist answering when the
// test meant to exercise something else.
func adminIPRequest(t *testing.T, method, path string) *http.Request {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), method, path, http.NoBody)
	req.RemoteAddr = "192.0.2.10:5555"
	return req
}

// ---------------------------------------------------------------------------
// ADR-012: the forgery, attempted for real, end to end through the gateway.
// ---------------------------------------------------------------------------

// TestGateway_AdminTokenForgeryFailsEndToEnd is the test the brief asks for.
//
// ADR-012 records a live vulnerability: with both JWKS merged into one key
// set, user-service -- which signs every patient and doctor token -- could
// mint a token claiming "iss: keycloak" carrying super_admin, and it was
// accepted. The fix inverted the lookup so the claimed issuer SELECTS the key
// set rather than being compared to an allowlist afterwards.
//
// dualissuer_test.go already asserts that at the Authenticator.Verify level.
// This asserts it where it actually matters: a real HTTP request, through the
// real chi router, through the real route table, at a real admin route, with a
// real upstream watching to confirm it was never reached. A unit test proving
// Verify rejects a token is worth less than a request proving nothing behind
// the gateway ever saw it.
func TestGateway_AdminTokenForgeryFailsEndToEnd(t *testing.T) {
	f := newDualIssuerFixture(t)
	upSrv, echo := newEchoUpstream(t)
	r, _, _ := buildTestRouter(t, f.identity, f.cfg, upSrv.URL, nil)

	attacker := uuid.New()

	cases := []struct {
		name          string
		token         string
		wantStatus    int
		wantUpstream  bool
		whatItProves  string
		alsoAssertHdr bool
	}{
		{
			name:         "user-service key signing iss:keycloak with super_admin",
			token:        f.user.mint(t, attacker, []platmw.Role{platmw.RoleSuperAdmin}, kcIssuer),
			wantStatus:   http.StatusUnauthorized,
			whatItProves: "ADR-012: the claimed issuer selects the key set, so a key that is not Keycloak's cannot speak for Keycloak",
		},
		{
			name:         "user-service key signing iss:keycloak with every admin role at once",
			token:        f.user.mint(t, attacker, platmw.AdminRoles, kcIssuer),
			wantStatus:   http.StatusUnauthorized,
			whatItProves: "the rejection is about the signature/issuer binding, not about which role was asked for",
		},
		{
			name:       "keycloak key signing iss:telemed-user-service with super_admin",
			token:      f.keycloak.mint(t, attacker, []platmw.Role{platmw.RoleSuperAdmin}, userIssuer),
			wantStatus: http.StatusUnauthorized,
			// 401, not 403: the binding is symmetric, so this never gets far
			// enough to be a role question. Keycloak's key is not in
			// user-service's key set, so the signature check fails outright.
			whatItProves: "the issuer-to-key binding runs in both directions; Keycloak's key cannot speak for user-service either",
		},
		{
			name:         "unlisted issuer with a valid signature",
			token:        f.user.mint(t, attacker, []platmw.Role{platmw.RoleSuperAdmin}, "https://attacker.invalid/"),
			wantStatus:   http.StatusUnauthorized,
			whatItProves: "no key set is configured for an unknown issuer, so there is nothing to verify against",
		},
		{
			name:         "no token at all",
			token:        "",
			wantStatus:   http.StatusUnauthorized,
			whatItProves: "the admin surface is not reachable anonymously",
		},
		{
			name:         "genuine patient token from the patient issuer",
			token:        f.user.mint(t, attacker, []platmw.Role{platmw.RolePatient}, ""),
			wantStatus:   http.StatusForbidden,
			whatItProves: "a valid non-admin token authenticates but does not authorize",
		},
		{
			name:          "genuine admin token from Keycloak",
			token:         f.keycloak.mint(t, uuid.New(), []platmw.Role{platmw.RoleSuperAdmin}, ""),
			wantStatus:    http.StatusOK,
			wantUpstream:  true,
			whatItProves:  "the control is not simply 'deny everything': the legitimate path still works",
			alsoAssertHdr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			echo.reset()

			req := adminIPRequest(t, http.MethodGet, "/api/v1/admin/doctors/pending")
			if tc.token != "" {
				req.Header.Set("Authorization", "Bearer "+tc.token)
			}
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (%s)\nbody: %s",
					rec.Code, tc.wantStatus, tc.whatItProves, rec.Body.String())
			}
			if reached := echo.headers() != nil; reached != tc.wantUpstream {
				t.Fatalf("upstream reached = %v, want %v -- %s", reached, tc.wantUpstream, tc.whatItProves)
			}
			if tc.alsoAssertHdr {
				if got := echo.headers().Get(HeaderRoles); !strings.Contains(got, "super_admin") {
					t.Fatalf("upstream saw X-Telemed-Roles=%q, want it to contain super_admin", got)
				}
			}
		})
	}
}

// TestGateway_PatientIssuerCannotAssertAnAdminRole covers the half of the
// two-issuer model that ADR-012 did NOT close.
//
// Binding an issuer to its keys stops user-service claiming to BE Keycloak.
// It does not stop user-service minting an entirely honest token -- correct
// "iss", correct key, verifying cleanly -- that asserts
// realm_access.roles = ["super_admin"]. RequireRole reads the role and admits
// it, because until RequireTokenIssuer existed nothing downstream could tell
// which issuer had produced it: middleware.Principal did not carry the issuer
// at all.
//
// That is not a theoretical concern. telemed-user-service copies users.role
// straight into the claim (internal/user/token.go:120), and the CHECK
// constraint on that column (migrations/000002_users.up.sql:21) accepts
// 'admin', 'super_admin', 'ops', 'finance' and 'support'. One UPDATE, or one
// future "promote to admin" feature, and phone-OTP login mints admin tokens --
// bypassing the SAML SSO and enforced 2FA that are the entire reason ADR-010
// keeps Keycloak as a hard dependency for staff.
//
// Before RequireTokenIssuer every case below returned 200.
func TestGateway_PatientIssuerCannotAssertAnAdminRole(t *testing.T) {
	f := newDualIssuerFixture(t)
	upSrv, echo := newEchoUpstream(t)
	r, _, _ := buildTestRouter(t, f.identity, f.cfg, upSrv.URL, nil)

	for _, role := range platmw.AdminRoles {
		t.Run(string(role), func(t *testing.T) {
			echo.reset()

			req := adminIPRequest(t, http.MethodGet, "/api/v1/admin/doctors/pending")
			req.Header.Set("Authorization", "Bearer "+f.user.mint(t, uuid.New(), []platmw.Role{role}, ""))
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)

			if rec.Code != http.StatusForbidden {
				t.Fatalf("a %q role asserted by the patient issuer got status %d, want 403.\n"+
					"The admin surface must accept role claims only from %s, the issuer that "+
					"enforces SSO and 2FA.\nbody: %s", role, rec.Code, kcIssuer, rec.Body.String())
			}
			if echo.headers() != nil {
				t.Fatal("the request reached admin-service")
			}
		})
	}
}

// TestGateway_PayoutRunIsIssuerBoundAndIPRestricted pins the route-table fix.
// POST /api/v1/payouts/run triggers a real money movement and was on auth mode
// "authenticated": role-gated, but with no IP allowlist, no admin-origin
// rejection and no issuer binding -- weaker than GET /admin/analytics/revenue,
// which only reads numbers.
func TestGateway_PayoutRunIsIssuerBoundAndIPRestricted(t *testing.T) {
	f := newDualIssuerFixture(t)
	upSrv, echo := newEchoUpstream(t)
	r, _, _ := buildTestRouter(t, f.identity, f.cfg, upSrv.URL, nil)

	t.Run("finance role from the patient issuer is refused", func(t *testing.T) {
		echo.reset()
		req := adminIPRequest(t, http.MethodPost, "/api/v1/payouts/run")
		req.Header.Set("Authorization", "Bearer "+f.user.mint(t, uuid.New(), []platmw.Role{platmw.RoleFinance}, ""))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403; body: %s", rec.Code, rec.Body.String())
		}
		if echo.headers() != nil {
			t.Fatal("a payout batch was triggered by a token from the patient issuer")
		}
	})

	t.Run("off-allowlist source address is refused even with a genuine admin token", func(t *testing.T) {
		echo.reset()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/payouts/run", http.NoBody)
		req.RemoteAddr = "203.0.113.7:5555" // outside 192.0.2.0/24
		req.Header.Set("Authorization", "Bearer "+f.keycloak.mint(t, uuid.New(), []platmw.Role{platmw.RoleFinance}, ""))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403 from the admin IP allowlist; body: %s", rec.Code, rec.Body.String())
		}
		if echo.headers() != nil {
			t.Fatal("a payout batch was triggered from outside the admin network")
		}
	})

	t.Run("genuine finance token from the admin network still works", func(t *testing.T) {
		echo.reset()
		req := adminIPRequest(t, http.MethodPost, "/api/v1/payouts/run")
		req.Header.Set("Authorization", "Bearer "+f.keycloak.mint(t, uuid.New(), []platmw.Role{platmw.RoleFinance}, ""))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
		}
	})
}

// TestGateway_PatientTokenCannotReachDoctorOnlyRoutes walks every route in the
// live table whose role list is exactly {doctor} and drives a genuine patient
// token at it. This is the horizontal half of the role matrix and it is worth
// enumerating from the table rather than spot-checking, because the failure
// mode is a single route added later with the role list left off.
func TestGateway_PatientTokenCannotReachDoctorOnlyRoutes(t *testing.T) {
	f := newDualIssuerFixture(t)
	upSrv, echo := newEchoUpstream(t)
	r, _, _ := buildTestRouter(t, f.identity, f.cfg, upSrv.URL, nil)

	patient := f.user.mint(t, uuid.New(), []platmw.Role{platmw.RolePatient}, "")

	var checked int
	for _, rule := range loadTestRoutes(t) {
		if len(rule.Roles) != 1 || rule.Roles[0] != string(platmw.RoleDoctor) {
			continue
		}
		method := http.MethodGet
		if len(rule.Methods) > 0 {
			method = rule.Methods[0]
		}
		t.Run(rule.Name, func(t *testing.T) {
			echo.reset()
			req := httptest.NewRequestWithContext(t.Context(), method, concretePath(rule.Pattern), http.NoBody)
			req.RemoteAddr = "192.0.2.10:5555"
			req.Header.Set("Authorization", "Bearer "+patient)
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)

			if rec.Code != http.StatusForbidden {
				t.Fatalf("patient token on doctor-only route %s %s: status = %d, want 403; body: %s",
					method, rule.Pattern, rec.Code, rec.Body.String())
			}
			if echo.headers() != nil {
				t.Fatalf("patient token reached the upstream on doctor-only route %s", rule.Pattern)
			}
		})
		checked++
	}
	if checked == 0 {
		t.Fatal("no doctor-only routes found in the table -- the test is not asserting anything")
	}
}

// ---------------------------------------------------------------------------
// Header forgery: over the wire, not through the Go map.
//
// Setting r.Header["x-telemed-user-id"] directly on an httptest request would
// test a state that cannot occur: net/textproto canonicalizes every header
// name as it is read off a connection, so a server-side Header map never
// contains a non-canonical key. Testing it that way would manufacture a
// finding that does not exist in production.
//
// So these tests run the gateway behind a real listener and send the odd
// casings and duplicates over an actual connection -- HTTP/1.1 and HTTP/2 --
// which is the shape an attacker actually has.
// ---------------------------------------------------------------------------

// forgeryAttempts are the header spellings a client might reach for. Every one
// must arrive at the upstream carrying the verified identity and nothing else.
var forgeryAttempts = [][]string{
	{"X-Telemed-User-ID"},
	{"x-telemed-user-id"},
	{"X-TELEMED-USER-ID"},
	{"X-tElEmEd-uSeR-iD"},
	{"X-Telemed-User-Id"},
	{"X-Telemed-User-ID", "x-telemed-user-id"}, // duplicate, mixed case
	{"X-Telemed-User-ID", "X-Telemed-User-ID", "X-Telemed-User-ID"}, // triplicate
}

func runHeaderForgery(t *testing.T, gwURL string, client *http.Client, wantProto int,
	realID uuid.UUID, token string, echo *echoUpstream,
) {
	t.Helper()

	forged := uuid.New().String()

	for _, names := range forgeryAttempts {
		t.Run(strings.Join(names, "+"), func(t *testing.T) {
			echo.reset()

			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, gwURL+"/api/v1/users/me", http.NoBody)
			if err != nil {
				t.Fatalf("new request: %v", err)
			}
			req.Header.Set("Authorization", "Bearer "+token)
			// Assigning the map key directly is deliberate on the CLIENT side:
			// net/http writes request header names verbatim, so this is how a
			// literal lowercase or SHOUTING name gets onto the wire. The
			// server end still canonicalizes, which is exactly what is under
			// test.
			for _, n := range names {
				req.Header[n] = append(req.Header[n], forged)
			}
			// Roles too: a forged role header is the escalation, the user id
			// is only the impersonation.
			req.Header["X-Telemed-Roles"] = []string{"super_admin,admin,finance"}

			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}
			if resp.ProtoMajor != wantProto {
				t.Fatalf("response arrived over HTTP/%d, want HTTP/%d -- the test is not "+
					"exercising the protocol it claims to", resp.ProtoMajor, wantProto)
			}

			hdrs := echo.headers()
			if hdrs == nil {
				t.Fatal("upstream was never called")
			}
			if got := hdrs.Get(HeaderUserID); got != realID.String() {
				t.Fatalf("upstream saw %s = %q, want the verified id %q (forged id was %q)",
					HeaderUserID, got, realID, forged)
			}
			if vals := hdrs.Values(HeaderUserID); len(vals) != 1 {
				t.Fatalf("upstream saw %d values for %s (%v); a duplicated inbound header "+
					"must collapse to exactly the one the gateway injected", len(vals), HeaderUserID, vals)
			}
			if got := hdrs.Get(HeaderRoles); got != "patient" {
				t.Fatalf("upstream saw %s = %q, want %q -- a forged role header survived",
					HeaderRoles, got, "patient")
			}
		})
	}
}

// TestGateway_HeaderForgeryOverHTTP1 sends the forged headers over a real
// HTTP/1.1 connection in every casing and duplication a client can produce.
func TestGateway_HeaderForgeryOverHTTP1(t *testing.T) {
	f := newDualIssuerFixture(t)
	upSrv, echo := newEchoUpstream(t)
	r, _, _ := buildTestRouter(t, f.identity, f.cfg, upSrv.URL, nil)

	gw := httptest.NewServer(r)
	t.Cleanup(gw.Close)

	realID := uuid.New()
	runHeaderForgery(t, gw.URL, gw.Client(), 1, realID,
		f.user.mint(t, realID, []platmw.Role{platmw.RolePatient}, ""), echo)
}

// TestGateway_HeaderForgeryOverHTTP2 repeats it over a genuine HTTP/2
// connection.
//
// HTTP/2 is worth its own test rather than being assumed equivalent: h2
// carries header names lowercased on the wire by protocol requirement, and
// header handling is a different code path in net/http (golang.org/x/net/http2
// rather than net/textproto). "Header.Del canonicalizes, so casing cannot
// matter" is a statement about HTTP/1 parsing; this asserts it for h2 instead
// of extrapolating.
func TestGateway_HeaderForgeryOverHTTP2(t *testing.T) {
	f := newDualIssuerFixture(t)
	upSrv, echo := newEchoUpstream(t)
	r, _, _ := buildTestRouter(t, f.identity, f.cfg, upSrv.URL, nil)

	gw := httptest.NewUnstartedServer(r)
	gw.EnableHTTP2 = true
	gw.StartTLS()
	t.Cleanup(gw.Close)

	realID := uuid.New()
	runHeaderForgery(t, gw.URL, gw.Client(), 2, realID,
		f.user.mint(t, realID, []platmw.Role{platmw.RolePatient}, ""), echo)
}

// TestGateway_ForgedHeadersOnAPublicRouteAreAlsoStripped closes the gap a
// reader might assume away: StripForgedHeaders is composed on every rule
// including public ones, so an unauthenticated caller cannot inject an
// identity into a route that has no auth middleware to overwrite it. This is
// the case that would actually work if stripping were attached to the auth
// chain rather than to the rule.
func TestGateway_ForgedHeadersOnAPublicRouteAreAlsoStripped(t *testing.T) {
	f := newDualIssuerFixture(t)
	upSrv, echo := newEchoUpstream(t)
	r, _, _ := buildTestRouter(t, f.identity, f.cfg, upSrv.URL, nil)

	gw := httptest.NewServer(r)
	t.Cleanup(gw.Close)

	forged := uuid.New().String()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, gw.URL+"/api/v1/doctors", http.NoBody)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header["x-telemed-user-id"] = []string{forged}
	req.Header["X-Telemed-Roles"] = []string{"super_admin"}

	resp, err := gw.Client().Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	hdrs := echo.headers()
	if hdrs == nil {
		t.Fatal("upstream was never called")
	}
	if got := hdrs.Get(HeaderUserID); got != "" {
		t.Fatalf("an anonymous caller injected %s = %q into a public route", HeaderUserID, got)
	}
	if got := hdrs.Get(HeaderRoles); got != "" {
		t.Fatalf("an anonymous caller injected %s = %q into a public route", HeaderRoles, got)
	}
}
