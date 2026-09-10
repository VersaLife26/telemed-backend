package cache

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// unlockScript releases a lock only when the stored token matches, making
// Unlock safe against the classic "my TTL expired and someone else took the
// lock, then I deleted theirs" race.
var unlockScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
	return redis.call("DEL", KEYS[1])
end
return 0
`)

// incrScript increments and sets the TTL only when the key was just created,
// giving a fixed rather than sliding rate-limit window.
var incrScript = redis.NewScript(`
local n = redis.call("INCR", KEYS[1])
if n == 1 then
	redis.call("PEXPIRE", KEYS[1], ARGV[1])
end
return n
`)

// RedisCache is the go-redis backed implementation of Cache. It works against a
// standalone server, a Sentinel-managed primary, or a cluster, depending on the
// URL supplied.
type RedisCache struct {
	client redis.UniversalClient
}

var _ Cache = (*RedisCache)(nil)

// Options configures the Redis connection.
type Options struct {
	URL           string
	Password      string
	DB            int
	SentinelAddrs []string
	MasterName    string
	PoolSize      int
}

// NewRedis dials Redis and verifies the connection. When SentinelAddrs and
// MasterName are set it builds a failover client so a primary election does not
// require a service restart.
func NewRedis(ctx context.Context, o Options) (*RedisCache, error) {
	var client redis.UniversalClient

	switch {
	case len(o.SentinelAddrs) > 0 && o.MasterName != "":
		client = redis.NewFailoverClient(&redis.FailoverOptions{
			MasterName:    o.MasterName,
			SentinelAddrs: o.SentinelAddrs,
			Password:      o.Password,
			DB:            o.DB,
			PoolSize:      poolSize(o.PoolSize),
		})
	default:
		opt, err := redisOptions(o)
		if err != nil {
			return nil, err
		}
		client = redis.NewClient(opt)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := client.Ping(pingCtx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("cache: ping redis: %w", err)
	}
	return &RedisCache{client: client}, nil
}

// redisOptions turns the service's Options into a go-redis client config.
//
// Split out of NewRedis so the settings that are security-relevant --
// principally whether REDIS_PASSWORD actually reaches the client -- can be
// asserted without a live server. Redis holds OTP hashes, every rate-limit
// counter and the suspension denylist, so "the password was configured but
// never passed through" is not a config bug, it is an unauthenticated Redis.
//
// An explicit Password overrides one embedded in REDIS_URL; an empty one
// leaves the URL's intact, so either form works and setting both is not a
// silent conflict.
func redisOptions(o Options) (*redis.Options, error) {
	opt, err := redis.ParseURL(o.URL)
	if err != nil {
		return nil, fmt.Errorf("cache: parse redis url: %w", err)
	}
	if o.Password != "" {
		opt.Password = o.Password
	}
	if o.DB != 0 {
		opt.DB = o.DB
	}
	opt.PoolSize = poolSize(o.PoolSize)
	return opt, nil
}

func poolSize(n int) int {
	if n > 0 {
		return n
	}
	return 20
}

// Client exposes the underlying handle for the rare call that needs a command
// outside the Cache contract. Prefer extending the interface over reaching for
// this; every use is a 2040 migration cost.
func (r *RedisCache) Client() redis.UniversalClient { return r.client }

func (r *RedisCache) Get(ctx context.Context, key string) ([]byte, error) {
	b, err := r.client.Get(ctx, key).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("cache: get %s: %w", key, err)
	}
	return b, nil
}

func (r *RedisCache) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	if err := r.client.Set(ctx, key, value, ttl).Err(); err != nil {
		return fmt.Errorf("cache: set %s: %w", key, err)
	}
	return nil
}

func (r *RedisCache) Del(ctx context.Context, keys ...string) error {
	if len(keys) == 0 {
		return nil
	}
	if err := r.client.Del(ctx, keys...).Err(); err != nil {
		return fmt.Errorf("cache: del: %w", err)
	}
	return nil
}

func (r *RedisCache) Exists(ctx context.Context, key string) (bool, error) {
	n, err := r.client.Exists(ctx, key).Result()
	if err != nil {
		return false, fmt.Errorf("cache: exists %s: %w", key, err)
	}
	return n > 0, nil
}

func (r *RedisCache) Incr(ctx context.Context, key string, ttl time.Duration) (int64, error) {
	n, err := incrScript.Run(ctx, r.client, []string{key}, ttl.Milliseconds()).Int64()
	if err != nil {
		return 0, fmt.Errorf("cache: incr %s: %w", key, err)
	}
	return n, nil
}

func (r *RedisCache) Lock(ctx context.Context, key string, ttl time.Duration) (token string, acquired bool, err error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", false, fmt.Errorf("cache: lock token: %w", err)
	}
	token = hex.EncodeToString(buf)

	ok, err := r.client.SetNX(ctx, key, token, ttl).Result()
	if err != nil {
		return "", false, fmt.Errorf("cache: lock %s: %w", key, err)
	}
	if !ok {
		return "", false, nil
	}
	return token, true, nil
}

func (r *RedisCache) Unlock(ctx context.Context, key, token string) error {
	// Best effort: the caller is usually in a defer, and a failed unlock is
	// self-healing because the lock carries a TTL.
	n, err := unlockScript.Run(ctx, r.client, []string{key}, token).Int64()
	if err != nil {
		return fmt.Errorf("cache: unlock %s: %w", key, err)
	}
	if n == 0 {
		return ErrLockNotHeld
	}
	return nil
}

func (r *RedisCache) ZAdd(ctx context.Context, key string, score float64, member string) error {
	if err := r.client.ZAdd(ctx, key, redis.Z{Score: score, Member: member}).Err(); err != nil {
		return fmt.Errorf("cache: zadd %s: %w", key, err)
	}
	return nil
}

// Expire sets a TTL on an existing key. A missing key is not an error: the
// caller's intent is "do not let this outlive the TTL", and a key that is
// already gone satisfies that.
func (r *RedisCache) Expire(ctx context.Context, key string, ttl time.Duration) error {
	return r.client.Expire(ctx, key, ttl).Err()
}

func (r *RedisCache) ZRem(ctx context.Context, key string, members ...string) error {
	if len(members) == 0 {
		return nil
	}
	args := make([]any, len(members))
	for i, m := range members {
		args[i] = m
	}
	if err := r.client.ZRem(ctx, key, args...).Err(); err != nil {
		return fmt.Errorf("cache: zrem %s: %w", key, err)
	}
	return nil
}

func (r *RedisCache) ZRank(ctx context.Context, key, member string) (int64, error) {
	n, err := r.client.ZRank(ctx, key, member).Result()
	if errors.Is(err, redis.Nil) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("cache: zrank %s: %w", key, err)
	}
	return n, nil
}

func (r *RedisCache) ZRange(ctx context.Context, key string, start, stop int64) ([]string, error) {
	vals, err := r.client.ZRange(ctx, key, start, stop).Result()
	if err != nil {
		return nil, fmt.Errorf("cache: zrange %s: %w", key, err)
	}
	return vals, nil
}

func (r *RedisCache) ZCard(ctx context.Context, key string) (int64, error) {
	n, err := r.client.ZCard(ctx, key).Result()
	if err != nil {
		return 0, fmt.Errorf("cache: zcard %s: %w", key, err)
	}
	return n, nil
}

func (r *RedisCache) Ping(ctx context.Context) error { return r.client.Ping(ctx).Err() }
func (r *RedisCache) Close() error                   { return r.client.Close() }
