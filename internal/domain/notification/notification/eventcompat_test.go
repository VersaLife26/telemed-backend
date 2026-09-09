package notification

import (
	"testing"

	"github.com/google/uuid"

	"telemed/internal/platform/events"
)

// TestApplyPaymentFailedCompat covers the migration window in which
// telemed-payment-service still publishes its own PaymentEvent shape, which
// spells the failure reason `failure_reason` rather than the canonical
// `reason`.
//
// Without the shim the patient is told their payment failed and not told why
// -- the one sentence in that message that lets them fix it -- and nothing
// errors, because encoding/json leaves an absent field as "".
func TestApplyPaymentFailedCompat(t *testing.T) {
	newEnv := func(t *testing.T, payload any) events.Envelope {
		t.Helper()
		env, err := events.NewEnvelope(events.SubjectPaymentFailed, "test", uuid.NewString(), payload)
		if err != nil {
			t.Fatalf("build envelope: %v", err)
		}
		return env
	}

	t.Run("canonical reason wins and reports no fallback", func(t *testing.T) {
		env := newEnv(t, map[string]any{"reason": "card_declined", "failure_reason": "something_else"})
		p := events.PaymentFailed{Reason: "card_declined"}
		if from := applyPaymentFailedCompat(env, &p); from != "" {
			t.Errorf("fallback source = %q, want empty", from)
		}
		if p.Reason != "card_declined" {
			t.Errorf("Reason = %q, want the canonical value untouched", p.Reason)
		}
	})

	t.Run("legacy failure_reason fills an empty canonical reason", func(t *testing.T) {
		env := newEnv(t, map[string]any{"failure_reason": "insufficient_funds"})
		var p events.PaymentFailed
		from := applyPaymentFailedCompat(env, &p)
		if from != "failure_reason" {
			t.Errorf("fallback source = %q, want %q so the outstanding migration is logged", from, "failure_reason")
		}
		if p.Reason != "insufficient_funds" {
			t.Errorf("Reason = %q, want the legacy value carried across", p.Reason)
		}
	})

	t.Run("neither spelling present leaves it empty and says so", func(t *testing.T) {
		env := newEnv(t, map[string]any{"payment_id": uuid.New()})
		var p events.PaymentFailed
		if from := applyPaymentFailedCompat(env, &p); from != "" {
			t.Errorf("fallback source = %q, want empty when there was nothing to fall back to", from)
		}
		if p.Reason != "" {
			t.Errorf("Reason = %q, want empty", p.Reason)
		}
	})
}
