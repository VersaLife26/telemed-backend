// Package users implements the admin console's user search, suspend and
// reinstate surface (V2 docs section 7.3 page 4: "Search patients/doctors,
// ban/unban, view activity"). user-service remains the system of record for
// account data; this service keeps a search-optimised local projection fed
// by user.registered/user.suspended/user.reinstated, and issues suspend and
// reinstate as commands over the outbox rather than a synchronous write to a
// database it does not own (ADR-004).
//
// Impersonation ("impersonate for support (audit logged)" in the same docs
// section) is deliberately NOT implemented here -- see docs/DESIGN.md
// "Impersonation" for why, and what would be required to add it safely.
package users

import (
	"time"

	"github.com/google/uuid"
)

// User is one user_projection row.
type User struct {
	UserID       uuid.UUID
	FullName     string
	Email        string
	Phone        string
	Role         string // patient | doctor
	Status       string // active | suspended
	RegisteredAt time.Time
}

// Registration is the repository's input for one projected user row.
//
// It is deliberately NOT a wire type and carries no json tags: the wire types
// live once, in internal/platform/events/payloads.go, shared with their
// producers. This struct is assembled by the projector from
// events.UserRegistered plus a directory lookup for the identity fields that
// event does not -- and must not -- broadcast.
type Registration struct {
	UserID       uuid.UUID
	FullName     string
	Email        string
	Phone        string
	Role         string
	RegisteredAt time.Time
}

type ListFilter struct {
	Query   string // matches name/email/phone
	Role    string
	Status  string
	Page    int
	PerPage int
}

// The command this service publishes as admin.user_suspend_requested /
// admin.user_reinstate_requested is events.AdminUserStatusRequested, the
// canonical type shared with user-service, which is the consumer that applies
// it. It used to be a private struct declared here -- and for a while nothing
// consumed the subject at all, so the shape was never tested against a reader.
// See internal/users/service.go.
