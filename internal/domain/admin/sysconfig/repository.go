package sysconfig

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"telemed/internal/platform/database"
)

// ErrNotFound means the key has never been set.
var ErrNotFound = errors.New("sysconfig: not found")

// ErrVersionRace means two PUTs for the same key computed the same next
// version number concurrently; UNIQUE(key, version) rejected the loser. The
// caller should retry -- Postgres, not application-level locking, is what
// makes this safe (ADR-007's reasoning applied here: the database is the
// source of truth for "what version comes next").
var ErrVersionRace = errors.New("sysconfig: concurrent update, retry")

type Repository struct {
	pool database.Pool
}

func NewRepository(pool database.Pool) *Repository { return &Repository{pool: pool} }

// Put inserts the next version for key. It never touches an existing row.
func (r *Repository) Put(ctx context.Context, key string, value []byte, updatedBy uuid.UUID, effectiveFrom time.Time) (Config, error) {
	if effectiveFrom.IsZero() {
		effectiveFrom = time.Now().UTC()
	}
	const q = `
		INSERT INTO system_configs (key, value, version, updated_by, effective_from)
		SELECT $1, $2, COALESCE(MAX(version), 0) + 1, $3, $4
		FROM system_configs WHERE key = $1
		RETURNING id, key, value, version, updated_by, effective_from, created_at`

	var updatedByArg any
	if updatedBy != uuid.Nil {
		updatedByArg = updatedBy
	}

	row := r.pool.QueryRow(ctx, q, key, value, updatedByArg, effectiveFrom)
	cfg, err := scan(row)
	if err != nil {
		if database.IsUniqueViolation(err) {
			return Config{}, ErrVersionRace
		}
		return Config{}, err
	}
	return cfg, nil
}

// Current returns the version of key in effect at asOf (defaulting to now):
// the row with the greatest effective_from <= asOf, breaking ties by the
// greatest version. This is a pure read -- there is no "close out the old
// version" write for a concurrent reader to race against.
func (r *Repository) Current(ctx context.Context, key string, asOf time.Time) (Config, error) {
	if asOf.IsZero() {
		asOf = time.Now().UTC()
	}
	const q = `
		SELECT id, key, value, version, updated_by, effective_from, created_at
		FROM system_configs
		WHERE key = $1 AND effective_from <= $2
		ORDER BY effective_from DESC, version DESC
		LIMIT 1`
	row := r.pool.QueryRow(ctx, q, key, asOf)
	cfg, err := scan(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Config{}, ErrNotFound
	}
	return cfg, err
}

// History returns every version of key, newest first.
func (r *Repository) History(ctx context.Context, key string) ([]Config, error) {
	const q = `
		SELECT id, key, value, version, updated_by, effective_from, created_at
		FROM system_configs WHERE key = $1 ORDER BY version DESC`
	rows, err := r.pool.Query(ctx, q, key)
	if err != nil {
		return nil, fmt.Errorf("sysconfig: history: %w", err)
	}
	defer rows.Close()
	return scanAll(rows)
}

// CurrentAll returns the currently-effective version of every key that has
// ever been set, as of now. Backs GET /api/v1/admin/configs (feature-flag
// dashboard, "what is every policy right now").
func (r *Repository) CurrentAll(ctx context.Context) ([]Config, error) {
	const q = `
		SELECT DISTINCT ON (key) id, key, value, version, updated_by, effective_from, created_at
		FROM system_configs
		WHERE effective_from <= NOW()
		ORDER BY key, effective_from DESC, version DESC`
	rows, err := r.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("sysconfig: current all: %w", err)
	}
	defer rows.Close()
	return scanAll(rows)
}

type rowScanner interface{ Scan(dest ...any) error }

func scan(row rowScanner) (Config, error) {
	var (
		c         Config
		updatedBy *uuid.UUID
	)
	if err := row.Scan(&c.ID, &c.Key, &c.Value, &c.Version, &updatedBy, &c.EffectiveFrom, &c.CreatedAt); err != nil {
		return Config{}, fmt.Errorf("sysconfig: scan: %w", err)
	}
	if updatedBy != nil {
		c.UpdatedBy = *updatedBy
	}
	return c, nil
}

func scanAll(rows pgx.Rows) ([]Config, error) {
	var out []Config
	for rows.Next() {
		c, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
