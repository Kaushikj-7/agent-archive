package state

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
)

// nativeImportOwnerCommit is minted only from a held prepared source or an
// actual matching durable typed stage and import reservation during recovery.
// The generic registration APIs cannot supply this private capability.
type nativeImportOwnerCommit struct {
	ctx          context.Context
	store        *Store
	confirmation *ConfirmedNativeImportBatch
	stage        ImportStage
	detach       *nativeImportDetachProof
	reg          archive.SessionRegistration
}

func nativeImportCommitArgument(values []*nativeImportOwnerCommit) *nativeImportOwnerCommit {
	if len(values) != 1 {
		return nil
	}
	return values[0]
}

func (p *nativeImportOwnerCommit) check(previous, next archive.SessionRegistration, found bool) error {
	if p != nil && p.detach != nil {
		return p.detach.check(p.ctx, previous, next, found)
	}
	if p == nil || p.store == nil || p.confirmation == nil || found || p.stage.Version != 2 || !p.stage.TypedSelection || next.StagedImportSHA256 != p.stage.SHA256 || next.ValidateNativeImport() != nil {
		return ErrReadySelectionFrozen
	}
	if err := p.stage.Validate(p.ctx, next); err != nil {
		return err
	}
	a, err := p.store.readyDigest(p.ctx, p.reg)
	if err != nil {
		return err
	}
	b, err := p.store.readyDigest(p.ctx, next)
	if err != nil || a != b {
		return errors.Join(ErrReadySelectionFrozen, err)
	}
	return p.confirmation.checkOpen(p.store)
}

// CommitNativeImport persists one exact privacy-filtered stage before exposing
// first ownership. Native verification and every file sync occur outside hooks,
// batch and request locks; final comparisons use hooks→batch→request ordering.
func (s *Store) CommitNativeImport(ctx context.Context, p *PreparedNativeImport) (result archive.SessionRegistration, err error) {
	if p == nil || p.self != p || p.store != s || p.closed {
		return archive.SessionRegistration{}, agentapi.ErrClosed
	}
	if p.committed {
		return p.committedOwner()
	}
	if err := p.verify(ctx); err != nil {
		return archive.SessionRegistration{}, err
	}
	if err := p.allocateImportIdentity(ctx); err != nil {
		return archive.SessionRegistration{}, err
	}
	locks, err := s.prepareNativeImportLocks(ctx, p.confirmation.control.ID, p.reg.ArchiveSessionID)
	if err != nil {
		return archive.SessionRegistration{}, err
	}
	defer func() { err = errors.Join(err, locks.Close()) }()
	if err := p.persistImportStage(ctx, locks); err != nil {
		return archive.SessionRegistration{}, err
	}
	if err := s.indexStep("import-stage"); err != nil {
		return archive.SessionRegistration{}, err
	}
	if err := p.reserveImportIdentity(ctx, locks); err != nil {
		return archive.SessionRegistration{}, err
	}
	cap := &nativeImportOwnerCommit{ctx: ctx, store: s, confirmation: p.confirmation, stage: p.stage, reg: p.reg}
	if err := s.commitNativeImportOwner(ctx, cap, locks, p.index, func() error { return p.verify(ctx) }); err != nil {
		return archive.SessionRegistration{}, err
	}
	p.committed = true
	return p.committedOwner()
}

func (p *PreparedNativeImport) allocateImportIdentity(ctx context.Context) error {
	key := agentmeta.SessionKey{Agent: agentmeta.Muse, NativeID: p.candidate.NativeID}
	if err := p.store.generationReservationAllowed(key); err != nil {
		return err
	}
	if p.stage.SHA256 != "" && p.reg.ArchiveSessionID != "" {
		return p.reloadPreparedImportIndex(ctx, key)
	}
	id, err := local.ID()
	if err != nil {
		return err
	}
	receipt, err := local.ID()
	if err != nil {
		return err
	}
	at := time.Now().UTC()
	c := p.confirmation.control.NativeImport
	p.reg = archive.SessionRegistration{ArchiveSessionID: id, NativeSessionID: p.candidate.NativeID, Harness: archive.Harness{Name: "muse"}, ProjectRoot: p.candidate.ProjectRoot, ProjectID: p.candidate.ProjectID, TranscriptPath: p.binding.Path, DestinationID: c.Scope.DestinationID, SessionStartedAt: p.binding.NativeCreatedAt, RegisteredAt: at, AdmittedAt: at, Origin: archive.SessionOriginImport, ImportBatch: archive.NewImportBatch(c.Scope.BatchID), StartedAtSource: archive.StartedAtSourceTranscript, NativeBinding: &p.binding, NativeImport: &archive.NativeImport{Contract: "muse-historical-import-v1", Confirmation: archive.NativeImportConfirmation{BatchID: c.Scope.BatchID, BatchInstance: c.Instance, ScopeSHA256: c.ScopeSHA256, DestinationID: c.Scope.DestinationID, ProjectID: p.candidate.ProjectID, RootFingerprint: p.selected.RootFingerprint, SelectedBindingSHA256: p.selected.BindingSHA256, SelectedSource: p.selected.Source}, Membership: archive.NativeImportMembership{ReceiptID: receipt, State: archive.NativeImportAttached, Effects: []string{"registration"}}}}
	if err := p.reg.Validate(); err != nil {
		return err
	}
	p.index, p.indexFound, err = p.store.readQualifiedIndex(key)
	if err != nil {
		return err
	}
	if p.indexFound && !p.index.Absent {
		expected, err := p.reservationAuthority()
		if err != nil || p.index.NativeReservation != expected || !supportedNativeReservationEntry(p.index) {
			return errors.Join(ErrSessionIdentityConflict, err)
		}
		p.reg.ArchiveSessionID = p.index.ArchiveSessionID
		stage, found, err := p.store.loadPreparedImportStage(ctx, p.index.ArchiveSessionID)
		if err != nil {
			return err
		}
		if found {
			authority, err := nativeImportReservationAuthority(stage.Registration)
			if err != nil || authority != expected || stage.Version != 2 || !stage.TypedSelection {
				return errors.Join(ErrReadySelectionFrozen, err)
			}
			current, owned, err := p.store.LoadRegistration(p.index.ArchiveSessionID)
			if err != nil || owned && (!sameImportStageOwner(stage.Registration, current) || !sameNativeImportReceipt(stage.Registration.NativeImport, current.NativeImport) || current.StagedImportSHA256 != stage.SHA256) {
				return errors.Join(ErrSessionIdentityConflict, err)
			}
			p.stage = stage
			p.reg = stage.Registration
			p.reg.StagedImportSHA256 = stage.SHA256
			return nil
		}
		if _, owned, err := p.store.LoadRegistration(p.index.ArchiveSessionID); err != nil || owned {
			return errors.Join(ErrSessionIdentityConflict, err)
		}
	} else if err := p.store.sessionIndexMissAllowed(); err != nil {
		return err
	}
	if !p.indexFound || p.index.Absent {
		if _, owned, err := p.store.ArchiveSessionID(key); err != nil || owned {
			return errors.Join(ErrSessionIdentityConflict, err)
		}
	}
	cfg, found, err := p.store.loadConfig()
	if err != nil || !found {
		return errors.Join(ErrReadySelectionFrozen, err)
	}
	// Charge the independently decoded filtered bundle to the same retained
	// result ledger before allocating it; wire and native leases remain distinct.
	bundleBytes := int64(32 << 10)
	for _, record := range p.filtered.Records {
		bundleBytes += 3*int64(len(record)) + 128
	}
	for _, text := range p.filtered.Text {
		bundleBytes += 3*int64(len(text)) + 128
	}
	reservation := p.read.readReservation()
	if !reservation.Reserve(bundleBytes) {
		return errStateBudget
	}
	*p.read.resourceReleases = append(*p.read.resourceReleases, func() { reservation.Release(bundleBytes) })
	bundle, err := archive.NewSourceBundle(p.reg, p.adapter, p.filtered, p.reg.AdmittedAt, nil)
	if err != nil {
		return err
	}
	stage := ImportStage{Version: 2, TypedSelection: true, Registration: p.reg, Bundle: bundle, SkillEvidence: cfg.EffectiveSkillEvidence()}
	stage.SHA256, err = stage.digest(ctx)
	if err != nil {
		return err
	}
	p.reg.StagedImportSHA256 = stage.SHA256
	if err := stage.Validate(ctx, p.reg); err != nil {
		return err
	}
	if err := validateImportStageCapacity(ctx, stage); err != nil {
		return err
	}
	p.stage = stage
	return nil
}

func (p *PreparedNativeImport) reserveImportIdentity(ctx context.Context, locks *local.RootedHome) error {
	s := p.store
	key := agentmeta.SessionKey{Agent: agentmeta.Muse, NativeID: p.candidate.NativeID}
	if p.indexFound && p.index == indexEntry(key, p.reg.ArchiveSessionID) {
		owner, owned, err := s.LoadRegistration(p.reg.ArchiveSessionID)
		if err != nil || !owned || !sameImportStageOwner(p.reg, owner) || !sameNativeImportReceipt(p.reg.NativeImport, owner.NativeImport) {
			return errors.Join(ErrSessionIdentityConflict, err)
		}
		return nil
	}
	expected := p.index
	next := indexEntry(key, p.reg.ArchiveSessionID)
	authority, err := p.reservationAuthority()
	if err != nil {
		return err
	}
	next.NativeReservation = authority
	if p.indexFound && p.index.NativeReservation != "" {
		next = p.index
	} else {
		next.Reservation, err = local.ID()
		if err != nil {
			return err
		}
	}
	indexStore := *s
	indexStore.indexSnapshots = true
	indexStore.onWriteSync = s.onIndexSync
	err = indexStore.writeUnderLock(lockedWrite{nativeImportReservation: p, path: qualifiedSessionIndexPath(s.home, key), lock: func() (func(), error) {
		return s.lockNativeImport(ctx, locks, p.confirmation.control.ID, p.reg.ArchiveSessionID)
	}, beforeLock: func() error { return p.verify(ctx) }, check: func() error {
		if err := p.checkImportHeld(ctx, locks); err != nil {
			return err
		}
		stage, found, err := s.LoadImportStage(ctx, p.reg)
		if err != nil || !found || stage.SHA256 != p.stage.SHA256 || stage.Validate(ctx, p.reg) != nil {
			return errors.Join(ErrReadySelectionFrozen, err)
		}
		return nil
	}, change: func(current fileSnapshot) (any, bool, error) {
		if current.found != p.indexFound {
			return nil, false, errIndexMoved
		}
		if current.found {
			var prior qualifiedSessionIndexEntry
			if err := json.Unmarshal(current.data, &prior); err != nil || prior != expected {
				return nil, false, errors.Join(errIndexMoved, err)
			}
		}
		owner, owned, err := s.LoadRegistration(p.reg.ArchiveSessionID)
		if err != nil || owned && (!sameImportStageOwner(p.reg, owner) || !sameNativeImportReceipt(p.reg.NativeImport, owner.NativeImport) || owner.StagedImportSHA256 != p.stage.SHA256) {
			return nil, false, errors.Join(ErrSessionIdentityConflict, err)
		}
		return next, next != expected || !p.indexFound, nil
	}})
	if err != nil {
		return err
	}
	p.index, p.indexFound = next, true
	return s.indexStep("import-reservation")
}

func (p *PreparedNativeImport) checkImportHeld(ctx context.Context, held *local.RootedHome) error {
	read, closeRead := p.confirmation.read.WithReadBudget(ctx, p.confirmation.read.resourceBudget)
	defer closeRead()
	if err := p.confirmation.checkCurrentHeld(ctx, p.store, read, held); err != nil {
		return err
	}
	if err := p.checkPhysicalProject(); err != nil {
		return err
	}
	return errors.Join(ctx.Err(), held.Check())
}

func (p *PreparedNativeImport) persistImportStage(ctx context.Context, locks *local.RootedHome) error {
	read, ctx, closeRead := p.store.withDurableContext(ctx)
	defer closeRead()
	closeWire, err := read.ownStageValidationWire(ctx, p.stage)
	if err != nil {
		return err
	}
	defer closeWire()
	if err := p.verify(ctx); err != nil {
		return err
	}
	existing, found, err := read.LoadImportStage(ctx, p.reg)
	if err != nil {
		return err
	}
	if found {
		if existing.SHA256 != p.stage.SHA256 {
			return ErrReadySelectionFrozen
		}
		return nil
	}
	return read.withImportStaging(ctx, func(g config.DurableStorageGuard) error {
		return read.writeDurableGuardWitnessed(ctx, g, importStageRelative(p.reg.ArchiveSessionID), p.stage, func(witness *local.AtomicWriteWitness, rename func() error) error {
			if err := p.verify(ctx); err != nil {
				return err
			}
			unlock, err := p.store.lockNativeImport(ctx, locks, p.confirmation.control.ID, p.reg.ArchiveSessionID)
			if err != nil {
				return err
			}
			defer unlock()
			scoped := *read
			scoped.atomicWitness = witness
			if err := p.checkImportHeld(ctx, locks); err != nil {
				return err
			}
			key := agentmeta.SessionKey{Agent: agentmeta.Muse, NativeID: p.candidate.NativeID}
			current, found, err := scoped.readQualifiedIndex(key)
			if err != nil || found != p.indexFound || found && current != p.index {
				return errors.Join(errIndexMoved, err)
			}
			if _, found, err := scoped.LoadRegistration(p.reg.ArchiveSessionID); err != nil || found {
				return errors.Join(ErrSessionIdentityConflict, err)
			}
			if _, found, err := scoped.LoadImportStage(ctx, p.reg); err != nil || found {
				return errors.Join(ErrReadySelectionFrozen, err)
			}
			if err := errors.Join(ctx.Err(), locks.Check()); err != nil {
				return err
			}
			return rename()
		})
	})
}

// Prepare every lock namespace before hooks can be held. Later acquisition uses
// only the retained rooted handle and existing regular, private lock files.
func (s *Store) prepareNativeImportLocks(ctx context.Context, batch, id string) (held *local.RootedHome, err error) {
	if !archive.ValidImportBatchID(batch) || !safeFileComponent(id) {
		return nil, ErrReadySelectionFrozen
	}
	held, err = local.OpenRootedHome(s.home)
	if err != nil {
		return nil, err
	}
	keep := false
	defer func() {
		if !keep {
			err = errors.Join(err, held.Close())
		}
	}()
	for _, name := range []string{importLocksDirectory, "request-locks"} {
		dir, e := privateDirectory(held.Root, name, true)
		if e != nil {
			return nil, e
		}
		if e := dir.Close(); e != nil {
			return nil, e
		}
	}
	for _, name := range []string{filepath.Join(importLocksDirectory, batch+".lock"), requestLockName(id)} {
		unlock, e := local.RootedLockWait(held, name, time.Second)
		if e != nil {
			return nil, e
		}
		if e := checkNativeImportLock(held, name); e != nil {
			unlock()
			return nil, e
		}
		unlock()
	}
	if err := errors.Join(ctx.Err(), held.Check()); err != nil {
		return nil, err
	}
	keep = true
	return held, nil
}

func (s *Store) lockNativeImport(ctx context.Context, held *local.RootedHome, batch, id string) (func(), error) {
	hooks, err := s.namedLockWait("hooks.lock", time.Second)
	if err != nil {
		return nil, err
	}
	batchUnlock, err := local.RootedLockWait(held, filepath.Join(importLocksDirectory, batch+".lock"), time.Second)
	if err != nil {
		hooks()
		return nil, err
	}
	request, err := local.RootedLockWait(held, requestLockName(id), time.Second)
	if err != nil {
		batchUnlock()
		hooks()
		return nil, err
	}
	unlock := func() { request(); batchUnlock(); hooks() }
	for _, name := range []string{filepath.Join(importLocksDirectory, batch+".lock"), requestLockName(id)} {
		if err := checkNativeImportLock(held, name); err != nil {
			unlock()
			return nil, err
		}
	}
	if err := errors.Join(ctx.Err(), held.Check()); err != nil {
		unlock()
		return nil, err
	}
	return unlock, nil
}

func (s *Store) commitNativeImportOwner(ctx context.Context, cap *nativeImportOwnerCommit, locks *local.RootedHome, reserved qualifiedSessionIndexEntry, verify func() error) error {
	read, ctx, closeRead := s.withDurableContext(ctx)
	defer closeRead()
	key := agentmeta.SessionKey{Agent: agentmeta.Muse, NativeID: cap.reg.NativeSessionID}
	lock := func() (func(), error) {
		return s.lockNativeImport(ctx, locks, cap.reg.NativeImport.Confirmation.BatchID, cap.reg.ArchiveSessionID)
	}
	check := func() error { return cap.checkCurrentHeld(ctx, read, locks, reserved) }
	err := read.writeUnderLock(lockedWrite{nativeImport: cap, path: s.registrationPath(cap.reg.ArchiveSessionID), lock: lock, beforeLock: verify, check: check, change: func(current fileSnapshot) (any, bool, error) {
		if current.found {
			var reg archive.SessionRegistration
			if err := archive.DecodeSessionRegistration(current.data, &reg); err != nil {
				return nil, false, err
			}
			if !sameImportStageOwner(cap.reg, reg) || !sameNativeImportReceipt(cap.reg.NativeImport, reg.NativeImport) || reg.StagedImportSHA256 != cap.stage.SHA256 {
				return nil, false, ErrSessionIdentityConflict
			}
			return nil, false, nil
		}
		return cap.reg, true, nil
	}})
	if err != nil {
		return err
	}
	if err := s.indexStep("import-registration"); err != nil {
		return err
	}
	// Finishing uses the same batch→request lock and exact current proof. Generic
	// recovery/packing has no capability to consume an import marker.
	indexStore := *read
	indexStore.indexSnapshots = true
	indexStore.onWriteSync = s.onIndexSync
	err = indexStore.writeUnderLock(lockedWrite{nativeImport: cap, path: qualifiedSessionIndexPath(s.home, key), lock: lock, check: check, change: func(current fileSnapshot) (any, bool, error) {
		if !current.found {
			return nil, false, errIndexMoved
		}
		var entry qualifiedSessionIndexEntry
		if err := json.Unmarshal(current.data, &entry); err != nil {
			return nil, false, err
		}
		if entry == indexEntry(key, cap.reg.ArchiveSessionID) {
			return nil, false, nil
		}
		if entry != reserved {
			return nil, false, errIndexMoved
		}
		return indexEntry(key, cap.reg.ArchiveSessionID), true, nil
	}})
	if err != nil {
		return err
	}
	if err := s.indexStep("import-committed"); err != nil {
		return err
	}
	// A later independent request is merged, never replaced or acknowledged by
	// registration completion. The exact creating-batch policy is rechecked.
	return read.writeUnderLock(lockedWrite{path: s.requestPath(cap.reg.ArchiveSessionID), lock: lock, check: check, change: func(current fileSnapshot) (any, bool, error) {
		return mergeRequest(cap.reg.ArchiveSessionID, current, "backfill", cap.reg.AdmittedAt, false, nil)
	}})
}

func (p *nativeImportOwnerCommit) checkCurrentHeld(ctx context.Context, read *Store, held *local.RootedHome, reserved qualifiedSessionIndexEntry) error {
	if err := p.confirmation.checkCurrentHeld(ctx, p.store, read, held); err != nil {
		return err
	}
	if err := p.confirmation.control.MatchesRegistration(ctx, p.reg); err != nil {
		return err
	}
	cfg, found, err := read.loadConfig()
	if err != nil || !found || nativeImportStagedPolicyMismatch(cfg, p.reg, found) || nativeImportConfiguredRoot(cfg, p.reg) != nil {
		return errors.Join(ErrReadySelectionFrozen, err)
	}
	key := agentmeta.SessionKey{Agent: agentmeta.Muse, NativeID: p.reg.NativeSessionID}
	current, found, err := read.readQualifiedIndex(key)
	if err != nil || !found || current != reserved && current != indexEntry(key, p.reg.ArchiveSessionID) {
		return errors.Join(errIndexMoved, err)
	}
	owner, found, err := read.LoadRegistration(p.reg.ArchiveSessionID)
	if err != nil {
		return err
	}
	if found && (!sameImportStageOwner(p.reg, owner) || !sameNativeImportReceipt(p.reg.NativeImport, owner.NativeImport) || owner.StagedImportSHA256 != p.stage.SHA256) {
		return ErrSessionIdentityConflict
	}
	stage, found, err := read.LoadImportStage(ctx, p.reg)
	if err != nil || !found || stage.SHA256 != p.stage.SHA256 || stage.Validate(ctx, p.reg) != nil {
		return errors.Join(ErrReadySelectionFrozen, err)
	}
	return errors.Join(ctx.Err(), held.Check())
}

// resumeNativeImportFirstOwner needs the actual disk stage, matching durable
// reservation and current closed confirmation. It never opens native input or
// reconstructs a receipt from a caller-provided registration.
func (s *Store) resumeNativeImportFirstOwner(ctx context.Context, stage ImportStage) (err error) {
	reg := stage.Registration
	reg.StagedImportSHA256 = stage.SHA256
	if err := stage.Validate(ctx, reg); err != nil {
		return err
	}
	if !stage.TypedSelection || stage.Version != 2 || reg.ValidateNativeImport() != nil {
		return ErrReadySelectionFrozen
	}
	key := agentmeta.SessionKey{Agent: agentmeta.Muse, NativeID: reg.NativeSessionID}
	entry, found, err := s.readQualifiedIndex(key)
	if err != nil {
		return err
	}
	absent := !found || entry.Absent
	if !absent && (entry.ArchiveSessionID != reg.ArchiveSessionID || !supportedNativeReservationEntry(entry)) {
		return ErrReadySelectionFrozen
	}
	authority, err := nativeImportReservationAuthority(reg)
	if err != nil || !absent && authority != entry.NativeReservation {
		return errors.Join(ErrReadySelectionFrozen, err)
	}
	confirmation, err := s.OpenConfirmedNativeImportBatch(ctx, reg.NativeImport.Confirmation.BatchID)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, confirmation.Close()) }()
	if err := confirmation.control.MatchesRegistration(ctx, reg); err != nil {
		return err
	}
	locks, err := s.prepareNativeImportLocks(ctx, reg.NativeImport.Confirmation.BatchID, reg.ArchiveSessionID)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, locks.Close()) }()
	cap := &nativeImportOwnerCommit{ctx: ctx, store: s, confirmation: confirmation, stage: stage, reg: reg}
	if absent {
		entry, err = s.reserveInitialNativeImportStage(ctx, cap, locks, entry, found)
		if err != nil {
			return err
		}
	}
	verify := func() error {
		if err := confirmation.checkCurrent(ctx, s); err != nil {
			return err
		}
		actual, found, err := s.LoadImportStage(ctx, reg)
		if err != nil || !found || actual.SHA256 != stage.SHA256 || actual.Validate(ctx, reg) != nil {
			return errors.Join(ErrReadySelectionFrozen, err)
		}
		return ctx.Err()
	}
	return s.commitNativeImportOwner(ctx, cap, locks, entry, verify)
}

func (p *PreparedNativeImport) committedOwner() (archive.SessionRegistration, error) {
	current, found, err := p.store.LoadRegistration(p.reg.ArchiveSessionID)
	if err != nil || !found || !sameImportStageOwner(p.reg, current) || !sameNativeImportReceipt(p.reg.NativeImport, current.NativeImport) || current.StagedImportSHA256 != p.stage.SHA256 {
		return archive.SessionRegistration{}, errors.Join(ErrSessionIdentityConflict, err)
	}
	return current, nil
}

func (p *nativeImportOwnerCommit) reservationAuthority() (string, error) {
	if p == nil || p.store == nil || p.confirmation == nil || p.stage.Version != 2 || !p.stage.TypedSelection || p.stage.Canceled || p.stage.CancellationOnly || p.stage.ReleaseOnly {
		return "", ErrReadySelectionFrozen
	}
	if err := p.stage.Validate(p.ctx, p.reg); err != nil {
		return "", err
	}
	return nativeImportReservationAuthority(p.reg)
}

// Stage-first crash recovery may reserve only an authoritatively absent native
// identity. No caller receipt, missing source or generic marker grants this port.
func (s *Store) reserveInitialNativeImportStage(ctx context.Context, cap *nativeImportOwnerCommit, locks *local.RootedHome, before qualifiedSessionIndexEntry, found bool) (qualifiedSessionIndexEntry, error) {
	key := agentmeta.SessionKey{Agent: agentmeta.Muse, NativeID: cap.reg.NativeSessionID}
	next := indexEntry(key, cap.reg.ArchiveSessionID)
	var err error
	next.NativeReservation, err = cap.reservationAuthority()
	if err != nil {
		return next, err
	}
	next.Reservation, err = local.ID()
	if err != nil {
		return next, err
	}
	verify := func() error { return cap.checkInitialStage(ctx, locks, before, found) }
	if err := verify(); err != nil {
		return next, err
	}
	read, ctx, closeRead := s.withDurableContext(ctx)
	defer closeRead()
	indexStore := *read
	indexStore.indexSnapshots = true
	indexStore.onWriteSync = s.onIndexSync
	err = indexStore.writeUnderLock(lockedWrite{nativeImportStageReservation: cap, path: qualifiedSessionIndexPath(s.home, key), beforeLock: verify, lock: func() (func(), error) {
		return s.lockNativeImport(ctx, locks, cap.reg.NativeImport.Confirmation.BatchID, cap.reg.ArchiveSessionID)
	}, check: verify, change: func(current fileSnapshot) (any, bool, error) {
		if current.found != found {
			return nil, false, errIndexMoved
		}
		if current.found {
			var entry qualifiedSessionIndexEntry
			if err := json.Unmarshal(current.data, &entry); err != nil || entry != before {
				return nil, false, errors.Join(errIndexMoved, err)
			}
		}
		return next, true, nil
	}})
	return next, err
}

func (p *nativeImportOwnerCommit) checkInitialStage(ctx context.Context, held *local.RootedHome, before qualifiedSessionIndexEntry, expectedFound bool) error {
	read, closeRead := p.confirmation.read.WithReadBudget(ctx, p.confirmation.read.resourceBudget)
	defer closeRead()
	if _, err := p.reservationAuthority(); err != nil {
		return err
	}
	if err := p.confirmation.checkCurrentHeld(ctx, p.store, read, held); err != nil {
		return err
	}
	if err := p.confirmation.control.MatchesRegistration(ctx, p.reg); err != nil {
		return err
	}
	cfg, present, err := read.loadConfig()
	if err != nil || nativeImportStagedPolicyMismatch(cfg, p.reg, present) || p.stage.SkillEvidence != cfg.EffectiveSkillEvidence() {
		return errors.Join(ErrReadySelectionFrozen, err)
	}
	key := agentmeta.SessionKey{Agent: agentmeta.Muse, NativeID: p.reg.NativeSessionID}
	if err := read.generationReservationAllowed(key); err != nil {
		return err
	}
	entry, found, err := read.readQualifiedIndex(key)
	if err != nil || found != expectedFound || found && (entry != before || !entry.Absent) {
		return errors.Join(errIndexMoved, err)
	}
	if _, owned, err := read.ArchiveSessionID(key); err != nil || owned {
		return errors.Join(ErrSessionIdentityConflict, err)
	}
	if err := read.sessionIndexMissAllowed(); err != nil {
		return err
	}
	if _, owned, err := read.LoadRegistration(p.reg.ArchiveSessionID); err != nil || owned {
		return errors.Join(ErrSessionIdentityConflict, err)
	}
	actual, found, err := read.loadPreparedImportStage(ctx, p.reg.ArchiveSessionID)
	if err != nil || !found || actual.Canceled || actual.CancellationOnly || actual.ReleaseOnly || actual.SHA256 != p.stage.SHA256 || actual.Validate(ctx, p.reg) != nil {
		return errors.Join(ErrReadySelectionFrozen, err)
	}
	published, err := read.LoadPublishedStateContext(ctx, p.reg.ArchiveSessionID)
	if err != nil || published.found {
		return errors.Join(ErrReadySelectionFrozen, err)
	}
	if _, pending, err := read.LoadPendingContext(ctx, p.reg.ArchiveSessionID); err != nil || pending {
		return errors.Join(ErrReadySelectionFrozen, err)
	}
	for _, name := range []stagedControlName{stagedReleaseReceiptName, stagedReleaseProgressName} {
		if _, err := held.Root.Lstat(stagedControl(p.reg.ArchiveSessionID, name)); !errors.Is(err, os.ErrNotExist) {
			return errors.Join(ErrReadySelectionFrozen, err)
		}
	}
	return errors.Join(ctx.Err(), held.Check())
}

func checkNativeImportLock(held *local.RootedHome, name string) error {
	info, err := held.Root.Lstat(name)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() != 0 {
		return errors.Join(ErrDurableStorageRecovery, err)
	}
	return nil
}

func (p *PreparedNativeImport) reloadPreparedImportIndex(ctx context.Context, key agentmeta.SessionKey) error {
	entry, found, err := p.store.readQualifiedIndex(key)
	if err != nil {
		return err
	}
	if !found || entry.Absent {
		if _, owned, err := p.store.ArchiveSessionID(key); err != nil || owned {
			return errors.Join(ErrSessionIdentityConflict, err)
		}
		if _, owned, err := p.store.LoadRegistration(p.reg.ArchiveSessionID); err != nil || owned {
			return errors.Join(ErrSessionIdentityConflict, err)
		}
		if err := p.store.sessionIndexMissAllowed(); err != nil {
			return err
		}
	} else {
		owner, owned, err := p.store.LoadRegistration(p.reg.ArchiveSessionID)
		if err != nil || entry.ArchiveSessionID != p.reg.ArchiveSessionID || owned && (!sameImportStageOwner(p.reg, owner) || !sameNativeImportReceipt(p.reg.NativeImport, owner.NativeImport)) {
			return errors.Join(ErrSessionIdentityConflict, err)
		}
		if entry.NativeReservation != "" {
			authority, err := p.reservationAuthority()
			if err != nil || authority != entry.NativeReservation || !supportedNativeReservationEntry(entry) {
				return errors.Join(errUnsupportedNativeReservation, err)
			}
		} else if !owned || entry != indexEntry(key, p.reg.ArchiveSessionID) {
			return ErrSessionIdentityConflict
		}
	}
	actual, staged, err := p.store.LoadImportStage(ctx, p.reg)
	if err != nil || staged && actual.SHA256 != p.stage.SHA256 {
		return errors.Join(ErrReadySelectionFrozen, err)
	}
	p.index, p.indexFound = entry, found
	return nil
}
