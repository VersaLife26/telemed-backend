package consultation

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"telemed/internal/platform/events"
	"telemed/internal/platform/middleware"
)

func newTestService(t *testing.T, opts Options) (*Service, *fakeStore, *MockProvider, *fakeCache) {
	t.Helper()
	st := newFakeStore()
	video := NewMockProvider("test-webhook-token")
	c := newFakeCache()
	svc := NewService(st, video, c, fakePool{}, events.NewOutbox("test-consultation-service"), zerolog.Nop(), opts)
	return svc, st, video, c
}

// seedConsultation writes a scheduled consultation straight into the fake
// store, bypassing the appointment.confirmed consumer -- tests below are
// about Service's own rules, not about event wiring.
func seedConsultation(t *testing.T, st *fakeStore, patientID, doctorID uuid.UUID) *Consultation {
	t.Helper()
	c := &Consultation{
		AppointmentID: uuid.New(),
		PatientID:     patientID,
		DoctorID:      doctorID,
		RoomName:      "room-" + uuid.NewString(),
		Status:        StatusScheduled,
		ScheduledAt:   time.Now().UTC(),
	}
	require.NoError(t, st.CreateConsultation(context.Background(), fakeTx{}, c))
	return c
}

// patientJoins drives the patient's own Join, which is the ONLY way a
// consultation reaches StatusWaiting and therefore -- since F3 -- the only way
// Admit can succeed. Tests that used to call Admit straight off a scheduled
// consultation now say out loud what the doctor cannot do without the patient.
// See docs/DESIGN.md, "What counts as a treating relationship".
func patientJoins(t *testing.T, svc *Service, patientID uuid.UUID, c *Consultation) {
	t.Helper()
	_, err := svc.Join(context.Background(), patientPrincipal(patientID), c.AppointmentID)
	require.NoError(t, err)
}

func patientPrincipal(userID uuid.UUID) middleware.Principal {
	return middleware.Principal{UserID: userID, Roles: []middleware.Role{middleware.RolePatient}}
}

func doctorPrincipal(userID, doctorID uuid.UUID) middleware.Principal {
	return middleware.Principal{UserID: userID, DoctorID: doctorID, Roles: []middleware.Role{middleware.RoleDoctor}}
}

// --- token authorization: a third party gets nothing -----------------------

func TestJoin_ThirdPartyGetsNothing(t *testing.T) {
	svc, st, _, _ := newTestService(t, Options{})
	patientID, doctorID := uuid.New(), uuid.New()
	c := seedConsultation(t, st, patientID, doctorID)

	ctx := context.Background()

	// The patient and doctor who actually own this consultation succeed.
	patientResult, err := svc.Join(ctx, patientPrincipal(patientID), c.AppointmentID)
	require.NoError(t, err)
	require.NotEmpty(t, patientResult.Token)
	require.Equal(t, c.RoomName, patientResult.RoomName)

	doctorResult, err := svc.Join(ctx, doctorPrincipal(uuid.New(), doctorID), c.AppointmentID)
	require.NoError(t, err)
	require.NotEmpty(t, doctorResult.Token)

	// A third party -- neither this patient nor this doctor -- gets nothing.
	stranger := patientPrincipal(uuid.New())
	_, err = svc.Join(ctx, stranger, c.AppointmentID)
	require.ErrorIs(t, err, ErrForbidden)

	// A doctor principal whose doctor_id does not match this consultation's
	// doctor also gets nothing, even though they hold a valid "doctor" role.
	wrongDoctor := doctorPrincipal(uuid.New(), uuid.New())
	_, err = svc.Join(ctx, wrongDoctor, c.AppointmentID)
	require.ErrorIs(t, err, ErrForbidden)
}

func TestJoin_UnknownAppointmentNotFound(t *testing.T) {
	svc, _, _, _ := newTestService(t, Options{})
	_, err := svc.Join(context.Background(), patientPrincipal(uuid.New()), uuid.New())
	require.ErrorIs(t, err, ErrNotFound)
}

func TestAdmit_OnlyTheAssignedDoctorMayAdmit(t *testing.T) {
	svc, st, _, _ := newTestService(t, Options{})
	patientID, doctorID := uuid.New(), uuid.New()
	c := seedConsultation(t, st, patientID, doctorID)
	ctx := context.Background()

	_, err := svc.Admit(ctx, doctorPrincipal(uuid.New(), uuid.New()), c.ID)
	require.ErrorIs(t, err, ErrForbidden)

	_, err = svc.Admit(ctx, patientPrincipal(patientID), c.ID)
	require.ErrorIs(t, err, ErrForbidden)

	patientJoins(t, svc, patientID, c)

	updated, err := svc.Admit(ctx, doctorPrincipal(uuid.New(), doctorID), c.ID)
	require.NoError(t, err)
	require.Equal(t, StatusActive, updated.Status)
	require.NotNil(t, updated.StartedAt)
}

// --- consent gating on recording --------------------------------------------

func TestConsent_SingleConsentDoesNotStartRecording(t *testing.T) {
	svc, st, video, _ := newTestService(t, Options{})
	patientID, doctorID := uuid.New(), uuid.New()
	c := seedConsultation(t, st, patientID, doctorID)
	ctx := context.Background()

	// Join lazily creates the LiveKit room; a real client always does this
	// before or around admitting, so the test does too.
	_, err := svc.Join(ctx, patientPrincipal(patientID), c.AppointmentID)
	require.NoError(t, err)
	_, err = svc.Admit(ctx, doctorPrincipal(uuid.New(), doctorID), c.ID)
	require.NoError(t, err)

	// Only the patient consents to recording.
	_, err = svc.SubmitConsent(ctx, patientPrincipal(patientID), c.ID, ConsentInput{
		Type: ConsentRecording, Granted: true,
	})
	require.NoError(t, err)

	stored, err := st.GetConsultation(ctx, fakePool{}, c.ID)
	require.NoError(t, err)
	require.Equal(t, RecordingNone, stored.RecordingStatus, "a single consent must not start recording")
	require.False(t, video.RoomExists(c.RoomName) && stored.EgressID != nil)

	// The doctor also consents -- now both parties have granted, recording starts.
	_, err = svc.SubmitConsent(ctx, doctorPrincipal(uuid.New(), doctorID), c.ID, ConsentInput{
		Type: ConsentRecording, Granted: true,
	})
	require.NoError(t, err)

	stored, err = st.GetConsultation(ctx, fakePool{}, c.ID)
	require.NoError(t, err)
	require.Equal(t, RecordingInProgress, stored.RecordingStatus, "both parties consenting must start recording")
	require.NotNil(t, stored.EgressID)

	eg, ok := video.Egress(*stored.EgressID)
	require.True(t, ok)
	require.Equal(t, EgressStatusActive, eg.Status)
}

func TestConsent_RevokedConsentAfterGrantStopsRecordingFromStarting(t *testing.T) {
	svc, st, _, _ := newTestService(t, Options{})
	patientID, doctorID := uuid.New(), uuid.New()
	c := seedConsultation(t, st, patientID, doctorID)
	ctx := context.Background()
	patientJoins(t, svc, patientID, c)
	_, err := svc.Admit(ctx, doctorPrincipal(uuid.New(), doctorID), c.ID)
	require.NoError(t, err)

	// Doctor grants, then changes their mind before the patient ever consents.
	_, err = svc.SubmitConsent(ctx, doctorPrincipal(uuid.New(), doctorID), c.ID, ConsentInput{Type: ConsentRecording, Granted: true})
	require.NoError(t, err)
	_, err = svc.SubmitConsent(ctx, doctorPrincipal(uuid.New(), doctorID), c.ID, ConsentInput{Type: ConsentRecording, Granted: false})
	require.NoError(t, err)

	// Patient grants -- but the doctor's MOST RECENT decision is "no".
	_, err = svc.SubmitConsent(ctx, patientPrincipal(patientID), c.ID, ConsentInput{Type: ConsentRecording, Granted: true})
	require.NoError(t, err)

	stored, err := st.GetConsultation(ctx, fakePool{}, c.ID)
	require.NoError(t, err)
	require.Equal(t, RecordingNone, stored.RecordingStatus, "the doctor's latest decision was a revocation, recording must not start")
}

// --- waiting room ordering and position calculation -------------------------

func TestWaitingRoom_OrderingAndPosition(t *testing.T) {
	svc, st, _, _ := newTestService(t, Options{DefaultConsultationDuration: 10 * time.Minute})
	doctorID := uuid.New()
	ctx := context.Background()

	var patients []uuid.UUID
	var consultations []*Consultation
	for range 3 {
		patientID := uuid.New()
		c := seedConsultation(t, st, patientID, doctorID)
		_, err := svc.Join(ctx, patientPrincipal(patientID), c.AppointmentID)
		require.NoError(t, err)
		patients = append(patients, patientID)
		consultations = append(consultations, c)
		// Guarantee strictly increasing arrival timestamps regardless of clock
		// resolution, so queue order is deterministic.
		time.Sleep(2 * time.Millisecond)
	}

	for i, c := range consultations {
		status, err := svc.WaitingRoomStatus(ctx, patientPrincipal(patients[i]), c.ID)
		require.NoError(t, err)
		require.True(t, status.Waiting)
		require.Equal(t, i, status.PatientsAhead, "patient %d should have %d patients ahead", i, i)
		require.Equal(t, i+1, status.Position)
		require.Equal(t, i*600, status.EstimatedWaitSeconds, "estimate uses the configured default until real history exists")
	}

	// The doctor admits the first patient; the other two shift up by one.
	_, err := svc.Admit(ctx, doctorPrincipal(uuid.New(), doctorID), consultations[0].ID)
	require.NoError(t, err)

	status, err := svc.WaitingRoomStatus(ctx, patientPrincipal(patients[0]), consultations[0].ID)
	require.NoError(t, err)
	require.False(t, status.Waiting, "an admitted patient is no longer in the waiting room")

	status, err = svc.WaitingRoomStatus(ctx, patientPrincipal(patients[1]), consultations[1].ID)
	require.NoError(t, err)
	require.Equal(t, 0, status.PatientsAhead)

	status, err = svc.WaitingRoomStatus(ctx, patientPrincipal(patients[2]), consultations[2].ID)
	require.NoError(t, err)
	require.Equal(t, 1, status.PatientsAhead)
}

func TestWaitingRoom_FallsBackToPostgresWhenRedisMisses(t *testing.T) {
	svc, st, _, c := newTestService(t, Options{DefaultConsultationDuration: 5 * time.Minute})
	doctorID := uuid.New()
	ctx := context.Background()

	var patients []uuid.UUID
	var consultations []*Consultation
	for range 3 {
		patientID := uuid.New()
		cons := seedConsultation(t, st, patientID, doctorID)
		_, err := svc.Join(ctx, patientPrincipal(patientID), cons.AppointmentID)
		require.NoError(t, err)
		patients = append(patients, patientID)
		consultations = append(consultations, cons)
		time.Sleep(2 * time.Millisecond)
	}

	// Simulate a Redis flush: ZRank can no longer find anyone.
	c.alwaysMissZRank = true

	status, err := svc.WaitingRoomStatus(ctx, patientPrincipal(patients[2]), consultations[2].ID)
	require.NoError(t, err, "a Redis miss must fall back to Postgres, not fail the request")
	require.True(t, status.Waiting)
	require.Equal(t, 2, status.PatientsAhead, "the Postgres mirror still knows the correct position")
}

// --- webhook idempotency ----------------------------------------------------

func TestHandleWebhook_DuplicateDeliveryIsANoOp(t *testing.T) {
	svc, st, video, _ := newTestService(t, Options{})
	patientID, doctorID := uuid.New(), uuid.New()
	c := seedConsultation(t, st, patientID, doctorID)
	ctx := context.Background()

	patientJoins(t, svc, patientID, c)
	_, err := svc.Admit(ctx, doctorPrincipal(uuid.New(), doctorID), c.ID)
	require.NoError(t, err)

	body := []byte(`{"id":"evt-room-finished-1","type":"room_finished","room_name":"` + c.RoomName + `"}`)
	auth := "Bearer " + video.WebhookToken()

	require.NoError(t, svc.HandleWebhook(ctx, auth, body))
	afterFirst, err := st.GetConsultation(ctx, fakePool{}, c.ID)
	require.NoError(t, err)
	require.Equal(t, StatusEnded, afterFirst.Status)
	eventsAfterFirst := len(st.events)

	// Redelivery of the exact same event id must not be processed twice.
	require.NoError(t, svc.HandleWebhook(ctx, auth, body))
	afterSecond, err := st.GetConsultation(ctx, fakePool{}, c.ID)
	require.NoError(t, err)
	require.Equal(t, afterFirst.Version, afterSecond.Version, "a duplicate webhook must not mutate the consultation again")
	require.Equal(t, eventsAfterFirst, len(st.events), "a duplicate webhook must not append a second timeline entry")
}

func TestHandleWebhook_UnverifiedRejected(t *testing.T) {
	svc, _, _, _ := newTestService(t, Options{})
	body := []byte(`{"id":"evt-x","type":"room_finished","room_name":"whatever"}`)
	err := svc.HandleWebhook(context.Background(), "Bearer wrong-token", body)
	require.ErrorIs(t, err, ErrWebhookUnverified)
}

// --- state machine, including the abandoned path ----------------------------

func TestStateMachine_AbandonedWhenNeverStarted(t *testing.T) {
	svc, st, _, _ := newTestService(t, Options{})
	patientID, doctorID := uuid.New(), uuid.New()
	c := seedConsultation(t, st, patientID, doctorID)
	ctx := context.Background()

	// The patient ends the call before the doctor ever admits them.
	updated, err := svc.End(ctx, patientPrincipal(patientID), c.ID, "")
	require.NoError(t, err)
	require.Equal(t, StatusAbandoned, updated.Status)
	require.Nil(t, updated.DurationSeconds)

	var sawAbandoned bool
	for _, e := range st.events {
		if e.ConsultationID == c.ID && e.Type == EventAbandoned {
			sawAbandoned = true
		}
	}
	require.True(t, sawAbandoned, "abandoning must be recorded on the timeline")
}

func TestStateMachine_EndedWhenStarted(t *testing.T) {
	svc, st, _, _ := newTestService(t, Options{})
	patientID, doctorID := uuid.New(), uuid.New()
	c := seedConsultation(t, st, patientID, doctorID)
	ctx := context.Background()

	patientJoins(t, svc, patientID, c)
	_, err := svc.Admit(ctx, doctorPrincipal(uuid.New(), doctorID), c.ID)
	require.NoError(t, err)

	updated, err := svc.End(ctx, doctorPrincipal(uuid.New(), doctorID), c.ID, "completed")
	require.NoError(t, err)
	require.Equal(t, StatusEnded, updated.Status)
	require.NotNil(t, updated.DurationSeconds)
	require.GreaterOrEqual(t, *updated.DurationSeconds, 0)
}

func TestStateMachine_EndIsIdempotentOnTerminalState(t *testing.T) {
	svc, st, _, _ := newTestService(t, Options{})
	patientID, doctorID := uuid.New(), uuid.New()
	c := seedConsultation(t, st, patientID, doctorID)
	ctx := context.Background()

	first, err := svc.End(ctx, patientPrincipal(patientID), c.ID, "")
	require.NoError(t, err)

	second, err := svc.End(ctx, patientPrincipal(patientID), c.ID, "different reason")
	require.NoError(t, err)
	require.Equal(t, first.Status, second.Status)
	require.Equal(t, first.Version, second.Version, "ending an already-terminal consultation must not mutate it again")
}

func TestStateMachine_JoinRejectedOnTerminalConsultation(t *testing.T) {
	svc, st, _, _ := newTestService(t, Options{})
	patientID, doctorID := uuid.New(), uuid.New()
	c := seedConsultation(t, st, patientID, doctorID)
	ctx := context.Background()

	_, err := svc.End(ctx, patientPrincipal(patientID), c.ID, "")
	require.NoError(t, err)

	_, err = svc.Join(ctx, patientPrincipal(patientID), c.AppointmentID)
	require.ErrorIs(t, err, ErrInvalidState)
}

// --- connection quality / low-bandwidth signal ------------------------------

func TestReportQuality_TriggersDowngradeAfterConsecutivePoorSamples(t *testing.T) {
	svc, st, _, _ := newTestService(t, Options{QualityDegradeThreshold: 3})
	patientID, doctorID := uuid.New(), uuid.New()
	c := seedConsultation(t, st, patientID, doctorID)
	ctx := context.Background()
	p := patientPrincipal(patientID)

	for i := range 2 {
		result, err := svc.ReportQuality(ctx, p, c.ID, QualityInput{Quality: QualityPoor})
		require.NoError(t, err)
		require.False(t, result.ShouldDowngradeVideo, "sample %d should not yet trip the threshold", i+1)
	}

	result, err := svc.ReportQuality(ctx, p, c.ID, QualityInput{Quality: QualityPoor})
	require.NoError(t, err)
	require.True(t, result.ShouldDowngradeVideo, "three consecutive poor samples must trigger a downgrade signal")

	var sawDegraded bool
	for _, e := range st.events {
		if e.ConsultationID == c.ID && e.Type == EventQualityDegraded {
			sawDegraded = true
		}
	}
	require.True(t, sawDegraded, "the degradation must be recorded on the timeline for later diagnosis")
}

func TestReportQuality_GoodSampleResetsTheStreak(t *testing.T) {
	svc, st, _, _ := newTestService(t, Options{QualityDegradeThreshold: 3})
	patientID, doctorID := uuid.New(), uuid.New()
	c := seedConsultation(t, st, patientID, doctorID)
	ctx := context.Background()
	p := patientPrincipal(patientID)

	_, err := svc.ReportQuality(ctx, p, c.ID, QualityInput{Quality: QualityPoor})
	require.NoError(t, err)
	_, err = svc.ReportQuality(ctx, p, c.ID, QualityInput{Quality: QualityGood})
	require.NoError(t, err)
	_, err = svc.ReportQuality(ctx, p, c.ID, QualityInput{Quality: QualityPoor})
	require.NoError(t, err)
	result, err := svc.ReportQuality(ctx, p, c.ID, QualityInput{Quality: QualityPoor})
	require.NoError(t, err)
	require.False(t, result.ShouldDowngradeVideo, "the good sample in the middle must break the consecutive-poor streak")
}

// --- appointment lifecycle consumers -----------------------------------

func TestCreateFromAppointment_IsIdempotent(t *testing.T) {
	svc, st, _, _ := newTestService(t, Options{})
	ctx := context.Background()
	in := events.AppointmentConfirmed{
		AppointmentID: uuid.New(), PatientID: uuid.New(), DoctorID: uuid.New(), StartAt: time.Now().UTC(),
	}

	require.NoError(t, svc.CreateFromAppointment(ctx, in))
	require.NoError(t, svc.CreateFromAppointment(ctx, in), "redelivery of the same appointment.confirmed event must not error")

	c, err := st.GetConsultationByAppointment(ctx, fakePool{}, in.AppointmentID)
	require.NoError(t, err)
	require.Equal(t, StatusScheduled, c.Status)
}

func TestTeardownForCancellation_AbandonsUnstartedConsultation(t *testing.T) {
	svc, st, _, _ := newTestService(t, Options{})
	patientID, doctorID := uuid.New(), uuid.New()
	c := seedConsultation(t, st, patientID, doctorID)
	ctx := context.Background()

	require.NoError(t, svc.TeardownForCancellation(ctx, c.AppointmentID))

	updated, err := st.GetConsultation(ctx, fakePool{}, c.ID)
	require.NoError(t, err)
	require.Equal(t, StatusAbandoned, updated.Status)
	require.NotNil(t, updated.DeletedAt)
}

func TestTeardownForCancellation_LeavesActiveCallAlone(t *testing.T) {
	svc, st, _, _ := newTestService(t, Options{})
	patientID, doctorID := uuid.New(), uuid.New()
	c := seedConsultation(t, st, patientID, doctorID)
	ctx := context.Background()

	patientJoins(t, svc, patientID, c)
	_, err := svc.Admit(ctx, doctorPrincipal(uuid.New(), doctorID), c.ID)
	require.NoError(t, err)

	require.NoError(t, svc.TeardownForCancellation(ctx, c.AppointmentID))

	updated, err := st.GetConsultation(ctx, fakePool{}, c.ID)
	require.NoError(t, err)
	require.Equal(t, StatusActive, updated.Status, "a cancellation racing a live call must not tear it down")
}

func TestTeardownForCancellation_UnknownAppointmentIsANoOp(t *testing.T) {
	svc, _, _, _ := newTestService(t, Options{})
	err := svc.TeardownForCancellation(context.Background(), uuid.New())
	require.NoError(t, err)
}

// TestJoin_ReturnsTheConsultationIDTheOtherEndpointsNeed is the regression
// test for the API-surface gap that made recording consent impossible to give.
//
// Join is keyed by APPOINTMENT id; /admit, /end, /consent, /waiting-room and
// /quality are keyed by CONSULTATION id. A client holds an appointment id and
// nothing else, so if Join does not hand back the consultation id, none of
// those five endpoints is reachable -- including the consent endpoint that
// gates recording. This test asserts the join response carries enough for the
// caller to actually make the follow-up calls, and then makes them.
func TestJoin_ReturnsTheConsultationIDTheOtherEndpointsNeed(t *testing.T) {
	svc, st, _, _ := newTestService(t, Options{TokenTTL: 7 * time.Minute})
	patientID, doctorID := uuid.New(), uuid.New()
	c := seedConsultation(t, st, patientID, doctorID)
	ctx := context.Background()

	before := time.Now().UTC()
	res, err := svc.Join(ctx, patientPrincipal(patientID), c.AppointmentID)
	require.NoError(t, err)

	// The identifier the follow-up endpoints are keyed by.
	require.Equal(t, c.ID, res.ConsultationID)
	require.NotEqual(t, c.AppointmentID, res.ConsultationID,
		"appointment id and consultation id are different id spaces; returning the wrong one is the bug this guards")
	require.Equal(t, c.AppointmentID, res.AppointmentID)

	// Enough context to render without a second round trip.
	require.Equal(t, string(RolePatient), res.Role)
	require.Equal(t, StatusWaiting, res.Status, "a patient's first join puts them in the waiting room")
	require.WithinRange(t, res.TokenExpiresAt,
		before.Add(7*time.Minute), time.Now().UTC().Add(7*time.Minute))

	// The existing contract is unchanged.
	require.NotEmpty(t, res.Token)
	require.Equal(t, c.RoomName, res.RoomName)
	require.NotNil(t, res.ICEServers)

	// And the id actually works: consent -- the endpoint that gates recording
	// and was unreachable before -- accepts it.
	_, err = svc.SubmitConsent(ctx, patientPrincipal(patientID), res.ConsultationID, ConsentInput{
		Type: ConsentRecording, Granted: true, IPAddress: "203.0.113.7", UserAgent: "test-agent",
	})
	require.NoError(t, err)

	// As does the doctor's admit.
	doctorRes, err := svc.Join(ctx, doctorPrincipal(uuid.New(), doctorID), c.AppointmentID)
	require.NoError(t, err)
	require.Equal(t, c.ID, doctorRes.ConsultationID)
	require.Equal(t, string(RoleDoctor), doctorRes.Role)

	admitted, err := svc.Admit(ctx, doctorPrincipal(uuid.New(), doctorID), doctorRes.ConsultationID)
	require.NoError(t, err)
	require.Equal(t, StatusActive, admitted.Status)

	// A join after the call is live reports the live status, not a stale one.
	rejoin, err := svc.Join(ctx, patientPrincipal(patientID), c.AppointmentID)
	require.NoError(t, err)
	require.Equal(t, StatusActive, rejoin.Status)
	require.Equal(t, c.ID, rejoin.ConsultationID)
}

func TestChatMessages(t *testing.T) {
	svc, st, _, _ := newTestService(t, Options{})
	ctx := context.Background()

	patientID := uuid.New()
	doctorID := uuid.New()
	doctorUserID := uuid.New()
	c := seedConsultation(t, st, patientID, doctorID)
	patientJoins(t, svc, patientID, c)

	// 1. Patient sends message
	msg1, err := svc.SendMessage(ctx, SendMessageInput{
		ConsultationID: c.ID,
		Caller:         patientPrincipal(patientID),
		Content:        "Hello doctor, can you hear me?",
	})
	require.NoError(t, err)
	require.Equal(t, c.ID, msg1.ConsultationID)
	require.Equal(t, patientID, msg1.SenderID)
	require.Equal(t, RolePatient, msg1.SenderRole)
	require.Equal(t, "Patient", msg1.SenderName)
	require.Equal(t, "Hello doctor, can you hear me?", msg1.Content)

	// 2. Doctor sends message with custom sender name
	msg2, err := svc.SendMessage(ctx, SendMessageInput{
		ConsultationID: c.ID,
		Caller:         doctorPrincipal(doctorUserID, doctorID),
		SenderName:     "Dr. Nimal Perera",
		Content:        "Yes, loud and clear. Starting video now.",
	})
	require.NoError(t, err)
	require.Equal(t, c.ID, msg2.ConsultationID)
	require.Equal(t, doctorUserID, msg2.SenderID)
	require.Equal(t, RoleDoctor, msg2.SenderRole)
	require.Equal(t, "Dr. Nimal Perera", msg2.SenderName)
	require.Equal(t, "Yes, loud and clear. Starting video now.", msg2.Content)

	// 3. Unauthorized user cannot send message
	unauthorizedID := uuid.New()
	_, err = svc.SendMessage(ctx, SendMessageInput{
		ConsultationID: c.ID,
		Caller:         patientPrincipal(unauthorizedID),
		Content:        "I am an intruder",
	})
	require.ErrorIs(t, err, ErrForbidden)

	// 4. Empty message rejected
	_, err = svc.SendMessage(ctx, SendMessageInput{
		ConsultationID: c.ID,
		Caller:         patientPrincipal(patientID),
		Content:        "   ",
	})
	require.ErrorIs(t, err, ErrEmptyMessage)

	// 5. List messages returns all messages in order
	msgs, err := svc.ListMessages(ctx, c.ID, patientPrincipal(patientID), time.Time{}, 50)
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	require.Equal(t, msg1.ID, msgs[0].ID)
	require.Equal(t, msg2.ID, msgs[1].ID)

	// 6. Unauthorized user cannot list messages
	_, err = svc.ListMessages(ctx, c.ID, patientPrincipal(unauthorizedID), time.Time{}, 50)
	require.ErrorIs(t, err, ErrForbidden)

	// 7. Terminal consultation rejects new messages but allows listing
	latest, err := st.GetConsultation(ctx, fakeTx{}, c.ID)
	require.NoError(t, err)
	latest.Status = StatusEnded
	require.NoError(t, st.UpdateConsultation(ctx, fakeTx{}, latest))

	_, err = svc.SendMessage(ctx, SendMessageInput{
		ConsultationID: c.ID,
		Caller:         patientPrincipal(patientID),
		Content:        "Another message after call ended",
	})
	require.ErrorIs(t, err, ErrInvalidState)

	// History is still readable after call ends
	msgsAfterEnd, err := svc.ListMessages(ctx, c.ID, patientPrincipal(patientID), time.Time{}, 50)
	require.NoError(t, err)
	require.Len(t, msgsAfterEnd, 2)
}
