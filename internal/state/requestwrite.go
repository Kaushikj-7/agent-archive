package state

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/wangjohn/agent-archive/internal/local"
)

// fileSnapshot is a file's content as one read saw it; found is false when
// there was no file.
type fileSnapshot struct {
	data  []byte
	found bool
}

func readSnapshot(path string) (fileSnapshot, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return fileSnapshot{}, nil
	}
	if err != nil {
		return fileSnapshot{}, err
	}
	return fileSnapshot{data: data, found: true}, nil
}

func (f fileSnapshot) equal(other fileSnapshot) bool {
	return f.found == other.found && bytes.Equal(f.data, other.data)
}

const (
	// unlockedWriteAttempts is how many times writeUnderLock stages a write
	// with the lock free for a writer that is not a hook. Each attempt costs
	// one disk sync, and a conflict needs another writer to commit to the
	// same file within it; a writer overtaken on every one gives up with
	// errWriteOvertaken, for its caller to retry later (the collector on its
	// next pass), rather than sync while holding a lock hooks wait on.
	unlockedWriteAttempts = 4
	// hookUnlockedWriteAttempts is the same for a hook (see ForHook), which
	// cannot retry later and has a two-second budget: after one overtaken
	// attempt it writes holding the lock.
	hookUnlockedWriteAttempts = 1
)

// errWriteOvertaken reports that other writers changed the file during every
// attempt of a writer that is not a hook.
var errWriteOvertaken = errors.New("other writers kept changing it while it was written; the next attempt retries")

// ForHook returns a Store for a hook's writes. A hook's evidence is lost if
// its write fails, so a hook overtaken by another writer writes holding the
// lock on its next attempt (see writeUnderLock). Hooks are serialized by
// hooks.lock, so that hold can only delay a writer that is not a hook, which
// retries.
func (s *Store) ForHook() *Store {
	hook := *s
	hook.hook = true
	return &hook
}

// lockedWrite is one read-modify-write for writeUnderLock.
type lockedWrite struct {
	// beforeLock verifies prepared external evidence after staging, on every retry.
	beforeLock       func() error
	nativeValidation *nativeValidationCommit
	snapshot         func(string) (fileSnapshot, error)
	expectation      *local.Staged
	packedEpoch      string
	packedHome       string
	expiryPath       string
	membership       *local.Staged
	membershipHome   string
	// lock takes the lock guarding path.
	lock func() (func(), error)
	path string
	// check, if not nil, runs under the lock right before the rename; an
	// error from it is returned as is, with nothing written.
	check func() error
	// change computes the new content from the file's current content;
	// write false leaves the file as it is.
	change func(current fileSnapshot) (value any, write bool, err error)
	// blind marks a replacement whose value does not depend on the file's
	// content. change gets an empty snapshot and ordinary commit does not
	// compare prior bytes. Registration fencing separately reads identity facts;
	// the caller's request-lock check still guards replacement and provenance.
	blind bool
}

// writeUnderRequestLock is writeUnderLock for path, one of the files a
// session's request lock guards (its request or its registration).
func (s *Store) writeUnderRequestLock(archiveSessionID, path string, check func() error, change func(current fileSnapshot) (value any, write bool, err error)) error {
	return s.writeUnderLock(lockedWrite{
		lock:   func() (func(), error) { return s.lockRequest(archiveSessionID) },
		path:   path,
		check:  check,
		change: change,
	})
}

// writeUnderLock replaces w.path with what w.change computes from the file's
// current content, holding w.lock only to check and rename, never across a
// disk sync, except for a hook overtaken by another writer (see below).
//
// Hooks wait a second for the request lock (lockRequest) and for a subagent
// candidate's lock, inside the harness's two-second hook budget, and the
// turn's evidence is lost when the lock stays held longer. A durable write
// syncs the file and then its directory, each an F_FULLFSYNC on macOS, and
// together they have been measured past two seconds on a loaded Mac. So no
// holder of the lock may sync under it:
//
//  1. The file is read without the lock, change computes the new content
//     from it, and that content is written and synced to a temporary file
//     beside the path (local.Stage).
//  2. Under the lock, check runs, and the file is read again. If it still
//     holds exactly what change saw, the temporary is renamed over it: the
//     commit point, which every other holder of the lock sees atomically,
//     just as it saw the whole read-modify-write before. If another writer
//     changed it meanwhile, the temporary is discarded and the next attempt
//     computes from the new content.
//  3. After the lock is released, and before this returns, the directory is
//     synced, making the rename durable.
//
// The comparison is on content, not on a token or file identity: change is
// a function of the content alone, so a file changed and changed back yields
// the same write. That keeps every guarantee the lock gave. A hook landing
// while the collector publishes still gets a fresh token (saveRequest), so
// CompleteRequest leaves its evidence pending; and a forget under the lock
// cannot slip between a writer's registration check and its write, because
// check runs under the lock right before the rename.
//
// Between the rename and the directory sync, another holder of the lock can
// read the new file before it is durable. The writer has not returned yet,
// so it has promised nothing: a crash then can undo the rename, as it could
// undo a write that never finished, and what that reader did with the
// content (the collector publishing a hook's evidence, retention keeping a
// session) was still correct for content that existed. A reader never sees
// a partial file, as the temporary is synced before it is renamed.
//
// A writer committing back to back would beat every staged attempt of
// another (a stress run with the collector writing continuously lost hook
// writes that way). So a hook (ForHook), whose write cannot be retried, is
// overtaken at most once: its next attempt holds the lock from its read
// through its rename, and so across the temporary file's sync. Any other
// writer never syncs under the lock; after unlockedWriteAttempts it returns
// errWriteOvertaken for its caller to retry. The directory sync always waits
// until the lock is released.
//
// change may run more than once, so it must compute only from its argument
// and the caller's inputs, and record its results afresh on every run.
// write false leaves the file as it is; check still runs under the lock and
// the file must still be unchanged, so a decision not to write is as atomic
// as a write. An error from change or check is returned as is, with nothing
// written.
func (s *Store) writeUnderLock(w lockedWrite) error {
	w = s.guardNativeRegistrationWrite(w)
	if s.indexSnapshots && w.snapshot == nil {
		w.snapshot = s.readIndexSnapshot
	}
	attempts := unlockedWriteAttempts
	if s.hook {
		attempts = hookUnlockedWriteAttempts
	}
	for range attempts {
		var before fileSnapshot
		if !w.blind {
			var err error
			if before, err = w.readSnapshot(); err != nil {
				return err
			}
		}
		value, write, err := w.change(before)
		if err != nil {
			return err
		}
		var staged *local.Staged
		w.expectation = nil
		w.expiryPath = ""
		if write {
			var err error
			w.expectation, w.packedEpoch, err = s.stagePackedExpectation(w.path)
			w.packedHome = s.home
			if err != nil {
				return err
			}
			if s.indexSnapshots {
				w.expiryPath = w.path
			}
			if staged, err = local.Stage(w.path, value); err != nil {
				w.expectation.Discard()
				return err
			}
			s.writeSynced()
		}
		membership, err := stageRegistrationRevision(s.home, w.path, before, value, write)
		if err != nil {
			staged.Discard()
			w.expectation.Discard()
			return err
		}
		w.membership, w.membershipHome = membership, s.home
		if w.beforeLock != nil {
			if err := w.beforeLock(); err != nil {
				membership.Discard()
				w.expectation.Discard()
				staged.Discard()
				return err
			}
		}
		committed, err := commitUnderLock(w, before, staged)
		membership.Discard()
		w.expectation.Discard()
		staged.Discard()
		if err != nil {
			return err
		}
		if !committed {
			continue
		}
		if staged == nil {
			return nil
		}
		s.writeSynced()
		return errors.Join(staged.SyncDir(), syncRegistrationRevision(membership))
	}
	if !s.hook {
		return fmt.Errorf("write %s: %w", w.path, errWriteOvertaken)
	}
	return s.writeHoldingLock(w)
}

// writeHoldingLock is a hook's attempt at writeUnderLock after it was
// overtaken: the read, change, and staging happen under the lock, so no
// other writer can overtake it. Only the directory sync waits until the lock
// is released.
func (s *Store) writeHoldingLock(w lockedWrite) error {
	expectation, epoch, err := s.stagePackedExpectation(w.path)
	w.packedEpoch, w.packedHome = epoch, s.home
	if err != nil {
		return err
	}
	defer expectation.Discard()
	w.expectation = expectation
	if s.indexSnapshots {
		w.expiryPath = w.path
	}
	revision, err := stageRegistrationRevision(s.home, w.path, fileSnapshot{}, nil, true)
	if err != nil {
		return err
	}
	defer revision.Discard()
	w.membership, w.membershipHome = revision, s.home
	staged, err := func() (*local.Staged, error) {
		unlock, err := w.lock()
		if err != nil {
			return nil, err
		}
		defer unlock()
		if err := w.packedEpochValid(); err != nil {
			return nil, err
		}
		if w.check != nil {
			if err := w.check(); err != nil {
				return nil, err
			}
		}
		current, err := w.readSnapshot()
		if err != nil {
			return nil, err
		}
		value, write, err := w.change(current)
		if err != nil || !write {
			return nil, err
		}
		staged, err := local.Stage(w.path, value)
		if err != nil {
			return nil, err
		}
		if w.expectation != nil {
			if err := w.expectation.Commit(); err != nil {
				staged.Discard()
				return nil, err
			}
		}
		if err := commitRegistrationRevision(w, staged); err != nil {
			staged.Discard()
			return nil, err
		}
		if w.expiryPath != "" {
			if err := removePackedExpiryFile(w.expiryPath); err != nil {
				staged.Discard()
				return nil, err
			}
		}
		return staged, nil
	}()
	if err != nil || staged == nil {
		return err
	}
	s.writeSynced()
	return errors.Join(staged.SyncDir(), syncRegistrationRevision(revision))
}

// commitUnderLock is writeUnderLock's step under the lock. committed is
// false when the file no longer holds before.
func commitUnderLock(w lockedWrite, before fileSnapshot, staged *local.Staged) (committed bool, err error) {
	unlock, err := w.lock()
	if err != nil {
		return false, err
	}
	defer unlock()
	if err := w.packedEpochValid(); err != nil {
		return false, err
	}
	if w.check != nil {
		if err := w.check(); err != nil {
			return false, err
		}
	}
	if !w.blind {
		current, err := w.readSnapshot()
		if err != nil || !current.equal(before) {
			return false, err
		}
	}
	if staged == nil {
		return true, nil
	}
	if w.expectation != nil {
		if err := w.expectation.Commit(); err != nil {
			return false, err
		}
	}
	if err := commitRegistrationRevision(w, staged); err != nil {
		return false, err
	}
	if w.expiryPath != "" {
		if err := removePackedExpiryFile(w.expiryPath); err != nil {
			return false, err
		}
	}
	return true, nil
}

// writeSynced runs the test seam, if any, where writeUnderLock syncs outside
// the lock.
func (s *Store) writeSynced() {
	if s.onWriteSync != nil {
		s.onWriteSync()
	}
}

// namedLockWait is local.NamedLockWait under home, telling the test seam, if
// any, about a wait that can last (a timeout above zero).
func (s *Store) namedLockWait(name string, timeout time.Duration) (func(), error) {
	if timeout > 0 && s.onLockWait != nil {
		s.onLockWait(name)
	}
	return local.NamedLockWait(s.home, name, timeout)
}

func (w lockedWrite) readSnapshot() (fileSnapshot, error) {
	if w.snapshot != nil {
		return w.snapshot(w.path)
	}
	return readSnapshot(w.path)
}
