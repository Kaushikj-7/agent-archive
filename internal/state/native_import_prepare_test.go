package state

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/agents/muse"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/transcriptio"
)

type nativeImportPrepareFixture struct {
	native       nativeAdmissionFixture
	confirmation *ConfirmedNativeImportBatch
	candidate    NativeImportCandidate
	snapshot     agentapi.SourceSnapshot
	pass         agentapi.SourcePass
	limits       agentapi.ReadLimits
}

func newNativeImportPrepareFixture(t *testing.T) nativeImportPrepareFixture {
	t.Helper()
	return newNativeImportPrepareFixtureProject(t, "")
}

func newNativeImportPrepareFixtureProject(t *testing.T, projectID string) nativeImportPrepareFixture {
	t.Helper()
	return nativeImportPrepareFixtureFromNative(t, newNativeAdmissionFixture(t), projectID, 8<<20)
}

func nativeImportPrepareFixtureFromNative(t *testing.T, f nativeAdmissionFixture, projectID string, filteredBytes int64) nativeImportPrepareFixture {
	t.Helper()
	cfg, _, err := config.Load(f.store.home)
	if err != nil {
		t.Fatal(err)
	}
	previous := cfg
	if projectID != "" {
		cfg.Archive.Projects = append([]archive.ProjectActivation(nil), cfg.Archive.Projects...)
		cfg.Archive.Projects[0].ProjectID = projectID
		f.candidate.ProjectID = projectID
	}
	cfg.Harnesses = nil
	cfg.ImportedHarnesses = []string{"muse"}
	cfg.NativeImportProtection, cfg.DurableStorageProtection = true, true
	cfg.PublicationCompositionProtection, cfg.ImportStagingProtection, cfg.StagedSelectionProtection = true, true, true
	// Keep the real original birth older than current project activation. That
	// is legitimate historical intent, independently of the retired live window.
	cfg.Archive.Projects[0].ActivatedAt = time.Now().UTC()
	cfg, err = config.ReconcileMuseCapture(cfg, previous, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := config.Save(f.store.home, cfg); err != nil {
		t.Fatal(err)
	}
	lookup := admissionUnitLocator{}
	pass, err := (muse.SourceProvider{}).OpenPass(t.Context(), agentapi.SourceEnvironment{NativeRoot: &f.candidate.Root, NativeLocators: lookup, ReadBudget: f.budget, Files: transcriptio.RootOpener{Root: f.candidate.Root.Canonical}, Policy: transcriptio.OpenPolicy{Root: f.candidate.Root.Canonical, RejectSymlinks: true}})
	if err != nil {
		t.Fatal(err)
	}
	limits := agentapi.ReadLimits{RawBytes: 32 << 20, RecordBytes: 1 << 20, FilteredBytes: filteredBytes, ReadBudget: f.budget}
	snapshot, err := pass.Read(t.Context(), agentapi.SourceRef{Kind: archive.SourceKindFile, Path: f.path, Key: f.candidate.Intent.NativeSessionID}, limits)
	if err != nil {
		_ = pass.Close()
		t.Fatal(err)
	}
	evidence, ok := snapshot.(agentapi.SourceAdmissionEvidence)
	if !ok {
		t.Fatal("concrete source lacks admission evidence")
	}
	facts, err := evidence.AdmissionEvidence(t.Context(), agentapi.SourceAdmission{NativeID: f.candidate.Intent.NativeSessionID})
	if err != nil {
		t.Fatal(err)
	}
	binding := facts.Binding.Native
	digest, err := archive.NativeImportBindingDigest(binding)
	if err != nil {
		t.Fatal(err)
	}
	root, err := archive.NativeRootFingerprint(f.candidate.Root)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC()
	scope := archive.NativeImportBatchScope{BatchID: "2026-10-17-2", StartedAtUTC: at.Format(time.RFC3339Nano), DestinationID: cfg.DestinationID(), Filters: archive.ImportBatchFilters{Harnesses: []string{"muse"}, ProjectIDs: []string{}}, ProjectIDs: []string{f.candidate.ProjectID}, Selected: []archive.NativeImportBatchSelection{{Kind: archive.NativeImportOrdinaryRoot, NativeID: binding.NativeID, OriginalTUTC: binding.NativeCreatedAt.UTC().Format(time.RFC3339Nano), ProjectID: f.candidate.ProjectID, RootFingerprint: root, RootRevision: f.candidate.Root.Revision, BindingSHA256: digest, Source: *binding.Checkpoint}}}
	scopeDigest, err := archive.NativeImportBatchScopeDigest(t.Context(), scope)
	if err != nil {
		t.Fatal(err)
	}
	control := archive.NativeImportBatchControl{ID: scope.BatchID, StartedAt: at, Filters: scope.Filters, DestinationID: scope.DestinationID, NativeImport: &archive.NativeImportBatchConfirmation{Contract: "muse-historical-import-v1", Instance: strings.Repeat("b", 32), ScopeSHA256: scopeDigest, Scope: scope}}
	if err := local.Write(filepath.Join(f.store.home, "imports", control.ID+".json"), control); err != nil {
		t.Fatal(err)
	}
	confirmation, err := f.store.OpenConfirmedNativeImportBatch(t.Context(), control.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := confirmation.Close(); err != nil {
			t.Error(err)
		}
		if err := snapshot.Close(); err != nil {
			t.Error(err)
		}
		if err := pass.Close(); err != nil {
			t.Error(err)
		}
	})
	return nativeImportPrepareFixture{native: f, confirmation: confirmation, candidate: NativeImportCandidate{NativeID: binding.NativeID, Root: f.candidate.Root, ProjectRoot: f.candidate.ProjectRoot, ProjectID: f.candidate.ProjectID}, snapshot: snapshot, pass: pass, limits: limits}
}

func (f nativeImportPrepareFixture) prepare(ctx context.Context) (*PreparedNativeImport, error) {
	return f.native.store.PrepareNativeImport(ctx, f.confirmation, f.candidate, f.snapshot, f.snapshot.Input(), muse.Filter{}, agentapi.FilterContext{Filename: f.native.path, Limits: f.limits}, admissionUnitLocator{}, "historical-unit-selection", f.native.budget, f.pass.Close)
}

func TestPreparedNativeImportProvesHistoricalSourceWithoutLiveWindow(t *testing.T) {
	t.Parallel()
	f := newNativeImportPrepareFixture(t)
	p, err := f.prepare(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if p.binding.NativeCreatedAt.IsZero() || p.binding.NativeCreatedAt.UTC().Format(time.RFC3339Nano) != p.selected.OriginalTUTC {
		t.Fatal("original birth was replaced")
	}
	if _, err := p.Filtered(); err != nil {
		t.Fatal(err)
	}
	if _, found, err := f.native.store.ArchiveSessionID(agentmeta.SessionKey{Agent: agentmeta.Muse, NativeID: f.candidate.NativeID}); err != nil || found {
		t.Fatal("preparation created an owner", found, err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	f.native.assertReleased(t)
	if _, err := p.Filtered(); !errors.Is(err, agentapi.ErrClosed) {
		t.Fatal("closed output available", err)
	}
}

func TestPreparedNativeImportRechecksSourceConfirmationAndCancellation(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"source", "confirmation", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			f := newNativeImportPrepareFixture(t)
			p, err := f.prepare(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := p.Close(); err != nil {
					t.Error(err)
				}
			}()
			ctx := t.Context()
			switch mode {
			case "source":
				raw, err := os.ReadFile(f.native.path)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(f.native.path, append(raw, '\n'), 0600); err != nil {
					t.Fatal(err)
				}
			case "confirmation":
				if err := f.confirmation.Close(); err != nil {
					t.Fatal(err)
				}
			case "cancel":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			if err := p.verify(ctx); err == nil {
				t.Fatal("changed source/scope or cancellation accepted")
			}
		})
	}
}

func TestPreparedNativeImportRejectsUnselectedAndWrongPhysicalProject(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"unselected", "project", "root"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			f := newNativeImportPrepareFixture(t)
			switch mode {
			case "unselected":
				f.candidate.NativeID = "different"
			case "project":
				f.candidate.ProjectRoot = t.TempDir()
			case "root":
				f.candidate.Root.Revision = "different"
			}
			if p, err := f.prepare(t.Context()); err == nil {
				_ = p.Close()
				t.Fatal("unconfirmed candidate accepted")
			}
		})
	}
	var zero PreparedNativeImport
	if _, err := zero.Filtered(); !errors.Is(err, agentapi.ErrClosed) {
		t.Fatal("zero output available", err)
	}
	if err := zero.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPreparedNativeImportPreservesOpaqueConfiguredProject(t *testing.T) {
	t.Parallel()
	f := newNativeImportPrepareFixtureProject(t, "existing-opaque-import-project")
	p, err := f.prepare(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if p.candidate.ProjectID != "existing-opaque-import-project" {
		t.Fatal("configured project identity recomputed")
	}
	copied := *p
	if err := copied.Close(); !errors.Is(err, agentapi.ErrClosed) {
		t.Fatal("copied prepared capability closed live source", err)
	}
	if err := p.verify(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	f.native.assertReleased(t)
}
