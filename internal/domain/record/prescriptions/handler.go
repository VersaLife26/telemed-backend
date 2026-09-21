package prescriptions

import (
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"telemed/internal/platform/httpx"
	"telemed/internal/platform/middleware"
	"telemed/internal/platform/storage"
)

// Handler wires HTTP to Service.
type Handler struct {
	svc *Service
}

// NewHandler builds the prescriptions HTTP handler.
func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// Routes mounts the authenticated /prescriptions/* tree.
func (h *Handler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Post("/", h.issue)
	r.Get("/", h.getByAppointment)
	r.Get("/{id}", h.get)
	r.Get("/{id}/pdf", h.pdf)
	return r
}

// DrugRoutes mounts the authenticated formulary search endpoint.
func (h *Handler) DrugRoutes() chi.Router {
	r := chi.NewRouter()
	r.Get("/", h.searchDrugs)
	return r
}

// VerifyRoutes mounts the PUBLIC, unauthenticated verification endpoint.
// Callers must apply a strict rate limit when mounting this (see main.go):
// it is an unauthenticated endpoint backed by a database read, exactly the
// shape AGENT-BRIEF warns to rate-limit hard.
func (h *Handler) VerifyRoutes() chi.Router {
	r := chi.NewRouter()
	r.Get("/{id}", h.verify)
	return r
}

type issueItemRequest struct {
	DrugName     string `json:"drug_name" validate:"required"`
	Strength     string `json:"strength"`
	Form         string `json:"form"`
	Dosage       string `json:"dosage" validate:"required"`
	Frequency    string `json:"frequency" validate:"required"`
	DurationDays int    `json:"duration_days" validate:"required,min=1,max=365"`
	Quantity     int    `json:"quantity" validate:"required,min=1"`
	Instructions string `json:"instructions" validate:"max=500"`
	IsGeneric    bool   `json:"is_generic"`
}

type issueRequest struct {
	AppointmentID        uuid.UUID          `json:"appointment_id" validate:"required"`
	DoctorName           string             `json:"doctor_name" validate:"required"`
	DoctorSLMC           string             `json:"doctor_slmc" validate:"required,slmc"`
	DoctorQualifications string             `json:"doctor_qualifications" validate:"max=200"`
	ClinicName           string             `json:"clinic_name" validate:"max=200"`
	PatientName          string             `json:"patient_name" validate:"required"`
	PatientAge           int                `json:"patient_age" validate:"min=0,max=130"`
	PatientNIC           string             `json:"patient_nic" validate:"max=20"`
	Items                []issueItemRequest `json:"items" validate:"required,min=1,max=20,dive"`
}

func (h *Handler) issue(w http.ResponseWriter, r *http.Request) {
	p := middleware.MustPrincipal(r.Context())
	if !p.HasRole(middleware.RoleDoctor) {
		httpx.Error(w, r, httpx.ErrForbidden)
		return
	}

	var req issueRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}

	// issueItemRequest and ItemInput have identical fields in the same
	// order, so a direct type conversion carries every field without
	// restating each one and risking the two falling out of sync silently.
	items := make([]ItemInput, len(req.Items))
	for i, it := range req.Items {
		items[i] = ItemInput(it)
	}

	created, err := h.svc.Issue(r.Context(), IssueInput{
		Principal: p, AppointmentID: req.AppointmentID,
		DoctorName: req.DoctorName, DoctorSLMC: req.DoctorSLMC, DoctorQualifications: req.DoctorQualifications,
		ClinicName: req.ClinicName,
		Patient:    PatientDisplay{Name: req.PatientName, Age: req.PatientAge, NIC: req.PatientNIC},
		Items:      items,
	})
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.Created(w, r, toPrescriptionResponse(created))
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	p := middleware.MustPrincipal(r.Context())
	id, err := httpx.PathUUID(r, "id", chi.URLParam)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	presc, err := h.svc.Get(r.Context(), p, id, middleware.ClientIP(r), r.UserAgent())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, toPrescriptionResponse(presc))
}

func (h *Handler) getByAppointment(w http.ResponseWriter, r *http.Request) {
	p := middleware.MustPrincipal(r.Context())
	appointmentID, ok, err := httpx.QueryUUID(r, "appointment_id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if !ok {
		httpx.Error(w, r, httpx.NewError(http.StatusBadRequest, httpx.CodeBadRequest, "appointment_id is required"))
		return
	}
	presc, err := h.svc.GetByAppointment(r.Context(), p, appointmentID, middleware.ClientIP(r), r.UserAgent())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, toPrescriptionResponse(presc))
}

func (h *Handler) pdf(w http.ResponseWriter, r *http.Request) {
	p := middleware.MustPrincipal(r.Context())
	id, err := httpx.PathUUID(r, "id", chi.URLParam)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	body, filename, err := h.svc.PDF(r.Context(), p, id, middleware.ClientIP(r), r.UserAgent())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	defer func() { _ = body.Close() }()

	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Disposition", storage.AttachmentDisposition(filename))
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, body)
}

func (h *Handler) verify(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id", chi.URLParam)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	hmacParam := r.URL.Query().Get("h")
	if hmacParam == "" {
		httpx.Error(w, r, httpx.NewError(http.StatusBadRequest, httpx.CodeBadRequest, "h query parameter is required"))
		return
	}

	result, err := h.svc.Verify(r.Context(), id, hmacParam, middleware.ClientIP(r), r.UserAgent())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if !result.Valid {
		httpx.OK(w, r, map[string]any{"valid": false})
		return
	}
	httpx.OK(w, r, map[string]any{
		"valid":       true,
		"doctor_name": result.DoctorName,
		"doctor_slmc": result.DoctorSLMC,
		"issued_at":   result.IssuedAt.UTC().Format(time.RFC3339),
		"status":      string(result.Status),
		"dispensed":   result.Status == StatusDispensed,
	})
}

func (h *Handler) searchDrugs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	limit := 20
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			limit = n
		}
	}
	drugs, err := h.svc.SearchDrugs(r.Context(), q, limit)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	out := make([]drugResponse, len(drugs))
	for i, d := range drugs {
		out[i] = toDrugResponse(d)
	}
	httpx.OK(w, r, out)
}

// --- response shapes ---------------------------------------------------

type itemResponse struct {
	DrugName     string `json:"drug_name"`
	Strength     string `json:"strength"`
	Form         string `json:"form"`
	Dosage       string `json:"dosage"`
	Frequency    string `json:"frequency"`
	DurationDays int    `json:"duration_days"`
	Quantity     int    `json:"quantity"`
	Instructions string `json:"instructions,omitempty"`
	IsGeneric    bool   `json:"is_generic"`
}

type prescriptionResponse struct {
	ID                      string         `json:"id"`
	AppointmentID           string         `json:"appointment_id"`
	DoctorID                string         `json:"doctor_id"`
	PatientID               string         `json:"patient_id"`
	DoctorName              string         `json:"doctor_name"`
	DoctorSLMC              string         `json:"doctor_slmc"`
	IssuedAt                string         `json:"issued_at"`
	Status                  string         `json:"status"`
	VerificationHMAC        string         `json:"verification_hmac"`
	FHIRMedicationRequestID string         `json:"fhir_medication_request_id,omitempty"`
	Items                   []itemResponse `json:"items"`
}

func toPrescriptionResponse(p Prescription) prescriptionResponse {
	items := make([]itemResponse, len(p.Items))
	for i := range p.Items {
		it := &p.Items[i]
		items[i] = itemResponse{
			DrugName: it.DrugName, Strength: it.Strength, Form: it.Form, Dosage: it.Dosage,
			Frequency: it.Frequency, DurationDays: it.DurationDays, Quantity: it.Quantity,
			Instructions: it.Instructions, IsGeneric: it.IsGeneric,
		}
	}
	return prescriptionResponse{
		ID: p.ID.String(), AppointmentID: p.AppointmentID.String(), DoctorID: p.DoctorID.String(),
		PatientID: p.PatientID.String(), DoctorName: p.DoctorName, DoctorSLMC: p.DoctorSLMC,
		IssuedAt: p.IssuedAt.UTC().Format(time.RFC3339), Status: string(p.Status),
		VerificationHMAC: p.VerificationHMAC, FHIRMedicationRequestID: p.FHIRMedicationRequestID, Items: items,
	}
}

type drugResponse struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	GenericName  string `json:"generic_name"`
	Strength     string `json:"strength"`
	Form         string `json:"form"`
	Manufacturer string `json:"manufacturer,omitempty"`
	Category     string `json:"category,omitempty"`
	IsControlled bool   `json:"is_controlled"`
	IsGeneric    bool   `json:"is_generic"`
}

func toDrugResponse(d Drug) drugResponse {
	return drugResponse{
		ID: d.ID.String(), Name: d.Name, GenericName: d.GenericName, Strength: d.Strength, Form: d.Form,
		Manufacturer: d.Manufacturer, Category: d.Category, IsControlled: d.IsControlled, IsGeneric: d.IsGeneric,
	}
}
