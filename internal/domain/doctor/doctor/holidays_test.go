package doctor

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"telemed/internal/platform/httpx"
)

// recordingUpstream is a stand-in for scheduling-service's
// POST /api/v1/doctors/me/holidays.
type recordingUpstream struct {
	mu       sync.Mutex
	requests []holidayPostBody
	auth     []string
	paths    []string

	status int
	body   any
}

func (u *recordingUpstream) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body holidayPostBody
		_ = json.Unmarshal(raw, &body)

		u.mu.Lock()
		u.requests = append(u.requests, body)
		u.auth = append(u.auth, r.Header.Get("Authorization"))
		u.paths = append(u.paths, r.URL.Path)
		status, payload := u.status, u.body
		u.mu.Unlock()

		if status == 0 {
			status = http.StatusCreated
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if payload != nil {
			_ = json.NewEncoder(w).Encode(payload)
		}
	}
}

func (u *recordingUpstream) count() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.requests)
}

func newUpstream(t *testing.T, u *recordingUpstream) *SchedulingHolidayClient {
	t.Helper()
	srv := httptest.NewServer(u.handler())
	t.Cleanup(srv.Close)
	return NewSchedulingHolidayClient(srv.URL, 2*time.Second)
}

// TestForwardsToTheOwningService covers the happy path, including the two
// things that make the forward safe: it hits the doctor-scoped endpoint, and it
// carries the CALLER's token rather than any credential of this service's.
func TestForwardsToTheOwningService(t *testing.T) {
	u := &recordingUpstream{}
	client := newUpstream(t, u)

	err := client.RegisterHolidays(context.Background(), "Bearer doctor-token", []HolidayRequest{
		{Date: "2026-04-13", Reason: "Sinhala and Tamil New Year"},
		{Date: "2026-04-14", Reason: "Sinhala and Tamil New Year", CancelBooked: true},
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	if u.count() != 2 {
		t.Fatalf("posted %d holidays, want 2", u.count())
	}
	if got := u.paths[0]; got != "/api/v1/doctors/me/holidays" {
		t.Errorf("posted to %q, want /api/v1/doctors/me/holidays", got)
	}
	if got := u.auth[0]; got != "Bearer doctor-token" {
		t.Errorf("Authorization = %q; the caller's own token must be forwarded verbatim so "+
			"scheduling-service authorises the doctor itself", got)
	}
	if u.requests[0].Date != "2026-04-13" || u.requests[1].Date != "2026-04-14" {
		t.Errorf("dates = %q, %q", u.requests[0].Date, u.requests[1].Date)
	}
	if u.requests[0].CancelBooked {
		t.Error("cancel_booked defaulted to true; a doctor must opt in to cancelling their patients")
	}
	if !u.requests[1].CancelBooked {
		t.Error("cancel_booked was not forwarded when the doctor did opt in")
	}
	// The date stays a YYYY-MM-DD string end to end. Parsing it into an instant
	// here would force a timezone choice that belongs to scheduling-service.
	if len(u.requests[0].Date) != len("2026-04-13") {
		t.Errorf("date reached the wire as %q, not a civil date", u.requests[0].Date)
	}
}

// TestBookingCollisionIsPassedThroughWithItsCode checks a 409 from
// scheduling-service reaches the client with its own code and its counts.
//
// Flattening a 409 HOLIDAY_HAS_BOOKINGS into a generic conflict would leave the
// client with nothing to act on: the doctor has to be shown the affected day and
// asked whether to cancel and refund, and "try again" is not that.
func TestBookingCollisionIsPassedThroughWithItsCode(t *testing.T) {
	u := &recordingUpstream{
		status: http.StatusConflict,
		body: upstreamError{
			Code:    "HOLIDAY_HAS_BOOKINGS",
			Message: "this day already has booked appointments",
			Fields:  map[string]string{"booked_appointments": "4"},
		},
	}
	client := newUpstream(t, u)

	err := client.RegisterHolidays(context.Background(), "Bearer t",
		[]HolidayRequest{{Date: "2026-04-14"}})
	if !errors.Is(err, ErrHolidayHasBookings) {
		t.Fatalf("err = %v, want ErrHolidayHasBookings", err)
	}

	apiErr := holidayConflictError(err)
	if apiErr.Code != httpx.CodeHolidayHasBookings {
		t.Errorf("code = %q, want %q", apiErr.Code, httpx.CodeHolidayHasBookings)
	}
	if apiErr.Status() != http.StatusConflict {
		t.Errorf("status = %d, want 409", apiErr.Status())
	}
	// The count and the day travel in `fields` so a client can render
	// "4 patients are booked on 14 April" without parsing a translated
	// sentence.
	if got := apiErr.Fields["booked_appointments"]; got != "4" {
		t.Errorf("fields[booked_appointments] = %q, want 4", got)
	}
	if got := apiErr.Fields["date"]; got != "2026-04-14" {
		t.Errorf("fields[date] = %q, want 2026-04-14", got)
	}
}

// TestUpstreamOutageIsRetryable maps upstream 5xx onto a retryable error. A 5xx
// from scheduling-service means
// the leave did not register; the working hours must not be saved either, and
// the client must be told to retry rather than shown a 500.
func TestUpstreamOutageIsRetryable(t *testing.T) {
	for _, status := range []int{http.StatusInternalServerError, http.StatusBadGateway, http.StatusTooManyRequests} {
		u := &recordingUpstream{status: status}
		client := newUpstream(t, u)

		err := client.RegisterHolidays(context.Background(), "Bearer t",
			[]HolidayRequest{{Date: "2026-04-14"}})
		if !errors.Is(err, ErrHolidayUpstreamUnavailable) {
			t.Errorf("upstream %d gave %v, want ErrHolidayUpstreamUnavailable", status, err)
		}
	}
}

// TestUnreachableUpstreamIsUnavailable maps a dial failure onto the same
// retryable error as a 502. A closed port is the same
// class of failure as a 502 and must not surface as a 500.
func TestUnreachableUpstreamIsUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close() // nothing is listening now

	client := NewSchedulingHolidayClient(url, 500*time.Millisecond)
	err := client.RegisterHolidays(context.Background(), "Bearer t",
		[]HolidayRequest{{Date: "2026-04-14"}})
	if !errors.Is(err, ErrHolidayUpstreamUnavailable) {
		t.Fatalf("err = %v, want ErrHolidayUpstreamUnavailable", err)
	}
}

// TestOtherRejectionsCarryTheUpstreamMessage keeps the upstream's reason. A
// date in the past, or a token
// with no doctor id, is the doctor's problem to see -- not a generic 422.
func TestOtherRejectionsCarryTheUpstreamMessage(t *testing.T) {
	u := &recordingUpstream{
		status: http.StatusUnprocessableEntity,
		body: upstreamError{
			Code: "UNPROCESSABLE", Message: "holiday date must be today or later",
		},
	}
	client := newUpstream(t, u)

	err := client.RegisterHolidays(context.Background(), "Bearer t",
		[]HolidayRequest{{Date: "2020-01-01"}})
	if !errors.Is(err, ErrHolidayRejected) {
		t.Fatalf("err = %v, want ErrHolidayRejected", err)
	}
	if !strings.Contains(err.Error(), "today or later") {
		t.Errorf("the upstream's reason was lost: %v", err)
	}
	if !strings.Contains(err.Error(), "2020-01-01") {
		t.Errorf("the offending date was lost: %v", err)
	}
}

// TestForwardingStopsAtTheFirstFailure checks a refused day aborts the rest of
// the batch.
//
// Partial application is acceptable and is not hidden: every entry is an
// idempotent upsert on (doctor_id, date), so a client retrying the identical
// array re-applies what already landed as no-ops. Continuing past a failure
// would mean a 409 on day two silently registering days three through seven.
func TestForwardingStopsAtTheFirstFailure(t *testing.T) {
	u := &recordingUpstream{
		status: http.StatusConflict,
		body:   upstreamError{Code: "HOLIDAY_HAS_BOOKINGS", Message: "booked"},
	}
	client := newUpstream(t, u)

	err := client.RegisterHolidays(context.Background(), "Bearer t", []HolidayRequest{
		{Date: "2026-04-13"}, {Date: "2026-04-14"}, {Date: "2026-04-15"},
	})
	if err == nil {
		t.Fatal("no error from a rejecting upstream")
	}
	if u.count() != 1 {
		t.Errorf("posted %d holidays after the first was refused, want 1", u.count())
	}
}

// TestDisabledDeploymentRefusesRatherThanDrops checks an unconfigured
// deployment refuses leave instead of accepting and discarding it.
//
// A deployment with no SCHEDULING_BASE_URL cannot register leave. Accepting the
// array and discarding it is the exact failure the old 422 existed to prevent:
// a doctor told their leave was saved while patients keep booking them.
func TestDisabledDeploymentRefusesRatherThanDrops(t *testing.T) {
	client := NewSchedulingHolidayClient("", 0)
	if client.Enabled() {
		t.Fatal("a client with no base URL reports itself enabled")
	}
	err := client.RegisterHolidays(context.Background(), "Bearer t",
		[]HolidayRequest{{Date: "2026-04-14"}})
	if !errors.Is(err, ErrHolidayForwardingDisabled) {
		t.Fatalf("err = %v, want ErrHolidayForwardingDisabled", err)
	}
}

// TestServiceWithNoRegistrarRefusesLeave applies the same guarantee one layer
// up:
// the service refuses rather than proceeding to save the working hours and
// dropping the leave on the floor.
func TestServiceWithNoRegistrarRefusesLeave(t *testing.T) {
	svc := &Service{} // no holiday registrar wired

	err := svc.SetAvailabilityWithLeave(context.Background(), AvailabilityInput{
		Settings: ScheduleSettings{SlotDurationMinutes: 30},
		Leave:    []HolidayRequest{{Date: "2026-04-14"}},
	})
	if !errors.Is(err, ErrHolidayForwardingDisabled) {
		t.Fatalf("err = %v, want ErrHolidayForwardingDisabled", err)
	}
}

// countingRegistrar records whether it was called at all.
type countingRegistrar struct{ calls int }

func (c *countingRegistrar) RegisterHolidays(context.Context, string, []HolidayRequest) error {
	c.calls++
	return nil
}

// TestEmptyLeaveNeverContactsScheduling bounds the coupling this forward
// introduces.
//
// The overwhelmingly common save carries "holidays": []. If that request called
// scheduling-service, a scheduling outage would stop every doctor on the
// platform editing their working hours -- for a field none of them used.
//
// The assertion is made where the decision is, before the transaction, so it
// needs no database: an unwired pool means the commit panics or errors AFTER
// the forwarding decision has already been taken.
func TestEmptyLeaveNeverContactsScheduling(t *testing.T) {
	reg := &countingRegistrar{}
	svc := (&Service{}).WithHolidayRegistrar(reg)

	// The overlap check runs first and is enough to stop before the commit,
	// which keeps this a pure test of the forwarding decision.
	err := svc.SetAvailabilityWithLeave(context.Background(), AvailabilityInput{
		WorkingHours: []WorkingHour{
			{DayOfWeek: 1, StartTime: "09:00", EndTime: "12:00"},
			{DayOfWeek: 1, StartTime: "10:00", EndTime: "13:00"}, // overlaps
		},
		Settings: ScheduleSettings{SlotDurationMinutes: 30},
	})
	if !errors.Is(err, ErrOverlappingHours) {
		t.Fatalf("err = %v, want ErrOverlappingHours", err)
	}
	if reg.calls != 0 {
		t.Errorf("scheduling-service was contacted %d times for a save with no leave", reg.calls)
	}
}

// TestInvalidHoursAreRejectedBeforeAnythingIsForwarded checks local validation
// runs before the forward.
//
// Order is the design: validate locally, then forward, then commit. Forwarding
// first would let a request that is about to be rejected for a client-side
// mistake cancel a patient's consultation on the way out.
func TestInvalidHoursAreRejectedBeforeAnythingIsForwarded(t *testing.T) {
	reg := &countingRegistrar{}
	svc := (&Service{}).WithHolidayRegistrar(reg)

	err := svc.SetAvailabilityWithLeave(context.Background(), AvailabilityInput{
		WorkingHours: []WorkingHour{
			{DayOfWeek: 3, StartTime: "09:00", EndTime: "12:00"},
			{DayOfWeek: 3, StartTime: "11:00", EndTime: "14:00"}, // overlaps
		},
		Settings: ScheduleSettings{SlotDurationMinutes: 30},
		Leave:    []HolidayRequest{{Date: "2026-04-14", CancelBooked: true}},
	})
	if !errors.Is(err, ErrOverlappingHours) {
		t.Fatalf("err = %v, want ErrOverlappingHours", err)
	}
	if reg.calls != 0 {
		t.Fatalf("a payload with overlapping windows reached scheduling-service and could have "+
			"cancelled patients (%d calls)", reg.calls)
	}
}
