package storage

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// FilesystemStorage is a Storage implementation backed by the local disk. It
// exists so the test suite -- and any developer running the service without
// Docker -- never needs a real MinIO server. Its presigned URLs are not
// usable by an external HTTP client; they are addresses this same process
// resolves via Resolve, which is enough to unit test the upload/download flow
// end to end.
type FilesystemStorage struct {
	root     string
	secret   []byte
	mu       sync.RWMutex
	metadata map[string]ObjectInfo // key: bucket/objectKey
	baseURL  string
}

var _ Storage = (*FilesystemStorage)(nil)

// NewFilesystem creates a filesystem-backed store rooted at dir. baseURL, if
// set, prefixes generated presigned URLs (e.g. "http://localhost:8087/__fs");
// it is only meaningful when the test harness also mounts Resolve behind that
// prefix.
func NewFilesystem(dir, baseURL string) (*FilesystemStorage, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("storage: create root %s: %w", dir, err)
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, fmt.Errorf("storage: generate presign secret: %w", err)
	}
	return &FilesystemStorage{
		root:     dir,
		secret:   secret,
		metadata: map[string]ObjectInfo{},
		baseURL:  baseURL,
	}, nil
}

// errPathEscapesRoot is returned when bucket/key, once joined and cleaned,
// would resolve outside the storage root -- a "../../etc/passwd"-style key.
// Every application caller in this service constructs keys itself from safe
// components (a UUID, an allowlisted extension), so this should never
// trigger in practice; it exists because FilesystemStorage is a general
// Storage implementation and a defensive check here costs nothing.
var errPathEscapesRoot = errors.New("storage: bucket/key escapes the storage root")

// path joins bucket and key under the storage root and verifies the result
// stays within that bucket's own subdirectory. It checks containment against
// the bucket directory specifically, not just the overall root: a key of
// "../secret.txt" joined under "medical-reports" would still land inside
// f.root (just in the wrong bucket, or at the root itself), which a
// root-only containment check would miss entirely.
func (f *FilesystemStorage) path(bucket, key string) (string, error) {
	bucketRoot := filepath.Join(f.root, filepath.FromSlash(bucket))
	rootClean := filepath.Clean(f.root)
	if bucketRoot != rootClean && !strings.HasPrefix(bucketRoot, rootClean+string(filepath.Separator)) {
		return "", errPathEscapesRoot
	}

	joined := filepath.Join(bucketRoot, filepath.FromSlash(key))
	if joined != bucketRoot && !strings.HasPrefix(joined, bucketRoot+string(filepath.Separator)) {
		return "", errPathEscapesRoot
	}
	return joined, nil
}

func (f *FilesystemStorage) metaKey(bucket, key string) string { return bucket + "/" + key }

func (f *FilesystemStorage) Put(_ context.Context, bucket, key string, r io.Reader, size int64, contentType string) error {
	p, err := f.path(bucket, key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		return fmt.Errorf("storage: mkdir: %w", err)
	}
	out, err := os.OpenFile(p, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600) //nolint:gosec // p is containment-checked by path() above, never a raw client-controlled path
	if err != nil {
		return fmt.Errorf("storage: create %s/%s: %w", bucket, key, err)
	}
	defer func() { _ = out.Close() }()

	n, err := io.Copy(out, r)
	if err != nil {
		return fmt.Errorf("storage: write %s/%s: %w", bucket, key, err)
	}
	if size >= 0 && n != size {
		return fmt.Errorf("storage: short write %s/%s: wrote %d wanted %d", bucket, key, n, size)
	}

	f.mu.Lock()
	f.metadata[f.metaKey(bucket, key)] = ObjectInfo{
		Bucket: bucket, Key: key, Size: n, ContentType: contentType,
		ETag:         fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%s/%s/%d", bucket, key, n)))),
		LastModified: time.Now().UTC(),
	}
	f.mu.Unlock()
	return nil
}

func (f *FilesystemStorage) Get(_ context.Context, bucket, key string) (io.ReadCloser, error) {
	p, err := f.path(bucket, key)
	if err != nil {
		return nil, err
	}
	file, err := os.Open(p) //nolint:gosec // p is containment-checked by path() above, never a raw client-controlled path
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("storage: open %s/%s: %w", bucket, key, err)
	}
	return file, nil
}

func (f *FilesystemStorage) Delete(_ context.Context, bucket, key string) error {
	p, err := f.path(bucket, key)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("storage: delete %s/%s: %w", bucket, key, err)
	}
	f.mu.Lock()
	delete(f.metadata, f.metaKey(bucket, key))
	f.mu.Unlock()
	return nil
}

func (f *FilesystemStorage) Stat(_ context.Context, bucket, key string) (ObjectInfo, error) {
	p, err := f.path(bucket, key)
	if err != nil {
		return ObjectInfo{}, err
	}
	fi, err := os.Stat(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ObjectInfo{}, ErrNotFound
		}
		return ObjectInfo{}, fmt.Errorf("storage: stat %s/%s: %w", bucket, key, err)
	}
	f.mu.RLock()
	info, ok := f.metadata[f.metaKey(bucket, key)]
	f.mu.RUnlock()
	if !ok {
		info = ObjectInfo{Bucket: bucket, Key: key, Size: fi.Size(), LastModified: fi.ModTime()}
	}
	return info, nil
}

// sign produces an HMAC over bucket, key, operation and expiry so the token
// embedded in the presigned URL cannot be forged or extended by the holder --
// the same defensive posture a real S3 presigned URL has, scaled down to what
// a filesystem fake needs to demonstrate correctly.
func (f *FilesystemStorage) sign(bucket, key, op string, expires time.Time) string {
	mac := hmac.New(sha256.New, f.secret)
	// hash.Hash.Write (which Fprintf calls into) is documented to never
	// return an error; the write result is discarded deliberately, not
	// overlooked.
	_, _ = fmt.Fprintf(mac, "%s|%s|%s|%d", bucket, key, op, expires.Unix())
	return hex.EncodeToString(mac.Sum(nil))
}

func (f *FilesystemStorage) presign(bucket, key, op string, ttl time.Duration) string {
	expires := time.Now().Add(ttl)
	sig := f.sign(bucket, key, op, expires)
	v := url.Values{}
	v.Set("bucket", bucket)
	v.Set("key", key)
	v.Set("op", op)
	v.Set("expires", fmt.Sprintf("%d", expires.Unix()))
	v.Set("sig", sig)
	return f.baseURL + "?" + v.Encode()
}

func (f *FilesystemStorage) PresignedGet(_ context.Context, bucket, key string, ttl time.Duration) (string, error) {
	return f.presign(bucket, key, "GET", ttl), nil
}

func (f *FilesystemStorage) PresignedPut(_ context.Context, bucket, key string, ttl time.Duration) (string, error) {
	return f.presign(bucket, key, "PUT", ttl), nil
}

// VerifyPresigned checks a URL produced by PresignedGet/PresignedPut. It is
// exported for tests and for a local-dev handler that wants to actually serve
// the bytes at baseURL.
func (f *FilesystemStorage) VerifyPresigned(rawQuery string) (bucket, key, op string, err error) {
	v, err := url.ParseQuery(rawQuery)
	if err != nil {
		return "", "", "", fmt.Errorf("storage: parse presigned query: %w", err)
	}
	bucket, key, op = v.Get("bucket"), v.Get("key"), v.Get("op")
	var expiresUnix int64
	if _, err := fmt.Sscanf(v.Get("expires"), "%d", &expiresUnix); err != nil {
		return "", "", "", errors.New("storage: malformed expiry")
	}
	expires := time.Unix(expiresUnix, 0)
	if time.Now().After(expires) {
		return "", "", "", errors.New("storage: presigned url expired")
	}
	want := f.sign(bucket, key, op, expires)
	if !hmac.Equal([]byte(want), []byte(v.Get("sig"))) {
		return "", "", "", errors.New("storage: presigned url signature invalid")
	}
	return bucket, key, op, nil
}

// Ping always succeeds: the filesystem is local, so there is nothing to
// health-check beyond what the OS already guarantees.
func (f *FilesystemStorage) Ping(context.Context) error { return nil }
