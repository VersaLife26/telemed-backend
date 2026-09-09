package database

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/jackc/pgx/v5"
)

// EnsureSchema provisions one domain's schema on a database and returns a DSN
// pinned to it.
//
// WHY TESTS NEED THIS
// Before consolidation each service owned a whole database, so an integration
// test could point a fresh container at `public` and be running the same shape
// as production. That is no longer true: production now puts all eight domains
// in one database separated by schema, and four migrations had to be rewritten
// for the move -- two SECURITY DEFINER functions whose pinned search_path
// names the domain schema, and the slots partition manager, which qualifies
// both sides of its CREATE.
//
// A test still running in `public` would not execute any of that. It would
// pass while production failed. So the helpers create the domain's schema and
// migrate into it, exactly as the deployment does.
//
// The returned DSN carries search_path as a connection parameter rather than a
// `SET` statement, because a pool opens many connections and a SET reaches
// only the one that ran it -- the classic form of this bug is a test that
// passes until the pool grows past one connection.
func EnsureSchema(ctx context.Context, dsn, domain string) (string, error) {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return "", fmt.Errorf("database: connect to provision schema: %w", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	schema := Schema(domain)
	// pgx.Identifier quotes correctly; the name is ours, but building DDL by
	// concatenation is how the next person introduces an injection.
	ident := pgx.Identifier{schema}.Sanitize()
	if _, err := conn.Exec(ctx, "CREATE SCHEMA IF NOT EXISTS "+ident); err != nil {
		return "", fmt.Errorf("database: create schema %s: %w", schema, err)
	}
	// Shared extensions live in public, which stays on the search_path. The
	// domains that ask for pgcrypto in their own migrations then find it
	// already present -- an extension is a database-global object, so a second
	// CREATE elsewhere would fail rather than duplicate.
	if _, err := conn.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS pgcrypto SCHEMA public"); err != nil {
		return "", fmt.Errorf("database: create pgcrypto: %w", err)
	}
	return WithSearchPath(dsn, schema+",public")
}

// WithSearchPath returns dsn with search_path set as a connection parameter,
// replacing one already present.
//
// Handles both DSN forms libpq accepts, because the test helpers use both: the
// URL form ("postgres://user@host/db?sslmode=disable") and the keyword form
// ("user=telemed host=/tmp/... dbname=telemed_scheduling"). Feeding a keyword
// DSN to url.Parse does not error -- it yields an opaque URL and silently
// drops the parameter -- so the form is detected rather than assumed.
func WithSearchPath(dsn, searchPath string) (string, error) {
	if !strings.HasPrefix(dsn, "postgres://") && !strings.HasPrefix(dsn, "postgresql://") {
		// Keyword form. Quoting matters: the value contains a comma and a
		// space ("svc_user, public"), which libpq would otherwise read as the
		// end of the value.
		return dsn + " search_path='" + strings.ReplaceAll(searchPath, "'", "\\'") + "'", nil
	}
	u, err := url.Parse(dsn)
	if err != nil {
		return "", fmt.Errorf("database: parse dsn: %w", err)
	}
	q := u.Query()
	q.Set("search_path", searchPath)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// SearchPathFor is the search_path a domain's connections should carry.
func SearchPathFor(domain string) string {
	return strings.Join([]string{Schema(domain), "public"}, ", ")
}
