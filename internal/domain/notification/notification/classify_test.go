package notification

import (
	"errors"
	"fmt"
	"testing"
)

func TestClassify_ExplicitTagsWin(t *testing.T) {
	base := errors.New("provider rejected the number")

	if got := Classify(Permanent(base)); got != ErrorClassPermanent {
		t.Errorf("Permanent(err) classified as %q, want %q", got, ErrorClassPermanent)
	}
	if got := Classify(Transient(base)); got != ErrorClassTransient {
		t.Errorf("Transient(err) classified as %q, want %q", got, ErrorClassTransient)
	}
}

func TestClassify_UnrecognisedErrorDefaultsTransient(t *testing.T) {
	// An error nobody classified is treated as retryable: a wrongly-retried
	// permanent error costs a few wasted attempts, but a wrongly-abandoned
	// transient one costs the patient a notification they never get. See
	// the doc comment on Classify.
	plain := errors.New("connection reset by peer")
	if got := Classify(plain); got != ErrorClassTransient {
		t.Errorf("unclassified error = %q, want default %q", got, ErrorClassTransient)
	}
}

func TestClassify_NilErrorHasNoClass(t *testing.T) {
	if got := Classify(nil); got != "" {
		t.Errorf("Classify(nil) = %q, want empty", got)
	}
}

func TestClassify_WrappedErrorsAndErrorsIs(t *testing.T) {
	// The dispatch loop's token-pruning path relies on errors.Is finding
	// ErrTokenUnregistered through a Permanent() wrap and an fmt.Errorf
	// %w chain, exactly the way the FCM provider constructs it.
	wrapped := Permanent(fmt.Errorf("fcm rejected token: %w", ErrTokenUnregistered))

	if got := Classify(wrapped); got != ErrorClassPermanent {
		t.Errorf("wrapped ErrTokenUnregistered classified as %q, want %q", got, ErrorClassPermanent)
	}
	if !errors.Is(wrapped, ErrTokenUnregistered) {
		t.Error("errors.Is should see through Permanent() + fmt.Errorf %w to ErrTokenUnregistered")
	}
}

func TestClassify_NestedPermanentSurvivesFurtherWrapping(t *testing.T) {
	// A caller further wrapping an already-classified error with plain
	// fmt.Errorf %w must not lose the classification -- Classify uses
	// errors.As, which walks the whole Unwrap chain.
	err := fmt.Errorf("send attempt 3: %w", Permanent(errors.New("invalid destination address")))
	if got := Classify(err); got != ErrorClassPermanent {
		t.Errorf("classification lost after further wrapping: got %q, want %q", got, ErrorClassPermanent)
	}
}

func TestPermanentTransient_NilInputReturnsNil(t *testing.T) {
	if Permanent(nil) != nil {
		t.Error("Permanent(nil) should return nil, not a wrapped error")
	}
	if Transient(nil) != nil {
		t.Error("Transient(nil) should return nil, not a wrapped error")
	}
}
