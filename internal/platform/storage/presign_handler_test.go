package storage

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// newServedStore returns a filesystem store whose presigned URLs are actually
// answered, which is the thing that was missing: the URLs existed, nothing
// served them, and every document link handed to a browser was dead.
func newServedStore(t *testing.T, secret []byte) (*FilesystemStorage, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(nil)
	fs, err := NewFilesystem(t.TempDir(), srv.URL, secret)
	if err != nil {
		t.Fatalf("NewFilesystem: %v", err)
	}
	srv.Config.Handler = PresignHandler(fs)
	t.Cleanup(srv.Close)
	return fs, srv
}

func TestPresignedGetRoundTripsOverHTTP(t *testing.T) {
	fs, _ := newServedStore(t, []byte("stable-secret"))
	ctx := context.Background()

	const body = "%PDF-1.4 pretend prescription"
	if err := fs.Put(ctx, "medical-reports", "a.pdf", strings.NewReader(body), int64(len(body)), "application/pdf"); err != nil {
		t.Fatalf("put: %v", err)
	}

	raw, err := fs.PresignedGet(ctx, "medical-reports", "a.pdf", 5*time.Minute)
	if err != nil {
		t.Fatalf("presign: %v", err)
	}

	resp, err := http.Get(raw) //nolint:noctx,gosec // test client against httptest
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %s, want 200", resp.Status)
	}
	got, _ := io.ReadAll(resp.Body)
	if string(got) != body {
		t.Errorf("body = %q, want %q", got, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/pdf" {
		t.Errorf("Content-Type = %q, want application/pdf", ct)
	}
	// Medical records behind a short-lived URL must not be cached by an
	// intermediary that would outlive the capability.
	if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	// Uploaded bytes are user-supplied; rendering them inline on the API's
	// own origin is stored XSS.
	if cd := resp.Header.Get("Content-Disposition"); !strings.Contains(cd, "attachment") {
		t.Errorf("Content-Disposition = %q, want attachment", cd)
	}
}

// TestPresignSurvivesRestartWithAStableSecret is the regression this whole
// change exists for. The secret used to be generated per process, so every URL
// issued before a restart stopped verifying after it -- and two processes
// sharing a volume could never verify each other's at all.
func TestPresignSurvivesRestartWithAStableSecret(t *testing.T) {
	secret := []byte("stable-secret")
	dir := t.TempDir()
	ctx := context.Background()

	first, err := NewFilesystem(dir, "", secret)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	const body = "report"
	if err := first.Put(ctx, "medical-reports", "a.pdf", strings.NewReader(body), int64(len(body)), "application/pdf"); err != nil {
		t.Fatalf("put: %v", err)
	}
	raw, err := first.PresignedGet(ctx, "medical-reports", "a.pdf", 5*time.Minute)
	if err != nil {
		t.Fatalf("presign: %v", err)
	}

	// A different process, same volume, same configured secret.
	second, err := NewFilesystem(dir, "", secret)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	u, _ := url.Parse(raw)
	if _, _, _, err := second.VerifyPresigned(u.RawQuery); err != nil {
		t.Fatalf("a URL minted by one process must verify in another: %v", err)
	}

	// And the bytes are still readable, with the content type recovered from
	// the extension rather than from the in-memory map the restart emptied.
	info, err := second.Stat(ctx, "medical-reports", "a.pdf")
	if err != nil {
		t.Fatalf("stat after restart: %v", err)
	}
	if info.Size != int64(len(body)) {
		t.Errorf("size = %d, want %d", info.Size, len(body))
	}
	if info.ContentType != "application/pdf" {
		t.Errorf("ContentType = %q, want application/pdf recovered from the extension", info.ContentType)
	}
}

func TestPresignedURLFromADifferentSecretIsRefused(t *testing.T) {
	_, srv := newServedStore(t, []byte("the-real-secret"))
	ctx := context.Background()

	forger, err := NewFilesystem(t.TempDir(), srv.URL, []byte("not-the-real-secret"))
	if err != nil {
		t.Fatalf("forger: %v", err)
	}
	raw, err := forger.PresignedGet(ctx, "medical-reports", "a.pdf", 5*time.Minute)
	if err != nil {
		t.Fatalf("presign: %v", err)
	}

	resp, err := http.Get(raw) //nolint:noctx,gosec // test client against httptest
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %s, want 403", resp.Status)
	}
}

// TestAGetSignatureCannotBeReplayedAsAPut is the reason the handler re-checks
// the method against the signed operation. The op is inside the HMAC, so a
// holder cannot edit it -- but VerifyPresigned only reports the op, it does
// not enforce it, and a handler that ignored the difference would let anyone
// holding a read link overwrite the object.
func TestAGetSignatureCannotBeReplayedAsAPut(t *testing.T) {
	fs, srv := newServedStore(t, []byte("stable-secret"))
	ctx := context.Background()

	const body = "original"
	if err := fs.Put(ctx, "medical-reports", "a.pdf", strings.NewReader(body), int64(len(body)), "application/pdf"); err != nil {
		t.Fatalf("put: %v", err)
	}
	raw, err := fs.PresignedGet(ctx, "medical-reports", "a.pdf", 5*time.Minute)
	if err != nil {
		t.Fatalf("presign: %v", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, raw, strings.NewReader("overwritten"))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %s, want 405", resp.Status)
	}

	rc, err := fs.Get(ctx, "medical-reports", "a.pdf")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = rc.Close() }()
	got, _ := io.ReadAll(rc)
	if string(got) != body {
		t.Errorf("object was overwritten through a GET signature: %q", got)
	}
}

func TestExpiredPresignedURLIsRefused(t *testing.T) {
	fs, _ := newServedStore(t, []byte("stable-secret"))
	ctx := context.Background()

	raw, err := fs.PresignedGet(ctx, "medical-reports", "a.pdf", -time.Minute)
	if err != nil {
		t.Fatalf("presign: %v", err)
	}
	resp, err := http.Get(raw) //nolint:noctx,gosec // test client against httptest
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %s, want 403", resp.Status)
	}
}

func TestPresignedPutStoresTheObject(t *testing.T) {
	fs, srv := newServedStore(t, []byte("stable-secret"))
	ctx := context.Background()

	raw, err := fs.PresignedPut(ctx, "doctor-credentials", "nic.jpg", 5*time.Minute)
	if err != nil {
		t.Fatalf("presign: %v", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, raw, strings.NewReader("scan-bytes"))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", "image/jpeg")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %s, want 204", resp.Status)
	}

	rc, err := fs.Get(ctx, "doctor-credentials", "nic.jpg")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = rc.Close() }()
	got, _ := io.ReadAll(rc)
	if string(got) != "scan-bytes" {
		t.Errorf("stored %q, want scan-bytes", got)
	}
}
