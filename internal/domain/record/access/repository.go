package access

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"telemed/internal/platform/database"
)

// dbtx is the narrow slice of pgx.Tx / database.Pool a repository needs.
// Accepting this instead of a concrete type is what lets every method run
// either standalone against the pool or inside a caller's transaction.
type dbtx interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Repository is the SQL layer for treating_relationships, record_shares,
// document_access_log and the consumer's idempotency ledger. It never
// returns an *httpx.APIError (AGENT-BRIEF layering rule): callers translate
// database errors into the HTTP error taxonomy.
type Repository struct{}

// NewRepository constructs the (stateless) repository.
func NewRepository() *Repository { return &Repository{} }

// UpsertTreatingRelationship records that doctorID treated patientID during
// appointmentID. startedAt/endedAt are nil when this call does not know that
// half of the window yet (e.g. the started event supplies startedAt and nil
// endedAt). The COALESCE logic makes this safe to call in any order and any
// number of times: a redelivered or out-of-order event can only ever fill in
// a timestamp, never erase one that is already set.
func (r *Repository) UpsertTreatingRelationship(ctx context.Context, db dbtx, appointmentID, doctorID, patientID uuid.UUID, startedAt, endedAt *time.Time) error {
	const q = `
		INSERT INTO treating_relationships (appointment_id, doctor_id, patient_id, started_at, ended_at)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (appointment_id) DO UPDATE SET
			doctor_id  = EXCLUDED.doctor_id,
			patient_id = EXCLUDED.patient_id,
			started_at = COALESCE(treating_relationships.started_at, EXCLUDED.started_at),
			ended_at   = COALESCE(EXCLUDED.ended_at, treating_relationships.ended_at)`
	if _, err := db.Exec(ctx, q, appointmentID, doctorID, patientID, startedAt, endedAt); err != nil {
		return fmt.Errorf("access: upsert treating relationship: %w", err)
	}
	return nil
}

// HasActiveTreatingRelationship reports whether doctorID is treating, or has
// recently treated, patientID -- "recently" being TreatingAccessWindow.
//
// The predicate used to be a bare EXISTS on (doctor_id, patient_id) with no
// time bound at all, which meant started_at and ended_at were written by the
// consumer and then never read by any authorisation decision. One
// consultation, however brief, was a permanent unrevocable grant over that
// patient's entire vault including everything they uploaded afterwards
// (SECURITY-REVIEW F3). The bound below is the whole fix; see
// TreatingRelationship.GrantsAccessAt for why the window is what it is, and
// keep the two in step.
//
// COALESCE is used in both directions on purpose. A row carrying only
// started_at is a consultation in progress -- or one whose consultation.ended
// event was lost -- and expires a window after it started rather than never.
// A row carrying only ended_at (the ordering the outbox relay can produce
// under a backlog) is a concluded consultation and expires a window after it
// ended. A row carrying neither grants nothing: every comparison against
// NULL is NULL, so EXISTS is false, which is the correct default for a fact
// this service cannot establish.
//
// The method is named "Active" rather than "Has" so a future caller cannot
// read the old name and assume it still means "has ever".
func (r *Repository) HasActiveTreatingRelationship(ctx context.Context, db dbtx, doctorID, patientID uuid.UUID, now time.Time) (bool, error) {
	const q = `
		SELECT EXISTS(
			SELECT 1 FROM treating_relationships
			WHERE doctor_id = $1
			  AND patient_id = $2
			  AND COALESCE(started_at, ended_at) <= $3
			  AND COALESCE(ended_at, started_at) >  $4
		)`
	var exists bool
	notAfter := now.Add(treatingFutureGrace)
	notBefore := now.Add(-TreatingAccessWindow)
	if err := db.QueryRow(ctx, q, doctorID, patientID, notAfter, notBefore).Scan(&exists); err != nil {
		return false, fmt.Errorf("access: check treating relationship: %w", err)
	}
	return exists, nil
}

// ReachablePatients lists the patients whose vault doctorID may currently
// read: an active treating relationship under the same window
// HasActiveTreatingRelationship applies, or an unexpired, unrevoked share.
func (r *Repository) ReachablePatients(ctx context.Context, db dbtx, doctorID uuid.UUID, now time.Time) ([]uuid.UUID, error) {
	const q = `
		SELECT patient_id FROM treating_relationships
		WHERE doctor_id = $1
		  AND COALESCE(started_at, ended_at) <= $2
		  AND COALESCE(ended_at, started_at) >  $3
		UNION
		SELECT patient_id FROM record_shares
		WHERE doctor_id = $1 AND revoked_at IS NULL AND expires_at > $4`
	rows, err := db.Query(ctx, q, doctorID, now.Add(treatingFutureGrace), now.Add(-TreatingAccessWindow), now)
	if err != nil {
		return nil, fmt.Errorf("access: list reachable patients: %w", err)
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("access: scan reachable patient: %w", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("access: reachable patients rows: %w", err)
	}
	return out, nil
}

// TreatingRelationshipByAppointment fetches the relationship for one
// appointment, used to verify a doctor issuing a prescription actually owns
// that appointment and that the consultation has concluded.
func (r *Repository) TreatingRelationshipByAppointment(ctx context.Context, db dbtx, appointmentID uuid.UUID) (TreatingRelationship, bool, error) {
	const q = `
		SELECT id, appointment_id, doctor_id, patient_id, started_at, ended_at, created_at, updated_at
		FROM treating_relationships WHERE appointment_id = $1`
	var t TreatingRelationship
	err := db.QueryRow(ctx, q, appointmentID).Scan(
		&t.ID, &t.AppointmentID, &t.DoctorID, &t.PatientID, &t.StartedAt, &t.EndedAt, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return TreatingRelationship{}, false, nil
		}
		return TreatingRelationship{}, false, fmt.Errorf("access: get treating relationship: %w", err)
	}
	return t, true, nil
}

// ActiveShare returns the most permissive currently-active share from
// doctorID over patientID's vault, if one exists.
func (r *Repository) ActiveShare(ctx context.Context, db dbtx, doctorID, patientID uuid.UUID, now time.Time) (RecordShare, bool, error) {
	const q = `
		SELECT id, patient_id, doctor_id, granted_by, expires_at, revoked_at, created_at, updated_at, version
		FROM record_shares
		WHERE doctor_id = $1 AND patient_id = $2 AND revoked_at IS NULL AND expires_at > $3
		ORDER BY expires_at DESC LIMIT 1`
	var s RecordShare
	err := db.QueryRow(ctx, q, doctorID, patientID, now).Scan(
		&s.ID, &s.PatientID, &s.DoctorID, &s.GrantedBy, &s.ExpiresAt, &s.RevokedAt, &s.CreatedAt, &s.UpdatedAt, &s.Version)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return RecordShare{}, false, nil
		}
		return RecordShare{}, false, fmt.Errorf("access: get active share: %w", err)
	}
	return s, true, nil
}

// CreateShare inserts a new share grant.
func (r *Repository) CreateShare(ctx context.Context, db dbtx, s RecordShare) (RecordShare, error) {
	const q = `
		INSERT INTO record_shares (id, patient_id, doctor_id, granted_by, expires_at)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id, patient_id, doctor_id, granted_by, expires_at, revoked_at, created_at, updated_at, version`
	var out RecordShare
	err := db.QueryRow(ctx, q, s.ID, s.PatientID, s.DoctorID, s.GrantedBy, s.ExpiresAt).Scan(
		&out.ID, &out.PatientID, &out.DoctorID, &out.GrantedBy, &out.ExpiresAt, &out.RevokedAt, &out.CreatedAt, &out.UpdatedAt, &out.Version)
	if err != nil {
		return RecordShare{}, fmt.Errorf("access: create share: %w", err)
	}
	return out, nil
}

// GetShare fetches one share by id.
func (r *Repository) GetShare(ctx context.Context, db dbtx, id uuid.UUID) (RecordShare, bool, error) {
	const q = `
		SELECT id, patient_id, doctor_id, granted_by, expires_at, revoked_at, created_at, updated_at, version
		FROM record_shares WHERE id = $1`
	var s RecordShare
	err := db.QueryRow(ctx, q, id).Scan(
		&s.ID, &s.PatientID, &s.DoctorID, &s.GrantedBy, &s.ExpiresAt, &s.RevokedAt, &s.CreatedAt, &s.UpdatedAt, &s.Version)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return RecordShare{}, false, nil
		}
		return RecordShare{}, false, fmt.Errorf("access: get share: %w", err)
	}
	return s, true, nil
}

// ListSharesByPatient returns every share (active or not) a patient has ever
// granted, newest first.
func (r *Repository) ListSharesByPatient(ctx context.Context, db dbtx, patientID uuid.UUID) ([]RecordShare, error) {
	const q = `
		SELECT id, patient_id, doctor_id, granted_by, expires_at, revoked_at, created_at, updated_at, version
		FROM record_shares WHERE patient_id = $1 ORDER BY created_at DESC`
	rows, err := db.Query(ctx, q, patientID)
	if err != nil {
		return nil, fmt.Errorf("access: list shares: %w", err)
	}
	defer rows.Close()

	var out []RecordShare
	for rows.Next() {
		var s RecordShare
		if err := rows.Scan(&s.ID, &s.PatientID, &s.DoctorID, &s.GrantedBy, &s.ExpiresAt, &s.RevokedAt, &s.CreatedAt, &s.UpdatedAt, &s.Version); err != nil {
			return nil, fmt.Errorf("access: scan share: %w", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("access: list shares rows: %w", err)
	}
	return out, nil
}

// RevokeShare marks a share revoked, guarded by optimistic locking on
// version so a concurrent revoke and a concurrent grant-extension (were one
// ever added) cannot silently clobber each other.
func (r *Repository) RevokeShare(ctx context.Context, db dbtx, id uuid.UUID, expectedVersion int) (bool, error) {
	const q = `
		UPDATE record_shares SET revoked_at = NOW(), version = version + 1
		WHERE id = $1 AND version = $2 AND revoked_at IS NULL`
	tag, err := db.Exec(ctx, q, id, expectedVersion)
	if err != nil {
		return false, fmt.Errorf("access: revoke share: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// InsertAccessLog appends one row. There is deliberately no Update or Delete
// method on this repository -- the database itself refuses those statements
// via trigger (migration 000002), so omitting them here is belt-and-braces,
// not the only line of defence.
func (r *Repository) InsertAccessLog(ctx context.Context, db dbtx, e LogEntry) error {
	const q = `
		INSERT INTO document_access_log
			(resource_type, resource_id, owner_user_id, accessed_by, accessed_by_role, action, granted, reason, ip_address, user_agent)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NULLIF($9, '')::inet, $10)`
	_, err := db.Exec(ctx, q, string(e.ResourceType), e.ResourceID, e.OwnerUserID, e.AccessedBy,
		e.AccessedByRole, string(e.Action), e.Granted, string(e.Reason), e.IPAddress, e.UserAgent)
	if err != nil {
		return fmt.Errorf("access: insert access log: %w", err)
	}
	return nil
}

// ListAccessLog returns the access trail for one resource, newest first --
// what an incident responder pulls during a breach investigation.
func (r *Repository) ListAccessLog(ctx context.Context, db dbtx, resourceType ResourceType, resourceID uuid.UUID, limit int) ([]LogEntry, error) {
	const q = `
		SELECT resource_type, resource_id, owner_user_id, accessed_by, accessed_by_role, action, granted, reason,
		       COALESCE(host(ip_address), ''), user_agent
		FROM document_access_log
		WHERE resource_type = $1 AND resource_id = $2
		ORDER BY created_at DESC LIMIT $3`
	rows, err := db.Query(ctx, q, string(resourceType), resourceID, limit)
	if err != nil {
		return nil, fmt.Errorf("access: list access log: %w", err)
	}
	defer rows.Close()

	var out []LogEntry
	for rows.Next() {
		var e LogEntry
		var resType, action, reason string
		if err := rows.Scan(&resType, &e.ResourceID, &e.OwnerUserID, &e.AccessedBy, &e.AccessedByRole, &action, &e.Granted, &reason, &e.IPAddress, &e.UserAgent); err != nil {
			return nil, fmt.Errorf("access: scan access log: %w", err)
		}
		e.ResourceType = ResourceType(resType)
		e.Action = Action(action)
		e.Reason = Reason(reason)
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("access: list access log rows: %w", err)
	}
	return out, nil
}

// IsEventConsumed reports whether eventID has already been processed, the
// idempotency check every event handler must perform before doing anything
// else (AGENT-BRIEF: "consumers must be idempotent on envelope.ID").
func (r *Repository) IsEventConsumed(ctx context.Context, db dbtx, eventID uuid.UUID) (bool, error) {
	const q = `SELECT EXISTS(SELECT 1 FROM consumed_events WHERE event_id = $1)`
	var exists bool
	if err := db.QueryRow(ctx, q, eventID).Scan(&exists); err != nil {
		return false, fmt.Errorf("access: check consumed event: %w", err)
	}
	return exists, nil
}

// MarkEventConsumed records that eventID has been processed. Called in the
// same transaction as the side effect it guards, so a crash between the two
// is impossible -- either both commit or neither does.
func (r *Repository) MarkEventConsumed(ctx context.Context, db dbtx, eventID uuid.UUID, subject string) error {
	const q = `INSERT INTO consumed_events (event_id, subject) VALUES ($1, $2) ON CONFLICT DO NOTHING`
	if _, err := db.Exec(ctx, q, eventID, subject); err != nil {
		return fmt.Errorf("access: mark event consumed: %w", err)
	}
	return nil
}

// compile-time assertions that database.Pool and pgx.Tx both satisfy dbtx.
var (
	_ dbtx = database.Pool(nil)
	_ dbtx = pgx.Tx(nil)
)
