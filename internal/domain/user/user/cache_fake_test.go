package user

import (
	"context"
	"sync"
	"time"

	"telemed/internal/platform/cache"
)

// fakeCache is a minimal in-memory cache.Cache for unit tests. It implements
// exactly what the OTP front door uses (Get/Set/Del/Incr) with real fixed-
// window TTL semantics matching RedisCache.Incr (the TTL is applied only on
// the window's first hit); the sorted-set and locking methods are unused by
// this domain and simply no-op.
type fakeCache struct {
	mu     sync.Mutex
	values map[string][]byte
	counts map[string]int64
	expiry map[string]time.Time
}

func newFakeCache() *fakeCache {
	return &fakeCache{
		values: map[string][]byte{},
		counts: map[string]int64{},
		expiry: map[string]time.Time{},
	}
}

var _ cache.Cache = (*fakeCache)(nil)

func (f *fakeCache) expiredLocked(key string) bool {
	exp, ok := f.expiry[key]
	return ok && time.Now().After(exp)
}

func (f *fakeCache) clearLocked(key string) {
	delete(f.values, key)
	delete(f.counts, key)
	delete(f.expiry, key)
}

func (f *fakeCache) Get(_ context.Context, key string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.expiredLocked(key) {
		f.clearLocked(key)
	}
	v, ok := f.values[key]
	if !ok {
		return nil, cache.ErrNotFound
	}
	return v, nil
}

func (f *fakeCache) Set(_ context.Context, key string, value []byte, ttl time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clearLocked(key)
	f.values[key] = value
	if ttl > 0 {
		f.expiry[key] = time.Now().Add(ttl)
	}
	return nil
}

func (f *fakeCache) Del(_ context.Context, keys ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, k := range keys {
		f.clearLocked(k)
	}
	return nil
}

func (f *fakeCache) Exists(_ context.Context, key string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.expiredLocked(key) {
		f.clearLocked(key)
	}
	if _, ok := f.values[key]; ok {
		return true, nil
	}
	_, ok := f.counts[key]
	return ok, nil
}

// Incr mirrors RedisCache's incrScript: the TTL is (re)applied only when the
// counter is created by this call, giving a fixed rather than sliding
// window -- exactly what the real Redis-backed rate limiter guarantees.
func (f *fakeCache) Incr(_ context.Context, key string, ttl time.Duration) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.expiredLocked(key) {
		f.clearLocked(key)
	}
	_, existed := f.counts[key]
	f.counts[key]++
	if !existed && ttl > 0 {
		f.expiry[key] = time.Now().Add(ttl)
	}
	return f.counts[key], nil
}

func (f *fakeCache) Lock(_ context.Context, _ string, _ time.Duration) (token string, acquired bool, err error) {
	return "fake-token", true, nil
}
func (f *fakeCache) Unlock(_ context.Context, _, _ string) error                 { return nil }
func (f *fakeCache) ZAdd(_ context.Context, _ string, _ float64, _ string) error { return nil }
func (f *fakeCache) ZRem(_ context.Context, _ string, _ ...string) error         { return nil }
func (f *fakeCache) ZRank(_ context.Context, _, _ string) (int64, error)         { return 0, cache.ErrNotFound }
func (f *fakeCache) ZRange(_ context.Context, _ string, _, _ int64) ([]string, error) {
	return nil, nil
}
func (f *fakeCache) ZCard(_ context.Context, _ string) (int64, error) { return 0, nil }
func (f *fakeCache) Ping(_ context.Context) error                     { return nil }
func (f *fakeCache) Close() error                                     { return nil }
