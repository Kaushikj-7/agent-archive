package state

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/local"
)

func nativeImportCommitForUndo(t *testing.T) (nativeImportPrepareFixture, archive.SessionRegistration) {
	t.Helper()
	f := newNativeImportPrepareFixture(t)
	prepared, err := f.prepare(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	owner, err := f.native.store.CommitNativeImport(t.Context(), prepared)
	if err != nil {
		_ = prepared.Close()
		t.Fatal(err)
	}
	if err := prepared.Close(); err != nil {
		t.Fatal(err)
	}
	f.native.assertReleased(t)
	return f, owner
}

func nativeImportSelectingACK(t *testing.T, s *Store, reg archive.SessionRegistration) PendingPublication {
	t.Helper()
	stage, found, err := s.LoadImportStage(t.Context(), reg)
	if err != nil || !found {
		t.Fatal("actual selected stage missing", found, err)
	}
	source, err := archive.BuildCompressedSource(stage.Bundle)
	if err != nil {
		t.Fatal(err)
	}
	sourceKey, err := archive.SourceObjectKey(stage.Bundle, source.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	metadataKey, err := archive.MetadataObjectKey("muse", reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	pending := PendingPublication{Bundle: stage.Bundle, SourceKey: sourceKey, MetadataKey: metadataKey, SourceBytes: source.Bytes, SourceSHA256: source.SHA256, ReadyAt: reg.AdmittedAt, Attempted: true, SkillEvidence: string(stage.SkillEvidence)}
	request, found, err := s.LoadRequest(reg.ArchiveSessionID)
	if err != nil || !found || request.Token == "" {
		t.Fatal("actual import request missing before selecting ACK", found, err)
	}
	pending.RequestToken = request.Token
	metadata, err := archive.BuildMetadataWithAnalysis(stage.Bundle, archive.Analysis{}, nil, "synthetic", reg.SessionStartedAt, reg.AdmittedAt, pending.SourceReference(), archive.ParserInfo{Name: "muse", Version: "synthetic"})
	if err != nil {
		t.Fatal(err)
	}
	metadata.ApplyRegistrationProvenance(reg)
	pending.MetadataBytes, err = json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	pending, err = s.FreezeStagedSelection(t.Context(), reg.ArchiveSessionID, pending, ReadyPredecessorAbsent, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SavePending(reg.ArchiveSessionID, pending); err != nil {
		t.Fatal(err)
	}
	published, err := s.LoadPublishedStateContext(t.Context(), reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := published.SaveReadyPublication(pending, reg.AdmittedAt); err != nil {
		t.Fatal(err)
	}
	return pending
}

func markActualNativeImportUndone(t *testing.T, s *Store, reg archive.SessionRegistration) {
	t.Helper()
	home, err := local.OpenRootedHome(s.home)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := home.Close(); err != nil {
			t.Error(err)
		}
	}()
	read, closeRead := s.WithImportResultScope(t.Context())
	defer closeRead()
	control, err := read.readNativeImportControl(t.Context(), home, reg.NativeImport.Confirmation.BatchID)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC()
	control.UndoneAt = &at
	if err := local.Write(filepath.Join(s.home, "imports", control.ID+".json"), control); err != nil {
		t.Fatal(err)
	}
}

func TestNativeImportExactUndoPreservesInitialACKAndFrozenStage(t *testing.T) {
	t.Parallel()
	f, reg := nativeImportCommitForUndo(t)
	s := f.native.store
	pending := nativeImportSelectingACK(t, s, reg)
	if err := s.SaveRequest(reg.ArchiveSessionID, "later-independent", reg.AdmittedAt.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	paths := []string{importStageRelative(reg.ArchiveSessionID), s.requestPath(reg.ArchiveSessionID), s.pendingPath(reg.ArchiveSessionID), s.publishedPath(reg.ArchiveSessionID), filepath.Join(s.home, "config.json")}
	before := map[string][]byte{}
	for _, path := range paths {
		if !filepath.IsAbs(path) {
			path = filepath.Join(s.home, path)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		before[path] = raw
	}
	markActualNativeImportUndone(t, s, reg)
	changed, err := s.DetachNativeImportBatchMembership(t.Context(), reg.NativeImport.Confirmation.BatchID, reg.ArchiveSessionID, reg.NativeImport.Membership.ReceiptID)
	if err != nil || !changed {
		t.Fatal("exact detach refused", changed, err)
	}
	current, found, err := s.LoadRegistration(reg.ArchiveSessionID)
	if err != nil || !found || current.NativeImport.Membership.State != archive.NativeImportDetached {
		t.Fatal("membership not detached", found, err)
	}
	for path, raw := range before {
		after, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(raw, after) {
			t.Fatal("undo changed independent/frozen data", path, err)
		}
	}
	changed, err = s.DetachNativeImportBatchMembership(t.Context(), reg.NativeImport.Confirmation.BatchID, reg.ArchiveSessionID, reg.NativeImport.Membership.ReceiptID)
	if err != nil || changed {
		t.Fatal("exact undo not idempotent", changed, err)
	}
	if err := os.RemoveAll(f.candidate.Root.Canonical); err != nil {
		t.Fatal(err)
	}
	changeNativeImportInitialConfig(t, s, current, nativeImportTestRemoved)
	reopened, err := Open(s.home)
	if err != nil {
		t.Fatal(err)
	}
	publication, err := reopened.LoadPublishedStateContext(t.Context(), current.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := publication.ValidateNativeImportReadAuthority(current); err != nil {
		t.Fatal("detached typed ACK local authority lost", err)
	}
	guard, err := local.LockCollectorGuard(s.home)
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Release()
	if err := reopened.VerifyAndBeginStagedRelease(t.Context(), current.ArchiveSessionID, stagedRemote(pending), guard); err != nil {
		t.Fatal("exact selecting ACK could not finish cleanup", err)
	}
	if err := reopened.NativeImportRetainedAuthority(t.Context(), current); err != nil {
		t.Fatal("detached retained authority lost", err)
	}
	request, found, err := reopened.LoadRequest(current.ArchiveSessionID)
	if err != nil || !found || !slices.Contains(request.Reasons, "later-independent") {
		t.Fatal("undo/release lost later request", found, err)
	}
	rawRequest, err := os.ReadFile(reopened.requestPath(current.ArchiveSessionID))
	if err != nil || !bytes.Equal(rawRequest, before[s.requestPath(current.ArchiveSessionID)]) {
		t.Fatal("old ACK changed later request token or bytes", err)
	}
}

func TestNativeImportExactUndoBeforeACKCannotAuthorizePublication(t *testing.T) {
	t.Parallel()
	f, reg := nativeImportCommitForUndo(t)
	s := f.native.store
	stage, found, err := s.LoadImportStage(t.Context(), reg)
	if err != nil || !found {
		t.Fatal(found, err)
	}
	before, err := os.ReadFile(filepath.Join(s.home, importStageRelative(reg.ArchiveSessionID)))
	if err != nil {
		t.Fatal(err)
	}
	markActualNativeImportUndone(t, s, reg)
	if changed, err := s.DetachNativeImportBatchMembership(t.Context(), reg.NativeImport.Confirmation.BatchID, reg.ArchiveSessionID, reg.NativeImport.Membership.ReceiptID); err != nil || !changed {
		t.Fatal(changed, err)
	}
	current, found, err := s.LoadRegistration(reg.ArchiveSessionID)
	if err != nil || !found {
		t.Fatal(found, err)
	}
	if err := s.nativeImportCurrentBatch(t.Context(), current); err == nil {
		t.Fatal("detachment granted new publication authority")
	}
	if err := s.resumePreparedImport(t.Context(), stage); err == nil {
		t.Fatal("undone staged owner resumed before ACK")
	}
	after, err := os.ReadFile(filepath.Join(s.home, importStageRelative(reg.ArchiveSessionID)))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("undo removed frozen stage", err)
	}
	if err := s.SaveRegistration(reg); err == nil {
		t.Fatal("generic writer reattached membership")
	}
}

func TestNativeImportExactUndoRefusesWrongReceiptAndMissingUndo(t *testing.T) {
	t.Parallel()
	for _, undone := range []bool{false, true} {
		t.Run(fmt.Sprint(undone), func(t *testing.T) {
			t.Parallel()
			f, reg := nativeImportCommitForUndo(t)
			if undone {
				markActualNativeImportUndone(t, f.native.store, reg)
			}
			path := f.native.store.registrationPath(reg.ArchiveSessionID)
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			receipt := reg.NativeImport.Membership.ReceiptID
			if undone {
				receipt = strings.Repeat("f", 32)
				if receipt == reg.NativeImport.Membership.ReceiptID {
					receipt = strings.Repeat("0", 32)
				}
			}
			if changed, err := f.native.store.DetachNativeImportBatchMembership(t.Context(), reg.NativeImport.Confirmation.BatchID, reg.ArchiveSessionID, receipt); err == nil || changed {
				t.Fatal("unproved undo accepted", changed, err)
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("refusal changed actual owner", err)
			}
		})
	}
}

func TestNativeImportExactUndoRefusesOwnerReplacementBeforeCAS(t *testing.T) {
	t.Parallel()
	f, reg := nativeImportCommitForUndo(t)
	s := f.native.store
	markActualNativeImportUndone(t, s, reg)
	path := s.registrationPath(reg.ArchiveSessionID)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	future := bytes.Replace(before, []byte("muse-historical-import-v1"), []byte("muse-historical-import-v9"), 1)
	replaced := false
	s.onWriteSync = func() {
		if replaced {
			return
		}
		replaced = true
		if err := os.WriteFile(path, future, 0600); err != nil {
			t.Error(err)
		}
	}
	changed, err := s.DetachNativeImportBatchMembership(t.Context(), reg.NativeImport.Confirmation.BatchID, reg.ArchiveSessionID, reg.NativeImport.Membership.ReceiptID)
	s.onWriteSync = nil
	if !replaced || changed || err == nil {
		t.Fatal("current future owner escaped final CAS", replaced, changed, err)
	}
	after, readErr := os.ReadFile(path)
	if readErr != nil || !bytes.Equal(future, after) {
		t.Fatal("future owner overwritten", readErr)
	}
}

func TestNativeImportDetachedACKRefusesChangedOwnerStageAndSelection(t *testing.T) {
	for _, mode := range []string{"owner", "receipt", "stage", "ack", "missing-ack"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			f, reg := nativeImportCommitForUndo(t)
			s := f.native.store
			pending := nativeImportSelectingACK(t, s, reg)
			markActualNativeImportUndone(t, s, reg)
			if changed, err := s.DetachNativeImportBatchMembership(t.Context(), reg.NativeImport.Confirmation.BatchID, reg.ArchiveSessionID, reg.NativeImport.Membership.ReceiptID); err != nil || !changed {
				t.Fatal(changed, err)
			}
			current, found, err := s.LoadRegistration(reg.ArchiveSessionID)
			if err != nil || !found {
				t.Fatal(found, err)
			}
			switch mode {
			case "owner":
				current.RegisteredAt = current.RegisteredAt.Add(time.Nanosecond)
				if err := local.Write(s.registrationPath(current.ArchiveSessionID), current); err != nil {
					t.Fatal(err)
				}
			case "receipt":
				receipt := *current.NativeImport
				receipt.Confirmation.BatchInstance = strings.Repeat("f", 32)
				current.NativeImport = &receipt
				if err := local.Write(s.registrationPath(current.ArchiveSessionID), current); err != nil {
					t.Fatal(err)
				}
			case "stage":
				stage, found, err := s.LoadImportStage(t.Context(), current)
				if err != nil || !found {
					t.Fatal(found, err)
				}
				stage.Bundle.Capture.AdapterVersion += "-tampered"
				if err := local.Write(filepath.Join(s.home, importStageRelative(current.ArchiveSessionID)), stage); err != nil {
					t.Fatal(err)
				}
			case "ack":
				var ack publishedState
				if err := s.readRegistrationJSON(s.publishedPath(current.ArchiveSessionID), &ack); err != nil {
					t.Fatal(err)
				}
				ack.MetadataBytes = append(ack.MetadataBytes, ' ')
				if err := local.Write(s.publishedPath(current.ArchiveSessionID), ack); err != nil {
					t.Fatal(err)
				}
			case "missing-ack":
				if err := os.Remove(s.publishedPath(current.ArchiveSessionID)); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.RemoveAll(f.candidate.Root.Canonical); err != nil {
				t.Fatal(err)
			}
			paths := []string{s.registrationPath(current.ArchiveSessionID), filepath.Join(s.home, importStageRelative(current.ArchiveSessionID)), s.pendingPath(current.ArchiveSessionID), s.requestPath(current.ArchiveSessionID)}
			if mode != "missing-ack" {
				paths = append(paths, s.publishedPath(current.ArchiveSessionID))
			}
			before := map[string][]byte{}
			for _, path := range paths {
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				before[path] = raw
			}
			restarted, err := Open(s.home)
			if err != nil {
				t.Fatal(err)
			}
			publication, err := restarted.LoadPublishedStateContext(t.Context(), current.ArchiveSessionID)
			if err == nil {
				err = publication.ValidateNativeImportReadAuthority(current)
			}
			if err == nil {
				t.Fatal("changed immutable ACK authority became readable")
			}
			guard, err := local.LockCollectorGuard(s.home)
			if err != nil {
				t.Fatal(err)
			}
			defer guard.Release()
			if err := restarted.VerifyAndBeginStagedRelease(t.Context(), current.ArchiveSessionID, stagedRemote(pending), guard); err == nil {
				t.Fatal("changed immutable ACK authority authorized cleanup")
			}
			for path, raw := range before {
				after, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(raw, after) {
					t.Fatal("refusal changed retained evidence", path, err)
				}
			}
		})
	}
}
