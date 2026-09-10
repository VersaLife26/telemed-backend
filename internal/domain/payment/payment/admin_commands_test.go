package payment

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"telemed/internal/platform/events"
)

// admin.refund_approved and admin.payout_batch_requested are the two commands
// that move money on someone else's authority. Both fail silently if this
// service stops listening: the administrator sees a success, the admin audit
// log records the approval, and the patient's money never moves.

func TestConsumerSubscribesToAdminCommands(t *testing.T) {
	subscribed := map[events.Subject]bool{}
	for _, s := range subjects() {
		subscribed[s] = true
	}

	for _, want := range []events.Subject{
		events.SubjectAdminRefundApproved,
		events.SubjectAdminPayoutBatchRequested,
	} {
		if !subscribed[want] {
			t.Errorf("payment does not subscribe to %s: the command would be "+
				"published and retained, the console would report success, and "+
				"no money would move", want)
		}
	}
}

// TestAdminRefundApprovedDecodes pins the wire contract with the admin domain.
// AmountCents is the field that matters most: it is a human overriding the
// cancellation policy, so a tag drift here does not fall back to a smaller
// refund, it falls back to zero.
func TestAdminRefundApprovedDecodes(t *testing.T) {
	paymentID, adminID := uuid.New(), uuid.New()
	env, err := events.NewEnvelope(events.SubjectAdminRefundApproved, "admin-service", paymentID.String(),
		events.AdminRefundApproved{
			PaymentID: paymentID, AmountCents: 4500, Reason: "duplicate charge", AdminID: adminID,
		})
	if err != nil {
		t.Fatalf("build envelope: %v", err)
	}

	var got events.AdminRefundApproved
	if err := env.Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.PaymentID != paymentID {
		t.Errorf("payment_id = %s, want %s", got.PaymentID, paymentID)
	}
	if got.AmountCents != 4500 {
		t.Errorf("amount_cents = %d, want 4500 -- a zero here silently refunds nothing", got.AmountCents)
	}
	if got.AdminID != adminID {
		t.Errorf("admin_id = %s, want %s", got.AdminID, adminID)
	}
}

func TestAdminPayoutBatchRequestedDecodes(t *testing.T) {
	from := time.Now().UTC().Add(-24 * time.Hour).Truncate(time.Second)
	to := time.Now().UTC().Truncate(time.Second)
	adminID := uuid.New()

	env, err := events.NewEnvelope(events.SubjectAdminPayoutBatchRequested, "admin-service", "",
		events.AdminPayoutBatchRequested{From: from, To: to, AdminID: adminID})
	if err != nil {
		t.Fatalf("build envelope: %v", err)
	}

	var got events.AdminPayoutBatchRequested
	if err := env.Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.From.Equal(from) || !got.To.Equal(to) {
		t.Errorf("window = %s..%s, want %s..%s", got.From, got.To, from, to)
	}
	if got.AdminID != adminID {
		t.Errorf("admin_id = %s, want %s", got.AdminID, adminID)
	}
}

// TestRefundReasonAdminOverrideIsValid guards the value the handler passes.
// RefundReason is a closed set the finance report groups by, and Refund
// rejects anything outside it -- so an invalid constant here would turn every
// approved refund into a permanent error that the handler then drops.
func TestRefundReasonAdminOverrideIsValid(t *testing.T) {
	if !ReasonAdminOverride.Valid() {
		t.Fatal("ReasonAdminOverride is not a valid RefundReason")
	}
}

// TestAdminRefundApprovedCarriesNoPatientIdentifiers keeps the command a
// command. It names a payment and an administrator; the patient it concerns is
// reachable from the payment row. Adding a name or a phone number here would
// put it on the bus for every consumer of the stream.
func TestAdminRefundApprovedCarriesNoPatientIdentifiers(t *testing.T) {
	raw, err := json.Marshal(events.AdminRefundApproved{
		PaymentID: uuid.New(), AmountCents: 1, Reason: "x", AdminID: uuid.New(),
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, forbidden := range []string{"patient_id", "patient_name", "phone", "email"} {
		if _, present := fields[forbidden]; present {
			t.Errorf("admin.refund_approved carries %q: %s", forbidden, raw)
		}
	}
}
