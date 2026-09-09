package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strings"
	"sync"
	"time"

	"telemed/internal/platform/cache"
)

// fakeCache is a minimal in-memory stand-in for cache.Cache, good enough to
// drive the breaker and rate-limit logic deterministically in unit tests
// without a real Redis. It implements every method of cache.Cache (not just
// breakerCache) so it can also stand in for platform middleware.RateLimit's
// cache.Cache parameter.
type fakeCache struct {
	mu      sync.Mutex
	values  map[string]fakeEntry
	zsets   map[string]map[string]float64
	failGet bool // when true, Get/Exists/Incr/Lock all fail, simulating a Redis outage
	// incrKeys records every key Incr was called with, in order. Rate limiting
	// is entirely a question of WHICH key a request is counted against, so a
	// test that only observes 200s and 429s cannot tell a per-user budget from
	// a per-IP one. This is how it tells.
	incrKeys []string
}

type fakeEntry struct {
	val     []byte
	expires time.Time // zero means no expiry
}

func newFakeCache() *fakeCache {
	return &fakeCache{values: map[string]fakeEntry{}, zsets: map[string]map[string]float64{}}
}

var _ cache.Cache = (*fakeCache)(nil)

func (f *fakeCache) expired(e fakeEntry) bool {
	return !e.expires.IsZero() && time.Now().After(e.expires)
}

func (f *fakeCache) Get(_ context.Context, key string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failGet {
		return nil, errSimulatedOutage
	}
	e, ok := f.values[key]
	if !ok || f.expired(e) {
		return nil, cache.ErrNotFound
	}
	return e.val, nil
}

func (f *fakeCache) Set(_ context.Context, key string, value []byte, ttl time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	e := fakeEntry{val: value}
	if ttl > 0 {
		e.expires = time.Now().Add(ttl)
	}
	f.values[key] = e
	return nil
}

func (f *fakeCache) Del(_ context.Context, keys ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, k := range keys {
		delete(f.values, k)
	}
	return nil
}

func (f *fakeCache) Exists(_ context.Context, key string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failGet {
		return false, errSimulatedOutage
	}
	e, ok := f.values[key]
	return ok && !f.expired(e), nil
}

func (f *fakeCache) Incr(_ context.Context, key string, ttl time.Duration) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.incrKeys = append(f.incrKeys, key)
	if f.failGet {
		return 0, errSimulatedOutage
	}
	e, ok := f.values[key]
	if !ok || f.expired(e) {
		e = fakeEntry{val: []byte("1")}
		if ttl > 0 {
			e.expires = time.Now().Add(ttl)
		}
		f.values[key] = e
		return 1, nil
	}
	n := parseInt(e.val) + 1
	e.val = []byte(formatInt(n))
	f.values[key] = e
	return n, nil
}

func (f *fakeCache) Lock(_ context.Context, key string, ttl time.Duration) (token string, acquired bool, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failGet {
		return "", false, errSimulatedOutage
	}
	if e, ok := f.values[key]; ok && !f.expired(e) {
		return "", false, nil
	}
	buf := make([]byte, 8)
	_, _ = rand.Read(buf)
	token = hex.EncodeToString(buf)
	f.values[key] = fakeEntry{val: []byte(token), expires: time.Now().Add(ttl)}
	return token, true, nil
}

func (f *fakeCache) Unlock(_ context.Context, key, token string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.values[key]
	if !ok || f.expired(e) || string(e.val) != token {
		return cache.ErrLockNotHeld
	}
	delete(f.values, key)
	return nil
}

func (f *fakeCache) ZAdd(_ context.Context, key string, score float64, member string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.zsets[key] == nil {
		f.zsets[key] = map[string]float64{}
	}
	f.zsets[key][member] = score
	return nil
}

func (f *fakeCache) ZRem(_ context.Context, key string, members ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, m := range members {
		delete(f.zsets[key], m)
	}
	return nil
}

func (f *fakeCache) ZRank(_ context.Context, key, member string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.zsets[key][member]
	if !ok {
		return 0, cache.ErrNotFound
	}
	return 0, nil
}

func (f *fakeCache) ZRange(_ context.Context, key string, _, _ int64) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.zsets[key]))
	for m := range f.zsets[key] {
		out = append(out, m)
	}
	return out, nil
}

func (f *fakeCache) ZCard(_ context.Context, key string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return int64(len(f.zsets[key])), nil
}

func (f *fakeCache) Ping(context.Context) error { return nil }
func (f *fakeCache) Close() error               { return nil }

var errSimulatedOutage = &simulatedOutageError{}

type simulatedOutageError struct{}

func (*simulatedOutageError) Error() string { return "fakeCache: simulated outage" }

func parseInt(b []byte) int64 {
	var n int64
	for _, c := range b {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int64(c-'0')
	}
	return n
}

func formatInt(n int64) string {
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

// rateLimitKeys returns every key the rate limiters counted against.
func (f *fakeCache) rateLimitKeys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.incrKeys))
	for _, k := range f.incrKeys {
		if strings.HasPrefix(k, "ratelimit:") {
			out = append(out, k)
		}
	}
	return out
}
