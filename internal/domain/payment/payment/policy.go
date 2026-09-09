package payment

import (
	"fmt"
	"strings"
	"time"
)

// Refund policy, transcribed from the platform documentation §11.5 and §36:
//
//	doctor cancels                → full refund
//	patient cancels  > 2h before  → full refund
//	patient cancels ≤ 2h before   → 50% refund
//	patient no-show               → no refund
//
// The documentation says ">2h full, <2h 50%" and never says what happens at
// exactly two hours. That gap is resolved here, deliberately and in one place:
// exactly 2h refunds in full. The patient is given the boundary because a
// clock-skew dispute over a 50% refund costs more in support time than the
// refund itself, and because "at least two hours' notice" is what a patient
// reading the policy believes it says.
//
// LateCancellationWindow is the sole definition of "late" in this service.
const LateCancellationWindow = 2 * time.Hour

// CancelActor is who initiated the cancellation.
type CancelActor string

const (
	ActorDoctor  CancelActor = "doctor"
	ActorPatient CancelActor = "patient"
	ActorSystem  CancelActor = "system" // ops, an outage, a failed slot
)

// ParseCancelActor maps the string scheduling puts on the event onto our enum,
// defaulting to system for anything unrecognised. An unknown actor must never
// silently become "patient", because that is the one that can cost the patient
// half their money.
func ParseCancelActor(s string) CancelActor {
	switch CancelActor(strings.ToLower(strings.TrimSpace(s))) {
	case ActorDoctor:
		return ActorDoctor
	case ActorPatient:
		return ActorPatient
	default:
		return ActorSystem
	}
}

// RefundDecision is the outcome of applying the policy. It carries the clause
// that produced it so the refund row, the API response and the support agent
// all quote the same sentence.
type RefundDecision struct {
	Percent int
	Reason  RefundReason
	Policy  string
	// NoticeGiven is how much warning the patient gave. Negative means the
	// cancellation arrived after the appointment was due to start.
	NoticeGiven time.Duration
}

// Refundable reports whether any money goes back.
func (d RefundDecision) Refundable() bool { return d.Percent > 0 }

// DecideRefund applies the cancellation policy.
//
// cancelledAt and startAt are both instants; the comparison is done in UTC and
// is therefore immune to the Asia/Colombo presentation layer. noShow overrides
// the actor, because a no-show is recorded after the fact by the system even
// though the patient is nominally the one who did not attend.
func DecideRefund(actor CancelActor, noShow bool, cancelledAt, startAt time.Time) RefundDecision {
	notice := startAt.Sub(cancelledAt)

	if noShow {
		return RefundDecision{
			Percent:     0,
			Reason:      ReasonNoShow,
			Policy:      "no_show: patient did not attend, no refund",
			NoticeGiven: notice,
		}
	}

	switch actor {
	case ActorDoctor:
		// The doctor cancelling is never the patient's fault, whenever it
		// happens. There is no late-cancellation penalty on this branch.
		return RefundDecision{
			Percent:     100,
			Reason:      ReasonDoctorCancelled,
			Policy:      "doctor_cancelled: full refund regardless of notice",
			NoticeGiven: notice,
		}

	case ActorSystem:
		// Platform-initiated cancellation: outage, failed slot generation,
		// ops intervention. The patient bears none of it.
		return RefundDecision{
			Percent:     100,
			Reason:      ReasonAdminOverride,
			Policy:      "system_cancelled: full refund, platform at fault",
			NoticeGiven: notice,
		}

	case ActorPatient:
		if notice >= LateCancellationWindow {
			return RefundDecision{
				Percent:     100,
				Reason:      ReasonPatientCancelledEarly,
				Policy:      fmt.Sprintf("patient_cancelled_early: at least %s notice, full refund", LateCancellationWindow),
				NoticeGiven: notice,
			}
		}
		return RefundDecision{
			Percent:     50,
			Reason:      ReasonPatientCancelledLate,
			Policy:      fmt.Sprintf("patient_cancelled_late: less than %s notice, 50%% refund", LateCancellationWindow),
			NoticeGiven: notice,
		}

	default:
		// Unreachable: ParseCancelActor collapses everything else to system.
		// Falling back to a full refund is the safe direction to be wrong in --
		// over-refunding is a reconcilable accounting error, under-refunding is
		// a regulator's complaint.
		return RefundDecision{
			Percent:     100,
			Reason:      ReasonAdminOverride,
			Policy:      "unknown_actor: full refund by default",
			NoticeGiven: notice,
		}
	}
}

// RefundDecisionFromEvent adopts the decision scheduling already made.
//
// Scheduling owns the cancellation policy. It knows the appointment's real
// start time, who cancelled, and how much notice was given, and it wrote its
// conclusion onto appointment.cancelled as refund_policy and refund_percent.
// Re-deriving that here would put the policy in two places, and two copies of a
// business rule diverge -- the only question is when. The first support ticket
// where the app says "full refund" and the bank statement says 50% is that
// divergence arriving.
//
// ok is false when the event carries no decision, which happens for a producer
// that predates the field. The caller then falls back to DecideRefund, so an
// old event still gets refunded rather than silently ignored.
func RefundDecisionFromEvent(policy string, percent int, actor CancelActor, noShow bool, cancelledAt, startAt time.Time) (RefundDecision, bool) {
	policy = strings.ToLower(strings.TrimSpace(policy))
	if policy == "" {
		return RefundDecision{}, false
	}
	if percent < 0 || percent > 100 {
		// A percentage outside 0-100 is a producer bug, not a policy. Refusing
		// it and falling back is safer than wiring an arbitrary number straight
		// into a money calculation.
		return RefundDecision{}, false
	}

	// The reason string is ours: it is what the refund row, the API response
	// and the support agent all quote, and it must stay in this service's
	// vocabulary. Only the NUMBER comes from scheduling.
	reason := reasonForCancellation(actor, noShow, percent)

	return RefundDecision{
		Percent:     percent,
		Reason:      reason,
		Policy:      fmt.Sprintf("scheduling_policy_%s: %d%% refund as decided by the scheduling service", policy, percent),
		NoticeGiven: startAt.Sub(cancelledAt),
	}, true
}

// reasonForCancellation labels a refund whose percentage was decided elsewhere.
func reasonForCancellation(actor CancelActor, noShow bool, percent int) RefundReason {
	switch {
	case noShow:
		return ReasonNoShow
	case actor == ActorDoctor:
		return ReasonDoctorCancelled
	case actor == ActorSystem:
		return ReasonAdminOverride
	case percent >= 100:
		return ReasonPatientCancelledEarly
	default:
		return ReasonPatientCancelledLate
	}
}

// RefundAmount converts a decision into cents against a specific payment,
// clamped to what is actually still refundable.
func RefundAmount(p Payment, d RefundDecision, mode Rounding) (int64, error) {
	if d.Percent <= 0 {
		return 0, nil
	}
	amount, err := PercentOf(p.AmountCents, d.Percent, mode)
	if err != nil {
		return 0, err
	}
	if remaining := p.RefundableCents(); amount > remaining {
		amount = remaining
	}
	return amount, nil
}
