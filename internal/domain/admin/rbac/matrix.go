// Package rbac is the single source of truth for which admin role may call
// which group of routes. cmd/server/main.go mounts every route group behind
// RequireRole(rbac.Roles(group)...) -- nothing in this service hardcodes a
// role list at the point a route is registered. That is what makes the test
// in matrix_test.go meaningful: it exercises the exact same Matrix main.go
// consumes, so a change here is a change to what is actually enforced, not a
// parallel table that can quietly drift out of sync with it.
package rbac

import mw "telemed/internal/platform/middleware"

// Group names one router mount point.
type Group string

const (
	GroupCredentialing Group = "credentialing"
	GroupAdminUsers    Group = "admin_users"
	GroupUsers         Group = "users"
	GroupAppointments  Group = "appointments"
	GroupFinance       Group = "finance"
	GroupContent       Group = "content"
	GroupDisputes      Group = "disputes"
	GroupConfig        Group = "config"
	GroupAnalytics     Group = "analytics"
	GroupAudit         Group = "audit"
	// GroupAuditExport is a sub-group of GroupAudit, not a mount point of its
	// own: internal/audit's router composes it on GET /export only. Reading a
	// filtered, paginated page of the audit log is an ordinary admin activity;
	// walking off with the entire trail as a CSV is not the same act and does
	// not deserve the same role list (security review F6).
	GroupAuditExport Group = "audit_export"
)

// AllGroups lists every mounted group, for tests that must cover "every
// route" rather than whichever groups a test author remembered to list.
var AllGroups = []Group{
	GroupCredentialing, GroupAdminUsers, GroupUsers, GroupAppointments,
	GroupFinance, GroupContent, GroupDisputes, GroupConfig, GroupAnalytics,
	GroupAudit, GroupAuditExport,
}

// Matrix maps each group to the roles permitted to call it. AGENT-BRIEF: all
// routes require RequireAuth + RequireRole(AdminRoles...) + IPAllowlist;
// finance routes additionally require finance or super_admin. We read that
// as "finance routes require *only* finance or super_admin" -- support and
// ops staff have no legitimate reason to move money or edit commission
// rules, and mw.AdminRoles would let them.
//
// admin_users (managing other admin accounts, including deactivating one) is
// scoped to super_admin alone: any lesser admin role granting itself or a
// peer more access would be a privilege-escalation hole.
//
// config is split from the general admin surface because it includes
// commission_rules and fee caps (financial policy) alongside feature flags
// and cancellation policy (operational policy); finance and ops both have a
// legitimate reason to touch it, support does not.
var Matrix = map[Group][]mw.Role{
	GroupCredentialing: mw.AdminRoles,
	GroupAdminUsers:    {mw.RoleSuperAdmin},
	GroupUsers:         mw.AdminRoles,
	GroupAppointments:  mw.AdminRoles,
	GroupFinance:       {mw.RoleFinance, mw.RoleSuperAdmin},
	GroupContent:       mw.AdminRoles,
	GroupDisputes:      mw.AdminRoles,
	GroupConfig:        {mw.RoleSuperAdmin, mw.RoleOps, mw.RoleFinance},
	GroupAnalytics:     mw.AdminRoles,
	GroupAudit:         mw.AdminRoles,
	// The bulk export is finance-or-super_admin, not the whole admin set. The
	// audit handler's own doc comment claimed this check already existed and
	// it did not (F6): one RequireRole(GroupAudit...) covered the entire
	// subtree, so a support account could pull every audit row on the
	// platform in one unbounded, unlogged GET. Reading the log is oversight;
	// exfiltrating it is the move that precedes tampering with it.
	GroupAuditExport: {mw.RoleFinance, mw.RoleSuperAdmin},
}

// Roles returns the allowed roles for g. An unrecognised group -- which
// should never happen outside a programming error caught in review --
// deliberately falls back to the *narrowest* named role (super_admin) rather
// than either panicking or granting broad access, so a typo in a Group
// constant fails closed.
func Roles(g Group) []mw.Role {
	if roles, ok := Matrix[g]; ok && len(roles) > 0 {
		return roles
	}
	return []mw.Role{mw.RoleSuperAdmin}
}
