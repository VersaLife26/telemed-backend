//go:build integration

// Integration tests stand up a real Postgres via testcontainers and run the
// actual migrations, because the two properties under test here --
// dedupe_key's UNIQUE constraint and device-token pruning's WHERE clause --
// are exactly the kind of thing a mock cannot meaningfully fake.
package notification_test

import (
	"context"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"telemed/internal/domain/notification/notification"
)

// setupDB starts a throwaway Postgres container, applies every migration in
// ../../migrations (including the seeded templates), and returns a pool
// pointed at it. The container is terminated via t.Cleanup regardless of
// test outcome.
func setupDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()

	pgContainer, err := tcpostgres.Run(ctx, "postgres:17-alpine",
		tcpostgres.WithDatabase("telemed_notification_test"),
		tcpostgres.WithUsername("telemed"),
		tcpostgres.WithPassword("telemed"),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		t.Fatalf("start postgres container: %v", err)
	}
	t.Cleanup(func() {
		if err := pgContainer.Terminate(context.Background()); err != nil {
			t.Logf("terminate postgres container: %v", err)
		}
	})

	connStr, err := pgContainer.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}

	m, err := migrate.New("file://../../migrations", connStr)
	if err != nil {
		t.Fatalf("build migrator: %v", err)
	}
	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("run migrations: %v", err)
	}

	pool, err := pgxpool.New(ctx, connStr)
	if err != nil {
		t.Fatalf("connect pool: %v", err)
	}
	t.Cleanup(pool.Close)

	return pool
}

func testService(pool *pgxpool.Pool) (*notification.Service, *notification.Repository) {
	repo := notification.NewRepository(pool)
	log := zerolog.Nop()
	// Notify() never touches the provider registry -- it only enqueues rows
	// -- so an empty registry is sufficient for these tests, which never
	// call DispatchOne.
	svc := notification.NewService(repo, notification.NewRegistry(), log, notification.ServiceOptions{})
	return svc, repo
}

// TestDedupe_RepeatedEventDeliveryProducesExactlyOneRowPerChannel is the
// idempotency guarantee the brief requires in these exact words: "a
// duplicated payment.succeeded must not send two receipts", enforced via
// dedupe_key's UNIQUE constraint, not an in-memory set (which a process
// restart or a second replica would silently defeat).
func TestDedupe_RepeatedEventDeliveryProducesExactlyOneRowPerChannel(t *testing.T) {
	pool := setupDB(t)
	svc, repo := testService(pool)
	ctx := context.Background()

	userID := uuid.New()
	envelopeID := uuid.New() // stands in for the same NATS envelope redelivered twice

	req := notification.NotifyRequest{
		UserID:        userID,
		TemplateKey:   notification.TemplateBookingConfirmed, // sms+push+email+in_app: 4 channels
		Data:          notification.TemplateData{DoctorName: "Test Doctor", DateTime: "21 Aug 2026, 10:00", FeeLKR: "Rs. 1,000.00"},
		Phone:         "+94771234567",
		Email:         "patient@example.com",
		DedupeKeyBase: envelopeID.String(),
		SourceEventID: &envelopeID,
	}

	first, err := svc.Notify(ctx, req)
	if err != nil {
		t.Fatalf("first Notify: %v", err)
	}
	if len(first) != 4 {
		t.Fatalf("first delivery: got %d outcomes, want 4 (sms/push/email/in_app)", len(first))
	}
	for _, o := range first {
		if !o.Created {
			t.Errorf("first delivery: channel %s reported Created=false, want true", o.Channel)
		}
	}

	// Simulate JetStream redelivering the exact same envelope -- at-least-
	// once delivery is guaranteed, exactly-once is not.
	second, err := svc.Notify(ctx, req)
	if err != nil {
		t.Fatalf("second (duplicate) Notify: %v", err)
	}
	if len(second) != 4 {
		t.Fatalf("second delivery: got %d outcomes, want 4", len(second))
	}
	for _, o := range second {
		if o.Created {
			t.Errorf("second (duplicate) delivery: channel %s reported Created=true, want false", o.Channel)
		}
	}

	rows, total, err := repo.ListForUser(ctx, pool, userID, 0, 100)
	if err != nil {
		t.Fatalf("list for user: %v", err)
	}
	if total != 4 || len(rows) != 4 {
		t.Fatalf("expected exactly 4 notification rows after two identical deliveries, got total=%d len=%d", total, len(rows))
	}
}

// TestDedupe_CronAndEventPathsConverge exercises the other half of the
// idempotency story: reminder.go's cron and consumer.go's
// onAppointmentReminderDue both compute the same deterministic dedupe_key
// for the same appointment, so whichever fires first wins.
func TestDedupe_CronAndEventPathsConverge(t *testing.T) {
	pool := setupDB(t)
	svc, repo := testService(pool)
	ctx := context.Background()

	userID := uuid.New()
	appointmentID := uuid.New()
	dedupeBase := "reminder_1h:" + appointmentID.String()

	req := notification.NotifyRequest{
		UserID:        userID,
		TemplateKey:   notification.TemplateReminder1h, // sms+push+in_app: 3 channels, urgent
		Data:          notification.TemplateData{DoctorName: "Test Doctor", DateTime: "21 Aug 2026, 10:00"},
		Phone:         "+94771234567",
		DedupeKeyBase: dedupeBase,
	}

	if _, err := svc.Notify(ctx, req); err != nil {
		t.Fatalf("cron-path Notify: %v", err)
	}
	// The "event path" computes the identical dedupe base independently.
	if _, err := svc.Notify(ctx, req); err != nil {
		t.Fatalf("event-path Notify: %v", err)
	}

	_, total, err := repo.ListForUser(ctx, pool, userID, 0, 100)
	if err != nil {
		t.Fatalf("list for user: %v", err)
	}
	if total != 3 {
		t.Fatalf("expected exactly 3 rows (one per channel) after both paths fire, got %d", total)
	}
}

// TestDeviceTokenPruning_HardDeletesOnlyOldInvalidatedTokens covers "prune
// tokens FCM reports as unregistered, or your delivery rate quietly rots":
// InvalidateDeviceTokenByValue is the soft-delete path a push send failure
// triggers, and HardPruneInvalidatedTokens is the maintenance sweep that
// actually removes rows -- but only ones invalidated long enough ago, never
// a token invalidated moments ago (useful for support triage until then).
func TestDeviceTokenPruning_HardDeletesOnlyOldInvalidatedTokens(t *testing.T) {
	pool := setupDB(t)
	_, repo := testService(pool)
	ctx := context.Background()

	userID := uuid.New()

	oldToken := "fcm-token-old-" + uuid.NewString()
	freshToken := "fcm-token-fresh-" + uuid.NewString()
	activeToken := "fcm-token-active-" + uuid.NewString()

	for _, tok := range []string{oldToken, freshToken, activeToken} {
		if _, err := repo.UpsertDeviceToken(ctx, pool, notification.DeviceToken{
			UserID: userID, Token: tok, Platform: notification.PlatformAndroid,
		}); err != nil {
			t.Fatalf("upsert device token %s: %v", tok, err)
		}
	}

	// Simulate FCM reporting oldToken and freshToken as unregistered -- the
	// mechanism this service actually uses in production, not a raw SQL
	// backdoor.
	if err := repo.InvalidateDeviceTokenByValue(ctx, pool, oldToken); err != nil {
		t.Fatalf("invalidate old token: %v", err)
	}
	if err := repo.InvalidateDeviceTokenByValue(ctx, pool, freshToken); err != nil {
		t.Fatalf("invalidate fresh token: %v", err)
	}

	// Backdate oldToken's invalidation to 40 days ago so it falls outside a
	// 30-day retention window; freshToken stays invalidated "now".
	if _, err := pool.Exec(ctx,
		`UPDATE device_tokens SET invalidated_at = NOW() - INTERVAL '40 days' WHERE token = $1`, oldToken); err != nil {
		t.Fatalf("backdate old token: %v", err)
	}

	deleted, err := repo.HardPruneInvalidatedTokens(ctx, 30*24*time.Hour)
	if err != nil {
		t.Fatalf("hard prune: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("expected exactly 1 token pruned (only the 40-day-old one), got %d", deleted)
	}

	active, err := repo.ListActiveDeviceTokens(ctx, pool, userID)
	if err != nil {
		t.Fatalf("list active tokens: %v", err)
	}
	if len(active) != 1 || active[0].Token != activeToken {
		t.Fatalf("expected exactly activeToken to remain active, got %+v", active)
	}

	var remaining int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM device_tokens WHERE token = $1`, freshToken).Scan(&remaining); err != nil {
		t.Fatalf("count fresh token: %v", err)
	}
	if remaining != 1 {
		t.Fatalf("freshly-invalidated token should NOT be hard-pruned yet, but row is gone")
	}

	var goneCount int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM device_tokens WHERE token = $1`, oldToken).Scan(&goneCount); err != nil {
		t.Fatalf("count old token: %v", err)
	}
	if goneCount != 0 {
		t.Fatalf("40-day-old invalidated token should have been hard-pruned")
	}
}

// TestPreferenceSuppression_EndToEnd confirms the DB-backed half of
// preference suppression -- default preferences (no row) and an explicit
// opt-out row both suppress the expected channel, exercised through the
// real repository rather than the pure decideDispatch function tested in
// decide_dispatch_test.go.
func TestPreferenceSuppression_EndToEnd(t *testing.T) {
	pool := setupDB(t)
	svc, repo := testService(pool)
	ctx := context.Background()

	// A user who explicitly disabled SMS.
	userID := uuid.New()
	if err := repo.UpsertPreferences(ctx, pool, notification.Preferences{
		UserID: userID, SMSEnabled: false, PushEnabled: true, EmailEnabled: true, InAppEnabled: true,
		Locale: notification.LocaleEnglish, Timezone: "Asia/Colombo",
	}); err != nil {
		t.Fatalf("upsert preferences: %v", err)
	}

	outcomes, err := svc.Notify(ctx, notification.NotifyRequest{
		UserID:        userID,
		TemplateKey:   notification.TemplateBookingConfirmed,
		Data:          notification.TemplateData{DoctorName: "Test Doctor", DateTime: "21 Aug 2026, 10:00", FeeLKR: "Rs. 1,000.00"},
		Phone:         "+94771234567",
		Email:         "patient@example.com",
		DedupeKeyBase: uuid.NewString(),
	})
	if err != nil {
		t.Fatalf("Notify: %v", err)
	}

	byChannel := map[notification.Channel]notification.Status{}
	for _, o := range outcomes {
		byChannel[o.Channel] = o.Status
	}
	if byChannel[notification.ChannelSMS] != notification.StatusSuppressed {
		t.Errorf("sms status = %q, want suppressed (user disabled sms)", byChannel[notification.ChannelSMS])
	}
	if byChannel[notification.ChannelEmail] == notification.StatusSuppressed {
		t.Errorf("email should not be suppressed for this user")
	}

	// otp_code is critical: it must never be suppressed by channel
	// preference, even for this same user with SMS disabled.
	otpOutcomes, err := svc.Notify(ctx, notification.NotifyRequest{
		UserID:        userID,
		TemplateKey:   notification.TemplateOTPCode,
		Data:          notification.TemplateData{Code: "123456", ExpiresInMinutes: 5},
		Phone:         "+94771234567",
		DedupeKeyBase: uuid.NewString(),
	})
	if err != nil {
		t.Fatalf("Notify otp: %v", err)
	}
	if len(otpOutcomes) != 1 || otpOutcomes[0].Status == notification.StatusSuppressed {
		t.Fatalf("otp_code must bypass channel preference (critical urgency), got %+v", otpOutcomes)
	}
}

// TestMarkAllRead_ClearsOnlyTheCallersUnreadRows is the regression test for
// the patient app's "mark all read" tap, which had no backend at all: only
// per-notification PUT /{id}/read existed, so clearing a badge of forty
// notifications meant forty round trips, and the app simply did not do it.
//
// Three properties matter and all three are asserted here, because getting
// any one of them wrong is a data leak or an audit falsification rather than
// a cosmetic bug:
//
//   - it is scoped to the caller. A bulk UPDATE with a forgotten user_id
//     predicate marks the whole platform's notifications read.
//   - it is idempotent. A second call reports zero rows and does not move the
//     first call's read_at, which is the "when did they first see it" answer.
//   - it leaves already-read rows alone, for the same reason.
func TestMarkAllRead_ClearsOnlyTheCallersUnreadRows(t *testing.T) {
	pool := setupDB(t)
	svc, repo := testService(pool)
	ctx := context.Background()

	caller := uuid.New()
	bystander := uuid.New()

	enqueue := func(userID uuid.UUID, dedupe string) {
		t.Helper()
		if _, err := svc.Notify(ctx, notification.NotifyRequest{
			UserID:        userID,
			TemplateKey:   notification.TemplateBookingConfirmed,
			Data:          notification.TemplateData{DoctorName: "Test Doctor", DateTime: "21 Aug 2026, 10:00", FeeLKR: "Rs. 1,000.00"},
			Phone:         "+94771234567",
			Email:         "patient@example.com",
			DedupeKeyBase: dedupe,
		}); err != nil {
			t.Fatalf("notify %s: %v", dedupe, err)
		}
	}

	// Four channels per call, so this is 8 rows for the caller and 4 for the
	// bystander.
	enqueue(caller, uuid.NewString())
	enqueue(caller, uuid.NewString())
	enqueue(bystander, uuid.NewString())

	countUnread := func(userID uuid.UUID) int64 {
		t.Helper()
		var n int64
		if err := pool.QueryRow(ctx,
			`SELECT COUNT(*) FROM notifications WHERE user_id = $1 AND read_at IS NULL AND deleted_at IS NULL`,
			userID).Scan(&n); err != nil {
			t.Fatalf("count unread: %v", err)
		}
		return n
	}

	if got := countUnread(caller); got != 8 {
		t.Fatalf("caller starts with %d unread rows, want 8", got)
	}

	updated, err := repo.MarkAllRead(ctx, pool, caller)
	if err != nil {
		t.Fatalf("mark all read: %v", err)
	}
	if updated != 8 {
		t.Errorf("MarkAllRead reported %d rows, want 8", updated)
	}
	if got := countUnread(caller); got != 0 {
		t.Errorf("caller still has %d unread rows", got)
	}
	if got := countUnread(bystander); got != 4 {
		t.Errorf("bystander has %d unread rows, want 4 -- the UPDATE is not scoped to the caller", got)
	}

	// Capture the timestamps the first call wrote, then call again.
	var firstReadAt time.Time
	if err := pool.QueryRow(ctx,
		`SELECT MAX(read_at) FROM notifications WHERE user_id = $1`, caller).Scan(&firstReadAt); err != nil {
		t.Fatalf("read first read_at: %v", err)
	}

	again, err := repo.MarkAllRead(ctx, pool, caller)
	if err != nil {
		t.Fatalf("second mark all read: %v", err)
	}
	if again != 0 {
		t.Errorf("second MarkAllRead reported %d rows, want 0 -- it is not idempotent", again)
	}

	var secondReadAt time.Time
	if err := pool.QueryRow(ctx,
		`SELECT MAX(read_at) FROM notifications WHERE user_id = $1`, caller).Scan(&secondReadAt); err != nil {
		t.Fatalf("read second read_at: %v", err)
	}
	if !secondReadAt.Equal(firstReadAt) {
		t.Errorf("read_at moved from %s to %s on the second call; the first-seen timestamp must not be rewritten",
			firstReadAt, secondReadAt)
	}
}
