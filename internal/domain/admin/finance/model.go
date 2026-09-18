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
	"encoding/json"
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

// RefundRequest is one row of finance_refund_requests — the queue the
// payments console already renders.
type RefundRequest struct {
	ID            uuid.UUID
	PaymentID     uuid.UUID
	AppointmentID uuid.UUID
	DisputeID     uuid.UUID
	AmountCents   int64
	Currency      string
	Reason        string
	Status        string
	RequestedAt   time.Time
	DecidedAt     *time.Time
	DecidedBy     uuid.UUID
}

// PayoutBatch is one settlement window as the console lists it.
type PayoutBatch struct {
	ID          uuid.UUID
	Status      string
	DoctorCount int
	TotalCents  int64
	Currency    string
	CreatedAt   time.Time
	CompletedAt *time.Time
}

// CommissionRule is the JSON shape stored under system_configs key
// "commission_rules": a specialty -> percentage map plus a default, matching
// the SDD's own example ({specialty: GP, commission: 20}).
type CommissionRule struct {
	DefaultPercent int            `json:"default_percent"`
	BySpecialty    map[string]int `json:"by_specialty,omitempty"`
}

// editorCommissionSet is the JSON the admin console edits. Both shapes are
// accepted on the wire so a PUT through /finance/commission-rules and a PUT
// through /configs/commission_rules stay interchangeable.
type editorCommissionSet struct {
	DefaultCommissionPercent int `json:"default_commission_percent"`
	Rules                    []struct {
		Specialty         string `json:"specialty"`
		CommissionPercent int    `json:"commission_percent"`
	} `json:"rules"`
}

func DecodeCommissionValue(raw []byte) (CommissionRule, error) {
	var rule CommissionRule
	if err := json.Unmarshal(raw, &rule); err != nil {
		return CommissionRule{}, err
	}
	if rule.DefaultPercent != 0 || len(rule.BySpecialty) > 0 {
		return rule, nil
	}
	var editor editorCommissionSet
	if err := json.Unmarshal(raw, &editor); err != nil {
		return CommissionRule{}, err
	}
	rule.DefaultPercent = editor.DefaultCommissionPercent
	if len(editor.Rules) == 0 {
		return rule, nil
	}
	rule.BySpecialty = make(map[string]int, len(editor.Rules))
	for _, item := range editor.Rules {
		if item.Specialty == "" {
			continue
		}
		rule.BySpecialty[item.Specialty] = item.CommissionPercent
	}
	return rule, nil
}

func EditorCommissionSet(rule CommissionRule) map[string]any {
	rules := make([]map[string]any, 0, len(rule.BySpecialty))
	for specialty, percent := range rule.BySpecialty {
		rules = append(rules, map[string]any{
			"specialty":          specialty,
			"commission_percent": percent,
		})
	}
	return map[string]any{
		"default_commission_percent": rule.DefaultPercent,
		"rules":                      rules,
	}
}
