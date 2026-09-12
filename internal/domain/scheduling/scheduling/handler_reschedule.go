package scheduling

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"telemed/internal/platform/httpx"
	"telemed/internal/platform/middleware"
)

type createRescheduleBody struct {
	ProposedStartAt time.Time `json:"proposed_start_at" validate:"required"`
	Reason          string    `json:"reason" validate:"max=500"`
}

func (h *Handler) createRescheduleRequest(w http.ResponseWriter, r *http.Request) {
	p := middleware.MustPrincipal(r.Context())
	actorID, role, err := h.actor(p)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if role != ActorDoctor {
		httpx.Error(w, r, httpx.NewError(http.StatusForbidden, httpx.CodeForbidden,
			"only the treating doctor may propose a reschedule"))
		return
	}
	id, err := httpx.PathUUID(r, "appointmentID", chi.URLParam)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var req createRescheduleBody
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	if req.ProposedStartAt.IsZero() {
		httpx.Error(w, r, httpx.NewError(http.StatusUnprocessableEntity, httpx.CodeValidation,
			"proposed_start_at is required and must be RFC3339"))
		return
	}

	out, err := h.svc.CreateRescheduleRequest(r.Context(), CreateRescheduleInput{
		AppointmentID:   id,
		ActorID:         actorID,
		ActorRole:       role,
		ProposedStartAt: req.ProposedStartAt,
		Reason:          req.Reason,
	})
	if err != nil {
		httpx.Error(w, r, APIError(err))
		return
	}
	httpx.Created(w, r, NewRescheduleRequestDTO(out, h.svc.Location()))
}

func (h *Handler) listRescheduleRequests(w http.ResponseWriter, r *http.Request) {
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
	items, err := h.svc.ListRescheduleRequestsForAppointment(r.Context(), id, actorID, role)
	if err != nil {
		httpx.Error(w, r, APIError(err))
		return
	}
	out := make([]RescheduleRequestDTO, len(items))
	for i := range items {
		out[i] = NewRescheduleRequestDTO(items[i], h.svc.Location())
	}
	httpx.OK(w, r, out)
}

func (h *Handler) acceptRescheduleRequest(w http.ResponseWriter, r *http.Request) {
	h.decideReschedule(w, r, false)
}

func (h *Handler) declineRescheduleRequest(w http.ResponseWriter, r *http.Request) {
	h.decideReschedule(w, r, true)
}

func (h *Handler) adminAcceptRescheduleRequest(w http.ResponseWriter, r *http.Request) {
	h.adminDecideReschedule(w, r, false)
}

func (h *Handler) adminDeclineRescheduleRequest(w http.ResponseWriter, r *http.Request) {
	h.adminDecideReschedule(w, r, true)
}

func (h *Handler) decideReschedule(w http.ResponseWriter, r *http.Request, decline bool) {
	p := middleware.MustPrincipal(r.Context())
	actorID, role, err := h.actor(p)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	id, err := httpx.PathUUID(r, "requestID", chi.URLParam)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	h.respondRescheduleDecision(w, r, DecideRescheduleInput{
		RequestID: id,
		ActorID:   actorID,
		ActorRole: role,
	}, decline)
}

func (h *Handler) adminDecideReschedule(w http.ResponseWriter, r *http.Request, decline bool) {
	p := middleware.MustPrincipal(r.Context())
	id, err := httpx.PathUUID(r, "requestID", chi.URLParam)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	h.respondRescheduleDecision(w, r, DecideRescheduleInput{
		RequestID: id,
		ActorID:   p.UserID,
		ActorRole: ActorAdmin,
	}, decline)
}

func (h *Handler) respondRescheduleDecision(w http.ResponseWriter, r *http.Request, in DecideRescheduleInput, decline bool) {
	var (
		req  RescheduleRequest
		appt Appointment
		err  error
	)
	if decline {
		req, appt, err = h.svc.DeclineRescheduleRequest(r.Context(), in)
	} else {
		req, appt, err = h.svc.AcceptRescheduleRequest(r.Context(), in)
	}
	if err != nil {
		httpx.Error(w, r, APIError(err))
		return
	}
	httpx.OK(w, r, map[string]any{
		"request":     NewRescheduleRequestDTO(req, h.svc.Location()),
		"appointment": NewAppointmentDTO(appt, h.svc.Location(), in.ActorRole != ActorAdmin),
	})
}

func (h *Handler) listPendingRescheduleRequests(w http.ResponseWriter, r *http.Request) {
	page, perPage, offset := httpx.Pagination(r)
	items, total, err := h.svc.ListPendingRescheduleRequests(r.Context(), perPage, offset)
	if err != nil {
		httpx.Error(w, r, APIError(err))
		return
	}
	out := make([]RescheduleRequestDTO, len(items))
	for i := range items {
		out[i] = NewRescheduleRequestDTO(items[i], h.svc.Location())
	}
	httpx.List(w, r, out, httpx.Meta{Page: page, PerPage: perPage, Total: total})
}
