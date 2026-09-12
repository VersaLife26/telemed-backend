package notifications

import (
	"context"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/rs/zerolog"

	"telemed/internal/platform/events"
	"telemed/internal/platform/httpx"
)

const KindDoctorApplication = "doctor_application"
const KindRescheduleRequest = "reschedule_request"

// Service owns admin inbox rows and projects application-submitted events into them.
type Service struct {
	repo *Repository
	log  zerolog.Logger
}

func NewService(repo *Repository, log zerolog.Logger) *Service {
	return &Service{repo: repo, log: log.With().Str("component", "admin_notifications").Logger()}
}

func (s *Service) ListUnread(ctx context.Context, limit int) ([]Notification, error) {
	return s.repo.ListUnread(ctx, limit)
}

func (s *Service) CountUnread(ctx context.Context) (int64, error) {
	return s.repo.CountUnread(ctx)
}

func (s *Service) MarkRead(ctx context.Context, id uuid.UUID) error {
	return s.repo.MarkRead(ctx, id)
}

func (s *Service) MarkAllRead(ctx context.Context) error {
	return s.repo.MarkAllRead(ctx)
}

// Subscribe listens for new doctor applications and creates inbox rows.
func (s *Service) Subscribe(ctx context.Context, sub events.Subscriber) error {
	if err := sub.Subscribe(ctx, "admin-notifications-doctor-application",
		[]events.Subject{events.SubjectDoctorApplicationSubmitted},
		s.handle); err != nil {
		return err
	}
	return sub.Subscribe(ctx, "admin-notifications-reschedule",
		[]events.Subject{events.SubjectAppointmentRescheduleRequested},
		s.handleReschedule)
}

func (s *Service) handle(ctx context.Context, env events.Envelope) error {
	if env.Subject != events.SubjectDoctorApplicationSubmitted {
		return nil
	}
	var p events.DoctorApplicationSubmitted
	if err := env.Decode(&p); err != nil {
		return fmt.Errorf("notifications: decode application_submitted: %w", err)
	}
	id := p.ApplicationID
	return s.repo.Insert(ctx, Notification{
		Kind:       KindDoctorApplication,
		Title:      "New doctor application",
		Body:       fmt.Sprintf("%s (%s) applied — SLMC %s, %s", p.FullName, p.Email, p.SLMCNumber, p.Specialty),
		Href:       "/doctors/" + p.ApplicationID.String(),
		ResourceID: &id,
	})
}

func (s *Service) handleReschedule(ctx context.Context, env events.Envelope) error {
	if env.Subject != events.SubjectAppointmentRescheduleRequested {
		return nil
	}
	var p events.AppointmentRescheduleRequested
	if err := env.Decode(&p); err != nil {
		return fmt.Errorf("notifications: decode reschedule_requested: %w", err)
	}
	id := p.RequestID
	return s.repo.Insert(ctx, Notification{
		Kind:       KindRescheduleRequest,
		Title:      "Reschedule requested",
		Body:       "A doctor asked to move a confirmed visit. Accept only after the patient agrees, or decline for a full refund.",
		Href:       "/appointments?tab=reschedule",
		ResourceID: &id,
	})
}

// Handler exposes GET /notifications, POST /notifications/{id}/read, POST /notifications/read-all.
type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

func (h *Handler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Get("/", h.list)
	r.Get("/unread-count", h.count)
	r.Post("/read-all", h.readAll)
	r.Post("/{id}/read", h.readOne)
	return r
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	items, err := h.svc.ListUnread(r.Context(), 20)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	out := make([]map[string]any, len(items))
	for i, n := range items {
		out[i] = map[string]any{
			"id": n.ID.String(), "kind": n.Kind, "title": n.Title, "body": n.Body,
			"href": n.Href, "created_at": n.CreatedAt,
		}
		if n.ResourceID != nil {
			out[i]["resource_id"] = n.ResourceID.String()
		}
	}
	httpx.OK(w, r, map[string]any{"items": out})
}

func (h *Handler) count(w http.ResponseWriter, r *http.Request) {
	n, err := h.svc.CountUnread(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, map[string]any{"count": n})
}

func (h *Handler) readAll(w http.ResponseWriter, r *http.Request) {
	if err := h.svc.MarkAllRead(r.Context()); err != nil {
		httpx.Error(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) readOne(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id", chi.URLParam)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if err := h.svc.MarkRead(r.Context(), id); err != nil {
		httpx.Error(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
