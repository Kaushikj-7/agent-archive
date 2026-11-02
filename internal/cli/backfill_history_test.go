package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/backfill"
	"github.com/wangjohn/agent-archive/internal/state"
)

// history words each state accurately, and neither history nor status fails
// on an unreadable import file.
//
// Regression: backfill B3 review, 2026-09 (e589f65).
func TestBackfillHistoryStates(t *testing.T) {
	t.Parallel()
	f, _ := newImportFixture(t)
	done := backfillNow.UTC()
	for i, b := range []backfill.Batch{
		{ID: "2026-09-20-1"},
		{ID: "2026-09-21-1", CompletedAt: &done},
		{ID: "2026-09-22-1"},
	} {
		b.StartedAt = backfillNow.Add(time.Duration(i-3) * 24 * time.Hour)
		if err := backfill.SaveBatch(f.data, b); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(f.data, "imports", "broken.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, errOut, code := f.command(t, "backfill", "history")
	if code != 0 || !strings.Contains(errOut, "broken") {
		t.Fatalf("code %d, %s", code, errOut)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 4 {
		t.Fatalf("history:\n%s", out)
	}
	for i, want := range []string{"interrupted; 0 registered", "nothing registered", "interrupted; 0 registered; run agent-archive backfill with the same options to finish"} {
		line := lines[i+1]
		if !strings.HasSuffix(line, want) || (i == 0 && strings.Contains(line, "run agent-archive")) {
			t.Errorf("line %d = %q, want it to end %q", i+1, line, want)
		}
	}

	out, errOut, code = f.command(t, "status", "--json")
	if code != 0 {
		t.Fatalf("status: %s", errOut)
	}
	var view statusView
	if err := json.Unmarshal([]byte(out), &view); err != nil {
		t.Fatal(err)
	}
	if view.LastImport != "2026-09-22-1" || !strings.Contains(strings.Join(view.Warnings, "\n"), "broken") {
		t.Fatalf("last import %q, warnings %v", view.LastImport, view.Warnings)
	}
}

// With every import file unreadable, history says so instead of printing an
// empty table, and an import refuses without changing anything.
//
// Regression: backfill B3 review, 2026-09 (e589f65).
func TestBackfillAllImportsUnreadable(t *testing.T) {
	t.Parallel()
	f, _ := newImportFixture(t)
	if err := os.MkdirAll(filepath.Join(f.data, "imports"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.data, "imports", "broken.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, errOut, code := f.command(t, "backfill", "history")
	if code != 1 || !strings.Contains(out, "No import could be read") || strings.Contains(out, "IMPORT") || !strings.Contains(errOut, "broken") {
		t.Fatalf("code %d\n%s\n%s", code, out, errOut)
	}
	cfgBefore, _ := os.ReadFile(filepath.Join(f.data, "config.json"))
	_, errOut, code = f.importRun(t, nil, false, "--yes")
	if code != 1 || !strings.Contains(errOut, "Nothing was changed") || !strings.Contains(errOut, "imports") {
		t.Fatalf("code %d, %s", code, errOut)
	}
	if cfgAfter, _ := os.ReadFile(filepath.Join(f.data, "config.json")); !bytes.Equal(cfgBefore, cfgAfter) {
		t.Fatal("config changed")
	}
	if parents, _ := importRegistrations(t, f.data, firstImport); len(parents) != 0 {
		t.Fatal("sessions registered")
	}
}

// History counts a batch's sessions as the batch records them: a native child
// it registered is one of its subagents, not another session.
func TestBackfillHistoryCountsNativeChildrenAsSubagents(t *testing.T) {
	t.Parallel()
	f, _ := newImportFixture(t)
	id := "2026-09-22-1"
	if err := backfill.SaveBatch(f.data, backfill.Batch{ID: id, StartedAt: backfillNow.Add(-time.Hour), Sessions: []string{"parent"}, Subagents: []string{"native-child"}}); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(f.data)
	if err != nil {
		t.Fatal(err)
	}
	for _, reg := range []archive.SessionRegistration{
		{ArchiveSessionID: "parent", NativeSessionID: "n-parent"},
		{ArchiveSessionID: "native-child", NativeSessionID: "n-child", NativeChild: true, ParentNativeSessionID: "n-parent"},
	} {
		reg.ProjectID, reg.ProjectRoot, reg.Harness = "p", "/p", archive.Harness{Name: "codex"}
		reg.SessionStartedAt, reg.ImportBatch, reg.Origin = backfillNow, archive.NewImportBatch(id), archive.SessionOriginImport
		if err := store.SaveRegistration(reg); err != nil {
			t.Fatal(err)
		}
	}
	out, errOut, code := f.command(t, "backfill", "history")
	if code != 0 {
		t.Fatalf("code %d, %s", code, errOut)
	}
	for line := range strings.SplitSeq(out, "\n") {
		if fields := strings.Fields(line); len(fields) > 3 && fields[0] == id {
			if fields[3] != "1" || !strings.Contains(line, "interrupted; 1 registered") {
				t.Fatalf("history row counts the native child as a session: %q", line)
			}
			return
		}
	}
	t.Fatalf("history has no row for the import: %s", out)
}
