package notification

import (
	"testing"
	"time"
)

func mustLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("LoadLocation(%q): %v", name, err)
	}
	return loc
}

func dur(h, m int) time.Duration {
	return time.Duration(h)*time.Hour + time.Duration(m)*time.Minute
}

func TestInQuietHours_NilBoundsMeansNoQuietHours(t *testing.T) {
	loc := mustLoc(t, "Asia/Colombo")
	now := time.Date(2026, 8, 20, 23, 0, 0, 0, loc)
	start := dur(22, 0)
	if InQuietHours(now, loc, nil, nil) {
		t.Fatal("nil start/end must never be in quiet hours")
	}
	if InQuietHours(now, loc, &start, nil) {
		t.Fatal("only start set (end nil) must never be in quiet hours")
	}
}

func TestInQuietHours_NonWrappingWindow(t *testing.T) {
	loc := mustLoc(t, "Asia/Colombo")
	start, end := dur(13, 0), dur(14, 0)

	cases := []struct {
		name string
		hm   [2]int
		want bool
	}{
		{"before window", [2]int{12, 59}, false},
		{"at start, inclusive", [2]int{13, 0}, true},
		{"mid window", [2]int{13, 30}, true},
		{"at end, exclusive", [2]int{14, 0}, false},
		{"after window", [2]int{14, 1}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Date(2026, 8, 20, tc.hm[0], tc.hm[1], 0, 0, loc)
			if got := InQuietHours(now, loc, &start, &end); got != tc.want {
				t.Errorf("InQuietHours at %02d:%02d = %v, want %v", tc.hm[0], tc.hm[1], got, tc.want)
			}
		})
	}
}

func TestInQuietHours_WrappingWindow(t *testing.T) {
	// 22:00-06:00: the common "don't text me overnight" shape. Wraps midnight.
	loc := mustLoc(t, "Asia/Colombo")
	start, end := dur(22, 0), dur(6, 0)

	cases := []struct {
		name string
		hm   [2]int
		want bool
	}{
		{"before window (evening)", [2]int{21, 59}, false},
		{"at start, inclusive", [2]int{22, 0}, true},
		{"late evening", [2]int{23, 30}, true},
		{"just after midnight", [2]int{0, 30}, true},
		{"early morning, still quiet", [2]int{5, 59}, true},
		{"at end, exclusive", [2]int{6, 0}, false},
		{"mid-day, well outside", [2]int{12, 0}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Date(2026, 8, 20, tc.hm[0], tc.hm[1], 0, 0, loc)
			if got := InQuietHours(now, loc, &start, &end); got != tc.want {
				t.Errorf("InQuietHours at %02d:%02d = %v, want %v", tc.hm[0], tc.hm[1], got, tc.want)
			}
		})
	}
}

func TestNextQuietHoursEnd_WrappingWindow(t *testing.T) {
	loc := mustLoc(t, "Asia/Colombo")
	end := dur(6, 0)

	// Called from late evening: the end is tomorrow's 06:00.
	now := time.Date(2026, 8, 20, 23, 0, 0, 0, loc)
	want := time.Date(2026, 8, 21, 6, 0, 0, 0, loc)
	if got := NextQuietHoursEnd(now, loc, end); !got.Equal(want) {
		t.Errorf("NextQuietHoursEnd(23:00) = %v, want %v", got.In(loc), want)
	}

	// Called from just after midnight: the end is later the same day.
	now2 := time.Date(2026, 8, 21, 1, 0, 0, 0, loc)
	want2 := time.Date(2026, 8, 21, 6, 0, 0, 0, loc)
	if got := NextQuietHoursEnd(now2, loc, end); !got.Equal(want2) {
		t.Errorf("NextQuietHoursEnd(01:00) = %v, want %v", got.In(loc), want2)
	}
}

// TestQuietHours_AcrossDSTBoundary is the "across timezone boundaries" case
// the brief calls out by name. America/New_York springs forward on
// 2026-03-08: 02:00 local skips straight to 03:00. A naive implementation
// that computes "quiet hours end" as `midnight.Add(6 * time.Hour)` (adding a
// fixed real-world duration to an absolute instant) lands on wall-clock
// 07:00 that day, one hour late, because only 5 real hours of clock time
// separate 00:00 from 06:00 on a spring-forward day. This implementation
// instead reconstructs the end instant from explicit wall-clock hour/minute
// components via time.Date, which stays correct across the transition.
func TestQuietHours_AcrossDSTBoundary(t *testing.T) {
	loc := mustLoc(t, "America/New_York")
	start, end := dur(22, 0), dur(6, 0)

	// 01:00 local on the transition day is still within the overnight
	// window (00:00-06:00 half of the wrap).
	now := time.Date(2026, 3, 8, 1, 0, 0, 0, loc)
	if !InQuietHours(now, loc, &start, &end) {
		t.Fatal("01:00 on the DST transition day should still be within quiet hours")
	}

	// The window must still end at wall-clock 06:00 EDT -- NOT 07:00, which
	// is what naive Duration-on-an-instant arithmetic would produce because
	// only 5 real hours elapse from local midnight to 6am on this day.
	wantEnd := time.Date(2026, 3, 8, 6, 0, 0, 0, loc)
	gotEnd := NextQuietHoursEnd(now, loc, end)
	if !gotEnd.Equal(wantEnd) {
		t.Fatalf("NextQuietHoursEnd across DST spring-forward = %v (%s), want %v (%s)",
			gotEnd.In(loc), gotEnd.In(loc).Format("15:04 MST"), wantEnd, wantEnd.Format("15:04 MST"))
	}

	// Sanity: the offset actually changed across this call, proving the test
	// exercises a real transition and is not vacuously true.
	_, beforeOff := now.Zone()
	_, afterOff := gotEnd.In(loc).Zone()
	if beforeOff == afterOff {
		t.Fatalf("expected a UTC offset change across the window (EST -> EDT), got %d both times", beforeOff)
	}
}

func TestShouldDefer_UrgencyBypassesQuietHours(t *testing.T) {
	loc := mustLoc(t, "Asia/Colombo")
	start, end := dur(22, 0), dur(6, 0)
	now := time.Date(2026, 8, 20, 23, 0, 0, 0, loc) // deep in quiet hours

	for _, u := range []Urgency{UrgencyCritical, UrgencyUrgent} {
		if _, hold := ShouldDefer(now, loc, ChannelSMS, u, &start, &end); hold {
			t.Errorf("urgency %q must bypass quiet hours, got held", u)
		}
	}

	if _, hold := ShouldDefer(now, loc, ChannelSMS, UrgencyNormal, &start, &end); !hold {
		t.Error("normal urgency during quiet hours should be held")
	}
}

func TestShouldDefer_NeverAppliesToEmailOrInApp(t *testing.T) {
	// Quiet hours protect against a device buzzing at 2am; an inbox or an
	// in-app list is silent regardless of when it fills.
	loc := mustLoc(t, "Asia/Colombo")
	start, end := dur(22, 0), dur(6, 0)
	now := time.Date(2026, 8, 20, 23, 0, 0, 0, loc)

	for _, ch := range []Channel{ChannelEmail, ChannelInApp} {
		if _, hold := ShouldDefer(now, loc, ch, UrgencyNormal, &start, &end); hold {
			t.Errorf("channel %q must never be deferred by quiet hours, got held", ch)
		}
	}
}

func TestShouldDefer_OutsideQuietHoursNeverHolds(t *testing.T) {
	loc := mustLoc(t, "Asia/Colombo")
	start, end := dur(22, 0), dur(6, 0)
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, loc) // midday

	if _, hold := ShouldDefer(now, loc, ChannelSMS, UrgencyNormal, &start, &end); hold {
		t.Error("midday send should never be deferred by an overnight quiet-hours window")
	}
}

// quiet_hours_start_min and quiet_hours_end_min are SMALLINT columns with
// CHECK (... BETWEEN 0 AND 1439). durationToMinutes used to narrow int64 to
// int16 unchecked, so 32768 minutes became -32768 -- a plausible-looking clock
// value that only Postgres would have caught, as a 500. Reject it here.
func TestDurationToMinutesRejectsWhatTheColumnCannotHold(t *testing.T) {
	t.Parallel()

	ptr := func(d time.Duration) *time.Duration { return &d }

	cases := []struct {
		name    string
		in      *time.Duration
		want    int16
		wantErr bool
	}{
		{name: "nil is nil", in: nil},
		{name: "midnight", in: ptr(0), want: 0},
		{name: "22:00", in: ptr(22 * time.Hour), want: 1320},
		{name: "the last minute of the day", in: ptr(1439 * time.Minute), want: 1439},
		{name: "exactly one day is out of range", in: ptr(24 * time.Hour), wantErr: true},
		{name: "negative is out of range", in: ptr(-time.Minute), wantErr: true},
		// 32768 minutes is the first value that wraps an int16 to negative.
		{name: "the first wrapping value", in: ptr(32768 * time.Minute), wantErr: true},
		{name: "a year", in: ptr(365 * 24 * time.Hour), wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := durationToMinutes(tc.in)
			if tc.wantErr {
				if err == nil {
					v := int16(-1)
					if got != nil {
						v = *got
					}
					t.Fatalf("durationToMinutes(%v) returned %d with no error; the column CHECK is 0..1439", *tc.in, v)
				}
				return
			}
			if err != nil {
				t.Fatalf("durationToMinutes: unexpected error: %v", err)
			}
			if tc.in == nil {
				if got != nil {
					t.Fatalf("nil in, %d out", *got)
				}
				return
			}
			if got == nil || *got != tc.want {
				t.Fatalf("durationToMinutes(%v) = %v, want %d", *tc.in, got, tc.want)
			}
		})
	}
}
