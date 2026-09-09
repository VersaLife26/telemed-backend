// Package adminusers links a verified Keycloak identity to a local admin
// account: whether it is active, and any per-admin IP scope narrower than
// the service-wide allowlist. Authorization by role continues to come from
// the JWT (RequireRole reads realm_access.roles, exactly as every other
// telemed service does) -- this package adds the two things a JWT cannot
// express: "deactivate this one admin's access right now, without touching
// Keycloak" and "this admin may only ever connect from this narrower CIDR
// set". See RequireActiveAdminUser.
package adminusers

import (
	"time"

	"github.com/google/uuid"
)

// AdminUser is one admin_users row.
type AdminUser struct {
	ID              uuid.UUID
	KeycloakSubject string
	Email           string
	DisplayName     string
	Role            string
	IPAllowlist     []string
	Active          bool
	LastLoginAt     *time.Time
	CreatedAt       time.Time
	UpdatedAt       time.Time
	Version         int
}

// UpdateParams is the mutable subset of an admin account a super_admin may
// change via PUT /api/v1/admin/admin-users/{id}.
type UpdateParams struct {
	Active      *bool
	Role        *string
	IPAllowlist *[]string
	Version     int // optimistic lock: caller must supply the version it read
}

// CreateParams is a new admin account. Role is validated against the same set
// the database CHECK constraint permits and rbac.AssignableRoles offers.
type CreateParams struct {
	KeycloakSubject string
	Email           string
	DisplayName     string
	Role            string
	IPAllowlist     []string
}
