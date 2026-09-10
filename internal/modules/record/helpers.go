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
	"strings"

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
// Where PresignHandler lives, spelled twice because the two spellings differ
// and getting that wrong is silent: Module.API is mounted at /api/v1, so the
// route registered inside it is relative, while the URL handed to a browser
// must be absolute. Declaring both here keeps them one edit apart.
const (
	// filesRoute is the path registered on the module's own router.
	filesRoute = "/files"
	// filesPublicPath is the same endpoint as seen from outside, and is the
	// suffix appended to PUBLIC_API_BASE_URL when building presigned URLs.
	filesPublicPath = "/api/v1" + filesRoute
)

func buildStorage(ctx context.Context, cfg Config) (storage.Storage, func(context.Context) error, error) {
	return storage.Build(ctx, storage.BuildOptions{
		Backend:           cfg.StorageBackend,
		FilesystemDir:     cfg.FilesystemStorageDir,
		FilesystemBaseURL: strings.TrimSuffix(cfg.PublicAPIBaseURL, "/") + filesPublicPath,
		FilesystemSecret:  []byte(cfg.FilesystemPresignSecret),
		MinIOEndpoint:     cfg.MinIOEndpoint,
		MinIOAccessKey:    cfg.MinIOAccessKey,
		MinIOSecretKey:    cfg.MinIOSecretKey,
		MinIOSecure:       cfg.MinIOSecure,
		MinIORegion:       cfg.MinIORegion,
		EnsureBuckets:     storage.AllBuckets,
	})
}
