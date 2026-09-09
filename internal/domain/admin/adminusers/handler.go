package adminusers

import (
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"telemed/internal/domain/admin/rbac"
	"telemed/internal/platform/httpx"
)

type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// Routes mounts the admin-account surface. Every route here requires
// GroupAdminUsers (super_admin only, see internal/rbac): managing who else is
// an admin is the one action that can grant access rather than use it.
func (h *Handler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Get("/", h.list)
	r.Get("/permissions", h.permissions)
	r.Post("/", h.create)
	r.Put("/{id}", h.update)
	return r
}

// Me returns the caller's own admin_users row. Every authenticated admin role
// may read it; it carries no more privilege than the JWT already does.
func (h *Handler) Me(w http.ResponseWriter, r *http.Request) {
	au, ok := FromContext(r.Context())
	if !ok {
		httpx.Error(w, r, httpx.ErrUnauthorized)
		return
	}
	httpx.OK(w, r, toDTO(au))
}

// permissions returns the live RBAC matrix: which roles reach which group,
// with a sentence on what each group means.
//
// It exists so the console shows what this service ACTUALLY enforces. The
// console has its own synchronous copy of the table for edge-time route
// guarding, and two hand-maintained copies of an authorization matrix drift --
// at which point a super_admin is shown a permission the server does not
// grant, or not shown one it does, while deciding what a colleague may reach.
// A confidently wrong permission screen is worse than no permission screen.
func (h *Handler) permissions(w http.ResponseWriter, r *http.Request) {
	httpx.OK(w, r, map[string]any{
		"groups": rbac.Describe(),
		"roles":  rbac.AssignableRoles(),
	})
}

type adminUserDTO struct {
	ID          string     `json:"id"`
	Email       string     `json:"email"`
	DisplayName string     `json:"display_name"`
	Role        string     `json:"role"`
	IPAllowlist []string   `json:"ip_allowlist"`
	Active      bool       `json:"active"`
	LastLoginAt *time.Time `json:"last_login_at,omitempty"`
	Version     int        `json:"version"`
}

func toDTO(u AdminUser) adminUserDTO {
	return adminUserDTO{
		ID: u.ID.String(), Email: u.Email, DisplayName: u.DisplayName, Role: u.Role,
		IPAllowlist: u.IPAllowlist, Active: u.Active, LastLoginAt: u.LastLoginAt, Version: u.Version,
	}
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	users, err := h.svc.List(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	dtos := make([]adminUserDTO, len(users))
	for i := range users {
		dtos[i] = toDTO(users[i])
	}
	httpx.OK(w, r, dtos)
}

type createRequest struct {
	Email       string   `json:"email" validate:"required,email,max=254"`
	DisplayName string   `json:"display_name" validate:"required,min=1,max=120"`
	Role        string   `json:"role" validate:"required,oneof=admin super_admin ops finance support"`
	IPAllowlist []string `json:"ip_allowlist" validate:"omitempty,dive,cidr"`
}

// create provisions a colleague's admin account: the Keycloak login and the
// local row, in that order.
//
// No password appears anywhere in this request or its response. Keycloak is
// given UPDATE_PASSWORD, CONFIGURE_TOTP and VERIFY_EMAIL as required actions,
// so the new admin sets their own credential and enrols their second factor on
// first login. That means this platform never generates, transmits, stores or
// logs an admin password, and there is no temporary one sitting in an inbox.
//
// ip_allowlist is validated as CIDRs here rather than at use: a malformed
// entry is skipped by ipInAllowlist rather than treated as a wildcard, so a
// typo would silently narrow nothing and the admin would appear unrestricted.
// Rejecting it at the door is the only place it is still cheap to fix.
func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	var body createRequest
	if err := httpx.DecodeJSON(w, r, &body); err != nil {
		httpx.Error(w, r, err)
		return
	}

	created, err := h.svc.Create(r.Context(), CreateParams{
		Email:       body.Email,
		DisplayName: body.DisplayName,
		Role:        body.Role,
		IPAllowlist: body.IPAllowlist,
	})
	if err != nil {
		if errors.Is(err, ErrIdentityProviderUnavailable) {
			// 503, not 500: nothing is wrong with the request, and the
			// operator can retry once Keycloak is reachable.
			httpx.Error(w, r, httpx.ErrUnavailable.WithCause(err))
			return
		}
		httpx.Error(w, r, err)
		return
	}
	httpx.Created(w, r, toDTO(created))
}

type updateRequest struct {
	Active      *bool     `json:"active"`
	Role        *string   `json:"role" validate:"omitempty,oneof=admin super_admin ops finance support"`
	IPAllowlist *[]string `json:"ip_allowlist"`
	Version     int       `json:"version" validate:"required,min=1"`
}

func (h *Handler) update(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id", chi.URLParam)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var body updateRequest
	if err := httpx.DecodeJSON(w, r, &body); err != nil {
		httpx.Error(w, r, err)
		return
	}

	// A direct conversion, not a field-by-field literal: the two structs are
	// identical by construction, and the conversion stops compiling the moment
	// they are not -- which forces whoever adds a field to UpdateParams to
	// decide explicitly whether a client may set it.
	updated, err := h.svc.Update(r.Context(), id, UpdateParams(body))
	switch {
	case errors.Is(err, ErrVersionConflict):
		httpx.Error(w, r, httpx.NewError(http.StatusConflict, httpx.CodeConflict,
			"admin account was modified by someone else; re-fetch and retry"))
		return
	case errors.Is(err, ErrNotFound):
		httpx.Error(w, r, httpx.ErrNotFound)
		return
	case errors.Is(err, ErrIdentityProviderUnavailable):
		// A role or active change could not reach Keycloak, so it was not
		// applied anywhere -- the service writes the realm before the row
		// precisely so this is a clean refusal rather than a database that
		// disagrees with what actually authorizes the account. 503, because
		// the request was fine and retrying later is the correct response.
		httpx.Error(w, r, httpx.ErrUnavailable.WithCause(err))
		return
	case err != nil:
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, toDTO(updated))
}
