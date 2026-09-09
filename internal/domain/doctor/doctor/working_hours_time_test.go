package doctor

import "testing"

// TestNormalizeTimeOfDay pins the fix for a total outage of the availability
// endpoint.
//
// pgx v5's binary protocol has an encode plan for "08:00:00" and none for
// "08:00". CopyFrom uses the binary protocol, so every PUT /doctors/me/availability
// returned 500 -- and because the DTO accepted both forms, nothing upstream
// rejected the short one. The knock-on was worse than the 500: no working
// hours meant doctor.approved carried no schedule, which meant zero slots, so
// an approved doctor was permanently unbookable with no error anywhere.
func TestNormalizeTimeOfDay(t *testing.T) {
	ok := map[string]string{
		"08:00":    "08:00:00", // the form the doctor app sends
		"08:00:00": "08:00:00",
		"9:05":     "09:05:00", // a doctor typing without the leading zero
		"23:59":    "23:59:00",
		"00:00":    "00:00:00",
		"17:30:45": "17:30:45",
		// Go accepts fractional seconds and truncates. Sub-second precision on
		// a working-hours boundary is meaningless, so this is right.
		"08:00:00.5": "08:00:00",
	}
	for in, want := range ok {
		got, err := normalizeTimeOfDay(in)
		if err != nil {
			t.Errorf("normalizeTimeOfDay(%q) errored: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("normalizeTimeOfDay(%q) = %q, want %q", in, got, want)
		}
	}

	// Rejected loudly at the repository boundary rather than becoming an
	// opaque "cannot find encode plan" from deep inside the driver.
	for _, bad := range []string{"", "25:00", "8", "noon", "08-00", "08:60"} {
		if got, err := normalizeTimeOfDay(bad); err == nil {
			t.Errorf("normalizeTimeOfDay(%q) = %q, want an error", bad, got)
		}
	}
}
