//go:build integration

package doctor

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"telemed/internal/platform/database"
	"telemed/internal/platform/middleware"
	"telemed/internal/platform/storage"
)

// The unit tests in dto_exposure_test.go and objectkey_test.go pin the shapes
// and the derivation. These drive the REAL chi router, with the REAL
// middleware chain and a REAL database, and read the bytes that come back off
// the wire -- because "the DTO is correct" and "the endpoint does not leak"
// are different claims, and only the second one is the finding.

type exposureEnv struct {
	router chi.Router
	svc    *Service
	repo   *Repository
	pool   *pgxpool.Pool
	signer *meshSigner
}

func newExposureEnv(t *testing.T) *exposureEnv {
	t.Helper()

	pool := setupPostgres(t)
	svc, repo := newTestService(t, pool)
	store, err := storage.NewFilesystem(t.TempDir(), "http://storage.test", nil)
	if err != nil {
		t.Fatalf("filesystem storage: %v", err)
	}
	svc.WithCredentialStore(store)
	signer := newMeshSigner(t)

	h := NewHandler(svc, signer.authenticator(t))
	r := chi.NewRouter()
	r.Mount("/api/v1/doctors", h.Routes())

	return &exposureEnv{router: r, svc: svc, repo: repo, pool: pool, signer: signer}
}

// uploadCredential posts a real file the way the profile page does.
func (e *exposureEnv) uploadCredential(t *testing.T, token, docType string, data []byte) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	if err := mw.WriteField("document_type", docType); err != nil {
		t.Fatalf("write field: %v", err)
	}
	fw, err := mw.CreateFormFile("file", "upload")
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := fw.Write(data); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart: %v", err)
	}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/doctors/me/documents", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	return rec
}

var testPDF = []byte("%PDF-1.4\n1 0 obj<<>>endobj\ntrailer<<>>\n%%EOF\n")

func (e *exposureEnv) do(t *testing.T, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *strings.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	} else {
		rdr = strings.NewReader("")
	}
	req := httptest.NewRequestWithContext(t.Context(), method, path, rdr)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	return rec
}

// registerDoctor creates a doctor profile and returns its id plus a token for
// the account that owns it.
func (e *exposureEnv) registerDoctor(t *testing.T, slmc, name string) (doctorID uuid.UUID, token string) {
	t.Helper()

	userID := uuid.New()
	d, err := e.svc.Register(context.Background(), RegisterInput{
		UserID: userID, DisplayName: name, SLMCNumber: slmc, Specialty: "psychiatry",
		ExperienceYears: 5, FeeCents: 250000, Languages: []Language{LanguageEN},
	})
	if err != nil {
		t.Fatalf("register %s: %v", name, err)
	}
	return d.ID, e.signer.token(t, userID, "doctor")
}

// TestIntegration_CredentialUploadRefusesAnAttackerChosenKey is F8 end to end.
//
// Dr B has Dr A's slmc_certificate_key -- it travels on
// doctor.documents_updated, sits in admin-service's projection, and is on the
// pending-review response. Before this fix, POSTing it as their own stored it
// verbatim, and the credentialing reviewer would presign and open Dr A's
// genuine certificate against Dr B's application.
func TestIntegration_CredentialUploadRefusesAnAttackerChosenKey(t *testing.T) {
	env := newExposureEnv(t)
	ctx := context.Background()

	drA, tokenA := env.registerDoctor(t, "SLMC7001", "Dr. A")
	drB, tokenB := env.registerDoctor(t, "SLMC7002", "Dr. B")

	// Dr A uploads their genuine SLMC certificate.
	rec := env.uploadCredential(t, tokenA, "slmc_certificate", testPDF)
	if rec.Code != http.StatusCreated {
		t.Fatalf("Dr A's own upload: status %d, body %s", rec.Code, rec.Body.String())
	}

	var created struct {
		Data struct {
			ObjectKey string `json:"object_key"`
			DoctorID  string `json:"doctor_id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v (body %s)", err, rec.Body.String())
	}
	victimKey := created.Data.ObjectKey
	if !strings.HasPrefix(victimKey, drA.String()+"/") {
		t.Fatalf("the derived key %q is not bound to Dr A's doctor id %s", victimKey, drA)
	}

	// Dr B submits it as their own. This is the exploit.
	rec = env.do(t, http.MethodPost, "/api/v1/doctors/me/documents", tokenB,
		`{"document_type":"slmc_certificate","object_key":"`+victimKey+`"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("Dr B submitted Dr A's certificate key and got status %d, want 400.\n"+
			"A credentialing reviewer would open Dr A's genuine SLMC certificate against Dr B's "+
			"application and approve someone to practise medicine on it.\nbody: %s",
			rec.Code, rec.Body.String())
	}

	// Nothing was written, and in particular Dr B holds no document pointing
	// at Dr A's object.
	docs, err := env.repo.ListDocuments(ctx, drB)
	if err != nil {
		t.Fatalf("list Dr B's documents: %v", err)
	}
	for _, d := range docs {
		if d.ObjectKey == victimKey || strings.HasPrefix(d.ObjectKey, drA.String()) {
			t.Fatalf("Dr B holds a document pointing at Dr A's storage: %q", d.ObjectKey)
		}
	}
	if len(docs) != 0 {
		t.Fatalf("the refused upload still wrote %d document row(s)", len(docs))
	}

	// Positive control: Dr B's own legitimate upload still works, and lands
	// under their own prefix.
	rec = env.uploadCredential(t, tokenB, "slmc_certificate", testPDF)
	if rec.Code != http.StatusCreated {
		t.Fatalf("Dr B's own upload: status %d, body %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.HasPrefix(created.Data.ObjectKey, drB.String()+"/") {
		t.Fatalf("Dr B's key %q is not bound to Dr B's doctor id %s", created.Data.ObjectKey, drB)
	}
	if !strings.HasSuffix(created.Data.ObjectKey, ".pdf") {
		t.Fatalf("the sniffed extension was dropped: %q", created.Data.ObjectKey)
	}

	// The object behind the row exists: a reviewer's link opens the file.
	if _, err := env.svc.store.Stat(ctx, storage.BucketDoctorCredentials, created.Data.ObjectKey); err != nil {
		t.Fatalf("stored document row has no object behind it: %v", err)
	}

	// Metadata alone is refused; it used to create a row pointing at nothing.
	rec = env.do(t, http.MethodPost, "/api/v1/doctors/me/documents", tokenB,
		`{"document_type":"slmc_certificate","filename":"ghost.pdf"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("metadata-only upload: status %d, want 400", rec.Code)
	}
}

// TestIntegration_PublicReviewsDoNotMapPatientsToDoctors is F7 end to end.
//
// The request carries no credentials at all, which is exactly how the exploit
// runs: walk the public doctor search, then pull each doctor's reviews.
func TestIntegration_PublicReviewsDoNotMapPatientsToDoctors(t *testing.T) {
	env := newExposureEnv(t)
	ctx := context.Background()

	// A psychiatrist, deliberately: for this specialty the doctor IS the
	// diagnosis, so the patient-to-doctor mapping is a disclosure of medical
	// condition on its own.
	doctorID, _ := env.registerDoctor(t, "SLMC7003", "Dr. Psychiatrist")

	patientID := uuid.MustParse("55555555-5555-4555-8555-555555555555")
	appointmentID := uuid.MustParse("66666666-6666-4666-8666-666666666666")

	rv := &Review{
		ID: uuid.New(), DoctorID: doctorID, PatientID: patientID, AppointmentID: appointmentID,
		Rating: 5, Comment: "Very thorough, explained everything clearly.", IsPublished: true,
	}
	if err := database.InTx(ctx, env.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		return env.repo.InsertReview(ctx, tx, rv)
	}); err != nil {
		t.Fatalf("insert review: %v", err)
	}

	// No Authorization header. None is required -- that is the finding.
	rec := env.do(t, http.MethodGet, "/api/v1/doctors/"+doctorID.String()+"/reviews", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	if strings.Contains(body, patientID.String()) {
		t.Fatalf("an unauthenticated caller received the patient id.\n"+
			"Walking the public doctor search now yields a table of which patient consulted which "+
			"doctor.\nbody: %s", body)
	}
	if strings.Contains(body, appointmentID.String()) {
		t.Fatalf("an unauthenticated caller received the appointment id.\nbody: %s", body)
	}
	if strings.Contains(body, "patient_id") {
		t.Fatalf("the public reviews response has a patient_id key.\nbody: %s", body)
	}

	// Positive control: the endpoint still returns a usable review.
	var listed struct {
		Data []struct {
			Rating    int    `json:"rating"`
			Comment   string `json:"comment"`
			CreatedAt string `json:"created_at"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode: %v (body %s)", err, body)
	}
	if len(listed.Data) != 1 {
		t.Fatalf("got %d reviews, want 1", len(listed.Data))
	}
	if listed.Data[0].Rating != 5 || listed.Data[0].Comment != rv.Comment || listed.Data[0].CreatedAt == "" {
		t.Fatalf("the public review lost the fields it exists to carry: %s", body)
	}
}

// The other half of F7: rejection_reason is an internal credentialing note and
// must not be on the profile a patient can fetch, while the doctor themselves
// must still be able to read it.
func TestIntegration_PublicDoctorProfileHidesTheRejectionReason(t *testing.T) {
	env := newExposureEnv(t)
	ctx := context.Background()

	doctorID, ownerToken := env.registerDoctor(t, "SLMC7004", "Dr. Rejected")

	const reason = "SLMC certificate appears altered; registrar could not confirm the number"
	if _, err := env.svc.Verify(ctx, doctorID, ActionReject, reason, nil); err != nil {
		t.Fatalf("reject: %v", err)
	}
	// Back to under_review, then approved, so the profile is publicly
	// visible while still carrying the note from the earlier rejection --
	// which is precisely the row that used to leak.
	if _, err := env.svc.Verify(ctx, doctorID, ActionReopen, "reopened after resubmission", nil); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if _, err := env.svc.Verify(ctx, doctorID, ActionApprove, "", nil); err != nil {
		t.Fatalf("approve: %v", err)
	}

	anon := env.do(t, http.MethodGet, "/api/v1/doctors/"+doctorID.String(), "", "")
	if anon.Code != http.StatusOK {
		t.Fatalf("anonymous profile fetch: status %d, body %s", anon.Code, anon.Body.String())
	}
	if strings.Contains(anon.Body.String(), reason) {
		t.Fatalf("the public doctor profile carries the internal rejection reason.\nbody: %s", anon.Body.String())
	}

	own := env.do(t, http.MethodGet, "/api/v1/doctors/me", ownerToken, "")
	if own.Code != http.StatusOK {
		t.Fatalf("own profile fetch: status %d, body %s", own.Code, own.Body.String())
	}
	if !strings.Contains(own.Body.String(), reason) {
		t.Fatalf("the doctor's own profile lost the rejection reason; a rejected applicant cannot act "+
			"on a decision they cannot read.\nbody: %s", own.Body.String())
	}
}

// The verification actor is read from the token, never from the request body.
// Before this, the audit record of who approved a doctor to practise medicine
// was whatever uuid the caller put in actor_id.
func TestIntegration_VerificationActorComesFromTheTokenNotTheBody(t *testing.T) {
	env := newExposureEnv(t)
	ctx := context.Background()

	doctorID, _ := env.registerDoctor(t, "SLMC7005", "Dr. Applicant")

	realAdmin := uuid.New()
	framedColleague := uuid.New()

	// The internal surface is mounted separately in main.go, behind
	// RequireAuth + RequireRole. Reproduced here so the test exercises the
	// same chain a deployment runs.
	h := NewHandler(env.svc, env.signer.authenticator(t))
	r := chi.NewRouter()
	r.Route("/api/v1/internal/doctors", func(r chi.Router) {
		r.Use(middleware.RequireAuth(env.signer.authenticator(t)))
		r.Use(middleware.RequireRole(middleware.RoleAdmin, middleware.RoleSuperAdmin, middleware.RoleService))
		r.Mount("/", h.InternalRoutes())
	})

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost,
		"/api/v1/internal/doctors/"+doctorID.String()+"/verify",
		strings.NewReader(`{"action":"approve","actor_id":"`+framedColleague.String()+`"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+env.signer.token(t, realAdmin, "admin"))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("verify: status %d, body %s", rec.Code, rec.Body.String())
	}

	var verifiedBy *uuid.UUID
	if err := env.pool.QueryRow(ctx,
		`SELECT verified_by FROM doctors WHERE id = $1`, doctorID).Scan(&verifiedBy); err != nil {
		t.Fatalf("read verified_by: %v", err)
	}
	if verifiedBy == nil {
		t.Fatal("verified_by is NULL: the approval has no recorded actor at all")
	}
	if *verifiedBy == framedColleague {
		t.Fatalf("verified_by is the id the CALLER put in the body (%s).\n"+
			"The audit record of who approved a doctor to practise medicine was chosen by the request.",
			framedColleague)
	}
	if *verifiedBy != realAdmin {
		t.Fatalf("verified_by = %s, want the authenticated caller %s", *verifiedBy, realAdmin)
	}
}
