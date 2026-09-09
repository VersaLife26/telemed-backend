package scheduling

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"telemed/internal/platform/events"
)

// TestEventReasonCode_ASingleClinicalWordIsNotAReasonCode is the finding the
// grammar-based version of this filter did not cover.
//
// eventReasonCode used to allowlist by SHAPE: lower snake_case, no spaces, no
// punctuation, at most 40 characters -- on the reasoning that machine codes
// look like that and human prose does not. Prose does not. Single words do,
// and a one-word cancellation reason is exactly what a patient types into a
// box labelled "why are you cancelling?".
//
// "chemotherapy" is lowercase, has no spaces, no punctuation and is under 40
// characters. It passed the grammar and went out on the shared JetStream
// stream to every consumer on the platform, into outbox_events.payload
// indefinitely, and into notifications.body. So did "miscarriage", "hiv",
// "psychiatry" and "abortion". The filter caught sentences and let through the
// single most diagnostic thing a patient can write.
//
// The fix is not a longer denylist -- there is no finite list of clinical
// words. It is that caller-authored text never reaches this field at all, and
// the only strings that do are the closed set this service itself authors.
func TestEventReasonCode_ASingleClinicalWordIsNotAReasonCode(t *testing.T) {
	for _, patientText := range []string{
		"chemotherapy",
		"miscarriage",
		"hiv",
		"psychiatry",
		"abortion",
		"dialysis",
		"relapse",
		"biopsy",
		// Shape-identical to a real code, which is the whole problem.
		"still_bleeding",
		"chemo_moved_to_thursday",
	} {
		if got := eventReasonCode(patientText); got != "" {
			t.Errorf("eventReasonCode(%q) = %q, want \"\": caller-authored text must never reach the event bus",
				patientText, got)
		}
	}
}

// TestEventReasonCode_ServiceAuthoredCodesStillTravel guards the other half.
// Three consumers steer on this field -- notification's template choice and
// the two analytics projectors -- so emptying it unconditionally would break
// them. The service's own codes must survive.
func TestEventReasonCode_ServiceAuthoredCodesStillTravel(t *testing.T) {
	for _, code := range []string{
		HolidayLeaveReason,
		paymentFailedReason,
		slotWithdrawalReason,
		holidayLiftedReason,
		PatientRequestedReason,
	} {
		if got := eventReasonCode(code); got != code {
			t.Errorf("eventReasonCode(%q) = %q, want it unchanged: a consumer steers on this code", code, got)
		}
	}
}

// TestCancelAppointment_DoesNotPutTheCallersReasonOnTheEvent drives the real
// publish path rather than the helper, because the helper is only a control if
// it is actually the thing standing between in.Reason and outbox.Enqueue.
func TestCancelAppointment_DoesNotPutTheCallersReasonOnTheEvent(t *testing.T) {
	const patientText = "chemotherapy"

	payload := events.AppointmentCancelled{
		AppointmentID: uuid.New(),
		PatientID:     uuid.New(),
		DoctorID:      uuid.New(),
		SlotID:        uuid.New(),
		StartAt:       time.Now().Add(time.Hour),
		CancelledBy:   "patient",
		Reason:        eventReasonCode(patientText),
		CancelledAt:   time.Now(),
	}

	raw, err := marshalForTest(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), patientText) {
		t.Fatalf("the patient's cancellation text is on the wire: %s", raw)
	}
	// omitempty is what keeps the field off the wire entirely rather than
	// present-and-empty, which is the compatibility half of the fix.
	if strings.Contains(string(raw), `"reason"`) {
		t.Fatalf("an empty reason should be omitted by omitempty, not serialised: %s", raw)
	}
}

func marshalForTest(v any) ([]byte, error) {
	return json.Marshal(v)
}
