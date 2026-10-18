package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/backfill"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/gitremote"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

type recoveryReviewFixture struct {
	env      Env
	home     string
	userHome string
	target   string
	missing  string
	cfg      config.Config
}

type recoveryScenario string

const (
	reviewAmbiguousClones   recoveryScenario = "ambiguous clones"
	reviewUnresolvedChain   recoveryScenario = "unresolved chain"
	reviewConflictingKey    recoveryScenario = "conflicting key"
	reviewExcludedTarget    recoveryScenario = "excluded target"
	reviewUnavailableTarget recoveryScenario = "unavailable target"
	reviewExcludedOriginal  recoveryScenario = "excluded original"
	reviewExcludedAncestor  recoveryScenario = "excluded ancestor"
	reviewParentTraversal   recoveryScenario = "noncanonical excluded traversal"
	reviewMissingSubtree    recoveryScenario = "missing subtree"
)

func newRecoveryReviewFixture(t *testing.T) *recoveryReviewFixture {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	must(t, err)
	f := &recoveryReviewFixture{home: filepath.Join(root, "archive"), userHome: filepath.Join(root, "home")}
	must(t, os.MkdirAll(f.home, 0700))
	must(t, os.MkdirAll(f.userHome, 0700))
	t.Setenv("HOME", f.userHome)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "xdg"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	f.target = f.repository(t, "a-target", "https://example.test/acme/repo")
	f.missing = filepath.Join(f.userHome, ".codex", "worktrees", "deleted", "repo")
	f.cfg = config.Config{MachineID: "synthetic-machine", Storage: credentialsTestConfig(), RetentionDays: 90, Harnesses: []string{"claude", "codex"}, Archive: archive.Config{Enabled: true, Projects: []archive.ProjectActivation{{Root: f.target, Included: true, ProjectID: archive.ProjectID(f.target)}}}}
	f.env = setupTestEnv(t, f.home, f.userHome, newFakeKeychain(), time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC))
	f.env.BackfillTempDirs = []string{}
	f.env.IsTerminal = func(any) bool { return true }
	bucket := storagetest.NewMemoryStore()
	f.env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return bucket, nil }
	f.save(t)
	return f
}

func (f *recoveryReviewFixture) repository(t *testing.T, name, origin string) string {
	t.Helper()
	root := filepath.Join(f.userHome, name)
	must(t, os.MkdirAll(root, 0700))
	f.git(t, root, "init", "--quiet")
	f.git(t, root, "config", "remote.origin.url", origin)
	return root
}

func (f *recoveryReviewFixture) git(t *testing.T, root string, args ...string) {
	t.Helper()
	if out, err := gitremote.ExecRunner(t.Context(), root, append([]string{"-C", root}, args...)...); err != nil {
		t.Fatalf("synthetic Git %v: %s: %v", args, out, err)
	}
}

func (f *recoveryReviewFixture) save(t *testing.T) {
	t.Helper()
	must(t, config.Save(f.home, f.cfg))
}

func (f *recoveryReviewFixture) codex(t *testing.T, id, cwd, extra string) {
	t.Helper()
	source := filepath.Join(f.userHome, ".codex", "sessions", "rollout-"+id+".jsonl")
	must(t, os.MkdirAll(filepath.Dir(source), 0700))
	body := fmt.Sprintf(`{"type":"session_meta","payload":{"id":%q,"timestamp":"2026-10-13T12:00:00Z","cwd":%q,"source":"cli","originator":"codex_cli_rs","cli_version":"0.160.0","git":{"repository_url":"https://example.test/acme/repo"}%s}}`+"\n"+`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Private synthetic prompt must never appear in review"}]}}`+"\n", id, cwd, extra)
	must(t, os.WriteFile(source, []byte(body), 0600))
}

func (f *recoveryReviewFixture) command(t *testing.T, input string, args ...string) (string, string, int) {
	t.Helper()
	var out, errout bytes.Buffer
	code := Run(args, strings.NewReader(input), &out, &errout, f.env)
	return out.String(), errout.String(), code
}

func TestRecoveryReviewExportsExactConfiguredAssignmentsAndResumesCanonicalFile(t *testing.T) {
	f := newRecoveryReviewFixture(t)
	const first = "11111111-1111-4111-8111-111111111111"
	const second = "22222222-2222-4222-8222-222222222222"
	f.codex(t, first, f.missing, "")
	f.codex(t, second, f.missing+"-second", "")
	output := filepath.Join(t.TempDir(), "reviewed.json")
	before := snapshotTree(t, f.home)
	out, errout, code := f.command(t, "1\n1\nexport\n", "backfill", "review-projects", "--output", output, "--harness", "codex")
	if code != 0 || !strings.Contains(out, "bounded, incomplete coverage") || !strings.Contains(out, "Candidates: 1") || !strings.Contains(out, "compatible exact assignment") {
		t.Fatal(code, out, errout)
	}
	if strings.Contains(out, "Private synthetic prompt") || strings.Contains(out, first) || strings.Contains(out, second) || strings.Contains(out, "rollout-") {
		t.Fatal("review leaked transcript content or locators", out)
	}
	if snapshotTree(t, f.home) != before {
		t.Fatal("review changed configuration, stages or archive")
	}
	info, err := os.Stat(output)
	must(t, err)
	if info.Mode().Perm() != 0600 {
		t.Fatal("export is not private", info.Mode())
	}
	data, err := os.ReadFile(output)
	must(t, err)
	var assignments backfill.Filters
	must(t, backfill.ReadMappingFile(bytes.NewReader(data), &assignments))
	if len(assignments.ProjectMappings) != 2 || assignments.ProjectMappings[f.missing] != f.target || assignments.ProjectMappings[f.missing+"-second"] != f.target || len(assignments.HistoricalMappings) != 0 {
		t.Fatal("export widened or lost exact original cwds", assignments)
	}
	out, errout, code = f.command(t, "", "backfill", "--dry-run", "--json", "--harness", "codex")
	if code != 0 || strings.Contains(out, f.missing) || strings.Contains(out, "recorded_repository_key") {
		t.Fatal("ordinary preview exposed private review", code, out, errout)
	}
	interrupted := false
	f.env.backfillCheckpoint = func(step string) error {
		if step == "batch saved" {
			interrupted = true
			return errors.New("synthetic review import interruption")
		}
		return nil
	}
	_, errout, code = f.command(t, "", "backfill", "--yes", "--background", "--harness", "codex", "--mapping-file", output)
	if code != 1 || !interrupted {
		t.Fatal("interruption did not reach confirmed assignments", code, errout)
	}
	var reordered struct {
		SchemaVersion      int               `json:"schema_version"`
		HistoricalProjects []json.RawMessage `json:"historical_projects"`
		Mappings           []json.RawMessage `json:"mappings"`
	}
	must(t, json.Unmarshal(data, &reordered))
	slices.Reverse(reordered.Mappings)
	changed, err := json.Marshal(reordered)
	must(t, err)
	restartFile := filepath.Join(t.TempDir(), "same-assignments-other-name.json")
	must(t, os.WriteFile(restartFile, changed, 0600))
	f.env.backfillCheckpoint = nil
	out, errout, code = f.command(t, "", "backfill", "--yes", "--background", "--harness", "codex", "--mapping-file", restartFile)
	if code != 0 {
		t.Fatal("canonical restart failed", code, out, errout)
	}
	store, err := state.Open(f.home)
	must(t, err)
	regs, err := store.LoadRegistrations()
	must(t, err)
	batches, err := backfill.LoadBatches(f.home)
	must(t, err)
	if len(regs) != 2 || len(batches) != 1 {
		t.Fatal("canonical mapping did not resume exact batch", regs, batches)
	}
	for _, reg := range regs {
		if reg.ProjectResolution == nil || reg.ProjectResolution.Method != "explicit_mapping" || reg.ProjectRoot != f.target || !slices.Contains([]string{f.missing, f.missing + "-second"}, reg.ProjectResolution.OriginalCwd) {
			t.Fatal("accepted assignment lost exact proof", reg)
		}
	}
	out, errout, code = f.command(t, "", "backfill", "undo", batches[0].ID, "--yes")
	if code != 0 {
		t.Fatal("undo failed", code, out, errout)
	}
	regs, err = store.LoadRegistrations()
	must(t, err)
	if len(regs) != 0 {
		t.Fatal("undo retained reviewed owners", regs)
	}
}

func TestRecoveryReviewReportsCloneConflictChainAndExclusions(t *testing.T) {
	for _, scenario := range []recoveryScenario{reviewAmbiguousClones, reviewUnresolvedChain, reviewConflictingKey, reviewExcludedTarget, reviewUnavailableTarget, reviewExcludedOriginal, reviewExcludedAncestor, reviewParentTraversal, reviewMissingSubtree} {
		t.Run(string(scenario), func(t *testing.T) {
			f := newRecoveryReviewFixture(t)
			want := ""
			switch scenario {
			case reviewAmbiguousClones:
				clone := f.repository(t, "b-clone", f.target)
				f.cfg.Archive.Projects = append(f.cfg.Archive.Projects, archive.ProjectActivation{Root: clone, ProjectID: archive.ProjectID(clone), Included: true})
				want = "project_ambiguous"
			case reviewUnresolvedChain:
				f.git(t, f.target, "config", "remote.origin.url", filepath.Join(f.userHome, "unavailable-chain"))
				want = "project_origin_missing"
			case reviewConflictingKey:
				f.git(t, f.target, "config", "remote.origin.url", "https://example.test/acme/other")
				want = "Rejected configured target: " + f.target + " (project_mapping_conflict)"
			case reviewExcludedTarget:
				f.cfg.Archive.Projects[0].Included = false
				want = "Rejected configured target: " + f.target + " (project_mapping_conflict)"
			case reviewUnavailableTarget:
				must(t, os.RemoveAll(f.target))
				want = "Rejected configured target: " + f.target + " (project_mapping_conflict)"
			case reviewExcludedOriginal, reviewExcludedAncestor:
				root := f.missing
				if scenario == reviewExcludedAncestor {
					root = filepath.Dir(root)
				}
				f.cfg.Archive.Projects = append(f.cfg.Archive.Projects, archive.ProjectActivation{Root: root, ProjectID: archive.ProjectID(root), Included: false})
				want = "Assignment unavailable: source restriction excluded_project"
			case reviewParentTraversal:
				private := filepath.Join(f.userHome, "private")
				must(t, os.MkdirAll(filepath.Join(private, "child"), 0700))
				alias := filepath.Join(f.userHome, "alias")
				must(t, os.Symlink(filepath.Join(private, "child"), alias))
				f.missing = alias + "/../vanished"
				f.cfg.Archive.Projects = append(f.cfg.Archive.Projects, archive.ProjectActivation{Root: private, ProjectID: archive.ProjectID(private), Included: false})
				want = "Original cwd contains parent traversal and requires separate manual review"
			case reviewMissingSubtree:
				f.cfg.Archive.Projects[0].Root = filepath.Join(f.target, "subtree")
				f.cfg.Archive.Projects[0].ProjectID = archive.ProjectID(f.cfg.Archive.Projects[0].Root)
				must(t, os.MkdirAll(f.cfg.Archive.Projects[0].Root, 0700))
				want = "project_subtree_unavailable"
			}
			f.save(t)
			f.codex(t, "11111111-1111-4111-8111-111111111111", f.missing, "")
			before := snapshotTree(t, f.home)
			output := filepath.Join(t.TempDir(), "declined.json")
			out, errout, code := f.command(t, "skip\n", "backfill", "review-projects", "--harness", "codex", "--output", output)
			if code != 0 || !strings.Contains(out, want) {
				t.Fatal(scenario, code, out, errout)
			}
			if scenario == reviewAmbiguousClones && strings.Count(out, "compatible exact assignment") != 2 {
				t.Fatal("distinct compatible clones were hidden or auto-selected", out)
			}
			if scenario == reviewUnresolvedChain && strings.Contains(out, "compatible exact assignment") {
				t.Fatal("unresolved chain was silently relaxed", out)
			}
			if _, err := os.Stat(output); !os.IsNotExist(err) || snapshotTree(t, f.home) != before {
				t.Fatal("declined/excluded review mutated state", err)
			}
		})
	}
}

func TestRecoveryReviewCancellationPrivacyAndNoOverwrite(t *testing.T) {
	f := newRecoveryReviewFixture(t)
	f.codex(t, "11111111-1111-4111-8111-111111111111", f.missing, "")
	for _, tc := range []struct {
		name     string
		input    string
		args     []string
		terminal bool
		wantCode int
	}{
		{"EOF at selection", "", nil, true, 1},
		{"EOF at export", "1\n", nil, true, 1},
		{"cancel", "cancel\n", nil, true, 1},
		{"rejected", "1\nno\n", nil, true, 0},
		{"piped", "1\nexport\n", nil, false, 1},
		{"JSON", "", []string{"--json"}, true, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f.env.IsTerminal = func(any) bool { return tc.terminal }
			output := filepath.Join(t.TempDir(), "review.json")
			before := snapshotTree(t, f.home)
			args := append([]string{"backfill", "review-projects", "--harness", "codex", "--output", output}, tc.args...)
			out, errout, code := f.command(t, tc.input, args...)
			if code != tc.wantCode {
				t.Fatal(code, out, errout)
			}
			if _, err := os.Stat(output); !os.IsNotExist(err) || snapshotTree(t, f.home) != before {
				t.Fatal("cancelled review wrote state", err)
			}
			if !tc.terminal && (strings.Contains(out, f.missing) || strings.Contains(out, f.target)) {
				t.Fatal("piped output leaked private paths", out)
			}
		})
	}
	f.env.IsTerminal = func(any) bool { return true }
	privateOutput := filepath.Join(t.TempDir(), "redirected.json")
	f.env.IsTerminal = func(stream any) bool { _, terminal := stream.(*strings.Reader); return terminal }
	out, errout, code := f.command(t, "1\nexport\n", "backfill", "review-projects", "--output", privateOutput)
	if code != 1 || strings.Contains(out, f.missing) || !strings.Contains(errout, "terminal input and output") {
		t.Fatal("redirected output exposed private review", code, out, errout)
	}
	f.env.IsTerminal = func(any) bool { return true }
	output := filepath.Join(t.TempDir(), "existing.json")
	original := []byte("operator-owned assignments")
	must(t, os.WriteFile(output, original, 0640))
	_, errout, code = f.command(t, "1\nexport\n", "backfill", "review-projects", "--output", output)
	if code != 1 || !strings.Contains(errout, "output already exists") {
		t.Fatal(code, errout)
	}
	got, err := os.ReadFile(output)
	must(t, err)
	if !bytes.Equal(got, original) {
		t.Fatal("existing assignment file overwritten")
	}
	if err := writePrivateMappingExclusive(output, []byte("changed")); err == nil {
		t.Fatal("exclusive creation allowed a file created during review")
	}
	link := filepath.Join(t.TempDir(), "existing-link.json")
	must(t, os.Symlink(output, link))
	_, errout, code = f.command(t, "1\nexport\n", "backfill", "review-projects", "--output", link)
	if code != 1 || !strings.Contains(errout, "output already exists") {
		t.Fatal("existing symlink was accepted", code, errout)
	}
}

func TestRecoveryReviewHistoricalExportUnblocksSupportedSourcesOnly(t *testing.T) {
	f := newRecoveryReviewFixture(t)
	f.cfg.Archive.Projects = nil
	f.save(t)
	const ordinary = "11111111-1111-4111-8111-111111111111"
	const pending = "22222222-2222-4222-8222-222222222222"
	f.codex(t, ordinary, f.missing, "")
	f.codex(t, pending, f.missing+"-pending", `,"forked_from_id":"33333333-3333-4333-8333-333333333333","forked_from_ordinal_exclusive":3`)
	output := filepath.Join(t.TempDir(), "historical.json")
	before := snapshotTree(t, f.home)
	out, errout, code := f.command(t, "new\nRetired service\n1\nexport\n", "backfill", "review-projects", "--harness", "codex", "--output", output)
	if code != 0 || !strings.Contains(out, "Native history/own-task evidence unresolved or unchecked: 1") || !strings.Contains(out, "import only") {
		t.Fatal(code, out, errout)
	}
	if snapshotTree(t, f.home) != before {
		t.Fatal("export committed historical permissions")
	}
	data, err := os.ReadFile(output)
	must(t, err)
	var assignments backfill.Filters
	must(t, backfill.ReadMappingFile(bytes.NewReader(data), &assignments))
	if len(assignments.HistoricalProjects) != 1 || len(assignments.HistoricalMappings) != 2 {
		t.Fatal("historical exact assignments lost", assignments)
	}
	out, errout, code = f.command(t, "", "backfill", "--yes", "--background", "--harness", "codex", "--mapping-file", output)
	if code != 0 {
		t.Fatal(code, out, errout)
	}
	store, err := state.Open(f.home)
	must(t, err)
	regs, err := store.LoadRegistrations()
	must(t, err)
	if len(regs) != 1 || regs[0].NativeSessionID != ordinary || regs[0].HistoricalProject == nil || regs[0].ProjectRoot != "" {
		t.Fatal("project assignment bypassed remaining native refusal", regs, out)
	}
	cfg := mustLoadConfig(t, f.home)
	if len(cfg.Archive.Projects) != 0 || len(cfg.HistoricalProjects) != 1 {
		t.Fatal("historical assignment granted live capture", cfg)
	}
}

func TestRecoveryReviewReportsBoundedRepeatedCwdsAndDisabledHistoricalChoice(t *testing.T) {
	f := newRecoveryReviewFixture(t)
	const disabled = "11111111111111111111111111111111"
	f.cfg.Archive.Projects = nil
	f.cfg.HistoricalProjects = []config.HistoricalProject{{ID: disabled, Label: "Disabled retired service", Permissions: []config.HistoricalProjectPermission{{DestinationID: f.cfg.DestinationID(), Enabled: false}}}}
	f.save(t)
	for i := range 132 {
		id := fmt.Sprintf("synthetic-%03d", i)
		index := i
		if i == 131 {
			index = 0
		}
		cwd := filepath.Join(f.userHome, "gone", fmt.Sprintf("%03d", index))
		file := filepath.Join(f.userHome, ".claude", "projects", "synthetic", id+".jsonl")
		must(t, os.MkdirAll(filepath.Dir(file), 0700))
		must(t, os.WriteFile(file, []byte(fmt.Sprintf(`{"type":"user","sessionId":%q,"cwd":%q,"timestamp":"2026-10-13T12:00:00Z","message":{"role":"user","content":"Synthetic private prompt"}}`+"\n", id, cwd)), 0600))
	}
	output := filepath.Join(t.TempDir(), "omitted.json")
	before := snapshotTree(t, f.home)
	out, errout, code := f.command(t, strings.Repeat("skip\n", backfill.RecoveryReviewLimit), "backfill", "review-projects", "--harness", "claude", "--output", output)
	if code != 0 || !strings.Contains(out, "128 groups shown; 3 observed candidate records outside these groups") || !strings.Contains(out, "Candidates: 2") || !strings.Contains(out, "distinct omitted group count unknown") || !strings.Contains(out, "Rejected historical target: "+disabled) {
		t.Fatal("bounded review hid omitted/repeated counts or disabled policy", code, out, errout)
	}
	if strings.Contains(out, "Synthetic private prompt") || snapshotTree(t, f.home) != before {
		t.Fatal("bounded review leaked or wrote native content")
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatal("skipped bounded review exported assignments", err)
	}
}

func TestRecoveryReviewRefusesNoninteractiveModeBeforePrivatePlanning(t *testing.T) {
	for _, variables := range []map[string]string{{envNonInteractive: "1"}, {"CODEX_THREAD_ID": "synthetic-agent"}} {
		env := withEnvironment(Env{IsTerminal: func(any) bool { return true }, Home: func() (string, error) {
			t.Fatal("prompts-off review accessed archive state")
			return "", nil
		}}, variables)
		var out, errout bytes.Buffer
		input := strings.NewReader("new\nPrivate label\nexport\n")
		output := filepath.Join(t.TempDir(), "not-exported.json")
		if code := Run([]string{"backfill", "review-projects", "--output", output}, input, &out, &errout, env); code != 1 || !strings.Contains(errout.String(), "terminal input and output") || input.Len() == 0 {
			t.Fatal("prompts-off review reached private planning or export", code, out.String(), errout.String())
		}
		if _, err := os.Stat(output); !os.IsNotExist(err) {
			t.Fatal("prompts-off review created output", err)
		}
	}
}
