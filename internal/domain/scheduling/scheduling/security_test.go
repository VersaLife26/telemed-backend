package scheduling_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"telemed/internal/domain/scheduling/scheduling"
	schedulingv1 "telemed/internal/pb/scheduling/v1"
	"telemed/internal/platform/events"
	"telemed/internal/platform/middleware"
)

// Regression tests for the security review's F5, F4 and F20 against this repo.
//
// Every test here is written to FAIL against the code as it stood before the
// fix, not merely to pass against the code after it. Where that is not obvious
// from reading the test, the comment says which line reverting would flip it.

// ---------------------------------------------------------------------------
// F5 -- gRPC identity
// ---------------------------------------------------------------------------

// TestGRPCEmptyActorIsNotAnAdmin is the sharpest instance in the finding.
//
// grpc.go used to open GetAppointment with:
//
//	actorID, role := uuid.Nil, "admin"
//	if s := req.GetActorId(); s != "" { ... }
//
// so the LEAST a caller could supply -- nothing -- bought the MOST authority
// the service can grant. Combined with a server that had no interceptor at all,
// an empty message on a TCP socket read any patient's appointment.
//
// Reverting grpc.go's GetAppointment to that shape makes this test return the
// appointment instead of an error, and it fails.
func TestGRPCEmptyActorIsNotAnAdmin(t *testing.T) {
	pool := requireDB(t)
	resetTables(t, pool)

	svc := newTestService(t, pool, scheduling.NoopLocker{}, nil)
	srv := scheduling.NewGRPCServer(svc)

	doctorID, patientID := uuid.New(), uuid.New()
	slotID := seedPricedSlot(t, pool, doctorID, time.Now().Add(5*time.Hour), 15*time.Minute)
	appt, err := svc.BookSlot(context.Background(), scheduling.BookSlotInput{
		SlotID: slotID, PatientID: patientID,
	})
	if err != nil {
		t.Fatalf("book: %v", err)
	}

	// No principal on the context is what an unauthenticated socket looks like
	// once the interceptor is in place -- and what a REMOVED interceptor looks
	// like too. Either way the handler refuses on its own.
	_, err = srv.GetAppointment(context.Background(), &schedulingv1.GetAppointmentRequest{
		AppointmentId: appt.ID.String(),
	})
	if got := status.Code(err); got != codes.Unauthenticated {
		t.Fatalf("an empty actor_id with no principal gave %v, want Unauthenticated: %v", got, err)
	}
}

// TestGRPCRequestSuppliedIdentityIsIgnored pins the general rule the finding
// names: identity comes from the verified caller, never from the message.
//
// Both halves matter. A service caller who claims to be some other patient is
// still served (proving the field is inert rather than merely validated), and a
// non-service caller who claims to be an admin is refused (proving the role
// claim in the message buys nothing).
func TestGRPCRequestSuppliedIdentityIsIgnored(t *testing.T) {
	pool := requireDB(t)
	resetTables(t, pool)

	svc := newTestService(t, pool, scheduling.NoopLocker{}, nil)
	srv := scheduling.NewGRPCServer(svc)

	doctorID, patientID := uuid.New(), uuid.New()
	slotID := seedPricedSlot(t, pool, doctorID, time.Now().Add(5*time.Hour), 15*time.Minute)
	appt, err := svc.BookSlot(context.Background(), scheduling.BookSlotInput{
		SlotID: slotID, PatientID: patientID,
	})
	if err != nil {
		t.Fatalf("book: %v", err)
	}

	t.Run("actor_id and actor_role in the message are inert", func(t *testing.T) {
		// A stranger's id and a narrower role. Under the old code this was the
		// ownership check and it would 404; under the new code the fields are
		// simply not read, and the verified service caller is served.
		got, err := srv.GetAppointment(serviceCtx(), &schedulingv1.GetAppointmentRequest{
			AppointmentId: appt.ID.String(),
			ActorId:       uuid.NewString(),
			ActorRole:     "patient",
		})
		if err != nil {
			t.Fatalf("verified service read refused: %v", err)
		}
		if got.GetAppointment().GetId() != appt.ID.String() {
			t.Fatalf("id = %s, want %s", got.GetAppointment().GetId(), appt.ID)
		}
	})

	t.Run("a patient token claiming admin in the message is refused", func(t *testing.T) {
		patientCtx := middleware.WithPrincipal(context.Background(), middleware.Principal{
			UserID: uuid.New(),
			Roles:  []middleware.Role{middleware.RolePatient},
		})
		_, err := srv.GetAppointment(patientCtx, &schedulingv1.GetAppointmentRequest{
			AppointmentId: appt.ID.String(),
			ActorRole:     "admin",
		})
		if got := status.Code(err); got != codes.PermissionDenied {
			t.Fatalf("a non-service caller got %v, want PermissionDenied", got)
		}
	})

	t.Run("a service token with no subject is refused", func(t *testing.T) {
		anonCtx := middleware.WithPrincipal(context.Background(), middleware.Principal{
			Roles: []middleware.Role{middleware.RoleService},
		})
		_, err := srv.GetAppointment(anonCtx, &schedulingv1.GetAppointmentRequest{
			AppointmentId: appt.ID.String(),
		})
		if got := status.Code(err); got != codes.PermissionDenied {
			t.Fatalf("an unattributable service token got %v, want PermissionDenied", got)
		}
	})
}

// TestGRPCWriteMethodsAreWithdrawn pins the capability removal, and with it the
// exploit the finding leads on: "SchedulingService.BookSlot -- takes patient_id
// from the request. Book on anyone's behalf."
//
// A well-formed booking request from a fully authenticated service caller must
// still not create an appointment.
func TestGRPCWriteMethodsAreWithdrawn(t *testing.T) {
	pool := requireDB(t)
	resetTables(t, pool)

	svc := newTestService(t, pool, scheduling.NoopLocker{}, nil)
	srv := scheduling.NewGRPCServer(svc)
	ctx := serviceCtx()

	doctorID, victim := uuid.New(), uuid.New()
	slotID := seedPricedSlot(t, pool, doctorID, time.Now().Add(5*time.Hour), 15*time.Minute)

	_, err := srv.BookSlot(ctx, &schedulingv1.BookSlotRequest{
		SlotId:         slotID.String(),
		PatientId:      victim.String(),
		DoctorId:       doctorID.String(),
		FamilyMemberId: uuid.NewString(),
		IntakeJson:     `{"symptoms":"fabricated"}`,
	})
	if got := status.Code(err); got != codes.Unimplemented {
		t.Fatalf("BookSlot gave %v, want Unimplemented", got)
	}

	// The refusal must be real, not cosmetic: nothing was written.
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM appointments WHERE patient_id = $1`, victim).Scan(&n); err != nil {
		t.Fatalf("count appointments: %v", err)
	}
	if n != 0 {
		t.Fatalf("gRPC booked %d appointments for a patient who never asked", n)
	}

	appt, err := svc.BookSlot(context.Background(), scheduling.BookSlotInput{
		SlotID: slotID, PatientID: victim,
	})
	if err != nil {
		t.Fatalf("book: %v", err)
	}

	_, err = srv.CancelAppointment(ctx, &schedulingv1.CancelAppointmentRequest{
		AppointmentId: appt.ID.String(),
		ActorId:       uuid.NewString(),
		ActorRole:     "admin",
		Force:         true,
	})
	if got := status.Code(err); got != codes.Unimplemented {
		t.Fatalf("CancelAppointment gave %v, want Unimplemented", got)
	}

	var st string
	if err := pool.QueryRow(context.Background(),
		`SELECT status FROM appointments WHERE id = $1`, appt.ID).Scan(&st); err != nil {
		t.Fatalf("read appointment: %v", err)
	}
	if st == string(scheduling.AppointmentCancelled) {
		t.Fatal("a request-supplied actor_role of \"admin\" cancelled an appointment over gRPC")
	}
}

// TestNilActorIsNeverElevated is the structural half of F5: not "this handler
// no longer does it" but "no handler can".
//
// The service layer used to accept uuid.Nil for any role, and the two roles
// that never compare the actor -- admin, and now service -- meant an unset
// actor read anything. Deleting the guard at the top of authorizeAppointment
// makes both sub-tests return the appointment, and both fail.
func TestNilActorIsNeverElevated(t *testing.T) {
	pool := requireDB(t)
	resetTables(t, pool)
	ctx := context.Background()

	svc := newTestService(t, pool, scheduling.NoopLocker{}, nil)
	doctorID, patientID := uuid.New(), uuid.New()
	slotID := seedPricedSlot(t, pool, doctorID, time.Now().Add(5*time.Hour), 15*time.Minute)
	appt, err := svc.BookSlot(ctx, scheduling.BookSlotInput{SlotID: slotID, PatientID: patientID})
	if err != nil {
		t.Fatalf("book: %v", err)
	}

	for _, role := range []string{"admin", "service", "patient", "doctor"} {
		t.Run("read as "+role+" with no actor", func(t *testing.T) {
			if _, err := svc.GetAppointment(ctx, appt.ID, uuid.Nil, role); err == nil {
				t.Fatalf("an unset actor read an appointment as %q", role)
			}
		})
	}

	t.Run("cancel with no actor", func(t *testing.T) {
		if _, err := svc.CancelAppointment(ctx, scheduling.CancelInput{
			AppointmentID: appt.ID, ActorID: uuid.Nil, ActorRole: "admin", Force: true,
		}); err == nil {
			t.Fatal("an unset actor cancelled an appointment as an admin")
		}
	})

	t.Run("list with no actor", func(t *testing.T) {
		if _, _, err := svc.ListMyAppointments(ctx, uuid.Nil, "patient", nil, 20, 0); err == nil {
			t.Fatal("an unset actor listed appointments")
		}
	})
}

// TestUnaryServiceAuthFailsClosed exercises the copied platform interceptor as
// this repo mounts it: no token, a malformed header, and -- the one that
// matters most -- a server whose authenticator never got configured.
func TestUnaryServiceAuthFailsClosed(t *testing.T) {
	reached := false
	handler := func(ctx context.Context, req any) (any, error) {
		reached = true
		return "served", nil
	}
	info := &grpc.UnaryServerInfo{FullMethod: "/scheduling.v1.SchedulingService/GetAppointment"}

	t.Run("an unconfigured authenticator refuses every call", func(t *testing.T) {
		reached = false
		in := middleware.UnaryServiceAuth(middleware.ServiceAuthConfig{
			RequiredRole: middleware.RoleService,
		})
		_, err := in(context.Background(), nil, info, handler)
		if got := status.Code(err); got != codes.Unavailable {
			t.Fatalf("code = %v, want Unavailable", got)
		}
		if reached {
			t.Fatal("a misconfigured server served the call unauthenticated")
		}
	})

	t.Run("no metadata is unauthenticated", func(t *testing.T) {
		reached = false
		in := middleware.UnaryServiceAuth(middleware.ServiceAuthConfig{
			Authenticator: &middleware.Authenticator{},
			RequiredRole:  middleware.RoleService,
		})
		if _, err := in(context.Background(), nil, info, handler); status.Code(err) != codes.Unauthenticated {
			t.Fatalf("code = %v, want Unauthenticated", status.Code(err))
		}
		if reached {
			t.Fatal("handler reached without credentials")
		}
	})

	t.Run("a non-bearer authorization is unauthenticated", func(t *testing.T) {
		reached = false
		in := middleware.UnaryServiceAuth(middleware.ServiceAuthConfig{
			Authenticator: &middleware.Authenticator{},
			RequiredRole:  middleware.RoleService,
		})
		ctx := metadata.NewIncomingContext(context.Background(),
			metadata.Pairs("authorization", "Basic aGk6dGhlcmU="))
		if _, err := in(ctx, nil, info, handler); status.Code(err) != codes.Unauthenticated {
			t.Fatalf("code = %v, want Unauthenticated", status.Code(err))
		}
		if reached {
			t.Fatal("handler reached with a non-bearer credential")
		}
	})
}

// ---------------------------------------------------------------------------
// F4 -- the admin filter bypass
// ---------------------------------------------------------------------------

// TestAdminCannotListEveryAppointment is F4 for this repo.
//
// ListMyAppointments used to carry `case "admin":` with an empty body and a
// comment claiming the admin surface was IP-allowlisted and audited. This route
// is neither. Restoring that branch makes the first sub-test return both
// patients' rows, and it fails.
func TestAdminCannotListEveryAppointment(t *testing.T) {
	pool := requireDB(t)
	resetTables(t, pool)
	ctx := context.Background()

	svc := newTestService(t, pool, scheduling.NoopLocker{}, nil)
	docA, docB := uuid.New(), uuid.New()
	pat1, pat2 := uuid.New(), uuid.New()
	slotA := seedPricedSlot(t, pool, docA, time.Now().Add(4*time.Hour), 15*time.Minute)
	slotB := seedPricedSlot(t, pool, docB, time.Now().Add(6*time.Hour), 15*time.Minute)
	if _, err := svc.BookSlot(ctx, scheduling.BookSlotInput{SlotID: slotA, PatientID: pat1}); err != nil {
		t.Fatalf("book a: %v", err)
	}
	if _, err := svc.BookSlot(ctx, scheduling.BookSlotInput{SlotID: slotB, PatientID: pat2}); err != nil {
		t.Fatalf("book b: %v", err)
	}

	t.Run("the patient surface serves no admin listing at all", func(t *testing.T) {
		_, _, err := svc.ListMyAppointments(ctx, uuid.New(), "admin", nil, 50, 0)
		if err == nil {
			t.Fatal("an admin token listed appointments on the patient surface")
		}
	})

	t.Run("a patient still sees only their own", func(t *testing.T) {
		got, total, err := svc.ListMyAppointments(ctx, pat1, "patient", nil, 50, 0)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if total != 1 || len(got) != 1 || got[0].PatientID != pat1 {
			t.Fatalf("patient saw %d rows (total %d), want only their own", len(got), total)
		}
	})

	t.Run("the admin listing is bounded to one doctor and a window", func(t *testing.T) {
		now := time.Now()
		got, _, err := svc.ListAppointmentsForAdmin(ctx, scheduling.AdminListInput{
			DoctorID: docA, From: now, To: now.Add(24 * time.Hour), Limit: 50,
		})
		if err != nil {
			t.Fatalf("scoped list: %v", err)
		}
		for _, a := range got {
			if a.DoctorID != docA {
				t.Fatalf("scoped listing returned doctor %s", a.DoctorID)
			}
		}
		if len(got) != 1 {
			t.Fatalf("scoped listing returned %d rows, want 1", len(got))
		}
	})

	t.Run("an unbounded admin listing is refused", func(t *testing.T) {
		now := time.Now()
		cases := map[string]scheduling.AdminListInput{
			"no doctor":       {From: now, To: now.Add(time.Hour)},
			"no window":       {DoctorID: docA},
			"inverted window": {DoctorID: docA, From: now.Add(time.Hour), To: now},
			"a whole year":    {DoctorID: docA, From: now, To: now.Add(365 * 24 * time.Hour)},
		}
		for name, in := range cases {
			in.Limit = 50
			if _, _, err := svc.ListAppointmentsForAdmin(ctx, in); err == nil {
				t.Errorf("%s: an unbounded administrative listing was served", name)
			}
		}
	})
}

// TestAdminListingRefusesAPatientFilter pins the line the design draws: an
// administrator resolving a clash looks at a calendar, never at a person.
func TestAdminListingRefusesAPatientFilter(t *testing.T) {
	pool := requireDB(t)
	resetTables(t, pool)

	svc := newTestService(t, pool, scheduling.NoopLocker{}, nil)
	h := scheduling.NewHandler(svc, "")
	r := chi.NewRouter()
	r.Route("/api/v1/admin", h.AdminRoutes)

	now := time.Now().UTC()
	q := "?doctor_id=" + uuid.NewString() +
		"&from=" + now.Format(time.RFC3339) +
		"&to=" + now.Add(24*time.Hour).Format(time.RFC3339) +
		"&patient_id=" + uuid.NewString()

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/admin/appointments"+q, http.NoBody))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "patient") {
		t.Fatalf("the refusal does not say why: %s", rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// F20 -- PHI on the event bus
// ---------------------------------------------------------------------------

// TestCancellationReasonNeverLeavesTheService is F20(d).
//
// The cancellation reason is up to 500 characters of free text written by a
// patient about a medical appointment. It used to be copied onto
// events.AppointmentCancelled, which lands in outbox_events.payload forever,
// fans out to every consumer on the shared stream, and is rendered into an SMS
// body by notification-service.
//
// Restoring `Reason: in.Reason` at the enqueue site makes this test fail on the
// first assertion.
func TestCancellationReasonNeverLeavesTheService(t *testing.T) {
	pool := requireDB(t)
	resetTables(t, pool)
	ctx := context.Background()

	svc := newTestService(t, pool, scheduling.NoopLocker{}, nil)
	doctorID, patientID := uuid.New(), uuid.New()
	slotID := seedPricedSlot(t, pool, doctorID, time.Now().Add(5*time.Hour), 15*time.Minute)
	appt, err := svc.BookSlot(ctx, scheduling.BookSlotInput{SlotID: slotID, PatientID: patientID})
	if err != nil {
		t.Fatalf("book: %v", err)
	}

	const phi = "my chemotherapy was moved to Thursday and the oncologist says the counts are still too low"
	if _, err := svc.CancelAppointment(ctx, scheduling.CancelInput{
		AppointmentID: appt.ID, ActorID: patientID, ActorRole: "patient", Reason: phi,
	}); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	// Nothing on the bus, and nothing in the durable outbox row either --
	// outbox_events.payload is itself a retained copy.
	rows, err := pool.Query(ctx, `SELECT payload::text FROM outbox_events`)
	if err != nil {
		t.Fatalf("read outbox: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if strings.Contains(raw, "chemotherapy") || strings.Contains(raw, "oncologist") {
			t.Fatalf("patient free text reached the event bus: %s", raw)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}

	// And it is not lost: the parties can still read it on their own row.
	var stored string
	if err := pool.QueryRow(ctx,
		`SELECT cancellation_reason FROM appointments WHERE id = $1`, appt.ID).Scan(&stored); err != nil {
		t.Fatalf("read appointment: %v", err)
	}
	if stored != phi {
		t.Fatalf("cancellation_reason = %q, want the text the patient wrote", stored)
	}
}

// TestMachineReasonCodesStillTravel is the other half: containment that also
// discarded "payment_failed" would have broken the consumers that steer on it.
func TestMachineReasonCodesStillTravel(t *testing.T) {
	pool := requireDB(t)
	resetTables(t, pool)
	ctx := context.Background()

	svc := newTestService(t, pool, scheduling.NoopLocker{}, nil)
	doctorID, patientID := uuid.New(), uuid.New()
	slotID := seedPricedSlot(t, pool, doctorID, time.Now().Add(5*time.Hour), 15*time.Minute)
	appt, err := svc.BookSlot(ctx, scheduling.BookSlotInput{SlotID: slotID, PatientID: patientID})
	if err != nil {
		t.Fatalf("book: %v", err)
	}

	if _, err := svc.CancelAppointment(ctx, scheduling.CancelInput{
		AppointmentID: appt.ID, ActorID: patientID, ActorRole: "patient", Reason: "patient_requested",
	}); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	var raw []byte
	if err := pool.QueryRow(ctx,
		`SELECT payload FROM outbox_events WHERE subject = 'appointment.cancelled' LIMIT 1`).
		Scan(&raw); err != nil {
		t.Fatalf("read outbox: %v", err)
	}
	var env struct {
		Payload events.AppointmentCancelled `json:"payload"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Payload.Reason != "patient_requested" {
		t.Fatalf("event reason = %q, want the machine code to survive", env.Payload.Reason)
	}
}

// TestLeaveReasonStaysOffTheBus is the same containment on the other producer:
// the doctor's free-text explanation of their leave used to be concatenated
// onto the appointment.cancelled event as "doctor_on_leave: <sentence>".
//
// Restoring that concatenation at holidays.go's enqueue site fails this test.
func TestLeaveReasonStaysOffTheBus(t *testing.T) {
	pool := requireDB(t)
	resetTables(t, pool)
	ctx := context.Background()

	svc := newTestService(t, pool, scheduling.NoopLocker{}, nil)
	loc := svc.Location()
	doctorID, patientID := uuid.New(), uuid.New()
	leaveDate := scheduling.DateIn(time.Now(), loc).AddDays(3)
	start := leaveDate.StartOfDay(loc).Add(10 * time.Hour)

	slotID := seedPricedSlot(t, pool, doctorID, start, 15*time.Minute)
	if _, err := svc.BookSlot(ctx, scheduling.BookSlotInput{SlotID: slotID, PatientID: patientID}); err != nil {
		t.Fatalf("book: %v", err)
	}

	const phi = "caring for my mother after her mastectomy"
	if _, err := svc.AddHoliday(ctx, scheduling.AddHolidayInput{
		DoctorID:        &doctorID,
		Date:            leaveDate,
		Reason:          phi,
		ApplyToExisting: true,
		CancelBooked:    true,
	}); err != nil {
		t.Fatalf("add holiday: %v", err)
	}

	var raw []byte
	if err := pool.QueryRow(ctx,
		`SELECT payload FROM outbox_events WHERE subject = 'appointment.cancelled' LIMIT 1`).
		Scan(&raw); err != nil {
		t.Fatalf("read outbox: %v", err)
	}
	var env struct {
		Payload events.AppointmentCancelled `json:"payload"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Payload.Reason != scheduling.HolidayLeaveReason {
		t.Fatalf("event reason = %q, want the bare code %q",
			env.Payload.Reason, scheduling.HolidayLeaveReason)
	}
	if strings.Contains(string(raw), "mastectomy") {
		t.Fatalf("the doctor's free text reached the bus: %s", raw)
	}
}

// ---------------------------------------------------------------------------
// family_member_id -- an identity claim this service cannot verify
// ---------------------------------------------------------------------------

// TestBookingRefusesACallerSuppliedFamilyMemberID closes the half of F5 that
// the gRPC withdrawal did not reach.
//
// The review recorded that SchedulingService.BookSlot "takes patient_id and
// family_member_id from the request. Book on anyone's behalf." The gRPC method
// was withdrawn and patient_id on the HTTP path was bound to the token --
// handler.go takes it from MustPrincipal and the body cannot name a patient.
// family_member_id was left exactly as it was: read from the body at
// handler.go's bookAppointmentRequest, carried through BookSlotInput, and
// INSERTed onto the appointment row, with no ownership predicate anywhere in
// the repo. `grep -rn FamilyMember` over this service returns the DTO, the
// input struct, the model field and the INSERT -- and no lookup.
//
// family_members lives in telemed_user, so scheduling cannot check ownership
// without a cross-service read it does not have. That leaves two honest
// options and one dishonest one. The dishonest one is to keep accepting an
// unverifiable claim about WHICH PERSON a medical appointment is for, and to
// write it onto the row. Patient A supplies patient B's dependant id and the
// appointment -- the booking, the consultation it becomes, the record that
// hangs off it -- is attributed to a child A has no relationship with.
//
// So it is refused, following the precedent already set two files over: when
// this service cannot establish who is acting, it withdraws the surface rather
// than guessing (grpc.go's BookSlot and CancelAppointment both return
// Unimplemented with a sentence saying why).
//
// Reverting the guard in bookAppointment makes this test return 201 and store
// the foreign id.
func TestBookingRefusesACallerSuppliedFamilyMemberID(t *testing.T) {
	pool := requireDB(t)
	resetTables(t, pool)
	ctx := context.Background()

	svc := newTestService(t, pool, scheduling.NoopLocker{}, nil)
	h := scheduling.NewHandler(svc, "")
	r := chi.NewRouter()
	r.Route("/api/v1", func(sub chi.Router) {
		sub.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				next.ServeHTTP(w, req.WithContext(middleware.WithPrincipal(req.Context(), middleware.Principal{
					UserID: attackerID, Roles: []middleware.Role{middleware.RolePatient},
				})))
			})
		})
		h.PatientRoutes(sub)
	})

	doctorID := uuid.New()
	slotID := seedPricedSlot(t, pool, doctorID, time.Now().Add(5*time.Hour), 15*time.Minute)

	// A dependant id belonging to somebody else entirely. The attacker never
	// had to guess it -- family member ids are returned to their own owner by
	// user-service, and any leak of one is enough.
	victimsDependant := uuid.New()

	body, _ := json.Marshal(map[string]any{
		"slot_id":             slotID,
		"family_member_id":    victimsDependant,
		"visit_patient_name":  "Someone Else",
		"visit_patient_dob":   "2010-05-01",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/appointments", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code == http.StatusCreated {
		t.Fatalf("booking accepted an unverified family_member_id: status 201, body %s", rec.Body.String())
	}
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body = %s", rec.Code, rec.Body.String())
	}

	// And nothing was written. A guard that rejects the response but keeps the
	// row would be worse than no guard, because it would look fixed.
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM appointments WHERE family_member_id = $1`, victimsDependant).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("%d appointment(s) were stamped with another account's dependant id", n)
	}
}

// TestBookingWithoutAFamilyMemberIDStillWorks is the guard on the guard: the
// booking path is the most important one on the platform and the refusal above
// must be scoped to the field, not to the request.
func TestBookingWithoutAFamilyMemberIDStillWorks(t *testing.T) {
	pool := requireDB(t)
	resetTables(t, pool)

	svc := newTestService(t, pool, scheduling.NoopLocker{}, nil)
	h := scheduling.NewHandler(svc, "")
	r := chi.NewRouter()
	r.Route("/api/v1", func(sub chi.Router) {
		sub.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				next.ServeHTTP(w, req.WithContext(middleware.WithPrincipal(req.Context(), middleware.Principal{
					UserID: attackerID, Roles: []middleware.Role{middleware.RolePatient},
				})))
			})
		})
		h.PatientRoutes(sub)
	})

	doctorID := uuid.New()
	slotID := seedPricedSlot(t, pool, doctorID, time.Now().Add(6*time.Hour), 15*time.Minute)

	body, _ := json.Marshal(map[string]any{
		"slot_id":             slotID,
		"visit_patient_name":  "Attacker Self",
		"visit_patient_dob":   "1990-01-15",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/appointments", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("ordinary booking broke: status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

// attackerID is a stable id for the two tests above so the principal middleware
// and the assertions agree without threading a parameter through chi.
var attackerID = uuid.New()

// ---------------------------------------------------------------------------
// Admin roles on the patient surface -- the issuer half of F1, in this repo
// ---------------------------------------------------------------------------

// TestAdminRoleFromThePatientIssuerCannotReadAnotherPatientsAppointment closes
// the residue of the review's F1 that the gateway fix could not reach.
//
// F1's fix binds admin ROLES to the admin ISSUER, because user-service can mint
// a perfectly honest token -- correct `iss`, correct key, verifying cleanly --
// that asserts realm_access.roles = ["super_admin"]. Since user-service is the
// phone-OTP issuer and Keycloak is where SAML SSO and enforced 2FA live, that
// is a 2FA bypass rather than a role escalation.
//
// The gateway enforces it via RequireTokenIssuer, but only on rules with
// `auth: "admin"`. The platform middleware enforces it via checkAdminIssuer,
// but only inside RequireRole. And this service's appointment routes are
// neither: cmd/server/main.go mounts them with RequireAuth ALONE (no roles in
// the route table either -- appointment-get and appointment-cancel carry
// `roles: null`), and then handler.go's actor() branches on
// p.HasAnyRole(AdminRoles...) all by itself and returns the "admin" role, for
// which authorizeAppointment applies no ownership predicate at all.
//
// So a token from the OTP issuer carrying roles:["admin"] read and cancelled
// any patient's appointment, from any IP, with no allowlist, no origin check
// and no audit -- while the identical claim on /api/v1/admin/* was refused.
//
// Reverting the issuer check in actor() makes this test return the appointment.
func TestAdminRoleFromThePatientIssuerCannotReadAnotherPatientsAppointment(t *testing.T) {
	pool := requireDB(t)
	resetTables(t, pool)
	ctx := context.Background()

	const keycloak = "http://localhost:8180/realms/telemedicine"
	const userService = "telemed-user-service"

	svc := newTestService(t, pool, scheduling.NoopLocker{}, nil)
	h := scheduling.NewHandler(svc, keycloak)

	victim, doctorID := uuid.New(), uuid.New()
	slotID := seedPricedSlot(t, pool, doctorID, time.Now().Add(4*time.Hour), 15*time.Minute)
	appt, err := svc.BookSlot(ctx, scheduling.BookSlotInput{SlotID: slotID, PatientID: victim})
	if err != nil {
		t.Fatalf("book: %v", err)
	}

	route := func(p middleware.Principal) http.Handler {
		r := chi.NewRouter()
		r.Route("/api/v1", func(sub chi.Router) {
			sub.Use(func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					next.ServeHTTP(w, req.WithContext(middleware.WithPrincipal(req.Context(), p)))
				})
			})
			h.PatientRoutes(sub)
		})
		return r
	}

	for _, role := range []middleware.Role{"admin", "super_admin", "ops", "finance", "support"} {
		t.Run(string(role)+" from the OTP issuer", func(t *testing.T) {
			forged := middleware.Principal{
				UserID: uuid.New(),
				Issuer: userService, // honest token, wrong issuer for this claim
				Roles:  []middleware.Role{role},
			}

			req := httptest.NewRequest(http.MethodGet, "/api/v1/appointments/"+appt.ID.String(), http.NoBody)
			rec := httptest.NewRecorder()
			route(forged).ServeHTTP(rec, req)
			if rec.Code == http.StatusOK {
				t.Fatalf("a %q role asserted by the patient issuer read another patient's appointment: %s",
					role, rec.Body.String())
			}

			// And it must not be able to cancel it either -- cancellation
			// decides a refund.
			cancel := httptest.NewRequest(http.MethodPut,
				"/api/v1/appointments/"+appt.ID.String()+"/cancel", strings.NewReader(`{}`))
			cancel.Header.Set("Content-Type", "application/json")
			rec2 := httptest.NewRecorder()
			route(forged).ServeHTTP(rec2, cancel)
			if rec2.Code == http.StatusOK || rec2.Code == http.StatusNoContent {
				t.Fatalf("a %q role asserted by the patient issuer cancelled another patient's appointment: %s",
					role, rec2.Body.String())
			}
		})
	}

	// The legitimate path still works: the same role from Keycloak is honoured.
	t.Run("a genuine Keycloak admin is still admitted", func(t *testing.T) {
		genuine := middleware.Principal{
			UserID: uuid.New(),
			Issuer: keycloak,
			Roles:  []middleware.Role{"admin"},
		}
		req := httptest.NewRequest(http.MethodGet, "/api/v1/appointments/"+appt.ID.String(), http.NoBody)
		rec := httptest.NewRecorder()
		route(genuine).ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("a genuine Keycloak admin was refused: status %d, body %s", rec.Code, rec.Body.String())
		}
	})

	// And the patient who owns it is unaffected.
	t.Run("the owning patient still reads their own", func(t *testing.T) {
		owner := middleware.Principal{
			UserID: victim,
			Issuer: userService,
			Roles:  []middleware.Role{middleware.RolePatient},
		}
		req := httptest.NewRequest(http.MethodGet, "/api/v1/appointments/"+appt.ID.String(), http.NoBody)
		rec := httptest.NewRecorder()
		route(owner).ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("the owning patient was refused their own appointment: status %d, body %s",
				rec.Code, rec.Body.String())
		}
	})
}
