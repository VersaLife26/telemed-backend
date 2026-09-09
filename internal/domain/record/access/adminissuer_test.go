package access

import (
	"testing"

	"github.com/google/uuid"

	"telemed/internal/platform/middleware"
)

const (
	keycloakIssuer = "https://auth.yourapp.lk/realms/telemedicine"
	patientIssuer  = "telemed-user-service"
)

// An admin role asserted by the patient issuer must not open the credential
// bucket -- and it must not open anything else either.
//
// ADR-012 bound each issuer to its own key set, so user-service cannot sign a
// token that CLAIMS to be Keycloak. It can still mint an entirely honest token
// under its own issuer carrying realm_access.roles = ["super_admin"], which
// verifies cleanly. Until SetAdminIssuer was called at boot, adminIssuer was
// "" in this process and every admin-role check in the service layer waved
// such a token straight through: a phone-OTP login would have been an admin,
// which is a 2FA bypass rather than a role escalation.
func TestDecideAccess_AdminRoleIsOnlyHonouredFromTheAdminIssuer(t *testing.T) {
	middleware.SetAdminIssuer(keycloakIssuer)
	t.Cleanup(func() { middleware.SetAdminIssuer("") })

	forged := middleware.Principal{
		UserID: uuid.New(),
		Roles:  middleware.AdminRoles,
		Issuer: patientIssuer,
	}
	genuine := middleware.Principal{
		UserID: uuid.New(),
		Roles:  []middleware.Role{middleware.RoleAdmin},
		Issuer: keycloakIssuer,
	}

	// The credential bucket is the one thing an admin legitimately opens.
	d := decideAccess(genuine, uuid.New(), ResourceCredentialDocument, ActionView, false, nil)
	if !d.Granted || d.Reason != ReasonAdmin {
		t.Fatalf("a genuine Keycloak admin must still read credential documents; got %+v", d)
	}

	for _, role := range middleware.AdminRoles {
		p := forged
		p.Roles = []middleware.Role{role}
		got := decideAccess(p, uuid.New(), ResourceCredentialDocument, ActionView, false, nil)
		if got.Granted {
			t.Fatalf("role %q asserted by the patient issuer was granted %s access: %+v",
				role, ResourceCredentialDocument, got)
		}
		if got.Reason == ReasonDeniedAdminClinical {
			t.Fatalf("role %q asserted by the patient issuer was still treated as an admin (reason %q); it must fall through to the non-admin path",
				role, got.Reason)
		}
	}

	// And a forged admin gets no clinical access either.
	for _, resource := range []ResourceType{ResourceDocument, ResourcePrescription} {
		got := decideAccess(forged, uuid.New(), resource, ActionView, false, nil)
		if got.Granted {
			t.Fatalf("a forged admin was granted %s: %+v", resource, got)
		}
	}
}

// IsAdmin and HasAdminRole must both apply the binding, because the service
// layer never passes through RequireRole.
func TestPrincipalAdminHelpersApplyTheIssuerBinding(t *testing.T) {
	middleware.SetAdminIssuer(keycloakIssuer)
	t.Cleanup(func() { middleware.SetAdminIssuer("") })

	forged := middleware.Principal{Roles: middleware.AdminRoles, Issuer: patientIssuer}
	genuine := middleware.Principal{Roles: middleware.AdminRoles, Issuer: keycloakIssuer}

	if forged.IsAdmin() {
		t.Fatal("IsAdmin honoured an admin role from the patient issuer")
	}
	if forged.HasAdminRole(middleware.RoleSuperAdmin, middleware.RoleFinance) {
		t.Fatal("HasAdminRole honoured an admin role from the patient issuer")
	}
	if !genuine.IsAdmin() || !genuine.HasAdminRole(middleware.RoleSuperAdmin) {
		t.Fatal("a genuine Keycloak admin must still be recognised")
	}
}
