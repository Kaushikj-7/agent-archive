package discovery

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agents/builtin"
	"github.com/wangjohn/agent-archive/internal/agents/muse"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

type collectorInventoryObserver struct {
	*NativeInventory
	selected map[string]bool
}

func (l collectorInventoryObserver) Locate(ctx context.Context, q agentapi.LocatorRequest) (agentapi.LocatorResult, error) {
	r, e := l.NativeInventory.Locate(ctx, q)
	if r.Status == agentapi.LocatorFound {
		l.selected[q.NativeID] = true
	}
	return r, e
}

func TestNativeInventoryActualCollectorFairnessAcrossRequestsTimeAndStop(t *testing.T) {
	// Sequential: bounded affected fixture has >256 owners and real pass state writes.
	home, root, raw := inventoryFixture(t)
	if err := os.Chmod(home, 0700); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// Direct JSON is fixture construction only. The actual collector loads the
	// immutable already-admitted owners; this test never creates consent via lookup.
	for i := range 257 {
		id := inventoryMeasurementID(i)
		inventoryFile(t, root, "2026/10/15", id, inventoryMeasurementRaw(t, raw, id))
		reg := archive.SessionRegistration{ArchiveSessionID: fmt.Sprintf("owner-%03d", i), NativeSessionID: id, Harness: archive.Harness{Name: "muse"}, ProjectID: "synthetic-project", ProjectRoot: "/work/synthetic-muse", SessionStartedAt: start, RegisteredAt: start}
		encoded, _ := json.Marshal(reg)
		if err = os.WriteFile(filepath.Join(home, "registrations", reg.ArchiveSessionID+".json"), encoded, 0600); err != nil {
			t.Fatal(err)
		}
		if i%64 == 0 {
			if err = store.SaveRequest(reg.ArchiveSessionID, "stop", start.Add(time.Duration(i)*time.Second)); err != nil {
				t.Fatal(err)
			}
		}
	}
	sum := sha256.Sum256(raw)
	bound := archive.SessionRegistration{ArchiveSessionID: "owner-100-bound", NativeSessionID: inventoryOwner, Harness: archive.Harness{Name: "muse"}, ProjectID: "synthetic-project", ProjectRoot: "/work/synthetic-muse", SessionStartedAt: start, RegisteredAt: start, TranscriptPath: filepath.Join(root.Canonical, "2026", "10", "15", inventoryOwner, "session.jsonl")}
	bound.NativeBinding = &archive.NativeSourceBinding{Agent: "muse", Version: 2, NativeID: inventoryOwner, StreamID: inventoryOwner, Cwd: "/work/synthetic-muse", NativeCreatedAt: time.UnixMicro(1792083855882704).UTC(), Path: bound.TranscriptPath, RootRevision: root.Revision, Checkpoint: &archive.NativeCheckpoint{Version: 1, Domain: "muse-ordinary", Algorithm: "sha256", Boundary: int64(len(raw)), SHA256: hex.EncodeToString(sum[:])}}
	encoded, _ := json.Marshal(bound)
	if err = os.WriteFile(filepath.Join(home, "registrations", bound.ArchiveSessionID+".json"), encoded, 0600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{MachineID: "synthetic", NativeRoots: []archive.NativeRoot{root}}
	if err = config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	selected := map[string]bool{}
	registries := builtin.NewBuiltins()
	tick := 0
	stopCalls := 0
	var current *NativeInventory
	opts := collector.Options{Sources: registries, Parsers: registries, MachineID: "synthetic", NativeRoots: cfg.NativeRoots, SkipSessionIndexRecovery: true, RepoKey: func(string) string { return "" }, NativeLocatorFactory: func(ctx context.Context, pass collector.NativeLocatorPass) (agentapi.NativeLocatorLookup, func() error, error) {
		l, e := NewNativeInventory(ctx, pass.Store.Home(), pass.Roots, pass.Queries, pass.ReadBudget, muse.ValidateNativeLocator)
		if e != nil {
			return nil, nil, e
		}
		if tick == 0 {
			l.remaining = Budget/4 + time.Millisecond
		}
		current = l
		return collectorInventoryObserver{NativeInventory: l, selected: selected}, l.Close, nil
	}, Stop: func() bool { stopCalls++; return tick >= 2 && stopCalls > 2 }}
	remote := storagetest.NewMemoryStore()
	for tick = 0; tick < 8 && len(selected) < 257; tick++ {
		stopCalls = 0
		result, err := collector.Run(t.Context(), store, remote, opts)
		result.CloseStagedResults()
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Published) != 0 {
			t.Fatal("production Muse capture fence opened")
		}
		if selected[bound.NativeSessionID] && current.Counts().Probes > HeaderProbes {
			t.Fatal("bound owner added inventory probes")
		}
		t.Logf("actual Run tick=%d selected_union=%d counts=%+v stop_checks=%d", tick, len(selected), current.Counts(), stopCalls)
	}
	if len(selected) != 257 {
		t.Fatalf("mixed request/no-request owners starved: %d", len(selected))
	}
	for _, id := range []string{"owner-000", "owner-256"} {
		reg, found, err := store.LoadRegistration(id)
		if err != nil || !found || reg.TranscriptPath != "" || reg.NativeBinding != nil {
			t.Fatal("fenced production capture mutated owner", id, reg, err)
		}
	}
}

// Query-only churn uses a physically stable native tree and fresh collector processes.
func TestNativeInventoryCollectorQueryArrivalProcessStep(t *testing.T) {
	home := os.Getenv("AA_NATIVE_INVENTORY_CHURN_HOME")
	if home == "" {
		t.Skip("fresh-process collector query-arrival helper")
	}
	rootPath := os.Getenv("AA_NATIVE_INVENTORY_CHURN_ROOT")
	root := archive.NativeRoot{Agent: "muse", Configured: rootPath, Canonical: rootPath, Revision: "one"}
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	selected := map[string]bool{}
	var current *NativeInventory
	registries := builtin.NewBuiltins()
	opts := collector.Options{Sources: registries, Parsers: registries, MachineID: "synthetic", NativeRoots: []archive.NativeRoot{root}, SkipSessionIndexRecovery: true, RepoKey: func(string) string { return "" }, NativeLocatorFactory: func(ctx context.Context, pass collector.NativeLocatorPass) (agentapi.NativeLocatorLookup, func() error, error) {
		l, e := NewNativeInventory(ctx, pass.Store.Home(), pass.Roots, pass.Queries, pass.ReadBudget, muse.ValidateNativeLocator)
		if e != nil {
			return nil, nil, e
		}
		current = l
		return collectorInventoryObserver{NativeInventory: l, selected: selected}, l.Close, nil
	}}
	result, err := collector.Run(t.Context(), store, storagetest.NewMemoryStore(), opts)
	result.CloseStagedResults()
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Published) != 0 || current == nil {
		t.Fatal("invalid fenced collector run")
	}
	encoded, err := json.Marshal(struct {
		Selected map[string]bool       `json:"Selected"`
		Counts   NativeInventoryCounts `json:"Counts"`
	}{selected, current.Counts()})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(home, "churn-result.json"), encoded, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestNativeInventoryCollectorQueryArrivalsAndShrinkProgress(t *testing.T) {
	home, root, raw := inventoryFixture(t)
	if err := os.Chmod(home, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := state.Open(home); err != nil {
		t.Fatal(err)
	}
	for i := range 32 {
		id := inventoryMeasurementID(i)
		inventoryFile(t, root, "2026/10/15", id, inventoryMeasurementRaw(t, raw, id))
	}
	for i := range 6000 {
		if err := os.WriteFile(filepath.Join(root.Canonical, "2026", "10", "15", fmt.Sprintf("unrelated-%05d", i)), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for tick := range 36 {
		at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		reg := archive.SessionRegistration{ArchiveSessionID: fmt.Sprintf("arrival-%03d", tick), NativeSessionID: inventoryMeasurementID(tick), Harness: archive.Harness{Name: "muse"}, ProjectID: "synthetic-project", ProjectRoot: "/work/synthetic-muse", SessionStartedAt: at, RegisteredAt: at}
		encoded, _ := json.Marshal(reg)
		if tick < 24 {
			if err = os.WriteFile(filepath.Join(home, "registrations", reg.ArchiveSessionID+".json"), encoded, 0600); err != nil {
				t.Fatal(err)
			}
		}
		if tick == 6 {
			if err = os.Remove(filepath.Join(home, "registrations", "arrival-004.json")); err != nil {
				t.Fatal(err)
			}
			boundID := inventoryMeasurementID(5)
			bound := reg
			bound.ArchiveSessionID = "arrival-005"
			bound.NativeSessionID = boundID
			bound.TranscriptPath = filepath.Join(root.Canonical, "2026", "10", "15", boundID, "session.jsonl")
			sum := sha256.Sum256(inventoryMeasurementRaw(t, raw, boundID))
			bound.NativeBinding = &archive.NativeSourceBinding{Agent: "muse", Version: 2, NativeID: boundID, StreamID: boundID, Cwd: "/work/synthetic-muse", NativeCreatedAt: time.UnixMicro(1792083855882704).UTC(), Path: bound.TranscriptPath, RootRevision: root.Revision, Checkpoint: &archive.NativeCheckpoint{Version: 1, Domain: "muse-ordinary", Algorithm: "sha256", Boundary: int64(len(raw)), SHA256: hex.EncodeToString(sum[:])}}
			encoded, _ = json.Marshal(bound)
			if err = os.WriteFile(filepath.Join(home, "registrations", bound.ArchiveSessionID+".json"), encoded, 0600); err != nil {
				t.Fatal(err)
			}
		}
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		cmd := exec.CommandContext(ctx, exe, "-test.run=^TestNativeInventoryCollectorQueryArrivalProcessStep$", "-test.count=1")
		cmd.Env = append(os.Environ(), "AA_NATIVE_INVENTORY_CHURN_HOME="+home, "AA_NATIVE_INVENTORY_CHURN_ROOT="+root.Canonical)
		out, err := cmd.CombinedOutput()
		cancel()
		if err != nil {
			t.Fatalf("tick%d: %v %s", tick, err, out)
		}
		encoded, err = os.ReadFile(filepath.Join(home, "churn-result.json"))
		if err != nil {
			t.Fatal(err)
		}
		var result struct {
			Selected map[string]bool       `json:"Selected"`
			Counts   NativeInventoryCounts `json:"Counts"`
		}
		if err = json.Unmarshal(encoded, &result); err != nil {
			t.Fatal(err)
		}
		for id := range result.Selected {
			seen[id] = true
		}
		t.Logf("tick=%d pid=%d selected=%d counts=%+v", tick, cmd.Process.Pid, len(result.Selected), result.Counts)
		if tick < 3 && len(result.Selected) != 0 {
			t.Fatal("newcomer inherited incomplete cohort pages")
		}
		if tick == 4 && !seen[inventoryMeasurementID(0)] {
			t.Fatal("oldest owner starved under ongoing arrivals")
		}
		if tick >= 6 && (result.Selected[inventoryMeasurementID(4)] || result.Selected[inventoryMeasurementID(5)]) {
			t.Fatal("removed/bound historical owner selected")
		}
	}
	for i := range 24 {
		if i != 4 && i != 5 && !seen[inventoryMeasurementID(i)] {
			t.Fatalf("arrival %d never progressed", i)
		}
	}
}
