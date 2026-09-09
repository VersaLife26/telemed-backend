package scheduling

// PHI containment for the event bus.
//
// F20(d): CancelAppointment put up to 500 characters of caller-authored free
// text on events.AppointmentCancelled. That payload does not stop at the
// consumer that wanted it. It is written to outbox_events.payload and kept
// there indefinitely, fans out on the shared JetStream stream to every
// consumer on the platform (analytics in admin-service, analytics in
// doctor-service, teardown in consultation-service, notification), and is
// rendered verbatim into notifications.body by
// telemed-notification-service/internal/notification/consumer.go. Five hundred
// characters of free text explaining why a medical appointment was cancelled
// reliably contains clinical content -- "chemo moved to Thursday", "the
// bleeding stopped" -- and none of those consumers asked for a symptom.
//
// events/payloads.go states the policy this violated: "this event fans out to
// every consumer, and PHI-adjacent identifiers should be fetched deliberately,
// not broadcast."
//
// # Why the grammar allowlist was not enough
//
// The first fix filtered by SHAPE: lower snake_case, no spaces, no
// punctuation, at most 40 characters, on the reasoning that machine codes look
// like that and human prose does not. Prose does not. SINGLE WORDS DO.
//
//	eventReasonCode("chemotherapy") -> "chemotherapy"
//	eventReasonCode("miscarriage")  -> "miscarriage"
//	eventReasonCode("hiv")          -> "hiv"
//
// A one-word answer is exactly what a patient types into a box labelled "why
// are you cancelling?", and a single word is the most diagnostic thing they
// can write. The filter caught sentences and let through diagnoses.
//
// There is no denylist that fixes this -- the set of clinical words is not
// finite, and it spans Sinhala and Tamil transliterations besides. So the
// containment is inverted: instead of asking whether a string LOOKS
// machine-authored, this file holds the closed set of reason codes the service
// actually authors, and everything else becomes "". A patient's text cannot
// pass a membership test against a list it is not on, whatever shape it has.
//
// The free text is not lost. It stays in appointments.cancellation_reason,
// where the parties to the appointment can read it over the API and an
// investigator can find it, scoped to the one row it belongs to.

// The reason codes this service authors. Adding one here is a deliberate act
// with this file's comment attached; that is the point. A new code must be a
// machine constant defined in the service -- never a value that reached the
// process from a request body, an event payload or a provider response.
const (
	// paymentFailedReason is the fallback the payment.failed consumer
	// supplies. The provider's own failure TEXT is not broadcast: "card
	// declined for J. Perera" is a real Stripe string and names a patient.
	paymentFailedReason = "payment_failed"
	// holidayLiftedReason is stamped on slots put back on sale when a doctor's
	// holiday is withdrawn.
	holidayLiftedReason = "holiday_lifted"
	// PatientRequestedReason is the one code a CLIENT may select, and it is on
	// the list for exactly that reason: a cancellation UI offers a picklist,
	// and "the patient asked to cancel" is the ordinary answer. It carries no
	// clinical content -- it says a cancellation happened, which the event
	// already says by existing.
	//
	// Membership is what makes this safe, not the shape of the string. The
	// client may send this token and have it broadcast; it may send
	// "chemotherapy", which is the same shape, and have it dropped.
	PatientRequestedReason = "patient_requested"
)

// broadcastableReasons is the closed set. A string not in it is not a reason
// code, regardless of how much it looks like one.
var broadcastableReasons = map[string]struct{}{
	HolidayLeaveReason:   {}, // "doctor_on_leave"
	slotWithdrawalReason: {}, // "holiday"
	paymentFailedReason:  {},
	holidayLiftedReason:  {},

	// The only client-selectable member. Every other value a caller can put in
	// the reason field is dropped.
	PatientRequestedReason: {},
}

// eventReasonCode returns s when it is one of the codes above, and "" for
// everything else -- including every string a caller invents.
//
// "" is the safe answer because Reason carries `omitempty` on both
// events.AppointmentCancelled and events.SlotReleased, so a dropped reason
// does not appear on the wire at all. The FIELD stays for compatibility --
// removing it would need a new subject under the versioning rule in
// CONTRACTS.md, and consumers that steer on the codes above still get them.
//
// Call it at the outbox.Enqueue site, not at the caller, so the containment
// lives where the fan-out happens and cannot be forgotten by a new caller.
func eventReasonCode(s string) string {
	if _, ok := broadcastableReasons[s]; ok {
		return s
	}
	return ""
}
