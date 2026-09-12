package consultation

import (
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/rs/zerolog"

	"telemed/internal/platform/httpx"
	"telemed/internal/platform/middleware"
)

// maxWebhookBodyBytes caps the LiveKit webhook body. Real payloads are a few
// KB of JSON; this is generous headroom, not an invitation to stream a
// recording through the webhook endpoint.
const maxWebhookBodyBytes = 64 * 1024

// Handler is the HTTP surface for the consultation domain. It never runs SQL
// and never touches VideoProvider directly -- everything goes through Service.
type Handler struct {
	service *Service
	log     zerolog.Logger
}

// NewHandler builds Handler.
func NewHandler(s *Service, log zerolog.Logger) *Handler {
	return &Handler{service: s, log: log}
}

// Routes returns the authenticated /api/v1/consultations surface.
func (h *Handler) Routes(auth *middleware.Authenticator) chi.Router {
	r := chi.NewRouter()
	r.Use(middleware.RequireAuth(auth))
	r.Use(middleware.NoStore)

	r.Post("/ready-for-next", h.readyForNextLatest)
	r.Post("/{appointment_id}/join", h.join)
	r.Post("/{appointment_id}/ready-for-next", h.readyForNext)
	r.Get("/{appointment_id}/early-join", h.getEarlyJoin)
	r.Post("/{appointment_id}/early-join/accept", h.acceptEarlyJoin)
	r.Post("/{appointment_id}/early-join/decline", h.declineEarlyJoin)
	r.Post("/{id}/admit", h.admit)
	r.Post("/{id}/end", h.end)
	r.Post("/{id}/consent", h.consent)
	r.Get("/{id}", h.get)
	r.Get("/{id}/waiting-room", h.waitingRoom)
	r.Post("/{id}/quality", h.quality)
	return r
}

// WebhookRoutes returns the LiveKit webhook route, meant to be mounted at
// /webhooks -- separately from Routes, and deliberately outside
// middleware.RequireAuth. LiveKit authenticates the request itself via a
// JWT-signed Authorization header that VideoProvider.VerifyWebhook checks;
// a platform user JWT was never going to be present on a server-to-server
// call. rl is a rate-limit middleware the caller wires up (the body-size cap
// below handles the other half of "rate-limited and size-capped").
func (h *Handler) WebhookRoutes(rl func(http.Handler) http.Handler) chi.Router {
	r := chi.NewRouter()
	r.With(rl).Post("/livekit", h.webhook)
	return r
}

func (h *Handler) join(w http.ResponseWriter, r *http.Request) {
	p := middleware.MustPrincipal(r.Context())
	appointmentID, err := httpx.PathUUID(r, "appointment_id", chi.URLParam)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	result, err := h.service.Join(r.Context(), p, appointmentID)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	httpx.OK(w, r, result)
}

type emptyJSONRequest struct{}

func (h *Handler) readyForNextLatest(w http.ResponseWriter, r *http.Request) {
	h.handleReadyForNext(w, r, uuid.Nil)
}

func (h *Handler) readyForNext(w http.ResponseWriter, r *http.Request) {
	appointmentID, err := httpx.PathUUID(r, "appointment_id", chi.URLParam)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	h.handleReadyForNext(w, r, appointmentID)
}

func (h *Handler) handleReadyForNext(w http.ResponseWriter, r *http.Request, appointmentID uuid.UUID) {
	p := middleware.MustPrincipal(r.Context())
	var body emptyJSONRequest
	if err := httpx.DecodeJSON(w, r, &body); err != nil {
		httpx.Error(w, r, err)
		return
	}

	result, err := h.service.ReadyForNext(r.Context(), p, appointmentID)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	httpx.OK(w, r, result)
}

func (h *Handler) getEarlyJoin(w http.ResponseWriter, r *http.Request) {
	p := middleware.MustPrincipal(r.Context())
	appointmentID, err := httpx.PathUUID(r, "appointment_id", chi.URLParam)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	result, err := h.service.GetEarlyJoin(r.Context(), p, appointmentID)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	httpx.OK(w, r, result)
}

func (h *Handler) acceptEarlyJoin(w http.ResponseWriter, r *http.Request) {
	h.respondEarlyJoin(w, r, true)
}

func (h *Handler) declineEarlyJoin(w http.ResponseWriter, r *http.Request) {
	h.respondEarlyJoin(w, r, false)
}

func (h *Handler) respondEarlyJoin(w http.ResponseWriter, r *http.Request, accept bool) {
	p := middleware.MustPrincipal(r.Context())
	appointmentID, err := httpx.PathUUID(r, "appointment_id", chi.URLParam)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var body emptyJSONRequest
	if err := httpx.DecodeJSON(w, r, &body); err != nil {
		httpx.Error(w, r, err)
		return
	}

	result, err := h.service.RespondEarlyJoin(r.Context(), p, appointmentID, accept)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	httpx.OK(w, r, result)
}

// admitRequest is intentionally empty: the brief for this endpoint carries no
// fields, only the path id. A JSON object is still required so the handler
// stays consistent with every other POST in this service using
// httpx.DecodeJSON -- clients send "{}".
type admitRequest struct{}

func (h *Handler) admit(w http.ResponseWriter, r *http.Request) {
	p := middleware.MustPrincipal(r.Context())
	id, err := httpx.PathUUID(r, "id", chi.URLParam)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var body admitRequest
	if err := httpx.DecodeJSON(w, r, &body); err != nil {
		httpx.Error(w, r, err)
		return
	}

	c, err := h.service.Admit(r.Context(), p, id)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	httpx.OK(w, r, toConsultationDTO(c, nil))
}

type endRequest struct {
	Reason string `json:"reason" validate:"omitempty,max=200"`
}

func (h *Handler) end(w http.ResponseWriter, r *http.Request) {
	p := middleware.MustPrincipal(r.Context())
	id, err := httpx.PathUUID(r, "id", chi.URLParam)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var body endRequest
	if err := httpx.DecodeJSON(w, r, &body); err != nil {
		httpx.Error(w, r, err)
		return
	}

	c, err := h.service.End(r.Context(), p, id, body.Reason)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	httpx.OK(w, r, toConsultationDTO(c, nil))
}

type consentRequest struct {
	ConsentType string `json:"consent_type" validate:"required,oneof=recording telemedicine"`
	Granted     bool   `json:"granted"`
}

func (h *Handler) consent(w http.ResponseWriter, r *http.Request) {
	p := middleware.MustPrincipal(r.Context())
	id, err := httpx.PathUUID(r, "id", chi.URLParam)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var body consentRequest
	if err := httpx.DecodeJSON(w, r, &body); err != nil {
		httpx.Error(w, r, err)
		return
	}

	c, err := h.service.SubmitConsent(r.Context(), p, id, ConsentInput{
		Type:      ConsentType(body.ConsentType),
		Granted:   body.Granted,
		IPAddress: middleware.ClientIP(r),
		UserAgent: r.UserAgent(),
	})
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	httpx.OK(w, r, toConsultationDTO(c, nil))
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	p := middleware.MustPrincipal(r.Context())
	id, err := httpx.PathUUID(r, "id", chi.URLParam)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	c, participants, err := h.service.Get(r.Context(), p, id)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	httpx.OK(w, r, toConsultationDTO(c, participants))
}

func (h *Handler) waitingRoom(w http.ResponseWriter, r *http.Request) {
	p := middleware.MustPrincipal(r.Context())
	id, err := httpx.PathUUID(r, "id", chi.URLParam)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	status, err := h.service.WaitingRoomStatus(r.Context(), p, id)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	httpx.OK(w, r, status)
}

type qualityRequest struct {
	Quality       string   `json:"quality" validate:"required,oneof=excellent good poor lost"`
	PacketLossPct *float64 `json:"packet_loss_pct" validate:"omitempty,gte=0,lte=100"`
	BitrateKbps   *int     `json:"bitrate_kbps" validate:"omitempty,gte=0"`
}

func (h *Handler) quality(w http.ResponseWriter, r *http.Request) {
	p := middleware.MustPrincipal(r.Context())
	id, err := httpx.PathUUID(r, "id", chi.URLParam)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var body qualityRequest
	if err := httpx.DecodeJSON(w, r, &body); err != nil {
		httpx.Error(w, r, err)
		return
	}

	result, err := h.service.ReportQuality(r.Context(), p, id, QualityInput{
		Quality:       Quality(body.Quality),
		PacketLossPct: body.PacketLossPct,
		BitrateKbps:   body.BitrateKbps,
	})
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	httpx.OK(w, r, result)
}

func (h *Handler) webhook(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxWebhookBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		httpx.Error(w, r, httpx.NewError(http.StatusRequestEntityTooLarge, httpx.CodeBadRequest, "webhook body too large or unreadable").WithCause(err))
		return
	}

	if err := h.service.HandleWebhook(r.Context(), r.Header.Get("Authorization"), body); err != nil {
		h.writeError(w, r, err)
		return
	}
	httpx.NoContent(w, r)
}

func (h *Handler) writeError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		httpx.Error(w, r, httpx.ErrNotFound.WithCause(err))
	case errors.Is(err, ErrForbidden):
		httpx.Error(w, r, httpx.ErrForbidden.WithCause(err))
	case errors.Is(err, ErrInvalidState):
		httpx.Error(w, r, httpx.NewError(http.StatusConflict, httpx.CodeConflict, "consultation is not in a state that allows this action").WithCause(err))
	case errors.Is(err, ErrOptimisticLock):
		httpx.Error(w, r, httpx.ErrConflict.WithCause(err))
	case errors.Is(err, ErrWebhookUnverified):
		httpx.Error(w, r, httpx.NewError(http.StatusUnauthorized, httpx.CodeUnauthorized, "webhook signature could not be verified").WithCause(err))
	case errors.Is(err, ErrRoomNotFound):
		httpx.Error(w, r, httpx.ErrNotFound.WithCause(err))
	default:
		h.log.Error().Err(err).Msg("consultation handler error")
		httpx.Error(w, r, httpx.ErrInternal.WithCause(err))
	}
}

// --- response DTOs -----------------------------------------------------

type consultationDTO struct {
	ID              string           `json:"id"`
	AppointmentID   string           `json:"appointment_id"`
	PatientID       string           `json:"patient_id"`
	DoctorID        string           `json:"doctor_id"`
	RoomName        string           `json:"room_name"`
	Status          string           `json:"status"`
	ScheduledAt     time.Time        `json:"scheduled_at"`
	StartedAt       *time.Time       `json:"started_at,omitempty"`
	EndedAt         *time.Time       `json:"ended_at,omitempty"`
	DurationSeconds *int             `json:"duration_seconds,omitempty"`
	RecordingURL    *string          `json:"recording_url,omitempty"`
	RecordingStatus string           `json:"recording_status"`
	EndReason       *string          `json:"end_reason,omitempty"`
	Participants    []participantDTO `json:"participants,omitempty"`
}

type participantDTO struct {
	Identity       string     `json:"identity"`
	Role           string     `json:"role"`
	JoinedAt       *time.Time `json:"joined_at,omitempty"`
	LeftAt         *time.Time `json:"left_at,omitempty"`
	ReconnectCount int        `json:"reconnect_count"`
}

func toConsultationDTO(c *Consultation, participants []Participant) consultationDTO {
	dto := consultationDTO{
		ID: c.ID.String(), AppointmentID: c.AppointmentID.String(),
		PatientID: c.PatientID.String(), DoctorID: c.DoctorID.String(),
		RoomName: c.RoomName, Status: string(c.Status), ScheduledAt: c.ScheduledAt,
		StartedAt: c.StartedAt, EndedAt: c.EndedAt, DurationSeconds: c.DurationSeconds,
		RecordingURL: c.RecordingURL, RecordingStatus: string(c.RecordingStatus), EndReason: c.EndReason,
	}
	for _, p := range participants {
		dto.Participants = append(dto.Participants, participantDTO{
			Identity: p.Identity, Role: string(p.Role),
			JoinedAt: p.JoinedAt, LeftAt: p.LeftAt, ReconnectCount: p.ReconnectCount,
		})
	}
	return dto
}
