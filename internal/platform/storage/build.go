package storage

import (
	"context"
	"fmt"
	"strings"
)

// BuildOptions selects and configures a Storage implementation.
type BuildOptions struct {
	// Backend is "minio" or "filesystem". Empty defaults to "minio", which is
	// what every existing deployment set.
	Backend string

	FilesystemDir string
	// FilesystemBaseURL is the absolute, publicly reachable address of a
	// mounted PresignHandler. See NewFilesystem.
	FilesystemBaseURL string
	FilesystemSecret  []byte

	MinIOEndpoint  string
	MinIOAccessKey string
	MinIOSecretKey string
	MinIOSecure    bool
	MinIORegion    string
	// EnsureBuckets, when non-empty, is created on a MinIO backend at boot.
	// Ignored by the filesystem backend, which makes directories on demand.
	EnsureBuckets []string
}

// Build returns the configured Storage and a health probe for it.
//
// It exists because two domains -- record and admin -- both need object
// storage, and only one of them used to honour STORAGE_BACKEND. The other
// called storage.New unconditionally, so a deployment that set
// STORAGE_BACKEND=filesystem got a MinIO client built against an empty
// endpoint and a process that failed to boot with an error naming neither
// setting.
func Build(ctx context.Context, o BuildOptions) (Storage, func(context.Context) error, error) {
	switch strings.TrimSpace(o.Backend) {
	case "filesystem":
		fs, err := NewFilesystem(o.FilesystemDir, o.FilesystemBaseURL, o.FilesystemSecret)
		if err != nil {
			return nil, nil, err
		}
		return fs, fs.Ping, nil

	case "", "minio":
		m, err := New(Config{
			Endpoint:  o.MinIOEndpoint,
			AccessKey: o.MinIOAccessKey,
			SecretKey: o.MinIOSecretKey,
			Secure:    o.MinIOSecure,
			Region:    o.MinIORegion,
		})
		if err != nil {
			return nil, nil, err
		}
		if len(o.EnsureBuckets) > 0 {
			if err := m.EnsureBuckets(ctx, o.EnsureBuckets, o.MinIORegion); err != nil {
				return nil, nil, fmt.Errorf("ensure buckets: %w", err)
			}
		}
		return m, m.Ping, nil

	default:
		return nil, nil, fmt.Errorf("storage: unknown STORAGE_BACKEND %q, want \"minio\" or \"filesystem\"", o.Backend)
	}
}

// BucketProbe returns a health check asserting a specific bucket is reachable.
//
// Only MinIO can answer that meaningfully: on the filesystem backend a bucket
// is a directory created on first write, so asserting it exists before
// anything has been written would fail a healthy service. There the generic
// Ping is the honest check, and the difference is stated here rather than left
// as a nil-check at the call site.
func BucketProbe(s Storage, bucket string) func(context.Context) error {
	if m, ok := s.(*MinIOStorage); ok && bucket != "" {
		return func(ctx context.Context) error { return m.PingBucket(ctx, bucket) }
	}
	if p, ok := s.(interface {
		Ping(context.Context) error
	}); ok {
		return p.Ping
	}
	return func(context.Context) error { return nil }
}
