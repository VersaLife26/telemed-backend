package scheduling_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"telemed/internal/domain/scheduling/scheduling"
	"telemed/internal/platform/database"
	"telemed/internal/platform/events"
)

func confirmTestAppointment(t *testing.T, svc *scheduling.Service, appointmentID uuid.UUID) uuid.UUID {
	t.Helper()
	payID := uuid.New()
	err := database.InTx(context.Background(), testPool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		return svc.ConfirmAppointment(context.Background(), tx, appointmentID, &payID)
	})
	if err != nil {
		t.Fatalf("confirm appointment: %v", err)
	}
	return payID
}

func bookConfirmed(t *testing.T, svc *scheduling.Service, doctorID, patientID uuid.UUID, start time.Time) scheduling.Appointment {
	t.Helper()
	slotID := seedPricedSlot(t, testPool, doctorID, start, 15*time.Minute)
	appt, err := svc.BookSlot(context.Background(), scheduling.BookSlotInput{
		SlotID:    slotID,
		PatientID: patientID,
	})
	if err != nil {
		t.Fatalf("book: %v", err)
	}
	payID := confirmTestAppointment(t, svc, appt.ID)
	appt.PaymentID = &payID
	appt.Status = scheduling.AppointmentConfirmed
	return appt
}

func countOutbox(t *testing.T, subject string) int {
	t.Helper()
	var n int
	if err := testPool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM outbox_events WHERE subject = $1`, subject).Scan(&n); err != nil {
		t.Fatalf("count outbox %s: %v", subject, err)
	}
	return n
}

func slotRow(t *testing.T, id uuid.UUID) (status string, reservedFor *uuid.UUID, reservedUntil *time.Time) {
	t.Helper()
	err := testPool.QueryRow(context.Background(),
		`SELECT status, reserved_for, reserved_until FROM slots WHERE id = $1`, id).
		Scan(&status, &reservedFor, &reservedUntil)
	if err != nil {
		t.Fatalf("read slot: %v", err)
	}
	return status, reservedFor, reservedUntil
}

func TestRescheduleCreateHoldsAdHocSlot(t *testing.T) {
	pool := requireDB(t)
	resetTables(t, pool)
	svc := newTestService(t, pool, scheduling.NoopLocker{}, nil)

	doctorID, patientID := uuid.New(), uuid.New()
	appt := bookConfirmed(t, svc, doctorID, patientID, time.Now().Add(90*time.Minute))
	proposed := time.Now().Add(26 * time.Hour).UTC().Truncate(time.Minute)

	req, err := svc.CreateRescheduleRequest(context.Background(), scheduling.CreateRescheduleInput{
		AppointmentID:   appt.ID,
		ActorID:         doctorID,
		ActorRole:       scheduling.ActorDoctor,
		ProposedStartAt: proposed,
		Reason:          "clinic closed that morning",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !req.ProposedSlotCreated {
		t.Fatal("expected an ad-hoc slot to be inserted")
	}
	status, reservedFor, reservedUntil := slotRow(t, req.ProposedSlotID)
	if status != string(scheduling.SlotBlocked) {
		t.Fatalf("proposed slot status = %s, want BLOCKED", status)
	}
	if reservedFor == nil || *reservedFor != patientID {
		t.Fatalf("reserved_for = %v, want patient", reservedFor)
	}
	if reservedUntil != nil {
		t.Fatalf("reserved_until = %v, want NULL so the waitlist sweeper cannot steal it", reservedUntil)
	}

	var origStatus string
	if err := pool.QueryRow(context.Background(), `SELECT status FROM slots WHERE id = $1`, appt.SlotID).Scan(&origStatus); err != nil {
		t.Fatalf("original slot: %v", err)
	}
	if origStatus != string(scheduling.SlotBooked) {
		t.Fatalf("original slot status = %s, want BOOKED until a decision", origStatus)
	}

	if countOutbox(t, "appointment.reschedule_requested") != 1 {
		t.Fatal("want appointment.reschedule_requested published")
	}
	var raw []byte
	if err := pool.QueryRow(context.Background(),
		`SELECT payload FROM outbox_events WHERE subject = 'appointment.reschedule_requested' LIMIT 1`).Scan(&raw); err != nil {
		t.Fatalf("read outbox: %v", err)
	}
	if jsonContainsReason(raw, "clinic closed") {
		t.Fatal("doctor reason must not appear on the NATS payload")
	}

	_, err = svc.CreateRescheduleRequest(context.Background(), scheduling.CreateRescheduleInput{
		AppointmentID:   appt.ID,
		ActorID:         doctorID,
		ActorRole:       scheduling.ActorDoctor,
		ProposedStartAt: proposed.Add(time.Hour),
	})
	if !errors.Is(err, scheduling.ErrRescheduleAlreadyPending) {
		t.Fatalf("second request err = %v, want ErrRescheduleAlreadyPending", err)
	}
}

func jsonContainsReason(raw []byte, needle string) bool {
	return strings.Contains(string(raw), needle)
}

func TestRescheduleCreateRejected(t *testing.T) {
	pool := requireDB(t)
	svc := newTestService(t, pool, scheduling.NoopLocker{}, nil)
	ctx := context.Background()

	t.Run("non-owner doctor", func(t *testing.T) {
		resetTables(t, pool)
		doctorID, patientID := uuid.New(), uuid.New()
		appt := bookConfirmed(t, svc, doctorID, patientID, time.Now().Add(90*time.Minute))
		_, err := svc.CreateRescheduleRequest(ctx, scheduling.CreateRescheduleInput{
			AppointmentID:   appt.ID,
			ActorID:         uuid.New(),
			ActorRole:       scheduling.ActorDoctor,
			ProposedStartAt: time.Now().Add(26 * time.Hour),
		})
		if !errors.Is(err, scheduling.ErrAppointmentNotFound) {
			t.Fatalf("err = %v, want ErrAppointmentNotFound", err)
		}
	})

	t.Run("proposed time in the past", func(t *testing.T) {
		resetTables(t, pool)
		clock := newTestClock()
		svcClock := newTestServiceWithClock(t, pool, clock, scheduling.NoopWaitlistQueue{})
		doctorID, patientID := uuid.New(), uuid.New()
		appt := bookConfirmed(t, svcClock, doctorID, patientID, clock.Now().Add(90*time.Minute))
		_, err := svcClock.CreateRescheduleRequest(ctx, scheduling.CreateRescheduleInput{
			AppointmentID:   appt.ID,
			ActorID:         doctorID,
			ActorRole:       scheduling.ActorDoctor,
			ProposedStartAt: clock.Now().Add(-time.Hour),
		})
		if !errors.Is(err, scheduling.ErrProposedTimeInPast) {
			t.Fatalf("err = %v, want ErrProposedTimeInPast", err)
		}
	})

	t.Run("overlap with another live booking", func(t *testing.T) {
		resetTables(t, pool)
		doctorID := uuid.New()
		first := bookConfirmed(t, svc, doctorID, uuid.New(), time.Now().Add(90*time.Minute))
		otherStart := time.Now().Add(26 * time.Hour).UTC().Truncate(time.Minute)
		_ = bookConfirmed(t, svc, doctorID, uuid.New(), otherStart)

		_, err := svc.CreateRescheduleRequest(ctx, scheduling.CreateRescheduleInput{
			AppointmentID:   first.ID,
			ActorID:         doctorID,
			ActorRole:       scheduling.ActorDoctor,
			ProposedStartAt: otherStart,
		})
		if !errors.Is(err, scheduling.ErrDuplicateBooking) {
			t.Fatalf("err = %v, want ErrDuplicateBooking", err)
		}
	})
}

func TestRescheduleAcceptMovesWithoutCancel(t *testing.T) {
	pool := requireDB(t)
	resetTables(t, pool)
	svc := newTestService(t, pool, scheduling.NoopLocker{}, nil)
	ctx := context.Background()

	doctorID, patientID := uuid.New(), uuid.New()
	appt := bookConfirmed(t, svc, doctorID, patientID, time.Now().Add(90*time.Minute))
	payID := appt.PaymentID
	originalSlot := appt.SlotID
	proposed := time.Now().Add(26 * time.Hour).UTC().Truncate(time.Minute)

	req, err := svc.CreateRescheduleRequest(ctx, scheduling.CreateRescheduleInput{
		AppointmentID:   appt.ID,
		ActorID:         doctorID,
		ActorRole:       scheduling.ActorDoctor,
		ProposedStartAt: proposed,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	_, moved, err := svc.AcceptRescheduleRequest(ctx, scheduling.DecideRescheduleInput{
		RequestID: req.ID,
		ActorID:   patientID,
		ActorRole: scheduling.ActorPatient,
	})
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	if moved.ID != appt.ID {
		t.Fatalf("appointment id changed: %s -> %s", appt.ID, moved.ID)
	}
	if moved.SlotID != req.ProposedSlotID {
		t.Fatalf("slot_id = %s, want proposed %s", moved.SlotID, req.ProposedSlotID)
	}
	if payID != nil && (moved.PaymentID == nil || *moved.PaymentID != *payID) {
		t.Fatal("payment_id must stay on the same appointment")
	}

	origStatus, _, _ := slotRow(t, originalSlot)
	if origStatus != string(scheduling.SlotAvailable) {
		t.Fatalf("original slot status = %s, want AVAILABLE", origStatus)
	}
	propStatus, _, _ := slotRow(t, req.ProposedSlotID)
	if propStatus != string(scheduling.SlotBooked) {
		t.Fatalf("proposed slot status = %s, want BOOKED", propStatus)
	}
	if countOutbox(t, "appointment.cancelled") != 0 {
		t.Fatal("accept must not publish appointment.cancelled")
	}
	if countOutbox(t, "appointment.rescheduled") != 1 {
		t.Fatal("want appointment.rescheduled")
	}
	if countOutbox(t, "slot.booked") < 2 {
		t.Fatal("accept must publish slot.booked for the proposed time")
	}
}

func TestRescheduleDeclineFullRefundDropsHold(t *testing.T) {
	pool := requireDB(t)
	resetTables(t, pool)
	svc := newTestService(t, pool, scheduling.NoopLocker{}, nil)
	ctx := context.Background()

	doctorID, patientID := uuid.New(), uuid.New()
	appt := bookConfirmed(t, svc, doctorID, patientID, time.Now().Add(90*time.Minute))
	proposed := time.Now().Add(26 * time.Hour).UTC().Truncate(time.Minute)
	req, err := svc.CreateRescheduleRequest(ctx, scheduling.CreateRescheduleInput{
		AppointmentID:   appt.ID,
		ActorID:         doctorID,
		ActorRole:       scheduling.ActorDoctor,
		ProposedStartAt: proposed,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	_, cancelled, err := svc.DeclineRescheduleRequest(ctx, scheduling.DecideRescheduleInput{
		RequestID: req.ID,
		ActorID:   patientID,
		ActorRole: scheduling.ActorPatient,
	})
	if err != nil {
		t.Fatalf("decline: %v", err)
	}
	if cancelled.Status != scheduling.AppointmentCancelled {
		t.Fatalf("status = %s, want cancelled", cancelled.Status)
	}
	if cancelled.RefundPolicy == nil || *cancelled.RefundPolicy != scheduling.RefundFull {
		t.Fatalf("refund policy = %v, want FULL", cancelled.RefundPolicy)
	}

	propStatus, _, _ := slotRow(t, req.ProposedSlotID)
	if propStatus != string(scheduling.SlotCancelled) {
		t.Fatalf("ad-hoc proposed slot status = %s, want CANCELLED", propStatus)
	}
	origStatus, _, _ := slotRow(t, appt.SlotID)
	if origStatus != string(scheduling.SlotAvailable) {
		t.Fatalf("original slot status = %s, want AVAILABLE", origStatus)
	}

	var raw []byte
	if err := pool.QueryRow(ctx,
		`SELECT payload FROM outbox_events WHERE subject = 'appointment.cancelled' LIMIT 1`).Scan(&raw); err != nil {
		t.Fatalf("read cancelled event: %v", err)
	}
	var env struct {
		Payload events.AppointmentCancelled `json:"payload"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if scheduling.RefundPolicy(env.Payload.RefundPolicy) != scheduling.RefundFull {
		t.Fatalf("event refund_policy = %s, want FULL", env.Payload.RefundPolicy)
	}
	if env.Payload.RefundPercent != 100 {
		t.Fatalf("event refund_percent = %d, want 100", env.Payload.RefundPercent)
	}
}

func TestRescheduleAuthorization(t *testing.T) {
	pool := requireDB(t)
	resetTables(t, pool)
	svc := newTestService(t, pool, scheduling.NoopLocker{}, nil)
	ctx := context.Background()

	doctorID, patientID := uuid.New(), uuid.New()
	appt := bookConfirmed(t, svc, doctorID, patientID, time.Now().Add(90*time.Minute))
	req, err := svc.CreateRescheduleRequest(ctx, scheduling.CreateRescheduleInput{
		AppointmentID:   appt.ID,
		ActorID:         doctorID,
		ActorRole:       scheduling.ActorDoctor,
		ProposedStartAt: time.Now().Add(26 * time.Hour),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	_, _, err = svc.AcceptRescheduleRequest(ctx, scheduling.DecideRescheduleInput{
		RequestID: req.ID,
		ActorID:   uuid.New(),
		ActorRole: scheduling.ActorPatient,
	})
	if !errors.Is(err, scheduling.ErrRescheduleNotFound) {
		t.Fatalf("other patient err = %v, want ErrRescheduleNotFound", err)
	}

	_, moved, err := svc.AcceptRescheduleRequest(ctx, scheduling.DecideRescheduleInput{
		RequestID: req.ID,
		ActorID:   uuid.New(),
		ActorRole: scheduling.ActorAdmin,
	})
	if err != nil {
		t.Fatalf("admin accept: %v", err)
	}
	if moved.SlotID != req.ProposedSlotID {
		t.Fatal("admin accept should move the booking")
	}
}

func TestRescheduleExpiryRunsDecline(t *testing.T) {
	pool := requireDB(t)
	resetTables(t, pool)
	clock := newTestClock()
	svc := newTestServiceWithClock(t, pool, clock, scheduling.NoopWaitlistQueue{})
	ctx := context.Background()

	doctorID, patientID := uuid.New(), uuid.New()
	start := clock.Now().Add(90 * time.Minute)
	appt := bookConfirmed(t, svc, doctorID, patientID, start)
	req, err := svc.CreateRescheduleRequest(ctx, scheduling.CreateRescheduleInput{
		AppointmentID:   appt.ID,
		ActorID:         doctorID,
		ActorRole:       scheduling.ActorDoctor,
		ProposedStartAt: clock.Now().Add(26 * time.Hour),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if n, err := svc.SweepExpiredRescheduleRequests(ctx); err != nil || n != 0 {
		t.Fatalf("sweep before original start: n=%d err=%v, want 0", n, err)
	}

	clock.Advance(2 * time.Hour)
	n, err := svc.SweepExpiredRescheduleRequests(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 1 {
		t.Fatalf("swept %d, want 1", n)
	}

	got, err := svc.Repo().GetRescheduleRequest(ctx, pool, req.ID)
	if err != nil {
		t.Fatalf("reload request: %v", err)
	}
	if got.Status != scheduling.RescheduleExpired {
		t.Fatalf("status = %s, want expired", got.Status)
	}
	cancelled, err := svc.Repo().GetAppointment(ctx, pool, appt.ID)
	if err != nil {
		t.Fatalf("reload appointment: %v", err)
	}
	if cancelled.Status != scheduling.AppointmentCancelled {
		t.Fatalf("appointment status = %s, want cancelled", cancelled.Status)
	}
	if cancelled.RefundPolicy == nil || *cancelled.RefundPolicy != scheduling.RefundFull {
		t.Fatalf("refund = %v, want FULL", cancelled.RefundPolicy)
	}
}
