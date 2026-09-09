//go:build integration

package audit_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"telemed/internal/domain/admin/audit"
	"telemed/internal/domain/admin/rbac"
	"telemed/internal/domain/admin/testutil"
	mw "telemed/internal/platform/middleware"
)

func seedEntries(ctx context.Context, t *testing.T, repo *audit.Repository, n int) {
	t.Helper()
	for i := range n {
		_, err := repo.Insert(ctx, audit.InsertParams{
			ActorRole: "super_admin", Action: "doctor.approved", ResourceType: "doctor",
			ResourceID: fmt.Sprintf("doctor-%d", i), IP: "10.0.0.1",
		})
		require.NoError(t, err)
	}
}

// TestAuditLogs_CannotBeTruncated is security review F14(b).
//
// 000002 blocked UPDATE and DELETE with BEFORE ... FOR EACH ROW triggers. Row
// triggers do not fire on TRUNCATE -- Postgres implements it as a relation
// operation, not as a set of row deletions -- so the single statement that
// erases the entire trail at once walked past both of them. The REVOKE covered
// the app role; the table's owner and any superuser session were unguarded.
//
// Drop trg_audit_logs_no_truncate from migration 000009 and the owner case
// below succeeds silently.
func TestAuditLogs_CannotBeTruncated(t *testing.T) {
	ctx := context.Background()
	pool := testutil.StartPostgres(t)
	repo := audit.NewRepository(pool)
	seedEntries(ctx, t, repo, 3)

	t.Run("the owner is stopped by the trigger", func(t *testing.T) {
		_, err := pool.Exec(ctx, `TRUNCATE audit_logs`)
		require.Error(t, err, "TRUNCATE must be refused even for the table owner")
		require.Contains(t, err.Error(), "append-only")
	})

	t.Run("RESTART IDENTITY does not slip past it", func(t *testing.T) {
		_, err := pool.Exec(ctx, `TRUNCATE audit_logs RESTART IDENTITY`)
		require.Error(t, err)
		require.Contains(t, err.Error(), "append-only")
	})

	t.Run("the app role is stopped by the grant, before the trigger", func(t *testing.T) {
		appPool, err := pgxpool.New(ctx, testutil.AppRoleDSN(ctx, t, pool))
		require.NoError(t, err)
		defer appPool.Close()

		_, err = appPool.Exec(ctx, `TRUNCATE audit_logs`)
		require.Error(t, err)
		require.Contains(t, err.Error(), "permission denied")
	})

	t.Run("the chain anchor cannot be truncated either", func(t *testing.T) {
		_, err := pool.Exec(ctx, `TRUNCATE audit_chain_state`)
		require.Error(t, err)
		require.Contains(t, err.Error(), "append-only")
	})

	// Nothing was lost along the way.
	var count int64
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM audit_logs`).Scan(&count))
	require.EqualValues(t, 3, count)
}

// TestAuditChain_DetectsTruncation is the question the brief asked directly:
// would the SHA-256 chain notice if the log were truncated anyway?
//
// Before migration 000009 the answer was NO, and this test is the proof. The
// chain links each row to the previous one, so it can only speak about rows
// that are still present; a walk over an empty table finds zero broken links
// and reported `{"valid": true, "checked": 0}`. audit_chain_state.last_hash
// survived the truncate but nothing compared it to the table.
//
// The truncate is performed with the guard trigger disabled, which is exactly
// the threat model the chain exists for: a DBA or a compromised superuser
// session, against whom a trigger is not a control. The chain's job is to make
// it DETECTABLE afterwards.
func TestAuditChain_DetectsTruncation(t *testing.T) {
	ctx := context.Background()
	pool := testutil.StartPostgres(t)
	repo := audit.NewRepository(pool)
	seedEntries(ctx, t, repo, 6)

	before, err := repo.VerifyChain(ctx, audit.VerifyRequest{})
	require.NoError(t, err)
	require.True(t, before.Valid)
	require.EqualValues(t, 6, before.ExpectedEntries)
	require.EqualValues(t, 6, before.ActualEntries)

	truncateBypassingTheGuard(ctx, t, pool)

	after, err := repo.VerifyChain(ctx, audit.VerifyRequest{})
	require.NoError(t, err)
	require.False(t, after.Valid,
		"an emptied audit_logs must NOT verify clean: %+v", after)
	require.NotNil(t, after.Broken)
	require.Contains(t, after.Broken.Reason, "missing")
	require.EqualValues(t, 6, after.ExpectedEntries)
	require.EqualValues(t, 0, after.ActualEntries)
}

// TestAuditChain_DetectsTruncateThenReplay closes the smarter version of the
// attack: empty the table, then write a plausible-looking history back into it.
// Every remaining row chains correctly to the one before, so linkage alone is
// satisfied; only the anchor contradicts it.
func TestAuditChain_DetectsTruncateThenReplay(t *testing.T) {
	ctx := context.Background()
	pool := testutil.StartPostgres(t)
	repo := audit.NewRepository(pool)
	seedEntries(ctx, t, repo, 5)

	truncateBypassingTheGuard(ctx, t, pool)

	// The attacker writes a tidier history. These go through the real insert
	// trigger, so each one is internally consistent.
	seedEntries(ctx, t, repo, 2)

	result, err := repo.VerifyChain(ctx, audit.VerifyRequest{})
	require.NoError(t, err)
	require.False(t, result.Valid, "a re-populated audit log must not verify clean: %+v", result)
	require.NotNil(t, result.Broken)
}

// TestAuditChainState_CannotBeResetOrRewound removes the obvious next move: if
// the anchor is what gives the truncation away, tamper with the anchor.
//
// The app role has no write privilege on it at all (the chain trigger is
// SECURITY DEFINER, so the connection does not need one), and even the owner
// can only move it forward by exactly one entry at a time.
func TestAuditChainState_CannotBeResetOrRewound(t *testing.T) {
	ctx := context.Background()
	pool := testutil.StartPostgres(t)
	repo := audit.NewRepository(pool)
	seedEntries(ctx, t, repo, 4)

	t.Run("the owner cannot reset it to genesis", func(t *testing.T) {
		_, err := pool.Exec(ctx,
			`UPDATE audit_chain_state SET last_hash = repeat('0', 64), entry_count = 0, last_id = 0 WHERE id = TRUE`)
		require.Error(t, err)
		require.Contains(t, err.Error(), "advance by one")
	})

	t.Run("the owner cannot rewind the count", func(t *testing.T) {
		_, err := pool.Exec(ctx, `UPDATE audit_chain_state SET entry_count = entry_count - 1 WHERE id = TRUE`)
		require.Error(t, err)
	})

	t.Run("the owner cannot delete it", func(t *testing.T) {
		_, err := pool.Exec(ctx, `DELETE FROM audit_chain_state`)
		require.Error(t, err)
		require.Contains(t, err.Error(), "DELETE is not permitted")
	})

	t.Run("the app role cannot write it at all", func(t *testing.T) {
		appPool, err := pgxpool.New(ctx, testutil.AppRoleDSN(ctx, t, pool))
		require.NoError(t, err)
		defer appPool.Close()

		_, err = appPool.Exec(ctx, `UPDATE audit_chain_state SET last_hash = repeat('0', 64) WHERE id = TRUE`)
		require.Error(t, err)
		require.Contains(t, err.Error(), "permission denied")

		// ...and yet it can still do the one thing it needs to, because the
		// chain trigger runs as the definer, not as the caller.
		appRepo := audit.NewRepository(appPool)
		_, err = appRepo.Insert(ctx, audit.InsertParams{ActorRole: "admin", Action: "user.suspend", ResourceType: "user"})
		require.NoError(t, err, "the app role must still be able to append")
	})

	result, err := repo.VerifyChain(ctx, audit.VerifyRequest{})
	require.NoError(t, err)
	require.True(t, result.Valid, "none of the attempts above may have partially applied: %+v", result)
	require.EqualValues(t, 5, result.ActualEntries)
}

// TestAuditExport_IsItselfAudited is the third part of security review F6. The
// export was a GET, audit.Middleware skips GETs, and the query had no bound --
// so one request copied the entire trail and left no row in the trail it
// copied. An export that records nothing is the one an attacker uses.
func TestAuditExport_IsItselfAudited(t *testing.T) {
	ctx := context.Background()
	pool := testutil.StartPostgres(t)
	repo := audit.NewRepository(pool)
	svc := audit.NewService(repo)
	seedEntries(ctx, t, repo, 3)

	router := audit.NewHandler(svc, rbac.Roles(rbac.GroupAuditExport)...).Routes()
	window := fmt.Sprintf("?from=%s&to=%s",
		time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
		time.Now().Add(time.Hour).UTC().Format(time.RFC3339))

	exporter := uuid.New()
	reqCtx := mw.WithPrincipal(ctx, mw.Principal{UserID: exporter, Roles: []mw.Role{mw.RoleFinance}})
	req := httptest.NewRequestWithContext(reqCtx, http.MethodGet, "/export"+window, http.NoBody)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	require.Contains(t, rec.Body.String(), "doctor.approved", "the export must actually contain the rows")

	entries, total, err := svc.List(ctx, audit.ListFilter{Action: audit.ActionExported})
	require.NoError(t, err)
	require.EqualValues(t, 1, total, "the export itself must have been recorded")
	require.Equal(t, exporter, entries[0].ActorID)
	require.Equal(t, "finance", entries[0].ActorRole)
	require.Contains(t, string(entries[0].NewValue), `"row_count": 3`,
		"the audit row must record the scope of what was taken: %s", entries[0].NewValue)

	// The chain absorbed the new row without breaking.
	result, err := repo.VerifyChain(ctx, audit.VerifyRequest{})
	require.NoError(t, err)
	require.True(t, result.Valid, "%+v", result)
}

// TestAuditExport_RefusedRoleLeavesNoExport confirms the gate is in front of
// the recording, not the other way round: a refused export must neither serve
// data nor manufacture an audit row implying one happened.
func TestAuditExport_RefusedRoleLeavesNoExport(t *testing.T) {
	ctx := context.Background()
	pool := testutil.StartPostgres(t)
	repo := audit.NewRepository(pool)
	svc := audit.NewService(repo)
	seedEntries(ctx, t, repo, 2)

	router := audit.NewHandler(svc, rbac.Roles(rbac.GroupAuditExport)...).Routes()
	window := fmt.Sprintf("?from=%s&to=%s",
		time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
		time.Now().Add(time.Hour).UTC().Format(time.RFC3339))

	reqCtx := mw.WithPrincipal(ctx, mw.Principal{UserID: uuid.New(), Roles: []mw.Role{mw.RoleSupport}})
	req := httptest.NewRequestWithContext(reqCtx, http.MethodGet, "/export"+window, http.NoBody)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusForbidden, rec.Code)
	require.NotContains(t, rec.Body.String(), "doctor.approved")

	_, total, err := svc.List(ctx, audit.ListFilter{Action: audit.ActionExported})
	require.NoError(t, err)
	require.Zero(t, total)
}

// truncateBypassingTheGuard is the DBA-level attack the hash chain exists to
// catch: disable the statement trigger, truncate, put it back. The GRANT and
// the trigger both stop the SERVICE; neither stops someone sitting at psql as
// the owner. Detection afterwards is the control that does.
func truncateBypassingTheGuard(ctx context.Context, t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `ALTER TABLE audit_logs DISABLE TRIGGER trg_audit_logs_no_truncate`); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `TRUNCATE audit_logs`); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `ALTER TABLE audit_logs ENABLE TRIGGER trg_audit_logs_no_truncate`)
		return err
	})
	require.NoError(t, err)
}
