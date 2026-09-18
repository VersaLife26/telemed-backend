package doctor

import (
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"telemed/internal/platform/httpx"
	"telemed/internal/platform/middleware"
)

// register handles POST /doctors/register.
func (h *Handler) register(w http.ResponseWriter, r *http.Request) {
	p := middleware.MustPrincipal(r.Context())

	var req registerRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}

	d, err := h.svc.Register(r.Context(), RegisterInput{
		UserID: p.UserID, DisplayName: req.DisplayName, SLMCNumber: req.SLMCNumber,
		Specialty: req.Specialty, SubSpecialties: req.SubSpecialties, ExperienceYears: req.ExperienceYears,
		FeeCents: req.FeeCents, Languages: toLanguages(req.Languages), Bio: req.Bio, PhotoURL: req.PhotoURL,
		Qualifications: req.Qualifications, Bank: req.Bank,
	})
	if err != nil {
		writeError(w, r, err)
		return
	}
	httpx.Created(w, r, toOwnDoctorResponse(d))
}

// search handles GET /doctors.
func (h *Handler) search(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	page, perPage, _ := httpx.Pagination(r)

	f := SearchFilters{
		Specialty: q.Get("specialty"),
		Language:  q.Get("language"),
		Query:     q.Get("q"),
		SortBy:    SortField(q.Get("sort")),
		Page:      page,
		PerPage:   perPage,
	}
	if !f.SortBy.Valid() {
		httpx.Error(w, r, httpx.NewError(http.StatusBadRequest, httpx.CodeBadRequest, "sort must be one of: rating, fee, experience, next_available"))
		return
	}
	if v := q.Get("min_fee"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			httpx.Error(w, r, httpx.NewError(http.StatusBadRequest, httpx.CodeBadRequest, "min_fee must be an integer"))
			return
		}
		f.MinFeeCents = &n
	}
	if v := q.Get("max_fee"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			httpx.Error(w, r, httpx.NewError(http.StatusBadRequest, httpx.CodeBadRequest, "max_fee must be an integer"))
			return
		}
		f.MaxFeeCents = &n
	}
	if v := q.Get("min_rating"); v != "" {
		n, err := strconv.ParseFloat(v, 64)
		if err != nil {
			httpx.Error(w, r, httpx.NewError(http.StatusBadRequest, httpx.CodeBadRequest, "min_rating must be a number"))
			return
		}
		f.MinRating = &n
	}

	switch avail := q.Get("available"); avail {
	case "now":
		f.RequireAvailable = true
		f.AvailableAfter = time.Now().UTC()
	case "":
		// no availability filter requested
	default:
		httpx.Error(w, r, httpx.NewError(http.StatusBadRequest, httpx.CodeBadRequest, "available must be 'now' when set without a timestamp"))
		return
	}
	if v := q.Get("available_after"); v != "" {
		ts, err := time.Parse(time.RFC3339, v)
		if err != nil {
			httpx.Error(w, r, httpx.NewError(http.StatusBadRequest, httpx.CodeBadRequest, "available_after must be RFC3339"))
			return
		}
		f.RequireAvailable = true
		f.AvailableAfter = ts
	}

	results, total, err := h.svc.Search(r.Context(), f)
	if err != nil {
		writeError(w, r, err)
		return
	}
	out := make([]searchResultResponse, len(results))
	for i := range results {
		out[i] = toSearchResultResponse(results[i])
	}
	httpx.List(w, r, out, httpx.Meta{Page: page, PerPage: perPage, Total: total})
}

// getByID handles GET /doctors/{id}.
func (h *Handler) getByID(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id", chi.URLParam)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	var callerUserID *uuid.UUID
	isAdmin := false
	if p, ok := middleware.PrincipalFrom(r.Context()); ok {
		uid := p.UserID
		callerUserID = &uid
		isAdmin = p.HasAnyRole(middleware.AdminRoles...)
	}

	d, err := h.svc.GetPublic(r.Context(), id, callerUserID, isAdmin)
	if err != nil {
		writeError(w, r, err)
		return
	}

	// This one route serves three audiences: any anonymous patient browsing
	// the directory, the doctor looking at their own profile, and an admin
	// reviewing them. Only the last two may see rejection_reason -- the
	// internal note explaining why a credentialing application was refused.
	// GetPublic has already established which of the three this is; the
	// response shape follows the same answer rather than a second one.
	if isAdmin || (callerUserID != nil && *callerUserID == d.UserID) {
		httpx.OK(w, r, toOwnDoctorResponse(d))
		return
	}
	httpx.OK(w, r, toDoctorResponse(d))
}

// getMine handles GET /doctors/me.
func (h *Handler) getMine(w http.ResponseWriter, r *http.Request) {
	p := middleware.MustPrincipal(r.Context())
	d, err := h.svc.GetMine(r.Context(), p.UserID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	httpx.OK(w, r, toOwnDoctorResponse(d))
}

// updateMine handles PUT /doctors/me.
func (h *Handler) updateMine(w http.ResponseWriter, r *http.Request) {
	p := middleware.MustPrincipal(r.Context())

	var req updateRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}

	d, err := h.svc.UpdateMine(r.Context(), p.UserID, req.Version, UpdateInput{
		Specialty: req.Specialty, SubSpecialties: req.SubSpecialties, ExperienceYears: req.ExperienceYears,
		FeeCents: req.FeeCents, Languages: toLanguages(req.Languages), Bio: req.Bio, PhotoURL: req.PhotoURL,
		Qualifications: req.Qualifications, AcceptsNewPatients: req.AcceptsNewPatients, Bank: req.Bank,
	})
	if err != nil {
		writeError(w, r, err)
		return
	}
	httpx.OK(w, r, toOwnDoctorResponse(d))
}

// getAvailability handles GET /doctors/me/availability.
func (h *Handler) getAvailability(w http.ResponseWriter, r *http.Request) {
	p := middleware.MustPrincipal(r.Context())
	d, err := h.svc.GetMine(r.Context(), p.UserID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	hours, err := h.svc.GetAvailability(r.Context(), d.ID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	httpx.OK(w, r, toWorkingHoursResponse(hours))
}

// setAvailability handles PUT /doctors/me/availability.
func (h *Handler) setAvailability(w http.ResponseWriter, r *http.Request) {
	p := middleware.MustPrincipal(r.Context())

	var req setAvailabilityRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}

	d, err := h.svc.GetMine(r.Context(), p.UserID)
	if err != nil {
		writeError(w, r, err)
		return
	}

	hours := make([]WorkingHour, len(req.WorkingHours))
	for i, wh := range req.WorkingHours {
		hours[i] = WorkingHour{DoctorID: d.ID, DayOfWeek: wh.DayOfWeek, StartTime: wh.StartTime, EndTime: wh.EndTime, IsAvailable: wh.IsAvailable}
	}
	settings := ScheduleSettings{
		DoctorID:            d.ID,
		SlotDurationMinutes: req.SlotDurationMinutes,
		BufferMinutes:       req.BufferMinutes,
		MaxPerDay:           req.MaxPerDay,
		Timezone:            DefaultScheduleTimezone,
	}

	// Holidays are FORWARDED to scheduling-service, which owns the table and
	// the generator. They used to be refused with a 422 because there was no
	// doctor-facing way in; there is one now, and the refusal would be the
	// thing standing between a doctor and their leave.
	//
	// The forward is additive: each entry is an upsert on (doctor_id, date) and
	// this array is never read as the doctor's complete leave set. Removing
	// leave is DELETE /api/v1/doctors/me/holidays/{id} on scheduling-service.
	// See internal/doctor/holidays.go and docs/DESIGN.md.
	leave := make([]HolidayRequest, len(req.Holidays))
	for i := range req.Holidays {
		// Converted, not copied field by field: holidayRequest is the wire
		// shape and HolidayRequest the domain one, deliberately identical, so a
		// field added to one and not the other must fail the build here rather
		// than quietly stop being forwarded.
		leave[i] = HolidayRequest(req.Holidays[i])
	}

	if err := h.svc.SetAvailabilityWithLeave(r.Context(), AvailabilityInput{
		DoctorID:     d.ID,
		WorkingHours: hours,
		Settings:     settings,
		Leave:        leave,
		// The caller's own token, forwarded verbatim so scheduling-service
		// authorises the doctor rather than trusting this service's assertion
		// about who is calling.
		Bearer: r.Header.Get("Authorization"),
	}); err != nil {
		writeError(w, r, err)
		return
	}
	// The response stays a bare working-hours array: the doctor app parses it
	// as one, and changing the shape under a client mid-migration would break
	// the very screen this fixes. The saved slot-shape settings are readable at
	// GET /doctors/me/schedule-settings.
	httpx.OK(w, r, toWorkingHoursResponse(hours))
}

// getScheduleSettings handles GET /doctors/me/schedule-settings.
//
// It exists because PUT /doctors/me/availability now accepts settings that its
// sibling GET cannot return: that GET responds with a bare working-hours array,
// and widening it to an object would break the doctor app's parser. Without
// this endpoint the slot-shape declaration is write-only -- fine until the
// doctor signs in on a second device, where the editor would show defaults it
// has no way to know are wrong.
func (h *Handler) getScheduleSettings(w http.ResponseWriter, r *http.Request) {
	p := middleware.MustPrincipal(r.Context())
	d, err := h.svc.GetMine(r.Context(), p.UserID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	set, err := h.svc.GetScheduleSettings(r.Context(), d.ID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	httpx.OK(w, r, toScheduleSettingsResponse(set))
}

// uploadDocument handles POST /doctors/me/documents.
func (h *Handler) uploadDocument(w http.ResponseWriter, r *http.Request) {
	p := middleware.MustPrincipal(r.Context())

	var req documentRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	docType := DocumentType(req.DocumentType)
	if !docType.Valid() {
		httpx.Error(w, r, httpx.NewError(http.StatusBadRequest, httpx.CodeBadRequest, "unknown document_type"))
		return
	}
	if req.ObjectKey != "" {
		// SECURITY-REVIEW F8. This field used to be stored verbatim, so a
		// doctor could name any key in the shared doctor-credentials bucket --
		// including another doctor's genuine SLMC certificate -- and have the
		// credentialing reviewer approve them on it.
		httpx.Error(w, r, httpx.NewError(http.StatusBadRequest, httpx.CodeBadRequest,
			"object_key is no longer accepted: the server derives the storage key from your own doctor id. "+
				"Send document_type and (optionally) filename."))
		return
	}

	// d.ID comes from the caller's OWN profile, resolved from the verified
	// principal -- so the derived key can only ever be prefixed with the
	// uploader's doctor id. There is no request shape that attaches one
	// doctor's key to another doctor's application.
	d, err := h.svc.GetMine(r.Context(), p.UserID)
	if err != nil {
		writeError(w, r, err)
		return
	}

	doc, err := h.svc.UploadDocument(r.Context(), d.ID, docType, req.Filename)
	if err != nil {
		writeError(w, r, err)
		return
	}
	httpx.Created(w, r, toDocumentResponse(doc))
}

// listReviews handles GET /doctors/{id}/reviews.
func (h *Handler) listReviews(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id", chi.URLParam)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	page, perPage, _ := httpx.Pagination(r)

	reviews, total, err := h.svc.ListReviews(r.Context(), id, page, perPage)
	if err != nil {
		writeError(w, r, err)
		return
	}
	// The PUBLIC shape. This route is auth: "public" in the gateway route
	// table, and the row used to carry patient_id -- which turned a public
	// doctor search into a public map of who consulted whom.
	out := make([]publicReviewResponse, len(reviews))
	for i := range reviews {
		out[i] = toPublicReviewResponse(reviews[i])
	}
	httpx.List(w, r, out, httpx.Meta{Page: page, PerPage: perPage, Total: total})
}

// createReview handles POST /doctors/{id}/reviews.
func (h *Handler) createReview(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id", chi.URLParam)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	p := middleware.MustPrincipal(r.Context())

	var req reviewRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}

	rv, err := h.svc.CreateReview(r.Context(), id, p.UserID, req.AppointmentID, req.Rating, req.Comment)
	if err != nil {
		writeError(w, r, err)
		return
	}
	// The caller wrote this review, so echoing their own patient_id and
	// appointment_id back to them discloses nothing they did not send.
	httpx.Created(w, r, toOwnReviewResponse(rv))
}

// listPending handles GET /internal/doctors/pending.
func (h *Handler) listPending(w http.ResponseWriter, r *http.Request) {
	page, perPage, _ := httpx.Pagination(r)

	pending, total, err := h.svc.ListPending(r.Context(), page, perPage)
	if err != nil {
		writeError(w, r, err)
		return
	}
	out := make([]pendingDoctorResponse, len(pending))
	for i := range pending {
		pd := &pending[i]
		docs := make([]documentResponse, len(pd.Documents))
		for j := range pd.Documents {
			docs[j] = toDocumentResponse(pd.Documents[j])
		}
		// The credentialing queue is admin-only and rejection_reason is the
		// reviewer's own note, so this is the OWN shape, not the public one.
		out[i] = pendingDoctorResponse{doctorResponse: toOwnDoctorResponse(pd.Doctor), Documents: docs}
	}
	httpx.List(w, r, out, httpx.Meta{Page: page, PerPage: perPage, Total: total})
}

// verify handles POST /internal/doctors/{id}/verify.
func (h *Handler) verify(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id", chi.URLParam)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	var req verifyRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}

	// The actor comes from the verified token, never from req.ActorID. This
	// route is mounted behind RequireAuth + RequireRole(internalRoles...), so
	// a principal is always present; taking the id from the body meant the
	// audit record of who approved a doctor to practise medicine was chosen
	// by whoever sent the request.
	//
	// A token with no resolvable user id still records NULL rather than the
	// zero UUID, because 00000000-0000-0000-0000-000000000000 reads as a real
	// actor in a query and an absent one does not.
	//
	// Note for whoever wires admin-service to this route: a `service` token
	// records admin-service's own account, not the human who clicked approve.
	// Carrying the human across that hop needs a signed on-behalf-of claim,
	// not a body field -- a body field is exactly what this change removes.
	var actorID *uuid.UUID
	if caller := middleware.MustPrincipal(r.Context()); caller.UserID != uuid.Nil {
		uid := caller.UserID
		actorID = &uid
	}

	d, err := h.svc.Verify(r.Context(), id, VerificationAction(req.Action), req.Reason, actorID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	httpx.OK(w, r, toOwnDoctorResponse(d))
}

// ---------------------------------------------------------------------------

type workingHourResponse struct {
	DayOfWeek   int    `json:"day_of_week"`
	StartTime   string `json:"start_time"`
	EndTime     string `json:"end_time"`
	IsAvailable bool   `json:"is_available"`
}

func toWorkingHoursResponse(hours []WorkingHour) []workingHourResponse {
	out := make([]workingHourResponse, len(hours))
	for i, h := range hours {
		out[i] = workingHourResponse{DayOfWeek: h.DayOfWeek, StartTime: h.StartTime, EndTime: h.EndTime, IsAvailable: h.IsAvailable}
	}
	return out
}

// scheduleSettingsResponse mirrors what the availability editor PUTs, so a
// client can round-trip its own payload. buffer_minutes is a pointer and is
// emitted as JSON null when the doctor has expressed no preference -- null and
// 0 are different answers, and flattening them here would undo the care taken
// in the column, the domain type and the event.
type scheduleSettingsResponse struct {
	SlotDurationMinutes int    `json:"slot_duration_minutes"`
	BufferMinutes       *int   `json:"buffer_minutes"`
	MaxPerDay           int    `json:"max_per_day"`
	Timezone            string `json:"timezone"`
}

func toScheduleSettingsResponse(s ScheduleSettings) scheduleSettingsResponse {
	return scheduleSettingsResponse{
		SlotDurationMinutes: s.SlotDurationMinutes,
		BufferMinutes:       s.BufferMinutes,
		MaxPerDay:           s.MaxPerDay,
		Timezone:            s.Timezone,
	}
}

func toLanguages(ss []string) []Language {
	out := make([]Language, len(ss))
	for i, s := range ss {
		out[i] = Language(s)
	}
	return out
}

// ---------------------------------------------------------------------------
// Staff-operated schedule editing
//
// The doctor id comes from the PATH and the actor from the verified token --
// never from the body. That is the same rule the verify route above was fixed
// to follow (security review F28): a body-supplied actor lets the caller
// choose who the record says did this.
//
// Attribution of the HUMAN lives in admin-service's hash-chained audit log,
// which stages an entry before calling here. A `service` token on this hop
// records admin-service itself, exactly as it does for verify; carrying the
// operator across the hop needs a signed on-behalf-of claim, not a body field.
// ---------------------------------------------------------------------------

// adminGetAvailability handles GET /internal/doctors/{id}/availability.
func (h *Handler) adminGetAvailability(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id", chi.URLParam)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	hours, err := h.svc.GetAvailability(r.Context(), id)
	if err != nil {
		writeError(w, r, err)
		return
	}
	httpx.OK(w, r, toWorkingHoursResponse(hours))
}

// adminGetScheduleSettings handles GET /internal/doctors/{id}/schedule-settings.
//
// Its /me sibling exists because PUT accepts settings the availability GET
// cannot return. The same is true here, and more sharply: an administrator
// editing someone else's schedule has no other way to see the slot shape they
// are about to overwrite.
func (h *Handler) adminGetScheduleSettings(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id", chi.URLParam)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	settings, err := h.svc.GetScheduleSettings(r.Context(), id)
	if err != nil {
		writeError(w, r, err)
		return
	}
	httpx.OK(w, r, toScheduleSettingsResponse(settings))
}

// adminSetAvailability handles PUT /internal/doctors/{id}/availability.
func (h *Handler) adminSetAvailability(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id", chi.URLParam)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	var req adminSetAvailabilityRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}

	// Resolve the doctor before writing. GetAvailability on an unknown id
	// returns an empty set rather than an error, so without this a typo in the
	// path would report a successful save of a schedule belonging to nobody.
	if _, err := h.svc.GetPublic(r.Context(), id, nil, true); err != nil {
		writeError(w, r, err)
		return
	}

	hours := make([]WorkingHour, len(req.WorkingHours))
	for i, wh := range req.WorkingHours {
		hours[i] = WorkingHour{
			DoctorID: id, DayOfWeek: wh.DayOfWeek, StartTime: wh.StartTime,
			EndTime: wh.EndTime, IsAvailable: wh.IsAvailable,
		}
	}

	if err := h.svc.SetAvailabilityWithLeave(r.Context(), AvailabilityInput{
		DoctorID:     id,
		WorkingHours: hours,
		Settings: ScheduleSettings{
			DoctorID:            id,
			SlotDurationMinutes: req.SlotDurationMinutes,
			BufferMinutes:       req.BufferMinutes,
			MaxPerDay:           req.MaxPerDay,
			Timezone:            DefaultScheduleTimezone,
		},
		// No Leave, so no Bearer: this path never contacts scheduling-service.
		// Leave is set on scheduling-service's own admin surface.
	}); err != nil {
		writeError(w, r, err)
		return
	}

	saved, err := h.svc.GetAvailability(r.Context(), id)
	if err != nil {
		writeError(w, r, err)
		return
	}
	httpx.OK(w, r, toWorkingHoursResponse(saved))
}

// apply handles POST /doctors/apply (public, no auth).
func (h *Handler) apply(w http.ResponseWriter, r *http.Request) {
	var req applyRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	req.Phone = httpx.NormalizePhone(req.Phone)
	if req.Phone == "" {
		invalid := httpx.NewError(http.StatusUnprocessableEntity, httpx.CodeValidation, "one or more fields failed validation")
		invalid.Fields = map[string]string{"phone": "must be a valid Sri Lankan mobile number"}
		httpx.Error(w, r, invalid)
		return
	}
	app, err := h.svc.Apply(r.Context(), ApplyInput{
		Phone: req.Phone, Email: req.Email, Password: req.Password,
		FirstName: req.FirstName, LastName: req.LastName,
		DisplayName: req.DisplayName, SLMCNumber: req.SLMCNumber, Specialty: req.Specialty,
		ExperienceYears: req.ExperienceYears, FeeCents: req.FeeCents, RequiredFeeCents: req.RequiredFeeCents,
		Languages: toLanguages(req.Languages), LanguageOther: req.LanguageOther, Bio: req.Bio,
		PGIMBoardCertified: req.PGIMBoardCertified, MedicalSchool: req.MedicalSchool,
		QualificationsText: req.QualificationsText, AvailabilityNotes: req.AvailabilityNotes,
		IsGeneralPractitioner: req.IsGeneralPractitioner, PracticingLocations: req.PracticingLocations,
		TermsAccepted: req.TermsAccepted, Bank: req.Bank,
	})
	if err != nil {
		writeError(w, r, err)
		return
	}
	httpx.Created(w, r, map[string]any{
		"application_id": app.ID.String(),
		"status":         string(app.Status),
		"message":        "Application received. You will be emailed when an admin reviews it.",
	})
}

// applicationEligibility handles GET /doctors/applications/eligibility?phone=
func (h *Handler) applicationEligibility(w http.ResponseWriter, r *http.Request) {
	phone := r.URL.Query().Get("phone")
	out, err := h.svc.EligibilityByPhone(r.Context(), phone)
	if err != nil {
		writeError(w, r, err)
		return
	}
	httpx.OK(w, r, out)
}

// getApplicationByPhone handles GET /internal/doctors/applications/by-phone/{phone}
func (h *Handler) getApplicationByPhone(w http.ResponseWriter, r *http.Request) {
	phone, err := url.PathUnescape(chi.URLParam(r, "phone"))
	if err != nil || phone == "" {
		httpx.Error(w, r, httpx.NewError(http.StatusBadRequest, httpx.CodeBadRequest, "phone is required"))
		return
	}
	app, err := h.svc.GetApplicationByPhone(r.Context(), phone)
	if err != nil {
		writeError(w, r, err)
		return
	}
	httpx.OK(w, r, toInternalApplicationResponse(app))
}

// listPendingApplications handles GET /internal/doctors/applications/pending
func (h *Handler) listPendingApplications(w http.ResponseWriter, r *http.Request) {
	page, perPage, _ := httpx.Pagination(r)
	apps, total, err := h.svc.ListPendingApplications(r.Context(), page, perPage)
	if err != nil {
		writeError(w, r, err)
		return
	}
	out := make([]map[string]any, len(apps))
	for i := range apps {
		out[i] = toApplicationResponse(apps[i])
	}
	httpx.List(w, r, out, httpx.Meta{Page: page, PerPage: perPage, Total: total})
}

// getApplicationByEmail handles GET /internal/doctors/applications/by-email/{email}
func (h *Handler) getApplicationByEmail(w http.ResponseWriter, r *http.Request) {
	email, err := url.PathUnescape(chi.URLParam(r, "email"))
	if err != nil || strings.TrimSpace(email) == "" {
		httpx.Error(w, r, httpx.NewError(http.StatusBadRequest, httpx.CodeBadRequest, "email is required"))
		return
	}
	app, err := h.svc.GetApplicationByEmail(r.Context(), email)
	if err != nil {
		writeError(w, r, err)
		return
	}
	httpx.OK(w, r, toInternalApplicationResponse(app))
}

// getApplicationByID handles GET /internal/doctors/applications/{id}
func (h *Handler) getApplicationByID(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id", chi.URLParam)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	app, err := h.svc.GetApplication(r.Context(), id)
	if err != nil {
		writeError(w, r, err)
		return
	}
	httpx.OK(w, r, toInternalApplicationResponse(app))
}

// toInternalApplicationResponse is the mesh shape for OTP activation. It
// includes password_hash so user-service can set email/password login; the
// admin console uses toApplicationResponse, which never exposes the hash.
func toInternalApplicationResponse(a Application) map[string]any {
	out := toApplicationResponse(a)
	if a.PasswordHash != "" {
		out["password_hash"] = a.PasswordHash
	}
	return out
}

// verifyApplication handles POST /internal/doctors/applications/{id}/verify
func (h *Handler) verifyApplication(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id", chi.URLParam)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var req applicationVerifyRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	var actorID *uuid.UUID
	if caller := middleware.MustPrincipal(r.Context()); caller.UserID != uuid.Nil {
		uid := caller.UserID
		actorID = &uid
	}
	app, err := h.svc.VerifyApplication(r.Context(), id, req.Action == "approve", req.Reason, actorID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	httpx.OK(w, r, toApplicationResponse(app))
}

// attachApplication handles POST /internal/doctors/applications/{id}/attach
func (h *Handler) attachApplication(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id", chi.URLParam)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var req applicationAttachRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	d, err := h.svc.Attach(r.Context(), AttachInput{
		ApplicationID: id, UserID: req.UserID, Email: req.Email,
	})
	if err != nil {
		writeError(w, r, err)
		return
	}
	httpx.OK(w, r, toOwnDoctorResponse(d))
}

func toApplicationResponse(a Application) map[string]any {
	out := map[string]any{
		"application_id":          a.ID.String(),
		"phone":                   a.Phone,
		"email":                   a.Email,
		"first_name":              a.FirstName,
		"last_name":               a.LastName,
		"display_name":            a.DisplayName,
		"slmc_number":             a.SLMCNumber,
		"specialty":               a.Specialty,
		"languages":               languagesToStrings(a.Languages),
		"language_other":          a.LanguageOther,
		"experience_years":        a.ExperienceYears,
		"fee_cents":               a.FeeCents,
		"required_fee_cents":      a.RequiredFeeCents,
		"bio":                     a.Bio,
		"pgim_board_certified":    a.PGIMBoardCertified,
		"medical_school":          a.MedicalSchool,
		"qualifications":          a.QualificationsText,
		"availability_notes":      a.AvailabilityNotes,
		"is_general_practitioner": a.IsGeneralPractitioner,
		"practicing_locations":    a.PracticingLocations,
		"bank_name":               a.BankName,
		"bank_branch":             a.BankBranch,
		"bank_details_submitted":  a.BankEncrypted != "",
		"terms_accepted":          a.TermsAcceptedAt != nil,
		"status":                  string(a.Status),
		"created_at":              a.CreatedAt,
	}
	if a.TermsAcceptedAt != nil {
		out["terms_accepted_at"] = a.TermsAcceptedAt
	}
	if a.RejectionReason != "" {
		out["rejection_reason"] = a.RejectionReason
	}
	return out
}

func (h *Handler) applyDocument(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "applicationID", chi.URLParam)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	// Body is already bounded by MaxBytesReader, so ParseMultipartForm cannot
	// read past the cap regardless of its own in-memory threshold.
	r.Body = http.MaxBytesReader(w, r.Body, MaxApplyDocumentBytes+1<<20)
	if err := r.ParseMultipartForm(MaxApplyDocumentBytes + 1<<20); err != nil { //nolint:gosec // bounded by MaxBytesReader above
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			httpx.Error(w, r, httpx.NewError(http.StatusRequestEntityTooLarge, httpx.CodeBadRequest, "document is too large"))
			return
		}
		httpx.Error(w, r, httpx.NewError(http.StatusUnprocessableEntity, httpx.CodeValidation, "could not read the uploaded file"))
		return
	}
	docType := DocumentType(r.FormValue("document_type"))
	file, header, err := r.FormFile("file")
	if err != nil {
		httpx.Error(w, r, httpx.NewError(http.StatusUnprocessableEntity, httpx.CodeValidation, "file is required"))
		return
	}
	defer func() { _ = file.Close() }()
	body, err := io.ReadAll(file)
	if err != nil {
		writeError(w, r, err)
		return
	}
	filename := ""
	contentType := "application/octet-stream"
	if header != nil {
		filename = header.Filename
		if header.Header.Get("Content-Type") != "" {
			contentType = header.Header.Get("Content-Type")
		}
	}
	if err := h.svc.SaveApplyDocument(r.Context(), id, docType, filename, contentType, body); err != nil {
		writeError(w, r, err)
		return
	}
	httpx.Created(w, r, map[string]any{
		"application_id": id.String(),
		"document_type":  string(docType),
		"filename":       filename,
	})
}

func (h *Handler) adminGetApplication(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id", chi.URLParam)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	app, err := h.svc.repo.GetApplication(r.Context(), id)
	if err != nil {
		writeError(w, r, err)
		return
	}
	includeBytes := r.URL.Query().Get("include") == "bytes"
	docs, err := h.svc.ListApplyDocuments(r.Context(), id, includeBytes)
	if err != nil {
		writeError(w, r, err)
		return
	}
	listed := make([]map[string]any, 0, len(docs))
	for i := range docs {
		d := &docs[i]
		item := map[string]any{
			"document_type": string(d.DocumentType),
			"filename":      d.Filename,
			"content_type":  d.ContentType,
			"uploaded_at":   d.UploadedAt,
		}
		if includeBytes && len(d.Bytes) > 0 {
			item["data_base64"] = base64.StdEncoding.EncodeToString(d.Bytes)
		}
		listed = append(listed, item)
	}
	out := toApplicationResponse(app)
	out["documents"] = listed
	httpx.OK(w, r, out)
}

func (h *Handler) adminGetApplicationDocument(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id", chi.URLParam)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	docType := DocumentType(chi.URLParam(r, "docType"))
	doc, err := h.svc.GetApplyDocument(r.Context(), id, docType)
	if err != nil {
		writeError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", doc.ContentType)
	w.Header().Set("Content-Disposition", `inline; filename="`+safeContentDispositionName(doc.Filename)+`"`)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(doc.Bytes)
}

func safeContentDispositionName(name string) string {
	name = strings.Map(func(r rune) rune {
		if r == '"' || r == '\\' || r == '\n' || r == '\r' {
			return -1
		}
		return r
	}, name)
	if strings.TrimSpace(name) == "" {
		return "document"
	}
	return name
}

// getByUserID handles GET /internal/doctors/by-user/{userID}.
func (h *Handler) getByUserID(w http.ResponseWriter, r *http.Request) {
	userID, err := httpx.PathUUID(r, "userID", chi.URLParam)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	doc, err := h.svc.GetMine(r.Context(), userID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	httpx.OK(w, r, map[string]string{"doctor_id": doc.ID.String()})
}
