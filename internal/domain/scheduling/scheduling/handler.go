package scheduling

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"telemed/internal/platform/httpx"
	"telemed/internal/platform/middleware"
)

// Handler is the HTTP surface. It parses, authorises by role, calls the service
// and renders. It contains no SQL and no business rule -- when a decision looks
// like it belongs here, it belongs in service.go.
type Handler struct {
	svc *Service
	// adminIssuer is the one token issuer whose administrative role claims
	// this service honours. Empty disables the check. See actor().
	adminIssuer string
}

// NewHandler builds the HTTP adapter.
//
// adminIssuer is the ONE token issuer whose administrative role claims this
// service honours -- KEYCLOAK_ISSUER. It is a constructor parameter rather than
// package state because the check it drives is an authorisation decision, and
// an authorisation decision configured by a package-level variable is one a
// test can silently leave switched off.
//
// An empty value disables the check, which is correct for a developer stack
// that runs no Keycloak, and is why cmd/server/main.go passes cfg.KeycloakIssuer
// straight through: a deployment that configures Keycloak gets the binding, one
// that does not was never going to receive a Keycloak token anyway.
func NewHandler(svc *Service, adminIssuer string) *Handler {
	return &Handler{svc: svc, adminIssuer: adminIssuer}
}

// actor resolves the caller into the identity the domain uses.
//
// The distinction matters: appointments.doctor_id is a *doctor* id, not a user
// id, so a doctor's own appointments are found through the telemed_doctor_id
// claim. A doctor token without that claim is a misconfigured Keycloak mapper,
// and failing closed is the only safe response.
//
// # Why the admin branch checks the ISSUER
//
// An administrative role is only honoured from the admin issuer. user-service
// can mint a completely honest token -- correct `iss`, correct key, verifying
// cleanly -- that asserts realm_access.roles = ["super_admin"]; the review's F1
// recorded that, and it is a 2FA bypass rather than a role escalation, because
// SAML SSO and enforced 2FA live on the Keycloak side and phone-OTP login does
// not.
//
// The two existing enforcement points both miss this path. The gateway's
// RequireTokenIssuer runs only on rules with `auth: "admin"`, and these routes
// are `authenticated` with no roles. The platform middleware's checkAdminIssuer
// runs only inside RequireRole, and cmd/server/main.go mounts this route group
// with RequireAuth alone -- correctly, since it serves patients and doctors
// too. So the branch below was the last unguarded place on this service where
// an admin role was believed, and authorizeAppointment applies no ownership
// predicate to ActorAdmin at all.
//
// The check is here rather than in middleware for the same reason: this is the
// line that turns a role claim into authority.
func (h *Handler) actor(p middleware.Principal) (uuid.UUID, string, error) {
	switch {
	case p.HasAnyRole(middleware.AdminRoles...):
		if h.adminIssuer != "" && p.Issuer != h.adminIssuer {
			return uuid.Nil, "", httpx.NewError(http.StatusForbidden, httpx.CodeForbidden,
				"administrative roles are not accepted from this token issuer")
		}
		return p.UserID, "admin", nil
	case p.HasRole(middleware.RoleDoctor):
		if p.DoctorID == uuid.Nil {
			return uuid.Nil, "", httpx.NewError(http.StatusForbidden, httpx.CodeForbidden,
				"this doctor token carries no doctor id")
		}
		return p.DoctorID, "doctor", nil
	case p.HasRole(middleware.RolePatient):
		return p.UserID, "patient", nil
	default:
		return uuid.Nil, "", httpx.ErrForbidden
	}
}

// PublicRoutes are readable without authentication in front of the gateway's
// optional-auth middleware: browsing a doctor's availability is how a patient
// decides to sign up at all.
func (h *Handler) PublicRoutes(r chi.Router) {
	r.Get("/doctors/{doctorID}/slots", h.listSlots)
}

// PatientRoutes are the authenticated patient and doctor surface.
func (h *Handler) PatientRoutes(r chi.Router) {
	r.Route("/appointments", func(r chi.Router) {
		r.Post("/", h.bookAppointment)
		r.Get("/", h.listAppointments)
		r.Get("/{appointmentID}", h.getAppointment)
		r.Put("/{appointmentID}/cancel", h.cancelAppointment)
		r.Post("/{appointmentID}/complete", h.completeAppointment)
		r.Post("/{appointmentID}/no-show", h.noShowAppointment)
	})
	r.Route("/waitlist", func(r chi.Router) {
		r.Post("/", h.joinWaitlist)
		r.Get("/", h.listWaitlist)
		r.Delete("/{waitlistID}", h.leaveWaitlist)
	})

	// A doctor's own leave. It lives in THIS service and not in doctor-service
	// because this service owns the holidays table, owns the generator that
	// reads it, and owns the slots and appointments that registering leave has
	// to act on. A copy in doctor-service would have to be replicated back by
	// event, and would be wrong for the window between the two.
	//
	// The path is /doctors/me/holidays rather than /holidays so it reads the
	// same as every other "my" resource on the platform. It sits beside the
	// public /doctors/{doctorID}/slots without ambiguity: chi matches the
	// static "me" segment in preference to the {doctorID} parameter, and
	// TestDoctorHolidayRoutesDoNotShadowSlots pins that.
	r.Route("/doctors/me/holidays", func(r chi.Router) {
		r.Post("/", h.createMyHoliday)
		r.Get("/", h.listMyHolidays)
		r.Delete("/{holidayID}", h.deleteMyHoliday)
	})
}

// ---------------------------------------------------------------------------
// Doctor-managed leave
// ---------------------------------------------------------------------------

// doctorActor resolves the caller as a doctor, or refuses. Leave belongs to one
// doctor; there is no meaningful patient or admin reading of "my holidays", and
// an admin managing somebody else's leave has the /admin/holidays endpoint.
func doctorActor(p middleware.Principal) (uuid.UUID, error) {
	if !p.HasRole(middleware.RoleDoctor) {
		return uuid.Nil, httpx.ErrForbidden
	}
	if p.DoctorID == uuid.Nil {
		return uuid.Nil, httpx.NewError(http.StatusForbidden, httpx.CodeForbidden,
			"this doctor token carries no doctor id")
	}
	return p.DoctorID, nil
}

type createHolidayRequest struct {
	// Date is the civil date of the leave, YYYY-MM-DD in the business
	// timezone. Not a timestamp: "the 14th of April" is a day in Colombo, and
	// an instant would make it the 13th for eighteen and a half hours.
	Date string `json:"date" validate:"required"`

	// Reason is shown to nobody but the doctor. It is optional because the
	// doctor app's holiday dialog offers a Skip button, and refusing a save
	// because somebody did not want to type why they are away would be absurd.
	Reason string `json:"reason" validate:"max=200"`

	// CancelBooked is the doctor's explicit consent to cancel and refund the
	// patients already booked that day.
	//
	// Absent (the default) the request FAILS with 409 HOLIDAY_HAS_BOOKINGS and
	// reports the count, and nothing is written. That is the point: a doctor
	// tapping a date does not necessarily know six people are booked that
	// morning, and neither silent outcome is defensible.
	CancelBooked bool `json:"cancel_booked"`
}

type holidayResponse struct {
	ID   uuid.UUID `json:"id"`
	Date Date      `json:"date"`
	// DoctorID is null for a platform-wide closure.
	DoctorID *uuid.UUID `json:"doctor_id"`
	Reason   string     `json:"reason"`
	// PlatformWide tells a client it may not offer a delete button for this
	// row. It is derived from doctor_id, and sent explicitly so the client does
	// not have to re-derive a permission rule.
	PlatformWide bool      `json:"platform_wide"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

func newHolidayResponse(h Holiday) holidayResponse {
	return holidayResponse{
		ID:           h.ID,
		Date:         h.Date,
		DoctorID:     h.DoctorID,
		Reason:       h.Reason,
		PlatformWide: h.PlatformWide(),
		CreatedAt:    h.CreatedAt,
		UpdatedAt:    h.UpdatedAt,
	}
}

// holidayEffectResponse reports what registering or lifting leave actually did.
//
// The counts are not decoration. A doctor who is told only "saved" has no way
// to know four patients were just cancelled on their behalf, and that is
// exactly the thing they most need to be told.
type holidayEffectResponse struct {
	holidayResponse
	SlotsWithdrawn        int `json:"slots_withdrawn"`
	SlotsRestored         int `json:"slots_restored,omitempty"`
	AppointmentsCancelled int `json:"appointments_cancelled"`
}

func newHolidayEffectResponse(e HolidayEffect) holidayEffectResponse {
	return holidayEffectResponse{
		holidayResponse:       newHolidayResponse(e.Holiday),
		SlotsWithdrawn:        e.SlotsWithdrawn,
		SlotsRestored:         e.SlotsRestored,
		AppointmentsCancelled: e.AppointmentsCancelled,
	}
}

// createMyHoliday handles POST /api/v1/doctors/me/holidays.
func (h *Handler) createMyHoliday(w http.ResponseWriter, r *http.Request) {
	doctorID, err := doctorActor(middleware.MustPrincipal(r.Context()))
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	var req createHolidayRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	date, err := ParseDate(req.Date)
	if err != nil {
		httpx.Error(w, r, APIError(ErrHolidayDateInvalid))
		return
	}

	effect, svcErr := h.svc.AddHoliday(r.Context(), AddHolidayInput{
		DoctorID: &doctorID,
		Date:     date,
		Reason:   req.Reason,
		// A doctor's own leave always acts on the slots that already exist.
		// Leave that patients can still book into is not leave.
		ApplyToExisting: true,
		CancelBooked:    req.CancelBooked,
	})
	if svcErr != nil {
		if errors.Is(svcErr, ErrHolidayHasBookings) {
			// The count goes in `fields` so the client can render "4 patients
			// are booked that day" without parsing the message string, which is
			// translated and may be reworded at any time.
			// errors.As, not a type assertion: APIError already wraps the
			// domain error as a cause, and an assertion would miss it the day
			// anything wraps it once more.
			var apiErr *httpx.APIError
			if errors.As(APIError(svcErr), &apiErr) {
				apiErr.Fields = map[string]string{
					"booked_appointments": strconv.Itoa(effect.BlockedByAppointments),
				}
				httpx.Error(w, r, apiErr)
				return
			}
		}
		httpx.Error(w, r, APIError(svcErr))
		return
	}
	httpx.Created(w, r, newHolidayEffectResponse(effect))
}

// listMyHolidays handles GET /api/v1/doctors/me/holidays?from&to.
//
// It returns the doctor's own leave AND the platform-wide closures, because
// both stop them being booked and a screen showing one but not the other is
// wrong about the doctor's actual availability.
func (h *Handler) listMyHolidays(w http.ResponseWriter, r *http.Request) {
	doctorID, err := doctorActor(middleware.MustPrincipal(r.Context()))
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	var from, to Date
	if raw := r.URL.Query().Get("from"); raw != "" {
		from, err = ParseDate(raw)
		if err != nil {
			httpx.Error(w, r, APIError(ErrHolidayDateInvalid))
			return
		}
	}
	if raw := r.URL.Query().Get("to"); raw != "" {
		to, err = ParseDate(raw)
		if err != nil {
			httpx.Error(w, r, APIError(ErrHolidayDateInvalid))
			return
		}
	}

	holidays, err := h.svc.ListDoctorHolidays(r.Context(), doctorID, from, to)
	if err != nil {
		httpx.Error(w, r, APIError(err))
		return
	}
	out := make([]holidayResponse, len(holidays))
	for i := range holidays {
		out[i] = newHolidayResponse(holidays[i])
	}
	httpx.OK(w, r, out)
}

// deleteMyHoliday handles DELETE /api/v1/doctors/me/holidays/{holidayID}.
//
// Lifting leave puts the day back on the market: withdrawn slots are revived
// and a generation pass fills anything that was never materialised. It does NOT
// resurrect the appointments the leave cancelled -- those patients were told,
// refunded, and have very likely booked elsewhere.
func (h *Handler) deleteMyHoliday(w http.ResponseWriter, r *http.Request) {
	doctorID, err := doctorActor(middleware.MustPrincipal(r.Context()))
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	holidayID, err := httpx.PathUUID(r, "holidayID", chi.URLParam)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	effect, err := h.svc.RemoveHoliday(r.Context(), doctorID, holidayID)
	if err != nil {
		httpx.Error(w, r, APIError(err))
		return
	}
	// 200 with the effect rather than 204: how many slots came back is the
	// answer to "did lifting my leave actually do anything", and a 204 cannot
	// carry it.
	httpx.OK(w, r, newHolidayEffectResponse(effect))
}

// AdminRoutes are the override surface. The caller has already passed the IP
// allowlist and an admin role check before reaching here.
func (h *Handler) AdminRoutes(r chi.Router) {
	r.Post("/slots/{slotID}/block", h.blockSlot)
	// The scoped administrative listing. It lives HERE and not beside
	// GET /appointments because this subtree is the only one that carries
	// IPAllowlist + RequireRole; the patient surface carries neither, which is
	// what made the unfiltered admin listing a T1 bulk PHI read (F4).
	r.Get("/appointments", h.listAppointmentsForAdmin)
	r.Post("/appointments/{appointmentID}/force-cancel", h.forceCancel)
	r.Post("/holidays", h.setHoliday)
}

// ---------------------------------------------------------------------------
// Availability
// ---------------------------------------------------------------------------

func (h *Handler) listSlots(w http.ResponseWriter, r *http.Request) {
	doctorID, err := httpx.PathUUID(r, "doctorID", chi.URLParam)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	raw := r.URL.Query().Get("date")
	if raw == "" {
		// Defaulting to today in the business timezone is what a patient means
		// by "show me slots".
		raw = time.Now().In(h.svc.Location()).Format(time.DateOnly)
	}
	date, err := ParseDate(raw)
	if err != nil {
		httpx.Error(w, r, httpx.NewError(http.StatusBadRequest, httpx.CodeBadRequest,
			"date must be in YYYY-MM-DD form"))
		return
	}

	slots, err := h.svc.ListSlots(r.Context(), doctorID, date)
	if err != nil {
		httpx.Error(w, r, APIError(err))
		return
	}

	out := make([]SlotDTO, len(slots))
	for i := range slots {
		out[i] = NewSlotDTO(slots[i], h.svc.Location())
	}
	httpx.OK(w, r, map[string]any{
		"doctor_id": doctorID,
		"date":      date,
		"timezone":  h.svc.Location().String(),
		"slots":     out,
	})
}

// ---------------------------------------------------------------------------
// Appointments
// ---------------------------------------------------------------------------

type bookAppointmentRequest struct {
	SlotID   uuid.UUID  `json:"slot_id" validate:"required"`
	DoctorID *uuid.UUID `json:"doctor_id,omitempty"`
	// FamilyMemberID is still DECODED so the request can be refused with a
	// reason. Dropping the field from the struct instead would make
	// httpx.DecodeJSON's DisallowUnknownFields answer "unknown field
	// family_member_id", which reads like a typo rather than like a control,
	// and would tell an operator nothing about why. See bookAppointment.
	FamilyMemberID *uuid.UUID      `json:"family_member_id,omitempty"`
	Intake         json.RawMessage `json:"intake,omitempty"`
}

func (h *Handler) bookAppointment(w http.ResponseWriter, r *http.Request) {
	p := middleware.MustPrincipal(r.Context())
	actorID, role, err := h.actor(p)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	// A doctor booking their own slot is not a supported flow; an administrator
	// booking on a patient's behalf goes through support tooling, not this
	// endpoint, so that the audit trail records who really booked.
	if role != "patient" {
		httpx.Error(w, r, httpx.NewError(http.StatusForbidden, httpx.CodeForbidden,
			"only a patient may book an appointment"))
		return
	}

	var req bookAppointmentRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	if req.SlotID == uuid.Nil {
		httpx.Error(w, r, httpx.NewError(http.StatusUnprocessableEntity, httpx.CodeValidation,
			"slot_id is required"))
		return
	}
	if len(req.Intake) > 0 && !json.Valid(req.Intake) {
		httpx.Error(w, r, httpx.NewError(http.StatusUnprocessableEntity, httpx.CodeValidation,
			"intake must be a JSON object"))
		return
	}
	// A caller-supplied family_member_id is refused rather than trusted.
	//
	// patient_id on this path comes from the token and the body cannot name a
	// patient -- that half of the review's F5 was fixed. family_member_id was
	// not: it was read from the body and INSERTed onto the appointment with no
	// ownership predicate anywhere in the service, because the dependant rows
	// live in telemed_user and scheduling has no read of them. An id is not a
	// permission, and "which person is this appointment for" is not a field a
	// caller gets to assert about a stranger's child.
	//
	// Refused, not silently dropped: a booking that quietly loses the
	// dependant the patient chose would attribute a real consultation to the
	// account holder instead, which is a clinical-record error rather than a
	// UX one. The client is told the field is not accepted, and by whom.
	if req.FamilyMemberID != nil {
		httpx.Error(w, r, httpx.NewError(http.StatusUnprocessableEntity, httpx.CodeValidation,
			"family_member_id is not accepted: scheduling cannot verify that a dependant belongs to the caller, "+
				"and an unverified one would attribute this appointment to someone else"))
		return
	}

	appt, err := h.svc.BookSlot(r.Context(), BookSlotInput{
		SlotID:    req.SlotID,
		PatientID: actorID,
		DoctorID:  req.DoctorID,
		Intake:    req.Intake,
	})
	if err != nil {
		httpx.Error(w, r, APIError(err))
		return
	}
	httpx.Created(w, r, NewAppointmentDTO(appt, h.svc.Location(), true))
}

func (h *Handler) getAppointment(w http.ResponseWriter, r *http.Request) {
	p := middleware.MustPrincipal(r.Context())
	actorID, role, err := h.actor(p)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	id, err := httpx.PathUUID(r, "appointmentID", chi.URLParam)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	appt, err := h.svc.GetAppointment(r.Context(), id, actorID, role)
	if err != nil {
		httpx.Error(w, r, APIError(err))
		return
	}
	// An administrator resolving a dispute has no clinical need for the intake
	// form; HIPAA minimum-necessary says they do not get it.
	httpx.OK(w, r, NewAppointmentDTO(appt, h.svc.Location(), role != "admin"))
}

func (h *Handler) listAppointments(w http.ResponseWriter, r *http.Request) {
	p := middleware.MustPrincipal(r.Context())
	actorID, role, err := h.actor(p)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	// F4: this route is auth: "authenticated" in the gateway's table -- no role
	// gate, no IP allowlist, no admin-origin check, no audit. It used to return
	// the entire appointments table to an admin token. It now serves only the
	// caller's own rows, and says where the administrative view went rather
	// than returning a bare 403 somebody will file as a bug.
	if role == ActorAdmin {
		httpx.Error(w, r, httpx.NewError(http.StatusForbidden, httpx.CodeForbidden,
			"administrative listing is served by GET /api/v1/admin/appointments, scoped to one doctor and a bounded window"))
		return
	}

	page, perPage, offset := httpx.Pagination(r)

	var status *AppointmentStatus
	if raw := r.URL.Query().Get("status"); raw != "" {
		s := AppointmentStatus(raw)
		switch s {
		case AppointmentPendingPayment, AppointmentConfirmed, AppointmentCancelled,
			AppointmentCompleted, AppointmentNoShow:
			status = &s
		default:
			httpx.Error(w, r, httpx.NewError(http.StatusBadRequest, httpx.CodeBadRequest,
				"status must be one of pending_payment, confirmed, cancelled, completed, no_show"))
			return
		}
	}

	appts, total, err := h.svc.ListMyAppointments(r.Context(), actorID, role, status, perPage, offset)
	if err != nil {
		httpx.Error(w, r, APIError(err))
		return
	}

	out := make([]AppointmentDTO, len(appts))
	for i := range appts {
		// Never in a list: intake is PHI and a listing is the least likely
		// place it is actually needed.
		out[i] = NewAppointmentDTO(appts[i], h.svc.Location(), false)
	}
	httpx.List(w, r, out, httpx.Meta{Page: page, PerPage: perPage, Total: total})
}

type cancelRequest struct {
	Reason string `json:"reason,omitempty" validate:"omitempty,max=500"`
}

func (h *Handler) cancelAppointment(w http.ResponseWriter, r *http.Request) {
	p := middleware.MustPrincipal(r.Context())
	actorID, role, err := h.actor(p)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	id, err := httpx.PathUUID(r, "appointmentID", chi.URLParam)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	var req cancelRequest
	if r.ContentLength > 0 {
		if err := httpx.DecodeJSON(w, r, &req); err != nil {
			httpx.Error(w, r, err)
			return
		}
	}

	appt, err := h.svc.CancelAppointment(r.Context(), CancelInput{
		AppointmentID: id,
		ActorID:       actorID,
		ActorRole:     role,
		Reason:        req.Reason,
	})
	if err != nil {
		httpx.Error(w, r, APIError(err))
		return
	}
	httpx.OK(w, r, NewAppointmentDTO(appt, h.svc.Location(), role != "admin"))
}

func (h *Handler) completeAppointment(w http.ResponseWriter, r *http.Request) {
	h.markTerminal(w, r, AppointmentCompleted)
}

func (h *Handler) noShowAppointment(w http.ResponseWriter, r *http.Request) {
	h.markTerminal(w, r, AppointmentNoShow)
}

func (h *Handler) markTerminal(w http.ResponseWriter, r *http.Request, status AppointmentStatus) {
	p := middleware.MustPrincipal(r.Context())
	actorID, role, err := h.actor(p)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	id, err := httpx.PathUUID(r, "appointmentID", chi.URLParam)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	appt, err := h.svc.MarkTerminal(r.Context(), id, actorID, role, status)
	if err != nil {
		httpx.Error(w, r, APIError(err))
		return
	}
	httpx.OK(w, r, NewAppointmentDTO(appt, h.svc.Location(), role != "admin"))
}

// ---------------------------------------------------------------------------
// Waitlist
// ---------------------------------------------------------------------------

type joinWaitlistRequest struct {
	DoctorID      uuid.UUID `json:"doctor_id" validate:"required"`
	PreferredDate string    `json:"preferred_date" validate:"required"`
}

func (h *Handler) joinWaitlist(w http.ResponseWriter, r *http.Request) {
	p := middleware.MustPrincipal(r.Context())
	actorID, role, err := h.actor(p)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if role != "patient" {
		httpx.Error(w, r, httpx.NewError(http.StatusForbidden, httpx.CodeForbidden,
			"only a patient may join a waitlist"))
		return
	}

	var req joinWaitlistRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	date, err := ParseDate(req.PreferredDate)
	if err != nil {
		httpx.Error(w, r, httpx.NewError(http.StatusUnprocessableEntity, httpx.CodeValidation,
			"preferred_date must be in YYYY-MM-DD form"))
		return
	}

	entry, err := h.svc.JoinWaitlist(r.Context(), actorID, req.DoctorID, date)
	if err != nil {
		httpx.Error(w, r, APIError(err))
		return
	}
	httpx.Created(w, r, NewWaitlistDTO(entry))
}

func (h *Handler) listWaitlist(w http.ResponseWriter, r *http.Request) {
	p := middleware.MustPrincipal(r.Context())
	actorID, role, err := h.actor(p)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if role != "patient" {
		httpx.Error(w, r, httpx.ErrForbidden)
		return
	}

	entries, err := h.svc.ListWaitlist(r.Context(), actorID)
	if err != nil {
		httpx.Error(w, r, APIError(err))
		return
	}
	out := make([]WaitlistDTO, len(entries))
	for i := range entries {
		out[i] = NewWaitlistDTO(entries[i])
	}
	httpx.OK(w, r, out)
}

func (h *Handler) leaveWaitlist(w http.ResponseWriter, r *http.Request) {
	p := middleware.MustPrincipal(r.Context())
	actorID, role, err := h.actor(p)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	id, err := httpx.PathUUID(r, "waitlistID", chi.URLParam)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if err := h.svc.LeaveWaitlist(r.Context(), id, actorID, role == "admin"); err != nil {
		httpx.Error(w, r, APIError(err))
		return
	}
	httpx.NoContent(w, r)
}

// ---------------------------------------------------------------------------
// Admin overrides
// ---------------------------------------------------------------------------

type blockSlotRequest struct {
	// Status is BLOCKED to withhold a slot, CANCELLED to retire it, AVAILABLE
	// to put it back. Defaults to BLOCKED.
	Status string `json:"status,omitempty" validate:"omitempty,oneof=BLOCKED CANCELLED AVAILABLE"`
	Reason string `json:"reason" validate:"required,max=500"`
}

func (h *Handler) blockSlot(w http.ResponseWriter, r *http.Request) {
	slotID, err := httpx.PathUUID(r, "slotID", chi.URLParam)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var req blockSlotRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	status := SlotBlocked
	if req.Status != "" {
		status = SlotStatus(req.Status)
	}

	slot, err := h.svc.BlockSlot(r.Context(), slotID, status, req.Reason)
	if err != nil {
		httpx.Error(w, r, APIError(err))
		return
	}
	httpx.OK(w, r, NewSlotDTO(slot, h.svc.Location()))
}

type setHolidayRequest struct {
	// DoctorID absent means a platform-wide holiday: a Poya day, Independence
	// Day, or a national closure.
	DoctorID *uuid.UUID `json:"doctor_id,omitempty"`
	Date     string     `json:"date" validate:"required"`
	Reason   string     `json:"reason" validate:"required,max=200"`

	// ApplyToExisting withdraws slots that were already generated for the day.
	//
	// It defaults to FALSE, which is this endpoint's long-standing behaviour
	// and is kept so an existing caller loading the Poya calendar cannot
	// suddenly find it cancelling the marketplace. It is ignored entirely for a
	// platform-wide entry: withdrawing every doctor's calendar is a mass
	// cancellation and needs an operational procedure, not a request flag.
	ApplyToExisting bool `json:"apply_to_existing"`

	// CancelBooked authorises cancelling and refunding the patients already
	// booked. Only meaningful alongside ApplyToExisting on a doctor-scoped
	// entry; without it the request fails with 409 HOLIDAY_HAS_BOOKINGS.
	CancelBooked bool `json:"cancel_booked"`
}

func (h *Handler) setHoliday(w http.ResponseWriter, r *http.Request) {
	var req setHolidayRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	date, err := ParseDate(req.Date)
	if err != nil {
		httpx.Error(w, r, httpx.NewError(http.StatusUnprocessableEntity, httpx.CodeValidation,
			"date must be in YYYY-MM-DD form"))
		return
	}
	effect, err := h.svc.SetHoliday(r.Context(), req.DoctorID, date, req.Reason,
		req.ApplyToExisting, req.CancelBooked)
	if err != nil {
		httpx.Error(w, r, APIError(err))
		return
	}
	httpx.Created(w, r, newHolidayEffectResponse(effect))
}

// listAppointmentsForAdmin answers "what does this doctor's calendar look like
// around the clash", which is the double-booking investigation, and refuses
// everything wider than that.
//
// It deliberately has no patient_id parameter. Adding one would make it the
// endpoint F4 was about: a named patient's clinical timeline, readable by all
// five admin roles. If a dispute needs one appointment, the admin has its id
// and GET /api/v1/appointments/{id} serves it -- without intake.
func (h *Handler) listAppointmentsForAdmin(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	// Refuse rather than ignore. A silently dropped filter is how somebody
	// concludes the endpoint "does not work" and adds the parameter for real.
	if q.Has("patient_id") {
		httpx.Error(w, r, httpx.NewError(http.StatusUnprocessableEntity, httpx.CodeValidation,
			"this endpoint does not filter by patient: a patient's appointment history is not an administrative view"))
		return
	}

	doctorID, err := uuid.Parse(q.Get("doctor_id"))
	if err != nil {
		httpx.Error(w, r, httpx.NewError(http.StatusUnprocessableEntity, httpx.CodeValidation,
			"doctor_id is required and must be a UUID"))
		return
	}
	from, err := time.Parse(time.RFC3339, q.Get("from"))
	if err != nil {
		httpx.Error(w, r, httpx.NewError(http.StatusUnprocessableEntity, httpx.CodeValidation,
			"from is required and must be RFC3339"))
		return
	}
	to, err := time.Parse(time.RFC3339, q.Get("to"))
	if err != nil {
		httpx.Error(w, r, httpx.NewError(http.StatusUnprocessableEntity, httpx.CodeValidation,
			"to is required and must be RFC3339"))
		return
	}

	var status *AppointmentStatus
	if raw := q.Get("status"); raw != "" {
		s := AppointmentStatus(raw)
		switch s {
		case AppointmentPendingPayment, AppointmentConfirmed, AppointmentCancelled,
			AppointmentCompleted, AppointmentNoShow:
			status = &s
		default:
			httpx.Error(w, r, httpx.NewError(http.StatusBadRequest, httpx.CodeBadRequest,
				"status must be one of pending_payment, confirmed, cancelled, completed, no_show"))
			return
		}
	}

	page, perPage, offset := httpx.Pagination(r)
	appts, total, err := h.svc.ListAppointmentsForAdmin(r.Context(), AdminListInput{
		DoctorID: doctorID,
		From:     from,
		To:       to,
		Status:   status,
		Limit:    perPage,
		Offset:   offset,
	})
	if err != nil {
		httpx.Error(w, r, APIError(err))
		return
	}

	out := make([]AppointmentDTO, len(appts))
	for i := range appts {
		// Never intake. Minimum necessary: resolving a clash needs times and
		// ids, not the patient's account of their symptoms.
		out[i] = NewAppointmentDTO(appts[i], h.svc.Location(), false)
	}
	httpx.List(w, r, out, httpx.Meta{Page: page, PerPage: perPage, Total: total})
}

type forceCancelRequest struct {
	Reason string `json:"reason" validate:"required,max=500"`
}

func (h *Handler) forceCancel(w http.ResponseWriter, r *http.Request) {
	p := middleware.MustPrincipal(r.Context())
	id, err := httpx.PathUUID(r, "appointmentID", chi.URLParam)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var req forceCancelRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}

	// Force is what separates this from the ordinary cancel endpoint: an
	// administrator can cancel an appointment whose slot has already started,
	// which is the "resolve a stuck consultation" case in the runbook.
	appt, err := h.svc.CancelAppointment(r.Context(), CancelInput{
		AppointmentID: id,
		ActorID:       p.UserID,
		ActorRole:     "admin",
		Reason:        req.Reason,
		Force:         true,
	})
	if err != nil {
		httpx.Error(w, r, APIError(err))
		return
	}
	httpx.OK(w, r, NewAppointmentDTO(appt, h.svc.Location(), false))
}
