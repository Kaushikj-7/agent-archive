package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/capture"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/discovery"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func TestFreshScriptRequiresSeparateCodexChoicesBeforeInstallation(t *testing.T) {
	t.Parallel()
	for _, flags := range [][]string{nil, {"--codex-discovery", "on"}, {"--codex-capture-scope", "all-projects"}} {
		t.Run(strings.Join(flags, " "), func(t *testing.T) {
			t.Parallel()
			home, userHome := t.TempDir(), t.TempDir()
			env := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Now())
			args := []string{"--yes", "--provider", "s3", "--bucket", "synthetic", "--aws-profile", "profile", "--region", "us-east-1", "--apps", "codex", "--project", t.TempDir()}
			output := setupYes(t, env, "", 1, append(args, flags...)...)
			if !strings.Contains(output, "fresh scripted Codex setup requires") {
				t.Fatalf("missing explicit choice explanation: %s", output)
			}
			for _, path := range []string{filepath.Join(home, "config.json"), filepath.Join(home, "setup-transaction.json"), filepath.Join(userHome, ".codex", "hooks.json")} {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatalf("refused setup changed %s: %v", path, err)
				}
			}
		})
	}
}

func TestAllCodexScopeSupportsZeroProjectsAndDiscoveryOff(t *testing.T) {
	t.Parallel()
	for _, setting := range []string{"on", "off"} {
		t.Run(setting, func(t *testing.T) {
			t.Parallel()
			at := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
			home, userHome := t.TempDir(), t.TempDir()
			env := setupTestEnv(t, home, userHome, newFakeKeychain(), at)
			output := setupYes(t, env, "", 0, "--yes", "--provider", "s3", "--bucket", "synthetic", "--aws-profile", "profile", "--region", "us-east-1", "--apps", "codex", "--codex-discovery", setting, "--codex-capture-scope", "all-projects")
			cfg := mustLoadConfig(t, home)
			if cfg.EffectiveCodexCaptureScope() != config.CodexAllProjects || len(cfg.Archive.Projects) != 0 || cfg.Discovery.Enabled != (setting == "on") {
				t.Fatalf("wrong independent scope/source choices: %+v", cfg)
			}
			if cfg.CodexCapture.Authorization == nil || !cfg.CodexCapture.Authorization.NativeStartFloor.Equal(at) {
				t.Fatalf("scripted local consent lost its native-start floor: %+v", cfg.CodexCapture)
			}
			if setting == "on" && (cfg.CodexCapture.SourceAuthorization == nil || !cfg.CodexCapture.SourceAuthorization.NativeStartFloor.Equal(at)) {
				t.Fatal("discovery consent lost its independent native-start floor")
			}
			assertCodexFloorWriter(t, home)
			for _, row := range []string{"Codex scope: All current and future projects (Codex only)", "Starts: After local consent", "History: Last 7 days of included projects imported at setup", "Copies: Qualifying recent native copies"} {
				if !strings.Contains(output, row) {
					t.Fatalf("missing concise consent row %q: %s", row, output)
				}
			}
			if !setupContainsText(output, "start from now on in any non-excluded current or future project") && !strings.Contains(output, "Codex hooks capture supported tasks in any non-excluded project.") {
				t.Fatalf("all-mode next step required an included project: %s", output)
			}
			before := cfg.CodexCapture
			setupYes(t, env, "", 0, "--yes", "--no-skills")
			cfg = mustLoadConfig(t, home)
			if !reflect.DeepEqual(before, cfg.CodexCapture) || cfg.Discovery.Enabled != (setting == "on") {
				t.Fatal("omitted reconfiguration changed permission")
			}
		})
	}
}

func TestAllCodexScopeDoesNotRemoveOtherAppsProjectRequirement(t *testing.T) {
	t.Parallel()
	cfg := config.Config{Harnesses: []string{"codex", "claude"}}
	if err := config.SetCodexCaptureScope(&cfg, config.CodexAllProjects); err != nil {
		t.Fatal(err)
	}
	if problems := setupProjects(&cfg, nil, t.TempDir()); len(problems) == 0 {
		t.Fatal("blanket Codex scope silently included another app")
	}
}

func TestInteractiveCodexScopeDefaultsIncludedAndAllNeedsChoice(t *testing.T) {
	t.Parallel()
	for _, answer := range []string{"\n", "all-projects\n"} {
		cfg := config.Config{Harnesses: []string{"codex"}}
		var out bytes.Buffer
		if err := promptCodexCaptureScope(newPrompter(strings.NewReader(answer), &out), &cfg); err != nil {
			t.Fatal(err)
		}
		want := config.CodexIncludedProjects
		if answer != "\n" {
			want = config.CodexAllProjects
		}
		if cfg.EffectiveCodexCaptureScope() != want {
			t.Fatalf("answer %q: %s", answer, cfg.EffectiveCodexCaptureScope())
		}
	}
}

func TestBasicCodexStatusSeparatesMixedSourceAndPublicationEvidence(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	app := appStatus{Name: "codex", Hooks: "missing or incomplete", CodexCaptureScope: string(config.CodexAllProjects), Sessions: 2, UploadingSessions: 1, PublishedSessions: 1, VerifiedSessions: 0, Discovery: &discovery.Health{Enabled: true, Supported: true, Pending: true, LastAttempt: at.Add(-time.Minute), Outcomes: map[string]int{"native_format": 2, "unsupported_producer": 1, "incomplete_metadata": 1, "admission_retry": 1}}}
	screen := statusScreen{now: at, style: styleFor(&bytes.Buffer{})}
	row := screen.appRow(app, 0)
	parts := []string{strings.Join(row.cells, " ")}
	for _, note := range row.notes {
		parts = append(parts, note.text)
	}
	text := strings.Join(parts, " ")
	for _, fragment := range []string{"capture check pending", "all current and future Codex projects", "skipped 2", "status --verbose"} {
		if !strings.Contains(text, fragment) {
			t.Fatalf("missing %q in basic status: %s", fragment, text)
		}
	}
	if strings.Contains(text, "ETA") || strings.Contains(text, "%") {
		t.Fatal("basic discovery progress invented a completion estimate")
	}
	app.Discovery.Outcomes = nil
	if row := screen.appRow(app, 0); row.mark != screen.style.okMark() {
		t.Fatal("absent optional hooks or ordinary scan progress were marked broken")
	}
	app.Discovery.Supported = false
	app.Discovery.Outcomes = map[string]int{"incomplete_metadata": 1}
	if row := screen.appRow(app, 0); row.mark != screen.style.okMark() {
		t.Fatal("task waiting for complete metadata was treated as a repair warning")
	}
	app.Hooks = "installed"
	if optionalCodexHooks(app) {
		t.Fatal("installed hooks were mislabeled absent")
	}
	app.Hooks = "missing or incomplete"
	app.hooksNeedRepair = true
	if row := screen.appRow(app, 0); row.mark == screen.style.okMark() {
		t.Fatal("owned incomplete hooks lost repair warning")
	}
}

func TestCodexStatusTreatsMissingCorruptAndStaleSummaryAsUnknown(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"missing", "corrupt", "stale", "fresh"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			at := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
			home, userHome := t.TempDir(), t.TempDir()
			env := setupTestEnv(t, home, userHome, newFakeKeychain(), at)
			setupYes(t, env, "", 0, "--yes", "--provider", "s3", "--bucket", "synthetic", "--aws-profile", "profile", "--region", "us-east-1", "--apps", "codex", "--codex-discovery", "on", "--codex-capture-scope", "all-projects")
			path := filepath.Join(home, "discovery-health.json")
			if kind == "corrupt" {
				must(t, os.WriteFile(path, []byte(`{"private_prompt":"SYNTHETIC_BODY","health":`), 0600))
			} else if kind != "missing" {
				attempt := at
				if kind == "stale" {
					attempt = at.Add(-6 * time.Minute)
				}
				must(t, local.Write(path, struct {
					Version int              `json:"version"`
					Health  discovery.Health `json:"health"`
				}{Version: 1, Health: discovery.Health{Supported: true, LastAttempt: attempt}}))
			}
			// Status must not recover health by decoding the full source catalog.
			must(t, os.WriteFile(filepath.Join(home, "discovery-catalog.json"), []byte(`{"private_prompt":"SYNTHETIC_BODY","health":`), 0600))
			view, err := readStatus(env)
			must(t, err)
			if len(view.Apps) != 1 || view.Apps[0].Discovery == nil {
				t.Fatalf("missing Codex health: %+v", view.Apps)
			}
			app := view.Apps[0]
			unknown := len(app.Discovery.Errors) > 0
			if unknown != (kind != "fresh") {
				t.Fatalf("%s summary health=%+v", kind, app.Discovery)
			}
			if strings.Contains(strings.Join(app.Discovery.Errors, " "), "SYNTHETIC_BODY") {
				t.Fatal("status leaked discarded source content")
			}
			if view.problem == "No projects are included" {
				t.Fatal("zero-project Codex all-mode was treated as misconfiguration")
			}
		})
	}
}

func TestCodexExceptionReviewSupportsParentExclusionAndChildOverride(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	child := filepath.Join(root, "child")
	must(t, os.Mkdir(child, 0700))
	cfg := config.Config{Harnesses: []string{"codex"}}
	must(t, config.SetCodexCaptureScope(&cfg, config.CodexAllProjects))
	var out bytes.Buffer
	input := "exclude\n" + root + "\ninclude\n" + child + "\ndone\n"
	must(t, promptCodexExceptions(newPrompter(strings.NewReader(input), &out), &cfg, t.TempDir()))
	if len(cfg.Archive.Projects) != 2 || cfg.Archive.Projects[0].Included || !cfg.Archive.Projects[1].Included {
		t.Fatalf("exception choices lost: %+v", cfg.Archive.Projects)
	}
	if !strings.Contains(out.String(), "lifting an exclusion admits only eligible future starts") {
		t.Fatal("forward-only boundary missing from exception review")
	}
}

func TestScriptedAllCodexScopePublishesSeparateUnlistedProjects(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	home, userHome := t.TempDir(), t.TempDir()
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), at)
	remote := storagetest.NewMemoryStore()
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return remote, nil }
	setupYes(t, env, "", 0, "--yes", "--provider", "s3", "--bucket", "synthetic", "--aws-profile", "profile", "--region", "us-east-1", "--apps", "codex", "--codex-discovery", "on", "--codex-capture-scope", "all-projects")
	before := mustLoadConfig(t, home)
	if len(before.Discovery.CodexHomes) != 1 {
		t.Fatalf("source consent missing: %+v", before.Discovery)
	}
	created := at.Add(time.Minute)
	for n := range 2 {
		project, err := filepath.EvalSymlinks(t.TempDir())
		must(t, err)
		// An explicit synthetic Git root isolates this physical-identity test
		// from any repository an independent test creates above its tempdir.
		must(t, os.Mkdir(filepath.Join(project, ".git"), 0700))
		id := fmt.Sprintf("00000000-0000-0000-0000-%012d", n+1)
		path := filepath.Join(before.Discovery.CodexHomes[0], "sessions", "rollout-2026-10-03T12-01-00-"+id+".jsonl")
		must(t, os.MkdirAll(filepath.Dir(path), 0700))
		meta, err := json.Marshal(map[string]any{"type": "session_meta", "ordinal": 0, "timestamp": created.Format(time.RFC3339Nano), "payload": map[string]any{"id": id, "timestamp": created.Format(time.RFC3339Nano), "cwd": project, "source": "vscode", "originator": "Codex Desktop", "cli_version": "0.160.0", "history_mode": "paginated"}})
		must(t, err)
		task, err := json.Marshal(map[string]any{"type": "event_msg", "ordinal": 1, "timestamp": created.Format(time.RFC3339Nano), "payload": map[string]any{"type": "task_started", "turn_id": id, "root_turn_id": id, "started_at": created.Format(time.RFC3339Nano)}})
		must(t, err)
		prompt := `{"type":"response_item","ordinal":2,"payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Synthetic independent project task"}]}}`
		must(t, os.WriteFile(path, []byte(string(meta)+"\n"+string(task)+"\n"+prompt+"\n"), 0600))
	}
	env.Now = func() time.Time { return at.Add(2 * time.Minute) }
	for range 3 {
		result, err := runOnePass(env, true)
		if err != nil || len(result.Errors) != 0 {
			t.Fatalf("automatic collection failed: %+v %v", result, err)
		}
	}
	registrations, err := state.OpenReadOnly(home).LoadRegistrations()
	must(t, err)
	if len(registrations) != 2 || registrations[0].ProjectID == registrations[1].ProjectID || registrations[0].ProjectRoot == registrations[1].ProjectRoot {
		t.Fatalf("unlisted physical projects collapsed: %+v", registrations)
	}
	for _, registration := range registrations {
		evidence, err := readVerification(home, registration.ArchiveSessionID)
		if err != nil || evidence.VerifiedAt.IsZero() || !registration.HookObservedAt.IsZero() {
			t.Fatalf("unlisted project publication/read-back or source evidence failed: %+v %v", evidence, err)
		}
	}
	after := mustLoadConfig(t, home)
	if len(after.Archive.Projects) != 0 || !reflect.DeepEqual(before.CodexCapture, after.CodexCapture) {
		t.Fatal("newly discovered projects grew or rotated config permission")
	}
	// An unrelated explicit inclusion in blanket mode is a policy exception,
	// not a new requirement to capture and verify that directory before ready.
	setupYes(t, env, "", 0, "--yes", "--project", t.TempDir())
	view, err := readStatus(env)
	must(t, err)
	if len(view.Apps) != 1 || len(view.Apps[0].Projects) != 2 || !view.Apps[0].ReadBackVerified || view.Apps[0].HookObserved {
		t.Fatalf("basic/JSON evidence lost unlisted projects: %+v", view.Apps)
	}
}

func TestCodexChoiceFlagsRejectInvalidValuesAndOtherAppsBeforeMutation(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		flags []string
		exit  int
		want  string
	}{
		{"invalid source", []string{"--codex-discovery", "automatic"}, 2, "requires on or off"},
		{"invalid scope", []string{"--codex-capture-scope", "everything"}, 2, "requires included-projects or all-projects"},
		{"source without Codex", []string{"--codex-discovery", "off"}, 1, "requires Codex in --apps"},
		{"scope without Codex", []string{"--codex-capture-scope", "all-projects"}, 1, "requires Codex in --apps"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			home, userHome := t.TempDir(), t.TempDir()
			env := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Now())
			env.OpenStore = func(config.Config) (storage.ObjectStore, error) {
				t.Fatal("refused choices reached storage")
				return nil, nil
			}
			args := []string{"--yes", "--apps", "claude", "--provider", "s3", "--bucket", "synthetic", "--aws-profile", "profile", "--region", "us-east-1", "--project", t.TempDir()}
			output := setupYes(t, env, "", tc.exit, append(args, tc.flags...)...)
			if !strings.Contains(output, tc.want) {
				t.Fatalf("missing correction %q: %s", tc.want, output)
			}
			for _, name := range []string{"config.json", "setup-transaction.json", "setup-draft.json"} {
				if _, err := os.Stat(filepath.Join(home, name)); !os.IsNotExist(err) {
					t.Fatalf("refused setup mutated %s: %v", name, err)
				}
			}
		})
	}
}

func TestAllCodexDiagnosticStatusDoesNotInventProjectLocation(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	screen := statusScreen{now: at, style: styleFor(&bytes.Buffer{})}
	view := statusView{CaptureDiagnostics: []capture.Diagnostic{{Harness: "codex", Code: capture.DiagnosticProjectUnavailable, ObservedAt: at}}}
	rows := screen.captureRows(view)
	text := strings.Join(rows[len(rows)-1].cells, " ")
	if !strings.Contains(text, "Codex skipped a session") || strings.Contains(text, " in ") {
		t.Fatalf("path-free diagnostic invented a location: %s", text)
	}
}

func TestCodexStatusSeparatesActualRecoveryEvidence(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	home, userHome := t.TempDir(), t.TempDir()
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), at)
	setupYes(t, env, "", 0, "--yes", "--provider", "s3", "--bucket", "synthetic", "--aws-profile", "profile", "--region", "us-east-1", "--apps", "codex", "--codex-discovery", "on", "--codex-capture-scope", "all-projects")
	view, err := readStatus(env)
	must(t, err)
	if view.IdentityRecovery == nil || view.IdentityRecovery.Complete || view.IdentityRecovery.Phase != "unknown" {
		t.Fatalf("missing recovery claimed coverage: %+v", view.IdentityRecovery)
	}
	store, err := state.Open(home)
	must(t, err)
	complete, err := store.RecoverSessionIndexScheduled(context.Background(), state.SessionIndexRecoverySlice)
	must(t, err)
	if !complete {
		t.Fatal("empty synthetic inventory did not complete")
	}
	view, err = readStatus(env)
	must(t, err)
	if view.IdentityRecovery == nil || !view.IdentityRecovery.Complete || view.IdentityRecovery.Pending {
		t.Fatalf("actual complete evidence: %+v", view.IdentityRecovery)
	}
	if view.Apps[0].Discovery.Supported || view.Apps[0].PublishedSessions > 0 || view.Apps[0].ReadBackVerified {
		t.Fatal("local recovery invented discovery or publication proof")
	}
	must(t, os.Remove(filepath.Join(home, "session-membership.json")))
	view, err = readStatus(env)
	must(t, err)
	if view.IdentityRecovery.Complete || view.IdentityRecovery.Phase != "unknown" {
		t.Fatal("deleted fence remained complete")
	}
	screen := statusScreen{now: at, style: styleFor(&bytes.Buffer{})}
	for _, phase := range []string{"registrations", "candidates", "packed-shards", "packed-fallback", "requested-misses"} {
		rows := screen.captureRows(statusView{Apps: []appStatus{{Name: "codex", Hooks: "installed"}}, IdentityRecovery: &state.SessionIndexRecoveryStatus{Pending: true, Phase: phase}})
		row := rows[1]
		if row.mark != screen.info() || !strings.Contains(strings.Join(row.cells, " "), "Identity recovery: pending") {
			t.Fatalf("ordinary pending became repair: %+v", row)
		}
	}
}

func TestAnotherMachineCommandCarriesExplicitCodexPreferences(t *testing.T) {
	t.Parallel()
	for _, enabled := range []bool{false, true} {
		cfg := config.Config{Harnesses: []string{"codex"}, Storage: credentialsTestConfig(), Discovery: &config.DiscoveryConfig{Enabled: enabled}}
		must(t, config.SetCodexCaptureScope(&cfg, config.CodexAllProjects))
		command := anotherMachineCommand(cfg, "")
		choice := "off"
		if enabled {
			choice = "on"
		}
		if !strings.Contains(command, "--codex-discovery "+choice+" --codex-capture-scope all-projects") {
			t.Fatalf("fresh-machine command missing explicit choices: %s", command)
		}
	}
}

func assertCodexFloorWriter(t *testing.T, home string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(home, "config.json"))
	must(t, err)
	var document struct {
		SchemaVersion struct {
			Version int    `json:"version"`
			Writer  string `json:"writer"`
		} `json:"schema_version"`
	}
	must(t, json.Unmarshal(raw, &document))
	if document.SchemaVersion.Version != 3 || document.SchemaVersion.Writer != "codex-scope-floor-v3" {
		t.Fatalf("Codex scope save weakened floor fence: %+v", document.SchemaVersion)
	}
}

func TestScriptedPausedAllCodexSetupKeepsLocalFloorThroughResume(t *testing.T) {
	t.Parallel()
	for _, setting := range []string{"on", "off"} {
		t.Run(setting, func(t *testing.T) {
			t.Parallel()
			at := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
			home, userHome := t.TempDir(), t.TempDir()
			env := setupTestEnv(t, home, userHome, newFakeKeychain(), at)
			setupYes(t, env, "", 0, "--yes", "--provider", "s3", "--bucket", "synthetic", "--aws-profile", "profile", "--region", "us-east-1", "--apps", "codex", "--codex-discovery", setting, "--codex-capture-scope", "all-projects")
			env.Now = func() time.Time { return at.Add(time.Minute) }
			var out bytes.Buffer
			if code := runPauseCommand(&out, &out, env, true); code != 0 {
				t.Fatal(out.String())
			}
			floor := at.Add(2 * time.Minute)
			env.Now = func() time.Time { return floor }
			setupYes(t, env, "", 0, "--yes", "--provider", "s3", "--bucket", "replacement", "--aws-profile", "profile", "--region", "us-east-1")
			cfg := mustLoadConfig(t, home)
			if !cfg.Paused || len(cfg.Archive.Projects) != 0 || cfg.EffectiveCodexCaptureScope() != config.CodexAllProjects || cfg.CodexCapture.Authorization == nil || !cfg.CodexCapture.Authorization.NativeStartFloor.Equal(floor) || len(cfg.CodexCapture.Authorization.Intervals) != 0 {
				t.Fatalf("paused local setup lost scope/floor: %+v", cfg.CodexCapture)
			}
			assertCodexFloorWriter(t, home)
			before, err := os.ReadFile(filepath.Join(home, "config.json"))
			must(t, err)
			env.Now = func() time.Time { return floor.Add(-time.Nanosecond) }
			out.Reset()
			if code := runPauseCommand(&out, &out, env, false); code == 0 {
				t.Fatal("CLI resumed behind the newly committed local floor")
			}
			after, err := os.ReadFile(filepath.Join(home, "config.json"))
			must(t, err)
			if !bytes.Equal(before, after) {
				t.Fatal("rejected resume changed permission or writer fence")
			}
			env.Now = func() time.Time { return floor }
			out.Reset()
			if code := runPauseCommand(&out, &out, env, false); code != 0 {
				t.Fatal(out.String())
			}
			cfg = mustLoadConfig(t, home)
			root := t.TempDir()
			if _, allowed := cfg.CodexGeneration(root, root, floor.Add(-time.Nanosecond), floor.Add(time.Minute)); allowed {
				t.Fatal("resume widened native starts behind local floor")
			}
			if _, allowed := cfg.CodexGeneration(root, root, floor, floor.Add(time.Minute)); !allowed {
				t.Fatal("equality resume lost eligible local start")
			}
		})
	}
}

func TestCodexStatusShowsScheduledPackedRecoveryCompletion(t *testing.T) {
	at := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	home, userHome := t.TempDir(), t.TempDir()
	if err := os.Chmod(home, 0700); err != nil {
		t.Fatal(err)
	}
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), at)
	setupYes(t, env, "", 0, "--yes", "--provider", "s3", "--bucket", "synthetic", "--aws-profile", "profile", "--region", "us-east-1", "--apps", "codex", "--codex-discovery", "on", "--codex-capture-scope", "all-projects")
	store, err := state.Open(home)
	must(t, err)
	must(t, store.MarkSessionIndexRecoveryNeeded())
	// Existing packed evidence selects the actual packed writer on this small
	// synthetic inventory. The scheduler supplies current revision and inventory.
	var marker map[string]any
	path := filepath.Join(home, "session-index.json")
	must(t, local.Read(path, &marker))
	marker["version"] = 2
	marker["packed_epoch"] = "fixture-epoch"
	marker["packed_revision"] = "fixture-revision"
	marker["packed_inventory"] = "fixture-inventory"
	must(t, local.Write(path, marker))
	complete, err := store.RecoverSessionIndexScheduled(context.Background(), time.Nanosecond)
	must(t, err)
	if complete {
		t.Fatal("tiny packed slice unexpectedly completed")
	}
	view, err := readStatus(env)
	must(t, err)
	if view.IdentityRecovery == nil || !view.IdentityRecovery.Pending || view.IdentityRecovery.Phase != "packed-shards" {
		t.Fatalf("packed pending status: %+v", view.IdentityRecovery)
	}
	for range 20 {
		complete, err = store.RecoverSessionIndexScheduled(context.Background(), state.SessionIndexRecoverySlice)
		must(t, err)
		if complete {
			break
		}
	}
	if !complete {
		t.Fatal("packed recovery did not complete")
	}
	view, err = readStatus(env)
	must(t, err)
	if view.IdentityRecovery == nil || !view.IdentityRecovery.Complete || view.IdentityRecovery.Pending {
		t.Fatalf("packed completion status: %+v", view.IdentityRecovery)
	}
	data, err := json.Marshal(view)
	must(t, err)
	if !strings.Contains(string(data), `"identity_recovery":{"complete":true,"pending":false,"phase":"complete"}`) {
		t.Fatalf("packed status JSON: %s", data)
	}
	screen := statusScreen{now: at, style: styleFor(&bytes.Buffer{})}
	foundComplete := false
	for _, row := range screen.captureRows(view) {
		text := strings.Join(row.cells, " ")
		if strings.Contains(text, "Identity recovery:") {
			if row.mark != screen.info() || text != "Identity recovery: complete" {
				t.Fatalf("completed recovery retained warning: %+v", row)
			}
			foundComplete = true
		}
	}
	if !foundComplete {
		t.Fatal("completed recovery omitted informational row")
	}
	if view.Apps[0].Discovery.Supported || view.Apps[0].PublishedSessions > 0 || view.Apps[0].ReadBackVerified {
		t.Fatal("local packed recovery invented source/publication proof")
	}
}
