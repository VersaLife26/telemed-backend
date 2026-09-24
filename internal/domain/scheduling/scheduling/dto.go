package scheduling

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// Wire types. The rule at this boundary: instants go out in UTC as RFC3339 and
// again as the Colombo wall-clock the patient actually reads, because a
// TypeScript client that has to do the conversion will eventually do it wrong. Storage stays UTC; only this file knows about Asia/Colombo.

// SlotDTO is one bookable slot as the API returns it.
type SlotDTO struct {
	ID       uuid.UUID `json:"id"`
	DoctorID uuid.UUID `json:"doctor_id"`
	StartAt  time.Time `json:"start_at"`
	EndAt    time.Time `json:"end_at"`
	// StartAtLocal and EndAtLocal are the same instants rendered in the
	// business timezone, offset included.
	StartAtLocal    string `json:"start_at_local"`
	EndAtLocal      string `json:"end_at_local"`
	DurationMinutes int    `json:"duration_minutes"`
	Status          string `json:"status"`
}

// NewSlotDTO renders a slot for the wire.
func NewSlotDTO(s Slot, loc *time.Location) SlotDTO {
	return SlotDTO{
		ID:              s.ID,
		DoctorID:        s.DoctorID,
		StartAt:         s.StartAt.UTC(),
		EndAt:           s.EndAt.UTC(),
		StartAtLocal:    s.StartAt.In(loc).Format(time.RFC3339),
		EndAtLocal:      s.EndAt.In(loc).Format(time.RFC3339),
		DurationMinutes: int(s.EndAt.Sub(s.StartAt).Minutes()),
		Status:          string(s.Status),
	}
}

// AppointmentDTO is an appointment as the API returns it.
//
// Intake is included only for the patient who owns it and the doctor who will
// read it; the list endpoints omit it entirely. That is not a size
// optimisation -- it is symptom and allergy data, and it should travel as
// rarely as it can.
type AppointmentDTO struct {
	ID             uuid.UUID  `json:"id"`
	PatientID      uuid.UUID  `json:"patient_id"`
	DoctorID       uuid.UUID  `json:"doctor_id"`
	SlotID         uuid.UUID  `json:"slot_id"`
	FamilyMemberID *uuid.UUID `json:"family_member_id,omitempty"`

	StartAt      time.Time `json:"start_at"`
	EndAt        time.Time `json:"end_at"`
	StartAtLocal string    `json:"start_at_local"`
	EndAtLocal   string    `json:"end_at_local"`

	Status             string          `json:"status"`
	PrepaymentRequired bool            `json:"prepayment_required"`
	Intake             json.RawMessage `json:"intake,omitempty"`

	// AmountCents is the quote the patient was given at booking, in cents. It
	// is returned so the checkout screen shows the same number the payment
	// service will charge, rather than re-deriving it from the doctor's current
	// fee and disagreeing by the width of one profile edit.
	//
	// omitempty: appointments booked before pricing existed carry no quote, and
	// a rendered "LKR 0.00" would be a lie.
	AmountCents int64  `json:"amount_cents,omitempty"`
	Currency    string `json:"currency,omitempty"`
	Specialty   string `json:"specialty,omitempty"`

	// PatientName is filled only for the treating doctor's own view, so the
	// queue and the call screen say who the visit is with.
	PatientName string `json:"patient_name,omitempty"`

	// VisitPatientName is the person this consultation is for, captured at
	// booking (may differ from the account holder's profile name).
	VisitPatientName string `json:"visit_patient_name,omitempty"`
	VisitPatientDOB  string `json:"visit_patient_dob,omitempty"`
	VisitPatientAge  int    `json:"visit_patient_age,omitempty"`

	VisitPatientSex       string   `json:"visit_patient_sex,omitempty"`
	VisitPatientWeightKg  *float64 `json:"visit_patient_weight_kg,omitempty"`
	VisitPatientAllergies string   `json:"visit_patient_allergies,omitempty"`

	// CounterpartName is the other party's display name: the patient's name
	// for a doctor, the doctor's name for a patient.
	CounterpartName string `json:"counterpart_name,omitempty"`

	PaymentID   *uuid.UUID `json:"payment_id,omitempty"`
	ConfirmedAt *time.Time `json:"confirmed_at,omitempty"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
	NoShowAt    *time.Time `json:"no_show_at,omitempty"`

	CancelledAt        *time.Time `json:"cancelled_at,omitempty"`
	CancelledByRole    string     `json:"cancelled_by_role,omitempty"`
	CancellationReason string     `json:"cancellation_reason,omitempty"`
	RefundPolicy       string     `json:"refund_policy,omitempty"`
	RefundPercent      *int       `json:"refund_percent,omitempty"`

	// CancellableUntil is when a patient cancellation stops earning a full
	// refund. Returning it means the mobile clients render the policy instead of
	// reimplementing it.
	CancellableUntil time.Time `json:"cancellable_until"`

	Version   int       `json:"version"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// NewAppointmentDTO renders an appointment. withIntake must be false for any
// listing and for any caller who is not the patient or the treating doctor.
func NewAppointmentDTO(a Appointment, loc *time.Location, withIntake bool) AppointmentDTO {
	d := AppointmentDTO{
		ID:                    a.ID,
		PatientID:             a.PatientID,
		DoctorID:              a.DoctorID,
		SlotID:                a.SlotID,
		FamilyMemberID:        a.FamilyMemberID,
		StartAt:               a.SlotStartAt.UTC(),
		EndAt:                 a.SlotEndAt.UTC(),
		StartAtLocal:          a.SlotStartAt.In(loc).Format(time.RFC3339),
		EndAtLocal:            a.SlotEndAt.In(loc).Format(time.RFC3339),
		Status:                string(a.Status),
		PrepaymentRequired:    a.PrepaymentRequired,
		AmountCents:           a.AmountCents,
		Currency:              a.Currency,
		Specialty:             a.Specialty,
		PaymentID:             a.PaymentID,
		ConfirmedAt:           a.ConfirmedAt,
		CompletedAt:           a.CompletedAt,
		NoShowAt:              a.NoShowAt,
		CancelledAt:           a.CancelledAt,
		CancelledByRole:       a.CancelledByRole,
		CancellationReason:    a.CancellationReason,
		CancellableUntil:      a.SlotStartAt.Add(-CancellationNoticeWindow).UTC(),
		VisitPatientSex:       a.VisitPatientSex,
		VisitPatientWeightKg:  a.VisitPatientWeightKg,
		VisitPatientAllergies: a.VisitPatientAllergies,
		Version:               a.Version,
		CreatedAt:             a.CreatedAt.UTC(),
		UpdatedAt:             a.UpdatedAt.UTC(),
	}
	if withIntake {
		d.Intake = a.Intake
	}
	if a.VisitPatientName != "" {
		d.VisitPatientName = a.VisitPatientName
	}
	if a.VisitPatientDOB != nil && !a.VisitPatientDOB.IsZero() {
		d.VisitPatientDOB = a.VisitPatientDOB.UTC().Format("2006-01-02")
		d.VisitPatientAge = AgeAtVisit(*a.VisitPatientDOB, a.SlotStartAt, loc)
	}
	if a.RefundPolicy != nil {
		d.RefundPolicy = string(*a.RefundPolicy)
		pct := a.RefundPolicy.Percent()
		d.RefundPercent = &pct
	}
	return d
}

// WaitlistDTO is a waitlist entry as the API returns it.
type WaitlistDTO struct {
	ID            uuid.UUID  `json:"id"`
	PatientID     uuid.UUID  `json:"patient_id"`
	DoctorID      uuid.UUID  `json:"doctor_id"`
	PreferredDate Date       `json:"preferred_date"`
	Status        string     `json:"status"`
	OfferedSlotID *uuid.UUID `json:"offered_slot_id,omitempty"`
	// OfferExpiresAt is the deadline the "you have five minutes" push refers to.
	OfferExpiresAt *time.Time `json:"offer_expires_at,omitempty"`
	OfferCount     int        `json:"offer_count"`
	CreatedAt      time.Time  `json:"created_at"`
}

// NewWaitlistDTO renders a waitlist entry.
func NewWaitlistDTO(w WaitlistEntry) WaitlistDTO {
	return WaitlistDTO{
		ID:             w.ID,
		PatientID:      w.PatientID,
		DoctorID:       w.DoctorID,
		PreferredDate:  w.PreferredDate,
		Status:         string(w.Status),
		OfferedSlotID:  w.OfferedSlotID,
		OfferExpiresAt: w.OfferExpiresAt,
		OfferCount:     w.OfferCount,
		CreatedAt:      w.CreatedAt.UTC(),
	}
}

// RescheduleRequestDTO is a reschedule request as the API returns it.
type RescheduleRequestDTO struct {
	ID                   uuid.UUID  `json:"id"`
	AppointmentID        uuid.UUID  `json:"appointment_id"`
	PatientID            uuid.UUID  `json:"patient_id"`
	DoctorID             uuid.UUID  `json:"doctor_id"`
	OriginalSlotID       uuid.UUID  `json:"original_slot_id"`
	OriginalStartAt      time.Time  `json:"original_start_at"`
	OriginalEndAt        time.Time  `json:"original_end_at"`
	OriginalStartAtLocal string     `json:"original_start_at_local"`
	OriginalEndAtLocal   string     `json:"original_end_at_local"`
	ProposedSlotID       uuid.UUID  `json:"proposed_slot_id"`
	ProposedStartAt      time.Time  `json:"proposed_start_at"`
	ProposedEndAt        time.Time  `json:"proposed_end_at"`
	ProposedStartAtLocal string     `json:"proposed_start_at_local"`
	ProposedEndAtLocal   string     `json:"proposed_end_at_local"`
	Reason               string     `json:"reason,omitempty"`
	Status               string     `json:"status"`
	DecidedBy            *uuid.UUID `json:"decided_by,omitempty"`
	DecidedByRole        string     `json:"decided_by_role,omitempty"`
	DecidedAt            *time.Time `json:"decided_at,omitempty"`
	Version              int        `json:"version"`
	CreatedAt            time.Time  `json:"created_at"`
	UpdatedAt            time.Time  `json:"updated_at"`
}

// NewRescheduleRequestDTO renders a request. Reason is included for every
// party to the booking: the patient has to decide against it, and it is
// already scoped to this row rather than broadcast on the bus.
func NewRescheduleRequestDTO(r RescheduleRequest, loc *time.Location) RescheduleRequestDTO {
	d := RescheduleRequestDTO{
		ID:                   r.ID,
		AppointmentID:        r.AppointmentID,
		PatientID:            r.PatientID,
		DoctorID:             r.DoctorID,
		OriginalSlotID:       r.OriginalSlotID,
		OriginalStartAt:      r.OriginalStartAt.UTC(),
		OriginalEndAt:        r.OriginalEndAt.UTC(),
		OriginalStartAtLocal: r.OriginalStartAt.In(loc).Format(time.RFC3339),
		OriginalEndAtLocal:   r.OriginalEndAt.In(loc).Format(time.RFC3339),
		ProposedSlotID:       r.ProposedSlotID,
		ProposedStartAt:      r.ProposedStartAt.UTC(),
		ProposedEndAt:        r.ProposedEndAt.UTC(),
		ProposedStartAtLocal: r.ProposedStartAt.In(loc).Format(time.RFC3339),
		ProposedEndAtLocal:   r.ProposedEndAt.In(loc).Format(time.RFC3339),
		Reason:               r.Reason,
		Status:               string(r.Status),
		DecidedBy:            r.DecidedBy,
		DecidedByRole:        r.DecidedByRole,
		Version:              r.Version,
		CreatedAt:            r.CreatedAt.UTC(),
		UpdatedAt:            r.UpdatedAt.UTC(),
	}
	if r.DecidedAt != nil {
		at := r.DecidedAt.UTC()
		d.DecidedAt = &at
	}
	return d
}
