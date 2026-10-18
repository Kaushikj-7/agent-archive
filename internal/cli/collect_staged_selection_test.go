package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/agents/builtin"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/catalog"
	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/destination"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

type stagedSyncFixture struct {
	home   string
	env    Env
	store  *state.Store
	remote *storagetest.MemoryStore
	reg    archive.SessionRegistration
	bundle archive.SourceBundle
}

// Prepare through the actual source-selection, staging and admission owners.
// Publication then runs through Run's sync command, without an injected guard.
func newStagedSyncFixture(t *testing.T) stagedSyncFixture {
	t.Helper()
	home, project := t.TempDir(), t.TempDir()
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	setUpTestConfig(t, home, project, at.Add(-24*time.Hour))
	cfg := mustLoadConfig(t, home)
	cfg.ImportedHarnesses = []string{"claude"}
	cfg.DurableStorageProtection = true
	cfg.PublicationCompositionProtection = true
	cfg.ImportStagingProtection = true
	cfg.StagedSelectionProtection = true
	must(t, config.Save(home, cfg))
	cfg = mustLoadConfig(t, home)
	if cfg.SchemaVersion != 13 || !cfg.SupportsStagedSelection() {
		t.Fatal("fixture did not establish the supported legacy staged floor")
	}
	path := filepath.Join(project, "native.jsonl")
	must(t, os.WriteFile(path, []byte(`{"type":"assistant","sessionId":"synthetic-native","timestamp":"2026-10-01T11:01:00Z","message":{"role":"assistant","content":"Synthetic frozen stage."}}`+"\n"), 0600))
	store, err := state.Open(home)
	must(t, err)
	id, _, err := store.EnsureArchiveSessionID(agentmeta.SessionKey{Agent: agentmeta.Claude, NativeID: "synthetic-native"})
	must(t, err)
	reg := archive.SessionRegistration{ArchiveSessionID: id, NativeSessionID: "synthetic-native", ProjectID: archive.ProjectID(project), ProjectRoot: project, Harness: archive.Harness{Name: "claude"}, TranscriptPath: path, SessionStartedAt: at.Add(-time.Hour), RegisteredAt: at, AdmittedAt: at, Origin: archive.SessionOriginImport, ImportBatch: archive.NewImportBatch("synthetic-stage"), DestinationID: cfg.DestinationID()}
	env := testEnv(t, home, at)
	remote := storagetest.NewMemoryStore()
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return remote, nil }
	bundle, preserved, _, err := collector.ReadImportSourceSelection(t.Context(), reg, at, collector.Options{Sources: registryFor(env), Parsers: parsersFor(env), MachineID: cfg.MachineID, Now: env.Now, SkillEvidence: cfg.EffectiveSkillEvidence()})
	must(t, err)
	if len(preserved) != 0 || len(bundle.NativeRecords) != 1 {
		t.Fatal("fixture did not select one ordinary native record")
	}
	reg.StagedImportSHA256, err = store.SaveImportStage(t.Context(), reg, bundle, cfg.EffectiveSkillEvidence())
	must(t, err)
	stage, found, err := store.LoadImportStage(t.Context(), reg)
	if err != nil || !found || !stage.TypedSelection || stage.Version != 2 || stage.SHA256 != reg.StagedImportSHA256 {
		t.Fatalf("fixture did not freeze supported typed evidence: %v %v", found, err)
	}
	must(t, store.SaveRegistration(reg))
	must(t, store.SaveRequest(id, "backfill", at))
	confirmation := struct {
		ID            string `json:"id"`
		DestinationID string `json:"destination_id"`
	}{reg.ImportBatch.Recorded(), cfg.DestinationID()}
	must(t, local.Write(filepath.Join(home, "imports", reg.ImportBatch.Recorded()+".json"), confirmation))
	must(t, os.Remove(path))
	return stagedSyncFixture{home: home, env: env, store: store, remote: remote, reg: reg, bundle: bundle}
}

func (f stagedSyncFixture) sync(t *testing.T) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := Run([]string{"sync"}, nil, &out, &errOut, f.env)
	return code, out.String(), errOut.String()
}

func (f stagedSyncFixture) assertPublished(t *testing.T) {
	t.Helper()
	key := "sessions/claude/" + f.reg.ArchiveSessionID + "/metadata.json"
	metadata, err := reader.ReadMetadata(t.Context(), f.remote, key)
	must(t, err)
	bundle, err := reader.LoadSource(t.Context(), f.remote, metadata, reader.Limits{})
	must(t, err)
	if !reflect.DeepEqual(bundle.NativeRecords, f.bundle.NativeRecords) || !bundle.Capture.CapturedAt.Equal(f.bundle.Capture.CapturedAt) {
		t.Fatal("publication changed frozen native evidence or capture time")
	}
	published, err := f.store.LoadPublishedState(f.reg.ArchiveSessionID)
	must(t, err)
	if !published.Summary().Published {
		t.Fatal("remote readback did not establish local publication")
	}
	for _, directory := range []string{"pending", "requests"} {
		if _, err := os.Stat(filepath.Join(f.home, directory, f.reg.ArchiveSessionID+".json")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("completed %s retained: %v", directory, err)
		}
	}
	if _, found, err := f.store.LoadImportStage(t.Context(), f.reg); err != nil || found {
		t.Fatalf("verified stage retained: %v %v", found, err)
	}
	assertCollectorUnlocked(t, f.home)
}

func assertCollectorUnlocked(t *testing.T, home string) {
	t.Helper()
	if _, found := readCollectorLockRecord(home); found {
		t.Fatal("collector holder record leaked")
	}
	unlock, err := local.Lock(home)
	must(t, err)
	unlock()
}

func TestSyncPublishesFrozenLegacyStageAfterNativeDeletion(t *testing.T) {
	t.Parallel()
	f := newStagedSyncFixture(t)
	f.forbidNativeReads(t)
	if code, out, errOut := f.sync(t); code != 0 || !strings.Contains(out, "1 published") {
		t.Fatalf("supported frozen-stage sync failed: exit=%d stdout=%s stderr=%s", code, out, errOut)
	}
	f.assertPublished(t)
}

// Block every native content operation and refilter after staging. Keeping the
// real adapter's version cannot confer permission to replace selected bytes.
type frozenStageSource struct {
	agentapi.SourceProvider
	t *testing.T
}

func (s frozenStageSource) OpenPass(ctx context.Context, env agentapi.SourceEnvironment) (agentapi.SourcePass, error) {
	p, err := s.SourceProvider.OpenPass(ctx, env)
	if err != nil {
		return nil, err
	}
	return frozenStagePass{SourcePass: p, t: s.t}, nil
}

type frozenStagePass struct {
	agentapi.SourcePass
	t *testing.T
}

func (p frozenStagePass) Read(context.Context, agentapi.SourceRef, agentapi.ReadLimits) (agentapi.SourceSnapshot, error) {
	p.t.Error("frozen publication attempted a native source read")
	return nil, errors.New("native evidence deleted")
}

type frozenStageFilter struct {
	agentapi.TranscriptFilter
	t *testing.T
}

func (f frozenStageFilter) Filter(context.Context, agentapi.NativeInput, agentapi.FilterContext) (archive.FilteredTranscript, error) {
	f.t.Error("frozen publication attempted native filtering")
	return archive.FilteredTranscript{}, state.ErrReadySelectionFrozen
}

func (f frozenStageFilter) Refilter(context.Context, archive.SourceBundle, time.Time) (archive.FilteredTranscript, error) {
	f.t.Error("frozen publication attempted refiltering")
	return archive.FilteredTranscript{}, state.ErrReadySelectionFrozen
}

func (f frozenStageFilter) RefilterBounded(context.Context, archive.SourceBundle, time.Time, agentapi.ReadLimits) (archive.FilteredTranscript, error) {
	f.t.Error("frozen publication attempted bounded refiltering")
	return archive.FilteredTranscript{}, state.ErrReadySelectionFrozen
}

func (f *stagedSyncFixture) forbidNativeReads(t *testing.T) {
	t.Helper()
	base := f.env.agentRegistry()
	var bindings []builtin.Integration
	for _, d := range base.Catalog().All() {
		b, _ := base.Lookup(string(d.ID))
		b.Descriptor = agentmeta.Descriptor{ID: d.ID}
		if d.ID == agentmeta.Claude {
			b.Sources = frozenStageSource{SourceProvider: b.Sources, t: t}
			b.Filter = frozenStageFilter{TranscriptFilter: b.Filter, t: t}
		}
		bindings = append(bindings, b)
	}
	registry, err := builtin.New(base.Catalog(), bindings)
	must(t, err)
	f.env.Agents = registry
}

// A committed upload whose acknowledgement is lost must keep its exact pending
// selection and recover through a fresh CLI pass and a fresh actual lock guard.
type stagedSyncLostAck struct {
	*storagetest.MemoryStore
	metadataKey string
	lost        bool
}

func (s *stagedSyncLostAck) Put(ctx context.Context, key string, body []byte) error {
	if err := s.MemoryStore.Put(ctx, key, body); err != nil {
		return err
	}
	if key == s.metadataKey {
		s.lost = true
		return errors.New("synthetic lost metadata acknowledgement")
	}
	return nil
}

func TestSyncRecoversExactStagedSelectionAfterLostAcknowledgement(t *testing.T) {
	t.Parallel()
	f := newStagedSyncFixture(t)
	f.forbidNativeReads(t)
	remote := &stagedSyncLostAck{MemoryStore: f.remote, metadataKey: "sessions/claude/" + f.reg.ArchiveSessionID + "/metadata.json"}
	f.env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return remote, nil }
	if code, _, errOut := f.sync(t); code != 1 || !remote.lost || !strings.Contains(errOut, "lost metadata acknowledgement") {
		t.Fatalf("lost acknowledgement was not retained: exit=%d lost=%v stderr=%s", code, remote.lost, errOut)
	}
	pending, found, err := f.store.LoadPending(f.reg.ArchiveSessionID)
	if err != nil || !found || pending.StagedProof() == nil || pending.StagedProof().StageSHA256 != f.reg.StagedImportSHA256 {
		t.Fatalf("exact staged pending selection missing: %v %v", found, err)
	}
	proof := *pending.StagedProof()
	selected := bytes.Clone(pending.MetadataBytes)
	raw, err := f.remote.Get(t.Context(), remote.metadataKey)
	must(t, err)
	if !bytes.Equal(raw, selected) {
		t.Fatal("lost acknowledgement did not follow the exact remote commit")
	}
	assertCollectorUnlocked(t, f.home)
	// Reopen local state and remove the fault, as a restarted sync would.
	f.store, err = state.Open(f.home)
	must(t, err)
	pending, found, err = f.store.LoadPending(f.reg.ArchiveSessionID)
	if err != nil || !found || pending.StagedProof() == nil || *pending.StagedProof() != proof || !bytes.Equal(pending.MetadataBytes, selected) {
		t.Fatalf("restart changed the pending selection: %v %v", found, err)
	}
	f.env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return f.remote, nil }
	if code, _, errOut := f.sync(t); code != 0 {
		t.Fatalf("exact staged restart failed: exit=%d stderr=%s", code, errOut)
	}
	raw, err = f.remote.Get(t.Context(), remote.metadataKey)
	must(t, err)
	if !bytes.Equal(raw, selected) {
		t.Fatal("restart replaced selected metadata")
	}
	f.assertPublished(t)
}

func TestSyncStagedSelectionReleasesLockAfterPreflightFailure(t *testing.T) {
	t.Parallel()
	f := newStagedSyncFixture(t)
	f.env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return nil, context.Canceled }
	stagePath := filepath.Join(f.home, "sessions", f.reg.ArchiveSessionID, "import-stage", "stage.json")
	before, err := os.ReadFile(stagePath)
	must(t, err)
	if code, _, errOut := f.sync(t); code != 1 || !strings.Contains(errOut, context.Canceled.Error()) {
		t.Fatalf("preflight cancellation not reported: exit=%d stderr=%s", code, errOut)
	}
	assertCollectorUnlocked(t, f.home)
	after, err := os.ReadFile(stagePath)
	must(t, err)
	if !bytes.Equal(before, after) {
		t.Fatal("preflight failure changed staged evidence")
	}
	f.env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return f.remote, nil }
	if code, _, errOut := f.sync(t); code != 0 {
		t.Fatalf("retry after cancellation failed: exit=%d stderr=%s", code, errOut)
	}
	f.assertPublished(t)
}

func TestSyncStagedSelectionKeepsBusyHolderAndQueuedWork(t *testing.T) {
	t.Parallel()
	f := newStagedSyncFixture(t)
	unlock, err := lockCollector(f.home, "backfill import", f.env.now())
	must(t, err)
	t.Cleanup(func() {
		if unlock != nil {
			unlock()
		}
	})
	before, found := readCollectorLockRecord(f.home)
	if !found {
		t.Fatal("busy holder record missing")
	}
	if code, _, errOut := f.sync(t); code != 1 || !strings.Contains(errOut, "agent-archive backfill import (process ") || !strings.Contains(errOut, "holds the collector lock; retry when it finishes") {
		t.Fatalf("sync did not report the actual busy holder: exit=%d stderr=%s", code, errOut)
	}
	var out, errOut bytes.Buffer
	if code := Run([]string{"_collect"}, nil, &out, &errOut, f.env); code != 0 || out.Len() != 0 || errOut.Len() != 0 {
		t.Fatalf("busy scheduled collection did not stay quiet: exit=%d stdout=%s stderr=%s", code, &out, &errOut)
	}
	if after, found := readCollectorLockRecord(f.home); !found || before != after {
		t.Fatal("busy pass changed the real holder record")
	}
	if _, found, err := f.store.LoadRequest(f.reg.ArchiveSessionID); err != nil || !found {
		t.Fatalf("busy pass lost queued work: %v %v", found, err)
	}
	if objects, err := f.remote.List(t.Context(), ""); err != nil || len(objects) != 0 {
		t.Fatalf("busy pass wrote to storage: %v", err)
	}
	unlock()
	unlock = nil
	if code, _, errOut := f.sync(t); code != 0 {
		t.Fatalf("retry after busy holder failed: exit=%d stderr=%s", code, errOut)
	}
	f.assertPublished(t)
}

func TestSyncRetainsOrdinaryLegacyAndCatalogPublication(t *testing.T) {
	t.Parallel()
	for _, format := range []destination.ArchiveFormat{destination.FormatLegacy, destination.FormatCatalogV4} {
		t.Run(string(format), func(t *testing.T) {
			t.Parallel()
			home, env, raw := collectFixture(t, time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC))
			cfg := mustLoadConfig(t, home)
			cfg.Storage.ArchiveFormat = format
			must(t, config.Save(home, cfg))
			cfg = mustLoadConfig(t, home)
			var remote storage.ObjectStore = raw
			if format == destination.FormatCatalogV4 {
				wrapped, err := catalog.WrapConfigured(&qualifiedCLIStore{raw}, cfg.Storage)
				must(t, err)
				remote = wrapped
			}
			env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return remote, nil }
			reg := theRegistration(t, home)
			var out, errOut bytes.Buffer
			if code := Run([]string{"sync"}, nil, &out, &errOut, env); code != 0 || !strings.Contains(out.String(), "1 published") {
				t.Fatalf("ordinary %s publication failed: exit=%d stdout=%s stderr=%s", format, code, &out, &errOut)
			}
			metadata, err := reader.ReadMetadata(t.Context(), remote, "sessions/codex/"+reg.ArchiveSessionID+"/metadata.json")
			must(t, err)
			bundle, err := reader.LoadSource(t.Context(), remote, metadata, reader.Limits{})
			if err != nil || len(bundle.NativeRecords) == 0 {
				t.Fatalf("ordinary %s retained readback failed: %v", format, err)
			}
			assertCollectorUnlocked(t, home)
			if format == destination.FormatLegacy {
				if _, err := os.Stat(filepath.Join(home, "collector-principal.json")); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("ordinary legacy pass acquired typed ownership: %v", err)
				}
			}
		})
	}
}

func TestSyncRefusesUnsupportedStagedConfiguration(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"catalog", "future-floor"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			f := newStagedSyncFixture(t)
			f.forbidNativeReads(t)
			stagePath := filepath.Join(f.home, "sessions", f.reg.ArchiveSessionID, "import-stage", "stage.json")
			before, err := os.ReadFile(stagePath)
			must(t, err)
			if mode == "catalog" {
				cfg := mustLoadConfig(t, f.home)
				cfg.Storage.ArchiveFormat = destination.FormatCatalogV4
				cfg.ScopedPickerProtection = true
				must(t, config.Save(f.home, cfg))
				cfg = mustLoadConfig(t, f.home)
				if cfg.SupportsStagedSelection() {
					t.Fatal("catalog remembered protection enabled typed staging")
				}
				if _, err := f.store.SaveImportStage(t.Context(), f.reg, f.bundle, cfg.EffectiveSkillEvidence()); !errors.Is(err, state.ErrReadySelectionFrozen) {
					t.Fatalf("catalog created a new typed stage: %v", err)
				}
				remote, err := catalog.WrapConfigured(&qualifiedCLIStore{f.remote}, cfg.Storage)
				must(t, err)
				f.env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return remote, nil }
			} else {
				path := filepath.Join(f.home, "config.json")
				raw, err := os.ReadFile(path)
				must(t, err)
				var document map[string]json.RawMessage
				must(t, json.Unmarshal(raw, &document))
				document["schema_version"] = json.RawMessage(`{"version":999,"writer":"synthetic-future-writer"}`)
				raw, err = json.Marshal(document)
				must(t, err)
				must(t, os.WriteFile(path, raw, 0600))
				f.env.OpenStore = func(config.Config) (storage.ObjectStore, error) {
					t.Error("unsupported writer floor reached storage")
					return f.remote, nil
				}
			}
			if code, _, _ := f.sync(t); code != 1 {
				t.Fatalf("unsupported staged configuration was accepted: exit=%d", code)
			}
			after, err := os.ReadFile(stagePath)
			must(t, err)
			if !bytes.Equal(before, after) {
				t.Fatal("unsupported staged configuration changed frozen evidence")
			}
			if objects, err := f.remote.List(t.Context(), "sessions"); err != nil || len(objects) != 0 {
				t.Fatalf("unsupported staged configuration published session objects: %v", err)
			}
			assertCollectorUnlocked(t, f.home)
		})
	}
}
