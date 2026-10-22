package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

// Regression: pre-release review, carried over from agent-skills (e371b6a).
func TestStatusAttributesClaudeRecordVersionToVerifiedCapture(t *testing.T) {
	t.Parallel()
	home, userHome, project := t.TempDir(), t.TempDir(), t.TempDir()
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC()
	cfg := config.Config{MachineID: "machine", Storage: credentialsTestConfig(), Harnesses: []string{"claude"}, Archive: archive.Config{Enabled: true, Projects: []archive.ProjectActivation{{Root: project, Included: true, ActivatedAt: at.Add(-time.Hour)}}}}
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	localStore, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(project, "claude.jsonl")
	record := `{"version":"1.0.83","type":"user","uuid":"u1","timestamp":"2026-09-17T18:00:00Z","message":{"role":"user","content":"hello"}}` + "\n"
	if err := os.WriteFile(path, []byte(record), 0600); err != nil {
		t.Fatal(err)
	}
	// The hook has no version source for Claude Code; the transcript does.
	reg := archive.SessionRegistration{ArchiveSessionID: "s", NativeSessionID: "n", ProjectID: "p", ProjectRoot: project, Harness: archive.Harness{Name: "claude"}, TranscriptPath: path, SessionStartedAt: at, RegisteredAt: at}
	if err := localStore.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	remote := storagetest.NewMemoryStore()
	result, err := collector.Run(context.Background(), localStore, remote, collector.Options{Parsers: productionAgents, Sources: productionAgents, MachineID: cfg.MachineID, Now: func() time.Time { return at }})
	if err != nil || len(result.Errors) > 0 {
		t.Fatalf("%+v %v", result, err)
	}
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), at)
	if summary, err := verifyPublications(home, cfg, env, localStore, remote); err != nil || summary.Verified != 1 {
		t.Fatalf("%+v %v", summary, err)
	}
	if err := recordApplicationDiscoveries(home, map[string]applicationDiscovery{"claude": {Installed: true, Version: "1.0.83 (Claude Code)", VersionKind: versionKindCLI, VersionState: "observed"}}, at); err != nil {
		t.Fatal(err)
	}
	view, err := readStatus(env)
	if err != nil {
		t.Fatal(err)
	}
	if view.Apps[0].VersionSupport != "verified_by_capture" || view.Apps[0].VersionSupportReason != "" {
		t.Fatalf("claude version not attributed: %+v", view.Apps[0])
	}
	if err := recordApplicationDiscoveries(home, map[string]applicationDiscovery{"claude": {Installed: true, Version: "1.0.90", VersionKind: versionKindCLI, VersionState: "observed"}}, at); err != nil {
		t.Fatal(err)
	}
	if view, err = readStatus(env); err != nil || view.Apps[0].VersionSupport != "unverified" || view.Apps[0].VersionSupportReason != "no_matching_verified_version" {
		t.Fatalf("%+v %v", view.Apps[0], err)
	}
	var out strings.Builder
	if code := runStatusCommand([]string{"--verbose"}, &out, &out, env); code != 0 || !strings.Contains(out.String(), "Sessions verified so far came from a different Claude Code version.") {
		t.Fatalf("exit %d: %s", code, out.String())
	}
}

// Regression: pre-release review, carried over from agent-skills (e371b6a).
func TestStatusSurvivesCorruptApplicationVersions(t *testing.T) {
	t.Parallel()
	home, userHome := t.TempDir(), t.TempDir()
	at := time.Now().UTC()
	cfg := config.Config{MachineID: "machine", Storage: credentialsTestConfig(), Harnesses: []string{"codex"}, Archive: archive.Config{Enabled: true}}
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(applicationDiscoveriesPath(home), []byte("{not json"), 0600); err != nil {
		t.Fatal(err)
	}
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), at)
	view, err := readStatus(env)
	if err != nil {
		t.Fatalf("corrupt advisory file failed status: %v", err)
	}
	if !strings.Contains(strings.Join(view.Warnings, "\n"), "application-versions.json") {
		t.Fatalf("warnings %v", view.Warnings)
	}
	if app := view.Apps[0]; app.VersionState != "unknown" || app.VersionSupport != "unknown" || app.Installed {
		t.Fatalf("%+v", app)
	}
	var out strings.Builder
	if code := runStatusCommand([]string{"--verbose"}, &out, &out, env); code != 0 || !strings.Contains(out.String(), "! Installed versions could not be read") {
		t.Fatalf("exit %d: %s", code, out.String())
	}
}
