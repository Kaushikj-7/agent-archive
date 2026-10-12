package state

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
)

// StagedReadback exposes only bounded reads; state does not import a provider SDK.
type StagedReadback interface {
	GetLimited(context.Context, string, int64) ([]byte, error)
}

// Receipt construction remains private. Exported entrypoints perform actual
// bounded readback themselves and accept no caller readback bool or digest.
type stagedReleaseReceipt struct {
	Version   int             `json:"version"`
	Selection StagedSelection `json:"selection"`
	SHA256    string          `json:"sha256"`
}
type stagedReleaseProgress struct {
	Version       int    `json:"version"`
	ReceiptSHA256 string `json:"receipt_sha256"`
	Phase         string `json:"phase"`
}

func (r *stagedReleaseReceipt) UnmarshalJSON(raw []byte) error {
	if len(raw) > 16<<10 {
		return ErrDurableStorageRecovery
	}
	if err := strictStagedKeys(raw, []string{"version", "selection", "sha256"}); err != nil {
		return err
	}
	type plain stagedReleaseReceipt
	var next plain
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&next); err != nil {
		return ErrDurableStorageRecovery
	}
	*r = stagedReleaseReceipt(next)
	if r.Version != 3 || r.Selection.validateDigest() != nil {
		return ErrDurableStorageRecovery
	}
	copy := *r
	copy.SHA256 = ""
	b, err := json.Marshal(copy)
	if err != nil || readySHA(b) != r.SHA256 {
		return ErrDurableStorageRecovery
	}
	return nil
}
func (p *stagedReleaseProgress) UnmarshalJSON(raw []byte) error {
	if len(raw) > 16<<10 {
		return ErrDurableStorageRecovery
	}
	if err := strictStagedKeys(raw, []string{"version", "receipt_sha256", "phase"}); err != nil {
		return err
	}
	type plain stagedReleaseProgress
	var next plain
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&next); err != nil {
		return ErrDurableStorageRecovery
	}
	*p = stagedReleaseProgress(next)
	if p.Version != 3 || !readyDigestValid(p.ReceiptSHA256) || (p.Phase != "verified" && p.Phase != "local-complete" && p.Phase != "released") {
		return ErrDurableStorageRecovery
	}
	return nil
}
func stagedControl(id, name string) string {
	return filepath.Join("sessions", id, "import-stage", name)
}

func (s *Store) requireStagedReleaseProgress(id string, q *StagedSelection) error {
	var receipt stagedReleaseReceipt
	if err := s.readStagedControl(id, "release.json", &receipt); err != nil {
		return errors.Join(ErrDurableStorageRecovery, err)
	}
	var progress stagedReleaseProgress
	if err := s.readStagedControl(id, "release-progress.json", &progress); err != nil {
		return errors.Join(ErrDurableStorageRecovery, err)
	}
	if receipt.Selection != *q || progress.ReceiptSHA256 != receipt.SHA256 || (progress.Phase != "local-complete" && progress.Phase != "released") {
		return ErrReadySelectionFrozen
	}
	return nil
}

func (s *Store) stagedReadback(ctx context.Context, remote StagedReadback, key string, want []byte) error {
	if remote == nil {
		return ErrReadySelectionFrozen
	}
	if len(want) == 0 || len(want) > maxPendingHistoryBytes {
		return ErrDurableStorageCapacity
	}
	owner := s.resourceBudget.Reservation()
	n := int64(len(want))
	if !owner.Reserve(2*n + 1) {
		return errStateBudget
	}
	defer owner.Release(2*n + 1)
	raw, err := remote.GetLimited(ctx, key, n+1)
	if err != nil || !bytes.Equal(raw, want) {
		return errors.Join(ErrDurableStorageRecovery, err)
	}
	return nil
}

func (s *Store) verifyStagedRemote(ctx context.Context, remote StagedReadback, p PendingPublication) error {
	if authority, ok := remote.(interface{ CatalogMetadataAuthority() bool }); ok && authority.CatalogMetadataAuthority() {
		return ErrReadySelectionFrozen
	}
	if p.Catalog != nil || p.StagedSelection == nil {
		return ErrReadySelectionFrozen
	}
	if err := s.stagedReadback(ctx, remote, p.MetadataKey, p.MetadataBytes); err != nil {
		return err
	}
	if err := s.stagedReadback(ctx, remote, p.SourceKey, p.SourceBytes); err != nil {
		return err
	}
	return s.stagedReadback(ctx, remote, p.MetadataKey, p.MetadataBytes)
}

// VerifyAndBeginStagedRelease is the only constructor of release authority.
// It requires the actual held collector guard and exact persisted pending and
// published3 proof, then verifies real bounded remote bytes before token effects.
func (s *Store) VerifyAndBeginStagedRelease(ctx context.Context, id string, remote StagedReadback, guard *local.CollectorGuard) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	budget := s.resourceBudget
	if budget == nil {
		budget = agentapi.NewNativeReadBudget(128 << 20)
	}
	read, closeRead := s.WithReadBudget(ctx, budget)
	defer closeRead()
	return read.verifyAndBeginStagedRelease(ctx, id, remote, guard)
}

func (s *Store) verifyAndBeginStagedRelease(ctx context.Context, id string, remote StagedReadback, guard *local.CollectorGuard) error {
	if guard == nil || !safeFileComponent(id) {
		return ErrReadySelectionFrozen
	}
	origin, err := guard.Origin()
	if err != nil {
		return err
	}
	done, err := guard.BeginOperation(origin)
	if err != nil {
		return err
	}
	defer done()
	home, err := guard.Home()
	expected, e := filepath.EvalSymlinks(s.home)
	if err != nil || e != nil || home != expected {
		return ErrReadySelectionFrozen
	}
	cfg, err := s.inspectDurableReadRoots()
	if err != nil || !cfg.SupportsStagedSelection() {
		return errors.Join(ErrReadySelectionFrozen, err)
	}
	if authority, ok := remote.(interface{ CatalogMetadataAuthority() bool }); ok && authority.CatalogMetadataAuthority() {
		return ErrReadySelectionFrozen
	}
	state, err := s.loadStagedReleaseState(ctx, id)
	if err != nil {
		return err
	}
	if state.released {
		return nil
	}
	q := state.selection
	if state.pendingFound {
		if err := s.validateStagedSelection(ctx, state.pending); err != nil {
			return err
		}
		if err := s.verifyStagedRemote(ctx, remote, state.pending); err != nil {
			return err
		}
	}
	if !state.receiptFound {
		state.receipt = stagedReleaseReceipt{Version: 3, Selection: *q}
		state.receipt.SHA256, err = s.readyDigest(ctx, state.receipt)
		if err != nil {
			return err
		}
		if err = s.writeStagedControl(ctx, id, "release.json", state.receipt); err != nil {
			return err
		}
	}
	if !state.progressFound {
		state.progress = stagedReleaseProgress{Version: 3, ReceiptSHA256: state.receipt.SHA256, Phase: "verified"}
		if err = s.writeStagedControl(ctx, id, "release-progress.json", state.progress); err != nil {
			return err
		}
	}
	return config.WithDurableStorage(s.home, func(g config.DurableStorageGuard) error {
		return s.finishStagedRelease(ctx, id, q, state.progress, state.pending, state.pendingFound, g)
	})
}

func (s *Store) finishStagedRelease(ctx context.Context, id string, q *StagedSelection, progress stagedReleaseProgress, pending PendingPublication, found bool, g config.DurableStorageGuard) error {
	cfg, present, err := config.Load(s.home)
	if err != nil || !present || !cfg.SupportsStagedSelection() || cfg.DestinationID() != q.Core.DestinationID {
		return errors.Join(ErrReadySelectionFrozen, err)
	}
	// Lock spans the last request comparison, local progress, exact pending and
	// stage removal and directory sync; a hook cannot replace a token mid-unlink.
	unlock, err := local.NamedLockWait(s.home, requestLockName(id), time.Second)
	if err != nil {
		return err
	}
	defer unlock()
	current, requested, err := s.LoadRequest(id)
	if err != nil {
		return err
	}
	if requested && (!q.HadRequest || current.Token != q.CoveredToken) {
		return ErrReadySelectionFrozen
	}
	stage, staged, err := s.loadPreparedImportStage(ctx, id)
	if err != nil {
		return err
	}
	if err := s.validateFinalStagedAuthority(ctx, id, q, pending, found, stage, staged); err != nil {
		return err
	}
	if staged && (!stage.TypedSelection || stage.SHA256 != q.StageSHA256) {
		return ErrDurableStorageRecovery
	}
	if staged {
		if err := s.checkStagedChildHandoffs(ctx, stage, q); err != nil {
			return err
		}
	} else if progress.Phase != "local-complete" {
		return ErrDurableStorageRecovery
	}
	if requested {
		if err = s.removeCoveredStagedRequest(id); err != nil {
			return err
		}
	}
	progress.Phase = "local-complete"
	if err = s.writeDurableGuard(ctx, g, stagedControl(id, "release-progress.json"), progress); err != nil {
		return err
	}
	if found {
		if err = s.removeExactStagedPending(ctx, g, id, pending); err != nil {
			return err
		}
	}
	if staged {
		// Generic id-only stage cleanup refuses typed owners; this private route is
		// reachable only after persisted selecting proof, readback and locked token.
		if err = s.removeImportFile(id, "stage.json"); err != nil {
			return err
		}
	}
	progress.Phase = "released"
	return s.writeDurableGuard(ctx, g, stagedControl(id, "release-progress.json"), progress)
}

func (s *Store) writeStagedControl(ctx context.Context, id, name string, value any) error {
	if _, err := agentmeta.JSONWireBound(ctx, value, 16<<10); err != nil {
		return errors.Join(ErrDurableStorageCapacity, err)
	}
	return config.WithDurableStorage(s.home, func(g config.DurableStorageGuard) error {
		return s.writeDurableGuard(ctx, g, stagedControl(id, name), value)
	})
}
func (s *Store) removeCoveredStagedRequest(id string) error {
	home, err := local.OpenRootedHome(s.home)
	if err != nil {
		return err
	}
	defer home.Close()
	dir, err := privateDirectory(home.Root, "requests", false)
	if err != nil {
		return err
	}
	defer dir.Close()
	if err = dir.Remove(id + ".json"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	file, err := dir.Open(".")
	if err != nil {
		return err
	}
	return errors.Join(file.Sync(), file.Close())
}

// TypedStagedPublished is a dispatch hint only; cleanup still performs the full
// persisted proof/guard/readback checks above.
func (p *Published) TypedStagedPublished() bool {
	return p.state.LastPublished != nil && p.state.LastPublished.StagedSelection != nil
}

func (s *Store) removeExactStagedPending(ctx context.Context, g config.DurableStorageGuard, id string, p PendingPublication) error {
	if p.StagedSelection == nil || p.Catalog != nil || p.Bundle.ArchiveSessionID != id {
		return ErrReadySelectionFrozen
	}
	if err := s.requireStagedReleaseProgress(id, p.StagedSelection); err != nil {
		return err
	}
	if err := s.validateExactLegacyPending(ctx, id, p); err != nil {
		return err
	}
	digest, length, err := s.exactLegacyPendingWire(ctx, p)
	if err != nil {
		return err
	}
	return s.removeExactLegacyPendingGuard(ctx, g, id, digest, length)
}

// ResumeStagedReleases visits one bounded page following the existing admission
// cursor. Receipt-only entries survive payload unlink and remain explicit owners.
func (s *Store) ResumeStagedReleases(ctx context.Context, remote StagedReadback, guard *local.CollectorGuard) (map[string]error, error) {
	holds := make(map[string]error)
	cfg, present, err := config.Load(s.home)
	if err != nil {
		return holds, err
	}
	if !present || !cfg.StagedSelectionProtection {
		return holds, nil
	}
	if !cfg.SupportsStagedSelection() || guard == nil {
		return holds, ErrReadySelectionFrozen
	}
	var cursor struct {
		After string `json:"after"`
	}
	if err := s.readImportControl("import-stage-cursor.json", &cursor); err != nil && !errors.Is(err, os.ErrNotExist) {
		return holds, err
	}
	stages, _, closePage, err := s.ImportStagesOwned(ctx, cursor.After, 50)
	defer closePage()
	if err != nil {
		return holds, err
	}
	for _, stage := range stages {
		if err := ctx.Err(); err != nil {
			return holds, err
		}
		if !stage.TypedSelection {
			continue
		}
		id := stage.Registration.ArchiveSessionID
		published, err := s.LoadPublishedStateContext(ctx, id)
		if err != nil {
			holds[id] = err
			continue
		}
		if !published.TypedStagedPublished() {
			continue
		}
		if err := s.VerifyAndBeginStagedRelease(ctx, id, remote, guard); err != nil {
			holds[id] = err
		}
	}
	return holds, nil
}

type stagedReleaseState struct {
	selection                                           *StagedSelection
	receipt                                             stagedReleaseReceipt
	progress                                            stagedReleaseProgress
	pending                                             PendingPublication
	receiptFound, progressFound, pendingFound, released bool
}

func (s *Store) loadStagedReleaseState(ctx context.Context, id string) (out stagedReleaseState, err error) {
	published, err := s.LoadPublishedStateContext(ctx, id)
	if err != nil || published.state.LastPublished == nil || published.state.LastPublished.StagedSelection == nil {
		return out, errors.Join(ErrDurableStorageRecovery, err)
	}
	out.selection = published.state.LastPublished.StagedSelection
	err = s.readStagedControl(id, "release.json", &out.receipt)
	if err == nil {
		out.receiptFound = true
		if out.receipt.Selection != *out.selection {
			return out, ErrDurableStorageRecovery
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return out, err
	}
	err = s.readStagedControl(id, "release-progress.json", &out.progress)
	if err == nil {
		out.progressFound = true
		if !out.receiptFound || out.progress.ReceiptSHA256 != out.receipt.SHA256 {
			return out, ErrDurableStorageRecovery
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return out, err
	}
	if out.progressFound && out.progress.Phase == "released" {
		if err := s.checkReleasedStageAbsent(ctx, id); err != nil {
			return out, err
		}
		out.released = true
		return out, nil
	}
	out.pending, out.pendingFound, err = s.LoadPendingContext(ctx, id)
	if err != nil {
		return out, err
	}
	if out.pendingFound {
		if out.pending.StagedSelection == nil || *out.pending.StagedSelection != *out.selection || out.pending.Catalog != nil {
			return out, ErrDurableStorageRecovery
		}
	} else if !out.receiptFound || !out.progressFound || out.progress.Phase != "local-complete" {
		return out, ErrDurableStorageRecovery
	}
	return out, nil
}
func (s *Store) checkReleasedStageAbsent(ctx context.Context, id string) error {
	_, present, err := s.loadPreparedImportStage(ctx, id)
	if err != nil {
		return err
	}
	if present {
		return ErrReadySelectionFrozen
	}
	_, present, err = s.LoadRequest(id)
	if err != nil {
		return err
	}
	if present {
		return ErrReadySelectionFrozen
	}
	return nil
}

func (s *Store) checkStagedChildHandoffs(ctx context.Context, stage ImportStage, q *StagedSelection) error {
	id := stage.Registration.ArchiveSessionID
	for _, child := range stage.Children {
		reg, admitted, e := s.LoadRegistration(child.ArchiveSessionID)
		if e != nil {
			return e
		}
		if !admitted || reg.StagedImportSHA256 != child.StagedImportSHA256 || reg.ParentSessionID != id || reg.NativeSessionID != child.NativeSessionID || reg.ParentNativeSessionID != child.ParentNativeSessionID || reg.Harness.Name != child.Harness.Name || reg.ProjectID != child.ProjectID || reg.DestinationID != q.Core.DestinationID || !reg.InBatch(stage.Registration.ImportBatch.Recorded()) {
			return ErrReadySelectionFrozen
		}
		request, present, e := s.LoadRequest(child.ArchiveSessionID)
		if e != nil {
			return e
		}
		if present && request.Token != "" {
			continue
		}
		committed, e := s.LoadPublishedStateContext(ctx, child.ArchiveSessionID)
		if e != nil {
			return e
		}
		if committed.state.LastPublished == nil || committed.state.LastPublished.StagedSelection == nil || committed.state.LastPublished.StagedSelection.StageSHA256 != child.StagedImportSHA256 {
			return ErrReadySelectionFrozen
		}
		if err := s.requireStagedReleaseProgress(child.ArchiveSessionID, committed.state.LastPublished.StagedSelection); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) readStagedControl(id, name string, value any) (err error) {
	if !safeFileComponent(id) || (name != "release.json" && name != "release-progress.json") {
		return ErrDurableStorageRecovery
	}
	home, err := local.OpenRootedHome(s.home)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, home.Close()) }()
	root, err := privateDirectory(home.Root, filepath.Dir(stagedControl(id, name)), false)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	info, err := root.Lstat(name)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > 16<<10 || info.Size() < 1 {
		return ErrDurableStorageRecovery
	}
	reservation := s.resourceBudget.Reservation()
	n := info.Size()
	if !reservation.Reserve(3 * n) {
		return errStateBudget
	}
	keep := false
	defer func() {
		if keep && s.resourceBudget != nil {
			reservation.Release(2 * n)
			*s.resourceReleases = append(*s.resourceReleases, func() { reservation.Release(n) })
		} else {
			reservation.Release(3 * n)
		}
	}()
	file, err := root.Open(name)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	opened, err := file.Stat()
	if err != nil || !sameDurableStamp(info, opened) {
		return errors.Join(ErrDurableStorageRecovery, err)
	}
	decoder := json.NewDecoder(io.LimitReader(file, n+1))
	if err = decoder.Decode(value); err != nil {
		return err
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return ErrDurableStorageRecovery
	}
	named, err := root.Lstat(name)
	if err != nil || !sameDurableStamp(info, named) {
		return errors.Join(ErrDurableStorageRecovery, err)
	}
	err = errors.Join(s.durableContext().Err(), home.Check())
	keep = err == nil
	return err
}

func (s *Store) validateFinalStagedAuthority(ctx context.Context, id string, q *StagedSelection, pending PendingPublication, found bool, stage ImportStage, staged bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	cfg, present, err := config.Load(s.home)
	if err != nil || !present || !cfg.SupportsStagedSelection() || cfg.Paused || !cfg.Archive.Enabled {
		return errors.Join(ErrReadySelectionFrozen, err)
	}
	reg, registered, err := s.LoadRegistration(id)
	if err != nil || !registered || !cfg.AcceptSession(reg) || reg.StagedImportSHA256 != q.StageSHA256 {
		return errors.Join(ErrReadySelectionFrozen, err)
	}
	admission, err := s.readyDigest(ctx, reg)
	if err != nil || admission != q.Core.AdmissionContext {
		return errors.Join(ErrReadySelectionFrozen, err)
	}
	published, err := s.LoadPublishedStateContext(ctx, id)
	if err != nil || !published.TypedStagedPublished() || *published.state.LastPublished.StagedSelection != *q {
		return errors.Join(ErrReadySelectionFrozen, err)
	}
	retained, _, ok := published.state.resolveLastPublished()
	if !ok {
		return ErrDurableStorageRecovery
	}
	policy := PendingPublication{Bundle: retained, SkillEvidence: string(cfg.EffectiveSkillEvidence())}
	digest, err := s.readyDigest(ctx, readyPolicy(policy))
	if err != nil || digest != q.Core.PolicyContext || retained.Capture.FilterVersion != archive.FilterVersion {
		return errors.Join(ErrReadySelectionFrozen, err)
	}
	if found {
		current, present, err := s.LoadPendingContext(ctx, id)
		if err != nil || !present || current.StagedSelection == nil || *current.StagedSelection != *q {
			return errors.Join(ErrReadySelectionFrozen, err)
		}
		if err := s.validateStagedSelection(ctx, current); err != nil {
			return err
		}
		binding, err := s.readyBinding(ctx, pending)
		if err != nil || binding != q.Core.BindingSHA256 {
			return errors.Join(ErrReadySelectionFrozen, err)
		}
	}
	if staged {
		return s.validateFinalStagedRoster(ctx, stage, q, cfg)
	}
	return nil
}
func (s *Store) validateFinalStagedRoster(ctx context.Context, stage ImportStage, q *StagedSelection, cfg config.Config) error {
	if !stage.TypedSelection || stage.SHA256 != q.StageSHA256 || stage.SkillEvidence != cfg.EffectiveSkillEvidence() {
		return ErrReadySelectionFrozen
	}
	roster, err := s.readyDigest(ctx, stage.Children)
	if err != nil || roster != q.RosterSHA256 {
		return errors.Join(ErrReadySelectionFrozen, err)
	}
	bundle, err := s.readyDigest(ctx, stage.Bundle)
	if err != nil || bundle != q.Core.BundleSHA256 {
		return errors.Join(ErrReadySelectionFrozen, err)
	}
	return nil
}
func (s *Store) refuseTypedStagedRetirement(id string) error {
	cfg, present, err := config.Load(s.home)
	if err != nil {
		return err
	}
	if !present || !cfg.StagedSelectionProtection {
		return nil
	}
	stage, found, err := s.loadPreparedImportStage(s.durableContext(), id)
	if err != nil {
		return err
	}
	if found && stage.TypedSelection {
		return ErrReadySelectionFrozen
	}
	var receipt stagedReleaseReceipt
	if err := s.readStagedControl(id, "release.json", &receipt); err == nil {
		return ErrReadySelectionFrozen
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	published, err := s.LoadPublishedState(id)
	if err != nil {
		return err
	}
	if published.TypedStagedPublished() {
		return ErrReadySelectionFrozen
	}
	return nil
}
