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
	mw "telemed/internal/platform/middleware"
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
		t.Error("a role change did not reach the identity provider; under a provider " +
			"that holds the authorising role this makes the demotion cosmetic " +
			"(security review F4)")
	}
	if !sawRevoke {
		t.Error("sessions were not revoked; the demotion only applies at token " +
			"expiry, leaving a window where the console and reality disagree")
	}
}

// TestRoleChangeIsRefusedIfTheProviderRejectsIt proves the write order. The
// identity provider is called first precisely so a failure is a clean refusal
// rather than a database that disagrees with it.
func TestRoleChangeIsRefusedIfTheProviderRejectsIt(t *testing.T) {
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

// TestAccessProviderIsADatabaseFirstNoOp states the contract that replaced the
// Keycloak provider, so a future edit cannot quietly reintroduce a dependency
// on an identity provider this deployment does not run.
//
// Under Keycloak the realm held the role that authorised every route, so these
// calls had to succeed for a change to be real. Under Cloudflare Access the
// admin_users row is what Directory.RolesForSubject reads, so the service's own
// write IS the change and these are records of it.
func TestAccessProviderIsADatabaseFirstNoOp(t *testing.T) {
	p := adminusers.NewAccessProvider(zerolog.Nop())
	ctx := t.Context()

	// The subject is the normalised email, because that is what an Access
	// token is matched on and what admin_users.keycloak_subject (NOT NULL,
	// unique) has to hold.
	subject, err := p.CreateAdmin(ctx, "  Ops@Clinic.LK ", "Ops", "ops")
	if err != nil {
		t.Fatalf("CreateAdmin: %v", err)
	}
	if subject != "ops@clinic.lk" {
		t.Errorf("subject = %q, want the normalised email", subject)
	}

	if err := p.SetRole(ctx, subject, "ops", "finance"); err != nil {
		t.Errorf("SetRole = %v, want nil", err)
	}
	if err := p.SetEnabled(ctx, subject, false); err != nil {
		t.Errorf("SetEnabled = %v, want nil", err)
	}
	if err := p.RevokeSessions(ctx, subject); err != nil {
		t.Errorf("RevokeSessions = %v, want nil", err)
	}
}

// TestFirstSignInRekeysAConsoleCreatedAdmin: the console stores the email as a
// placeholder subject, and the first sign-in carries the Access user id.
// Without the re-key the upsert collides with idx_admin_users_email and every
// admin request 500s.
func TestFirstSignInRekeysAConsoleCreatedAdmin(t *testing.T) {
	svc := newService(t, adminusers.NewAccessProvider(zerolog.Nop()))
	ctx := t.Context()

	created, err := svc.Create(ctx, adminusers.CreateParams{Email: "ops@clinic.lk", Role: "ops"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	accessID := uuid.NewString()
	got, err := svc.Ensure(ctx, mw.Principal{Subject: accessID, Email: "ops@clinic.lk", Roles: []mw.Role{"ops"}})
	if err != nil {
		t.Fatalf("first sign-in: %v", err)
	}
	if got.ID != created.ID || got.KeycloakSubject != accessID {
		t.Errorf("first sign-in resolved row %s subject %q, want row %s subject %q",
			got.ID, got.KeycloakSubject, created.ID, accessID)
	}

	// Only placeholder rows are re-keyed: another subject presenting the same
	// email must not take over an account that has already signed in.
	if _, err := svc.Ensure(ctx, mw.Principal{Subject: uuid.NewString(), Email: "ops@clinic.lk", Roles: []mw.Role{"ops"}}); err == nil {
		t.Error("a second subject for a bound email resolved; want the unique-email error")
	}
}
