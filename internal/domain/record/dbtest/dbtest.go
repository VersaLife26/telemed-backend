//go:build integration

// Package dbtest is test-only scaffolding shared by every domain package's
// integration suite: it starts a real Postgres via testcontainers, applies
// every migration in order, and hands back a ready pool. It exists so each
// integration_test.go does not reinvent container bring-up.
package dbtest

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"telemed/internal/platform/database"
	"telemed/internal/platform/repopath"
)

// NewPostgres starts a postgres:17-alpine container (matching AGENT-BRIEF's
// canonical infra image), applies migrations/*.up.sql in order, and returns
// a connected pool. The container and pool are torn down automatically via
// t.Cleanup, so callers never need their own cleanup logic.
func NewPostgres(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	ctr, err := tcpostgres.Run(ctx, "postgres:17-alpine",
		tcpostgres.WithDatabase("telemed_record_test"),
		tcpostgres.WithUsername("telemed"),
		tcpostgres.WithPassword("telemed"),
		tcpostgres.BasicWaitStrategies(),
	)
	testcontainers.CleanupContainer(t, ctr)
	if err != nil {
		t.Fatalf("dbtest: start postgres container: %v", err)
	}

	connStr, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("dbtest: connection string: %v", err)
	}

	connStr, err = database.EnsureSchema(ctx, connStr, "record")
	if err != nil {
		t.Fatalf("dbtest: provision schema: %v", err)
	}

	pool, err := pgxpool.New(ctx, connStr)
	if err != nil {
		t.Fatalf("dbtest: open pool: %v", err)
	}
	t.Cleanup(pool.Close)

	applyMigrations(t, ctx, pool)
	return pool
}

// applyMigrations runs every *.up.sql file in the repository's migrations
// directory, in filename order, exactly as golang-migrate would.
func applyMigrations(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	dir := migrationsDir(t)

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("dbtest: read migrations dir %s: %v", dir, err)
	}

	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".up.sql") {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files)
	if len(files) == 0 {
		t.Fatalf("dbtest: no .up.sql migrations found in %s", dir)
	}

	for _, f := range files {
		body, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			t.Fatalf("dbtest: read migration %s: %v", f, err)
		}
		if _, err := pool.Exec(ctx, string(body)); err != nil {
			t.Fatalf("dbtest: apply migration %s: %v", f, err)
		}
	}
}

// migrationsDir resolves the record domain's migrations directory.
//
// Hard-coded to "record" because this helper belongs to the record domain --
// it lived in internal/platform before consolidation only because each service
// was its own repository and platform/ was where shared test scaffolding went.
func migrationsDir(t *testing.T) string {
	t.Helper()
	return repopath.Migrations(t, "record")
}
