// Package analytics answers "how is my practice doing" for the signed-in
// doctor, from a projection this service maintains locally off the event bus.
//
// WHY IT IS A PROJECTION
// The numbers doctor-app screen 9 renders live in four services: appointments
// in telemed_scheduling, consultations in telemed_consultation, payments in
// telemed_payment, and reviews here. ADR-004 forbids joining across those
// databases, and answering the screen with three synchronous fan-out calls
// would make a doctor's morning dashboard fail whenever any one of the three is
// having a bad day.
//
// So this package consumes the facts as they happen and folds them into local
// rollups, exactly as internal/availability already does for search. It is the
// second instance of that pattern in this service, deliberately shaped the same
// way, so an operator debugging one recognises the other.
//
// IDEMPOTENCE AND ORDERING
// JetStream is at-least-once with no cross-delivery ordering guarantee. Two
// consequences drive the whole design:
//
//   - A counter that increments per event is unsafe. A redelivery is
//     indistinguishable from a second real appointment, so `sessions` would
//     drift upward forever with nothing to reconcile it against.
//   - "Have I seen this envelope id" is the wrong question. It makes a
//     redelivery a no-op but does nothing about an OLD event arriving after a
//     newer one for the same aggregate.
//
// Both are solved the same way availability solves them: every event lands as a
// FACT ROW keyed on its natural business key, upserted last-writer-wins by the
// PRODUCER's timestamp, and the rollups are recomputed from the facts rather
// than incremented. Replay the entire stream in any order and the answer is the
// same.
package analytics

import (
	"time"

	"github.com/google/uuid"
)

// Outcome is the terminal state of an appointment, as this projection records
// it. It is deliberately narrower than scheduling-service's appointment status
// machine: pending_payment and confirmed are not outcomes, they are waiting.
type Outcome string

const (
	// OutcomeCompleted is a consultation that happened.
	OutcomeCompleted Outcome = "completed"
	// OutcomeNoShow is a patient who never joined.
	OutcomeNoShow Outcome = "no_show"
	// OutcomeCancelled covers every cancellation, whoever initiated it --
	// patient, doctor, the unpaid-booking sweeper, or an administrator.
	OutcomeCancelled Outcome = "cancelled"
)

// Valid reports whether o is an outcome the database will accept.
func (o Outcome) Valid() bool {
	switch o {
	case OutcomeCompleted, OutcomeNoShow, OutcomeCancelled:
		return true
	default:
		return false
	}
}

// AppointmentFact is one row of doctor_appointment_fact.
type AppointmentFact struct {
	AppointmentID uuid.UUID
	DoctorID      uuid.UUID
	StartAt       time.Time
	LocalDate     time.Time
	DayOfWeek     int
	HourOfDay     int
	Outcome       Outcome
	LastEventID   uuid.UUID
	LastEventAt   time.Time
}

// ConsultationFact is one row of doctor_consultation_fact.
type ConsultationFact struct {
	ConsultationID  uuid.UUID
	DoctorID        uuid.UUID
	AppointmentID   uuid.UUID
	EndedAt         time.Time
	LocalDate       time.Time
	DurationSeconds int
	EndReason       string
	LastEventID     uuid.UUID
	LastEventAt     time.Time
}

// PaymentFact is one row of doctor_payment_fact. Money is cents, always.
type PaymentFact struct {
	PaymentID       uuid.UUID
	DoctorID        uuid.UUID
	AppointmentID   uuid.UUID
	SucceededAt     time.Time
	LocalDate       time.Time
	GrossCents      int64
	CommissionCents int64
	NetCents        int64
	Currency        string
	LastEventID     uuid.UUID
	LastEventAt     time.Time
}

// PayoutFact is one row of doctor_payout_fact: money that actually reached the
// doctor's bank, and the period it settled.
type PayoutFact struct {
	PayoutID    uuid.UUID
	DoctorID    uuid.UUID
	AmountCents int64
	Currency    string
	PeriodStart time.Time
	PeriodEnd   time.Time
	TransferID  string
	SentAt      time.Time
	LastEventID uuid.UUID
	LastEventAt time.Time
}

// DailyRow is one day of doctor_analytics_daily.
//
// ConsultationSeconds is a SUM and ConsultationCount a COUNT, kept separate
// rather than stored pre-divided. Averaging a stored average over a date range
// is wrong -- it weights a day with one consultation the same as a day with
// twenty -- and keeping both components is the only way the range endpoint can
// divide exactly once, at the end.
type DailyRow struct {
	Date                time.Time
	CompletedCount      int
	NoShowCount         int
	CancelledCount      int
	ConsultationCount   int
	ConsultationSeconds int64
	GrossCents          int64
	CommissionCents     int64
	NetCents            int64
	Currency            string
	PaymentCount        int
}

// HourBucket is one hour-of-week cell of doctor_peak_hours.
type HourBucket struct {
	DayOfWeek      int // 0 = Sunday .. 6 = Saturday, matching Go's time.Weekday
	HourOfDay      int
	BookingCount   int
	CompletedCount int
	NoShowCount    int
	CancelledCount int
}

// Summary is what GET /doctors/me/analytics answers, before it is shaped for
// the wire.
//
// Every "rate" here is a FRACTION in [0,1], never a percentage. The doctor app
// already renders the doctor profile's no_show_rate as
// `(noShowRate * 100).toStringAsFixed(1)`, so returning 9.68 where it expects
// 0.0968 would silently show a 968% no-show rate. One convention, stated once.
type Summary struct {
	From time.Time
	To   time.Time

	CompletedCount int
	NoShowCount    int
	CancelledCount int

	// AverageRating is over reviews written INSIDE the window, and is zero when
	// there are none. LifetimeRating is the doctors.rating aggregate the
	// marketplace shows. Both are returned because they answer different
	// questions and a doctor comparing them is the point: "am I doing better
	// this month than I usually do".
	AverageRating       float64
	ReviewCount         int
	LifetimeRating      float64
	LifetimeReviewCount int

	ConsultationCount   int
	ConsultationSeconds int64

	Daily []DailyRow
}

// TotalAppointments is every appointment that reached a terminal state in the
// window.
func (s Summary) TotalAppointments() int {
	return s.CompletedCount + s.NoShowCount + s.CancelledCount
}

// CompletionRate is completed over every terminal appointment, in [0,1].
func (s Summary) CompletionRate() float64 {
	total := s.TotalAppointments()
	if total == 0 {
		return 0
	}
	return float64(s.CompletedCount) / float64(total)
}

// NoShowRate is no-shows over appointments the patient was expected to ATTEND,
// i.e. completed plus no-show. Cancellations are excluded from the denominator
// on purpose: a cancellation is a patient who told somebody, often days ahead,
// and counting it as an attended appointment would dilute the one number that
// is supposed to measure people who simply did not turn up.
//
// This is the same definition scheduling-service's per-patient
// NoShowStats.NoShowRate uses for the prepayment rule, and the two must not
// drift -- a doctor and an administrator looking at "no-show rate" have to be
// looking at the same quantity.
func (s Summary) NoShowRate() float64 {
	attended := s.CompletedCount + s.NoShowCount
	if attended == 0 {
		return 0
	}
	return float64(s.NoShowCount) / float64(attended)
}

// CancellationRate is cancellations over every terminal appointment, in [0,1].
func (s Summary) CancellationRate() float64 {
	total := s.TotalAppointments()
	if total == 0 {
		return 0
	}
	return float64(s.CancelledCount) / float64(total)
}

// AverageConsultationSeconds is the mean length of a COMPLETED call. It divides
// once, at the end, from the summed components.
func (s Summary) AverageConsultationSeconds() float64 {
	if s.ConsultationCount == 0 {
		return 0
	}
	return float64(s.ConsultationSeconds) / float64(s.ConsultationCount)
}

// PeakHours is what GET /doctors/me/analytics/peak-hours answers.
//
// It is LIFETIME, not windowed, and that is the right shape for the question:
// "which hours am I busiest" is a claim about a doctor's practice, and slicing
// it to the last thirty days makes a Tuesday-morning clinic disappear from the
// chart because the doctor happened to be on leave that Tuesday.
type PeakHours struct {
	// Buckets holds only the hour-of-week cells that have any bookings at all,
	// ordered by day then hour. A 168-cell dense array would be mostly zeroes:
	// a doctor works perhaps twenty of those hours.
	Buckets []HourBucket
	// ByHourOfDay is the same data collapsed across weekdays, always 24
	// entries indexed 0..23 so a client can use it as a dense array.
	ByHourOfDay [24]HourBucket
	Total       int
}

// Busiest returns the hour-of-week cell with the most bookings, and whether
// there is one at all.
func (p PeakHours) Busiest() (HourBucket, bool) {
	var best HourBucket
	found := false
	for _, b := range p.Buckets {
		if !found || b.BookingCount > best.BookingCount {
			best, found = b, true
		}
	}
	return best, found
}

// CurrencyTotals is one currency's slice of an earnings window.
//
// Earnings are grouped by currency rather than summed blindly because adding
// cents across currencies produces a number that is not money. The platform is
// LKR-only today, so there is exactly one of these in practice -- but "there is
// only ever one" is the assumption that makes the first multi-currency doctor a
// silent accounting bug rather than an obvious one.
type CurrencyTotals struct {
	Currency        string
	GrossCents      int64
	CommissionCents int64
	NetCents        int64
	PaymentCount    int
	// PaidCents is the part of NetCents that a payout has actually settled.
	PaidCents int64
}

// UnpaidCents is what the doctor has earned in this window and not yet been
// paid.
func (c CurrencyTotals) UnpaidCents() int64 { return c.NetCents - c.PaidCents }

// PayoutStatus is the coarse answer the earnings screen renders as a badge.
type PayoutStatus string

const (
	// PayoutStatusNoEarnings is a window with nothing settled in it. It is
	// distinct from "not paid": a doctor with no consultations has not been
	// left unpaid, and a badge saying so would be alarming and wrong.
	PayoutStatusNoEarnings PayoutStatus = "no_earnings"
	// PayoutStatusPending is earned, none of it paid out yet.
	PayoutStatusPending PayoutStatus = "pending"
	// PayoutStatusPartial is some paid, some still owed.
	PayoutStatusPartial PayoutStatus = "partially_paid"
	// PayoutStatusPaid is every day in the window covered by a settled payout.
	PayoutStatusPaid PayoutStatus = "paid"
)

// Status derives the badge from the amounts.
func (c CurrencyTotals) Status() PayoutStatus {
	switch {
	case c.NetCents == 0:
		return PayoutStatusNoEarnings
	case c.PaidCents == 0:
		return PayoutStatusPending
	case c.PaidCents >= c.NetCents:
		return PayoutStatusPaid
	default:
		return PayoutStatusPartial
	}
}

// Earnings is what GET /doctors/me/earnings answers.
type Earnings struct {
	From time.Time
	To   time.Time

	// ByCurrency is ordered by gross descending, so ByCurrency[0] is the
	// currency the doctor actually works in.
	ByCurrency []CurrencyTotals
	// Payouts are the settlements whose period overlaps the window, newest
	// first.
	Payouts []PayoutFact
	Daily   []DailyRow
}

// Primary returns the currency the doctor earns most in, which is the one the
// summary tiles render. The second return is false for a doctor with no settled
// payments in the window.
func (e Earnings) Primary() (CurrencyTotals, bool) {
	if len(e.ByCurrency) == 0 {
		return CurrencyTotals{}, false
	}
	return e.ByCurrency[0], true
}
