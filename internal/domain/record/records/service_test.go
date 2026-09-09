package records

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/rs/zerolog"

	"telemed/internal/domain/record/access"
	"telemed/internal/domain/record/fhir"
	"telemed/internal/platform/events"
	"telemed/internal/platform/middleware"
	"telemed/internal/platform/scan"
	"telemed/internal/platform/storage"
)

// fakeInfectedScanner always reports an infection, letting the rejection
// path be tested without a real ClamAV daemon.
type fakeInfectedScanner struct{}

func (fakeInfectedScanner) Scan(_ context.Context, r io.Reader, _ int64) (scan.Verdict, error) {
	_, _ = io.Copy(io.Discard, r)
	return scan.VerdictInfected, scan.ErrInfected
}

// newTestService builds a Service whose only live dependency is a
// filesystem-backed Storage; the database pool is nil. This is safe for
// every Upload() rejection case, because every rejection happens before the
// service reaches its database transaction (see the ordering comment on
// Service.Upload). The one thing it cannot exercise is a *successful*
// upload, which needs the outbox/InTx path against a real Postgres --
// that is covered by the //go:build integration suite instead.
func newTestService(t *testing.T, scanner scan.VirusScanner) (*Service, middleware.Principal) {
	t.Helper()
	store, err := storage.NewFilesystem(t.TempDir(), "")
	if err != nil {
		t.Fatalf("NewFilesystem: %v", err)
	}
	log := zerolog.Nop()
	accessSvc := access.NewService(access.NewRepository(), nil, log)
	svc := NewService(NewRepository(), nil, store, scanner, fhir.NewNoOp(log), events.NewOutbox("test"), accessSvc, log)

	caller := middleware.Principal{UserID: uuid.New(), Roles: []middleware.Role{middleware.RolePatient}}
	return svc, caller
}

func TestUpload_RejectsUnsupportedDocumentType(t *testing.T) {
	svc, caller := newTestService(t, scan.PassthroughScanner{})
	_, err := svc.Upload(context.Background(), UploadInput{
		Principal: caller, DocumentType: DocumentType("x-ray-of-a-time-traveler"),
		Filename: "a.pdf", Data: strings.NewReader("%PDF-1.4 fake"),
	})
	if err == nil {
		t.Fatal("expected an error for an unsupported document_type")
	}
}

func TestUpload_RejectsEmptyFile(t *testing.T) {
	svc, caller := newTestService(t, scan.PassthroughScanner{})
	_, err := svc.Upload(context.Background(), UploadInput{
		Principal: caller, DocumentType: DocumentTypeReport,
		Filename: "a.pdf", Data: strings.NewReader(""),
	})
	if err == nil {
		t.Fatal("expected an error for an empty upload")
	}
}

func TestUpload_RejectsOversizeFile(t *testing.T) {
	svc, caller := newTestService(t, scan.PassthroughScanner{})
	big := bytes.Repeat([]byte("a"), MaxUploadBytes+1)
	_, err := svc.Upload(context.Background(), UploadInput{
		Principal: caller, DocumentType: DocumentTypeReport,
		Filename: "a.pdf", Data: bytes.NewReader(big),
	})
	if err == nil {
		t.Fatal("expected an error for a file exceeding the 10MB cap")
	}
}

func TestUpload_RejectsMismatchedContentType(t *testing.T) {
	svc, caller := newTestService(t, scan.PassthroughScanner{})
	// An HTML payload with a .pdf extension: extension allowlist passes,
	// sniffed content type does not match what .pdf permits.
	_, err := svc.Upload(context.Background(), UploadInput{
		Principal: caller, DocumentType: DocumentTypeReport,
		Filename: "innocuous.pdf", Data: strings.NewReader("<html><script>alert(1)</script></html>"),
	})
	if err == nil {
		t.Fatal("expected an error when sniffed content type does not match the extension")
	}
}

func TestUpload_RejectsDisallowedExtension(t *testing.T) {
	svc, caller := newTestService(t, scan.PassthroughScanner{})
	_, err := svc.Upload(context.Background(), UploadInput{
		Principal: caller, DocumentType: DocumentTypeReport,
		Filename: "malware.exe", Data: strings.NewReader("MZ\x90\x00fake-pe-header"),
	})
	if err == nil {
		t.Fatal("expected an error for a .exe upload")
	}
}

func TestUpload_RejectsInfectedFile(t *testing.T) {
	svc, caller := newTestService(t, fakeInfectedScanner{})
	_, err := svc.Upload(context.Background(), UploadInput{
		Principal: caller, DocumentType: DocumentTypeReport,
		Filename: "report.pdf", Data: strings.NewReader("%PDF-1.4 pretend this is a real pdf"),
	})
	if err == nil {
		t.Fatal("expected an error for a file flagged by the virus scanner")
	}
}
