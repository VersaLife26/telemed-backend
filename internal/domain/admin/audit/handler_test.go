package audit_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"telemed/internal/domain/admin/audit"
	"telemed/internal/domain/admin/rbac"
	mw "telemed/internal/platform/middleware"
)

// exportHandler builds the audit router exactly as cmd/server/main.go does --
// same rbac group, same composition -- but over a nil pool.
//
// A nil pool is not a shortcut here, it is the assertion. Every case below is
// one the endpoint must refuse BEFORE it goes anywhere near the database:
// the wrong role, or a filter that is not bounded. If any of them ever reaches
// the repository, the test panics rather than quietly passing, which is a
// stronger statement than checking a status code alone.
func exportHandler() http.Handler {
	svc := audit.NewService(audit.NewRepository(nil))
	return audit.NewHandler(svc, rbac.Roles(rbac.GroupAuditExport)...).Routes()
}

func exportRequest(t *testing.T, role mw.Role, query string) *httptest.ResponseRecorder {
	t.Helper()
	ctx := mw.WithPrincipal(context.Background(), mw.Principal{UserID: uuid.New(), Roles: []mw.Role{role}})
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/export"+query, http.NoBody)
	rec := httptest.NewRecorder()
	exportHandler().ServeHTTP(rec, req)
	return rec
}

// TestAuditExport_OnlyFinanceAndSuperAdmin is finding F6's first half. One
// RequireRole(rbac.Roles(GroupAudit)...) covered the whole /audit subtree and
// GroupAudit is mw.AdminRoles, so `support` -- the lowest admin tier, the one
// handed to a contractor on day one -- could GET the entire audit trail.
func TestAuditExport_OnlyFinanceAndSuperAdmin(t *testing.T) {
	// A perfectly well-formed, bounded request. The refused roles must not get
	// it either: this is a role gate, not a rate limit.
	window := fmt.Sprintf("?from=%s&to=%s",
		time.Now().Add(-24*time.Hour).UTC().Format(time.RFC3339),
		time.Now().UTC().Format(time.RFC3339))

	refused := []mw.Role{mw.RoleSupport, mw.RoleOps, mw.RoleAdmin, mw.RolePatient, mw.RoleDoctor}
	for _, role := range refused {
		t.Run("refused/"+string(role), func(t *testing.T) {
			rec := exportRequest(t, role, window)
			require.Equal(t, http.StatusForbidden, rec.Code,
				"role %q must not be able to bulk-export the audit trail; got %d: %s",
				role, rec.Code, rec.Body.String())
		})
	}

	// The allowed roles are sent an UNBOUNDED request on purpose. It gets past
	// the role gate and is then refused by the export bound, which is a 400 --
	// so the assertion "not 403" distinguishes "this role may export" from
	// "the gate rejects everyone", without the request ever reaching the
	// database. The bounded happy path is covered end to end in
	// TestAuditExport_IsItselfAudited against a real Postgres.
	for _, role := range []mw.Role{mw.RoleFinance, mw.RoleSuperAdmin} {
		t.Run("allowed/"+string(role), func(t *testing.T) {
			rec := exportRequest(t, role, "")
			require.Equal(t, http.StatusBadRequest, rec.Code,
				"role %q must get past the role gate (and then hit the export bound); got %d: %s",
				role, rec.Code, rec.Body.String())
		})
	}
}

// TestAuditExport_RefusesAnUnboundedRequest is F6's second half: the query had
// no LIMIT and no mandatory date range, so a single GET with no parameters
// returned every audit row on the platform.
func TestAuditExport_RefusesAnUnboundedRequest(t *testing.T) {
	now := time.Now().UTC()

	cases := []struct {
		name  string
		query string
	}{
		{"no parameters at all", ""},
		{"from without to", "?from=" + now.Add(-24*time.Hour).Format(time.RFC3339)},
		{"to without from", "?to=" + now.Format(time.RFC3339)},
		{
			"a window wider than the cap",
			fmt.Sprintf("?from=%s&to=%s",
				now.Add(-(audit.MaxExportWindow + time.Hour)).Format(time.RFC3339),
				now.Format(time.RFC3339)),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := exportRequest(t, mw.RoleFinance, tc.query)
			require.Equal(t, http.StatusBadRequest, rec.Code,
				"an export with %s must be refused, not served; got %d: %s", tc.name, rec.Code, rec.Body.String())
		})
	}
}

// TestAuditExport_UnauthenticatedIsRejected confirms the route does not fall
// through to the handler when no principal was ever attached.
func TestAuditExport_UnauthenticatedIsRejected(t *testing.T) {
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/export", http.NoBody)
	rec := httptest.NewRecorder()
	exportHandler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

// TestAuditExport_HandlerBuiltWithNoRolesFailsClosed pins the constructor's
// fallback. An empty variadic is the way this gate would most plausibly be
// broken by a future refactor -- a call site that stops passing the rbac
// group -- and the result must be "super_admin only", never "no gate".
func TestAuditExport_HandlerBuiltWithNoRolesFailsClosed(t *testing.T) {
	h := audit.NewHandler(audit.NewService(audit.NewRepository(nil))).Routes()

	ctx := mw.WithPrincipal(context.Background(), mw.Principal{UserID: uuid.New(), Roles: []mw.Role{mw.RoleFinance}})
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/export", http.NoBody)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusForbidden, rec.Code,
		"an export handler built with no role list must fall back to the narrowest gate, not to an open route")
}
