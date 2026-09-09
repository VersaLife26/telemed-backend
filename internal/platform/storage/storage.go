// Package storage defines the object storage contract used by the record
// service and its two implementations: MinIO for real deployments and a
// filesystem-backed one for tests, so the suite runs with no MinIO server.
//
// Business logic never imports minio-go directly (AGENT-BRIEF §0.6). See
// DECISIONS.md ADR-003: MinIO's server is AGPLv3, but we run it as a separate
// network service and talk to it only via minio-go (Apache-2.0) over the S3
// protocol -- we neither link nor modify the AGPL-covered server binary.
package storage

import (
	"context"
	"errors"
	"io"
	"time"
)

// ErrNotFound is returned by Get and Stat when the object does not exist.
var ErrNotFound = errors.New("storage: object not found")

// ObjectInfo describes a stored object without its content.
type ObjectInfo struct {
	Bucket       string
	Key          string
	Size         int64
	ContentType  string
	ETag         string
	LastModified time.Time
}

// Storage is the contract every service dependency on object storage is
// written against. Swapping MinIO for SeaweedFS, Garage, or S3 itself is a
// new file implementing this interface plus a config change -- see the
// README's "50-Year Maintenance" section.
type Storage interface {
	// Put uploads size bytes read from r to bucket/key with the given
	// content type. It does not return until the upload is durable.
	Put(ctx context.Context, bucket, key string, r io.Reader, size int64, contentType string) error

	// Get opens the object for reading. The caller must close the returned
	// reader.
	Get(ctx context.Context, bucket, key string) (io.ReadCloser, error)

	// PresignedGet returns a time-boxed URL a client can use to download the
	// object directly, without proxying the bytes through this service.
	PresignedGet(ctx context.Context, bucket, key string, ttl time.Duration) (string, error)

	// PresignedPut returns a time-boxed URL a client can use to upload
	// directly to the bucket.
	PresignedPut(ctx context.Context, bucket, key string, ttl time.Duration) (string, error)

	// Delete removes the object. Deleting an object that does not exist is
	// not an error -- delete is idempotent by design.
	Delete(ctx context.Context, bucket, key string) error

	// Stat returns object metadata without downloading its content.
	Stat(ctx context.Context, bucket, key string) (ObjectInfo, error)
}

// Bucket names are fixed by AGENT-BRIEF: private, SSE-S3 AES-256, versioning
// on. A service boots by calling EnsureBuckets with this list.
const (
	BucketMedicalReports    = "medical-reports"
	BucketPrescriptions     = "prescriptions"
	BucketDoctorCredentials = "doctor-credentials"
	BucketRecordings        = "recordings"
)

// AllBuckets is every bucket this service owns.
var AllBuckets = []string{BucketMedicalReports, BucketPrescriptions, BucketDoctorCredentials, BucketRecordings}

// Default presigned URL lifetimes per AGENT-BRIEF: 15 minutes for records,
// 24 hours for prescriptions (a patient may not open the link the moment it
// is issued).
const (
	RecordPresignTTL       = 15 * time.Minute
	PrescriptionPresignTTL = 24 * time.Hour
)
