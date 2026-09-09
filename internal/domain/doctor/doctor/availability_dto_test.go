package doctor

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"telemed/internal/platform/httpx"
)

// doctorAppAvailabilityBody is the EXACT body telemed-doctor-app's
// AvailabilityRepository._push sends, transcribed field for field from
// lib/features/availability/data/availability_repository.dart.
//
// It is pinned as a literal rather than assembled from the DTO, because a test
// that builds the request from the struct under test proves only that the
// struct equals itself. This is the client's bytes.
//
// httpx.DecodeJSON sets DisallowUnknownFields, so a field the DTO does not
// declare does not degrade -- it rejects the whole save with a 400. The DTO
// accepted only working_hours, and the availability editor is the screen every
// booking depends on: no availability, no slots; no slots, no revenue.
const doctorAppAvailabilityBody = `{
  "working_hours": [
    {"day_of_week": 1, "start_time": "09:00", "end_time": "12:00", "is_available": true},
    {"day_of_week": 1, "start_time": "14:00", "end_time": "17:00", "is_available": true},
    {"day_of_week": 6, "start_time": "09:00", "end_time": "11:00", "is_available": false}
  ],
  "slot_duration_minutes": 20,
  "buffer_minutes": 10,
  "max_per_day": 24,
  "holidays": []
}`

func decodeAvailability(t *testing.T, body string) (setAvailabilityRequest, error) {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPut,
		"/api/v1/doctors/me/availability", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	var out setAvailabilityRequest
	err := httpx.DecodeJSON(rec, req, &out)
	return out, err
}

// TestSetAvailabilityRequest_AcceptsWhatTheDoctorAppSends is the regression
// test for the 400 that made the availability editor unusable.
func TestSetAvailabilityRequest_AcceptsWhatTheDoctorAppSends(t *testing.T) {
	got, err := decodeAvailability(t, doctorAppAvailabilityBody)
	if err != nil {
		t.Fatalf("the doctor app's own request body was rejected: %v", err)
	}

	if len(got.WorkingHours) != 3 {
		t.Errorf("working_hours = %d entries, want 3", len(got.WorkingHours))
	}
	if got.SlotDurationMinutes != 20 {
		t.Errorf("slot_duration_minutes = %d, want 20", got.SlotDurationMinutes)
	}
	if got.BufferMinutes == nil || *got.BufferMinutes != 10 {
		t.Errorf("buffer_minutes = %v, want 10", got.BufferMinutes)
	}
	if got.MaxPerDay != 24 {
		t.Errorf("max_per_day = %d, want 24", got.MaxPerDay)
	}
	if len(got.Holidays) != 0 {
		t.Errorf("holidays = %v, want empty", got.Holidays)
	}
}

// TestSetAvailabilityRequest_BufferMinutesKeepsItsThreeStates is the rule the
// whole pointer exists for. Losing it is silent: the doctor asks for
// back-to-back consultations, gets the default gap forever, and nothing
// anywhere reports a problem.
func TestSetAvailabilityRequest_BufferMinutesKeepsItsThreeStates(t *testing.T) {
	tests := []struct {
		name   string
		body   string
		wantNi bool
		want   int
	}{
		{
			name:   "absent means no preference expressed",
			body:   `{"working_hours": []}`,
			wantNi: true,
		},
		{
			name:   "explicit null means no preference expressed",
			body:   `{"working_hours": [], "buffer_minutes": null}`,
			wantNi: true,
		},
		{
			name: "zero means back-to-back, which is a preference",
			body: `{"working_hours": [], "buffer_minutes": 0}`,
			want: 0,
		},
		{
			name: "a positive value is itself",
			body: `{"working_hours": [], "buffer_minutes": 15}`,
			want: 15,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := decodeAvailability(t, tt.body)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			switch {
			case tt.wantNi && got.BufferMinutes != nil:
				t.Errorf("buffer_minutes = %d, want nil: an absent preference must not "+
					"become a transmitted zero", *got.BufferMinutes)
			case !tt.wantNi && got.BufferMinutes == nil:
				t.Error("buffer_minutes = nil, want a value")
			case !tt.wantNi && *got.BufferMinutes != tt.want:
				t.Errorf("buffer_minutes = %d, want %d", *got.BufferMinutes, tt.want)
			}
		})
	}
}

func TestSetAvailabilityRequest_RejectsOutOfRangeSettings(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"a 2-minute consultation is a typo", `{"working_hours": [], "slot_duration_minutes": 2}`},
		{"a 10-hour consultation is a typo", `{"working_hours": [], "slot_duration_minutes": 600}`},
		{"a negative buffer is meaningless", `{"working_hours": [], "buffer_minutes": -5}`},
		{"a 4-hour buffer is a typo", `{"working_hours": [], "buffer_minutes": 400}`},
		{"a negative daily cap is meaningless", `{"working_hours": [], "max_per_day": -1}`},
		{"200 appointments a day is a typo", `{"working_hours": [], "max_per_day": 200}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := decodeAvailability(t, tt.body); err == nil {
				t.Error("expected the request to be rejected")
			}
		})
	}
}

// TestSetAvailabilityRequest_StillRejectsGenuinelyUnknownFields: widening the
// DTO must not turn into "accept anything". DisallowUnknownFields is what
// catches a client typo before it becomes a setting silently ignored for a
// year.
func TestSetAvailabilityRequest_StillRejectsGenuinelyUnknownFields(t *testing.T) {
	if _, err := decodeAvailability(t, `{"working_hours": [], "slot_duraton_minutes": 20}`); err == nil {
		t.Error("a misspelled field must be rejected, not silently dropped")
	}
}

// TestScheduleSettings_Validate covers the service-layer bounds, which are the
// same numbers the migration's CHECK constraints enforce.
func TestScheduleSettings_Validate(t *testing.T) {
	base := func() ScheduleSettings { return DefaultScheduleSettings(uuid.New()) }
	ptr := func(i int) *int { return &i }

	tests := []struct {
		name    string
		mutate  func(*ScheduleSettings)
		wantErr bool
	}{
		{"defaults are valid", func(*ScheduleSettings) {}, false},
		{"nil buffer is valid", func(s *ScheduleSettings) { s.BufferMinutes = nil }, false},
		{"zero buffer is valid", func(s *ScheduleSettings) { s.BufferMinutes = ptr(0) }, false},
		{"negative buffer is not", func(s *ScheduleSettings) { s.BufferMinutes = ptr(-1) }, true},
		{"huge buffer is not", func(s *ScheduleSettings) { s.BufferMinutes = ptr(121) }, true},
		{"slot below the floor", func(s *ScheduleSettings) { s.SlotDurationMinutes = 4 }, true},
		{"slot above the ceiling", func(s *ScheduleSettings) { s.SlotDurationMinutes = 241 }, true},
		{"zero max_per_day means no cap", func(s *ScheduleSettings) { s.MaxPerDay = 0 }, false},
		{"negative max_per_day", func(s *ScheduleSettings) { s.MaxPerDay = -1 }, true},
		{"empty timezone", func(s *ScheduleSettings) { s.Timezone = "" }, true},
		{"unknown timezone", func(s *ScheduleSettings) { s.Timezone = "Mars/Olympus" }, true},
		{"a real timezone other than the default", func(s *ScheduleSettings) { s.Timezone = "Asia/Kolkata" }, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := base()
			tt.mutate(&s)
			err := s.Validate()
			if tt.wantErr && err == nil {
				t.Error("expected an error")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

// TestDefaultScheduleSettings_LeavesTheBufferUnset. The default must not
// invent a preference the doctor never expressed -- that is the producer-side
// half of the same nullability rule.
func TestDefaultScheduleSettings_LeavesTheBufferUnset(t *testing.T) {
	s := DefaultScheduleSettings(uuid.New())
	if s.BufferMinutes != nil {
		t.Errorf("BufferMinutes = %d, want nil", *s.BufferMinutes)
	}
	if s.SlotDurationMinutes != DefaultSlotDurationMinutes {
		t.Errorf("SlotDurationMinutes = %d, want %d", s.SlotDurationMinutes, DefaultSlotDurationMinutes)
	}
	if s.Timezone != DefaultScheduleTimezone {
		t.Errorf("Timezone = %q, want %q", s.Timezone, DefaultScheduleTimezone)
	}
}

// TestUpdatedPayloadCarriesTheSlotShape is the wire half: the settings the
// editor saved must reach scheduling-service, or an availability edit changes
// the windows and leaves the slicing wrong.
func TestUpdatedPayloadCarriesTheSlotShape(t *testing.T) {
	got := updatedPayload(fixtureDoctor(), fixtureWorkingHours(), fixtureSettings(), time.Now().UTC())

	if got.SlotDurationMinutes != 20 {
		t.Errorf("slot_duration_minutes = %d, want 20", got.SlotDurationMinutes)
	}
	if got.BufferMinutes == nil {
		t.Fatal("buffer_minutes is nil, want a pointer to 0: the doctor asked for back-to-back")
	}
	if *got.BufferMinutes != 0 {
		t.Errorf("buffer_minutes = %d, want 0", *got.BufferMinutes)
	}
	if got.MaxPerDay != 12 {
		t.Errorf("max_per_day = %d, want 12", got.MaxPerDay)
	}
	if got.Timezone != DefaultScheduleTimezone {
		t.Errorf("timezone = %q, want %q", got.Timezone, DefaultScheduleTimezone)
	}
	if len(got.WorkingHours) != 5 {
		t.Errorf("working_hours = %d entries, want 5", len(got.WorkingHours))
	}
}

// TestApprovedPayloadOmitsAnUnsetBuffer is the pair to the golden payload's
// `"buffer_minutes":0`. Absent and zero must render differently on the wire,
// because scheduling-service distinguishes them.
func TestApprovedPayloadOmitsAnUnsetBuffer(t *testing.T) {
	set := fixtureSettings()
	set.BufferMinutes = nil

	raw, err := json.Marshal(approvedPayload(fixtureDoctor(), fixtureWorkingHours(), set, time.Now().UTC()))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if bytes.Contains(raw, []byte("buffer_minutes")) {
		t.Errorf("an unset buffer must be absent from the wire, not sent as a value: %s", raw)
	}

	set.BufferMinutes = new(int) // pointer to 0
	raw, err = json.Marshal(approvedPayload(fixtureDoctor(), fixtureWorkingHours(), set, time.Now().UTC()))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !bytes.Contains(raw, []byte(`"buffer_minutes":0`)) {
		t.Errorf("a deliberate zero buffer must be present on the wire: %s", raw)
	}
}

// TestHolidaysParseForForwarding pins the exact shape the doctor app sends.
//
// This test used to be TestHolidaysAreRefusedNotDropped, because there was
// nowhere on the platform for a doctor's leave to go: scheduling-service owned
// the holidays table and the only route in was POST /api/v1/admin/holidays,
// behind the admin IP allowlist. Refusing loudly beat accepting and discarding.
//
// scheduling-service now exposes POST/GET/DELETE /api/v1/doctors/me/holidays
// and this service forwards to it, so the array has to DECODE correctly --
// httpx.DecodeJSON sets DisallowUnknownFields, and an unrecognised key rejects
// the whole save with a 400 on the screen every booking depends on.
func TestHolidaysParseForForwarding(t *testing.T) {
	// Exactly what telemed-doctor-app's availability repository builds: a
	// zero-padded civil date and a reason that is "" when the doctor tapped
	// Skip. cancel_booked is NOT sent by the app today, so it must be optional.
	body := `{"working_hours": [], "holidays": [{"date": "2026-04-14", "reason": "Sinhala and Tamil New Year"}, {"date": "2026-04-15", "reason": ""}]}`
	got, err := decodeAvailability(t, body)
	if err != nil {
		t.Fatalf("the availability editor's payload must parse: %v", err)
	}
	if len(got.Holidays) != 2 {
		t.Fatalf("holidays = %d entries, want 2", len(got.Holidays))
	}
	if got.Holidays[0].Date != "2026-04-14" {
		t.Errorf("date = %q", got.Holidays[0].Date)
	}
	if got.Holidays[1].Reason != "" {
		t.Errorf("an empty reason must survive as empty, not be rejected: %q", got.Holidays[1].Reason)
	}
	if got.Holidays[0].CancelBooked {
		t.Error("cancel_booked defaulted to true; cancelling a doctor's patients must be opt-in")
	}

	// And the flag is accepted when a client does send it.
	withConsent, err := decodeAvailability(t,
		`{"working_hours": [], "holidays": [{"date": "2026-04-14", "reason": "leave", "cancel_booked": true}]}`)
	if err != nil {
		t.Fatalf("cancel_booked must be accepted: %v", err)
	}
	if !withConsent.Holidays[0].CancelBooked {
		t.Error("cancel_booked did not decode")
	}
}
