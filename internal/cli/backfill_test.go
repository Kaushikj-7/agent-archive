package cli

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/platform"
	"github.com/wangjohn/agent-archive/internal/setupjournal"
	"github.com/wangjohn/agent-archive/internal/testutil/golden"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/backfill"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/state"
)

// backfillNow is the planning clock: 2026-09-23 in California.
var backfillNow = time.Date(2026, 9, 23, 12, 0, 0, 0, time.FixedZone("PDT", -7*3600))

// backfillFixture is a Mac with history in all three apps. Every transcript
// is synthetic, shaped like internal/archive/testdata's fixtures.
type backfillFixture struct {
	env      Env
	root     string
	userHome string
	data     string
	// nativeIDs are every native session ID in the tree; none may be printed.
	nativeIDs []string
}

func newBackfillFixture(t *testing.T) *backfillFixture {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &backfillFixture{root: root, userHome: filepath.Join(root, "home"), data: filepath.Join(root, "data")}
	for _, dir := range []string{f.userHome, f.data, filepath.Join(root, "tmp")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(f.data, 0700); err != nil {
		t.Fatal(err)
	}
	repo := func(name string) string {
		dir := filepath.Join(f.userHome, name)
		if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	agentArchive, levenshtein, website := repo("agent-archive"), repo("levenshtein"), repo("personal-website")
	scratch := filepath.Join(f.userHome, "Library", "Application Support", "Claude", "scratch-workspaces", "a1-b2", "scratch-2026-09-21")
	if err := os.MkdirAll(scratch, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		MachineID:     "machine-1",
		Storage:       credentials.Config{Provider: credentials.ProviderS3, Bucket: "personal-agent-archive", Region: "us-east-1", AWSProfile: "test", Prefix: "agent-archive/"},
		Harnesses:     []string{"claude"},
		RetentionDays: 90,
		Archive: archive.Config{SchemaVersion: 1, MachineID: "machine-1", Enabled: true, Projects: []archive.ProjectActivation{
			{ProjectID: archive.ProjectID(agentArchive), Root: agentArchive, Included: true, ActivatedAt: time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC)},
		}},
	}
	if err := config.Save(f.data, cfg); err != nil {
		t.Fatal(err)
	}

	day := func(d, h int) time.Time { return time.Date(2026, 9, d, h, 0, 0, 0, time.UTC) }
	claude := func(id, cwd string, start time.Time) {
		f.nativeIDs = append(f.nativeIDs, id)
		f.write(t, filepath.Join(".claude", "projects", "slug-"+id, id+".jsonl"), fmt.Sprintf(`{"type":"user","uuid":"a","sessionId":%q,"cwd":%q,"timestamp":%q,"message":{"role":"user","content":"please check it"}}
{"type":"assistant","uuid":"b","sessionId":%q,"timestamp":%q,"message":{"role":"assistant","content":[{"type":"text","text":"Checked."}]}}
`, id, cwd, start.Format(time.RFC3339), id, start.Add(time.Minute).Format(time.RFC3339)))
	}
	claude("c-aa-1", agentArchive, day(19, 9))
	claude("c-aa-2", filepath.Join(agentArchive, ".claude", "worktrees", "gone"), day(21, 9))
	for _, agent := range []string{"a1", "a2"} {
		f.write(t, filepath.Join(".claude", "projects", "slug-c-aa-2", "c-aa-2", "subagents", "agent-"+agent+".jsonl"), `{"type":"user","sessionId":"c-aa-2","agentId":"`+agent+`","timestamp":"2026-09-21T09:05:00Z","message":{"role":"user","content":"look"}}`+"\n")
	}
	claude("c-lev-1", levenshtein, day(17, 20))
	claude("c-lev-2", filepath.Join(levenshtein, "src"), day(18, 9))
	claude("c-lev-3", levenshtein, day(20, 9))
	claude("c-scratch-1", scratch, day(21, 10))
	claude("c-scratch-2", filepath.Join(filepath.Dir(scratch), "scratch-2026-09-22"), day(22, 10))
	claude("c-old", filepath.Join(f.userHome, "old-notes"), day(18, 12))
	claude("c-archived", agentArchive, day(22, 12))
	claude("c-tmp-1", filepath.Join(root, "tmp", "run-1"), day(20, 1))
	claude("c-tmp-2", filepath.Join(root, "tmp", "run-2"), day(20, 2))
	claude("c-home", f.userHome, day(20, 3))
	f.nativeIDs = append(f.nativeIDs, "c-empty")
	f.write(t, filepath.Join(".claude", "projects", "slug-empty", "c-empty.jsonl"), `{"type":"summary","summary":"recap","cwd":"`+agentArchive+`"}`+"\n")

	const codexID = "0a9b3c4d-0000-4000-8000-0000000000aa"
	f.nativeIDs = append(f.nativeIDs, codexID)
	f.write(t, filepath.Join(".codex", "sessions", "2026", "09", "20", "rollout-2026-09-20T10-00-00-"+codexID+".jsonl"), fmt.Sprintf(`{"type":"session_meta","timestamp":"2026-09-20T17:00:00Z","payload":{"id":%q,"timestamp":"2026-09-20T17:00:00Z","cwd":%q,"source":"cli"}}
{"type":"response_item","timestamp":"2026-09-20T17:00:05Z","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"inspect the file"}]}}
`, codexID, agentArchive))

	cursor := func(id, folder string) {
		f.nativeIDs = append(f.nativeIDs, id)
		path := f.write(t, filepath.Join(".cursor", "projects", cursorSlugFor(folder), "agent-transcripts", id, id+".jsonl"), `{"role":"user","message":{"content":[{"type":"text","text":"Summarize the failing widget test."}]}}
`)
		// Setting an earlier modification time moves the birth time back with
		// it on macOS; elsewhere the modification time is the fallback.
		created := time.Date(2026, 9, 20, 18, 0, 0, 0, time.UTC)
		if err := os.Chtimes(path, created, created); err != nil {
			t.Fatal(err)
		}
	}
	cursor("k-aa", agentArchive)
	cursor("k-site", website)
	cursor("k-lost", filepath.Join(f.userHome, "no-such-folder"))

	// Cursor's database: one chat only it holds, whose workspace (named by
	// its workspace.json, so its size does not depend on the test's paths)
	// is agent-archive; one that also has a transcript; and a draft. Only the
	// first is imported from the database.
	f.nativeIDs = append(f.nativeIDs, "k-db-only", "k-db-draft")
	f.write(t, filepath.Join("Library", "Application Support", "Cursor", "User", "workspaceStorage", "ws1", "workspace.json"), `{"folder":"file://`+agentArchive+`"}`)
	f.cursorDatabase(t, map[string]string{
		"composerData:k-db-only": `{"_v":18,"composerId":"k-db-only","createdAt":1789923600000,"lastUpdatedAt":1789923660000,"status":"completed",` +
			`"workspaceIdentifier":{"id":"ws1"},"fullConversationHeadersOnly":[{"bubbleId":"m1","type":1},{"bubbleId":"m2","type":2}]}`,
		"bubbleId:k-db-only:m1":   `{"_v":3,"bubbleId":"m1","type":1,"text":"Why does the upload retry twice?","createdAt":1789923600000}`,
		"bubbleId:k-db-only:m2":   `{"_v":3,"bubbleId":"m2","type":2,"text":"The first attempt times out.","createdAt":1789923630000}`,
		"composerData:k-aa":       `{"_v":18,"composerId":"k-aa","fullConversationHeadersOnly":[{"bubbleId":"m1","type":1}]}`,
		"composerData:k-db-draft": `{"_v":18,"composerId":"k-db-draft","isDraft":true,"fullConversationHeadersOnly":[{"bubbleId":"m1","type":1}]}`,
	})

	// c-archived is already registered by a hook.
	store, err := state.Open(f.data)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.RegisterNewSession(agentmeta.SessionKey{Agent: agentmeta.ID(archive.CanonicalHarness("claude")), NativeID: "c-archived"}, func(id string) archive.SessionRegistration {
		return archive.SessionRegistration{ArchiveSessionID: id, NativeSessionID: "c-archived", ProjectID: archive.ProjectID(agentArchive), ProjectRoot: agentArchive,
			Harness: archive.Harness{Name: "claude"}, SessionStartedAt: day(22, 12), RegisteredAt: day(22, 12)}
	}); err != nil {
		t.Fatal(err)
	}

	f.env = Env{
		UserHomeDir:      func() (string, error) { return f.userHome, nil },
		Home:             func() (string, error) { return f.data, nil },
		Now:              func() time.Time { return backfillNow },
		LookupEnv:        func(string) (string, bool) { return "", false },
		BackfillTempDirs: []string{filepath.Join(root, "tmp")},
		// The fixture is a Mac: Library folders, Documents/Codex, TCC.
		OS: platform.Darwin,
		// status reads the collector's job state; no test may ask launchd.
		Scheduler:  newFakeScheduler(t, "missing"),
		Interrupts: noInterrupts,
	}
	return f
}

// cursorSlugFor is Cursor's folder name for a workspace path.
func cursorSlugFor(path string) string {
	b := []byte(strings.TrimPrefix(path, "/"))
	for i, c := range b {
		alnum := 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9'
		if !alnum {
			b[i] = '-'
		}
	}
	return string(b)
}

// cursorDatabase writes a synthetic Cursor state.vscdb, in rollback-journal
// mode so it can be read with Cursor closed.
func (f *backfillFixture) cursorDatabase(t *testing.T, rows map[string]string) {
	t.Helper()
	path := macCursorDatabase(f.userHome)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
	}()
	if _, err := db.ExecContext(t.Context(), `CREATE TABLE cursorDiskKV (key TEXT UNIQUE ON CONFLICT REPLACE, value BLOB)`); err != nil {
		t.Fatal(err)
	}
	for k, v := range rows {
		if _, err := db.ExecContext(t.Context(), `INSERT INTO cursorDiskKV (key, value) VALUES (?, ?)`, k, []byte(v)); err != nil {
			t.Fatal(err)
		}
	}
}

// rootPad is the length every occurrence of the fixture's root is padded to,
// so file sizes, and the plan's byte counts, do not depend on where
// t.TempDir happens to be.
const rootPad = 400

func (f *backfillFixture) write(t *testing.T, rel, content string) string {
	t.Helper()
	if n := strings.Count(content, f.root); n > 0 {
		if len(f.root) >= rootPad {
			t.Fatalf("temporary directory path longer than %d bytes", rootPad)
		}
		pad := `{"pad":"` + strings.Repeat("x", n*(rootPad-len(f.root))) + `",`
		content = strings.Replace(content, "{", pad, 1)
	}
	path := filepath.Join(f.userHome, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func (f *backfillFixture) run(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := Run(append([]string{"backfill"}, args...), nil, &out, &errOut, f.env)
	return out.String(), errOut.String(), code
}

// checkGolden compares output with testdata/backfill/name. Regenerate with
// `go test ./internal/cli -run TestBackfillGolden -update` and review the
// diff.
func checkGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	golden.Check(t, filepath.Join("testdata", "backfill", name), []byte(normalizeBackfillPromptGlyphs(string(got))))
}

// normalizeBackfillPromptGlyphs keeps redirected prompt fixtures independent
// of the inherited TERM. Only line-leading cursor/receipt glyphs vary; choices,
// defaults, receipt text, blank boundaries and every control byte stay intact.
func normalizeBackfillPromptGlyphs(text string) string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		if rest, ok := strings.CutPrefix(line, "› "); ok {
			lines[i] = "> " + rest
		} else if rest, ok := strings.CutPrefix(line, "✓ "); ok {
			lines[i] = "OK " + rest
		}
	}
	return strings.Join(lines, "\n")
}

func TestBackfillPromptGlyphNormalizationPreservesStructure(t *testing.T) {
	t.Parallel()
	plain := "? Import sessions?\n\n  1) Import\n  2) Skip (default)\n\n> Choose [2]: \nOK Skip\n\nPath contains › and ✓.\n\x1b[2Kwarning\n"
	unicode := strings.ReplaceAll(strings.ReplaceAll(plain, "> Choose", "› Choose"), "OK Skip", "✓ Skip")
	if got := normalizeBackfillPromptGlyphs(unicode); got != plain {
		t.Fatalf("normalization changed prompt structure: %q", got)
	}
	if got := normalizeBackfillPromptGlyphs(plain); got != plain {
		t.Fatalf("normalization changed ASCII output: %q", got)
	}
}

//lint:ignore tparallel the test is parallel, but its subtests share one fixture, in order
func TestBackfillGolden(t *testing.T) {
	t.Parallel()
	f := newBackfillFixture(t)
	cases := []struct {
		name string
		args []string
	}{
		{"default.txt", []string{"--dry-run"}},
		// Temporary folders lie outside home, so their rows would show the
		// temporary root; --include-home keeps every row under ~.
		{"filtered.txt", []string{"--dry-run", "--harness", "claude", "--since", "2026-09-19", "--include-home"}},
		{"plan.json", []string{"--dry-run", "--json"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, errOut, code := f.run(t, tc.args...)
			if code != 0 {
				t.Fatalf("code %d, stderr %s", code, errOut)
			}
			for _, id := range f.nativeIDs {
				if strings.Contains(out, id) {
					t.Errorf("output names native session %q", id)
				}
			}
			if strings.Contains(out, ".jsonl") || strings.Contains(out, "please check it") {
				t.Error("output names a transcript or its content")
			}
			checkGolden(t, tc.name, []byte(strings.ReplaceAll(out, f.root, "$ROOT")))
		})
	}
}

// A dry run changes only disposable inspection facts. Archive/configuration
// and Cursor database contents remain byte-for-byte unchanged.
func TestBackfillDryRunPreservesArchiveAndNativeData(t *testing.T) {
	t.Parallel()
	f := newBackfillFixture(t)
	cursorDir := filepath.Dir(macCursorDatabase(f.userHome))
	before, cursorBefore := snapshotArchiveTree(t, f.data), snapshotTree(t, cursorDir)
	for _, args := range [][]string{{"--dry-run"}, {"--dry-run", "--json"}} {
		if _, errOut, code := f.run(t, args...); code != 0 {
			t.Fatalf("%v: code %d: %s", args, code, errOut)
		}
	}
	if after := snapshotArchiveTree(t, f.data); after != before {
		t.Fatalf("data directory changed:\n%s\n---\n%s", before, after)
	}
	if after := snapshotTree(t, cursorDir); after != cursorBefore {
		t.Fatalf("Cursor's database folder changed:\n%s\n---\n%s", cursorBefore, after)
	}
}

// A folder in an app's store that cannot be read no longer stops the plan
// and prints its path: it is counted on one "Not imported" line, and the
// rest of the plan is shown.
func TestBackfillUnreadableFolder(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root reads any folder")
	}
	f := newBackfillFixture(t)
	locked := filepath.Join(f.userHome, ".claude", "projects", "slug-c-old")
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	out, errOut, code := f.run(t, "--dry-run")
	if code != 0 {
		t.Fatalf("code %d: %s", code, errOut)
	}
	if !strings.Contains(out, "   1  folder in the app stores could not be read\n") || !strings.Contains(out, "Total: 11 sessions") {
		t.Fatalf("output:\n%s", out)
	}
	jsonOut, _, code := f.run(t, "--dry-run", "--json")
	if code != 0 || !strings.Contains(jsonOut, `"unreadable_folders": 1`) {
		t.Fatalf("json (code %d):\n%s", code, jsonOut)
	}
	for _, text := range []string{out, errOut, jsonOut} {
		if strings.Contains(text, "slug-c-old") || strings.Contains(text, ".claude") {
			t.Fatalf("the unreadable folder is named:\n%s", text)
		}
	}

	// Claude Code's whole store unreadable: the app is named, and the other
	// apps' sessions can still be imported.
	store := filepath.Join(f.userHome, ".claude", "projects")
	if err := os.Chmod(store, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(store, 0o755) })
	out, errOut, code = f.run(t, "--dry-run")
	if code != 0 || !strings.Contains(out, "      Claude Code's session folder could not be read (check permissions);\n      none of its sessions are included.\n") || !strings.Contains(out, "Total: 4 sessions") {
		t.Fatalf("code %d, %s\n%s", code, errOut, out)
	}
	jsonOut, _, _ = f.run(t, "--dry-run", "--json")
	if !strings.Contains(jsonOut, "\"unreadable_stores\": [\n    \"claude\"\n  ]") || !strings.Contains(jsonOut, `"unreadable_folders": 0`) {
		t.Fatalf("json:\n%s", jsonOut)
	}
	for _, text := range []string{out, errOut, jsonOut} {
		if strings.Contains(text, ".claude") {
			t.Fatalf("the unreadable store is named:\n%s", text)
		}
	}
}

func snapshotTree(t *testing.T, dir string) string {
	t.Helper()
	return snapshotTreeFiltered(t, dir, false)
}

// snapshotArchiveTree permits only the command-owned disposable cache. Every
// other child retains its original bytes, modes, size and modification time.
func snapshotArchiveTree(t *testing.T, dir string) string {
	t.Helper()
	return snapshotTreeFiltered(t, dir, true)
}

func snapshotTreeFiltered(t *testing.T, dir string, ignoreCache bool) string {
	t.Helper()
	var b strings.Builder
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if ignoreCache && path == filepath.Join(dir, "backfill-inspection-cache") {
			return filepath.SkipDir
		}
		if ignoreCache && path == dir {
			fmt.Fprintf(&b, "%s\n", info.Mode())
			return nil
		}
		fmt.Fprintf(&b, "%s %d %s %s", strings.TrimPrefix(path, dir), info.Size(), info.Mode(), info.ModTime().Format(time.RFC3339Nano))
		if info.Mode().IsRegular() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			fmt.Fprintf(&b, " %x", sha256.Sum256(data))
		}
		b.WriteString("\n")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestBackfillRefusals(t *testing.T) {
	t.Parallel()
	f := newBackfillFixture(t)

	if _, errOut, code := f.run(t); code != 1 || !strings.Contains(errOut, "needs a terminal") {
		t.Fatalf("import: code %d, %q", code, errOut)
	}
	for _, args := range [][]string{{"--json"}, {"--dry-run", "--harness", "vim"}, {"--dry-run", "--since", "yesterday"}, {"--dry-run", "extra"}, {"--dry-run", "--background"}, {"history", "extra"}} {
		if _, _, code := f.run(t, args...); code != 2 {
			t.Errorf("%v: code %d, want 2", args, code)
		}
	}
	// With nothing to undo, undo says so without needing a terminal.
	if out, errOut, code := f.run(t, "undo"); code != 0 || !strings.Contains(out, "No imports to undo.") {
		t.Fatalf("undo: code %d, %q, %q", code, out, errOut)
	}
	for _, args := range [][]string{{"undo", "a", "b"}, {"undo", "--bogus"}} {
		if _, _, code := f.run(t, args...); code != 2 {
			t.Errorf("%v: code %d, want 2", args, code)
		}
	}

	// --yes is harmless with --dry-run.
	if out, errOut, code := f.run(t, "--dry-run", "--yes"); code != 0 || !strings.Contains(out, "Dry run: nothing was changed.") {
		t.Fatalf("--dry-run --yes: code %d, stderr %q", code, errOut)
	}

	// A dry run works while paused and while a setup transaction is pending.
	if _, err := config.SetPaused(f.data, true); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(setupjournal.JournalPath(f.data), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, errOut, code := f.run(t, "--dry-run"); code != 0 || !strings.Contains(out, "Dry run: nothing was changed.") {
		t.Fatalf("paused dry run: code %d, stderr %q", code, errOut)
	}

	notSetUp := testEnv(t, t.TempDir(), backfillNow)
	var out, errOut bytes.Buffer
	if code := Run([]string{"backfill", "--dry-run"}, nil, &out, &errOut, notSetUp); code != 1 || !strings.Contains(errOut.String(), "not set up") {
		t.Fatalf("not set up: code %d, %q", code, errOut.String())
	}
}

func TestBackfillHelp(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	if code := Run([]string{"backfill", "--help"}, nil, &out, nil, Env{}); code != 0 || !strings.Contains(out.String(), "Usage: agent-archive backfill") {
		t.Fatalf("code %d: %s", code, out.String())
	}
	out.Reset()
	if code := Run([]string{"help", "backfill"}, nil, &out, nil, Env{}); code != 0 || !strings.Contains(out.String(), "--dry-run") {
		t.Fatalf("code %d: %s", code, out.String())
	}
}

// Classify reports a registration the configuration no longer accepts as
// registered_not_admitted.
func TestBackfillArchiveState(t *testing.T) {
	t.Parallel()
	f := newBackfillFixture(t)
	cfg, _, err := config.Load(f.data)
	if err != nil {
		t.Fatal(err)
	}
	state := newArchiveState(f.data, cfg)
	if got, err := state.Classify("claude", "c-archived"); err != nil || got != backfill.SkipAlreadyArchived {
		t.Fatalf("accepted: %q, %v", got, err)
	}
	if got, err := state.Classify("claude", "c-lev-1"); err != nil || got != "" {
		t.Fatalf("unknown: %q, %v", got, err)
	}
	cfg.Archive.Projects[0].Included = false
	if got, err := newArchiveState(f.data, cfg).Classify("claude", "c-archived"); err != nil || got != backfill.SkipRegisteredNotAdmitted {
		t.Fatalf("not admitted: %q, %v", got, err)
	}
}
