package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/url"
	"strings"
	"testing"
	"time"
)

func newTestFS(t *testing.T) *FilesystemStorage {
	t.Helper()
	fs, err := NewFilesystem(t.TempDir(), "http://localhost/__fs")
	if err != nil {
		t.Fatalf("NewFilesystem: %v", err)
	}
	return fs
}

func TestFilesystemStorage_PutGetRoundTrip(t *testing.T) {
	fs := newTestFS(t)
	ctx := context.Background()
	content := []byte("hello, medical record")

	if err := fs.Put(ctx, "medical-reports", "patient-1/doc.pdf", bytes.NewReader(content), int64(len(content)), "application/pdf"); err != nil {
		t.Fatalf("Put: %v", err)
	}

	r, err := fs.Get(ctx, "medical-reports", "patient-1/doc.pdf")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer r.Close()

	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("got %q, want %q", got, content)
	}
}

func TestFilesystemStorage_GetMissing(t *testing.T) {
	fs := newTestFS(t)
	_, err := fs.Get(context.Background(), "medical-reports", "does/not/exist.pdf")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestFilesystemStorage_StatMissing(t *testing.T) {
	fs := newTestFS(t)
	_, err := fs.Stat(context.Background(), "medical-reports", "does/not/exist.pdf")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestFilesystemStorage_StatAfterPut(t *testing.T) {
	fs := newTestFS(t)
	ctx := context.Background()
	content := []byte("some bytes")
	if err := fs.Put(ctx, "prescriptions", "rx-1.pdf", bytes.NewReader(content), int64(len(content)), "application/pdf"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	info, err := fs.Stat(ctx, "prescriptions", "rx-1.pdf")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Size != int64(len(content)) {
		t.Errorf("Size = %d, want %d", info.Size, len(content))
	}
	if info.ContentType != "application/pdf" {
		t.Errorf("ContentType = %q, want application/pdf", info.ContentType)
	}
}

func TestFilesystemStorage_Delete(t *testing.T) {
	fs := newTestFS(t)
	ctx := context.Background()
	content := []byte("x")
	if err := fs.Put(ctx, "medical-reports", "a.pdf", bytes.NewReader(content), int64(len(content)), "application/pdf"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := fs.Delete(ctx, "medical-reports", "a.pdf"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := fs.Get(ctx, "medical-reports", "a.pdf"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound after delete, got %v", err)
	}
}

func TestFilesystemStorage_RejectsPathTraversal(t *testing.T) {
	fs := newTestFS(t)
	ctx := context.Background()

	traversalKeys := []string{
		"../../../../etc/passwd",
		"../secret.txt",
		"a/../../b",
	}
	for _, key := range traversalKeys {
		t.Run(key, func(t *testing.T) {
			if err := fs.Put(ctx, "medical-reports", key, bytes.NewReader([]byte("x")), 1, "text/plain"); !errors.Is(err, errPathEscapesRoot) {
				t.Errorf("Put(%q) error = %v, want errPathEscapesRoot", key, err)
			}
			if _, err := fs.Get(ctx, "medical-reports", key); !errors.Is(err, errPathEscapesRoot) {
				t.Errorf("Get(%q) error = %v, want errPathEscapesRoot", key, err)
			}
			if _, err := fs.Stat(ctx, "medical-reports", key); !errors.Is(err, errPathEscapesRoot) {
				t.Errorf("Stat(%q) error = %v, want errPathEscapesRoot", key, err)
			}
			if err := fs.Delete(ctx, "medical-reports", key); !errors.Is(err, errPathEscapesRoot) {
				t.Errorf("Delete(%q) error = %v, want errPathEscapesRoot", key, err)
			}
		})
	}
}

func TestFilesystemStorage_DeleteMissingIsIdempotent(t *testing.T) {
	fs := newTestFS(t)
	if err := fs.Delete(context.Background(), "medical-reports", "never-existed.pdf"); err != nil {
		t.Fatalf("Delete of a missing object must not error, got %v", err)
	}
}

func TestFilesystemStorage_PresignedGet_ValidSignatureAllows(t *testing.T) {
	fs := newTestFS(t)
	raw, err := fs.PresignedGet(context.Background(), "medical-reports", "a.pdf", 15*time.Minute)
	if err != nil {
		t.Fatalf("PresignedGet: %v", err)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse presigned url: %v", err)
	}
	bucket, key, op, err := fs.VerifyPresigned(u.RawQuery)
	if err != nil {
		t.Fatalf("VerifyPresigned: %v", err)
	}
	if bucket != "medical-reports" || key != "a.pdf" || op != "GET" {
		t.Errorf("got (%s, %s, %s)", bucket, key, op)
	}
}

func TestFilesystemStorage_PresignedGet_ExpiredIsRejected(t *testing.T) {
	fs := newTestFS(t)
	raw, err := fs.PresignedGet(context.Background(), "medical-reports", "a.pdf", -1*time.Minute)
	if err != nil {
		t.Fatalf("PresignedGet: %v", err)
	}
	u, _ := url.Parse(raw)
	if _, _, _, err := fs.VerifyPresigned(u.RawQuery); err == nil {
		t.Fatal("expected an already-expired presigned url to be rejected")
	}
}

func TestFilesystemStorage_PresignedGet_TamperedSignatureIsRejected(t *testing.T) {
	fs := newTestFS(t)
	raw, err := fs.PresignedGet(context.Background(), "medical-reports", "a.pdf", 15*time.Minute)
	if err != nil {
		t.Fatalf("PresignedGet: %v", err)
	}
	tampered := strings.Replace(raw, "medical-reports", "doctor-credentials", 1)
	u, _ := url.Parse(tampered)
	if _, _, _, err := fs.VerifyPresigned(u.RawQuery); err == nil {
		t.Fatal("expected a bucket-swapped presigned url to fail signature verification")
	}
}

func TestFilesystemStorage_PresignedPut_TamperedOpIsRejected(t *testing.T) {
	fs := newTestFS(t)
	raw, err := fs.PresignedPut(context.Background(), "medical-reports", "a.pdf", 15*time.Minute)
	if err != nil {
		t.Fatalf("PresignedPut: %v", err)
	}
	tampered := strings.Replace(raw, "op=PUT", "op=GET", 1)
	u, _ := url.Parse(tampered)
	if _, _, _, err := fs.VerifyPresigned(u.RawQuery); err == nil {
		t.Fatal("expected a PUT url rewritten to GET to fail signature verification")
	}
}
