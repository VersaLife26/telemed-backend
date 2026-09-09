package analytics

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"telemed/internal/platform/httpx"
	"telemed/internal/platform/middleware"
)

// DoctorResolver turns the signed-in principal into the doctor id this
// service's tables are keyed on.
//
// It is an interface rather than a direct call into the doctor package so that
// analytics does not depend on the doctor domain -- the dependency runs one way,
// and a test can drive these handlers with a two-line fake instead of a
// registered doctor.
//
// It deliberately does NOT read the token's telemed_doctor_id claim. Every
// other /doctors/me endpoint in this service resolves the profile from the user
// id, and an endpoint that trusted a claim the others ignore would return a
// different doctor's numbers the first time a token was minted without it.
type DoctorResolver interface {
	ResolveDoctorID(ctx context.Context, userID uuid.UUID) (uuid.UUID, error)
}

// Handler is the HTTP surface for a doctor's own analytics. It parses, calls
// the service, and renders; it holds no SQL and no business rule.
type Handler struct {
	svc      *Service
	resolver DoctorResolver
}

// NewHandler builds the analytics handler.
func NewHandler(svc *Service, resolver DoctorResolver) *Handler {
	return &Handler{svc: svc, resolver: resolver}
}

// Register mounts the analytics endpoints onto an existing /doctors/me router.
//
// It registers onto the caller's router rather than returning its own: chi
// panics on two Mounts sharing a prefix, and /doctors is already mounted by the
// doctor handler. Registering into that subtree is how these endpoints sit at
// /api/v1/doctors/me/... without a second mount point.
func (h *Handler) Register(r chi.Router) {
	r.Get("/analytics", h.summary)
	r.Get("/analytics/peak-hours", h.peakHours)
	r.Get("/earnings", h.earnings)
}

// doctorID resolves the caller, mapping a missing profile onto 404 rather than
// leaking that the account exists but is not a doctor.
func (h *Handler) doctorID(r *http.Request) (uuid.UUID, error) {
	p := middleware.MustPrincipal(r.Context())
	if !p.HasRole(middleware.RoleDoctor) {
		return uuid.Nil, httpx.ErrForbidden
	}
	id, err := h.resolver.ResolveDoctorID(r.Context(), p.UserID)
	if err != nil {
		return uuid.Nil, apiError(err)
	}
	return id, nil
}

// apiError maps this package's errors onto the platform envelope. Anything
// unmapped becomes a 500 with the cause attached for the log and stripped from
// the response.
func apiError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrDateInvalid):
		return httpx.NewError(http.StatusUnprocessableEntity, httpx.CodeValidation,
			"from and to must be in YYYY-MM-DD form")
	case errors.Is(err, ErrRangeInvalid):
		return httpx.NewError(http.StatusUnprocessableEntity, httpx.CodeValidation,
			`"to" must not precede "from"`)
	case errors.Is(err, ErrRangeTooLong):
		return httpx.NewError(http.StatusUnprocessableEntity, httpx.CodeValidation,
			"the range may not exceed two years")
	case errors.Is(err, ErrDoctorProfileNotFound):
		return httpx.ErrNotFound
	default:
		return httpx.ErrInternal.WithCause(err)
	}
}

// ---------------------------------------------------------------------------
// wire types
// ---------------------------------------------------------------------------
//
// Dates go out as YYYY-MM-DD civil dates in the business timezone, never as
// instants. "The 14th of August" is a day in Colombo; rendering it as a UTC
// timestamp makes it the 13th for eighteen and a half hours, on a chart whose
// x-axis is days.
//
// Rates go out as FRACTIONS in [0,1]. The doctor app already renders the
// profile's no_show_rate as `(rate * 100).toStringAsFixed(1)`, so one
// convention across both is the difference between "9.7%" and "968%".

type summaryResponse struct {
	From     string `json:"from"`
	To       string `json:"to"`
	Timezone string `json:"timezone"`

	// Sessions is the headline tile: consultations that actually happened.
	Sessions          int `json:"sessions"`
	TotalAppointments int `json:"total_appointments"`
	CompletedCount    int `json:"completed_count"`
	NoShowCount       int `json:"no_show_count"`
	CancelledCount    int `json:"cancelled_count"`

	CompletionRate   float64 `json:"completion_rate"`
	NoShowRate       float64 `json:"no_show_rate"`
	CancellationRate float64 `json:"cancellation_rate"`

	AverageRating       float64 `json:"average_rating"`
	ReviewCount         int     `json:"review_count"`
	LifetimeRating      float64 `json:"lifetime_rating"`
	LifetimeReviewCount int     `json:"lifetime_review_count"`

	ConsultationCount int `json:"consultation_count"`
	// Both units are emitted: seconds because it is the stored quantity and
	// loses nothing, minutes because it is what the tile renders and every
	// client would otherwise divide by sixty itself, one of them wrongly.
	AverageConsultationSeconds float64 `json:"average_consultation_seconds"`
	AverageConsultationMinutes float64 `json:"average_consultation_minutes"`

	Daily []dailySessionsResponse `json:"daily"`
}

type dailySessionsResponse struct {
	Date                string `json:"date"`
	Completed           int    `json:"completed"`
	NoShow              int    `json:"no_show"`
	Cancelled           int    `json:"cancelled"`
	ConsultationCount   int    `json:"consultation_count"`
	ConsultationSeconds int64  `json:"consultation_seconds"`
}

func newSummaryResponse(s Summary, loc *time.Location) summaryResponse {
	daily := make([]dailySessionsResponse, len(s.Daily))
	for i, d := range s.Daily {
		daily[i] = dailySessionsResponse{
			Date:                d.Date.Format(time.DateOnly),
			Completed:           d.CompletedCount,
			NoShow:              d.NoShowCount,
			Cancelled:           d.CancelledCount,
			ConsultationCount:   d.ConsultationCount,
			ConsultationSeconds: d.ConsultationSeconds,
		}
	}
	return summaryResponse{
		From:     s.From.Format(time.DateOnly),
		To:       s.To.Format(time.DateOnly),
		Timezone: loc.String(),

		Sessions:          s.CompletedCount,
		TotalAppointments: s.TotalAppointments(),
		CompletedCount:    s.CompletedCount,
		NoShowCount:       s.NoShowCount,
		CancelledCount:    s.CancelledCount,

		CompletionRate:   s.CompletionRate(),
		NoShowRate:       s.NoShowRate(),
		CancellationRate: s.CancellationRate(),

		AverageRating:       s.AverageRating,
		ReviewCount:         s.ReviewCount,
		LifetimeRating:      s.LifetimeRating,
		LifetimeReviewCount: s.LifetimeReviewCount,

		ConsultationCount:          s.ConsultationCount,
		AverageConsultationSeconds: s.AverageConsultationSeconds(),
		AverageConsultationMinutes: s.AverageConsultationSeconds() / 60,

		Daily: daily,
	}
}

func (h *Handler) summary(w http.ResponseWriter, r *http.Request) {
	doctorID, err := h.doctorID(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	rng, err := h.svc.ResolveRange(r.URL.Query().Get("from"), r.URL.Query().Get("to"))
	if err != nil {
		httpx.Error(w, r, apiError(err))
		return
	}
	out, err := h.svc.Summarise(r.Context(), doctorID, rng)
	if err != nil {
		httpx.Error(w, r, apiError(err))
		return
	}
	httpx.OK(w, r, newSummaryResponse(out, h.svc.Location()))
}

type hourBucketResponse struct {
	// DayOfWeek is 0 = Sunday .. 6 = Saturday, matching Go's time.Weekday and
	// the day_of_week the availability editor already sends. It is omitted from
	// the hour-of-day collapse, where it has no meaning.
	DayOfWeek *int `json:"day_of_week,omitempty"`
	HourOfDay int  `json:"hour_of_day"`
	Bookings  int  `json:"bookings"`
	Completed int  `json:"completed"`
	NoShow    int  `json:"no_show"`
	Cancelled int  `json:"cancelled"`
}

type peakHoursResponse struct {
	Timezone string `json:"timezone"`
	// ByHourOfWeek holds only the cells with bookings, ordered by day then
	// hour. A dense 168-cell grid would be almost entirely zeroes: a doctor
	// works perhaps twenty of those hours.
	ByHourOfWeek []hourBucketResponse `json:"by_hour_of_week"`
	// ByHourOfDay is ALWAYS 24 entries, indexed 0..23 in order, so a client can
	// treat it as a dense array without checking. It is the same data collapsed
	// across weekdays.
	ByHourOfDay   []hourBucketResponse `json:"by_hour_of_day"`
	Busiest       *hourBucketResponse  `json:"busiest"`
	TotalBookings int                  `json:"total_bookings"`
}

func newPeakHoursResponse(p PeakHours, loc *time.Location) peakHoursResponse {
	week := make([]hourBucketResponse, len(p.Buckets))
	for i, b := range p.Buckets {
		dow := b.DayOfWeek
		week[i] = hourBucketResponse{
			DayOfWeek: &dow, HourOfDay: b.HourOfDay, Bookings: b.BookingCount,
			Completed: b.CompletedCount, NoShow: b.NoShowCount, Cancelled: b.CancelledCount,
		}
	}
	day := make([]hourBucketResponse, len(p.ByHourOfDay))
	// Indexed rather than ranged: ByHourOfDay is a fixed 24-element array and
	// `range` would copy the whole thing on every call.
	for i := range p.ByHourOfDay {
		b := &p.ByHourOfDay[i]
		day[i] = hourBucketResponse{
			HourOfDay: b.HourOfDay, Bookings: b.BookingCount,
			Completed: b.CompletedCount, NoShow: b.NoShowCount, Cancelled: b.CancelledCount,
		}
	}
	out := peakHoursResponse{
		Timezone:      loc.String(),
		ByHourOfWeek:  week,
		ByHourOfDay:   day,
		TotalBookings: p.Total,
	}
	if best, ok := p.Busiest(); ok {
		dow := best.DayOfWeek
		out.Busiest = &hourBucketResponse{
			DayOfWeek: &dow, HourOfDay: best.HourOfDay, Bookings: best.BookingCount,
			Completed: best.CompletedCount, NoShow: best.NoShowCount, Cancelled: best.CancelledCount,
		}
	}
	return out
}

func (h *Handler) peakHours(w http.ResponseWriter, r *http.Request) {
	doctorID, err := h.doctorID(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	out, err := h.svc.PeakHours(r.Context(), doctorID)
	if err != nil {
		httpx.Error(w, r, apiError(err))
		return
	}
	httpx.OK(w, r, newPeakHoursResponse(out, h.svc.Location()))
}

type currencyTotalsResponse struct {
	Currency        string `json:"currency"`
	GrossCents      int64  `json:"gross_cents"`
	CommissionCents int64  `json:"commission_cents"`
	NetCents        int64  `json:"net_cents"`
	PaidCents       int64  `json:"paid_cents"`
	UnpaidCents     int64  `json:"unpaid_cents"`
	PaymentCount    int    `json:"payment_count"`
	PayoutStatus    string `json:"payout_status"`
}

type payoutResponse struct {
	PayoutID    uuid.UUID `json:"payout_id"`
	AmountCents int64     `json:"amount_cents"`
	Currency    string    `json:"currency"`
	PeriodStart string    `json:"period_start"`
	PeriodEnd   string    `json:"period_end"`
	TransferID  string    `json:"transfer_id,omitempty"`
	SentAt      time.Time `json:"sent_at"`
}

type dailyEarningsResponse struct {
	Date            string `json:"date"`
	GrossCents      int64  `json:"gross_cents"`
	CommissionCents int64  `json:"commission_cents"`
	NetCents        int64  `json:"net_cents"`
	Currency        string `json:"currency,omitempty"`
	PaymentCount    int    `json:"payment_count"`
}

type earningsResponse struct {
	From     string `json:"from"`
	To       string `json:"to"`
	Timezone string `json:"timezone"`

	// The top-level totals are the primary currency's -- the one the doctor
	// earns most in, which on this platform is the only one they earn in. A
	// doctor with no settled payments in the window gets zeroes and
	// payout_status "no_earnings", never a null the client has to guard.
	Currency        string `json:"currency"`
	GrossCents      int64  `json:"gross_cents"`
	CommissionCents int64  `json:"commission_cents"`
	NetCents        int64  `json:"net_cents"`
	PaidCents       int64  `json:"paid_cents"`
	UnpaidCents     int64  `json:"unpaid_cents"`
	PaymentCount    int    `json:"payment_count"`
	PayoutStatus    string `json:"payout_status"`

	// ByCurrency carries every currency in the window. It exists so that the
	// day a doctor is paid in two currencies the totals above are visibly a
	// subset rather than a silently wrong sum.
	ByCurrency []currencyTotalsResponse `json:"by_currency"`

	Payouts []payoutResponse        `json:"payouts"`
	Daily   []dailyEarningsResponse `json:"daily"`
}

func newEarningsResponse(e Earnings, loc *time.Location) earningsResponse {
	byCurrency := make([]currencyTotalsResponse, len(e.ByCurrency))
	for i, c := range e.ByCurrency {
		byCurrency[i] = currencyTotalsResponse{
			Currency: c.Currency, GrossCents: c.GrossCents,
			CommissionCents: c.CommissionCents, NetCents: c.NetCents,
			PaidCents: c.PaidCents, UnpaidCents: c.UnpaidCents(),
			PaymentCount: c.PaymentCount, PayoutStatus: string(c.Status()),
		}
	}
	payouts := make([]payoutResponse, len(e.Payouts))
	for i := range e.Payouts {
		p := &e.Payouts[i]
		payouts[i] = payoutResponse{
			PayoutID: p.PayoutID, AmountCents: p.AmountCents, Currency: p.Currency,
			PeriodStart: p.PeriodStart.Format(time.DateOnly),
			PeriodEnd:   p.PeriodEnd.Format(time.DateOnly),
			TransferID:  p.TransferID, SentAt: p.SentAt.UTC(),
		}
	}
	daily := make([]dailyEarningsResponse, 0, len(e.Daily))
	for _, d := range e.Daily {
		if d.PaymentCount == 0 {
			// A day with sessions and no settled payment is not an earnings
			// data point; including it would draw a zero bar on a revenue chart
			// for a day whose money simply has not landed yet.
			continue
		}
		daily = append(daily, dailyEarningsResponse{
			Date: d.Date.Format(time.DateOnly), GrossCents: d.GrossCents,
			CommissionCents: d.CommissionCents, NetCents: d.NetCents,
			Currency: d.Currency, PaymentCount: d.PaymentCount,
		})
	}

	out := earningsResponse{
		From:       e.From.Format(time.DateOnly),
		To:         e.To.Format(time.DateOnly),
		Timezone:   loc.String(),
		Currency:   DefaultCurrency,
		ByCurrency: byCurrency,
		Payouts:    payouts,
		Daily:      daily,
	}
	primary, ok := e.Primary()
	if !ok {
		out.PayoutStatus = string(PayoutStatusNoEarnings)
		return out
	}
	out.Currency = primary.Currency
	out.GrossCents = primary.GrossCents
	out.CommissionCents = primary.CommissionCents
	out.NetCents = primary.NetCents
	out.PaidCents = primary.PaidCents
	out.UnpaidCents = primary.UnpaidCents()
	out.PaymentCount = primary.PaymentCount
	out.PayoutStatus = string(primary.Status())
	return out
}

func (h *Handler) earnings(w http.ResponseWriter, r *http.Request) {
	doctorID, err := h.doctorID(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	rng, err := h.svc.ResolveRange(r.URL.Query().Get("from"), r.URL.Query().Get("to"))
	if err != nil {
		httpx.Error(w, r, apiError(err))
		return
	}
	out, err := h.svc.Earn(r.Context(), doctorID, rng)
	if err != nil {
		httpx.Error(w, r, apiError(err))
		return
	}
	httpx.OK(w, r, newEarningsResponse(out, h.svc.Location()))
}
