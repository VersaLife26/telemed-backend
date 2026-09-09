package scheduling_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"telemed/internal/domain/scheduling/scheduling"
	schedulingv1 "telemed/internal/pb/scheduling/v1"
	"telemed/internal/platform/middleware"
)

// TestSlotsEndpoint exercises the one route that needs no token: browsing a
// doctor's availability.
func TestSlotsEndpoint(t *testing.T) {
	pool := requireDB(t)
	resetTables(t, pool)
	ctx := context.Background()

	svc := newTestService(t, pool, scheduling.NoopLocker{}, nil)
	h := scheduling.NewHandler(svc, "")
	r := chi.NewRouter()
	r.Route("/api/v1", h.PublicRoutes)

	loc := svc.Location()
	doctorID := uuid.New()
	// 09:00 and 09:20 Colombo, tomorrow.
	tomorrow := scheduling.DateIn(time.Now(), loc).AddDays(1)
	base := tomorrow.StartOfDay(loc).Add(9 * time.Hour)
	seedPricing(t, pool, doctorID)
	seedSlot(t, pool, doctorID, base, 15*time.Minute)
	seedSlot(t, pool, doctorID, base.Add(20*time.Minute), 15*time.Minute)
	// A booked slot must not appear.
	booked := seedSlot(t, pool, doctorID, base.Add(40*time.Minute), 15*time.Minute)
	if _, err := svc.BookSlot(ctx, scheduling.BookSlotInput{SlotID: booked, PatientID: uuid.New()}); err != nil {
		t.Fatalf("book: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/doctors/"+doctorID.String()+"/slots?date="+tomorrow.String(), http.NoBody)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var envelope struct {
		Data struct {
			DoctorID uuid.UUID              `json:"doctor_id"`
			Date     string                 `json:"date"`
			Timezone string                 `json:"timezone"`
			Slots    []scheduling.SlotDTO   `json:"slots"`
			Extra    map[string]interface{} `json:"-"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode: %v -- body %s", err, rec.Body.String())
	}

	if envelope.Data.Timezone != "Asia/Colombo" {
		t.Fatalf("timezone = %s", envelope.Data.Timezone)
	}
	if envelope.Data.Date != tomorrow.String() {
		t.Fatalf("date = %s, want %s", envelope.Data.Date, tomorrow.String())
	}
	if len(envelope.Data.Slots) != 2 {
		t.Fatalf("got %d slots, want 2 (the booked one must be hidden)", len(envelope.Data.Slots))
	}
	if got := envelope.Data.Slots[0].StartAtLocal[11:16]; got != "09:00" {
		t.Fatalf("first slot renders as %s locally, want 09:00", got)
	}

	t.Run("rejects a malformed date", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet,
			"/api/v1/doctors/"+doctorID.String()+"/slots?date=14-04-2027", http.NoBody)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
		var apiErr struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &apiErr)
		if apiErr.Code != "BAD_REQUEST" {
			t.Fatalf("code = %s, want BAD_REQUEST", apiErr.Code)
		}
	})

	t.Run("rejects a malformed doctor id", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/doctors/not-a-uuid/slots", http.NoBody)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
	})

	t.Run("an unknown doctor is an empty list, not an error", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet,
			"/api/v1/doctors/"+uuid.NewString()+"/slots?date="+tomorrow.String(), http.NoBody)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
	})
}

// serviceCtx is the context middleware.UnaryServiceAuth attaches after
// verifying a service token: a principal with RoleService and a real subject.
func serviceCtx() context.Context {
	return middleware.WithPrincipal(context.Background(), middleware.Principal{
		UserID: uuid.New(),
		Roles:  []middleware.Role{middleware.RoleService},
		Issuer: "keycloak",
	})
}

// TestGRPCSurface drives the internal API after F5.
//
// The surface is authenticated, service-role-only and read-only. It calls the
// server implementation directly with a context carrying a verified principal,
// exactly as middleware.UnaryServiceAuth would have attached it: the transport
// is gRPC's problem, the translation and the authorization are ours.
func TestGRPCSurface(t *testing.T) {
	pool := requireDB(t)
	resetTables(t, pool)

	svc := newTestService(t, pool, scheduling.NoopLocker{}, nil)
	srv := scheduling.NewGRPCServer(svc)
	ctx := serviceCtx()

	doctorID, patientID := uuid.New(), uuid.New()
	slotID := seedPricedSlot(t, pool, doctorID, time.Now().Add(5*time.Hour), 15*time.Minute)

	// Booking happens on the patient's own HTTP surface; the internal API has
	// no way to name a patient any more.
	appt, err := svc.BookSlot(context.Background(), scheduling.BookSlotInput{
		SlotID:    slotID,
		PatientID: patientID,
		DoctorID:  &doctorID,
		Intake:    json.RawMessage(`{"symptoms":"fever"}`),
	})
	if err != nil {
		t.Fatalf("book: %v", err)
	}

	got, err := srv.GetAppointment(ctx, &schedulingv1.GetAppointmentRequest{
		AppointmentId: appt.ID.String(),
	})
	if err != nil {
		t.Fatalf("GetAppointment: %v", err)
	}
	if got.GetAppointment().GetSlotId() != slotID.String() {
		t.Fatalf("slot_id = %s", got.GetAppointment().GetSlotId())
	}
	if got.GetAppointment().GetStatus() != schedulingv1.AppointmentStatus_APPOINTMENT_STATUS_PENDING_PAYMENT {
		t.Fatalf("status = %v", got.GetAppointment().GetStatus())
	}
	// Intake is PHI and must not be echoed on the internal API.
	body, _ := json.Marshal(got.GetAppointment())
	if contains(string(body), "fever") {
		t.Fatalf("intake leaked into the gRPC response: %s", body)
	}

	// Bad input is InvalidArgument, never Internal.
	if _, err := srv.GetAppointment(ctx, &schedulingv1.GetAppointmentRequest{
		AppointmentId: "nope",
	}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("bad appointment id gave %v, want InvalidArgument", status.Code(err))
	}
	if _, err := srv.GetAppointment(ctx, &schedulingv1.GetAppointmentRequest{
		AppointmentId: uuid.NewString(),
	}); status.Code(err) != codes.NotFound {
		t.Fatalf("unknown appointment gave %v, want NotFound", status.Code(err))
	}
}

// TestAdminOverrides covers blocking a slot and force-cancelling a booking that
// has already started.
func TestAdminOverrides(t *testing.T) {
	pool := requireDB(t)
	ctx := context.Background()

	t.Run("block withdraws a free slot", func(t *testing.T) {
		resetTables(t, pool)
		svc := newTestService(t, pool, scheduling.NoopLocker{}, nil)
		doctorID := uuid.New()
		slotID := seedPricedSlot(t, pool, doctorID, time.Now().Add(4*time.Hour), 15*time.Minute)

		slot, err := svc.BlockSlot(ctx, slotID, scheduling.SlotBlocked, "doctor called in sick")
		if err != nil {
			t.Fatalf("block: %v", err)
		}
		if slot.Status != scheduling.SlotBlocked {
			t.Fatalf("status = %s", slot.Status)
		}
		if _, err := svc.BookSlot(ctx, scheduling.BookSlotInput{
			SlotID: slotID, PatientID: uuid.New(),
		}); err == nil {
			t.Fatal("a blocked slot was bookable")
		}
	})

	t.Run("block refuses to silently discard a booking", func(t *testing.T) {
		resetTables(t, pool)
		svc := newTestService(t, pool, scheduling.NoopLocker{}, nil)
		slotID := seedPricedSlot(t, pool, uuid.New(), time.Now().Add(4*time.Hour), 15*time.Minute)
		if _, err := svc.BookSlot(ctx, scheduling.BookSlotInput{
			SlotID: slotID, PatientID: uuid.New(),
		}); err != nil {
			t.Fatalf("book: %v", err)
		}
		if _, err := svc.BlockSlot(ctx, slotID, scheduling.SlotBlocked, "oops"); err == nil {
			t.Fatal("blocking a booked slot succeeded; the patient would never be told")
		}
	})

	t.Run("force-cancel works after the slot has started", func(t *testing.T) {
		resetTables(t, pool)
		clock := newTestClock()
		svc := newTestServiceWithClock(t, pool, clock, scheduling.NoopWaitlistQueue{})

		slotID := seedPricedSlot(t, pool, uuid.New(), clock.Now().Add(time.Hour), 15*time.Minute)
		appt, err := svc.BookSlot(ctx, scheduling.BookSlotInput{SlotID: slotID, PatientID: uuid.New()})
		if err != nil {
			t.Fatalf("book: %v", err)
		}

		clock.Advance(2 * time.Hour) // the consultation window has opened

		if _, err := svc.CancelAppointment(ctx, scheduling.CancelInput{
			AppointmentID: appt.ID, ActorID: uuid.New(), ActorRole: "patient",
		}); err == nil {
			t.Fatal("a patient cancelled an appointment that had already started")
		}

		cancelled, err := svc.CancelAppointment(ctx, scheduling.CancelInput{
			AppointmentID: appt.ID, ActorID: uuid.New(), ActorRole: "admin",
			Reason: "stuck consultation", Force: true,
		})
		if err != nil {
			t.Fatalf("force cancel: %v", err)
		}
		if *cancelled.RefundPolicy != scheduling.RefundFull {
			t.Fatalf("refund policy = %s, want FULL for an admin cancellation", *cancelled.RefundPolicy)
		}
	})
}

// TestListings covers the paginated read paths and the holiday override.
func TestListings(t *testing.T) {
	pool := requireDB(t)
	resetTables(t, pool)
	ctx := context.Background()

	svc := newTestService(t, pool, scheduling.NoopLocker{}, nil)
	loc := svc.Location()
	doctorID, patientID := uuid.New(), uuid.New()

	base := time.Now().Add(3 * time.Hour)
	for i := 0; i < 3; i++ {
		slotID := seedPricedSlot(t, pool, doctorID, base.Add(time.Duration(i)*time.Hour), 15*time.Minute)
		if _, err := svc.BookSlot(ctx, scheduling.BookSlotInput{SlotID: slotID, PatientID: patientID}); err != nil {
			t.Fatalf("book %d: %v", i, err)
		}
	}
	// A different patient's booking must never appear in the first patient's list.
	other := seedPricedSlot(t, pool, doctorID, base.Add(10*time.Hour), 15*time.Minute)
	if _, err := svc.BookSlot(ctx, scheduling.BookSlotInput{SlotID: other, PatientID: uuid.New()}); err != nil {
		t.Fatalf("book other: %v", err)
	}

	t.Run("a patient sees only their own", func(t *testing.T) {
		list, total, err := svc.ListMyAppointments(ctx, patientID, "patient", nil, 20, 0)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if total != 3 || len(list) != 3 {
			t.Fatalf("total=%d len=%d, want 3/3", total, len(list))
		}
		for _, a := range list {
			if a.PatientID != patientID {
				t.Fatalf("somebody else's appointment leaked into the list: %s", a.ID)
			}
		}
		// Newest consultation first.
		for i := 1; i < len(list); i++ {
			if list[i].SlotStartAt.After(list[i-1].SlotStartAt) {
				t.Fatal("listing is not ordered by slot start descending")
			}
		}
	})

	t.Run("a doctor sees every patient's", func(t *testing.T) {
		_, total, err := svc.ListMyAppointments(ctx, doctorID, "doctor", nil, 20, 0)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if total != 4 {
			t.Fatalf("doctor total = %d, want 4", total)
		}
	})

	t.Run("pagination", func(t *testing.T) {
		page1, total, err := svc.ListMyAppointments(ctx, patientID, "patient", nil, 2, 0)
		if err != nil {
			t.Fatalf("page 1: %v", err)
		}
		page2, _, err := svc.ListMyAppointments(ctx, patientID, "patient", nil, 2, 2)
		if err != nil {
			t.Fatalf("page 2: %v", err)
		}
		if total != 3 || len(page1) != 2 || len(page2) != 1 {
			t.Fatalf("total=%d page1=%d page2=%d, want 3/2/1", total, len(page1), len(page2))
		}
		if page1[0].ID == page2[0].ID {
			t.Fatal("pages overlap")
		}
	})

	t.Run("status filter", func(t *testing.T) {
		confirmed := scheduling.AppointmentConfirmed
		_, total, err := svc.ListMyAppointments(ctx, patientID, "patient", &confirmed, 20, 0)
		if err != nil {
			t.Fatalf("filtered list: %v", err)
		}
		if total != 0 {
			t.Fatalf("confirmed total = %d, want 0 (nothing is paid yet)", total)
		}
	})

	t.Run("an unknown role may list nothing", func(t *testing.T) {
		_, _, err := svc.ListMyAppointments(ctx, patientID, "stranger", nil, 20, 0)
		if !errors.Is(err, scheduling.ErrForbidden) {
			t.Fatalf("err = %v, want ErrForbidden", err)
		}
	})

	t.Run("waitlist listing", func(t *testing.T) {
		date := scheduling.DateIn(time.Now(), loc).AddDays(2)
		if _, err := svc.JoinWaitlist(ctx, patientID, doctorID, date); err != nil {
			t.Fatalf("join: %v", err)
		}
		entries, err := svc.ListWaitlist(ctx, patientID)
		if err != nil {
			t.Fatalf("list waitlist: %v", err)
		}
		if len(entries) != 1 || entries[0].PreferredDate != date {
			t.Fatalf("waitlist = %+v, want one entry for %s", entries, date)
		}
	})
}

// TestSetHolidayStopsGeneration: the admin holiday override must actually keep
// slots off the calendar.
func TestSetHolidayStopsGeneration(t *testing.T) {
	pool := requireDB(t)
	resetTables(t, pool)
	ctx := context.Background()

	svc := newTestService(t, pool, scheduling.NoopLocker{}, nil)
	loc := svc.Location()
	doctorID := uuid.New()

	var wh []scheduling.WorkingHour
	for d := time.Sunday; d <= time.Saturday; d++ {
		wh = append(wh, scheduling.WorkingHour{DoctorID: doctorID, DayOfWeek: d,
			StartMinute: 9 * 60, EndMinute: 10 * 60, IsAvailable: true})
	}
	seedDoctorSchedule(t, pool, doctorID, "Asia/Colombo", 30, 0, 100, 5, wh)

	poya := scheduling.DateIn(time.Now(), loc).AddDays(2)
	if _, err := svc.SetHoliday(ctx, nil, poya, "Nikini Poya", false, false); err != nil {
		t.Fatalf("set holiday: %v", err)
	}
	if _, err := svc.GenerateForDoctor(ctx, doctorID); err != nil {
		t.Fatalf("generate: %v", err)
	}

	slots, err := svc.ListSlots(ctx, doctorID, poya)
	if err != nil {
		t.Fatalf("list slots: %v", err)
	}
	if len(slots) != 0 {
		t.Fatalf("%d slots generated on a platform-wide holiday", len(slots))
	}

	// Re-recording the same holiday must not fail.
	if _, err := svc.SetHoliday(ctx, nil, poya, "Nikini Poya (corrected)", false, false); err != nil {
		t.Fatalf("re-set holiday: %v", err)
	}
}

// TestGenerateAllCoversEveryActiveDoctor, and survives one broken doctor.
func TestGenerateAllCoversEveryActiveDoctor(t *testing.T) {
	pool := requireDB(t)
	resetTables(t, pool)
	ctx := context.Background()

	svc := newTestService(t, pool, scheduling.NoopLocker{}, nil)
	var wh []scheduling.WorkingHour
	good := []uuid.UUID{uuid.New(), uuid.New()}
	for _, id := range good {
		wh = wh[:0]
		for d := time.Sunday; d <= time.Saturday; d++ {
			wh = append(wh, scheduling.WorkingHour{DoctorID: id, DayOfWeek: d,
				StartMinute: 9 * 60, EndMinute: 10 * 60, IsAvailable: true})
		}
		seedDoctorSchedule(t, pool, id, "Asia/Colombo", 30, 0, 100, 3, wh)
	}
	// A doctor who is configured but published no working hours. One bad row
	// must not cost the others their next thirty days.
	broken := uuid.New()
	seedDoctorSchedule(t, pool, broken, "Asia/Colombo", 30, 0, 100, 3, nil)

	doctors, inserted, err := svc.GenerateAll(ctx)
	if err != nil {
		t.Fatalf("generate all: %v", err)
	}
	if doctors != len(good) {
		t.Fatalf("generated for %d doctors, want %d", doctors, len(good))
	}
	if inserted == 0 {
		t.Fatal("generated no slots")
	}
	for _, id := range good {
		var n int
		if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM slots WHERE doctor_id = $1`, id).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		if n == 0 {
			t.Fatalf("doctor %s got no slots", id)
		}
	}
}
