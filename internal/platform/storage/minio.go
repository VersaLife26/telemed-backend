package storage

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/minio/minio-go/v7/pkg/sse"
)

// MinIOStorage is the production Storage implementation. It talks to a
// stock MinIO (or any S3-compatible) server over the S3 protocol via
// minio-go, which is Apache-2.0 licensed and safe to link regardless of the
// AGPLv3 licence on the MinIO server binary itself (DECISIONS.md ADR-003).
type MinIOStorage struct {
	client *minio.Client
	region string
}

var _ Storage = (*MinIOStorage)(nil)

// Config configures the MinIO client.
type Config struct {
	Endpoint  string
	AccessKey string
	SecretKey string
	Secure    bool
	Region    string
}

// New dials MinIO and returns a client. It does not verify connectivity --
// callers should follow up with EnsureBuckets, which both proves the
// connection works and provisions the bucket topology idempotently.
func New(cfg Config) (*MinIOStorage, error) {
	client, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: cfg.Secure,
		Region: cfg.Region,
	})
	if err != nil {
		return nil, fmt.Errorf("storage: create minio client: %w", err)
	}
	return &MinIOStorage{client: client, region: cfg.Region}, nil
}

// EnsureBuckets creates any missing bucket from the given list and applies
// the platform's baseline: private (no public policy is ever set), SSE-S3
// AES-256 default encryption, and versioning enabled.
func (m *MinIOStorage) EnsureBuckets(ctx context.Context, buckets []string, region string) error {
	for _, bucket := range buckets {
		exists, err := m.client.BucketExists(ctx, bucket)
		if err != nil {
			return fmt.Errorf("storage: check bucket %s: %w", bucket, err)
		}
		if !exists {
			if err := m.client.MakeBucket(ctx, bucket, minio.MakeBucketOptions{Region: region}); err != nil {
				return fmt.Errorf("storage: create bucket %s: %w", bucket, err)
			}
		}

		if err := m.client.SetBucketVersioning(ctx, bucket, minio.BucketVersioningConfiguration{Status: "Enabled"}); err != nil {
			return fmt.Errorf("storage: enable versioning on %s: %w", bucket, err)
		}

		if err := m.client.SetBucketEncryption(ctx, bucket, sse.NewConfigurationSSES3()); err != nil {
			return fmt.Errorf("storage: enable sse-s3 on %s: %w", bucket, err)
		}
	}
	return nil
}

func (m *MinIOStorage) Put(ctx context.Context, bucket, key string, r io.Reader, size int64, contentType string) error {
	// No per-object ServerSideEncryption is set here: SSE-S3 is applied by
	// the bucket's default encryption configuration (see EnsureBuckets), and
	// the ServerSideEncryption option on PutObjectOptions is only for
	// client-driven SSE-C/SSE-KMS, which this platform does not use.
	opts := minio.PutObjectOptions{ContentType: contentType}
	_, err := m.client.PutObject(ctx, bucket, key, r, size, opts)
	if err != nil && isNoSuchBucket(err) {
		if seeker, ok := r.(io.Seeker); ok {
			if _, seekErr := seeker.Seek(0, io.SeekStart); seekErr == nil {
				if ensureErr := m.EnsureBuckets(ctx, []string{bucket}, m.region); ensureErr == nil {
					_, err = m.client.PutObject(ctx, bucket, key, r, size, opts)
				}
			}
		}
	}
	if err != nil {
		return fmt.Errorf("storage: put %s/%s: %w", bucket, key, err)
	}
	return nil
}

func isNoSuchBucket(err error) bool {
	return minio.ToErrorResponse(err).Code == "NoSuchBucket"
}

func (m *MinIOStorage) Get(ctx context.Context, bucket, key string) (io.ReadCloser, error) {
	obj, err := m.client.GetObject(ctx, bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("storage: get %s/%s: %w", bucket, key, err)
	}
	// GetObject is lazy: the round trip that can 404 happens on first Stat/
	// Read, so probe it now rather than handing the caller a reader that
	// fails invisibly later.
	if _, err := obj.Stat(); err != nil {
		_ = obj.Close()
		if isNotFound(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("storage: stat %s/%s: %w", bucket, key, err)
	}
	return obj, nil
}

func (m *MinIOStorage) PresignedGet(ctx context.Context, bucket, key string, ttl time.Duration) (string, error) {
	u, err := m.client.PresignedGetObject(ctx, bucket, key, ttl, url.Values{})
	if err != nil {
		return "", fmt.Errorf("storage: presign get %s/%s: %w", bucket, key, err)
	}
	return u.String(), nil
}

func (m *MinIOStorage) PresignedPut(ctx context.Context, bucket, key string, ttl time.Duration) (string, error) {
	u, err := m.client.PresignedPutObject(ctx, bucket, key, ttl)
	if err != nil {
		return "", fmt.Errorf("storage: presign put %s/%s: %w", bucket, key, err)
	}
	return u.String(), nil
}

func (m *MinIOStorage) Delete(ctx context.Context, bucket, key string) error {
	if err := m.client.RemoveObject(ctx, bucket, key, minio.RemoveObjectOptions{}); err != nil {
		return fmt.Errorf("storage: delete %s/%s: %w", bucket, key, err)
	}
	return nil
}

func (m *MinIOStorage) Stat(ctx context.Context, bucket, key string) (ObjectInfo, error) {
	info, err := m.client.StatObject(ctx, bucket, key, minio.StatObjectOptions{})
	if err != nil {
		if isNotFound(err) {
			return ObjectInfo{}, ErrNotFound
		}
		return ObjectInfo{}, fmt.Errorf("storage: stat %s/%s: %w", bucket, key, err)
	}
	return ObjectInfo{
		Bucket:       bucket,
		Key:          key,
		Size:         info.Size,
		ContentType:  info.ContentType,
		ETag:         info.ETag,
		LastModified: info.LastModified,
	}, nil
}

// Ping verifies connectivity for the health-check chain by listing buckets,
// the cheapest call the S3 API offers with no required parameters.
func (m *MinIOStorage) Ping(ctx context.Context) error {
	_, err := m.client.ListBuckets(ctx)
	if err != nil {
		return fmt.Errorf("storage: ping: %w", err)
	}
	return nil
}

// PingBucket verifies connectivity AND that a specific bucket exists.
//
// Stronger than Ping, and deliberately kept alongside it: a readiness probe
// that only proves MinIO answers will report healthy on a node where the
// bucket the service actually writes to was never provisioned, and the first
// real upload is then the thing that discovers it. admin-service's readiness
// chain uses this against the doctor-documents bucket for exactly that reason.
func (m *MinIOStorage) PingBucket(ctx context.Context, bucket string) error {
	ok, err := m.client.BucketExists(ctx, bucket)
	if err != nil {
		return fmt.Errorf("storage: check bucket %s: %w", bucket, err)
	}
	if !ok {
		return fmt.Errorf("storage: bucket %s does not exist", bucket)
	}
	return nil
}

func isNotFound(err error) bool {
	resp := minio.ToErrorResponse(err)
	return resp.StatusCode == http.StatusNotFound || resp.Code == "NoSuchKey" || resp.Code == "NoSuchBucket"
}
