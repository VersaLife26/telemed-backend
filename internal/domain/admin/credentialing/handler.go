package credentialing

import (
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"telemed/internal/domain/admin/adminusers"
	"telemed/internal/platform/httpx"
)

type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// Routes mounts GET /pending, GET /{id}, POST /{id}/verify, PUT /{id}/checklist
// under whatever prefix main.go mounts this at (/api/v1/admin/doctors).
func (h *Handler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Get("/pending", h.pending)
	r.Get("/{id}", h.get)
	r.Get("/{id}/checklist", h.getChecklist)
	r.Post("/{id}/verify", h.verify)
	r.Put("/{id}/checklist", h.updateChecklist)
	return r
}

type doctorDTO struct {
	DoctorID             string    `json:"doctor_id"`
	FullName             string    `json:"full_name"`
	Email                string    `json:"email"`
	Phone                string    `json:"phone"`
	SLMCNumber           string    `json:"slmc_number"`
	YearsExperience      int       `json:"years_experience"`
	SpecialtyCode        string    `json:"specialty_code"`
	VerificationStatus   string    `json:"verification_status"`
	RegisteredAt         time.Time `json:"registered_at"`
	SLMCCertificateURL   string    `json:"slmc_certificate_url,omitempty"`
	NICDocumentURL       string    `json:"nic_document_url,omitempty"`
	DegreeCertificateURL string    `json:"degree_certificate_url,omitempty"`
	PhotoURL             string    `json:"photo_url,omitempty"`
	DocumentsExpireAt    time.Time `json:"documents_expire_at,omitempty"`
}

func toDoctorDTO(d PresignedDoctorSummary) doctorDTO {
	return doctorDTO{
		DoctorID: d.DoctorID.String(), FullName: d.FullName, Email: d.Email, Phone: d.Phone,
		SLMCNumber: d.SLMCNumber, YearsExperience: d.YearsExperience, SpecialtyCode: d.SpecialtyCode,
		VerificationStatus: d.VerificationStatus, RegisteredAt: d.RegisteredAt,
		SLMCCertificateURL: d.SLMCCertificateURL, NICDocumentURL: d.NICDocumentURL,
		DegreeCertificateURL: d.DegreeCertificateURL, PhotoURL: d.PhotoURL,
		DocumentsExpireAt: d.DocumentsExpireAt,
	}
}

type checklistDTO struct {
	ID                  string `json:"id,omitempty"`
	DoctorID            string `json:"doctor_id"`
	SLMCFormatValid     *bool  `json:"slmc_format_valid"`
	SLMCRegistryChecked *bool  `json:"slmc_registry_checked"`
	ExperienceVerified  *bool  `json:"experience_verified"`
	NICMatches          *bool  `json:"nic_matches"`
	PhotoClear          *bool  `json:"photo_clear"`
	OverallStatus       string `json:"overall_status"`
	DecisionReason      string `json:"decision_reason,omitempty"`
	Version             int    `json:"version,omitempty"`
}

func toChecklistDTO(c Checklist) checklistDTO {
	d := checklistDTO{
		DoctorID: c.DoctorID.String(), SLMCFormatValid: c.SLMCFormatValid,
		SLMCRegistryChecked: c.SLMCRegistryChecked, ExperienceVerified: c.ExperienceVerified,
		NICMatches: c.NICMatches, PhotoClear: c.PhotoClear, OverallStatus: c.OverallStatus,
		DecisionReason: c.DecisionReason, Version: c.Version,
	}
	if c.ID != uuid.Nil {
		d.ID = c.ID.String()
	}
	if d.OverallStatus == "" {
		d.OverallStatus = "not_started"
	}
	return d
}

func (h *Handler) pending(w http.ResponseWriter, r *http.Request) {
	page, perPage, _ := httpx.Pagination(r)
	docs, total, err := h.svc.ListPending(r.Context(), page, perPage)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	dtos := make([]doctorDTO, len(docs))
	for i := range docs {
		dtos[i] = toDoctorDTO(docs[i])
	}
	httpx.List(w, r, dtos, httpx.Meta{Page: page, PerPage: perPage, Total: total})
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id", chi.URLParam)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	doctor, checklist, err := h.svc.GetDoctor(r.Context(), id)
	if errors.Is(err, ErrNotFound) {
		httpx.Error(w, r, httpx.ErrNotFound)
		return
	}
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, map[string]any{
		"doctor":    toDoctorDTO(doctor),
		"checklist": toChecklistDTO(checklist),
	})
}

func (h *Handler) getChecklist(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id", chi.URLParam)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	_, checklist, err := h.svc.GetDoctor(r.Context(), id)
	if errors.Is(err, ErrNotFound) {
		httpx.Error(w, r, httpx.ErrNotFound)
		return
	}
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, toChecklistDTO(checklist))
}

type checklistUpdateRequest struct {
	SLMCFormatValid     *bool `json:"slmc_format_valid"`
	SLMCRegistryChecked *bool `json:"slmc_registry_checked"`
	ExperienceVerified  *bool `json:"experience_verified"`
	NICMatches          *bool `json:"nic_matches"`
	PhotoClear          *bool `json:"photo_clear"`
}

func (h *Handler) updateChecklist(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id", chi.URLParam)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var body checklistUpdateRequest
	if err := httpx.DecodeJSON(w, r, &body); err != nil {
		httpx.Error(w, r, err)
		return
	}
	actor, _ := adminusers.FromContext(r.Context())

	updated, err := h.svc.UpdateChecklist(r.Context(), id, ChecklistUpdate{
		SLMCFormatValid: body.SLMCFormatValid, SLMCRegistryChecked: body.SLMCRegistryChecked,
		ExperienceVerified: body.ExperienceVerified, NICMatches: body.NICMatches,
		PhotoClear: body.PhotoClear, CheckerID: actor.ID,
	})
	if errors.Is(err, ErrNotFound) {
		httpx.Error(w, r, httpx.ErrNotFound)
		return
	}
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, toChecklistDTO(updated))
}

type verifyRequest struct {
	Action string `json:"action" validate:"required,oneof=approve reject"`
	Reason string `json:"reason" validate:"required_if=Action reject,max=1000"`
}

func (h *Handler) verify(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id", chi.URLParam)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var body verifyRequest
	if err := httpx.DecodeJSON(w, r, &body); err != nil {
		httpx.Error(w, r, err)
		return
	}
	actor, _ := adminusers.FromContext(r.Context())

	result, err := h.svc.Verify(r.Context(), id, VerifyDecision{
		Approve: body.Action == "approve", Reason: body.Reason, DeciderID: actor.ID,
	})
	if errors.Is(err, ErrNotFound) {
		httpx.Error(w, r, httpx.NewError(http.StatusConflict, httpx.CodeConflict,
			"doctor has no pending checklist to decide (already decided, or checklist never started)"))
		return
	}
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, toChecklistDTO(result))
}
