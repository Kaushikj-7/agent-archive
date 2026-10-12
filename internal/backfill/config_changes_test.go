package backfill

import (
	"bytes"
	"errors"
	"github.com/wangjohn/agent-archive/internal/agents/builtin"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/state"
)

// Regression: 2026-09 review B-21. An import never lowers the archive-wide
// retention, nor turns it on while it is off: retention applies to every
// session in the archive, and a shorter one deletes hook-captured sessions
// too. ApplyToConfig refuses it and leaves the configuration as it was.
func TestApplyToConfigNeverShortensRetention(t *testing.T) {
	t.Parallel()
	p := Plan{Candidates: []Candidate{{Harness: "claude", ProjectRoot: "/work/new"}}}
	for _, tc := range []struct {
		configured int
		planned    int
	}{{90, 7}, {90, 89}, {0, 90}} {
		cfg := config.Config{RetentionDays: tc.configured}
		p.RetentionDays = tc.planned
		if _, err := ApplyToConfig(&cfg, p, fixedNow); !errors.Is(err, ErrRetentionShortened) {
			t.Fatalf("%d -> %d: %v", tc.configured, tc.planned, err)
		}
		if cfg.RetentionDays != tc.configured || len(cfg.Archive.Projects) != 0 {
			t.Fatalf("%d -> %d changed the configuration: %+v", tc.configured, tc.planned, cfg)
		}
	}
	cfg := config.Config{RetentionDays: 90}
	p.RetentionDays = 365
	changes, err := ApplyToConfig(&cfg, p, fixedNow)
	if err != nil || cfg.RetentionDays != 365 || changes.Retention == nil || *changes.Retention != (RetentionChange{From: 90, To: 365}) {
		t.Fatalf("raise: %+v %v %d", changes, err, cfg.RetentionDays)
	}
}

// A continued import keeps the retention it first raised from, so its undo
// restores what was there before the first run.
func TestBatchKeepsFirstRetentionAcrossRuns(t *testing.T) {
	t.Parallel()
	var b Batch
	b.AddChanges(ConfigChanges{Retention: &RetentionChange{From: 30, To: 90}})
	b.AddChanges(ConfigChanges{})
	b.AddChanges(ConfigChanges{Retention: &RetentionChange{From: 90, To: 365}})
	if *b.Retention != (RetentionChange{From: 30, To: 365}) {
		t.Fatalf("%+v", *b.Retention)
	}
}

// Guard (B-21 hardening): every configuration field an import changes is
// recorded in the batch and put back by its undo. The test applies an import
// that changes all it can (projects, a kept-out folder, an app, the
// retention), then diffs every field of config.Config, so a future change
// ApplyToConfig makes without recording it, or that undo does not reverse,
// fails here. The one intended difference after the undo: an added project
// stays configured, excluded.
func TestImportConfigChangesAreRecordedAndUndone(t *testing.T) {
	t.Parallel()
	f := newUndoFixture(t)
	f.include("/work/in")
	f.cfg.Harnesses = []string{"claude"}
	f.cfg.RetentionDays = 90
	before := cloneConfig(t, f.cfg)
	plan := Plan{Harnesses: f.cfg.Harnesses, RetentionDays: 365, Candidates: []Candidate{
		{Harness: "claude", ProjectRoot: "/work/in", ProjectIncluded: true},
		{Harness: "codex", ProjectRoot: "/work/new"},
		{Harness: "claude", ProjectRoot: "/work/dir", ProjectKind: ProjectKindDirectory},
	},
		// A plain folder the import adds keeps a repository inside it out.
		nested: map[string]nestedFolders{"/work/dir": {KeptOut: []string{"/work/dir/repo"}, Complete: true}},
	}
	cfg := cloneConfig(t, f.cfg)
	admitted := fixedNow.Add(-time.Hour).UTC()
	changes, err := ApplyToConfig(&cfg, plan, admitted)
	if err != nil {
		t.Fatal(err)
	}
	b := f.batch("2026-09-23-1", admitted)
	b.AddChanges(changes)

	// Every changed field is one the batch records.
	recorded := map[string]func() bool{
		"Archive":           func() bool { return len(b.ProjectsAdded) == 2 && len(b.ProjectsKeptOut) == 1 },
		"ImportedHarnesses": func() bool { return strings.Join(b.AppsAdded, ",") == "codex" },
		"RetentionDays":     func() bool { return b.Retention != nil && b.Retention.From == 90 && b.Retention.To == 365 },
	}
	changed := changedFields(before, cfg)
	if len(changed) != len(recorded) {
		t.Errorf("import changed %v; the batch records %d fields", changed, len(recorded))
	}
	for _, field := range changed {
		check, ok := recorded[field]
		if !ok {
			t.Errorf("an import changes config.%s, which the batch does not record and undo does not restore", field)
			continue
		}
		if !check() {
			t.Errorf("config.%s changed but the batch did not record it: %+v", field, b)
		}
	}

	f.cfg = cfg
	undo, err := PlanUndo(Environment{Sources: testSources, Discovery: builtin.NewBuiltins(), DatabaseCatalogs: builtin.NewBuiltins(), NativePaths: builtin.NewBuiltins(), Worktrees: builtin.NewBuiltins(), Workspaces: builtin.NewBuiltins(), Children: builtin.NewBuiltins(), Imports: builtin.NewBuiltins(), Home: f.home, Now: func() time.Time { return fixedNow }}, f.store, cfg, []Batch{b}, b, "")
	if err != nil {
		t.Fatal(err)
	}
	after := cloneConfig(t, cfg)
	undone := undo.ApplyToConfig(&after)
	if !undone.RetentionRestored || len(undone.Excluded) != 2 || len(undone.RemovedKeptOut) != 1 {
		t.Fatalf("undo changes %+v", undone)
	}
	// The added projects stay, excluded; the kept-out entry is gone;
	// everything else is as before.
	want := cloneConfig(t, before)
	for _, p := range after.Archive.Projects {
		if slices.Contains([]string{"/work/new", "/work/dir"}, p.Root) {
			if p.Included {
				t.Fatalf("added project still included: %+v", p)
			}
			want.Archive.Projects = append(want.Archive.Projects, p)
		}
	}
	if diff := changedFields(want, after); len(diff) != 0 {
		t.Fatalf("undo left config fields changed: %v\nbefore %+v\nafter  %+v", diff, want, after)
	}
}

// changedFields names the top-level config.Config fields that differ.
func changedFields(a, b config.Config) []string {
	var out []string
	va, vb := reflect.ValueOf(a), reflect.ValueOf(b)
	for i := range va.NumField() {
		fa, fb := va.Field(i), vb.Field(i)
		// An empty list or map is the same setting whether it is nil or not.
		if k := fa.Kind(); (k == reflect.Slice || k == reflect.Map) && fa.Len() == 0 && fb.Len() == 0 {
			continue
		}
		if !reflect.DeepEqual(fa.Interface(), fb.Interface()) {
			out = append(out, va.Type().Field(i).Name)
		}
	}
	return out
}

func cloneConfig(t *testing.T, cfg config.Config) config.Config {
	t.Helper()
	out := cfg
	out.Archive.Projects = append([]archive.ProjectActivation(nil), cfg.Archive.Projects...)
	out.Harnesses = append([]string(nil), cfg.Harnesses...)
	out.ImportedHarnesses = append([]string(nil), cfg.ImportedHarnesses...)
	return out
}

// B-21: undo puts the retention from before the import back, but only while
// it is still what the import set, and says how many sessions the shorter
// retention then deletes: those admitted (or last captured) over the old
// period ago and not already past the new one, and not removed by the undo.
func TestUndoRestoresRetentionAndCountsWhatItDeletes(t *testing.T) {
	t.Parallel()
	f := newUndoFixture(t)
	f.include("/work/p")
	f.cfg.RetentionDays = 365
	env := Environment{Sources: testSources, Discovery: builtin.NewBuiltins(), DatabaseCatalogs: builtin.NewBuiltins(), NativePaths: builtin.NewBuiltins(), Worktrees: builtin.NewBuiltins(), Workspaces: builtin.NewBuiltins(), Children: builtin.NewBuiltins(), Imports: builtin.NewBuiltins(), Home: f.home, Now: func() time.Time { return fixedNow }}
	day := 24 * time.Hour
	b := f.batch("2026-09-23-1", fixedNow.Add(-time.Hour))
	b.Retention = &RetentionChange{From: 30, To: 365}
	f.register("imported", "/work/p", b.ID, fixedNow.Add(-time.Hour+time.Minute))
	f.register("recent-hook", "/work/p", "", fixedNow.Add(-10*day))
	f.register("old-hook", "/work/p", "", fixedNow.Add(-100*day))
	f.register("ancient-hook", "/work/p", "", fixedNow.Add(-400*day))
	recaptured := f.register("recaptured-hook", "/work/p", "", fixedNow.Add(-100*day))
	published, err := f.store.LoadPublishedState(recaptured.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := published.Save(archive.SourceBundle{Capture: archive.SourceCapture{CapturedAt: fixedNow.Add(-5 * day)}}, fixedNow.Add(-5*day), state.CacheStatusPublished); err != nil {
		t.Fatal(err)
	}

	plan, err := PlanUndo(env, f.store, f.cfg, []Batch{b}, b, "")
	if err != nil {
		t.Fatal(err)
	}
	if plan.RestoreRetention == nil || plan.RetentionDeletes != 1 {
		t.Fatalf("restore %+v, deletes %d; want only old-hook", plan.RestoreRetention, plan.RetentionDeletes)
	}
	var out bytes.Buffer
	RenderUndo(&out, plan)
	if !strings.Contains(out.String(), "Retention goes back from 365 to 30 days") || !strings.Contains(out.String(), "deletes 1 session older than\n    30 days") {
		t.Fatalf("plan:\n%s", out.String())
	}

	// Applied under the locks, it restores only while retention is still
	// what the import set.
	changed := f.cfg
	changed.RetentionDays = 200
	if c := plan.ApplyToConfig(&changed); c.RetentionRestored || changed.RetentionDays != 200 {
		t.Fatalf("restored over a retention changed since: %+v, %d", c, changed.RetentionDays)
	}

	// Rechecked under the locks, a restore the confirmed plan lacked, or one
	// that now deletes more, is refused (Grew).
	if !plan.Grew(UndoPlan{}) {
		t.Fatal("a restore that appeared since the confirmation was not refused")
	}
	fewer := plan
	fewer.RetentionDeletes = 0
	if !plan.Grew(fewer) || plan.Grew(plan) {
		t.Fatal("Grew does not compare the sessions the restore deletes")
	}

	// A --project undo leaves retention alone.
	if plan, err = PlanUndo(env, f.store, f.cfg, []Batch{b}, b, "/work/p"); err != nil || plan.RestoreRetention != nil {
		t.Fatalf("--project: %+v %v", plan.RestoreRetention, err)
	}
	// Changed since the import: left alone, and the plan says so.
	f.cfg.RetentionDays = 200
	if plan, err = PlanUndo(env, f.store, f.cfg, []Batch{b}, b, ""); err != nil || plan.RestoreRetention != nil || !plan.RetentionChangedSince {
		t.Fatalf("changed since: %+v %v", plan.RestoreRetention, err)
	}
	out.Reset()
	RenderUndo(&out, plan)
	if !strings.Contains(out.String(), "Retention stays at 200 days") {
		t.Fatalf("plan:\n%s", out.String())
	}
	// Already restored once: never again, even if it is back at To.
	f.cfg.RetentionDays = 365
	b.Retention.Restored = true
	if plan, err = PlanUndo(env, f.store, f.cfg, []Batch{b}, b, ""); err != nil || plan.RestoreRetention != nil {
		t.Fatalf("restored: %+v %v", plan.RestoreRetention, err)
	}
}

func TestImportForeverJournalAndUndo(t *testing.T) {
	cfg := config.Config{RetentionDays: 90}
	changes, err := ApplyToConfig(&cfg, Plan{RetentionDays: 0}, fixedNow)
	if err != nil || changes.Retention == nil || changes.Retention.From != 90 || changes.Retention.To != 0 || cfg.RetentionDays != 0 || !cfg.RetentionProtection {
		t.Fatalf("raise %+v %+v %v", cfg, changes, err)
	}
	plan := UndoPlan{RestoreRetention: changes.Retention}
	plan.ApplyToConfig(&cfg)
	if cfg.RetentionDays != 90 || !cfg.RetentionProtection {
		t.Fatalf("restore loses fence %+v", cfg)
	}
	if date, expires := (Plan{RetentionDays: 0}).ExpiresOn(); expires || date != "" {
		t.Fatalf("forever expiry %q %v", date, expires)
	}
}
