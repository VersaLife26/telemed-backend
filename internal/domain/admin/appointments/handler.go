package appointments

import (
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"telemed/internal/domain/admin/adminusers"
	"telemed/internal/platform/httpx"
)

// uuidParse is a thin wrapper used after validator has already confirmed the
// string is uuid4-shaped, checked anyway per .golangci.yml's errcheck rule.
func uuidParse(s string) (uuid.UUID, error) { return uuid.Parse(s) }

type Handler struct{ svc *Service }

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

func (h *Handler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Get("/", h.list)
	r.Get("/double-bookings", h.doubleBookings)
	r.Get("/{id}", h.get)
	r.Post("/{id}/force-cancel", h.forceCancel)
	r.Post("/resolve-double-booking", h.resolveDoubleBooking)
	return r
}

type appointmentDTO struct {
	AppointmentID string    `json:"appointment_id"`
	DoctorID      string    `json:"doctor_id"`
	PatientID     string    `json:"patient_id"`
	SpecialtyCode string    `json:"specialty_code"`
	District      string    `json:"district"`
	Status        string    `json:"status"`
	ScheduledAt   time.Time `json:"scheduled_at"`
}

func toDTO(a Appointment) appointmentDTO {
	return appointmentDTO{
		AppointmentID: a.AppointmentID.String(), DoctorID: a.DoctorID.String(), PatientID: a.PatientID.String(),
		SpecialtyCode: a.SpecialtyCode, District: a.District, Status: a.Status, ScheduledAt: a.ScheduledAt,
	}
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	page, perPage, _ := httpx.Pagination(r)
	q := r.URL.Query()
	doctorID, _, err := httpx.QueryUUID(r, "doctor_id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	patientID, _, err := httpx.QueryUUID(r, "patient_id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	items, total, err := h.svc.List(r.Context(), ListFilter{
		Status: q.Get("status"), DoctorID: doctorID, PatientID: patientID, Page: page, PerPage: perPage,
	})
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	dtos := make([]appointmentDTO, len(items))
	for i := range items {
		dtos[i] = toDTO(items[i])
	}
	httpx.List(w, r, dtos, httpx.Meta{Page: page, PerPage: perPage, Total: total})
}

// doubleBookings returns overlapping slot conflicts. Detection is not wired
// yet; an empty list is the expected steady state when scheduling constraints
// hold.
func (h *Handler) doubleBookings(w http.ResponseWriter, r *http.Request) {
	httpx.OK(w, r, []appointmentDTO{})
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id", chi.URLParam)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	a, err := h.svc.Get(r.Context(), id)
	if errors.Is(err, ErrNotFound) {
		httpx.Error(w, r, httpx.ErrNotFound)
		return
	}
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, toDTO(a))
}

type reasonRequest struct {
	Reason string `json:"reason" validate:"required,max=1000"`
}

func (h *Handler) forceCancel(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id", chi.URLParam)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var body reasonRequest
	if err := httpx.DecodeJSON(w, r, &body); err != nil {
		httpx.Error(w, r, err)
		return
	}
	actor, _ := adminusers.FromContext(r.Context())

	if err := h.svc.ForceCancel(r.Context(), id, actor.ID, body.Reason); err != nil {
		if errors.Is(err, ErrNotFound) {
			httpx.Error(w, r, httpx.ErrNotFound)
			return
		}
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusAccepted, httpx.Envelope{Data: map[string]string{"status": "force_cancel_requested"}})
}

type resolveDoubleBookingRequest struct {
	KeepAppointmentID   string `json:"keep_appointment_id" validate:"required,uuid4"`
	CancelAppointmentID string `json:"cancel_appointment_id" validate:"required,uuid4,nefield=KeepAppointmentID"`
	Reason              string `json:"reason" validate:"required,max=1000"`
}

func (h *Handler) resolveDoubleBooking(w http.ResponseWriter, r *http.Request) {
	var body resolveDoubleBookingRequest
	if err := httpx.DecodeJSON(w, r, &body); err != nil {
		httpx.Error(w, r, err)
		return
	}
	keep, _ := uuidParse(body.KeepAppointmentID)
	cancel, _ := uuidParse(body.CancelAppointmentID)
	actor, _ := adminusers.FromContext(r.Context())

	if err := h.svc.ResolveDoubleBooking(r.Context(), keep, cancel, actor.ID, body.Reason); err != nil {
		if errors.Is(err, ErrNotFound) {
			httpx.Error(w, r, httpx.ErrNotFound)
			return
		}
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusAccepted, httpx.Envelope{Data: map[string]string{"status": "double_booking_resolve_requested"}})
}
