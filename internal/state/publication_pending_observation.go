package state

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/config"
)

const pendingObservationBytes int64 = 4096

// pendingWriteObservation remembers only absence facts from one successful
// fully validated modern write. It cannot select, acknowledge, migrate or
// discard any publication. A later use hashes the entire actual file again.
type pendingWriteObservation struct {
	owner       *Store
	budget      *agentapi.NativeReadBudget
	context     context.Context
	reservation stateReservation
	home        os.FileInfo
	id          string
	preparation string
	phase       PublicationPhase
	sum         [32]byte
	size        int64
	closed      bool
}

func ordinaryPendingObservation(p PendingPublication) bool {
	return p.JournalVersion == 5 && p.Preparation != nil && p.Preparation.OriginalEvidenceSHA256 == "" && p.Selection == nil && p.StagedProof() == nil && p.Catalog == nil
}

func (s *Store) pendingObservation() *pendingWriteObservation {
	w := s.pendingWrite
	if w == nil || w.closed || s.resourceReleases == nil || *s.resourceReleases == nil || w.owner != s || w.budget != s.resourceBudget || w.context.Err() != nil || s.resourceContext == nil || !reflect.ValueOf(s.resourceContext).Comparable() || w.context != s.resourceContext {
		return nil
	}
	return w
}

func (s *Store) clearPendingObservation() {
	if w := s.pendingWrite; w != nil && w.owner == s {
		w.home = nil
		w.id, w.preparation = "", ""
		w.sum, w.size = [32]byte{}, 0
	}
}

// writeObservedPending uses the same guarded exact-wire writer as other modern
// state. The observation is optional, charged before allocation, and retained
// only by the exact read owner until that owner's existing close.
func (s *Store) writeObservedPending(ctx context.Context, g config.DurableStorageGuard, id string, p PendingPublication) error {
	if s.resourceBudget == nil || s.resourceReleases == nil || *s.resourceReleases == nil || s.resourceContext == nil || !reflect.ValueOf(s.resourceContext).Comparable() || !ordinaryPendingObservation(p) {
		return s.writeDurableGuard(ctx, g, filepath.Join("pending", id+".json"), publicationWire(p))
	}
	w := s.pendingObservation()
	if w == nil {
		reservation := s.readReservation()
		if reservation.Available() < pendingObservationBytes {
			return s.writeDurableGuard(ctx, g, filepath.Join("pending", id+".json"), publicationWire(p))
		}
		if !reservation.Reserve(pendingObservationBytes) {
			return errStateBudget
		}
		w = &pendingWriteObservation{owner: s, budget: s.resourceBudget, context: s.resourceContext, reservation: reservation}
		s.pendingWrite = w
		*s.resourceReleases = append(*s.resourceReleases, func() {
			w.closed = true
			w.home = nil
			w.id, w.preparation = "", ""
			w.sum, w.size = [32]byte{}, 0
			w.reservation.Release(pendingObservationBytes)
		})
	}
	// Capture immutable scalar facts before encoding or any injected sync seam.
	preparation := strings.Clone(p.Preparation.SHA256)
	s.clearPendingObservation()
	sum, n, err := s.writeDurableGuardDigest(ctx, g, filepath.Join("pending", id+".json"), publicationWire(p))
	if err != nil {
		return err
	}
	home, err := g.RootedHome(s.home)
	if err != nil {
		return err
	}
	info, err := home.Root.Stat(".")
	if err != nil {
		return err
	}
	if err := g.CheckHome(s.home); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	w.home, w.id, w.preparation, w.sum, w.size, w.phase = info, strings.Clone(id), preparation, sum, n, p.Phase
	return nil
}

// rewriteObservedPending avoids retaining a second full old journal solely to
// discover that no original-evidence transition is needed. Different bytes or
// protocols take the existing complete decoder, never a projected authority.
func (s *Store) rewriteObservedPending(g config.DurableStorageGuard, id string, p PendingPublication) (bool, error) {
	w := s.pendingObservation()
	if w == nil || w.home == nil || w.id != id || !ordinaryPendingObservation(p) || p.Preparation.SHA256 != w.preparation {
		return false, nil
	}
	// Completing the same immutable preparation is monotonic. A ready
	// publication can never roll back through this absence-only observation.
	if p.Phase != w.phase && (w.phase != PublicationPreparing || p.Phase != PublicationReady) {
		return false, nil
	}
	home, err := g.RootedHome(s.home)
	if err != nil {
		return true, err
	}
	info, err := home.Root.Stat(".")
	if err != nil {
		return true, err
	}
	if !os.SameFile(w.home, info) {
		s.clearPendingObservation()
		return false, nil
	}
	if owed, err := rootHasEntries(home.Root, filepath.Join("publication-evidence", id)); err != nil || owed {
		return err != nil, err
	}
	cfg, found, closeConfig, err := config.LoadRootedOwned(s.durableContext(), home, s.readReservation())
	if err != nil {
		return true, err
	}
	closeConfig()
	if !found || cfg.PublicationCompositionProtection {
		return false, nil
	}
	path := filepath.Join("pending", id+".json")
	matches, err := s.matchesObservedPending(g, path, w)
	if err != nil || !matches {
		s.clearPendingObservation()
		return err != nil, err
	}
	// SavePending already ran every complete new-candidate validation. The
	// observed old protocol has no staged, selecting or catalog fence, and no
	// original evidence exists under this same exclusive composition guard.
	if len(p.SourceBytes) > maxPendingHistoryBytes {
		return true, ErrDurableStorageCapacity
	}
	return true, s.writeObservedPending(s.durableContext(), g, id, p)
}

func (s *Store) matchesObservedPending(g config.DurableStorageGuard, path string, w *pendingWriteObservation) (matches bool, err error) {
	home, err := g.RootedHome(s.home)
	if err != nil {
		return false, err
	}
	before, err := home.Root.Lstat(path)
	if err != nil {
		return false, err
	}
	if !before.Mode().IsRegular() || before.Size() != w.size || w.size < 0 || w.size > publicationEvidenceLimit {
		return false, nil
	}
	reservation := s.readReservation()
	scratch := min(w.size+1, int64(32<<10))
	if !reservation.Reserve(scratch) {
		return false, errStateBudget
	}
	defer reservation.Release(scratch)
	buffer := make([]byte, scratch)
	f, err := home.Root.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return false, err
	}
	defer func() {
		err = errors.Join(err, s.closePublicationFile(f))
		if err != nil {
			matches = false
		}
	}()
	opened, err := f.Stat()
	if err != nil || !sameDurableStamp(before, opened) {
		return false, errors.Join(ErrDurableStorageRecovery, err)
	}
	hash := sha256.New()
	remaining := w.size
	for remaining > 0 {
		if err := s.durableContext().Err(); err != nil {
			return false, err
		}
		n, err := f.Read(buffer[:min(int64(len(buffer)), remaining)])
		if err != nil {
			return false, err
		}
		if n == 0 {
			return false, io.ErrNoProgress
		}
		_, _ = hash.Write(buffer[:n])
		remaining -= int64(n)
	}
	var extra [1]byte
	if n, err := f.Read(extra[:]); n != 0 || !errors.Is(err, io.EOF) {
		return false, errors.Join(ErrDurableStorageRecovery, err)
	}
	opened, err = f.Stat()
	named, namedErr := home.Root.Lstat(path)
	if err != nil || namedErr != nil || !sameDurableStamp(before, opened) || !sameDurableStamp(before, named) {
		return false, errors.Join(ErrDurableStorageRecovery, err, namedErr)
	}
	if err := g.CheckHome(s.home); err != nil {
		return false, err
	}
	if err := s.durableContext().Err(); err != nil {
		return false, err
	}
	return [32]byte(hash.Sum(nil)) == w.sum, nil
}
