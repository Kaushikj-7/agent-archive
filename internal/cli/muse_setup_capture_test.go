package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agents/builtin"
	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/discovery"
	"github.com/wangjohn/agent-archive/internal/scheduler"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func TestMuseSetupCommitsOnlyIssuedWindowAndRefreshPreservesIt(t *testing.T) {
	t.Parallel()
	f := museRootCaptureFixtureWithConfig(t, false)
	env := f.env
	env.MuseRootCaptureQualified = true
	env.Now = func() time.Time { return f.birth.Add(-time.Minute) }
	xdg := t.TempDir()
	lookup := env.LookupEnv
	env.LookupEnv = func(key string) (string, bool) {
		if key == "XDG_CONFIG_HOME" {
			return xdg, true
		}
		return lookup(key)
	}
	setupYes(t, env, "", 0, "--yes", "--no-skills", "--provider", "s3", "--bucket", "synthetic-muse", "--aws-profile", "synthetic", "--region", "us-east-1", "--prefix", "synthetic", "--project", f.project, "--apps", "muse", "--muse-home", filepath.Dir(f.root))
	cfg := mustLoadConfig(t, f.home)
	if cfg.MuseCapture == nil || !cfg.MuseCapture.Since.Equal(f.birth.Add(-time.Minute)) || !cfg.NativeAdmissionProtection || !cfg.NativeReplayProtection {
		t.Fatalf("setup did not issue exact window: %+v", cfg.MuseCapture)
	}
	first := *cfg.MuseCapture
	raw, err := os.ReadFile(filepath.Join(xdg, "muse", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var settings map[string]any
	if err := json.Unmarshal(raw, &settings); err != nil || settings["schema_version"] != float64(1) {
		t.Fatalf("generated native settings: %s %v", raw, err)
	}
	env.Now = func() time.Time { return f.observed }
	const id = "00000000-0000-7000-8000-000000000001"
	payload, err := json.Marshal(map[string]any{"hook_event_name": "SessionStart", "session_id": id, "cwd": f.project, "transcript_path": nil, "source": "startup"})
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	if code := runHookCommand([]string{"--harness", "muse"}, bytes.NewReader(payload), &stderr, env); code != 0 || stderr.Len() != 0 {
		t.Fatalf("first setup hook: %d %s", code, &stderr)
	}
	entries, err := os.ReadDir(filepath.Join(f.home, "admission-intents"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("hook before any manual index recovery: %v %v", entries, err)
	}
	view, err := readStatus(env)
	if err != nil || len(view.Apps) != 1 || view.Apps[0].Hooks != "installed" || view.Apps[0].NativeStartup == nil || view.Apps[0].NativeStartup.Pending != 1 || view.Apps[0].CapturedLocally || view.Apps[0].Published || view.Apps[0].ReadBackVerified {
		t.Fatalf("generated setup pending status: %+v %v", view.Apps, err)
	}
	remote := storagetest.NewMemoryStore()
	result, err := collector.Run(t.Context(), f.store, remote, collector.Options{Sources: f.registry, Parsers: f.registry, Decoders: f.registry, MachineID: cfg.MachineID, NativeRoots: cfg.NativeRoots, AcceptSession: cfg.AcceptSession, Now: func() time.Time { return f.observed }, NativeLocatorFactory: func(ctx context.Context, scope collector.NativeLocatorPass) (agentapi.NativeLocatorLookup, func() error, error) {
		inventory, err := discovery.NewNativeInventory(ctx, f.home, scope.Roots, scope.Queries, scope.ReadBudget, builtin.ValidateNativeLocator)
		if err != nil {
			return nil, nil, err
		}
		return inventory, inventory.Close, nil
	}})
	defer result.CloseStagedResults()
	if err != nil || len(result.Errors) != 0 || len(result.Published) != 1 {
		t.Fatalf("normal setup through publication: %+v %v", result, err)
	}
	if _, err := verifyPublicationsWithin(t.Context(), f.home, cfg, env, f.store, remote); err != nil {
		t.Fatal(err)
	}
	view, err = readStatus(env)
	if err != nil || len(view.Apps) != 1 || view.Apps[0].NativeStartup != nil || !view.Apps[0].CapturedLocally || !view.Apps[0].Published || !view.Apps[0].ReadBackVerified {
		t.Fatalf("normal setup published status: %+v %v", view.Apps, err)
	}
	setupYes(t, env, "", 0, "--refresh")
	refreshed := mustLoadConfig(t, f.home)
	if refreshed.MuseCapture == nil || *refreshed.MuseCapture != first || !refreshed.MuseCaptureLastIssuedAt.Equal(cfg.MuseCaptureLastIssuedAt) || !refreshed.NativeReplayProtection {
		t.Fatal("refresh widened admission")
	}
}

func TestMuseSetupPrefencePreservesCommittedFloor(t *testing.T) {
	t.Parallel()
	f := newMuseRootCaptureFixture(t)
	// First enable on an existing installation must pre-fence its old policy,
	// without moving the issuance floor before the transaction commits.
	current := f.cfg
	current.Archive.Enabled = false
	current, err := config.ReconcileMuseCapture(current, f.cfg, f.observed)
	if err != nil {
		t.Fatal(err)
	}
	if err := config.Save(f.home, current); err != nil {
		t.Fatal(err)
	}
	proposed := current
	proposed.Archive.Enabled = true
	proposed, err = config.ReconcileMuseCapture(proposed, current, f.observed.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	current.NativeAdmissionProtection = false
	// Use the normal on-disk inactive protected state; the proposed window is
	// tentative even when pre-fencing is already sticky and becomes a no-op.
	if err := protectSetupWriter(f.home, current, proposed); err != nil {
		t.Fatal(err)
	}
	after := mustLoadConfig(t, f.home)
	if after.MuseCapture != nil || !after.MuseCaptureLastIssuedAt.Equal(f.cfg.MuseCaptureLastIssuedAt) {
		t.Fatal("pre-fence activated prospective authority")
	}
}

func TestMuseSetupFailureRestoresPriorCommittedWindow(t *testing.T) {
	t.Parallel()
	f := newMuseRootCaptureFixture(t)
	env := f.env
	env.MuseRootCaptureQualified = true
	env.Now = func() time.Time { return f.observed }
	fakeSched(env).beforeLoad = func(scheduler.Ref) error { return errors.New("synthetic setup load failure") }
	setupYes(t, env, "", 1, "--yes", "--no-skills", "--provider", "s3", "--bucket", "changed-destination", "--aws-profile", "synthetic", "--region", "us-east-1", "--project", f.project, "--apps", "muse", "--muse-home", filepath.Dir(f.root))
	after := mustLoadConfig(t, f.home)
	if after.MuseCapture == nil || *after.MuseCapture != *f.cfg.MuseCapture || !after.MuseCaptureLastIssuedAt.Equal(f.cfg.MuseCaptureLastIssuedAt) {
		t.Fatal("rollback lost committed window or retained tentative floor")
	}
}

func TestMuseRefreshInstallsReplayFenceWithoutReissuingWindow(t *testing.T) {
	t.Parallel()
	f := newMuseRootCaptureFixture(t)
	if f.cfg.NativeReplayProtection {
		t.Fatal("shared foundation automatically enabled replay")
	}
	env := f.env
	env.MuseRootCaptureQualified = true
	executable, err := env.executable()
	if err != nil {
		t.Fatal(err)
	}
	current := f.cfg
	current.InstalledExecutable = executable
	if err := config.Save(f.home, current); err != nil {
		t.Fatal(err)
	}
	setupYes(t, env, "", 0, "--refresh")
	after := mustLoadConfig(t, f.home)
	if !after.NativeReplayProtection {
		t.Fatal("same-executable refresh did not install replay protection")
	}
	if after.MuseCapture == nil || *after.MuseCapture != *f.cfg.MuseCapture || !after.MuseCaptureLastIssuedAt.Equal(f.cfg.MuseCaptureLastIssuedAt) {
		t.Fatal("refresh reissued admission")
	}
}
