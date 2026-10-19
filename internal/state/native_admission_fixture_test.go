package state

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agents/muse"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/destination"
	"github.com/wangjohn/agent-archive/internal/transcriptio"
)

// State-unit locator only revalidates the test selection token. End-to-end CLI
// tests separately exercise the actual shared inventory and complete census.
type admissionUnitLocator struct{}

func (admissionUnitLocator) Locate(context.Context, agentapi.LocatorRequest) (agentapi.LocatorResult, error) {
	return agentapi.LocatorResult{}, errors.New("state unit does not discover")
}

func (admissionUnitLocator) Check(ctx context.Context, id, token string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if id != "00000000-0000-7000-8000-000000000001" || token != "state-unit-selection" {
		return agentapi.ErrInvalidSourceBinding
	}
	return nil
}

type nativeAdmissionFixture struct {
	store     *Store
	candidate NativeAdmissionCandidate
	path      string
	budget    *agentapi.NativeReadBudget
}

func newNativeAdmissionFixture(t *testing.T) nativeAdmissionFixture {
	t.Helper()
	store := newTestStore(t)
	project, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(project, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	rootPath, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const id = "00000000-0000-7000-8000-000000000001"
	path := filepath.Join(rootPath, "2026", "10", "15", id, "session.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join("..", "agents", "muse", "testdata", "synthetic-exec-conversation.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	raw = bytes.ReplaceAll(raw, []byte("/work/synthetic-muse"), []byte(project))
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	birth := time.UnixMicro(1792083855882704).UTC()
	root := archive.NativeRoot{Agent: "muse", Configured: rootPath, Canonical: rootPath, Revision: "one"}
	cfg := config.Config{MachineID: "synthetic", RetentionDays: 90, Harnesses: []string{"muse"}, Storage: destination.Config{Provider: destination.ProviderS3, Bucket: "synthetic-native-admission", Prefix: "synthetic", AWSProfile: "synthetic"}, DestinationSince: birth.Add(-time.Hour), NativeRoots: []archive.NativeRoot{root}, Archive: archive.Config{Enabled: true, SchemaVersion: 1, Projects: []archive.ProjectActivation{{Root: project, ProjectID: archive.ProjectID(project), Included: true, ActivatedAt: birth.Add(-time.Hour)}}}}
	cfg, err = config.ReconcileMuseCapture(cfg, config.Config{}, birth.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := config.Save(store.home, cfg); err != nil {
		t.Fatal(err)
	}
	rootFingerprint, err := archive.NativeRootFingerprint(root)
	if err != nil {
		t.Fatal(err)
	}
	intent := agentapi.AdmissionIntent{Version: 2, Harness: "muse", NativeMode: agentapi.NativeStartupCandidate, NativeSessionID: id, ProjectRoot: project, ObservedCwd: project, DestinationID: cfg.DestinationID(), ObservedAt: time.Now().UTC(), Event: "SessionStart", NativeAdmission: &archive.NativeAdmission{Contract: "muse-root-admission", HookContract: "muse-root-hooks-v1", RootFingerprint: rootFingerprint, PolicyFingerprint: cfg.MuseCapture.PolicyFingerprint, WindowGeneration: cfg.MuseCapture.Generation}}
	return nativeAdmissionFixture{store: store, candidate: NativeAdmissionCandidate{Intent: intent, Root: root, ProjectRoot: project, ProjectID: archive.ProjectID(project)}, path: path, budget: agentapi.NewNativeReadBudget(agentapi.PassReadBudget)}
}

func (f nativeAdmissionFixture) prepare(ctx context.Context) (*PreparedNativeAdmission, error) {
	lookup := admissionUnitLocator{}
	pass, err := (muse.SourceProvider{}).OpenPass(ctx, agentapi.SourceEnvironment{NativeRoot: &f.candidate.Root, NativeLocators: lookup, ReadBudget: f.budget, Files: transcriptio.RootOpener{Root: f.candidate.Root.Canonical}, Policy: transcriptio.OpenPolicy{Root: f.candidate.Root.Canonical, RejectSymlinks: true}})
	if err != nil {
		return nil, err
	}
	limits := agentapi.ReadLimits{RawBytes: 32 << 20, RecordBytes: 1 << 20, FilteredBytes: 8 << 20, ReadBudget: f.budget}
	snapshot, err := pass.Read(ctx, agentapi.SourceRef{Kind: archive.SourceKindFile, Path: f.path, Key: f.candidate.Intent.NativeSessionID}, limits)
	if err != nil {
		return nil, errors.Join(err, pass.Close())
	}
	prepared, err := f.store.PrepareNativeAdmission(ctx, f.candidate, snapshot, snapshot.Input(), muse.Filter{}, agentapi.FilterContext{Filename: f.path, Limits: limits}, lookup, "state-unit-selection", f.budget, pass.Close)
	if err != nil {
		return nil, errors.Join(err, snapshot.Close(), pass.Close())
	}
	return prepared, nil
}

func (f nativeAdmissionFixture) assertReleased(t *testing.T) {
	t.Helper()
	if f.budget.Available() != agentapi.PassReadBudget {
		t.Fatalf("native/output lease leaked: available %d", f.budget.Available())
	}
}
