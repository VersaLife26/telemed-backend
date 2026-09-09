package analytics

import (
	"encoding/json"
	"time"

	"telemed/internal/platform/events"
)

// Producer-spelling compatibility, scoped to exactly the fields this
// projection reads and no others.
//
// telemed-payment-service has not yet adopted the canonical payloads in
// internal/platform/events/payloads.go. It still publishes its own
// PaymentEvent/RefundEvent shapes, which agree with the canonical ones on
// every field this projection uses except the event timestamp: it sends a
// single `occurred_at` where the canonical types declare `succeeded_at`,
// `failed_at` and `refunded_at`.
//
// That one difference is not cosmetic here. occurred_at is the column
// payments_projection orders last-write-wins on, AND the column
// revenue_daily buckets by. Decoding it as the zero time would file every
// payment on the platform under year 1 -- the revenue dashboard would read
// zero for every real day, the daily rollup would be silently wrong, and
// nothing would error, because encoding/json does not complain about a field
// nobody sent.
//
// Deliberately NOT a private payload struct: it does not redeclare the event.
// It names one legacy spelling and is consulted only when the canonical field
// is absent, so the canonical spelling already wins and this file can be
// deleted the moment payment-service migrates.
type legacyPaymentTimestamp struct {
	OccurredAt time.Time `json:"occurred_at"`
}

// resolveOccurredAt picks the timestamp to file this payment under.
//
// Order: the canonical per-subject field, then the producer's current
// `occurred_at`, then the envelope's own publish time. The last resort is a
// real fallback and not a silent one -- the caller logs when it is used --
// because "when the relay drained the outbox" is not the same fact as "when
// the money moved", and under a backlog they land on different days.
func resolveOccurredAt(env events.Envelope, canonical time.Time) (at time.Time, source string) {
	if !canonical.IsZero() {
		return canonical, ""
	}
	var legacy legacyPaymentTimestamp
	if err := json.Unmarshal(env.Payload, &legacy); err == nil && !legacy.OccurredAt.IsZero() {
		return legacy.OccurredAt, "occurred_at"
	}
	return env.OccurredAt, "envelope.occurred_at"
}
