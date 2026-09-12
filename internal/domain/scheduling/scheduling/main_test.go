package scheduling_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"

	"telemed/internal/platform/database"
	"telemed/internal/platform/repopath"
)

// The scheduling tests run against a real PostgreSQL, not a mock.
//
// That is not gold-plating. The single most important claim this service makes
// -- "one slot, one appointment, under any concurrency" -- rests on
// SELECT ... FOR UPDATE semantics, on a partial unique index, and on how READ
// COMMITTED re-evaluates a row after a lock is granted. A fake repository that
// modelled those would be a fake of the thing under test, and it would pass
// whether or not Postgres actually behaves that way.
//
// Getting a real database, in order of preference:
//
//  1. TEST_DATABASE_URL, if the developer or CI supplied one.
//  2. An ephemeral cluster started from the local initdb/postgres binaries into
//     a temp directory. Costs about a second, needs no Docker, and is torn down
//     (and Pdeathsig'd) when the test binary exits.
//  3. Skip, loudly.
//
// Option 2 is what makes `go test -race ./...` meaningful on a laptop and on a
// GitHub runner alike, both of which ship PostgreSQL binaries.

var (
	testPool *pgxpool.Pool
	testDSN  string
	skipDB   string
)

func TestMain(m *testing.M) {
	code := func() int {
		cleanup, err := setupDatabase()
		if cleanup != nil {
			defer cleanup()
		}
		if err != nil {
			// Skipping is only ever correct when NO database was asked for.
			//
			// If TEST_DATABASE_URL is set, the operator explicitly asked these
			// tests to run against that database, and a failure to prepare it
			// is a hard error. Skipping there produces the worst possible
			// outcome: `go test` prints ok, the suite reports green, and every
			// invariant this package exists to prove -- including
			// TestConcurrentBooking -- was never executed. That nearly shipped
			// once already, because pointing at an already-migrated database
			// makes the raw .up.sql replay fail.
			if os.Getenv("TEST_DATABASE_URL") != "" {
				fmt.Fprintf(os.Stderr,
					"\n*** TEST_DATABASE_URL is set but the database could not be prepared.\n"+
						"*** Refusing to skip: a green run that tested nothing is worse than a red one.\n"+
						"*** %v\n\n", err)
				return 1
			}
			skipDB = err.Error()
			fmt.Fprintf(os.Stderr, "\n*** database-backed scheduling tests will SKIP: %v\n\n", err)
		}
		return m.Run()
	}()
	os.Exit(code)
}

func setupDatabase() (func(), error) {
	if dsn := os.Getenv("TEST_DATABASE_URL"); dsn != "" {
		pool, pinnedDSN, err := connectAndMigrate(dsn)
		if err != nil {
			return nil, err
		}
		testPool, testDSN = pool, pinnedDSN
		return func() { pool.Close() }, nil
	}

	if os.Getenv("TELEMED_NO_EPHEMERAL_PG") != "" {
		return nil, fmt.Errorf("TEST_DATABASE_URL is unset and ephemeral Postgres is disabled")
	}

	initdb, err := exec.LookPath("initdb")
	if err != nil {
		return nil, fmt.Errorf("TEST_DATABASE_URL is unset and initdb is not on PATH")
	}
	postgresBin, err := exec.LookPath("postgres")
	if err != nil {
		return nil, fmt.Errorf("TEST_DATABASE_URL is unset and the postgres binary is not on PATH")
	}

	// /tmp explicitly, not t.TempDir(): a unix socket path is capped at ~107
	// bytes by the kernel, and Go's temp dirs under a long TMPDIR blow through
	// that with an error that reads as "connection refused".
	dir, err := os.MkdirTemp("/tmp", "telemed-sched-pg-")
	if err != nil {
		return nil, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }

	dataDir := filepath.Join(dir, "data")
	if out, err := exec.Command(initdb, "-D", dataDir, "-U", "telemed",
		"--auth=trust", "-E", "UTF8", "--locale=C").CombinedOutput(); err != nil {
		cleanup()
		return nil, fmt.Errorf("initdb: %w: %s", err, truncate(string(out)))
	}

	// Durability is pointless for a cluster that lives for thirty seconds, and
	// turning it off makes the 100-goroutine test spend its time on lock
	// contention rather than on fsync.
	cmd := exec.Command(postgresBin,
		"-D", dataDir,
		"-k", dir,
		"-h", "", // unix socket only: no TCP port to collide with another agent
		"-c", "fsync=off",
		"-c", "full_page_writes=off",
		"-c", "synchronous_commit=off",
		"-c", "max_connections=300",
		"-c", "log_min_messages=fatal",
	)
	// Postgres writes its own log to a file inside the temp dir rather than to
	// the test output: a clean run should say nothing, and a failed startup is
	// surfaced explicitly below.
	pgLog, err := os.Create(filepath.Join(dir, "postgres.log")) //nolint:gosec // path is our own temp dir
	if err != nil {
		cleanup()
		return nil, err
	}
	cmd.Stdout, cmd.Stderr = pgLog, pgLog
	dieWithParent(cmd)
	if err := cmd.Start(); err != nil {
		_ = pgLog.Close()
		cleanup()
		return nil, fmt.Errorf("start postgres: %w", err)
	}
	logPath := pgLog.Name()

	stop := func() {
		_ = pgLog.Close()
		if cmd.Process != nil {
			_ = cmd.Process.Signal(os.Interrupt) // SIGINT is postgres' fast shutdown
			done := make(chan struct{})
			go func() { _, _ = cmd.Process.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				_ = cmd.Process.Kill()
			}
		}
		cleanup()
	}

	adminDSN := fmt.Sprintf("user=telemed host=%s dbname=postgres sslmode=disable", dir)
	admin, err := waitForPostgres(adminDSN, 30*time.Second)
	if err != nil {
		if b, readErr := os.ReadFile(logPath); readErr == nil {
			err = fmt.Errorf("%w\npostgres log:\n%s", err, truncate(string(b)))
		}
		stop()
		return nil, err
	}
	_, err = admin.Exec(context.Background(), "CREATE DATABASE telemed_scheduling")
	admin.Close()
	if err != nil {
		stop()
		return nil, fmt.Errorf("create database: %w", err)
	}

	// Do not put pool_max_conns on this DSN: EnsureSchema opens it with
	// pgx.Connect, which forwards unknown keywords to the server as runtime
	// parameters and Postgres rejects them. MaxConns is set below via
	// database.Connect's Config.
	dsn := fmt.Sprintf("user=telemed host=%s dbname=telemed_scheduling sslmode=disable", dir)
	pool, pinnedDSN, err := connectAndMigrate(dsn)
	if err != nil {
		stop()
		return nil, err
	}
	testPool, testDSN = pool, pinnedDSN
	return func() { pool.Close(); stop() }, nil
}

func waitForPostgres(dsn string, limit time.Duration) (*pgxpool.Pool, error) {
	deadline := time.Now().Add(limit)
	var lastErr error
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		pool, err := pgxpool.New(ctx, dsn)
		if err == nil {
			err = pool.Ping(ctx)
			if err == nil {
				cancel()
				return pool, nil
			}
			pool.Close()
		}
		cancel()
		lastErr = err
		time.Sleep(100 * time.Millisecond)
	}
	return nil, fmt.Errorf("postgres never became ready: %w", lastErr)
}

// connectAndMigrate applies every *.up.sql in order. The tests run the same SQL
// the migrate CLI runs in production -- not a hand-maintained schema fixture
// that drifts from it.
func connectAndMigrate(dsn string) (*pgxpool.Pool, string, error) {
	// Migrate into svc_scheduling, not public: the slots partition manager
	// qualifies both sides of its CREATE, so a suite running in public would
	// never exercise it. The pinned DSN is returned because callers keep it in
	// testDSN and open further connections from it.
	dsn, err := database.EnsureSchema(context.Background(), dsn, "scheduling")
	if err != nil {
		return nil, "", err
	}

	// database.Connect, not a bare pgxpool.New: it pins the session timezone to
	// UTC exactly as production does. A test pool that inherited the machine's
	// Asia/Colombo session would silently mask the timezone bugs these tests
	// exist to catch.
	pool, err := database.Connect(context.Background(), database.Config{
		URL:      dsn,
		MaxConns: 60,
		MinConns: 2,
		AppName:  "telemed-scheduling-test",
	}, zerolog.Nop())
	if err != nil {
		return nil, "", err
	}

	migDir, err := repopath.FindMigrations("scheduling")
	if err != nil {
		pool.Close()
		return nil, "", err
	}
	files, err := filepath.Glob(filepath.Join(migDir, "*.up.sql"))
	if err != nil {
		pool.Close()
		return nil, "", err
	}
	if len(files) == 0 {
		pool.Close()
		return nil, "", fmt.Errorf("no migrations found")
	}
	sort.Strings(files)

	ctx := context.Background()
	for _, f := range files {
		sql, err := os.ReadFile(f) //nolint:gosec // path comes from a glob of our own repo
		if err != nil {
			pool.Close()
			return nil, "", err
		}
		// pgx uses the simple protocol when a query has no arguments, which is
		// what lets a whole multi-statement migration file go through in one
		// Exec.
		if _, err := pool.Exec(ctx, string(sql)); err != nil {
			pool.Close()
			return nil, "", fmt.Errorf("migration %s: %w", filepath.Base(f), err)
		}
	}
	return pool, dsn, nil
}

func truncate(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 400 {
		return s[:400] + "..."
	}
	return s
}

// requireDB skips a test when no database is available, with the reason.
func requireDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if testPool == nil {
		t.Skipf("no test database: %s", skipDB)
	}
	return testPool
}

// resetTables clears domain state between tests. Truncating rather than
// dropping keeps the partitions and the functions in place, which is what we
// want: the partitioned layout is part of what is under test.
func resetTables(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	const q = `TRUNCATE appointments, slots, waitlists, holidays, working_hours,
	           doctor_schedule_settings, doctor_pricing, no_show_stats,
	           consumed_events, outbox_events, reschedule_requests`
	if _, err := pool.Exec(context.Background(), q); err != nil {
		t.Fatalf("reset tables: %v", err)
	}
}
