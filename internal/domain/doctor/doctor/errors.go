package doctor

import "errors"

// Sentinel errors returned by the repository and service layers. handler.go
// is the only place these are translated into an *httpx.APIError -- neither
// repository.go nor service.go imports httpx or net/http, per the layering
// rule in AGENT-BRIEF.md.
var (
	ErrNotFound          = errors.New("doctor: not found")
	ErrSLMCTaken         = errors.New("doctor: slmc number already registered")
	ErrAlreadyRegistered = errors.New("doctor: this account already has a doctor profile")
	ErrVersionConflict   = errors.New("doctor: version conflict, reload and retry")
	ErrInvalidTransition = errors.New("doctor: verification status transition not allowed")
	ErrReasonRequired    = errors.New("doctor: a reason is required for this action")
	ErrNotEligibleReview = errors.New("doctor: caller did not complete an appointment with this doctor")
	ErrReviewExists      = errors.New("doctor: this appointment has already been reviewed")
	ErrOverlappingHours  = errors.New("doctor: overlapping working hours for the same day")
	ErrInvalidSchedule   = errors.New("doctor: invalid schedule settings")
	ErrTermsNotAccepted  = errors.New("doctor: terms of the service retention agreement must be accepted")
	ErrInvalidPassword   = errors.New("doctor: password must be 8–72 characters")
	// ErrAccountProvision is the RETRYABLE provisioning failure: user-service
	// was unreachable, slow, or broken. Approving again is the fix.
	ErrAccountProvision = errors.New("doctor: could not create the doctor login")
	// ErrAccountConflict is the PERMANENT one: the identity on the application
	// cannot own a doctor login as it stands -- the email and phone belong to
	// two different accounts, the account is suspended, or the address itself
	// is unusable. Retrying approval will fail identically every time; a human
	// has to reconcile the accounts first.
	ErrAccountConflict     = errors.New("doctor: the identity on this application cannot be given a doctor login")
	ErrDocumentTooLarge    = errors.New("doctor: uploaded document is too large")
	ErrInvalidDocumentType = errors.New("doctor: unsupported document type")
)

// ErrHolidaysNotOwnedHere is GONE, and its absence is the change rather than an
// oversight.
//
// It used to 422 any availability save that carried a non-empty `holidays`
// array, because scheduling-service owns the holidays table and had no
// doctor-facing way in: the only route was POST /api/v1/admin/holidays, behind
// the admin IP allowlist. Refusing loudly was correct at the time -- accepting
// the field and discarding it would have told a doctor their leave was
// registered while patients went on booking them.
//
// scheduling-service now exposes POST/GET/DELETE
// /api/v1/doctors/me/holidays, and this service forwards to it (see
// holidays.go). The errors that replace this one -- ErrHolidayHasBookings,
// ErrHolidayRejected, ErrHolidayUpstreamUnavailable,
// ErrHolidayForwardingDisabled -- all describe something that actually went
// wrong with the registration, rather than the platform not having built it.
