package prescriptions

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/rs/zerolog"

	"telemed/internal/domain/record/access"
	"telemed/internal/domain/record/fhir"
	"telemed/internal/platform/database"
	"telemed/internal/platform/events"
	"telemed/internal/platform/httpx"
	"telemed/internal/platform/middleware"
	"telemed/internal/platform/storage"
)

// DoctorCredentialImages resolves a doctor's currently uploaded signature
// and seal object keys, so an issued prescription PDF can carry the actual
// images instead of a blank line. Implemented in-process by the doctor
// domain (see modular.KeyDoctorCredentialImages in cmd/telemed) -- this
// package never reads doctor-service's database directly (ADR-004); only
// the two keys cross the boundary, and this service still fetches the
// bytes themselves from the shared doctor-credentials bucket with its own
// storage client, the same way admin-service's credentialing review does.
type DoctorCredentialImages interface {
	// SignatureAndSealKeys returns "" for either key never uploaded. That is
	// a valid, expected state for a newly-approved doctor, not an error.
	SignatureAndSealKeys(ctx context.Context, doctorID uuid.UUID) (signatureKey, sealKey string, err error)
}

// Service implements prescription issuance, retrieval, PDF delivery, and
// public verification.
type Service struct {
	repo          *Repository
	pool          database.Pool
	store         storage.Storage
	fhirCli       fhir.Client
	outbox        *events.Outbox
	access        *access.Service
	hmacSecret    []byte
	verifyBaseURL string
	credentials   DoctorCredentialImages
	log           zerolog.Logger
}

// Config carries the settings Service needs beyond its dependencies.
type Config struct {
	// HMACSecret signs and verifies the QR/verification-URL content. It must
	// be the same value across every replica of this service (it is not
	// per-instance) and must never be logged.
	HMACSecret []byte
	// VerifyBaseURL is the public verification URL prefix, e.g.
	// "https://verify.yourapp.lk". The QR code encodes
	// "{VerifyBaseURL}/p/{id}?h={hmac}".
	VerifyBaseURL string
}

// NewService wires the prescriptions service.
func NewService(repo *Repository, pool database.Pool, store storage.Storage, fhirCli fhir.Client,
	outbox *events.Outbox, accessSvc *access.Service, cfg Config, log zerolog.Logger,
) *Service {
	return &Service{
		repo: repo, pool: pool, store: store, fhirCli: fhirCli, outbox: outbox, access: accessSvc,
		hmacSecret: cfg.HMACSecret, verifyBaseURL: strings.TrimRight(cfg.VerifyBaseURL, "/"),
		log: log.With().Str("component", "prescriptions").Logger(),
	}
}

// WithCredentialImages wires the doctor domain's signature/seal lookup.
// Mirrors doctor.Service.WithHolidayRegistrar: an optional, in-process
// cross-domain dependency set once at composition time, not a constructor
// parameter every test and every future caller has to thread through.
func (s *Service) WithCredentialImages(c DoctorCredentialImages) *Service {
	s.credentials = c
	return s
}

// ItemInput is one requested drug line.
type ItemInput struct {
	DrugName     string
	Strength     string
	Form         string
	Dosage       string
	Frequency    string
	DurationDays int
	Quantity     int
	Instructions string
	IsGeneric    bool
}

// IssueInput is everything needed to issue a prescription. DoctorName,
// DoctorSLMC, DoctorQualifications and Patient are display-only fields
// supplied by the issuing doctor's own client -- this service does not hold
// a doctor-service or user-service integration in this pass (see the
// README's "Known gaps" section); AppointmentID, DoctorID and PatientID are
// never taken from the request body for anything security-relevant --
// PatientID and DoctorID are always resolved from the treating relationship
// record, not trusted from the caller.
type IssueInput struct {
	Principal            middleware.Principal
	AppointmentID        uuid.UUID
	DoctorName           string
	DoctorSLMC           string
	DoctorQualifications string
	ClinicName           string
	Patient              PatientDisplay
	Items                []ItemInput
}

// Issue creates a prescription. The caller must be the doctor who actually
// treated the patient in the given, now-concluded appointment -- verified
// against the treating_relationships read-model (internal/access), never
// trusted from the request. See hmac.go for the tamper-evidence scheme
// applied to the result.
func (s *Service) Issue(ctx context.Context, in IssueInput) (Prescription, error) {
	if !in.Principal.HasRole(middleware.RoleDoctor) || in.Principal.DoctorID == uuid.Nil {
		return Prescription{}, httpx.ErrForbidden.WithCause(fmt.Errorf("prescriptions: only a doctor may issue a prescription"))
	}
	if len(in.Items) == 0 {
		return Prescription{}, httpx.NewError(http.StatusUnprocessableEntity, httpx.CodeValidation, "at least one drug item is required")
	}
	for i, it := range in.Items {
		if strings.TrimSpace(it.DrugName) == "" || strings.TrimSpace(it.Dosage) == "" || strings.TrimSpace(it.Frequency) == "" {
			return Prescription{}, httpx.NewError(http.StatusUnprocessableEntity, httpx.CodeValidation, fmt.Sprintf("item %d: drug_name, dosage and frequency are required", i))
		}
		if it.DurationDays <= 0 || it.Quantity <= 0 {
			return Prescription{}, httpx.NewError(http.StatusUnprocessableEntity, httpx.CodeValidation, fmt.Sprintf("item %d: duration_days and quantity must be positive", i))
		}
	}

	rel, ok, err := s.access.Repository().TreatingRelationshipByAppointment(ctx, s.access.Pool(), in.AppointmentID)
	if err != nil {
		return Prescription{}, httpx.ErrInternal.WithCause(err)
	}
	if !ok || rel.DoctorID != in.Principal.DoctorID {
		return Prescription{}, httpx.ErrForbidden.WithCause(fmt.Errorf("prescriptions: caller does not own appointment %s", in.AppointmentID))
	}
	if rel.EndedAt == nil {
		return Prescription{}, httpx.NewError(http.StatusConflict, httpx.CodeConflict, "the consultation for this appointment has not concluded yet")
	}
	// The treating relationship expires, and so does the authority to
	// prescribe from it. Without this, the doctor who saw a patient once in
	// 2020 could still issue them an HMAC-signed, pharmacy-verifiable
	// prescription today -- the same "one consultation is a permanent
	// capability" shape as SECURITY-REVIEW F3, on the more dangerous verb.
	// The window is the shared one (access.TreatingAccessWindow); a doctor
	// who needs to prescribe after it lapses needs a consultation, which is
	// the clinically correct answer anyway.
	if !rel.GrantsAccessAt(time.Now().UTC()) {
		return Prescription{}, httpx.NewError(http.StatusConflict, httpx.CodeConflict,
			"the consultation for this appointment is too old to prescribe against; see the patient again")
	}

	// A prescription without the treating doctor's actual signature and
	// seal is not a document a pharmacist should be asked to trust just
	// because this service was willing to print a blank line for it. The
	// lookup is in-process (never a database this service does not own,
	// ADR-004); "not wired" is a deployment misconfiguration and fails
	// closed the same way an unwired holiday registrar does elsewhere in
	// this codebase, and "not uploaded yet" is reported back to the doctor
	// as the thing they need to fix, not swallowed into a generic 500.
	if s.credentials == nil {
		s.log.Error().Msg("prescriptions: doctor.credential_images is not wired; refusing to issue without a signature/seal check")
		return Prescription{}, httpx.NewError(http.StatusServiceUnavailable, httpx.CodeUnavailable,
			"prescription issuance is temporarily unavailable")
	}
	signatureKey, sealKey, err := s.credentials.SignatureAndSealKeys(ctx, rel.DoctorID)
	if err != nil {
		return Prescription{}, httpx.ErrInternal.WithCause(fmt.Errorf("prescriptions: resolve doctor credential images: %w", err))
	}
	if signatureKey == "" || sealKey == "" {
		return Prescription{}, httpx.NewError(http.StatusUnprocessableEntity, httpx.CodeValidation,
			"upload your signature and seal/stamp in your profile before issuing a prescription")
	}
	// Each fetch is best-effort past this point: the keys existing is the
	// gate above, but a transient storage read failure or an image format
	// this pass cannot decode (see credentialImageKind) must degrade to "no
	// image on this PDF", not block a prescription the doctor is otherwise
	// fully entitled to issue.
	signatureImg := s.loadCredentialImage(ctx, signatureKey)
	sealImg := s.loadCredentialImage(ctx, sealKey)

	p := Prescription{
		ID: uuid.New(), AppointmentID: in.AppointmentID, DoctorID: rel.DoctorID, PatientID: rel.PatientID,
		DoctorName: in.DoctorName, DoctorSLMC: in.DoctorSLMC, DoctorQualifications: in.DoctorQualifications,
		// Truncated to microsecond precision because Postgres TIMESTAMPTZ
		// only stores microseconds: signing a nanosecond-precision value
		// here and later re-verifying against the microsecond-truncated
		// value Postgres hands back on read would make every legitimate,
		// untampered prescription fail verification. Truncating before
		// signing means the signed value and the persisted value are
		// bit-for-bit identical.
		IssuedAt: time.Now().UTC().Truncate(time.Microsecond), Status: StatusIssued,
	}
	for i, it := range in.Items {
		p.Items = append(p.Items, Item{
			DrugName: it.DrugName, Strength: it.Strength, Form: it.Form, Dosage: it.Dosage, Frequency: it.Frequency,
			DurationDays: it.DurationDays, Quantity: it.Quantity, Instructions: it.Instructions, IsGeneric: it.IsGeneric,
			SortOrder: i,
		})
	}
	p.VerificationHMAC = Sign(s.hmacSecret, p)

	verifyURL := fmt.Sprintf("%s/p/%s?h=%s", s.verifyBaseURL, p.ID, p.VerificationHMAC)
	pdfBytes, err := GeneratePDF(p, in.Patient, ClinicDisplay{ClinicName: in.ClinicName}, verifyURL, signatureImg, sealImg)
	if err != nil {
		return Prescription{}, httpx.ErrInternal.WithCause(err)
	}

	objectKey := p.ID.String() + ".pdf"
	if err := s.store.Put(ctx, storage.BucketPrescriptions, objectKey, bytes.NewReader(pdfBytes), int64(len(pdfBytes)), "application/pdf"); err != nil {
		return Prescription{}, httpx.ErrInternal.WithCause(fmt.Errorf("prescriptions: store pdf: %w", err))
	}
	p.PDFObjectKey = objectKey

	var created Prescription
	err = database.InTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		out, err := s.repo.Create(ctx, tx, p)
		if err != nil {
			return err
		}
		created = out
		// events.PrescriptionIssued is the canonical payload, shared with
		// every consumer. ItemCount is the only thing it says about the
		// contents: drug names are PHI and this event fans out, so a
		// consumer that needs the prescription itself fetches it over the
		// API and takes the access-log entry that comes with it.
		payload := events.PrescriptionIssued{
			PrescriptionID: out.ID, AppointmentID: out.AppointmentID, DoctorID: out.DoctorID,
			PatientID: out.PatientID, ItemCount: len(out.Items), IssuedAt: out.IssuedAt,
		}
		return s.outbox.Enqueue(ctx, tx, events.SubjectPrescriptionIssued, out.ID.String(), payload)
	})
	if err != nil {
		if database.IsUniqueViolation(err) {
			return Prescription{}, httpx.ErrConflict.WithCause(fmt.Errorf("prescriptions: a prescription already exists for appointment %s", in.AppointmentID))
		}
		s.log.Error().Err(err).Str("bucket", storage.BucketPrescriptions).Str("key", objectKey).
			Msg("prescription indexed write failed after pdf was stored")
		return Prescription{}, httpx.ErrInternal.WithCause(err)
	}

	s.attachFHIR(ctx, created)
	return created, nil
}

// loadCredentialImage fetches one doctor credential image and decides
// whether fpdf can embed it as-is. A nil result means "render the PDF
// without this image" -- logged, but never propagated as an Issue() error,
// because the key existing already proved the doctor did upload something;
// a transient storage blip or an exotic format (HEIC, TIFF) this pass does
// not decode is this service's limitation, not the doctor's.
func (s *Service) loadCredentialImage(ctx context.Context, key string) *CredentialImage {
	kind := credentialImageKind(key)
	if kind == "" {
		s.log.Warn().Str("key", key).Msg("prescriptions: doctor credential image has an unsupported extension for pdf embedding; rendering without it")
		return nil
	}
	rc, err := s.store.Get(ctx, storage.BucketDoctorCredentials, key)
	if err != nil {
		s.log.Warn().Err(err).Str("bucket", storage.BucketDoctorCredentials).Msg("prescriptions: could not fetch a doctor credential image; rendering without it")
		return nil
	}
	defer func() { _ = rc.Close() }()
	data, err := io.ReadAll(rc)
	if err != nil {
		s.log.Warn().Err(err).Msg("prescriptions: could not read a doctor credential image; rendering without it")
		return nil
	}
	// Belt-and-suspenders past the extension check: fpdf's image registry
	// has no recovery path for bytes that merely look like the format they
	// claim -- one corrupt upload would otherwise fail Output() for the
	// entire PDF, not just drop the one image.
	if _, _, err := image.DecodeConfig(bytes.NewReader(data)); err != nil {
		s.log.Warn().Err(err).Str("key", key).Msg("prescriptions: doctor credential image failed to decode; rendering without it")
		return nil
	}
	return &CredentialImage{Bytes: data, Kind: kind}
}

// credentialImageKind maps a credential object key's extension to the fpdf
// image type it decodes as. Only the two formats fpdf's registry accepts
// without a re-encode step are supported here; doctor-service's own upload
// allowlist (objectkey.go) is wider (also .webp, .heic, .tif) because that
// list is about what a reviewer can open in a browser, not what a Go PDF
// library can inline -- the two lists are allowed to disagree.
func credentialImageKind(key string) string {
	switch strings.ToLower(path.Ext(key)) {
	case ".png":
		return "PNG"
	case ".jpg", ".jpeg":
		return "JPG"
	default:
		return ""
	}
}

// attachFHIR best-effort creates one FHIR MedicationRequest per drug line
// and records the resulting resource ids. A FHIR outage does not block
// issuance: the prescription and its PDF are already durable by the time
// this runs.
func (s *Service) attachFHIR(ctx context.Context, p Prescription) {
	var ids []string
	for i := range p.Items {
		it := &p.Items[i]
		mr := fhir.MedicationRequest{
			Status: "active", Intent: "order",
			MedicationCodeableConcept: fhir.CodeableConcept{Text: fmt.Sprintf("%s %s (%s)", it.DrugName, it.Strength, it.Form)},
			Subject:                   fhir.Reference{Identifier: &fhir.Identifier{System: "https://telemed.lk/user", Value: p.PatientID.String()}},
			Requester:                 &fhir.Reference{Identifier: &fhir.Identifier{System: "https://slmc.lk/registration", Value: p.DoctorSLMC}},
			AuthoredOn:                p.IssuedAt.UTC().Format(time.RFC3339),
			DosageInstruction: []fhir.DosageInstruction{{
				Text: fmt.Sprintf("%s, %s for %d days", it.Dosage, it.Frequency, it.DurationDays),
			}},
			DispenseRequest: &fhir.DispenseRequest{Quantity: &fhir.Quantity{Value: float64(it.Quantity), Unit: it.Form}},
		}
		id, err := s.fhirCli.CreateMedicationRequest(ctx, mr)
		if err != nil {
			// The drug used to be on this line. A drug name is PHI -- the
			// drug names the condition -- and .Str() bypasses
			// logger.Redact entirely, so it went to the sink verbatim
			// next to an unmasked prescription_id that makes it directly
			// patient-linkable (SECURITY-REVIEW F20b). The item index is
			// all an operator needs: it says which line failed, and the
			// prescription itself is one authorised, audited read away.
			s.log.Warn().Err(err).Str("prescription_id", p.ID.String()).Int("item_index", i).Msg("fhir MedicationRequest creation failed")
			continue
		}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return
	}
	if err := s.repo.SetFHIRReference(ctx, s.pool, p.ID, strings.Join(ids, ","), p.Version); err != nil {
		s.log.Warn().Err(err).Str("prescription_id", p.ID.String()).Msg("failed to persist fhir reference ids")
	}
}

// Get fetches a prescription for its owner, the issuing doctor, a doctor
// holding a valid share, or an admin.
func (s *Service) Get(ctx context.Context, caller middleware.Principal, id uuid.UUID, ip, ua string) (Prescription, error) {
	p, ok, err := s.repo.GetByID(ctx, s.pool, id)
	if err != nil {
		return Prescription{}, httpx.ErrInternal.WithCause(err)
	}
	if !ok {
		return Prescription{}, httpx.ErrNotFound
	}
	if err := s.access.Check(ctx, access.CheckOptions{
		Principal: caller, OwnerUserID: p.PatientID, Resource: access.ResourcePrescription,
		ResourceID: p.ID, Action: access.ActionView, IPAddress: ip, UserAgent: ua,
	}); err != nil {
		return Prescription{}, err
	}
	return p, nil
}

// GetByAppointment fetches the prescription issued for one appointment, if any.
// Authorization is the same as Get: owner, treating doctor, or an active share.
func (s *Service) GetByAppointment(ctx context.Context, caller middleware.Principal, appointmentID uuid.UUID, ip, ua string) (Prescription, error) {
	p, ok, err := s.repo.GetByAppointment(ctx, s.pool, appointmentID)
	if err != nil {
		return Prescription{}, httpx.ErrInternal.WithCause(err)
	}
	if !ok {
		return Prescription{}, httpx.ErrNotFound
	}
	if err := s.access.Check(ctx, access.CheckOptions{
		Principal: caller, OwnerUserID: p.PatientID, Resource: access.ResourcePrescription,
		ResourceID: p.ID, Action: access.ActionView, IPAddress: ip, UserAgent: ua,
	}); err != nil {
		return Prescription{}, err
	}
	return p, nil
}

// PDF authorizes and opens the stored prescription file.
//
// Bytes go through this service rather than a presigned /files URL. The
// public API gateway has no /files route, so handing the browser a
// filesystem presign made Download PDF land on NOT_FOUND. The gateway
// already documents this endpoint as application/pdf.
func (s *Service) PDF(ctx context.Context, caller middleware.Principal, id uuid.UUID, ip, ua string) (io.ReadCloser, string, error) {
	p, ok, err := s.repo.GetByID(ctx, s.pool, id)
	if err != nil {
		return nil, "", httpx.ErrInternal.WithCause(err)
	}
	if !ok {
		return nil, "", httpx.ErrNotFound
	}
	if err := s.access.Check(ctx, access.CheckOptions{
		Principal: caller, OwnerUserID: p.PatientID, Resource: access.ResourcePrescription,
		ResourceID: p.ID, Action: access.ActionDownload, IPAddress: ip, UserAgent: ua,
	}); err != nil {
		return nil, "", err
	}
	if p.PDFObjectKey == "" {
		return nil, "", httpx.ErrInternal.WithCause(fmt.Errorf("prescriptions: %s has no stored pdf", p.ID))
	}
	body, err := s.store.Get(ctx, storage.BucketPrescriptions, p.PDFObjectKey)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil, "", httpx.ErrNotFound.WithCause(err)
		}
		return nil, "", httpx.NewError(http.StatusServiceUnavailable, httpx.CodeUnavailable,
			"prescription storage is unavailable").WithCause(err)
	}
	return body, "prescription-" + p.ID.String() + ".pdf", nil
}

// VerifyResult is the deliberately minimal shape returned by the public
// verification endpoint: enough for a pharmacist to trust the document in
// front of them, nothing that identifies the patient or their condition.
type VerifyResult struct {
	Valid      bool
	DoctorName string
	DoctorSLMC string
	IssuedAt   time.Time
	Status     Status
}

// Verify is the public, unauthenticated endpoint a pharmacist's scanner
// calls. It recomputes the HMAC over the prescription's current database
// content and compares it against providedHMAC using hmac.Equal (see
// hmac.go's Verify for why this, and not a comparison against the stored
// verification_hmac column, is what actually detects tampering). Every call
// is written to the append-only access log, matching every other read in
// this service.
func (s *Service) Verify(ctx context.Context, id uuid.UUID, providedHMAC, ip, ua string) (VerifyResult, error) {
	p, ok, err := s.repo.GetByID(ctx, s.pool, id)
	if err != nil {
		return VerifyResult{}, httpx.ErrInternal.WithCause(err)
	}
	if !ok {
		// An unknown id and a known id with a wrong HMAC return the SAME
		// answer.
		//
		// This used to 404 for a miss and 200 {"valid": false} for a hit,
		// which on the platform's one unauthenticated endpoint is a clean
		// existence oracle over the prescription table: no credential needed,
		// and it distinguishes "this prescription exists" from "it does not"
		// without ever holding the HMAC. UUIDv4 ids make enumeration
		// infeasible, which is why this was LOW and not worse -- but the
		// endpoint's whole contract is "valid or not", and there is no reason
		// for it to answer a second question it was never asked
		// (SECURITY-REVIEW F31, edge 1).
		//
		// Nothing is logged for a miss: there is no prescription to attribute
		// the access to, and inventing an owner_user_id for a row that does
		// not exist would put fiction in the append-only PHI log. That is also
		// what stops an anonymous caller inflating document_access_log with
		// random ids and burying a real breach in noise.
		return VerifyResult{Valid: false}, nil
	}

	valid := Verify(s.hmacSecret, p, providedHMAC)

	reason := access.ReasonPublicVerify
	if !valid {
		reason = access.Reason("denied:invalid_hmac")
	}
	// This log entry matters more here than anywhere else in the service.
	// Verify is the one unauthenticated endpoint: there is no principal, no
	// token and no session, so the caller's address is the ONLY identifying
	// signal a breach investigation will ever have about who scanned a
	// patient's prescription. Serving the answer without recording it --
	// which is what this did, logging the failure and returning the result
	// anyway (SECURITY-REVIEW F15) -- makes the endpoint permanently
	// unauditable. It fails closed, for the same reason and on the same
	// terms as access.Service.Check.
	if err := s.access.Repository().InsertAccessLog(ctx, s.access.Pool(), access.LogEntry{
		ResourceType: access.ResourcePrescription, ResourceID: p.ID, OwnerUserID: p.PatientID,
		AccessedByRole: "anonymous", Action: access.ActionVerify, Granted: valid, Reason: reason,
		IPAddress: ip, UserAgent: ua,
	}); err != nil {
		s.log.Error().Err(err).
			Str("audit", "phi_access_log_write_failed").
			Str("prescription_id", p.ID.String()).
			Str("action", string(access.ActionVerify)).
			Msg("prescription verification denied: the append-only access log could not be written")
		return VerifyResult{}, httpx.ErrUnavailable.WithCause(
			fmt.Errorf("prescriptions: refusing to serve a verification without an audit record: %w", err))
	}

	if !valid {
		return VerifyResult{Valid: false}, nil
	}
	return VerifyResult{
		Valid: true, DoctorName: p.DoctorName, DoctorSLMC: p.DoctorSLMC,
		IssuedAt: p.IssuedAt, Status: p.Status,
	}, nil
}

// SearchDrugs performs a prefix search over the Sri Lankan formulary.
func (s *Service) SearchDrugs(ctx context.Context, query string, limit int) ([]Drug, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, httpx.NewError(http.StatusBadRequest, httpx.CodeBadRequest, "q is required")
	}
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	drugs, err := s.repo.SearchDrugs(ctx, s.pool, query, limit)
	if err != nil {
		return nil, httpx.ErrInternal.WithCause(err)
	}
	return drugs, nil
}
