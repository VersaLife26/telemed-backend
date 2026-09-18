package users

import (
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"telemed/internal/domain/admin/adminusers"
	"telemed/internal/platform/httpx"
)

type Handler struct{ svc *Service }

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// Routes mounts GET /, GET /{id}, GET /{id}/activity, POST /{id}/suspend,
// POST /{id}/reinstate. Impersonation is deliberately absent -- see docs/DESIGN.md.
func (h *Handler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Get("/", h.list)
	r.Get("/{id}/activity", h.activity)
	r.Get("/{id}", h.get)
	r.Post("/{id}/suspend", h.suspend)
	r.Post("/{id}/reinstate", h.reinstate)
	return r
}

type userDTO struct {
	UserID       string    `json:"user_id"`
	FullName     string    `json:"full_name"`
	Email        string    `json:"email"`
	Phone        string    `json:"phone"`
	Role         string    `json:"role"`
	Status       string    `json:"status"`
	RegisteredAt time.Time `json:"registered_at,omitempty"`
}

func toDTO(u User) userDTO {
	return userDTO{UserID: u.UserID.String(), FullName: u.FullName, Email: u.Email, Phone: u.Phone, Role: u.Role, Status: u.Status, RegisteredAt: u.RegisteredAt}
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	page, perPage, _ := httpx.Pagination(r)
	q := r.URL.Query()
	items, total, err := h.svc.List(r.Context(), ListFilter{
		Query: firstNonEmpty(q.Get("query"), q.Get("q")), Role: q.Get("role"), Status: q.Get("status"), Page: page, PerPage: perPage,
	})
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	dtos := make([]userDTO, len(items))
	for i, u := range items {
		dtos[i] = toDTO(u)
	}
	httpx.List(w, r, dtos, httpx.Meta{Page: page, PerPage: perPage, Total: total})
}

type activityDTO struct {
	OccurredAt  time.Time `json:"occurred_at"`
	Kind        string    `json:"kind"`
	Summary     string    `json:"summary"`
	ReferenceID *string   `json:"reference_id"`
}

func toActivityDTO(e ActivityEntry) activityDTO {
	d := activityDTO{
		OccurredAt: e.OccurredAt,
		Kind:       e.Kind,
		Summary:    e.Summary,
	}
	if e.ReferenceID != nil && *e.ReferenceID != uuid.Nil {
		id := e.ReferenceID.String()
		d.ReferenceID = &id
	}
	return d
}

func (h *Handler) activity(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id", chi.URLParam)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	page, perPage, _ := httpx.Pagination(r)
	items, total, err := h.svc.Activity(r.Context(), id, page, perPage)
	if errors.Is(err, ErrNotFound) {
		httpx.Error(w, r, httpx.ErrNotFound)
		return
	}
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	dtos := make([]activityDTO, len(items))
	for i := range items {
		dtos[i] = toActivityDTO(items[i])
	}
	httpx.List(w, r, dtos, httpx.Meta{Page: page, PerPage: perPage, Total: total})
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id", chi.URLParam)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	u, err := h.svc.Get(r.Context(), id)
	if errors.Is(err, ErrNotFound) {
		httpx.Error(w, r, httpx.ErrNotFound)
		return
	}
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, toDTO(u))
}

type actionRequest struct {
	Reason string `json:"reason" validate:"required,max=1000"`
}

func (h *Handler) suspend(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id", chi.URLParam)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var body actionRequest
	if err := httpx.DecodeJSON(w, r, &body); err != nil {
		httpx.Error(w, r, err)
		return
	}
	actor, _ := adminusers.FromContext(r.Context())

	if err := h.svc.Suspend(r.Context(), id, actor.ID, body.Reason); err != nil {
		if errors.Is(err, ErrNotFound) {
			httpx.Error(w, r, httpx.ErrNotFound)
			return
		}
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusAccepted, httpx.Envelope{Data: map[string]string{
		"status": "suspend_requested",
		"detail": "user-service will apply this and publish user.suspended; the user list reflects it once that event is consumed",
	}})
}

func (h *Handler) reinstate(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id", chi.URLParam)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var body actionRequest
	if err := httpx.DecodeJSON(w, r, &body); err != nil {
		httpx.Error(w, r, err)
		return
	}
	actor, _ := adminusers.FromContext(r.Context())

	if err := h.svc.Reinstate(r.Context(), id, actor.ID, body.Reason); err != nil {
		if errors.Is(err, ErrNotFound) {
			httpx.Error(w, r, httpx.ErrNotFound)
			return
		}
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusAccepted, httpx.Envelope{Data: map[string]string{
		"status": "reinstate_requested",
		"detail": "user-service will apply this and publish user.reinstated; the user list reflects it once that event is consumed",
	}})
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
