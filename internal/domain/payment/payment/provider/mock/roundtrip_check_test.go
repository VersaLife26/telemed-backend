package mockprovider

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"telemed/internal/domain/payment/payment"
)

// TestSignVerifyRoundTrip checks the two halves of the mock rail agree. If
// Sign and VerifyWebhook disagree, no local end-to-end payment can ever
// complete, which defeats the rail's only purpose.
func TestSignVerifyRoundTrip(t *testing.T) {
	const secret = "whsec_roundtrip"
	body := []byte(`{"id":"evt_1","type":"payment.succeeded","amount_cents":450000}`)

	p := New(Config{WebhookSecret: secret})
	h := http.Header{}
	h.Set(SignatureHeader, Sign(secret, time.Now(), body))

	ev, err := p.VerifyWebhook(context.Background(), h, body)
	if err != nil {
		t.Fatalf("Sign output rejected by VerifyWebhook: %v", err)
	}
	if ev.EventID != "evt_1" {
		t.Errorf("EventID = %q, want evt_1", ev.EventID)
	}
}

// TestMalformedBodyIsNotReportedAsASignatureFailure pins a fix for a genuinely
// expensive misdiagnosis.
//
// A body that authenticates cleanly but cannot be read used to return
// ErrSignatureInvalid, which the handler turned into 401 SIGNATURE_INVALID.
// Anyone debugging that goes after the crypto -- rotating secrets, re-deriving
// the HMAC, suspecting clock skew -- when the real fault is a field name. It
// cost an hour here against our own mock rail; against a live provider at 2am
// it would cost considerably more.
func TestMalformedBodyIsNotReportedAsASignatureFailure(t *testing.T) {
	const secret = "whsec_roundtrip"
	p := New(Config{WebhookSecret: secret})

	for name, body := range map[string]string{
		"valid JSON, no id field": `{"type":"payment.succeeded"}`,
		"not JSON at all":         `this is not json`,
	} {
		t.Run(name, func(t *testing.T) {
			h := http.Header{}
			h.Set(SignatureHeader, Sign(secret, time.Now(), []byte(body)))

			_, err := p.VerifyWebhook(context.Background(), h, []byte(body))
			if err == nil {
				t.Fatal("expected an error")
			}
			if errors.Is(err, payment.ErrSignatureInvalid) {
				t.Errorf("a correctly signed body reported as a SIGNATURE failure: %v", err)
			}
			if !errors.Is(err, payment.ErrWebhookMalformed) {
				t.Errorf("want ErrWebhookMalformed, got %v", err)
			}
		})
	}
}
