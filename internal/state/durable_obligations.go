package state

import (
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
func (s *Store) DurableStorageObligations() (out []DurableStorageObligation, err error) {
	home, err := local.OpenRootedHome(s.home)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		out, appendErr := s.appendDurableObligations(nil, DurableStorageObligation{Namespace: UnavailableStorage})
		return out, errors.Join(ErrDurableStorageRecovery, err, appendErr)
	}
	defer func() { err = errors.Join(err, home.Close()) }()
	cfg, found, e := s.loadDurableInspectionConfig(home)
	if e != nil {
		out, appendErr := s.appendDurableObligations(nil, DurableStorageObligation{Namespace: UnavailableStorage})
		return out, errors.Join(ErrDurableStorageRecovery, e, appendErr)
	}
	pending, _, used, e := s.inspectPendingRoots(home, found && !cfg.DurableStorageProtection)
	out, err = s.classifyGenerationObligations(home, pending, found)
	if err != nil {
		return out, err
	}
	if e != nil {
		out, appendErr := s.appendDurableObligations(out, DurableStorageObligation{Namespace: UnavailableStorage})
		return out, errors.Join(ErrDurableStorageRecovery, e, appendErr)
	}
	visited := used
	for _, base := range []string{"sessions", "publication-evidence", deletionDirectory} {
		dir, e := privateDirectory(home.Root, base, false)
		if errors.Is(e, os.ErrNotExist) {
			continue
		}
		if e != nil {
			var appendErr error
			out, appendErr = s.appendDurableObligations(out, DurableStorageObligation{Namespace: UnavailableStorage})
			return out, errors.Join(ErrDurableStorageRecovery, e, appendErr)
		}
		d, e := dir.Open(".")
		if e != nil {
			return out, errors.Join(ErrDurableStorageRecovery, e, dir.Close())
		}
		for {
			if e := s.durableContext().Err(); e != nil {
				return out, errors.Join(ErrDurableStorageRecovery, e, d.Close(), dir.Close())
			}
			entries, e := readDurableDirectory(s.durableContext(), d)
			if e != nil && !errors.Is(e, io.EOF) {
				return out, errors.Join(ErrDurableStorageRecovery, e, d.Close(), dir.Close())
			}
			for _, entry := range entries {
				if e := countDurableEntry(&visited); e != nil {
					return out, errors.Join(ErrDurableStorageRecovery, e, d.Close(), dir.Close())
				}
				info, x := dir.Lstat(entry.Name())
				if x != nil {
					return out, errors.Join(ErrDurableStorageRecovery, x, d.Close(), dir.Close())
				}
				if base == "sessions" && info.Mode().IsRegular() {
					continue
				}
				if !namedDurableDirectory(entry.Name(), info) {
					var appendErr error
					out, appendErr = s.appendDurableObligations(out, DurableStorageObligation{Namespace: UnavailableStorage})
					return out, errors.Join(ErrDurableStorageRecovery, appendErr, d.Close(), dir.Close())
				}
				if base == deletionDirectory {
					out, err = s.appendDeletionObligation(out, nil, entry.Name())
					if err != nil {
						return out, errors.Join(ErrDurableStorageRecovery, err, d.Close(), dir.Close())
					}
					continue
				}
				out, err = s.appendImportStageObligation(out, nil, home.Root, base, entry.Name())
				if err != nil {
					return out, errors.Join(ErrDurableStorageRecovery, err, d.Close(), dir.Close())
				}
				path := filepath.Join(base, entry.Name())
				namespace := PublicationEvidenceStorage
				if base == "sessions" {
					path = filepath.Join(path, "pending-sources")
					namespace = PendingHistoryStorage
				}
				present, x := rootHasEntries(home.Root, path)
				if x != nil {
					return out, errors.Join(ErrDurableStorageRecovery, x, d.Close(), dir.Close())
				}
				if present {
					if e := countDurableEntry(&visited); e != nil {
						return out, errors.Join(ErrDurableStorageRecovery, e, d.Close(), dir.Close())
					}
					var appendErr error
					out, appendErr = s.appendDurableObligations(out, DurableStorageObligation{SessionID: entry.Name(), Namespace: namespace})
					if appendErr != nil {
						return out, errors.Join(ErrDurableStorageRecovery, appendErr, d.Close(), dir.Close())
					}
				}
			}
			if errors.Is(e, io.EOF) {
				break
			}
		}
		if err = errors.Join(d.Close(), dir.Close()); err != nil {
			return out, errors.Join(ErrDurableStorageRecovery, err)
		}
	}
	return s.finishDurableStorageObligations(home, &visited, out, err)
}

func (s *Store) finishDurableStorageObligations(home *local.RootedHome, visited *int, out []DurableStorageObligation, err error) ([]DurableStorageObligation, error) {
	if e := s.durableContext().Err(); e != nil {
		return out, errors.Join(ErrDurableStorageRecovery, err, e)
	}
	unavailable, e := s.unavailableStorageObligations(home.Root, visited)
	out, appendErr := s.appendDurableObligations(out, unavailable...)
	err = errors.Join(err, appendErr)
	err = errors.Join(err, e)
	if err != nil {
		err = errors.Join(ErrDurableStorageRecovery, err)
	}
	return out, errors.Join(err, home.Check(), s.durableContext().Err())
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
