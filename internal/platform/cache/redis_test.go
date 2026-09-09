package cache

import "testing"

// REDIS_PASSWORD has to reach the client, or the whole `requirepass` exercise
// is decoration. Redis holds OTP hashes, every rate-limit counter and the
// suspension denylist: an unauthenticated Redis means `DEL
// otp:verify_attempts:*` removes the OTP brute-force cap.
func TestRedisOptionsCarryThePassword(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name         string
		opts         Options
		wantPassword string
		wantDB       int
	}{
		{
			name:         "explicit password reaches the client",
			opts:         Options{URL: "redis://localhost:6379", Password: "telemed-dev-redis"},
			wantPassword: "telemed-dev-redis",
		},
		{
			name:         "a password embedded in the url still works",
			opts:         Options{URL: "redis://:from-url@localhost:6379"},
			wantPassword: "from-url",
		},
		{
			name:         "an explicit password wins over the url's",
			opts:         Options{URL: "redis://:from-url@localhost:6379", Password: "explicit"},
			wantPassword: "explicit",
		},
		{
			name:         "no password configured stays empty rather than erroring",
			opts:         Options{URL: "redis://localhost:6379"},
			wantPassword: "",
		},
		{
			name:         "db selection survives alongside the password",
			opts:         Options{URL: "redis://localhost:6379", Password: "p", DB: 3},
			wantPassword: "p",
			wantDB:       3,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			opt, err := redisOptions(tc.opts)
			if err != nil {
				t.Fatalf("redisOptions: %v", err)
			}
			if opt.Password != tc.wantPassword {
				t.Fatalf("password = %q, want %q -- REDIS_PASSWORD never reached the client", opt.Password, tc.wantPassword)
			}
			if opt.DB != tc.wantDB {
				t.Fatalf("db = %d, want %d", opt.DB, tc.wantDB)
			}
			if opt.PoolSize <= 0 {
				t.Fatalf("pool size = %d", opt.PoolSize)
			}
		})
	}
}
