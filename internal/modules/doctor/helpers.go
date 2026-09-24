// Command server is the entrypoint template every telemed Go service follows.
//
// The boot sequence is intentionally rigid and identical across services:
//
//	config -> logger -> metrics -> tracing -> postgres -> redis -> nats
//	  -> repositories -> services -> handlers -> routes -> background workers
//	  -> listen -> drain
//
// An operator debugging service #7 at 3am should recognise the shape of
// service #2 immediately.
//
// telemed-doctor-service additionally runs a gRPC listener (doctor.v1, for
// other services to resolve a doctor without joining across the database
// boundary) and two NATS consumers: one that folds slot.generated /
// slot.booked / slot.released into the local availability projection search
// depends on, and one that reacts to appointment.completed. See
// docs/DESIGN.md for why both exist.
package doctor

import (
	"strings"
	"time"

	"telemed/internal/platform/config"
	"telemed/internal/platform/middleware"
)

// Version is stamped at build time via -ldflags "-X main.Version=$(git describe)".
var Version = "dev"

const serviceName = "telemed-doctor-service"

// serviceConfig extends the shared config.Base with the keys this service
// alone needs. Embedding rather than duplicating fields is what keeps every
// service's HTTP port, database URL, etc. bound the same way.
type serviceConfig struct {
	config.Base `mapstructure:",squash"`

	// BankEncryptionKey is a base64-encoded 32-byte key for the
	// doctor.Encryptor that seals bank payout details before they touch the
	// database. See docs/RUNBOOK.md for rotation.
	BankEncryptionKey string `mapstructure:"bank_encryption_key"`

	// SearchCacheTTL bounds how long a doctor-search result page is served
	// from Redis before the query is re-run.
	SearchCacheTTL time.Duration `mapstructure:"search_cache_ttl"`

	// TrustedProxies is a comma-separated CIDR list whose X-Forwarded-For this
	// service honours. Empty means the private ranges only, never the public
	// internet -- see middleware.DefaultTrustedProxies.
	TrustedProxies string `mapstructure:"trusted_proxies"`

	// SchedulingBaseURL is where scheduling-service listens, e.g.
	// http://scheduling:8083. It is used for exactly one thing: forwarding the
	// `holidays` array on PUT /doctors/me/availability to the service that owns
	// the holidays table (ADR-004 -- doctor-service must not write it).
	//
	// Empty disables forwarding. An availability save carrying leave is then
	// refused with 501 rather than accepted and dropped, because a doctor told
	// their leave saved while patients keep booking them is the failure this
	// whole path exists to prevent. Saves with no leave -- the common case --
	// are unaffected.
	SchedulingBaseURL string `mapstructure:"scheduling_base_url"`

	// SchedulingTimeout bounds the holiday forward. It sits on a doctor's Save
	// button, so it is short by design.
	SchedulingTimeout time.Duration `mapstructure:"scheduling_timeout"`

	// Object storage for signature and seal images, read from the same keys
	// the record domain uses: record-service embeds these images in issued
	// prescriptions, so both must resolve the same doctor-credentials bucket.
	StorageBackend          string `mapstructure:"storage_backend"`
	FilesystemStorageDir    string `mapstructure:"filesystem_storage_dir"`
	FilesystemPresignSecret string `mapstructure:"filesystem_presign_secret"`
	PublicAPIBaseURL        string `mapstructure:"public_api_base_url"`
	MinIOEndpoint           string `mapstructure:"minio_endpoint"`
	MinIOAccessKey          string `mapstructure:"minio_access_key"`
	MinIOSecretKey          string `mapstructure:"minio_secret_key"`
	MinIOSecure             bool   `mapstructure:"minio_secure"`
	MinIORegion             string `mapstructure:"minio_region"`
}

// ProxyCIDRs splits the trusted-proxy list, dropping empty entries so that a
// trailing comma is not read as a network.
func (c serviceConfig) ProxyCIDRs() []string {
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

// internalRoles are the roles permitted on the internal admin-service-facing
// surface: every human admin role plus the machine-to-machine "service" role
// admin-service itself authenticates as.
var internalRoles = append([]middleware.Role{middleware.RoleService}, middleware.AdminRoles...)
