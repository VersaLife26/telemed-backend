//go:build integration

package adminusers_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/rs/zerolog"

	"telemed/internal/domain/admin/adminusers"
	"telemed/internal/domain/admin/testutil"
)

// recordingIDP is a fake Keycloak that records the calls made to it, so a test
// can assert not just the end state but the ORDER things happened in -- which
// is the entire security property here.
type recordingIDP struct {
	calls        []string
	subject      string
	failCreate   error
	failSetRole  error
	failEnabled  error
	failRevoke   error
	createdRole  string
	rolledBackTo *bool
}

func (f *recordingIDP) CreateAdmin(_ context.Context, _, _, role string) (string, error) {
	f.calls = append(f.calls, "CreateAdmin")
	if f.failCreate != nil {
		return "", f.failCreate
	}
	f.createdRole = role
	if f.subject == "" {
		f.subject = uuid.NewString()
	}
	return f.subject, nil
}

func (f *recordingIDP) SetRole(_ context.Context, _, _, _ string) error {
	f.calls = append(f.calls, "SetRole")
	return f.failSetRole
}

func (f *recordingIDP) SetEnabled(_ context.Context, _ string, enabled bool) error {
	f.calls = append(f.calls, "SetEnabled")
	f.rolledBackTo = &enabled
	return f.failEnabled
}

func (f *recordingIDP) RevokeSessions(_ context.Context, _ string) error {
	f.calls = append(f.calls, "RevokeSessions")
	return f.failRevoke
}

func newService(t *testing.T, idp adminusers.IdentityProvider) *adminusers.Service {
	t.Helper()
	pool := testutil.StartPostgres(t)
	return adminusers.NewService(adminusers.NewRepository(pool), idp, zerolog.Nop())
}

// TestCreateProvisionsTheLoginBeforeTheRow pins the ordering. A row without a
// Keycloak subject is an admin who cannot sign in and nothing surfaces it
// until someone tries.
func TestCreateProvisionsTheLoginBeforeTheRow(t *testing.T) {
	idp := &recordingIDP{}
	svc := newService(t, idp)

	created, err := svc.Create(t.Context(), adminusers.CreateParams{
		Email: "ops@clinic.lk", DisplayName: "Ops Person", Role: "ops",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.KeycloakSubject != idp.subject {
		t.Errorf("row subject %q, keycloak issued %q", created.KeycloakSubject, idp.subject)
	}
	if idp.createdRole != "ops" {
		t.Errorf("keycloak was given role %q, want ops -- the realm role is what "+
			"actually authorizes the account", idp.createdRole)
	}
	if created.Role != "ops" || !created.Active {
		t.Errorf("row = role %q active %v, want ops/true", created.Role, created.Active)
	}
}

// TestCreateRefusesWhenKeycloakIsDown proves admin-service does NOT degrade
// the way user-service does. A database row for an admin with no login is not
// a partial success, it is an account nobody can use and nobody notices.
func TestCreateRefusesWhenKeycloakIsDown(t *testing.T) {
	idp := &recordingIDP{failCreate: adminusers.ErrIdentityProviderUnavailable}
	svc := newService(t, idp)

	_, err := svc.Create(t.Context(), adminusers.CreateParams{
		Email: "nobody@clinic.lk", DisplayName: "Nobody", Role: "support",
	})
	if !errors.Is(err, adminusers.ErrIdentityProviderUnavailable) {
		t.Fatalf("Create with Keycloak down = %v, want ErrIdentityProviderUnavailable", err)
	}

	all, err := svc.List(t.Context())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, a := range all {
		if a.Email == "nobody@clinic.lk" {
			t.Fatal("a row was written for an admin whose login was never created")
		}
	}
}

// TestRoleChangeReachesKeycloak is the F4 regression: admin_users.role is a
// label, and authorization reads realm_access.roles off the JWT. Writing the
// row without writing the realm produced a console that reported a demotion
// which had not happened.
func TestRoleChangeReachesKeycloak(t *testing.T) {
	idp := &recordingIDP{}
	svc := newService(t, idp)

	created, err := svc.Create(t.Context(), adminusers.CreateParams{
		Email: "finance@clinic.lk", DisplayName: "Finance Person", Role: "finance",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	idp.calls = nil

	demoted := "support"
	updated, err := svc.Update(t.Context(), created.ID, adminusers.UpdateParams{
		Role: &demoted, Version: created.Version,
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.Role != "support" {
		t.Errorf("row role = %q, want support", updated.Role)
	}

	var sawSetRole, sawRevoke bool
	for _, c := range idp.calls {
		switch c {
		case "SetRole":
			sawSetRole = true
		case "RevokeSessions":
			sawRevoke = true
		}
	}
	if !sawSetRole {
		t.Error("a role change did not reach Keycloak; the demotion is cosmetic " +
			"and the account keeps its old access (security review F4)")
	}
	if !sawRevoke {
		t.Error("sessions were not revoked; the demotion only applies at token " +
			"expiry, leaving a window where the console and reality disagree")
	}
}

// TestRoleChangeIsRefusedIfKeycloakRejectsIt proves the write order. Keycloak
// is written first precisely so a failure is a clean refusal rather than a
// database that disagrees with what actually authorizes the account.
func TestRoleChangeIsRefusedIfKeycloakRejectsIt(t *testing.T) {
	idp := &recordingIDP{}
	svc := newService(t, idp)

	created, err := svc.Create(t.Context(), adminusers.CreateParams{
		Email: "ops2@clinic.lk", DisplayName: "Ops Two", Role: "ops",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	idp.failSetRole = adminusers.ErrIdentityProviderUnavailable
	promoted := "super_admin"
	if _, err := svc.Update(t.Context(), created.ID, adminusers.UpdateParams{
		Role: &promoted, Version: created.Version,
	}); !errors.Is(err, adminusers.ErrIdentityProviderUnavailable) {
		t.Fatalf("Update = %v, want ErrIdentityProviderUnavailable", err)
	}

	after, err := svc.Get(t.Context(), created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if after.Role != "ops" {
		t.Fatalf("row says %q after a REJECTED promotion; the database must not "+
			"claim access the realm did not grant", after.Role)
	}
}

// TestUnavailableProviderRefusesEverything states the fail-closed contract of
// the no-credentials stand-in, so a future edit cannot make one method a
// silent no-op.
func TestUnavailableProviderRefusesEverything(t *testing.T) {
	p := adminusers.NewUnavailableProvider(zerolog.Nop())
	ctx := t.Context()

	if _, err := p.CreateAdmin(ctx, "a@b.c", "A", "ops"); !errors.Is(err, adminusers.ErrIdentityProviderUnavailable) {
		t.Errorf("CreateAdmin = %v", err)
	}
	if err := p.SetRole(ctx, "s", "ops", "finance"); !errors.Is(err, adminusers.ErrIdentityProviderUnavailable) {
		t.Errorf("SetRole = %v", err)
	}
	if err := p.SetEnabled(ctx, "s", false); !errors.Is(err, adminusers.ErrIdentityProviderUnavailable) {
		t.Errorf("SetEnabled = %v", err)
	}
	if err := p.RevokeSessions(ctx, "s"); !errors.Is(err, adminusers.ErrIdentityProviderUnavailable) {
		t.Errorf("RevokeSessions = %v", err)
	}
}
