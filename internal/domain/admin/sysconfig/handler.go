package sysconfig

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"telemed/internal/domain/admin/adminusers"
	"telemed/internal/platform/httpx"
	"telemed/internal/platform/middleware"
)

type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// Routes mounts GET /, GET /{key}, GET /{key}/history and PUT /{key}.
func (h *Handler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Get("/", h.listCurrent)
	r.Get("/{key}", h.get)
	r.Get("/{key}/history", h.history)
	r.Put("/{key}", h.put)
	return r
}

type configDTO struct {
	Key           string          `json:"key"`
	Value         json.RawMessage `json:"value"`
	Version       int             `json:"version"`
	UpdatedBy     string          `json:"updated_by,omitempty"`
	EffectiveFrom time.Time       `json:"effective_from"`
	CreatedAt     time.Time       `json:"created_at"`
}

func toDTO(c Config) configDTO {
	d := configDTO{Key: c.Key, Value: c.Value, Version: c.Version, EffectiveFrom: c.EffectiveFrom, CreatedAt: c.CreatedAt}
	if c.UpdatedBy != uuid.Nil {
		d.UpdatedBy = c.UpdatedBy.String()
	}
	return d
}

func (h *Handler) listCurrent(w http.ResponseWriter, r *http.Request) {
	configs, err := h.svc.CurrentAll(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	dtos := make([]configDTO, len(configs))
	for i, c := range configs {
		dtos[i] = toDTO(c)
	}
	httpx.OK(w, r, dtos)
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	key := chi.URLParam(r, "key")
	cfg, err := h.svc.Get(r.Context(), key)
	switch {
	case errors.Is(err, ErrInvalidKey):
		httpx.Error(w, r, httpx.NewError(http.StatusBadRequest, httpx.CodeBadRequest, err.Error()))
		return
	case errors.Is(err, ErrNotFound):
		httpx.Error(w, r, httpx.ErrNotFound)
		return
	case err != nil:
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, toDTO(cfg))
}

func (h *Handler) history(w http.ResponseWriter, r *http.Request) {
	key := chi.URLParam(r, "key")
	configs, err := h.svc.History(r.Context(), key)
	switch {
	case errors.Is(err, ErrInvalidKey):
		httpx.Error(w, r, httpx.NewError(http.StatusBadRequest, httpx.CodeBadRequest, err.Error()))
		return
	case err != nil:
		httpx.Error(w, r, err)
		return
	}
	dtos := make([]configDTO, len(configs))
	for i, c := range configs {
		dtos[i] = toDTO(c)
	}
	httpx.OK(w, r, dtos)
}

type putRequest struct {
	Value         json.RawMessage `json:"value" validate:"required"`
	EffectiveFrom *time.Time      `json:"effective_from"`
}

func (h *Handler) put(w http.ResponseWriter, r *http.Request) {
	key := chi.URLParam(r, "key")
	var body putRequest
	if err := httpx.DecodeJSON(w, r, &body); err != nil {
		httpx.Error(w, r, err)
		return
	}

	actor, _ := adminusers.FromContext(r.Context())
	var effectiveFrom time.Time
	if body.EffectiveFrom != nil {
		effectiveFrom = *body.EffectiveFrom
	}

	cfg, err := h.svc.Put(r.Context(), middleware.MustPrincipal(r.Context()), actor.ID, key, body.Value, effectiveFrom)
	switch {
	case errors.Is(err, ErrInvalidKey), errors.Is(err, ErrUnknownKey):
		httpx.Error(w, r, httpx.NewError(http.StatusBadRequest, httpx.CodeBadRequest, err.Error()))
		return
	case errors.Is(err, ErrKeyNotPermitted):
		// Deliberately not a 404: the key exists, the caller may read it, and
		// pretending otherwise makes the console's error message a lie.
		httpx.Error(w, r, httpx.NewError(http.StatusForbidden, httpx.CodeForbidden, err.Error()))
		return
	case err != nil:
		httpx.Error(w, r, err)
		return
	}
	httpx.Created(w, r, toDTO(cfg))
}
