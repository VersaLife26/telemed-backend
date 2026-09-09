package records

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"telemed/internal/platform/database"
)

// dbtx is the narrow slice of pgx.Tx / database.Pool this repository needs.
type dbtx interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Repository is the SQL layer for documents. It never returns an
// *httpx.APIError -- that translation happens in service.go.
type Repository struct{}

// NewRepository constructs the (stateless) repository.
func NewRepository() *Repository { return &Repository{} }

const documentColumns = `id, owner_user_id, uploaded_by, document_type, bucket, object_key, filename,
	content_type, size_bytes, checksum_sha256, scan_status, COALESCE(fhir_reference_id, ''),
	created_at, updated_at, deleted_at, version`

func scanDocument(row pgx.Row) (Document, error) {
	var d Document
	var docType, scanStatus string
	err := row.Scan(&d.ID, &d.OwnerUserID, &d.UploadedBy, &docType, &d.Bucket, &d.ObjectKey, &d.Filename,
		&d.ContentType, &d.SizeBytes, &d.ChecksumSHA256, &scanStatus, &d.FHIRReferenceID,
		&d.CreatedAt, &d.UpdatedAt, &d.DeletedAt, &d.Version)
	d.DocumentType = DocumentType(docType)
	d.ScanStatus = ScanStatus(scanStatus)
	return d, err
}

// Create inserts a new document row.
func (r *Repository) Create(ctx context.Context, db dbtx, d Document) (Document, error) {
	q := fmt.Sprintf(`
		INSERT INTO documents (id, owner_user_id, uploaded_by, document_type, bucket, object_key, filename,
			content_type, size_bytes, checksum_sha256, scan_status, fhir_reference_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, NULLIF($12, ''))
		RETURNING %s`, documentColumns)
	row := db.QueryRow(ctx, q, d.ID, d.OwnerUserID, d.UploadedBy, string(d.DocumentType), d.Bucket, d.ObjectKey,
		d.Filename, d.ContentType, d.SizeBytes, d.ChecksumSHA256, string(d.ScanStatus), d.FHIRReferenceID)
	out, err := scanDocument(row)
	if err != nil {
		return Document{}, fmt.Errorf("records: create document: %w", err)
	}
	return out, nil
}

// GetByID fetches a non-deleted document.
func (r *Repository) GetByID(ctx context.Context, db dbtx, id uuid.UUID) (Document, bool, error) {
	q := fmt.Sprintf(`SELECT %s FROM documents WHERE id = $1 AND deleted_at IS NULL`, documentColumns)
	out, err := scanDocument(db.QueryRow(ctx, q, id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Document{}, false, nil
		}
		return Document{}, false, fmt.Errorf("records: get document: %w", err)
	}
	return out, true, nil
}

// ListByOwner returns one page of a patient's vault, newest first.
func (r *Repository) ListByOwner(ctx context.Context, db dbtx, f ListFilter) ([]Document, int64, error) {
	where := `WHERE owner_user_id = $1 AND deleted_at IS NULL`
	args := []any{f.OwnerUserID}
	if f.DocumentType != "" {
		where += ` AND document_type = $2`
		args = append(args, string(f.DocumentType))
	}

	var total int64
	countQ := `SELECT COUNT(*) FROM documents ` + where
	if err := db.QueryRow(ctx, countQ, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("records: count documents: %w", err)
	}

	limit, offset := f.PerPage, (f.Page-1)*f.PerPage
	args = append(args, limit, offset)
	listQ := fmt.Sprintf(`SELECT %s FROM documents %s ORDER BY created_at DESC LIMIT $%d OFFSET $%d`,
		documentColumns, where, len(args)-1, len(args))

	rows, err := db.Query(ctx, listQ, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("records: list documents: %w", err)
	}
	defer rows.Close()

	var out []Document
	for rows.Next() {
		d, err := scanDocument(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("records: scan document: %w", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("records: list documents rows: %w", err)
	}
	return out, total, nil
}

// UpdateScanStatus records the virus-scan verdict for a document.
func (r *Repository) UpdateScanStatus(ctx context.Context, db dbtx, id uuid.UUID, status ScanStatus, expectedVersion int) error {
	const q = `UPDATE documents SET scan_status = $3, version = version + 1 WHERE id = $1 AND version = $2`
	tag, err := db.Exec(ctx, q, id, expectedVersion, string(status))
	if err != nil {
		return fmt.Errorf("records: update scan status: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return database.ErrOptimisticLock
	}
	return nil
}

// SetFHIRReference stores the DocumentReference id once the FHIR call
// completes.
func (r *Repository) SetFHIRReference(ctx context.Context, db dbtx, id uuid.UUID, fhirRefID string, expectedVersion int) error {
	const q = `UPDATE documents SET fhir_reference_id = $3, version = version + 1 WHERE id = $1 AND version = $2`
	tag, err := db.Exec(ctx, q, id, expectedVersion, fhirRefID)
	if err != nil {
		return fmt.Errorf("records: set fhir reference: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return database.ErrOptimisticLock
	}
	return nil
}

// SoftDelete marks a document deleted without touching the underlying
// object -- retention policy (SDD §15: medical records kept 7 years) means
// the bytes outlive the patient's decision to hide the record from their
// own listing.
func (r *Repository) SoftDelete(ctx context.Context, db dbtx, id uuid.UUID, expectedVersion int) error {
	const q = `UPDATE documents SET deleted_at = NOW(), version = version + 1 WHERE id = $1 AND version = $2 AND deleted_at IS NULL`
	tag, err := db.Exec(ctx, q, id, expectedVersion)
	if err != nil {
		return fmt.Errorf("records: soft delete document: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return database.ErrOptimisticLock
	}
	return nil
}

var (
	_ dbtx = database.Pool(nil)
	_ dbtx = pgx.Tx(nil)
)
