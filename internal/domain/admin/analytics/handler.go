package analytics

import (
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"telemed/internal/platform/httpx"
)

type Handler struct{ svc *Service }

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// Routes mounts every read under whatever prefix main.go mounts this at
// (/api/v1/admin/analytics). Every one reads a materialized view, never a
// raw projection table, per the package doc.
func (h *Handler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Get("/dashboard", h.dashboard)
	r.Get("/revenue", h.revenue)
	r.Get("/bookings", h.bookings)
	r.Get("/doctors", h.doctors)
	r.Get("/utilization", h.utilization)
	r.Get("/districts", h.districts)
	return r
}

func dateRange(r *http.Request) (DateRange, error) {
	var dr DateRange
	if raw := r.URL.Query().Get("from"); raw != "" {
		t, err := parseDay(raw)
		if err != nil {
			return DateRange{}, httpx.NewError(http.StatusBadRequest, httpx.CodeBadRequest, "from must be YYYY-MM-DD or RFC3339")
		}
		dr.From = t
	}
	if raw := r.URL.Query().Get("to"); raw != "" {
		t, err := parseDay(raw)
		if err != nil {
			return DateRange{}, httpx.NewError(http.StatusBadRequest, httpx.CodeBadRequest, "to must be YYYY-MM-DD or RFC3339")
		}
		dr.To = t
	}
	return dr, nil
}

func parseDay(raw string) (time.Time, error) {
	if t, err := time.Parse("2006-01-02", raw); err == nil {
		return t, nil
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t, nil
	}
	return time.Parse(time.RFC3339Nano, raw)
}

func (h *Handler) dashboard(w http.ResponseWriter, r *http.Request) {
	dr, err := dateRange(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	summary, err := h.svc.Dashboard(r.Context(), dr)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, summary)
}

func (h *Handler) revenue(w http.ResponseWriter, r *http.Request) {
	dr, err := dateRange(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	rows, err := h.svc.Revenue(r.Context(), dr)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, rows)
}

func (h *Handler) bookings(w http.ResponseWriter, r *http.Request) {
	dr, err := dateRange(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	rows, err := h.svc.Bookings(r.Context(), dr)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, rows)
}

func (h *Handler) doctors(w http.ResponseWriter, r *http.Request) {
	dr, err := dateRange(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	limit := 20
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			limit = n
		}
	}
	rows, err := h.svc.TopDoctors(r.Context(), dr, limit)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, rows)
}

func (h *Handler) utilization(w http.ResponseWriter, r *http.Request) {
	dr, err := dateRange(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	doctorID, _, err := httpx.QueryUUID(r, "doctor_id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	rows, err := h.svc.Utilization(r.Context(), doctorID, dr)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, rows)
}

func (h *Handler) districts(w http.ResponseWriter, r *http.Request) {
	dr, err := dateRange(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	rows, err := h.svc.Districts(r.Context(), dr)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, rows)
}
