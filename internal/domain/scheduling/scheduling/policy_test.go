package scheduling_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"telemed/internal/domain/scheduling/scheduling"
	"telemed/internal/platform/httpx"
)

// These tests need no database. They pin the policy decisions that the rest of
// the platform reads off events, so they are the ones a reviewer should read
// first when asking "what does this service actually promise?".

func TestRefundPolicyFor(t *testing.T) {
	now := time.Date(2027, time.April, 14, 9, 0, 0, 0, time.UTC)

	cases := []struct {
		name            string
		start           time.Time
		doctorInitiated bool
		want            scheduling.RefundPolicy
		wantPercent     int
	}{
		{"patient, a day ahead", now.Add(24 * time.Hour), false, scheduling.RefundFull, 100},
		{"patient, exactly on the boundary", now.Add(2 * time.Hour), false, scheduling.RefundFull, 100},
		{"patient, a second inside the boundary", now.Add(2*time.Hour - time.Second), false, scheduling.RefundPartial, 50},
		{"patient, ten minutes before", now.Add(10 * time.Minute), false, scheduling.RefundPartial, 50},
		{"doctor, ten minutes before", now.Add(10 * time.Minute), true, scheduling.RefundFull, 100},
		{"doctor, a day ahead", now.Add(24 * time.Hour), true, scheduling.RefundFull, 100},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := scheduling.RefundPolicyFor(tc.start, now, tc.doctorInitiated)
			if got != tc.want {
				t.Fatalf("policy = %s, want %s", got, tc.want)
			}
			if pct := got.Percent(); pct != tc.wantPercent {
				t.Fatalf("percent = %d, want %d", pct, tc.wantPercent)
			}
		})
	}

	if scheduling.RefundNone.Percent() != 0 {
		t.Fatal("a no-show must refund nothing")
	}
}

func TestRequiresPrepayment(t *testing.T) {
	cases := []struct {
		name  string
		total int
		noS   int
		want  bool
	}{
		{"brand new patient", 0, 0, false},
		{"one no-show on a first booking is not a pattern", 1, 1, false},
		{"two of two, still under the minimum history", 2, 2, false},
		{"one of three is 33%, over the threshold", 3, 1, true},
		{"one of four is 25%, under", 4, 1, false},
		{"two of five is 40%", 5, 2, true},
		{"three of ten is exactly 30%, not over", 10, 3, false},
		{"four of ten is 40%", 10, 4, true},
		{"a reformed patient falls back out", 100, 5, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stats := scheduling.NoShowStats{
				PatientID: uuid.New(), TotalAppointments: tc.total, NoShowCount: tc.noS,
			}
			if got := scheduling.RequiresPrepayment(stats); got != tc.want {
				t.Fatalf("RequiresPrepayment(%d/%d) = %v (rate %.3f), want %v",
					tc.noS, tc.total, got, stats.NoShowRate(), tc.want)
			}
		})
	}
}

func TestEffectiveMaxPerDay(t *testing.T) {
	cases := []struct{ maxPerDay, percent, want int }{
		{24, 0, 24},
		{24, 10, 27}, // ceil(2.4) = 3
		{10, 10, 11}, // ceil(1.0) = 1
		{5, 10, 6},   // ceil(0.5) = 1 -- never rounds an allowance down to nothing
		{1, 10, 2},   //
		{100, 10, 110},
	}
	for _, tc := range cases {
		s := scheduling.ScheduleSettings{MaxPerDay: tc.maxPerDay, OverbookingPercent: tc.percent}
		if got := s.EffectiveMaxPerDay(); got != tc.want {
			t.Errorf("max=%d percent=%d -> %d, want %d", tc.maxPerDay, tc.percent, got, tc.want)
		}
	}
}

func TestSlotBookable(t *testing.T) {
	now := time.Date(2027, time.April, 14, 9, 0, 0, 0, time.UTC)
	me, other := uuid.New(), uuid.New()
	soon, past := now.Add(time.Minute), now.Add(-time.Minute)

	cases := []struct {
		name string
		slot scheduling.Slot
		who  uuid.UUID
		want bool
	}{
		{"plain available", scheduling.Slot{Status: scheduling.SlotAvailable}, me, true},
		{"booked", scheduling.Slot{Status: scheduling.SlotBooked}, me, false},
		{"cancelled", scheduling.Slot{Status: scheduling.SlotCancelled}, me, false},
		{"blocked with no reservation", scheduling.Slot{Status: scheduling.SlotBlocked}, me, false},
		{
			"reserved for me",
			scheduling.Slot{Status: scheduling.SlotBlocked, ReservedFor: &me, ReservedUntil: &soon},
			me, true,
		},
		{
			"reserved for someone else",
			scheduling.Slot{Status: scheduling.SlotBlocked, ReservedFor: &other, ReservedUntil: &soon},
			me, false,
		},
		{
			"reservation has lapsed but the sweeper has not run",
			scheduling.Slot{Status: scheduling.SlotBlocked, ReservedFor: &other, ReservedUntil: &past},
			me, false, // still BLOCKED: the sweeper, not the reader, decides
		},
		{
			"available with a stale reservation stamp",
			scheduling.Slot{Status: scheduling.SlotAvailable, ReservedFor: &other, ReservedUntil: &past},
			me, true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.slot.Bookable(tc.who, now); got != tc.want {
				t.Fatalf("Bookable = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestAppointmentStatusIsLive(t *testing.T) {
	// This must mirror uq_appointments_slot_live exactly. If the two ever
	// disagree, either a cancelled slot becomes unbookable forever or two live
	// appointments can share one slot.
	live := []scheduling.AppointmentStatus{
		scheduling.AppointmentPendingPayment, scheduling.AppointmentConfirmed,
		scheduling.AppointmentCompleted, scheduling.AppointmentNoShow,
	}
	for _, s := range live {
		if !s.IsLive() {
			t.Errorf("%s should be live", s)
		}
	}
	if scheduling.AppointmentCancelled.IsLive() {
		t.Error("cancelled must not be live, or the slot can never be rebooked")
	}
}

func TestDate(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Colombo")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}

	d, err := scheduling.ParseDate("2027-04-14")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if d.String() != "2027-04-14" {
		t.Fatalf("String = %s", d.String())
	}
	if d.Weekday() != time.Wednesday {
		t.Fatalf("2027-04-14 is a %s, want Wednesday", d.Weekday())
	}

	// A Colombo day starts at 18:30 UTC the evening before. Getting this wrong
	// is how "today's slots" ends up showing yesterday evening's.
	start := d.StartOfDay(loc)
	if got := start.UTC(); !got.Equal(time.Date(2027, time.April, 13, 18, 30, 0, 0, time.UTC)) {
		t.Fatalf("StartOfDay in UTC = %s, want 2027-04-13T18:30:00Z", got.Format(time.RFC3339))
	}
	if got := d.EndOfDay(loc).Sub(start); got != 24*time.Hour {
		t.Fatalf("day length = %v, want 24h in a DST-free zone", got)
	}

	// The same instant is a different civil date in different zones -- which is
	// exactly why Date exists rather than a time.Time.
	instant := time.Date(2027, time.April, 14, 2, 0, 0, 0, time.UTC)
	if got := scheduling.DateIn(instant, loc); got.String() != "2027-04-14" {
		t.Fatalf("02:00 UTC is %s in Colombo, want 2027-04-14", got)
	}
	la, err := time.LoadLocation("America/Los_Angeles")
	if err == nil {
		if got := scheduling.DateIn(instant, la); got.String() != "2027-04-13" {
			t.Fatalf("02:00 UTC is %s in Los Angeles, want 2027-04-13", got)
		}
	}

	if got := d.AddDays(20).String(); got != "2027-05-04" {
		t.Fatalf("AddDays across a month boundary = %s, want 2027-05-04", got)
	}
	if !d.Before(d.AddDays(1)) || d.Before(d.AddDays(-1)) {
		t.Fatal("Before is wrong")
	}

	// JSON round trip: the mobile clients send and receive YYYY-MM-DD.
	raw, err := json.Marshal(struct {
		D scheduling.Date `json:"d"`
	}{d})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(raw) != `{"d":"2027-04-14"}` {
		t.Fatalf("marshalled to %s", raw)
	}
	var back struct {
		D scheduling.Date `json:"d"`
	}
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.D != d {
		t.Fatalf("round trip gave %v, want %v", back.D, d)
	}

	if _, err := scheduling.ParseDate("14/04/2027"); err == nil {
		t.Fatal("a non-ISO date should be rejected")
	}
}

// TestAPIErrorMapping pins the HTTP contract the mobile clients switch on.
func TestAPIErrorMapping(t *testing.T) {
	cases := []struct {
		err        error
		wantStatus int
		wantCode   httpx.ErrorCode
	}{
		{scheduling.ErrSlotUnavailable, http.StatusConflict, httpx.CodeSlotUnavailable},
		{scheduling.ErrVersionConflict, http.StatusConflict, httpx.CodeSlotUnavailable},
		{scheduling.ErrSlotReserved, http.StatusConflict, httpx.CodeSlotUnavailable},
		{scheduling.ErrSlotLocked, http.StatusConflict, httpx.CodeSlotLocked},
		{scheduling.ErrSlotNotFound, http.StatusNotFound, httpx.CodeNotFound},
		{scheduling.ErrAppointmentNotFound, http.StatusNotFound, httpx.CodeNotFound},
		{scheduling.ErrSlotInPast, http.StatusUnprocessableEntity, httpx.CodeUnprocessable},
		{scheduling.ErrDuplicateBooking, http.StatusConflict, httpx.CodeConflict},
		{scheduling.ErrWaitlistDuplicate, http.StatusConflict, httpx.CodeConflict},
		{scheduling.ErrForbidden, http.StatusForbidden, httpx.CodeForbidden},
		{errors.New("some raw database detail"), http.StatusInternalServerError, httpx.CodeInternal},
	}

	for _, tc := range cases {
		got := scheduling.APIError(tc.err)
		var apiErr *httpx.APIError
		if !errors.As(got, &apiErr) {
			t.Fatalf("%v did not map to an APIError", tc.err)
		}
		if apiErr.Status() != tc.wantStatus || apiErr.Code != tc.wantCode {
			t.Errorf("%v -> %d %s, want %d %s", tc.err, apiErr.Status(), apiErr.Code, tc.wantStatus, tc.wantCode)
		}
	}

	if scheduling.APIError(nil) != nil {
		t.Fatal("APIError(nil) must be nil")
	}

	// A raw error must never leak its message to a client: it can carry SQL or
	// a patient identifier.
	leaky := scheduling.APIError(errors.New("pq: duplicate key for patient nimal@example.lk"))
	var apiErr *httpx.APIError
	_ = errors.As(leaky, &apiErr)
	body, _ := json.Marshal(apiErr)
	if len(body) > 0 && contains(string(body), "nimal@example.lk") {
		t.Fatalf("the internal error message reached the client envelope: %s", body)
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// TestCacheKeys pins the key shapes the runbook tells an operator to inspect.
func TestCacheKeys(t *testing.T) {
	id := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	if got := scheduling.SlotLockKey(id); got != "lock:slot:11111111-2222-3333-4444-555555555555" {
		t.Fatalf("SlotLockKey = %s", got)
	}
	d := scheduling.Date{Year: 2027, Month: time.April, Day: 14}
	want := "waitlist:11111111-2222-3333-4444-555555555555:2027-04-14"
	if got := scheduling.WaitlistQueueKey(id, d); got != want {
		t.Fatalf("WaitlistQueueKey = %s, want %s", got, want)
	}
}

// TestDTORendersLocalTime: the API hands the clients both the UTC instant and
// the Colombo wall clock, so no client has to do the conversion itself.
func TestDTORendersLocalTime(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Colombo")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	start := time.Date(2027, time.April, 14, 3, 30, 0, 0, time.UTC)
	slot := scheduling.Slot{
		ID: uuid.New(), DoctorID: uuid.New(),
		StartAt: start, EndAt: start.Add(15 * time.Minute), Status: scheduling.SlotAvailable,
	}
	d := scheduling.NewSlotDTO(slot, loc)

	if d.StartAtLocal != "2027-04-14T09:00:00+05:30" {
		t.Fatalf("start_at_local = %s, want 2027-04-14T09:00:00+05:30", d.StartAtLocal)
	}
	if d.DurationMinutes != 15 {
		t.Fatalf("duration = %d", d.DurationMinutes)
	}
	if d.StartAt.Location() != time.UTC {
		t.Fatal("start_at must serialise as UTC")
	}
}

// TestAppointmentDTOWithholdsIntake: intake is symptom data. It travels only
// when the caller is entitled to it.
func TestAppointmentDTOWithholdsIntake(t *testing.T) {
	loc := time.UTC
	appt := scheduling.Appointment{
		ID: uuid.New(), SlotStartAt: time.Now(), SlotEndAt: time.Now().Add(time.Minute),
		Intake: json.RawMessage(`{"symptoms":"chest pain"}`),
	}

	withheld := scheduling.NewAppointmentDTO(appt, loc, false)
	body, _ := json.Marshal(withheld)
	if contains(string(body), "chest pain") {
		t.Fatalf("intake leaked into a listing DTO: %s", body)
	}

	shown := scheduling.NewAppointmentDTO(appt, loc, true)
	body, _ = json.Marshal(shown)
	if !contains(string(body), "chest pain") {
		t.Fatal("intake was withheld from the patient who owns it")
	}
}
