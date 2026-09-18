// Package audit is the immutable, tamper-evident record of every
// state-changing action performed through the admin console.
//
// The table this package writes to (audit_logs) is append-only enforced at
// the database level: the connecting role has no UPDATE/DELETE grant, and a
// trigger raises on any attempt regardless of role (see
// migrations/000002_audit_log.up.sql). Every row also carries a SHA-256 hash
// chain (prev_hash/row_hash) so a mutation that somehow bypassed both of
// those -- a superuser disabling the trigger, for instance -- is still
// detectable by walking the chain. See VerifyChain and
// POST /api/v1/admin/audit/verify.
package audit

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ActionExported is the action recorded when someone bulk-exports the audit
// trail. It is a first-class audit action, not a log line: an export is an
// admin act with a subject, an actor and a scope, and the whole point of
// recording it is that it must be visible in the same trail it copied.
const ActionExported = "audit.exported"

// Entry is one persisted audit_logs row.
type Entry struct {
	ID           int64
	ActorID      uuid.UUID
	ActorRole    string
	Action       string
	ResourceType string
	ResourceID   string
	OldValue     json.RawMessage
	NewValue     json.RawMessage
	IP           string
	UserAgent    string
	RequestID    string
	CreatedAt    time.Time
	PrevHash     string
	RowHash      string
}

// Draft is what a service layer stages via Stage(ctx, ...) to describe the
// change it just made. The middleware fills in everything a draft cannot
// know about itself -- who, from where, in which request.
type Draft struct {
	Action       string
	ResourceType string
	ResourceID   string
	OldValue     any
	NewValue     any
}

// ListFilter narrows GET /api/v1/admin/audit and the CSV export.
type ListFilter struct {
	ActorID      uuid.UUID
	Action       string
	ResourceType string
	ResourceID   string
	From         time.Time
	To           time.Time
	Page         int
	PerPage      int
	// OldestFirst flips List from id DESC (the audit page) to id ASC (a
	// single-resource trail, which the console reads top-to-bottom).
	OldestFirst bool
}

// VerifyRequest scopes a chain walk. FromID lets a very large table be
// verified incrementally across several calls instead of one long-running
// scan; Limit caps how many rows a single call inspects.
type VerifyRequest struct {
	FromID int64
	Limit  int
}

// BrokenLink describes the first row at which the chain no longer checks out.
//
// ID is 0 for a finding about the table as a whole rather than about one row
// -- specifically, for the completeness checks that detect a TRUNCATE, which
// removes rows without leaving one behind to point at.
type BrokenLink struct {
	ID     int64  `json:"id"`
	Reason string `json:"reason"`
}

// VerifyResult is the response body of POST /api/v1/admin/audit/verify.
//
// ExpectedEntries/ActualEntries report the completeness check, which is
// whole-table and therefore independent of the FromID/Limit window walked.
// They are what makes truncation visible: linkage alone cannot see rows that
// are simply gone.
type VerifyResult struct {
	Valid           bool        `json:"valid"`
	Checked         int64       `json:"checked"`
	FromID          int64       `json:"from_id"`
	LastID          int64       `json:"last_id,omitempty"`
	NextFromID      *int64      `json:"next_from_id,omitempty"`
	ExpectedEntries int64       `json:"expected_entries"`
	ActualEntries   int64       `json:"actual_entries"`
	Broken          *BrokenLink `json:"broken,omitempty"`
}

// genesisHash is the well-known "no previous row" value the chain starts
// from (see migrations/000002_audit_log.up.sql: audit_chain_state seed row).
var genesisHash = strings.Repeat("0", 64)
