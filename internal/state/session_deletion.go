package state

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
)

const deletionDirectory = "session-deletions"

const deletionWireLimit int64 = durableControlBytes / 2

// ErrRemovalPending keeps an interrupted, authorized deletion actionable.
var ErrRemovalPending = errors.New("authorized session removal is unfinished; run sync to finish recovery")

// ErrDeletionWorkChanged leaves newer queued work with its original owner.
var ErrDeletionWorkChanged = errors.New("session work changed during removal; preserve retained evidence and retry")

// SessionDeletionPhase is the durable state of an exact selecting union.
type SessionDeletionPhase string

const (
	DeletionPrepared   SessionDeletionPhase = "prepared"
	DeletionDeleting   SessionDeletionPhase = "deleting"
	DeletionAbsent     SessionDeletionPhase = "absent"
	DeletionCleaned    SessionDeletionPhase = "cleaned"
	DeletionFinalizing SessionDeletionPhase = "finalizing"
	DeletionRestoring  SessionDeletionPhase = "restoring"
	DeletionRestored   SessionDeletionPhase = "restored"
	DeletionCancelled  SessionDeletionPhase = "cancelled"
)

// SessionDeletion binds removal to one immutable local admission and exact wire.
// Sources are retained privately before Prepared can advance to Deleting.
type SessionDeletion struct {
	Version           int                         `json:"version"`
	Registration      archive.SessionRegistration `json:"registration"`
	OwnerSHA256       string                      `json:"owner_sha256"`
	Reason            RemovalReason               `json:"reason"`
	Phase             SessionDeletionPhase        `json:"phase"`
	MetadataBytes     []byte                      `json:"-"`
	MetadataSHA256    string                      `json:"metadata_sha256,omitempty"`
	References        []archive.SourceReference   `json:"references,omitempty"`
	RequestToken      string                      `json:"request_token,omitempty"`
	At                time.Time                   `json:"at"`
	RestoringToken    string                      `json:"restoring_token,omitempty"`
	RestorationSHA256 string                      `json:"restoration_sha256,omitempty"`
	LocalRemoved      bool                        `json:"local_removed,omitempty"`
	SHA256            string                      `json:"sha256"`
}

// RetentionRestoration binds a source-set baseline replay to actual durable
// deletion evidence. A newer hook grants restoration, never fabricated content.
type RetentionRestoration struct {
	IntentSHA256   string `json:"intent_sha256"`
	OwnerSHA256    string `json:"owner_sha256"`
	MetadataSHA256 string `json:"metadata_sha256"`
	RequestToken   string `json:"request_token"`
}

// Freeze the admitted owner and source/relationship authority. Hook observations
// and git-head observations may advance without retargeting this deletion;
// relocation or link reconciliation must wait for its terminal resolution.
func deletionOwner(reg archive.SessionRegistration) string {
	reg.HookObservedAt = time.Time{}
	reg.LastHead = nil
	reg.LastHeadSeenAt = nil
	raw, _ := json.Marshal(reg)
	return publicationSHA256(append([]byte("deletion-owner/v1\x00"), raw...))
}

func deletionSHA(j SessionDeletion) string {
	j.SHA256 = ""
	raw, _ := json.Marshal(j)
	return publicationSHA256(raw)
}

func deletionIntentSHA(j SessionDeletion) string {
	j.Phase = DeletionPrepared
	j.RestoringToken = ""
	j.RestorationSHA256 = ""
	j.LocalRemoved = false
	return deletionSHA(j)
}

func deletionPath(id string) string { return filepath.Join(deletionDirectory, id, "intent.json") }

func (j SessionDeletion) validateControl(id string) error {
	if err := j.validateOwner(id); err != nil {
		return err
	}
	switch j.Phase {
	case DeletionPrepared, DeletionDeleting, DeletionAbsent, DeletionCleaned, DeletionRestoring, DeletionRestored, DeletionCancelled, DeletionFinalizing:
	default:
		return ErrDurableStorageRecovery
	}
	if j.LocalRemoved && j.Phase != DeletionCleaned {
		return ErrDurableStorageRecovery
	}
	if err := j.validateReferences(id); err != nil {
		return err
	}
	if j.Phase == DeletionRestoring || j.Phase == DeletionRestored {
		if j.Reason != RemovalReasonRetention || j.RestoringToken == "" || j.RestoringToken == j.RequestToken || j.RestorationSHA256 != j.MetadataSHA256 {
			return ErrDurableStorageRecovery
		}
	}
	if j.Phase == DeletionCancelled && j.Reason != RemovalReasonRetention {
		return ErrDurableStorageRecovery
	}
	if (j.MetadataSHA256 == "") != (len(j.References) == 0) || j.MetadataSHA256 != "" && !validPublicationDigest(j.MetadataSHA256) {
		return ErrDurableStorageRecovery
	}
	return nil
}

func (j SessionDeletion) validate(id string) error {
	if err := j.validateControl(id); err != nil {
		return err
	}
	if len(j.MetadataBytes) == 0 {
		if !j.allowsMissingMetadata() {
			return ErrDurableStorageRecovery
		}
		if j.MetadataSHA256 == "" && len(j.References) > 0 {
			return ErrDurableStorageRecovery
		}
		return nil
	}
	if len(j.MetadataBytes) > 32<<20 || publicationSHA256(j.MetadataBytes) != j.MetadataSHA256 {
		return ErrDurableStorageRecovery
	}
	return j.validateMetadata(id)
}

func (j SessionDeletion) validateMetadata(id string) error {
	var m archive.Metadata
	if json.Unmarshal(j.MetadataBytes, &m) != nil || m.SessionID != id || m.NativeSessionID != j.Registration.NativeSessionID || m.ProjectID != j.Registration.ProjectID || m.Harness.Name != j.Registration.Harness.Name {
		return ErrDurableStorageRecovery
	}
	refs, err := m.SourceReferences()
	if err != nil || !slices.Equal(refs, j.References) {
		return errors.Join(ErrDurableStorageRecovery, err)
	}
	return nil
}

func (j SessionDeletion) validateOwner(id string) error {
	if unsafeDurableSessionID(id) || j.Version != 1 || j.Registration.ArchiveSessionID != id || j.Registration.Validate() != nil || j.OwnerSHA256 != deletionOwner(j.Registration) || j.At.IsZero() || j.SHA256 != deletionSHA(j) || (j.Reason != RemovalReasonRetention && j.Reason != RemovalReasonUndo) {
		return ErrDurableStorageRecovery
	}
	return nil
}

// LoadSessionDeletion preserves corrupt/temporary controls as recovery work.
func (s *Store) LoadSessionDeletion(reg archive.SessionRegistration) (SessionDeletion, bool, error) {
	j, found, err := s.loadSessionDeletion(reg.ArchiveSessionID)
	if err == nil && found && j.OwnerSHA256 != deletionOwner(reg) {
		err = ErrDurableStorageRecovery
	}
	return j, found, err
}

func (s *Store) loadSessionDeletion(id string) (SessionDeletion, bool, error) {
	var j SessionDeletion
	if unsafeDurableSessionID(id) {
		return j, true, ErrDurableStorageRecovery
	}
	home, err := local.OpenRootedHome(s.home)
	if err != nil {
		return j, true, err
	}
	defer func() { _ = home.Close() }()
	dir, err := privateDirectory(home.Root, filepath.Join(deletionDirectory, id), false)
	if errors.Is(err, os.ErrNotExist) {
		return j, false, nil
	}
	if err != nil {
		return j, true, err
	}
	defer func() { _ = dir.Close() }()
	raw, err := s.readDeletionObject(home, dir, "intent.json", deletionWireLimit)
	if errors.Is(err, os.ErrNotExist) {
		present, e := rootHasEntries(home.Root, filepath.Join(deletionDirectory, id))
		if e != nil || present {
			return j, true, errors.Join(ErrDurableStorageRecovery, e)
		}
		return j, false, nil
	}
	if err != nil {
		return j, true, err
	}
	reservation := s.readReservation()
	if !reservation.Reserve(2 * int64(len(raw))) {
		return j, true, errStateBudget
	}
	defer reservation.Release(2 * int64(len(raw)))
	if err = s.unmarshalOwned(raw, &j); err != nil {
		return j, true, errors.Join(ErrDurableStorageRecovery, err)
	}
	if j.MetadataSHA256 != "" && !j.LocalRemoved {
		sources, e := privateDirectory(home.Root, filepath.Join(deletionDirectory, id, "sources"), false)
		if errors.Is(e, os.ErrNotExist) && (j.Phase == DeletionFinalizing || j.Phase == DeletionRestored || j.Phase == DeletionCancelled) {
			sources = nil
			e = nil
		}
		if e != nil {
			return j, true, e
		}
		if sources != nil {
			j.MetadataBytes, e = s.readDeletionObject(home, sources, "metadata.json", 32<<20)
			if errors.Is(e, os.ErrNotExist) && (j.Phase == DeletionFinalizing || j.Phase == DeletionRestored || j.Phase == DeletionCancelled) {
				e = nil
			}
			e = errors.Join(e, sources.Close())
			if e != nil {
				return j, true, e
			}
		}
	}
	if err = s.retainDeletionCharge(3 * int64(len(j.MetadataBytes))); err != nil {
		return j, true, err
	}
	if err = j.validate(id); err != nil {
		return j, true, err
	}
	canonical, err := json.Marshal(j)
	if err != nil || !bytes.Equal(raw, append(canonical, '\n')) {
		return j, true, ErrDurableStorageRecovery
	}
	if err := validateDeletionEntries(dir, j); err != nil {
		return j, true, err
	}
	return j, true, home.Check()
}

func (s *Store) readDeletionObject(home *local.RootedHome, dir *os.Root, name string, limit int64) (out []byte, err error) {
	before, err := dir.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() || before.Size() < 0 || before.Size() > limit {
		return nil, ErrDurableStorageRecovery
	}
	reservation := s.readReservation()
	n := before.Size()
	if !reservation.Reserve(n) {
		return nil, errStateBudget
	}
	keep := false
	defer func() {
		if !keep {
			reservation.Release(n)
		}
	}()
	open := dir.Open
	if s.deletionFileOpen != nil {
		open = func(name string) (*os.File, error) { return s.deletionFileOpen(dir, name) }
	}
	f, err := open(name)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, s.closePublicationFile(f)) }()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(before, opened) || opened.Size() != n || !before.ModTime().Equal(opened.ModTime()) {
		return nil, ErrDurableStorageRecovery
	}
	out = make([]byte, n)
	if _, err = io.ReadFull(f, out); err != nil {
		return nil, err
	}
	var extra [1]byte
	if count, e := f.Read(extra[:]); count != 0 || !errors.Is(e, io.EOF) {
		return nil, errors.Join(ErrDurableStorageRecovery, e)
	}
	after, err := dir.Lstat(name)
	if err != nil || int64(len(out)) != n || !os.SameFile(opened, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return nil, ErrDurableStorageRecovery
	}
	if err = home.Check(); err != nil {
		return nil, err
	}
	if s.resourceBudget != nil && s.resourceReleases != nil {
		keep = true
		*s.resourceReleases = append(*s.resourceReleases, func() { reservation.Release(n) })
	}
	return out, nil
}

func (s *Store) saveSessionDeletion(j SessionDeletion, expected *string) error {
	return s.saveSessionDeletionContext(s.durableContext(), j, expected)
}

func (s *Store) saveSessionDeletionContext(ctx context.Context, j SessionDeletion, expected *string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	previousSHA := j.SHA256
	j.SHA256 = deletionSHA(j)
	if err := j.validate(j.Registration.ArchiveSessionID); err != nil {
		return err
	}
	if _, err := agentmeta.JSONWireBound(ctx, j, durableControlBytes/2-1); err != nil {
		return errors.Join(ErrDurableStorageCapacity, err)
	}
	return config.WithDeletionProtection(s.home, func(g config.DurableStorageGuard) error {
		if len(j.MetadataBytes) > 0 {
			if err := s.retainDeletionMetadata(ctx, g, j); err != nil {
				return err
			}
		}
		commit := func(rename func() error) error {
			unlock, err := s.lockRequest(j.Registration.ArchiveSessionID)
			if err != nil {
				return err
			}
			defer unlock()
			current, found, err := s.LoadRegistration(j.Registration.ArchiveSessionID)
			if err != nil {
				return err
			}
			if found && deletionOwner(current) != j.OwnerSHA256 {
				return ErrDurableStorageRecovery
			}
			if expected != nil {
				req, found, err := s.LoadRequest(j.Registration.ArchiveSessionID)
				if err != nil {
					return err
				}
				if found && req.Token != *expected || !found && *expected != "" {
					return ErrDeletionWorkChanged
				}
			}
			if err := s.checkDeletionControl(g, j, previousSHA); err != nil {
				return err
			}
			return rename()
		}
		return s.writeDurableGuardChecked(ctx, g, deletionPath(j.Registration.ArchiveSessionID), j, commit)
	})
}

// PrepareSessionDeletion freezes complete, exact selecting metadata under the
// request decision. It does not itself authorize any remote deletion.
func (s *Store) PrepareSessionDeletion(reg archive.SessionRegistration, reason RemovalReason, raw []byte, at time.Time, token string) (SessionDeletion, error) {
	if err := s.retainDeletionCharge(3 * int64(len(raw))); err != nil {
		return SessionDeletion{}, err
	}
	old, found, err := s.LoadSessionDeletion(reg)
	if err != nil {
		return old, err
	}
	if found && old.Phase != DeletionRestored && old.Phase != DeletionCancelled {
		if reason == RemovalReasonUndo && old.Reason == RemovalReasonRetention {
			old.Reason = reason
			old.RestoringToken = ""
			old.RestorationSHA256 = ""
			if old.Phase == DeletionRestoring {
				old.Phase = DeletionAbsent
			}
			err = s.saveSessionDeletion(old, nil)
			return old, err
		}
		if old.Reason != reason {
			return old, ErrRemovalPending
		}
		return old, nil
	}
	j := SessionDeletion{Version: 1, Registration: reg, OwnerSHA256: deletionOwner(reg), Reason: reason, Phase: DeletionPrepared, MetadataBytes: raw, RequestToken: token, At: at.UTC()}
	if len(raw) > 0 {
		var m archive.Metadata
		if err = json.Unmarshal(raw, &m); err != nil {
			return j, err
		}
		j.References, err = m.SourceReferences()
		if err != nil {
			return j, err
		}
		j.MetadataSHA256 = publicationSHA256(raw)
	}
	j.SHA256 = deletionSHA(j)
	if err = j.validate(reg.ArchiveSessionID); err != nil {
		return j, err
	}
	var expected *string
	if reason == RemovalReasonRetention {
		expected = &token
	}
	if found {
		j.SHA256 = old.SHA256
	}
	err = s.saveSessionDeletion(j, expected)
	j.SHA256 = deletionSHA(j)
	return j, err
}

// AdvanceSessionDeletion rechecks exact immutable intent before each transition.
func (s *Store) AdvanceSessionDeletion(expected SessionDeletion, phase SessionDeletionPhase) (SessionDeletion, error) {
	j, found, err := s.LoadSessionDeletion(expected.Registration)
	if err != nil {
		return j, err
	}
	if !found || deletionIntentSHA(j) != deletionIntentSHA(expected) {
		return j, ErrDurableStorageRecovery
	}
	allowed := j.Phase == phase || j.Phase == DeletionPrepared && phase == DeletionDeleting || j.Phase == DeletionDeleting && phase == DeletionAbsent || j.Phase == DeletionAbsent && phase == DeletionCleaned
	if !allowed {
		return j, ErrDurableStorageRecovery
	}
	j.Phase = phase
	err = s.saveSessionDeletion(j, nil)
	return j, err
}

// StageDeletionSource retains one exact source under the same shared disk quota.
func (s *Store) StageDeletionSource(j SessionDeletion, ref archive.SourceReference, data []byte) error {
	current, found, err := s.LoadSessionDeletion(j.Registration)
	if err != nil {
		return err
	}
	if !found || deletionIntentSHA(current) != deletionIntentSHA(j) || current.Phase != DeletionPrepared || !slices.Contains(j.References, ref) || len(data) != ref.CompressedBytes || publicationSHA256(data) != ref.SHA256 {
		return ErrDurableStorageRecovery
	}
	return config.WithDeletionProtection(s.home, func(g config.DurableStorageGuard) (err error) {
		q, err := s.openDurableQuota(g)
		if err != nil {
			return err
		}
		defer func() { err = errors.Join(err, q.Close()) }()
		return q.write(filepath.Join(deletionDirectory, j.Registration.ArchiveSessionID, "sources", ref.SHA256+".gz"), int64(len(data)), func(w io.Writer) error { _, err := w.Write(data); return err })
	})
}

// ReadDeletionSource accepts only a member of a validated exact intent.
func (s *Store) ReadDeletionSource(id string, ref archive.SourceReference) ([]byte, error) {
	j, found, err := s.loadSessionDeletion(id)
	if err != nil {
		return nil, err
	}
	if !found || !slices.Contains(j.References, ref) {
		return nil, ErrDurableStorageRecovery
	}
	home, err := local.OpenRootedHome(s.home)
	if err != nil {
		return nil, err
	}
	defer func() { _ = home.Close() }()
	dir, err := privateDirectory(home.Root, filepath.Join(deletionDirectory, id, "sources"), false)
	if err != nil {
		return nil, err
	}
	defer func() { _ = dir.Close() }()
	data, err := s.readDeletionObject(home, dir, ref.SHA256+".gz", int64(ref.CompressedBytes))
	if err == nil && (len(data) != ref.CompressedBytes || publicationSHA256(data) != ref.SHA256) {
		err = ErrDurableStorageRecovery
	}
	return data, err
}

// DeletionRequestChanged reports a hook newer than the reviewed deletion work.
func (s *Store) DeletionRequestChanged(j SessionDeletion) (bool, error) {
	req, found, err := s.LoadRequest(j.Registration.ArchiveSessionID)
	return found && req.Token != j.RequestToken, err
}

func (s *Store) hasDeletionWork(id string) (bool, error) {
	j, found, err := s.loadSessionDeletion(id)
	if err != nil {
		return true, err
	}
	if !found {
		return false, nil
	}
	if !j.LocalRemoved && j.Phase != DeletionRestored && j.Phase != DeletionCancelled {
		return true, nil
	}
	home, err := local.OpenRootedHome(s.home)
	if err != nil {
		return true, err
	}
	defer func() { _ = home.Close() }()
	dir, err := privateDirectory(home.Root, filepath.Join(deletionDirectory, id), false)
	if err != nil {
		return true, err
	}
	defer func() { _ = dir.Close() }()
	f, err := dir.Open(".")
	if err != nil {
		return true, err
	}
	defer func() { _ = f.Close() }()
	entries, err := f.ReadDir(2)
	if err != nil && !errors.Is(err, io.EOF) {
		return true, err
	}
	return len(entries) > 1, nil
}

// CancelUnstartedDeletion withdraws an intent that never acquired destructive
// phase authority. It cannot authorize absent publication or clear owed work.
func (s *Store) CancelUnstartedDeletion(reg archive.SessionRegistration, req Request) error {
	j, found, err := s.LoadSessionDeletion(reg)
	if err != nil {
		return err
	}
	if !found || j.Phase != DeletionPrepared || j.Reason != RemovalReasonRetention || !validRestorationRequest(j, req) {
		return ErrRemovalPending
	}
	j.Phase = DeletionCancelled
	return s.saveSessionDeletion(j, &req.Token)
}

// IntentSHA256 identifies immutable removal authority independently of its phase.
func (j SessionDeletion) IntentSHA256() string { return deletionIntentSHA(j) }

func validateDeletionEntries(dir *os.Root, j SessionDeletion) error {
	f, err := dir.Open(".")
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	entries, err := f.ReadDir(3)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if len(entries) > 2 {
		return ErrDurableStorageRecovery
	}
	for _, entry := range entries {
		info, e := dir.Lstat(entry.Name())
		if e != nil {
			return e
		}
		if entry.Name() == "intent.json" {
			if !info.Mode().IsRegular() || info.Size() > durableControlBytes/2 {
				return ErrDurableStorageRecovery
			}
			continue
		}
		if entry.Name() != "sources" || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return ErrDurableStorageRecovery
		}
		sources, e := privateDirectory(dir, "sources", false)
		if e != nil {
			return e
		}
		e = validateDeletionSourceNames(sources, j)
		e = errors.Join(e, sources.Close())
		if e != nil {
			return e
		}
	}
	return nil
}

func validateDeletionSourceNames(dir *os.Root, j SessionDeletion) error {
	f, err := dir.Open(".")
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	allowed := map[string]int64{"metadata.json": 32 << 20}
	for _, ref := range j.References {
		allowed[ref.SHA256+".gz"] = int64(ref.CompressedBytes)
	}
	remaining := durableEntryLimit
	for {
		entries, e := f.ReadDir(128)
		if e != nil && !errors.Is(e, io.EOF) {
			return e
		}
		for _, entry := range entries {
			remaining--
			limit, ok := allowed[entry.Name()]
			if remaining < 0 || !ok {
				return ErrDurableStorageRecovery
			}
			info, e := dir.Lstat(entry.Name())
			if e != nil || !info.Mode().IsRegular() || info.Size() > limit {
				return errors.Join(ErrDurableStorageRecovery, e)
			}
		}
		if errors.Is(e, io.EOF) {
			return nil
		}
	}
}

func (s *Store) retainDeletionMetadata(ctx context.Context, g config.DurableStorageGuard, j SessionDeletion) (err error) {
	q, err := s.openDurableQuota(g)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, q.Close()) }()
	path := filepath.Join(deletionDirectory, j.Registration.ArchiveSessionID, "sources", "metadata.json")
	if _, e := q.home.Root.Lstat(path); e == nil {
		dir, e := privateDirectory(q.home.Root, filepath.Dir(path), false)
		if e != nil {
			return e
		}
		defer func() { err = errors.Join(err, dir.Close()) }()
		raw, e := s.readDeletionObject(q.home, dir, "metadata.json", 32<<20)
		if e != nil || !bytes.Equal(raw, j.MetadataBytes) {
			return errors.Join(ErrDurableStorageRecovery, e)
		}
		return nil
	} else if !errors.Is(e, os.ErrNotExist) {
		return e
	}
	return q.writeWithHeadroomContext(ctx, path, int64(len(j.MetadataBytes)), func(w io.Writer) error { _, e := w.Write(j.MetadataBytes); return e }, nil, 0)
}

func (j SessionDeletion) validateReferences(id string) error {
	if len(j.References) > archive.MaxPreservedRevisions+1 {
		return ErrDurableStorageRecovery
	}
	seen := map[string]bool{}
	total := 0
	for _, ref := range j.References {
		prefix := "sessions/" + j.Registration.Harness.Name + "/" + id + "/source."
		if !validSourceReference(ref) || ref.Key != prefix+ref.SHA256+".jsonl.gz" || ref.CompressedBytes > MaxPublicationReplayBytes-total {
			return ErrDurableStorageRecovery
		}
		if seen[ref.Key] {
			return ErrDurableStorageRecovery
		}
		seen[ref.Key] = true
		total += ref.CompressedBytes
	}
	return nil
}

func (j SessionDeletion) allowsMissingMetadata() bool {
	return j.MetadataSHA256 == "" || j.LocalRemoved || j.Phase == DeletionFinalizing || j.Phase == DeletionRestored || j.Phase == DeletionCancelled
}

// Check under the durable writer lock immediately before selecting the new
// control. Immutable payload staging does not grant permission to overwrite a
// different phase, a replacement owner, or an absent active journal.
func (s *Store) checkDeletionControl(g config.DurableStorageGuard, next SessionDeletion, previousSHA string) (err error) {
	home, err := g.RootedHome(s.home)
	if err != nil {
		return err
	}
	dir, err := privateDirectory(home.Root, filepath.Join(deletionDirectory, next.Registration.ArchiveSessionID), false)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, dir.Close()) }()
	raw, err := s.readDeletionObject(home, dir, "intent.json", durableControlBytes/2)
	if errors.Is(err, os.ErrNotExist) && next.Phase == DeletionPrepared && previousSHA == next.SHA256 {
		return nil
	}
	if err != nil {
		return errors.Join(ErrDurableStorageRecovery, err)
	}
	var current SessionDeletion
	if err := s.unmarshalOwned(raw, &current); err != nil {
		return errors.Join(ErrDurableStorageRecovery, err)
	}
	if err := s.retainDeletionCharge(int64(len(raw))); err != nil {
		return err
	}
	canonical, err := json.Marshal(current)
	if err != nil || !bytes.Equal(raw, append(canonical, '\n')) || current.validateOwner(next.Registration.ArchiveSessionID) != nil || current.SHA256 != previousSHA || current.OwnerSHA256 != next.OwnerSHA256 {
		return ErrDurableStorageRecovery
	}
	return home.Check()
}

func (s *Store) retainDeletionCharge(n int64) error {
	reservation := s.readReservation()
	if !reservation.Reserve(n) {
		return errStateBudget
	}
	if s.resourceBudget != nil && s.resourceReleases != nil {
		*s.resourceReleases = append(*s.resourceReleases, func() { reservation.Release(n) })
	} else {
		reservation.Release(n)
	}
	return nil
}

// Shared by the existing registration preflight and request-lock commit check,
// including blind replacements and prepared source binding updates. A durable
// intent may appear after a proposed registration was staged.
func (s *Store) checkDeletionRegistrationTransition(next archive.SessionRegistration) error {
	j, found, err := s.loadDeletionControl(next.ArchiveSessionID)
	if err != nil {
		return err
	}
	if found && j.OwnerSHA256 != deletionOwner(next) {
		return ErrRemovalPending
	}
	return nil
}
