package scheduling

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"telemed/internal/platform/cache"
	"telemed/internal/platform/events"
)

// Clock exists so the cancellation window, the waitlist offer expiry and the
// no-show detector can be tested without sleeping. Production uses SystemClock.
type Clock interface {
	Now() time.Time
}

// SystemClock reads the wall clock, always in UTC.
type SystemClock struct{}

// Now returns the current instant in UTC.
func (SystemClock) Now() time.Time { return time.Now().UTC() }

// FixedClock returns a constant instant. Test-only, but it lives here because
// it is part of the Clock contract.
type FixedClock struct{ T time.Time }

// Now returns the fixed instant.
func (c FixedClock) Now() time.Time { return c.T }

// SlotLocker is the cheap early-reject lock in front of a booking.
//
// It is NOT a correctness layer (ADR-007). Redis can lose a key on failover and
// a lock with a TTL is not a mutex; the entire correctness argument rests on
// SELECT ... FOR UPDATE plus the version guard plus uq_appointments_slot_live,
// all inside one Postgres transaction. What this buys is throughput: under a
// thundering herd on one popular slot, 99 of 100 callers are turned away before
// they take a row lock.
//
// Because it is an optimisation, it is switchable -- and the concurrency test
// runs the whole booking path with NoopLocker to prove nothing depends on it.
type SlotLocker interface {
	// Acquire returns acquired=false when someone else holds the lock. An
	// error means the locker itself is unavailable; callers fail open.
	Acquire(ctx context.Context, key string, ttl time.Duration) (token string, acquired bool, err error)
	// Release is token-guarded: a caller whose TTL expired mid-transaction
	// cannot delete the lock a different caller has since taken (ADR-006).
	Release(ctx context.Context, key, token string) error
}

// RedisLocker adapts the platform Cache to SlotLocker.
type RedisLocker struct{ Cache cache.Cache }

// Acquire performs a SET NX with a random token.
func (l RedisLocker) Acquire(ctx context.Context, key string, ttl time.Duration) (token string, acquired bool, err error) {
	return l.Cache.Lock(ctx, key, ttl)
}

// Release runs the compare-and-delete Lua script.
func (l RedisLocker) Release(ctx context.Context, key, token string) error {
	return l.Cache.Unlock(ctx, key, token)
}

// NoopLocker acquires everything and releases nothing. Selected by setting
// SLOT_LOCK_ENABLED=false, and used by TestConcurrentBookingWithoutRedisLock to
// demonstrate that Postgres alone is sufficient.
type NoopLocker struct{}

// Acquire always succeeds.
func (NoopLocker) Acquire(context.Context, string, time.Duration) (token string, acquired bool, err error) {
	return "", true, nil
}

// Release does nothing.
func (NoopLocker) Release(context.Context, string, string) error { return nil }

// SlotLockKey is the Redis key for one slot's booking lock. Shared with the
// runbook, which tells an operator to inspect exactly this key.
func SlotLockKey(slotID uuid.UUID) string { return "lock:slot:" + slotID.String() }

// EventPublisher is the outbox seam. Taking a pgx.Tx rather than a pool is the
// whole point (ADR-005): the type system refuses to let anyone publish outside
// the transaction that produced the change.
type EventPublisher interface {
	Enqueue(ctx context.Context, tx pgx.Tx, subject events.Subject, aggregateID string, payload any) error
}

// WaitlistQueue is the ordered index of who is waiting for a cancellation.
//
// Redis sorted sets are the right shape for this and the source documentation
// asks for them, but they are an index, not a ledger: the waitlists table is
// the truth. Every read falls back to Postgres when the queue is cold, so a
// flushed Redis costs latency and never costs a patient their place.
type WaitlistQueue interface {
	Push(ctx context.Context, doctorID uuid.UUID, date Date, entryID uuid.UUID, joinedAt time.Time) error
	Remove(ctx context.Context, doctorID uuid.UUID, date Date, entryIDs ...uuid.UUID) error
	// Head returns up to n entry ids in join order.
	Head(ctx context.Context, doctorID uuid.UUID, date Date, n int) ([]uuid.UUID, error)
	Len(ctx context.Context, doctorID uuid.UUID, date Date) (int64, error)
}

// WaitlistQueueKey names the sorted set for one doctor-day.
func WaitlistQueueKey(doctorID uuid.UUID, date Date) string {
	return fmt.Sprintf("waitlist:%s:%s", doctorID, date)
}

// RedisWaitlistQueue is the Cache-backed WaitlistQueue.
type RedisWaitlistQueue struct{ Cache cache.Cache }

// Push adds an entry scored by its join time, so ZRANGE returns FIFO order.
func (q RedisWaitlistQueue) Push(ctx context.Context, doctorID uuid.UUID, date Date, entryID uuid.UUID, joinedAt time.Time) error {
	// Nanoseconds as a float64 loses precision above ~2^53ns (about 1970+104
	// days squared -- i.e. immediately), so score by microseconds. Two entries
	// in the same microsecond tie, and ZRANGE breaks ties lexicographically by
	// member, which is stable if arbitrary. Postgres created_at remains the
	// authority when it matters.
	return q.Cache.ZAdd(ctx, WaitlistQueueKey(doctorID, date), float64(joinedAt.UnixMicro()), entryID.String())
}

// Remove drops entries from the queue.
func (q RedisWaitlistQueue) Remove(ctx context.Context, doctorID uuid.UUID, date Date, entryIDs ...uuid.UUID) error {
	if len(entryIDs) == 0 {
		return nil
	}
	members := make([]string, len(entryIDs))
	for i, id := range entryIDs {
		members[i] = id.String()
	}
	return q.Cache.ZRem(ctx, WaitlistQueueKey(doctorID, date), members...)
}

// Head returns the n oldest entries.
func (q RedisWaitlistQueue) Head(ctx context.Context, doctorID uuid.UUID, date Date, n int) ([]uuid.UUID, error) {
	if n <= 0 {
		return nil, nil
	}
	raw, err := q.Cache.ZRange(ctx, WaitlistQueueKey(doctorID, date), 0, int64(n-1))
	if err != nil {
		return nil, err
	}
	out := make([]uuid.UUID, 0, len(raw))
	for _, s := range raw {
		id, err := uuid.Parse(s)
		if err != nil {
			// A malformed member is corruption in the index, not in the ledger.
			// Skip it; the Postgres fallback will still find the entry.
			continue
		}
		out = append(out, id)
	}
	return out, nil
}

// Len reports the queue depth.
func (q RedisWaitlistQueue) Len(ctx context.Context, doctorID uuid.UUID, date Date) (int64, error) {
	return q.Cache.ZCard(ctx, WaitlistQueueKey(doctorID, date))
}

// NoopWaitlistQueue disables the Redis index entirely; promotion then reads
// order straight from Postgres. Selected when Redis is not configured.
type NoopWaitlistQueue struct{}

// Push does nothing.
func (NoopWaitlistQueue) Push(context.Context, uuid.UUID, Date, uuid.UUID, time.Time) error {
	return nil
}

// Remove does nothing.
func (NoopWaitlistQueue) Remove(context.Context, uuid.UUID, Date, ...uuid.UUID) error { return nil }

// Head reports an empty queue, which sends the caller to Postgres.
func (NoopWaitlistQueue) Head(context.Context, uuid.UUID, Date, int) ([]uuid.UUID, error) {
	return nil, nil
}

// Len reports zero.
func (NoopWaitlistQueue) Len(context.Context, uuid.UUID, Date) (int64, error) { return 0, nil }
