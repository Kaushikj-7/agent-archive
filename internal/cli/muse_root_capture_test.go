package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agents/builtin"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/destination"
	"github.com/wangjohn/agent-archive/internal/discovery"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func TestMuseRootHookCandidatePublishesThroughSharedInventory(t *testing.T) {
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
	root, err := filepath.EvalSymlinks(t.TempDir())
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
	cfg := config.Config{MachineID: "00000000000000000000000000000001", RetentionDays: 90, Harnesses: []string{"muse"}, Storage: destination.Config{Provider: destination.ProviderS3, Bucket: "synthetic-muse", Prefix: "synthetic", AWSProfile: "synthetic"}, DestinationSince: birth.Add(-time.Hour), NativeRoots: []archive.NativeRoot{{Agent: "muse", Configured: root, Canonical: root, Revision: "one"}}, Archive: archive.Config{Enabled: true, SchemaVersion: 1, Projects: []archive.ProjectActivation{{Root: project, ProjectID: archive.ProjectID(project), Included: true, ActivatedAt: birth.Add(-time.Hour)}}}}
	cfg, err = config.ReconcileMuseCapture(cfg, config.Config{}, birth.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecoverSessionIndexScheduled(t.Context(), state.SessionIndexRecoverySlice); err != nil {
		t.Fatal(err)
	}
	registry := builtin.NewBuiltins()
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), observed)
	env.Agents = registry
	env.LookupEnv = func(key string) (string, bool) {
		if key == "MUSE_SESSION_ID" || key == "MUSE_THREAD_ID" {
			return id, true
		}
		return "", false
	}
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
	if registrations[0].SessionStartedAt != birth {
		t.Fatalf("original native time replaced: %v", registrations[0].SessionStartedAt)
	}
	published, err := store.LoadPublishedState(registrations[0].ArchiveSessionID)
	if err != nil || published == nil {
		t.Fatalf("published readback missing: %v", err)
	}
	if strings.Contains(stderr.String(), "PRIVATE") {
		t.Fatal("native input leaked")
	}
}
