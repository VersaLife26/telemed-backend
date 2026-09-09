// Package cache defines the Cache abstraction and its Redis implementation.
//
// Every call site depends on the Cache interface, never on *redis.Client. That
// is the 50-year rule in practice: when Redis is replaced by Dragonfly, Valkey,
// or something not yet written, only this file changes.
package cache

import (
	"context"
	"errors"
	"time"
)

// ErrNotFound is returned by Get when the key is absent or expired.
var ErrNotFound = errors.New("cache: key not found")

// ErrLockNotHeld is returned by Unlock when the caller no longer owns the lock,
// which means the TTL expired mid-operation and another worker may have taken
// over. Treat it as a signal to abort, never to retry blindly.
var ErrLockNotHeld = errors.New("cache: lock not held by caller")

// Cache is the storage contract used across every telemed service.
type Cache interface {
	Get(ctx context.Context, key string) ([]byte, error)
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
	Del(ctx context.Context, keys ...string) error
	Exists(ctx context.Context, key string) (bool, error)

	// Incr increments a counter and applies ttl only on first creation. Used
	// for OTP and API rate limiting, where resetting the window on every hit
	// would let an attacker keep the window open forever.
	Incr(ctx context.Context, key string, ttl time.Duration) (int64, error)

	// Lock acquires a distributed lock via SET NX. The returned token must be
	// passed to Unlock so a caller can never release a lock it no longer owns.
	Lock(ctx context.Context, key string, ttl time.Duration) (token string, acquired bool, err error)
	Unlock(ctx context.Context, key, token string) error

	// ZAdd/ZRange back waiting rooms and waitlists, which are naturally sorted
	// sets keyed by arrival time.
	ZAdd(ctx context.Context, key string, score float64, member string) error
	ZRem(ctx context.Context, key string, members ...string) error
	ZRank(ctx context.Context, key, member string) (int64, error)
	ZRange(ctx context.Context, key string, start, stop int64) ([]string, error)
	ZCard(ctx context.Context, key string) (int64, error)

	Ping(ctx context.Context) error
	Close() error
}
