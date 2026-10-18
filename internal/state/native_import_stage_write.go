package state

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
)

// This preparation can never enter generic durable writers: its quota object
// borrows only the existing usage/accounting methods and owns no ordinary guard.
type nativeImportStageQuota struct {
	quota       *durableQuota
	preparation config.NativeImportStoragePreparation
	held        bool
}

func (q *nativeImportStageQuota) acquire() error {
	if q.held {
		return nil
	}
	unlock, err := local.RootedLockWait(q.quota.home, "temporary-quota", time.Second)
	if err != nil {
		return err
	}
	q.quota.unlock = unlock
	q.held = true
	return nil
}
func (q *nativeImportStageQuota) release() {
	if q.held {
		q.quota.unlock()
		q.quota.unlock = nil
		q.held = false
	}
}

func (p *PreparedNativeImport) writeInitialNativeImportStage(ctx context.Context, read *Store, locks *local.RootedHome, preparation config.NativeImportStoragePreparation) (err error) {
	home, err := preparation.RootedHome(p.store.home)
	if err != nil {
		return err
	}
	q := &nativeImportStageQuota{quota: &durableQuota{store: read, home: home, path: p.store.home}, preparation: preparation}
	if err := q.acquire(); err != nil {
		return err
	}
	defer q.release()
	owner := read.readReservation()
	const scratchBytes int64 = 4096
	n, err := agentmeta.JSONWireBound(ctx, p.stage, MaxImportStageBytes)
	if err != nil {
		return err
	}
	n++
	if !owner.Reserve(n + scratchBytes) {
		return errStateBudget
	}
	defer owner.Release(n + scratchBytes)
	path := importStageRelative(p.reg.ArchiveSessionID)
	usage, err := q.quota.usageContext(ctx)
	if err != nil {
		return err
	}
	if _, err := home.Root.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		return errors.Join(ErrReadySelectionFrozen, err)
	}
	additional, err := q.quota.controlDirectoryAdditional(path, n, 2*n)
	if err != nil {
		return err
	}
	if additional > durableStorageQuota-usage.charged {
		return ErrDurableStorageCapacity
	}
	dir, err := privateDirectory(home.Root, filepath.Dir(path), true)
	if err != nil {
		return err
	}
	if err := dir.Close(); err != nil {
		return err
	}
	if err := errors.Join(ctx.Err(), preparation.CheckHome(p.store.home)); err != nil {
		return err
	}
	encode := func(w io.Writer) error {
		bounded := &boundedDurableWriter{writer: w, remaining: n}
		if err := json.NewEncoder(bounded).Encode(p.stage); err != nil {
			return err
		}
		return errors.Join(ctx.Err(), preparation.CheckHome(p.store.home))
	}
	return local.RootedAtomicWritePreverifiedWitnessed(ctx, home.Root, path, encode, make([]byte, scratchBytes), func(witness *local.AtomicWriteWitness, verify func() (func() error, error)) (err error) {
		// The complete synced temporary is already physically charged 2*n plus the
		// directory allowance. No reservation credit or accounting exception exists.
		q.release()
		// Restoration also handles verification/cancellation failures before policy
		// locks. On restoration failure the local writer preserves its charged temp.
		defer func() { err = errors.Join(err, q.acquire()) }()
		p.store.writeSynced()
		if err := p.verify(ctx); err != nil {
			return err
		}
		rename, err := verify()
		if err != nil {
			return err
		}
		hooks, err := p.store.namedLockWait("hooks.lock", time.Second)
		if err != nil {
			return err
		}
		defer hooks()
		if err := q.acquire(); err != nil {
			return err
		}
		unlock, err := p.store.lockNativeImportAfterHooks(ctx, locks, p.confirmation.control.ID, p.reg.ArchiveSessionID)
		if err != nil {
			return err
		}
		defer unlock()
		if err := p.checkInitialNativeImportStageRename(ctx, read, locks, witness, preparation); err != nil {
			return err
		}
		// The target remains absent. Rename replaces the name of the same complete
		// charged file, so no quota growth or second admission charge occurs.
		return rename()
	}, q.acquire)
}

func (p *PreparedNativeImport) checkInitialNativeImportStageRename(ctx context.Context, read *Store, locks *local.RootedHome, witness *local.AtomicWriteWitness, preparation config.NativeImportStoragePreparation) error {
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
	home, err := preparation.RootedHome(p.store.home)
	if err != nil {
		return err
	}
	dir, err := home.Root.Open(filepath.Dir(importStageRelative(p.reg.ArchiveSessionID)))
	if err != nil {
		return err
	}
	entries, readErr := dir.ReadDir(2)
	closeErr := dir.Close()
	if readErr != nil && !errors.Is(readErr, io.EOF) || closeErr != nil || len(entries) != 1 {
		return errors.Join(ErrDurableStorageRecovery, readErr, closeErr)
	}
	info, err := entries[0].Info()
	if err != nil {
		return err
	}
	matched, err := witness.Match(home.Root, filepath.Join(filepath.Dir(importStageRelative(p.reg.ArchiveSessionID)), entries[0].Name()), info)
	if err != nil || !matched {
		return errors.Join(ErrDurableStorageRecovery, err)
	}
	return errors.Join(ctx.Err(), locks.Check(), preparation.CheckHome(p.store.home))
}
