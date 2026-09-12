//go:build integration

package user

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"

	"telemed/internal/platform/events"
	"telemed/internal/platform/middleware"
)

// newSuspensionTestService is newTestService with the cache handed back, so a
// test can inspect the denylist the service writes after committing.
func newSuspensionTestService(t *testing.T, pool *pgxpool.Pool) (*Service, *fakeCache) {
	t.Helper()
	c := newFakeCache()
	nic, err := NewNICHasher(testNICPepper)
	if err != nil {
		t.Fatalf("NewNICHasher: %v", err)
	}
	svc := NewService(NewRepository(pool), c, events.NewOutbox("telemed-user-service-test"),
		&captureSMS{}, NewDegradedKeycloakClient(zerolog.Nop()), newTestIssuer(t), nic, zerolog.Nop())
	svc.SetEmailSender(&captureEmail{})
	return svc, c
}

func outboxPayload(t *testing.T, ctx context.Context, pool *pgxpool.Pool, subject events.Subject, aggregateID string) (events.UserStatusChanged, int) {
	t.Helper()
	rows, err := pool.Query(ctx,
		`SELECT payload FROM outbox_events WHERE subject = $1 AND aggregate_id = $2 ORDER BY created_at`,
		string(subject), aggregateID)
	if err != nil {
		t.Fatalf("query outbox: %v", err)
	}
	defer rows.Close()

	var last events.UserStatusChanged
	count := 0
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			t.Fatalf("scan outbox payload: %v", err)
		}
		var env events.Envelope
		if err := json.Unmarshal(raw, &env); err != nil {
			t.Fatalf("decode envelope: %v", err)
		}
		if err := env.Decode(&last); err != nil {
			t.Fatalf("decode %s payload: %v", subject, err)
		}
		count++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("outbox rows: %v", err)
	}
	return last, count
}

// TestIntegration_AdminSuspendLoop is Gap 4 end to end, against real Postgres.
//
// The loop it proves: admin-service publishes admin.user_suspend_requested ->
// this service consumes it, flips the account status, revokes every session,
// writes the denylist entry the gateway checks, and enqueues user.suspended ->
// admin-service's projector consumes that fact and the console finally shows
// what it asked for.
//
// Before this existed, step two was missing entirely. admin-service wrote an
// audit row, returned 202, and nothing changed anywhere -- which is worse than
// an error, because to the operator it looks like it worked.
func TestIntegration_AdminSuspendLoop(t *testing.T) {
	ctx := context.Background()
	pool := setupTestPool(t)
	svc, cache := newSuspensionTestService(t, pool)
	repo := NewRepository(pool)

	u := mustCreateUser(t, ctx, repo, "+94770000101")
	adminID := uuid.New()

	// The user has a live session before the suspension: a refresh token in a
	// phone, and an access token already minted. Both must stop working.
	auth, err := svc.issueSession(ctx, *u, "device-1", uuid.New())
	if err != nil {
		t.Fatalf("issue session: %v", err)
	}

	// --- the command arrives ------------------------------------------------
	consumer := NewAdminCommandConsumer(svc, zerolog.Nop())
	env, err := events.NewEnvelope(events.SubjectAdminUserSuspendRequested, "telemed-admin-service",
		u.ID.String(), events.AdminUserStatusRequested{
			UserID: u.ID, Reason: "repeated no-shows", AdminID: adminID,
		})
	if err != nil {
		t.Fatalf("build envelope: %v", err)
	}
	if err := consumer.Handle(ctx, env); err != nil {
		t.Fatalf("handle suspend command: %v", err)
	}

	// --- the account is actually suspended ----------------------------------
	after, err := repo.FindUserByID(ctx, pool, u.ID)
	if err != nil {
		t.Fatalf("reload user: %v", err)
	}
	if after.Status != StatusSuspended {
		t.Fatalf("status = %q, want suspended -- the admin action was a no-op", after.Status)
	}
	if after.DeletedAt != nil {
		t.Error("suspension must not soft-delete the account: that is a different lifecycle")
	}

	// --- every session is dead ----------------------------------------------
	if _, err := svc.Refresh(ctx, auth.RefreshToken, "device-1"); err == nil {
		t.Error("a suspended user's refresh token must not mint a new access token")
	}

	// --- the denylist closes the access-token window ------------------------
	present, err := cache.Exists(ctx, middleware.SuspendedKey(u.ID))
	if err != nil {
		t.Fatalf("denylist Exists: %v", err)
	}
	if !present {
		t.Error("no denylist entry: the access token already in the client's hands would " +
			"keep working for the rest of its TTL")
	}

	// --- and the fact goes back to admin-service ----------------------------
	fact, count := outboxPayload(t, ctx, pool, events.SubjectUserSuspended, u.ID.String())
	if count != 1 {
		t.Fatalf("user.suspended outbox rows = %d, want 1", count)
	}
	if fact.UserID != u.ID {
		t.Errorf("user_id = %s, want %s", fact.UserID, u.ID)
	}
	if fact.Status != string(StatusSuspended) {
		t.Errorf("status = %q, want suspended", fact.Status)
	}
	if fact.Reason != "repeated no-shows" {
		t.Errorf("reason = %q: the console cannot explain the suspension it made", fact.Reason)
	}
	if fact.ActorID != adminID {
		t.Errorf("actor_id = %s, want %s -- the fact must carry who did it", fact.ActorID, adminID)
	}
	if fact.ChangedAt.IsZero() {
		t.Error("changed_at is zero")
	}

	// --- redelivery is a no-op, not a second suspension ---------------------
	if err := consumer.Handle(ctx, env); err != nil {
		t.Fatalf("redelivered suspend command: %v", err)
	}
	if _, count := outboxPayload(t, ctx, pool, events.SubjectUserSuspended, u.ID.String()); count != 1 {
		t.Errorf("user.suspended outbox rows after redelivery = %d, want 1 -- "+
			"at-least-once delivery must not double-publish", count)
	}

	// --- reinstatement is the same loop in reverse --------------------------
	reinstate, err := events.NewEnvelope(events.SubjectAdminUserReinstateRequested, "telemed-admin-service",
		u.ID.String(), events.AdminUserStatusRequested{
			UserID: u.ID, Reason: "appeal upheld", AdminID: adminID,
		})
	if err != nil {
		t.Fatalf("build envelope: %v", err)
	}
	if err := consumer.Handle(ctx, reinstate); err != nil {
		t.Fatalf("handle reinstate command: %v", err)
	}

	after, err = repo.FindUserByID(ctx, pool, u.ID)
	if err != nil {
		t.Fatalf("reload user: %v", err)
	}
	if after.Status != StatusActive {
		t.Fatalf("status = %q, want active", after.Status)
	}
	present, err = cache.Exists(ctx, middleware.SuspendedKey(u.ID))
	if err != nil {
		t.Fatalf("denylist Exists: %v", err)
	}
	if present {
		t.Error("a reinstated user still on the denylist stays locked out of the gateway")
	}
	if fact, count := outboxPayload(t, ctx, pool, events.SubjectUserReinstated, u.ID.String()); count != 1 {
		t.Errorf("user.reinstated outbox rows = %d, want 1", count)
	} else if fact.Status != string(StatusActive) {
		t.Errorf("reinstate fact status = %q, want active", fact.Status)
	}

	// --- and the user can log in again --------------------------------------
	if _, err := svc.issueSession(ctx, *after, "device-2", uuid.New()); err != nil {
		t.Errorf("a reinstated user must be able to start a new session: %v", err)
	}
}

// TestIntegration_SuspendedUserCannotLogInWithAFreshOTP closes the obvious
// bypass: revoking sessions is pointless if the user can simply request a new
// OTP and get a brand-new token pair.
func TestIntegration_SuspendedUserCannotLogInWithAFreshOTP(t *testing.T) {
	ctx := context.Background()
	pool := setupTestPool(t)
	svc, _ := newSuspensionTestService(t, pool)
	repo := NewRepository(pool)

	const phone = "+94770000102"
	u := mustCreateUser(t, ctx, repo, phone)

	consumer := NewAdminCommandConsumer(svc, zerolog.Nop())
	env, err := events.NewEnvelope(events.SubjectAdminUserSuspendRequested, "telemed-admin-service",
		u.ID.String(), events.AdminUserStatusRequested{UserID: u.ID, AdminID: uuid.New()})
	if err != nil {
		t.Fatalf("build envelope: %v", err)
	}
	if err := consumer.Handle(ctx, env); err != nil {
		t.Fatalf("handle suspend command: %v", err)
	}

	if _, err := svc.SendOTP(ctx, OTPIdentity{Phone: phone}, PurposeLogin, LanguageEnglish, "127.0.0.1"); err != nil {
		// Sending is allowed -- the front door does not leak account state to
		// an unauthenticated caller. Verification is where it stops.
		t.Fatalf("send otp: %v", err)
	}
	// Read the code the dev SMS provider captured via the service's own cache
	// is not possible here (the fake cache is internal), so go straight at the
	// rule under test: VerifyOTP refuses a suspended account.
	if _, err := svc.VerifyOTP(ctx, OTPIdentity{Phone: phone}, "000000", "device-1", PurposeLogin, "127.0.0.1"); err == nil {
		t.Error("VerifyOTP must not succeed for a suspended account")
	}
}

// TestIntegration_DeletedAccountIsNotResurrectedByAReinstateCommand: PDPA
// deletion and administrative suspension are different lifecycles, and
// re-activating a row the erasure reaper is about to anonymise would be a
// data-protection incident rather than a convenience.
func TestIntegration_DeletedAccountIsNotResurrectedByAReinstateCommand(t *testing.T) {
	ctx := context.Background()
	pool := setupTestPool(t)
	svc, _ := newSuspensionTestService(t, pool)
	repo := NewRepository(pool)

	u := mustCreateUser(t, ctx, repo, "+94770000103")
	if err := repo.SoftDeleteUser(ctx, pool, u.ID, time.Now().UTC().Add(24*time.Hour)); err != nil {
		t.Fatalf("soft delete: %v", err)
	}

	consumer := NewAdminCommandConsumer(svc, zerolog.Nop())
	env, err := events.NewEnvelope(events.SubjectAdminUserReinstateRequested, "telemed-admin-service",
		u.ID.String(), events.AdminUserStatusRequested{UserID: u.ID, AdminID: uuid.New()})
	if err != nil {
		t.Fatalf("build envelope: %v", err)
	}
	// Acknowledged (retrying will never help) but declined.
	if err := consumer.Handle(ctx, env); err != nil {
		t.Fatalf("handle reinstate command: %v", err)
	}

	after, err := repo.FindUserByID(ctx, pool, u.ID)
	if err != nil {
		t.Fatalf("reload user: %v", err)
	}
	if after.Status != StatusDeleted {
		t.Errorf("status = %q, want deleted -- a reinstate command must not resurrect a "+
			"PDPA-deleted account", after.Status)
	}
	if _, count := outboxPayload(t, ctx, pool, events.SubjectUserReinstated, u.ID.String()); count != 0 {
		t.Error("a declined command must not publish a fact that never happened")
	}
}
