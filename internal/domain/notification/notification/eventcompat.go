package notification

import (
	"encoding/json"

	"telemed/internal/platform/events"
)

// Producer-spelling compatibility, scoped to exactly the fields this service
// reads and no others.
//
// telemed-payment-service has not yet adopted the canonical payloads in
// internal/platform/events/payloads.go. It still publishes its own
// PaymentEvent shape, which agrees with the canonical one on every field this
// service uses except the failure reason: it sends `failure_reason` where
// events.PaymentFailed declares `reason`.
//
// That difference is invisible without this shim. encoding/json does not
// error on a field nobody sent, so events.PaymentFailed.Reason decodes as "",
// the payment_failed template renders "Your payment of Rs. 2,500.00 failed. "
// with the explanation missing, and the patient is told their payment failed
// without being told why -- which is the single most useful sentence in that
// message.
//
// Deliberately NOT a private payload struct: it does not redeclare the event,
// it names one legacy spelling and fills the canonical field only when the
// canonical field is absent. Delete this file when payment-service publishes
// events.PaymentFailed; the canonical spelling already wins, so removal is
// safe the moment the producer migrates.
type legacyPaymentFields struct {
	FailureReason string `json:"failure_reason"`
}

// applyPaymentFailedCompat fills Reason from the producer's current spelling
// when the canonical field was absent. It returns the name of the field it had
// to fall back on, or "" when the payload was already canonical, so the caller
// can record that the migration is still outstanding.
func applyPaymentFailedCompat(env events.Envelope, p *events.PaymentFailed) string {
	if p.Reason != "" {
		return ""
	}
	var legacy legacyPaymentFields
	// A decode failure here is not actionable: the canonical decode already
	// succeeded, so the payload is valid JSON and simply lacks the field.
	if err := json.Unmarshal(env.Payload, &legacy); err != nil {
		return ""
	}
	if legacy.FailureReason == "" {
		return ""
	}
	p.Reason = legacy.FailureReason
	return "failure_reason"
}
