package adminusers

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/rs/zerolog"

	"telemed/internal/domain/admin/audit"
	mw "telemed/internal/platform/middleware"
)

type Service struct {
	repo *Repository
	idp  IdentityProvider
	log  zerolog.Logger
}

func NewService(repo *Repository, idp IdentityProvider, log zerolog.Logger) *Service {
	return &Service{repo: repo, idp: idp, log: log}
}

// Ensure resolves the calling principal to a local admin_users row, creating
// it on first sight. Every request that reaches an admin route after
// RequireAuth calls this exactly once, via RequireActiveAdminUser.
func (s *Service) Ensure(ctx context.Context, p mw.Principal) (AdminUser, error) {
	role := primaryRole(p)
	return s.repo.EnsureByKeycloakSubject(ctx, p.Subject, p.Email, "", role)
}

func (s *Service) List(ctx context.Context) ([]AdminUser, error) {
	return s.repo.List(ctx)
}

func (s *Service) Get(ctx context.Context, id uuid.UUID) (AdminUser, error) {
	return s.repo.GetByID(ctx, id)
}

// Update applies a super_admin's change to another admin's active flag,
// role or IP scope, and stages the audit entry describing exactly what
// changed.
func (s *Service) Update(ctx context.Context, id uuid.UUID, p UpdateParams) (AdminUser, error) {
	before, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return AdminUser{}, err
	}

	// Keycloak FIRST, database second.
	//
	// admin_users.role is a label: authorization reads realm_access.roles off
	// the JWT, exactly as every other service does. So writing the row without
	// writing the realm produced a console that reported a demotion which had
	// not happened -- the security review's "role changes are inert" (F4).
	//
	// Doing the identity provider first means the failure mode is "the change
	// did not apply and the console says so", not "the console says support
	// and the account still moves money". If the database write below fails
	// afterwards, the account is left with the NEW privileges and the OLD
	// label, which is the safe direction for a demotion and is visible on the
	// next read.
	if p.Role != nil && *p.Role != before.Role {
		if err := s.idp.SetRole(ctx, before.KeycloakSubject, before.Role, *p.Role); err != nil {
			return AdminUser{}, fmt.Errorf("adminusers: propagate role change: %w", err)
		}
	}
	if p.Active != nil && *p.Active != before.Active {
		if err := s.idp.SetEnabled(ctx, before.KeycloakSubject, *p.Active); err != nil {
			return AdminUser{}, fmt.Errorf("adminusers: propagate active change: %w", err)
		}
	}

	after, err := s.repo.Update(ctx, id, p)
	if err != nil {
		return AdminUser{}, err
	}

	// End live sessions so the change takes effect now. Without this the
	// window between "the console says this person is now support" and "they
	// can still move money" is one access-token lifetime.
	//
	// A failure here is logged, not returned: the change itself has been
	// applied and reporting it as failed would invite the operator to repeat
	// an action that already succeeded. RequireActiveAdminUser re-reads the
	// row on every request, so a deactivation is enforced regardless; only the
	// immediacy of a ROLE change depends on this call.
	if (p.Role != nil && *p.Role != before.Role) || (p.Active != nil && !*p.Active) {
		if err := s.idp.RevokeSessions(ctx, before.KeycloakSubject); err != nil {
			s.log.Error().Err(err).Str("admin_user_id", id.String()).
				Msg("adminusers: could not revoke sessions; change applies at token expiry")
		}
	}

	audit.Stage(ctx, audit.Draft{
		Action:       "admin_user.updated",
		ResourceType: "admin_user",
		ResourceID:   id.String(),
		OldValue:     map[string]any{"active": before.Active, "role": before.Role, "ip_allowlist": before.IPAllowlist},
		NewValue:     map[string]any{"active": after.Active, "role": after.Role, "ip_allowlist": after.IPAllowlist},
	})
	return after, nil
}

// Create provisions a new admin: the Keycloak login first, then the row.
//
// That order is the point. A row without a login is an admin who cannot sign
// in, and nothing surfaces it until someone tries. A login without a row is
// refused by RequireActiveAdminUser, which fails closed -- so if the second
// step fails we unwind the first rather than leave an authenticated principal
// on the admin issuer that no local record explains.
func (s *Service) Create(ctx context.Context, p CreateParams) (AdminUser, error) {
	subject, err := s.idp.CreateAdmin(ctx, p.Email, p.DisplayName, p.Role)
	if err != nil {
		return AdminUser{}, err
	}
	p.KeycloakSubject = subject

	created, err := s.repo.Create(ctx, p)
	if err != nil {
		// Best-effort unwind. Disabling rather than deleting: if the delete
		// itself fails we have still removed the account's ability to
		// authenticate, and a disabled orphan is visible in the realm whereas
		// a half-deleted one is not.
		if disableErr := s.idp.SetEnabled(ctx, subject, false); disableErr != nil {
			s.log.Error().Err(disableErr).Str("keycloak_subject", subject).
				Msg("adminusers: could not disable orphaned keycloak login after a failed create; remove it by hand")
		}
		return AdminUser{}, err
	}

	audit.Stage(ctx, audit.Draft{
		Action:       "admin_user.created",
		ResourceType: "admin_user",
		ResourceID:   created.ID.String(),
		NewValue: map[string]any{
			"email": created.Email, "role": created.Role,
			"ip_allowlist": created.IPAllowlist,
		},
	})
	return created, nil
}

func primaryRole(p mw.Principal) string {
	precedence := []mw.Role{mw.RoleSuperAdmin, mw.RoleFinance, mw.RoleOps, mw.RoleSupport, mw.RoleAdmin}
	for _, r := range precedence {
		if p.HasRole(r) {
			return string(r)
		}
	}
	if len(p.Roles) > 0 {
		return string(p.Roles[0])
	}
	return "admin"
}

// ipInAllowlist reports whether ip matches at least one CIDR in scope. An
// invalid stored CIDR is skipped rather than treated as a match-everything
// wildcard -- a data-entry mistake in ip_allowlist must fail closed.
func ipInAllowlist(ip string, cidrs []string) (bool, error) {
	if len(cidrs) == 0 {
		return true, nil
	}
	matched, err := mw.CIDRContainsAny(ip, cidrs)
	if err != nil {
		return false, fmt.Errorf("adminusers: check ip scope: %w", err)
	}
	return matched, nil
}
