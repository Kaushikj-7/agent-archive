package search

import (
	"context"
	"database/sql"
	"errors"
	"github.com/wangjohn/agent-archive/internal/agents/builtin"
	"github.com/wangjohn/agent-archive/internal/archive"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func oldSearchIndex(tb testing.TB, count int) string {
	tb.Helper()
	dir := tb.TempDir()
	s, err := Open(tb.Context(), dir, "synthetic")
	if err != nil {
		tb.Fatal(err)
	}
	_, err = s.db.ExecContext(tb.Context(), `WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i<?) INSERT INTO search_sessions SELECT printf('claude/session-%08d',i),'p','claude',0,0,0,'synthetic','hash','[]','repo','project',printf('session-%08d',i) FROM n`, count)
	if err != nil {
		tb.Fatal(err)
	}
	for _, statement := range []string{"DROP TABLE search_discovery", "DROP TABLE search_observed", "DROP INDEX search_coverage_gaps", "PRAGMA user_version=2"} {
		if _, err = s.db.ExecContext(tb.Context(), statement); err != nil {
			tb.Fatal(err)
		}
	}
	if err = s.Close(); err != nil {
		tb.Fatal(err)
	}
	return dir
}

func TestSearchMigrationCancellationLeavesRetryableSchema(t *testing.T) {
	t.Parallel()
	dir := oldSearchIndex(t, 20000)
	ctx, cancel := context.WithTimeout(t.Context(), time.Millisecond)
	defer cancel()
	s, err := Open(ctx, dir, "synthetic")
	if s != nil {
		_ = s.Close()
		t.Fatal("canceled migration unexpectedly opened")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("migration cancellation: %v", err)
	}
	raw, err := sql.Open("sqlite", filepath.Join(dir, "content.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	var version int
	err = raw.QueryRowContext(t.Context(), "PRAGMA user_version").Scan(&version)
	_ = raw.Close()
	if err != nil || version != 2 {
		t.Fatalf("canceled migration committed partial schema: version=%d %v", version, err)
	}
	started := time.Now()
	s, err = Open(t.Context(), dir, "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	var count, seeded int
	if err = s.db.QueryRowContext(t.Context(), "SELECT count(*) FROM search_sessions").Scan(&count); err != nil || count != 20000 {
		t.Fatalf("migration lost sessions: %d %v", count, err)
	}
	if err = s.db.QueryRowContext(t.Context(), "SELECT seeded FROM search_discovery").Scan(&seeded); err != nil || seeded != 0 {
		t.Fatalf("migration eagerly seeded identities: %d %v", seeded, err)
	}
	t.Logf("20k-row schema2 migration after interrupted attempt: %s; identity seeding remains pending", time.Since(started))
}

func BenchmarkSearchSchemaMigrationAndWarmOpen(b *testing.B) {
	for _, count := range []int{20000, 100000} {
		b.Run(strconv.Itoa(count), func(b *testing.B) {
			b.Run("migration", func(b *testing.B) {
				for range b.N {
					b.StopTimer()
					dir := oldSearchIndex(b, count)
					b.StartTimer()
					s, err := Open(b.Context(), dir, "synthetic")
					if err != nil {
						b.Fatal(err)
					}
					if err = s.Close(); err != nil {
						b.Fatal(err)
					}
				}
			})
			b.Run("warm", func(b *testing.B) {
				dir := oldSearchIndex(b, count)
				s, err := Open(b.Context(), dir, "synthetic")
				if err != nil {
					b.Fatal(err)
				}
				_ = s.Close()
				b.ResetTimer()
				for range b.N {
					s, err = Open(b.Context(), dir, "synthetic")
					if err != nil {
						b.Fatal(err)
					}
					if err = s.Close(); err != nil {
						b.Fatal(err)
					}
				}
			})
		})
	}
}

func TestSearchMigrationValidatesKeysInsertedBehindCheckCursor(t *testing.T) {
	t.Parallel()
	dir := oldSearchIndex(t, 8)
	raw, err := sql.Open("sqlite", filepath.Join(dir, "content.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"-child", "-fork"} {
		_, err = raw.ExecContext(t.Context(), `INSERT INTO search_sessions SELECT id||?,project,harness,captured,replay,child,title,projection_hash,gaps,repo,project_name,session_id||? FROM search_sessions WHERE session_id='session-00000008'`, suffix, suffix)
		if err != nil {
			t.Fatal(err)
		}
	}
	_ = raw.Close()
	a := newSearchArchive(t)
	m := a.session(t, "authority")
	key, _ := archive.MetadataObjectKey(m.Harness.Name, m.SessionID)
	a.mutate(t, key, nil)
	s, err := Open(t.Context(), dir, "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	for range 2 {
		if _, err = s.Reconcile(t.Context(), a.remote, builtin.NewBuiltins(), m.CapturedAt, ReconcileBudget{Entries: 100, Checks: 8}); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err = s.db.QueryRowContext(t.Context(), "SELECT count(*) FROM search_sessions").Scan(&count); err != nil || count != 0 {
		t.Fatalf("seeded keys behind validation cursor were skipped: count=%d %v", count, err)
	}
}

func TestSearchSchemaThreeAddsFairDiscoveryTurnWithoutResettingProgress(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s, err := Open(t.Context(), dir, "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"ALTER TABLE search_discovery DROP COLUMN turn", "UPDATE search_discovery SET after_key='checkpoint',seeded=3", "PRAGMA user_version=3"} {
		if _, err = s.db.ExecContext(t.Context(), q); err != nil {
			t.Fatal(err)
		}
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(t.Context(), dir, "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	var after string
	var turn, seeded, version int
	if err = s.db.QueryRowContext(t.Context(), "SELECT after_key,turn,seeded FROM search_discovery").Scan(&after, &turn, &seeded); err != nil || after != "checkpoint" || turn != 0 || seeded != 3 {
		t.Fatalf("migration reset progress: %s %d %d %v", after, turn, seeded, err)
	}
	if err = s.db.QueryRowContext(t.Context(), "PRAGMA user_version").Scan(&version); err != nil || version != 4 {
		t.Fatalf("schema=%d %v", version, err)
	}
}
