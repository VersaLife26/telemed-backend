package doctor

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"telemed/internal/platform/events"
)

// docQueryer is satisfied by both *pgxpool.Pool and pgx.Tx, so a document read
// can run either on its own or inside the transaction that just wrote one.
type docQueryer interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// InsertDocument records a credential upload. Only the MinIO object key is
// stored -- the client uploads bytes directly to record-service's presigned
// URL and hands us back the key, so file content never transits this
// service or lands in its logs.
//
// It takes a transaction because the doctor.documents_updated event announcing
// the upload is enqueued in the same one (ADR-005): a document that exists in
// this database but never reached the reviewer's queue is exactly the failure
// the outbox is here to prevent.
func (r *Repository) InsertDocument(ctx context.Context, tx pgx.Tx, doc *Document) error {
	const q = `
		INSERT INTO doctor_documents (id, doctor_id, document_type, object_key, uploaded_at, created_at, updated_at)
		VALUES ($1, $2, $3, $4, NOW(), NOW(), NOW())`
	_, err := tx.Exec(ctx, q, doc.ID, doc.DoctorID, string(doc.DocumentType), doc.ObjectKey)
	if err != nil {
		return fmt.Errorf("doctor: insert document: %w", err)
	}
	return nil
}

// ListDocuments returns every credential document a doctor has uploaded,
// newest first.
func (r *Repository) ListDocuments(ctx context.Context, doctorID uuid.UUID) ([]Document, error) {
	return r.listDocuments(ctx, r.pool, doctorID)
}

// ListDocumentsTx is ListDocuments inside a caller-supplied transaction, so
// the key set published on doctor.documents_updated is the one the same
// transaction just committed -- not a racing read that could miss a
// concurrent second upload or see a rolled-back one.
func (r *Repository) ListDocumentsTx(ctx context.Context, tx pgx.Tx, doctorID uuid.UUID) ([]Document, error) {
	return r.listDocuments(ctx, tx, doctorID)
}

// CredentialDocumentKeys collapses a document list into the four keys the
// admin verification queue renders. Documents are append-only, and
// listDocuments returns them newest first, so the first match for a type wins:
// re-uploading a rejected NIC scan replaces it for the reviewer without
// deleting the audit trail of what was submitted before.
//
// A specialty_board_certificate is deliberately NOT folded into the degree
// slot: they are different claims, and a reviewer ticking "degree verified"
// against a board certificate is a credentialing error, not a display quirk.
func CredentialDocumentKeys(docs []Document) events.DoctorCredentialDocuments {
	var out events.DoctorCredentialDocuments
	set := func(dst *string, key string) {
		if *dst == "" {
			*dst = key
		}
	}
	for _, d := range docs {
		switch d.DocumentType {
		case DocumentSLMCCertificate:
			set(&out.SLMCCertificateKey, d.ObjectKey)
		case DocumentNIC:
			set(&out.NICDocumentKey, d.ObjectKey)
		case DocumentDegreeCertificate:
			set(&out.DegreeCertificateKey, d.ObjectKey)
		case DocumentPhoto:
			set(&out.PhotoKey, d.ObjectKey)
		}
	}
	return out
}

// SignatureAndSealKeys collapses a document list into the doctor's current
// signature and seal object keys, or "" for either one never uploaded.
// Documents are append-only and listDocuments returns them newest first, so
// the first match for each type is the current one -- the same rule
// CredentialDocumentKeys applies to the four admin-review slots.
func SignatureAndSealKeys(docs []Document) (signatureKey, sealKey string) {
	for _, d := range docs {
		switch d.DocumentType {
		case DocumentSignature:
			if signatureKey == "" {
				signatureKey = d.ObjectKey
			}
		case DocumentSeal:
			if sealKey == "" {
				sealKey = d.ObjectKey
			}
		}
	}
	return signatureKey, sealKey
}

func (r *Repository) listDocuments(ctx context.Context, q docQueryer, doctorID uuid.UUID) ([]Document, error) {
	const sql = `
		SELECT id, doctor_id, document_type, object_key, uploaded_at, reviewed_at, reviewed_by, COALESCE(review_notes, '')
		FROM doctor_documents
		WHERE doctor_id = $1
		ORDER BY uploaded_at DESC`

	rows, err := q.Query(ctx, sql, doctorID)
	if err != nil {
		return nil, fmt.Errorf("doctor: list documents: %w", err)
	}
	defer rows.Close()

	var out []Document
	for rows.Next() {
		var doc Document
		var docType string
		if err := rows.Scan(&doc.ID, &doc.DoctorID, &docType, &doc.ObjectKey,
			&doc.UploadedAt, &doc.ReviewedAt, &doc.ReviewedBy, &doc.ReviewNotes); err != nil {
			return nil, fmt.Errorf("doctor: scan document: %w", err)
		}
		doc.DocumentType = DocumentType(docType)
		out = append(out, doc)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("doctor: documents rows: %w", err)
	}
	return out, nil
}
