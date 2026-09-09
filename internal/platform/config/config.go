// Package config loads service configuration from environment variables and
// optional .env files. Configuration is read once at boot and passed explicitly
// down the call graph -- no globals, no reflection-based magic. This keeps the
// binary auditable in 2075 when whoever maintains it was not born yet.
package config

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/viper"
)

// Base holds the configuration keys every telemed service shares. Services
// embed it in their own Config struct and add service-specific fields.
type Base struct {
	ServiceName string `mapstructure:"service_name"`
	Env         string `mapstructure:"env"` // dev | staging | prod
	HTTPPort    int    `mapstructure:"http_port"`
	GRPCPort    int    `mapstructure:"grpc_port"`
	LogLevel    string `mapstructure:"log_level"`

	DatabaseURL         string        `mapstructure:"database_url"`
	DatabaseMaxConns    int32         `mapstructure:"database_max_conns"`
	DatabaseMinConns    int32         `mapstructure:"database_min_conns"`
	DatabaseMaxConnLife time.Duration `mapstructure:"database_max_conn_life"`

	RedisURL      string `mapstructure:"redis_url"`
	RedisPassword string `mapstructure:"redis_password"`
	RedisDB       int    `mapstructure:"redis_db"`

	NATSURL           string `mapstructure:"nats_url"`
	NATSStream        string `mapstructure:"nats_stream"`
	NATSCredentials   string `mapstructure:"nats_credentials"`
	NATSMaxBytes      int64  `mapstructure:"nats_max_bytes"`
	OutboxPollSeconds int    `mapstructure:"outbox_poll_seconds"`
	OutboxBatchSize   int    `mapstructure:"outbox_batch_size"`

	// The platform has two token issuers -- telemed-user-service for patients
	// and doctors, Keycloak for admins. Every service accepts both; see
	// middleware.Authenticator for why.
	KeycloakIssuer   string `mapstructure:"keycloak_issuer"`
	KeycloakAudience string `mapstructure:"keycloak_audience"`
	KeycloakJWKSURL  string `mapstructure:"keycloak_jwks_url"`
	UserIssuer       string `mapstructure:"user_issuer"`
	UserJWKSURL      string `mapstructure:"user_jwks_url"`

	// TrustedProxies are the networks whose X-Forwarded-For this service
	// believes. Every service needed this, so it belongs here rather than
	// being re-declared six times in six service configs.
	TrustedProxies []string `mapstructure:"trusted_proxies"`

	OTLPEndpoint  string  `mapstructure:"otlp_endpoint"`
	TraceSampling float64 `mapstructure:"trace_sampling"`

	ShutdownGrace  time.Duration `mapstructure:"shutdown_grace"`
	RequestTimeout time.Duration `mapstructure:"request_timeout"`

	// Timezone is the operational timezone for all business scheduling.
	// Sri Lanka does not observe DST, but we never hardcode a fixed offset:
	// tzdata is the only correct source over a 50-year horizon.
	Timezone string `mapstructure:"timezone"`
}

// Validate returns an error if a required key is missing. Fail fast at boot
// rather than at 3am when the first request arrives.
func (b Base) Validate() error {
	var missing []string
	if b.ServiceName == "" {
		missing = append(missing, "SERVICE_NAME")
	}
	if b.DatabaseURL == "" {
		missing = append(missing, "DATABASE_URL")
	}
	if b.HTTPPort == 0 {
		missing = append(missing, "HTTP_PORT")
	}
	if len(missing) > 0 {
		return fmt.Errorf("config: missing required keys: %s", strings.Join(missing, ", "))
	}
	if _, err := time.LoadLocation(b.Timezone); err != nil {
		return fmt.Errorf("config: invalid TIMEZONE %q: %w", b.Timezone, err)
	}
	return nil
}

// Location resolves the configured business timezone.
func (b Base) Location() (*time.Location, error) { return time.LoadLocation(b.Timezone) }

// IsProd reports whether the service runs in a production-like environment.
func (b Base) IsProd() bool {
	// Case- and spelling-tolerant on purpose. Every caller uses this to decide
	// whether to REFUSE something -- an unauthenticated debug provider, a
	// missing IP allowlist -- so being wrong must fail closed. ENV=Production
	// or ENV=live previously read as "not production" and quietly re-enabled
	// exactly those things.
	switch strings.ToLower(strings.TrimSpace(b.Env)) {
	case "prod", "production", "live":
		return true
	}
	return false
}

// JWKSURLs returns every key set this service should trust, skipping any that
// are unconfigured. A service that only ever sees admin traffic can leave
// USER_JWKS_URL empty, and vice versa.
func (b Base) JWKSURLs() []string {
	var out []string
	for _, u := range []string{b.UserJWKSURL, b.KeycloakJWKSURL} {
		if u != "" {
			out = append(out, u)
		}
	}
	return out
}

// IssuerKeys binds each configured issuer to the ONE key set allowed to sign
// for it, ready to hand to middleware.AuthConfig. Prefer this over
// JWKSURLs()+TrustedIssuers(): merging both key sets and then checking the
// "iss" claim lets either issuer sign for the other, because the merged set
// matches on "kid" alone and "iss" is chosen by whoever signs.
func (b Base) IssuerKeys() map[string]string {
	out := make(map[string]string, 2)
	if b.UserIssuer != "" && b.UserJWKSURL != "" {
		out[b.UserIssuer] = b.UserJWKSURL
	}
	if b.KeycloakIssuer != "" && b.KeycloakJWKSURL != "" {
		out[b.KeycloakIssuer] = b.KeycloakJWKSURL
	}
	return out
}

// TrustedIssuers returns the allowlist of acceptable "iss" claims.
func (b Base) TrustedIssuers() []string {
	var out []string
	for _, i := range []string{b.UserIssuer, b.KeycloakIssuer} {
		if i != "" {
			out = append(out, i)
		}
	}
	return out
}

// New returns a viper instance preloaded with the shared defaults and bound to
// the environment. Services call this, register their own defaults, then
// Unmarshal into their Config struct.
func New(serviceName string) *viper.Viper {
	v := viper.New()
	v.SetConfigFile(".env")
	v.SetConfigType("env")
	_ = v.ReadInConfig() // .env is optional; env vars win in every deployment

	v.AutomaticEnv()
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))

	v.SetDefault("service_name", serviceName)
	v.SetDefault("env", "dev")
	v.SetDefault("http_port", 8080)
	v.SetDefault("grpc_port", 9090)
	v.SetDefault("log_level", "info")

	v.SetDefault("database_max_conns", 20)
	v.SetDefault("database_min_conns", 2)
	v.SetDefault("database_max_conn_life", time.Hour)

	v.SetDefault("redis_url", "redis://localhost:6379")
	v.SetDefault("redis_db", 0)

	v.SetDefault("nats_url", "nats://localhost:4222")
	v.SetDefault("nats_stream", "TELEMED")
	// -1 means "bounded by the NATS server's own max_storage". A fixed byte
	// ceiling larger than the server's store makes stream creation fail hard.
	v.SetDefault("nats_max_bytes", -1)
	v.SetDefault("outbox_poll_seconds", 2)
	v.SetDefault("outbox_batch_size", 100)

	v.SetDefault("trace_sampling", 0.1)
	v.SetDefault("shutdown_grace", 20*time.Second)
	v.SetDefault("request_timeout", 30*time.Second)
	v.SetDefault("timezone", "Asia/Colombo")

	// viper's AutomaticEnv does not discover keys that only exist in the
	// environment, so every key we intend to read must be bound explicitly.
	for _, k := range []string{
		"service_name", "env", "http_port", "grpc_port", "log_level",
		"database_url", "database_max_conns", "database_min_conns", "database_max_conn_life",
		"redis_url", "redis_password", "redis_db",
		"nats_url", "nats_stream", "nats_credentials", "nats_max_bytes",
		"outbox_poll_seconds", "outbox_batch_size",
		"keycloak_issuer", "keycloak_audience", "keycloak_jwks_url",
		"user_issuer", "user_jwks_url", "trusted_proxies",
		"otlp_endpoint", "trace_sampling", "shutdown_grace", "request_timeout", "timezone",
	} {
		_ = v.BindEnv(k)
	}
	return v
}
