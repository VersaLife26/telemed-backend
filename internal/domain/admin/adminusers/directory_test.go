package adminusers

import (
	"context"
	"testing"

	"github.com/rs/zerolog"
)

// The issuer gate is the whole security value of this type, so it is tested
// with a nil repository on purpose: every case below must return before it
// could possibly reach the database. If one of them ever does, the test panics
// rather than quietly passing against a fake that answered anyway.

func TestDirectoryAnswersOnlyForTheAdminIssuer(t *testing.T) {
	const adminIssuer = "https://team.cloudflareaccess.com"
	d := NewDirectory(nil, adminIssuer, 0, zerolog.Nop())

	// user-service is a trusted issuer for patients and doctors. If this
	// directory answered for it, a patient whose email happens to match an
	// administrator's would arrive at the admin surface holding that
	// administrator's role -- which is the exact escalation the issuer
	// binding in ADR-012 exists to prevent.
	roles, err := d.RolesForSubject(context.Background(),
		"telemed-user-service", "sub-1", "ops@example.lk")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(roles) != 0 {
		t.Errorf("resolved %v for a non-admin issuer: roles must only ever be "+
			"attached to a token minted by the admin issuer", roles)
	}
}

func TestDirectoryWithNoIssuerConfiguredIsInert(t *testing.T) {
	d := NewDirectory(nil, "", 0, zerolog.Nop())

	// An unset ADMIN_ISSUER must disable the directory rather than make it
	// answer for everything. The gateway refuses to boot in prod without one;
	// this is the belt to that braces.
	roles, err := d.RolesForSubject(context.Background(), "anything", "sub", "ops@example.lk")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(roles) != 0 {
		t.Errorf("resolved %v with no issuer configured", roles)
	}
}

func TestDirectoryIgnoresATokenWithNoEmail(t *testing.T) {
	const adminIssuer = "https://team.cloudflareaccess.com"
	d := NewDirectory(nil, adminIssuer, 0, zerolog.Nop())

	// Email is the join key. A token from the right issuer but without one
	// cannot be resolved, and guessing is not an option.
	for _, email := range []string{"", "   "} {
		roles, err := d.RolesForSubject(context.Background(), adminIssuer, "sub", email)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(roles) != 0 {
			t.Errorf("resolved %v for an empty email", roles)
		}
	}
}
