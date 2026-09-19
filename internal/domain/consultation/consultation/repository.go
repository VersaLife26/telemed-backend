package consultation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"telemed/internal/platform/database"
)

// ErrOptimisticLock is returned by UpdateConsultation when the row's version
// no longer matches what the caller read, meaning someone else changed the
// consultation first. Callers reload and retry or surface a conflict.
var ErrOptimisticLock = errors.New("consultation: version conflict")

// ErrDuplicateConsultation is returned by CreateConsultation when a row for
// the appointment already exists. The appointment.confirmed consumer treats
// this as success: at-least-once delivery means it will see the same event
// again, and the UNIQUE(appointment_id) constraint is what makes that safe to
// no-op on rather than needing a separate processed-events table.
var ErrDuplicateConsultation = errors.New("consultation: already exists for this appointment")

// queryer is the read-only subset both database.Pool and pgx.Tx satisfy. Read
// methods accept it so a caller inside an open transaction gets
// read-your-writes for free, while a caller outside one just passes the pool.
type queryer interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Repository is the pgx-backed implementation of the store interface
// service.go depends on. It contains SQL and nothing else: no business rules,
// no HTTP, per the platform's handler -> service -> repository layering.
type Repository struct{}

// NewRepository builds the repository. It is stateless -- every method takes
// its connection (pool or transaction) explicitly.
func NewRepository() *Repository { return &Repository{} }

var _ store = (*Repository)(nil)

// --- consultations -----------------------------------------------------

func (r *Repository) CreateConsultation(ctx context.Context, tx pgx.Tx, c *Consultation) error {
	const q = `
		INSERT INTO consultations (id, appointment_id, patient_id, doctor_id, room_name, status, scheduled_at, scheduled_end_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING created_at, updated_at, version`
	if c.ID == uuid.Nil {
		c.ID = uuid.New()
	}
	// The INSERT leaves recording_status to the column default ('none'), so
	// mirror that into the in-memory struct. Without it a caller that built a
	// Consultation literal and then called UpdateConsultation would write ''
	// and trip consultations_recording_status_check -- see the note there.
	if c.RecordingStatus == "" {
		c.RecordingStatus = RecordingNone
	}
	if c.ScheduledEndAt.IsZero() || !c.ScheduledEndAt.After(c.ScheduledAt) {
		c.ScheduledEndAt = c.ScheduledAt.Add(15 * time.Minute)
	}
	err := tx.QueryRow(ctx, q, c.ID, c.AppointmentID, c.PatientID, c.DoctorID, c.RoomName, string(c.Status), c.ScheduledAt, c.ScheduledEndAt).
		Scan(&c.CreatedAt, &c.UpdatedAt, &c.Version)
	if err != nil {
		if database.IsUniqueViolation(err) {
			return ErrDuplicateConsultation
		}
		return fmt.Errorf("consultation: create consultation: %w", err)
	}
	return nil
}

const consultationColumns = `id, appointment_id, patient_id, doctor_id, room_name, status, scheduled_at, scheduled_end_at,
	started_at, ended_at, duration_seconds, recording_url, recording_status, egress_id, end_reason,
	running_late_notified_at, early_join_offered_at, early_join_response, early_join_responded_at,
	created_at, updated_at, deleted_at, version`

func scanConsultation(row pgx.Row) (*Consultation, error) {
	var c Consultation
	var status, recordingStatus string
	err := row.Scan(
		&c.ID, &c.AppointmentID, &c.PatientID, &c.DoctorID, &c.RoomName, &status, &c.ScheduledAt, &c.ScheduledEndAt,
		&c.StartedAt, &c.EndedAt, &c.DurationSeconds, &c.RecordingURL, &recordingStatus, &c.EgressID, &c.EndReason,
		&c.RunningLateNotifiedAt, &c.EarlyJoinOfferedAt, &c.EarlyJoinResponse, &c.EarlyJoinRespondedAt,
		&c.CreatedAt, &c.UpdatedAt, &c.DeletedAt, &c.Version,
	)
	if err != nil {
		return nil, err
	}
	c.Status = Status(status)
	c.RecordingStatus = RecordingStatus(recordingStatus)
	return &c, nil
}

func (r *Repository) GetConsultation(ctx context.Context, q queryer, id uuid.UUID) (*Consultation, error) {
	row := q.QueryRow(ctx, `SELECT `+consultationColumns+` FROM consultations WHERE id = $1`, id)
	c, err := scanConsultation(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("consultation: get consultation %s: %w", id, err)
	}
	return c, nil
}

func (r *Repository) GetConsultationByAppointment(ctx context.Context, q queryer, appointmentID uuid.UUID) (*Consultation, error) {
	row := q.QueryRow(ctx, `SELECT `+consultationColumns+` FROM consultations WHERE appointment_id = $1`, appointmentID)
	c, err := scanConsultation(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("consultation: get consultation by appointment %s: %w", appointmentID, err)
	}
	return c, nil
}

func (r *Repository) GetConsultationByRoomName(ctx context.Context, q queryer, roomName string) (*Consultation, error) {
	row := q.QueryRow(ctx, `SELECT `+consultationColumns+` FROM consultations WHERE room_name = $1`, roomName)
	c, err := scanConsultation(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("consultation: get consultation by room %s: %w", roomName, err)
	}
	return c, nil
}

// UpdateConsultation persists every mutable field of c using c.Version as the
// optimistic-lock guard, then advances c.Version and c.UpdatedAt to match what
// was written. Every status transition in service.go goes through this single
// method so there is exactly one place that can race on a consultation row.
func (r *Repository) UpdateConsultation(ctx context.Context, tx pgx.Tx, c *Consultation) error {
	const q = `
		UPDATE consultations SET
			status = $1, started_at = $2, ended_at = $3, duration_seconds = $4,
			recording_url = $5, recording_status = $6, egress_id = $7, end_reason = $8,
			deleted_at = $9, updated_at = NOW(), version = version + 1
		WHERE id = $10 AND version = $11
		RETURNING updated_at, version`
	// recording_status is NOT NULL with a CHECK that does not admit the empty
	// string, and '' is never a meaningful value -- 'none' is. Normalising
	// here means a caller holding a struct that never had the field set gets
	// the column default rather than a 23514 from deep inside an UPDATE.
	recordingStatus := c.RecordingStatus
	if recordingStatus == "" {
		recordingStatus = RecordingNone
	}
	err := tx.QueryRow(ctx, q,
		string(c.Status), c.StartedAt, c.EndedAt, c.DurationSeconds,
		c.RecordingURL, string(recordingStatus), c.EgressID, c.EndReason,
		c.DeletedAt, c.ID, c.Version,
	).Scan(&c.UpdatedAt, &c.Version)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrOptimisticLock
		}
		return fmt.Errorf("consultation: update consultation %s: %w", c.ID, err)
	}
	return nil
}

// UpdateConsultationScheduledAt moves the visit window when a paid appointment
// is rescheduled. Active or ended consultations are left alone: a live call
// must not jump clocks under the participants.
func (r *Repository) UpdateConsultationScheduledAt(ctx context.Context, tx pgx.Tx, appointmentID uuid.UUID, startAt, endAt time.Time) error {
	if !endAt.After(startAt) {
		endAt = startAt.Add(15 * time.Minute)
	}
	const q = `
		UPDATE consultations
		SET scheduled_at = $2,
		    scheduled_end_at = $3,
		    running_late_notified_at = NULL,
		    early_join_offered_at = NULL,
		    early_join_response = NULL,
		    early_join_responded_at = NULL,
		    updated_at = NOW(),
		    version = version + 1
		WHERE appointment_id = $1
		  AND status IN ('scheduled', 'waiting')
		  AND deleted_at IS NULL`
	if _, err := tx.Exec(ctx, q, appointmentID, startAt.UTC(), endAt.UTC()); err != nil {
		return fmt.Errorf("consultation: update scheduled window %s: %w", appointmentID, err)
	}
	return nil
}

// ListOverrunActive returns live consults whose booked end has passed and that
// have not yet triggered a running-late notice.
func (r *Repository) ListOverrunActive(ctx context.Context, q queryer, now time.Time, limit int) ([]*Consultation, error) {
	if limit <= 0 {
		limit = 50
	}
	const sql = `
		SELECT ` + consultationColumns + `
		FROM consultations
		WHERE status = 'active'
		  AND deleted_at IS NULL
		  AND running_late_notified_at IS NULL
		  AND scheduled_end_at < $1
		ORDER BY scheduled_end_at ASC
		LIMIT $2`
	rows, err := q.Query(ctx, sql, now.UTC(), limit)
	if err != nil {
		return nil, fmt.Errorf("consultation: list overrun active: %w", err)
	}
	defer rows.Close()
	var out []*Consultation
	for rows.Next() {
		c, err := scanConsultation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// FindNextUpcomingForDoctor returns the doctor's next not-yet-started consult
// after the active one, if any.
func (r *Repository) FindNextUpcomingForDoctor(ctx context.Context, q queryer, doctorID uuid.UUID, afterScheduledAt time.Time) (*Consultation, error) {
	const sql = `
		SELECT ` + consultationColumns + `
		FROM consultations
		WHERE doctor_id = $1
		  AND deleted_at IS NULL
		  AND status IN ('scheduled', 'waiting')
		  AND scheduled_at > $2
		ORDER BY scheduled_at ASC
		LIMIT 1`
	c, err := scanConsultation(q.QueryRow(ctx, sql, doctorID, afterScheduledAt.UTC()))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("consultation: find next upcoming: %w", err)
	}
	return c, nil
}

// ClaimRunningLateNotified stamps the active consult so a second replica or
// tick cannot double-notify the next patient.
func (r *Repository) ClaimRunningLateNotified(ctx context.Context, tx pgx.Tx, consultationID uuid.UUID, at time.Time) (bool, error) {
	const q = `
		UPDATE consultations
		SET running_late_notified_at = $2, updated_at = NOW(), version = version + 1
		WHERE id = $1
		  AND status = 'active'
		  AND deleted_at IS NULL
		  AND running_late_notified_at IS NULL`
	tag, err := tx.Exec(ctx, q, consultationID, at.UTC())
	if err != nil {
		return false, fmt.Errorf("consultation: claim running late %s: %w", consultationID, err)
	}
	return tag.RowsAffected() == 1, nil
}

// FindActiveForDoctor returns the doctor's live consult, if any. Ready-for-next
// uses this so a doctor still in a call cannot ping the next patient.
func (r *Repository) FindActiveForDoctor(ctx context.Context, q queryer, doctorID uuid.UUID) (*Consultation, error) {
	const sql = `
		SELECT ` + consultationColumns + `
		FROM consultations
		WHERE doctor_id = $1
		  AND status = 'active'
		  AND deleted_at IS NULL
		LIMIT 1`
	c, err := scanConsultation(q.QueryRow(ctx, sql, doctorID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("consultation: find active for doctor: %w", err)
	}
	return c, nil
}

// FindLatestTerminalForDoctor is the visit the doctor most recently closed,
// used when they tap ready-for-next from the queue without an appointment id.
func (r *Repository) FindLatestTerminalForDoctor(ctx context.Context, q queryer, doctorID uuid.UUID) (*Consultation, error) {
	const sql = `
		SELECT ` + consultationColumns + `
		FROM consultations
		WHERE doctor_id = $1
		  AND status IN ('ended', 'abandoned', 'failed')
		  AND deleted_at IS NULL
		ORDER BY COALESCE(ended_at, updated_at) DESC
		LIMIT 1`
	c, err := scanConsultation(q.QueryRow(ctx, sql, doctorID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("consultation: find latest terminal for doctor: %w", err)
	}
	return c, nil
}

// ClaimEarlyJoinOffered stamps the next consult so a second tap cannot spam
// the same patient. It does not change scheduled_at.
func (r *Repository) ClaimEarlyJoinOffered(ctx context.Context, tx pgx.Tx, consultationID uuid.UUID, at time.Time) (bool, error) {
	const q = `
		UPDATE consultations
		SET early_join_offered_at = $2, updated_at = NOW(), version = version + 1
		WHERE id = $1
		  AND status IN ('scheduled', 'waiting')
		  AND deleted_at IS NULL
		  AND early_join_offered_at IS NULL`
	tag, err := tx.Exec(ctx, q, consultationID, at.UTC())
	if err != nil {
		return false, fmt.Errorf("consultation: claim early join %s: %w", consultationID, err)
	}
	return tag.RowsAffected() == 1, nil
}

// SetEarlyJoinResponse records accepted or declined once. scheduled_at is
// left alone either way.
func (r *Repository) SetEarlyJoinResponse(ctx context.Context, tx pgx.Tx, consultationID uuid.UUID, response string, at time.Time) (bool, error) {
	const q = `
		UPDATE consultations
		SET early_join_response = $2,
		    early_join_responded_at = $3,
		    updated_at = NOW(),
		    version = version + 1
		WHERE id = $1
		  AND early_join_offered_at IS NOT NULL
		  AND early_join_response IS NULL
		  AND deleted_at IS NULL`
	tag, err := tx.Exec(ctx, q, consultationID, response, at.UTC())
	if err != nil {
		return false, fmt.Errorf("consultation: set early join response %s: %w", consultationID, err)
	}
	return tag.RowsAffected() == 1, nil
}

// ListScheduledPastJoinCutoff returns consults still waiting for a first
// patient join after the booked slot ended. Waiting/active rows are excluded
// so someone who arrived during the slot is not closed out.
func (r *Repository) ListScheduledPastJoinCutoff(ctx context.Context, q queryer, cutoff time.Time, limit int) ([]*Consultation, error) {
	if limit <= 0 {
		limit = 50
	}
	const sql = `
		SELECT ` + consultationColumns + `
		FROM consultations
		WHERE status = 'scheduled'
		  AND deleted_at IS NULL
		  AND scheduled_end_at <= $1
		ORDER BY scheduled_end_at ASC
		LIMIT $2`
	rows, err := q.Query(ctx, sql, cutoff.UTC(), limit)
	if err != nil {
		return nil, fmt.Errorf("consultation: list scheduled past join cutoff: %w", err)
	}
	defer rows.Close()
	var out []*Consultation
	for rows.Next() {
		c, err := scanConsultation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// --- participants --------------------------------------------------------

// UpsertParticipantJoin records a join. A second join by the same identity is
// a reconnect: reconnect_count increments and left_at is cleared, which is
// exactly the signal RUNBOOK.md's "why was this call choppy" query relies on.
func (r *Repository) UpsertParticipantJoin(ctx context.Context, tx pgx.Tx, consultationID uuid.UUID, identity string, role ParticipantRole, at time.Time) error {
	const q = `
		INSERT INTO consultation_participants (id, consultation_id, identity, role, joined_at, reconnect_count)
		VALUES ($1, $2, $3, $4, $5, 0)
		ON CONFLICT (consultation_id, identity) DO UPDATE SET
			joined_at = $5,
			left_at = NULL,
			reconnect_count = consultation_participants.reconnect_count +
				CASE WHEN consultation_participants.joined_at IS NOT NULL THEN 1 ELSE 0 END,
			updated_at = NOW()`
	_, err := tx.Exec(ctx, q, uuid.New(), consultationID, identity, string(role), at)
	if err != nil {
		return fmt.Errorf("consultation: upsert participant join %s/%s: %w", consultationID, identity, err)
	}
	return nil
}

func (r *Repository) MarkParticipantLeft(ctx context.Context, tx pgx.Tx, consultationID uuid.UUID, identity string, at time.Time) error {
	const q = `UPDATE consultation_participants SET left_at = $3, updated_at = NOW()
		WHERE consultation_id = $1 AND identity = $2`
	_, err := tx.Exec(ctx, q, consultationID, identity, at)
	if err != nil {
		return fmt.Errorf("consultation: mark participant left %s/%s: %w", consultationID, identity, err)
	}
	return nil
}

func (r *Repository) ListParticipants(ctx context.Context, q queryer, consultationID uuid.UUID) ([]Participant, error) {
	rows, err := q.Query(ctx, `
		SELECT id, consultation_id, identity, role, joined_at, left_at, reconnect_count
		FROM consultation_participants WHERE consultation_id = $1 ORDER BY joined_at NULLS LAST`, consultationID)
	if err != nil {
		return nil, fmt.Errorf("consultation: list participants %s: %w", consultationID, err)
	}
	defer rows.Close()

	var out []Participant
	for rows.Next() {
		var p Participant
		var role string
		if err := rows.Scan(&p.ID, &p.ConsultationID, &p.Identity, &role, &p.JoinedAt, &p.LeftAt, &p.ReconnectCount); err != nil {
			return nil, fmt.Errorf("consultation: scan participant: %w", err)
		}
		p.Role = ParticipantRole(role)
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("consultation: list participants rows %s: %w", consultationID, err)
	}
	return out, nil
}

// --- consents --------------------------------------------------------------

func (r *Repository) SaveConsent(ctx context.Context, tx pgx.Tx, c Consent) error {
	const q = `
		INSERT INTO consultation_consents (id, consultation_id, user_id, consent_type, granted, granted_at, ip_address, user_agent)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`
	id := c.ID
	if id == uuid.Nil {
		id = uuid.New()
	}
	grantedAt := c.GrantedAt
	if grantedAt.IsZero() {
		grantedAt = time.Now().UTC()
	}
	_, err := tx.Exec(ctx, q, id, c.ConsultationID, c.UserID, string(c.Type), c.Granted, grantedAt, c.IPAddress, c.UserAgent)
	if err != nil {
		return fmt.Errorf("consultation: save consent %s/%s: %w", c.ConsultationID, c.UserID, err)
	}
	return nil
}

// RecordingConsentStatus returns, for every user who has ever submitted a
// recording consent decision on this consultation, whether their MOST RECENT
// decision was "granted". A user absent from the map has never decided.
func (r *Repository) RecordingConsentStatus(ctx context.Context, q queryer, consultationID uuid.UUID) (map[uuid.UUID]bool, error) {
	const query = `
		SELECT DISTINCT ON (user_id) user_id, granted
		FROM consultation_consents
		WHERE consultation_id = $1 AND consent_type = 'recording'
		ORDER BY user_id, granted_at DESC`
	rows, err := q.Query(ctx, query, consultationID)
	if err != nil {
		return nil, fmt.Errorf("consultation: recording consent status %s: %w", consultationID, err)
	}
	defer rows.Close()

	out := map[uuid.UUID]bool{}
	for rows.Next() {
		var userID uuid.UUID
		var granted bool
		if err := rows.Scan(&userID, &granted); err != nil {
			return nil, fmt.Errorf("consultation: scan consent status: %w", err)
		}
		out[userID] = granted
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("consultation: consent status rows %s: %w", consultationID, err)
	}
	return out, nil
}

// --- events (append-only timeline) -----------------------------------------

func (r *Repository) RecordEvent(ctx context.Context, tx pgx.Tx, e Event) error {
	meta := e.Metadata
	if meta == nil {
		meta = map[string]any{}
	}
	body, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("consultation: marshal event metadata: %w", err)
	}
	occurredAt := e.OccurredAt
	if occurredAt.IsZero() {
		occurredAt = time.Now().UTC()
	}
	const q = `
		INSERT INTO consultation_events (consultation_id, event_type, actor_identity, metadata, occurred_at)
		VALUES ($1, $2, $3, $4, $5)`
	if _, err := tx.Exec(ctx, q, e.ConsultationID, e.Type, nullIfEmpty(e.ActorIdentity), body, occurredAt); err != nil {
		return fmt.Errorf("consultation: record event %s/%s: %w", e.ConsultationID, e.Type, err)
	}
	return nil
}

func (r *Repository) RecentQualityEvents(ctx context.Context, q queryer, consultationID uuid.UUID, identity string, limit int) ([]Event, error) {
	const query = `
		SELECT id, consultation_id, event_type, actor_identity, metadata, occurred_at
		FROM consultation_events
		WHERE consultation_id = $1 AND actor_identity = $2 AND event_type = $3
		ORDER BY occurred_at DESC
		LIMIT $4`
	rows, err := q.Query(ctx, query, consultationID, identity, EventQualitySample, limit)
	if err != nil {
		return nil, fmt.Errorf("consultation: recent quality events %s/%s: %w", consultationID, identity, err)
	}
	defer rows.Close()

	var out []Event
	for rows.Next() {
		var e Event
		var actor *string
		var meta []byte
		if err := rows.Scan(&e.ID, &e.ConsultationID, &e.Type, &actor, &meta, &e.OccurredAt); err != nil {
			return nil, fmt.Errorf("consultation: scan quality event: %w", err)
		}
		if actor != nil {
			e.ActorIdentity = *actor
		}
		if len(meta) > 0 {
			if err := json.Unmarshal(meta, &e.Metadata); err != nil {
				return nil, fmt.Errorf("consultation: unmarshal event metadata: %w", err)
			}
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("consultation: quality events rows %s: %w", consultationID, err)
	}
	return out, nil
}

// --- waiting room ------------------------------------------------------

func (r *Repository) UpsertWaitingRoomEntry(ctx context.Context, tx pgx.Tx, e WaitingRoomEntry) error {
	const q = `
		INSERT INTO waiting_room_entries (id, consultation_id, doctor_id, patient_id, entered_at, status)
		VALUES ($1, $2, $3, $4, $5, 'waiting')
		ON CONFLICT (consultation_id) DO UPDATE SET
			entered_at = $5, status = 'waiting', admitted_at = NULL, left_at = NULL, updated_at = NOW()`
	id := e.ID
	if id == uuid.Nil {
		id = uuid.New()
	}
	_, err := tx.Exec(ctx, q, id, e.ConsultationID, e.DoctorID, e.PatientID, e.EnteredAt)
	if err != nil {
		return fmt.Errorf("consultation: upsert waiting room entry %s: %w", e.ConsultationID, err)
	}
	return nil
}

func (r *Repository) GetWaitingRoomEntry(ctx context.Context, q queryer, consultationID uuid.UUID) (*WaitingRoomEntry, error) {
	row := q.QueryRow(ctx, `
		SELECT id, consultation_id, doctor_id, patient_id, entered_at, status, admitted_at, left_at
		FROM waiting_room_entries WHERE consultation_id = $1`, consultationID)
	var e WaitingRoomEntry
	var status string
	err := row.Scan(&e.ID, &e.ConsultationID, &e.DoctorID, &e.PatientID, &e.EnteredAt, &status, &e.AdmittedAt, &e.LeftAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("consultation: get waiting room entry %s: %w", consultationID, err)
	}
	e.Status = WaitingRoomStatus(status)
	return &e, nil
}

func (r *Repository) MarkWaitingRoomAdmitted(ctx context.Context, tx pgx.Tx, consultationID uuid.UUID, at time.Time) error {
	const q = `UPDATE waiting_room_entries SET status = 'admitted', admitted_at = $2, updated_at = NOW()
		WHERE consultation_id = $1`
	_, err := tx.Exec(ctx, q, consultationID, at)
	if err != nil {
		return fmt.Errorf("consultation: mark waiting room admitted %s: %w", consultationID, err)
	}
	return nil
}

func (r *Repository) MarkWaitingRoomLeft(ctx context.Context, tx pgx.Tx, consultationID uuid.UUID, at time.Time, status WaitingRoomStatus) error {
	const q = `UPDATE waiting_room_entries SET status = $3, left_at = $2, updated_at = NOW()
		WHERE consultation_id = $1 AND status = 'waiting'`
	_, err := tx.Exec(ctx, q, consultationID, at, string(status))
	if err != nil {
		return fmt.Errorf("consultation: mark waiting room left %s: %w", consultationID, err)
	}
	return nil
}

// CountWaitingAhead is the Postgres source of truth for queue position -- the
// Redis sorted set (ADR-007 in the platform) is the fast path, this count is
// what a Redis flush falls back to. before is normally the caller's own
// entered_at; the comparison is strict so the caller never counts itself.
func (r *Repository) CountWaitingAhead(ctx context.Context, q queryer, doctorID uuid.UUID, before time.Time) (int64, error) {
	var n int64
	err := q.QueryRow(ctx, `
		SELECT COUNT(*) FROM waiting_room_entries
		WHERE doctor_id = $1 AND status = 'waiting' AND entered_at < $2`, doctorID, before).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("consultation: count waiting ahead %s: %w", doctorID, err)
	}
	return n, nil
}

// --- analytics ---------------------------------------------------------

// AverageDurationSeconds derives the doctor's expected consultation length
// from their own recent history rather than a hardcoded guess, per the
// design brief. ok is false when the doctor has no completed consultations
// yet, in which case the caller falls back to a configured default.
func (r *Repository) AverageDurationSeconds(ctx context.Context, q queryer, doctorID uuid.UUID, sampleSize int) (avgSeconds float64, ok bool, err error) {
	const query = `
		SELECT AVG(duration_seconds) FROM (
			SELECT duration_seconds FROM consultations
			WHERE doctor_id = $1 AND status = 'ended' AND duration_seconds IS NOT NULL
			ORDER BY ended_at DESC LIMIT $2
		) recent`
	var avg *float64
	if err := q.QueryRow(ctx, query, doctorID, sampleSize).Scan(&avg); err != nil {
		return 0, false, fmt.Errorf("consultation: average duration %s: %w", doctorID, err)
	}
	if avg == nil {
		return 0, false, nil
	}
	return *avg, true, nil
}

// --- webhook idempotency ------------------------------------------------

// ClaimWebhookEvent inserts a receipt for eventID and reports whether this
// call was the one that created it. A false return means the event was
// already processed and the caller should ack without repeating side effects
// -- this is what makes a LiveKit webhook retry a safe no-op instead of a
// double-recorded egress_ended.
func (r *Repository) ClaimWebhookEvent(ctx context.Context, tx pgx.Tx, eventID, eventType string) (bool, error) {
	const q = `
		INSERT INTO consultation_webhook_receipts (event_id, event_type)
		VALUES ($1, $2)
		ON CONFLICT (event_id) DO NOTHING`
	tag, err := tx.Exec(ctx, q, eventID, eventType)
	if err != nil {
		return false, fmt.Errorf("consultation: claim webhook event %s: %w", eventID, err)
	}
	return tag.RowsAffected() == 1, nil
}

func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// --- stale sweep ------------------------------------------------------------

// ListStaleActive returns consultations still in 'active' whose local timeline
// has been silent since quietSince.
//
// "Silent" is the latest of: when the call went active, the last participant
// join or leave we recorded, and the last entry on the consultation's own
// event timeline -- which for a live call is the periodic quality sample, the
// only heartbeat a two-party call in progress produces at all.
//
// This is a CANDIDATE query. A healthy long consultation whose clients stopped
// posting quality samples looks identical here to an abandoned one; only the
// video provider can tell them apart, and Service.sweepOne asks it before
// ending anything.
func (r *Repository) ListStaleActive(ctx context.Context, q queryer, quietSince time.Time, limit int) ([]StaleCandidate, error) {
	const query = `
		SELECT c.id,
		       c.room_name,
		       c.started_at,
		       GREATEST(
		           c.started_at,
		           COALESCE(p.last_seen, c.started_at),
		           COALESCE(e.last_event, c.started_at)
		       ) AS last_activity
		FROM consultations c
		LEFT JOIN LATERAL (
		    SELECT GREATEST(MAX(cp.joined_at), MAX(cp.left_at)) AS last_seen
		    FROM consultation_participants cp
		    WHERE cp.consultation_id = c.id
		) p ON TRUE
		LEFT JOIN LATERAL (
		    SELECT MAX(ce.occurred_at) AS last_event
		    FROM consultation_events ce
		    WHERE ce.consultation_id = c.id
		) e ON TRUE
		WHERE c.status = 'active'
		  AND c.deleted_at IS NULL
		  AND c.started_at IS NOT NULL
		  AND c.started_at < $1
		ORDER BY c.started_at
		LIMIT $2`

	rows, err := q.Query(ctx, query, quietSince, limit)
	if err != nil {
		return nil, fmt.Errorf("consultation: list stale active: %w", err)
	}
	defer rows.Close()

	out := make([]StaleCandidate, 0, limit)
	for rows.Next() {
		var c StaleCandidate
		var startedAt, lastActivity *time.Time
		if err := rows.Scan(&c.ID, &c.RoomName, &startedAt, &lastActivity); err != nil {
			return nil, fmt.Errorf("consultation: scan stale active: %w", err)
		}
		if startedAt != nil {
			c.StartedAt = startedAt.UTC()
		}
		if lastActivity != nil {
			c.LastActivity = lastActivity.UTC()
		} else {
			c.LastActivity = c.StartedAt
		}
		// started_at is bounded by the query; last_activity is not, because a
		// consultation that is quiet in the join table can still be posting
		// quality samples every few seconds.
		if c.LastActivity.After(quietSince) {
			continue
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("consultation: iterate stale active: %w", err)
	}
	return out, nil
}

// --- consultation_messages ---------------------------------------------

func (r *Repository) InsertMessage(ctx context.Context, q queryer, msg *ChatMessage) error {
	if msg.ID == uuid.Nil {
		msg.ID = uuid.New()
	}
	if msg.CreatedAt.IsZero() {
		msg.CreatedAt = time.Now().UTC()
	}
	if len(msg.Metadata) == 0 {
		msg.Metadata = json.RawMessage("{}")
	}

	const sql = `
		INSERT INTO consultation_messages (id, consultation_id, sender_id, sender_role, sender_name, content, metadata, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING id, created_at`

	err := q.QueryRow(ctx, sql,
		msg.ID,
		msg.ConsultationID,
		msg.SenderID,
		string(msg.SenderRole),
		msg.SenderName,
		msg.Content,
		msg.Metadata,
		msg.CreatedAt,
	).Scan(&msg.ID, &msg.CreatedAt)
	if err != nil {
		return fmt.Errorf("consultation: insert message: %w", err)
	}
	return nil
}

func (r *Repository) ListMessages(ctx context.Context, q queryer, consultationID uuid.UUID, since time.Time, limit int) ([]ChatMessage, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}

	var (
		rows pgx.Rows
		err  error
	)
	if since.IsZero() {
		const sql = `
			SELECT id, consultation_id, sender_id, sender_role, sender_name, content, metadata, created_at
			FROM consultation_messages
			WHERE consultation_id = $1
			ORDER BY created_at ASC
			LIMIT $2`
		rows, err = q.Query(ctx, sql, consultationID, limit)
	} else {
		const sql = `
			SELECT id, consultation_id, sender_id, sender_role, sender_name, content, metadata, created_at
			FROM consultation_messages
			WHERE consultation_id = $1 AND created_at > $2
			ORDER BY created_at ASC
			LIMIT $3`
		rows, err = q.Query(ctx, sql, consultationID, since, limit)
	}
	if err != nil {
		return nil, fmt.Errorf("consultation: list messages: %w", err)
	}
	defer rows.Close()

	out := make([]ChatMessage, 0)
	for rows.Next() {
		var m ChatMessage
		var role string
		if err := rows.Scan(
			&m.ID,
			&m.ConsultationID,
			&m.SenderID,
			&role,
			&m.SenderName,
			&m.Content,
			&m.Metadata,
			&m.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("consultation: scan message: %w", err)
		}
		m.SenderRole = ParticipantRole(role)
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("consultation: iterate messages: %w", err)
	}
	return out, nil
}
