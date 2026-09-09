package scheduling

import (
	"os"
	"testing"

	"telemed/internal/platform/config"
)

// TestRedisPasswordReachesServiceConfig pins the wiring for REDIS_PASSWORD,
// which the dev stack now requires (`requirepass telemed-dev-redis`).
//
// Two links have to hold, and neither is visible from reading serviceConfig:
//
//  1. viper's AutomaticEnv cannot discover a key that exists only in the
//     environment -- it overlays keys viper already knows from a default or a
//     config file. redis_password has no default (a default password is worse
//     than no password), so it is readable at all only because config.New puts
//     it on the explicit BindEnv list.
//  2. config.Base is embedded with `mapstructure:",squash"`, so the field has
//     to survive the squash into this service's own config struct.
//
// Break either and the service sends no AUTH. Against a passworded Redis that
// is a hard failure at every cache call, which for this service includes the
// booking early-reject lock, the waitlist queue index and the per-principal
// booking rate limiter.
func TestRedisPasswordReachesServiceConfig(t *testing.T) {
	const want = "telemed-dev-redis"

	prev, existed := os.LookupEnv("REDIS_PASSWORD")
	t.Cleanup(func() {
		if existed {
			os.Setenv("REDIS_PASSWORD", prev)
		} else {
			os.Unsetenv("REDIS_PASSWORD")
		}
	})
	os.Setenv("REDIS_PASSWORD", want)

	v := config.New(serviceName)
	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		t.Fatalf("unmarshal config: %v", err)
	}
	if cfg.RedisPassword != want {
		t.Fatalf("RedisPassword = %q, want %q -- REDIS_PASSWORD is not reaching cache.Options",
			cfg.RedisPassword, want)
	}
}
