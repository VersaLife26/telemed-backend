package consultation

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"telemed/internal/platform/events"
)

func TestJoin_PatientLateWithinGraceIsAllowed(t *testing.T) {
	svc, st, _, _ := newTestService(t, Options{})
	patientID, doctorID := uuid.New(), uuid.New()
	c := seedConsultation(t, st, patientID, doctorID)
	st.mu.Lock()
	st.consultations[c.ID].ScheduledAt = time.Now().UTC().Add(-5 * time.Minute)
	st.mu.Unlock()

	got, err := svc.Join(context.Background(), patientPrincipal(patientID), c.AppointmentID)
	require.NoError(t, err)
	require.Equal(t, StatusWaiting, got.Status)
}

func TestJoin_PatientPastCutoffIsBlocked(t *testing.T) {
	svc, st, _, _ := newTestService(t, Options{})
	patientID, doctorID := uuid.New(), uuid.New()
	c := seedConsultation(t, st, patientID, doctorID)
	st.mu.Lock()
	st.consultations[c.ID].ScheduledAt = time.Now().UTC().Add(-(LateJoinCutoff + time.Minute))
	st.mu.Unlock()

	_, err := svc.Join(context.Background(), patientPrincipal(patientID), c.AppointmentID)
	require.ErrorIs(t, err, ErrJoinCutoff)
}

func TestJoin_DoctorPastCutoffStillAllowed(t *testing.T) {
	svc, st, _, _ := newTestService(t, Options{})
	patientID, doctorID := uuid.New(), uuid.New()
	c := seedConsultation(t, st, patientID, doctorID)
	st.mu.Lock()
	st.consultations[c.ID].ScheduledAt = time.Now().UTC().Add(-(LateJoinCutoff + time.Minute))
	st.mu.Unlock()

	got, err := svc.Join(context.Background(), doctorPrincipal(uuid.New(), doctorID), c.AppointmentID)
	require.NoError(t, err)
	require.Equal(t, "doctor", got.Role)
	require.Equal(t, StatusScheduled, got.Status)
}

func TestJoin_PatientReconnectAfterCutoffIfAlreadyWaiting(t *testing.T) {
	svc, st, _, _ := newTestService(t, Options{})
	patientID, doctorID := uuid.New(), uuid.New()
	c := seedConsultation(t, st, patientID, doctorID)
	patientJoins(t, svc, patientID, c)

	st.mu.Lock()
	st.consultations[c.ID].ScheduledAt = time.Now().UTC().Add(-(LateJoinCutoff + time.Minute))
	st.mu.Unlock()

	got, err := svc.Join(context.Background(), patientPrincipal(patientID), c.AppointmentID)
	require.NoError(t, err)
	require.Equal(t, StatusWaiting, got.Status)
}

func TestSweepPatientNoShow_MarksScheduledPastGrace(t *testing.T) {
	svc, st, _ := newSweepService(t)
	ctx := context.Background()
	now := time.Now().UTC()
	c := seedConsultation(t, st, uuid.New(), uuid.New())
	st.mu.Lock()
	st.consultations[c.ID].ScheduledAt = now.Add(-LateJoinGrace - time.Minute)
	st.mu.Unlock()

	n, err := svc.SweepPatientNoShow(ctx, now, 50)
	require.NoError(t, err)
	require.Equal(t, 1, n)

	reloaded, err := st.GetConsultation(ctx, fakePool{}, c.ID)
	require.NoError(t, err)
	require.Equal(t, StatusAbandoned, reloaded.Status)
	require.NotNil(t, reloaded.EndReason)
	require.Equal(t, "no_show", *reloaded.EndReason)

	n, err = svc.SweepPatientNoShow(ctx, now.Add(time.Minute), 50)
	require.NoError(t, err)
	require.Equal(t, 0, n, "second sweep must not re-mark")

	_, err = svc.Join(ctx, patientPrincipal(c.PatientID), c.AppointmentID)
	require.ErrorIs(t, err, ErrInvalidState)
}

func TestSweepPatientNoShow_SkipsWaitingPatient(t *testing.T) {
	svc, st, _ := newSweepService(t)
	ctx := context.Background()
	now := time.Now().UTC()
	patientID, doctorID := uuid.New(), uuid.New()
	c := seedConsultation(t, st, patientID, doctorID)
	patientJoins(t, svc, patientID, c)
	st.mu.Lock()
	st.consultations[c.ID].ScheduledAt = now.Add(-LateJoinGrace - time.Minute)
	st.mu.Unlock()

	n, err := svc.SweepPatientNoShow(ctx, now, 50)
	require.NoError(t, err)
	require.Equal(t, 0, n)

	reloaded, err := st.GetConsultation(ctx, fakePool{}, c.ID)
	require.NoError(t, err)
	require.Equal(t, StatusWaiting, reloaded.Status)
}

func TestSweepPatientNoShow_SkipsInsideGrace(t *testing.T) {
	svc, st, _ := newSweepService(t)
	ctx := context.Background()
	now := time.Now().UTC()
	c := seedConsultation(t, st, uuid.New(), uuid.New())
	st.mu.Lock()
	st.consultations[c.ID].ScheduledAt = now.Add(-5 * time.Minute)
	st.mu.Unlock()

	n, err := svc.SweepPatientNoShow(ctx, now, 50)
	require.NoError(t, err)
	require.Equal(t, 0, n)
}

func TestSweepPatientNoShow_SkipsActiveConsult(t *testing.T) {
	svc, st, _ := newSweepService(t)
	ctx := context.Background()
	now := time.Now().UTC()
	c := activeConsultation(t, svc, st, time.Minute)
	st.mu.Lock()
	st.consultations[c.ID].ScheduledAt = now.Add(-LateJoinGrace - time.Minute)
	st.mu.Unlock()

	n, err := svc.SweepPatientNoShow(ctx, now, 50)
	require.NoError(t, err)
	require.Equal(t, 0, n)

	reloaded, err := st.GetConsultation(ctx, fakePool{}, c.ID)
	require.NoError(t, err)
	require.Equal(t, StatusActive, reloaded.Status)
}

func TestAppointmentTeardownSubjectsIncludeNoShow(t *testing.T) {
	require.Contains(t, appointmentTeardownSubjects, events.SubjectAppointmentCancelled)
	require.Contains(t, appointmentTeardownSubjects, events.SubjectAppointmentNoShow)
}
