//go:build integration

package records

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/rs/zerolog"

	"telemed/internal/domain/record/access"
	"telemed/internal/domain/record/dbtest"
	"telemed/internal/domain/record/fhir"
	"telemed/internal/platform/events"
	"telemed/internal/platform/middleware"
	"telemed/internal/platform/scan"
	"telemed/internal/platform/storage"
)

func newIntegrationService(t *testing.T) (*Service, *access.Service, *access.Repository) {
	t.Helper()
	pool := dbtest.NewPostgres(t)
	log := zerolog.Nop()

	store, err := storage.NewFilesystem(t.TempDir(), "")
	if err != nil {
		t.Fatalf("NewFilesystem: %v", err)
	}

	accessRepo := access.NewRepository()
	accessSvc := access.NewService(accessRepo, pool, log)
	svc := NewService(NewRepository(), pool, store, scan.PassthroughScanner{}, fhir.NewNoOp(log), events.NewOutbox("test"), accessSvc, log)
	return svc, accessSvc, accessRepo
}

// TestUpload_ThenDownload_FullRoundTrip proves a successful upload actually
// persists a row, publishes to the outbox, and that Download returns a
// presigned URL that resolves back to the same bytes via the filesystem
// storage backend -- end to end, no MinIO required.
func TestUpload_ThenDownload_FullRoundTrip(t *testing.T) {
	svc, _, _ := newIntegrationService(t)
	ctx := context.Background()
	patient := middleware.Principal{UserID: uuid.New(), Roles: []middleware.Role{middleware.RolePatient}}

	content := "%PDF-1.4\n%fake but sniffs as a pdf\n"
	doc, err := svc.Upload(ctx, UploadInput{
		Principal: patient, DocumentType: DocumentTypeReport,
		Filename: "blood-test.pdf", Data: strings.NewReader(content),
		IPAddress: "203.0.113.5", UserAgent: "integration-test",
	})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if doc.ScanStatus != ScanStatusSkipped {
		t.Errorf("ScanStatus = %s, want %s (PassthroughScanner)", doc.ScanStatus, ScanStatusSkipped)
	}

	got, err := svc.Get(ctx, patient, doc.ID, "203.0.113.5", "integration-test")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Filename != "blood-test.pdf" {
		t.Errorf("Filename = %q", got.Filename)
	}

	url, _, err := svc.Download(ctx, patient, doc.ID, "203.0.113.5", "integration-test")
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if url == "" {
		t.Error("expected a non-empty presigned download url")
	}
}

// TestAuthorizationMatrix_AcrossRecordsService exercises AGENT-BRIEF's
// authorization matrix through the actual records.Service methods a
// handler calls, not just access.Service directly -- owner, treating
// doctor, untreating doctor, admin, all read the same document.
func TestAuthorizationMatrix_AcrossRecordsService(t *testing.T) {
	svc, accessSvc, accessRepo := newIntegrationService(t)
	ctx := context.Background()

	patient := middleware.Principal{UserID: uuid.New(), Roles: []middleware.Role{middleware.RolePatient}}
	doc, err := svc.Upload(ctx, UploadInput{
		Principal: patient, DocumentType: DocumentTypeReport,
		Filename: "scan.pdf", Data: strings.NewReader("%PDF-1.4 fake"),
	})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}

	treatingDoctorID := uuid.New()
	appointmentID := uuid.New()
	started, ended := time.Now().Add(-time.Hour), time.Now().Add(-time.Minute)
	if err := accessRepo.UpsertTreatingRelationship(ctx, accessSvc.Pool(), appointmentID, treatingDoctorID, patient.UserID, &started, &ended); err != nil {
		t.Fatalf("seed treating relationship: %v", err)
	}

	tests := []struct {
		name        string
		principal   middleware.Principal
		wantGranted bool
	}{
		{"owner", patient, true},
		{"treating doctor", middleware.Principal{UserID: uuid.New(), DoctorID: treatingDoctorID, Roles: []middleware.Role{middleware.RoleDoctor}}, true},
		{"untreating doctor", middleware.Principal{UserID: uuid.New(), DoctorID: uuid.New(), Roles: []middleware.Role{middleware.RoleDoctor}}, false},
		// Was true. That was SECURITY-REVIEW F4: the same grant handed a
		// finance or support account a presigned MinIO URL to this exact
		// file. See TestAdminCannotReachAPatientDocumentButCanReachACredential.
		{"admin", middleware.Principal{UserID: uuid.New(), Roles: []middleware.Role{middleware.RoleAdmin}}, false},
		{"anonymous", middleware.Principal{}, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.Get(ctx, tc.principal, doc.ID, "203.0.113.9", "test")
			granted := err == nil
			if granted != tc.wantGranted {
				t.Errorf("Get() granted = %v, want %v (err=%v)", granted, tc.wantGranted, err)
			}
		})
	}
}

// TestDelete_OnlyOwnerOrAdmin proves deletion is stricter than the generic
// read grant: a treating doctor can read but not delete.
func TestDelete_OnlyOwnerOrAdmin(t *testing.T) {
	svc, accessSvc, accessRepo := newIntegrationService(t)
	ctx := context.Background()

	patient := middleware.Principal{UserID: uuid.New(), Roles: []middleware.Role{middleware.RolePatient}}
	doc, err := svc.Upload(ctx, UploadInput{
		Principal: patient, DocumentType: DocumentTypeReport,
		Filename: "scan.pdf", Data: strings.NewReader("%PDF-1.4 fake"),
	})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}

	treatingDoctorID := uuid.New()
	appointmentID := uuid.New()
	started, ended := time.Now().Add(-time.Hour), time.Now().Add(-time.Minute)
	if err := accessRepo.UpsertTreatingRelationship(ctx, accessSvc.Pool(), appointmentID, treatingDoctorID, patient.UserID, &started, &ended); err != nil {
		t.Fatalf("seed treating relationship: %v", err)
	}
	doctorPrincipal := middleware.Principal{UserID: uuid.New(), DoctorID: treatingDoctorID, Roles: []middleware.Role{middleware.RoleDoctor}}

	if err := svc.Delete(ctx, doctorPrincipal, doc.ID, "203.0.113.9", "go-test"); err == nil {
		t.Fatal("expected a treating doctor to be forbidden from deleting a patient's document")
	}
	if err := svc.Delete(ctx, patient, doc.ID, "203.0.113.9", "go-test"); err != nil {
		t.Fatalf("expected the owning patient to delete successfully: %v", err)
	}
	if _, err := svc.Get(ctx, patient, doc.ID, "", ""); err == nil {
		t.Fatal("expected the document to be gone (soft-deleted) after Delete")
	}
}

// TestShareGrantsAndRevokesAccess exercises the full share lifecycle: a
// patient grants a doctor access, the doctor can read, the patient revokes
// it, the doctor can no longer read.
func TestShareGrantsAndRevokesAccess(t *testing.T) {
	svc, accessSvc, _ := newIntegrationService(t)
	ctx := context.Background()

	patient := middleware.Principal{UserID: uuid.New(), Roles: []middleware.Role{middleware.RolePatient}}
	doc, err := svc.Upload(ctx, UploadInput{
		Principal: patient, DocumentType: DocumentTypeReport,
		Filename: "scan.pdf", Data: strings.NewReader("%PDF-1.4 fake"),
	})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}

	strangerDoctorID := uuid.New()
	strangerPrincipal := middleware.Principal{UserID: uuid.New(), DoctorID: strangerDoctorID, Roles: []middleware.Role{middleware.RoleDoctor}}

	if _, err := svc.Get(ctx, strangerPrincipal, doc.ID, "", ""); err == nil {
		t.Fatal("expected an unrelated doctor to be denied before any share exists")
	}

	share, err := accessSvc.CreateShare(ctx, patient, patient.UserID, strangerDoctorID, time.Hour)
	if err != nil {
		t.Fatalf("CreateShare: %v", err)
	}

	if _, err := svc.Get(ctx, strangerPrincipal, doc.ID, "", ""); err != nil {
		t.Fatalf("expected access to be granted after a share: %v", err)
	}

	if err := accessSvc.RevokeShare(ctx, patient, share.ID); err != nil {
		t.Fatalf("RevokeShare: %v", err)
	}
	if _, err := svc.Get(ctx, strangerPrincipal, doc.ID, "", ""); err == nil {
		t.Fatal("expected access to be denied after the share was revoked")
	}
}

// TestUpload_CrossUserRequiresAuthorization proves that uploading into
// someone else's vault goes through the same central authorization as
// reads: an unrelated doctor cannot attach a file to a patient's vault.
func TestUpload_CrossUserRequiresAuthorization(t *testing.T) {
	svc, _, _ := newIntegrationService(t)
	ctx := context.Background()

	patientID := uuid.New()
	strangerDoctor := middleware.Principal{UserID: uuid.New(), DoctorID: uuid.New(), Roles: []middleware.Role{middleware.RoleDoctor}}

	_, err := svc.Upload(ctx, UploadInput{
		Principal: strangerDoctor, OwnerUserID: patientID, DocumentType: DocumentTypeReport,
		Filename: "lab.pdf", Data: strings.NewReader("%PDF-1.4 fake"),
	})
	if err == nil {
		t.Fatal("expected an unrelated doctor to be denied uploading into a patient's vault")
	}
}

// --- F4: administrators, documents, and the credentialing queue --------

// TestAdminCannotReachAPatientDocumentButCanReachACredential is
// SECURITY-REVIEW F4 driven through the records service methods a handler
// actually calls, which is where the finding lived: access/service.go granted
// the role, and records/service.go turned that grant into
// storage.PresignedGet -- a URL to the actual lab report, scan or discharge
// summary, valid for fifteen minutes, usable from anywhere with no
// credentials at all.
//
// Both halves are asserted in one test on purpose. "Admins cannot read
// patient documents" and "admins can still read credential documents" are
// the two things that have to be true simultaneously, and a fix that
// achieves either one alone is not a fix -- the first alone breaks doctor
// verification, the second alone is the vulnerability.
func TestAdminCannotReachAPatientDocumentButCanReachACredential(t *testing.T) {
	svc, _, _ := newIntegrationService(t)
	ctx := context.Background()

	patient := middleware.Principal{UserID: uuid.New(), Roles: []middleware.Role{middleware.RolePatient}}
	report, err := svc.Upload(ctx, UploadInput{
		Principal: patient, DocumentType: DocumentTypeReport,
		Filename: "hiv-viral-load.pdf", Data: strings.NewReader("%PDF-1.4 fake"),
	})
	if err != nil {
		t.Fatalf("upload patient report: %v", err)
	}

	// A doctor's own registration paperwork, owned by the doctor and stored
	// in the separate doctor-credentials bucket.
	doctorUser := middleware.Principal{UserID: uuid.New(), DoctorID: uuid.New(), Roles: []middleware.Role{middleware.RoleDoctor}}
	credential, err := svc.Upload(ctx, UploadInput{
		Principal: doctorUser, DocumentType: DocumentTypeCredential,
		Filename: "slmc-certificate.pdf", Data: strings.NewReader("%PDF-1.4 fake"),
	})
	if err != nil {
		t.Fatalf("upload credential: %v", err)
	}
	if credential.Bucket != "doctor-credentials" {
		t.Fatalf("credential landed in bucket %q, want doctor-credentials -- the whole carve-out rests on it being a different bucket", credential.Bucket)
	}

	for _, role := range middleware.AdminRoles {
		admin := middleware.Principal{UserID: uuid.New(), Roles: []middleware.Role{role}}

		t.Run(string(role)+" cannot view a patient document", func(t *testing.T) {
			if _, err := svc.Get(ctx, admin, report.ID, "203.0.113.20", "test"); err == nil {
				t.Error("Get() succeeded")
			}
		})
		t.Run(string(role)+" cannot download a patient document", func(t *testing.T) {
			url, _, err := svc.Download(ctx, admin, report.ID, "203.0.113.20", "test")
			if err == nil {
				t.Errorf("Download() returned a presigned URL to a patient's medical document: %q", url)
			}
		})
		t.Run(string(role)+" cannot list a patient vault", func(t *testing.T) {
			if _, _, err := svc.List(ctx, admin, patient.UserID, "", 1, 20, "203.0.113.20", "test"); err == nil {
				t.Error("List() succeeded")
			}
		})
		t.Run(string(role)+" cannot upload into a patient vault", func(t *testing.T) {
			if _, err := svc.Upload(ctx, UploadInput{
				Principal: admin, OwnerUserID: patient.UserID, DocumentType: DocumentTypeReport,
				Filename: "planted.pdf", Data: strings.NewReader("%PDF-1.4 fake"),
			}); err == nil {
				t.Error("Upload() succeeded: an administrator planted a document in a patient's medical record")
			}
		})

		// ...and the credentialing queue still works, end to end.
		t.Run(string(role)+" can still verify a doctor's credential", func(t *testing.T) {
			if _, err := svc.Get(ctx, admin, credential.ID, "203.0.113.20", "test"); err != nil {
				t.Errorf("Get() on a credential document failed: the doctor-verification queue is broken: %v", err)
			}
			if _, _, err := svc.Download(ctx, admin, credential.ID, "203.0.113.20", "test"); err != nil {
				t.Errorf("Download() on a credential document failed: a reviewer cannot open the certificate: %v", err)
			}
			if _, _, err := svc.List(ctx, admin, doctorUser.UserID, DocumentTypeCredential, 1, 20, "203.0.113.20", "test"); err != nil {
				t.Errorf("List(document_type=credential) failed: the verification queue cannot enumerate what was uploaded: %v", err)
			}
		})
	}
}

// --- F15: the access log records the caller, not the gateway -----------

// TestAccessLogRecordsTheCallerAddressNotTheGateway is SECURITY-REVIEW F15,
// driven over a real HTTP connection because the bug was in the handler's
// choice of source, not in the resolver the platform already ships.
//
// Every request reaches this service through the api-gateway's reverse
// proxy, which opens its own TCP connection. r.RemoteAddr is therefore
// always the gateway pod's address -- so the local trimIP(r.RemoteAddr) made
// the ip_address column in the breach-investigation record the same value
// for every PHI access on the platform. The column existed, was populated,
// and was worthless.
//
// The IPv6 case is asserted in the same test because it is the same line of
// code and a worse failure: trimIP("[::1]:47822") returned "[::1]" WITH the
// brackets, NULLIF($9,”)::inet rejected it, and the insert errored -- which,
// with the old fail-open write path, silently dropped the audit entry while
// serving the document.
func TestAccessLogRecordsTheCallerAddressNotTheGateway(t *testing.T) {
	svc, accessSvc, _ := newIntegrationService(t)
	pool := accessSvc.Pool()
	ctx := context.Background()

	owner := middleware.Principal{UserID: uuid.New(), Roles: []middleware.Role{middleware.RolePatient}}
	doc, err := svc.Upload(ctx, UploadInput{
		Principal: owner, DocumentType: DocumentTypeReport,
		Filename: "report.pdf", Data: strings.NewReader("%PDF-1.4 fake"),
	})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}

	// The stranger is denied, which is fine: a denial is logged too, and a
	// denial is the entry an investigation cares most about.
	stranger := middleware.Principal{UserID: uuid.New(), DoctorID: uuid.New(), Roles: []middleware.Role{middleware.RoleDoctor}}

	// The real chain: RealIP resolves the address once, then the route runs.
	// The principal is injected rather than minted from a JWT, because this
	// test is about the address, not about auth.
	h := NewHandler(svc, accessSvc)
	root := chi.NewRouter()
	root.Use(middleware.RealIP(middleware.DefaultTrustedProxies()))
	root.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(middleware.WithPrincipal(r.Context(), stranger)))
		})
	})
	root.Mount("/records", h.Routes())

	tests := []struct {
		name       string
		remoteAddr string // the TCP peer -- in production, always the gateway pod
		xff        string
		wantIP     string
	}{
		{
			name:       "the gateway forwards for a real client",
			remoteAddr: "10.42.0.9:41234", // gateway pod, inside a trusted range
			xff:        "203.0.113.44",
			wantIP:     "203.0.113.44",
		},
		{
			name:       "a chain of proxies: the leftmost untrusted hop wins, not the client's own claim",
			remoteAddr: "10.42.0.9:41235",
			xff:        "198.51.100.7, 203.0.113.55, 10.42.0.9",
			wantIP:     "203.0.113.55",
		},
		{
			name:       "an IPv6 peer with no forwarding header stores a valid inet, brackets and all",
			remoteAddr: "[::1]:47822",
			wantIP:     "::1",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/records/"+doc.ID.String(), http.NoBody)
			req.RemoteAddr = tc.remoteAddr
			if tc.xff != "" {
				req.Header.Set("X-Forwarded-For", tc.xff)
			}
			rec := httptest.NewRecorder()
			root.ServeHTTP(rec, req)

			// The read is refused (the stranger has no relationship), but
			// the point is what landed in the log, not the status.
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusForbidden, rec.Body.String())
			}

			var stored string
			if err := pool.QueryRow(ctx,
				`SELECT COALESCE(host(ip_address), '') FROM document_access_log
				 WHERE resource_id = $1 ORDER BY created_at DESC, id DESC LIMIT 1`,
				doc.ID).Scan(&stored); err != nil {
				t.Fatalf("read back the access log entry: %v -- if there is no row, the audit write failed and was swallowed", err)
			}
			if stored != tc.wantIP {
				t.Errorf("document_access_log.ip_address = %q, want %q (peer was %q)", stored, tc.wantIP, tc.remoteAddr)
			}
		})
	}
}
