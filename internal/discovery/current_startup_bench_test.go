package discovery

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/codexmeta"
	"github.com/wangjohn/agent-archive/internal/local"
)

// Startup/warm selection never runs the shared enumeration epoch. Large SQL
// and catalog fixtures contain metadata only; the selected source is real.
func BenchmarkCurrentStartupFromRestoredCatalog(b *testing.B) {
	store, cfg, at, root := fixture(b)
	wanted := writeRollout(b, root, cfg.Archive.Projects[0].Root, at, 100000, "sessions")
	path := filepath.Join(root, "sessions", "rollout-2026-10-01T12-00-00-"+wanted+".jsonl")
	info, err := os.Stat(path)
	if err != nil {
		b.Fatal(err)
	}
	db := hintDatabase(b, root, true)
	tx, err := db.BeginTx(b.Context(), nil)
	if err != nil {
		b.Fatal(err)
	}
	stmt, err := tx.PrepareContext(b.Context(), "INSERT INTO threads(id,rollout_path) VALUES(?,?)")
	if err != nil {
		b.Fatal(err)
	}
	for n := range 100000 {
		id := fmt.Sprintf("00000000-0000-0000-0000-%012d", n+1)
		if _, err := stmt.ExecContext(b.Context(), id, path); err != nil {
			b.Fatal(err)
		}
	}
	defer func() { _ = stmt.Close() }()
	if err := stmt.Close(); err != nil {
		b.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
	if _, err := db.ExecContext(b.Context(), "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		b.Fatal(err)
	}
	c := catalog{Version: catalogVersion, Roots: []string{root}, Cache: map[string]cached{}}
	for n := range maxCatalog {
		id := fmt.Sprintf("00000000-0000-0000-0000-%012d", n+1)
		key := filepath.Join(root, "sessions", fmt.Sprintf("cached-%d.jsonl", n))
		if n == 0 {
			key = path
			id = wanted
		}
		c.Cache[key] = cached{Size: info.Size(), Mtime: info.ModTime().UnixNano(), Observation: Observation{Identity: &codexmeta.CodexIdentity{ThreadID: id, RolloutID: id}}}
	}
	catalogPath := filepath.Join(store.Home(), "discovery-catalog.json")
	if err := local.WriteCompact(catalogPath, c); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for _, phase := range []string{"startup", "warm"} {
		start := time.Now()
		l, err := NewCodexRolloutLookup(b.Context(), store, []string{root})
		if err != nil {
			b.Fatal(err)
		}
		constructor := time.Since(start)
		start = time.Now()
		set, err := l.Thread(b.Context(), wanted)
		if err != nil || set.Current == nil || set.Current.Path != path {
			b.Fatal(set, err)
		}
		if err := l.Check(b.Context(), wanted, set.Revision); err != nil {
			b.Fatal(err)
		}
		selection := time.Since(start)
		before, err := os.Stat(catalogPath)
		if err != nil {
			b.Fatal(err)
		}
		dirty := l.dirty
		used, peak := l.readBudget.Charged()
		if err := l.Close(); err != nil {
			b.Fatal(err)
		}
		after, err := os.Stat(catalogPath)
		if err != nil {
			b.Fatal(err)
		}
		wrote := !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime())
		if phase == "warm" && (dirty || wrote) {
			b.Fatal("unchanged warm lookup wrote catalog")
		}
		usedAfter, _ := l.readBudget.Charged()
		if usedAfter != 0 {
			b.Fatal("lookup leaked charge", usedAfter)
		}
		b.ReportMetric(float64(constructor.Microseconds()), phase+"-constructor-us")
		b.ReportMetric(float64(selection.Microseconds()), phase+"-current-us")
		b.ReportMetric(float64(peak), phase+"-peak-B")
		b.ReportMetric(float64(used), phase+"-retained-B")
		writes := 0
		if wrote {
			writes = 1
		}
		b.ReportMetric(float64(writes), phase+"-catalog-writes")
	}
	b.StopTimer()
}
