// Command server is the entrypoint for telemed-record-service: medical
// document storage, e-prescriptions with QR verification, clinical (SOAP)
// notes with their append-only amendment trail, and the authorization layer
// that gates all three.
//
// The boot sequence is intentionally rigid and identical across services:
//
//	config -> logger -> metrics -> tracing -> postgres -> redis -> nats
//	  -> repositories -> services -> handlers -> routes -> background workers
//	  -> listen -> drain
//
// An operator debugging service #7 at 3am should recognise the shape of
// service #2 immediately.
package record

import (
	"context"
	"fmt"

	"github.com/rs/zerolog"

	"telemed/internal/platform/middleware"
	"telemed/internal/platform/storage"
)

// Version is stamped at build time via -ldflags "-X main.Version=$(git describe)".
var Version = "dev"

const serviceName = "telemed-record-service"

// buildStorage constructs the Storage implementation selected by
// STORAGE_BACKEND and, for MinIO, provisions the bucket topology
// (AGENT-BRIEF: private, SSE-S3 AES-256, versioning on) before the server
// starts accepting traffic. It also returns a health-check probe when the
// backend supports one.
func buildStorage(ctx context.Context, cfg Config) (storage.Storage, func(context.Context) error, error) {
	switch cfg.StorageBackend {
	case "filesystem":
		fsStore, err := storage.NewFilesystem(cfg.FilesystemStorageDir, "")
		if err != nil {
			return nil, nil, err
		}
		return fsStore, fsStore.Ping, nil
	default: // "minio", enforced by Config.Validate
		minioStore, err := storage.New(storage.Config{
			Endpoint: cfg.MinIOEndpoint, AccessKey: cfg.MinIOAccessKey, SecretKey: cfg.MinIOSecretKey,
			Secure: cfg.MinIOSecure, Region: cfg.MinIORegion,
		})
		if err != nil {
			return nil, nil, err
		}
		if err := minioStore.EnsureBuckets(ctx, storage.AllBuckets, cfg.MinIORegion); err != nil {
			return nil, nil, fmt.Errorf("ensure buckets: %w", err)
		}
		return minioStore, minioStore.Ping, nil
	}
}

// buildTrustedProxies parses TRUSTED_PROXIES. Returning nil hands
// server.New the platform default (private ranges only), which is the right
// answer for a docker-compose or single-ingress deployment. A malformed entry
// is logged and skipped rather than fatal -- one typo in a config map must not
// stop a pod starting -- but it is never treated as a wildcard.
func buildTrustedProxies(cidrs []string, log zerolog.Logger) *middleware.TrustedProxies {
	if len(cidrs) == 0 {
		return nil
	}
	tp, malformed := middleware.NewTrustedProxies(cidrs)
	for _, m := range malformed {
		log.Error().Str("cidr", m).Msg("ignoring malformed TRUSTED_PROXIES entry")
	}
	return tp
}
