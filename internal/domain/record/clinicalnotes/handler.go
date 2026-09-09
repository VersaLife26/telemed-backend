package clinicalnotes

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"telemed/internal/platform/httpx"
	"telemed/internal/platform/middleware"
)

// Handler wires HTTP to Service. No SQL and no business rule lives here
// (AGENT-BRIEF layering rule): it decodes, calls, and re-encodes.
type Handler struct {
	svc *Service
}

// NewHandler builds the clinical notes HTTP handler.
func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// Routes mounts the /clinical-notes/* tree. Mount under /api/v1/clinical-notes
// behind RequireAuth and NoStore.
//
// The appointment id is the resource key, not a note id. That is what lets
// the doctor app autosave with PUT from the first keystroke: it already knows
// the appointment it is in, so it never has to create a note and wait to
// learn an id before it can save the second character.
func (h *Handler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Get("/", h.list)
	r.Put("/{appointment_id}", h.save)
	r.Get("/{appointment_id}", h.get)
	r.Post("/{appointment_id}/finalise", h.finalise)
	r.Post("/{appointment_id}/amend", h.amend)
	r.Get("/{appointment_id}/revisions", h.revisions)
	return r
}

// ICD10Routes mounts the authenticated ICD-10 reference search. It carries no
// patient data at all -- it is a published classification -- so it needs no
// access-log entry and no role gate beyond authentication, exactly like the
// drug formulary at /drugs.
func (h *Handler) ICD10Routes() chi.Router {
	r := chi.NewRouter()
	r.Get("/", h.searchICD10)
	return r
}

// --- request shapes -----------------------------------------------------

// diagnosisRequest is one ICD-10 code as the client sends it.
//
// The JSON field is "description", not "display", because that is what the
// doctor app's Icd10Code model already calls it and there was no reason to
// make the client rename a field to talk to us. The value is accepted and
// then IGNORED: the stored term is always re-resolved from icd10_codes, so a
// client cannot write free text into a coded field.
type diagnosisRequest struct {
	Code        string `json:"code" validate:"required,max=10"`
	Description string `json:"description" validate:"max=300"`
	IsPrimary   bool   `json:"is_primary"`
}

func toDiagnosisInputs(in []diagnosisRequest) []DiagnosisInput {
	out := make([]DiagnosisInput, len(in))
	for i, d := range in {
		out[i] = DiagnosisInput{Code: d.Code, IsPrimary: d.IsPrimary}
	}
	return out
}

type saveRequest struct {
	// AppointmentID is optional and, when present, must agree with the path.
	// It is accepted because the doctor app's SoapNote.toJson() puts it in
	// the body; rejecting the field would mean the client had to strip it.
	AppointmentID *uuid.UUID         `json:"appointment_id"`
	Subjective    string             `json:"subjective" validate:"max=20000"`
	Objective     string             `json:"objective" validate:"max=20000"`
	Assessment    string             `json:"assessment" validate:"max=20000"`
	Plan          string             `json:"plan" validate:"max=20000"`
	Diagnoses     []diagnosisRequest `json:"diagnoses" validate:"max=10,dive"`
	// Version is the optimistic-lock token from the last read. Absent or 0
	// means "I believe there is no note yet".
	Version int `json:"version" validate:"gte=0"`
	// UpdatedAt is accepted and ignored. The doctor app stamps its local
	// clock on every draft; a phone's clock is not allowed to decide when a
	// medical record was written, so the server's own updated_at stands.
	UpdatedAt string `json:"updated_at"`
}

type finaliseRequest struct {
	Version int `json:"version" validate:"required,gte=1"`
}

type amendRequest struct {
	Subjective      *string             `json:"subjective" validate:"omitempty,max=20000"`
	Objective       *string             `json:"objective" validate:"omitempty,max=20000"`
	Assessment      *string             `json:"assessment" validate:"omitempty,max=20000"`
	Plan            *string             `json:"plan" validate:"omitempty,max=20000"`
	Diagnoses       *[]diagnosisRequest `json:"diagnoses" validate:"omitempty,max=10,dive"`
	AmendmentReason string              `json:"amendment_reason" validate:"required,min=3,max=1000"`
	Version         int                 `json:"version" validate:"required,gte=1"`
}

// --- handlers -----------------------------------------------------------

func (h *Handler) save(w http.ResponseWriter, r *http.Request) {
	p := middleware.MustPrincipal(r.Context())
	appointmentID, err := httpx.PathUUID(r, "appointment_id", chi.URLParam)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	var req saveRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	if req.AppointmentID != nil && *req.AppointmentID != appointmentID {
		httpx.Error(w, r, httpx.NewError(http.StatusBadRequest, httpx.CodeBadRequest,
			"appointment_id in the body does not match the one in the path"))
		return
	}

	note, err := h.svc.Save(r.Context(), SaveInput{
		Principal: p, AppointmentID: appointmentID,
		Subjective: req.Subjective, Objective: req.Objective,
		Assessment: req.Assessment, Plan: req.Plan,
		Diagnoses: toDiagnosisInputs(req.Diagnoses), Version: req.Version,
	})
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	// 200, not 201, on every call including the first. The client is
	// autosaving the same logical resource over and over; a status code that
	// differs on the first keystroke of a consultation is a branch in the
	// client for no benefit.
	httpx.OK(w, r, toNoteResponse(note))
}

func (h *Handler) finalise(w http.ResponseWriter, r *http.Request) {
	p := middleware.MustPrincipal(r.Context())
	appointmentID, err := httpx.PathUUID(r, "appointment_id", chi.URLParam)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var req finaliseRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	note, err := h.svc.Finalise(r.Context(), FinaliseInput{
		Principal: p, AppointmentID: appointmentID, Version: req.Version,
		IPAddress: middleware.ClientIP(r), UserAgent: r.UserAgent(),
	})
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, toNoteResponse(note))
}

func (h *Handler) amend(w http.ResponseWriter, r *http.Request) {
	p := middleware.MustPrincipal(r.Context())
	appointmentID, err := httpx.PathUUID(r, "appointment_id", chi.URLParam)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var req amendRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}

	in := AmendInput{
		Principal: p, AppointmentID: appointmentID,
		Subjective: req.Subjective, Objective: req.Objective,
		Assessment: req.Assessment, Plan: req.Plan,
		Reason: req.AmendmentReason, Version: req.Version,
		IPAddress: middleware.ClientIP(r), UserAgent: r.UserAgent(),
	}
	if req.Diagnoses != nil {
		converted := toDiagnosisInputs(*req.Diagnoses)
		in.Diagnoses = &converted
	}

	note, err := h.svc.Amend(r.Context(), in)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, toNoteResponse(note))
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	p := middleware.MustPrincipal(r.Context())
	appointmentID, err := httpx.PathUUID(r, "appointment_id", chi.URLParam)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	note, err := h.svc.Get(r.Context(), p, appointmentID, middleware.ClientIP(r), r.UserAgent())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, toNoteResponse(note))
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	p := middleware.MustPrincipal(r.Context())
	page, perPage, _ := httpx.Pagination(r)
	status := Status(strings.TrimSpace(r.URL.Query().Get("status")))

	notes, total, err := h.svc.ListForDoctor(r.Context(), p, status, page, perPage)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	out := make([]noteSummaryResponse, len(notes))
	for i := range notes {
		out[i] = toNoteSummaryResponse(notes[i])
	}
	httpx.List(w, r, out, httpx.Meta{Page: page, PerPage: perPage, Total: total})
}

func (h *Handler) revisions(w http.ResponseWriter, r *http.Request) {
	p := middleware.MustPrincipal(r.Context())
	appointmentID, err := httpx.PathUUID(r, "appointment_id", chi.URLParam)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	revisions, err := h.svc.ListRevisions(r.Context(), p, appointmentID, middleware.ClientIP(r), r.UserAgent())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	out := make([]revisionResponse, len(revisions))
	for i := range revisions {
		out[i] = toRevisionResponse(revisions[i])
	}
	httpx.OK(w, r, out)
}

func (h *Handler) searchICD10(w http.ResponseWriter, r *http.Request) {
	limit := 20
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			limit = n
		}
	}
	codes, err := h.svc.SearchICD10(r.Context(), r.URL.Query().Get("q"), limit)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	out := make([]icd10Response, len(codes))
	for i, c := range codes {
		out[i] = icd10Response{Code: c.Code, Description: c.Display, Category: c.Category}
	}
	httpx.OK(w, r, out)
}

// --- response shapes ----------------------------------------------------

type diagnosisResponse struct {
	Code        string `json:"code"`
	Description string `json:"description"`
	IsPrimary   bool   `json:"is_primary"`
}

// noteResponse carries the note's clinical content. Field names match the
// doctor app's SoapNote model so its existing parser works unchanged; the
// extra fields it does not know about (id, status, version, ...) are ignored
// by a Dart fromJson, which is what makes adding them safe.
type noteResponse struct {
	ID            string              `json:"id"`
	AppointmentID string              `json:"appointment_id"`
	DoctorID      string              `json:"doctor_id"`
	PatientID     string              `json:"patient_id"`
	Subjective    string              `json:"subjective"`
	Objective     string              `json:"objective"`
	Assessment    string              `json:"assessment"`
	Plan          string              `json:"plan"`
	Diagnoses     []diagnosisResponse `json:"diagnoses"`
	Status        string              `json:"status"`
	FinalisedAt   string              `json:"finalised_at,omitempty"`
	CreatedAt     string              `json:"created_at"`
	// UpdatedAt is the server's, and it is what the client must echo back as
	// its own updated_at once it has saved.
	UpdatedAt string `json:"updated_at"`
	// Version is the optimistic-lock token. A client that autosaves must send
	// back the value it last received here, or its next save is a 409.
	Version int `json:"version"`
}

// noteSummaryResponse is the list shape. It deliberately carries no SOAP
// text and no diagnosis codes: a list endpoint that returned clinical
// content would need a per-row access-log entry, and would leak a whole
// day's diagnoses in one response.
type noteSummaryResponse struct {
	ID            string `json:"id"`
	AppointmentID string `json:"appointment_id"`
	PatientID     string `json:"patient_id"`
	Status        string `json:"status"`
	FinalisedAt   string `json:"finalised_at,omitempty"`
	CreatedAt     string `json:"created_at"`
	UpdatedAt     string `json:"updated_at"`
	Version       int    `json:"version"`
}

type revisionResponse struct {
	Revision        int                 `json:"revision"`
	ChangeType      string              `json:"change_type"`
	AmendmentReason string              `json:"amendment_reason,omitempty"`
	Subjective      string              `json:"subjective"`
	Objective       string              `json:"objective"`
	Assessment      string              `json:"assessment"`
	Plan            string              `json:"plan"`
	Diagnoses       []diagnosisResponse `json:"diagnoses"`
	ChangedBy       string              `json:"changed_by"`
	ChangedByRole   string              `json:"changed_by_role"`
	CreatedAt       string              `json:"created_at"`
}

type icd10Response struct {
	Code        string `json:"code"`
	Description string `json:"description"`
	Category    string `json:"category,omitempty"`
}

func toDiagnosisResponses(in []Diagnosis) []diagnosisResponse {
	// Never nil: the doctor app reads diagnoses as a list and a JSON null
	// where an array was promised is a decode error on a screen a doctor is
	// mid-consultation on.
	out := make([]diagnosisResponse, len(in))
	for i, d := range in {
		out[i] = diagnosisResponse{Code: d.Code, Description: d.Display, IsPrimary: d.IsPrimary}
	}
	return out
}

func toNoteResponse(n Note) noteResponse {
	resp := noteResponse{
		ID: n.ID.String(), AppointmentID: n.AppointmentID.String(),
		DoctorID: n.DoctorID.String(), PatientID: n.PatientID.String(),
		Subjective: n.Subjective, Objective: n.Objective,
		Assessment: n.Assessment, Plan: n.Plan,
		Diagnoses: toDiagnosisResponses(n.Diagnoses),
		Status:    string(n.Status),
		CreatedAt: n.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt: n.UpdatedAt.UTC().Format(time.RFC3339),
		Version:   n.Version,
	}
	if n.FinalisedAt != nil {
		resp.FinalisedAt = n.FinalisedAt.UTC().Format(time.RFC3339)
	}
	return resp
}

func toNoteSummaryResponse(n Note) noteSummaryResponse {
	resp := noteSummaryResponse{
		ID: n.ID.String(), AppointmentID: n.AppointmentID.String(),
		PatientID: n.PatientID.String(), Status: string(n.Status),
		CreatedAt: n.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt: n.UpdatedAt.UTC().Format(time.RFC3339),
		Version:   n.Version,
	}
	if n.FinalisedAt != nil {
		resp.FinalisedAt = n.FinalisedAt.UTC().Format(time.RFC3339)
	}
	return resp
}

func toRevisionResponse(rev Revision) revisionResponse {
	return revisionResponse{
		Revision: rev.Revision, ChangeType: string(rev.ChangeType),
		AmendmentReason: rev.AmendmentReason,
		Subjective:      rev.Subjective, Objective: rev.Objective,
		Assessment: rev.Assessment, Plan: rev.Plan,
		Diagnoses: toDiagnosisResponses(rev.Diagnoses),
		ChangedBy: rev.ChangedBy.String(), ChangedByRole: rev.ChangedByRole,
		CreatedAt: rev.CreatedAt.UTC().Format(time.RFC3339),
	}
}
