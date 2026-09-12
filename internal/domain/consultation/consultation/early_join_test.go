package consultation

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func seedEndedConsult(t *testing.T, st *fakeStore, doctorID, patientID uuid.UUID, scheduledAt time.Time) *Consultation {
	t.Helper()
	endedAt := scheduledAt.Add(10 * time.Minute)
	c := &Consultation{
		AppointmentID:  uuid.New(),
		PatientID:      patientID,
		DoctorID:       doctorID,
		RoomName:       "room-ended-" + uuid.NewString(),
		Status:         StatusEnded,
		ScheduledAt:    scheduledAt,
		ScheduledEndAt: scheduledAt.Add(15 * time.Minute),
		EndedAt:        &endedAt,
	}
	require.NoError(t, st.CreateConsultation(context.Background(), fakeTx{}, c))
	return c
}

func seedNextConsult(t *testing.T, st *fakeStore, doctorID, patientID uuid.UUID, scheduledAt time.Time, status Status) *Consultation {
	t.Helper()
	c := &Consultation{
		AppointmentID:  uuid.New(),
		PatientID:      patientID,
		DoctorID:       doctorID,
		RoomName:       "room-next-" + uuid.NewString(),
		Status:         status,
		ScheduledAt:    scheduledAt,
		ScheduledEndAt: scheduledAt.Add(15 * time.Minute),
	}
	require.NoError(t, st.CreateConsultation(context.Background(), fakeTx{}, c))
	return c
}

func TestReadyForNext_NotifiesNextPatientOnce(t *testing.T) {
	svc, st, _ := newSweepService(t)
	ctx := context.Background()
	now := time.Now().UTC()
	doctorID := uuid.New()
	doctor := doctorPrincipal(uuid.New(), doctorID)

	finished := seedEndedConsult(t, st, doctorID, uuid.New(), now.Add(-20*time.Minute))
	next := seedNextConsult(t, st, doctorID, uuid.New(), now.Add(10*time.Minute), StatusScheduled)

	first, err := svc.ReadyForNext(ctx, doctor, finished.AppointmentID)
	require.NoError(t, err)
	require.Equal(t, ReadyForNextOffered, first.Status)
	require.Equal(t, next.AppointmentID, first.AppointmentID)
	require.Equal(t, next.ScheduledAt.UTC(), first.ScheduledAt.UTC(), "booked time must not move")
	require.NotNil(t, first.OfferedAt)

	second, err := svc.ReadyForNext(ctx, doctor, finished.AppointmentID)
	require.NoError(t, err)
	require.Equal(t, ReadyForNextAlreadyOffered, second.Status)
	require.Equal(t, next.AppointmentID, second.AppointmentID)
}

func TestReadyForNext_NotEndedIsConflict(t *testing.T) {
	svc, st, _ := newSweepService(t)
	ctx := context.Background()
	now := time.Now().UTC()
	doctorID := uuid.New()

	open := seedNextConsult(t, st, doctorID, uuid.New(), now.Add(-5*time.Minute), StatusScheduled)
	seedNextConsult(t, st, doctorID, uuid.New(), now.Add(20*time.Minute), StatusScheduled)

	_, err := svc.ReadyForNext(ctx, doctorPrincipal(uuid.New(), doctorID), open.AppointmentID)
	require.ErrorIs(t, err, ErrInvalidState)
}

func TestReadyForNext_NoNextPatient(t *testing.T) {
	svc, st, _ := newSweepService(t)
	ctx := context.Background()
	now := time.Now().UTC()
	doctorID := uuid.New()
	finished := seedEndedConsult(t, st, doctorID, uuid.New(), now.Add(-15*time.Minute))

	_, err := svc.ReadyForNext(ctx, doctorPrincipal(uuid.New(), doctorID), finished.AppointmentID)
	require.ErrorIs(t, err, ErrNotFound)
}

func TestReadyForNext_AlreadyWaitingSkipsPing(t *testing.T) {
	svc, st, _ := newSweepService(t)
	ctx := context.Background()
	now := time.Now().UTC()
	doctorID := uuid.New()
	finished := seedEndedConsult(t, st, doctorID, uuid.New(), now.Add(-20*time.Minute))
	next := seedNextConsult(t, st, doctorID, uuid.New(), now.Add(5*time.Minute), StatusWaiting)

	got, err := svc.ReadyForNext(ctx, doctorPrincipal(uuid.New(), doctorID), finished.AppointmentID)
	require.NoError(t, err)
	require.Equal(t, ReadyForNextAlreadyWaiting, got.Status)
	require.Equal(t, next.AppointmentID, got.AppointmentID)

	reloaded, err := st.GetConsultation(ctx, fakePool{}, next.ID)
	require.NoError(t, err)
	require.Nil(t, reloaded.EarlyJoinOfferedAt, "already-waiting patient must not be pinged")
}

func TestReadyForNext_SkipsOtherDoctorsNextPatient(t *testing.T) {
	svc, st, _ := newSweepService(t)
	ctx := context.Background()
	now := time.Now().UTC()
	doctorA, doctorB := uuid.New(), uuid.New()
	finished := seedEndedConsult(t, st, doctorA, uuid.New(), now.Add(-20*time.Minute))
	seedNextConsult(t, st, doctorB, uuid.New(), now.Add(5*time.Minute), StatusScheduled)

	_, err := svc.ReadyForNext(ctx, doctorPrincipal(uuid.New(), doctorA), finished.AppointmentID)
	require.ErrorIs(t, err, ErrNotFound)
}

func TestReadyForNext_ActiveConsultIsConflict(t *testing.T) {
	svc, st, _ := newSweepService(t)
	ctx := context.Background()
	now := time.Now().UTC()
	doctorID := uuid.New()
	finished := seedEndedConsult(t, st, doctorID, uuid.New(), now.Add(-30*time.Minute))
	seedNextConsult(t, st, doctorID, uuid.New(), now.Add(10*time.Minute), StatusScheduled)
	seedNextConsult(t, st, doctorID, uuid.New(), now.Add(-5*time.Minute), StatusActive)

	_, err := svc.ReadyForNext(ctx, doctorPrincipal(uuid.New(), doctorID), finished.AppointmentID)
	require.ErrorIs(t, err, ErrInvalidState)
}

func TestReadyForNext_WithoutAppointmentUsesLatestEnded(t *testing.T) {
	svc, st, _ := newSweepService(t)
	ctx := context.Background()
	now := time.Now().UTC()
	doctorID := uuid.New()
	seedEndedConsult(t, st, doctorID, uuid.New(), now.Add(-20*time.Minute))
	next := seedNextConsult(t, st, doctorID, uuid.New(), now.Add(10*time.Minute), StatusScheduled)

	got, err := svc.ReadyForNext(ctx, doctorPrincipal(uuid.New(), doctorID), uuid.Nil)
	require.NoError(t, err)
	require.Equal(t, ReadyForNextOffered, got.Status)
	require.Equal(t, next.AppointmentID, got.AppointmentID)
}

func TestReadyForNext_PatientForbidden(t *testing.T) {
	svc, st, _ := newSweepService(t)
	ctx := context.Background()
	now := time.Now().UTC()
	doctorID, patientID := uuid.New(), uuid.New()
	finished := seedEndedConsult(t, st, doctorID, patientID, now.Add(-20*time.Minute))
	seedNextConsult(t, st, doctorID, uuid.New(), now.Add(10*time.Minute), StatusScheduled)

	_, err := svc.ReadyForNext(ctx, patientPrincipal(patientID), finished.AppointmentID)
	require.ErrorIs(t, err, ErrForbidden)
}

func TestRespondEarlyJoin_AcceptDoesNotMoveSlot(t *testing.T) {
	svc, st, _ := newSweepService(t)
	ctx := context.Background()
	now := time.Now().UTC()
	doctorID, nextPatient := uuid.New(), uuid.New()
	finished := seedEndedConsult(t, st, doctorID, uuid.New(), now.Add(-20*time.Minute))
	next := seedNextConsult(t, st, doctorID, nextPatient, now.Add(12*time.Minute), StatusScheduled)
	booked := next.ScheduledAt

	_, err := svc.ReadyForNext(ctx, doctorPrincipal(uuid.New(), doctorID), finished.AppointmentID)
	require.NoError(t, err)

	got, err := svc.RespondEarlyJoin(ctx, patientPrincipal(nextPatient), next.AppointmentID, true)
	require.NoError(t, err)
	require.NotNil(t, got.Response)
	require.Equal(t, EarlyJoinAccepted, *got.Response)
	require.Equal(t, booked.UTC(), got.ScheduledAt.UTC())

	reloaded, err := st.GetConsultation(ctx, fakePool{}, next.ID)
	require.NoError(t, err)
	require.Equal(t, booked.UTC(), reloaded.ScheduledAt.UTC())
}

func TestRespondEarlyJoin_DeclineKeepsBookedTime(t *testing.T) {
	svc, st, _ := newSweepService(t)
	ctx := context.Background()
	now := time.Now().UTC()
	doctorID, nextPatient := uuid.New(), uuid.New()
	finished := seedEndedConsult(t, st, doctorID, uuid.New(), now.Add(-20*time.Minute))
	next := seedNextConsult(t, st, doctorID, nextPatient, now.Add(12*time.Minute), StatusScheduled)
	booked := next.ScheduledAt

	_, err := svc.ReadyForNext(ctx, doctorPrincipal(uuid.New(), doctorID), finished.AppointmentID)
	require.NoError(t, err)

	got, err := svc.RespondEarlyJoin(ctx, patientPrincipal(nextPatient), next.AppointmentID, false)
	require.NoError(t, err)
	require.NotNil(t, got.Response)
	require.Equal(t, EarlyJoinDeclined, *got.Response)
	require.Equal(t, booked.UTC(), got.ScheduledAt.UTC())

	again, err := svc.ReadyForNext(ctx, doctorPrincipal(uuid.New(), doctorID), finished.AppointmentID)
	require.NoError(t, err)
	require.Equal(t, ReadyForNextDeclined, again.Status)

	reloaded, err := st.GetConsultation(ctx, fakePool{}, next.ID)
	require.NoError(t, err)
	require.Equal(t, booked.UTC(), reloaded.ScheduledAt.UTC())
}

func TestRespondEarlyJoin_OnlyOwningPatient(t *testing.T) {
	svc, st, _ := newSweepService(t)
	ctx := context.Background()
	now := time.Now().UTC()
	doctorID, nextPatient := uuid.New(), uuid.New()
	finished := seedEndedConsult(t, st, doctorID, uuid.New(), now.Add(-20*time.Minute))
	next := seedNextConsult(t, st, doctorID, nextPatient, now.Add(12*time.Minute), StatusScheduled)

	_, err := svc.ReadyForNext(ctx, doctorPrincipal(uuid.New(), doctorID), finished.AppointmentID)
	require.NoError(t, err)

	_, err = svc.RespondEarlyJoin(ctx, patientPrincipal(uuid.New()), next.AppointmentID, true)
	require.ErrorIs(t, err, ErrForbidden)

	_, err = svc.RespondEarlyJoin(ctx, doctorPrincipal(uuid.New(), doctorID), next.AppointmentID, true)
	require.ErrorIs(t, err, ErrForbidden)
}

func TestGetEarlyJoin_HiddenUntilOffered(t *testing.T) {
	svc, st, _ := newSweepService(t)
	ctx := context.Background()
	now := time.Now().UTC()
	doctorID, nextPatient := uuid.New(), uuid.New()
	next := seedNextConsult(t, st, doctorID, nextPatient, now.Add(12*time.Minute), StatusScheduled)

	_, err := svc.GetEarlyJoin(ctx, patientPrincipal(nextPatient), next.AppointmentID)
	require.ErrorIs(t, err, ErrNotFound)
}
