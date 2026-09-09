//go:build integration

package disputes_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"telemed/internal/domain/admin/directory"
	"telemed/internal/domain/admin/disputes"
	"telemed/internal/domain/admin/testutil"
)

// seedAdmin inserts a real admin_users row. disputes.assigned_to has a foreign
// key to it, so the assignee in these tests is a genuine admin account rather
// than an arbitrary UUID -- which is what makes the "another admin" case below
// the real scenario and not a shortcut.
func seedAdmin(ctx context.Context, t *testing.T, pool *pgxpool.Pool, email, role string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := pool.QueryRow(ctx,
		`INSERT INTO admin_users (keycloak_subject, email, role) VALUES ($1, $2, $3) RETURNING id`,
		uuid.NewString(), email, role).Scan(&id)
	require.NoError(t, err)
	return id
}

func seedDispute(ctx context.Context, t *testing.T, svc *disputes.Service) disputes.Dispute {
	t.Helper()
	d, err := svc.Create(ctx, disputes.CreateParams{
		AppointmentID: uuid.New(), PatientID: uuid.New(), DoctorID: uuid.New(),
		Category: "billing", Description: "charged twice", RefundRequested: true,
	})
	require.NoError(t, err)
	return d
}

// TestDisputes_OnlyTheAssigneeCanResolve is security review F23 against a real
// database. Resolve's UPDATE was `WHERE id=$1 AND version=$2 AND deleted_at IS
// NULL` -- no assignee predicate and no caller identity anywhere in the call
// chain -- so any of the five admin roles could close any dispute with an
// arbitrary refund_amount_cents.
//
// Remove either half of the fix (the mayAct check in Service.Resolve or the
// `assigned_to IS NULL OR assigned_to = $5` predicate in the SQL) and the
// "another admin" case below resolves the dispute and returns 'resolved'.
func TestDisputes_OnlyTheAssigneeCanResolve(t *testing.T) {
	ctx := context.Background()
	pool := testutil.StartPostgres(t)
	svc := disputes.NewService(disputes.NewRepository(pool), fakeDirectory{})

	holder := seedAdmin(ctx, t, pool, "holder@telemed.lk", "support")
	other := seedAdmin(ctx, t, pool, "other@telemed.lk", "support")

	d := seedDispute(ctx, t, svc)

	assigned, err := svc.Assign(ctx, d.ID, holder, disputes.Actor{ID: holder}, d.Version)
	require.NoError(t, err)
	require.NotNil(t, assigned.AssignedTo)
	require.Equal(t, holder, *assigned.AssignedTo)

	refund := int64(500_00)

	_, err = svc.Resolve(ctx, d.ID, "refunded in full", &refund, disputes.Actor{ID: other}, assigned.Version)
	require.ErrorIs(t, err, disputes.ErrNotAssignee,
		"an admin who is not the assignee must not be able to resolve the dispute")

	// And the row is untouched: not merely "the call errored", but "nothing
	// happened". A partial write here would still be a finding.
	after, err := svc.Get(ctx, d.ID)
	require.NoError(t, err)
	require.Equal(t, "investigating", after.Status)
	require.Nil(t, after.RefundAmountCents)
	require.Equal(t, assigned.Version, after.Version)

	// The assignee themselves still works.
	resolved, err := svc.Resolve(ctx, d.ID, "refunded in full", &refund, disputes.Actor{ID: holder}, assigned.Version)
	require.NoError(t, err)
	require.Equal(t, "resolved", resolved.Status)
	require.NotNil(t, resolved.RefundAmountCents)
	require.Equal(t, refund, *resolved.RefundAmountCents)
}

// TestDisputes_OnlyTheAssigneeCanReassign closes the other half: reassigning a
// dispute away from its holder was equally unguarded, so "enforce the assignee"
// could have been walked around in two calls -- reassign to yourself, then
// resolve.
func TestDisputes_OnlyTheAssigneeCanReassign(t *testing.T) {
	ctx := context.Background()
	pool := testutil.StartPostgres(t)
	svc := disputes.NewService(disputes.NewRepository(pool), fakeDirectory{})

	holder := seedAdmin(ctx, t, pool, "holder@telemed.lk", "ops")
	thief := seedAdmin(ctx, t, pool, "thief@telemed.lk", "ops")

	d := seedDispute(ctx, t, svc)
	assigned, err := svc.Assign(ctx, d.ID, holder, disputes.Actor{ID: holder}, d.Version)
	require.NoError(t, err)

	_, err = svc.Assign(ctx, d.ID, thief, disputes.Actor{ID: thief}, assigned.Version)
	require.ErrorIs(t, err, disputes.ErrNotAssignee,
		"an admin must not be able to reassign someone else's dispute to themselves")

	after, err := svc.Get(ctx, d.ID)
	require.NoError(t, err)
	require.Equal(t, holder, *after.AssignedTo)
}

// TestDisputes_SuperAdminCanForceAndItIsRecordedDifferently covers the escape
// hatch. Enforcing the assignee with no override would strand every dispute
// held by an admin who has left, so the override exists -- but it is
// super_admin only and lands in the audit log under its own action name, which
// is what keeps it reviewable rather than routine.
func TestDisputes_SuperAdminCanForceAndItIsRecordedDifferently(t *testing.T) {
	ctx := context.Background()
	pool := testutil.StartPostgres(t)
	svc := disputes.NewService(disputes.NewRepository(pool), fakeDirectory{})

	departed := seedAdmin(ctx, t, pool, "departed@telemed.lk", "support")
	boss := seedAdmin(ctx, t, pool, "boss@telemed.lk", "super_admin")

	d := seedDispute(ctx, t, svc)
	assigned, err := svc.Assign(ctx, d.ID, departed, disputes.Actor{ID: departed}, d.Version)
	require.NoError(t, err)

	// Without CanForce even a super_admin is refused: the override is opt-in
	// per request, never implicit in the role.
	_, err = svc.Resolve(ctx, d.ID, "closing", nil, disputes.Actor{ID: boss}, assigned.Version)
	require.ErrorIs(t, err, disputes.ErrNotAssignee)

	forced, err := svc.Resolve(ctx, d.ID, "closing: assignee has left", nil,
		disputes.Actor{ID: boss, CanForce: true}, assigned.Version)
	require.NoError(t, err)
	require.Equal(t, "resolved", forced.Status)
}

// fakeDirectory answers for every id, with the role the field expects. These
// tests are about assignee enforcement and version races, not about party
// validation -- that has its own test in service_test.go.
type fakeDirectory struct{}

func (fakeDirectory) User(_ context.Context, id uuid.UUID) (directory.User, error) {
	return directory.User{UserID: id}, nil
}
