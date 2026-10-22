package backfill

import (
	"errors"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/state"
)

// Batches are numbered per local day. An interrupted batch is continued by
// a run with the same filters and destination, and by no other.
func TestOpenBatch(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	if err := os.Chmod(home, 0700); err != nil {
		t.Fatal(err)
	}
	store := state.OpenReadOnly(home)
	filters := Plan{Filters: Filters{Harnesses: []string{"claude-code"}, IncludeTemp: true}, projectFilter: []string{"/work/repo"}}.BatchFilters()
	if filters.Harnesses[0] != "claude" || filters.ProjectIDs[0] != archive.ProjectID("/work/repo") {
		t.Fatalf("filters %+v", filters)
	}
	b, err := OpenBatch(home, store, filters, "dest", fixedNow)
	if err != nil || b.ID != "2026-09-23-1" {
		t.Fatalf("%+v, %v", b, err)
	}
	if err := SaveBatch(home, b); err != nil {
		t.Fatal(err)
	}
	if again, _ := OpenBatch(home, store, filters, "dest", fixedNow.Add(time.Minute)); again.ID != b.ID {
		t.Fatalf("interrupted batch not continued: %s", again.ID)
	}
	for _, other := range []struct {
		filters BatchFilters
		dest    string
	}{{Plan{}.BatchFilters(), "dest"}, {filters, "elsewhere"}} {
		if next, _ := OpenBatch(home, store, other.filters, other.dest, fixedNow); next.ID != "2026-09-23-2" {
			t.Fatalf("different run continued the batch: %s", next.ID)
		}
	}
	done := fixedNow.UTC()
	b.CompletedAt = &done
	if err := SaveBatch(home, b); err != nil {
		t.Fatal(err)
	}
	if next, _ := OpenBatch(home, store, filters, "dest", fixedNow); next.ID != "2026-09-23-2" {
		t.Fatalf("completed batch continued: %s", next.ID)
	}
	if tomorrow, _ := OpenBatch(home, store, filters, "dest", fixedNow.Add(24*time.Hour)); tomorrow.ID != "2026-09-24-1" {
		t.Fatalf("next day: %s", tomorrow.ID)
	}
	batches, err := LoadBatches(home)
	if err != nil || len(batches) != 1 || batches[0].CompletedAt == nil {
		t.Fatalf("%+v, %v", batches, err)
	}
}

// New projects are added included, activated at the import, in the plan's
// spelling; apps without hooks are added to ImportedHarnesses.
func TestApplyToConfigAndClock(t *testing.T) {
	t.Parallel()
	admitted := fixedNow.UTC()
	cfg := config.Config{Harnesses: []string{"claude"}, Archive: archive.Config{Projects: []archive.ProjectActivation{
		{ProjectID: archive.ProjectID("/work/in"), Root: "/work/in", Included: true, ActivatedAt: admitted.Add(-time.Hour)},
	}}}
	p := Plan{Home: "/Users/p", Harnesses: cfg.Harnesses, Candidates: []Candidate{
		{Harness: "claude", ProjectRoot: "/work/in", ProjectIncluded: true},
		{Harness: "codex", ProjectRoot: "/work/new"},
		{Harness: "claude", ProjectRoot: "/work/new"},
		{Harness: "cursor", ProjectRoot: "/work/skipped", Skip: SkipTemporaryDirectory},
	}}
	if err := CheckClock(cfg, p, admitted); err != nil {
		t.Fatal(err)
	}
	if err := CheckClock(cfg, p, admitted.Add(-2*time.Hour)); err == nil || !strings.Contains(err.Error(), "check the clock") {
		t.Fatalf("clock before activation: %v", err)
	}
	cfg.DestinationSince = admitted.Add(time.Minute)
	if err := CheckClock(cfg, p, admitted); err == nil {
		t.Fatal("clock before the destination accepted")
	}
	cfg.DestinationSince = time.Time{}
	// A clock set back between the plan and the commit.
	p.GeneratedAt = admitted
	if err := CheckClock(cfg, p, admitted.Add(-time.Minute)); err == nil || !strings.Contains(err.Error(), "earlier than when this plan was made") {
		t.Fatalf("clock before the plan: %v", err)
	}
	if err := CheckClock(cfg, p, admitted); err != nil {
		t.Fatalf("admission at the plan's time: %v", err)
	}

	changes, err := ApplyToConfig(&cfg, p, admitted)
	if err != nil {
		t.Fatal(err)
	}
	if projects, apps := changes.ProjectIDs, changes.Apps; len(projects) != 1 || projects[0] != archive.ProjectID("/work/new") || strings.Join(apps, ",") != "codex" || changes.Retention != nil {
		t.Fatalf("added %+v", changes)
	}
	added := cfg.Archive.Projects[1]
	if added.Root != "/work/new" || !added.Included || !added.ActivatedAt.Equal(admitted) {
		t.Fatalf("%+v", added)
	}
	if again, err := ApplyToConfig(&cfg, p, admitted); err != nil || len(again.ProjectIDs) != 0 || len(again.Apps) != 0 || len(cfg.Archive.Projects) != 2 {
		t.Fatalf("applied twice: %+v %v %+v", again, err, cfg.Archive.Projects)
	}
}

// Each imported registration records the destination the import was
// confirmed for. If the configured destination is a different one by the
// time a session is registered, the session is not admitted.
func TestRegistrationRecordsTheConfirmedDestination(t *testing.T) {
	t.Parallel()
	home, project := t.TempDir(), t.TempDir()
	admitted := fixedNow.UTC()
	bucketA := credentials.Config{Provider: "s3", Bucket: "a"}
	cfg := config.Config{Storage: bucketA, Archive: archive.Config{Enabled: true, Projects: []archive.ProjectActivation{
		{ProjectID: archive.ProjectID(project), Root: project, Included: true, ActivatedAt: admitted},
	}}}
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	candidate := func(id string) Candidate {
		path := filepath.Join(project, id+".jsonl")
		if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return Candidate{Harness: "claude", NativeSessionID: id, TranscriptPath: path, ProjectRoot: project, StartedAt: admitted.Add(-time.Hour), StartedAtSource: archive.StartedAtSourceTranscript}
	}
	r := Registration{Home: home, Store: store, Batch: "2026-09-23-1", AdmittedAt: admitted, DestinationID: config.DestinationID(bucketA)}
	result, err := r.Run([]Candidate{candidate("a")})
	if err != nil || len(result.Sessions) != 1 {
		t.Fatalf("%+v %v", result, err)
	}
	if reg, _, _ := store.LoadRegistration(result.Sessions[0]); reg.DestinationID != config.DestinationID(bucketA) {
		t.Fatalf("destination ID %q, want bucket A's", reg.DestinationID)
	}
	r.DestinationID = config.DestinationID(credentials.Config{Provider: "s3", Bucket: "b"})
	result, err = r.Run([]Candidate{candidate("b")})
	if err != nil || len(result.Sessions) != 0 || result.NotAdmitted != 1 {
		t.Fatalf("an import confirmed for bucket B was registered while the destination is A: %+v %v", result, err)
	}
}

// Registration skips, and counts, what changed since the plan: a transcript
// that is gone, a session registered meanwhile, and a project no longer
// admitted.
func TestRegistrationSkipsChanges(t *testing.T) {
	t.Parallel()
	home, project := t.TempDir(), t.TempDir()
	admitted := fixedNow.UTC()
	cfg := config.Config{Archive: archive.Config{Enabled: true, Projects: []archive.ProjectActivation{
		{ProjectID: archive.ProjectID(project), Root: project, Included: true, ActivatedAt: admitted},
		{ProjectID: "x", Root: "/work/excluded", Included: false},
	}}}
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	candidate := func(id, root string) Candidate {
		path := filepath.Join(project, id+".jsonl")
		if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return Candidate{Harness: "claude", NativeSessionID: id, TranscriptPath: path, ProjectRoot: root, StartedAt: admitted.Add(-time.Hour), StartedAtSource: archive.StartedAtSourceTranscript}
	}
	ok, gone, taken, excluded := candidate("ok", project), candidate("gone", project), candidate("taken", project), candidate("excluded", "/work/excluded")
	if err := os.Remove(gone.TranscriptPath); err != nil {
		t.Fatal(err)
	}
	for _, native := range []string{"taken", "taken-future"} {
		if _, err := store.RegisterNewSession(agentmeta.SessionKey{Agent: agentmeta.ID(archive.CanonicalHarness("claude")), NativeID: native}, func(id string) archive.SessionRegistration {
			return archive.SessionRegistration{ArchiveSessionID: id, NativeSessionID: native, ProjectID: "p", ProjectRoot: project, Harness: archive.Harness{Name: "claude"}, SessionStartedAt: admitted}
		}); err != nil {
			t.Fatal(err)
		}
	}
	// A start after the admission is skipped at registration too, as a
	// backstop to CheckClock. A session a hook registered meanwhile is
	// already archived whatever its planned start: that reason comes first.
	future := candidate("future", project)
	future.StartedAt = admitted.Add(time.Second)
	takenFuture := candidate("taken-future", project)
	takenFuture.StartedAt = admitted.Add(time.Second)
	r := Registration{Home: home, Store: store, Batch: "2026-09-23-1", AdmittedAt: admitted}
	result, err := r.Run([]Candidate{ok, gone, taken, excluded, future, takenFuture})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Sessions) != 1 || result.Gone != 1 || result.AlreadyArchived != 2 || result.NotAdmitted != 1 || result.StartInFuture != 1 {
		t.Fatalf("%+v", result)
	}
	if _, found, _ := store.ArchiveSessionID(agentmeta.SessionKey{Agent: agentmeta.ID(archive.CanonicalHarness("claude")), NativeID: "future"}); found {
		t.Fatal("a session starting after its admission was given an archive ID")
	}
	reg, _, _ := store.LoadRegistration(result.Sessions[0])
	if reg.Origin != archive.SessionOriginImport || reg.ImportBatch.Recorded() != "2026-09-23-1" || !reg.AdmittedAt.Equal(admitted) || reg.NativeSessionID != "ok" {
		t.Fatalf("%+v", reg)
	}

	// A session that cannot be registered, and a subagent whose candidate
	// conflicts with an earlier one, are skipped and counted; the import
	// goes on.
	noStart := candidate("no-start", project)
	noStart.StartedAt = time.Time{}
	parent := candidate("parent", project)
	subPath := filepath.Join(project, "agent-s1.jsonl")
	if err := os.WriteFile(subPath, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	parent.Subagents = []Subagent{{Path: subPath, AgentID: "s1"}, {Path: subPath, AgentID: "s2"}}
	parentID, _, _ := store.EnsureArchiveSessionID(agentmeta.SessionKey{Agent: agentmeta.ID(archive.CanonicalHarness("claude")), NativeID: "parent"})
	childID, _, _ := store.EnsureArchiveSessionID(agentmeta.SessionKey{Agent: agentmeta.ID(archive.CanonicalHarness("claude")), NativeID: "parent:subagent:s1"})
	if err := store.SaveSubagentCandidate(state.SubagentCandidate{
		ArchiveSessionID: childID, NativeSessionID: "parent:subagent:s1", ParentArchiveSessionID: parentID, ParentNativeSessionID: "parent",
		ProjectID: archive.ProjectID(project), ProjectRoot: project, Harness: archive.Harness{Name: "claude"},
		AgentID: "s1", TranscriptPath: "/elsewhere/agent-s1.jsonl", ObservedAt: admitted,
	}); err != nil {
		t.Fatal(err)
	}
	later := candidate("later", project)
	// A session registered meanwhile counts as already archived even when
	// the plan's record of it would not be valid.
	takenNoStart := taken
	takenNoStart.StartedAt = time.Time{}
	result, err = r.Run([]Candidate{noStart, parent, later, takenNoStart})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Sessions) != 2 || result.Invalid != 1 || result.AlreadyArchived != 1 || result.SubagentsInvalid != 1 || len(result.Subagents) != 1 {
		t.Fatalf("%+v", result)
	}

	// Stop ends registration between holds.
	r.MaxHoldSteps = 1
	holds := 0
	r.Stop = func() bool { holds++; return holds > 1 }
	result, err = r.Run([]Candidate{candidate("stop-1", project), candidate("stop-2", project)})
	if !errors.Is(err, ErrStopped) || len(result.Sessions) != 0 {
		t.Fatalf("stop: %+v, %v", result, err)
	}
}

// Reconcile rebuilds a batch's sessions from the registrations carrying its
// ID, and its subagents from their imported candidates.
func TestBatchReconcile(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	save := func(id, batch, parent string, origins ...archive.SessionOrigin) {
		origin := archive.SessionOriginImport
		if len(origins) > 0 {
			origin = origins[0]
		}
		t.Helper()
		reg := archive.SessionRegistration{ArchiveSessionID: id, NativeSessionID: "n-" + id, ProjectID: "p", ProjectRoot: "/p", Harness: archive.Harness{Name: "claude"},
			SessionStartedAt: fixedNow, ImportBatch: archive.NewImportBatch(batch), ParentSessionID: parent, Origin: origin}
		if err := store.SaveRegistration(reg); err != nil {
			t.Fatal(err)
		}
	}
	save("a", "2026-09-23-1", "")
	save("b", "2026-09-23-1", "")
	save("c", "2026-09-23-2", "")
	save("a-child", "2026-09-23-1", "a")
	// A hook registration carrying the ID is not the import's (B-23).
	save("hook", "2026-09-23-1", "", archive.SessionOriginHook)
	if err := store.SaveSubagentCandidate(state.SubagentCandidate{
		ArchiveSessionID: "b-child", NativeSessionID: "n-b:subagent:x", ParentArchiveSessionID: "b", ParentNativeSessionID: "n-b",
		ProjectID: "p", ProjectRoot: "/p", Harness: archive.Harness{Name: "claude"}, AgentID: "x", TranscriptPath: "/p/x.jsonl",
		ObservedAt: fixedNow, Origin: archive.SessionOriginImport,
	}); err != nil {
		t.Fatal(err)
	}
	b := Batch{ID: "2026-09-23-1", Sessions: []string{"a"}}
	if err := b.Reconcile(store); err != nil {
		t.Fatal(err)
	}
	if strings.Join(b.Sessions, ",") != "a,b" || strings.Join(b.Subagents, ",") != "a-child,b-child" {
		t.Fatalf("sessions %v, subagents %v", b.Sessions, b.Subagents)
	}
}

// The same --project folder typed through a symlink records the same
// filters, so either spelling continues the batch.
func TestBatchFiltersResolveProjects(t *testing.T) {
	t.Parallel()
	tr := newTree(t)
	repo := tr.repo("home/repo")
	if err := os.Symlink(tr.home, tr.path("alias")); err != nil {
		t.Fatal(err)
	}
	direct := plan(t, tr.env(), nil, config.Config{}, Filters{Projects: []string{repo}}).BatchFilters()
	aliased := plan(t, tr.env(), nil, config.Config{}, Filters{Projects: []string{filepath.Join(tr.path("alias"), "repo")}}).BatchFilters()
	if !direct.equal(aliased) {
		t.Fatalf("%+v != %+v", direct, aliased)
	}
}
