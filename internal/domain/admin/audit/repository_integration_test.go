//go:build integration

package audit_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"telemed/internal/domain/admin/audit"
	"telemed/internal/domain/admin/testutil"
)

func TestAuditChain_ValidAfterInserts(t *testing.T) {
	ctx := context.Background()
	pool := testutil.StartPostgres(t)
	repo := audit.NewRepository(pool)

	for i := range 5 {
		_, err := repo.Insert(ctx, audit.InsertParams{
			ActorRole:    "super_admin",
			Action:       "doctor.approved",
			ResourceType: "doctor",
			ResourceID:   "doctor-1",
			NewValue:     map[string]any{"seq": i, "status": "approved"},
			IP:           "10.0.0.1",
			UserAgent:    "test-agent",
			RequestID:    "req-x",
		})
		require.NoError(t, err)
	}

	result, err := repo.VerifyChain(ctx, audit.VerifyRequest{})
	require.NoError(t, err)
	require.True(t, result.Valid, "expected an untampered chain to verify clean: %+v", result)
	require.EqualValues(t, 5, result.Checked)
	require.Nil(t, result.Broken)
}

// TestAuditChain_DetectsTamperedRow is the test the brief calls out
// explicitly: a row mutated after insert (by disabling the trigger, exactly
// the "superuser mistake" scenario the trigger exists to make loud) must be
// the first broken link VerifyChain reports.
func TestAuditChain_DetectsTamperedRow(t *testing.T) {
	ctx := context.Background()
	pool := testutil.StartPostgres(t)
	repo := audit.NewRepository(pool)

	var ids []int64
	for i := range 4 {
		e, err := repo.Insert(ctx, audit.InsertParams{
			ActorRole:    "finance",
			Action:       "config.update",
			ResourceType: "system_config",
			ResourceID:   "commission_rules",
			OldValue:     map[string]any{"v": i},
			NewValue:     map[string]any{"v": i + 1},
			IP:           "10.0.0.2",
		})
		require.NoError(t, err)
		ids = append(ids, e.ID)
	}

	tamperedID := ids[1]
	tamperRow(ctx, t, pool, tamperedID)

	result, err := repo.VerifyChain(ctx, audit.VerifyRequest{})
	require.NoError(t, err)
	require.False(t, result.Valid)
	require.NotNil(t, result.Broken)
	require.Equal(t, tamperedID, result.Broken.ID, "verify must report the exact row that was tampered with")
}

// TestAuditChain_TamperIsDetectableEvenIfLaterRowsAreUntouched confirms the
// walk stops at the FIRST broken link rather than reporting the last one,
// and that rows before the tamper still verify.
func TestAuditChain_TamperIsDetectableEvenIfLaterRowsAreUntouched(t *testing.T) {
	ctx := context.Background()
	pool := testutil.StartPostgres(t)
	repo := audit.NewRepository(pool)

	var ids []int64
	for range 6 {
		e, err := repo.Insert(ctx, audit.InsertParams{
			ActorRole: "ops", Action: "appointment.force_cancel", ResourceType: "appointment",
		})
		require.NoError(t, err)
		ids = append(ids, e.ID)
	}

	tamperRow(ctx, t, pool, ids[3])

	result, err := repo.VerifyChain(ctx, audit.VerifyRequest{})
	require.NoError(t, err)
	require.False(t, result.Valid)
	require.Equal(t, ids[3], result.Broken.ID)
	require.EqualValues(t, 4, result.Checked, "walk must stop at the broken row, not continue past it")
}

// TestAuditLogs_AppendOnly_RejectsUpdate is the other test the brief calls
// out explicitly: the trigger must reject a plain UPDATE outright, with no
// need to disable anything first.
func TestAuditLogs_AppendOnly_RejectsUpdate(t *testing.T) {
	ctx := context.Background()
	pool := testutil.StartPostgres(t)
	repo := audit.NewRepository(pool)

	e, err := repo.Insert(ctx, audit.InsertParams{ActorRole: "support", Action: "dispute.assign", ResourceType: "dispute"})
	require.NoError(t, err)

	_, err = pool.Exec(ctx, `UPDATE audit_logs SET action = 'tampered' WHERE id = $1`, e.ID)
	require.Error(t, err)
	require.Contains(t, err.Error(), "append-only")

	_, err = pool.Exec(ctx, `DELETE FROM audit_logs WHERE id = $1`, e.ID)
	require.Error(t, err)
	require.Contains(t, err.Error(), "append-only")
}

// TestAuditLogs_AppRoleCannotMutate proves the GRANT/REVOKE, not just the
// trigger: connecting as the restricted telemed_admin_app role (the role the
// service actually uses in production) has no UPDATE/DELETE privilege at
// all, independent of the trigger.
func TestAuditLogs_AppRoleCannotMutate(t *testing.T) {
	ctx := context.Background()
	pool := testutil.StartPostgres(t)
	repo := audit.NewRepository(pool)

	e, err := repo.Insert(ctx, audit.InsertParams{ActorRole: "admin", Action: "user.suspend", ResourceType: "user"})
	require.NoError(t, err)

	appDSN := testutil.AppRoleDSN(ctx, t, pool)
	appPool, err := pgxpool.New(ctx, appDSN)
	require.NoError(t, err)
	defer appPool.Close()

	_, err = appPool.Exec(ctx, `UPDATE audit_logs SET action = 'tampered' WHERE id = $1`, e.ID)
	require.Error(t, err, "telemed_admin_app must not be able to UPDATE audit_logs even before the trigger runs")

	_, err = appPool.Exec(ctx, `DELETE FROM audit_logs WHERE id = $1`, e.ID)
	require.Error(t, err, "telemed_admin_app must not be able to DELETE from audit_logs")

	// It must still be able to do the one thing the service needs: insert.
	appRepo := audit.NewRepository(appPool)
	_, err = appRepo.Insert(ctx, audit.InsertParams{ActorRole: "admin", Action: "user.reinstate", ResourceType: "user"})
	require.NoError(t, err, "telemed_admin_app must retain INSERT")
}

// tamperRow simulates a DBA-level bypass: disable the append-only trigger,
// mutate a row's content in place, re-enable the trigger. This is the
// specific attack VerifyChain exists to catch -- the GRANT/REVOKE and the
// trigger both stop the *service*, not a superuser sitting at psql.
func tamperRow(ctx context.Context, t *testing.T, pool *pgxpool.Pool, id int64) {
	t.Helper()
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `ALTER TABLE audit_logs DISABLE TRIGGER trg_audit_logs_no_update`); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE audit_logs SET new_value = '{"tampered":true}'::jsonb WHERE id = $1`, id); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `ALTER TABLE audit_logs ENABLE TRIGGER trg_audit_logs_no_update`)
		return err
	})
	require.NoError(t, err)
}
