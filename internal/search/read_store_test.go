package search

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestSearchReadOpenNeverCreatesOrMigrates(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "missing")
	if _, err := OpenRead(t.Context(), dir, "destination"); !os.IsNotExist(err) {
		t.Fatalf("missing: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("created missing directory: %v", err)
	}
	index, err := Open(t.Context(), dir, "destination")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = index.Close() }()
	if _, err = index.db.ExecContext(t.Context(), "PRAGMA user_version=2"); err != nil {
		t.Fatal(err)
	}
	if _, err = OpenRead(t.Context(), dir, "destination"); !errors.Is(err, ErrUpgradeRequired) {
		t.Fatalf("upgrade: %v", err)
	}
	var version int
	if err = index.db.QueryRowContext(t.Context(), "PRAGMA user_version").Scan(&version); err != nil || version != 2 {
		t.Fatalf("readonly migrated: %d %v", version, err)
	}
}

func TestSearchCachedOpenDoesNotRequireWriterAdmission(t *testing.T) {
	dir := t.TempDir()
	index, err := Open(t.Context(), dir, "destination")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = index.Close() }()
	if _, err = index.db.ExecContext(t.Context(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = index.db.ExecContext(t.Context(), "ROLLBACK") }()
	cached, err := OpenRead(t.Context(), dir, "destination")
	if err != nil {
		t.Fatalf("cached reader blocked on writer: %v", err)
	}
	defer func() { _ = cached.Close() }()
	if _, err = cached.Observation(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestSearchRebuildInvalidatesGenerationAtomically(t *testing.T) {
	index, err := Open(t.Context(), t.TempDir(), "destination")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = index.Close() }()
	if _, err = index.db.ExecContext(t.Context(), "UPDATE search_state SET generation=123"); err != nil {
		t.Fatal(err)
	}
	if err = index.Rebuild(t.Context()); err != nil {
		t.Fatal(err)
	}
	var generation int
	if err = index.db.QueryRowContext(t.Context(), "SELECT generation FROM search_state").Scan(&generation); err != nil || generation != 124 {
		t.Fatalf("generation %d %v", generation, err)
	}
	o, err := index.Observation(t.Context())
	if err != nil || o.Complete || o.Known != 0 {
		t.Fatalf("coverage %+v %v", o, err)
	}
}
