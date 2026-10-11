package discovery

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agents/builtin"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/state"
)

func refusedCandidate(id string, started time.Time) (Candidate, string) {
	loc := "/codex/sessions/rollout-" + id + ".jsonl"
	return Candidate{Agent: "codex", NativeSessionID: id, StartedAt: started}, loc
}

func TestRefusedStartsRecordsTopLevelStartsShortlyBeforeTheFloor(t *testing.T) {
	t.Parallel()
	floor := time.Date(2026, 10, 5, 21, 43, 0, 0, time.UTC)
	home := t.TempDir()
	r := &refusedStarts{floor: floor, sessions: map[string]RefusedStart{}}
	cache := map[string]cached{}
	add := func(c Candidate, loc string) {
		cache[loc] = cached{}
		r.refuse(c, loc)
	}
	add(refusedCandidate("recent", floor.Add(-time.Hour)))
	add(refusedCandidate("eight-days", floor.Add(-RefusedStartLookback-24*time.Hour)))
	add(refusedCandidate("too-old", floor.Add(-RefusedStartLookback-24*time.Hour-time.Second)))
	add(refusedCandidate("at-floor", floor))
	child, loc := refusedCandidate("child", floor.Add(-time.Hour))
	child.NativeChild = true
	add(child, loc)
	forked, loc := refusedCandidate("parented", floor.Add(-time.Hour))
	forked.ParentNativeID = "recent"
	add(forked, loc)
	long, _ := refusedCandidate("long", floor.Add(-time.Hour))
	add(long, "/"+strings.Repeat("x", maxRefusedLocator))
	must(t, r.save(home, cache))
	got, err := ReadRefusedStarts(home)
	must(t, err)
	if len(got) != 2 || got[0].NativeSessionID != "recent" || got[1].NativeSessionID != "eight-days" || got[0].Locator != "/codex/sessions/rollout-recent.jsonl" || !got[0].StartedAt.Equal(floor.Add(-time.Hour)) {
		t.Fatalf("recorded %+v", got)
	}

	// A later pass that admits or settles one drops it; one whose source
	// is gone from the catalog is dropped too.
	reloaded := loadRefusedStarts(home, config.Config{}, "codex")
	reloaded.floor = floor
	settled, _ := refusedCandidate("recent", floor.Add(-time.Hour))
	reloaded.settle(settled)
	must(t, reloaded.save(home, cache))
	if got, err := ReadRefusedStarts(home); err != nil || len(got) != 1 || got[0].NativeSessionID != "eight-days" {
		t.Fatalf("after settle: %+v %v", got, err)
	}
	reloaded = loadRefusedStarts(home, config.Config{}, "codex")
	reloaded.floor = floor
	must(t, reloaded.save(home, map[string]cached{}))
	if _, err := os.Stat(filepath.Join(home, refusedStartsName)); !os.IsNotExist(err) {
		t.Fatalf("an empty record was kept: %v", err)
	}
	if got, err := ReadRefusedStarts(home); err != nil || got != nil {
		t.Fatalf("missing record: %+v %v", got, err)
	}
}

func TestRefusedStartsRefusalAfterTheFloorClearsAnEarlierEntry(t *testing.T) {
	t.Parallel()
	floor := time.Date(2026, 10, 5, 21, 43, 0, 0, time.UTC)
	r := &refusedStarts{floor: floor, sessions: map[string]RefusedStart{}}
	c, loc := refusedCandidate("moved", floor.Add(-time.Hour))
	r.refuse(c, loc)
	// Its start is re-read as after the floor (a paused interval refuses
	// it): it is no longer a pre-setup session.
	c.StartedAt = floor.Add(time.Hour)
	r.refuse(c, loc)
	if len(r.sessions) != 0 {
		t.Fatalf("kept %+v", r.sessions)
	}
	// Without a floor nothing is recorded.
	none := &refusedStarts{sessions: map[string]RefusedStart{}}
	c.StartedAt = floor.Add(-time.Hour)
	none.refuse(c, loc)
	if len(none.sessions) != 0 {
		t.Fatalf("recorded without a floor: %+v", none.sessions)
	}
	var missing *refusedStarts
	missing.refuse(c, loc)
	missing.settle(c)
	must(t, missing.save(t.TempDir(), nil))
}

func TestRefusedStartsKeepsTheLatestWithinTheBound(t *testing.T) {
	t.Parallel()
	floor := time.Date(2026, 10, 5, 21, 43, 0, 0, time.UTC)
	home := t.TempDir()
	r := &refusedStarts{floor: floor, sessions: map[string]RefusedStart{}}
	cache := map[string]cached{}
	for i := range maxRefusedStarts + 10 {
		c, loc := refusedCandidate(fmt.Sprintf("s%03d", i), floor.Add(-time.Duration(i+1)*time.Minute))
		cache[loc] = cached{}
		r.refuse(c, loc)
	}
	must(t, r.save(home, cache))
	got, err := ReadRefusedStarts(home)
	must(t, err)
	if len(got) != maxRefusedStarts || got[0].NativeSessionID != "s000" || got[len(got)-1].NativeSessionID != fmt.Sprintf("s%03d", maxRefusedStarts-1) {
		t.Fatalf("kept %d, first %+v last %+v", len(got), got[0], got[len(got)-1])
	}
	if len(r.sessions) != maxRefusedStarts {
		t.Fatalf("in-memory record kept %d", len(r.sessions))
	}
}

func TestRefusedStartsUnchangedRecordIsNotRewritten(t *testing.T) {
	t.Parallel()
	floor := time.Date(2026, 10, 5, 21, 43, 0, 0, time.UTC)
	home := t.TempDir()
	r := &refusedStarts{floor: floor, sessions: map[string]RefusedStart{}}
	c, loc := refusedCandidate("same", floor.Add(-time.Hour))
	cache := map[string]cached{loc: {}}
	r.refuse(c, loc)
	must(t, r.save(home, cache))
	path := filepath.Join(home, refusedStartsName)
	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	must(t, os.Chtimes(path, old, old))
	r.refuse(c, loc)
	must(t, r.save(home, cache))
	info, err := os.Stat(path)
	must(t, err)
	if !info.ModTime().Equal(old) {
		t.Fatalf("an unchanged record was rewritten at %v", info.ModTime())
	}
}

func TestReadRefusedStartsRejectsInvalidRecords(t *testing.T) {
	t.Parallel()
	for name, body := range map[string]string{
		"version":   `{"version":2,"sessions":[]}`,
		"corrupt":   `{`,
		"oversized": `{"version":1,"sessions":[],"pad":"` + strings.Repeat("x", maxRefusedStartsBytes) + `"}`,
		"too many":  `{"version":1,"sessions":[` + strings.TrimSuffix(strings.Repeat(`{"agent":"codex"},`, maxRefusedStarts+1), ",") + `]}`,
	} {
		home := t.TempDir()
		must(t, os.WriteFile(filepath.Join(home, refusedStartsName), []byte(body), 0o600))
		if got, err := ReadRefusedStarts(home); err == nil {
			t.Errorf("%s: read %+v", name, got)
		}
		// A scan rebuilds an unreadable record rather than keeping it.
		r := loadRefusedStarts(home, config.Config{}, "codex")
		must(t, r.save(home, nil))
		if _, err := os.Stat(filepath.Join(home, refusedStartsName)); !os.IsNotExist(err) {
			t.Errorf("%s: unreadable record kept: %v", name, err)
		}
	}
}

// In all-projects scope a project rule refuses through the same start
// check: a session in an excluded project is not a pre-setup session, since
// backfill skips it and status would name it for as long as it is kept.
func TestRefusedStartsSkipsExcludedProjectsInAllProjectsScope(t *testing.T) {
	t.Parallel()
	store, cfg, at, codex := fixture(t)
	parent, err := filepath.EvalSymlinks(t.TempDir())
	must(t, err)
	kept, excluded := filepath.Join(parent, "kept"), filepath.Join(parent, "excluded")
	for _, dir := range []string{kept, excluded} {
		must(t, os.MkdirAll(filepath.Join(dir, ".git"), 0o700))
	}
	cfg.Archive.Projects = append(cfg.Archive.Projects,
		archive.ProjectActivation{Root: parent, ProjectID: archive.ProjectID(parent), Included: true, ActivatedAt: at},
		archive.ProjectActivation{Root: excluded, ProjectID: archive.ProjectID(excluded), Included: false, ActivatedAt: at})
	setTestCodexScope(&cfg, config.CodexAllProjects)
	must(t, config.ReconcileDiscovery(&cfg, config.Config{}, at))
	must(t, config.Save(store.Home(), cfg))
	floor, ok := cfg.DiscoveryStartFloor("codex")
	if !ok {
		t.Fatal("no start floor")
	}
	keptID := writeRollout(t, codex, kept, floor.Add(-time.Hour), 1, "sessions")
	writeRollout(t, codex, excluded, floor.Add(-time.Hour), 2, "sessions")
	h, err := run(context.Background(), store, cfg, Options{Sources: builtin.NewBuiltins(), Now: func() time.Time { return at.Add(2 * time.Minute) }}, syntheticSupport)
	if err != nil || h.Registered != 0 || h.Outcomes["start_not_authorized"] != 2 {
		t.Fatalf("scan %#v %v", h, err)
	}
	got, err := ReadRefusedStarts(store.Home())
	if err != nil || len(got) != 1 || got[0].NativeSessionID != keptID {
		t.Fatalf("recorded %+v %v", got, err)
	}
}

// A scan that reaches a recorded session again drops it once it has been
// removed, or once its project is no longer included.
func TestRefusedStartsScanDropsRemovedAndExcludedSessions(t *testing.T) {
	t.Parallel()
	store, cfg, at, codex := fixture(t)
	project := cfg.Archive.Projects[0].Root
	// Another included project keeps a start floor once project is excluded.
	other, err := filepath.EvalSymlinks(t.TempDir())
	must(t, err)
	before := cfg
	cfg.Archive.Projects = append(slices.Clone(cfg.Archive.Projects), archive.ProjectActivation{Root: other, ProjectID: archive.ProjectID(other), Included: true, ActivatedAt: at})
	must(t, config.ReconcileDiscovery(&cfg, before, at))
	floor, ok := cfg.DiscoveryStartFloor("codex")
	if !ok {
		t.Fatal("no start floor")
	}
	removed := writeRollout(t, codex, project, floor.Add(-time.Hour), 1, "sessions")
	kept := writeRollout(t, codex, project, floor.Add(-2*time.Hour), 2, "sessions")
	scan := func(cfg config.Config, after time.Duration) []RefusedStart {
		t.Helper()
		if _, err := run(context.Background(), store, cfg, Options{Sources: builtin.NewBuiltins(), Now: func() time.Time { return at.Add(after) }}, syntheticSupport); err != nil {
			t.Fatal(err)
		}
		got, err := ReadRefusedStarts(store.Home())
		must(t, err)
		return got
	}
	if got := scan(cfg, 2*time.Minute); len(got) != 2 {
		t.Fatalf("recorded %+v", got)
	}
	must(t, store.RecordRemoval("codex", removed, state.RemovalReasonUndo, at.Add(3*time.Minute)))
	if got := scan(cfg, 4*time.Minute); len(got) != 1 || got[0].NativeSessionID != kept {
		t.Fatalf("after removal %+v", got)
	}
	excluded := cfg
	excluded.Archive.Projects = slices.Clone(cfg.Archive.Projects)
	excluded.Archive.Projects[0].Included = false
	must(t, config.ReconcileDiscovery(&excluded, cfg, at.Add(5*time.Minute)))
	if _, ok := excluded.DiscoveryStartFloor("codex"); !ok {
		t.Fatal("no start floor with the other project")
	}
	if got := scan(excluded, 6*time.Minute); len(got) != 0 {
		t.Fatalf("after exclusion %+v", got)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
