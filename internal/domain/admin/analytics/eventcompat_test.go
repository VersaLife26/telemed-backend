package analytics

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"telemed/internal/platform/events"
)

// TestResolveOccurredAt covers the migration window in which
// telemed-payment-service still publishes its own PaymentEvent shape.
//
// The failure this prevents is quiet and expensive: occurred_at is what
// revenue_daily buckets by, so a zero timestamp files every payment under
// year 1 and the revenue dashboard reads zero for every real day, with no
// error anywhere.
func TestResolveOccurredAt(t *testing.T) {
	canonical := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	legacy := time.Date(2026, 8, 20, 11, 0, 0, 0, time.UTC)
	published := time.Date(2026, 8, 21, 3, 0, 0, 0, time.UTC)

	newEnv := func(t *testing.T, payload any) events.Envelope {
		t.Helper()
		env, err := events.NewEnvelope(events.SubjectPaymentSucceeded, "test", uuid.NewString(), payload)
		if err != nil {
			t.Fatalf("build envelope: %v", err)
		}
		env.OccurredAt = published
		return env
	}

	t.Run("canonical field wins", func(t *testing.T) {
		env := newEnv(t, map[string]any{"succeeded_at": canonical, "occurred_at": legacy})
		at, source := resolveOccurredAt(env, canonical)
		if !at.Equal(canonical) {
			t.Errorf("at = %v, want the canonical %v", at, canonical)
		}
		if source != "" {
			t.Errorf("source = %q, want empty (no fallback used)", source)
		}
	})

	t.Run("falls back to the producer's occurred_at", func(t *testing.T) {
		env := newEnv(t, map[string]any{"occurred_at": legacy})
		at, source := resolveOccurredAt(env, time.Time{})
		if !at.Equal(legacy) {
			t.Errorf("at = %v, want the producer's %v", at, legacy)
		}
		if source != "occurred_at" {
			t.Errorf("source = %q, want %q so the outstanding migration is logged", source, "occurred_at")
		}
	})

	t.Run("last resort is the envelope, never the zero time", func(t *testing.T) {
		env := newEnv(t, map[string]any{"payment_id": uuid.New()})
		at, source := resolveOccurredAt(env, time.Time{})
		if at.IsZero() {
			t.Fatal("resolveOccurredAt returned the zero time; this is what files revenue under year 1")
		}
		if !at.Equal(published) {
			t.Errorf("at = %v, want the envelope's %v", at, published)
		}
		if source != "envelope.occurred_at" {
			t.Errorf("source = %q, want %q", source, "envelope.occurred_at")
		}
	})
}
