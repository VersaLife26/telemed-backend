package doctor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Holiday registration lives in scheduling-service. This file is the seam.
//
// WHY DOCTOR-SERVICE FORWARDS RATHER THAN THE CLIENT CALLING DIRECTLY
// See docs/DESIGN.md for the full argument. The short form: the doctor app's
// availability editor is one screen with one Save button and one offline retry
// queue, and its holidays live in that payload. Splitting the write across two
// services at the client would mean the app has to invent two-phase failure
// handling on a screen that already has a sync queue keyed to a single body --
// and until it does, a doctor's leave silently would not save.
//
// The forward is deliberately NARROW so the coupling it introduces is bounded:
//
//   - It is ADDITIVE ONLY. Each entry is an idempotent upsert on
//     (doctor_id, date); the array is never treated as the doctor's complete
//     leave set. That matters because the doctor app's holiday list is held
//     ONLY in its local cache -- GET /doctors/me/availability has never
//     returned holidays -- so a second device's list is guaranteed stale, and
//     a full-replace forward would have that device wipe leave registered on
//     the first. Removing leave is DELETE
//     /api/v1/doctors/me/holidays/{id} against scheduling-service, and there
//     is no other way to do it.
//   - It only happens when the array is NON-EMPTY. The overwhelmingly common
//     save carries `"holidays": []`, and that request never touches
//     scheduling-service at all, so an outage there does not stop doctors
//     editing their working hours.
//   - It FAILS THE REQUEST when it fails. Reporting success for a save whose
//     leave did not register is the exact failure the previous 422 existed to
//     prevent, and swapping a loud refusal for a quiet lie would be a
//     regression dressed as a feature.

// HolidayRequest is one day of leave to register.
type HolidayRequest struct {
	// Date is YYYY-MM-DD in the business timezone, exactly as the doctor app
	// sends it. It stays a string end to end: "the 14th of April" is a civil
	// date, and parsing it into an instant here would force a timezone choice
	// that scheduling-service is the one entitled to make.
	Date   string
	Reason string
	// CancelBooked is the doctor's explicit consent to cancel and refund the
	// patients already booked that day. Absent, scheduling-service refuses the
	// day with ErrHolidayHasBookings rather than choosing for them.
	CancelBooked bool
}

// HolidayRegistrar registers a doctor's leave with the service that owns it.
//
// It is an interface for the reason every external dependency in this platform
// is one (AGENT-BRIEF §0.6): the transport is an implementation detail, a test
// substitutes a fake without a listening socket, and the day scheduling exposes
// this over gRPC only the adapter changes.
type HolidayRegistrar interface {
	// RegisterHolidays applies every entry, in order. bearer is the caller's
	// own access token, forwarded verbatim so scheduling-service authorises the
	// doctor itself.
	RegisterHolidays(ctx context.Context, bearer string, days []HolidayRequest) error
}

// Holiday-forwarding errors. handler.go maps them; nothing else interprets
// them.
var (
	// ErrHolidayUpstreamUnavailable is scheduling-service being unreachable,
	// slow, or returning a 5xx. The doctor's working hours are NOT saved when
	// this is returned, so a retry of the identical request is safe and
	// correct.
	ErrHolidayUpstreamUnavailable = errors.New("doctor: the scheduling service could not be reached to register your leave")

	// ErrHolidayHasBookings is scheduling-service refusing a day that already
	// has patients booked. It is passed through rather than flattened into a
	// generic conflict because the client has to react to it specifically: show
	// the doctor the affected day and ask whether to cancel and refund.
	ErrHolidayHasBookings = errors.New("doctor: that day already has booked appointments")

	// ErrHolidayRejected is any other 4xx from scheduling-service -- a date in
	// the past, a malformed date, a token with no doctor id. The upstream
	// message is carried alongside it.
	ErrHolidayRejected = errors.New("doctor: the scheduling service rejected this leave")

	// ErrHolidayForwardingDisabled is a deployment with no SCHEDULING_BASE_URL
	// configured. It is a 501, not a 500: nothing is broken, the capability was
	// simply not wired up, and telling an operator that plainly is worth more
	// than a stack trace.
	ErrHolidayForwardingDisabled = errors.New("doctor: holiday registration is not configured in this deployment")
)

// holidayRejection carries the upstream's own message so the doctor sees why
// their leave was refused instead of a generic conflict.
type holidayRejection struct {
	base    error
	date    string
	message string
	// bookedAppointments is echoed from the upstream's `fields` map on a
	// HOLIDAY_HAS_BOOKINGS refusal, so the client can render "4 patients are
	// booked that day" without parsing a translated sentence.
	bookedAppointments string
}

func (e *holidayRejection) Error() string {
	if e.date != "" {
		return fmt.Sprintf("%v (%s): %s", e.base, e.date, e.message)
	}
	return fmt.Sprintf("%v: %s", e.base, e.message)
}

func (e *holidayRejection) Unwrap() error { return e.base }

// SchedulingHolidayClient is the HTTP adapter onto scheduling-service.
type SchedulingHolidayClient struct {
	baseURL string
	http    *http.Client
}

// NewSchedulingHolidayClient builds the adapter. An empty baseURL yields a
// client that refuses every call with ErrHolidayForwardingDisabled rather than
// a nil pointer or a request to "": a misconfigured deployment should say so on
// the first save, not panic on it.
func NewSchedulingHolidayClient(baseURL string, timeout time.Duration) *SchedulingHolidayClient {
	if timeout <= 0 {
		// Short on purpose. This sits on a doctor's Save button, and a
		// scheduling-service that is thinking for thirty seconds is one the
		// doctor should be told about rather than watch a spinner for.
		timeout = 5 * time.Second
	}
	return &SchedulingHolidayClient{
		baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		http:    &http.Client{Timeout: timeout},
	}
}

// Enabled reports whether this deployment can forward at all.
func (c *SchedulingHolidayClient) Enabled() bool { return c.baseURL != "" }

type holidayPostBody struct {
	Date         string `json:"date"`
	Reason       string `json:"reason,omitempty"`
	CancelBooked bool   `json:"cancel_booked"`
}

// upstreamError is the platform error envelope, which scheduling-service
// returns for every 4xx.
type upstreamError struct {
	Code    string            `json:"code"`
	Message string            `json:"message"`
	Fields  map[string]string `json:"fields,omitempty"`
}

// RegisterHolidays posts each day in turn.
//
// Sequentially and not concurrently: each POST takes row locks over one day's
// slots, and firing a fortnight of leave at scheduling-service in parallel
// would have the requests contend with each other for no benefit on a payload
// that is almost always one or two days long.
//
// It stops at the first failure. Partial application is acceptable here and is
// not papered over: every entry is an idempotent upsert, so the client retrying
// the same array re-applies the days that already landed as no-ops and
// re-attempts the one that failed.
func (c *SchedulingHolidayClient) RegisterHolidays(ctx context.Context, bearer string, days []HolidayRequest) error {
	if !c.Enabled() {
		return ErrHolidayForwardingDisabled
	}
	endpoint, err := url.JoinPath(c.baseURL, "/api/v1/doctors/me/holidays")
	if err != nil {
		return fmt.Errorf("doctor: build holiday endpoint: %w", err)
	}

	for _, day := range days {
		if err := c.postOne(ctx, endpoint, bearer, day); err != nil {
			return err
		}
	}
	return nil
}

func (c *SchedulingHolidayClient) postOne(ctx context.Context, endpoint, bearer string, day HolidayRequest) error {
	// A conversion, not a field-by-field literal. The two structs are the same
	// shape on purpose -- holidayPostBody exists only to carry the JSON tags --
	// and converting means the day one of them gains a field the other lacks is
	// a COMPILE error here rather than a field that silently stops being sent.
	body, err := json.Marshal(holidayPostBody(day))
	if err != nil {
		return fmt.Errorf("doctor: marshal holiday: %w", err)
	}

	// gosec flags this as SSRF because `endpoint` is not a literal. It is
	// SCHEDULING_BASE_URL plus a constant path -- operator configuration, fixed
	// at boot, never derived from a request. There is no caller-controlled
	// input anywhere in it.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body)) //nolint:gosec // endpoint is operator config, not request input
	if err != nil {
		return fmt.Errorf("doctor: build holiday request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		// The CALLER's token, forwarded verbatim. doctor-service holds no
		// service credential for scheduling and should not: forwarding the
		// doctor's own token means scheduling authorises the doctor itself,
		// and doctor-service cannot act for anyone who has not called it.
		req.Header.Set("Authorization", bearer)
	}

	resp, err := c.http.Do(req) //nolint:gosec // same: the request URL is operator config, not request input
	if err != nil {
		return fmt.Errorf("%w: %w", ErrHolidayUpstreamUnavailable, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		// Drain so the connection is reusable rather than abandoned mid-body.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return nil
	}

	// 1 MiB is far more than an error envelope and far less than anything that
	// could exhaust memory if the upstream is misbehaving.
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var envelope upstreamError
	_ = json.Unmarshal(raw, &envelope)
	message := envelope.Message
	if message == "" {
		message = http.StatusText(resp.StatusCode)
	}

	switch {
	case resp.StatusCode >= 500, resp.StatusCode == http.StatusTooManyRequests:
		return fmt.Errorf("%w: upstream returned %d", ErrHolidayUpstreamUnavailable, resp.StatusCode)
	case envelope.Code == "HOLIDAY_HAS_BOOKINGS":
		return &holidayRejection{
			base: ErrHolidayHasBookings, date: day.Date, message: message,
			bookedAppointments: envelope.Fields["booked_appointments"],
		}
	default:
		return &holidayRejection{base: ErrHolidayRejected, date: day.Date, message: message}
	}
}

// Compile-time proof the adapter satisfies the port.
var _ HolidayRegistrar = (*SchedulingHolidayClient)(nil)
