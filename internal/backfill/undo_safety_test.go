package backfill

import (
	"bytes"
	"errors"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/agents/builtin"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/state/statetest"
)

// undoFixture is a local store and configuration with imported sessions.
type undoFixture struct {
	t     *testing.T
	home  string
	store *state.Store
	cfg   config.Config
}

func newUndoFixture(t *testing.T) *undoFixture {
	t.Helper()
	home := t.TempDir()
	if err := os.Chmod(home, 0700); err != nil {
		t.Fatal(err)
	}
	if err := config.Save(home, config.Config{RetentionDays: 90}); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	return &undoFixture{t: t, home: home, store: store}
}

// include adds an included project at root.
func (f *undoFixture) include(root string) string {
	id := archive.ProjectID(root)
	f.cfg.Archive.Projects = append(f.cfg.Archive.Projects, archive.ProjectActivation{ProjectID: id, Root: root, Included: true, ActivatedAt: fixedNow.UTC()})
	return id
}

// register registers one session in root, imported by batch when batch is
// not "", admitted at admitted.
func (f *undoFixture) register(native, root, batch string, admitted time.Time) archive.SessionRegistration {
	f.t.Helper()
	reg, err := f.store.RegisterNewSession(agentmeta.SessionKey{Agent: agentmeta.ID(archive.CanonicalHarness("claude")), NativeID: native}, func(id string) archive.SessionRegistration {
		origin, destination := archive.SessionOriginHook, ""
		if batch != "" {
			origin, destination = archive.SessionOriginImport, "dest"
		}
		return archive.SessionRegistration{
			ArchiveSessionID: id,
			NativeSessionID:  native,
			ProjectID:        archive.ProjectID(root),
			ProjectRoot:      root,
			Harness:          archive.Harness{Name: "claude"},
			SessionStartedAt: admitted.Add(-time.Hour),
			StartedAtSource:  archive.StartedAtSourceTranscript,
			RegisteredAt:     admitted,
			AdmittedAt:       admitted,
			Origin:           origin,
			ImportBatch:      archive.NewImportBatch(batch),
			DestinationID:    destination,
		}
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return reg
}

func (f *undoFixture) batch(id string, started time.Time, projectsAdded ...string) Batch {
	f.t.Helper()
	done := started.Add(time.Minute)
	b := Batch{ID: id, StartedAt: started, CompletedAt: &done, DestinationID: "dest",
		ProjectsAdded: append([]string{}, projectsAdded...), AppsAdded: []string{}, Sessions: []string{}, Subagents: []string{}}
	if err := SaveBatch(f.home, b); err != nil {
		f.t.Fatal(err)
	}
	return b
}

// OpenBatch tells an unreadable batch file (ErrUnreadableImport, which the
// CLI answers with advice about imports/) from registrations it can't list,
// which is a different problem.
func TestOpenBatchSeparatesRegistrationErrorsFromImportFiles(t *testing.T) {
	t.Parallel()
	f := newUndoFixture(t)
	if err := os.RemoveAll(filepath.Join(f.home, "registrations")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.home, "registrations"), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := OpenBatch(f.home, f.store, BatchFilters{}, "dest", fixedNow)
	if err == nil || errors.Is(err, ErrUnreadableImport) || !strings.Contains(err.Error(), "read registrations") {
		t.Fatalf("registrations: %v", err)
	}
	f = newUndoFixture(t)
	if err := os.MkdirAll(filepath.Join(f.home, "imports"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.home, "imports", "2026-09-22-1.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenBatch(f.home, f.store, BatchFilters{}, "dest", fixedNow); !errors.Is(err, ErrUnreadableImport) {
		t.Fatalf("batch file: %v", err)
	}
}

// B-1: a new import is numbered past every ID a registration still carries,
// so an ID whose batch file was moved aside is never reused.
func TestOpenBatchSkipsIDsRegistrationsStillCarry(t *testing.T) {
	t.Parallel()
	f := newUndoFixture(t)
	first, err := OpenBatch(f.home, f.store, BatchFilters{}, "dest", fixedNow)
	if err != nil || first.ID != "2026-09-23-1" {
		t.Fatalf("%+v %v", first, err)
	}
	f.register("old-native", "/p", first.ID, fixedNow.UTC())
	// No batch file was saved: as if it was moved out of imports/.
	second, err := OpenBatch(f.home, f.store, BatchFilters{Harnesses: []string{"codex"}}, "dest", fixedNow.Add(time.Hour))
	if err != nil || second.ID != "2026-09-23-2" {
		t.Fatalf("an ID an earlier import's sessions carry was reused: %+v %v", second, err)
	}
}

// B-1: when an ID was already reused (by an earlier release), undo refuses
// rather than remove the earlier import's sessions too.
func TestPlanUndoRefusesABatchIDSharedWithAnEarlierImport(t *testing.T) {
	t.Parallel()
	f := newUndoFixture(t)
	f.include("/p")
	f.register("earlier", "/p", "2026-09-23-1", fixedNow.UTC())
	later := f.batch("2026-09-23-1", fixedNow.Add(time.Hour).UTC())
	f.register("later", "/p", later.ID, later.StartedAt)
	_, err := PlanUndo(Environment{Sources: testSources, Discovery: builtin.NewBuiltins(), DatabaseCatalogs: builtin.NewBuiltins(), NativePaths: builtin.NewBuiltins(), Worktrees: builtin.NewBuiltins(), Workspaces: builtin.NewBuiltins(), Children: builtin.NewBuiltins(), Imports: builtin.NewBuiltins(), Home: f.home}, f.store, f.cfg, []Batch{later}, later, "")
	var shared *SharedBatchIDError
	if !errors.As(err, &shared) || shared.Sessions != 1 || !strings.Contains(err.Error(), "Nothing was changed") {
		t.Fatalf("err %v", err)
	}
	// Sessions admitted during the import's own run, including a continued
	// run after its start, are its own.
	f2 := newUndoFixture(t)
	f2.include("/p")
	own := f2.batch("2026-09-23-1", fixedNow.UTC())
	f2.register("a", "/p", own.ID, own.StartedAt)
	f2.register("b", "/p", own.ID, own.StartedAt.Add(30*time.Second))
	if p, err := PlanUndo(Environment{Sources: testSources, Discovery: builtin.NewBuiltins(), DatabaseCatalogs: builtin.NewBuiltins(), NativePaths: builtin.NewBuiltins(), Worktrees: builtin.NewBuiltins(), Workspaces: builtin.NewBuiltins(), Children: builtin.NewBuiltins(), Imports: builtin.NewBuiltins(), Home: f2.home}, f2.store, f2.cfg, []Batch{own}, own, ""); err != nil || len(p.Sessions) != 2 {
		t.Fatalf("own sessions: %d, %v", len(p.Sessions), err)
	}
}

// B-2: undoing import A keeps a project A added while import B still has
// sessions there, says so, and names it for B; B's undo then excludes it.
func TestUndoKeepsAProjectAnotherImportStillNeeds(t *testing.T) {
	t.Parallel()
	f := newUndoFixture(t)
	p := f.include("/work/p")
	q := f.include("/work/q")
	a := f.batch("2026-09-23-1", fixedNow.UTC(), p, q)
	f.register("a1", "/work/p", a.ID, a.StartedAt)
	f.register("a2", "/work/q", a.ID, a.StartedAt)
	b := f.batch("2026-09-23-2", fixedNow.Add(time.Hour).UTC())
	f.register("b1", "/work/p", b.ID, b.StartedAt)
	f.register("b2", "/work/p", b.ID, b.StartedAt)

	env := Environment{Sources: testSources, Discovery: builtin.NewBuiltins(), DatabaseCatalogs: builtin.NewBuiltins(), NativePaths: builtin.NewBuiltins(), Worktrees: builtin.NewBuiltins(), Workspaces: builtin.NewBuiltins(), Children: builtin.NewBuiltins(), Imports: builtin.NewBuiltins(), Home: f.home, Now: func() time.Time { return fixedNow }}
	plan, err := PlanUndo(env, f.store, f.cfg, []Batch{a, b}, a, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.ExcludeProjects) != 1 || plan.ExcludeProjects[0].ProjectID != q {
		t.Fatalf("excluded %+v", plan.ExcludeProjects)
	}
	if len(plan.KeepProjects) != 1 || plan.KeepProjects[0].Project.ProjectID != p || plan.KeepProjects[0].Sessions != 2 || plan.KeepProjects[0].Imports[0] != b.ID {
		t.Fatalf("kept %+v", plan.KeepProjects)
	}
	var out bytes.Buffer
	RenderUndo(&out, plan)
	for _, want := range []string{"1 project the import added stays included", "/work/p (2 sessions from import 2026-09-23-2)", "Undoing the last of those imports excludes it."} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("plan lacks %q:\n%s", want, out.String())
		}
	}

	// A's undo ran: its sessions are gone, q is excluded, p is kept for B.
	cfg := f.cfg
	plan.ApplyToConfig(&cfg)
	for _, s := range plan.Sessions {
		if _, err := f.store.ForgetIdleSession(s.Registration.ArchiveSessionID, agentmeta.SessionKey{Agent: agentmeta.ID(archive.CanonicalHarness(s.Registration.Harness.Name)), NativeID: s.Registration.NativeSessionID}, false, &state.RemovalRecord{Harness: "claude", Reason: state.RemovalReasonUndo, At: fixedNow}); err != nil {
			t.Fatal(err)
		}
	}
	undone := fixedNow.UTC()
	a.UndoneAt, a.ProjectsExcluded = &undone, []string{q}
	a.RecordKept(plan.KeepProjects)

	plan, err = PlanUndo(env, f.store, cfg, []Batch{a, b}, b, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.ExcludeProjects) != 1 || plan.ExcludeProjects[0].ProjectID != p || len(plan.KeepProjects) != 0 {
		t.Fatalf("B's undo: excluded %+v, kept %+v", plan.ExcludeProjects, plan.KeepProjects)
	}
}

// B-2: a project kept for another import is not taken over by an undo of an
// import that has no sessions there.
func TestUndoDoesNotTakeOverAKeptProjectItHasNoSessionsIn(t *testing.T) {
	t.Parallel()
	f := newUndoFixture(t)
	p := f.include("/work/p")
	undone := fixedNow.UTC()
	a := f.batch("2026-09-23-1", fixedNow.UTC(), p)
	a.UndoneAt, a.ProjectsKept = &undone, []string{p}
	b := f.batch("2026-09-23-2", fixedNow.Add(time.Hour).UTC())
	f.register("b1", "/work/p", b.ID, b.StartedAt)
	c := f.batch("2026-09-23-3", fixedNow.Add(2*time.Hour).UTC())
	f.register("c1", "/elsewhere", c.ID, c.StartedAt)
	plan, err := PlanUndo(Environment{Sources: testSources, Discovery: builtin.NewBuiltins(), DatabaseCatalogs: builtin.NewBuiltins(), NativePaths: builtin.NewBuiltins(), Worktrees: builtin.NewBuiltins(), Workspaces: builtin.NewBuiltins(), Children: builtin.NewBuiltins(), Imports: builtin.NewBuiltins(), Home: f.home}, f.store, f.cfg, []Batch{a, b, c}, c, "")
	if err != nil || len(plan.ExcludeProjects) != 0 || len(plan.KeepProjects) != 0 {
		t.Fatalf("C's undo touched p: %+v %+v %v", plan.ExcludeProjects, plan.KeepProjects, err)
	}
}

// B-4: undo planning reads only the stored bundles' evidence, not their
// records: a session with a large published bundle costs a small fraction of
// its size to check, and hook evidence in it is still found.
func TestResumedByEvidenceDoesNotDecodeRecords(t *testing.T) {
	f := newUndoFixture(t)
	reg := f.register("n1", "/p", "b", fixedNow.UTC())
	records := make([]map[string]any, 0, 50000)
	text := strings.Repeat("x", 200)
	for i := range cap(records) {
		records = append(records, map[string]any{"type": "user", "i": float64(i), "text": text})
	}
	evidence := []archive.SupplementalEvidence{{Kind: archive.EvidenceKindFinalResponse, ObservedAt: fixedNow.UTC(), Provenance: "hook:claude-stop", Payload: map[string]any{"text": "done"}}}
	bundle := archive.SourceBundle{SchemaVersion: archive.SourceSchemaVersion, ArchiveSessionID: reg.ArchiveSessionID, NativeRecords: records, SupplementalEvidence: evidence}
	if err := statetest.SavePublished(f.store, reg.ArchiveSessionID, bundle, fixedNow, state.CacheStatusPublished); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(f.home, "published", reg.ArchiveSessionID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	resumed, err := resumedByEvidence(Environment{Discovery: builtin.NewBuiltins()}, f.store, reg, state.Request{})
	runtime.ReadMemStats(&after)
	if err != nil || !resumed {
		t.Fatalf("resumed %v, %v", resumed, err)
	}
	// Reading the file costs its size once (measured 1.0x); decoding every
	// record as well cost 2.8x.
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 3*uint64(info.Size())/2 {
		t.Fatalf("allocated %d bytes for a %d byte published state", allocated, info.Size())
	}
}
