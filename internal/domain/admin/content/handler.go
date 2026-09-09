package content

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"telemed/internal/domain/admin/adminusers"
	"telemed/internal/platform/httpx"
)

type Handler struct{ svc *Service }

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// Routes mounts /specialties, /symptoms, /drugs and /articles under whatever
// prefix main.go mounts this at (/api/v1/admin/content).
func (h *Handler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Get("/specialties", h.listSpecialties)
	r.Post("/specialties", h.createSpecialty)
	r.Put("/specialties/{id}", h.updateSpecialty)

	r.Get("/symptoms", h.listSymptoms)
	r.Post("/symptoms", h.createSymptom)
	r.Put("/symptoms/{id}", h.updateSymptom)

	r.Get("/drugs", h.listDrugs)
	r.Post("/drugs", h.createDrug)
	r.Put("/drugs/{id}", h.updateDrug)

	r.Get("/articles", h.listArticles)
	r.Post("/articles", h.createArticle)
	r.Put("/articles/{id}", h.updateArticle)
	return r
}

// --- specialties -------------------------------------------------------

type specialtyRequest struct {
	Code    string `json:"code" validate:"omitempty,lowercase,max=64"`
	NameEN  string `json:"name_en" validate:"required,max=200"`
	NameSI  string `json:"name_si" validate:"max=200"`
	NameTA  string `json:"name_ta" validate:"max=200"`
	Active  bool   `json:"active"`
	Version int    `json:"version"`
}

func specialtyDTO(s Specialty) map[string]any {
	return map[string]any{
		"id": s.ID, "code": s.Code, "name_en": s.NameEN, "name_si": s.NameSI, "name_ta": s.NameTA,
		"active": s.Active, "version": s.Version, "created_at": s.CreatedAt, "updated_at": s.UpdatedAt,
	}
}

func (h *Handler) listSpecialties(w http.ResponseWriter, r *http.Request) {
	items, err := h.svc.ListSpecialties(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	dtos := make([]map[string]any, len(items))
	for i := range items {
		dtos[i] = specialtyDTO(items[i])
	}
	httpx.OK(w, r, dtos)
}

func (h *Handler) createSpecialty(w http.ResponseWriter, r *http.Request) {
	var body specialtyRequest
	if err := httpx.DecodeJSON(w, r, &body); err != nil {
		httpx.Error(w, r, err)
		return
	}
	if body.Code == "" {
		httpx.Error(w, r, httpx.NewError(http.StatusBadRequest, httpx.CodeBadRequest, "code is required"))
		return
	}
	s, err := h.svc.SaveSpecialty(r.Context(), uuid.Nil, Specialty{Code: body.Code, NameEN: body.NameEN, NameSI: body.NameSI, NameTA: body.NameTA, Active: true}, 0)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.Created(w, r, specialtyDTO(s))
}

func (h *Handler) updateSpecialty(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id", chi.URLParam)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var body specialtyRequest
	if err := httpx.DecodeJSON(w, r, &body); err != nil {
		httpx.Error(w, r, err)
		return
	}
	s, err := h.svc.SaveSpecialty(r.Context(), id, Specialty{NameEN: body.NameEN, NameSI: body.NameSI, NameTA: body.NameTA, Active: body.Active}, body.Version)
	if errors.Is(err, ErrNotFound) {
		httpx.Error(w, r, httpx.NewError(http.StatusConflict, httpx.CodeConflict, "specialty was modified since last read, or does not exist"))
		return
	}
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, specialtyDTO(s))
}

// --- symptoms ------------------------------------------------------------

type symptomRequest struct {
	Code           string   `json:"code"`
	NameEN         string   `json:"name_en" validate:"required,max=200"`
	NameSI         string   `json:"name_si" validate:"max=200"`
	NameTA         string   `json:"name_ta" validate:"max=200"`
	SpecialtyCodes []string `json:"specialty_codes"`
	Active         bool     `json:"active"`
	Version        int      `json:"version"`
}

func symptomDTO(s Symptom) map[string]any {
	return map[string]any{
		"id": s.ID, "code": s.Code, "name_en": s.NameEN, "name_si": s.NameSI, "name_ta": s.NameTA,
		"specialty_codes": s.SpecialtyCodes, "active": s.Active, "version": s.Version,
		"created_at": s.CreatedAt, "updated_at": s.UpdatedAt,
	}
}

func (h *Handler) listSymptoms(w http.ResponseWriter, r *http.Request) {
	items, err := h.svc.ListSymptoms(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	dtos := make([]map[string]any, len(items))
	for i := range items {
		dtos[i] = symptomDTO(items[i])
	}
	httpx.OK(w, r, dtos)
}

func (h *Handler) createSymptom(w http.ResponseWriter, r *http.Request) {
	var body symptomRequest
	if err := httpx.DecodeJSON(w, r, &body); err != nil {
		httpx.Error(w, r, err)
		return
	}
	if body.Code == "" {
		httpx.Error(w, r, httpx.NewError(http.StatusBadRequest, httpx.CodeBadRequest, "code is required"))
		return
	}
	s, err := h.svc.SaveSymptom(r.Context(), uuid.Nil, Symptom{Code: body.Code, NameEN: body.NameEN, NameSI: body.NameSI, NameTA: body.NameTA, SpecialtyCodes: body.SpecialtyCodes, Active: true}, 0)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.Created(w, r, symptomDTO(s))
}

func (h *Handler) updateSymptom(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id", chi.URLParam)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var body symptomRequest
	if err := httpx.DecodeJSON(w, r, &body); err != nil {
		httpx.Error(w, r, err)
		return
	}
	s, err := h.svc.SaveSymptom(r.Context(), id, Symptom{NameEN: body.NameEN, NameSI: body.NameSI, NameTA: body.NameTA, SpecialtyCodes: body.SpecialtyCodes, Active: body.Active}, body.Version)
	if errors.Is(err, ErrNotFound) {
		httpx.Error(w, r, httpx.NewError(http.StatusConflict, httpx.CodeConflict, "symptom was modified since last read, or does not exist"))
		return
	}
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, symptomDTO(s))
}

// --- drugs -----------------------------------------------------------------

type drugRequest struct {
	Name         string `json:"name" validate:"required,max=200"`
	Strength     string `json:"strength" validate:"max=100"`
	Form         string `json:"form" validate:"max=100"`
	Manufacturer string `json:"manufacturer" validate:"max=200"`
	Active       bool   `json:"active"`
	Version      int    `json:"version"`
}

func drugDTO(d Drug) map[string]any {
	return map[string]any{
		"id": d.ID, "name": d.Name, "strength": d.Strength, "form": d.Form, "manufacturer": d.Manufacturer,
		"active": d.Active, "version": d.Version, "created_at": d.CreatedAt, "updated_at": d.UpdatedAt,
	}
}

func (h *Handler) listDrugs(w http.ResponseWriter, r *http.Request) {
	items, err := h.svc.ListDrugs(r.Context(), r.URL.Query().Get("search"))
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	dtos := make([]map[string]any, len(items))
	for i := range items {
		dtos[i] = drugDTO(items[i])
	}
	httpx.OK(w, r, dtos)
}

func (h *Handler) createDrug(w http.ResponseWriter, r *http.Request) {
	var body drugRequest
	if err := httpx.DecodeJSON(w, r, &body); err != nil {
		httpx.Error(w, r, err)
		return
	}
	d, err := h.svc.SaveDrug(r.Context(), uuid.Nil, Drug{Name: body.Name, Strength: body.Strength, Form: body.Form, Manufacturer: body.Manufacturer, Active: true}, 0)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.Created(w, r, drugDTO(d))
}

func (h *Handler) updateDrug(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id", chi.URLParam)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var body drugRequest
	if err := httpx.DecodeJSON(w, r, &body); err != nil {
		httpx.Error(w, r, err)
		return
	}
	d, err := h.svc.SaveDrug(r.Context(), id, Drug{Name: body.Name, Strength: body.Strength, Form: body.Form, Manufacturer: body.Manufacturer, Active: body.Active}, body.Version)
	if errors.Is(err, ErrNotFound) {
		httpx.Error(w, r, httpx.NewError(http.StatusConflict, httpx.CodeConflict, "drug was modified since last read, or does not exist"))
		return
	}
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, drugDTO(d))
}

// --- articles ------------------------------------------------------------

type articleRequest struct {
	Title         string `json:"title" validate:"required,max=300"`
	Slug          string `json:"slug" validate:"omitempty,lowercase,max=200"`
	Body          string `json:"body" validate:"required"`
	Language      string `json:"language" validate:"required,oneof=en si ta"`
	SpecialtyCode string `json:"specialty_code"`
	Published     bool   `json:"published"`
	Version       int    `json:"version"`
}

func articleDTO(a Article) map[string]any {
	return map[string]any{
		"id": a.ID, "title": a.Title, "slug": a.Slug, "body": a.Body, "language": a.Language,
		"specialty_code": a.SpecialtyCode, "published": a.Published, "published_at": a.PublishedAt,
		"version": a.Version, "created_at": a.CreatedAt, "updated_at": a.UpdatedAt,
	}
}

func (h *Handler) listArticles(w http.ResponseWriter, r *http.Request) {
	publishedOnly := r.URL.Query().Get("published") == "true"
	items, err := h.svc.ListArticles(r.Context(), publishedOnly)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	dtos := make([]map[string]any, len(items))
	for i := range items {
		dtos[i] = articleDTO(items[i])
	}
	httpx.OK(w, r, dtos)
}

func (h *Handler) createArticle(w http.ResponseWriter, r *http.Request) {
	var body articleRequest
	if err := httpx.DecodeJSON(w, r, &body); err != nil {
		httpx.Error(w, r, err)
		return
	}
	if body.Slug == "" {
		httpx.Error(w, r, httpx.NewError(http.StatusBadRequest, httpx.CodeBadRequest, "slug is required"))
		return
	}
	actor, _ := adminusers.FromContext(r.Context())
	a, err := h.svc.SaveArticle(r.Context(), uuid.Nil, Article{Title: body.Title, Slug: body.Slug, Body: body.Body, Language: body.Language, SpecialtyCode: body.SpecialtyCode, Published: body.Published}, 0, actor.ID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.Created(w, r, articleDTO(a))
}

func (h *Handler) updateArticle(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id", chi.URLParam)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var body articleRequest
	if err := httpx.DecodeJSON(w, r, &body); err != nil {
		httpx.Error(w, r, err)
		return
	}
	actor, _ := adminusers.FromContext(r.Context())
	a, err := h.svc.SaveArticle(r.Context(), id, Article{Title: body.Title, Body: body.Body, Language: body.Language, SpecialtyCode: body.SpecialtyCode, Published: body.Published}, body.Version, actor.ID)
	if errors.Is(err, ErrNotFound) {
		httpx.Error(w, r, httpx.NewError(http.StatusConflict, httpx.CodeConflict, "article was modified since last read, or does not exist"))
		return
	}
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, articleDTO(a))
}
