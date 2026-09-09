package clinicalnotes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"telemed/internal/platform/database"
)

// dbtx is the narrow slice of pgx.Tx / database.Pool this repository needs.
// Accepting this rather than a concrete type is what lets every method run
// standalone against the pool or inside a caller's transaction -- which
// matters here because finalise and amend must write the note, its
// diagnoses, its revision and the outbox row in ONE transaction.
type dbtx interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Repository is the SQL layer for clinical notes, their diagnoses, their
// append-only revisions, and the ICD-10 reference table. It never returns an
// *httpx.APIError (AGENT-BRIEF layering rule); service.go translates.
type Repository struct{}

// NewRepository constructs the (stateless) repository.
func NewRepository() *Repository { return &Repository{} }

const noteColumns = `id, appointment_id, doctor_id, patient_id, subjective, objective, assessment, plan,
	status, finalised_at, COALESCE(fhir_composition_id, ''), created_at, updated_at, deleted_at, version`

func scanNote(row pgx.Row) (Note, error) {
	var n Note
	var status string
	err := row.Scan(&n.ID, &n.AppointmentID, &n.DoctorID, &n.PatientID,
		&n.Subjective, &n.Objective, &n.Assessment, &n.Plan,
		&status, &n.FinalisedAt, &n.FHIRCompositionID,
		&n.CreatedAt, &n.UpdatedAt, &n.DeletedAt, &n.Version)
	n.Status = Status(status)
	return n, err
}

// GetByAppointment fetches the note for one appointment, without its
// diagnoses. Returns ok=false when there is none, which is the normal state
// before the doctor's first keystroke.
func (r *Repository) GetByAppointment(ctx context.Context, db dbtx, appointmentID uuid.UUID) (Note, bool, error) {
	q := fmt.Sprintf(`SELECT %s FROM clinical_notes WHERE appointment_id = $1 AND deleted_at IS NULL`, noteColumns)
	n, err := scanNote(db.QueryRow(ctx, q, appointmentID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Note{}, false, nil
		}
		return Note{}, false, fmt.Errorf("clinicalnotes: get note by appointment: %w", err)
	}
	return n, true, nil
}

// Create inserts a new draft note.
//
// ON CONFLICT DO NOTHING plus a zero-row return is how the "two devices both
// typed the first character at once" race is resolved: appointment_id is
// UNIQUE, so exactly one INSERT wins and the loser is told the row changed
// under it rather than being handed a raw 23505. The caller turns that into
// the same 409 a version mismatch produces, and the losing client re-reads
// and continues -- which is precisely what it would have done had it been a
// millisecond slower.
func (r *Repository) Create(ctx context.Context, db dbtx, n Note) (Note, error) {
	q := fmt.Sprintf(`
		INSERT INTO clinical_notes (id, appointment_id, doctor_id, patient_id, subjective, objective, assessment, plan, status)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'draft')
		ON CONFLICT (appointment_id) DO NOTHING
		RETURNING %s`, noteColumns)
	out, err := scanNote(db.QueryRow(ctx, q, n.ID, n.AppointmentID, n.DoctorID, n.PatientID,
		n.Subjective, n.Objective, n.Assessment, n.Plan))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Note{}, database.ErrOptimisticLock
		}
		return Note{}, fmt.Errorf("clinicalnotes: create note: %w", err)
	}
	return out, nil
}

// UpdateDraft writes the four SOAP sections of a note that is still a draft,
// guarded by both the version and the status.
//
// The `status = 'draft'` predicate is not redundant with the service's own
// check. Between the service reading the note and issuing this UPDATE, the
// doctor's other device may have finalised it; without the predicate this
// statement would silently rewrite the text of a signed legal record. With
// it, the update matches zero rows and the caller gets ErrOptimisticLock,
// which is the honest answer: the row did change underneath you.
func (r *Repository) UpdateDraft(ctx context.Context, db dbtx, id uuid.UUID, expectedVersion int, n Note) (Note, error) {
	q := fmt.Sprintf(`
		UPDATE clinical_notes
		SET subjective = $3, objective = $4, assessment = $5, plan = $6, version = version + 1
		WHERE id = $1 AND version = $2 AND status = 'draft' AND deleted_at IS NULL
		RETURNING %s`, noteColumns)
	out, err := scanNote(db.QueryRow(ctx, q, id, expectedVersion, n.Subjective, n.Objective, n.Assessment, n.Plan))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Note{}, database.ErrOptimisticLock
		}
		return Note{}, fmt.Errorf("clinicalnotes: update draft: %w", err)
	}
	return out, nil
}

// Finalise flips a draft to finalised and stamps finalised_at, guarded by
// version and by the current status being 'draft'. The status predicate is
// what makes finalisation idempotent-safe under a double-tap: the second
// call matches no row and the caller reports a conflict instead of
// re-stamping a new finalised_at over the original signature time.
func (r *Repository) Finalise(ctx context.Context, db dbtx, id uuid.UUID, expectedVersion int, at time.Time) (Note, error) {
	q := fmt.Sprintf(`
		UPDATE clinical_notes
		SET status = 'finalised', finalised_at = $3, version = version + 1
		WHERE id = $1 AND version = $2 AND status = 'draft' AND deleted_at IS NULL
		RETURNING %s`, noteColumns)
	out, err := scanNote(db.QueryRow(ctx, q, id, expectedVersion, at))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Note{}, database.ErrOptimisticLock
		}
		return Note{}, fmt.Errorf("clinicalnotes: finalise note: %w", err)
	}
	return out, nil
}

// AmendFinalised rewrites the SOAP sections of an already-finalised note.
// The `status = 'finalised'` predicate mirrors UpdateDraft's: this statement
// must never be the thing that edits a draft, because a draft edit writes no
// revision and this path assumes one has been written alongside it.
//
// finalised_at is deliberately left alone. It records when the note was
// SIGNED, not when it was last touched; an amendment's own timestamp lives
// on its revision row, where it belongs.
func (r *Repository) AmendFinalised(ctx context.Context, db dbtx, id uuid.UUID, expectedVersion int, n Note) (Note, error) {
	q := fmt.Sprintf(`
		UPDATE clinical_notes
		SET subjective = $3, objective = $4, assessment = $5, plan = $6, version = version + 1
		WHERE id = $1 AND version = $2 AND status = 'finalised' AND deleted_at IS NULL
		RETURNING %s`, noteColumns)
	out, err := scanNote(db.QueryRow(ctx, q, id, expectedVersion, n.Subjective, n.Objective, n.Assessment, n.Plan))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Note{}, database.ErrOptimisticLock
		}
		return Note{}, fmt.Errorf("clinicalnotes: amend note: %w", err)
	}
	return out, nil
}

// SetFHIRComposition records the FHIR resource id of the Composition this
// note was last written to.
//
// This is the one write in the package that does NOT advance `version`, and
// that is deliberate. `version` is the optimistic-lock token the doctor's
// client holds; bumping it from a best-effort background FHIR sync that ran
// after the transaction committed would invalidate the token the client was
// just handed, and the doctor's next keystroke would come back 409 with
// nothing having changed in the note at all. The version guards CONTENT, and
// a server-side reference id is not content.
func (r *Repository) SetFHIRComposition(ctx context.Context, db dbtx, id uuid.UUID, compositionID string) error {
	const q = `UPDATE clinical_notes SET fhir_composition_id = $2 WHERE id = $1 AND deleted_at IS NULL`
	if _, err := db.Exec(ctx, q, id, compositionID); err != nil {
		return fmt.Errorf("clinicalnotes: set fhir composition: %w", err)
	}
	return nil
}

// ListDiagnoses returns a note's coded diagnoses in the order the doctor put
// them in.
func (r *Repository) ListDiagnoses(ctx context.Context, db dbtx, noteID uuid.UUID) ([]Diagnosis, error) {
	const q = `SELECT code, display, is_primary, sort_order FROM clinical_note_diagnoses WHERE note_id = $1 ORDER BY sort_order, code`
	rows, err := db.Query(ctx, q, noteID)
	if err != nil {
		return nil, fmt.Errorf("clinicalnotes: list diagnoses: %w", err)
	}
	defer rows.Close()

	out := make([]Diagnosis, 0, 4)
	for rows.Next() {
		var d Diagnosis
		if err := rows.Scan(&d.Code, &d.Display, &d.IsPrimary, &d.SortOrder); err != nil {
			return nil, fmt.Errorf("clinicalnotes: scan diagnosis: %w", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("clinicalnotes: list diagnoses rows: %w", err)
	}
	return out, nil
}

// ReplaceDiagnoses swaps a note's diagnosis set wholesale.
//
// Delete-then-insert rather than a diff. The set is at most MaxDiagnoses
// rows, it always arrives from the client as a complete list, and a diff
// would have to reason about reordering, primary-flag moves and code
// replacement to save two statements on a call the service only makes when
// the set actually changed. The caller is responsible for that check -- see
// Note.SameContentAs -- which is what keeps this off the per-keystroke path.
//
// Must be called inside the caller's transaction: between the DELETE and the
// INSERT the note momentarily has no diagnoses at all.
func (r *Repository) ReplaceDiagnoses(ctx context.Context, db dbtx, noteID uuid.UUID, diagnoses []Diagnosis) error {
	if _, err := db.Exec(ctx, `DELETE FROM clinical_note_diagnoses WHERE note_id = $1`, noteID); err != nil {
		return fmt.Errorf("clinicalnotes: clear diagnoses: %w", err)
	}
	const q = `INSERT INTO clinical_note_diagnoses (note_id, code, display, is_primary, sort_order) VALUES ($1, $2, $3, $4, $5)`
	for i, d := range diagnoses {
		if _, err := db.Exec(ctx, q, noteID, d.Code, d.Display, d.IsPrimary, i); err != nil {
			return fmt.Errorf("clinicalnotes: insert diagnosis %d: %w", i, err)
		}
	}
	return nil
}

// revisionDiagnosis is the JSONB shape frozen into a revision row. It is
// separate from Diagnosis so that adding a field to the domain type does not
// silently change the encoding of rows already written -- a revision is a
// permanent record and its stored shape is part of that permanence.
type revisionDiagnosis struct {
	Code      string `json:"code"`
	Display   string `json:"display"`
	IsPrimary bool   `json:"is_primary"`
}

// InsertRevision appends one revision. There is deliberately no Update or
// Delete method: the database refuses both via trigger (migration 000004),
// so omitting them here is belt-and-braces rather than the only defence.
//
// The revision number is computed in SQL as max+1 over the note's existing
// revisions, inside the caller's transaction, so two concurrent amendments
// cannot both claim the same number -- and if they somehow raced past that,
// UNIQUE (note_id, revision) rejects the second.
func (r *Repository) InsertRevision(ctx context.Context, db dbtx, rev Revision) (Revision, error) {
	payload := make([]revisionDiagnosis, len(rev.Diagnoses))
	for i, d := range rev.Diagnoses {
		payload[i] = revisionDiagnosis{Code: d.Code, Display: d.Display, IsPrimary: d.IsPrimary}
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return Revision{}, fmt.Errorf("clinicalnotes: encode revision diagnoses: %w", err)
	}

	const q = `
		INSERT INTO clinical_note_revisions
			(note_id, revision, subjective, objective, assessment, plan, diagnoses,
			 change_type, amendment_reason, changed_by, changed_by_role)
		VALUES ($1,
			COALESCE((SELECT MAX(revision) FROM clinical_note_revisions WHERE note_id = $1), 0) + 1,
			$2, $3, $4, $5, $6::jsonb, $7, $8, $9, $10)
		RETURNING id, note_id, revision, subjective, objective, assessment, plan, diagnoses,
			change_type, amendment_reason, changed_by, changed_by_role, created_at`
	out, err := scanRevision(db.QueryRow(ctx, q, rev.NoteID, rev.Subjective, rev.Objective, rev.Assessment, rev.Plan,
		string(encoded), string(rev.ChangeType), rev.AmendmentReason, rev.ChangedBy, rev.ChangedByRole))
	if err != nil {
		return Revision{}, fmt.Errorf("clinicalnotes: insert revision: %w", err)
	}
	return out, nil
}

func scanRevision(row pgx.Row) (Revision, error) {
	var rev Revision
	var changeType string
	var raw []byte
	if err := row.Scan(&rev.ID, &rev.NoteID, &rev.Revision, &rev.Subjective, &rev.Objective, &rev.Assessment,
		&rev.Plan, &raw, &changeType, &rev.AmendmentReason, &rev.ChangedBy, &rev.ChangedByRole, &rev.CreatedAt); err != nil {
		return Revision{}, err
	}
	rev.ChangeType = ChangeType(changeType)

	var decoded []revisionDiagnosis
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &decoded); err != nil {
			return Revision{}, fmt.Errorf("clinicalnotes: decode revision diagnoses: %w", err)
		}
	}
	rev.Diagnoses = make([]Diagnosis, len(decoded))
	for i, d := range decoded {
		rev.Diagnoses[i] = Diagnosis{Code: d.Code, Display: d.Display, IsPrimary: d.IsPrimary, SortOrder: i}
	}
	return rev, nil
}

// ListRevisions returns a note's revision trail, oldest first. Oldest first
// is deliberate: this is read as a history, and revision 1 -- the text as
// signed -- is the row a reviewer wants at the top.
func (r *Repository) ListRevisions(ctx context.Context, db dbtx, noteID uuid.UUID) ([]Revision, error) {
	const q = `
		SELECT id, note_id, revision, subjective, objective, assessment, plan, diagnoses,
		       change_type, amendment_reason, changed_by, changed_by_role, created_at
		FROM clinical_note_revisions WHERE note_id = $1 ORDER BY revision ASC`
	rows, err := db.Query(ctx, q, noteID)
	if err != nil {
		return nil, fmt.Errorf("clinicalnotes: list revisions: %w", err)
	}
	defer rows.Close()

	out := make([]Revision, 0, 2)
	for rows.Next() {
		rev, err := scanRevision(rows)
		if err != nil {
			return nil, fmt.Errorf("clinicalnotes: scan revision: %w", err)
		}
		out = append(out, rev)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("clinicalnotes: list revisions rows: %w", err)
	}
	return out, nil
}

// ListFilter narrows the doctor's own note list.
type ListFilter struct {
	DoctorID uuid.UUID
	Status   Status // empty means both
	Page     int
	PerPage  int
}

// ListByDoctor returns a doctor's own notes, newest activity first, plus the
// total for pagination. Diagnoses are not loaded: the list screen renders
// titles and status, and pulling every note's codes to render a list is the
// N+1 that turns a 30ms endpoint into a 300ms one.
func (r *Repository) ListByDoctor(ctx context.Context, db dbtx, f ListFilter) ([]Note, int64, error) {
	countQ := `SELECT COUNT(*) FROM clinical_notes WHERE doctor_id = $1 AND deleted_at IS NULL AND ($2 = '' OR status = $2)`
	var total int64
	if err := db.QueryRow(ctx, countQ, f.DoctorID, string(f.Status)).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("clinicalnotes: count notes: %w", err)
	}
	if total == 0 {
		return nil, 0, nil
	}

	q := fmt.Sprintf(`
		SELECT %s FROM clinical_notes
		WHERE doctor_id = $1 AND deleted_at IS NULL AND ($2 = '' OR status = $2)
		ORDER BY updated_at DESC
		LIMIT $3 OFFSET $4`, noteColumns)
	rows, err := db.Query(ctx, q, f.DoctorID, string(f.Status), f.PerPage, (f.Page-1)*f.PerPage)
	if err != nil {
		return nil, 0, fmt.Errorf("clinicalnotes: list notes: %w", err)
	}
	defer rows.Close()

	out := make([]Note, 0, f.PerPage)
	for rows.Next() {
		n, err := scanNote(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("clinicalnotes: scan note: %w", err)
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("clinicalnotes: list notes rows: %w", err)
	}
	return out, total, nil
}

// SearchICD10 runs the diagnosis picker's query.
//
// Two matching strategies in one statement, because doctors use two:
//
//   - Code prefix. Typing "E11" must return every type 2 diabetes subtype in
//     numeric order. Backed by idx_icd10_code_prefix.
//   - Full-text over the rubric and the synonym list. Typing "sugar" must
//     find E11.9; typing "dengue haem" must find A91 before A90. Backed by
//     idx_icd10_search, a GIN index over a stored generated tsvector, so the
//     ranking runs on precomputed vectors rather than re-parsing 600 rubrics
//     per keystroke.
//
// tsQuery arrives already sanitised and prefix-suffixed by the service (see
// buildTSQuery); it is passed as a bound parameter and cast, never
// concatenated. codePrefix likewise contains only characters the service
// allowed through, so it cannot smuggle a LIKE wildcard.
//
// Ordering puts an exact code first, then a code prefix, then text relevance.
// A doctor who types a full code wants that code, not the rubric that happens
// to rank highest for its words.
func (r *Repository) SearchICD10(ctx context.Context, db dbtx, codePrefix, tsQuery string, limit int) ([]ICD10Code, error) {
	const q = `
		WITH args AS (
			SELECT $1::text AS prefix,
			       CASE WHEN $2 = '' THEN NULL ELSE to_tsquery('english', $2) END AS tsq
		)
		SELECT c.code, c.display, c.category
		FROM icd10_codes c, args a
		WHERE (a.prefix <> '' AND c.code LIKE a.prefix || '%')
		   OR (a.tsq IS NOT NULL AND c.search_vector @@ a.tsq)
		ORDER BY
			(a.prefix <> '' AND c.code = a.prefix) DESC,
			(a.prefix <> '' AND c.code LIKE a.prefix || '%') DESC,
			CASE WHEN a.tsq IS NULL THEN 0 ELSE ts_rank(c.search_vector, a.tsq) END DESC,
			length(c.code),
			c.code
		LIMIT $3`
	rows, err := db.Query(ctx, q, codePrefix, tsQuery, limit)
	if err != nil {
		return nil, fmt.Errorf("clinicalnotes: search icd10: %w", err)
	}
	defer rows.Close()

	out := make([]ICD10Code, 0, limit)
	for rows.Next() {
		var c ICD10Code
		if err := rows.Scan(&c.Code, &c.Display, &c.Category); err != nil {
			return nil, fmt.Errorf("clinicalnotes: scan icd10 code: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("clinicalnotes: search icd10 rows: %w", err)
	}
	return out, nil
}

// LookupICD10 fetches the reference rows for a set of codes, so the service
// can reject a diagnosis code that is not in the classification instead of
// storing whatever string a client sent.
func (r *Repository) LookupICD10(ctx context.Context, db dbtx, codes []string) (map[string]ICD10Code, error) {
	const q = `SELECT code, display, category FROM icd10_codes WHERE code = ANY($1)`
	rows, err := db.Query(ctx, q, codes)
	if err != nil {
		return nil, fmt.Errorf("clinicalnotes: lookup icd10: %w", err)
	}
	defer rows.Close()

	out := make(map[string]ICD10Code, len(codes))
	for rows.Next() {
		var c ICD10Code
		if err := rows.Scan(&c.Code, &c.Display, &c.Category); err != nil {
			return nil, fmt.Errorf("clinicalnotes: scan icd10 lookup: %w", err)
		}
		out[c.Code] = c
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("clinicalnotes: lookup icd10 rows: %w", err)
	}
	return out, nil
}

// compile-time assertions that database.Pool and pgx.Tx both satisfy dbtx.
var (
	_ dbtx = database.Pool(nil)
	_ dbtx = pgx.Tx(nil)
)
