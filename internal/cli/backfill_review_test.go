package cli

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/backfill"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/gitremote"
	"github.com/wangjohn/agent-archive/internal/sourcefacts"
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

type recoveryReviewDestination string

const (
	reviewHistoricalDestination recoveryReviewDestination = "historical"
	reviewConfiguredDestination recoveryReviewDestination = "configured"
	reviewSkippedDestination    recoveryReviewDestination = "skip"
)

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
				want = "Rejected configured target: " + strconv.Quote(f.target) + " (project_mapping_conflict)"
			case reviewExcludedTarget:
				f.cfg.Archive.Projects[0].Included = false
				want = "Rejected configured target: " + strconv.Quote(f.target) + " (project_mapping_conflict)"
			case reviewUnavailableTarget:
				must(t, os.RemoveAll(f.target))
				want = "Rejected configured target: " + strconv.Quote(f.target) + " (project_mapping_conflict)"
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

func TestRecoveryReviewEscapesMetadataAndPreservesExactExport(t *testing.T) {
	f := newRecoveryReviewFixture(t)
	f.missing += "-\u202e-hidden\u200b"
	physical := f.repository(t, "physical-\x1b[2J-checkout", "https://example.test/acme/repo")
	must(t, os.RemoveAll(f.target))
	must(t, os.Symlink(physical, f.target))
	const id = "11111111111111111111111111111111"
	label := "Retired \u202e-service\u200b"
	f.cfg.HistoricalProjects = []config.HistoricalProject{{ID: id, Label: label}}
	f.save(t)
	f.codex(t, "11111111-1111-4111-8111-111111111111", f.missing, "")
	filename := "review-\u202e.json"
	output := filepath.Join(t.TempDir(), filename)
	before := snapshotTree(t, f.home)
	out, errout, code := f.command(t, "2\nexport\n", "backfill", "review-projects", "--harness", "codex", "--output", output)
	if code != 0 {
		t.Fatal(code, out, errout)
	}
	if strings.ContainsAny(out, "\x1b\u202e") || !strings.Contains(out, strconv.Quote(physical)) || !strings.Contains(out, strconv.Quote(label)) || !strings.Contains(out, strconv.Quote(output)) {
		t.Fatal("review metadata reached the terminal as active controls or ambiguous text", out)
	}
	data, err := os.ReadFile(output)
	must(t, err)
	var decoded backfill.Filters
	must(t, backfill.ReadMappingFile(bytes.NewReader(data), &decoded))
	if decoded.HistoricalMappings[f.missing] != id || len(decoded.HistoricalProjects) != 1 || decoded.HistoricalProjects[0].Label != label || snapshotTree(t, f.home) != before {
		t.Fatal("safe display changed the exact exported facts or archive state", decoded)
	}
}

func TestRecoveryReviewMakesOneDecisionForAllKeysAtExactCwd(t *testing.T) {
	for _, choice := range []recoveryReviewDestination{reviewHistoricalDestination, reviewConfiguredDestination, reviewSkippedDestination} {
		t.Run(string(choice), func(t *testing.T) {
			f := newRecoveryReviewFixture(t)
			const first = "11111111-1111-4111-8111-111111111111"
			const second = "22222222-2222-4222-8222-222222222222"
			f.codex(t, first, f.missing, "")
			f.codex(t, second, f.missing, "")
			secondPath := filepath.Join(f.userHome, ".codex", "sessions", "rollout-"+second+".jsonl")
			data, err := os.ReadFile(secondPath)
			must(t, err)
			must(t, os.WriteFile(secondPath, bytes.ReplaceAll(data, []byte("https://example.test/acme/repo"), []byte("https://example.test/other/repo")), 0600))
			input := ""
			switch choice {
			case reviewHistoricalDestination:
				input = "new\nRetired mixed identities\nexport\n"
			case reviewSkippedDestination:
				input = "skip\n"
			case reviewConfiguredDestination:
				input = "1\nexport\n"
			}
			output := filepath.Join(t.TempDir(), "one-cwd.json")
			out, errout, code := f.command(t, input, "backfill", "review-projects", "--harness", "codex", "--output", output)
			if code != 0 || strings.Count(out, "Choose an exact destination") != 1 || strings.Count(out, "Recorded repository key:") != 2 || !strings.Contains(out, "including unlisted identities") || !strings.Contains(out, "skip skips that whole cwd") || !strings.Contains(out, "compatible exact assignment for at least one observed group") || !strings.Contains(out, "project_mapping_conflict") {
				t.Fatal("cwd-only mapping was presented as key-selective authority", code, out, errout)
			}
			if choice == reviewSkippedDestination {
				if _, err := os.Stat(output); !os.IsNotExist(err) {
					t.Fatal("skipped cwd exported an assignment", err)
				}
				return
			}
			data, err = os.ReadFile(output)
			must(t, err)
			var decoded backfill.Filters
			must(t, backfill.ReadMappingFile(bytes.NewReader(data), &decoded))
			if len(decoded.HistoricalMappings)+len(decoded.ProjectMappings) != 1 || choice == reviewHistoricalDestination && decoded.HistoricalMappings[f.missing] == "" || choice == reviewConfiguredDestination && decoded.ProjectMappings[f.missing] != f.target {
				t.Fatal("review failed to export one exact cwd-only assignment", decoded)
			}
			out, errout, code = f.command(t, "", "backfill", "--yes", "--background", "--harness", "codex", "--mapping-file", output)
			store, err := state.Open(f.home)
			must(t, err)
			regs, err := store.LoadRegistrations()
			must(t, err)
			want := 2
			if choice == reviewConfiguredDestination {
				want = 1
			}
			if code != 0 || len(regs) != want || choice == reviewConfiguredDestination && regs[0].NativeSessionID != first {
				t.Fatal("cwd-wide assignment failed to preserve per-source compatibility", code, regs, out, errout)
			}
		})
	}
}

func TestRecoveryReviewHistoricalAssignmentPreservesExcludedPhysicalHistory(t *testing.T) {
	f := newRecoveryReviewFixture(t)
	private := filepath.Join(f.userHome, "excluded-physical-checkout")
	f.cfg.Archive.Projects = []archive.ProjectActivation{{Root: private, Included: false, ProjectID: archive.ProjectID(private)}}
	f.save(t)
	const thread = "11111111-1111-4111-8111-111111111111"
	const revision = "22222222-2222-4222-8222-222222222222"
	const selectedRevision = "33333333-3333-4333-8333-333333333333"
	native := filepath.Join(f.userHome, ".codex")
	must(t, os.MkdirAll(filepath.Join(native, "sessions"), 0700))
	meta := func(cwd string, base map[string]any, ordinal int) []byte {
		payload := map[string]any{"id": thread, "timestamp": "2026-10-13T12:00:00Z", "cwd": cwd, "source": "cli", "originator": "codex_cli_rs", "cli_version": "0.160.0", "history_mode": "paginated"}
		if base != nil {
			payload["history_base"] = base
		}
		data, err := json.Marshal(map[string]any{"type": "session_meta", "ordinal": ordinal, "payload": payload})
		must(t, err)
		return append(data, '\n')
	}
	prefix := append(meta(f.missing, nil, 0), []byte(`{"type":"event_msg","ordinal":1,"payload":{"type":"task_started","turn_id":"synthetic-task","root_turn_id":"synthetic-task","started_at":"2026-10-13T12:00:00Z"}}`+"\n")...)
	seed := filepath.Join(native, "sessions", "rollout-"+thread+".jsonl")
	must(t, os.WriteFile(seed, prefix, 0600))
	current := filepath.Join(native, "sessions", "rollout-"+revision+".jsonl")
	data := meta(private, map[string]any{"thread_id": thread, "end_ordinal_exclusive": 2, "end_byte_offset": len(prefix)}, 2)
	data = append(data, []byte(`{"type":"response_item","ordinal":3,"payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Synthetic current own history"}]}}`+"\n")...)
	must(t, os.WriteFile(current, data, 0600))
	selected := filepath.Join(native, "sessions", "rollout-"+selectedRevision+".jsonl")
	selectedData := meta(f.missing, map[string]any{"thread_id": revision, "end_ordinal_exclusive": 4, "end_byte_offset": len(data)}, 4)
	selectedData = append(selectedData, []byte(`{"type":"response_item","ordinal":5,"payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Synthetic selected own history"}]}}`+"\n")...)
	must(t, os.WriteFile(selected, selectedData, 0600))
	db, err := sql.Open("sqlite", filepath.Join(native, "state_5.sqlite"))
	must(t, err)
	defer func() { _ = db.Close() }()
	_, err = db.ExecContext(t.Context(), "CREATE TABLE threads(id TEXT PRIMARY KEY,rollout_path TEXT)")
	must(t, err)
	_, err = db.ExecContext(t.Context(), "INSERT INTO threads VALUES(?,?)", thread, selected)
	must(t, err)
	output := filepath.Join(t.TempDir(), "historical.json")
	out, errout, code := f.command(t, "new\nRetired service\nexport\n", "backfill", "review-projects", "--harness", "codex", "--output", output)
	if code != 0 {
		t.Fatal("review could not export the otherwise permitted original cwd", code, out, errout)
	}
	var plan backfill.Plan
	f.env.observeBackfillPlan = func(observed backfill.Plan) { plan = observed }
	out, errout, code = f.command(t, "", "backfill", "--yes", "--background", "--harness", "codex", "--mapping-file", output)
	store, err := state.Open(f.home)
	must(t, err)
	regs, err := store.LoadRegistrations()
	must(t, err)
	excluded := 0
	for _, candidate := range plan.Candidates {
		if candidate.Skip == backfill.SkipExcludedHistory {
			excluded++
		}
	}
	if code != 0 || len(regs) != 0 || excluded != 1 {
		t.Fatal("reviewed assignment bypassed an excluded physical history cwd", code, regs, out, errout)
	}
	f.cfg.Archive.Projects = nil
	f.save(t)
	out, errout, code = f.command(t, "", "backfill", "--yes", "--background", "--harness", "codex", "--mapping-file", output)
	regs, err = store.LoadRegistrations()
	must(t, err)
	if code != 0 || len(regs) != 1 || regs[0].HistoricalProject == nil {
		t.Fatal("the unrestricted control did not prove supported physical history", code, regs, out, errout)
	}
}

func TestRecoveryCwdTargetsKeepsBoundedUnionIndependentOfKeyOrder(t *testing.T) {
	t.Parallel()
	groups := make([]backfill.RecoveryReviewGroup, 3)
	for i := range groups {
		for target := range backfill.RecoveryReviewLimit {
			outcome := sourcefacts.RecoveryMappingConflict
			if target%2 == i {
				outcome = ""
			} else if i == 2 {
				outcome = sourcefacts.RecoveryOriginUnresolved
			}
			groups[i].Targets = append(groups[i].Targets, backfill.RecoveryTarget{Root: fmt.Sprintf("/target/%03d", target), Outcome: outcome})
		}
	}
	first := recoveryCwdTargets(groups)
	slices.Reverse(groups)
	second := recoveryCwdTargets(groups)
	if len(first) != backfill.RecoveryReviewLimit || !slices.Equal(first, second) {
		t.Fatal("cwd target union changed the owner's bounded menu or key-order semantics")
	}
	for _, target := range first {
		if target.Outcome != "" {
			t.Fatal("one compatible observed key was hidden by another key's refusal", target)
		}
	}
}

func TestRecoveryReviewExportsDatabaseOnlyCursorAssignments(t *testing.T) {
	f := newRecoveryReviewFixture(t)
	f.cfg.Archive.Projects = nil
	f.save(t)
	path := f.env.cursorDatabase()
	must(t, os.MkdirAll(filepath.Dir(path), 0700))
	db, err := sql.Open("sqlite", path)
	must(t, err)
	defer func() { must(t, db.Close()) }()
	_, err = db.ExecContext(t.Context(), "CREATE TABLE cursorDiskKV (key TEXT UNIQUE ON CONFLICT REPLACE, value BLOB)")
	must(t, err)
	for _, id := range []string{"synthetic-cursor-first", "synthetic-cursor-second"} {
		composer := map[string]any{"_v": 18, "composerId": id, "createdAt": time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC).UnixMilli(), "status": "completed", "workspaceIdentifier": map[string]any{"uri": "file://" + f.missing}, "fullConversationHeadersOnly": []map[string]any{{"bubbleId": "m1", "type": 1}}}
		data, err := json.Marshal(composer)
		must(t, err)
		_, err = db.ExecContext(t.Context(), "INSERT INTO cursorDiskKV VALUES (?, ?)", "composerData:"+id, data)
		must(t, err)
		_, err = db.ExecContext(t.Context(), "INSERT INTO cursorDiskKV VALUES (?, ?)", "bubbleId:"+id+":m1", `{"_v":3,"bubbleId":"m1","type":1,"text":"Private synthetic Cursor prompt"}`)
		must(t, err)
		if id == "synthetic-cursor-first" {
			_, err = db.ExecContext(t.Context(), "INSERT INTO cursorDiskKV VALUES (?, ?)", "composerData:synthetic-alias", data)
			must(t, err)
		}
	}
	must(t, db.Close())
	var observed backfill.Plan
	f.env.observeBackfillPlan = func(plan backfill.Plan) { observed = plan }
	out, errout, code := f.command(t, "", "backfill", "--dry-run", "--json", "--harness", "cursor")
	unresolved := 0
	for _, candidate := range observed.Candidates {
		if candidate.SourceKind == archive.SourceKindCursorSQLite && candidate.Skip == backfill.SkipWorktreeUnresolved {
			unresolved++
		}
	}
	if code != 0 || unresolved != 2 {
		t.Fatal("database-only fixture did not reach project recovery", code, unresolved, out, errout)
	}
	const historicalID = "44444444444444444444444444444444"
	manual, err := backfill.EncodeMappingFile(backfill.Filters{HistoricalMappings: map[string]string{f.missing: historicalID}, HistoricalProjects: []backfill.HistoricalProjectDefinition{{ID: historicalID, Label: "Retired Cursor project"}}})
	must(t, err)
	manualPath := filepath.Join(t.TempDir(), "manual-cursor.json")
	must(t, os.WriteFile(manualPath, manual, 0600))
	out, errout, code = f.command(t, "", "backfill", "--dry-run", "--json", "--harness", "cursor", "--mapping-file", manualPath)
	if code != 0 || len(observed.Imported()) != 2 {
		t.Fatal("existing manual assignments did not qualify the supported database sources", code, len(observed.Imported()), out, errout)
	}
	output := filepath.Join(t.TempDir(), "cursor-reviewed.json")
	beforeArchive, beforeNative := snapshotTree(t, f.home), snapshotTree(t, f.userHome)
	out, errout, code = f.command(t, "new\nRetired Cursor project\nexport\n", "backfill", "review-projects", "--harness", "cursor", "--output", output)
	if code != 0 || !strings.Contains(out, "Original cwd: "+strconv.Quote(f.missing)) || !strings.Contains(out, "Candidates: 2") || strings.Count(out, "Choose an exact destination") != 1 {
		t.Fatal("database-only missing cwd was omitted or duplicate rows widened review", code, out, errout)
	}
	if strings.Contains(out, "Private synthetic Cursor prompt") || strings.Contains(out, "synthetic-cursor-") || strings.Contains(out, "state.vscdb") || beforeArchive != snapshotTree(t, f.home) || beforeNative != snapshotTree(t, f.userHome) {
		t.Fatal("database review exposed content or changed archive/native state", out)
	}
	data, err := os.ReadFile(output)
	must(t, err)
	var assignments backfill.Filters
	must(t, backfill.ReadMappingFile(bytes.NewReader(data), &assignments))
	if len(assignments.HistoricalMappings) != 1 || assignments.HistoricalMappings[f.missing] == "" || len(assignments.ProjectMappings) != 0 {
		t.Fatal("database review did not export one exact shared-cwd assignment", assignments)
	}
	out, errout, code = f.command(t, "", "backfill", "--yes", "--background", "--harness", "cursor", "--mapping-file", output)
	store, err := state.Open(f.home)
	must(t, err)
	regs, err := store.LoadRegistrations()
	must(t, err)
	if code != 0 || len(regs) != 2 {
		t.Fatal("reviewed database-only assignments did not unblock supported imports", code, regs, out, errout)
	}
	for _, reg := range regs {
		if reg.SourceKind != archive.SourceKindCursorSQLite || reg.TranscriptPath != "" || reg.HistoricalProject == nil || reg.HistoricalProject.ProjectID != assignments.HistoricalMappings[f.missing] || reg.HistoricalProject.OriginalCwd != f.missing {
			t.Fatal("database-only import lost exact historical assignment", reg)
		}
	}
}
