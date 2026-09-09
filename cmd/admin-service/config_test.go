package main

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"telemed/internal/platform/config"
)

// validConfig is the minimum this service needs to boot anywhere. Each test
// below removes exactly one thing from it, so a failure names the field that
// caused it rather than "config invalid".
func validConfig(env string) appConfig {
	return appConfig{
		Base: config.Base{
			ServiceName:     "telemed-admin-service",
			Env:             env,
			HTTPPort:        8088,
			DatabaseURL:     "postgres://telemed_admin_app:x@postgres:5432/telemed_admin?sslmode=require",
			Timezone:        "Asia/Colombo",
			KeycloakIssuer:  "https://auth.yourapp.lk/realms/telemedicine",
			KeycloakJWKSURL: "https://auth.yourapp.lk/realms/telemedicine/protocol/openid-connect/certs",
		},
		AdminIPAllowlist: "10.0.0.0/8,203.0.113.7",
		DocPresignTTL:    5 * time.Minute,
	}
}

// TestConfig_ProdRefusesToBootWithoutAnAdminIPAllowlist is security review F17.
//
// middleware.IPAllowlist treats an empty list as "not configured" and serves
// the request. Its own comment said the readiness check asserted the variable
// separately, and no such check existed -- so ADMIN_IP_ALLOWLIST="" (which is
// what telemed-infra's ConfigMap shipped) meant the doctor-credentialing queue,
// the user directory and the audit log were served to any address that could
// reach the pod, with nothing at runtime saying so.
//
// Delete the prod branch in appConfig.Validate and this test fails: the empty
// allowlist is accepted and the service boots wide open.
func TestConfig_ProdRefusesToBootWithoutAnAdminIPAllowlist(t *testing.T) {
	for _, env := range []string{"prod", "production"} {
		t.Run(env, func(t *testing.T) {
			cfg := validConfig(env)
			cfg.AdminIPAllowlist = ""

			err := cfg.Validate()
			require.Error(t, err, "an admin service must not boot in %s with an open admin surface", env)
			require.Contains(t, err.Error(), "ADMIN_IP_ALLOWLIST")

			// Whitespace is not configuration either.
			cfg.AdminIPAllowlist = "   ,  , "
			require.Error(t, cfg.Validate())
		})
	}
}

// TestConfig_ProdRefusesToBootWithoutTheAdminIssuer: Keycloak is the only
// issuer permitted to assert an admin role (ADR-010, and F1's issuer binding).
// Unset, Base.IssuerKeys() silently omits it and this service trusts
// user-service -- a phone-OTP issuer -- alone, for the surface whose entire
// justification is SAML SSO and enforced 2FA.
func TestConfig_ProdRefusesToBootWithoutTheAdminIssuer(t *testing.T) {
	cases := map[string]func(*appConfig){
		"no issuer":   func(c *appConfig) { c.KeycloakIssuer = "" },
		"no jwks url": func(c *appConfig) { c.KeycloakJWKSURL = "" },
		"neither": func(c *appConfig) {
			c.KeycloakIssuer = ""
			c.KeycloakJWKSURL = ""
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := validConfig("prod")
			mutate(&cfg)
			err := cfg.Validate()
			require.Error(t, err)
			require.Contains(t, err.Error(), "KEYCLOAK_ISSUER")
		})
	}
}

// TestConfig_ProdReportsEveryProblemAtOnce. An operator fixing a deployment
// should not have to restart the pod once per missing variable to discover the
// next one.
func TestConfig_ProdReportsEveryProblemAtOnce(t *testing.T) {
	cfg := validConfig("prod")
	cfg.AdminIPAllowlist = ""
	cfg.KeycloakIssuer = ""

	err := cfg.Validate()
	require.Error(t, err)
	require.Contains(t, err.Error(), "ADMIN_IP_ALLOWLIST")
	require.Contains(t, err.Error(), "KEYCLOAK_ISSUER")
}

// TestConfig_DevStillBootsWithoutAnAllowlist. The control has to be enforceable
// without making local development impossible, or it gets disabled rather than
// configured.
func TestConfig_DevStillBootsWithoutAnAllowlist(t *testing.T) {
	for _, env := range []string{"dev", "development", "staging", ""} {
		cfg := validConfig(env)
		cfg.AdminIPAllowlist = ""
		require.NoError(t, cfg.Validate(), "env %q must still boot without an allowlist", env)
	}
}

// TestConfig_RejectsAnAllowlistThatDoesNotParse is the subtler half of F17.
//
// middleware.IPAllowlist logs and SKIPS a malformed entry. So an allowlist of
// two typo'd CIDRs parses to an EMPTY list, which is the fail-open case
// reached from a config file that looks configured -- the worst of both, since
// the variable is set and a reviewer scanning the ConfigMap sees a control.
// Catching it at boot turns that into a startup error naming the entry.
func TestConfig_RejectsAnAllowlistThatDoesNotParse(t *testing.T) {
	cases := []string{
		"10.0.0/8",                    // not a CIDR
		"10.0.0.0/8,192.168.0.0.0/16", // one good, one with an extra octet
		"not-an-ip",
		"10.0.0.0/33", // prefix out of range
	}
	for _, raw := range cases {
		t.Run(raw, func(t *testing.T) {
			cfg := validConfig("prod")
			cfg.AdminIPAllowlist = raw
			err := cfg.Validate()
			require.Error(t, err, "%q must not be accepted as an allowlist", raw)
			require.Contains(t, err.Error(), "ADMIN_IP_ALLOWLIST")
		})
	}
}

// TestConfig_AcceptsTheFormsIPAllowlistItselfAccepts. The validator and the
// middleware must agree on what a valid entry is, or boot rejects a list that
// would have worked (or, worse, accepts one that would not). Both go through
// middleware.NewTrustedProxies, which applies exactly IPAllowlist's parsing
// rules including the bare-IP-as-/32 shorthand.
func TestConfig_AcceptsTheFormsIPAllowlistItselfAccepts(t *testing.T) {
	cases := []string{
		"10.0.0.0/8",
		"203.0.113.7",                  // bare IPv4 -> /32
		"2001:db8::1",                  // bare IPv6 -> /128
		"2001:db8::/32",                //
		" 10.0.0.0/8 , 172.16.0.0/12 ", // whitespace around entries
	}
	for _, raw := range cases {
		t.Run(strings.TrimSpace(raw), func(t *testing.T) {
			cfg := validConfig("prod")
			cfg.AdminIPAllowlist = raw
			require.NoError(t, cfg.Validate(), "%q is a form middleware.IPAllowlist accepts", raw)
		})
	}
}

// TestConfig_BaseValidationStillRuns guards against the prod branch being
// added in a way that skips config.Base.Validate.
func TestConfig_BaseValidationStillRuns(t *testing.T) {
	cfg := validConfig("prod")
	cfg.DatabaseURL = ""
	err := cfg.Validate()
	require.Error(t, err)
	require.Contains(t, err.Error(), "DATABASE_URL")
}
