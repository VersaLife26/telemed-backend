package consultation

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"telemed/internal/platform/events"
)

// sweepVideo wraps MockProvider so a test can say what the provider reports
// about a room without reaching into production types. Only ListParticipants
// matters to the sweeper.
type sweepVideo struct {
	*MockProvider
	listErr error
	rooms   map[string][]ProviderParticipant
}

func newSweepVideo() *sweepVideo {
	return &sweepVideo{MockProvider: NewMockProvider("test-webhook-token"), rooms: map[string][]ProviderParticipant{}}
}

func (v *sweepVideo) ListParticipants(ctx context.Context, roomName string) ([]ProviderParticipant, error) {
	if v.listErr != nil {
		return nil, v.listErr
	}
	if ps, ok := v.rooms[roomName]; ok {
		return ps, nil
	}
	// Not registered by the test: the room is gone, which is what LiveKit
	// reports once its empty-room timeout has torn it down.
	return v.MockProvider.ListParticipants(ctx, roomName)
}

// roomEmptyButPresent is the room still existing with nobody in it.
func (v *sweepVideo) roomEmptyButPresent(name string) { v.rooms[name] = nil }

// roomOccupied is somebody genuinely connected.
func (v *sweepVideo) roomOccupied(name, identity string) {
	v.rooms[name] = []ProviderParticipant{{Identity: identity, JoinedAt: time.Now().UTC(), ConnectionState: "ACTIVE"}}
}

// providerDown is the outage that most likely lost the webhook too.
func (v *sweepVideo) providerDown(err error) { v.listErr = err }

// newSweepService builds a Service whose video provider the test controls.
func newSweepService(t *testing.T) (*Service, *fakeStore, *sweepVideo) {
	t.Helper()
	st := newFakeStore()
	video := newSweepVideo()
	svc := NewService(st, video, newFakeCache(), fakePool{},
		events.NewOutbox("test-consultation-service"), zerolog.Nop(), Options{})
	return svc, st, video
}

// activeConsultation drives a consultation to 'active' the way the real flow
// does -- patient joins, doctor admits -- then backdates it so the sweeper's
// idle window has elapsed.
func activeConsultation(t *testing.T, svc *Service, st *fakeStore, startedAgo time.Duration) *Consultation {
	t.Helper()
	patientID, doctorID := uuid.New(), uuid.New()
	c := seedConsultation(t, st, patientID, doctorID)
	ctx := context.Background()
	patientJoins(t, svc, patientID, c)
	_, err := svc.Admit(ctx, doctorPrincipal(uuid.New(), doctorID), c.ID)
	require.NoError(t, err)

	stored, err := st.GetConsultation(ctx, fakePool{}, c.ID)
	require.NoError(t, err)
	require.Equal(t, StatusActive, stored.Status)

	backdate(t, st, stored.ID, time.Now().UTC().Add(-startedAgo))
	return stored
}

// backdate rewrites started_at and every timeline timestamp so the row looks
// as it would after the elapsed time, without a test sleeping for it.
func backdate(t *testing.T, st *fakeStore, id uuid.UUID, to time.Time) {
	t.Helper()
	st.mu.Lock()
	defer st.mu.Unlock()
	c := st.consultations[id]
	require.NotNil(t, c)
	c.StartedAt = &to
	for _, p := range st.participants[id] {
		if p.JoinedAt != nil {
			p.JoinedAt = &to
		}
		if p.LeftAt != nil {
			p.LeftAt = &to
		}
	}
	for i := range st.events {
		if st.events[i].ConsultationID == id {
			st.events[i].OccurredAt = to
		}
	}
}

// The case the sweeper exists for: both clients died, LiveKit tore the room
// down, and the room_finished webhook never reached us. Without the sweeper
// the row stays 'active' forever -- consultation.ended is never published, so
// scheduling never completes the appointment and record-service never learns
// when care ended.
func TestSweeper_EndsAnActiveConsultationWhoseRoomIsGone(t *testing.T) {
	svc, st, video := newSweepService(t)
	ctx := context.Background()

	c := activeConsultation(t, svc, st, 3*time.Hour)
	// LiveKit's empty-room timeout already deleted the room; we never heard.
	video.roomEmptyButPresent(c.RoomName)

	swept, err := svc.SweepStaleActive(ctx, DefaultIdleTimeout, DefaultSweepBatch)
	require.NoError(t, err)
	require.Equal(t, 1, swept, "a consultation whose room no longer exists must be closed")

	after, err := st.GetConsultation(ctx, fakePool{}, c.ID)
	require.NoError(t, err)
	require.Equal(t, StatusEnded, after.Status)
	require.NotNil(t, after.EndReason)
	require.Equal(t, EndReasonStale, *after.EndReason)
	require.NotNil(t, after.EndedAt)

	// Dated at the last evidence of activity, not at the sweep. A webhook lost
	// overnight must not be recorded as a nine-hour consultation.
	require.NotNil(t, after.DurationSeconds)
	require.Less(t, *after.DurationSeconds, 60,
		"duration must come from the consultation's own timeline, not from how long the webhook was missing")
}

// A live consultation must survive the sweeper. The database cannot tell a
// long call from an abandoned one -- once both parties have joined, a healthy
// call writes no rows -- so the provider is the decider, and this is the test
// that says so.
func TestSweeper_LeavesAConsultationSomebodyIsStillIn(t *testing.T) {
	svc, st, video := newSweepService(t)
	ctx := context.Background()

	c := activeConsultation(t, svc, st, 6*time.Hour)
	video.roomOccupied(c.RoomName, c.PatientID.String())

	swept, err := svc.SweepStaleActive(ctx, DefaultIdleTimeout, DefaultSweepBatch)
	require.NoError(t, err)
	require.Zero(t, swept, "a consultation with a participant still connected must never be ended by the sweeper")

	after, err := st.GetConsultation(ctx, fakePool{}, c.ID)
	require.NoError(t, err)
	require.Equal(t, StatusActive, after.Status)
}

// A provider outage is the most likely reason the webhook went missing in the
// first place. Silence from the provider must never be read as "the room is
// empty" -- failing to sweep costs one interval, ending a live consultation
// costs a consultation.
func TestSweeper_ProviderErrorIsNotEvidenceOfAnEmptyRoom(t *testing.T) {
	svc, st, video := newSweepService(t)
	ctx := context.Background()

	c := activeConsultation(t, svc, st, 6*time.Hour)
	video.providerDown(errors.New("livekit: connection refused"))

	swept, err := svc.SweepStaleActive(ctx, DefaultIdleTimeout, DefaultSweepBatch)
	require.NoError(t, err, "a provider outage must not fail the whole sweep")
	require.Zero(t, swept)

	after, err := st.GetConsultation(ctx, fakePool{}, c.ID)
	require.NoError(t, err)
	require.Equal(t, StatusActive, after.Status,
		"the sweeper ended a consultation on the strength of an unreachable video provider")
}

// The idle threshold has to actually bite. A consultation that started twenty
// minutes ago is a doctor still working, not a stale row -- even with an empty
// room, which is exactly what "the patient has left and the doctor is writing
// their notes" looks like.
func TestSweeper_RespectsTheIdleThreshold(t *testing.T) {
	svc, st, video := newSweepService(t)
	ctx := context.Background()

	c := activeConsultation(t, svc, st, 20*time.Minute)
	video.roomEmptyButPresent(c.RoomName)

	swept, err := svc.SweepStaleActive(ctx, DefaultIdleTimeout, DefaultSweepBatch)
	require.NoError(t, err)
	require.Zero(t, swept, "20 minutes is inside every legitimate reason a room is briefly empty")

	after, err := st.GetConsultation(ctx, fakePool{}, c.ID)
	require.NoError(t, err)
	require.Equal(t, StatusActive, after.Status)

	// And it does get swept once genuinely idle.
	backdate(t, st, c.ID, time.Now().UTC().Add(-DefaultIdleTimeout-time.Minute))
	swept, err = svc.SweepStaleActive(ctx, DefaultIdleTimeout, DefaultSweepBatch)
	require.NoError(t, err)
	require.Equal(t, 1, swept)
}

// A recent quality sample is the only heartbeat a two-party call in progress
// produces, so it has to count as activity even when started_at is old.
func TestSweeper_ARecentQualitySampleKeepsAConsultationAlive(t *testing.T) {
	svc, st, video := newSweepService(t)
	ctx := context.Background()

	c := activeConsultation(t, svc, st, 5*time.Hour)
	video.roomEmptyButPresent(c.RoomName)

	// The call is still going: a sample landed a minute ago.
	require.NoError(t, st.RecordEvent(ctx, fakeTx{}, Event{
		ConsultationID: c.ID, Type: EventQualitySample, OccurredAt: time.Now().UTC().Add(-time.Minute),
	}))

	swept, err := svc.SweepStaleActive(ctx, DefaultIdleTimeout, DefaultSweepBatch)
	require.NoError(t, err)
	require.Zero(t, swept, "a consultation posting quality samples a minute ago is live, whatever started_at says")
}

// Ending is idempotent and terminal rows are never revisited.
func TestSweeper_IgnoresConsultationsThatAlreadyEnded(t *testing.T) {
	svc, st, video := newSweepService(t)
	ctx := context.Background()

	c := activeConsultation(t, svc, st, 6*time.Hour)
	video.roomEmptyButPresent(c.RoomName)

	swept, err := svc.SweepStaleActive(ctx, DefaultIdleTimeout, DefaultSweepBatch)
	require.NoError(t, err)
	require.Equal(t, 1, swept)

	swept, err = svc.SweepStaleActive(ctx, DefaultIdleTimeout, DefaultSweepBatch)
	require.NoError(t, err)
	require.Zero(t, swept, "a swept consultation must not be swept again")
}

// The provider reporting the room does not exist at all -- LiveKit's own
// empty-room timeout tore it down and the room_finished webhook that should
// have told us was lost. This is the literal case the sweeper was built for.
func TestSweeper_EndsWhenTheProviderSaysTheRoomIsGone(t *testing.T) {
	svc, st, video := newSweepService(t)
	ctx := context.Background()

	c := activeConsultation(t, svc, st, 4*time.Hour)
	require.NoError(t, video.EndRoom(ctx, c.RoomName))

	swept, err := svc.SweepStaleActive(ctx, DefaultIdleTimeout, DefaultSweepBatch)
	require.NoError(t, err)
	require.Equal(t, 1, swept)

	after, err := st.GetConsultation(ctx, fakePool{}, c.ID)
	require.NoError(t, err)
	require.Equal(t, StatusEnded, after.Status)
	require.Equal(t, EndReasonStale, *after.EndReason)
}
