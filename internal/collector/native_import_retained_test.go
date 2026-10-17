package collector

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

type historicalNativeNoOpen struct {
	agentapi.SourceProvider
	t *testing.T
}

func (p historicalNativeNoOpen) OpenPass(context.Context, agentapi.SourceEnvironment) (agentapi.SourcePass, error) {
	p.t.Fatal("released historical maintenance opened native input")
	return nil, errors.New("native access forbidden")
}

type historicalNativeSources struct{ t *testing.T }

func (s historicalNativeSources) LookupSources(name string) (agentapi.SourceProvider, agentapi.TranscriptFilter, bool) {
	provider, filter, ok := testSources.LookupSources(name)
	return historicalNativeNoOpen{provider, s.t}, filter, ok
}

type historicalNativeReadback struct {
	*storagetest.MemoryStore
	failRelease bool
	sourceReads map[string]int
}

func (s *historicalNativeReadback) GetLimited(ctx context.Context, key string, limit int64) ([]byte, error) {
	if strings.HasSuffix(key, ".gz") {
		s.sourceReads[key]++
		if s.failRelease && s.sourceReads[key] >= 2 {
			return nil, errors.New("synthetic release readback interruption")
		}
	}
	return s.MemoryStore.GetLimited(ctx, key, limit)
}

type historicalNativeStage struct {
	store  *state.Store
	reg    archive.SessionRegistration
	opts   Options
	remote *historicalNativeReadback
	at     time.Time
}

func newHistoricalNativeStage(t *testing.T) historicalNativeStage {
	t.Helper()
	store := newTestStore(t)
	cfg, _, err := config.Load(store.Home())
	if err != nil {
		t.Fatal(err)
	}
	cfg.Archive.Enabled = true
	cfg.DurableStorageProtection = true
	cfg.PublicationCompositionProtection = true
	cfg.ImportStagingProtection = true
	cfg.StagedSelectionProtection = true
	cfg.NativeImportProtection = true
	cfg.ImportedHarnesses = []string{"muse"}
	if err := config.Save(store.Home(), cfg); err != nil {
		t.Fatal(err)
	}
	cfg, _, err = config.Load(store.Home())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join("..", "agents", "muse", "testdata", "synthetic-exec-conversation.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	path := writeTranscript(t, t.TempDir(), "ordinary.jsonl", string(raw))
	at := time.Date(2026, 10, 17, 1, 0, 0, 0, time.UTC)
	filtered, adapter, err := FilterSource(t.Context(), "muse", agentapi.SourceRef{Path: path}, at, testSources)
	if err != nil || len(filtered.SessionIDs) != 1 || filtered.NativeStartAt.IsZero() {
		t.Fatal("synthetic selection", err)
	}
	reg := historicalNativeRegistration(t)
	reg.NativeSessionID = filtered.SessionIDs[0]
	reg.TranscriptPath = path
	reg.SessionStartedAt = filtered.NativeStartAt
	reg.ProjectRoot = "/work/synthetic-muse"
	reg.AdmittedAt, reg.RegisteredAt = at, at
	reg.DestinationID = cfg.DestinationID()
	reg.NativeImport.Confirmation.DestinationID = reg.DestinationID
	reg.NativeBinding.NativeID = reg.NativeSessionID
	reg.NativeBinding.Path = path
	reg.NativeBinding.Cwd = reg.ProjectRoot
	reg.NativeBinding.NativeCreatedAt = reg.SessionStartedAt
	sum := sha256.Sum256(raw)
	reg.NativeBinding.Checkpoint.Boundary = int64(len(raw))
	reg.NativeBinding.Checkpoint.SHA256 = hex.EncodeToString(sum[:])
	reg.NativeImport.Confirmation.SelectedSource = *reg.NativeBinding.Checkpoint
	reg.NativeImport.Confirmation.SelectedBindingSHA256, err = archive.NativeImportBindingDigest(reg.NativeBinding)
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.Validate(); err != nil {
		t.Fatal(err)
	}
	// Arrange an already-owned receipt to isolate retained publication/release.
	// This fixture grants no first-owner authority and does not qualify admission.
	if err := local.Write(filepath.Join(store.Home(), "registrations", reg.ArchiveSessionID+".json"), reg); err != nil {
		t.Fatal(err)
	}
	bundle, err := archive.NewSourceBundle(reg, adapter, filtered, at, nil)
	if err != nil {
		t.Fatal(err)
	}
	reg.StagedImportSHA256, err = store.SaveImportStage(t.Context(), reg, bundle, cfg.EffectiveSkillEvidence())
	if err != nil {
		t.Fatal(err)
	}
	// Complete the already-owned synthetic arrangement with its selected stage.
	if err := local.Write(filepath.Join(store.Home(), "registrations", reg.ArchiveSessionID+".json"), reg); err != nil {
		t.Fatal(err)
	}
	confirmation := struct {
		ID            string `json:"id"`
		DestinationID string `json:"destination_id"`
	}{reg.ImportBatch.Recorded(), reg.DestinationID}
	if err := local.Write(filepath.Join(store.Home(), "imports", reg.ImportBatch.Recorded()+".json"), confirmation); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRequest(reg.ArchiveSessionID, "backfill", at); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	guard, err := local.LockCollectorGuard(store.Home())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(guard.Release)
	opts := Options{Sources: historicalNativeSources{t}, Parsers: testParsers, MachineID: "synthetic", Now: func() time.Time { return at }, AcceptSession: cfg.AcceptSession, SkillEvidence: cfg.EffectiveSkillEvidence(), CollectorGuard: guard, Retry: storage.RetryPolicy{MaxAttempts: 1}}
	return historicalNativeStage{store: store, reg: reg, opts: opts, remote: &historicalNativeReadback{MemoryStore: storagetest.NewMemoryStore(), sourceReads: map[string]int{}}, at: at}
}

func TestNativeImportTypedACKReleaseRetainedParserAndRestart(t *testing.T) {
	f := newHistoricalNativeStage(t)
	f.remote.failRelease = true
	result, err := Run(t.Context(), f.store, f.remote, f.opts)
	closeStagedControlResult(result)
	if err != nil {
		t.Fatal(err)
	}
	published, err := f.store.LoadPublishedState(f.reg.ArchiveSessionID)
	if err != nil || !published.TypedStagedPublished() {
		t.Fatal("typed ACK missing before injected release interruption", err, result.Errors)
	}
	if _, found, err := f.store.LoadImportStage(t.Context(), f.reg); err != nil || !found {
		t.Fatal("interrupted release lost selected payload", err)
	}
	f.remote.failRelease = false
	result, err = Run(t.Context(), f.store, f.remote, f.opts)
	closeStagedControlResult(result)
	if err != nil || len(result.Errors) != 0 {
		t.Fatal("release restart", err, result.Errors)
	}
	if _, found, err := f.store.LoadImportStage(t.Context(), f.reg); err != nil || found {
		t.Fatal("ACKed payload not released", err)
	}
	if _, found, err := f.store.LoadRequest(f.reg.ArchiveSessionID); err != nil || found {
		t.Fatal("covered request not released", err)
	}
	saved, found, err := f.store.LoadRegistration(f.reg.ArchiveSessionID)
	if err != nil || !found || saved.StagedImportSHA256 != f.reg.StagedImportSHA256 {
		t.Fatal("original stage attribution changed", err)
	}
	if _, err := ReadLocalBundle(t.Context(), f.store.Home(), saved, f.at.Add(time.Hour), "", f.opts.Sources); err != nil {
		t.Fatal("retained local read", err)
	}
	if err := os.Remove(filepath.Join(f.store.Home(), "imports", saved.ImportBatch.Recorded()+".json")); err != nil {
		t.Fatal(err)
	}
	f.opts.ParserVersion = "historical-native-parser-upgrade"
	f.opts.Now = func() time.Time { return f.at.Add(time.Hour) }
	result, err = Run(t.Context(), f.store, f.remote, f.opts)
	closeStagedControlResult(result)
	if err != nil || len(result.Errors) != 0 || len(result.Published) != 1 {
		t.Fatal("retained parser successor", err, result.Errors, result.Published)
	}
	metadata := fetchMetadata(t, f.remote, "muse", f.reg.ArchiveSessionID)
	if metadata.Parser.Version != f.opts.ParserVersion || !metadata.CapturedAt.Equal(f.at) {
		t.Fatal("retained parser changed capture age")
	}
	reopened, err := state.Open(f.store.Home())
	if err != nil {
		t.Fatal(err)
	}
	result, err = Run(t.Context(), reopened, f.remote, f.opts)
	closeStagedControlResult(result)
	if err != nil || len(result.Errors) != 0 {
		t.Fatal("retained successor restart", err, result.Errors)
	}
}
