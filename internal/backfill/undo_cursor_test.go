package backfill

import (
	"github.com/wangjohn/agent-archive/internal/agents/builtin"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/state"
)

// Undo reads a Cursor database chat's lastUpdatedAt to tell whether it was
// resumed. Changed after the admission, it was; gone, it was not; with the
// database unreadable (an unfinished write Cursor holds), undo says it could
// not check rather than silently counting the chat as not resumed.
func TestUndoResumedCursorDatabaseChat(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	admitted := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	reg := func(id string) archive.SessionRegistration {
		return archive.SessionRegistration{ArchiveSessionID: "s-" + id, NativeSessionID: id, Harness: archive.Harness{Name: "cursor"},
			SourceKind: archive.SourceKindCursorSQLite, SourceKey: id, AdmittedAt: admitted, Origin: archive.SessionOriginImport}
	}
	db := CursorStateDatabase(home)
	writeCursorDB(t, db, true, mergeRows(
		chatRows("old", map[string]any{"lastUpdatedAt": admitted.Add(-time.Hour).UnixMilli()}, "a"),
		chatRows("new", map[string]any{"lastUpdatedAt": admitted.Add(time.Hour).UnixMilli()}, "a"),
	))
	env := Environment{Sources: testSources, Discovery: testImportDiscovery(), DatabaseCatalogs: builtin.NewBuiltins(), NativePaths: builtin.NewBuiltins(), Worktrees: builtin.NewBuiltins(), Workspaces: builtin.NewBuiltins(), Children: builtin.NewBuiltins(), Imports: builtin.NewBuiltins(), Home: home}
	for id, want := range map[string][2]bool{"old": {false, false}, "new": {true, false}, "gone": {false, false}} {
		resumed, unknown, err := resumedSinceImport(env, store, reg(id), state.Request{})
		if err != nil || resumed != want[0] || unknown != want[1] {
			t.Errorf("%s: resumed %v, unknown %v, err %v", id, resumed, unknown, err)
		}
	}

	if err := os.WriteFile(db+"-journal", []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	resumed, unknown, err := resumedSinceImport(env, store, reg("new"), state.Request{})
	if err != nil || resumed || !unknown {
		t.Fatalf("locked: resumed %v, unknown %v, err %v", resumed, unknown, err)
	}
	p := UndoPlan{Sessions: []UndoSession{{Registration: reg("new"), InCurrentDestination: true, ResumeUnknown: true}}}
	var out strings.Builder
	RenderUndo(&out, p)
	if !strings.Contains(out.String(), "Whether 1 Cursor chat was resumed since the import could not be checked:\n    Cursor's database could not be read.") {
		t.Fatalf("no unchecked line:\n%s", out.String())
	}
}
