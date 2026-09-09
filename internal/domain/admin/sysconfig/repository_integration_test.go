//go:build integration

package sysconfig_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"telemed/internal/domain/admin/sysconfig"
	"telemed/internal/domain/admin/testutil"
	"telemed/internal/platform/middleware"
)

func TestSysConfig_PutNeverMutatesPreviousVersion(t *testing.T) {
	ctx := context.Background()
	pool := testutil.StartPostgres(t)
	repo := sysconfig.NewRepository(pool)

	v1, err := repo.Put(ctx, "commission_rules", []byte(`{"gp_percent":20}`), uuid.Nil, time.Time{})
	require.NoError(t, err)
	require.Equal(t, 1, v1.Version)

	v2, err := repo.Put(ctx, "commission_rules", []byte(`{"gp_percent":25}`), uuid.Nil, time.Time{})
	require.NoError(t, err)
	require.Equal(t, 2, v2.Version)

	history, err := repo.History(ctx, "commission_rules")
	require.NoError(t, err)
	require.Len(t, history, 2, "both versions must exist side by side")
	require.JSONEq(t, `{"gp_percent":20}`, string(history[1].Value), "version 1's content must be untouched by the version 2 write")
	require.JSONEq(t, `{"gp_percent":25}`, string(history[0].Value))

	current, err := repo.Current(ctx, "commission_rules", time.Time{})
	require.NoError(t, err)
	require.Equal(t, 2, current.Version, "current must be the latest version")
}

func TestSysConfig_AppendOnly_RejectsDirectUpdate(t *testing.T) {
	ctx := context.Background()
	pool := testutil.StartPostgres(t)
	repo := sysconfig.NewRepository(pool)

	v1, err := repo.Put(ctx, "fee_caps", []byte(`{"max_lkr":10000}`), uuid.Nil, time.Time{})
	require.NoError(t, err)

	_, err = pool.Exec(ctx, `UPDATE system_configs SET value = '{"max_lkr":999999}' WHERE id = $1`, v1.ID)
	require.Error(t, err)
	require.Contains(t, err.Error(), "append-only")

	_, err = pool.Exec(ctx, `DELETE FROM system_configs WHERE id = $1`, v1.ID)
	require.Error(t, err)
	require.Contains(t, err.Error(), "append-only")
}

// TestSysConfig_EffectiveFromScheduling confirms a config version scheduled
// for the future does not become "current" until its effective_from arrives,
// and that Current(asOf) can time-travel for exactly that reason.
func TestSysConfig_EffectiveFromScheduling(t *testing.T) {
	ctx := context.Background()
	pool := testutil.StartPostgres(t)
	repo := sysconfig.NewRepository(pool)

	now := time.Now().UTC()
	_, err := repo.Put(ctx, "cancellation_policy", []byte(`{"hours_before":24}`), uuid.Nil, now.Add(-time.Hour))
	require.NoError(t, err)

	future := now.Add(48 * time.Hour)
	_, err = repo.Put(ctx, "cancellation_policy", []byte(`{"hours_before":48}`), uuid.Nil, future)
	require.NoError(t, err)

	currentNow, err := repo.Current(ctx, "cancellation_policy", now)
	require.NoError(t, err)
	require.JSONEq(t, `{"hours_before":24}`, string(currentNow.Value), "the future-dated version must not be current yet")

	currentFuture, err := repo.Current(ctx, "cancellation_policy", future.Add(time.Minute))
	require.NoError(t, err)
	require.JSONEq(t, `{"hours_before":48}`, string(currentFuture.Value), "asOf in the future must see the scheduled version")
}

// TestSysConfig_ConcurrentPutsAllSucceedWithDistinctVersions drives several
// concurrent PUTs for the same key and asserts every one lands with a
// distinct, gap-free version number -- proving UNIQUE(key, version) plus the
// service's retry loop is what serializes them, not application-level
// locking (ADR-007's reasoning: Postgres is the source of truth).
func TestSysConfig_ConcurrentPutsAllSucceedWithDistinctVersions(t *testing.T) {
	ctx := context.Background()
	pool := testutil.StartPostgres(t)
	svc := sysconfig.NewService(sysconfig.NewRepository(pool))

	const n = 10
	var wg sync.WaitGroup
	versions := make([]int, n)
	errs := make([]error, n)

	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			val, _ := json.Marshal(map[string]int{"attempt": i})
			cfg, err := svc.Put(ctx, opsPrincipal(), uuid.Nil, "feature.waitlist_v2", val, time.Time{})
			versions[i] = cfg.Version
			errs[i] = err
		}(i)
	}
	wg.Wait()

	seen := map[int]bool{}
	for i, err := range errs {
		require.NoErrorf(t, err, "attempt %d should have succeeded (service retries on version races)", i)
		require.Falsef(t, seen[versions[i]], "version %d was assigned twice", versions[i])
		seen[versions[i]] = true
	}
	for v := 1; v <= n; v++ {
		require.Truef(t, seen[v], "version %d should have been assigned to exactly one writer", v)
	}
}

// opsPrincipal is an ops admin, which sysconfig's write policy permits for
// feature flags. The concurrency test is about version assignment, not about
// authorisation -- but Put applies the policy, so the caller has to be real.
func opsPrincipal() middleware.Principal {
	return middleware.Principal{UserID: uuid.New(), Roles: []middleware.Role{middleware.RoleOps}}
}
