package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/backfill"
	"github.com/wangjohn/agent-archive/internal/discovery"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/testutil/musefixture"
)

func TestBackfillNativeImportReportsOneSessionInTheSingular(t *testing.T) {
	f := newPublicNativeImportFixture(t)
	out, stderr, code := f.run(nil, "--harness", "muse", "--yes", "--background")
	if code != 0 || !strings.Contains(out, ": 1 historical Muse session registered.") {
		t.Fatal(code, out, stderr)
	}
	batches, err := backfill.LoadBatches(f.home)
	if err != nil || len(batches) != 1 {
		t.Fatal(batches, err)
	}
	out, stderr, code = f.run(nil, "undo", batches[0].ID, "--yes")
	if code != 0 || !strings.Contains(out, ": detach 1 exact historical registration membership.") || !strings.Contains(out, "Detached 1 import membership.") {
		t.Fatal(code, out, stderr)
	}
}

func TestBackfillHistoryCountsSessionsRegisteredBeforeAnInterruptedSave(t *testing.T) {
	f := newPublicNativeImportFixture(t)
	f.env.backfillCheckpoint = func(step string) error {
		if step == "native import registered" {
			return errors.New("synthetic interrupted batch progress")
		}
		return nil
	}
	if _, _, code := f.run(nil, "--harness", "muse", "--yes", "--background"); code != 1 {
		t.Fatal("interruption not reached", code)
	}
	// The registration is durable before the batch records it; an interruption
	// between the two leaves the batch one session behind.
	batches, err := backfill.LoadBatches(f.home)
	if err != nil || len(batches) != 1 || len(batches[0].Sessions) != 1 {
		t.Fatal(batches, err)
	}
	batches[0].Sessions = nil
	if err := backfill.SaveBatch(f.home, batches[0]); err != nil {
		t.Fatal(err)
	}
	out, stderr, code := f.run(nil, "history")
	if code != 0 {
		t.Fatal(code, out, stderr)
	}
	for line := range strings.SplitSeq(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) > 3 && fields[0] == batches[0].ID {
			if fields[3] != "1" || !strings.Contains(line, "1 registered") {
				t.Fatalf("history row disagrees with registrations: %q", line)
			}
			return
		}
	}
	t.Fatalf("history has no row for the import: %s", out)
}

func TestBackfillNativeImportPreviewSaysWhyEachSourceIsRefused(t *testing.T) {
	t.Parallel()
	f := newPublicNativeImportFixture(t)
	raw, err := os.ReadFile(filepath.Join("..", "agents", "muse", "testdata", "synthetic-exec-conversation.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSuffix(raw, []byte("\n")), []byte("\n"))
	sideLog := "00000000-0000-7000-8000-00000000000a"
	frameOnly := "00000000-0000-7000-8000-00000000000b"
	contradicting := "00000000-0000-7000-8000-00000000000e"
	opened := bytes.Split(musefixture.Rewrite(t, raw, contradicting, f.project), []byte("\n"))
	// The opening names another session than the file's owner.
	opened[2] = bytes.Replace(opened[2], []byte(`"session_id":"`+contradicting+`"`), []byte(`"session_id":"`+sideLog+`"`), 1)
	for id, content := range map[string][]byte{
		contradicting: bytes.Join(opened, []byte("\n")),
		// A side log starts directly with ordinary records.
		sideLog: bytes.ReplaceAll(append(bytes.Join(lines[1:3], []byte("\n")), '\n'), []byte(publicNativeImportID), []byte(sideLog)),
		// Only the permission frame was written.
		frameOnly: musefixture.Rewrite(t, append(bytes.Clone(lines[0]), '\n'), frameOnly, f.project),
	} {
		path := filepath.Join(f.native, "2026", "10", "15", id, "session.jsonl")
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	out, stderr, code := f.run(nil, "--harness", "muse", "--dry-run", "--json", "--project", f.project)
	var result struct {
		Sessions []nativeImportRow `json:"sessions"`
	}
	if err := json.Unmarshal([]byte(out), &result); code != 0 || err != nil {
		t.Fatal(code, err, out, stderr)
	}
	reasons := map[nativeImportStatus][]string{}
	for _, row := range result.Sessions {
		reasons[row.Status] = append(reasons[row.Status], row.Reason)
	}
	refused := strings.Join(reasons[nativeImportRefused], "\n")
	if len(reasons[nativeImportSelected]) != 1 || len(reasons[nativeImportRefused]) != 3 || !strings.Contains(refused, "not a session: a Muse side log with no session start; nothing to import") || !strings.Contains(refused, "incomplete: the file ends before its startup records; nothing to import") || !strings.Contains(refused, "inconsistent: Muse record session.opened.observed at line 3 contradicts the session's other records; nothing to import") {
		t.Fatalf("preview reasons: %+v", result.Sessions)
	}
}

func TestBackfillNativeImportFilesARemovedWorktreeUnderItsProject(t *testing.T) {
	t.Parallel()
	f := newPublicNativeImportFixture(t)
	raw, err := os.ReadFile(filepath.Join("..", "agents", "muse", "testdata", "synthetic-exec-conversation.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(f.project, ".claude", "worktrees"), 0o700); err != nil {
		t.Fatal(err)
	}
	removed := "00000000-0000-7000-8000-00000000000c"
	elsewhere := "00000000-0000-7000-8000-00000000000d"
	for id, cwd := range map[string]string{
		removed:   filepath.Join(f.project, ".claude", "worktrees", "removed-task"),
		elsewhere: filepath.Join(t.TempDir(), "gone", "removed-task"),
	} {
		path := filepath.Join(f.native, "2026", "10", "15", id, "session.jsonl")
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, musefixture.Rewrite(t, raw, id, cwd), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	out, stderr, code := f.run(nil, "--harness", "muse", "--dry-run", "--json", "--project", f.project)
	var result struct {
		Sessions []nativeImportRow `json:"sessions"`
	}
	if err := json.Unmarshal([]byte(out), &result); code != 0 || err != nil {
		t.Fatal(code, err, out, stderr)
	}
	removedRows, refusedRows := 0, 0
	for _, row := range result.Sessions {
		switch {
		case row.Status == nativeImportSelected && strings.Contains(row.Reason, "removed worktree"):
			if row.Project != f.project {
				t.Fatalf("removed worktree filed under %q", row.Project)
			}
			removedRows++
		case row.Status == nativeImportRefused && strings.Contains(row.Reason, "physical project/worktree unavailable"):
			refusedRows++
		}
	}
	if removedRows != 1 || refusedRows != 1 {
		t.Fatalf("preview rows: %s", out)
	}
}

// withAuthorityRecordBeforeEnd inserts an authority-bearing record the
// startup preview cannot see, before the session end.
func withAuthorityRecordBeforeEnd(t *testing.T, raw []byte) []byte {
	t.Helper()
	lines := bytes.Split(bytes.TrimSuffix(raw, []byte("\n")), []byte("\n"))
	var end map[string]any
	if err := json.Unmarshal(lines[len(lines)-1], &end); err != nil || end["payload_type"] != "session.end" {
		t.Fatal("synthetic session end changed", err)
	}
	extra := maps.Clone(end)
	extra["id"] = "00000000-0000-4000-8000-0000000000e9"
	extra["causation_id"] = nil
	extra["payload_type"], extra["payload_schema_version"] = "session.permission_grant.recorded", 1
	extra["payload"] = map[string]any{"kind": "permission_grant"}
	end["sequence"], end["recorded_at"] = end["sequence"].(float64)+1, end["recorded_at"].(float64)+1
	a, err := json.Marshal(extra)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(end)
	if err != nil {
		t.Fatal(err)
	}
	lines = append(lines[:len(lines)-1], a, b)
	return append(bytes.Join(lines, []byte("\n")), '\n')
}

func TestBackfillNativeImportSkipsAnEntryItsFinalReadRefuses(t *testing.T) {
	t.Parallel()
	f := newPublicNativeImportFixture(t)
	raw, err := os.ReadFile(filepath.Join("..", "agents", "muse", "testdata", "synthetic-exec-conversation.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	refused := "00000000-0000-7000-8000-0000000000e1"
	path := filepath.Join(f.native, "2026", "10", "15", refused, "session.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, withAuthorityRecordBeforeEnd(t, musefixture.Rewrite(t, raw, refused, f.project)), 0o600); err != nil {
		t.Fatal(err)
	}
	// The startup preview selects both sessions; it cannot see the record,
	// whose permission word keeps it authority-bearing.
	preview, stderr, code := f.run(nil, "--harness", "muse", "--dry-run", "--json")
	if code != 0 || strings.Count(preview, `"status":"selected"`) != 2 {
		t.Fatal("preview", code, preview, stderr)
	}
	out, stderr, code := f.run(nil, "--harness", "muse", "--yes", "--background")
	if code != 0 || !strings.Contains(out, "1 historical Muse session registered, 1 skipped.") || !strings.Contains(out, "skipped: unsupported Muse record session.permission_grant.recorded") {
		t.Fatal("confirmed import", code, out, stderr)
	}
	batches, err := backfill.LoadBatches(f.home)
	if err != nil || len(batches) != 1 || batches[0].CompletedAt == nil || len(batches[0].Sessions) != 1 {
		t.Fatal("batch did not complete past the refused entry", batches, err)
	}
	store, err := state.Open(f.home)
	if err != nil {
		t.Fatal(err)
	}
	if _, found, err := store.ArchiveSessionID(agentmeta.SessionKey{Agent: agentmeta.Muse, NativeID: refused}); err != nil || found {
		t.Fatal("skipped entry left an owner or reservation", found, err)
	}
}

func TestBackfillNativeImportPreviewLeavesOutAnEntryABatchSkippedUntilItChanges(t *testing.T) {
	t.Parallel()
	f := newPublicNativeImportFixture(t)
	raw, err := os.ReadFile(filepath.Join("..", "agents", "muse", "testdata", "synthetic-exec-conversation.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	refused := "00000000-0000-7000-8000-0000000000e4"
	path := filepath.Join(f.native, "2026", "10", "15", refused, "session.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	source := withAuthorityRecordBeforeEnd(t, musefixture.Rewrite(t, raw, refused, f.project))
	if err := os.WriteFile(path, source, 0o600); err != nil {
		t.Fatal(err)
	}
	out, stderr, code := f.run(nil, "--harness", "muse", "--yes", "--background")
	if code != 0 || !strings.Contains(out, "1 skipped.") {
		t.Fatal("confirmed import", code, out, stderr)
	}
	reason := "unsupported Muse record session.permission_grant.recorded"
	// The next preview reports the skipped entry as refused, with the reason
	// the batch printed, and confirms no batch that would only skip it again.
	preview, stderr, code := f.run(nil, "--harness", "muse", "--dry-run")
	if code != 0 || strings.Contains(preview, "selected") || !strings.Contains(preview, "refused  "+reason) {
		t.Fatal("preview after skip", code, preview, stderr)
	}
	out, stderr, code = f.run(nil, "--harness", "muse", "--yes", "--background")
	if batches, err := backfill.LoadBatches(f.home); code != 0 || err != nil || len(batches) != 1 {
		t.Fatal("unchanged skipped entry started another batch", code, out, stderr, batches, err)
	}
	// A version other than the one that refused it reads it again.
	control := filepath.Join(f.home, "imports", f.env.now().Format("2006-01-02")+"-1.json")
	recorded, err := os.ReadFile(control)
	if err != nil || bytes.Count(recorded, []byte(`"version": "dev"`)) != 1 {
		t.Fatal("skip version not recorded", err, string(recorded))
	}
	if err := os.WriteFile(control, bytes.Replace(recorded, []byte(`"version": "dev"`), []byte(`"version": "0.0.1"`), 1), 0o600); err != nil {
		t.Fatal(err)
	}
	if preview, stderr, code := f.run(nil, "--harness", "muse", "--dry-run"); code != 0 || strings.Count(preview, "selected") != 1 {
		t.Fatal("preview after a version change", code, preview, stderr)
	}
	if err := os.WriteFile(control, recorded, 0o600); err != nil {
		t.Fatal(err)
	}
	// So does a changed source.
	if err := os.WriteFile(path, append(source, source[len(source)-1]), 0o600); err != nil {
		t.Fatal(err)
	}
	if preview, stderr, code := f.run(nil, "--harness", "muse", "--dry-run"); code != 0 || strings.Count(preview, "selected") != 1 {
		t.Fatal("preview after a source change", code, preview, stderr)
	}
}

func TestBackfillNativeImportRenewsASpentInventoryPassMidBatch(t *testing.T) {
	t.Parallel()
	f := newPublicNativeImportFixture(t)
	raw, err := os.ReadFile(filepath.Join("..", "agents", "muse", "testdata", "synthetic-exec-conversation.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"00000000-0000-7000-8000-0000000000e2", "00000000-0000-7000-8000-0000000000e3"} {
		path := filepath.Join(f.native, "2026", "10", "15", id, "session.jsonl")
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, musefixture.Rewrite(t, raw, id, f.project), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// The second row finds the preview's inventory pass spent, once; the
	// third must then locate through the renewed pass.
	rows, spent := 0, false
	f.env.backfillCheckpoint = func(step string) error {
		if step != "native import row" {
			return nil
		}
		rows++
		if rows == 2 && !spent {
			spent = true
			return agentapi.Wrap(agentapi.Unavailable, discovery.ErrNativeInventorySpent)
		}
		return nil
	}
	out, stderr, code := f.run(nil, "--harness", "muse", "--yes", "--background")
	if code != 0 || !spent || !strings.Contains(out, "3 historical Muse sessions registered.") {
		t.Fatal("confirmed import", code, spent, out, stderr)
	}
	batches, err := backfill.LoadBatches(f.home)
	if err != nil || len(batches) != 1 || batches[0].CompletedAt == nil || len(batches[0].Sessions) != 3 {
		t.Fatal("batch did not complete after renewing its inventory", batches, err)
	}
}

func TestBackfillNativeImportAsksAboutOneSessionInTheSingular(t *testing.T) {
	t.Parallel()
	f := newPublicNativeImportFixture(t)
	f.env.IsTerminal = func(any) bool { return true }
	out, stderr, code := f.run(strings.NewReader("\n"), "--harness", "muse", "--background")
	if code != 0 || !strings.Contains(out, "Import this historical Muse session?") || !strings.Contains(out, "Confirm exactly this session") || strings.Contains(out, "these sessions") {
		t.Fatal(code, out, stderr)
	}
}

func TestNativeImportReadReasonSaysWhatToDoNext(t *testing.T) {
	t.Parallel()
	for err, want := range map[error]string{
		// The same scope spends the allowance the same way again.
		agentapi.Wrap(agentapi.Unavailable, discovery.ErrNativeStartupAllowance):  "narrow it with --since and --until",
		agentapi.ReadBudgetLimit(errors.New("synthetic budget")):                  "narrow it with --since and --until",
		agentapi.Wrap(agentapi.Changed, errors.New("synthetic change")):           "source changed during this preview; run the preview again",
		agentapi.Wrap(agentapi.Unavailable, errors.New("synthetic read failure")): "source could not be read during this preview; run the preview again",
	} {
		if got := nativeImportReadReason(err); !strings.HasSuffix(got, want) {
			t.Fatalf("%v: %q, want it to end %q", err, got, want)
		}
	}
}
