package scheduling

import (
	"database/sql/driver"
	"fmt"
	"time"
)

// Date is a calendar date with no time and no zone -- "14 April 2027", the
// thing a patient means when they say "I want an appointment on Thursday".
//
// It exists because time.Time cannot represent that. A time.Time is an instant,
// so storing a date in one forces a timezone choice, and every such choice is a
// bug waiting for the first patient who books from Dubai: 2027-04-14T00:00Z is
// 2027-04-14 05:30 in Colombo, and 2027-04-13 in Los Angeles. The waitlist keys
// on "the doctor's clinic day", which is a civil date, so we model it as one.
type Date struct {
	Year  int
	Month time.Month
	Day   int
}

// DateIn returns the calendar date on which t falls in loc.
func DateIn(t time.Time, loc *time.Location) Date {
	y, m, d := t.In(loc).Date()
	return Date{Year: y, Month: m, Day: d}
}

// ParseDate reads the ISO-8601 form the API uses, YYYY-MM-DD.
func ParseDate(s string) (Date, error) {
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		return Date{}, fmt.Errorf("scheduling: parse date %q: %w", s, err)
	}
	y, m, d := t.Date()
	return Date{Year: y, Month: m, Day: d}, nil
}

// String renders YYYY-MM-DD.
func (d Date) String() string { return fmt.Sprintf("%04d-%02d-%02d", d.Year, int(d.Month), d.Day) }

// IsZero reports whether the date was never set.
func (d Date) IsZero() bool { return d.Year == 0 && d.Month == 0 && d.Day == 0 }

// StartOfDay returns the first instant of this date in loc.
func (d Date) StartOfDay(loc *time.Location) time.Time {
	return time.Date(d.Year, d.Month, d.Day, 0, 0, 0, 0, loc)
}

// EndOfDay returns the first instant of the following date in loc, i.e. the
// exclusive upper bound of a half-open day range.
func (d Date) EndOfDay(loc *time.Location) time.Time {
	return d.StartOfDay(loc).AddDate(0, 0, 1)
}

// Weekday reports the day of the week this date falls on. Weekday is a property
// of the civil date alone, so any location gives the same answer.
func (d Date) Weekday() time.Weekday { return d.StartOfDay(time.UTC).Weekday() }

// AddDays returns the date n days later.
func (d Date) AddDays(n int) Date {
	t := d.StartOfDay(time.UTC).AddDate(0, 0, n)
	y, m, dd := t.Date()
	return Date{Year: y, Month: m, Day: dd}
}

// Before reports whether d falls before other.
func (d Date) Before(other Date) bool {
	return d.StartOfDay(time.UTC).Before(other.StartOfDay(time.UTC))
}

// MarshalJSON emits "YYYY-MM-DD".
func (d Date) MarshalJSON() ([]byte, error) { return []byte(`"` + d.String() + `"`), nil }

// UnmarshalJSON accepts "YYYY-MM-DD".
func (d *Date) UnmarshalJSON(b []byte) error {
	s := string(b)
	if len(s) < 2 || s[0] != '"' || s[len(s)-1] != '"' {
		return fmt.Errorf("scheduling: date must be a JSON string, got %s", s)
	}
	parsed, err := ParseDate(s[1 : len(s)-1])
	if err != nil {
		return err
	}
	*d = parsed
	return nil
}

// Value implements driver.Valuer so a Date can be bound to a DATE column.
// UTC midnight is the canonical carrier: the driver only transmits the date
// part, and pinning the zone stops a server-side session timezone from shifting
// the day.
func (d Date) Value() (driver.Value, error) { return d.StartOfDay(time.UTC), nil }

// Scan implements sql.Scanner for DATE columns.
func (d *Date) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		*d = Date{}
		return nil
	case time.Time:
		// Postgres hands back a DATE as midnight; read the civil parts without
		// converting zones, because there is nothing to convert.
		y, m, day := v.Date()
		*d = Date{Year: y, Month: m, Day: day}
		return nil
	case string:
		parsed, err := ParseDate(v)
		if err != nil {
			return err
		}
		*d = parsed
		return nil
	case []byte:
		parsed, err := ParseDate(string(v))
		if err != nil {
			return err
		}
		*d = parsed
		return nil
	default:
		return fmt.Errorf("scheduling: cannot scan %T into Date", src)
	}
}
