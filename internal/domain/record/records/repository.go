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
	folder_id, created_at, updated_at, deleted_at, version`

func scanDocument(row pgx.Row) (Document, error) {
	var d Document
	var docType, scanStatus string
	err := row.Scan(&d.ID, &d.OwnerUserID, &d.UploadedBy, &docType, &d.Bucket, &d.ObjectKey, &d.Filename,
		&d.ContentType, &d.SizeBytes, &d.ChecksumSHA256, &scanStatus, &d.FHIRReferenceID,
		&d.FolderID, &d.CreatedAt, &d.UpdatedAt, &d.DeletedAt, &d.Version)
	d.DocumentType = DocumentType(docType)
	d.ScanStatus = ScanStatus(scanStatus)
	return d, err
}

// Create inserts a new document row.
func (r *Repository) Create(ctx context.Context, db dbtx, d Document) (Document, error) {
	q := fmt.Sprintf(`
		INSERT INTO documents (id, owner_user_id, uploaded_by, document_type, bucket, object_key, filename,
			content_type, size_bytes, checksum_sha256, scan_status, fhir_reference_id, folder_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, NULLIF($12, ''), $13)
		RETURNING %s`, documentColumns)
	row := db.QueryRow(ctx, q, d.ID, d.OwnerUserID, d.UploadedBy, string(d.DocumentType), d.Bucket, d.ObjectKey,
		d.Filename, d.ContentType, d.SizeBytes, d.ChecksumSHA256, string(d.ScanStatus), d.FHIRReferenceID, d.FolderID)
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
		args = append(args, string(f.DocumentType))
		where += fmt.Sprintf(` AND document_type = $%d`, len(args))
	}
	if f.Folder.Set {
		if f.Folder.ID == nil {
			where += ` AND folder_id IS NULL`
		} else {
			args = append(args, *f.Folder.ID)
			where += fmt.Sprintf(` AND folder_id = $%d`, len(args))
		}
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

// UpdateDocumentPlacement renames a document and/or files it into a folder.
func (r *Repository) UpdateDocumentPlacement(ctx context.Context, db dbtx, id uuid.UUID, filename string, folderID *uuid.UUID, expectedVersion int) (Document, error) {
	q := fmt.Sprintf(`
		UPDATE documents SET filename = $3, folder_id = $4, version = version + 1
		WHERE id = $1 AND version = $2 AND deleted_at IS NULL
		RETURNING %s`, documentColumns)
	out, err := scanDocument(db.QueryRow(ctx, q, id, expectedVersion, filename, folderID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Document{}, database.ErrOptimisticLock
		}
		return Document{}, fmt.Errorf("records: update document placement: %w", err)
	}
	return out, nil
}

// --- folders -----------------------------------------------------------

const folderColumns = `id, owner_user_id, parent_id, name, created_by, created_at, updated_at, version`

func scanFolder(row pgx.Row) (Folder, error) {
	var f Folder
	err := row.Scan(&f.ID, &f.OwnerUserID, &f.ParentID, &f.Name, &f.CreatedBy, &f.CreatedAt, &f.UpdatedAt, &f.Version)
	return f, err
}

// ErrDuplicateFolderName is returned when a sibling folder already has the name.
var ErrDuplicateFolderName = errors.New("records: a folder with this name already exists here")

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// CreateFolder inserts a folder.
func (r *Repository) CreateFolder(ctx context.Context, db dbtx, f Folder) (Folder, error) {
	q := fmt.Sprintf(`
		INSERT INTO folders (id, owner_user_id, parent_id, name, created_by)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING %s`, folderColumns)
	out, err := scanFolder(db.QueryRow(ctx, q, f.ID, f.OwnerUserID, f.ParentID, f.Name, f.CreatedBy))
	if err != nil {
		if isUniqueViolation(err) {
			return Folder{}, ErrDuplicateFolderName
		}
		return Folder{}, fmt.Errorf("records: create folder: %w", err)
	}
	return out, nil
}

// GetFolder fetches a non-deleted folder.
func (r *Repository) GetFolder(ctx context.Context, db dbtx, id uuid.UUID) (Folder, bool, error) {
	q := fmt.Sprintf(`SELECT %s FROM folders WHERE id = $1 AND deleted_at IS NULL`, folderColumns)
	out, err := scanFolder(db.QueryRow(ctx, q, id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Folder{}, false, nil
		}
		return Folder{}, false, fmt.Errorf("records: get folder: %w", err)
	}
	return out, true, nil
}

// ListFolders returns the child folders of parentID (nil = the vault root),
// alphabetically.
func (r *Repository) ListFolders(ctx context.Context, db dbtx, ownerUserID uuid.UUID, parentID *uuid.UUID) ([]Folder, error) {
	q := fmt.Sprintf(`
		SELECT %s FROM folders
		WHERE owner_user_id = $1 AND parent_id IS NOT DISTINCT FROM $2 AND deleted_at IS NULL
		ORDER BY lower(name)`, folderColumns)
	rows, err := db.Query(ctx, q, ownerUserID, parentID)
	if err != nil {
		return nil, fmt.Errorf("records: list folders: %w", err)
	}
	defer rows.Close()
	out := []Folder{}
	for rows.Next() {
		f, err := scanFolder(rows)
		if err != nil {
			return nil, fmt.Errorf("records: scan folder: %w", err)
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("records: list folders rows: %w", err)
	}
	return out, nil
}

// FolderPath returns the chain of folders from the vault root down to id,
// inclusive. It powers breadcrumbs and the cycle check on a move.
func (r *Repository) FolderPath(ctx context.Context, db dbtx, id uuid.UUID) ([]Folder, error) {
	q := `
		WITH RECURSIVE chain AS (
			SELECT id, owner_user_id, parent_id, name, created_by, created_at, updated_at, version, 0 AS depth
			FROM folders WHERE id = $1 AND deleted_at IS NULL
			UNION ALL
			SELECT f.id, f.owner_user_id, f.parent_id, f.name, f.created_by, f.created_at, f.updated_at, f.version, c.depth + 1
			FROM folders f JOIN chain c ON f.id = c.parent_id
			WHERE f.deleted_at IS NULL AND c.depth < 64
		)
		SELECT id, owner_user_id, parent_id, name, created_by, created_at, updated_at, version
		FROM chain ORDER BY depth DESC`
	rows, err := db.Query(ctx, q, id)
	if err != nil {
		return nil, fmt.Errorf("records: folder path: %w", err)
	}
	defer rows.Close()
	var out []Folder
	for rows.Next() {
		f, err := scanFolder(rows)
		if err != nil {
			return nil, fmt.Errorf("records: scan folder path: %w", err)
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("records: folder path rows: %w", err)
	}
	return out, nil
}

// UpdateFolder renames and/or re-parents a folder.
func (r *Repository) UpdateFolder(ctx context.Context, db dbtx, id uuid.UUID, name string, parentID *uuid.UUID, expectedVersion int) (Folder, error) {
	q := fmt.Sprintf(`
		UPDATE folders SET name = $3, parent_id = $4, version = version + 1
		WHERE id = $1 AND version = $2 AND deleted_at IS NULL
		RETURNING %s`, folderColumns)
	out, err := scanFolder(db.QueryRow(ctx, q, id, expectedVersion, name, parentID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Folder{}, database.ErrOptimisticLock
		}
		if isUniqueViolation(err) {
			return Folder{}, ErrDuplicateFolderName
		}
		return Folder{}, fmt.Errorf("records: update folder: %w", err)
	}
	return out, nil
}

// FolderIsEmpty reports whether a folder has no live child folders or documents.
func (r *Repository) FolderIsEmpty(ctx context.Context, db dbtx, id uuid.UUID) (bool, error) {
	const q = `
		SELECT NOT EXISTS (SELECT 1 FROM folders WHERE parent_id = $1 AND deleted_at IS NULL)
		   AND NOT EXISTS (SELECT 1 FROM documents WHERE folder_id = $1 AND deleted_at IS NULL)`
	var empty bool
	if err := db.QueryRow(ctx, q, id).Scan(&empty); err != nil {
		return false, fmt.Errorf("records: folder emptiness: %w", err)
	}
	return empty, nil
}

// SoftDeleteFolder marks a folder deleted.
func (r *Repository) SoftDeleteFolder(ctx context.Context, db dbtx, id uuid.UUID, expectedVersion int) error {
	const q = `UPDATE folders SET deleted_at = NOW(), version = version + 1 WHERE id = $1 AND version = $2 AND deleted_at IS NULL`
	tag, err := db.Exec(ctx, q, id, expectedVersion)
	if err != nil {
		return fmt.Errorf("records: soft delete folder: %w", err)
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
