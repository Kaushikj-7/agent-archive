package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/agents/builtin"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/capture"
	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/destination"
	"github.com/wangjohn/agent-archive/internal/discovery"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
	"github.com/wangjohn/agent-archive/internal/trace"
)

type museTestEnvironmentKey string

const (
	museTestSessionID museTestEnvironmentKey = "MUSE_SESSION_ID"
	museTestThreadID  museTestEnvironmentKey = "MUSE_THREAD_ID"
)

func museTestIdentityEnvironment(key string) (string, bool) {
	switch museTestEnvironmentKey(key) {
	case museTestSessionID, museTestThreadID:
		return "00000000-0000-7000-8000-000000000001", true
	default:
		return "", false
	}
}

type museRootRefusalCase string

const (
	refusalBeforeWindow        museRootRefusalCase = "original_before_window"
	refusalAfterActivation     museRootRefusalCase = "activation_after_original"
	refusalMalformedSuffix     museRootRefusalCase = "malformed_suffix"
	refusalDuplicateCopy       museRootRefusalCase = "duplicate_copy"
	refusalUnknownContinuation museRootRefusalCase = "unknown_continuation"
	refusalDuplicateSelector   museRootRefusalCase = "duplicate_selector"
	refusalTrailingPayload     museRootRefusalCase = "trailing_payload"
)

type museRootCaptureFixture struct {
	home     string
	project  string
	root     string
	path     string
	store    *state.Store
	registry *builtin.Registry
	cfg      config.Config
	env      Env
	observed time.Time
	birth    time.Time
}

func newMuseRootCaptureFixture(t *testing.T) museRootCaptureFixture {
	t.Helper()
	return museRootCaptureFixtureWithConfig(t, true)
}

func museRootCaptureFixtureWithConfig(t *testing.T, saveConfig bool) museRootCaptureFixture {
	t.Helper()
	home, userHome := t.TempDir(), t.TempDir()
	if err := os.Chmod(home, 0700); err != nil {
		t.Fatal(err)
	}
	project, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(project, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	dataHome := t.TempDir()
	if err := os.Mkdir(filepath.Join(dataHome, "sessions"), 0700); err != nil {
		t.Fatal(err)
	}
	root, err := filepath.EvalSymlinks(filepath.Join(dataHome, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	const id = "00000000-0000-7000-8000-000000000001"
	raw, err := os.ReadFile(filepath.Join("..", "agents", "muse", "testdata", "synthetic-exec-conversation.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	raw = bytes.ReplaceAll(raw, []byte("/work/synthetic-muse"), []byte(project))
	path := filepath.Join(root, "2026", "10", "15", id, "session.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	birth := time.UnixMicro(1792083855882704).UTC()
	observed := time.Now().UTC()
	cfg := config.Config{MachineID: "00000000000000000000000000000001", RetentionDays: 90, Harnesses: []string{"muse"}, Storage: destination.Config{Provider: destination.ProviderS3, Bucket: "synthetic-muse", Prefix: "synthetic", AWSProfile: "synthetic"}, DestinationSince: birth.Add(-time.Hour), NativeRoots: []archive.NativeRoot{{Agent: "muse", Configured: filepath.Dir(root), Canonical: root, Revision: "one"}}, Archive: archive.Config{Enabled: true, SchemaVersion: 1, Projects: []archive.ProjectActivation{{Root: project, ProjectID: archive.ProjectID(project), Included: true, ActivatedAt: birth.Add(-time.Hour)}}}}
	cfg, err = config.ReconcileMuseCapture(cfg, config.Config{}, birth.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if saveConfig {
		if err := config.Save(home, cfg); err != nil {
			t.Fatal(err)
		}
	}
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	registry := builtin.NewBuiltins()
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), observed)
	env.Agents = registry
	env.LookupEnv = museTestIdentityEnvironment
	return museRootCaptureFixture{home: home, project: project, root: root, path: path, store: store, registry: registry, cfg: cfg, env: env, observed: observed, birth: birth}
}

func TestMuseRootHookCandidatePublishesThroughSharedInventory(t *testing.T) {
	f := newMuseRootCaptureFixture(t)
	disableTrace := trace.Enable()
	defer disableTrace()
	home, project, store, registry, cfg, env, observed, birth := f.home, f.project, f.store, f.registry, f.cfg, f.env, f.observed, f.birth
	const id = "00000000-0000-7000-8000-000000000001"
	payload, _ := json.Marshal(map[string]any{"hook_event_name": "SessionStart", "session_id": id, "transcript_path": nil, "cwd": project, "source": "startup"})
	var stderr bytes.Buffer
	if code := runHookCommand([]string{"--harness", "muse"}, bytes.NewReader(payload), &stderr, env); code != 0 || stderr.Len() != 0 {
		t.Fatalf("hook code=%d stderr=%s", code, stderr.String())
	}
	registrations, err := store.LoadRegistrations()
	if err != nil || len(registrations) != 0 {
		t.Fatalf("hook created owner: %+v %v", registrations, err)
	}
	entries, err := os.ReadDir(filepath.Join(home, "admission-intents"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("candidate count: %v %v stderr=%s", entries, err, stderr.String())
	}
	pendingView, err := readStatus(env)
	if err != nil || len(pendingView.Apps) != 1 || pendingView.Apps[0].NativeStartup == nil || pendingView.Apps[0].NativeStartup.Pending != 1 || !pendingView.Apps[0].NativeStartup.Complete || !pendingView.Apps[0].HookObserved || pendingView.Apps[0].CapturedLocally || pendingView.Apps[0].Published || pendingView.Apps[0].ReadBackVerified {
		t.Fatalf("pending startup status claimed proof/publication: %+v %v", pendingView.Apps, err)
	}
	cloud := storagetest.NewMemoryStore()
	calls := 0
	opts := collector.Options{Sources: registry, Parsers: registry, Decoders: registry, MachineID: cfg.MachineID, NativeRoots: cfg.NativeRoots, AcceptSession: cfg.AcceptSession, Now: func() time.Time { return observed }, NativeLocatorFactory: func(ctx context.Context, pass collector.NativeLocatorPass) (agentapi.NativeLocatorLookup, func() error, error) {
		calls++
		if len(pass.Queries) != 1 || pass.Queries[0].Purpose != agentapi.LocatorStartupCandidate {
			t.Fatalf("unscoped startup query: %+v", pass.Queries)
		}
		inventory, err := discovery.NewNativeInventory(ctx, home, pass.Roots, pass.Queries, pass.ReadBudget, builtin.ValidateNativeLocator)
		if err != nil {
			return nil, nil, err
		}
		return inventory, inventory.Close, nil
	}}
	result, err := collector.Run(t.Context(), store, cloud, opts)
	defer result.CloseStagedResults()
	if err != nil || len(result.Errors) != 0 || len(result.Published) != 1 || calls != 1 {
		t.Fatalf("publication: %+v %v calls=%d", result, err, calls)
	}
	registrations, err = store.LoadRegistrations()
	if err != nil || len(registrations) != 1 || registrations[0].NativeBinding == nil || registrations[0].NativeBinding.Version != 2 || registrations[0].NativeAdmission == nil {
		t.Fatalf("complete first owner: %+v %v", registrations, err)
	}
	entries, err = os.ReadDir(filepath.Join(home, "admission-intents"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("candidate not acknowledged: %v %v", entries, err)
	}
	var measured bytes.Buffer
	trace.Write(&measured)
	sourceBytes, readErr := os.ReadFile(f.path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !strings.Contains(measured.String(), "filter_input_bytes "+strconv.Itoa(len(sourceBytes))) {
		t.Fatalf("native Filter ran more or less than once: %s", measured.String())
	}
	if registrations[0].SessionStartedAt != birth {
		t.Fatalf("original native time replaced: %v", registrations[0].SessionStartedAt)
	}
	published, err := store.LoadPublishedState(registrations[0].ArchiveSessionID)
	if err != nil || published == nil {
		t.Fatalf("published readback missing: %v", err)
	}
	publishedView, err := readStatus(env)
	if err != nil || len(publishedView.Apps) != 1 || !publishedView.Apps[0].Published || publishedView.Apps[0].ReadBackVerified {
		t.Fatalf("publication prematurely verified: %+v %v", publishedView.Apps, err)
	}
	if _, err := verifyPublicationsWithin(t.Context(), home, cfg, env, store, cloud); err != nil {
		t.Fatal(err)
	}
	completedView, err := readStatus(env)
	if err != nil || len(completedView.Apps) != 1 || completedView.Apps[0].NativeStartup != nil || !completedView.Apps[0].CapturedLocally || !completedView.Apps[0].Published || !completedView.Apps[0].ReadBackVerified {
		t.Fatalf("admitted/publication/readback status missing: %+v %v", completedView.Apps, err)
	}
	// This test stays sequential because trace observes the process-wide filter
	// counter. Continue through the same production queue after a real resume.
	owner := registrations[0]
	proof := *owner.NativeAdmission
	if _, _, err := store.EnsureArchiveSessionID(agentmeta.SessionKey{Agent: agentmeta.Muse, NativeID: "unowned"}); err == nil {
		t.Fatal("generic API reserved strong Muse ownership")
	}
	if _, err := config.SetPaused(home, true, observed.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	resumed, err := config.SetPaused(home, false, observed.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	env.Now = func() time.Time { return observed.Add(3 * time.Second) }
	continuation, err := json.Marshal(map[string]any{"hook_event_name": "UserPromptSubmit", "session_id": id, "transcript_path": nil, "cwd": project})
	if err != nil {
		t.Fatal(err)
	}
	stderr.Reset()
	if code := runHookCommand([]string{"--harness", "muse"}, bytes.NewReader(continuation), &stderr, env); code != 0 || stderr.Len() != 0 {
		t.Fatalf("continuation hook code=%d stderr=%s", code, stderr.String())
	}
	entries, err = os.ReadDir(filepath.Join(home, "admission-intents"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("continuation did not enter real queue: %v %v", entries, err)
	}
	queuedPath := filepath.Join(home, "admission-intents", entries[0].Name())
	rawIntent, err := os.ReadFile(queuedPath)
	if err != nil {
		t.Fatal(err)
	}
	var intent agentapi.AdmissionIntent
	if err := json.Unmarshal(rawIntent, &intent); err != nil {
		t.Fatal(err)
	}
	if intent.NativeWindowGeneration != resumed.MuseCapture.Generation || intent.NativeAdmission == nil || *intent.NativeAdmission != proof {
		t.Fatal("continuation replaced birth or lost observed resumed window")
	}
	canceledContext, cancelCollector := context.WithCancel(t.Context())
	cancelCollector()
	beforeRegistration, err := os.ReadFile(filepath.Join(home, "registrations", owner.ArchiveSessionID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	canceledResult, _ := collector.Run(canceledContext, store, cloud, collector.Options{Sources: registry, Parsers: registry, Decoders: registry, MachineID: cfg.MachineID, NativeRoots: resumed.NativeRoots, AcceptSession: resumed.AcceptSession, Now: func() time.Time { return observed.Add(3 * time.Second) }})
	canceledResult.CloseStagedResults()
	afterCanceledIntent, err := os.ReadFile(queuedPath)
	if err != nil || !bytes.Equal(rawIntent, afterCanceledIntent) {
		t.Fatalf("canceled collector acknowledged continuation: %v", err)
	}
	afterCanceledRegistration, err := os.ReadFile(filepath.Join(home, "registrations", owner.ArchiveSessionID+".json"))
	if err != nil || !bytes.Equal(beforeRegistration, afterCanceledRegistration) {
		t.Fatalf("canceled collector changed owner: %v", err)
	}
	if _, found, err := store.LoadRequest(owner.ArchiveSessionID); err != nil || found {
		t.Fatalf("canceled collector wrote a continuation request: %t %v", found, err)
	}
	for _, mutate := range []func(*agentapi.AdmissionIntent){
		func(i *agentapi.AdmissionIntent) { i.NativeOwnerID = "00000000000000000000000000000000" },
		func(i *agentapi.AdmissionIntent) { i.ObservedCwd = filepath.Dir(project) },
		func(i *agentapi.AdmissionIntent) { i.Effects = nil },
		func(i *agentapi.AdmissionIntent) {
			i.NativeWindowGeneration = strings.ToUpper(i.NativeWindowGeneration) + "bad"
		},
		func(i *agentapi.AdmissionIntent) { i.ObservedAt = resumed.MuseCapture.Since.Add(-time.Second) },
	} {
		mutated := intent
		mutate(&mutated)
		if _, err := store.ForNativeContinuation(t.Context(), mutated); err == nil {
			t.Fatal("direct continuation accepted foreign or incomplete authority")
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := store.ForNativeContinuation(ctx, intent); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled continuation: %v", err)
	}
	guarded, err := store.ForNativeContinuation(t.Context(), intent)
	if err != nil {
		t.Fatal(err)
	}
	if err := guarded.SaveRequest("foreign-owner", "turn_start", observed.Add(3*time.Second)); err == nil {
		t.Fatal("continuation wrote foreign owner request")
	}
	if err := capture.ReplayAdmissionIntents(home, observed.Add(3*time.Second), registry); err != nil {
		t.Fatal(err)
	}
	updated, found, err := store.LoadRegistration(owner.ArchiveSessionID)
	if err != nil || !found || updated.NativeAdmission == nil || *updated.NativeAdmission != proof || !updated.SessionStartedAt.Equal(birth) || !updated.HookObservedAt.Equal(observed.Add(3*time.Second)) {
		t.Fatalf("resumed continuation changed birth or lost observation: %+v %v", updated, err)
	}
	// A captured continuation cannot borrow a later window after pause/resume.
	if _, err := config.SetPaused(home, true, observed.Add(4*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := config.SetPaused(home, false, observed.Add(5*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ForNativeContinuation(t.Context(), intent); err == nil {
		t.Fatal("stale captured continuation borrowed a later window")
	}
	if strings.Contains(stderr.String(), "PRIVATE") {
		t.Fatal("native input leaked")
	}
}

// Sequential: each case observes the process-wide FilterBytes trace counter.
func TestMuseRootAdmissionRefusesUnsupportedSourceBeforeOwnership(t *testing.T) {
	for _, kind := range []museRootRefusalCase{refusalBeforeWindow, refusalAfterActivation, refusalMalformedSuffix, refusalDuplicateCopy, refusalUnknownContinuation, refusalDuplicateSelector, refusalTrailingPayload} {
		t.Run(string(kind), func(t *testing.T) {
			f := newMuseRootCaptureFixture(t)
			disableTrace := trace.Enable()
			defer disableTrace()
			cfg := f.cfg
			switch kind {
			case refusalBeforeWindow:
				next := cfg
				next.NativeRoots = append([]archive.NativeRoot(nil), cfg.NativeRoots...)
				next.NativeRoots[0].Revision = "two"
				cfg, _ = config.ReconcileMuseCapture(next, cfg, f.birth.Add(time.Second))
			case refusalAfterActivation:
				cfg.Archive.Projects = append([]archive.ProjectActivation(nil), cfg.Archive.Projects...)
				cfg.Archive.Projects[0].ActivatedAt = f.birth.Add(time.Second)
				cfg, _ = config.ReconcileMuseCapture(cfg, f.cfg, f.birth.Add(-time.Second))
			case refusalMalformedSuffix:
				file, err := os.OpenFile(f.path, os.O_APPEND|os.O_WRONLY, 0600)
				if err != nil {
					t.Fatal(err)
				}
				_, writeErr := file.WriteString("{\"unknown_authority\":true}\n")
				closeErr := file.Close()
				if writeErr != nil || closeErr != nil {
					t.Fatal(writeErr, closeErr)
				}
			case refusalDuplicateCopy:
				raw, err := os.ReadFile(f.path)
				if err != nil {
					t.Fatal(err)
				}
				other := filepath.Join(f.root, "2026", "10", "16", "00000000-0000-7000-8000-000000000001", "session.jsonl")
				if err := os.MkdirAll(filepath.Dir(other), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(other, raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := config.Save(f.home, cfg); err != nil {
				t.Fatal(err)
			}
			name := "SessionStart"
			payload := map[string]any{"hook_event_name": name, "session_id": "00000000-0000-7000-8000-000000000001", "cwd": f.project, "transcript_path": nil, "source": "startup"}
			if kind == refusalUnknownContinuation {
				payload["hook_event_name"] = "Stop"
				delete(payload, "source")
			}
			raw, _ := json.Marshal(payload)
			if kind == refusalDuplicateSelector {
				raw = append([]byte(`{"session_id":"old",`), raw[1:]...)
			}
			if kind == refusalTrailingPayload {
				raw = append(raw, []byte(` {"session_id":"later"}`)...)
			}
			var stderr bytes.Buffer
			runHookCommand([]string{"--harness", "muse"}, bytes.NewReader(raw), &stderr, f.env)
			calls := 0
			remote := storagetest.NewMemoryStore()
			result, err := collector.Run(t.Context(), f.store, remote, collector.Options{Sources: f.registry, Parsers: f.registry, Decoders: f.registry, MachineID: cfg.MachineID, NativeRoots: cfg.NativeRoots, AcceptSession: cfg.AcceptSession, Now: func() time.Time { return f.observed }, NativeLocatorFactory: func(ctx context.Context, scope collector.NativeLocatorPass) (agentapi.NativeLocatorLookup, func() error, error) {
				calls += len(scope.Queries)
				inventory, err := discovery.NewNativeInventory(ctx, f.home, scope.Roots, scope.Queries, scope.ReadBudget, builtin.ValidateNativeLocator)
				if err != nil {
					return nil, nil, err
				}
				return inventory, inventory.Close, nil
			}})
			defer result.CloseStagedResults()
			if err != nil {
				t.Fatal(err)
			}
			registrations, err := f.store.LoadRegistrations()
			if err != nil || len(registrations) != 0 || len(result.Published) != 0 {
				t.Fatalf("unsupported source acquired ownership: %+v %+v %v", registrations, result, err)
			}
			var measured bytes.Buffer
			trace.Write(&measured)
			if kind == refusalBeforeWindow || kind == refusalAfterActivation || kind == refusalDuplicateCopy || kind == refusalUnknownContinuation || kind == refusalDuplicateSelector || kind == refusalTrailingPayload {
				if strings.Contains(measured.String(), "filter_input_bytes") {
					t.Fatalf("refused input reached full Filter: %s", measured.String())
				}
			}
			objects, listErr := remote.List(t.Context(), "")
			if listErr != nil || len(objects) != 0 {
				t.Fatalf("unowned remote effects: %v %v", objects, listErr)
			}
			requests, err := f.store.LoadRequests()
			if err != nil || len(requests) != 0 {
				t.Fatalf("unowned request effects: %+v %v", requests, err)
			}
			for _, dir := range []string{"registrations", "requests", "sessions-v1"} {
				entries, err := os.ReadDir(filepath.Join(f.home, dir))
				if err != nil {
					t.Fatal(err)
				}
				for _, entry := range entries {
					if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".json") {
						t.Fatalf("unowned %s effect: %s", dir, entry.Name())
					}
				}
			}
			if kind == refusalUnknownContinuation || kind == refusalDuplicateSelector || kind == refusalTrailingPayload {
				if calls != 0 {
					t.Fatalf("unknown observation entered inventory: %d", calls)
				}
			}
		})
	}
}
