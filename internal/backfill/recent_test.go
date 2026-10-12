package backfill

import (
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
)

// Recent keeps the sessions of included projects that start on or after the
// day, counts the older ones there, and leaves out every session whose
// import would add a project.
func TestPlanRecentKeepsIncludedProjectsSinceTheDay(t *testing.T) {
	t.Parallel()
	loc := time.FixedZone("local", -7*3600)
	now := time.Date(2026, 10, 8, 9, 0, 0, 0, loc)
	cfg := config.Config{Archive: archive.Config{Projects: []archive.ProjectActivation{
		{Root: "/src/app", Included: true},
		{Root: "/src/old", Included: false},
	}}}
	candidate := func(id, root string, included bool, age time.Duration) Candidate {
		return Candidate{Harness: "claude", NativeSessionID: id, ProjectRoot: root, ProjectIncluded: included, ProjectKind: ProjectKindRepository, StartedAt: now.Add(-age)}
	}
	day := 24 * time.Hour
	plan := Plan{GeneratedAt: now, Candidates: []Candidate{
		candidate("two-days", "/src/app", true, 2*day),
		candidate("six-days", "/src/app", true, 6*day),
		// The first moment of the day --since names is in.
		{Harness: "codex", NativeSessionID: "day-start", ProjectRoot: "/src/app", ProjectIncluded: true, StartedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, loc)},
		{Harness: "codex", NativeSessionID: "day-before", ProjectRoot: "/src/app", ProjectIncluded: true, StartedAt: time.Date(2026, 9, 30, 23, 59, 0, 0, loc)},
		candidate("nine-days", "/src/app", true, 9*day),
		// Importing these would add a project.
		candidate("new-folder", "/tmp/scratch", false, day),
		candidate("excluded", "/src/old", true, day),
		func() Candidate {
			c := candidate("archived", "/src/app", true, day)
			c.Skip = SkipAlreadyArchived
			return c
		}(),
	}}
	recent, older, err := plan.Recent(cfg, "2026-10-01", "7d")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range recent.Imported() {
		got = append(got, c.NativeSessionID)
	}
	if want := "two-days,six-days,day-start"; strings.Join(got, ",") != want {
		t.Fatalf("imported %s, want %s", strings.Join(got, ","), want)
	}
	if older != 2 {
		t.Fatalf("older = %d, want 2 (day-before, nine-days)", older)
	}
	if f := recent.BatchFilters(); f.Since != "2026-10-01" || f.SinceArg != "7d" {
		t.Fatalf("batch filters %+v", f)
	}
	changes, err := ApplyToConfig(&cfg, recent, now)
	if err != nil || len(changes.ProjectIDs) != 0 || len(changes.KeptOut) != 0 || len(changes.Apps) != 0 {
		t.Fatalf("the import changes the configuration: %+v %v", changes, err)
	}
	if len(plan.Imported()) != 7 {
		t.Fatalf("the full plan changed: %d imported", len(plan.Imported()))
	}
	if _, _, err := recent.Recent(cfg, "2026-10-01", "7d"); err == nil {
		t.Fatal("a dated plan was narrowed again")
	}
}
