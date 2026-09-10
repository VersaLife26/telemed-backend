package scheduling_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"sort"
	"sync"
	"time"

	"telemed/internal/platform/cache"
)

// memCache is an in-process cache.Cache with the same semantics as the Redis
// implementation: SET NX with a random token, token-guarded delete, TTL expiry,
// and sorted sets.
//
// It is used so the concurrency test can exercise the *locked* booking path
// without standing up Redis in the unit tier. It is a substitute for the
// transport, not for the correctness argument -- the point of
// TestConcurrentBookingWithoutRedisLock is that swapping this for nothing at all
// changes no outcome.
type memCache struct {
	mu   sync.Mutex
	kv   map[string]memEntry
	zset map[string]map[string]float64
	// failLock makes Acquire return an error, so a test can exercise the
	// fail-open path a Redis outage produces.
	failLock bool
}

type memEntry struct {
	value     []byte
	expiresAt time.Time
}

func newMemCache() *memCache {
	return &memCache{kv: map[string]memEntry{}, zset: map[string]map[string]float64{}}
}

var _ cache.Cache = (*memCache)(nil)

func (c *memCache) live(key string) (memEntry, bool) {
	e, ok := c.kv[key]
	if !ok {
		return memEntry{}, false
	}
	if !e.expiresAt.IsZero() && time.Now().After(e.expiresAt) {
		delete(c.kv, key)
		return memEntry{}, false
	}
	return e, true
}

func (c *memCache) Get(_ context.Context, key string) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.live(key)
	if !ok {
		return nil, cache.ErrNotFound
	}
	return e.value, nil
}

func (c *memCache) Set(_ context.Context, key string, value []byte, ttl time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.kv[key] = memEntry{value: value, expiresAt: expiry(ttl)}
	return nil
}

func (c *memCache) Del(_ context.Context, keys ...string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, k := range keys {
		delete(c.kv, k)
	}
	return nil
}

func (c *memCache) Exists(_ context.Context, key string) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.live(key)
	return ok, nil
}

func (c *memCache) Incr(_ context.Context, key string, ttl time.Duration) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.live(key)
	n := int64(1)
	if ok {
		for _, b := range e.value {
			n = n*10 + int64(b-'0')
		}
		n++
	} else {
		e.expiresAt = expiry(ttl)
	}
	e.value = []byte(itoa(n))
	c.kv[key] = e
	return n, nil
}

func (c *memCache) Lock(_ context.Context, key string, ttl time.Duration) (token string, acquired bool, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failLock {
		return "", false, errFakeCacheDown
	}
	if _, ok := c.live(key); ok {
		return "", false, nil
	}
	buf := make([]byte, 16)
	_, _ = rand.Read(buf)
	token = hex.EncodeToString(buf)
	c.kv[key] = memEntry{value: []byte(token), expiresAt: expiry(ttl)}
	return token, true, nil
}

func (c *memCache) Unlock(_ context.Context, key, token string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.live(key)
	if !ok || string(e.value) != token {
		return cache.ErrLockNotHeld
	}
	delete(c.kv, key)
	return nil
}

func (c *memCache) ZAdd(_ context.Context, key string, score float64, member string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.zset[key] == nil {
		c.zset[key] = map[string]float64{}
	}
	c.zset[key][member] = score
	return nil
}

func (c *memCache) ZRem(_ context.Context, key string, members ...string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, m := range members {
		delete(c.zset[key], m)
	}
	return nil
}

// Expire records the TTL. The fakes do not evict on it -- no test here
// depends on expiry happening, only on the call being made.
func (c *memCache) Expire(_ context.Context, _ string, _ time.Duration) error { return nil }

func (c *memCache) ZRank(_ context.Context, key, member string) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ordered := c.orderedLocked(key)
	for i, m := range ordered {
		if m == member {
			return int64(i), nil
		}
	}
	return 0, cache.ErrNotFound
}

func (c *memCache) ZRange(_ context.Context, key string, start, stop int64) ([]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ordered := c.orderedLocked(key)
	if start < 0 {
		start = 0
	}
	if stop < 0 || stop >= int64(len(ordered)) {
		stop = int64(len(ordered)) - 1
	}
	if start > stop {
		return nil, nil
	}
	return append([]string(nil), ordered[start:stop+1]...), nil
}

func (c *memCache) ZCard(_ context.Context, key string) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return int64(len(c.zset[key])), nil
}

// orderedLocked sorts by score then member, matching Redis' tie-break.
func (c *memCache) orderedLocked(key string) []string {
	members := make([]string, 0, len(c.zset[key]))
	for m := range c.zset[key] {
		members = append(members, m)
	}
	sort.Slice(members, func(i, j int) bool {
		si, sj := c.zset[key][members[i]], c.zset[key][members[j]]
		if si == sj {
			return members[i] < members[j]
		}
		return si < sj
	})
	return members
}

func (c *memCache) Ping(context.Context) error { return nil }
func (c *memCache) Close() error               { return nil }

func expiry(ttl time.Duration) time.Time {
	if ttl <= 0 {
		return time.Time{}
	}
	return time.Now().Add(ttl)
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

type fakeCacheError struct{}

func (fakeCacheError) Error() string { return "fake cache: unavailable" }

var errFakeCacheDown = fakeCacheError{}
