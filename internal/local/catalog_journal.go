package local

import (
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// CatalogJournal binds one frozen publication to its originating collector
// lock and configured destination. It contains hashes, never source payloads.
type CatalogJournal struct {
	Owner            string `json:"owner"`
	Origin           string `json:"origin"`
	Destination      string `json:"destination"`
	SessionID        string `json:"session_id"`
	MutationID       string `json:"mutation_id"`
	ExpectedRevision string `json:"expected_revision"`
	SHA256           string `json:"sha256"`
}

// Validate refuses incomplete recovery descriptors before remote mutation.
func (j CatalogJournal) Validate() error {
	if !journalHex(j.Owner, 16) || !journalHex(j.Origin, 32) || !journalHex(j.SHA256, 32) || j.Destination == "" || len(j.Destination) > 128 || j.SessionID == "" || len(j.SessionID) > 512 || strings.ContainsAny(j.SessionID, "/\\") || j.SessionID == "." || j.SessionID == ".." || j.MutationID == "" || len(j.MutationID) > 128 || len(j.ExpectedRevision) > 128 {
		return errors.New("invalid catalog journal recovery descriptor")
	}
	return nil
}

func journalHex(value string, size int) bool {
	raw, err := hex.DecodeString(value)
	return err == nil && len(raw) == size && hex.EncodeToString(raw) == value
}

// JournalRemoval is opaque positive evidence of an explicitly recorded durable
// pending removal. ENOENT without that record cannot construct this proof.
type JournalRemoval struct {
	journal CatalogJournal
	guard   *CollectorGuard
}

func (g *CollectorGuard) completionPath(id string) string {
	return filepath.Join(filepath.Dir(g.path), "catalog-completions", id+".json")
}

// RecordJournalRemoval writes the exact tombstone before pending unlink. It is
// called only after remote completion and all local/history acknowledgments.
func (g *CollectorGuard) RecordJournalRemoval(j CatalogJournal) error {
	if err := j.Validate(); err != nil {
		return err
	}
	done, err := g.BeginOperation(j.Origin)
	if err != nil {
		return err
	}
	defer done()
	if err = prepareGuardDirectory(filepath.Dir(g.completionPath(j.SessionID))); err != nil {
		return err
	}
	var previous CatalogJournal
	if err = readGuardRecord(g.completionPath(j.SessionID), &previous); err == nil && previous != j {
		return errors.New("another catalog removal record is unresolved")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return Write(g.completionPath(j.SessionID), j)
}

// ProveJournalRemoval requires both the exact tombstone and completed unlink.
func (g *CollectorGuard) ProveJournalRemoval(j CatalogJournal) (*JournalRemoval, error) {
	if err := j.Validate(); err != nil {
		return nil, err
	}
	done, err := g.BeginOperation(j.Origin)
	if err != nil {
		return nil, err
	}
	defer done()
	var recorded CatalogJournal
	if err = readGuardRecord(g.completionPath(j.SessionID), &recorded); err != nil || recorded != j {
		return nil, errors.New("catalog journal removal record differs")
	}
	if _, err = os.Lstat(filepath.Join(filepath.Dir(g.path), "pending", j.SessionID+".json")); !errors.Is(err, os.ErrNotExist) {
		return nil, errors.New("catalog pending journal has not been removed")
	}
	return &JournalRemoval{journal: j, guard: g}, nil
}

// Journal verifies that this proof still belongs to the actual held guard.
func (p *JournalRemoval) Journal(g *CollectorGuard) (CatalogJournal, error) {
	if p == nil || p.guard != g {
		return CatalogJournal{}, ErrBusy
	}
	verified, err := g.ProveJournalRemoval(p.journal)
	if err != nil {
		return CatalogJournal{}, err
	}
	return verified.journal, nil
}

// RemoveJournalRemoval retires a tombstone only after remote receipt removal.
func (g *CollectorGuard) RemoveJournalRemoval(j CatalogJournal) error {
	if _, err := g.ProveJournalRemoval(j); err != nil {
		return err
	}
	path := g.completionPath(j.SessionID)
	if err := os.Remove(path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}

// JournalRemovals lists at most the coordinator's explicit receipt capacity.
// Every record must decode and belong to this exact held origin.
func (g *CollectorGuard) JournalRemovals() ([]CatalogJournal, error) {
	origin, err := g.Origin()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(filepath.Dir(g.path), "catalog-completions")
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		return nil, errors.New("catalog removal record directory is not private")
	}
	dir, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	opened, statErr := dir.Stat()
	if statErr != nil || !os.SameFile(info, opened) {
		return nil, errors.Join(statErr, dir.Close(), errors.New("catalog removal record directory changed"))
	}
	entries, err := dir.ReadDir(257)
	if errors.Is(err, io.EOF) {
		err = nil
	}
	err = errors.Join(err, dir.Close())
	if err != nil || len(entries) > 256 {
		return nil, errors.Join(err, errors.New("catalog removal record inventory unavailable or over capacity"))
	}
	result := make([]CatalogJournal, 0, len(entries))
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), tempPrefix) {
			continue // An uncommitted atomic-write temporary grants no authority.
		}
		var j CatalogJournal
		if !entry.Type().IsRegular() || readGuardRecord(filepath.Join(filepath.Dir(g.path), "catalog-completions", entry.Name()), &j) != nil || j.Validate() != nil || j.Origin != origin || entry.Name() != j.SessionID+".json" {
			return nil, errors.New("invalid catalog removal record")
		}
		result = append(result, j)
	}
	return result, nil
}

// HoldJournalRemoval verifies the exact durable pre-unlink completion record
// under the actual held originating guard. The returned operation must remain
// held through cleanup, pending unlink and fsync; missing pending grants nothing.
func (g *CollectorGuard) HoldJournalRemoval(j CatalogJournal) (func(), error) {
	if err := j.Validate(); err != nil {
		return nil, err
	}
	done, err := g.BeginOperation(j.Origin)
	if err != nil {
		return nil, err
	}
	var recorded CatalogJournal
	if err = readGuardRecord(g.completionPath(j.SessionID), &recorded); err != nil || recorded != j {
		done()
		return nil, errors.New("catalog pre-removal record differs")
	}
	return done, nil
}
