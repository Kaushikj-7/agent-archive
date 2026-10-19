package collector

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	root := archive.NativeRoot{Agent: "muse", Configured: filepath.Dir(path), Canonical: filepath.Dir(path), Revision: reg.NativeBinding.RootRevision}
	reg.NativeImport.Confirmation.RootFingerprint, err = archive.NativeRootFingerprint(root)
	if err != nil {
		t.Fatal(err)
	}
	cfg.NativeRoots = []archive.NativeRoot{root}
	if err := config.Save(store.Home(), cfg); err != nil {
		t.Fatal(err)
	}
	scope := archive.NativeImportBatchScope{BatchID: reg.ImportBatch.Recorded(), StartedAtUTC: at.UTC().Format(time.RFC3339Nano), DestinationID: reg.DestinationID, Filters: archive.ImportBatchFilters{Harnesses: []string{"muse"}, ProjectIDs: []string{}}, ProjectIDs: []string{reg.ProjectID}, Selected: []archive.NativeImportBatchSelection{{Kind: archive.NativeImportOrdinaryRoot, NativeID: reg.NativeSessionID, OriginalTUTC: reg.SessionStartedAt.UTC().Format(time.RFC3339Nano), ProjectID: reg.ProjectID, RootFingerprint: reg.NativeImport.Confirmation.RootFingerprint, RootRevision: reg.NativeBinding.RootRevision, BindingSHA256: reg.NativeImport.Confirmation.SelectedBindingSHA256, Source: *reg.NativeBinding.Checkpoint}}}
	reg.NativeImport.Confirmation.ScopeSHA256, err = archive.NativeImportBatchScopeDigest(t.Context(), scope)
	if err != nil {
		t.Fatal(err)
	}
	confirmation := archive.NativeImportBatchControl{ID: scope.BatchID, StartedAt: at, DestinationID: reg.DestinationID, Filters: scope.Filters, NativeImport: &archive.NativeImportBatchConfirmation{Contract: "muse-historical-import-v1", Instance: reg.NativeImport.Confirmation.BatchInstance, ScopeSHA256: reg.NativeImport.Confirmation.ScopeSHA256, Scope: scope}}
	if err := reg.Validate(); err != nil {
		t.Fatal(err)
	}
	// Arrange an already-owned receipt to isolate retained publication/release.
	// This fixture grants no first-owner authority and does not qualify admission.
	if err := local.Write(filepath.Join(store.Home(), "registrations", reg.ArchiveSessionID+".json"), reg); err != nil {
		t.Fatal(err)
	}
	bundle, err := archive.NewSourceBundle(reg, adapter, filtered, at, []archive.SupplementalEvidence{{Kind: archive.EvidenceKindSkillSnapshot, ObservedAt: at, Provenance: "synthetic-test", Payload: map[string]any{"name": "SyntheticSkill", "snapshot": "historical-import-private-skill-body"}}})
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
	assertInterruptedNativeImportLocalRead(t, f)
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

func TestNativeImportReleasedPrivacySuccessorUsesOnlyRetainedSource(t *testing.T) {
	f := newHistoricalNativeStage(t)
	result, err := Run(t.Context(), f.store, f.remote, f.opts)
	closeStagedControlResult(result)
	if err != nil || len(result.Errors) != 0 {
		t.Fatal("initial typed publication", err, result.Errors)
	}
	before, err := ReadLocalBundle(t.Context(), f.store.Home(), f.reg, f.at, "", f.opts.Sources)
	beforeEvidence, marshalErr := json.Marshal(before.SupplementalEvidence)
	if err != nil || marshalErr != nil || !strings.Contains(string(beforeEvidence), "historical-import-private-skill-body") {
		t.Fatal("missing retained privacy fixture", err)
	}
	cfg, _, err := config.Load(f.store.Home())
	if err != nil {
		t.Fatal(err)
	}
	cfg.SkillEvidence = config.SkillEvidenceNone
	if err := config.Save(f.store.Home(), cfg); err != nil {
		t.Fatal(err)
	}
	f.opts.SkillEvidence = config.SkillEvidenceNone
	f.opts.Now = func() time.Time { return f.at.Add(time.Hour) }
	f.opts.SupplementalEvidence = func(archive.SessionRegistration, time.Time) ([]archive.SupplementalEvidence, error) {
		t.Fatal("retained privacy observed current filesystem evidence")
		return nil, nil
	}
	published := 0
	for range 3 {
		result, err = Run(t.Context(), f.store, f.remote, f.opts)
		closeStagedControlResult(result)
		if err != nil || len(result.Errors) != 0 {
			t.Fatal("retained privacy successor", err, result.Errors)
		}
		published += len(result.Published)
	}
	if published != 1 {
		t.Fatal("privacy successor did not settle once", published)
	}
	after, err := ReadLocalBundle(t.Context(), f.store.Home(), f.reg, f.at.Add(time.Hour), "", f.opts.Sources)
	if err != nil {
		t.Fatal("retained privacy local read", err)
	}
	if len(after.SupplementalEvidence) != 0 || !after.Capture.CapturedAt.Equal(f.at) {
		t.Fatal("privacy successor retained body or changed capture provenance")
	}
	if err := f.store.NativeImportRetainedAuthority(t.Context(), f.reg); err != nil {
		t.Fatal("privacy successor lost original release lineage", err)
	}
	reopened, err := state.Open(f.store.Home())
	if err != nil {
		t.Fatal(err)
	}
	result, err = Run(t.Context(), reopened, f.remote, f.opts)
	closeStagedControlResult(result)
	if err != nil || len(result.Errors) != 0 || len(result.Published) != 0 {
		t.Fatal("privacy successor restart", err, result.Errors)
	}
}

func assertInterruptedNativeImportLocalRead(t *testing.T, f historicalNativeStage) {
	t.Helper()
	// The fixture removed native input before its first publication, and its
	// Sources lookup fails the test if any provider OpenPass is attempted.
	for range 2 {
		bundle, err := ReadLocalBundle(t.Context(), f.store.Home(), f.reg, f.at.Add(time.Hour), "", f.opts.Sources)
		if err != nil {
			t.Fatal("typed ACK local read before release retry", err)
		}
		if bundle.ArchiveSessionID != f.reg.ArchiveSessionID || bundle.NativeSessionID != f.reg.NativeSessionID || !bundle.Capture.CapturedAt.Equal(f.at) {
			t.Fatal("interrupted ACK local read changed retained identity")
		}
	}
	published, err := f.store.LoadPublishedState(f.reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := published.ValidateNativeImportAuthority(t.Context(), f.reg); err == nil {
		t.Fatal("local-read exception granted released publication authority")
	}
	// Exact persisted ACK reads retain their authority when the configured
	// native root disappears. Stage decoding still checks the strong flags.
	cfg, _, err := config.Load(f.store.Home())
	if err != nil {
		t.Fatal(err)
	}
	originalConfig := cfg
	cfg.Paused = true
	cfg.NativeRoots = nil
	if err := config.Save(f.store.Home(), cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadLocalBundle(t.Context(), f.store.Home(), f.reg, f.at, "", f.opts.Sources); err != nil {
		t.Fatal("paused ACK local read required removed native root", err)
	}
	assertInterruptedACKPendingRefusals(t, f)
	stageDirectory := filepath.Join(f.store.Home(), "sessions", f.reg.ArchiveSessionID, "import-stage")
	for _, name := range []string{"release.json", "release-progress.json"} {
		path := filepath.Join(stageDirectory, name)
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("unexpected existing release control", err)
		}
		if err := os.WriteFile(path, []byte(`{"version":999}`), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadLocalBundle(t.Context(), f.store.Home(), f.reg, f.at, "", f.opts.Sources); err == nil {
			t.Fatal("typed ACK excused future release control", name)
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	changed := f.reg
	receipt := *changed.NativeImport
	receipt.Membership.ReceiptID = strings.Repeat("9", 32)
	changed.NativeImport = &receipt
	if _, err := ReadLocalBundle(t.Context(), f.store.Home(), changed, f.at, "", f.opts.Sources); err == nil {
		t.Fatal("typed ACK excused another receipt")
	}
	stagePath := filepath.Join(f.store.Home(), "sessions", f.reg.ArchiveSessionID, "import-stage", "stage.json")
	raw, err := os.ReadFile(stagePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stagePath, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadLocalBundle(t.Context(), f.store.Home(), f.reg, f.at, "", f.opts.Sources); err == nil {
		t.Fatal("typed ACK excused corrupt held stage")
	}
	if err := os.Remove(stagePath); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadLocalBundle(t.Context(), f.store.Home(), f.reg, f.at, "", f.opts.Sources); err == nil {
		t.Fatal("typed ACK excused missing held stage without released lineage")
	}
	if err := os.WriteFile(stagePath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := config.Save(f.store.Home(), originalConfig); err != nil {
		t.Fatal(err)
	}
}

func assertInterruptedACKPendingRefusals(t *testing.T, f historicalNativeStage) {
	t.Helper()
	path := filepath.Join(f.store.Home(), "pending", f.reg.ArchiveSessionID+".json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for key, replacement := range map[string]json.RawMessage{
		"journal_version": json.RawMessage(`999`),
		"source_bytes":    json.RawMessage(`"AAAA"`),
		"metadata_bytes":  json.RawMessage(`"e30="`),
	} {
		var doc map[string]json.RawMessage
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatal(err)
		}
		if _, present := doc[key]; !present {
			t.Fatal("pending refusal fixture field absent", key)
		}
		doc[key] = replacement
		changed, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, changed, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadLocalBundle(t.Context(), f.store.Home(), f.reg, f.at, "", f.opts.Sources); err == nil {
			t.Fatal("ACK local read ignored invalid present pending control", key)
		}
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
}
