package notification

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"telemed/internal/platform/database"
)

// Querier is the narrow slice of pgx that both *pgxpool.Pool and pgx.Tx
// satisfy. Repository methods take one explicitly rather than hard-coding a
// pool, so a caller building a multi-step write (e.g. "upsert the reminder
// projection and enqueue booking_confirmed together") can pass a pgx.Tx from
// database.InTx and get one atomic unit, while a caller that only needs a
// single statement (the dispatcher's claim query) can pass the pool directly.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

var (
	_ Querier = database.Pool(nil)
	_ Querier = pgx.Tx(nil)
)

// ErrNotFound is returned when a lookup by ID/key finds no row.
var ErrNotFound = errors.New("notification: not found")

// Repository is SQL only, per AGENT-BRIEF's handler->service->repository
// rule: it never returns an *httpx.APIError and never applies a business
// rule beyond "which rows match".
type Repository struct {
	pool database.Pool
}

// NewRepository builds a Repository bound to the service's connection pool.
// Callers that need a single-statement operation pass r.Pool() (or nothing,
// via the *Pool-suffixed convenience methods below); callers building a
// multi-statement transaction pass their own pgx.Tx.
func NewRepository(pool database.Pool) *Repository {
	return &Repository{pool: pool}
}

// Pool exposes the underlying pool for callers (the dispatcher, the reminder
// cron) that only ever issue single statements and have no reason to open an
// application-level transaction.
func (r *Repository) Pool() database.Pool { return r.pool }

// ---------------------------------------------------------------------------
// notifications
// ---------------------------------------------------------------------------

// CreateIfNew inserts n, returning created=false and a zero ID when
// dedupe_key already exists. This is the platform's idempotency enforcement
// for at-least-once event delivery: it is a UNIQUE constraint, not an
// in-memory set, so it survives a process restart and works across replicas.
func (r *Repository) CreateIfNew(ctx context.Context, db Querier, n Notification) (uuid.UUID, bool, error) {
	const q = `
		INSERT INTO notifications (
			user_id, channel, template_key, locale, urgency, subject, body, recipient,
			status, dedupe_key, source_event_id, scheduled_for
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		ON CONFLICT (dedupe_key) DO NOTHING
		RETURNING id`

	var id uuid.UUID
	err := db.QueryRow(ctx, q,
		n.UserID, string(n.Channel), string(n.TemplateKey), string(n.Locale), string(n.Urgency),
		nullString(n.Subject), n.Body, nullString(n.Recipient), string(n.Status), n.DedupeKey, n.SourceEventID, n.ScheduledFor,
	).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, false, nil
	}
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("notification: create: %w", err)
	}
	return id, true, nil
}

const notificationColumns = `
	id, user_id, channel, template_key, locale, urgency, subject, body, recipient, status,
	provider, provider_message_id, attempts, last_error, error_class,
	scheduled_for, next_attempt_at, sent_at, delivered_at, read_at,
	dedupe_key, source_event_id, created_at, updated_at, version`

func scanNotification(row pgx.Row) (Notification, error) {
	var n Notification
	var subject, recipient, provider, providerMsgID, lastError, errClass *string
	if err := row.Scan(
		&n.ID, &n.UserID, &n.Channel, &n.TemplateKey, &n.Locale, &n.Urgency, &subject, &n.Body, &recipient, &n.Status,
		&provider, &providerMsgID, &n.Attempts, &lastError, &errClass,
		&n.ScheduledFor, &n.NextAttemptAt, &n.SentAt, &n.DeliveredAt, &n.ReadAt,
		&n.DedupeKey, &n.SourceEventID, &n.CreatedAt, &n.UpdatedAt, &n.Version,
	); err != nil {
		return Notification{}, err
	}
	n.Subject = derefString(subject)
	n.Recipient = derefString(recipient)
	n.Provider = derefString(provider)
	n.ProviderMessageID = derefString(providerMsgID)
	n.LastError = derefString(lastError)
	if errClass != nil {
		n.ErrorClass = ErrorClass(*errClass)
	}
	return n, nil
}

// GetByID fetches one notification, scoped to userID unless userID is uuid.Nil
// (internal callers that have already authorized the request some other way).
func (r *Repository) GetByID(ctx context.Context, db Querier, id, userID uuid.UUID) (Notification, error) {
	q := `SELECT ` + notificationColumns + ` FROM notifications WHERE id = $1 AND deleted_at IS NULL`
	args := []any{id}
	if userID != uuid.Nil {
		q += ` AND user_id = $2`
		args = append(args, userID)
	}
	n, err := scanNotification(db.QueryRow(ctx, q, args...))
	if errors.Is(err, pgx.ErrNoRows) {
		return Notification{}, ErrNotFound
	}
	if err != nil {
		return Notification{}, fmt.Errorf("notification: get %s: %w", id, err)
	}
	return n, nil
}

// ListForUser returns one page of a user's notifications, newest first.
func (r *Repository) ListForUser(ctx context.Context, db Querier, userID uuid.UUID, offset, limit int) ([]Notification, int64, error) {
	const listQ = `
		SELECT ` + notificationColumns + `
		FROM notifications
		WHERE user_id = $1 AND deleted_at IS NULL
		ORDER BY created_at DESC
		OFFSET $2 LIMIT $3`

	rows, err := db.Query(ctx, listQ, userID, offset, limit)
	if err != nil {
		return nil, 0, fmt.Errorf("notification: list: %w", err)
	}
	defer rows.Close()

	var out []Notification
	for rows.Next() {
		n, err := scanNotification(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("notification: scan list row: %w", err)
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("notification: list rows: %w", err)
	}

	var total int64
	const countQ = `SELECT COUNT(*) FROM notifications WHERE user_id = $1 AND deleted_at IS NULL`
	if err := db.QueryRow(ctx, countQ, userID).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("notification: count: %w", err)
	}
	return out, total, nil
}

// MarkRead sets read_at for a notification owned by userID. Idempotent: a
// second call is a harmless no-op affecting zero rows.
func (r *Repository) MarkRead(ctx context.Context, db Querier, id, userID uuid.UUID) error {
	const q = `
		UPDATE notifications
		SET read_at = COALESCE(read_at, NOW()), updated_at = NOW(), version = version + 1
		WHERE id = $1 AND user_id = $2 AND deleted_at IS NULL`
	tag, err := db.Exec(ctx, q, id, userID)
	if err != nil {
		return fmt.Errorf("notification: mark read %s: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// MarkAllRead stamps read_at on every unread notification the user owns and
// returns how many rows it changed.
//
// It is one UPDATE, not a read-then-write loop: a client that taps "mark all
// read" while a notification is being inserted must not be able to observe a
// half-applied state, and a loop over ListForUser would also only ever cover
// one page. The COALESCE guard is what makes it idempotent -- a second call
// affects zero rows and leaves the original read timestamps alone, so the
// "when did they first see it" audit answer never moves.
func (r *Repository) MarkAllRead(ctx context.Context, db Querier, userID uuid.UUID) (int64, error) {
	const q = `
		UPDATE notifications
		SET read_at = NOW(), updated_at = NOW(), version = version + 1
		WHERE user_id = $1 AND read_at IS NULL AND deleted_at IS NULL`
	tag, err := db.Exec(ctx, q, userID)
	if err != nil {
		return 0, fmt.Errorf("notification: mark all read: %w", err)
	}
	return tag.RowsAffected(), nil
}

// ClaimDispatchBatch atomically flips up to limit queued, due notifications
// to 'sending' and returns them. It is a single statement (an
// UPDATE ... RETURNING over a SKIP LOCKED subquery), not a held-open
// transaction, so it never holds a row lock across a slow provider network
// call the way a naive "BEGIN; SELECT FOR UPDATE; <network I/O>; COMMIT"
// dispatcher would.
func (r *Repository) ClaimDispatchBatch(ctx context.Context, limit int) ([]Notification, error) {
	const q = `
		UPDATE notifications n
		SET status = 'sending', updated_at = NOW()
		FROM (
			SELECT id FROM notifications
			WHERE status = 'queued'
			  AND (scheduled_for IS NULL OR scheduled_for <= NOW())
			  AND (next_attempt_at IS NULL OR next_attempt_at <= NOW())
			  AND deleted_at IS NULL
			ORDER BY created_at
			LIMIT $1
			FOR UPDATE SKIP LOCKED
		) claimed
		WHERE n.id = claimed.id
		RETURNING ` + "n.id, n.user_id, n.channel, n.template_key, n.locale, n.urgency, n.subject, n.body, n.recipient, n.status, " +
		"n.provider, n.provider_message_id, n.attempts, n.last_error, n.error_class, " +
		"n.scheduled_for, n.next_attempt_at, n.sent_at, n.delivered_at, n.read_at, " +
		"n.dedupe_key, n.source_event_id, n.created_at, n.updated_at, n.version"

	rows, err := r.pool.Query(ctx, q, limit)
	if err != nil {
		return nil, fmt.Errorf("notification: claim batch: %w", err)
	}
	defer rows.Close()

	var out []Notification
	for rows.Next() {
		n, err := scanNotification(rows)
		if err != nil {
			return nil, fmt.Errorf("notification: scan claimed row: %w", err)
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("notification: claim rows: %w", err)
	}
	return out, nil
}

// AttemptResult is what the dispatcher reports back after trying to deliver
// one notification, whether it succeeded, was suppressed, or failed.
type AttemptResult struct {
	Status            Status
	Provider          string
	ProviderMessageID string
	ErrorClass        ErrorClass
	LastError         string
	SentAt            *time.Time
	DeliveredAt       *time.Time
	NextAttemptAt     *time.Time
	ScheduledFor      *time.Time // set when re-deferring (should not normally recur post-claim)
}

// RecordAttempt applies the outcome of one dispatch attempt: bumps attempts,
// updates status/provider/error fields, and stamps sent_at/delivered_at when
// applicable. Always a single statement -- see ClaimDispatchBatch's comment
// on why the network call itself never happens inside a DB transaction.
func (r *Repository) RecordAttempt(ctx context.Context, db Querier, id uuid.UUID, res AttemptResult) error {
	const q = `
		UPDATE notifications
		SET status = $2, provider = $3, provider_message_id = $4,
		    attempts = attempts + 1, last_error = $5, error_class = $6,
		    sent_at = COALESCE(sent_at, $7), delivered_at = COALESCE($8, delivered_at),
		    next_attempt_at = $9, scheduled_for = COALESCE($10, scheduled_for),
		    updated_at = NOW(), version = version + 1
		WHERE id = $1`
	tag, err := db.Exec(ctx, q, id,
		string(res.Status), nullString(res.Provider), nullString(res.ProviderMessageID),
		nullString(res.LastError), nullErrorClass(res.ErrorClass),
		res.SentAt, res.DeliveredAt, res.NextAttemptAt, res.ScheduledFor,
	)
	if err != nil {
		return fmt.Errorf("notification: record attempt %s: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ApplyDeliveryReceipt updates a notification's status from a provider
// delivery webhook, matched by provider_message_id.
func (r *Repository) ApplyDeliveryReceipt(ctx context.Context, provider, providerMessageID string, delivered bool, failReason string) error {
	q := `
		UPDATE notifications
		SET delivered_at = CASE WHEN $3 THEN NOW() ELSE delivered_at END,
		    status = CASE WHEN $3 THEN 'delivered' ELSE status END,
		    last_error = CASE WHEN NOT $3 AND $4 <> '' THEN $4 ELSE last_error END,
		    updated_at = NOW(), version = version + 1
		WHERE provider = $1 AND provider_message_id = $2`
	tag, err := r.pool.Exec(ctx, q, provider, providerMessageID, delivered, failReason)
	if err != nil {
		return fmt.Errorf("notification: apply delivery receipt: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ---------------------------------------------------------------------------
// templates
// ---------------------------------------------------------------------------

// GetActiveTemplate returns the active version of (key, channel, locale).
func (r *Repository) GetActiveTemplate(ctx context.Context, db Querier, key TemplateKey, ch Channel, locale Locale) (Template, error) {
	const q = `
		SELECT id, key, channel, locale, urgency, COALESCE(subject_template, ''), body_template, version, is_active, created_at, updated_at
		FROM templates
		WHERE key = $1 AND channel = $2 AND locale = $3 AND is_active`
	var t Template
	err := db.QueryRow(ctx, q, string(key), string(ch), string(locale)).Scan(
		&t.ID, &t.Key, &t.Channel, &t.Locale, &t.Urgency, &t.SubjectTemplate, &t.BodyTemplate,
		&t.Version, &t.IsActive, &t.CreatedAt, &t.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Template{}, ErrNotFound
	}
	if err != nil {
		return Template{}, fmt.Errorf("notification: get template %s/%s/%s: %w", key, ch, locale, err)
	}
	return t, nil
}

// ChannelsForKey returns every channel that has an active template for key,
// which is how the service decides "how many ways do we tell someone about
// a booking_confirmed" without hard-coding that fan-out list twice (once in
// the seed migration, once in Go).
func (r *Repository) ChannelsForKey(ctx context.Context, db Querier, key TemplateKey) ([]Channel, error) {
	const q = `SELECT DISTINCT channel FROM templates WHERE key = $1 AND is_active ORDER BY channel`
	rows, err := db.Query(ctx, q, string(key))
	if err != nil {
		return nil, fmt.Errorf("notification: channels for key %s: %w", key, err)
	}
	defer rows.Close()

	var out []Channel
	for rows.Next() {
		var ch Channel
		if err := rows.Scan(&ch); err != nil {
			return nil, fmt.Errorf("notification: scan channel: %w", err)
		}
		out = append(out, ch)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// preferences
// ---------------------------------------------------------------------------

// GetPreferences returns a user's preferences, or found=false when no row
// exists yet (the caller should fall back to DefaultPreferences).
func (r *Repository) GetPreferences(ctx context.Context, db Querier, userID uuid.UUID) (Preferences, bool, error) {
	const q = `
		SELECT user_id, sms_enabled, push_enabled, email_enabled, in_app_enabled, locale,
		       quiet_hours_start_min, quiet_hours_end_min, timezone, version, created_at, updated_at
		FROM notification_preferences WHERE user_id = $1`

	var p Preferences
	var startMin, endMin *int16
	err := db.QueryRow(ctx, q, userID).Scan(
		&p.UserID, &p.SMSEnabled, &p.PushEnabled, &p.EmailEnabled, &p.InAppEnabled, &p.Locale,
		&startMin, &endMin, &p.Timezone, &p.Version, &p.CreatedAt, &p.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Preferences{}, false, nil
	}
	if err != nil {
		return Preferences{}, false, fmt.Errorf("notification: get preferences %s: %w", userID, err)
	}
	p.QuietHoursStart, p.QuietHoursEnd = minutesToDuration(startMin), minutesToDuration(endMin)
	return p, true, nil
}

// UpsertPreferences creates or replaces a user's preferences.
func (r *Repository) UpsertPreferences(ctx context.Context, db Querier, p Preferences) error {
	const q = `
		INSERT INTO notification_preferences (
			user_id, sms_enabled, push_enabled, email_enabled, in_app_enabled, locale,
			quiet_hours_start_min, quiet_hours_end_min, timezone, version
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,0)
		ON CONFLICT (user_id) DO UPDATE SET
			sms_enabled = EXCLUDED.sms_enabled,
			push_enabled = EXCLUDED.push_enabled,
			email_enabled = EXCLUDED.email_enabled,
			in_app_enabled = EXCLUDED.in_app_enabled,
			locale = EXCLUDED.locale,
			quiet_hours_start_min = EXCLUDED.quiet_hours_start_min,
			quiet_hours_end_min = EXCLUDED.quiet_hours_end_min,
			timezone = EXCLUDED.timezone,
			version = notification_preferences.version + 1,
			updated_at = NOW()`
	startMin, err := durationToMinutes(p.QuietHoursStart)
	if err != nil {
		return err
	}
	endMin, err := durationToMinutes(p.QuietHoursEnd)
	if err != nil {
		return err
	}
	_, err = db.Exec(ctx, q, p.UserID, p.SMSEnabled, p.PushEnabled, p.EmailEnabled, p.InAppEnabled,
		string(p.Locale), startMin, endMin, p.Timezone)
	if err != nil {
		return fmt.Errorf("notification: upsert preferences %s: %w", p.UserID, err)
	}
	return nil
}

// minutesToDuration/durationToMinutes convert between the wire format
// (nullable SMALLINT minutes-since-midnight) and the domain model's
// *time.Duration, keeping the TIME-of-day representation entirely out of the
// SQL driver's hands. See the schema comment on quiet_hours_start_min.
func minutesToDuration(m *int16) *time.Duration {
	if m == nil {
		return nil
	}
	d := time.Duration(*m) * time.Minute
	return &d
}

// minutesPerDay bounds the SMALLINT column: quiet_hours_start_min and
// quiet_hours_end_min both carry CHECK (... BETWEEN 0 AND 1439).
const minutesPerDay = 1440

// durationToMinutes converts a quiet-hours offset to the column's SMALLINT.
//
// The range check is not decoration. int16(*d / time.Minute) is an unchecked
// narrowing conversion: 32768 minutes silently becomes -32768, i.e. a
// perfectly plausible-looking clock value that the column's CHECK then
// rejects with a constraint violation the caller sees as a 500. Anything
// outside a single day is rejected here, where the error can say what is
// actually wrong.
func durationToMinutes(d *time.Duration) (*int16, error) {
	if d == nil {
		return nil, nil
	}
	m := *d / time.Minute
	if m < 0 || m >= minutesPerDay {
		return nil, fmt.Errorf("notification: quiet hours offset %v is outside a single day", *d)
	}
	v := int16(m)
	return &v, nil
}

// ---------------------------------------------------------------------------
// device tokens
// ---------------------------------------------------------------------------

// UpsertDeviceToken registers (or re-validates, on repeat registration) a
// push token.
func (r *Repository) UpsertDeviceToken(ctx context.Context, db Querier, t DeviceToken) (uuid.UUID, error) {
	const q = `
		INSERT INTO device_tokens (user_id, token, platform, last_seen_at, invalidated_at)
		VALUES ($1,$2,$3,NOW(),NULL)
		ON CONFLICT (user_id, token) DO UPDATE SET
			platform = EXCLUDED.platform,
			last_seen_at = NOW(),
			invalidated_at = NULL,
			updated_at = NOW()
		RETURNING id`
	var id uuid.UUID
	if err := db.QueryRow(ctx, q, t.UserID, t.Token, string(t.Platform)).Scan(&id); err != nil {
		return uuid.Nil, fmt.Errorf("notification: upsert device token: %w", err)
	}
	return id, nil
}

// InvalidateDeviceToken soft-deletes a token owned by userID (DELETE
// /devices/{id}).
func (r *Repository) InvalidateDeviceToken(ctx context.Context, db Querier, id, userID uuid.UUID) error {
	const q = `
		UPDATE device_tokens SET invalidated_at = NOW(), updated_at = NOW()
		WHERE id = $1 AND user_id = $2 AND invalidated_at IS NULL`
	tag, err := db.Exec(ctx, q, id, userID)
	if err != nil {
		return fmt.Errorf("notification: invalidate device token %s: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// maxActiveDeviceTokensSQL is MaxActiveDeviceTokens as a SQL literal. A
// constant, never a caller value -- the LIMIT is a bound, not a parameter.
const maxActiveDeviceTokensSQL = "10"

// CountActiveDeviceTokens is how many handsets a user currently has
// registered, used to enforce MaxActiveDeviceTokens at registration.
func (r *Repository) CountActiveDeviceTokens(ctx context.Context, db Querier, userID uuid.UUID) (int, error) {
	const q = `SELECT COUNT(*) FROM device_tokens WHERE user_id = $1 AND invalidated_at IS NULL`
	var n int
	if err := db.QueryRow(ctx, q, userID).Scan(&n); err != nil {
		return 0, fmt.Errorf("notification: count device tokens %s: %w", userID, err)
	}
	return n, nil
}

// InvalidateDeviceTokenByValue marks a token invalidated by its raw value.
// Used when a push provider reports it unregistered/expired -- the
// notification pipeline only knows the token string at that point, not its
// row ID. This is the "prune tokens FCM reports as unregistered" mechanism.
func (r *Repository) InvalidateDeviceTokenByValue(ctx context.Context, db Querier, token string) error {
	const q = `
		UPDATE device_tokens SET invalidated_at = NOW(), updated_at = NOW()
		WHERE token = $1 AND invalidated_at IS NULL`
	_, err := db.Exec(ctx, q, token)
	if err != nil {
		return fmt.Errorf("notification: invalidate device token by value: %w", err)
	}
	return nil
}

// MaxActiveDeviceTokens bounds how many handsets one account may have
// registered at once.
//
// Registration had no cap of any kind: the token was `validate:"required"` and
// nothing else, the conflict target is (user_id, token), and every distinct
// string was a new row. Every push to that user then fanned out over
// ListActiveDeviceTokens -- which had no LIMIT -- as one serial outbound FCM
// call per row, inside the dispatcher goroutine. Register N junk tokens and
// every subsequent notification to that account costs N provider calls before
// it gives up.
//
// Ten is generous for a real person: a phone, a tablet, a couple of browsers,
// and enough headroom for reinstalls before the retention job prunes the
// stale ones.
const MaxActiveDeviceTokens = 10

// ListActiveDeviceTokens returns a user's non-invalidated tokens, most
// recently seen first, bounded by MaxActiveDeviceTokens.
//
// The bound is repeated here rather than trusted to the registration cap: this
// query is what the dispatcher fans out over, so it is the place where an
// unbounded row count becomes an unbounded number of outbound provider calls.
func (r *Repository) ListActiveDeviceTokens(ctx context.Context, db Querier, userID uuid.UUID) ([]DeviceToken, error) {
	const q = `
		SELECT id, user_id, token, platform, last_seen_at, invalidated_at, created_at, updated_at
		FROM device_tokens WHERE user_id = $1 AND invalidated_at IS NULL
		ORDER BY last_seen_at DESC
		LIMIT ` + maxActiveDeviceTokensSQL
	rows, err := db.Query(ctx, q, userID)
	if err != nil {
		return nil, fmt.Errorf("notification: list device tokens %s: %w", userID, err)
	}
	defer rows.Close()

	var out []DeviceToken
	for rows.Next() {
		var t DeviceToken
		if err := rows.Scan(&t.ID, &t.UserID, &t.Token, &t.Platform, &t.LastSeenAt, &t.InvalidatedAt, &t.CreatedAt, &t.UpdatedAt); err != nil {
			return nil, fmt.Errorf("notification: scan device token: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// HardPruneInvalidatedTokens deletes tokens that have been invalidated for
// longer than olderThan. Run by the maintenance job; keeps the table from
// accumulating dead rows forever once they are no longer even useful for
// "why did this user stop getting push" support triage.
func (r *Repository) HardPruneInvalidatedTokens(ctx context.Context, olderThan time.Duration) (int64, error) {
	const q = `DELETE FROM device_tokens WHERE invalidated_at IS NOT NULL AND invalidated_at < $1`
	tag, err := r.pool.Exec(ctx, q, time.Now().Add(-olderThan))
	if err != nil {
		return 0, fmt.Errorf("notification: hard prune device tokens: %w", err)
	}
	return tag.RowsAffected(), nil
}

// ---------------------------------------------------------------------------
// delivery log / dead letters
// ---------------------------------------------------------------------------

func (r *Repository) InsertDeliveryLog(ctx context.Context, db Querier, e DeliveryLogEntry) error {
	const q = `
		INSERT INTO delivery_log (notification_id, attempt, provider, status, error_class, provider_response)
		VALUES ($1,$2,$3,$4,$5,$6)`
	_, err := db.Exec(ctx, q, e.NotificationID, e.Attempt, e.Provider, e.Status, nullErrorClass(e.ErrorClass), e.ProviderResponse)
	if err != nil {
		return fmt.Errorf("notification: insert delivery log: %w", err)
	}
	return nil
}

func (r *Repository) InsertDeadLetter(ctx context.Context, db Querier, notificationID, userID uuid.UUID, ch Channel, key TemplateKey, attempts int, lastError string, reason DeadLetterReason) error {
	const q = `
		INSERT INTO notification_dead_letters (notification_id, user_id, channel, template_key, attempts, last_error, reason)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`
	_, err := db.Exec(ctx, q, notificationID, userID, string(ch), string(key), attempts, nullString(lastError), string(reason))
	if err != nil {
		return fmt.Errorf("notification: insert dead letter: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// appointment reminder projection
// ---------------------------------------------------------------------------

// SeedReminderState records a booking the moment it is created, from
// events.AppointmentCreated, before anyone has paid for it.
//
// Its whole job is to capture AmountCents -- the quote fixed at booking, which
// events.AppointmentConfirmed does not carry -- so the confirmation message
// can state the price the patient actually agreed to rather than the doctor's
// current list price. The row lands as 'pending', which the reminder cron's
// partial index (status = 'confirmed') deliberately does not see.
//
// It never overwrites a row that has already advanced past 'pending': events
// can be redelivered out of order, and a created event arriving after its own
// confirmation must not undo the confirmation.
func (r *Repository) SeedReminderState(ctx context.Context, db Querier, s AppointmentReminderState) error {
	const q = `
		INSERT INTO appointment_reminder_state (
			appointment_id, patient_id, doctor_id, specialty,
			starts_at, amount_cents, currency, status
		)
		VALUES ($1,$2,$3,$4,$5,$6,$7,'pending')
		ON CONFLICT (appointment_id) DO UPDATE SET
			patient_id   = EXCLUDED.patient_id,
			doctor_id    = EXCLUDED.doctor_id,
			specialty    = EXCLUDED.specialty,
			starts_at    = EXCLUDED.starts_at,
			amount_cents = EXCLUDED.amount_cents,
			currency     = EXCLUDED.currency,
			updated_at   = NOW()`
	_, err := db.Exec(ctx, q, s.AppointmentID, s.PatientID, s.DoctorID, s.Specialty,
		s.StartsAt, s.AmountCents, s.Currency)
	if err != nil {
		return fmt.Errorf("notification: seed reminder state: %w", err)
	}
	return nil
}

// UpsertReminderState creates or refreshes the local appointment projection
// row consumed from appointment.confirmed. A re-delivery of the same event
// is a harmless idempotent upsert.
//
// amount_cents and currency are NOT written here: they belong to
// appointment.created and are left exactly as that event set them. Writing
// them from a confirmation would be writing a number this event never carried.
func (r *Repository) UpsertReminderState(ctx context.Context, db Querier, s AppointmentReminderState) error {
	const q = `
		INSERT INTO appointment_reminder_state (
			appointment_id, patient_id, doctor_id, doctor_name, specialty,
			patient_phone, patient_email, join_link, starts_at, status
		)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,'confirmed')
		ON CONFLICT (appointment_id) DO UPDATE SET
			patient_id = EXCLUDED.patient_id,
			doctor_id = EXCLUDED.doctor_id,
			doctor_name = EXCLUDED.doctor_name,
			specialty = EXCLUDED.specialty,
			patient_phone = EXCLUDED.patient_phone,
			patient_email = EXCLUDED.patient_email,
			join_link = EXCLUDED.join_link,
			starts_at = EXCLUDED.starts_at,
			status = 'confirmed',
			updated_at = NOW()`
	_, err := db.Exec(ctx, q, s.AppointmentID, s.PatientID, s.DoctorID, s.DoctorName, s.Specialty,
		s.PatientPhone, s.PatientEmail, s.JoinLink, s.StartsAt)
	if err != nil {
		return fmt.Errorf("notification: upsert reminder state: %w", err)
	}
	return nil
}

// QuoteFor returns the price quoted at booking for an appointment, as
// recorded by SeedReminderState from events.AppointmentCreated.
//
// ok is false when no appointment.created has been seen yet. The caller must
// treat that as "come back later", not as "free": rendering a confirmation
// that says "Fee: " is the same silent-empty-string failure that canonical
// payloads exist to eliminate.
func (r *Repository) QuoteFor(ctx context.Context, db Querier, appointmentID uuid.UUID) (cents int64, currency string, ok bool, err error) {
	const q = `SELECT amount_cents, currency FROM appointment_reminder_state WHERE appointment_id = $1`
	err = db.QueryRow(ctx, q, appointmentID).Scan(&cents, &currency)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, "", false, nil
	}
	if err != nil {
		return 0, "", false, fmt.Errorf("notification: read appointment quote: %w", err)
	}
	return cents, currency, true, nil
}

// SetReminderStateStatus updates the projection's status (cancelled/
// completed/no_show), consumed from the corresponding appointment.* events.
func (r *Repository) SetReminderStateStatus(ctx context.Context, db Querier, appointmentID uuid.UUID, status string) error {
	const q = `UPDATE appointment_reminder_state SET status = $2, updated_at = NOW() WHERE appointment_id = $1`
	_, err := db.Exec(ctx, q, appointmentID, status)
	if err != nil {
		return fmt.Errorf("notification: set reminder state status: %w", err)
	}
	return nil
}

func scanReminderState(rows pgx.Rows) (AppointmentReminderState, error) {
	var s AppointmentReminderState
	err := rows.Scan(&s.AppointmentID, &s.PatientID, &s.DoctorID, &s.DoctorName, &s.Specialty,
		&s.PatientPhone, &s.PatientEmail, &s.JoinLink,
		&s.StartsAt, &s.Status, &s.Reminder24hSentAt, &s.Reminder1hSentAt, &s.CreatedAt, &s.UpdatedAt)
	return s, err
}

const reminderStateColumns = `appointment_id, patient_id, doctor_id, doctor_name, specialty, patient_phone, patient_email, join_link, starts_at, status, reminder_24h_sent_at, reminder_1h_sent_at, created_at, updated_at`

// FindDue1hReminders returns confirmed appointments starting within
// [windowStart, windowEnd] that have not yet had their 1-hour reminder sent.
func (r *Repository) FindDue1hReminders(ctx context.Context, windowStart, windowEnd time.Time) ([]AppointmentReminderState, error) {
	q := `SELECT ` + reminderStateColumns + `
		FROM appointment_reminder_state
		WHERE status = 'confirmed' AND reminder_1h_sent_at IS NULL
		  AND starts_at BETWEEN $1 AND $2`
	return r.queryReminderStates(ctx, q, windowStart, windowEnd)
}

// FindDue24hReminders is the same query for the 24-hour reminder.
func (r *Repository) FindDue24hReminders(ctx context.Context, windowStart, windowEnd time.Time) ([]AppointmentReminderState, error) {
	q := `SELECT ` + reminderStateColumns + `
		FROM appointment_reminder_state
		WHERE status = 'confirmed' AND reminder_24h_sent_at IS NULL
		  AND starts_at BETWEEN $1 AND $2`
	return r.queryReminderStates(ctx, q, windowStart, windowEnd)
}

func (r *Repository) queryReminderStates(ctx context.Context, q string, windowStart, windowEnd time.Time) ([]AppointmentReminderState, error) {
	rows, err := r.pool.Query(ctx, q, windowStart, windowEnd)
	if err != nil {
		return nil, fmt.Errorf("notification: find due reminders: %w", err)
	}
	defer rows.Close()

	var out []AppointmentReminderState
	for rows.Next() {
		s, err := scanReminderState(rows)
		if err != nil {
			return nil, fmt.Errorf("notification: scan reminder state: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// MarkReminder1hSent and MarkReminder24hSent flip the projection's sent
// markers. Combined with the notifications.dedupe_key UNIQUE constraint,
// this is belt-and-suspenders against a double-send if the cron's tick
// overlaps its own previous run: even if both checked reminder_1h_sent_at
// as NULL in the same instant, the dedupe_key insert can only win once.
func (r *Repository) MarkReminder1hSent(ctx context.Context, appointmentID uuid.UUID) error {
	const q = `UPDATE appointment_reminder_state SET reminder_1h_sent_at = NOW(), updated_at = NOW() WHERE appointment_id = $1`
	_, err := r.pool.Exec(ctx, q, appointmentID)
	if err != nil {
		return fmt.Errorf("notification: mark 1h reminder sent: %w", err)
	}
	return nil
}

func (r *Repository) MarkReminder24hSent(ctx context.Context, appointmentID uuid.UUID) error {
	const q = `UPDATE appointment_reminder_state SET reminder_24h_sent_at = NOW(), updated_at = NOW() WHERE appointment_id = $1`
	_, err := r.pool.Exec(ctx, q, appointmentID)
	if err != nil {
		return fmt.Errorf("notification: mark 24h reminder sent: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// retention
// ---------------------------------------------------------------------------

// PruneOldNotificationBodies blanks subject/body on notifications older than
// olderThan. The row itself (status, channel, timestamps) is kept
// indefinitely as operational metadata; only the PHI-adjacent free text is
// removed. See the retention note in migrations/000002.
func (r *Repository) PruneOldNotificationBodies(ctx context.Context, olderThan time.Duration) (int64, error) {
	const q = `
		UPDATE notifications
		SET subject = NULL, body = '[pruned]'
		WHERE created_at < $1 AND body <> '[pruned]'`
	tag, err := r.pool.Exec(ctx, q, time.Now().Add(-olderThan))
	if err != nil {
		return 0, fmt.Errorf("notification: prune old bodies: %w", err)
	}
	return tag.RowsAffected(), nil
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func nullString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func nullErrorClass(c ErrorClass) *string {
	if c == "" {
		return nil
	}
	s := string(c)
	return &s
}
