package analytics

import (
	"errors"
	"math"
	"testing"
	"time"
)

func almost(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// TestRatesAreFractionsNotPercentages pins the convention the doctor app
// already depends on.
//
// Its stat card renders the profile's no_show_rate as
// `(rate * 100).toStringAsFixed(1)`. Returning 9.68 where it expects 0.0968
// shows a doctor a 968% no-show rate, and nothing anywhere errors.
func TestRatesAreFractionsNotPercentages(t *testing.T) {
	s := Summary{CompletedCount: 84, NoShowCount: 9, CancelledCount: 7}

	if got := s.TotalAppointments(); got != 100 {
		t.Fatalf("total = %d, want 100", got)
	}
	if got := s.CompletionRate(); !almost(got, 0.84) {
		t.Errorf("completion rate = %v, want 0.84", got)
	}
	if got := s.CancellationRate(); !almost(got, 0.07) {
		t.Errorf("cancellation rate = %v, want 0.07", got)
	}
	for _, r := range []float64{s.CompletionRate(), s.NoShowRate(), s.CancellationRate()} {
		if r < 0 || r > 1 {
			t.Errorf("rate %v is outside [0,1]", r)
		}
	}
}

// TestNoShowRateExcludesCancellations keeps cancellations out of the
// denominator. A cancellation is a patient who told somebody, often days ahead. Counting it in the denominator dilutes the one
// number that is supposed to measure people who simply did not turn up, and it
// would put this figure out of step with scheduling-service's per-patient
// NoShowStats.NoShowRate -- which drives the prepayment rule, so a doctor and
// an administrator would be arguing about two different quantities under one
// name.
func TestNoShowRateExcludesCancellations(t *testing.T) {
	s := Summary{CompletedCount: 9, NoShowCount: 1, CancelledCount: 90}
	if got := s.NoShowRate(); !almost(got, 0.1) {
		t.Errorf("no-show rate = %v, want 0.1 (1 of 10 attended, not 1 of 100)", got)
	}
}

// TestEmptyWindowDoesNotDivideByZero guards every ratio against an empty
// window. A doctor's first week has no appointments, and a NaN on the wire is a chart that renders nothing with no
// explanation.
func TestEmptyWindowDoesNotDivideByZero(t *testing.T) {
	var s Summary
	for name, got := range map[string]float64{
		"completion":   s.CompletionRate(),
		"no_show":      s.NoShowRate(),
		"cancellation": s.CancellationRate(),
		"avg_duration": s.AverageConsultationSeconds(),
	} {
		if math.IsNaN(got) || math.IsInf(got, 0) {
			t.Errorf("%s = %v on an empty window; JSON cannot even encode it", name, got)
		}
		if got != 0 {
			t.Errorf("%s = %v on an empty window, want 0", name, got)
		}
	}
}

// TestAverageDurationDividesOnceAtTheEnd checks the range average is computed
// from the summed components rather than from the per-day means.
//
// Storing a pre-divided daily average and then averaging the averages weights a
// day with one consultation the same as a day with twenty. Keeping the sum and
// the count is what makes the range figure exact.
func TestAverageDurationDividesOnceAtTheEnd(t *testing.T) {
	s := Summary{
		Daily: []DailyRow{
			{ConsultationCount: 1, ConsultationSeconds: 60},     // 60s average
			{ConsultationCount: 19, ConsultationSeconds: 22800}, // 1200s average
		},
	}
	for _, d := range s.Daily {
		s.ConsultationCount += d.ConsultationCount
		s.ConsultationSeconds += d.ConsultationSeconds
	}

	const want = 22860.0 / 20.0 // 1143
	if got := s.AverageConsultationSeconds(); !almost(got, want) {
		t.Errorf("average = %v, want %v", got, want)
	}
	// The naive "mean of the daily means" would be 630 -- almost half.
	if almost(s.AverageConsultationSeconds(), (60+1200)/2.0) {
		t.Error("the average is the mean of the daily means, which weights one consultation like nineteen")
	}
}

// TestPayoutStatus covers each badge, including the distinction between a
// doctor who is owed nothing and a doctor who has not been paid.
func TestPayoutStatus(t *testing.T) {
	cases := []struct {
		name string
		c    CurrencyTotals
		want PayoutStatus
	}{
		{"no earnings at all", CurrencyTotals{}, PayoutStatusNoEarnings},
		{"earned, nothing settled", CurrencyTotals{NetCents: 100000}, PayoutStatusPending},
		{"half settled", CurrencyTotals{NetCents: 100000, PaidCents: 40000}, PayoutStatusPartial},
		{"fully settled", CurrencyTotals{NetCents: 100000, PaidCents: 100000}, PayoutStatusPaid},
		// A payout can exceed the window's net when its period straddles the
		// window edge. That is paid, not an error.
		{"overpaid by an overlapping period", CurrencyTotals{NetCents: 100000, PaidCents: 150000}, PayoutStatusPaid},
	}
	for _, tc := range cases {
		if got := tc.c.Status(); got != tc.want {
			t.Errorf("%s: status = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestUnpaidCentsIsExact keeps the arithmetic in integer cents. Money is int64
// end to end; a float here would
// be the one place rounding could enter a figure a doctor reconciles against
// their bank statement.
func TestUnpaidCentsIsExact(t *testing.T) {
	c := CurrencyTotals{NetCents: 333333, PaidCents: 111111}
	if got := c.UnpaidCents(); got != 222222 {
		t.Errorf("unpaid = %d, want 222222", got)
	}
}

func testService(t *testing.T) *Service {
	t.Helper()
	loc, err := time.LoadLocation("Asia/Colombo")
	if err != nil {
		t.Fatalf("load Asia/Colombo: %v", err)
	}
	return NewService(nil, loc)
}

// TestResolveRange covers the defaulting and the guards.
func TestResolveRange(t *testing.T) {
	svc := testService(t)

	rng, err := svc.ResolveRange("2026-08-01", "2026-08-31")
	if err != nil {
		t.Fatalf("explicit range: %v", err)
	}
	if rng.From.Format(time.DateOnly) != "2026-08-01" || rng.To.Format(time.DateOnly) != "2026-08-31" {
		t.Errorf("range = %s..%s", rng.From.Format(time.DateOnly), rng.To.Format(time.DateOnly))
	}

	// A single day is a legal range: "how did today go".
	if _, err := svc.ResolveRange("2026-08-01", "2026-08-01"); err != nil {
		t.Errorf("single-day range: %v", err)
	}

	// Defaults: thirty days ending today, inclusive at both ends.
	def, err := svc.ResolveRange("", "")
	if err != nil {
		t.Fatalf("default range: %v", err)
	}
	if days := int(def.To.Sub(def.From).Hours()/24) + 1; days != DefaultWindowDays {
		t.Errorf("default window is %d days, want %d", days, DefaultWindowDays)
	}

	for name, tc := range map[string]struct{ from, to string }{
		"reversed":         {"2026-08-31", "2026-08-01"},
		"bad from":         {"31-08-2026", "2026-08-31"},
		"bad to":           {"2026-08-01", "not-a-date"},
		"instant not date": {"2026-08-01T00:00:00Z", "2026-08-31"},
	} {
		if _, err := svc.ResolveRange(tc.from, tc.to); err == nil {
			t.Errorf("%s (%q..%q) was accepted", name, tc.from, tc.to)
		}
	}

	if _, err := svc.ResolveRange("2000-01-01", "2026-08-31"); !errors.Is(err, ErrRangeTooLong) {
		t.Errorf("a 26-year range: %v, want ErrRangeTooLong", err)
	}
}

// TestRangeInstantsCoverTheWholeLocalDay checks the civil window becomes a
// half-open instant range anchored on Colombo midnight, not UTC midnight.
//
// The reviews table is TIMESTAMPTZ, so an inclusive civil range has to become a
// half-open instant range. Getting it wrong by one day silently drops every
// review written on the last day of the window -- and a doctor checking "how
// did I do this month" on the 31st would see nothing they earned that day.
func TestRangeInstantsCoverTheWholeLocalDay(t *testing.T) {
	svc := testService(t)
	rng, err := svc.ResolveRange("2026-08-01", "2026-08-31")
	if err != nil {
		t.Fatalf("range: %v", err)
	}
	start, end := rng.instants(svc.Location())

	// Colombo is +05:30, so local midnight on 1 August is 18:30 UTC on 31 July.
	if got := start.Format(time.RFC3339); got != "2026-07-31T18:30:00Z" {
		t.Errorf("window starts at %s, want 2026-07-31T18:30:00Z (local midnight, not UTC midnight)", got)
	}
	if got := end.Format(time.RFC3339); got != "2026-08-31T18:30:00Z" {
		t.Errorf("window ends at %s, want 2026-08-31T18:30:00Z (exclusive, local midnight on 1 September)", got)
	}

	// A review written at 23:00 Colombo on the last day must fall inside.
	late := time.Date(2026, 8, 31, 23, 0, 0, 0, svc.Location())
	if !late.After(start) || !late.Before(end) {
		t.Error("a review written late on the final day of the window falls outside it")
	}
}

// TestParsePeriodDateAcceptsBothForms checks a payout period parses whether it
// arrives as a civil date or as an RFC3339 instant.
//
// events.PayoutSent declares period_start/period_end as "YYYY-MM-DD" and
// payment-service currently sends RFC3339 -- a live mismatch recorded in
// _shared/INTEGRATION-FIXES.md. Refusing the wrong one would drop every payout
// event and report a doctor as unpaid for money already in their account.
func TestParsePeriodDateAcceptsBothForms(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Colombo")
	if err != nil {
		t.Fatalf("load Asia/Colombo: %v", err)
	}

	plain, err := parsePeriodDate("2026-08-15", loc)
	if err != nil {
		t.Fatalf("declared form: %v", err)
	}
	if got := plain.Format(time.DateOnly); got != "2026-08-15" {
		t.Errorf("declared form parsed to %s", got)
	}

	// 18:30Z on the 15th is 00:00 on the 16th in Colombo -- and a period end of
	// "2026-08-15T18:30:00Z" is a payment-service instant meaning the END of
	// the 15th. Resolving it in the business timezone is what keeps the
	// coverage window on the day the doctor was told.
	instant, err := parsePeriodDate("2026-08-15T12:00:00Z", loc)
	if err != nil {
		t.Fatalf("rfc3339 form: %v", err)
	}
	if got := instant.Format(time.DateOnly); got != "2026-08-15" {
		t.Errorf("rfc3339 midday parsed to %s, want 2026-08-15", got)
	}

	if _, err := parsePeriodDate("", loc); err == nil {
		t.Error("an empty period date was accepted")
	}
	if _, err := parsePeriodDate("15/08/2026", loc); err == nil {
		t.Error("a non-ISO period date was accepted")
	}
}

// TestPeakHoursCollapse checks the hour-of-day view is always 24 dense,
// positional buckets.
//
// The doctor app's existing chart builds `List<int>.filled(24, 0)` and indexes
// it by hour. by_hour_of_day is its drop-in replacement, so it has to be dense
// and in order -- a sparse array would make the client index by position and
// read Tuesday's count into the 9am slot.
func TestPeakHoursCollapse(t *testing.T) {
	buckets := []HourBucket{
		{DayOfWeek: 1, HourOfDay: 9, BookingCount: 3, CompletedCount: 3},
		{DayOfWeek: 2, HourOfDay: 9, BookingCount: 4, CompletedCount: 2, NoShowCount: 2},
		{DayOfWeek: 2, HourOfDay: 14, BookingCount: 1, CancelledCount: 1},
	}
	out := collapse(buckets)

	if len(out.ByHourOfDay) != 24 {
		t.Fatalf("by_hour_of_day has %d entries, want exactly 24", len(out.ByHourOfDay))
	}
	for h := range out.ByHourOfDay {
		if out.ByHourOfDay[h].HourOfDay != h {
			t.Fatalf("by_hour_of_day[%d] reports hour %d: the array is not positional", h, out.ByHourOfDay[h].HourOfDay)
		}
	}
	if got := out.ByHourOfDay[9].BookingCount; got != 7 {
		t.Errorf("09:00 collapsed to %d bookings, want 7 (3 Monday + 4 Tuesday)", got)
	}
	if got := out.ByHourOfDay[9].NoShowCount; got != 2 {
		t.Errorf("09:00 no-shows = %d, want 2", got)
	}
	if out.Total != 8 {
		t.Errorf("total = %d, want 8", out.Total)
	}

	best, ok := out.Busiest()
	if !ok {
		t.Fatal("no busiest bucket for a doctor with bookings")
	}
	if best.DayOfWeek != 2 || best.HourOfDay != 9 {
		t.Errorf("busiest = day %d hour %d, want Tuesday 09:00", best.DayOfWeek, best.HourOfDay)
	}

	// A doctor with no history must not produce a busiest cell at all, or the
	// screen renders "your busiest hour is Sunday midnight".
	if _, ok := collapse(nil).Busiest(); ok {
		t.Error("a doctor with no bookings has a busiest hour")
	}
}
