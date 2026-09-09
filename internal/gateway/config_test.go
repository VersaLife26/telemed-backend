package gateway

import (
	"os"
	"strings"
	"testing"
)

// setEnvs sets each env var, and registers cleanup to restore the previous
// value (or unset it) after the test.
func setEnvs(t *testing.T, kv map[string]string) {
	t.Helper()
	for k, v := range kv {
		prev, existed := os.LookupEnv(k)
		os.Setenv(k, v)
		t.Cleanup(func() {
			if existed {
				os.Setenv(k, prev)
			} else {
				os.Unsetenv(k)
			}
		})
	}
}

func baseValidEnv() map[string]string {
	return map[string]string{
		"SERVICE_NAME": "telemed-api-gateway",
		"HTTP_PORT":    "8080",
		"REDIS_URL":    "redis://localhost:6379",
		// Both halves: a key set is bound to the issuer allowed to sign with
		// it, so a JWKS URL alone is not a usable configuration.
		"KEYCLOAK_JWKS_URL":         "http://localhost:8180/certs",
		"KEYCLOAK_ISSUER":           "http://localhost:8180/realms/telemedicine",
		"TIMEZONE":                  "Asia/Colombo",
		"UPSTREAM_USER_URL":         "http://localhost:8081",
		"UPSTREAM_DOCTOR_URL":       "http://localhost:8082",
		"UPSTREAM_SCHEDULING_URL":   "http://localhost:8083",
		"UPSTREAM_CONSULTATION_URL": "http://localhost:8084",
		"UPSTREAM_PAYMENT_URL":      "http://localhost:8085",
		"UPSTREAM_NOTIFICATION_URL": "http://localhost:8086",
		"UPSTREAM_RECORD_URL":       "http://localhost:8087",
		"UPSTREAM_ADMIN_URL":        "http://localhost:8088",
	}
}

// TestLoad_CommaSeparatedListsAreSplit guards a real bug found while
// building this gateway: viper's GetStringSlice does not split a plain
// string env var on commas -- it returns the whole value as one element.
// ADMIN_IP_ALLOWLIST=10.0.0.0/8,10.1.0.0/16 must become two CIDRs, not one
// nonsensical one.
func TestLoad_CommaSeparatedListsAreSplit(t *testing.T) {
	env := baseValidEnv()
	env["ADMIN_IP_ALLOWLIST"] = "10.0.0.0/8, 10.1.0.0/16 ,192.168.1.1"
	env["CORS_PATIENT_ORIGINS"] = "https://patient.yourapp.lk,https://patient-staging.yourapp.lk"
	setEnvs(t, env)

	v := NewViper("telemed-api-gateway")
	cfg, err := Load(v)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	wantCIDRs := []string{"10.0.0.0/8", "10.1.0.0/16", "192.168.1.1"}
	if len(cfg.AdminIPAllowlist) != len(wantCIDRs) {
		t.Fatalf("AdminIPAllowlist = %#v, want %d entries", cfg.AdminIPAllowlist, len(wantCIDRs))
	}
	for i, want := range wantCIDRs {
		if cfg.AdminIPAllowlist[i] != want {
			t.Errorf("AdminIPAllowlist[%d] = %q, want %q", i, cfg.AdminIPAllowlist[i], want)
		}
	}

	wantOrigins := []string{"https://patient.yourapp.lk", "https://patient-staging.yourapp.lk"}
	if len(cfg.CORSPatientOrigins) != len(wantOrigins) {
		t.Fatalf("CORSPatientOrigins = %#v, want %d entries", cfg.CORSPatientOrigins, len(wantOrigins))
	}
}

func TestLoad_DefaultOriginListsSurviveWhenEnvUnset(t *testing.T) {
	setEnvs(t, baseValidEnv())

	v := NewViper("telemed-api-gateway")
	cfg, err := Load(v)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.CORSPatientOrigins) != 1 || cfg.CORSPatientOrigins[0] != "https://patient.yourapp.lk" {
		t.Fatalf("default CORSPatientOrigins = %#v, want [https://patient.yourapp.lk]", cfg.CORSPatientOrigins)
	}
}

func TestLoad_MissingUpstreamFailsValidation(t *testing.T) {
	env := baseValidEnv()
	delete(env, "UPSTREAM_ADMIN_URL")
	setEnvs(t, env)
	os.Unsetenv("UPSTREAM_ADMIN_URL")

	v := NewViper("telemed-api-gateway")
	v.Set("upstream_admin_url", "") // force empty regardless of leaked defaults from other tests
	_, err := Load(v)
	if err == nil {
		t.Fatal("expected Load to fail validation with an empty upstream URL")
	}
}

func TestConfig_ValidateRejectsBadTimezone(t *testing.T) {
	c := Config{
		ServiceName: "x", HTTPPort: 8080, RedisURL: "redis://x",
		// A key set is only usable when bound to its issuer, so both halves
		// are set here -- otherwise Validate would fail on the missing issuer
		// and this test would pass for the wrong reason.
		KeycloakJWKSURL: "http://x", KeycloakIssuer: "https://kc.invalid/realms/telemedicine",
		Timezone: "Not/A_Real_Zone",
	}
	if err := c.Validate(); err == nil {
		t.Fatal("expected Validate to reject an invalid timezone")
	}
}

func TestConfig_IsProd(t *testing.T) {
	if (Config{Env: "dev"}).IsProd() {
		t.Error("dev must not be prod")
	}
	if !(Config{Env: "prod"}).IsProd() {
		t.Error("prod must be prod")
	}
	if !(Config{Env: "production"}).IsProd() {
		t.Error("production must be prod")
	}
}

// TestConfig_ValidateRejectsAnOutOfRangeMaxInFlight pins the boot-time half of
// the load-shed overflow fix.
//
// LoadShed compares against an int32 counter. Before the fix the conversion was
// a bare int32(c.MaxInFlight) at the call site, so a MAX_IN_FLIGHT above the
// int32 range did not mean "a very large cap" -- it meant a wrapped one:
// 4294967297 becomes a cap of 1, and the gateway starts 503ing its own traffic
// at the second concurrent request; 2147483648 becomes negative, and load
// shedding switches off entirely. Neither says anything at runtime, which is
// exactly the class of misconfiguration this Config.Validate exists to catch
// at boot rather than at 3am.
func TestConfig_ValidateRejectsAnOutOfRangeMaxInFlight(t *testing.T) {
	base := func() Config {
		return Config{
			ServiceName: "x", HTTPPort: 8080, RedisURL: "redis://x",
			KeycloakJWKSURL: "http://x", KeycloakIssuer: "https://kc.invalid/realms/telemedicine",
			Timezone: "Asia/Colombo",
		}
	}

	for _, tc := range []struct {
		name        string
		maxInFlight int
	}{
		// Truncates to a cap of 1: the gateway would shed almost everything.
		{"wraps to a tiny positive cap", int(int64(1)<<32 + 1)},
		// Truncates to math.MinInt32: load shedding silently disabled.
		{"wraps to a negative cap", int(int64(1) << 31)},
		{"negative is not how shedding is disabled", -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := base()
			c.MaxInFlight = tc.maxInFlight
			err := c.Validate()
			if err == nil {
				t.Fatalf("Validate accepted MAX_IN_FLIGHT=%d; a value outside the int32 range is a wrapped cap, not a large one", tc.maxInFlight)
			}
			if !strings.Contains(err.Error(), "MAX_IN_FLIGHT") {
				t.Fatalf("error should name the offending key so an operator can fix it, got: %v", err)
			}
		})
	}

	t.Run("zero still means disabled and boots", func(t *testing.T) {
		c := base()
		c.MaxInFlight = 0
		c.UpstreamUserURL, c.UpstreamDoctorURL = "http://u", "http://d"
		c.UpstreamSchedulingURL, c.UpstreamConsultationURL = "http://s", "http://c"
		c.UpstreamPaymentURL, c.UpstreamNotificationURL = "http://p", "http://n"
		c.UpstreamRecordURL, c.UpstreamAdminURL = "http://r", "http://a"
		if err := c.Validate(); err != nil {
			t.Fatalf("MAX_IN_FLIGHT=0 is the documented way to disable shedding: %v", err)
		}
	})
}

// TestLoad_RedisPasswordIsBoundFromTheEnvironment pins the wiring for
// REDIS_PASSWORD, which the dev stack now requires (`requirepass
// telemed-dev-redis`).
//
// viper's AutomaticEnv does not discover a key that exists ONLY in the
// environment -- it can only overlay keys viper already knows about from a
// default or a config file. redis_password has no default (a default password
// is worse than none), so it is readable only because it appears in the
// explicit BindEnv list. Drop it from that list and the gateway starts sending
// no AUTH at all: against a passworded Redis every rate-limit and breaker call
// fails, and the limiter's documented fail-open turns the platform's entire
// throttle off at once.
func TestLoad_RedisPasswordIsBoundFromTheEnvironment(t *testing.T) {
	env := baseValidEnv()
	env["REDIS_PASSWORD"] = "telemed-dev-redis"
	setEnvs(t, env)

	v := NewViper("telemed-api-gateway")
	cfg, err := Load(v)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.RedisPassword != "telemed-dev-redis" {
		t.Fatalf("RedisPassword = %q, want %q -- REDIS_PASSWORD is not reaching Config",
			cfg.RedisPassword, "telemed-dev-redis")
	}
}
