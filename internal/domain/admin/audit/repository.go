package audit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"telemed/internal/platform/database"
)

// Repository is SQL only, per the platform layering rule: it takes a
// database.Pool and returns domain values, never an *httpx.APIError.
type Repository struct {
	pool database.Pool
}

// NewRepository builds the audit repository.
func NewRepository(pool database.Pool) *Repository { return &Repository{pool: pool} }

// InsertParams carries everything the trigger-computed columns (prev_hash,
// row_hash, created_at) do not need from the caller.
type InsertParams struct {
	ActorID      uuid.UUID
	ActorRole    string
	Action       string
	ResourceType string
	ResourceID   string
	OldValue     any
	NewValue     any
	IP           string
	UserAgent    string
	RequestID    string
}

// Insert writes one row. The hash chain columns are computed entirely by the
// BEFORE INSERT trigger (trg_audit_logs_chain); this function never sets them.
func (r *Repository) Insert(ctx context.Context, p InsertParams) (Entry, error) {
	oldRaw, err := marshalNullable(p.OldValue)
	if err != nil {
		return Entry{}, fmt.Errorf("audit: marshal old_value: %w", err)
	}
	newRaw, err := marshalNullable(p.NewValue)
	if err != nil {
		return Entry{}, fmt.Errorf("audit: marshal new_value: %w", err)
	}

	var actorID any
	if p.ActorID != uuid.Nil {
		actorID = p.ActorID
	}

	// A malformed or empty IP must never fail the write -- the audit trail
	// recording is far more important than the IP column being populated.
	var ip any
	if parsed := net.ParseIP(strings.TrimSpace(p.IP)); parsed != nil {
		ip = parsed.String()
	}

	const q = `
		INSERT INTO audit_logs
			(actor_id, actor_role, action, resource_type, resource_id, old_value, new_value, ip, user_agent, request_id)
		VALUES ($1, $2, $3, $4, NULLIF($5, ''), $6, $7, $8, NULLIF($9, ''), NULLIF($10, ''))
		RETURNING id, created_at, prev_hash, row_hash`

	var e Entry
	e.ActorID = p.ActorID
	e.ActorRole = p.ActorRole
	e.Action = p.Action
	e.ResourceType = p.ResourceType
	e.ResourceID = p.ResourceID
	e.OldValue = oldRaw
	e.NewValue = newRaw
	e.IP = p.IP
	e.UserAgent = p.UserAgent
	e.RequestID = p.RequestID

	row := r.pool.QueryRow(ctx, q,
		actorID, p.ActorRole, p.Action, p.ResourceType, p.ResourceID,
		oldRaw, newRaw, ip, p.UserAgent, p.RequestID)
	if err := row.Scan(&e.ID, &e.CreatedAt, &e.PrevHash, &e.RowHash); err != nil {
		return Entry{}, fmt.Errorf("audit: insert: %w", err)
	}
	return e, nil
}

func marshalNullable(v any) (json.RawMessage, error) {
	if v == nil {
		return nil, nil
	}
	if raw, ok := v.(json.RawMessage); ok {
		return raw, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return b, nil
}

// List returns a page of entries matching f, newest first, plus the total
// matching row count for pagination metadata.
func (r *Repository) List(ctx context.Context, f ListFilter) ([]Entry, int64, error) {
	where, args := buildFilter(f)

	perPage := f.PerPage
	if perPage <= 0 {
		perPage = 20
	}
	page := f.Page
	if page <= 0 {
		page = 1
	}
	offset := (page - 1) * perPage

	countQ := "SELECT COUNT(*) FROM audit_logs" + where
	var total int64
	if err := r.pool.QueryRow(ctx, countQ, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("audit: count: %w", err)
	}

	listQ := fmt.Sprintf(`
		SELECT id, actor_id, actor_role, action, resource_type, resource_id,
		       old_value, new_value, `+ipColumn+`, user_agent, request_id, created_at,
		       prev_hash, row_hash
		FROM audit_logs%s
		ORDER BY id DESC
		LIMIT $%d OFFSET $%d`, where, len(args)+1, len(args)+2)

	rows, err := r.pool.Query(ctx, listQ, append(args, perPage, offset)...)
	if err != nil {
		return nil, 0, fmt.Errorf("audit: list: %w", err)
	}
	defer rows.Close()

	entries, err := scanEntries(rows)
	if err != nil {
		return nil, 0, err
	}
	return entries, total, nil
}

// CountMatching returns how many entries match f. Used by the export
// preflight, which has to know the size of a result set before it commits to
// streaming one (see Service.PrepareExport).
func (r *Repository) CountMatching(ctx context.Context, f ListFilter) (int64, error) {
	where, args := buildFilter(f)
	var total int64
	if err := r.pool.QueryRow(ctx, "SELECT COUNT(*) FROM audit_logs"+where, args...).Scan(&total); err != nil {
		return 0, fmt.Errorf("audit: count matching: %w", err)
	}
	return total, nil
}

// StreamExport calls emit for every entry matching f, ordered oldest first and
// capped at limit rows, without materialising the whole result set -- the CSV
// export can cover a wide date range and must not require loading it all into
// memory at once.
//
// The LIMIT is not optional. Before it existed this query had no bound of any
// kind, so a single GET returned every audit row on the platform (security
// review F6).
func (r *Repository) StreamExport(ctx context.Context, f ListFilter, limit int, emit func(Entry) error) error {
	where, args := buildFilter(f)
	args = append(args, limit)
	q := fmt.Sprintf(`
		SELECT id, actor_id, actor_role, action, resource_type, resource_id,
		       old_value, new_value, `+ipColumn+`, user_agent, request_id, created_at,
		       prev_hash, row_hash
		FROM audit_logs%s
		ORDER BY id ASC
		LIMIT $%d`, where, len(args))

	rows, err := r.pool.Query(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("audit: export query: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			return err
		}
		if err := emit(e); err != nil {
			return err
		}
	}
	return rows.Err()
}

func buildFilter(f ListFilter) (clause string, args []any) {
	var clauses []string

	if f.ActorID != uuid.Nil {
		args = append(args, f.ActorID)
		clauses = append(clauses, fmt.Sprintf("actor_id = $%d", len(args)))
	}
	if f.Action != "" {
		args = append(args, f.Action)
		clauses = append(clauses, fmt.Sprintf("action = $%d", len(args)))
	}
	if f.ResourceType != "" {
		args = append(args, f.ResourceType)
		clauses = append(clauses, fmt.Sprintf("resource_type = $%d", len(args)))
	}
	if f.ResourceID != "" {
		args = append(args, f.ResourceID)
		clauses = append(clauses, fmt.Sprintf("resource_id = $%d", len(args)))
	}
	if !f.From.IsZero() {
		args = append(args, f.From)
		clauses = append(clauses, fmt.Sprintf("created_at >= $%d", len(args)))
	}
	if !f.To.IsZero() {
		args = append(args, f.To)
		clauses = append(clauses, fmt.Sprintf("created_at <= $%d", len(args)))
	}

	if len(clauses) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(clauses, " AND "), args
}

func scanEntries(rows pgx.Rows) ([]Entry, error) {
	var out []Entry
	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("audit: rows: %w", err)
	}
	return out, nil
}

func scanEntry(rows pgx.Rows) (Entry, error) {
	var (
		e        Entry
		actorID  *uuid.UUID
		resource *string
		ip       *string
		ua       *string
		reqID    *string
	)
	if err := rows.Scan(
		&e.ID, &actorID, &e.ActorRole, &e.Action, &e.ResourceType, &resource,
		&e.OldValue, &e.NewValue, &ip, &ua, &reqID, &e.CreatedAt,
		&e.PrevHash, &e.RowHash,
	); err != nil {
		return Entry{}, fmt.Errorf("audit: scan: %w", err)
	}
	if actorID != nil {
		e.ActorID = *actorID
	}
	if resource != nil {
		e.ResourceID = *resource
	}
	if ip != nil {
		e.IP = *ip
	}
	if ua != nil {
		e.UserAgent = *ua
	}
	if reqID != nil {
		e.RequestID = *reqID
	}
	return e, nil
}

// ipColumn is how every read projects audit_logs.ip.
//
// The column is INET. pgx v5 negotiates the binary wire format for it and has
// no binary codec that lands in a Go string, so `SELECT ip` into a *string
// fails with "cannot scan inet (OID 869) in binary format into **string" --
// on every row that actually has an address, which after audit.Middleware
// started recording ClientIP is every row. That made GET /admin/audit return
// 500 and made GET /admin/audit/export emit a header and nothing else, with
// the streaming error discarded. host() renders the address as text, which is
// also exactly the form audit_row_canonical hashes, so the export and the
// chain agree on what the value is.
const ipColumn = `host(ip) AS ip`

// chainRow is the shape returned by the verify query.
type chainRow struct {
	ID           int64
	PrevHash     string
	RowHash      string
	ExpectedHash string
}

// VerifyChain walks audit_logs from FromID (default 1) for up to Limit rows
// (default/maximum 5000, so one call over a very large table cannot run
// unbounded) and reports the first row whose stored hash does not match a
// hash freshly recomputed from its current content, or whose prev_hash does
// not match the previous row's stored row_hash.
//
// The recomputation uses audit_row_canonical/audit_row_hash -- the exact
// same SQL functions the insert trigger uses -- so there is one
// implementation of "how a row hashes", not two that could quietly drift
// apart. What makes this a real integrity check rather than a tautology is
// that it compares a value computed *now*, from the row's *current* content,
// against a value stored *at insert time*: if a row's content changed after
// insert (whether through a bypassed trigger or direct file-level tampering
// restored via backup), the two will disagree.
func (r *Repository) VerifyChain(ctx context.Context, req VerifyRequest) (VerifyResult, error) {
	fromID := req.FromID
	if fromID < 1 {
		fromID = 1
	}
	limit := req.Limit
	if limit <= 0 || limit > 5000 {
		limit = 5000
	}

	// Completeness first, before any linkage walk. A hash chain proves that
	// the rows PRESENT are the rows that were written; on its own it says
	// nothing about rows that are no longer there. Walking a truncated table
	// finds zero broken links and would report valid -- which is precisely the
	// answer an attacker who truncated it wants (security review F14).
	complete, err := r.verifyCompleteness(ctx)
	if err != nil {
		return VerifyResult{}, err
	}
	if complete.Broken != nil {
		complete.FromID = fromID
		return complete, nil
	}

	expectedPrev := genesisHash
	if fromID > 1 {
		var h string
		err := r.pool.QueryRow(ctx, `SELECT row_hash FROM audit_logs WHERE id = $1`, fromID-1).Scan(&h)
		switch {
		case err == nil:
			expectedPrev = h
		case errors.Is(err, pgx.ErrNoRows):
			// No row immediately precedes fromID (a gap, or fromID is past
			// the start of a table that does not begin at id=1 -- possible
			// after the retention job referenced in 000001 prunes rows in a
			// future migration). Chain linkage for the first row in this
			// window cannot be verified against a prior row; content hashing
			// still applies to every row that is returned.
			expectedPrev = ""
		default:
			return VerifyResult{}, fmt.Errorf("audit: verify seed: %w", err)
		}
	}

	const q = `
		SELECT id, prev_hash, row_hash,
		       audit_row_hash(prev_hash, audit_row_canonical(
		           id, actor_id, actor_role, action, resource_type, resource_id,
		           old_value, new_value, ip, user_agent, request_id, created_at
		       )) AS expected_hash
		FROM audit_logs
		WHERE id >= $1
		ORDER BY id
		LIMIT $2`

	rows, err := r.pool.Query(ctx, q, fromID, limit)
	if err != nil {
		return VerifyResult{}, fmt.Errorf("audit: verify query: %w", err)
	}
	defer rows.Close()

	result := VerifyResult{
		Valid:           true,
		FromID:          fromID,
		ExpectedEntries: complete.ExpectedEntries,
		ActualEntries:   complete.ActualEntries,
	}
	var count int64
	for rows.Next() {
		var c chainRow
		if err := rows.Scan(&c.ID, &c.PrevHash, &c.RowHash, &c.ExpectedHash); err != nil {
			return VerifyResult{}, fmt.Errorf("audit: verify scan: %w", err)
		}
		count++
		result.LastID = c.ID

		if expectedPrev != "" && c.PrevHash != expectedPrev {
			result.Valid = false
			result.Checked = count
			result.Broken = &BrokenLink{ID: c.ID, Reason: "prev_hash does not match the preceding row's row_hash"}
			return result, nil
		}
		if c.RowHash != c.ExpectedHash {
			result.Valid = false
			result.Checked = count
			result.Broken = &BrokenLink{ID: c.ID, Reason: "row_hash does not match a hash recomputed from the row's current content"}
			return result, nil
		}
		expectedPrev = c.RowHash
	}
	if err := rows.Err(); err != nil {
		return VerifyResult{}, fmt.Errorf("audit: verify rows: %w", err)
	}

	result.Checked = count
	if count == int64(limit) {
		next := result.LastID + 1
		result.NextFromID = &next
	}
	return result, nil
}

// verifyCompleteness answers the question the hash chain cannot: are all the
// rows still here?
//
// audit_chain_state is a single-row anchor maintained by the BEFORE INSERT
// trigger. entry_count counts every row ever chained, last_id is the highest
// id ever chained, and last_hash is the newest row's hash. The table can only
// move forward -- trg_audit_chain_state_forward_only refuses any UPDATE that is
// not exactly one step, and the app role has no write privilege on it at all
// (migrations/000009_audit_truncate_guard.up.sql) -- so it survives a TRUNCATE
// of audit_logs and contradicts it.
//
// Three independent statements are checked:
//
//   - the row count matches the anchor. Catches TRUNCATE and any bulk delete.
//   - MAX(id) matches the anchor. Catches TRUNCATE ... RESTART IDENTITY, which
//     would otherwise let a re-populated table look like a shorter but
//     self-consistent history.
//   - the newest row's row_hash matches the anchor's last_hash. Catches
//     tampering with the tail, which a windowed walk may not reach.
//
// COUNT(*) is a full scan. This is an operator-invoked integrity check, not a
// request-path query, and being able to say "1,412 rows are missing" is worth
// the scan. It runs on every call, including a windowed one, deliberately: a
// partial verify that reports valid while the table has been emptied is the
// exact failure this exists to prevent.
func (r *Repository) verifyCompleteness(ctx context.Context) (VerifyResult, error) {
	const q = `
		SELECT s.entry_count,
		       s.last_id,
		       s.last_hash,
		       (SELECT COUNT(*) FROM audit_logs),
		       COALESCE((SELECT MAX(id) FROM audit_logs), 0),
		       COALESCE((SELECT row_hash FROM audit_logs ORDER BY id DESC LIMIT 1), $1)
		FROM audit_chain_state s
		WHERE s.id = TRUE`

	var (
		expectedCount, expectedLastID int64
		expectedHash                  string
		actualCount, actualLastID     int64
		actualHash                    string
	)
	err := r.pool.QueryRow(ctx, q, genesisHash).Scan(
		&expectedCount, &expectedLastID, &expectedHash,
		&actualCount, &actualLastID, &actualHash,
	)
	if err != nil {
		return VerifyResult{}, fmt.Errorf("audit: verify completeness: %w", err)
	}

	out := VerifyResult{Valid: true, ExpectedEntries: expectedCount, ActualEntries: actualCount}

	switch {
	case actualCount != expectedCount:
		out.Valid = false
		out.Broken = &BrokenLink{Reason: fmt.Sprintf(
			"audit_logs holds %d rows but the chain anchor records %d ever written: %d rows are missing",
			actualCount, expectedCount, expectedCount-actualCount)}
	case actualLastID != expectedLastID:
		out.Valid = false
		out.Broken = &BrokenLink{Reason: fmt.Sprintf(
			"highest audit_logs id is %d but the chain anchor records %d: the table was emptied and rewritten",
			actualLastID, expectedLastID)}
	case actualHash != expectedHash:
		out.Valid = false
		out.Broken = &BrokenLink{ID: actualLastID, Reason: "the newest row's row_hash does not match the chain anchor's last_hash"}
	}
	return out, nil
}

// LatestHash returns the row_hash of the most recently inserted row, or the
// genesis hash if the table is empty. Used by tests and by RUNBOOK
// diagnostics to cross-check against audit_chain_state.last_hash.
func (r *Repository) LatestHash(ctx context.Context) (string, error) {
	var h string
	err := r.pool.QueryRow(ctx, `SELECT last_hash FROM audit_chain_state WHERE id = TRUE`).Scan(&h)
	if err != nil {
		return "", fmt.Errorf("audit: latest hash: %w", err)
	}
	return h, nil
}
