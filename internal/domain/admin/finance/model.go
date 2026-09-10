// Package finance implements the payments ledger view, commission rule
// editing, payout batch triggering, refund approval, and CSV financial
// reports (V2 docs section 7.3 page 6). The ledger view reads
// payments_projection -- the same table internal/analytics.Projector
// populates from payment.* events, so there is exactly one consumer of
// those events. Commission rules are stored as a system_configs version
// (internal/sysconfig) under key "commission_rules". Payout batches and
// refund approvals are commands published over the outbox: this service
// holds no connection to telemed_payment and cannot move money directly
// (ADR-004) -- every route here is also gated to finance/super_admin only
// (internal/rbac.GroupFinance), on top of the general admin role check.
package finance

import (
	"time"

	"github.com/google/uuid"
)

// LedgerEntry is a read-only view over payments_projection.
type LedgerEntry struct {
	PaymentID       uuid.UUID
	AppointmentID   uuid.UUID
	DoctorID        uuid.UUID
	PatientID       uuid.UUID
	AmountCents     int64
	CommissionCents int64
	Currency        string
	Status          string
	Provider        string
	OccurredAt      time.Time
}

type LedgerFilter struct {
	Status   string
	DoctorID uuid.UUID
	From     time.Time
	To       time.Time
	Page     int
	PerPage  int
}

// CommissionRule is the JSON shape stored under system_configs key
// "commission_rules": a specialty -> percentage map plus a default, matching
// the SDD's own example ({specialty: GP, commission: 20}).
type CommissionRule struct {
	DefaultPercent int            `json:"default_percent"`
	BySpecialty    map[string]int `json:"by_specialty,omitempty"`
}
