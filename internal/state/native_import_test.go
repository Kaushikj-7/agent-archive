package state

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
)

func historicalNativeOwnerFixture(t *testing.T) (nativeAdmissionFixture, archive.SessionRegistration) {
	t.Helper()
	f := newNativeAdmissionFixture(t)
	prepared, err := f.prepare(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	owner, err := f.store.CommitNativeAdmission(t.Context(), prepared)
	if err != nil {
		t.Fatal(err)
	}
	if err := prepared.Close(); err != nil {
		t.Fatal(err)
	}
	owner = historicalNativeRegistrationFromOwner(t, f, owner)
	// Arrange a persisted historical owner; first-owner authority is tested only
	// through its dedicated prepared port once that producer is connected.
	if err := local.Write(f.store.registrationPath(owner.ArchiveSessionID), owner); err != nil {
		t.Fatal(err)
	}
	return f, owner
}

func historicalNativeRegistrationFromOwner(t *testing.T, f nativeAdmissionFixture, owner archive.SessionRegistration) archive.SessionRegistration {
	t.Helper()
	owner.Origin = archive.SessionOriginImport
	owner.NativeAdmission = nil
	owner.ImportBatch = archive.NewImportBatch("2026-10-17-1")
	root, err := archive.NativeRootFingerprint(f.candidate.Root)
	if err != nil {
		t.Fatal(err)
	}
	binding, err := archive.NativeImportBindingDigest(owner.NativeBinding)
	if err != nil {
		t.Fatal(err)
	}
	owner.NativeImport = &archive.NativeImport{Contract: "muse-historical-import-v1", Confirmation: archive.NativeImportConfirmation{BatchID: owner.ImportBatch.Recorded(), BatchInstance: strings.Repeat("a", 32), ScopeSHA256: strings.Repeat("b", 64), DestinationID: owner.DestinationID, ProjectID: owner.ProjectID, RootFingerprint: root, SelectedBindingSHA256: binding, SelectedSource: *owner.NativeBinding.Checkpoint}, Membership: archive.NativeImportMembership{ReceiptID: strings.Repeat("c", 32), State: "attached", Effects: []string{"registration"}}}
	scope := archive.NativeImportBatchScope{BatchID: owner.ImportBatch.Recorded(), StartedAtUTC: owner.AdmittedAt.UTC().Format(time.RFC3339Nano), DestinationID: owner.DestinationID, Filters: archive.ImportBatchFilters{Harnesses: []string{"muse"}, ProjectIDs: []string{}}, ProjectIDs: []string{owner.ProjectID}, Selected: []archive.NativeImportBatchSelection{{Kind: "ordinary-root", NativeID: owner.NativeSessionID, OriginalTUTC: owner.SessionStartedAt.UTC().Format(time.RFC3339Nano), ProjectID: owner.ProjectID, RootFingerprint: root, RootRevision: owner.NativeBinding.RootRevision, BindingSHA256: binding, Source: *owner.NativeBinding.Checkpoint}}}
	owner.NativeImport.Confirmation.ScopeSHA256, err = archive.NativeImportBatchScopeDigest(t.Context(), scope)
	if err != nil {
		t.Fatal(err)
	}
	control := archive.NativeImportBatchControl{ID: scope.BatchID, StartedAt: owner.AdmittedAt, DestinationID: owner.DestinationID, Filters: scope.Filters, NativeImport: &archive.NativeImportBatchConfirmation{Contract: "muse-historical-import-v1", Instance: owner.NativeImport.Confirmation.BatchInstance, ScopeSHA256: owner.NativeImport.Confirmation.ScopeSHA256, Scope: scope}}
	// Explicit control arrangement accompanies this already-owned test receipt;
	// it is not the purpose-specific confirmation/admission producer.
	if err := local.Write(filepath.Join(f.store.home, "imports", scope.BatchID+".json"), control); err != nil {
		t.Fatal(err)
	}
	if err := owner.Validate(); err != nil {
		t.Fatal(err)
	}
	return owner
}

func TestNativeImportGenericWritersPreserveClosedReceiptAndNeedFence(t *testing.T) {
	f, owner := historicalNativeOwnerFixture(t)
	if err := f.store.SaveRegistration(owner); err == nil {
		t.Fatal("receipt owner accepted without import writer fence")
	}
	cfg, found, err := config.Load(f.store.home)
	if err != nil || !found {
		t.Fatal(err)
	}
	cfg.NativeImportProtection = true
	if err := config.Save(f.store.home, cfg); err != nil {
		t.Fatal(err)
	}
	if err := f.store.SaveRegistration(owner); err != nil {
		t.Fatal("unchanged historical receipt refused under independent live+import fences", err)
	}
	for name, change := range map[string]func(*archive.SessionRegistration){
		"drop": func(r *archive.SessionRegistration) { r.NativeImport = nil },
		"replace": func(r *archive.SessionRegistration) {
			copy := *r.NativeImport
			copy.Membership.ReceiptID = strings.Repeat("d", 32)
			r.NativeImport = &copy
		},
		"detach": func(r *archive.SessionRegistration) {
			copy := *r.NativeImport
			copy.Membership.State = "detached"
			r.NativeImport = &copy
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := f.store.UpdateRegistration(owner.ArchiveSessionID, func(r *archive.SessionRegistration) error { change(r); return nil }); err == nil {
				t.Fatal("generic writer changed creating-batch authority")
			}
		})
	}
	current, found, err := f.store.LoadRegistration(owner.ArchiveSessionID)
	if err != nil || !found || current.NativeImport == nil || current.NativeImport.Membership.State != "attached" {
		t.Fatal("refused transition changed receipt", err)
	}
	f.assertReleased(t)
}

func TestNativeImportHistoricalOwnerCannotGainLiveCheckpointAuthority(t *testing.T) {
	f, owner := historicalNativeOwnerFixture(t)
	verified := false
	if _, err := f.store.CommitNativeValidation(t.Context(), owner, f.candidate.Root, owner.NativeBinding, func(context.Context) error { verified = true; return nil }); err == nil || verified {
		t.Fatal("historical owner entered live validation")
	}
	if !owner.RetainedOnly() || owner.CaptureFrozen {
		t.Fatal("historical receipt fabricated frozen generation")
	}
	f.assertReleased(t)
}

func TestNativeImportDualAuthorityRefusalPreservesOrdinaryLiveReplay(t *testing.T) {
	f := newNativeAdmissionFixture(t)
	prepared, err := f.prepare(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	live, err := f.store.CommitNativeAdmission(t.Context(), prepared)
	if err != nil {
		t.Fatal(err)
	}
	if err := prepared.Close(); err != nil {
		t.Fatal(err)
	}
	historical := historicalNativeRegistrationFromOwner(t, f, live)
	historical.NativeAdmission = live.NativeAdmission
	if err := historical.Validate(); err == nil {
		t.Fatal("receipt accepted actual valid live admission")
	}
	historical.NativeAdmission = nil
	for _, replay := range []*archive.Replay{{}, {RunID: "ordinary-live-run"}} {
		historical.Replay = replay
		if err := historical.Validate(); err == nil {
			t.Fatal("receipt accepted replay authority")
		}
	}
	cfg, found, err := config.Load(f.store.home)
	if err != nil || !found {
		t.Fatal(err)
	}
	cfg.NativeReplayProtection = true
	if err := config.Save(f.store.home, cfg); err != nil {
		t.Fatal(err)
	}
	live.Replay = &archive.Replay{RunID: "ordinary-live-run"}
	if err := live.Validate(); err != nil {
		t.Fatal(err)
	}
	// Arrange an already-classified owner; generic writers preserve its replay.
	if err := local.Write(f.store.registrationPath(live.ArchiveSessionID), live); err != nil {
		t.Fatal(err)
	}
	if err := f.store.SaveRegistration(live); err != nil {
		t.Fatal("ordinary live replay owner refused", err)
	}
	if _, err := f.store.UpdateRegistration(live.ArchiveSessionID, func(r *archive.SessionRegistration) error { r.HookObservedAt = r.HookObservedAt.Add(1); return nil }); err != nil {
		t.Fatal("ordinary live replay update refused", err)
	}
	f.assertReleased(t)
}
