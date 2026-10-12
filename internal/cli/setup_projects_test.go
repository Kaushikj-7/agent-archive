package cli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/backfill"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
	"github.com/wangjohn/agent-archive/internal/testutil/golden"
)

func selectorRoots(t *testing.T, n int) []backfill.KnownProject {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	must(t, err)
	var roots []backfill.KnownProject
	for i := range n {
		root := filepath.Join(base, fmt.Sprintf("project-%02d", i+1))
		must(t, os.MkdirAll(root, 0700))
		roots = append(roots, backfill.KnownProject{Root: root, Sessions: i + 1})
	}
	return roots
}

func TestSetupAllIncludesEveryPageAndCurrentFolder(t *testing.T) {
	t.Parallel()
	roots := selectorRoots(t, 25)
	var out bytes.Buffer
	p := newPrompter(strings.NewReader("next\n\n"), &out)
	defer p.close()
	got, err := selectSetupProjects(p, nil, nil, roots, roots[24].Root, "", nil)
	must(t, err)
	if len(got) != 25 || got[0].Root != roots[24].Root || !strings.Contains(out.String(), "Showing 13–24 of 25") || !strings.Contains(out.String(), "All 25 found projects (default)") {
		t.Fatalf("selection %+v\n%s", got, &out)
	}
}

func TestSetupSpecificRestoresRulesAndDeduplicatesToggles(t *testing.T) {
	t.Parallel()
	roots := selectorRoots(t, 3)
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	old := []archive.ProjectActivation{{Root: roots[0].Root, ProjectID: archive.ProjectID(roots[0].Root), Included: true, ActivatedAt: at}, {Root: roots[1].Root, ProjectID: archive.ProjectID(roots[1].Root), Included: false, ActivatedAt: at}}
	var out bytes.Buffer
	p := newPrompter(strings.NewReader("specific\n2-3 2 3\n\n"), &out)
	defer p.close()
	got, err := selectSetupProjects(p, old, old, roots, "", "", nil)
	must(t, err)
	if len(got) != 3 || !got[1].Included || got[1].ActivatedAt != at {
		t.Fatalf("selection %+v", got)
	}
	if !strings.Contains(out.String(), "[✓]") || !strings.Contains(out.String(), "[ ]") {
		t.Fatal(out.Bytes())
	}
}

func TestSetupSpecificFreshStartsAllChecked(t *testing.T) {
	t.Parallel()
	roots := selectorRoots(t, 3)
	var out bytes.Buffer
	p := newPrompter(strings.NewReader("specific\n2\n\n"), &out)
	defer p.close()
	got, err := selectSetupProjects(p, nil, nil, roots, "", "", nil)
	must(t, err)
	if len(got) != 2 || got[0].Root != roots[0].Root || got[1].Root != roots[2].Root {
		t.Fatalf("selection %+v", got)
	}
}

func TestSetupSpecificPreservesImportedAndNestedExclusions(t *testing.T) {
	t.Parallel()
	roots := selectorRoots(t, 1)
	nested := filepath.Join(roots[0].Root, "private")
	must(t, os.MkdirAll(nested, 0700))
	old := []archive.ProjectActivation{{Root: roots[0].Root, ProjectID: archive.ProjectID(roots[0].Root), Included: true}, {Root: nested, ProjectID: archive.ProjectID(nested), Included: false}}
	var out bytes.Buffer
	p := newPrompter(strings.NewReader("specific\np\n"+selectorRoots(t, 1)[0].Root+"\n1\n\n"), &out)
	defer p.close()
	got, err := selectSetupProjects(p, old, old, roots, "", "", map[string]bool{old[0].ProjectID: true})
	must(t, err)
	if got[0].Included || got[1].Included || len(got) != 3 {
		t.Fatalf("rules %+v", got)
	}
}

func TestSetupMissingSelectedPathRequiresExplicitRemoval(t *testing.T) {
	t.Parallel()
	roots := selectorRoots(t, 2)
	must(t, os.Remove(roots[0].Root))
	old := []archive.ProjectActivation{{Root: roots[0].Root, Included: true}, {Root: roots[1].Root, Included: true}}
	var out bytes.Buffer
	p := newPrompter(strings.NewReader("\nspecific\n1\n\n"), &out)
	defer p.close()
	got, err := selectSetupProjects(p, old, old, roots, "", "", nil)
	must(t, err)
	if len(got) != 1 || got[0].Root != roots[1].Root || !got[0].Included || !strings.Contains(out.String(), "does not exist") {
		t.Fatalf("rules %+v\n%s", got, &out)
	}
}

func TestSetupCandidateFoldingPreservesExplicitNestedRules(t *testing.T) {
	t.Parallel()
	root := selectorRoots(t, 1)[0].Root
	child := filepath.Join(root, "child")
	must(t, os.Mkdir(child, 0700))
	known := []backfill.KnownProject{{Root: root, Sessions: 2}, {Root: child, Sessions: 3}}
	got := setupProjectCandidates(nil, nil, known, root)
	if len(got) != 1 || got[0].evidence.Sessions != 5 {
		t.Fatalf("candidates %+v", got)
	}
	rules := []archive.ProjectActivation{{Root: child, Included: false}}
	got = setupProjectCandidates(rules, rules, known, root)
	if len(got) != 2 || got[1].selected || got[0].evidence.Sessions != 2 {
		t.Fatalf("candidates %+v", got)
	}
}

func TestSetupProjectSearchCacheIncludesAppsAndPreservesEvidence(t *testing.T) {
	t.Parallel()
	home, userHome := t.TempDir(), t.TempDir()
	root := gitRepo(t)
	now := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	writeClaudeSession(t, userHome, "one", root, now)
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), now)
	env.BackfillTempDirs = []string{}
	search := &setupProjectSearch{env: env, home: userHome}
	cfg := config.Config{Harnesses: []string{"claude"}}
	first := search.projects(cfg)
	if len(first) != 1 || first[0].Sessions != 1 || !first[0].LastUsed.Equal(now) {
		t.Fatalf("evidence %+v", first)
	}
	writeClaudeSession(t, userHome, "two", root, now)
	if got := search.projects(cfg); got[0].Sessions != 1 {
		t.Fatal("cached rendering rescanned")
	}
	search.scanned = false
	if got := search.projects(cfg); got[0].Sessions != 2 {
		t.Fatalf("retry %+v", got)
	}
	cfg.Harnesses = []string{"codex"}
	if got := search.projects(cfg); len(got) != 0 {
		t.Fatalf("apps did not invalidate %+v", got)
	}
}

func TestSetupBlockedReviewRejectsStartAliases(t *testing.T) {
	t.Parallel()
	for _, answer := range []string{"yes", "start", "y"} {
		t.Run(answer, func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			p := newPrompter(strings.NewReader(answer+"\ncheck\n"), &out)
			defer p.close()
			got, err := reviewAction(p, false, true, false)
			must(t, err)
			if got != "check" || !strings.Contains(out.String(), "Enter one of the available choices") {
				t.Fatalf("action %s\n%s", got, &out)
			}
		})
	}
}

func TestSetupReviewPresentationMatrix(t *testing.T) {
	t.Parallel()
	for _, size := range [][2]int{{80, 24}, {100, 30}, {60, 20}, {36, 20}} {
		for _, color := range []bool{false, true} {
			t.Run(fmt.Sprintf("%dx%d-color-%t", size[0], size[1], color), func(t *testing.T) {
				t.Parallel()
				out := &promptScreen{caps: promptCapabilities{Color: color, Width: size[0], Height: size[1]}}
				p := newPrompter(strings.NewReader("d\nq\n"), out)
				defer p.close()
				p.style.color = color
				f := newScreenFixture(t)
				cfg := config.Config{Harnesses: []string{"claude", "cursor"}, RetentionDays: 90, SkillEvidence: config.SkillEvidenceMetadata, Storage: credentials.Config{Provider: credentials.ProviderS3, Bucket: "team-archive", Region: "us-east-1", Prefix: "agent-archive"}, Archive: archive.Config{Projects: []archive.ProjectActivation{{Root: filepath.Join(f.userHome, "src", "長い-project-path"), Included: true}}}}
				cfg.BucketPrivacy = inspectBucketPrivacy(cfg, &privateTestStore{MemoryStore: storagetest.NewMemoryStore()}, screenNow)
				model := buildSetupReviewModel(cfg, setupReview{userHome: f.userHome, hookFiles: f.env.hookFiles(f.userHome)}, screenNow)
				renderSetupReview(p, model, false)
				choice, err := reviewAction(p, false, false, true)
				must(t, err)
				if choice != "details" {
					t.Fatal(choice)
				}
				renderSetupReview(p, model, true)
				renderSetupReview(p, model, false)
				_, err = reviewAction(p, false, false, true)
				must(t, err)
				golden.Check(t, filepath.Join("testdata", "setup-review", fmt.Sprintf("%dx%d-color-%t.txt", size[0], size[1], color)), []byte(strings.ReplaceAll(f.normalize(out.String()), "\x1b", "\\e")))
			})
		}
	}
}

func TestSetupCompleteTranscriptMatrix(t *testing.T) {
	t.Parallel()
	for _, size := range [][2]int{{80, 24}, {100, 30}, {60, 20}, {36, 20}} {
		for _, color := range []bool{false, true} {
			t.Run(fmt.Sprintf("%dx%d-color-%t", size[0], size[1], color), func(t *testing.T) {
				t.Parallel()
				f := newScreenFixture(t)
				f.withApps(t, "codex", "claude", "cursor")
				f.inWebApp(t)
				f.pastSession(t, "one", "src/web-app", screenNow.Add(-2*time.Hour))
				f.pastSession(t, "two", "src/長い-project-with-a-long-name", screenNow.Add(-time.Hour))
				out := &promptScreen{caps: promptCapabilities{Color: color, Width: size[0], Height: size[1]}}
				input := strings.NewReader("\nincluded-projects\n\ns3-existing\nwork\n2\ndetails\nstart\ndone\n")
				if code := Run([]string{"setup"}, input, out, out, f.env); code != 0 {
					t.Fatalf("exit %d\n%s", code, out.String())
				}
				cfg, found, err := config.Load(f.home)
				must(t, err)
				if !found || !cfg.Archive.Enabled || includedProjects(cfg.Archive.Projects) != 2 {
					t.Fatalf("config %+v", cfg)
				}
				text := f.normalize(out.String())
				if !strings.Contains(text, "Setup complete") || !setupContainsText(text, "Imported 2 sessions from the last 7 days (Claude Code 2). Uploading in the background.") || strings.Contains(text, "To set up another machine with this storage") {
					t.Fatal(text)
				}
				golden.Check(t, filepath.Join("testdata", "setup-transcripts", fmt.Sprintf("%dx%d-color-%t.txt", size[0], size[1], color)), []byte(strings.ReplaceAll(text, "\x1b", "\\e")))
			})
		}
	}
}

// setupContainsText keeps behavioral assertions independent of terminal wrapping.
// Complete layout is covered by the width-specific transcript matrix.
func setupContainsText(text, want string) bool {
	return strings.Contains(strings.Join(strings.Fields(text), " "), strings.Join(strings.Fields(want), " "))
}

func TestSetupIncompleteDiscoveryDoesNotPromiseEmptyOrCompleteTotals(t *testing.T) {
	t.Parallel()
	for _, empty := range []bool{true, false} {
		t.Run(strconv.FormatBool(empty), func(t *testing.T) {
			t.Parallel()
			roots := selectorRoots(t, 1)
			input := "\n"
			known := roots
			if empty {
				known = nil
				input = "\n" + roots[0].Root + "\n\n"
			}
			var out bytes.Buffer
			p := newPrompter(strings.NewReader(input), &out)
			defer p.close()
			p.projectScan = &setupProjectSearch{result: backfill.KnownProjectsResult{Unreadable: 1}}
			got, err := selectSetupProjects(p, nil, nil, known, "", "", nil)
			must(t, err)
			if includedProjects(got) != 1 || strings.Contains(out.String(), "All 0") || !strings.Contains(out.String(), "unreadable sources") {
				t.Fatalf("coverage omitted\n%s", &out)
			}
			if !empty && !strings.Contains(out.String(), "at least 1 session") {
				t.Fatalf("partial count promised a total\n%s", &out)
			}
		})
	}
}

func TestSetupManualProjectRequiresExplicitPath(t *testing.T) {
	t.Parallel()
	roots := selectorRoots(t, 1)
	var out bytes.Buffer
	p := newPrompter(strings.NewReader("\n"+roots[0].Root+"\n\n"), &out)
	defer p.close()
	got, err := selectSetupProjects(p, nil, nil, nil, "", "", nil)
	must(t, err)
	if len(got) != 1 || got[0].Root != roots[0].Root || !strings.Contains(out.String(), "enter a project path") || strings.Contains(out.String(), "OK Project \n") {
		t.Fatalf("blank input granted project consent: %+v\n%s", got, &out)
	}
}

func TestSetupSpecificRespectsInheritedNestedOwnership(t *testing.T) {
	t.Parallel()
	parent := selectorRoots(t, 1)[0].Root
	child := filepath.Join(parent, "child")
	must(t, os.Mkdir(child, 0700))
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	old := []archive.ProjectActivation{{Root: parent, ProjectID: archive.ProjectID(parent), Included: true, ActivatedAt: at}}
	candidates := setupProjectCandidates(old, old, nil, child)
	if len(candidates) != 2 || !candidates[0].selected {
		t.Fatalf("inherited inclusion not restored: %+v", candidates)
	}
	same := applyProjectCandidates(candidates, old, nil)
	if len(same) != 1 || same[0].ActivatedAt != at {
		t.Fatalf("confirmation reset inherited activation: %+v", same)
	}
	candidates[0].selected = false
	excluded := applyProjectCandidates(candidates, old, nil)
	if len(excluded) != 2 || excluded[1].Root != child || excluded[1].Included || excluded[0].ActivatedAt != at {
		t.Fatalf("unchecked child still inherits parent: %+v", excluded)
	}
}

func TestSetupSpecificRetainsImportedExclusionForNewCandidate(t *testing.T) {
	t.Parallel()
	roots := selectorRoots(t, 2)
	candidates := []setupProjectCandidate{{evidence: roots[0], selected: true}, {evidence: roots[1]}}
	got := applyProjectCandidates(candidates, nil, map[string]bool{archive.ProjectID(roots[1].Root): true})
	if len(got) != 2 || got[1].Included || got[1].Root != roots[1].Root {
		t.Fatalf("imported exclusion lost: %+v", got)
	}
}

// Explicitly opening Projects after resuming or changing another setting must
// offer the current repository even if it has no saved rule or session history.
// Regression: 2026-10 setup review P2-R1-01.
func TestSetupReviewProjectsIncludesCurrentFolderAfterSkippedCaptureStep(t *testing.T) {
	t.Parallel()
	for _, entry := range []string{"storage", "retention", "continue"} {
		t.Run(entry, func(t *testing.T) {
			t.Parallel()
			f := newScreenFixture(t)
			f.installed(t)
			current := f.project(t, "src/new-project")
			f.env.WorkingDir = func() (string, error) { return current, nil }
			input := "storage\n\n"
			if entry == "retention" {
				input = "retention\n90-days\n"
			}
			if entry == "continue" {
				cfg := mustLoadConfig(t, f.home)
				must(t, local.Write(draftPath(f.home), setupDraft{Version: draftFormat, Config: cfg, Step: 2}))
				input = "continue\n"
			}
			out := setupRun(t, f.env, input+"edit\nprojects\n\nq\n", 0)
			draft, found, _, err := readDraft(f.home)
			must(t, err)
			if !found || includedProjects(draft.Config.Archive.Projects) != 2 || !setupContainsText(out, "1) ~/src/new-project · this folder") {
				t.Fatalf("current folder omitted after %s: %+v\n%s", entry, draft.Config.Archive.Projects, out)
			}
			saved := mustLoadConfig(t, f.home)
			if includedProjects(saved.Archive.Projects) != 1 {
				t.Fatal("cancelled review saved project changes")
			}
		})
	}
}

// setupReceiptIndex checks resolved receipts with either supported terminal glyph.
func setupReceiptIndex(text, answer string) int {
	for _, mark := range []string{"OK ", "✓ "} {
		if i := strings.Index(text, mark+answer+"\n"); i >= 0 {
			return i
		}
	}
	return -1
}

// Retrying discovery keeps manually added candidates, including unchecked ones,
// so stable project numbers can be used to revise the selection afterward.
func TestSetupRetryPreservesUncheckedManualCandidate(t *testing.T) {
	t.Parallel()
	roots := selectorRoots(t, 2)
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	existing := []archive.ProjectActivation{{Root: roots[0].Root, ProjectID: archive.ProjectID(roots[0].Root), Included: true, ActivatedAt: at}}
	userHome := t.TempDir()
	env := setupTestEnv(t, t.TempDir(), userHome, newFakeKeychain(), time.Now())
	var out bytes.Buffer
	p := newPrompter(strings.NewReader("specific\np\n"+roots[1].Root+"\n2\nr\n2\n\n"), &out)
	defer p.close()
	p.projectConfig = config.Config{Harnesses: []string{"claude"}, Archive: archive.Config{Projects: existing}}
	p.projectScan = &setupProjectSearch{env: env, home: userHome}
	got, err := selectSetupProjects(p, existing, existing, nil, "", userHome, nil)
	must(t, err)
	if includedProjects(got) != 2 || got[0].ActivatedAt != at || strings.Contains(out.String(), "enter project numbers from 1 to 1") {
		t.Fatalf("retry lost manual candidate or activation: %+v\n%s", got, &out)
	}
}
