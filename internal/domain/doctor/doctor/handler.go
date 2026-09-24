package doctor

import (
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"telemed/internal/platform/httpx"
	"telemed/internal/platform/middleware"
)

// SubRegistrar is a sibling domain that mounts endpoints inside this handler's
// /doctors/me subtree.
//
// It exists because chi panics when two Mounts share a prefix: /doctors is
// already mounted by this handler, so internal/analytics cannot mount its own
// router at /doctors/me/analytics. Handing it the live router instead is the
// only way those endpoints reach their natural path, and keeping the seam an
// interface means this package does not import the analytics package.
type SubRegistrar interface {
	Register(r chi.Router)
}

// Handler is the HTTP surface for the doctor domain. It decodes requests,
// calls Service, and encodes responses -- no SQL, no business rules.
type Handler struct {
	svc  *Service
	auth *middleware.Authenticator
	// subs are mounted inside /doctors/me. Nil is valid and mounts nothing,
	// which is what a test that only exercises the profile endpoints wants.
	subs []SubRegistrar
}

// NewHandler builds a Handler. subs are registered inside the authenticated
// /doctors/me subtree, in the order given.
func NewHandler(svc *Service, auth *middleware.Authenticator, subs ...SubRegistrar) *Handler {
	return &Handler{svc: svc, auth: auth, subs: subs}
}

// Routes mounts the patient/doctor-facing surface at whatever prefix the
// caller chooses (main.go mounts it at /api/v1/doctors).
func (h *Handler) Routes() chi.Router {
	r := chi.NewRouter()

	r.With(middleware.OptionalAuth(h.auth)).Get("/", h.search)
	r.Post("/apply", h.apply)
	r.Get("/applications/eligibility", h.applicationEligibility)
	r.Post("/applications/{applicationID}/documents", h.applyDocument)
	r.With(middleware.RequireAuth(h.auth)).Post("/register", h.register)

	r.Route("/me", func(r chi.Router) {
		r.Use(middleware.RequireAuth(h.auth))
		r.Get("/", h.getMine)
		r.Put("/", h.updateMine)
		r.Put("/photo", h.putPhoto)
		r.Get("/photo", h.getMinePhoto)
		r.Delete("/photo", h.deletePhoto)
		r.Put("/signature", h.putCredentialImage(DocumentSignature))
		r.Get("/signature", h.getCredentialImage(DocumentSignature))
		r.Put("/seal", h.putCredentialImage(DocumentSeal))
		r.Get("/seal", h.getCredentialImage(DocumentSeal))
		r.Get("/availability", h.getAvailability)
		r.Put("/availability", h.setAvailability)
		r.Get("/schedule-settings", h.getScheduleSettings)
		r.Post("/documents", h.uploadDocument)
		for _, sub := range h.subs {
			sub.Register(r)
		}
	})

	r.With(middleware.OptionalAuth(h.auth)).Get("/{id}/photo", h.getPhoto)
	r.With(middleware.OptionalAuth(h.auth)).Get("/{id}", h.getByID)
	r.With(middleware.OptionalAuth(h.auth)).Get("/{id}/reviews", h.listReviews)
	r.With(middleware.RequireAuth(h.auth)).Post("/{id}/reviews", h.createReview)

	return r
}

// InternalRoutes mounts the admin-service-facing surface (main.go mounts it
// at /api/v1/internal/doctors, behind RequireRole(service|admin roles)).
func (h *Handler) InternalRoutes() chi.Router {
	r := chi.NewRouter()
	r.Get("/pending", h.listPending)
	r.Post("/{id}/verify", h.verify)
	r.Get("/by-user/{userID}", h.getByUserID)
	r.Get("/applications/by-phone/{phone}", h.getApplicationByPhone)
	r.Get("/applications/by-email/{email}", h.getApplicationByEmail)
	r.Get("/applications/pending", h.listPendingApplications)
	r.Get("/applications/{id}", h.getApplicationByID)
	r.Post("/applications/{id}/verify", h.verifyApplication)
	r.Post("/applications/{id}/attach", h.attachApplication)
	return r
}

// AdminScheduleRoutes is staff-operated schedule editing, mounted under
// /api/v1/admin/doctors so it satisfies the gateway's convention that every
// admin auth-mode route lives beneath the admin prefix.
//
// It is separate from InternalRoutes for that reason alone: the gateway never
// rewrites a path, only scheme and host, so the path a client calls is the
// path this service must serve. Putting these under /internal would have meant
// either an admin-mode route outside the admin prefix -- which the gateway's
// route table test rejects, correctly -- or weakening the auth mode to get the
// naming right, which is exactly the trade that once left the payout trigger
// without the admin network controls.
//
// The /me routes assume the doctor is the person at the keyboard. In practice
// they are not: a doctor phones or messages the clinic with "I am free 9 to 1
// on Thursday" and an administrator enters it. Without these routes that
// workflow had nowhere to happen, so the platform either forced clinical staff
// into an app they do not use or left the schedule wrong -- and a wrong
// schedule is patients booking slots the doctor will not attend.
//
// Leave is deliberately NOT here. scheduling-service owns the holidays table
// and already exposes POST /holidays on its own admin surface, so duplicating
// it would give two write paths to one table.
func (h *Handler) AdminScheduleRoutes() chi.Router {
	r := chi.NewRouter()
	r.Get("/{id}/availability", h.adminGetAvailability)
	r.Put("/{id}/availability", h.adminSetAvailability)
	r.Get("/{id}/schedule-settings", h.adminGetScheduleSettings)
	r.Get("/{id}/application", h.adminGetApplication)
	r.Get("/{id}/application-documents/{docType}", h.adminGetApplicationDocument)
	return r
}

// ---------------------------------------------------------------------------
// wire DTOs
// ---------------------------------------------------------------------------

type registerRequest struct {
	SLMCNumber      string   `json:"slmc_number" validate:"required,slmc"`
	DisplayName     string   `json:"display_name" validate:"required,min=2,max=200"`
	Specialty       string   `json:"specialty" validate:"required"`
	SubSpecialties  []string `json:"sub_specialties" validate:"omitempty,dive,required"`
	ExperienceYears int      `json:"experience_years" validate:"gte=0,lte=70"`
	// The wire name stays fee_lkr: the patient app, the doctor app and the
	// gateway's OpenAPI all send it, and v1 does not break. The value has
	// always been CENTS -- the Go field and the database column now say so.
	FeeCents       int64           `json:"fee_lkr" validate:"gte=0"`
	Languages      []string        `json:"languages" validate:"required,min=1,dive,oneof=en si ta other"`
	Bio            string          `json:"bio" validate:"max=2000"`
	PhotoURL       string          `json:"photo_url" validate:"omitempty,max=2048"`
	Qualifications []Qualification `json:"qualifications" validate:"omitempty,dive"`
	Bank           *BankDetails    `json:"bank" validate:"omitempty"`
}

type applyRequest struct {
	Phone                 string       `json:"phone" validate:"required"`
	Email                 string       `json:"email" validate:"required,email"`
	Password              string       `json:"password" validate:"required,min=8,max=72"`
	FirstName             string       `json:"first_name" validate:"required,min=1,max=100"`
	LastName              string       `json:"last_name" validate:"required,min=1,max=100"`
	DisplayName           string       `json:"display_name" validate:"omitempty,min=2,max=200"`
	SLMCNumber            string       `json:"slmc_number" validate:"required,slmc"`
	Specialty             string       `json:"specialty" validate:"required"`
	ExperienceYears       int          `json:"experience_years" validate:"gte=0,lte=70"`
	FeeCents              int64        `json:"fee_lkr" validate:"gte=0"`
	RequiredFeeCents      int64        `json:"required_fee_lkr" validate:"gte=0"`
	Languages             []string     `json:"languages" validate:"required,min=1,dive,oneof=en si ta other"`
	LanguageOther         string       `json:"language_other" validate:"max=100"`
	Bio                   string       `json:"bio" validate:"max=2000"`
	PGIMBoardCertified    bool         `json:"pgim_board_certified"`
	MedicalSchool         string       `json:"medical_school" validate:"required,min=2,max=200"`
	QualificationsText    string       `json:"qualifications" validate:"required,min=2,max=2000"`
	AvailabilityNotes     string       `json:"availability_notes" validate:"required,min=2,max=2000"`
	IsGeneralPractitioner bool         `json:"is_general_practitioner"`
	PracticingLocations   []string     `json:"practicing_locations" validate:"required,min=1,dive,required,max=200"`
	TermsAccepted         bool         `json:"terms_accepted"`
	Bank                  *BankDetails `json:"bank" validate:"required"`
}

type applicationVerifyRequest struct {
	Action string `json:"action" validate:"required,oneof=approve reject"`
	Reason string `json:"reason" validate:"max=1000"`
}

type applicationAttachRequest struct {
	UserID uuid.UUID `json:"user_id" validate:"required"`
	Email  string    `json:"email" validate:"omitempty,email"`
}

type updateRequest struct {
	Version         int      `json:"version" validate:"gte=0"`
	Specialty       string   `json:"specialty" validate:"required"`
	SubSpecialties  []string `json:"sub_specialties" validate:"omitempty,dive,required"`
	ExperienceYears int      `json:"experience_years" validate:"gte=0,lte=70"`
	// Cents, despite the legacy wire name. See registerRequest.
	FeeCents           int64           `json:"fee_lkr" validate:"gte=0"`
	Languages          []string        `json:"languages" validate:"required,min=1,dive,oneof=en si ta other"`
	Bio                string          `json:"bio" validate:"max=2000"`
	PhotoURL           string          `json:"photo_url" validate:"omitempty,max=2048"`
	Qualifications     []Qualification `json:"qualifications" validate:"omitempty,dive"`
	AcceptsNewPatients bool            `json:"accepts_new_patients"`
	Bank               *BankDetails    `json:"bank" validate:"omitempty"`
}

type workingHourRequest struct {
	DayOfWeek   int    `json:"day_of_week" validate:"gte=0,lte=6"`
	StartTime   string `json:"start_time" validate:"required,len=5|len=8"`
	EndTime     string `json:"end_time" validate:"required,len=5|len=8"`
	IsAvailable bool   `json:"is_available"`
}

// setAvailabilityRequest is the availability editor's save payload.
//
// It has to accept EXACTLY what the doctor app sends, because
// httpx.DecodeJSON sets DisallowUnknownFields: an unrecognised key does not
// degrade, it rejects the whole request with a 400. The editor was sending
// slot_duration_minutes, buffer_minutes, max_per_day and holidays against a
// DTO that accepted only working_hours, so every save failed -- on the screen
// everything downstream depends on. No availability, no slots; no slots, no
// bookings.
type setAvailabilityRequest struct {
	WorkingHours []workingHourRequest `json:"working_hours" validate:"dive"`

	// SlotDurationMinutes is how long one consultation is booked for. Absent
	// (or 0) means "use the platform default", which is what a client that has
	// never shown the setting sends.
	SlotDurationMinutes int `json:"slot_duration_minutes" validate:"omitempty,gte=5,lte=240"`

	// BufferMinutes is a POINTER and must stay one, all the way to the wire.
	//
	// A doctor who wants back-to-back consultations sends 0, and that is a real
	// preference -- not the absence of one. Decoding into a plain int would
	// make "0" and "field not sent" the same value, and the doctor would keep
	// getting the default gap forever with nothing logged to say why.
	// scheduling-service's consumer already reads this as a *int for exactly
	// this reason; the nullability is preserved through the DTO, the domain
	// type, the database column and the event tag.
	BufferMinutes *int `json:"buffer_minutes" validate:"omitempty,gte=0,lte=120"`

	// MaxPerDay caps daily appointments. 0 means no cap.
	MaxPerDay int `json:"max_per_day" validate:"gte=0,lte=100"`

	// Holidays is FORWARDED to scheduling-service, which owns the holidays
	// table, the slot generator that reads it, and the slots and appointments
	// that registering leave has to act on.
	//
	// It used to be accepted and refused with a 422, because no doctor-facing
	// way to register leave existed anywhere on the platform and accepting the
	// field would have told a doctor their leave was saved while patients kept
	// booking them. scheduling-service now exposes
	// POST/GET/DELETE /api/v1/doctors/me/holidays, so the refusal is gone and
	// this array reaches it.
	//
	// It is ADDITIVE. Each entry is an idempotent upsert on
	// (doctor_id, date); the array is never treated as the doctor's complete
	// leave set, so a client whose cached list is stale cannot delete leave by
	// omitting it. Removal is DELETE /api/v1/doctors/me/holidays/{id} against
	// scheduling-service, and there is no other way to do it. An empty array is
	// a no-op and never contacts scheduling-service at all.
	Holidays []holidayRequest `json:"holidays" validate:"omitempty,dive"`
}

// adminSetAvailabilityRequest is the staff-operated version of
// setAvailabilityRequest.
//
// It carries no Holidays field, and that omission is the design. Leave is
// scheduling-service's table, reachable on its own admin surface; accepting it
// here too would mean two write paths to one set of rows, and the /me version
// only forwards it because the doctor app edits both on one screen.
//
// Every other field keeps the semantics of the /me DTO exactly, BufferMinutes
// included: it is still a pointer, because an administrator entering "no gap
// between consultations" is expressing the same real preference a doctor is,
// and collapsing 0 into "unset" would silently give that doctor the default
// gap forever.
type adminSetAvailabilityRequest struct {
	WorkingHours        []workingHourRequest `json:"working_hours" validate:"dive"`
	SlotDurationMinutes int                  `json:"slot_duration_minutes" validate:"omitempty,gte=5,lte=240"`
	BufferMinutes       *int                 `json:"buffer_minutes" validate:"omitempty,gte=0,lte=120"`
	MaxPerDay           int                  `json:"max_per_day" validate:"gte=0,lte=100"`
}

// holidayRequest is one day of leave, in the shape the doctor app already
// sends: a YYYY-MM-DD civil date and an optional reason.
type holidayRequest struct {
	Date   string `json:"date" validate:"required"`
	Reason string `json:"reason" validate:"max=200"`

	// CancelBooked is the doctor's explicit consent to cancel and refund the
	// patients already booked that day.
	//
	// It is optional and defaults to false, which is what the doctor app sends
	// today -- so a collision returns 409 HOLIDAY_HAS_BOOKINGS and nothing is
	// written. That is the intended outcome: a doctor tapping a date in a
	// picker does not necessarily know six people are booked that morning, and
	// the two silent alternatives are telling them they are off while patients
	// still expect them, or destroying six consultations with nobody deciding
	// to.
	CancelBooked bool `json:"cancel_booked,omitempty"`
}

// documentRequest is one credential submission.
//
// It carries NO object_key. The server derives the storage key from the
// authenticated caller's own doctor id (see objectkey.go): a client-chosen key
// let Dr B submit Dr A's SLMC certificate as their own and be approved on it.
//
// The field is still declared so the rejection can say WHY. httpx.DecodeJSON
// sets DisallowUnknownFields, so simply deleting it would answer a client that
// still sends one with "unknown field object_key" -- technically a 400, and
// useless to whoever has to work out that the storage contract changed. On a
// credential upload, where a reviewer opens the document to decide whether
// someone may practise medicine, silently redirecting the record to a
// different key would be worse still.
type documentRequest struct {
	DocumentType string `json:"document_type" validate:"required"`

	// Filename is advisory. Only an allowlisted extension is ever taken from
	// it, and never a path component.
	Filename string `json:"filename" validate:"omitempty,max=255"`

	// ObjectKey is REJECTED when non-empty. Deprecated, and deliberately not
	// silently ignored.
	ObjectKey string `json:"object_key" validate:"omitempty"`
}

type reviewRequest struct {
	AppointmentID uuid.UUID `json:"appointment_id" validate:"required"`
	Rating        int       `json:"rating" validate:"gte=1,lte=5"`
	Comment       string    `json:"comment" validate:"max=2000"`
}

type verifyRequest struct {
	Action string `json:"action" validate:"required,oneof=start_review approve reject reopen suspend reinstate"`
	Reason string `json:"reason" validate:"max=1000"`

	// ActorID is IGNORED. It is the same class of defect as F8's object_key:
	// an identity read out of the request instead of out of the verified
	// caller. The handler never called MustPrincipal, so the audit record of
	// WHO APPROVED A DOCTOR TO PRACTISE MEDICINE -- verified_by, and the
	// actor on the verification history row -- was chosen by whoever sent the
	// request. Anyone holding a token good enough to reach this route could
	// sign another admin's name to an approval.
	//
	// The actor is now middleware.MustPrincipal(ctx).UserID. The field stays
	// declared, and ignored rather than rejected, because httpx.DecodeJSON
	// sets DisallowUnknownFields: removing it would 400 every caller that
	// still sends one, and unlike F8's object_key nothing about where the
	// data LIVES changes here -- the value was only ever an audit label, and
	// the label is now taken from a fact instead of a claim.
	//
	// Deprecated: the actor is the authenticated caller. Sending this has no
	// effect.
	ActorID *uuid.UUID `json:"actor_id"`
}

// ---------------------------------------------------------------------------
// response shaping -- bank_encrypted never appears here.
// ---------------------------------------------------------------------------

type doctorResponse struct {
	ID              uuid.UUID `json:"id"`
	UserID          uuid.UUID `json:"user_id"`
	SLMCNumber      string    `json:"slmc_number"`
	Specialty       string    `json:"specialty"`
	SubSpecialties  []string  `json:"sub_specialties"`
	ExperienceYears int       `json:"experience_years"`
	// FeeCents is emitted twice on purpose. fee_cents is the canonical name and
	// matches events.DoctorApproved.FeeCents; fee_lkr is the deprecated alias
	// every existing client reads. Removing fee_lkr is a v2 change.
	FeeCents           int64              `json:"fee_cents"`
	FeeLKRDeprecated   int64              `json:"fee_lkr"`
	Currency           string             `json:"currency"`
	DisplayName        string             `json:"display_name"`
	Languages          []Language         `json:"languages"`
	Bio                string             `json:"bio"`
	PhotoURL           string             `json:"photo_url"`
	Qualifications     []Qualification    `json:"qualifications"`
	VerificationStatus VerificationStatus `json:"verification_status"`
	RejectionReason    string             `json:"rejection_reason,omitempty"`
	VerifiedAt         *time.Time         `json:"verified_at,omitempty"`
	Rating             float64            `json:"rating"`
	ReviewCount        int                `json:"review_count"`
	ConsultationCount  int                `json:"consultation_count"`
	NoShowRate         float64            `json:"no_show_rate"`
	AcceptsNewPatients bool               `json:"accepts_new_patients"`
	CreatedAt          time.Time          `json:"created_at"`
	UpdatedAt          time.Time          `json:"updated_at"`
	Version            int                `json:"version"`
}

// toDoctorResponse builds the PUBLIC doctor shape: what a patient browsing the
// directory may see.
//
// SECURITY-REVIEW F7, second half. One function used to serve both the public
// GET /doctors/{id} and the private GET /doctors/me, so rejection_reason -- an
// internal credentialing note written by an admin explaining why an
// application was refused -- was on the public profile of every doctor who had
// ever been rejected and reopened.
//
// The private shape is toOwnDoctorResponse below. Same reasoning as the review
// DTOs: two functions, so the public path cannot acquire a private field by
// someone adding one to a struct.
func toDoctorResponse(d Doctor) doctorResponse {
	return doctorResponse{
		ID: d.ID, UserID: d.UserID, SLMCNumber: d.SLMCNumber, Specialty: d.Specialty,
		SubSpecialties: d.SubSpecialties, ExperienceYears: d.ExperienceYears,
		FeeCents: d.FeeCents, FeeLKRDeprecated: d.FeeCents, Currency: d.Currency,
		DisplayName: d.DisplayName, Languages: d.Languages, Bio: d.Bio, PhotoURL: d.PhotoURL,
		Qualifications: d.Qualifications, VerificationStatus: d.VerificationStatus,
		VerifiedAt: d.VerifiedAt,
		Rating:     d.Rating, ReviewCount: d.ReviewCount, ConsultationCount: d.ConsultationCount,
		NoShowRate: d.NoShowRate, AcceptsNewPatients: d.AcceptsNewPatients,
		CreatedAt: d.CreatedAt, UpdatedAt: d.UpdatedAt, Version: d.Version,
	}
}

// toOwnDoctorResponse is the public shape plus the fields only the doctor
// themselves -- or an admin reviewing them -- has any business seeing.
//
// rejection_reason is the whole difference. A doctor must be told why their
// application was refused, or they cannot fix it; a patient browsing the
// directory must not be, because it is an internal note about a named
// individual's professional credentials.
func toOwnDoctorResponse(d Doctor) doctorResponse {
	out := toDoctorResponse(d)
	out.RejectionReason = d.RejectionReason
	return out
}

type searchResultResponse struct {
	doctorResponse
	NextAvailableAt    *time.Time `json:"next_available_at,omitempty"`
	AvailableSlotCount int        `json:"available_slot_count"`
}

func toSearchResultResponse(sr SearchResult) searchResultResponse {
	return searchResultResponse{
		doctorResponse:     toDoctorResponse(sr.Doctor),
		NextAvailableAt:    sr.NextAvailableAt,
		AvailableSlotCount: sr.AvailableSlotCount,
	}
}

// publicReviewResponse is what GET /doctors/{id}/reviews returns.
//
// SECURITY-REVIEW F7. That route is auth: "public" in the gateway route table
// and served under OptionalAuth, and it used to emit patient_id and
// appointment_id on every row. Walk the public GET /doctors search, then pull
// each doctor's reviews, and with no credentials at all you have built a table
// of WHICH PATIENT CONSULTED WHICH DOCTOR. For a psychiatrist, an HIV
// clinician or an obstetrician the specialty IS the diagnosis, so that table
// is a disclosure of medical condition on its own -- and the patient ids then
// feed ?owner_user_id= against record-service.
//
// A public review needs a rating, a comment and a date. Nothing about it
// requires naming the person who wrote it, and no client renders one: the
// patient's own display name is not in this service's database either
// (ADR-004), so the id was never being turned into anything a reader could
// use. It was pure leakage.
//
// This is a separate type from the owner shape below rather than a flag on one
// type. A boolean that decides whether PHI is serialised is one wrong argument
// away from a breach, and the wrong argument is invisible at the call site.
// Two types make the mistake a compile error.
type publicReviewResponse struct {
	ID        uuid.UUID `json:"id"`
	DoctorID  uuid.UUID `json:"doctor_id"`
	Rating    int       `json:"rating"`
	Comment   string    `json:"comment"`
	CreatedAt time.Time `json:"created_at"`
}

func toPublicReviewResponse(rv Review) publicReviewResponse {
	return publicReviewResponse{
		ID: rv.ID, DoctorID: rv.DoctorID,
		Rating: rv.Rating, Comment: rv.Comment, CreatedAt: rv.CreatedAt,
	}
}

// ownReviewResponse is returned only to the patient who just wrote the review,
// on POST /doctors/{id}/reviews. patient_id here is the caller's own id and
// appointment_id is their own appointment, so echoing them back discloses
// nothing they did not send -- and a client needs the appointment link to know
// which consultation it has now reviewed.
type ownReviewResponse struct {
	publicReviewResponse
	PatientID     uuid.UUID `json:"patient_id"`
	AppointmentID uuid.UUID `json:"appointment_id"`
}

func toOwnReviewResponse(rv Review) ownReviewResponse {
	return ownReviewResponse{
		publicReviewResponse: toPublicReviewResponse(rv),
		PatientID:            rv.PatientID,
		AppointmentID:        rv.AppointmentID,
	}
}

type documentResponse struct {
	ID           uuid.UUID  `json:"id"`
	DoctorID     uuid.UUID  `json:"doctor_id"`
	DocumentType string     `json:"document_type"`
	ObjectKey    string     `json:"object_key"`
	UploadedAt   time.Time  `json:"uploaded_at"`
	ReviewedAt   *time.Time `json:"reviewed_at,omitempty"`
}

func toDocumentResponse(d Document) documentResponse {
	return documentResponse{
		ID: d.ID, DoctorID: d.DoctorID, DocumentType: string(d.DocumentType), ObjectKey: d.ObjectKey,
		UploadedAt: d.UploadedAt, ReviewedAt: d.ReviewedAt,
	}
}

type pendingDoctorResponse struct {
	doctorResponse
	Documents []documentResponse `json:"documents"`
}

// ---------------------------------------------------------------------------
// error mapping -- the only place doctor errors become httpx.APIError
// ---------------------------------------------------------------------------

// holidayConflictError renders a leave collision, carrying the affected day and
// the number of patients booked into `fields` so a client can render "4
// patients are booked on 14 April" without parsing a translated sentence.
func holidayConflictError(err error) *httpx.APIError {
	out := httpx.NewError(http.StatusConflict, httpx.CodeHolidayHasBookings, err.Error())
	var rej *holidayRejection
	if errors.As(err, &rej) {
		fields := map[string]string{}
		if rej.date != "" {
			fields["date"] = rej.date
		}
		if rej.bookedAppointments != "" {
			fields["booked_appointments"] = rej.bookedAppointments
		}
		if len(fields) > 0 {
			out.Fields = fields
		}
	}
	return out
}

func writeError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrApplicationExists):
		httpx.Error(w, r, httpx.NewError(http.StatusConflict, httpx.CodeConflict, "an open application already exists for this phone or SLMC number"))
	case errors.Is(err, ErrApplicationNotFound):
		httpx.Error(w, r, httpx.ErrNotFound)
	case errors.Is(err, ErrApplicationNotReady):
		httpx.Error(w, r, httpx.NewError(http.StatusConflict, httpx.CodeConflict, "application is not approved for activation"))
	case errors.Is(err, ErrInvalidProfilePhoto):
		httpx.Error(w, r, httpx.NewError(http.StatusUnprocessableEntity, httpx.CodeValidation,
			"profile photo must be a JPEG, PNG, or WebP image"))
	case errors.Is(err, ErrProfilePhotoTooLarge):
		httpx.Error(w, r, httpx.NewError(http.StatusRequestEntityTooLarge, httpx.CodeBadRequest,
			"profile photo must be 2 MB or smaller"))
	case errors.Is(err, ErrInvalidCredentialImage):
		httpx.Error(w, r, httpx.NewError(http.StatusBadRequest, httpx.CodeBadRequest, "signature and seal must be PNG or JPEG images"))
	case errors.Is(err, ErrCredentialImageTooLarge):
		httpx.Error(w, r, httpx.NewError(http.StatusRequestEntityTooLarge, httpx.CodeBadRequest, "signature and seal must be 1 MB or smaller"))
	case errors.Is(err, ErrInvalidCredentialDocument):
		httpx.Error(w, r, httpx.NewError(http.StatusBadRequest, httpx.CodeBadRequest, "credential documents must be PDF, JPEG, PNG or WebP files"))
	case errors.Is(err, ErrCredentialStoreUnavailable):
		httpx.Error(w, r, httpx.NewError(http.StatusServiceUnavailable, httpx.CodeUnavailable, "signature and seal storage is temporarily unavailable"))
	case errors.Is(err, ErrNotFound):
		httpx.Error(w, r, httpx.ErrNotFound)
	case errors.Is(err, ErrSLMCTaken):
		httpx.Error(w, r, httpx.NewError(http.StatusConflict, httpx.CodeConflict, "this SLMC number is already registered"))
	case errors.Is(err, ErrAlreadyRegistered):
		httpx.Error(w, r, httpx.NewError(http.StatusConflict, httpx.CodeConflict, "this account already has a doctor profile"))
	case errors.Is(err, ErrVersionConflict):
		httpx.Error(w, r, httpx.NewError(http.StatusConflict, httpx.CodeConflict, "profile was modified by another request, reload and retry"))
	case errors.Is(err, ErrInvalidTransition):
		httpx.Error(w, r, httpx.NewError(http.StatusConflict, httpx.CodeConflict, err.Error()))
	case errors.Is(err, ErrTermsNotAccepted):
		httpx.Error(w, r, httpx.NewError(http.StatusUnprocessableEntity, httpx.CodeValidation, "you must accept the VersaLife service retention agreement"))
	case errors.Is(err, ErrDocumentTooLarge):
		httpx.Error(w, r, httpx.NewError(http.StatusRequestEntityTooLarge, httpx.CodeBadRequest, "each document must be 5 MB or smaller"))
	case errors.Is(err, ErrInvalidDocumentType):
		httpx.Error(w, r, httpx.NewError(http.StatusUnprocessableEntity, httpx.CodeValidation, "upload a signature, seal, or SLMC certificate"))
	case errors.Is(err, ErrReasonRequired):
		httpx.Error(w, r, httpx.NewError(http.StatusUnprocessableEntity, httpx.CodeValidation, "a reason is required for this action"))
	case errors.Is(err, ErrNotEligibleReview):
		httpx.Error(w, r, httpx.NewError(http.StatusForbidden, httpx.CodeForbidden, "you can only review a doctor after completing an appointment with them"))
	case errors.Is(err, ErrReviewExists):
		httpx.Error(w, r, httpx.NewError(http.StatusConflict, httpx.CodeConflict, "this appointment has already been reviewed"))
	case errors.Is(err, ErrOverlappingHours):
		httpx.Error(w, r, httpx.NewError(http.StatusUnprocessableEntity, httpx.CodeUnprocessable, "working hours overlap on the same day"))
	case errors.Is(err, ErrInvalidSchedule):
		httpx.Error(w, r, httpx.NewError(http.StatusUnprocessableEntity, httpx.CodeValidation, err.Error()))
	case errors.Is(err, ErrHolidayHasBookings):
		// Passed through with its own code, not flattened into CONFLICT: the
		// client must show the doctor the affected day and ask whether to
		// cancel and refund. A generic conflict reads as "retry", and retrying
		// the identical body does exactly nothing.
		httpx.Error(w, r, holidayConflictError(err))
	case errors.Is(err, ErrHolidayRejected):
		httpx.Error(w, r, httpx.NewError(http.StatusUnprocessableEntity, httpx.CodeUnprocessable, err.Error()))
	case errors.Is(err, ErrHolidayUpstreamUnavailable):
		// 503, not 500. Nothing in this request is wrong and nothing here is
		// broken; the service that owns holidays is unreachable, and the
		// working hours were NOT saved, so retrying the identical body is both
		// safe and the right advice.
		httpx.Error(w, r, httpx.NewError(http.StatusServiceUnavailable, httpx.CodeUnavailable,
			"your working hours were not saved because the scheduling service "+
				"could not be reached to register your leave; please try again"))
	case errors.Is(err, ErrHolidayForwardingDisabled):
		httpx.Error(w, r, httpx.NewError(http.StatusNotImplemented, httpx.CodeProviderError,
			"this deployment cannot register leave; set SCHEDULING_BASE_URL"))
	default:
		httpx.Error(w, r, err)
	}
}
