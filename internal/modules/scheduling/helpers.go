// Command server is the telemed-scheduling-service entrypoint.
//
// The boot sequence is the platform's, unchanged:
//
//	config -> logger -> metrics -> tracing -> postgres -> redis -> nats
//	  -> repositories -> services -> handlers -> routes -> background workers
//	  -> listen -> drain
//
// An operator debugging service #7 at 3am should recognise the shape of
// service #2 immediately.
package scheduling

import (
	"strings"

	"telemed/internal/platform/config"
)

// Version is stamped at build time via -ldflags "-X main.Version=$(git describe)".
var Version = "dev"

// BuildTime is stamped at build time.
var BuildTime = "unknown"

const serviceName = "telemed-scheduling-service"

// Config is the platform base plus this service's own knobs.
type Config struct {
	config.Base `mapstructure:",squash"`

	// SlotLockEnabled toggles the Redis early-reject lock. Correctness does not
	// depend on it (ADR-007) and the concurrency test proves that, so an
	// operator can switch it off during a Redis incident without stopping
	// bookings.
	SlotLockEnabled bool `mapstructure:"slot_lock_enabled"`

	// SchedulerEnabled toggles the cron jobs. Off in a replica dedicated to
	// serving traffic, or when running migrations in a maintenance pod.
	SchedulerEnabled bool `mapstructure:"scheduler_enabled"`

	// ConsumersEnabled toggles the JetStream subscriptions.
	ConsumersEnabled bool `mapstructure:"consumers_enabled"`

	// AdminIPAllowlist restricts the /api/v1/admin tree, in addition to the
	// role check. Defence at the edge that disappears the moment somebody
	// reaches the origin is not defence.
	AdminIPAllowlist []string `mapstructure:"admin_ip_allowlist"`

	CORSOrigins []string `mapstructure:"cors_origins"`

	// TrustedProxies is a comma-separated CIDR list whose X-Forwarded-For this
	// service honours. Empty means the private ranges only, never the public
	// internet -- see middleware.DefaultTrustedProxies.
	TrustedProxies string `mapstructure:"trusted_proxies"`
}

// ProxyCIDRs splits the trusted-proxy list, dropping empty entries so that a
// trailing comma is not read as a network.
func (c Config) ProxyCIDRs() []string {
	raw := strings.TrimSpace(c.TrustedProxies)
	if raw == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// reflectionAllowed reports whether gRPC server reflection may be registered.
//
// It is an ALLOWLIST of development environments, not a denylist of production
// ones, and that direction is the whole point. A denylist keyed on IsProd()
// leaves reflection on in staging, in any environment somebody names
// "prod-eu-west", and in a pod whose ENV was never set -- which is exactly the
// shape of F17, where an unset variable silently turned a documented security
// control into decoration. An unrecognised environment is treated as
// production, because the cost of being wrong that way is one operator running
// grpcurl with a .proto file instead of without one.
func reflectionAllowed(env string) bool {
	switch strings.ToLower(strings.TrimSpace(env)) {
	case "dev", "development", "local", "test":
		return true
	default:
		return false
	}
}
