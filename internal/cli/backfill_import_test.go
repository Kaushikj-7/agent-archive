package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/backfill"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/setupjournal"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

// The import the fixture produces: the date of backfillNow, first of the day.
const firstImport = "2026-09-23-1"

// newImportFixture is newBackfillFixture with an in-memory bucket, and
// subagent transcripts carrying their agent identity, as real ones do, so the
// collector registers them.
func newImportFixture(t *testing.T) (*backfillFixture, *storagetest.MemoryStore) {
	t.Helper()
	f := newBackfillFixture(t)
	bucket := storagetest.NewMemoryStore()
	f.env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return bucket, nil }
	for _, agent := range []string{"a1", "a2"} {
		f.write(t, filepath.Join(".claude", "projects", "slug-c-aa-2", "c-aa-2", "subagents", "agent-"+agent+".jsonl"),
			fmt.Sprintf(`{"type":"assistant","sessionId":"c-aa-2","agentId":%q,"timestamp":"2026-09-21T09:05:00Z","message":{"role":"assistant","content":"looked"}}`+"\n", agent))
	}
	return f, bucket
}

// importRun runs backfill with stdin, which is a terminal when terminal is
// set.
func (f *backfillFixture) importRun(t *testing.T, stdin io.Reader, terminal bool, args ...string) (string, string, int) {
	t.Helper()
	env := f.env
	env.IsTerminal = func(stream any) bool { return terminal && stream == any(stdin) }
	var out, errOut bytes.Buffer
	code := Run(append([]string{"backfill"}, args...), stdin, &out, &errOut, env)
	return out.String(), errOut.String(), code
}

func (f *backfillFixture) command(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := Run(args, nil, &out, &errOut, f.env)
	return out.String(), errOut.String(), code
}

// bucketSnapshot lists every object and its bytes.
func bucketSnapshot(t *testing.T, bucket *storagetest.MemoryStore) string {
	t.Helper()
	objects, err := bucket.List(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, o := range objects {
		data, err := bucket.Get(context.Background(), o.Key)
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, o.Key+" "+storage.SHA256Hex(data))
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

// unchanged fails the test if the data directory or the bucket differ from
// the snapshots taken before.
type unchanged struct {
	data   string
	bucket string
}

func snapshotAll(t *testing.T, f *backfillFixture, bucket *storagetest.MemoryStore) unchanged {
	t.Helper()
	return unchanged{snapshotArchiveTree(t, f.data), bucketSnapshot(t, bucket)}
}

func (u unchanged) check(t *testing.T, f *backfillFixture, bucket *storagetest.MemoryStore) {
	t.Helper()
	if after := snapshotArchiveTree(t, f.data); after != u.data {
		t.Fatalf("data directory changed:\n%s\n---\n%s", u.data, after)
	}
	if after := bucketSnapshot(t, bucket); after != u.bucket {
		t.Fatalf("bucket changed:\n%s\n---\n%s", u.bucket, after)
	}
}

func (f *backfillFixture) checkPrivate(t *testing.T, name, text string) {
	t.Helper()
	for _, id := range f.nativeIDs {
		if strings.Contains(text, id) {
			t.Errorf("%s names native session %q", name, id)
		}
	}
	if strings.Contains(text, ".jsonl") || strings.Contains(text, "please check it") {
		t.Errorf("%s names a transcript or its content", name)
	}
}

func importRegistrations(t *testing.T, home, batch string) (parents, children []archive.SessionRegistration) {
	t.Helper()
	regs, err := state.OpenReadOnly(home).LoadRegistrations()
	if err != nil {
		t.Fatal(err)
	}
	for _, reg := range regs {
		if reg.ImportBatch.Recorded() != batch {
			continue
		}
		if reg.ParentSessionID != "" {
			children = append(children, reg)
		} else {
			parents = append(parents, reg)
		}
	}
	return parents, children
}

// loadBatch reads the record of the fixture's import, firstImport, and its
// raw bytes.
func loadBatch(t *testing.T, home string) (backfill.Batch, []byte) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(home, "imports", firstImport+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var b backfill.Batch
	if err := json.Unmarshal(data, &b); err != nil {
		t.Fatal(err)
	}
	return b, data
}

// The default run: the plan, the prompt, the summary after confirmation, and
// everything the import leaves behind. A second run then imports and writes
// nothing.
func TestBackfillImportGolden(t *testing.T) {
	t.Parallel()
	f, bucket := newImportFixture(t)
	out, errOut, code := f.importRun(t, strings.NewReader("y\n"), true)
	if code != 0 {
		t.Fatalf("code %d, stderr %s\n%s", code, errOut, out)
	}
	f.checkPrivate(t, "output", out)
	checkGolden(t, "import.txt", []byte(strings.ReplaceAll(trimScreenLineEnds(out), f.root, "$ROOT")))

	cfg, _, err := config.Load(f.data)
	if err != nil {
		t.Fatal(err)
	}
	admittedAt := backfillNow.UTC()
	added := 0
	for _, p := range cfg.Archive.Projects {
		if p.ActivatedAt.Equal(admittedAt) {
			added++
			if !p.Included || p.ProjectID != archive.ProjectID(p.Root) {
				t.Errorf("added project %+v", p)
			}
		}
	}
	if added != 4 || len(cfg.Archive.Projects) != 5 {
		t.Fatalf("projects %+v", cfg.Archive.Projects)
	}
	if strings.Join(cfg.ImportedHarnesses, ",") != "codex,cursor" || strings.Join(cfg.Harnesses, ",") != "claude" {
		t.Fatalf("apps: hooks %v, imported %v", cfg.Harnesses, cfg.ImportedHarnesses)
	}

	parents, children := importRegistrations(t, f.data, firstImport)
	if len(parents) != 12 || len(children) != 2 {
		t.Fatalf("%d sessions and %d subagents registered", len(parents), len(children))
	}
	for _, reg := range append(parents, children...) {
		if reg.Origin != archive.SessionOriginImport || !reg.AdmittedAt.Equal(admittedAt) || reg.StartedAtSource == "" || reg.SessionStartedAt.After(admittedAt) {
			t.Errorf("registration %+v", reg)
		}
	}
	for _, reg := range parents {
		if !reg.RegisteredAt.Equal(admittedAt) {
			t.Errorf("registered at %s", reg.RegisteredAt)
		}
	}

	b, raw := loadBatch(t, f.data)
	f.checkPrivate(t, "batch file", string(raw))
	if strings.Contains(string(raw), f.root) {
		t.Errorf("batch file names a path:\n%s", raw)
	}
	var keys map[string]any
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"id", "started_at", "completed_at", "filters", "destination_id", "projects_added", "apps_added", "sessions", "subagents"} {
		if _, ok := keys[key]; !ok {
			t.Errorf("batch file has no %q", key)
		}
	}
	if b.ID != firstImport || b.CompletedAt == nil || len(b.Sessions) != 12 || len(b.Subagents) != 2 || len(b.ProjectsAdded) != 4 ||
		strings.Join(b.AppsAdded, ",") != "codex,cursor" || b.DestinationID != cfg.DestinationID() {
		t.Fatalf("batch %+v", b)
	}
	registered := map[string]bool{}
	for _, reg := range parents {
		registered[reg.ArchiveSessionID] = true
	}
	for _, id := range b.Sessions {
		if !registered[id] {
			t.Errorf("batch session %s is not a registered import", id)
		}
	}

	// Every imported session is in the bucket, marked as an import.
	for _, reg := range append(parents, children...) {
		key, _ := archive.MetadataObjectKey(reg.Harness.Name, reg.ArchiveSessionID)
		m, err := reader.ReadMetadata(context.Background(), bucket, key)
		if err != nil {
			t.Fatalf("%s: %v", reg.ArchiveSessionID, err)
		}
		if m.Origin != archive.SessionOriginImport || m.ImportedAt == nil || !m.ImportedAt.Equal(admittedAt) || m.StartedAtSource == "" {
			t.Errorf("metadata of %s: origin %q, imported_at %v, started_at_source %q", reg.ArchiveSessionID, m.Origin, m.ImportedAt, m.StartedAtSource)
		}
	}
	for _, child := range children {
		if len(child.SubagentID) == 0 || child.ParentNativeSessionID != "c-aa-2" {
			t.Errorf("child %+v", child)
		}
	}

	history, errOut, code := f.command(t, "backfill", "history")
	if code != 0 {
		t.Fatalf("history: %d %s", code, errOut)
	}
	checkGolden(t, "history.txt", []byte(history))

	status, _, _ := f.command(t, "status", "--json")
	var view statusView
	if err := json.Unmarshal([]byte(status), &view); err != nil {
		t.Fatal(err)
	}
	if view.LastImport != firstImport || view.ImportedSessions != 12 || view.ImportedPending != 0 {
		t.Fatalf("status: last %q, imported %d, pending %d", view.LastImport, view.ImportedSessions, view.ImportedPending)
	}

	// A second run finds nothing new, and writes nothing, locally or in the
	// bucket.
	before := snapshotAll(t, f, bucket)
	out, errOut, code = f.importRun(t, strings.NewReader(""), false, "--yes")
	if code != 0 {
		t.Fatalf("second run: code %d, %s", code, errOut)
	}
	checkGolden(t, "second-run.txt", []byte(strings.ReplaceAll(trimScreenLineEnds(out), f.root, "$ROOT")))
	before.check(t, f, bucket)
}

// list marks imports and filters on them; show prints their provenance.
func TestBackfillListAndShow(t *testing.T) {
	t.Parallel()
	f, _ := newImportFixture(t)
	if _, errOut, code := f.importRun(t, nil, false, "--yes", "--harness", "claude", "--project", filepath.Join(f.userHome, "levenshtein")); code != 0 {
		t.Fatalf("import: %s", errOut)
	}
	parents, _ := importRegistrations(t, f.data, firstImport)
	if len(parents) != 3 {
		t.Fatalf("%d imported", len(parents))
	}
	out, errOut, code := f.command(t, "list", "--imported", "--verbose")
	if code != 0 || !strings.Contains(out, "3 session(s).") || strings.Count(out, " imported ") != 3 || !strings.Contains(out, "ORIGIN") {
		t.Fatalf("list --imported: %d %s\n%s", code, errOut, out)
	}
	for _, reg := range parents {
		if !strings.Contains(out, reg.ArchiveSessionID) {
			t.Errorf("list --imported misses %s", reg.ArchiveSessionID)
		}
	}
	if out, _, code := f.command(t, "list", "--hook-captured"); code != 0 || !strings.Contains(out, "No archived sessions match.") {
		t.Fatalf("list --hook-captured: %s", out)
	}
	if _, _, code := f.command(t, "list", "--imported", "--hook-captured"); code != 2 {
		t.Fatalf("both filters: code %d", code)
	}
	out, errOut, code = f.command(t, "show", parents[0].ArchiveSessionID, "--json")
	if code != 0 {
		t.Fatalf("show: %s", errOut)
	}
	for _, field := range []string{`"origin": "import"`, `"imported_at": "2026-09-23T19:00:00Z"`, `"started_at_source": "transcript"`, archive.CaptureGapImportedWithoutHookEvidence} {
		if !strings.Contains(out, field) {
			t.Errorf("show lacks %s:\n%s", field, out)
		}
	}
}

// Declining, in any of its forms, changes nothing, locally or in the bucket.
// So does running out of input at the prompt.
func TestBackfillDeclineChangesNothing(t *testing.T) {
	t.Parallel()
	f, bucket := newImportFixture(t)
	before := snapshotAll(t, f, bucket)
	config0, err := os.ReadFile(filepath.Join(f.data, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, answer := range []string{"\n", "n\n", "No\n", "maybe\nn\n"} {
		out, errOut, code := f.importRun(t, strings.NewReader(answer), true)
		if code != 0 || !strings.Contains(out, "Cancelled. Nothing was changed.") {
			t.Fatalf("%q: code %d, %s\n%s", answer, code, errOut, out)
		}
		if strings.HasPrefix(answer, "maybe") && !strings.Contains(out, "Enter one of the available choices.") {
			t.Fatalf("%q: no hint", answer)
		}
		before.check(t, f, bucket)
	}
	if _, errOut, code := f.importRun(t, strings.NewReader(""), true); code != 1 || !strings.Contains(errOut, "Nothing was changed.") {
		t.Fatalf("no input: code %d, %s", code, errOut)
	}
	before.check(t, f, bucket)
	if after, _ := os.ReadFile(filepath.Join(f.data, "config.json")); !bytes.Equal(after, config0) {
		t.Fatal("config.json changed")
	}
}

// Without a terminal, only --yes imports; otherwise nothing is even read.
func TestBackfillNoTerminalRefuses(t *testing.T) {
	t.Parallel()
	f, bucket := newImportFixture(t)
	before := snapshotAll(t, f, bucket)
	out, errOut, code := f.importRun(t, strings.NewReader("y\n"), false)
	if code != 1 || out != "" || !strings.Contains(errOut, "--yes") {
		t.Fatalf("code %d, stdout %q, stderr %q", code, out, errOut)
	}
	before.check(t, f, bucket)
}

// edit asks for a retention period, shows the plan again with the new
// deletion date, and commits the retention with the import.
func TestBackfillEditRetention(t *testing.T) {
	t.Parallel()
	f, _ := newImportFixture(t)
	out, errOut, code := f.importRun(t, strings.NewReader("edit\n365\ny\n"), true, "--background")
	if code != 0 {
		t.Fatalf("code %d, %s", code, errOut)
	}
	if !strings.Contains(normalizeBackfillPromptGlyphs(out), "OK Keep sessions for how many days? 365\n\nBackfill imports") {
		t.Fatalf("retention receipt must have one blank line before the refreshed plan:\n%s", out)
	}
	checkGolden(t, "edit.txt", []byte(strings.ReplaceAll(trimScreenLineEnds(out), f.root, "$ROOT")))
	if strings.Count(out, "? Import 12 sessions from 5 projects?") != 2 || !strings.Contains(out, "Retention is 365 days, so these sessions are deleted on 2027-09-23.") {
		t.Fatalf("plan not shown again with the new date:\n%s", out)
	}
	cfg, _, _ := config.Load(f.data)
	if cfg.RetentionDays != 365 {
		t.Fatalf("retention %d", cfg.RetentionDays)
	}
}

// A failed storage check stops before the prompt and changes nothing; a
// Keychain failure says how to fix it.
func TestBackfillStorageCheckFails(t *testing.T) {
	t.Parallel()
	f, bucket := newImportFixture(t)
	before := snapshotAll(t, f, bucket)
	f.env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return failingPutStore{bucket}, nil }
	out, errOut, code := f.importRun(t, strings.NewReader("y\n"), true)
	if code != 1 || !strings.Contains(out, "Checking storage… failed.") || strings.Contains(out, "? Import ") || !strings.Contains(errOut, "nothing was imported") {
		t.Fatalf("code %d\n%s\n%s", code, out, errOut)
	}
	before.check(t, f, bucket)

	f.env.OpenStore = func(config.Config) (storage.ObjectStore, error) {
		return nil, fmt.Errorf("read credential: %w", credentials.ErrKeychainLocked)
	}
	_, errOut, code = f.importRun(t, strings.NewReader("y\n"), true)
	if code != 1 || !strings.Contains(errOut, credentials.RecoveryAction(credentials.ErrKeychainLocked)) {
		t.Fatalf("code %d, %s", code, errOut)
	}
	before.check(t, f, bucket)
}

type failingPutStore struct{ storage.ObjectStore }

func (failingPutStore) Put(context.Context, string, []byte) error { return errors.New("access denied") }

// onFirstRead runs before when the prompt first reads its answer: the
// moment between showing the plan and confirming it.
type onFirstRead struct {
	r      io.Reader
	before func()
	once   sync.Once
}

func (o *onFirstRead) Read(p []byte) (int, error) {
	o.once.Do(o.before)
	return o.r.Read(p)
}

// A configuration change while the prompt is open aborts the import; so does
// a pause, and an import refuses to start while paused or while setup needs
// recovery. A bucket privacy refresh by the collector is not a change.
func TestBackfillConcurrentChanges(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		change func(t *testing.T, home string)
		want   string
	}{
		{"setting changed", func(t *testing.T, home string) {
			t.Helper()
			cfg, _, _ := config.Load(home)
			cfg.RequireSkillUse = true
			if err := config.Save(home, cfg); err != nil {
				t.Fatal(err)
			}
		}, "the configuration changed while this was open; run backfill again"},
		{"paused", func(t *testing.T, home string) {
			t.Helper()
			if _, err := config.SetPaused(home, true); err != nil {
				t.Fatal(err)
			}
		}, "collection is paused"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, bucket := newImportFixture(t)
			stdin := &onFirstRead{r: strings.NewReader("y\n"), before: func() { tc.change(t, f.data) }}
			var out, errOut bytes.Buffer
			env := f.env
			env.IsTerminal = func(stream any) bool { return stream == any(stdin) }
			before := bucketSnapshot(t, bucket)
			if code := Run([]string{"backfill"}, stdin, &out, &errOut, env); code != 1 || !strings.Contains(errOut.String(), tc.want) {
				t.Fatalf("code %d, %s", code, errOut.String())
			}
			if parents, _ := importRegistrations(t, f.data, firstImport); len(parents) != 0 {
				t.Fatal("sessions were registered")
			}
			if _, err := os.Stat(filepath.Join(f.data, "imports")); !os.IsNotExist(err) {
				t.Fatalf("an import was recorded: %v", err)
			}
			cfg, _, _ := config.Load(f.data)
			if len(cfg.Archive.Projects) != 1 || len(cfg.ImportedHarnesses) != 0 {
				t.Fatalf("configuration changed: %+v", cfg.Archive.Projects)
			}
			if bucketSnapshot(t, bucket) != before {
				t.Fatal("bucket changed")
			}
		})
	}

	t.Run("privacy refresh", func(t *testing.T) {
		t.Parallel()
		f, _ := newImportFixture(t)
		checked := backfillNow.UTC()
		stdin := &onFirstRead{r: strings.NewReader("y\n"), before: func() {
			cfg, _, _ := config.Load(f.data)
			cfg.BucketPrivacy = &storage.PrivacyReport{State: "unknown", CheckedAt: &checked}
			if err := config.Save(f.data, cfg); err != nil {
				t.Fatal(err)
			}
		}}
		if _, errOut, code := f.importRun(t, stdin, true, "--background"); code != 0 {
			t.Fatalf("code %d, %s", code, errOut)
		}
	})

	t.Run("refused before planning", func(t *testing.T) {
		t.Parallel()
		f, bucket := newImportFixture(t)
		if _, err := config.SetPaused(f.data, true); err != nil {
			t.Fatal(err)
		}
		before := snapshotAll(t, f, bucket)
		if out, errOut, code := f.importRun(t, nil, false, "--yes"); code != 1 || out != "" || !strings.Contains(errOut, "paused") {
			t.Fatalf("paused: code %d, %q, %q", code, out, errOut)
		}
		before.check(t, f, bucket)
		if _, err := config.SetPaused(f.data, false); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(setupjournal.JournalPath(f.data), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		before = snapshotAll(t, f, bucket)
		if out, errOut, code := f.importRun(t, nil, false, "--yes"); code != 1 || out != "" || !strings.Contains(errOut, "setup needs recovery") {
			t.Fatalf("setup pending: code %d, %q, %q", code, out, errOut)
		}
		before.check(t, f, bucket)
	})
}

// A crash inside the configuration commit, after the batch file is written,
// leaves a batch naming projects the configuration lacks; a crash after the
// commit leaves projects added and nothing registered; a crash between
// registration holds leaves some sessions registered. Either way, running
// backfill again finishes the same import: one batch, each project added
// once, each session registered once.
func TestBackfillCrashConverges(t *testing.T) {
	t.Parallel()
	for _, crash := range []struct {
		name     string
		at       string
		n        int
		projects int
	}{{"inside commit", "batch saved", 1, 1}, {"after commit", "committed", 1, 5}, {"between holds", "registered", 2, 5}} {
		t.Run(crash.name, func(t *testing.T) {
			t.Parallel()
			f, _ := newImportFixture(t)
			calls := 0
			f.env.backfillHoldSteps = 3
			f.env.backfillCheckpoint = func(step string) error {
				if step == crash.at {
					if calls++; calls == crash.n {
						return errors.New("simulated crash")
					}
				}
				return nil
			}
			if _, errOut, code := f.importRun(t, nil, false, "--yes", "--background"); code != 1 || !strings.Contains(errOut, "simulated crash") {
				t.Fatalf("crash: code %d, %s", code, errOut)
			}
			cfg, _, _ := config.Load(f.data)
			if len(cfg.Archive.Projects) != crash.projects {
				t.Fatalf("projects after the crash: %d, want %d", len(cfg.Archive.Projects), crash.projects)
			}
			partial, _ := importRegistrations(t, f.data, firstImport)
			b, _ := loadBatch(t, f.data)
			if b.CompletedAt != nil || len(b.Sessions) != len(partial) || len(b.ProjectsAdded) != 4 {
				t.Fatalf("interrupted batch %+v, %d registered", b, len(partial))
			}
			if crash.at != "registered" && len(partial) != 0 || crash.at == "registered" && (len(partial) == 0 || len(partial) == 12) {
				t.Fatalf("%d registered at the crash", len(partial))
			}
			if history, _, _ := f.command(t, "backfill", "history"); !strings.Contains(history, "interrupted") {
				t.Fatalf("history:\n%s", history)
			}

			f.env.backfillCheckpoint = nil
			out, errOut, code := f.importRun(t, nil, false, "--yes")
			if code != 0 {
				t.Fatalf("rerun: code %d, %s\n%s", code, errOut, out)
			}
			if want := fmt.Sprintf("Registered %d sessions", 12-len(partial)); !strings.Contains(out, want) || !strings.Contains(out, "as import "+firstImport) {
				t.Fatalf("rerun did not continue the import (%s):\n%s", want, out)
			}
			parents, children := importRegistrations(t, f.data, firstImport)
			b, _ = loadBatch(t, f.data)
			if len(parents) != 12 || len(children) != 2 || len(b.Sessions) != 12 || len(b.Subagents) != 2 || len(b.ProjectsAdded) != 4 || b.CompletedAt == nil {
				t.Fatalf("after rerun: %d sessions, %d subagents, batch %+v", len(parents), len(children), b)
			}
			batches, _ := backfill.LoadBatches(f.data)
			if len(batches) != 1 {
				t.Fatalf("%d batches", len(batches))
			}
			cfg, _, _ = config.Load(f.data)
			roots := map[string]bool{}
			for _, p := range cfg.Archive.Projects {
				roots[p.Root] = true
			}
			if len(cfg.Archive.Projects) != 5 || len(roots) != 5 {
				t.Fatalf("projects after rerun: %+v", cfg.Archive.Projects)
			}
			natives := map[string]bool{}
			for _, reg := range parents {
				natives[reg.NativeSessionID] = true
			}
			if len(natives) != 12 {
				t.Fatalf("%d distinct sessions registered, want 12", len(natives))
			}
		})
	}
}

// With --background, registration writes subagent candidates marked as
// imports, observed at the import, and the parent's request carries the
// pending links. The collector then registers each child with the parent's
// import fields and no SubagentStop lifecycle evidence.
func TestBackfillSubagentsInheritImport(t *testing.T) {
	t.Parallel()
	f, bucket := newImportFixture(t)
	out, errOut, code := f.importRun(t, nil, false, "--yes", "--background", "--project", filepath.Join(f.userHome, "agent-archive"), "--harness", "claude")
	if code != 0 {
		t.Fatalf("code %d, %s", code, errOut)
	}
	if !strings.Contains(out, "Registered 2 sessions and 2 subagent transcripts as import "+firstImport) || !strings.Contains(out, "The background collector uploads them.") {
		t.Fatalf("output:\n%s", out)
	}
	store := state.OpenReadOnly(f.data)
	candidates, err := store.LoadSubagentCandidates()
	if err != nil || len(candidates) != 2 {
		t.Fatalf("candidates %+v, %v", candidates, err)
	}
	parentID, _, _ := store.ArchiveSessionID(agentmeta.SessionKey{Agent: agentmeta.ID(archive.CanonicalHarness("claude")), NativeID: "c-aa-2"})
	for _, c := range candidates {
		if c.Origin != archive.SessionOriginImport || !c.ObservedAt.Equal(backfillNow.UTC()) || c.ParentArchiveSessionID != parentID || c.NativeSessionID != "c-aa-2:subagent:"+c.AgentID {
			t.Errorf("candidate %+v", c)
		}
	}
	requests, _ := store.LoadRequests()
	links := 0
	for _, req := range requests {
		if req.ArchiveSessionID != parentID {
			continue
		}
		for _, e := range req.HookEvidence {
			if e.Kind == archive.EvidenceKindLinkedSession {
				links++
			}
		}
	}
	if links != 2 {
		t.Fatalf("parent request carries %d links", links)
	}

	if _, errOut, code := f.command(t, "sync"); code != 0 {
		t.Fatalf("sync: %s", errOut)
	}
	parents, children := importRegistrations(t, f.data, firstImport)
	if len(children) != 2 {
		t.Fatalf("children %+v", children)
	}
	var parent archive.SessionRegistration
	for _, p := range parents {
		if p.ArchiveSessionID == parentID {
			parent = p
		}
	}
	for _, child := range children {
		if child.ParentSessionID != parentID || child.Origin != archive.SessionOriginImport || child.ImportBatch.Recorded() != firstImport || !child.AdmittedAt.Equal(parent.AdmittedAt) ||
			parent.DestinationID == "" || child.DestinationID != parent.DestinationID {
			t.Errorf("child %+v", child)
		}
		key, _ := archive.MetadataObjectKey("claude", child.ArchiveSessionID)
		m, err := reader.ReadMetadata(context.Background(), bucket, key)
		if err != nil {
			t.Fatal(err)
		}
		if m.ParentSessionID != parentID || m.Origin != archive.SessionOriginImport {
			t.Errorf("child metadata: parent %q, origin %q", m.ParentSessionID, m.Origin)
		}
		if req, found, _ := requestFor(store, child.ArchiveSessionID); found && strings.Contains(fmt.Sprint(req.Reasons), "subagentstop") {
			t.Errorf("child has SubagentStop lifecycle evidence: %+v", req)
		}
	}
}

func requestFor(store *state.Store, id string) (state.Request, bool, error) {
	requests, err := store.LoadRequests()
	for _, req := range requests {
		if req.ArchiveSessionID == id {
			return req, true, err
		}
	}
	return state.Request{}, false, err
}

// Ctrl-C during the upload stops after the session in flight; the rest stay
// registered, with their requests, for the background collector. The watch
// says so at once, even in the middle of a long upload, and keeps watching,
// so a second Ctrl-C can still clean up before quitting (see
// TestSecondInterruptRemovesSnapshotsAndExits); it lets go when the command
// ends.
func TestBackfillInterruptedUpload(t *testing.T) {
	t.Parallel()
	f, bucket := newImportFixture(t)
	signals := make(chan os.Signal, 1)
	var mu sync.Mutex
	var events []string
	record := func(event string) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, event)
	}
	// Planning has a watch of its own, stopped before anything is confirmed.
	watches := 0
	f.env.Interrupts = func() (<-chan os.Signal, func()) {
		watches++
		if watches == 1 {
			return signals, func() { record("planning watch stopped") }
		}
		return signals, func() { record("watch stopped") }
	}
	out := &syncBuffer{}
	const notice = "Stopping after the current session; press Ctrl-C again to quit."
	f.env.backfillCheckpoint = func(step string) error {
		if step != "uploading" {
			return nil
		}
		signals <- os.Interrupt
		// The notice comes from the watch itself, not when the command next
		// looks, which can be a whole upload later.
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline) && !strings.Contains(out.String(), notice); {
			time.Sleep(5 * time.Millisecond)
		}
		record("upload starts")
		return nil
	}
	env := f.env
	env.IsTerminal = func(any) bool { return false }
	var errOut bytes.Buffer
	code := Run([]string{"backfill", "--yes"}, nil, out, &errOut, env)
	if strings.Join(events, ", ") != "planning watch stopped, upload starts, watch stopped" {
		t.Errorf("events %v", events)
	}
	if !strings.Contains(out.String(), notice) {
		t.Errorf("no notice of the stop:\n%s", out.String())
	}
	if code != 0 || !strings.Contains(out.String(), "Stopped. The remaining 4 sessions will be uploaded by the background collector.") || !strings.Contains(out.String(), "list --imported") {
		t.Fatalf("code %d, %s\n%s", code, errOut.String(), out.String())
	}
	parents, children := importRegistrations(t, f.data, firstImport)
	store := state.OpenReadOnly(f.data)
	verified, pending := 0, 0
	parentIDs := map[string]bool{}
	for _, reg := range parents {
		parentIDs[reg.ArchiveSessionID] = true
		_, requested, err := requestFor(store, reg.ArchiveSessionID)
		must(t, err)
		owed, err := store.Outstanding(reg, requested)
		must(t, err)
		if !owed.Published {
			pending++
			if !requested || !owed.Pending() || !owed.StagedImport {
				t.Errorf("unfinished owner lost its staged/requested state: %s %+v", reg.ArchiveSessionID, owed)
			}
			stage, found, err := store.LoadImportStage(t.Context(), reg)
			must(t, err)
			if !found || stage.SHA256 == "" || stage.SHA256 != reg.StagedImportSHA256 {
				t.Fatal("unfinished owner lost its exact frozen stage")
			}
			key, err := archive.MetadataObjectKey(reg.Harness.Name, reg.ArchiveSessionID)
			must(t, err)
			if _, err := bucket.Get(t.Context(), key); !errors.Is(err, storage.ErrNotFound) {
				t.Fatal("interrupted owner unexpectedly reached remote metadata", err)
			}
			continue
		}
		// Admission already verified the first bounded group before this
		// upload checkpoint. Ctrl-C must preserve its settled publications.
		verified++
		if requested || owed.Pending() {
			t.Errorf("completed group regained pending work: %s %+v", reg.ArchiveSessionID, owed)
		}
		published, err := store.LoadPublishedState(reg.ArchiveSessionID)
		must(t, err)
		metadata, found, err := published.LastPublishedMetadata()
		must(t, err)
		if !found {
			t.Fatal("completed group lost remote publication")
		}
		bundle, err := reader.LoadSource(t.Context(), bucket, metadata, reader.Limits{})
		must(t, err)
		if bundle.NativeSessionID != reg.NativeSessionID || !bundle.Capture.CapturedAt.Equal(reg.AdmittedAt) {
			t.Fatal("completed group publication changed its selected provenance")
		}
	}
	if len(parents) != 12 || verified != 8 || pending != 4 {
		t.Fatalf("registered=%d verified=%d pending=%d; want 12/8/4", len(parents), verified, pending)
	}
	// Classic linked children can still be queued behind their pending parent.
	// Account for their actual batch membership separately from parent totals.
	batch, _ := loadBatch(t, f.data)
	batchChildren, retainedChildren := map[string]bool{}, map[string]bool{}
	for _, id := range batch.Subagents {
		batchChildren[id] = true
	}
	for _, child := range children {
		if !batchChildren[child.ArchiveSessionID] || !parentIDs[child.ParentSessionID] {
			t.Fatal("registered child changed its batch or parent membership")
		}
		retainedChildren[child.ArchiveSessionID] = true
	}
	candidates, issues, err := store.ScanSubagentCandidates()
	must(t, err)
	if len(issues) != 0 {
		t.Fatal("interrupted import left unreadable linked children", issues)
	}
	for _, child := range candidates {
		if !parentIDs[child.ParentArchiveSessionID] {
			continue
		}
		if !batchChildren[child.ArchiveSessionID] || child.StagedImportSHA256 == "" {
			t.Fatal("queued child lost its frozen batch membership")
		}
		retainedChildren[child.ArchiveSessionID] = true
	}
	if len(batchChildren) != 2 || len(retainedChildren) != 2 {
		t.Fatalf("batch children=%d retained children=%d; want 2/2", len(batchChildren), len(retainedChildren))
	}
	t.Logf("parents registered=%d verified=%d pending=%d; linked children=%d (%d registered)", len(parents), verified, pending, len(retainedChildren), len(children))
}

// syncBuffer is a bytes.Buffer a test can read while a command writes it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// uninstall --delete-local-data removes the import batches too.
func TestUninstallDeleteLocalDataRemovesImports(t *testing.T) {
	t.Parallel()
	home, _, env := installedFixture(t, newFakeKeychain(), s3SetupInput("test-bucket", "us-east-1", "test-profile", true, false, false, t.TempDir()))
	if err := backfill.SaveBatch(home, backfill.Batch{ID: firstImport, StartedAt: backfillNow}); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := runUninstallCommand([]string{"--delete-local-data"}, strings.NewReader("y\ny\n"), &stdout, &stderr, env); code != 0 {
		t.Fatalf("code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(filepath.Join(home, "imports")); !os.IsNotExist(err) {
		t.Fatalf("imports survived uninstall: %v", err)
	}
	if strings.Contains(stderr.String(), "imports") {
		t.Fatalf("imports reported as a leftover:\n%s", stderr.String())
	}
}

// A hook that fires while backfill registers sessions gets hooks.lock within
// its one-second wait: registration holds the real flock only briefly. A
// hook that waited out that second fails with local.ErrBusy, so the errors
// are the check; the hook's total time also counts its own disk writes,
// which a loaded machine can stretch past a second without any wait.
func TestBackfillHookDuringRegistration(t *testing.T) {
	// Not parallel: the hook must get hooks.lock within its one-second wait, and
	// a busy parallel run can stretch registration's hold past that.
	home, project := t.TempDir(), t.TempDir()
	activated := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	setUpTestConfig(t, home, project, activated)
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	var candidates []backfill.Candidate
	for i := range 80 {
		id := fmt.Sprintf("import-%02d", i)
		path := filepath.Join(project, id+".jsonl")
		if err := os.WriteFile(path, []byte(`{"type":"user"}`+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		candidates = append(candidates, backfill.Candidate{
			Harness: "claude", NativeSessionID: id, TranscriptPath: path, ProjectRoot: project, ProjectKind: backfill.ProjectKindDirectory,
			ProjectIncluded: true, StartedAt: activated.Add(-time.Duration(i+1) * time.Hour), StartedAtSource: archive.StartedAtSourceTranscript,
		})
	}

	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		during    int
		hookErrs  []error
		finished  = make(chan struct{})
		startOnce sync.Once
	)
	// Hooks arrive every 25 ms, faster than a person works, for as long as
	// registration runs.
	fire := func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-finished:
				return
			case <-time.After(25 * time.Millisecond):
			}
			err := handleTestHookEvent(home, "claude", map[string]any{
				"hook_event_name": "SessionStart", "source": "startup", "session_id": fmt.Sprintf("hook-%d", i), "cwd": project,
				"transcript_path": filepath.Join(project, fmt.Sprintf("hook-%d.jsonl", i)),
			}, time.Now())
			mu.Lock()
			if err != nil {
				hookErrs = append(hookErrs, err)
			}
			select {
			case <-finished:
			default:
				during++
			}
			mu.Unlock()
		}
	}
	registration := backfill.Registration{
		Home: home, Store: store, Batch: firstImport, AdmittedAt: time.Now().UTC(),
		AfterHold: func(_, _ []string) error {
			startOnce.Do(func() { wg.Add(1); go fire() })
			return nil
		},
	}
	result, err := registration.Run(candidates)
	close(finished)
	wg.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Sessions) != len(candidates) {
		t.Fatalf("registered %d of %d", len(result.Sessions), len(candidates))
	}
	for _, err := range hookErrs {
		if errors.Is(err, local.ErrBusy) {
			t.Fatalf("a hook waited out its second for hooks.lock: %v", err)
		}
	}
	if len(hookErrs) > 0 {
		t.Fatalf("hooks failed during registration: %v", hookErrs)
	}
	if during == 0 {
		t.Fatal("no hook ran while registration was in progress")
	}
}

// A diagnostic-only discovery candidate must not abort foreground publication
// after the valid import's registrations and batch have already committed.
func TestBackfillForegroundUploadSkipsMalformedNeighbors(t *testing.T) {
	for _, tc := range []struct {
		name     string
		nativeID string
	}{
		{"empty", ""}, {"whitespace", " \t "}, {"invalid-utf8", string([]byte{0xff})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, bucket := newImportFixture(t)
			cfg, _, err := config.Load(f.data)
			must(t, err)
			var out, errOut bytes.Buffer
			plan, ok := planBackfill(f.env, &out, &errOut, f.data, f.userHome, cfg, backfillCommandOptions{}, styleFor(&out))
			if !ok {
				t.Fatalf("plan: %s", errOut.String())
			}
			want := len(plan.Imported())
			if want == 0 {
				t.Fatal("fixture contains no valid import")
			}
			// Also include a skipped duplicate of a real qualified identity with a
			// different byte size, so its diagnostic estimate cannot overwrite it.
			duplicate := plan.Imported()[0]
			duplicate.Skip = backfill.SkipDuplicateSession
			duplicate.Bytes = 1 << 40
			plan.Candidates = append(plan.Candidates, duplicate, backfill.Candidate{
				Harness: "claude", NativeSessionID: tc.nativeID, Skip: backfill.SkipUnsafeFormat,
			})
			out.Reset()
			code := importPlan(f.env, &out, &errOut, f.data, plan, configFingerprint(cfg), false)
			parents, children := importRegistrations(t, f.data, firstImport)
			if len(parents) != want {
				t.Fatalf("registered %d, want %d", len(parents), want)
			}
			batch, _ := loadBatch(t, f.data)
			if batch.CompletedAt == nil {
				t.Fatal("batch did not commit")
			}
			if code != 0 || !strings.Contains(out.String(), fmt.Sprintf("Uploaded %d sessions", want)) {
				t.Fatalf("foreground upload: code %d, stderr %s, stdout %s", code, errOut.String(), out.String())
			}
			var wantBytes int64
			for _, c := range plan.Imported() {
				wantBytes += c.Bytes
				for _, sub := range c.Subagents {
					wantBytes += sub.Bytes
				}
			}
			if !strings.Contains(out.String(), "("+backfill.FormatSize(wantBytes)+").") {
				t.Fatalf("wrong imported byte estimate: %s", out.String())
			}
			for _, reg := range append(parents, children...) {
				key, err := archive.MetadataObjectKey(reg.Harness.Name, reg.ArchiveSessionID)
				must(t, err)
				metadata, err := reader.ReadMetadata(context.Background(), bucket, key)
				must(t, err)
				if metadata.Origin != archive.SessionOriginImport {
					t.Fatalf("wrong origin: %s", metadata.Origin)
				}
			}
		})
	}
}
