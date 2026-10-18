package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/agents/builtin"
	"github.com/wangjohn/agent-archive/internal/agents/muse"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/backfill"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/platform"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

const publicNativeImportID = "00000000-0000-7000-8000-000000000001"

type publicImportFilter struct {
	muse.Filter
	calls int
}

func (f *publicImportFilter) LeasedFilterFor(adapter archive.Adapter) bool { return adapter == f }
func (f *publicImportFilter) FilterLeased(ctx context.Context, in agentapi.NativeInput, opts agentapi.FilterContext, budget *agentapi.NativeReadBudget) (archive.FilteredTranscript, func(), error) {
	f.calls++
	return f.Filter.FilterLeased(ctx, in, opts, budget)
}

type publicNativeImportFixture struct {
	env                         Env
	home, native, path, project string
	filter                      *publicImportFilter
	remote                      *storagetest.MemoryStore
}

func newPublicNativeImportFixture(t *testing.T) publicNativeImportFixture {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := publicNativeImportFixture{home: filepath.Join(root, "archive"), native: filepath.Join(root, "native"), project: filepath.Join(root, "project"), filter: &publicImportFilter{}, remote: storagetest.NewMemoryStore()}
	if err := os.MkdirAll(filepath.Join(f.project, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	f.path = filepath.Join(f.native, "2026", "10", "15", publicNativeImportID, "session.jsonl")
	if err := os.MkdirAll(filepath.Dir(f.path), 0700); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join("..", "agents", "muse", "testdata", "synthetic-exec-conversation.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	projectJSON, err := json.Marshal(f.project)
	if err != nil {
		t.Fatal(err)
	}
	raw = bytes.ReplaceAll(raw, []byte(`"/work/synthetic-muse"`), projectJSON)
	if err := os.WriteFile(f.path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC()
	cfg := config.Config{MachineID: "machine-1", Storage: credentialsTestConfig(), RetentionDays: 90, Archive: archive.Config{SchemaVersion: 1, MachineID: "machine-1"}}
	cfg.Harnesses = []string{"claude"}
	cfg.Archive.Enabled = true
	cfg.Archive.Projects = []archive.ProjectActivation{{ProjectID: "opaque-muse-project", Root: f.project, Included: true, ActivatedAt: at.Add(-time.Hour)}}
	cfg.NativeAuthorityProtection = true
	cfg.NativeRoots = []archive.NativeRoot{{Agent: "muse", Configured: f.native, Canonical: f.native, Revision: "public-import-root"}}
	if err := config.Save(f.home, cfg); err != nil {
		t.Fatal(err)
	}
	builtins := builtin.NewBuiltins()
	bindings := []builtin.Integration{}
	for _, descriptor := range builtins.Catalog().All() {
		binding, _ := builtins.Lookup(string(descriptor.ID))
		if descriptor.ID == agentmeta.Muse {
			binding.Filter = f.filter
		}
		bindings = append(bindings, binding)
	}
	registry, err := builtin.New(agentmeta.Builtins(), bindings)
	if err != nil {
		t.Fatal(err)
	}
	f.env = Env{Home: func() (string, error) { return f.home, nil }, UserHomeDir: func() (string, error) { return root, nil }, WorkingDir: func() (string, error) { return f.project, nil }, Now: func() time.Time { return at }, LookupEnv: func(string) (string, bool) { return "", false }, IsTerminal: func(any) bool { return false }, OpenStore: func(config.Config) (storage.ObjectStore, error) { return f.remote, nil }, OS: platform.Darwin, Scheduler: newFakeScheduler(t, "missing"), Interrupts: noInterrupts, Agents: registry, repoKey: func(string) string { return "" }, gitHead: func(string, bool) (string, *bool) { return "", nil }}
	return f
}

func (f publicNativeImportFixture) run(in io.Reader, args ...string) (string, string, int) {
	var out, stderr bytes.Buffer
	code := Run(append([]string{"backfill"}, args...), in, &out, &stderr, f.env)
	return out.String(), stderr.String(), code
}

func publicImportFiles(t *testing.T, home string) map[string]string {
	t.Helper()
	files := map[string]string{}
	if err := filepath.WalkDir(home, func(path string, entry os.DirEntry, err error) error {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			files[path] = string(raw)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return files
}

func assertPublicImportFiles(t *testing.T, home string, expected map[string]string) {
	t.Helper()
	actual := publicImportFiles(t, home)
	if len(actual) != len(expected) {
		t.Fatal("read-only command changed file set", len(actual), len(expected))
	}
	for path, raw := range expected {
		if actual[path] != raw {
			t.Fatal("read-only command changed bytes", path)
		}
	}
}

func TestBackfillNativeImportPublicPreviewAndCancelHaveNoEffects(t *testing.T) {
	t.Parallel()
	f := newPublicNativeImportFixture(t)
	f.env.OpenStore = func(config.Config) (storage.ObjectStore, error) {
		t.Fatal("preview/cancel opened remote")
		return nil, nil
	}
	before := publicImportFiles(t, f.home)
	for _, jsonOut := range []bool{false, true} {
		args := []string{"--harness", "MUSE", "--dry-run", "--project", f.project}
		if jsonOut {
			args = append(args, "--json")
		}
		out, stderr, code := f.run(nil, args...)
		if code != 0 || strings.Contains(out, publicNativeImportID) || strings.Contains(out, f.path) || strings.Contains(out, "PRIVATE CONTEXT") || !strings.Contains(out, "unavailable") && !strings.Contains(out, "UNAVAILABLE") {
			t.Fatal(code, out, stderr)
		}
		if jsonOut {
			var result struct {
				Sessions []nativeImportRow `json:"sessions"`
				Complete bool              `json:"identity_complete"`
			}
			if err := json.Unmarshal([]byte(out), &result); err != nil || !result.Complete || len(result.Sessions) != 1 || result.Sessions[0].Ordinal != 1 || result.Sessions[0].NativeID != "" || result.Sessions[0].Status != "selected" || result.Sessions[0].ProjectID != "opaque-muse-project" {
				t.Fatal("public JSON selection", result, err, stderr)
			}
		}
		assertPublicImportFiles(t, f.home, before)
	}
	f.env.IsTerminal = func(any) bool { return true }
	out, stderr, code := f.run(strings.NewReader("\n"), "--harness", "muse", "--background")
	if code != 0 || !strings.Contains(out, "Cancelled") {
		t.Fatal(code, out, stderr)
	}
	if f.filter.calls != 0 {
		t.Fatal("preview/default-No filtered a source", f.filter.calls)
	}
	assertPublicImportFiles(t, f.home, before)
}

func TestBackfillNativeImportPublicMixedAndUnsupportedFlagsRefuseEarly(t *testing.T) {
	t.Parallel()
	env := Env{Home: func() (string, error) { t.Fatal("invalid purpose accessed archive home"); return "", nil }, UserHomeDir: func() (string, error) { t.Fatal("invalid purpose accessed user home"); return "", nil }, LookupEnv: func(string) (string, bool) { return "", false }}
	for _, args := range [][]string{{"--harness", "muse", "--harness", "claude"}, {"--harness=muse", "--mapping-file", "/unavailable/never-read.json"}, {"-harness", "muse", "--include-removed"}} {
		var out, stderr bytes.Buffer
		if code := Run(append([]string{"backfill"}, args...), nil, &out, &stderr, env); code != 2 {
			t.Fatal(code, out.String(), stderr.String())
		}
	}
}

func TestBackfillNativeImportPublicRegistrationPublicationHistoryAndUndo(t *testing.T) {
	f := newPublicNativeImportFixture(t)
	out, stderr, code := f.run(nil, "--harness", "muse", "--yes", "--background")
	if code != 0 {
		t.Fatal(code, out, stderr)
	}
	if f.filter.calls != 1 {
		t.Fatal("confirmed source was not filtered exactly once", f.filter.calls)
	}
	batches, err := backfill.LoadBatches(f.home)
	if err != nil || len(batches) != 1 || batches[0].CompletedAt == nil || batches[0].NativeImport == nil || len(batches[0].Sessions) != 1 {
		t.Fatal("actual confirmed batch", batches, err)
	}
	store, err := state.Open(f.home)
	if err != nil {
		t.Fatal(err)
	}
	reg, found, err := store.LoadRegistration(batches[0].Sessions[0])
	if err != nil || !found || reg.NativeImport == nil || reg.NativeAdmission != nil || reg.ProjectID != "opaque-muse-project" {
		t.Fatal("historical owner", found, err)
	}
	if _, found, err := store.LoadImportStage(t.Context(), reg); err != nil || !found {
		t.Fatal("durable stage", found, err)
	}
	if err := os.RemoveAll(f.native); err != nil {
		t.Fatal(err)
	}
	var syncOut, syncErr bytes.Buffer
	if code := Run([]string{"sync"}, nil, &syncOut, &syncErr, f.env); code != 0 {
		t.Fatal("normal native-free publication", code, syncOut.String(), syncErr.String())
	}
	if err := store.NativeImportRetainedAuthority(t.Context(), reg); err != nil {
		t.Fatal("verified selecting release", err)
	}
	out, stderr, code = f.run(nil, "history")
	if code != 0 || !strings.Contains(out, batches[0].ID) {
		t.Fatal("public history", code, out, stderr)
	}
	cfgRaw, err := os.ReadFile(filepath.Join(f.home, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	out, stderr, code = f.run(nil, "undo", batches[0].ID, "--yes")
	if code != 0 || strings.Contains(out, publicNativeImportID) || strings.Contains(out, f.path) || strings.Contains(out, "PRIVATE CONTEXT") {
		t.Fatal("public exact undo/privacy", code, out, stderr)
	}
	reg, found, err = store.LoadRegistration(reg.ArchiveSessionID)
	if err != nil || !found || reg.NativeImport.Membership.State != archive.NativeImportDetached {
		t.Fatal("membership detach", found, err)
	}
	after, err := os.ReadFile(filepath.Join(f.home, "config.json"))
	if err != nil || !bytes.Equal(cfgRaw, after) {
		t.Fatal("undo changed config", err)
	}
	if err := store.NativeImportRetainedAuthority(t.Context(), reg); err != nil {
		t.Fatal("undo removed acknowledged source", err)
	}
}

func TestBackfillNativeImportPublicRestartUsesFrozenStageWithoutNative(t *testing.T) {
	f := newPublicNativeImportFixture(t)
	f.env.backfillCheckpoint = func(step string) error {
		if step == "native import registered" {
			return errors.New("synthetic interrupted batch progress")
		}
		return nil
	}
	_, _, code := f.run(nil, "--harness", "muse", "--yes", "--background")
	if code != 1 {
		t.Fatal("interruption not reached", code)
	}
	if err := os.RemoveAll(f.native); err != nil {
		t.Fatal(err)
	}
	f.env.backfillCheckpoint = nil
	out, stderr, code := f.run(nil, "--harness", "muse", "--yes", "--background")
	if code != 0 {
		t.Fatal("actual frozen scope restart", code, out, stderr)
	}
	batches, err := backfill.LoadBatches(f.home)
	if err != nil || len(batches) != 1 || batches[0].CompletedAt == nil || len(batches[0].Sessions) != 1 {
		t.Fatal(batches, err)
	}
	if f.filter.calls != 1 {
		t.Fatal("restart re-filtered native evidence", f.filter.calls)
	}
}

type nativeImportAnswer struct {
	before func()
	input  *strings.Reader
}

func (r *nativeImportAnswer) Read(p []byte) (int, error) {
	if r.before != nil {
		r.before()
		r.before = nil
	}
	return r.input.Read(p)
}

func TestBackfillNativeImportPublicChangedPreviewRefusesConfirmation(t *testing.T) {
	for _, change := range []string{"source", "destination", "root", "project"} {
		t.Run(change, func(t *testing.T) {
			f := newPublicNativeImportFixture(t)
			f.env.IsTerminal = func(any) bool { return true }
			f.env.OpenStore = func(config.Config) (storage.ObjectStore, error) {
				t.Fatal("stale preview accessed remote")
				return nil, nil
			}
			var externalConfig []byte
			answer := &nativeImportAnswer{input: strings.NewReader("yes\n"), before: func() {
				if change == "source" {
					file, err := os.OpenFile(f.path, os.O_APPEND|os.O_WRONLY, 0600)
					if err != nil {
						t.Fatal(err)
					}
					_, writeErr := file.WriteString("\n")
					if err := errors.Join(writeErr, file.Close()); err != nil {
						t.Fatal(err)
					}
					return
				}
				cfg, _, err := config.Load(f.home)
				if err != nil {
					t.Fatal(err)
				}
				switch change {
				case "destination":
					cfg.Storage.Bucket = "other-synthetic-bucket"
				case "root":
					cfg.NativeRoots[0].Revision = "changed-root-revision"
				case "project":
					cfg.Archive.Projects[0].Included = false
				}
				if err := config.Save(f.home, cfg); err != nil {
					t.Fatal(err)
				}
				externalConfig, err = os.ReadFile(filepath.Join(f.home, "config.json"))
				if err != nil {
					t.Fatal(err)
				}
			}}
			out, stderr, code := f.run(answer, "--harness", "muse", "--background")
			if code != 1 || f.filter.calls != 0 {
				t.Fatal("changed preview accepted/filtered", code, out, stderr, f.filter.calls)
			}
			batches, err := backfill.LoadBatches(f.home)
			if err != nil || len(batches) != 0 {
				t.Fatal("stale scope minted a batch", batches, err)
			}
			if externalConfig != nil {
				raw, err := os.ReadFile(filepath.Join(f.home, "config.json"))
				if err != nil || !bytes.Equal(raw, externalConfig) {
					t.Fatal("stale import overwrote later configuration", err)
				}
			}
		})
	}
}

func TestBackfillNativeImportPublicDoesNotJoinOmittedHarnessAndNeedsNoHomeBeforeSetup(t *testing.T) {
	t.Parallel()
	f := newPublicNativeImportFixture(t)
	out, stderr, code := f.run(nil, "--dry-run", "--json")
	if code != 0 || strings.Contains(out, "historical-muse-import") || f.filter.calls != 0 {
		t.Fatal("default backfill admitted Muse", code, out, stderr)
	}
	absent := filepath.Join(t.TempDir(), "absent-archive")
	f.env.Home = func() (string, error) { return absent, nil }
	f.env.OpenStore = func(config.Config) (storage.ObjectStore, error) {
		t.Fatal("unconfigured preview accessed remote")
		return nil, nil
	}
	_, _, code = f.run(nil, "--harness", "muse", "--dry-run")
	if code != 1 {
		t.Fatal("unconfigured import did not refuse", code)
	}
	if _, err := os.Stat(absent); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("unconfigured preview created archive home", err)
	}
}
