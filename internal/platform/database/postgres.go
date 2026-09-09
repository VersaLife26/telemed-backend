// Package database owns the Postgres connection pool and the migration runner.
//
// We use pgx v5 directly rather than database/sql: it is the only driver that
// exposes CopyFrom (needed for bulk slot generation), native TIMESTAMPTZ
// handling, and prepared-statement caching without CGO.
package database

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"
)

// ErrOptimisticLock is returned by a repository update or delete that targets a
// row by (id, version) when zero rows matched -- either the row is gone, or
// another writer already advanced its version.
//
// Every version-guarded table on the platform shares this one sentinel, so a
// caller can write a single errors.Is check instead of learning each repository's
// private error. Booking conflicts, profile edits, and config updates all
// surface the same way.
var ErrOptimisticLock = errors.New("database: row changed concurrently (optimistic lock)")

// Config carries the pool tuning knobs.
type Config struct {
	URL         string
	MaxConns    int32
	MinConns    int32
	MaxConnLife time.Duration
	AppName     string
}

// Pool is the subset of *pgxpool.Pool the application depends on. Depending on
// the interface rather than the concrete type keeps repositories testable and
// lets us swap the driver in 2040 without touching business logic.
type Pool interface {
	Begin(ctx context.Context) (pgx.Tx, error)
	BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	CopyFrom(ctx context.Context, table pgx.Identifier, cols []string, src pgx.CopyFromSource) (int64, error)
	Ping(ctx context.Context) error
	Close()
}

var _ Pool = (*pgxpool.Pool)(nil)

// Connect opens the pool and verifies it with a ping. It retries briefly so a
// service starting alongside Postgres in docker-compose or Kubernetes does not
// crash-loop on a database that is two seconds from ready.
func Connect(ctx context.Context, cfg Config, log zerolog.Logger) (*pgxpool.Pool, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("database: parse url: %w", err)
	}
	if cfg.MaxConns > 0 {
		poolCfg.MaxConns = cfg.MaxConns
	}
	if cfg.MinConns > 0 {
		poolCfg.MinConns = cfg.MinConns
	}
	if cfg.MaxConnLife > 0 {
		poolCfg.MaxConnLifetime = cfg.MaxConnLife
	}
	poolCfg.MaxConnIdleTime = 5 * time.Minute
	poolCfg.HealthCheckPeriod = 30 * time.Second
	if poolCfg.ConnConfig.RuntimeParams == nil {
		poolCfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	if cfg.AppName != "" {
		poolCfg.ConnConfig.RuntimeParams["application_name"] = cfg.AppName
	}
	// Every service reads and writes wall-clock instants in UTC and converts to
	// Asia/Colombo only at the presentation boundary.
	poolCfg.ConnConfig.RuntimeParams["timezone"] = "UTC"

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("database: create pool: %w", err)
	}

	const attempts = 10
	var pingErr error
	for i := range attempts {
		pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		pingErr = pool.Ping(pingCtx)
		cancel()
		if pingErr == nil {
			log.Info().Int32("max_conns", poolCfg.MaxConns).Msg("postgres connected")
			return pool, nil
		}
		wait := time.Duration(i+1) * 500 * time.Millisecond
		log.Warn().Err(pingErr).Dur("retry_in", wait).Int("attempt", i+1).Msg("postgres not ready")
		select {
		case <-ctx.Done():
			pool.Close()
			return nil, ctx.Err()
		case <-time.After(wait):
		}
	}
	pool.Close()
	return nil, fmt.Errorf("database: ping after %d attempts: %w", attempts, pingErr)
}

// InTx runs fn inside a transaction, committing on success and rolling back on
// any error or panic. Callers never manage tx lifecycle by hand, which is how
// connection leaks and half-committed bookings happen.
func InTx(ctx context.Context, pool Pool, opts pgx.TxOptions, fn func(pgx.Tx) error) (err error) {
	tx, err := pool.BeginTx(ctx, opts)
	if err != nil {
		return fmt.Errorf("database: begin: %w", err)
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback(context.WithoutCancel(ctx))
			panic(p)
		}
		if err != nil {
			if rbErr := tx.Rollback(context.WithoutCancel(ctx)); rbErr != nil && !errors.Is(rbErr, pgx.ErrTxClosed) {
				err = errors.Join(err, rbErr)
			}
		}
	}()

	// Assigning the NAMED return (err =, not err :=) is required, not sloppy:
	// the deferred rollback above inspects err to decide whether to roll back.
	// Shadowing it here would silently commit failed transactions.
	//nolint:gocritic // sloppyReassign is wrong here; see comment above.
	if err = fn(tx); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("database: commit: %w", err)
	}
	return nil
}

// IsUniqueViolation reports whether err is a Postgres 23505. Used to translate
// a race that slipped past the application lock into a clean 409.
func IsUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// UniqueConstraint returns the Postgres constraint / unique-index name for a
// 23505, or "" when err is not a unique violation. Callers branch on the name
// so "email already registered" is not reported as "phone already registered".
func UniqueConstraint(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return pgErr.ConstraintName
	}
	return ""
}

// IsSerializationFailure reports whether err is a 40001/40P01, meaning the
// caller should retry the whole transaction.
func IsSerializationFailure(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && (pgErr.Code == "40001" || pgErr.Code == "40P01")
}
