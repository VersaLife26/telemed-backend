package notification

import "time"

// InterruptiveChannels are the channels that make a sound or vibrate on a
// device -- the only ones quiet hours meaningfully protect against. Email
// sits in an inbox until opened; in_app sits in a notification list until
// opened. Neither wakes anyone at 2am, so quiet hours never apply to them
// regardless of urgency.
func quietHoursApply(ch Channel) bool {
	return ch == ChannelSMS || ch == ChannelPush
}

// clockDuration returns t's wall-clock time-of-day as a Duration since local
// midnight, read directly off t's own Hour/Minute/Second. This -- not
// subtracting a separately-constructed midnight time.Time -- is what stays
// correct on a DST transition day: subtracting two absolute instants counts
// elapsed real time, which runs an hour ahead or behind wall-clock time on
// the one or two days a year a zone's offset changes.
func clockDuration(t time.Time) time.Duration {
	h, m, s := t.Clock()
	return time.Duration(h)*time.Hour + time.Duration(m)*time.Minute + time.Duration(s)*time.Second
}

// atClock returns the instant on sameDayAs's calendar date, in loc, at
// wall-clock offset d since midnight. Building it via time.Date from
// explicit hour/minute/second components -- rather than adding a fixed
// Duration to a midnight time.Time -- is what resolves to the correct
// wall-clock instant even on a day loc's clocks skip or repeat an hour:
// time.Date's documented normalization handles the skipped/repeated hour,
// whereas Time.Add on an absolute instant has no notion of "wall clock" at
// all.
func atClock(sameDayAs time.Time, loc *time.Location, d time.Duration) time.Time {
	h := int(d / time.Hour)
	m := int((d % time.Hour) / time.Minute)
	s := int((d % time.Minute) / time.Second)
	return time.Date(sameDayAs.Year(), sameDayAs.Month(), sameDayAs.Day(), h, m, s, 0, loc)
}

// InQuietHours reports whether instant now falls within quiet hours,
// evaluated in loc (the user's own timezone) so "22:00" means 10pm Colombo
// time for a Colombo user regardless of what timezone the server or the
// caller's UTC instant implies.
//
// A window that crosses midnight (start=22:00, end=06:00) is handled by the
// wrap check: end < start means the window spans 22:00..24:00 and
// 00:00..06:00 rather than the (empty, nonsensical) 22:00..06:00-same-day.
func InQuietHours(now time.Time, loc *time.Location, start, end *time.Duration) bool {
	if start == nil || end == nil {
		return false
	}
	sinceMidnight := clockDuration(now.In(loc))

	if *end < *start {
		return sinceMidnight >= *start || sinceMidnight < *end
	}
	return sinceMidnight >= *start && sinceMidnight < *end
}

// NextQuietHoursEnd returns the next instant, in UTC, at which the quiet
// hours window containing `now` ends. Callers use this to compute
// notifications.scheduled_for when deferring a non-urgent, interruptive-
// channel send raised while InQuietHours(now, ...) is true.
func NextQuietHoursEnd(now time.Time, loc *time.Location, end time.Duration) time.Time {
	local := now.In(loc)
	candidate := atClock(local, loc, end)
	if !candidate.After(local) {
		candidate = atClock(local.AddDate(0, 0, 1), loc, end)
	}
	return candidate.UTC()
}

// ShouldDefer decides, for one message about to be sent on channel ch with
// urgency u, whether it must be held until quiet hours end. It returns the
// UTC instant to hold until when true.
func ShouldDefer(now time.Time, loc *time.Location, ch Channel, u Urgency, start, end *time.Duration) (time.Time, bool) {
	if u == UrgencyCritical || u == UrgencyUrgent {
		return time.Time{}, false
	}
	if !quietHoursApply(ch) {
		return time.Time{}, false
	}
	if !InQuietHours(now, loc, start, end) {
		return time.Time{}, false
	}
	return NextQuietHoursEnd(now, loc, *end), true
}
