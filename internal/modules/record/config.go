package record

import (
	"fmt"
	"time"

	"github.com/spf13/viper"

	"telemed/internal/platform/config"
)

// Config extends the shared config.Base with everything the record service
// needs beyond the platform defaults: object storage, virus scanning, FHIR,
// and the prescription verification scheme.
type Config struct {
	config.Base `mapstructure:",squash"`

	// StorageBackend selects the Storage implementation. "minio" (default)
	// talks to a real MinIO/S3-compatible server; "filesystem" writes to
	// local disk and needs no MinIO -- useful for a developer machine with
	// no Docker running, and what the test suite uses.
	StorageBackend       string `mapstructure:"storage_backend"`
	FilesystemStorageDir string `mapstructure:"filesystem_storage_dir"`
	MinIOEndpoint        string `mapstructure:"minio_endpoint"`
	MinIOAccessKey       string `mapstructure:"minio_access_key"`
	MinIOSecretKey       string `mapstructure:"minio_secret_key"`
	MinIOSecure          bool   `mapstructure:"minio_secure"`
	MinIORegion          string `mapstructure:"minio_region"`

	// ClamAVAddr, when set, enables real virus scanning against a clamd
	// sidecar at host:port. Left empty, uploads pass through unscanned
	// (scan_status "skipped") rather than the service failing to boot --
	// AGENT-BRIEF: "optional ClamAV scanning behind a VirusScanner
	// interface with a pass-through default."
	ClamAVAddr    string        `mapstructure:"clamav_addr"`
	ClamAVTimeout time.Duration `mapstructure:"clamav_timeout"`

	// FHIRBaseURL, when set, enables the Medplum REST client. Left empty,
	// fhir.NoOpClient is used and every FHIR write becomes a harmless local
	// no-op -- AGENT-BRIEF: "a no-op implementation (default, so nothing
	// breaks when no FHIR server runs)".
	FHIRBaseURL      string `mapstructure:"fhir_base_url"`
	FHIRClientID     string `mapstructure:"fhir_client_id"`
	FHIRClientSecret string `mapstructure:"fhir_client_secret"`

	// PrescriptionHMACSecret signs and verifies every prescription QR code.
	// It has no default: an empty secret would make every prescription
	// forgeable, so the service refuses to start without one (see Validate).
	PrescriptionHMACSecret string `mapstructure:"prescription_hmac_secret"`
	// VerifyBaseURL is the public prefix encoded into the QR code, e.g.
	// "https://verify.yourapp.lk".
	VerifyBaseURL string `mapstructure:"verify_base_url"`

	// VerifyRateLimitPerMinute bounds GET /api/v1/verify/prescriptions/{id},
	// the one unauthenticated endpoint backed by a database read.
	VerifyRateLimitPerMinute int `mapstructure:"verify_rate_limit_per_minute"`

	// TrustedProxies are the CIDRs whose X-Forwarded-For / X-Real-IP headers
	// this service believes. Requests from anywhere else have their client
	// address taken from the TCP peer, which no header can forge. Empty means
	// the private ranges only. This is a security control, not a convenience:
	// the public prescription-verification rate limiter buckets on the
	// resolved client IP, and every PHI access-log row records it.
	TrustedProxies []string `mapstructure:"trusted_proxies"`
}

// Validate extends config.Base.Validate with the record service's own
// required keys.
func (c Config) Validate() error {
	if err := c.Base.Validate(); err != nil {
		return err
	}
	if c.PrescriptionHMACSecret == "" {
		return fmt.Errorf("config: PRESCRIPTION_HMAC_SECRET is required (a prescription cannot be signed without one)")
	}
	if len(c.PrescriptionHMACSecret) < 32 {
		return fmt.Errorf("config: PRESCRIPTION_HMAC_SECRET must be at least 32 bytes")
	}
	if c.StorageBackend != "minio" && c.StorageBackend != "filesystem" {
		return fmt.Errorf("config: STORAGE_BACKEND must be \"minio\" or \"filesystem\", got %q", c.StorageBackend)
	}
	if c.StorageBackend == "minio" && c.MinIOEndpoint == "" {
		return fmt.Errorf("config: MINIO_ENDPOINT is required when STORAGE_BACKEND=minio")
	}
	return nil
}

// newViper returns a viper instance preloaded with the shared platform
// defaults plus this service's own.
func newViper(serviceName string) *viper.Viper {
	v := config.New(serviceName)
	v.SetDefault("storage_backend", "minio")
	v.SetDefault("filesystem_storage_dir", "./data/objects")
	v.SetDefault("minio_secure", false)
	v.SetDefault("minio_region", "us-east-1")
	v.SetDefault("clamav_timeout", 30*time.Second)
	v.SetDefault("verify_base_url", "https://verify.yourapp.lk")
	v.SetDefault("verify_rate_limit_per_minute", 20)

	for _, k := range []string{
		"storage_backend", "filesystem_storage_dir",
		"minio_endpoint", "minio_access_key", "minio_secret_key", "minio_secure", "minio_region",
		"clamav_addr", "clamav_timeout",
		"fhir_base_url", "fhir_client_id", "fhir_client_secret",
		"prescription_hmac_secret", "verify_base_url", "verify_rate_limit_per_minute",
		"trusted_proxies",
	} {
		_ = v.BindEnv(k)
	}
	return v
}
