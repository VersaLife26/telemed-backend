package scheduling

import (
	"errors"
	"net/http"

	"telemed/internal/platform/httpx"
)

// Domain errors. The service layer returns these; only handler.go and grpc.go
// know how to turn them into an HTTP status or a gRPC code. A repository never
// constructs one of these -- it returns the raw error and lets the service
// classify it.
var (
	// ErrSlotNotFound is a slot id that does not exist, or has been archived.
	ErrSlotNotFound = errors.New("scheduling: slot not found")

	// ErrSlotUnavailable is the honest answer to "somebody else got there
	// first". It covers every way that can happen: the row was already BOOKED,
	// the version guard failed, or the unique index rejected the insert.
	ErrSlotUnavailable = errors.New("scheduling: slot is not available")

	// ErrSlotLocked means another booking attempt holds the Redis early-reject
	// lock. It is advisory: the caller should retry in a moment, and the slot
	// may well still be free. Distinct from ErrSlotUnavailable so the mobile
	// clients can say "one moment" instead of "gone".
	ErrSlotLocked = errors.New("scheduling: slot is locked by another booking")

	// ErrSlotInPast rejects a booking for a slot that has already started.
	ErrSlotInPast = errors.New("scheduling: slot has already started")

	// ErrSlotReserved means the slot is held by a waitlist offer for someone
	// else for the next few minutes.
	ErrSlotReserved = errors.New("scheduling: slot is reserved for a waitlisted patient")

	// ErrAppointmentNotFound is a bad appointment id, or one the caller may not
	// see. Authorization failures deliberately collapse into "not found" so the
	// API cannot be used to enumerate other patients' appointments.
	ErrAppointmentNotFound = errors.New("scheduling: appointment not found")

	// ErrAppointmentNotCancellable is a cancel on something already cancelled
	// or completed.
	ErrAppointmentNotCancellable = errors.New("scheduling: appointment cannot be cancelled")

	// ErrAppointmentAlreadyStarted rejects a cancellation after the
	// consultation window opened.
	ErrAppointmentAlreadyStarted = errors.New("scheduling: appointment has already started")

	// ErrDuplicateBooking is one patient booking the same time twice.
	ErrDuplicateBooking = errors.New("scheduling: patient already has an appointment at this time")

	// ErrWaitlistNotFound is a bad waitlist id or one belonging to someone else.
	ErrWaitlistNotFound = errors.New("scheduling: waitlist entry not found")

	// ErrWaitlistDuplicate is a second join for the same doctor and day.
	ErrWaitlistDuplicate = errors.New("scheduling: already on this waitlist")

	// ErrWaitlistDateInPast rejects joining a waitlist for a day gone by.
	ErrWaitlistDateInPast = errors.New("scheduling: preferred date is in the past")

	// ErrDoctorNotConfigured means no working hours have arrived from
	// doctor-service, so there is nothing to generate.
	ErrDoctorNotConfigured = errors.New("scheduling: doctor has no schedule configuration")

	// ErrDoctorNotPriced means this service holds no usable list price for the
	// doctor, so it cannot quote the booking.
	//
	// It FAILS THE BOOKING, loudly, and that is the entire point. The
	// alternative -- defaulting the amount to zero and booking anyway -- is the
	// defect this error was introduced to make impossible: the slot would be
	// held, the appointment would look confirmed to the patient, and
	// payment-service would refuse the event as unpayable in a log line nobody
	// reads. A patient who cannot be charged must find that out at the moment
	// they try to book, not at the moment they try to join the consultation.
	//
	// Operationally it means doctor.approved has not arrived (or was dropped)
	// for this doctor. See docs/RUNBOOK.md.
	ErrDoctorNotPriced = errors.New("scheduling: no price is available for this doctor")

	// ErrVersionConflict is an optimistic-lock miss. It is internal: callers
	// see ErrSlotUnavailable.
	ErrVersionConflict = errors.New("scheduling: optimistic version conflict")

	// ErrForbidden is a caller acting on somebody else's resource.
	ErrForbidden = errors.New("scheduling: caller may not act on this resource")

	// ErrHolidayHasBookings stops a doctor registering leave over a day that
	// already has patients booked into it, unless they explicitly ask for those
	// patients to be cancelled and refunded.
	//
	// It is a refusal rather than a silent choice because both silent choices
	// are wrong. Registering the leave and leaving the bookings standing tells
	// a doctor they are off while patients still expect them to appear.
	// Registering it and cancelling silently destroys real consultations with
	// nobody deciding to. The doctor is shown the count and asked.
	ErrHolidayHasBookings = errors.New("scheduling: this day already has booked appointments")

	// ErrHolidayNotFound is a holiday id that does not exist, is already
	// lifted, or belongs to another doctor -- or is platform-wide, which no
	// doctor may delete. All four collapse into one error so the endpoint
	// cannot be used to enumerate holiday ids.
	ErrHolidayNotFound = errors.New("scheduling: holiday not found")

	// ErrHolidayDateInPast rejects leave for a day that has already gone.
	ErrHolidayDateInPast = errors.New("scheduling: holiday date is in the past")

	// ErrHolidayDateInvalid is a missing or unparseable date.
	ErrHolidayDateInvalid = errors.New("scheduling: holiday date is required")

	// ErrHolidayRangeInvalid is a list query whose "to" precedes its "from".
	ErrHolidayRangeInvalid = errors.New("scheduling: holiday range ends before it starts")

	// ErrAdminScopeRequired rejects an administrative listing that is not
	// bounded to one doctor and one window.
	//
	// It is a 422 rather than a 403 on purpose: the caller IS authorised to
	// look, they have simply asked for more than the endpoint will ever
	// answer. Saying so plainly is what stops the next person adding an
	// "unfiltered" mode to make their query work.
	ErrAdminScopeRequired = errors.New("scheduling: an administrative listing must name one doctor and a bounded window")
)

// APIError maps a domain error onto the platform's HTTP error envelope. Every
// unmapped error becomes a generic 500, which is what keeps an SQL fragment or
// a patient name from reaching a client through an error string.
func APIError(err error) error {
	switch {
	case err == nil:
		return nil

	case errors.Is(err, ErrSlotUnavailable), errors.Is(err, ErrVersionConflict):
		return httpx.NewError(http.StatusConflict, httpx.CodeSlotUnavailable,
			"that slot has just been taken")

	case errors.Is(err, ErrSlotReserved):
		return httpx.NewError(http.StatusConflict, httpx.CodeSlotUnavailable,
			"that slot is being held for a waitlisted patient")

	case errors.Is(err, ErrSlotLocked):
		return httpx.NewError(http.StatusConflict, httpx.CodeSlotLocked,
			"another booking for this slot is in progress, try again in a moment")

	case errors.Is(err, ErrSlotNotFound):
		return httpx.NewError(http.StatusNotFound, httpx.CodeNotFound, "slot not found")

	case errors.Is(err, ErrSlotInPast):
		return httpx.NewError(http.StatusUnprocessableEntity, httpx.CodeUnprocessable,
			"that slot has already started")

	case errors.Is(err, ErrAppointmentNotFound):
		return httpx.NewError(http.StatusNotFound, httpx.CodeNotFound, "appointment not found")

	case errors.Is(err, ErrAppointmentNotCancellable):
		return httpx.NewError(http.StatusConflict, httpx.CodeConflict,
			"this appointment can no longer be cancelled")

	case errors.Is(err, ErrAppointmentAlreadyStarted):
		return httpx.NewError(http.StatusConflict, httpx.CodeConflict,
			"the consultation has already started")

	case errors.Is(err, ErrDuplicateBooking):
		return httpx.NewError(http.StatusConflict, httpx.CodeConflict,
			"you already have an appointment at this time")

	case errors.Is(err, ErrWaitlistNotFound):
		return httpx.NewError(http.StatusNotFound, httpx.CodeNotFound, "waitlist entry not found")

	case errors.Is(err, ErrWaitlistDuplicate):
		return httpx.NewError(http.StatusConflict, httpx.CodeConflict,
			"you are already on this doctor's waitlist for that day")

	case errors.Is(err, ErrWaitlistDateInPast):
		return httpx.NewError(http.StatusUnprocessableEntity, httpx.CodeUnprocessable,
			"preferred date must be today or later")

	case errors.Is(err, ErrDoctorNotConfigured):
		return httpx.NewError(http.StatusUnprocessableEntity, httpx.CodeUnprocessable,
			"this doctor has not published any working hours yet")

	case errors.Is(err, ErrDoctorNotPriced):
		// 422, not 500: nothing is broken in the request, and nothing is broken
		// in this service. The doctor is genuinely not bookable right now. 503
		// would be wrong too -- retrying in a second will not help; the missing
		// doctor.approved has to arrive first.
		return httpx.NewError(http.StatusUnprocessableEntity, httpx.CodeUnprocessable,
			"this doctor's consultation fee has not been published yet, so the "+
				"booking cannot be priced; please try again shortly")

	case errors.Is(err, ErrHolidayHasBookings):
		// 409 with its own code, not a generic conflict: the client must show
		// the doctor how many patients are affected and offer to cancel them.
		// A generic CONFLICT reads as "retry", and retrying does nothing.
		return httpx.NewError(http.StatusConflict, httpx.CodeHolidayHasBookings,
			"this day already has booked appointments; re-send with "+
				"cancel_booked=true to cancel them and refund the patients in full")

	case errors.Is(err, ErrHolidayNotFound):
		return httpx.NewError(http.StatusNotFound, httpx.CodeNotFound, "holiday not found")

	case errors.Is(err, ErrHolidayDateInPast):
		return httpx.NewError(http.StatusUnprocessableEntity, httpx.CodeUnprocessable,
			"holiday date must be today or later")

	case errors.Is(err, ErrHolidayDateInvalid):
		return httpx.NewError(http.StatusUnprocessableEntity, httpx.CodeValidation,
			"date must be in YYYY-MM-DD form")

	case errors.Is(err, ErrHolidayRangeInvalid):
		return httpx.NewError(http.StatusUnprocessableEntity, httpx.CodeValidation,
			"\"to\" must not precede \"from\"")

	case errors.Is(err, ErrForbidden):
		return httpx.ErrForbidden

	case errors.Is(err, ErrAdminScopeRequired):
		return httpx.NewError(http.StatusUnprocessableEntity, httpx.CodeValidation,
			"an administrative listing must name one doctor_id and a from/to window of at most 31 days")

	default:
		return httpx.ErrInternal.WithCause(err)
	}
}
