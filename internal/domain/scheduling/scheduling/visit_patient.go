package scheduling

import (
	"strings"
	"time"
)

// AgeAtVisit returns whole years between dob and visitStart in the business
// timezone, matching how patients describe age on a prescription pad.
func AgeAtVisit(dob, visitStart time.Time, loc *time.Location) int {
	if dob.IsZero() || visitStart.IsZero() {
		return 0
	}
	d := dob.In(loc)
	v := visitStart.In(loc)
	age := v.Year() - d.Year()
	birthdayThisYear := time.Date(v.Year(), d.Month(), d.Day(), 0, 0, 0, 0, loc)
	if v.Before(birthdayThisYear) {
		age--
	}
	if age < 0 {
		return 0
	}
	return age
}

// ParseVisitDOB accepts YYYY-MM-DD for booking snapshots.
func ParseVisitDOB(raw string) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	t, err := time.ParseInLocation("2006-01-02", raw, time.UTC)
	if err != nil {
		return time.Time{}, err
	}
	return t, nil
}
