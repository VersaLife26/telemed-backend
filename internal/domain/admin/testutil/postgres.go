//go:build integration

// Package testutil spins up a real Postgres container for integration tests
// and applies every migration in migrations/ verbatim, so the tests exercise
// exactly the SQL that ships to production -- the hash-chain trigger, the
// append-only grants, and the materialized views -- rather than a
// hand-rolled approximation of the schema.
package testutil

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	tc "github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"telemed/internal/platform/database"
	"telemed/internal/platform/repopath"
)

// StartPostgres launches postgres:17-alpine (the platform's pinned version,
// see AGENT-BRIEF section 1), applies every migrations/*.up.sql file in
// order, and returns a pool connected as the database owner. The container
// is terminated automatically via t.Cleanup, so a test run never leaves a
// container behind regardless of pass/fail.
func StartPostgres(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()

	container, err := tcpostgres.Run(ctx, "postgres:17-alpine",
		tcpostgres.WithDatabase("telemed_admin"),
		tcpostgres.WithUsername("postgres"),
		tcpostgres.WithPassword("postgres"),
		tc.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		t.Fatalf("testutil: start postgres container: %v", err)
	}
	t.Cleanup(func() {
		if err := container.Terminate(context.Background()); err != nil {
			t.Logf("testutil: terminate postgres container: %v", err)
		}
	})

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("testutil: connection string: %v", err)
	}

	dsn, err = database.EnsureSchema(ctx, dsn, "admin")
	if err != nil {
		t.Fatalf("testutil: provision schema: %v", err)
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("testutil: connect pool: %v", err)
	}
	t.Cleanup(pool.Close)

	applyMigrations(t, ctx, pool)
	return pool
}

func applyMigrations(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	dir := migrationsDir(t)

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("testutil: read migrations dir %s: %v", dir, err)
	}
	var files []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".up.sql") {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files)
	if len(files) == 0 {
		t.Fatalf("testutil: no .up.sql migrations found in %s", dir)
	}

	for _, name := range files {
		body, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("testutil: read %s: %v", name, err)
		}
		// A plain string Exec with no arguments uses pgx's simple query
		// protocol, which -- like psql -f -- runs every semicolon-separated
		// statement in the file, including the PL/pgSQL DO blocks and
		// function bodies that contain their own internal semicolons.
		if _, err := pool.Exec(ctx, string(body)); err != nil {
			t.Fatalf("testutil: apply %s: %v", name, err)
		}
	}
}

// migrationsDir locates the admin domain's migrations directory.
func migrationsDir(t *testing.T) string {
	t.Helper()
	return repopath.Migrations(t, "admin")
}

// AppRoleDSN swaps the connection to the restricted telemed_admin_app role
// the migrations create, so a test can assert what that role can and cannot
// do -- as opposed to the owner role StartPostgres itself connects as.
func AppRoleDSN(ctx context.Context, t *testing.T, ownerPool *pgxpool.Pool) string {
	t.Helper()
	var db string
	if err := ownerPool.QueryRow(ctx, `SELECT current_database()`).Scan(&db); err != nil {
		t.Fatalf("testutil: current_database: %v", err)
	}
	cfg := ownerPool.Config().ConnConfig
	// search_path, like every other connection on the platform. The app role
	// connects to the same database as the owner and must resolve unqualified
	// names in svc_admin -- without it the tables are simply invisible and the
	// privilege assertions below fail as "relation does not exist" rather than
	// as the permission denial they are checking for.
	dsn, err := database.WithSearchPath(
		fmt.Sprintf("postgres://telemed_admin_app:changeme_in_deployment_secret_manager@%s:%d/%s?sslmode=disable",
			cfg.Host, cfg.Port, db),
		database.SearchPathFor("admin"))
	if err != nil {
		t.Fatalf("testutil: build app-role dsn: %v", err)
	}
	return dsn
}
