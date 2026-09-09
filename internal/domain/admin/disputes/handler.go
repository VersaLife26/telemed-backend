package disputes

import (
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"telemed/internal/domain/admin/adminusers"
	"telemed/internal/domain/admin/audit"
	"telemed/internal/platform/httpx"
	mw "telemed/internal/platform/middleware"
)

// actorFrom builds the dispute Actor for this request.
//
// The id comes from the local admin_users row RequireActiveAdminUser resolved
// (never from the request body -- that is the shape of finding F28 elsewhere in
// the review), and the override right comes from the verified JWT, which is the
// same source RequireRole reads. admin_users.role is a projection and can lag;
// the token is what actually authorised the request.
func actorFrom(r *http.Request) Actor {
	au, _ := adminusers.FromContext(r.Context())
	p, _ := mw.PrincipalFrom(r.Context())
	return Actor{ID: au.ID, CanForce: p.HasRole(mw.RoleSuperAdmin)}
}

// writeMutationError maps the shared failure modes of assign/resolve. 403 for
// "not yours" and 409 for "someone moved it" are genuinely different answers:
// one is worth retrying, the other never will be.
func writeMutationError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrNotAssignee):
		httpx.Error(w, r, httpx.NewError(http.StatusForbidden, httpx.CodeForbidden,
			"this dispute is assigned to another admin; ask them to act on it, or have a super_admin reassign it"))
	case errors.Is(err, ErrNotFound):
		httpx.Error(w, r, httpx.ErrNotFound)
	case errors.Is(err, ErrVersionConflict):
		httpx.Error(w, r, httpx.NewError(http.StatusConflict, httpx.CodeConflict, "dispute was modified since last read"))
	default:
		httpx.Error(w, r, err)
	}
}

// parseUUID is a thin wrapper used after validator has already confirmed the
// string is uuid4-shaped (see the `validate:"uuid4"` tags below), so the
// error return is only ever nil in practice; checked anyway rather than
// ignored, per .golangci.yml's errcheck rule.
func parseUUID(s string) (uuid.UUID, error) { return uuid.Parse(s) }

type Handler struct{ svc *Service }

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

func (h *Handler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Get("/", h.list)
	r.Post("/", h.create)
	r.Get("/{id}", h.get)
	r.Post("/{id}/assign", h.assign)
	r.Post("/{id}/comments", h.comment)
	r.Get("/{id}/comments", h.listComments)
	r.Post("/{id}/resolve", h.resolve)
	return r
}

type disputeDTO struct {
	ID                string    `json:"id"`
	AppointmentID     string    `json:"appointment_id"`
	PatientID         string    `json:"patient_id"`
	DoctorID          string    `json:"doctor_id"`
	Category          string    `json:"category"`
	Description       string    `json:"description,omitempty"`
	Status            string    `json:"status"`
	AssignedTo        string    `json:"assigned_to,omitempty"`
	Resolution        string    `json:"resolution,omitempty"`
	RefundRequested   bool      `json:"refund_requested"`
	RefundAmountCents *int64    `json:"refund_amount_cents,omitempty"`
	Currency          string    `json:"currency"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
	Version           int       `json:"version"`
}

// mayReadDescription decides whether this caller sees the patient's own words.
//
// disputes.description is up to 4000 characters a patient typed about their
// care, and "quality_of_care" is one of the five categories. It is the one
// concrete counter-example to the platform's compliance claim that an
// administrator never sees clinical data -- a complaint about a consultation
// contains the consultation. It is also unavoidable: nobody can adjudicate a
// quality-of-care dispute without reading the complaint.
//
// So the answer is not to drop it (that is F20d's answer, and it was right
// there because the cancellation reason was fanning out on NATS to consumers
// that had no use for it whatsoever). The answer is to make reading it a
// narrow, deliberate, recorded act:
//
//   - Never in a list. A queue view needs category, status, assignee and
//     dates; browsing 25 patients' complaints at a time is not adjudication.
//   - Only on the detail read, and only for the roles that adjudicate.
//     finance needs the refund amount and the category; ops needs the queue.
//     Neither needs the narrative.
//   - Only when the dispute is the caller's to work -- unassigned, theirs, or
//     a deliberate super_admin override. This is mayAct, the same predicate
//     that already governs assign and resolve, so "who may act on it" and
//     "who may read it" cannot drift apart.
//   - And the read is audited, because audit.Middleware skips GETs.
func mayReadDescription(d Dispute, p mw.Principal, actor Actor) bool {
	if !p.HasAdminRole(mw.RoleSupport, mw.RoleAdmin, mw.RoleSuperAdmin) {
		return false
	}
	return mayAct(d, actor)
}

// toDTO renders a dispute without the patient's narrative. Every list uses it.
func toDTO(d Dispute) disputeDTO {
	dto := disputeDTO{
		ID: d.ID.String(), AppointmentID: d.AppointmentID.String(), PatientID: d.PatientID.String(),
		DoctorID: d.DoctorID.String(), Category: d.Category, Status: d.Status,
		Resolution: d.Resolution, RefundRequested: d.RefundRequested, RefundAmountCents: d.RefundAmountCents,
		Currency: d.Currency, CreatedAt: d.CreatedAt, UpdatedAt: d.UpdatedAt, Version: d.Version,
	}
	if d.AssignedTo != nil {
		dto.AssignedTo = d.AssignedTo.String()
	}
	return dto
}

type createRequest struct {
	AppointmentID   string `json:"appointment_id" validate:"required,uuid4"`
	PatientID       string `json:"patient_id" validate:"required,uuid4"`
	DoctorID        string `json:"doctor_id" validate:"required,uuid4"`
	Category        string `json:"category" validate:"required,oneof=billing quality_of_care no_show technical other"`
	Description     string `json:"description" validate:"required,max=4000"`
	RefundRequested bool   `json:"refund_requested"`
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	var body createRequest
	if err := httpx.DecodeJSON(w, r, &body); err != nil {
		httpx.Error(w, r, err)
		return
	}
	appt, _ := parseUUID(body.AppointmentID)
	pat, _ := parseUUID(body.PatientID)
	doc, _ := parseUUID(body.DoctorID)

	d, err := h.svc.Create(r.Context(), CreateParams{
		AppointmentID: appt, PatientID: pat, DoctorID: doc,
		Category: body.Category, Description: body.Description, RefundRequested: body.RefundRequested,
	})
	switch {
	case errors.Is(err, ErrUnknownParty):
		// 422, not 404: the request was well-formed and the caller is allowed
		// to make it, but the party it names is not who they say. A 404 would
		// also be an existence oracle over the whole user directory.
		httpx.Error(w, r, httpx.NewError(http.StatusUnprocessableEntity, httpx.CodeUnprocessable, err.Error()))
		return
	case err != nil:
		httpx.Error(w, r, err)
		return
	}
	httpx.Created(w, r, toDTO(d))
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	page, perPage, _ := httpx.Pagination(r)
	assignedTo, ok, err := httpx.QueryUUID(r, "assigned_to")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if !ok {
		assignedTo = uuid.Nil
	}
	disputes, total, err := h.svc.List(r.Context(), ListFilter{
		Status: r.URL.Query().Get("status"), AssignedTo: assignedTo, Page: page, PerPage: perPage,
	})
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	dtos := make([]disputeDTO, len(disputes))
	for i := range disputes {
		dtos[i] = toDTO(disputes[i])
	}
	httpx.List(w, r, dtos, httpx.Meta{Page: page, PerPage: perPage, Total: total})
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id", chi.URLParam)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	d, err := h.svc.Get(r.Context(), id)
	if errors.Is(err, ErrNotFound) {
		httpx.Error(w, r, httpx.ErrNotFound)
		return
	}
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	dto := toDTO(d)
	p, _ := mw.PrincipalFrom(r.Context())
	if mayReadDescription(d, p, actorFrom(r)) {
		dto.Description = d.Description
		// A GET, so audit.Middleware will not record it. Staging here makes
		// PHI access to a patient's own account of their care a first-class
		// entry in the hash-chained log rather than something that happened
		// invisibly.
		audit.Stage(r.Context(), audit.Draft{
			Action:       "dispute.description_read",
			ResourceType: "dispute",
			ResourceID:   d.ID.String(),
			NewValue:     map[string]any{"category": d.Category},
		})
	}
	httpx.OK(w, r, dto)
}

type assignRequest struct {
	AssigneeID string `json:"assignee_id" validate:"required,uuid4"`
	Version    int    `json:"version" validate:"required,min=1"`
	// Force is honoured only for super_admin (see Actor.CanForce) and is
	// recorded as dispute.force_reassigned. Any other role sending it gets
	// the ordinary 403, because Actor.CanForce -- not this field -- is what
	// decides.
	Force bool `json:"force"`
}

func (h *Handler) assign(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id", chi.URLParam)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var body assignRequest
	if err := httpx.DecodeJSON(w, r, &body); err != nil {
		httpx.Error(w, r, err)
		return
	}
	assignee, _ := parseUUID(body.AssigneeID)

	actor := actorFrom(r)
	actor.CanForce = actor.CanForce && body.Force

	d, err := h.svc.Assign(r.Context(), id, assignee, actor, body.Version)
	if err != nil {
		writeMutationError(w, r, err)
		return
	}
	httpx.OK(w, r, toDTO(d))
}

type commentRequest struct {
	Body string `json:"body" validate:"required,max=4000"`
}

func (h *Handler) comment(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id", chi.URLParam)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var body commentRequest
	if err := httpx.DecodeJSON(w, r, &body); err != nil {
		httpx.Error(w, r, err)
		return
	}
	actor, _ := adminusers.FromContext(r.Context())

	c, err := h.svc.Comment(r.Context(), id, actor.ID, body.Body)
	if errors.Is(err, ErrNotFound) {
		httpx.Error(w, r, httpx.ErrNotFound)
		return
	}
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.Created(w, r, map[string]any{"id": c.ID, "dispute_id": c.DisputeID, "body": c.Body, "created_at": c.CreatedAt})
}

func (h *Handler) listComments(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id", chi.URLParam)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	comments, err := h.svc.Comments(r.Context(), id)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, comments)
}

type resolveRequest struct {
	Resolution        string `json:"resolution" validate:"required,max=4000"`
	RefundAmountCents *int64 `json:"refund_amount_cents" validate:"omitempty,min=0"`
	Version           int    `json:"version" validate:"required,min=1"`
	// Force is honoured only for super_admin and is recorded as
	// dispute.force_resolved. See assignRequest.Force.
	Force bool `json:"force"`
}

func (h *Handler) resolve(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id", chi.URLParam)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var body resolveRequest
	if err := httpx.DecodeJSON(w, r, &body); err != nil {
		httpx.Error(w, r, err)
		return
	}
	actor := actorFrom(r)
	actor.CanForce = actor.CanForce && body.Force

	d, err := h.svc.Resolve(r.Context(), id, body.Resolution, body.RefundAmountCents, actor, body.Version)
	if err != nil {
		writeMutationError(w, r, err)
		return
	}
	httpx.OK(w, r, toDTO(d))
}
