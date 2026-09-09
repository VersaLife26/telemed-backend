// Package repopath resolves paths inside this repository from test code.
//
// WHY THIS EXISTS
// Before consolidation each service was its own repository, so a test could
// reach its migrations with a hard-coded "../../migrations". In one module the
// depth from a package to the repository root is no longer uniform --
// internal/domain/doctor/analytics is four levels down, internal/gateway is
// two -- and a miscounted "../.." does not fail at compile time. It fails at
// run time, in an integration test, as "no migrations found", which reads like
// a broken database rather than a broken path.
//
// Root walks up from the calling file to the directory holding go.mod, so the
// answer is correct wherever the package is moved to. That matters again in
// the phase that collapses the nine binaries into one.
package repopath

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// FindRoot returns the repository root, or an error. Use it from TestMain and
// other helpers that have no *testing.T to fail on.
func FindRoot() (string, error) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("repopath: could not resolve caller")
	}
	dir := filepath.Dir(file)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("repopath: no go.mod found above %s", file)
		}
		dir = parent
	}
}

// FindMigrations is Migrations without a testing.TB.
func FindMigrations(domain string) (string, error) {
	root, err := FindRoot()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(root, "migrations", domain)
	if _, err := os.Stat(dir); err != nil {
		return "", fmt.Errorf("repopath: migrations dir not found at %s: %w", dir, err)
	}
	return dir, nil
}

// Root returns the absolute path of the repository root -- the directory
// containing go.mod.
func Root(tb testing.TB) string {
	tb.Helper()
	dir, err := FindRoot()
	if err != nil {
		tb.Fatal(err)
	}
	return dir
}

// Migrations returns the migrations directory for one domain, e.g.
// Migrations(t, "payment") -> <root>/migrations/payment.
func Migrations(tb testing.TB, domain string) string {
	tb.Helper()
	dir, err := FindMigrations(domain)
	if err != nil {
		tb.Fatal(err)
	}
	return dir
}

// File returns an absolute path to a file given relative to the repository
// root, e.g. File(t, "deploy/env/payment.env.example").
func File(tb testing.TB, rel ...string) string {
	tb.Helper()
	return filepath.Join(append([]string{Root(tb)}, rel...)...)
}
