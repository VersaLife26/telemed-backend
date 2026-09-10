package records

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
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
	"telemed/internal/platform/logger"
	"telemed/internal/platform/middleware"
	"telemed/internal/platform/scan"
	"telemed/internal/platform/storage"
)

// Service implements the documents business rules: upload validation,
// listing, download, and soft delete. Authorization for every read is
// delegated to access.Service -- this package holds no ownership logic of
// its own beyond "who may upload into whose vault", which is one notch
// stricter than read access (see Upload).
type Service struct {
	repo    *Repository
	pool    database.Pool
	store   storage.Storage
	scanner scan.VirusScanner
	fhirCli fhir.Client
	outbox  *events.Outbox
	access  *access.Service
	log     zerolog.Logger
}

// NewService wires the documents service. Every external dependency arrives
// as an interface (Storage, VirusScanner, fhir.Client), per AGENT-BRIEF §0.6.
func NewService(repo *Repository, pool database.Pool, store storage.Storage, scanner scan.VirusScanner,
	fhirCli fhir.Client, outbox *events.Outbox, accessSvc *access.Service, log zerolog.Logger,
) *Service {
	return &Service{
		repo: repo, pool: pool, store: store, scanner: scanner,
		fhirCli: fhirCli, outbox: outbox, access: accessSvc,
		log: log.With().Str("component", "records").Logger(),
	}
}

// accessResourceFor maps a document type to the access-control resource it
// is protected as.
//
// Everything in a patient's vault is ResourceDocument, which no
// administrator role may read (SECURITY-REVIEW F4). A credential -- a
// doctor's own SLMC certificate, degree or NIC, stored in the separate
// "doctor-credentials" bucket -- is ResourceCredentialDocument, which they
// may, because the doctor-verification queue cannot verify a registration
// without opening the certificate that was uploaded to prove it.
//
// The mapping is derived from the same DocumentType that chose the bucket
// (BucketFor), so the authorisation resource and the storage location can
// never disagree about what a document is.
func accessResourceFor(t DocumentType) access.ResourceType {
	if t == DocumentTypeCredential {
		return access.ResourceCredentialDocument
	}
	return access.ResourceDocument
}

// UploadInput is the already-decoded, already-size-limited multipart upload.
// Handler owns pulling this out of an *http.Request; nothing here touches
// net/http types.
type UploadInput struct {
	Principal    middleware.Principal
	OwnerUserID  uuid.UUID // zero value means "the caller's own vault"
	DocumentType DocumentType
	Filename     string
	Data         io.Reader
	IPAddress    string
	UserAgent    string
}

// Upload validates, scans, stores and indexes one file. It enforces the size
// cap, sniffs the real content type (never trusting a client-supplied
// header), checks the extension/content-type pairing allowlist, computes the
// SHA-256 checksum, and runs the configured virus scanner before anything is
// written to object storage.
func (s *Service) Upload(ctx context.Context, in UploadInput) (Document, error) {
	if !ValidDocumentTypes[in.DocumentType] {
		return Document{}, httpx.NewError(http.StatusUnprocessableEntity, httpx.CodeValidation, "unsupported document_type")
	}

	ownerID := in.OwnerUserID
	if ownerID == uuid.Nil {
		ownerID = in.Principal.UserID
	}
	if ownerID != in.Principal.UserID {
		// Uploading into someone else's vault (e.g. a doctor attaching a lab
		// result during a consultation) goes through the same central
		// authorization used for reads, and is logged the same way.
		if err := s.access.Check(ctx, access.CheckOptions{
			Principal: in.Principal, OwnerUserID: ownerID, Resource: accessResourceFor(in.DocumentType),
			ResourceID: ownerID, Action: access.ActionUpload, IPAddress: in.IPAddress, UserAgent: in.UserAgent,
		}); err != nil {
			return Document{}, err
		}
	}

	// Read at most MaxUploadBytes+1 so an oversize upload is detected
	// without ever buffering more than one byte past the limit.
	buf, err := io.ReadAll(io.LimitReader(in.Data, MaxUploadBytes+1))
	if err != nil {
		return Document{}, httpx.ErrBadRequest.WithCause(fmt.Errorf("records: read upload: %w", err))
	}
	if len(buf) == 0 {
		return Document{}, httpx.NewError(http.StatusBadRequest, httpx.CodeBadRequest, "uploaded file is empty")
	}
	if len(buf) > MaxUploadBytes {
		return Document{}, httpx.NewError(http.StatusRequestEntityTooLarge, httpx.CodeBadRequest, "file exceeds the 10MB upload limit")
	}

	ext := strings.ToLower(filepath.Ext(in.Filename))
	sniffHeader := buf
	if len(sniffHeader) > 512 {
		sniffHeader = sniffHeader[:512]
	}
	sniffed := http.DetectContentType(sniffHeader)
	// http.DetectContentType appends "; charset=..." for text types; strip it
	// so comparisons against our allowlist are exact.
	if i := strings.IndexByte(sniffed, ';'); i >= 0 {
		sniffed = sniffed[:i]
	}
	if !IsAllowedUpload(ext, sniffed) {
		return Document{}, httpx.NewError(http.StatusUnsupportedMediaType, httpx.CodeBadRequest,
			fmt.Sprintf("file type not permitted (extension %q, detected %q)", ext, sniffed))
	}

	sum := sha256.Sum256(buf)
	checksum := hex.EncodeToString(sum[:])

	scanStatus := ScanStatusPending
	verdict, err := s.scanner.Scan(ctx, bytes.NewReader(buf), int64(len(buf)))
	switch {
	case err != nil && verdict == scan.VerdictInfected:
		return Document{}, httpx.NewError(http.StatusUnprocessableEntity, httpx.CodeUnprocessable, "file failed virus scan").WithCause(err)
	case err != nil:
		return Document{}, httpx.ErrInternal.WithCause(fmt.Errorf("records: virus scan: %w", err))
	}
	switch verdict {
	case scan.VerdictClean:
		scanStatus = ScanStatusClean
	case scan.VerdictSkipped:
		scanStatus = ScanStatusSkipped
	}

	bucket := BucketFor(in.DocumentType)
	objectKey := fmt.Sprintf("%s/%s%s", ownerID, uuid.New(), ext)

	if err := s.store.Put(ctx, bucket, objectKey, bytes.NewReader(buf), int64(len(buf)), sniffed); err != nil {
		return Document{}, httpx.ErrInternal.WithCause(fmt.Errorf("records: store upload: %w", err))
	}

	doc := Document{
		ID: uuid.New(), OwnerUserID: ownerID, UploadedBy: in.Principal.UserID,
		DocumentType: in.DocumentType, Bucket: bucket, ObjectKey: objectKey,
		Filename: in.Filename, ContentType: sniffed, SizeBytes: int64(len(buf)),
		ChecksumSHA256: checksum, ScanStatus: scanStatus,
	}

	err = database.InTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		created, err := s.repo.Create(ctx, tx, doc)
		if err != nil {
			return err
		}
		doc = created

		return nil
	})
	if err != nil {
		// The object is already durably stored; leaving it orphaned on a
		// failed index write is the safer failure mode than trying to
		// compensate here and risking a partially-deleted object mid-crash.
		// A lifecycle sweep can reconcile orphans by listing objects with no
		// matching documents row.
		s.log.Error().Err(err).Str("bucket", bucket).Str("key", objectKey).
			Msg("document indexed write failed after object was stored")
		return Document{}, httpx.ErrInternal.WithCause(err)
	}

	s.attachFHIRReference(ctx, doc)
	return doc, nil
}

// attachFHIRReference best-effort creates a DocumentReference in the
// configured FHIR store and records its id. It runs after the upload has
// already committed: a FHIR outage must not block a clinician from
// uploading a report, so this failure is logged, not propagated.
func (s *Service) attachFHIRReference(ctx context.Context, doc Document) {
	dr := fhir.DocumentReference{
		Status:  "current",
		Type:    fhir.CodeableConcept{Text: string(doc.DocumentType)},
		Subject: &fhir.Reference{Identifier: &fhir.Identifier{System: "https://telemed.lk/user", Value: doc.OwnerUserID.String()}},
		Date:    doc.CreatedAt.UTC().Format(time.RFC3339),
		Content: []fhir.DocumentReferenceContent{{
			Attachment: fhir.Attachment{ContentType: doc.ContentType, Title: doc.Filename},
		}},
	}
	refID, err := s.fhirCli.CreateDocumentReference(ctx, dr)
	if err != nil {
		s.log.Warn().Err(err).Str("document_id", doc.ID.String()).Msg("fhir DocumentReference creation failed")
		return
	}
	if err := s.repo.SetFHIRReference(ctx, s.pool, doc.ID, refID, doc.Version); err != nil {
		s.log.Warn().Err(err).Str("document_id", doc.ID.String()).Msg("failed to persist fhir reference id")
	}
}

// List returns one page of a vault. Listing another user's vault requires
// the same authorization as viewing one of its documents.
func (s *Service) List(ctx context.Context, caller middleware.Principal, ownerUserID uuid.UUID, docType DocumentType, page, perPage int, ip, ua string) ([]Document, int64, error) {
	if ownerUserID == uuid.Nil {
		ownerUserID = caller.UserID
	}
	if ownerUserID != caller.UserID {
		// The resource being authorised is derived from the filter, so the
		// grant and the rows that come back describe the same thing. An
		// administrator listing a doctor's credentials
		// (?document_type=credential) is authorised as, and receives, only
		// credentials; an unfiltered listing of somebody's vault is
		// authorised as ResourceDocument and refused. Without this the
		// credentialing queue would be the one admin workflow F4's fix
		// broke.
		if err := s.access.Check(ctx, access.CheckOptions{
			Principal: caller, OwnerUserID: ownerUserID, Resource: accessResourceFor(docType),
			ResourceID: ownerUserID, Action: access.ActionList, IPAddress: ip, UserAgent: ua,
		}); err != nil {
			return nil, 0, err
		}
	}
	if page < 1 {
		page = 1
	}
	if perPage < 1 || perPage > 100 {
		perPage = 20
	}
	docs, total, err := s.repo.ListByOwner(ctx, s.pool, ListFilter{OwnerUserID: ownerUserID, DocumentType: docType, Page: page, PerPage: perPage})
	if err != nil {
		return nil, 0, httpx.ErrInternal.WithCause(err)
	}
	return docs, total, nil
}

// Get fetches one document, enforcing read authorization and writing the
// audit trail entry.
func (s *Service) Get(ctx context.Context, caller middleware.Principal, id uuid.UUID, ip, ua string) (Document, error) {
	doc, ok, err := s.repo.GetByID(ctx, s.pool, id)
	if err != nil {
		return Document{}, httpx.ErrInternal.WithCause(err)
	}
	if !ok {
		return Document{}, httpx.ErrNotFound
	}
	if err := s.access.Check(ctx, access.CheckOptions{
		Principal: caller, OwnerUserID: doc.OwnerUserID, Resource: accessResourceFor(doc.DocumentType),
		ResourceID: doc.ID, Action: access.ActionView, IPAddress: ip, UserAgent: ua,
	}); err != nil {
		return Document{}, err
	}
	return doc, nil
}

// Download authorizes and returns a short-lived presigned URL rather than
// proxying file bytes through this service.
func (s *Service) Download(ctx context.Context, caller middleware.Principal, id uuid.UUID, ip, ua string) (string, Document, error) {
	doc, ok, err := s.repo.GetByID(ctx, s.pool, id)
	if err != nil {
		return "", Document{}, httpx.ErrInternal.WithCause(err)
	}
	if !ok {
		return "", Document{}, httpx.ErrNotFound
	}
	if err := s.access.Check(ctx, access.CheckOptions{
		Principal: caller, OwnerUserID: doc.OwnerUserID, Resource: accessResourceFor(doc.DocumentType),
		ResourceID: doc.ID, Action: access.ActionDownload, IPAddress: ip, UserAgent: ua,
	}); err != nil {
		return "", Document{}, err
	}
	if doc.ScanStatus == ScanStatusInfected {
		return "", Document{}, httpx.NewError(http.StatusUnprocessableEntity, httpx.CodeUnprocessable, "this file was flagged by the virus scanner and cannot be downloaded")
	}

	url, err := s.store.PresignedGet(ctx, doc.Bucket, doc.ObjectKey, storage.RecordPresignTTL)
	if err != nil {
		return "", Document{}, httpx.ErrInternal.WithCause(err)
	}
	return url, doc, nil
}

// Delete soft-deletes a document. Deletion is a write, and deliberately
// stricter than the read grants access.Service hands out: a treating doctor
// or a share can read a patient's vault, but only the owning patient (or an
// admin) can remove something from it.
func (s *Service) Delete(ctx context.Context, caller middleware.Principal, id uuid.UUID, ipAddress, userAgent string) error {
	doc, ok, err := s.repo.GetByID(ctx, s.pool, id)
	if err != nil {
		return httpx.ErrInternal.WithCause(err)
	}
	if !ok {
		return httpx.ErrNotFound
	}
	// Deletion goes through the same central authorisation every read does,
	// which means it is also LOGGED by the same call -- access.Check writes
	// the decision before returning, and refuses to serve one it could not
	// record.
	//
	// Before this, Delete was the only PHI-touching method in the service that
	// called neither Check nor InsertAccessLog: an inline
	// `caller.UserID != doc.OwnerUserID && !caller.IsAdmin()` and then
	// SoftDelete. Two things followed. Any of the five admin roles could
	// destroy a patient's medical documents -- an administrator explicitly
	// forbidden from READING a document could still erase it -- and it left
	// no entry in the append-only log whose entire purpose is answering "who
	// touched this record" (SECURITY-REVIEW F27).
	//
	// ActionDelete is a write, so the read-only grants that let an
	// administrator open a doctor's credential paperwork do not reach it. The
	// owner branch answers first, so a patient deleting their own document is
	// unaffected.
	if err := s.access.Check(ctx, access.CheckOptions{
		Principal: caller, OwnerUserID: doc.OwnerUserID,
		Resource: accessResourceFor(doc.DocumentType), ResourceID: doc.ID,
		Action: access.ActionDelete, IPAddress: ipAddress, UserAgent: userAgent,
	}); err != nil {
		return err
	}
	if err := s.repo.SoftDelete(ctx, s.pool, id, doc.Version); err != nil {
		if errors.Is(err, database.ErrOptimisticLock) {
			return httpx.ErrConflict.WithCause(err)
		}
		return httpx.ErrInternal.WithCause(err)
	}
	return nil
}

// MaskFilename is used by handlers when logging, so a request log line never
// carries the raw filename of a medical document (which can itself be PHI,
// e.g. "jane_doe_hiv_results.pdf").
func MaskFilename(name string) string { return logger.MaskID(name) }
