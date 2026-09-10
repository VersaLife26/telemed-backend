// Package gateway implements the edge: routing, auth verification, resilience
// (circuit breakers, retries, load shedding), rate limiting and the OpenAPI
// surface for every telemed frontend.
//
// The gateway owns no database. Everything it needs to survive a restart or
// scale to N replicas lives in Redis or is re-derived from configuration.
package gateway

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/spf13/viper"

	"telemed/internal/platform/config"
)

// Config holds every setting the gateway needs. Unlike a domain service, it
// deliberately has no DatabaseURL, no NATS settings and no outbox tuning --
// the gateway proxies and protects, it does not persist.
type Config struct {
	ServiceName string
	Env         string
	HTTPPort    int
	LogLevel    string

	RedisURL      string
	RedisPassword string
	RedisDB       int

	// The platform has TWO token issuers and the gateway must accept both
	// (ADR-010): telemed-user-service signs patient and doctor tokens so
	// phone-OTP login survives a Keycloak outage, Keycloak signs admin
	// tokens. UserIssuer is the literal "iss" claim user-service mints (its
	// JWT_ISSUER), not a URL.
	KeycloakIssuer   string
	AdminIssuer      string
	AdminJWKSURL     string
	KeycloakAudience string
	KeycloakJWKSURL  string
	UserIssuer       string
	UserJWKSURL      string

	// TrustedProxies are the CIDRs whose X-Forwarded-For / X-Real-IP headers
	// the gateway believes. Everything else has its client address taken from
	// the TCP peer, which no header can forge. The gateway terminates client
	// connections, so this value is what the admin IP allowlist and every rate
	// limit bucket key on: a forgeable client IP makes both decorative.
	TrustedProxies []string

	TraceSampling float64

	ShutdownGrace  time.Duration
	RequestTimeout time.Duration
	Timezone       string

	// RoutesFile optionally points at an external route-table file. When
	// empty (or the file does not exist) the gateway falls back to the
	// table embedded in the binary at build time. This is the knob that
	// lets an operator add a tenth backend service without a recompile:
	// drop a new file, set ROUTES_FILE, restart the pods.
	RoutesFile string

	// Upstream base URLs, one per backend service. These are the only place
	// a port number appears; the route table refers to services by name.
	UpstreamUserURL         string
	UpstreamDoctorURL       string
	UpstreamSchedulingURL   string
	UpstreamConsultationURL string
	UpstreamPaymentURL      string
	UpstreamNotificationURL string
	UpstreamRecordURL       string
	UpstreamAdminURL        string

	// CORS: three distinct origin lists. Never merge patient and admin --
	// that is precisely the mistake that makes the admin surface reachable
	// from a patient browser tab.
	CORSPatientOrigins []string
	CORSDoctorOrigins  []string
	CORSAdminOrigins   []string

	// AdminIPAllowlist is enforced at the gateway in addition to whatever the
	// admin service enforces itself -- belt and braces, per the docs.
	AdminIPAllowlist []string

	// MaxInFlight caps total concurrent proxied requests. Above this the
	// gateway sheds load with 503 rather than queueing until every request
	// times out together.
	MaxInFlight int

	// Circuit breaker defaults, overridable per upstream in the route file.
	CircuitFailureThreshold int64
	CircuitOpenSeconds      int
	CircuitProbeSeconds     int

	// Body size caps, in bytes, by timeout/route class.
	MaxBodyBytesFast   int64
	MaxBodyBytesWrite  int64
	MaxBodyBytesUpload int64
}

// Validate fails fast at boot rather than at 3am when the first request
// arrives missing a dependency it needed all along.
func (c Config) Validate() error {
	var missing []string
	if c.ServiceName == "" {
		missing = append(missing, "SERVICE_NAME")
	}
	if c.HTTPPort == 0 {
		missing = append(missing, "HTTP_PORT")
	}
	if c.RedisURL == "" {
		missing = append(missing, "REDIS_URL")
	}
	if len(c.IssuerKeys()) == 0 {
		// Either issuer alone is a valid deployment; neither is not. And a
		// JWKS URL without its matching issuer is not a deployment at all --
		// the gateway binds each key set to the issuer allowed to use it, so
		// a key set nobody claims can verify nothing. Name both halves in the
		// error rather than letting an operator configure one and wonder why
		// every token is rejected.
		missing = append(missing, "KEYCLOAK_ISSUER+KEYCLOAK_JWKS_URL and/or USER_ISSUER+USER_JWKS_URL")
	}
	// Two controls on the admin surface fail OPEN when unconfigured, which is
	// what makes a developer stack usable and what would make a production
	// stack quietly undefended: middleware.IPAllowlist passes everything on an
	// empty CIDR list, and RequireTokenIssuer passes everything on an empty
	// issuer list. Neither failure is visible at runtime -- the admin surface
	// simply answers. So the deployment must name both, and prod refuses to
	// boot without them rather than serving with a documented control that is
	// in fact decoration.
	if c.IsProd() {
		if len(c.AdminIPAllowlist) == 0 {
			missing = append(missing, "ADMIN_IP_ALLOWLIST (required in prod: an empty allowlist admits every source address)")
		}
		if len(c.AdminTokenIssuers()) == 0 {
			missing = append(missing, "ADMIN_ISSUER (or KEYCLOAK_ISSUER) (required in prod: without it any trusted issuer may assert an admin role)")
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("config: missing required keys: %s", strings.Join(missing, ", "))
	}
	if _, err := time.LoadLocation(c.Timezone); err != nil {
		return fmt.Errorf("config: invalid TIMEZONE %q: %w", c.Timezone, err)
	}
	// The in-flight counter LoadShed compares against is an int32, so a
	// MAX_IN_FLIGHT outside that range is not a large cap -- it is a wrapped
	// one. 4294967297 truncates to a cap of 1 (the gateway 503s its own
	// traffic from the second concurrent request onwards) and 2147483648
	// truncates to a negative number (load shedding silently switches off).
	// Neither is visible at runtime, so refuse the value at boot instead.
	// Zero keeps its documented meaning: shedding disabled.
	if c.MaxInFlight < 0 || c.MaxInFlight > math.MaxInt32 {
		return fmt.Errorf("config: MAX_IN_FLIGHT must be between 0 and %d, got %d", math.MaxInt32, c.MaxInFlight)
	}
	for name, url := range c.upstreams() {
		if url == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("config: missing required upstream URLs: %s", strings.Join(missing, ", "))
	}
	return nil
}

// upstreams returns the configured base URL for every backend service, keyed
// by the logical name the route table uses to refer to it.
func (c Config) upstreams() map[string]string {
	return map[string]string{
		"user-service":         c.UpstreamUserURL,
		"doctor-service":       c.UpstreamDoctorURL,
		"scheduling-service":   c.UpstreamSchedulingURL,
		"consultation-service": c.UpstreamConsultationURL,
		"payment-service":      c.UpstreamPaymentURL,
		"notification-service": c.UpstreamNotificationURL,
		"record-service":       c.UpstreamRecordURL,
		"admin-service":        c.UpstreamAdminURL,
	}
}

// IsProd reports whether the service runs in a production-like environment.
func (c Config) IsProd() bool { return c.Env == "prod" || c.Env == "production" }

// AdminTokenIssuers is the set of issuers permitted to speak for the admin
// surface. Keycloak, and only Keycloak: it is the issuer that enforces SAML
// SSO and 2FA, which is the entire reason ADR-010 keeps it as a hard
// dependency for staff while patients get a Keycloak-independent path.
//
// An empty result disables the check, which is correct for a developer stack
// that runs no Keycloak at all. Validate refuses that combination in prod --
// see RequireTokenIssuer for why an unbound admin surface is a 2FA bypass and
// not merely a role question.
func (c Config) AdminTokenIssuers() []string {
	// ADMIN_ISSUER wins: it names the administrator identity provider
	// directly, and a deployment that has moved off Keycloak must not keep
	// trusting a stale KEYCLOAK_ISSUER for admin roles.
	if iss := strings.TrimSpace(c.AdminIssuer); iss != "" {
		return []string{iss}
	}
	if iss := strings.TrimSpace(c.KeycloakIssuer); iss != "" {
		return []string{iss}
	}
	return nil
}

// JWKSURLs returns every key set the gateway should trust, skipping any that
// are unconfigured. Mirrors config.Base.JWKSURLs; the gateway keeps its own
// flat Config rather than embedding config.Base, so the helper is repeated
// here rather than inherited.
func (c Config) JWKSURLs() []string {
	var out []string
	for _, u := range []string{c.UserJWKSURL, c.KeycloakJWKSURL, c.AdminJWKSURL} {
		if strings.TrimSpace(u) != "" {
			out = append(out, u)
		}
	}
	return out
}

// IssuerKeys binds each configured issuer to the ONE key set allowed to sign
// for it, ready to hand to platmw.AuthConfig. Prefer this over
// JWKSURLs()+TrustedIssuers(): merging both key sets and then checking "iss"
// lets either issuer sign for the other, because the merged set matches on
// "kid" alone and "iss" is chosen by whoever signs.
func (c Config) IssuerKeys() map[string]string {
	out := make(map[string]string, 2)
	if c.UserIssuer != "" && c.UserJWKSURL != "" {
		out[c.UserIssuer] = c.UserJWKSURL
	}
	if c.KeycloakIssuer != "" && c.KeycloakJWKSURL != "" {
		out[c.KeycloakIssuer] = c.KeycloakJWKSURL
	}
	if c.AdminIssuer != "" && c.AdminJWKSURL != "" {
		out[c.AdminIssuer] = c.AdminJWKSURL
	}
	return out
}

// TrustedIssuers returns the allowlist of acceptable "iss" claims. A token
// whose signature verifies against one of the key sets but whose issuer is not
// on this list is rejected -- otherwise the patient issuer could mint an admin
// token, since the gateway trusts both key sets.
func (c Config) TrustedIssuers() []string {
	var out []string
	for _, i := range []string{c.UserIssuer, c.KeycloakIssuer, c.AdminIssuer} {
		if strings.TrimSpace(i) != "" {
			out = append(out, i)
		}
	}
	return out
}

// NewViper builds the shared config loader (env + .env, same as every other
// telemed service) and layers on the gateway-specific keys and defaults.
func NewViper(serviceName string) *viper.Viper {
	v := config.New(serviceName)

	// The platform default (30s) is shorter than the gateway's own "upload"
	// route class (60s). chi's global Timeout middleware and our per-route
	// context.WithTimeout compose naturally (the earlier deadline always
	// wins), so a global timeout under 60s would silently clip every
	// upload-class route no matter what the route table says. Override it
	// here so the route table's own classes are the ones actually in force;
	// REQUEST_TIMEOUT in the environment still wins over this default.
	v.SetDefault("request_timeout", 65*time.Second)

	v.SetDefault("routes_file", "")

	v.SetDefault("upstream_user_url", "http://localhost:8081")
	v.SetDefault("upstream_doctor_url", "http://localhost:8082")
	v.SetDefault("upstream_scheduling_url", "http://localhost:8083")
	v.SetDefault("upstream_consultation_url", "http://localhost:8084")
	v.SetDefault("upstream_payment_url", "http://localhost:8085")
	v.SetDefault("upstream_notification_url", "http://localhost:8086")
	v.SetDefault("upstream_record_url", "http://localhost:8087")
	v.SetDefault("upstream_admin_url", "http://localhost:8088")

	v.SetDefault("cors_patient_origins", []string{"https://patient.yourapp.lk"})
	v.SetDefault("cors_doctor_origins", []string{"https://doctor.yourapp.lk"})
	v.SetDefault("cors_admin_origins", []string{"https://admin.yourapp.lk"})

	v.SetDefault("admin_ip_allowlist", []string{})
	v.SetDefault("trusted_proxies", []string{})

	v.SetDefault("max_in_flight", 2000)

	v.SetDefault("circuit_failure_threshold", 5)
	v.SetDefault("circuit_open_seconds", 30)
	v.SetDefault("circuit_probe_seconds", 5)

	v.SetDefault("max_body_bytes_fast", 1<<20)    // 1 MiB
	v.SetDefault("max_body_bytes_write", 4<<20)   // 4 MiB
	v.SetDefault("max_body_bytes_upload", 15<<20) // 15 MiB, headroom over the documented 10MB cap

	for _, k := range []string{
		"routes_file",
		"upstream_user_url", "upstream_doctor_url", "upstream_scheduling_url",
		"upstream_consultation_url", "upstream_payment_url", "upstream_notification_url",
		"upstream_record_url", "upstream_admin_url",
		"cors_patient_origins", "cors_doctor_origins", "cors_admin_origins",
		"admin_ip_allowlist", "trusted_proxies", "max_in_flight",
		"circuit_failure_threshold", "circuit_open_seconds", "circuit_probe_seconds",
		"max_body_bytes_fast", "max_body_bytes_write", "max_body_bytes_upload",
	} {
		_ = v.BindEnv(k)
	}
	return v
}

// Load unmarshals the gateway Config from an already-constructed viper
// instance (see NewViper).
func Load(v *viper.Viper) (Config, error) {
	var c Config
	c.ServiceName = v.GetString("service_name")
	c.Env = v.GetString("env")
	c.HTTPPort = v.GetInt("http_port")
	c.LogLevel = v.GetString("log_level")

	c.RedisURL = v.GetString("redis_url")
	c.RedisPassword = v.GetString("redis_password")
	c.RedisDB = v.GetInt("redis_db")

	c.KeycloakIssuer = v.GetString("keycloak_issuer")
	c.AdminIssuer = v.GetString("admin_issuer")
	c.AdminJWKSURL = v.GetString("admin_jwks_url")
	c.KeycloakAudience = v.GetString("keycloak_audience")
	c.KeycloakJWKSURL = v.GetString("keycloak_jwks_url")
	c.UserIssuer = v.GetString("user_issuer")
	c.UserJWKSURL = v.GetString("user_jwks_url")

	c.TraceSampling = v.GetFloat64("trace_sampling")

	c.ShutdownGrace = v.GetDuration("shutdown_grace")
	c.RequestTimeout = v.GetDuration("request_timeout")
	c.Timezone = v.GetString("timezone")

	c.RoutesFile = v.GetString("routes_file")

	c.UpstreamUserURL = v.GetString("upstream_user_url")
	c.UpstreamDoctorURL = v.GetString("upstream_doctor_url")
	c.UpstreamSchedulingURL = v.GetString("upstream_scheduling_url")
	c.UpstreamConsultationURL = v.GetString("upstream_consultation_url")
	c.UpstreamPaymentURL = v.GetString("upstream_payment_url")
	c.UpstreamNotificationURL = v.GetString("upstream_notification_url")
	c.UpstreamRecordURL = v.GetString("upstream_record_url")
	c.UpstreamAdminURL = v.GetString("upstream_admin_url")

	c.CORSPatientOrigins = stringListEnv(v, "cors_patient_origins")
	c.CORSDoctorOrigins = stringListEnv(v, "cors_doctor_origins")
	c.CORSAdminOrigins = stringListEnv(v, "cors_admin_origins")

	c.AdminIPAllowlist = stringListEnv(v, "admin_ip_allowlist")
	c.TrustedProxies = stringListEnv(v, "trusted_proxies")

	c.MaxInFlight = v.GetInt("max_in_flight")

	c.CircuitFailureThreshold = int64(v.GetInt("circuit_failure_threshold"))
	c.CircuitOpenSeconds = v.GetInt("circuit_open_seconds")
	c.CircuitProbeSeconds = v.GetInt("circuit_probe_seconds")

	c.MaxBodyBytesFast = v.GetInt64("max_body_bytes_fast")
	c.MaxBodyBytesWrite = v.GetInt64("max_body_bytes_write")
	c.MaxBodyBytesUpload = v.GetInt64("max_body_bytes_upload")

	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

// stringListEnv reads key as a string list, tolerating the two shapes it can
// legitimately arrive in: an actual []string from v.SetDefault, or a single
// comma-separated string from an environment variable. viper's
// GetStringSlice does NOT split a plain string on commas -- it returns the
// whole value as one element -- which would silently turn
// "ADMIN_IP_ALLOWLIST=10.0.0.0/8,10.1.0.0/16" into a single, wrong CIDR.
func stringListEnv(v *viper.Viper, key string) []string {
	switch val := v.Get(key).(type) {
	case []string:
		return val
	case string:
		return splitTrim(val)
	case []any:
		out := make([]string, 0, len(val))
		for _, item := range val {
			if s, ok := item.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

func splitTrim(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
