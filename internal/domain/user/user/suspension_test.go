package user

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/rs/zerolog"

	"telemed/internal/platform/events"
	"telemed/internal/platform/middleware"
)

// goldenAdminSuspendCommand is the exact wire format admin-service publishes
// for admin.user_suspend_requested. Pinning bytes rather than asserting
// struct-to-struct is deliberate: both sides now import the same Go type, so
// a struct assertion moves with a renamed json tag and proves nothing. The
// two services cannot import each other's tests, so each pins the same
// literal.
const goldenAdminSuspendCommand = `{` +
	`"user_id":"33333333-3333-4333-8333-333333333333",` +
	`"reason":"repeated no-shows",` +
	`"admin_id":"44444444-4444-4444-8444-444444444444"` +
	`}`

type fakeApplier struct {
	suspended  []events.AdminUserStatusRequested
	reinstated []events.AdminUserStatusRequested
	err        error
}

func (f *fakeApplier) SuspendUser(_ context.Context, cmd events.AdminUserStatusRequested) error {
	f.suspended = append(f.suspended, cmd)
	return f.err
}

func (f *fakeApplier) ReinstateUser(_ context.Context, cmd events.AdminUserStatusRequested) error {
	f.reinstated = append(f.reinstated, cmd)
	return f.err
}

func envelopeFor(t *testing.T, subject events.Subject, payload any) events.Envelope {
	t.Helper()
	env, err := events.NewEnvelope(subject, "telemed-admin-service", "", payload)
	if err != nil {
		t.Fatalf("build envelope: %v", err)
	}
	return env
}

// TestAdminCommand_WireFormatFromAdminService is the contract half of Gap 4.
// Before this, admin-service published a private struct that nothing decoded,
// so the shape had never been checked against a reader at all.
func TestAdminCommand_WireFormatFromAdminService(t *testing.T) {
	var cmd events.AdminUserStatusRequested
	if err := json.Unmarshal([]byte(goldenAdminSuspendCommand), &cmd); err != nil {
		t.Fatalf("decode admin command: %v", err)
	}
	if cmd.UserID != uuid.MustParse("33333333-3333-4333-8333-333333333333") {
		t.Errorf("user_id = %s", cmd.UserID)
	}
	if cmd.AdminID != uuid.MustParse("44444444-4444-4444-8444-444444444444") {
		t.Errorf("admin_id = %s -- the actor is lost and the resulting fact has no author", cmd.AdminID)
	}
	if cmd.Reason != "repeated no-shows" {
		t.Errorf("reason = %q", cmd.Reason)
	}
}

func TestAdminCommandConsumer_Dispatch(t *testing.T) {
	userID, adminID := uuid.New(), uuid.New()
	cmd := events.AdminUserStatusRequested{UserID: userID, Reason: "fraud", AdminID: adminID}

	tests := []struct {
		name           string
		subject        events.Subject
		wantSuspends   int
		wantReinstates int
	}{
		{"suspend command suspends", events.SubjectAdminUserSuspendRequested, 1, 0},
		{"reinstate command reinstates", events.SubjectAdminUserReinstateRequested, 0, 1},
		{"an unrelated subject is ignored, not misapplied", events.SubjectUserRegistered, 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			applier := &fakeApplier{}
			c := NewAdminCommandConsumer(applier, zerolog.Nop())
			if err := c.Handle(context.Background(), envelopeFor(t, tt.subject, cmd)); err != nil {
				t.Fatalf("Handle: %v", err)
			}
			if len(applier.suspended) != tt.wantSuspends {
				t.Errorf("suspend calls = %d, want %d", len(applier.suspended), tt.wantSuspends)
			}
			if len(applier.reinstated) != tt.wantReinstates {
				t.Errorf("reinstate calls = %d, want %d", len(applier.reinstated), tt.wantReinstates)
			}
		})
	}
}

// TestAdminCommandConsumer_UnknownUserIsDroppedNotRetried: a command naming a
// user this service does not have can never succeed on redelivery. Returning
// an error would burn five deliveries and land it in the dead-letter path
// where nobody looks.
func TestAdminCommandConsumer_UnknownUserIsDroppedNotRetried(t *testing.T) {
	for _, sentinel := range []error{ErrUserNotFound, ErrNotFound, ErrNotSuspendable} {
		applier := &fakeApplier{err: sentinel}
		c := NewAdminCommandConsumer(applier, zerolog.Nop())
		env := envelopeFor(t, events.SubjectAdminUserSuspendRequested,
			events.AdminUserStatusRequested{UserID: uuid.New(), AdminID: uuid.New()})
		if err := c.Handle(context.Background(), env); err != nil {
			t.Errorf("Handle with %v should acknowledge, got %v", sentinel, err)
		}
	}
}

// TestAdminCommandConsumer_TransientFailureIsRetried is the other direction:
// a database blip must come back, or an administrator's suspension is lost
// exactly as silently as it was before this loop existed.
func TestAdminCommandConsumer_TransientFailureIsRetried(t *testing.T) {
	boom := errors.New("connection refused")
	c := NewAdminCommandConsumer(&fakeApplier{err: boom}, zerolog.Nop())
	env := envelopeFor(t, events.SubjectAdminUserSuspendRequested,
		events.AdminUserStatusRequested{UserID: uuid.New(), AdminID: uuid.New()})
	if err := c.Handle(context.Background(), env); !errors.Is(err, boom) {
		t.Errorf("Handle = %v, want the underlying error so JetStream redelivers", err)
	}
}

func TestAdminCommandConsumer_MalformedPayload(t *testing.T) {
	c := NewAdminCommandConsumer(&fakeApplier{}, zerolog.Nop())
	env := events.Envelope{
		Subject: events.SubjectAdminUserSuspendRequested,
		Payload: json.RawMessage(`{"user_id": 12345}`),
	}
	if err := c.Handle(context.Background(), env); err == nil {
		t.Error("a payload that cannot be decoded must surface, not be silently swallowed")
	}
}

// TestSuspensionDenylistOutlivesAnAccessToken is the assertion that keeps the
// three suspension mechanisms consistent with each other.
//
// The denylist exists to cover the window in which an access token issued
// before the suspension is still valid. If the token TTL were ever raised past
// the denylist TTL, that window would silently reopen: the key would expire
// while tokens minted before the suspension were still being accepted, and
// nothing would report it. This test fails the build instead.
func TestSuspensionDenylistOutlivesAnAccessToken(t *testing.T) {
	if middleware.SuspensionDenylistTTL <= AccessTokenTTL {
		t.Fatalf("SuspensionDenylistTTL (%s) must exceed AccessTokenTTL (%s): a suspended "+
			"user's already-issued token would outlive the denylist entry that blocks it",
			middleware.SuspensionDenylistTTL, AccessTokenTTL)
	}
}

// TestSuspendedKeyIsStableAcrossServices: user-service writes this key and the
// gateway reads it. They are separate binaries in separate repositories, so
// the only thing keeping them in agreement is the shared helper -- and this
// test, which pins what the helper produces.
func TestSuspendedKeyIsStableAcrossServices(t *testing.T) {
	id := uuid.MustParse("33333333-3333-4333-8333-333333333333")
	const want = "auth:suspended:33333333-3333-4333-8333-333333333333"
	if got := middleware.SuspendedKey(id); got != want {
		t.Errorf("SuspendedKey = %q, want %q -- producer and consumer have drifted and "+
			"the denylist silently does nothing", got, want)
	}
}

// TestSyncSuspensionDenylist covers the cache side effect on its own: the key
// appears on suspension and is gone after reinstatement.
func TestSyncSuspensionDenylist(t *testing.T) {
	ctx := context.Background()
	c := newFakeCache()
	svc := &Service{cache: c, log: zerolog.Nop()}
	userID := uuid.New()

	svc.syncSuspensionDenylist(ctx, userID, StatusSuspended)
	present, err := c.Exists(ctx, middleware.SuspendedKey(userID))
	if err != nil {
		t.Fatalf("Exists: %v", err)
	}
	if !present {
		t.Fatal("suspending a user must put them on the denylist, or an access token " +
			"already in a client's hands keeps working for its full lifetime")
	}

	svc.syncSuspensionDenylist(ctx, userID, StatusActive)
	present, err = c.Exists(ctx, middleware.SuspendedKey(userID))
	if err != nil {
		t.Fatalf("Exists: %v", err)
	}
	if present {
		t.Fatal("reinstating a user must clear the denylist, or they stay locked out")
	}
}

// TestSyncSuspensionDenylistWithoutCacheDoesNotPanic: a deployment with no
// Redis still suspends accounts, just not instantly.
func TestSyncSuspensionDenylistWithoutCacheDoesNotPanic(t *testing.T) {
	svc := &Service{log: zerolog.Nop()}
	svc.syncSuspensionDenylist(context.Background(), uuid.New(), StatusSuspended)
}
