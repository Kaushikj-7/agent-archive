package state

import (
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
)

// FinishDeletedSession removes only exact checksum-owned deletion stages after
// the normal local generation/request/index retirement. Interrupted cleanup
// retains the journal, including when the registration has already gone.
func (s *Store) FinishDeletedSession(regID string) error {
	j, found, err := s.loadSessionDeletion(regID)
	if err != nil || !found {
		return err
	}
	if j.Phase != DeletionCleaned && j.Phase != DeletionFinalizing {
		return ErrRemovalPending
	}
	if _, found, err := s.LoadRegistration(regID); err != nil || found {
		return errors.Join(ErrRemovalPending, err)
	}
	if _, found, err := s.LoadRequest(regID); err != nil || found {
		return errors.Join(ErrDeletionWorkChanged, err)
	}
	if j.Phase == DeletionCleaned {
		j.Phase = DeletionFinalizing
		if err := s.saveSessionDeletion(j, nil); err != nil {
			return err
		}
	}
	if err := s.removeDeletionStages(j); err != nil {
		return err
	}
	// Every retained local publication/scan/stage/scratch obligation must be gone
	// before terminal bookkeeping. Unexpected ownership is recovery, not garbage.
	if pending, err := s.HasPending(regID); err != nil || pending {
		return errors.Join(ErrDurableStorageRecovery, err)
	}
	j, found, err = s.loadSessionDeletion(regID)
	if err != nil || !found {
		return errors.Join(ErrDurableStorageRecovery, err)
	}
	j.LocalRemoved = true
	j.Phase = DeletionCleaned
	j.MetadataBytes = nil
	return s.saveSessionDeletion(j, nil)
}

// RemoveRestoredDeletionStages releases exact restoration inputs only after the
// selecting journal was removed through its ordinary completion proof.
func (s *Store) RemoveRestoredDeletionStages(regID string) error {
	j, found, err := s.loadSessionDeletion(regID)
	if err != nil || !found {
		return err
	}
	if j.Phase != DeletionRestored && j.Phase != DeletionCancelled {
		return ErrRemovalPending
	}
	if _, err := os.Lstat(s.pendingPath(regID)); !errors.Is(err, os.ErrNotExist) {
		return errors.Join(ErrRemovalPending, err)
	}
	if err := s.removeDeletionStages(j); err != nil {
		return err
	}
	// A completed restoration leaves no replay authority behind. Published5 keeps
	// its selecting facts; any copied pending restoration now lacks its intent.
	return config.WithDeletionProtection(s.home, func(g config.DurableStorageGuard) (err error) {
		home, err := g.RootedHome(s.home)
		if err != nil {
			return err
		}
		dir, err := privateDirectory(home.Root, filepath.Join(deletionDirectory, regID), false)
		if err != nil {
			return err
		}
		defer func() { err = errors.Join(err, dir.Close()) }()
		raw, err := s.readDeletionObject(home, dir, "intent.json", deletionWireLimit)
		if err != nil {
			return err
		}
		var current SessionDeletion
		if err := s.unmarshalOwned(raw, &current); err != nil {
			return err
		}
		current.MetadataBytes = j.MetadataBytes
		if current.SHA256 != j.SHA256 || current.validate(regID) != nil {
			return ErrDurableStorageRecovery
		}
		if err = checkDeletionDirectory(home, regID, dir); err != nil {
			return err
		}
		if err = dir.Remove("intent.json"); err != nil {
			return err
		}
		if err = syncDurableParent(dir); err != nil {
			return err
		}
		parent, err := privateDirectory(home.Root, deletionDirectory, false)
		if err != nil {
			return err
		}
		defer func() { err = errors.Join(err, parent.Close()) }()
		if err = parent.Remove(regID); err != nil {
			return err
		}
		return syncDurableParent(parent)
	})
}

func checkDeletionDirectory(home *local.RootedHome, id string, held *os.Root) error {
	if err := home.Check(); err != nil {
		return err
	}
	current, err := privateDirectory(home.Root, filepath.Join(deletionDirectory, id), false)
	if err != nil {
		return err
	}
	defer func() { _ = current.Close() }()
	a, err := current.Stat(".")
	if err != nil {
		return err
	}
	b, err := held.Stat(".")
	if err != nil || !os.SameFile(a, b) {
		return errors.Join(ErrDurableStorageRecovery, err)
	}
	return nil
}

func (s *Store) removeDeletionStages(j SessionDeletion) error {
	return config.WithDeletionProtection(s.home, func(g config.DurableStorageGuard) (err error) {
		q, err := s.openDurableQuota(g)
		if err != nil {
			return err
		}
		defer func() { err = errors.Join(err, q.Close()) }()
		owner, err := privateDirectory(q.home.Root, filepath.Join(deletionDirectory, j.Registration.ArchiveSessionID), false)
		if err != nil {
			return err
		}
		defer func() { err = errors.Join(err, owner.Close()) }()
		dir, err := privateDirectory(owner, "sources", false)
		if errors.Is(err, os.ErrNotExist) {
			return checkDeletionDirectory(q.home, j.Registration.ArchiveSessionID, owner)
		}
		if err != nil {
			return err
		}
		defer func() { err = errors.Join(err, dir.Close()) }()
		if err := validateDeletionSourceNames(dir, j); err != nil {
			return err
		}
		f, err := dir.Open(".")
		if err != nil {
			return err
		}
		entries, err := f.ReadDir(durableEntryLimit + 1)
		err = errors.Join(err, f.Close())
		if err != nil {
			return err
		}
		if len(entries) > durableEntryLimit {
			return ErrDurableStorageRecovery
		}
		refs := map[string]archive.SourceReference{}
		for _, ref := range j.References {
			refs[ref.SHA256+".gz"] = ref
		}
		pinned, err := s.pinVerifiedDeletionSources(q.home, dir, j, entries, refs)
		if err != nil {
			return err
		}

		// Revalidate both the named owner directory and each regular-file identity
		// after full verification, before any irreversible unlink.
		for _, entry := range entries {
			if err = checkDeletionDirectory(q.home, j.Registration.ArchiveSessionID, owner); err != nil {
				return err
			}
			named, e := privateDirectory(owner, "sources", false)
			if e != nil {
				return e
			}
			a, e := named.Stat(".")
			e = errors.Join(e, named.Close())
			b, x := dir.Stat(".")
			if e != nil || x != nil || !os.SameFile(a, b) {
				return errors.Join(ErrDurableStorageRecovery, e, x)
			}
			name := entry.Name()
			now, e := dir.Lstat(name)
			before := pinned[name]
			if e != nil || !now.Mode().IsRegular() || !os.SameFile(before, now) || before.Size() != now.Size() || !before.ModTime().Equal(now.ModTime()) {
				return errors.Join(ErrDurableStorageRecovery, e)
			}
			if err = dir.Remove(name); err != nil {
				return err
			}
		}
		if err = syncDurableParent(dir); err != nil {
			return err
		}
		if err = owner.Remove("sources"); err != nil {
			return err
		}
		return syncDurableParent(owner)
	})
}

func (s *Store) deletionOrphans() (out []string, err error) {
	home, err := local.OpenRootedHome(s.home)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, home.Close()) }()
	dir, err := privateDirectory(home.Root, deletionDirectory, false)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, dir.Close()) }()
	f, err := dir.Open(".")
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	for {
		entries, e := f.ReadDir(128)
		if e != nil && !errors.Is(e, io.EOF) {
			return nil, e
		}
		for _, entry := range entries {
			if unsafeDurableSessionID(entry.Name()) {
				return nil, ErrDurableStorageRecovery
			}
			info, e := dir.Lstat(entry.Name())
			if e != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return nil, errors.Join(ErrDurableStorageRecovery, e)
			}
			_, found, e := s.loadSessionDeletion(entry.Name())
			owed, probeErr := s.hasDeletionWork(entry.Name())
			if e != nil || probeErr != nil || !found || owed {
				out = append(out, entry.Name())
			}
		}
		if errors.Is(e, io.EOF) {
			return out, nil
		}
	}
}

// RecoverDeletedLocalSession finishes known local terminal cleanup only. An
// orphan never receives publication or remote deletion permission from its ID.
func (s *Store) RecoverDeletedLocalSession(id string) (bool, error) {
	j, found, err := s.loadSessionDeletion(id)
	if err != nil || !found {
		return found, err
	}
	if j.LocalRemoved {
		owed, err := s.hasDeletionWork(id)
		if err != nil || owed {
			return true, errors.Join(ErrDurableStorageRecovery, err)
		}
		return true, nil
	}
	if j.Phase != DeletionCleaned && j.Phase != DeletionFinalizing {
		return true, ErrRemovalPending
	}
	return true, s.FinishDeletedSession(id)
}

func (s *Store) pinVerifiedDeletionSources(home *local.RootedHome, dir *os.Root, j SessionDeletion, entries []os.DirEntry, refs map[string]archive.SourceReference) (map[string]os.FileInfo, error) {
	pinned := map[string]os.FileInfo{}
	for _, entry := range entries {
		name := entry.Name()
		limit := int64(32 << 20)
		sha := j.MetadataSHA256
		if name != "metadata.json" {
			ref, ok := refs[name]
			if !ok {
				return nil, ErrDurableStorageRecovery
			}
			limit = int64(ref.CompressedBytes)
			sha = ref.SHA256
		}
		info, e := dir.Lstat(name)
		if e != nil || !info.Mode().IsRegular() {
			return nil, errors.Join(ErrDurableStorageRecovery, e)
		}
		scoped, closeScope := s.WithReadBudget(s.durableContext(), s.resourceBudget)
		raw, e := scoped.readDeletionObject(home, dir, name, limit)
		if e == nil && publicationSHA256(raw) != sha {
			e = ErrDurableStorageRecovery
		}
		closeScope()
		if e != nil {
			return nil, e
		}
		pinned[name] = info
	}
	return pinned, nil
}
