package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
)

// TestAdminRoleIsBoundToTheAdminIssuer pins the second half of ADR-012.
//
// The first half (binding an issuer to its key set) stops user-service signing
// a token that claims to be Keycloak. It does not stop user-service minting an
// entirely honest token -- correct issuer, correct signature -- that simply
// asserts super_admin. Since the admin surface is where SAML SSO and enforced
// 2FA live, that is a 2FA bypass rather than a mere role escalation.
//
// Today the only thing preventing it is that user-service happens never to
// write anything but 'patient' into users.role. That is an accident of the
// current code, not a control.
func TestAdminRoleIsBoundToTheAdminIssuer(t *testing.T) {
	const (
		keycloak = "http://localhost:8180/realms/telemedicine"
		userSvc  = "telemed-user-service"
	)

	SetAdminIssuer(keycloak)
	t.Cleanup(func() { SetAdminIssuer("") })

	cases := []struct {
		name     string
		issuer   string
		roles    []Role
		wantCode int
	}{
		{"keycloak super_admin is accepted", keycloak, []Role{RoleSuperAdmin}, http.StatusOK},
		{"keycloak finance is accepted", keycloak, []Role{RoleFinance}, http.StatusOK},
		{"user-service super_admin is refused", userSvc, []Role{RoleSuperAdmin}, http.StatusForbidden},
		{"user-service ops is refused", userSvc, []Role{RoleOps}, http.StatusForbidden},
		{"user-service support is refused", userSvc, []Role{RoleSupport}, http.StatusForbidden},
		{"user-service admin is refused", userSvc, []Role{RoleAdmin}, http.StatusForbidden},
		// A patient token reaching an admin route was already refused by the
		// role check; assert it still is, so this change cannot loosen it.
		{"user-service patient is refused", userSvc, []Role{RolePatient}, http.StatusForbidden},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := RequireRole(AdminRoles...)(http.HandlerFunc(
				func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))

			req := httptest.NewRequestWithContext(t.Context(),
				http.MethodGet, "/api/v1/admin/doctors/pending", http.NoBody)
			req = req.WithContext(WithPrincipal(req.Context(), Principal{
				UserID: uuid.New(),
				Roles:  tc.roles,
				Issuer: tc.issuer,
			}))

			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != tc.wantCode {
				t.Errorf("status = %d, want %d\nbody: %s", rec.Code, tc.wantCode, rec.Body.String())
			}
		})
	}
}

// TestAdminIssuerUnsetDoesNotBlock keeps a single-issuer deployment and the
// existing test suites working: the check is opt-in via SetAdminIssuer.
func TestAdminIssuerUnsetDoesNotBlock(t *testing.T) {
	SetAdminIssuer("")

	h := RequireRole(AdminRoles...)(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))

	req := httptest.NewRequestWithContext(t.Context(),
		http.MethodGet, "/api/v1/admin/x", http.NoBody)
	req = req.WithContext(WithPrincipal(req.Context(), Principal{
		UserID: uuid.New(), Roles: []Role{RoleSuperAdmin}, Issuer: "anything",
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("with no admin issuer configured, status = %d, want 200", rec.Code)
	}
}
