package state

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/wangjohn/agent-archive/internal/local"
)

// DurableStorageNamespace identifies an accounting root, never content ownership.
type DurableStorageNamespace string

const (
	// PendingPublicationStorage holds selecting records and anonymous atomic copies.
	PendingPublicationStorage DurableStorageNamespace = "pending-publication"
	// SessionDeletionStorage holds exact removal or retained restoration work.
	SessionDeletionStorage DurableStorageNamespace = "session-deletion"
	// GenerationRecoveryStorage holds the existing generation recovery journal.
	GenerationRecoveryStorage DurableStorageNamespace = "generation-recovery"
	// PendingHistoryStorage holds current history stages, including source-only leftovers.
	PendingHistoryStorage DurableStorageNamespace = "pending-history"
	// ImportAdmissionStorage identifies durable preparation and cancellation journals.
	ImportAdmissionStorage DurableStorageNamespace = "import-admission"
	// PublicationEvidenceStorage holds opaque original publication evidence.
	PublicationEvidenceStorage DurableStorageNamespace = "publication-evidence"
	// UnavailableStorage holds obligations whose runtime owner is not installed.
	UnavailableStorage DurableStorageNamespace = "unavailable-owner"
)

// DurableStorageObligation is a visible local obligation independent of registration.
type DurableStorageObligation struct {
	SessionID string
	Namespace DurableStorageNamespace
}

func rootHasEntries(root *os.Root, path string) (owed bool, err error) {
	dir, err := privateDirectory(root, path, false)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return true, err
	}
	defer func() {
		err = errors.Join(err, dir.Close())
		if err != nil {
			owed = true
		}
	}()
	d, err := dir.Open(".")
	if err != nil {
		return true, err
	}
	defer func() {
		err = errors.Join(err, d.Close())
		if err != nil {
			owed = true
		}
	}()
	entries, err := d.ReadDir(1)
	if errors.Is(err, io.EOF) {
		err = nil
	}
	return len(entries) > 0 || err != nil, err
}

func (s *Store) hasStorageEntries(path string) (owed bool, err error) {
	home, err := local.OpenRootedHome(s.home)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return true, errors.Join(ErrDurableStorageRecovery, err)
	}
	defer func() {
		err = errors.Join(err, home.Close())
		if err != nil {
			owed = true
		}
	}()
	return rootHasEntries(home.Root, path)
}

func (s *Store) hasPendingHistorySources(id string) (bool, error) {
	return s.hasStorageEntries(filepath.Join("sessions", id, "pending-sources"))
}

func (s *Store) hasPublicationEvidence(id string) (bool, error) {
	return s.hasStorageEntries(filepath.Join("publication-evidence", id))
}

func (s *Store) protectedStorage(id string) (bool, error) {
	history, herr := s.hasPendingHistorySources(id)
	evidence, eerr := s.hasPublicationEvidence(id)
	return history || evidence || herr != nil || eerr != nil, errors.Join(herr, eerr)
}

// DurableStorageObligations probes bounded private roots and small completed
// generation receipts without inventing registrations. Unknown roots remain
// recovery work, never deletion permission.
func (s *Store) DurableStorageObligations() ([]DurableStorageObligation, error) {
	return s.durableStorageObligations(s.durableContext())
}

func (s *Store) durableStorageObligations(ctx context.Context) (out []DurableStorageObligation, err error) {
	if err := s.beginDurableObligations(ctx); err != nil {
		return nil, err
	}
	home, err := local.OpenRootedHome(s.home)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		out, appendErr := s.appendDurableObligations(nil, DurableStorageObligation{Namespace: UnavailableStorage})
		return out, errors.Join(ErrDurableStorageRecovery, err, appendErr)
	}
	defer func() { err = errors.Join(err, home.Close()) }()
	cfg, found, e := s.loadDurableInspectionConfig(ctx, home)
	if e != nil {
		out, appendErr := s.appendDurableObligations(nil, DurableStorageObligation{Namespace: UnavailableStorage})
		return out, errors.Join(ErrDurableStorageRecovery, e, appendErr)
	}
	pending, _, used, generation, e := s.inspectPendingRoots(ctx, home, found && !cfg.DurableStorageProtection, true)
	out, err = s.classifyGenerationObligations(home, pending, found)
	if err != nil {
		return out, err
	}
	if e != nil {
		out, appendErr := s.appendDurableObligations(out, DurableStorageObligation{Namespace: UnavailableStorage})
		return out, errors.Join(ErrDurableStorageRecovery, e, appendErr)
	}
	visited := used
	if e := inspectImportLocks(ctx, home, &visited); e != nil {
		out, appendErr := s.appendDurableObligations(out, DurableStorageObligation{Namespace: UnavailableStorage})
		return out, errors.Join(ErrDurableStorageRecovery, e, appendErr)
	}
	for _, base := range []string{"sessions", "publication-evidence", deletionDirectory} {
		out, err = s.inspectStorageObligationRoot(ctx, home, base, &visited, out)
		if err != nil {
			return out, err
		}
	}
	out, err = s.finishDurableStorageObligations(ctx, home, &visited, out, err)
	if err == nil {
		// Optional retention starts only after every mandatory result vector
		// and receipt has been funded and the full census has completed.
		s.cacheDurableObligations(pending, generation)
	}
	return out, err
}

func (s *Store) inspectStorageObligationRoot(ctx context.Context, home *local.RootedHome, base string, visited *int, out []DurableStorageObligation) (result []DurableStorageObligation, err error) {
	result = out
	dir, e := privateDirectory(home.Root, base, false)
	if errors.Is(e, os.ErrNotExist) {
		return result, nil
	}
	if e != nil {
		result, err = s.appendDurableObligations(result, DurableStorageObligation{Namespace: UnavailableStorage})
		return result, errors.Join(ErrDurableStorageRecovery, e, err)
	}
	defer func() {
		err = errors.Join(err, dir.Close())
		if err != nil {
			err = errors.Join(ErrDurableStorageRecovery, err)
		}
	}()
	d, e := dir.Open(".")
	if e != nil {
		return result, errors.Join(ErrDurableStorageRecovery, e)
	}
	defer func() { err = errors.Join(err, d.Close()) }()
	for {
		entries, e := readDurableDirectory(ctx, d)
		if e != nil && !errors.Is(e, io.EOF) {
			return result, errors.Join(ErrDurableStorageRecovery, e)
		}
		for _, entry := range entries {
			result, err = s.inspectStorageObligationEntry(home, dir, base, entry, visited, result)
			if err != nil {
				return result, errors.Join(ErrDurableStorageRecovery, err)
			}
		}
		if errors.Is(e, io.EOF) {
			return result, nil
		}
	}
}

func (s *Store) inspectStorageObligationEntry(home *local.RootedHome, dir *os.Root, base string, entry os.DirEntry, visited *int, out []DurableStorageObligation) ([]DurableStorageObligation, error) {
	if err := countDurableEntry(visited); err != nil {
		return out, err
	}
	info, err := dir.Lstat(entry.Name())
	if err != nil {
		return out, err
	}
	if base == "sessions" && info.Mode().IsRegular() {
		return out, nil
	}
	if !namedDurableDirectory(entry.Name(), info) {
		out, err = s.appendDurableObligations(out, DurableStorageObligation{Namespace: UnavailableStorage})
		return out, errors.Join(ErrDurableStorageRecovery, err)
	}
	if base == deletionDirectory {
		return s.appendDeletionObligation(out, nil, entry.Name())
	}
	out, err = s.appendImportStageObligation(out, nil, home.Root, base, entry.Name())
	if err != nil {
		return out, err
	}
	path := filepath.Join(base, entry.Name())
	namespace := PublicationEvidenceStorage
	if base == "sessions" {
		path = filepath.Join(path, "pending-sources")
		namespace = PendingHistoryStorage
	}
	present, err := rootHasEntries(home.Root, path)
	if err != nil || !present {
		return out, err
	}
	if err := countDurableEntry(visited); err != nil {
		return out, err
	}
	return s.appendDurableObligations(out, DurableStorageObligation{SessionID: entry.Name(), Namespace: namespace})
}

func (s *Store) finishDurableStorageObligations(ctx context.Context, home *local.RootedHome, visited *int, out []DurableStorageObligation, err error) ([]DurableStorageObligation, error) {
	if e := ctx.Err(); e != nil {
		return out, errors.Join(ErrDurableStorageRecovery, err, e)
	}
	unavailable, e := s.unavailableStorageObligations(home.Root, visited)
	out, appendErr := s.appendDurableObligations(out, unavailable...)
	err = errors.Join(err, appendErr)
	err = errors.Join(err, e)
	if err != nil {
		err = errors.Join(ErrDurableStorageRecovery, err)
	}
	return out, errors.Join(err, home.Check(), ctx.Err())
}

func namedDurableDirectory(name string, info os.FileInfo) bool {
	return safeFileComponent(name) && info.IsDir() && info.Mode()&os.ModeSymlink == 0
}

func (s *Store) classifyGenerationObligations(home *local.RootedHome, pending []DurableStorageObligation, found bool) (out []DurableStorageObligation, err error) {
	for _, obligation := range pending {
		if found && obligation.Namespace == GenerationRecoveryStorage && obligation.SessionID != "" {
			complete, receiptErr := s.completedGenerationReceipt(home, obligation.SessionID)
			if receiptErr != nil {
				out, err = s.appendDurableObligations(out, obligation)
				if err != nil {
					return out, err
				}
				return out, errors.Join(ErrDurableStorageRecovery, receiptErr)
			}
			if complete {
				continue
			}
		}
		out, err = s.appendDurableObligations(out, obligation)
		if err != nil {
			return out, err
		}
	}
	return out, nil
}

func (s *Store) unavailableStorageObligations(root *os.Root, visited *int) (out []DurableStorageObligation, err error) {
	for _, dir := range []string{"admission-stages", "temporary-reservations", "temporary-scratch"} {
		present, e := rootHasEntries(root, dir)
		err = errors.Join(err, e)
		if present {
			if e := countDurableEntry(visited); e != nil {
				return out, ErrDurableStorageRecovery
			}
			out, e = s.appendDurableObligations(out, DurableStorageObligation{Namespace: UnavailableStorage})
			err = errors.Join(err, e)
		}
	}
	return out, err
}

func (s *Store) appendImportStageObligation(out []DurableStorageObligation, previous error, root *os.Root, base, id string) ([]DurableStorageObligation, error) {
	if base != "sessions" {
		return out, previous
	}
	present, err := rootHasEntries(root, filepath.Join(base, id, "import-stage"))
	if err != nil || !present {
		return out, errors.Join(previous, err)
	}
	out, err = s.appendDurableObligations(out, DurableStorageObligation{SessionID: id, Namespace: ImportAdmissionStorage})
	return out, errors.Join(previous, err)
}

func (s *Store) appendDeletionObligation(out []DurableStorageObligation, prior error, id string) ([]DurableStorageObligation, error) {
	present, err := s.hasDeletionWork(id)
	if present {
		var appendErr error
		out, appendErr = s.appendDurableObligations(out, DurableStorageObligation{SessionID: id, Namespace: SessionDeletionStorage})
		err = errors.Join(err, appendErr)
	}
	return out, errors.Join(prior, err)
}
