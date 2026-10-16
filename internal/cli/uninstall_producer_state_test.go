package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/capture"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/discovery"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/state"
)

func createDiscoveryCleanupState(t *testing.T) (string, Env) {
	t.Helper()
	home, userHome, project := t.TempDir(), t.TempDir(), t.TempDir()
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), now)
	env.AWSProfiles = func() ([]AWSProfile, error) { return []AWSProfile{{Name: "fixture", Region: "us-east-1"}}, nil }
	setupYes(t, env, "", 0, "--yes", "--provider", "s3", "--bucket", "fixture-bucket", "--aws-profile", "fixture", "--project", project, "--apps", "codex", "--codex-discovery", "off", "--codex-capture-scope", "included-projects")
	cfg := mustLoadConfig(t, home)
	previous := cfg
	cfg.CodexCapture = &config.CodexCaptureConfig{Scope: config.CodexAllProjects}
	cfg.Discovery = &config.DiscoveryConfig{Enabled: true, CodexHomes: []string{t.TempDir()}}
	must(t, config.ReconcileDiscovery(&cfg, previous, now))
	must(t, config.Save(home, cfg))
	store, err := state.Open(home)
	must(t, err)
	_, err = discovery.Run(context.Background(), store, cfg, discovery.Options{Sources: registryFor(env), Now: func() time.Time { return now }})
	must(t, err)
	unlock, err := local.NamedLock(home, "hooks.lock")
	must(t, err)
	payload := map[string]any{"hook_event_name": "SessionStart", "source": "startup", "session_id": "cleanup-control", "cwd": project}
	decoder, found := env.agentRegistry().LookupDecoder("codex")
	if !found {
		t.Fatal("missing production Codex decoder")
	}
	batch, err := decoder.Decode(context.Background(), agentapi.HookInput{Payload: payload, ObservedAt: now.Add(time.Second)})
	must(t, err)
	err = capture.HandleBatch(home, "codex", batch, now.Add(time.Second))
	unlock()
	must(t, err)
	must(t, capture.ReplayAdmissionIntents(home, now.Add(2*time.Second), env.agentRegistry()))
	names := []string{"discovery-health.json", "discovery-catalog.lock", "admission-replay-cursor.json"}
	for _, name := range names {
		if _, err := os.Stat(filepath.Join(home, name)); err != nil {
			t.Fatalf("producer did not create %s: %v", name, err)
		}
	}
	return home, env
}

func TestUninstallDiscoveryProducerState(t *testing.T) {
	for _, mode := range []string{"delete", "ordinary", "unrelated"} {
		t.Run(mode, func(t *testing.T) {
			home, env := createDiscoveryCleanupState(t)
			names := []string{"discovery-health.json", "discovery-catalog.lock", "admission-replay-cursor.json"}
			before := map[string][]byte{}
			for _, name := range names {
				raw, err := os.ReadFile(filepath.Join(home, name))
				must(t, err)
				before[name] = raw
			}
			unrelated := []string{"my-notes.txt", "discovery-catalog.lock.bak", "discovery-health.json.bak", "admission-replay-cursor.json.bak"}
			if mode == "unrelated" {
				for _, name := range unrelated {
					must(t, os.WriteFile(filepath.Join(home, name), []byte("user-owned"), 0600))
				}
			}
			args := []string{"uninstall", "--yes"}
			if mode != "ordinary" {
				args = append(args, "--delete-local-data")
			}
			var out, stderr bytes.Buffer
			code := Run(args, nil, &out, &stderr, env)
			t.Logf("uninstall exit=%d output=%s stderr=%s", code, &out, &stderr)
			expected := 0
			if mode == "unrelated" {
				expected = 1
			}
			if code != expected {
				t.Errorf("uninstall exit=%d want %d", code, expected)
			}
			for _, name := range names {
				raw, err := os.ReadFile(filepath.Join(home, name))
				if mode == "ordinary" {
					if err != nil || !bytes.Equal(raw, before[name]) {
						t.Fatalf("ordinary uninstall changed %s: %v", name, err)
					}
				} else if !os.IsNotExist(err) {
					t.Errorf("owned file remains %s: %v", name, err)
				}
			}
			if mode == "delete" {
				if _, err := os.Stat(home); !os.IsNotExist(err) {
					t.Errorf("owned data home remains: %v", err)
				}
			}
			if mode == "unrelated" {
				for _, name := range unrelated {
					raw, err := os.ReadFile(filepath.Join(home, name))
					if err != nil || string(raw) != "user-owned" {
						t.Errorf("unrelated file changed %s: %v", name, err)
					}
				}
			}
		})
	}
}
