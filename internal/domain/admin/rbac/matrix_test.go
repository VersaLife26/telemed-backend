package rbac_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"telemed/internal/domain/admin/rbac"

	mw "telemed/internal/platform/middleware"
)

// everyRole is every role token that can ever appear in a Keycloak realm
// token, admin or otherwise. The matrix test drives every group against
// every one of these, which is what "across every route" in the brief means
// in practice: not just confirming the roles a group *should* accept, but
// also confirming every role it should NOT accept is actually rejected --
// including patient and doctor tokens hitting the admin surface, which is
// exactly the RBAC-bypass scenario section 2.1 of the V2 docs warns about.
var everyRole = []mw.Role{
	mw.RolePatient, mw.RoleDoctor, mw.RoleAdmin, mw.RoleSuperAdmin,
	mw.RoleOps, mw.RoleFinance, mw.RoleSupport, mw.RoleService,
}

// buildGroupRouter mounts a single dummy 204 handler behind exactly the
// middleware chain main.go uses for every real route group:
// RequireRole(rbac.Roles(group)...). It does not include RequireAuth or
// IPAllowlist -- those are tested independently by the platform template's
// own middleware (auth verification and CIDR matching are generic, not
// specific to this service's route groups) and by requiring a principal to
// already be attached, this test isolates exactly the thing rbac.Matrix is
// responsible for: which roles pass RequireRole for which group.
func buildGroupRouter(g rbac.Group) http.Handler {
	r := chi.NewRouter()
	r.With(mw.RequireRole(rbac.Roles(g)...)).Get("/probe", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	return r
}

func requestAs(t *testing.T, handler http.Handler, role mw.Role) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/probe", http.NoBody)
	principal := mw.Principal{UserID: uuid.New(), Roles: []mw.Role{role}}
	ctx := mw.WithPrincipal(context.Background(), principal)
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

// TestMatrix_EveryGroupEveryRole is exhaustive: for every mounted group and
// every possible role token, it asserts 204 if and only if rbac.Matrix says
// that role belongs, and 403 otherwise.
func TestMatrix_EveryGroupEveryRole(t *testing.T) {
	for _, group := range rbac.AllGroups {
		allowed := rbac.Roles(group)
		handler := buildGroupRouter(group)

		for _, role := range everyRole {
			wantAllowed := slices.Contains(allowed, role)
			t.Run(string(group)+"/"+string(role), func(t *testing.T) {
				rec := requestAs(t, handler, role)
				if wantAllowed {
					require.Equal(t, http.StatusNoContent, rec.Code,
						"role %q must be permitted on group %q (matrix: %v)", role, group, allowed)
				} else {
					require.Equal(t, http.StatusForbidden, rec.Code,
						"role %q must NOT be permitted on group %q (matrix: %v)", role, group, allowed)
				}
			})
		}
	}
}

// TestMatrix_UnauthenticatedIsRejected confirms a request that never passed
// RequireAuth (no principal in context at all) is rejected on every group,
// not just the ones with a narrow role list.
func TestMatrix_UnauthenticatedIsRejected(t *testing.T) {
	for _, group := range rbac.AllGroups {
		handler := buildGroupRouter(group)
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/probe", http.NoBody)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		require.Equal(t, http.StatusUnauthorized, rec.Code, "group %q must reject an unauthenticated request", group)
	}
}

// TestMatrix_FinanceIsRestrictedToFinanceAndSuperAdmin pins the specific
// requirement from AGENT-BRIEF: "Finance routes additionally require
// finance or super_admin." A generic admin or ops token -- which passes
// every other admin group -- must not pass finance.
func TestMatrix_FinanceIsRestrictedToFinanceAndSuperAdmin(t *testing.T) {
	got := rbac.Roles(rbac.GroupFinance)
	require.ElementsMatch(t, []mw.Role{mw.RoleFinance, mw.RoleSuperAdmin}, got)
}

// TestMatrix_AdminUsersIsSuperAdminOnly guards against privilege escalation:
// only super_admin may manage other admin accounts.
func TestMatrix_AdminUsersIsSuperAdminOnly(t *testing.T) {
	got := rbac.Roles(rbac.GroupAdminUsers)
	require.ElementsMatch(t, []mw.Role{mw.RoleSuperAdmin}, got)
}

// TestMatrix_AuditExportIsNarrowerThanAuditRead is security review F6 stated
// as an invariant rather than as a route test: reading a filtered page of the
// audit log is ordinary oversight work, and bulk-exporting the whole trail as
// CSV is not the same act. The export group must therefore be a strict subset
// of the read group, and must exclude support -- the tier with the least
// vetting and the most turnover.
func TestMatrix_AuditExportIsNarrowerThanAuditRead(t *testing.T) {
	read := rbac.Roles(rbac.GroupAudit)
	export := rbac.Roles(rbac.GroupAuditExport)

	require.ElementsMatch(t, []mw.Role{mw.RoleFinance, mw.RoleSuperAdmin}, export)
	require.Less(t, len(export), len(read), "the export group must be strictly narrower than the read group")
	for _, role := range export {
		require.Contains(t, read, role, "export role %q must also be able to read the audit log", role)
	}
	require.NotContains(t, export, mw.RoleSupport)
	require.NotContains(t, export, mw.RoleOps)
	require.NotContains(t, export, mw.RoleAdmin)
}

// TestMatrix_NoGroupGrantsNonAdminRoles is a defence-in-depth check: no
// group, however it is edited in the future, may ever grant patient, doctor
// or the internal service role access to the admin surface.
func TestMatrix_NoGroupGrantsNonAdminRoles(t *testing.T) {
	forbidden := []mw.Role{mw.RolePatient, mw.RoleDoctor, mw.RoleService}
	for _, group := range rbac.AllGroups {
		for _, role := range rbac.Roles(group) {
			require.NotContains(t, forbidden, role, "group %q must never grant role %q", group, role)
		}
	}
}
