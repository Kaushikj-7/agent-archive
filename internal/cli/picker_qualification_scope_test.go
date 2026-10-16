package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
)

func qualificationDirectoryScope(t *testing.T, home, dir, repo string, projects int, reverse bool) sessionScope {
	t.Helper()
	cfg := config.Config{MachineID: "bench", Storage: credentialsTestConfig(), Archive: archive.Config{SchemaVersion: 1, MachineID: "bench", Enabled: true, Projects: []archive.ProjectActivation{{Root: dir, Included: true, ActivatedAt: time.Unix(0, 0)}}}}
	// Every activation contains the real directory. Count the path-derived
	// identities first, then add explicit IDs to obtain the exact boundary.
	base := len(archiveProjectIDs(cfg, dir))
	for i := range projects - base {
		cfg.Archive.Projects = append(cfg.Archive.Projects, archive.ProjectActivation{Root: dir, ProjectID: fmt.Sprintf("zz-boundary-project-%02d", i), Included: true, ActivatedAt: time.Unix(0, 0)})
	}
	// Repeated activation facts must not consume extra membership ranges.
	cfg.Archive.Projects = append(cfg.Archive.Projects, cfg.Archive.Projects[1])
	if reverse {
		slices.Reverse(cfg.Archive.Projects)
	}
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	scope, err := scopeFor(&scopeStub{home: home, dir: dir, keys: map[string]string{dir: repo}}, "", false)
	if err != nil || len(scope.ProjectIDs) != projects || scope.Dir != dir || scope.RepoKey != repo {
		t.Fatalf("actual configured directory scope: %+v error=%v", scope, err)
	}
	return scope
}

func TestPickerQualificationDirectoryRangeCeilingAndCompleteFallback(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "overlapping-project")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	all := qualificationDirectoryScope(t, t.TempDir(), dir, "", 33, false)
	metadata := qualificationMetadata(1400, 0)
	for i := range 1200 {
		metadata[i].ProjectID = all.ProjectIDs[i%len(all.ProjectIDs)]
	}
	// One newest eligible root per membership makes omission observable for
	// every scope size. All cases share this complete immutable universe.
	newest := map[string]string{}
	for i, project := range all.ProjectIDs {
		m := &metadata[i*20]
		m.ProjectID, m.RepoKey, m.Replay = project, "", nil
		m.CapturedAt = time.Date(2026, 1, 3, 12, 0, 0, 0, time.UTC).Add(time.Duration(i) * time.Second)
		turns := 1
		m.Counts.Turns = &turns
		newest[project] = m.Title
	}
	started := time.Now()
	fixture := newQualificationFixture(t, metadata)
	t.Logf("shared immutable1400 boundary seed setup: %s (excluded from phase metrics)", time.Since(started))
	for _, repo := range []string{"", qualificationRepo} {
		for _, ranges := range []int{32, 33} {
			projects := ranges
			if repo != "" {
				projects--
			}
			t.Run(fmt.Sprintf("repository=%t/ranges=%d", repo != "", ranges), func(t *testing.T) {
				scope := qualificationDirectoryScope(t, t.TempDir(), dir, repo, projects, false)
				for _, command := range []string{"list", "handoff"} {
					for _, reverse := range []bool{false, true} {
						t.Run(fmt.Sprintf("%s/reversed=%t", command, reverse), func(t *testing.T) {
							home := t.TempDir()
							result := qualificationRunScoped(t, fixture, home, command, "cold", 0, func(env *Env) sessionScope {
								env.WorkingDir = func() (string, error) { return dir, nil }
								env.repoKey = func(string) string { return repo }
								return qualificationDirectoryScope(t, home, dir, repo, projects, reverse)
							})
							if !strings.Contains(result.First.Frame, newest[scope.ProjectIDs[len(scope.ProjectIDs)-1]]) {
								t.Fatal("last scoped identity was dropped from actual CLI output")
							}
							if ranges == 32 {
								qualificationBudget(t, result.First.Reads, fixture.height, 0, ranges, false)
								qualificationBudget(t, result.Older.Reads, fixture.height, 0, ranges, true)
								if result.First.Reads.NonProjectRoots != 0 || result.Older.Reads.NonProjectRoots != 0 {
									t.Fatal("supported scope initialized a complete non-Project tree", result)
								}
							} else if result.First.Reads.NonProjectRoots == 0 {
								t.Fatal("33-range scope lacked complete compatibility initialization", result)
							}
							// A dropped last ProjectID must fail this same independent
							// frame oracle, rather than merely change a cost counter.
							incomplete := scope
							incomplete.ProjectIDs = incomplete.ProjectIDs[:len(incomplete.ProjectIDs)-1]
							wrong := qualificationScopeOracle(metadata, command, incomplete)
							if len(wrong) < 50 || qualificationFrameError(result.First.Frame, wrong[:50], 1) == nil {
								t.Fatal("range omission negative control did not reject the frame")
							}
						})
					}
				}
			})
		}
	}
}
