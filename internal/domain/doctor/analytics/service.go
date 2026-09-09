package analytics

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"telemed/internal/platform/database"
)

// DefaultCurrency is the platform's only currency today. It appears here as a
// named constant rather than an inline "LKR" so the places that assume it are
// greppable on the day a second currency arrives.
const DefaultCurrency = "LKR"

// Window bounds and defaults. They are constants rather than configuration
// because they shape a product surface: changing the default range changes what
// every doctor sees when they open the screen, and that is a reviewed decision,
// not an environment variable somebody edits at 2am.
const (
	// DefaultWindowDays is what a request with no from/to gets: the last thirty
	// days including today.
	DefaultWindowDays = 30

	// MaxWindowDays caps a range at roughly two years. The rollup is one row
	// per doctor per active day, so this bounds the response at a few hundred
	// rows rather than at whatever a client asks for.
	MaxWindowDays = 732
)

// Errors this package returns. handler.go is the only place they become an
// HTTP status.
var (
	// ErrRangeInvalid is a "to" that precedes its "from".
	ErrRangeInvalid = errors.New("analytics: range ends before it starts")
	// ErrRangeTooLong is a range beyond MaxWindowDays.
	ErrRangeTooLong = errors.New("analytics: range is too long")
	// ErrDateInvalid is an unparseable from/to.
	ErrDateInvalid = errors.New("analytics: dates must be in YYYY-MM-DD form")

	// ErrDoctorProfileNotFound is an authenticated account with the doctor role
	// but no doctor profile in this service -- a token minted before
	// registration completed, or an account whose profile was soft-deleted.
	//
	// It is returned by DoctorResolver implementations so that this package can
	// map it to 404 without importing the doctor domain to compare against its
	// sentinel.
	ErrDoctorProfileNotFound = errors.New("analytics: no doctor profile for this account")
)

// Service answers the doctor-facing analytics questions from the projection.
// It runs no SQL of its own and never sees an http.Request.
type Service struct {
	pool database.Pool
	repo *Repository
	loc  *time.Location
}

// NewService builds the read service.
func NewService(pool database.Pool, loc *time.Location) *Service {
	if loc == nil {
		loc = time.UTC
	}
	return &Service{pool: pool, repo: NewRepository(), loc: loc}
}

// Location returns the business timezone every date in this package is
// expressed in.
func (s *Service) Location() *time.Location { return s.loc }

// Range is a civil-date window, inclusive at both ends.
//
// Inclusive-to is what a doctor means by "1 August to 31 August", and it is
// what the client sends. Every half-open conversion happens once, here, rather
// than in three query builders that would eventually disagree by one day.
type Range struct {
	From time.Time // UTC midnight of the civil from-date
	To   time.Time // UTC midnight of the civil to-date
}

// ResolveRange turns optional YYYY-MM-DD strings into a validated window,
// defaulting to the last DefaultWindowDays ending today.
func (s *Service) ResolveRange(rawFrom, rawTo string) (Range, error) {
	today := dateIn(time.Now(), s.loc)

	to := today
	if rawTo != "" {
		parsed, err := time.Parse(time.DateOnly, rawTo)
		if err != nil {
			return Range{}, ErrDateInvalid
		}
		to = time.Date(parsed.Year(), parsed.Month(), parsed.Day(), 0, 0, 0, 0, time.UTC)
	}

	from := to.AddDate(0, 0, -(DefaultWindowDays - 1))
	if rawFrom != "" {
		parsed, err := time.Parse(time.DateOnly, rawFrom)
		if err != nil {
			return Range{}, ErrDateInvalid
		}
		from = time.Date(parsed.Year(), parsed.Month(), parsed.Day(), 0, 0, 0, 0, time.UTC)
	}

	if to.Before(from) {
		return Range{}, ErrRangeInvalid
	}
	if int(to.Sub(from).Hours()/24)+1 > MaxWindowDays {
		return Range{}, ErrRangeTooLong
	}
	return Range{From: from, To: to}, nil
}

// instants converts the civil window into the half-open instant range the
// reviews table's TIMESTAMPTZ column needs: local midnight on the from-date, up
// to local midnight on the day AFTER the to-date.
func (r Range) instants(loc *time.Location) (start, end time.Time) {
	start = time.Date(r.From.Year(), r.From.Month(), r.From.Day(), 0, 0, 0, 0, loc)
	endDay := r.To.AddDate(0, 0, 1)
	end = time.Date(endDay.Year(), endDay.Month(), endDay.Day(), 0, 0, 0, 0, loc)
	return start.UTC(), end.UTC()
}

// Summarise answers GET /doctors/me/analytics.
func (s *Service) Summarise(ctx context.Context, doctorID uuid.UUID, rng Range) (Summary, error) {
	daily, err := s.repo.ListDaily(ctx, s.pool, doctorID, rng.From, rng.To)
	if err != nil {
		return Summary{}, err
	}

	out := Summary{From: rng.From, To: rng.To, Daily: daily}
	for i := range daily {
		out.CompletedCount += daily[i].CompletedCount
		out.NoShowCount += daily[i].NoShowCount
		out.CancelledCount += daily[i].CancelledCount
		out.ConsultationCount += daily[i].ConsultationCount
		out.ConsultationSeconds += daily[i].ConsultationSeconds
	}

	start, end := rng.instants(s.loc)
	windowAvg, windowCount, lifetimeAvg, lifetimeCount, err :=
		s.repo.ReviewStats(ctx, s.pool, doctorID, start, end)
	if err != nil {
		return Summary{}, err
	}
	out.AverageRating = windowAvg
	out.ReviewCount = windowCount
	out.LifetimeRating = lifetimeAvg
	out.LifetimeReviewCount = lifetimeCount

	return out, nil
}

// PeakHours answers GET /doctors/me/analytics/peak-hours.
//
// It returns both shapes, and that is not indecision. The hour-of-WEEK grid is
// the useful answer -- "Tuesday at 10 is my busiest hour" is actionable in a way
// that "10am is busy" is not. The hour-of-DAY collapse is included because the
// doctor app's existing chart is a 24-bucket bar chart it builds client-side by
// counting appointment start hours, and giving it a drop-in replacement is what
// lets that screen switch to real data in one commit instead of a redesign.
func (s *Service) PeakHours(ctx context.Context, doctorID uuid.UUID) (PeakHours, error) {
	buckets, err := s.repo.ListPeakHours(ctx, s.pool, doctorID)
	if err != nil {
		return PeakHours{}, err
	}
	return collapse(buckets), nil
}

// collapse folds the hour-of-week cells into the dense 24-entry hour-of-day
// array and totals them. It is pure, so the shape the doctor app indexes by
// position is testable without a database.
func collapse(buckets []HourBucket) PeakHours {
	out := PeakHours{Buckets: buckets}
	for h := range out.ByHourOfDay {
		// DayOfWeek is meaningless once collapsed; -1 says so explicitly rather
		// than leaving a zero that reads as Sunday.
		out.ByHourOfDay[h] = HourBucket{DayOfWeek: -1, HourOfDay: h}
	}
	for _, b := range buckets {
		if b.HourOfDay < 0 || b.HourOfDay > 23 {
			continue
		}
		cell := &out.ByHourOfDay[b.HourOfDay]
		cell.BookingCount += b.BookingCount
		cell.CompletedCount += b.CompletedCount
		cell.NoShowCount += b.NoShowCount
		cell.CancelledCount += b.CancelledCount
		out.Total += b.BookingCount
	}
	return out
}

// Earn answers GET /doctors/me/earnings.
func (s *Service) Earn(ctx context.Context, doctorID uuid.UUID, rng Range) (Earnings, error) {
	totals, err := s.repo.EarningsByCurrency(ctx, s.pool, doctorID, rng.From, rng.To)
	if err != nil {
		return Earnings{}, err
	}
	payouts, err := s.repo.ListPayouts(ctx, s.pool, doctorID, rng.From, rng.To)
	if err != nil {
		return Earnings{}, err
	}
	daily, err := s.repo.ListDaily(ctx, s.pool, doctorID, rng.From, rng.To)
	if err != nil {
		return Earnings{}, err
	}
	return Earnings{
		From:       rng.From,
		To:         rng.To,
		ByCurrency: totals,
		Payouts:    payouts,
		Daily:      daily,
	}, nil
}
