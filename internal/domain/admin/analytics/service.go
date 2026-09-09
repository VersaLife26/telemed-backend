package analytics

import (
	"context"

	"github.com/google/uuid"
)

type Service struct {
	repo *Repository
}

func NewService(repo *Repository) *Service { return &Service{repo: repo} }

func (s *Service) Revenue(ctx context.Context, dr DateRange) ([]RevenueDay, error) {
	return s.repo.Revenue(ctx, dr)
}

func (s *Service) Bookings(ctx context.Context, dr DateRange) ([]BookingDay, error) {
	return s.repo.Bookings(ctx, dr)
}

// TopDoctors backs GET /analytics/doctors: doctors ranked by total booking
// volume across the range.
func (s *Service) TopDoctors(ctx context.Context, dr DateRange, limit int) ([]DoctorTotals, error) {
	return s.repo.TopDoctors(ctx, dr, limit)
}

// Utilization backs GET /analytics/utilization: the daily series, optionally
// scoped to one doctor.
func (s *Service) Utilization(ctx context.Context, doctorID uuid.UUID, dr DateRange) ([]DoctorUtilizationDay, error) {
	return s.repo.UtilizationDaily(ctx, doctorID, dr)
}

func (s *Service) Districts(ctx context.Context, dr DateRange) ([]DistrictDay, error) {
	return s.repo.Districts(ctx, dr)
}

// Dashboard aggregates the four materialized views into the single payload
// the admin console home page expects. Missing views (empty range, first
// boot before any events) yield zeros and empty series, never an error.
func (s *Service) Dashboard(ctx context.Context, dr DateRange) (DashboardSummary, error) {
	revenue, err := s.repo.Revenue(ctx, dr)
	if err != nil {
		return DashboardSummary{}, err
	}
	bookings, err := s.repo.Bookings(ctx, dr)
	if err != nil {
		return DashboardSummary{}, err
	}
	util, err := s.repo.DoctorUtilisationSummary(ctx, dr, 50)
	if err != nil {
		return DashboardSummary{}, err
	}
	districts, err := s.repo.DistrictSummary(ctx, dr)
	if err != nil {
		return DashboardSummary{}, err
	}
	activeUsers, err := s.repo.ActiveUsers(ctx, dr)
	if err != nil {
		return DashboardSummary{}, err
	}

	out := DashboardSummary{
		RangeFrom:         dr.From,
		RangeTo:           dr.To,
		Currency:          "LKR",
		Revenue:           revenue,
		BookingsDaily:     bookings,
		DoctorUtilisation: util,
		Districts:         districts,
		ActiveUsers:       activeUsers,
		TopSpecialties:    specialtyShares(bookings),
	}
	if out.Revenue == nil {
		out.Revenue = []RevenueDay{}
	}
	if out.BookingsDaily == nil {
		out.BookingsDaily = []BookingDay{}
	}
	if out.DoctorUtilisation == nil {
		out.DoctorUtilisation = []DoctorUtilRow{}
	}
	if out.Districts == nil {
		out.Districts = []DistrictActivity{}
	}
	if out.TopSpecialties == nil {
		out.TopSpecialties = []SpecialtyShare{}
	}

	var completed, noShows, totalAppts int64
	for _, row := range revenue {
		out.GrossCents += row.GrossCents
		out.CommissionCents += row.CommissionCents
		if row.Currency != "" {
			out.Currency = row.Currency
		}
	}
	for _, row := range bookings {
		out.Bookings += row.BookingCount
		switch row.Status {
		case "completed":
			completed += row.BookingCount
		case "no_show":
			noShows += row.BookingCount
		}
		if row.Status == "completed" || row.Status == "no_show" || row.Status == "cancelled" || row.Status == "confirmed" || row.Status == "created" {
			totalAppts += row.BookingCount
		}
	}
	out.CompletedConsultations = completed
	if totalAppts > 0 {
		out.NoShowRate = float64(noShows) / float64(totalAppts)
	}
	return out, nil
}

func specialtyShares(bookings []BookingDay) []SpecialtyShare {
	totals := map[string]int64{}
	for _, row := range bookings {
		totals[row.SpecialtyCode] += row.BookingCount
	}
	out := make([]SpecialtyShare, 0, len(totals))
	for code, count := range totals {
		out = append(out, SpecialtyShare{SpecialtyCode: code, BookingCount: count})
	}
	// Stable order: highest volume first.
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j].BookingCount > out[i].BookingCount {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	if len(out) > 10 {
		out = out[:10]
	}
	return out
}
