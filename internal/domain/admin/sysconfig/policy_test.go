package sysconfig

import (
	"testing"

	"telemed/internal/platform/middleware"
)

func principal(roles ...middleware.Role) middleware.Principal {
	return middleware.Principal{Roles: roles}
}

// SECURITY-REVIEW F18. GroupFinance is {finance, super_admin}, commented
// "support and ops staff have no legitimate reason to move money or edit
// commission rules". But the rule is STORED as sysconfig key
// "commission_rules", Put validated the key against a regex only, and
// GroupConfig is {super_admin, ops, finance}. So an ops admin who got a 403
// from PUT /admin/finance/commission-rules called
// PUT /admin/configs/commission_rules instead and wrote the same row --
// audited as a generic "config.updated", invisible in a finance review.
//
// The console has since blocked its own path to that key. That is a
// client-side guard on a server-side hole: curl still gets there. This is the
// server-side half.
func TestOpsCannotEditCommissionRulesThroughSysconfig(t *testing.T) {
	t.Parallel()

	refused := []middleware.Role{
		middleware.RoleOps, middleware.RoleSupport, middleware.RoleAdmin,
		middleware.RolePatient, middleware.RoleDoctor,
	}
	for _, role := range refused {
		if err := authoriseWrite(principal(role), "commission_rules"); err == nil {
			t.Fatalf("role %q may write commission_rules through the sysconfig endpoint; "+
				"GroupFinance exists precisely to stop that", role)
		}
	}

	for _, role := range []middleware.Role{middleware.RoleFinance, middleware.RoleSuperAdmin} {
		if err := authoriseWrite(principal(role), "commission_rules"); err != nil {
			t.Fatalf("role %q must still be able to edit commission rules: %v", role, err)
		}
	}
}

// The allowlist is an allowlist. A well-formed key nobody declared cannot be
// written at all -- otherwise the next money-shaped setting arrives with no
// policy and the hole reopens silently.
func TestUnknownKeysAreRefusedEvenForSuperAdmin(t *testing.T) {
	t.Parallel()

	unknown := []string{
		"payout_rules", "commission_rules_v2", "feature.anything",
		"commission_rules.default", "refund_policy",
	}
	for _, key := range unknown {
		if err := authoriseWrite(principal(middleware.RoleSuperAdmin), key); err == nil {
			t.Fatalf("key %q was writable without ever appearing in writePolicy", key)
		}
	}
}

func TestKnownKeysKeepTheirIntendedOwners(t *testing.T) {
	t.Parallel()

	cases := []struct {
		key     string
		allowed []middleware.Role
		refused []middleware.Role
	}{
		{"commission_rules", []middleware.Role{middleware.RoleFinance, middleware.RoleSuperAdmin},
			[]middleware.Role{middleware.RoleOps, middleware.RoleSupport, middleware.RoleAdmin}},
		{"cancellation_policy", []middleware.Role{middleware.RoleOps, middleware.RoleFinance, middleware.RoleSuperAdmin},
			[]middleware.Role{middleware.RoleSupport, middleware.RoleAdmin}},
		{"slot_defaults", []middleware.Role{middleware.RoleOps, middleware.RoleSuperAdmin},
			[]middleware.Role{middleware.RoleFinance, middleware.RoleSupport, middleware.RoleAdmin}},
		{"fee_caps", []middleware.Role{middleware.RoleFinance, middleware.RoleSuperAdmin},
			[]middleware.Role{middleware.RoleOps, middleware.RoleSupport, middleware.RoleAdmin}},
		{"feature_flags", []middleware.Role{middleware.RoleOps, middleware.RoleSuperAdmin},
			[]middleware.Role{middleware.RoleFinance, middleware.RoleSupport, middleware.RoleAdmin}},
		{"corporate_clients", []middleware.Role{middleware.RoleOps, middleware.RoleFinance, middleware.RoleSuperAdmin},
			[]middleware.Role{middleware.RoleSupport, middleware.RoleAdmin}},
		{"feature.waitlist_v2", []middleware.Role{middleware.RoleOps, middleware.RoleSuperAdmin},
			[]middleware.Role{middleware.RoleFinance, middleware.RoleSupport, middleware.RoleAdmin}},
	}
	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			t.Parallel()
			for _, role := range tc.allowed {
				if err := authoriseWrite(principal(role), tc.key); err != nil {
					t.Fatalf("%q must be writable by %q: %v", tc.key, role, err)
				}
			}
			for _, role := range tc.refused {
				if err := authoriseWrite(principal(role), tc.key); err == nil {
					t.Fatalf("%q was writable by %q", tc.key, role)
				}
			}
		})
	}
}

// The write policy must apply the issuer binding too: user-service can mint an
// honest token asserting finance, and the service layer never passes through
// RequireRole.
func TestWritePolicyHonoursTheAdminIssuer(t *testing.T) {
	middleware.SetAdminIssuer("https://auth.yourapp.lk/realms/telemedicine")
	t.Cleanup(func() { middleware.SetAdminIssuer("") })

	forged := middleware.Principal{Roles: []middleware.Role{middleware.RoleFinance}, Issuer: "telemed-user-service"}
	if err := authoriseWrite(forged, "commission_rules"); err == nil {
		t.Fatal("a finance role asserted by the patient issuer edited the commission rules")
	}

	genuine := middleware.Principal{Roles: []middleware.Role{middleware.RoleFinance}, Issuer: "https://auth.yourapp.lk/realms/telemedicine"}
	if err := authoriseWrite(genuine, "commission_rules"); err != nil {
		t.Fatalf("a genuine Keycloak finance admin must still be able to edit commission rules: %v", err)
	}
}
