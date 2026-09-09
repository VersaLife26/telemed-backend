package users

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"telemed/internal/domain/admin/directory"
	"telemed/internal/platform/events"
)

type fakeStore struct {
	mu       sync.Mutex
	rows     map[uuid.UUID]User
	statuses map[uuid.UUID]string
	events   map[uuid.UUID]uuid.UUID // user -> last applied event id
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		rows:     map[uuid.UUID]User{},
		statuses: map[uuid.UUID]string{},
		events:   map[uuid.UUID]uuid.UUID{},
	}
}

func (f *fakeStore) UpsertFromRegistration(_ context.Context, eventID uuid.UUID, r Registration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if last, ok := f.events[r.UserID]; ok && last == eventID {
		return nil
	}
	status := f.statuses[r.UserID]
	if status == "" {
		status = "active"
	}
	f.rows[r.UserID] = User{
		UserID: r.UserID, FullName: r.FullName, Email: r.Email, Phone: r.Phone,
		Role: r.Role, Status: status, RegisteredAt: r.RegisteredAt,
	}
	f.statuses[r.UserID] = status
	f.events[r.UserID] = eventID
	return nil
}

func (f *fakeStore) SetStatus(_ context.Context, eventID, userID uuid.UUID, status string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if last, ok := f.events[userID]; ok && last == eventID {
		return nil
	}
	row := f.rows[userID]
	row.UserID = userID
	row.Status = status
	f.rows[userID] = row
	f.statuses[userID] = status
	f.events[userID] = eventID
	return nil
}

func (f *fakeStore) get(id uuid.UUID) User {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rows[id]
}

type fakeDirectory struct {
	user directory.User
	err  error
}

func (f fakeDirectory) User(context.Context, uuid.UUID) (directory.User, error) {
	return f.user, f.err
}

func envelopeFor(t *testing.T, subject events.Subject, payload any) events.Envelope {
	t.Helper()
	env, err := events.NewEnvelope(subject, "telemed-user-service", "", payload)
	require.NoError(t, err)
	return env
}

// TestProjector_SuspendReinstateRoundTrip is the RETURN leg of Gap 4's loop.
//
// admin-service publishes admin.user_suspend_requested, user-service applies
// it and publishes user.suspended back, and this projector is what turns that
// fact into what the console shows. Until user-service grew a consumer, the
// fact was never published and this half of the loop had nothing to consume:
// the console's suspend button wrote an audit row and the user row it was
// looking at never changed.
func TestProjector_SuspendReinstateRoundTrip(t *testing.T) {
	st := newFakeStore()
	p := NewProjector(st, fakeDirectory{user: directory.User{
		FullName: "Nimal Silva", Email: "nimal@example.lk", Phone: "+94770000101",
	}}, zerolog.Nop())
	ctx := context.Background()

	userID, adminID := uuid.New(), uuid.New()
	require.NoError(t, p.handle(ctx, envelopeFor(t, events.SubjectUserRegistered, events.UserRegistered{
		UserID: userID, Role: "patient", Language: "en", CreatedAt: time.Now().UTC(),
	})))
	require.Equal(t, "active", st.get(userID).Status)

	// user-service applied the suspension and said so.
	suspended := events.UserStatusChanged{
		UserID: userID, Status: "suspended", Reason: "repeated no-shows",
		ActorID: adminID, ChangedAt: time.Now().UTC(),
	}
	require.NoError(t, p.handle(ctx, envelopeFor(t, events.SubjectUserSuspended, suspended)))
	require.Equal(t, "suspended", st.get(userID).Status,
		"the console still shows the user as active after the suspension was applied")
	require.Equal(t, "Nimal Silva", st.get(userID).FullName, "the status write must not blank the row")

	// And back again.
	require.NoError(t, p.handle(ctx, envelopeFor(t, events.SubjectUserReinstated, events.UserStatusChanged{
		UserID: userID, Status: "active", Reason: "appeal upheld",
		ActorID: adminID, ChangedAt: time.Now().UTC(),
	})))
	require.Equal(t, "active", st.get(userID).Status)
}

// TestProjector_StatusIsReadFromThePayloadNotTheSubject: one canonical type
// backs both subjects and carries the resulting status explicitly, so a
// consumer never has to infer state from a string it filtered on.
func TestProjector_StatusIsReadFromThePayloadNotTheSubject(t *testing.T) {
	st := newFakeStore()
	p := NewProjector(st, fakeDirectory{}, zerolog.Nop())
	userID := uuid.New()

	require.NoError(t, p.handle(context.Background(),
		envelopeFor(t, events.SubjectUserSuspended, events.UserStatusChanged{
			UserID: userID, Status: "suspended", ChangedAt: time.Now().UTC(),
		})))
	require.Equal(t, "suspended", st.get(userID).Status)
}

// TestProjector_StatusFallsBackToTheSubjectWhenThePayloadOmitsIt keeps an
// older producer working: absent status means infer it, not write "".
func TestProjector_StatusFallsBackToTheSubjectWhenThePayloadOmitsIt(t *testing.T) {
	tests := []struct {
		subject events.Subject
		want    string
	}{
		{events.SubjectUserSuspended, "suspended"},
		{events.SubjectUserReinstated, "active"},
	}
	for _, tt := range tests {
		st := newFakeStore()
		p := NewProjector(st, fakeDirectory{}, zerolog.Nop())
		userID := uuid.New()
		env := envelopeFor(t, tt.subject, map[string]any{"user_id": userID})
		require.NoError(t, p.handle(context.Background(), env))
		require.Equal(t, tt.want, st.get(userID).Status)
	}
}

// TestUserStatusChangedWireCompatibility decodes the exact bytes user-service
// publishes. The two services cannot import each other's tests, so each pins
// the same literal.
func TestUserStatusChangedWireCompatibility(t *testing.T) {
	const goldenFromUserService = `{` +
		`"user_id":"33333333-3333-4333-8333-333333333333",` +
		`"status":"suspended",` +
		`"reason":"repeated no-shows",` +
		`"actor_id":"44444444-4444-4444-8444-444444444444",` +
		`"changed_at":"2026-08-20T09:30:00Z"` +
		`}`

	var got events.UserStatusChanged
	require.NoError(t, json.Unmarshal([]byte(goldenFromUserService), &got))
	require.Equal(t, uuid.MustParse("33333333-3333-4333-8333-333333333333"), got.UserID)
	require.Equal(t, "suspended", got.Status)
	require.Equal(t, "repeated no-shows", got.Reason)
	require.Equal(t, uuid.MustParse("44444444-4444-4444-8444-444444444444"), got.ActorID)
	require.Equal(t, time.Date(2026, 8, 20, 9, 30, 0, 0, time.UTC), got.ChangedAt)
}

// TestAdminCommandWireFormat pins what THIS service publishes, so the
// matching literal in user-service's suspension_test.go cannot drift away
// from it unnoticed.
func TestAdminCommandWireFormat(t *testing.T) {
	raw, err := json.Marshal(events.AdminUserStatusRequested{
		UserID:  uuid.MustParse("33333333-3333-4333-8333-333333333333"),
		Reason:  "repeated no-shows",
		AdminID: uuid.MustParse("44444444-4444-4444-8444-444444444444"),
	})
	require.NoError(t, err)

	const golden = `{` +
		`"user_id":"33333333-3333-4333-8333-333333333333",` +
		`"reason":"repeated no-shows",` +
		`"admin_id":"44444444-4444-4444-8444-444444444444"` +
		`}`
	require.Equal(t, golden, string(raw),
		"admin.user_suspend_requested wire format drifted; user-service decodes these exact bytes")
}
