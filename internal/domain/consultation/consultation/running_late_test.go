package consultation

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestSweepRunningLate_NotifiesNextPatientOnce(t *testing.T) {
	svc, st, _ := newSweepService(t)
	ctx := context.Background()
	now := time.Now().UTC()

	doctorID := uuid.New()
	activePatient, nextPatient := uuid.New(), uuid.New()

	active := &Consultation{
		AppointmentID:  uuid.New(),
		PatientID:      activePatient,
		DoctorID:       doctorID,
		RoomName:       "room-active-" + uuid.NewString(),
		Status:         StatusScheduled,
		ScheduledAt:    now.Add(-2 * time.Minute),
		ScheduledEndAt: now.Add(13 * time.Minute),
	}
	require.NoError(t, st.CreateConsultation(ctx, fakeTx{}, active))
	patientJoins(t, svc, activePatient, active)
	_, err := svc.Admit(ctx, doctorPrincipal(uuid.New(), doctorID), active.ID)
	require.NoError(t, err)

	st.mu.Lock()
	st.consultations[active.ID].ScheduledAt = now.Add(-30 * time.Minute)
	st.consultations[active.ID].ScheduledEndAt = now.Add(-5 * time.Minute)
	st.consultations[active.ID].RunningLateNotifiedAt = nil
	st.mu.Unlock()

	next := &Consultation{
		AppointmentID:  uuid.New(),
		PatientID:      nextPatient,
		DoctorID:       doctorID,
		RoomName:       "room-next-" + uuid.NewString(),
		Status:         StatusWaiting,
		ScheduledAt:    now.Add(10 * time.Minute),
		ScheduledEndAt: now.Add(25 * time.Minute),
	}
	require.NoError(t, st.CreateConsultation(ctx, fakeTx{}, next))

	n, err := svc.SweepRunningLate(ctx, now, 50)
	require.NoError(t, err)
	require.Equal(t, 1, n, "next patient should be notified once the consult overruns")

	reloaded, err := st.GetConsultation(ctx, fakePool{}, active.ID)
	require.NoError(t, err)
	require.NotNil(t, reloaded.RunningLateNotifiedAt, "throttle stamp must be set")

	n, err = svc.SweepRunningLate(ctx, now.Add(time.Minute), 50)
	require.NoError(t, err)
	require.Equal(t, 0, n, "second sweep must not spam the next patient")
}

func TestSweepRunningLate_NoNextPatientStillClaims(t *testing.T) {
	svc, st, _ := newSweepService(t)
	ctx := context.Background()
	now := time.Now().UTC()
	doctorID, patientID := uuid.New(), uuid.New()

	active := &Consultation{
		AppointmentID:  uuid.New(),
		PatientID:      patientID,
		DoctorID:       doctorID,
		RoomName:       "room-solo-" + uuid.NewString(),
		Status:         StatusScheduled,
		ScheduledAt:    now.Add(-2 * time.Minute),
		ScheduledEndAt: now.Add(13 * time.Minute),
	}
	require.NoError(t, st.CreateConsultation(ctx, fakeTx{}, active))
	patientJoins(t, svc, patientID, active)
	_, err := svc.Admit(ctx, doctorPrincipal(uuid.New(), doctorID), active.ID)
	require.NoError(t, err)

	st.mu.Lock()
	st.consultations[active.ID].ScheduledEndAt = now.Add(-10 * time.Minute)
	st.consultations[active.ID].RunningLateNotifiedAt = nil
	st.mu.Unlock()

	n, err := svc.SweepRunningLate(ctx, now, 50)
	require.NoError(t, err)
	require.Equal(t, 0, n)

	reloaded, err := st.GetConsultation(ctx, fakePool{}, active.ID)
	require.NoError(t, err)
	require.NotNil(t, reloaded.RunningLateNotifiedAt, "claim even when nobody is waiting next")
}

func TestSweepRunningLate_IgnoresConsultStillInsideSlot(t *testing.T) {
	svc, st, _ := newSweepService(t)
	ctx := context.Background()
	now := time.Now().UTC()
	doctorID := uuid.New()
	patientID, nextPatient := uuid.New(), uuid.New()

	active := &Consultation{
		AppointmentID:  uuid.New(),
		PatientID:      patientID,
		DoctorID:       doctorID,
		RoomName:       "room-ontime-" + uuid.NewString(),
		Status:         StatusScheduled,
		ScheduledAt:    now.Add(-5 * time.Minute),
		ScheduledEndAt: now.Add(5 * time.Minute), // still inside the slot
	}
	require.NoError(t, st.CreateConsultation(ctx, fakeTx{}, active))
	patientJoins(t, svc, patientID, active)
	_, err := svc.Admit(ctx, doctorPrincipal(uuid.New(), doctorID), active.ID)
	require.NoError(t, err)

	require.NoError(t, st.CreateConsultation(ctx, fakeTx{}, &Consultation{
		AppointmentID:  uuid.New(),
		PatientID:      nextPatient,
		DoctorID:       doctorID,
		RoomName:       "room-next-ontime-" + uuid.NewString(),
		Status:         StatusScheduled,
		ScheduledAt:    now.Add(15 * time.Minute),
		ScheduledEndAt: now.Add(30 * time.Minute),
	}))

	n, err := svc.SweepRunningLate(ctx, now, 50)
	require.NoError(t, err)
	require.Equal(t, 0, n)
}

func TestSweepRunningLate_SkipsOtherDoctorsNextPatient(t *testing.T) {
	svc, st, _ := newSweepService(t)
	ctx := context.Background()
	now := time.Now().UTC()
	doctorA, doctorB := uuid.New(), uuid.New()
	patientA, patientB := uuid.New(), uuid.New()

	active := &Consultation{
		AppointmentID:  uuid.New(),
		PatientID:      patientA,
		DoctorID:       doctorA,
		RoomName:       "room-doc-a-" + uuid.NewString(),
		Status:         StatusScheduled,
		ScheduledAt:    now.Add(-2 * time.Minute),
		ScheduledEndAt: now.Add(13 * time.Minute),
	}
	require.NoError(t, st.CreateConsultation(ctx, fakeTx{}, active))
	patientJoins(t, svc, patientA, active)
	_, err := svc.Admit(ctx, doctorPrincipal(uuid.New(), doctorA), active.ID)
	require.NoError(t, err)
	st.mu.Lock()
	st.consultations[active.ID].ScheduledEndAt = now.Add(-2 * time.Minute)
	st.consultations[active.ID].RunningLateNotifiedAt = nil
	st.mu.Unlock()

	require.NoError(t, st.CreateConsultation(ctx, fakeTx{}, &Consultation{
		AppointmentID:  uuid.New(),
		PatientID:      patientB,
		DoctorID:       doctorB, // different doctor
		RoomName:       "room-doc-b-" + uuid.NewString(),
		Status:         StatusWaiting,
		ScheduledAt:    now.Add(5 * time.Minute),
		ScheduledEndAt: now.Add(20 * time.Minute),
	}))

	n, err := svc.SweepRunningLate(ctx, now, 50)
	require.NoError(t, err)
	require.Equal(t, 0, n, "must not notify another doctor's patient")
}

func TestFindNextUpcomingForDoctor_OrdersByScheduledAt(t *testing.T) {
	st := newFakeStore()
	ctx := context.Background()
	now := time.Now().UTC()
	doctorID := uuid.New()

	later := &Consultation{
		AppointmentID: uuid.New(), PatientID: uuid.New(), DoctorID: doctorID,
		RoomName: "later", Status: StatusScheduled,
		ScheduledAt: now.Add(40 * time.Minute), ScheduledEndAt: now.Add(55 * time.Minute),
	}
	sooner := &Consultation{
		AppointmentID: uuid.New(), PatientID: uuid.New(), DoctorID: doctorID,
		RoomName: "sooner", Status: StatusScheduled,
		ScheduledAt: now.Add(20 * time.Minute), ScheduledEndAt: now.Add(35 * time.Minute),
	}
	require.NoError(t, st.CreateConsultation(ctx, fakeTx{}, later))
	require.NoError(t, st.CreateConsultation(ctx, fakeTx{}, sooner))

	got, err := st.FindNextUpcomingForDoctor(ctx, fakePool{}, doctorID, now)
	require.NoError(t, err)
	require.Equal(t, sooner.AppointmentID, got.AppointmentID)
}
