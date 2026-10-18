package state

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/local"
)

func TestNativeImportCommitPersistsStageBeforeReservationAndOwner(t *testing.T) {
	t.Parallel()
	f := newNativeImportPrepareFixture(t)
	stateBudget := f.confirmation.read.resourceBudget
	beforeState := stateBudget.Available()
	p, err := f.prepare(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := p.Close(); err != nil {
			t.Error(err)
		}
	}()
	key := agentmeta.SessionKey{Agent: agentmeta.Muse, NativeID: f.candidate.NativeID}
	var steps []string
	f.native.store.onIndexStep = func(step string) error {
		steps = append(steps, step)
		if step == "import-stage" {
			stage, found, err := f.native.store.LoadImportStage(t.Context(), p.reg)
			if err != nil || !found || stage.SHA256 != p.reg.StagedImportSHA256 {
				t.Fatal("stage not durable first", found, err)
			}
			if _, found, err := f.native.store.readQualifiedIndex(key); err != nil || found {
				t.Fatal("reservation preceded stage", found, err)
			}
			if _, found, err := f.native.store.LoadRegistration(p.reg.ArchiveSessionID); err != nil || found {
				t.Fatal("owner preceded stage", found, err)
			}
		}
		return nil
	}
	owner, err := f.native.store.CommitNativeImport(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(steps, ",") != "import-stage,import-reservation,import-registration,import-committed" {
		t.Fatal("wrong durable order", steps)
	}
	if owner.NativeAdmission != nil || owner.NativeImport == nil || owner.CaptureFrozen || owner.ValidateNativeImport() != nil {
		t.Fatal("historical authority lost or mixed")
	}
	entry, found, err := f.native.store.readQualifiedIndex(key)
	if err != nil || !found || entry != indexEntry(key, owner.ArchiveSessionID) {
		t.Fatal("unfinished import index", entry, err)
	}
	if _, found, err := f.native.store.LoadRequest(owner.ArchiveSessionID); err != nil || !found {
		t.Fatal("no import request", found, err)
	}
	// Returned data is freshly decoded, not the prepared authority's private copy.
	owner.NativeImport.Membership.State = archive.NativeImportDetached
	again, err := f.native.store.CommitNativeImport(t.Context(), p)
	if err != nil || again.NativeImport.Membership.State != archive.NativeImportAttached {
		t.Fatal("returned receipt mutated prepared identity", err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	f.native.assertReleased(t)
	if stateBudget.Available() != beforeState {
		t.Fatal("prepared state bundle loan leaked", stateBudget.Available(), beforeState)
	}
	if p.stage.Bundle.NativeRecords != nil || p.reg.NativeImport != nil || p.read != nil || p.confirmation != nil {
		t.Fatal("expired prepared capability retained charged payload")
	}
}

func TestNativeImportStageFirstRecoveryNeedsNoNativeSource(t *testing.T) {
	t.Parallel()
	for _, stop := range []string{"import-stage", "import-reservation", "import-registration"} {
		t.Run(stop, func(t *testing.T) {
			t.Parallel()
			f := newNativeImportPrepareFixture(t)
			p, err := f.prepare(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			injected := errors.New("synthetic import interruption")
			f.native.store.onIndexStep = func(step string) error {
				if step == stop {
					return injected
				}
				return nil
			}
			if _, err := f.native.store.CommitNativeImport(t.Context(), p); !errors.Is(err, injected) {
				t.Fatal("did not reach exact interruption", err)
			}
			reg := p.reg
			stage, found, err := f.native.store.LoadImportStage(t.Context(), reg)
			if err != nil || !found {
				t.Fatal("missing actual stage", found, err)
			}
			if err := p.Close(); err != nil {
				t.Fatal(err)
			}
			f.native.assertReleased(t)
			if err := f.confirmation.Close(); err != nil {
				t.Fatal(err)
			}
			if err := os.RemoveAll(f.candidate.Root.Canonical); err != nil {
				t.Fatal(err)
			}
			restarted, err := Open(f.native.store.home)
			if err != nil {
				t.Fatal(err)
			}
			if err := restarted.resumePreparedImport(t.Context(), stage); err != nil {
				t.Fatal("stage-backed restart", err)
			}
			actual, found, err := restarted.LoadRegistration(reg.ArchiveSessionID)
			if err != nil || !found || !sameNativeImportReceipt(actual.NativeImport, reg.NativeImport) || actual.StagedImportSHA256 != stage.SHA256 {
				t.Fatal("recovery replaced original receipt/owner", found, err)
			}
			key := agentmeta.SessionKey{Agent: agentmeta.Muse, NativeID: reg.NativeSessionID}
			index, found, err := restarted.readQualifiedIndex(key)
			if err != nil || !found || index != indexEntry(key, reg.ArchiveSessionID) {
				t.Fatal("recovery did not complete exact index", index, err)
			}
			if _, found, err := restarted.LoadRequest(reg.ArchiveSessionID); err != nil || !found {
				t.Fatal("recovery lost request", found, err)
			}
		})
	}
}

func TestNativeImportStageFirstRecoveryRefusesForeignAuthority(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"future", "mismatch", "root", "other-owner", "undone", "released"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			f := newNativeImportPrepareFixture(t)
			p, err := f.prepare(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			stop := errors.New("after stage")
			f.native.store.onIndexStep = func(step string) error {
				if step == "import-stage" {
					return stop
				}
				return nil
			}
			if _, err := f.native.store.CommitNativeImport(t.Context(), p); !errors.Is(err, stop) {
				t.Fatal("stage checkpoint missing", err)
			}
			stage, found, err := f.native.store.LoadImportStage(t.Context(), p.reg)
			if err != nil || !found {
				t.Fatal(found, err)
			}
			key := agentmeta.SessionKey{Agent: agentmeta.Muse, NativeID: p.reg.NativeSessionID}
			path := qualifiedSessionIndexPath(f.native.store.home, key)
			switch mode {
			case "future", "mismatch", "root", "other-owner":
				entry := indexEntry(key, p.reg.ArchiveSessionID)
				entry.Reservation = strings.Repeat("a", 32)
				entry.NativeReservation = nativeImportReservationPrefix + strings.Repeat("0", 64)
				if mode == "future" {
					entry.NativeReservation = "muse-import-reservation-v2:" + strings.Repeat("0", 64)
				}
				if mode == "root" {
					entry.NativeReservation = nativeReservationPrefix + strings.Repeat("0", 64)
				}
				if mode == "other-owner" {
					entry = indexEntry(key, strings.Repeat("e", 32))
				}
				if err := local.Write(path, entry); err != nil {
					t.Fatal(err)
				}
			case "undone":
				control := f.confirmation.control
				at := time.Now().UTC()
				control.UndoneAt = &at
				path = filepath.Join(f.native.store.home, "imports", control.ID+".json")
				if err := local.Write(path, control); err != nil {
					t.Fatal(err)
				}
			case "released":
				path = filepath.Join(f.native.store.home, stagedControl(p.reg.ArchiveSessionID, stagedReleaseProgressName))
				if err := local.Write(path, map[string]any{"phase": "future"}); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			ownerID := p.reg.ArchiveSessionID
			if err := p.Close(); err != nil {
				t.Fatal(err)
			}
			if err := os.RemoveAll(f.candidate.Root.Canonical); err != nil {
				t.Fatal(err)
			}
			restarted, err := Open(f.native.store.home)
			if err != nil {
				t.Fatal(err)
			}
			if err := restarted.resumePreparedImport(t.Context(), stage); err == nil {
				t.Fatal("foreign/incomplete authority accepted")
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("refusal changed protected control", err)
			}
			if _, found, err := restarted.LoadRegistration(ownerID); err != nil || found {
				t.Fatal("refusal created owner", found, err)
			}
		})
	}
}
