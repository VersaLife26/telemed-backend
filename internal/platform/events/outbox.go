package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/rs/zerolog"
)

// Outbox writes events into the same Postgres transaction as the business
// change that produced them.
//
// Usage inside a service transaction:
//
//	database.InTx(ctx, pool, opts, func(tx pgx.Tx) error {
//	    if err := repo.BookSlot(ctx, tx, ...); err != nil { return err }
//	    return outbox.Enqueue(ctx, tx, events.SubjectSlotBooked, slotID.String(), payload)
//	})
//
// If the transaction rolls back, the event disappears with it. There is no
// window in which the database and the broker disagree.
type Outbox struct {
	producer string
}

// NewOutbox returns an Outbox that stamps events with the producing service.
func NewOutbox(producer string) *Outbox { return &Outbox{producer: producer} }

// Enqueue inserts an event row on the caller's transaction. It must be given a
// pgx.Tx, never a pool -- taking a Tx is what enforces the atomicity contract
// at compile time.
func (o *Outbox) Enqueue(ctx context.Context, tx pgx.Tx, subject Subject, aggregateID string, payload any) error {
	env, err := NewEnvelope(subject, o.producer, aggregateID, payload)
	if err != nil {
		return err
	}
	body, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("events: marshal outbox envelope: %w", err)
	}

	const q = `
		INSERT INTO outbox_events (id, subject, aggregate_id, producer, payload, created_at)
		VALUES ($1, $2, $3, $4, $5, NOW())`
	if _, err := tx.Exec(ctx, q, env.ID, string(subject), aggregateID, o.producer, body); err != nil {
		return fmt.Errorf("events: enqueue %s: %w", subject, err)
	}
	return nil
}

// Relay drains outbox_events into the broker. Exactly one replica does useful
// work at a time because the claim query takes a row lock with SKIP LOCKED;
// running the relay on every replica is safe and is how we get failover for
// free.
type Relay struct {
	pool      RelayPool
	publisher Publisher
	log       zerolog.Logger
	interval  time.Duration
	batchSize int
}

// RelayPool is the slice of the connection pool the relay needs. Keeping it
// narrow means a test can drive the relay with a fake that is twenty lines
// long instead of standing up Postgres.
type RelayPool interface {
	Begin(ctx context.Context) (pgx.Tx, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// NewRelay builds the relay worker.
func NewRelay(pool RelayPool, pub Publisher, log zerolog.Logger, interval time.Duration, batchSize int) *Relay {
	if interval <= 0 {
		interval = 2 * time.Second
	}
	if batchSize <= 0 {
		batchSize = 100
	}
	return &Relay{pool: pool, publisher: pub, log: log, interval: interval, batchSize: batchSize}
}

type outboxRow struct {
	id      uuid.UUID
	subject string
	payload []byte
	tries   int
}

// Run polls until ctx is cancelled. Call it in its own goroutine at boot.
func (r *Relay) Run(ctx context.Context) {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	r.log.Info().Dur("interval", r.interval).Int("batch", r.batchSize).Msg("outbox relay started")

	for {
		select {
		case <-ctx.Done():
			r.log.Info().Msg("outbox relay stopped")
			return
		case <-ticker.C:
			n, err := r.drainOnce(ctx)
			if err != nil && !errors.Is(err, context.Canceled) {
				r.log.Error().Err(err).Msg("outbox drain failed")
				continue
			}
			// A full batch means there is more waiting; keep going without
			// burning a whole tick of latency on a backlog.
			for err == nil && n == r.batchSize {
				n, err = r.drainOnce(ctx)
			}
		}
	}
}

func (r *Relay) drainOnce(ctx context.Context) (int, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("events: relay begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	// FOR UPDATE SKIP LOCKED lets N replicas poll the same table without ever
	// handing the same row to two of them.
	const claim = `
		SELECT id, subject, payload, publish_attempts
		FROM outbox_events
		WHERE published_at IS NULL
		  AND (next_attempt_at IS NULL OR next_attempt_at <= NOW())
		ORDER BY created_at
		LIMIT $1
		FOR UPDATE SKIP LOCKED`

	rows, err := tx.Query(ctx, claim, r.batchSize)
	if err != nil {
		return 0, fmt.Errorf("events: relay claim: %w", err)
	}

	var batch []outboxRow
	for rows.Next() {
		var row outboxRow
		if err := rows.Scan(&row.id, &row.subject, &row.payload, &row.tries); err != nil {
			rows.Close()
			return 0, fmt.Errorf("events: relay scan: %w", err)
		}
		batch = append(batch, row)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("events: relay rows: %w", err)
	}
	if len(batch) == 0 {
		return 0, nil
	}

	published := make([]uuid.UUID, 0, len(batch))
	for _, row := range batch {
		var env Envelope
		if err := json.Unmarshal(row.payload, &env); err != nil {
			// Unparseable rows are poison; park them so the relay never wedges.
			r.log.Error().Err(err).Str("outbox_id", row.id.String()).Msg("parking unparseable outbox row")
			if _, derr := tx.Exec(ctx,
				`UPDATE outbox_events SET last_error = $2, publish_attempts = publish_attempts + 1,
				 next_attempt_at = NOW() + INTERVAL '1 hour' WHERE id = $1`,
				row.id, err.Error()); derr != nil {
				return 0, derr
			}
			continue
		}

		if err := r.publisher.Publish(ctx, env); err != nil {
			backoff := relayBackoff(row.tries)
			r.log.Warn().Err(err).
				Str("subject", row.subject).
				Int("attempt", row.tries+1).
				Dur("retry_in", backoff).
				Msg("outbox publish failed")
			if _, derr := tx.Exec(ctx,
				`UPDATE outbox_events
				 SET publish_attempts = publish_attempts + 1, last_error = $2,
				     next_attempt_at = NOW() + $3::interval
				 WHERE id = $1`,
				row.id, err.Error(), fmt.Sprintf("%d seconds", int(backoff.Seconds()))); derr != nil {
				return 0, derr
			}
			continue
		}
		published = append(published, row.id)
	}

	if len(published) > 0 {
		if _, err := tx.Exec(ctx,
			`UPDATE outbox_events SET published_at = NOW(), last_error = NULL WHERE id = ANY($1)`,
			published); err != nil {
			return 0, fmt.Errorf("events: relay mark published: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("events: relay commit: %w", err)
	}
	if len(published) > 0 {
		r.log.Debug().Int("count", len(published)).Msg("outbox batch published")
	}
	return len(batch), nil
}

// relayBackoff grows the retry delay to a 5 minute ceiling. Broker outages are
// usually short; a stuck event should not hammer a recovering cluster.
func relayBackoff(attempt int) time.Duration {
	d := time.Duration(1<<min(attempt, 8)) * time.Second
	if d > 5*time.Minute {
		return 5 * time.Minute
	}
	return d
}
