// Package analytics feeds the admin dashboard (V2 docs section 7.3 page 2 /
// 8.8): revenue, bookings, doctor utilization, district activity. It never
// scans payment-service's or scheduling-service's raw tables -- it cannot,
// per ADR-004 -- and it never scans its own projection tables on a request
// path either. Every read here hits a materialized view, refreshed hourly by
// Refresher; every write here is one event handler applying one event to one
// narrow projection table (migrations/000004_analytics.up.sql).
package analytics

import (
	"time"

	"github.com/google/uuid"
)

// PaymentFact and AppointmentFact are the repository's input models for one
// projected row.
//
// They are deliberately NOT wire types and carry no json tags. The wire types
// live once, in internal/platform/events/payloads.go, shared with their
// producers; these are what a projector assembles after decoding one. The
// distinction matters: payment.succeeded, payment.failed and payment.refunded
// used to share a single private struct here, which quietly implied they have
// the same shape. They do not -- payment.refunded carries no doctor_id and no
// provider, payment.failed carries no commission -- and pretending otherwise
// wrote zeros into the revenue projection.
type PaymentFact struct {
	PaymentID       uuid.UUID
	AppointmentID   uuid.UUID
	DoctorID        uuid.UUID
	PatientID       uuid.UUID
	SpecialtyCode   string
	District        string
	AmountCents     int64
	CommissionCents int64
	Currency        string
	Provider        string
	OccurredAt      time.Time
}

// AppointmentFact is one projected appointment row.
type AppointmentFact struct {
	AppointmentID uuid.UUID
	DoctorID      uuid.UUID
	PatientID     uuid.UUID
	SpecialtyCode string
	District      string
	ScheduledAt   time.Time
	OccurredAt    time.Time
}

// RevenueDay is one row of revenue_daily.
type RevenueDay struct {
	Day             time.Time `json:"day"`
	Currency        string    `json:"currency"`
	GrossCents      int64     `json:"gross_cents"`
	CommissionCents int64     `json:"commission_cents"`
	PaymentCount    int64     `json:"payment_count"`
}

// BookingDay is one row of bookings_daily.
type BookingDay struct {
	Day           time.Time `json:"day"`
	SpecialtyCode string    `json:"specialty_code"`
	Status        string    `json:"status"`
	BookingCount  int64     `json:"booking_count"`
}

// DoctorUtilizationDay is one row of doctor_utilization_daily.
type DoctorUtilizationDay struct {
	Day            time.Time `json:"day"`
	DoctorID       uuid.UUID `json:"doctor_id"`
	CompletedCount int64     `json:"completed_count"`
	NoShowCount    int64     `json:"no_show_count"`
	CancelledCount int64     `json:"cancelled_count"`
	TotalCount     int64     `json:"total_count"`
}

// DoctorTotals aggregates DoctorUtilizationDay across a date range for the
// "top doctors" view.
type DoctorTotals struct {
	DoctorID       uuid.UUID `json:"doctor_id"`
	CompletedCount int64     `json:"completed_count"`
	NoShowCount    int64     `json:"no_show_count"`
	CancelledCount int64     `json:"cancelled_count"`
	TotalCount     int64     `json:"total_count"`
}

// DistrictDay is one row of district_activity_daily.
type DistrictDay struct {
	Day          time.Time `json:"day"`
	District     string    `json:"district"`
	BookingCount int64     `json:"booking_count"`
}

// DashboardSummary is the aggregate payload for GET /analytics/dashboard —
// what telemed-admin-web's home page renders in one round trip.
type DashboardSummary struct {
	RangeFrom              time.Time          `json:"range_from"`
	RangeTo                time.Time          `json:"range_to"`
	Currency               string             `json:"currency"`
	GrossCents             int64              `json:"gross_cents"`
	CommissionCents        int64              `json:"commission_cents"`
	Bookings               int64              `json:"bookings"`
	ActiveUsers            int64              `json:"active_users"`
	CompletedConsultations int64              `json:"completed_consultations"`
	NoShowRate             float64            `json:"no_show_rate"`
	Revenue                []RevenueDay       `json:"revenue"`
	BookingsDaily          []BookingDay       `json:"bookings_daily"`
	TopSpecialties         []SpecialtyShare   `json:"top_specialties"`
	DoctorUtilisation      []DoctorUtilRow    `json:"doctor_utilisation"`
	Districts              []DistrictActivity `json:"districts"`
}

// SpecialtyShare is bookings rolled up by specialty for the ranked-bars chart.
type SpecialtyShare struct {
	SpecialtyCode string `json:"specialty_code"`
	BookingCount  int64  `json:"booking_count"`
}

// DoctorUtilRow is doctor_utilization_daily rolled up across the range, with
// the display name from doctor_projection when we have one.
type DoctorUtilRow struct {
	DoctorID       uuid.UUID `json:"doctor_id"`
	DoctorName     *string   `json:"doctor_name"`
	CompletedCount int64     `json:"completed_count"`
	NoShowCount    int64     `json:"no_show_count"`
	CancelledCount int64     `json:"cancelled_count"`
	TotalCount     int64     `json:"total_count"`
}

// DistrictActivity is district_activity_daily rolled up across the range.
type DistrictActivity struct {
	District     string `json:"district"`
	BookingCount int64  `json:"booking_count"`
}

// DateRange bounds an analytics query. Zero values mean "unbounded".
type DateRange struct {
	From time.Time
	To   time.Time
}
