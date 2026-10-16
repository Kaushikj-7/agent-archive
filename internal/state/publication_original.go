package state

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
)

const publicationEvidenceLimit int64 = 512 << 20

// PublicationOriginalRole retains exact original bytes, never selecting authority.
type PublicationOriginalRole struct {
	Version             int              `json:"version"`
	Kind                OriginalRoleKind `json:"kind"`
	FrozenContextSHA256 string           `json:"frozen_context_sha256"`
	MetadataSHA256      string           `json:"metadata_sha256"`
	InputMapSHA256      string           `json:"input_map_sha256"`
	ProtectionMapSHA256 string           `json:"protection_map_sha256"`
	Raw                 []byte           `json:"raw"`
	SHA256              string           `json:"sha256"`
	Size                int64            `json:"size"`
}

// EvidenceTarget binds immutable preparation or exact ready selection.
type EvidenceTarget struct {
	Phase             PublicationPhase `json:"phase"`
	PreparationSHA256 string           `json:"preparation_sha256,omitempty"`
	MetadataSHA256    string           `json:"metadata_sha256,omitempty"`
	SourceSetSHA256   string           `json:"source_set_sha256,omitempty"`
	LegacyFileSHA256  string           `json:"legacy_file_sha256,omitempty"`
}

// EvidenceReplacement retains only the current and immediate crash predecessor.
type EvidenceReplacement struct {
	Version                int            `json:"version"`
	Target                 EvidenceTarget `json:"target"`
	PreviousTarget         EvidenceTarget `json:"previous_target"`
	PreviousEvidenceSHA256 string         `json:"previous_evidence_sha256,omitempty"`
}

// PublicationOriginalEvidence is one nonselecting original-evidence journal.
type PublicationOriginalEvidence struct {
	Version          int                      `json:"version"`
	Kind             OriginalEvidenceKind     `json:"kind"`
	SessionID        string                   `json:"session_id"`
	OwnerSHA256      string                   `json:"owner_sha256"`
	DestinationID    string                   `json:"destination_id"`
	AdmissionContext string                   `json:"admission_context"`
	MigrationOrigin  *PublicationOriginalRole `json:"migration_origin,omitempty"`
	PrivacyOrigin    *PublicationOriginalRole `json:"privacy_origin,omitempty"`
	Link             EvidenceReplacement      `json:"link"`
	SHA256           string                   `json:"sha256"`
}

func originalEvidenceSHA(e PublicationOriginalEvidence) string {
	type roleBinding struct {
		Version             int              `json:"Version"`
		Kind                OriginalRoleKind `json:"Kind"`
		FrozenContextSHA256 string           `json:"FrozenContextSHA256"`
		MetadataSHA256      string           `json:"MetadataSHA256"`
		InputMapSHA256      string           `json:"InputMapSHA256"`
		ProtectionMapSHA256 string           `json:"ProtectionMapSHA256"`
		SHA256              string           `json:"SHA256"`
		Size                int64            `json:"Size"`
	}
	binding := func(r *PublicationOriginalRole) *roleBinding {
		if r == nil {
			return nil
		}
		return &roleBinding{r.Version, r.Kind, r.FrozenContextSHA256, r.MetadataSHA256, r.InputMapSHA256, r.ProtectionMapSHA256, r.SHA256, r.Size}
	}
	raw, _ := json.Marshal(struct {
		Version          int                  `json:"Version"`
		Kind             OriginalEvidenceKind `json:"Kind"`
		SessionID        string               `json:"SessionID"`
		OwnerSHA256      string               `json:"OwnerSHA256"`
		DestinationID    string               `json:"DestinationID"`
		AdmissionContext string               `json:"AdmissionContext"`
		MigrationOrigin  *roleBinding         `json:"MigrationOrigin"`
		PrivacyOrigin    *roleBinding         `json:"PrivacyOrigin"`
		Link             EvidenceReplacement  `json:"Link"`
	}{e.Version, e.Kind, e.SessionID, e.OwnerSHA256, e.DestinationID, e.AdmissionContext, binding(e.MigrationOrigin), binding(e.PrivacyOrigin), e.Link})
	return publicationSHA256(append([]byte("publication-original-evidence/v2\x00"), raw...))
}

func evidenceTarget(p PendingPublication, raw []byte) EvidenceTarget {
	if p.JournalVersion == 0 {
		phase := PublicationReady
		if p.History != nil && p.History.Preparing {
			phase = PublicationPreparing
		}
		return EvidenceTarget{Phase: phase, LegacyFileSHA256: publicationSHA256(raw)}
	}
	metadataSHA, setSHA := "", ""
	if p.Phase == PublicationReady && p.Commit != nil {
		metadataSHA = p.Commit.MetadataSHA256
		setSHA = p.Commit.SourceSetSHA256
	}
	return EvidenceTarget{Phase: p.Phase, PreparationSHA256: p.Preparation.SHA256, MetadataSHA256: metadataSHA, SourceSetSHA256: setSHA}
}

func validEvidenceTarget(t EvidenceTarget) bool {
	if t.Phase != PublicationPreparing && t.Phase != PublicationReady {
		return false
	}
	if t.LegacyFileSHA256 != "" {
		return validPublicationDigest(t.LegacyFileSHA256) && t.PreparationSHA256 == "" && t.MetadataSHA256 == "" && t.SourceSetSHA256 == ""
	}
	if !validPublicationDigest(t.PreparationSHA256) {
		return false
	}
	if t.Phase == PublicationPreparing {
		return t.MetadataSHA256 == "" && t.SourceSetSHA256 == ""
	}
	return validPublicationDigest(t.MetadataSHA256) && validPublicationDigest(t.SourceSetSHA256)
}

func originalRole(p PendingPublication, raw []byte, destination, admission string) (*PublicationOriginalRole, error) {
	var metadata archive.Metadata
	if err := json.Unmarshal(p.MetadataBytes, &metadata); err != nil {
		return nil, err
	}
	sources, err := selectedPublicationSources(p, destination, admission)
	if err != nil {
		return nil, err
	}
	inputRaw, _ := json.Marshal(struct {
		Inputs         []HistoryInput `json:"Inputs"`
		SkillEvidence  string         `json:"SkillEvidence"`
		FilterVersion  string         `json:"FilterVersion"`
		AdapterVersion string         `json:"AdapterVersion"`
	}{func() []HistoryInput {
		if p.History == nil {
			return nil
		}
		return p.History.Inputs
	}(), p.SkillEvidence, p.Bundle.Capture.FilterVersion, p.Bundle.Capture.AdapterVersion})
	kind := OriginalSealedPending
	if p.History != nil && p.History.Preparing {
		kind = OriginalHistoryPreparing
	}
	return &PublicationOriginalRole{Version: 1, Kind: kind, FrozenContextSHA256: publicationOwner(metadata, destination, admission), MetadataSHA256: publicationSHA256(p.MetadataBytes), InputMapSHA256: publicationSHA256(append([]byte("original-input-map/v1\x00"), inputRaw...)), ProtectionMapSHA256: payloadSetSHA(sources), Raw: raw, SHA256: publicationSHA256(raw), Size: int64(len(raw))}, nil
}

func (s *Store) validateOriginalEvidence(e PublicationOriginalEvidence, id string) error {
	reservation := s.readReservation()
	total := int64(0)
	for _, role := range []*PublicationOriginalRole{e.MigrationOrigin, e.PrivacyOrigin} {
		if role != nil {
			if role.Size < 0 || role.Size > publicationEvidenceLimit-total {
				return ErrDurableStorageRecovery
			}
			total += role.Size
		}
	}
	if s.resourceBudget != nil {
		if !reservation.Reserve(total) {
			return errStateBudget
		}
		defer reservation.Release(total)
	}
	return e.validate(id)
}

func (e PublicationOriginalEvidence) validate(id string) error {
	if originalEvidenceEnvelopeInvalid(e, id) {
		return ErrDurableStorageRecovery
	}
	total := int64(0)
	for i, role := range []*PublicationOriginalRole{e.MigrationOrigin, e.PrivacyOrigin} {
		if role == nil {
			continue
		}
		if role.Version != 1 || (role.Kind != OriginalSealedPending && role.Kind != OriginalHistoryPreparing) {
			return ErrDurableStorageRecovery
		}
		if role.Size != int64(len(role.Raw)) || role.Size <= 0 || role.SHA256 != publicationSHA256(role.Raw) || role.Size > publicationEvidenceLimit-total {
			return ErrDurableStorageRecovery
		}
		total += role.Size
		if err := e.validateRole(id, i, role); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) rootedPublicationBytes(g config.DurableStorageGuard, path string) (raw []byte, closeBytes func(), err error) {
	reservation := s.readReservation()
	home, err := g.RootedHome(s.home)
	if err != nil {
		return nil, nil, err
	}
	before, err := home.Root.Lstat(path)
	if err != nil {
		return nil, nil, err
	}
	n := before.Size()
	if !before.Mode().IsRegular() || n < 0 || n > publicationEvidenceLimit {
		return nil, nil, ErrDurableStorageRecovery
	}
	release := func() {}
	if s.resourceBudget != nil {
		if !reservation.Reserve(2 * n) {
			return nil, nil, errStateBudget
		}
		release = func() { reservation.Release(2 * n) }
	}
	fail := func(e error) ([]byte, func(), error) { release(); return nil, nil, e }
	f, err := home.Root.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return fail(err)
	}
	defer func() {
		if closeErr := s.closePublicationFile(f); closeErr != nil {
			if err == nil {
				release()
				raw = nil
				closeBytes = nil
			}
			err = errors.Join(err, closeErr)
		}
	}()
	opened, err := f.Stat()
	if err != nil || !sameDurableStamp(before, opened) {
		return fail(errors.Join(ErrDurableStorageRecovery, err))
	}
	raw, err = io.ReadAll(io.LimitReader(f, n+1))
	if err != nil || int64(len(raw)) != n {
		return fail(errors.Join(ErrDurableStorageRecovery, err))
	}
	after, err := home.Root.Lstat(path)
	if err != nil || !sameDurableStamp(before, after) {
		return fail(errors.Join(ErrDurableStorageRecovery, err))
	}
	if err = g.CheckHome(s.home); err != nil {
		return fail(err)
	}
	if err = s.durableContext().Err(); err != nil {
		return fail(err)
	}
	return raw, release, nil
}

func evidencePath(id string) string {
	return filepath.Join("publication-evidence", id, "original.json")
}

// savePublicationPendingGuard preserves a supported legacy journal before its
// first protocol2 replacement, and validates every later crash-link advancement.
func (s *Store) savePublicationPendingGuard(g config.DurableStorageGuard, id string, p PendingPublication) error {
	path := filepath.Join("pending", id+".json")
	old, release, err := s.rootedPublicationBytes(g, path)
	if errors.Is(err, os.ErrNotExist) {
		return s.savePendingGuard(s.durableContext(), g, id, p)
	}
	if err != nil {
		return err
	}
	defer release()
	var previous PendingPublication
	if err = json.Unmarshal(old, &previous); err != nil {
		return errors.Join(ErrDurableStorageRecovery, err)
	}
	target := evidenceTarget(p, nil)
	raw, closeEvidence, readErr := s.rootedPublicationBytes(g, evidencePath(id))
	var evidence PublicationOriginalEvidence
	if readErr == nil {
		defer closeEvidence()
		if err = closedPublicationDecode(raw, &evidence); err != nil {
			return err
		}
		if err = s.validateOriginalEvidence(evidence, id); err != nil {
			return err
		}
		if evidence.DestinationID != p.Preparation.DestinationID || evidence.AdmissionContext != p.Preparation.AdmissionContext {
			return ErrDurableStorageRecovery
		}
		installed := evidenceTarget(previous, old)
		if installed != evidence.Link.Target {
			if installed != evidence.Link.PreviousTarget || target != evidence.Link.Target {
				return ErrDurableStorageRecovery
			}
			return s.savePendingGuard(s.durableContext(), g, id, p)
		}
		if target == installed {
			return s.savePendingGuard(s.durableContext(), g, id, p)
		}
		if replacesReadyPrivacyOriginal(p, previous) && evidence.PrivacyOrigin == nil {
			role, e := originalRole(previous, old, p.Preparation.DestinationID, p.Preparation.AdmissionContext)
			if e != nil {
				return e
			}
			evidence.PrivacyOrigin = role
		}
		evidence.Link = EvidenceReplacement{Version: 1, Target: target, PreviousTarget: installed, PreviousEvidenceSHA256: evidence.SHA256}
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return readErr
	} else if previous.JournalVersion == 0 {
		role, e := originalRole(previous, old, p.Preparation.DestinationID, p.Preparation.AdmissionContext)
		if e != nil {
			return e
		}
		evidence = PublicationOriginalEvidence{Version: 2, Kind: OriginalEvidence, SessionID: id, OwnerSHA256: role.FrozenContextSHA256, DestinationID: p.Preparation.DestinationID, AdmissionContext: p.Preparation.AdmissionContext, MigrationOrigin: role, Link: EvidenceReplacement{Version: 1, Target: target, PreviousTarget: evidenceTarget(previous, old)}}
	} else if replacesReadyPrivacyOriginal(p, previous) {
		role, e := originalRole(previous, old, p.Preparation.DestinationID, p.Preparation.AdmissionContext)
		if e != nil {
			return e
		}
		evidence = PublicationOriginalEvidence{Version: 2, Kind: OriginalEvidence, SessionID: id, OwnerSHA256: role.FrozenContextSHA256, DestinationID: p.Preparation.DestinationID, AdmissionContext: p.Preparation.AdmissionContext, PrivacyOrigin: role, Link: EvidenceReplacement{Version: 1, Target: target, PreviousTarget: evidenceTarget(previous, old)}}
	} else {
		return s.savePendingGuard(s.durableContext(), g, id, p)
	}
	evidence.SHA256 = originalEvidenceSHA(evidence)
	if err = s.validateOriginalEvidence(evidence, id); err != nil {
		return err
	}
	if err = s.writeDurableGuard(s.durableContext(), g, evidencePath(id), evidence); err != nil {
		return err
	}
	return s.savePendingGuard(s.durableContext(), g, id, p)
}

// LoadPublicationPending is the publication owner's closed migration replay.
// Generic native/deletion eligibility retains its opaque-evidence refusal.
func (s *Store) LoadPublicationPending(id string) (pending PendingPublication, found bool, err error) {
	reservation := s.readReservation()
	pending, found, err = s.LoadPending(id)
	if err == nil {
		return pending, found, nil
	}
	owed, probe := s.hasPublicationEvidence(id)
	if probe != nil || !owed {
		return pending, found, errors.Join(err, probe)
	}
	cfg, present, e := config.Load(s.home)
	if e != nil || !present || !cfg.SourceSetPublicationProtection {
		return PendingPublication{}, true, errors.Join(ErrDurableStorageRecovery, e)
	}
	if !safeFileComponent(id) {
		return PendingPublication{}, true, ErrDurableStorageRecovery
	}
	err = config.WithDurableStorage(s.home, func(g config.DurableStorageGuard) (err error) {
		home, e := g.RootedHome(s.home)
		if e != nil {
			return e
		}
		if e = s.validatePublicationEvidenceDirectory(home.Root, id); e != nil {
			return e
		}
		raw, release, e := s.rootedPublicationBytes(g, evidencePath(id))
		if e != nil {
			return e
		}
		defer release()
		var evidence PublicationOriginalEvidence
		if e = closedPublicationDecode(raw, &evidence); e != nil {
			return e
		}
		if e = s.validateOriginalEvidence(evidence, id); e != nil {
			return e
		}
		body, closeBody, e := s.rootedPublicationBytes(g, filepath.Join("pending", id+".json"))
		if e != nil {
			return e
		}
		defer closeBody()

		if e = s.decodePublicationOwned(s.durableContext(), body, &pending); e != nil {
			return e
		}
		if pending.Bundle.ArchiveSessionID != id {
			return ErrDurableStorageRecovery
		}
		target := evidenceTarget(pending, body)
		if target != evidence.Link.Target && target != evidence.Link.PreviousTarget {
			return ErrDurableStorageRecovery
		}
		if pending.JournalVersion == 5 && (pending.Preparation.DestinationID != evidence.DestinationID || pending.Preparation.AdmissionContext != evidence.AdmissionContext || pending.Preparation.OwnerSHA256 != evidence.OwnerSHA256) {
			return ErrDurableStorageRecovery
		}
		if e = s.validateReadablePendingContext(s.durableContext(), pending); e != nil {
			return e
		}
		if e = pending.ValidateHistoryBudgeted(id, s.resourceBudget); e != nil {
			return e
		}
		if s.resourceBudget != nil {
			n := int64(len(body))
			if !reservation.Reserve(n) {
				return errStateBudget
			}
			*s.resourceReleases = append(*s.resourceReleases, func() { reservation.Release(n) })
		}
		return nil
	})
	return pending, true, errors.Join(err)
}

// SettlePublicationMigration removes only the migration role after a full local
// selecting commit. Privacy-original discharge remains its lifecycle owner's work.
func (s *Store) SettlePublicationMigration(id string, p PendingPublication) error {
	return s.settlePublicationEvidence(id, p, false)
}

// SettleOrdinaryPublicationPrivacy releases the exact original only after its
// ordinary privacy successor is fully selected in durable published state.
// The collector records retirement and consumes its closing proof before this.
func (s *Store) SettleOrdinaryPublicationPrivacy(id string, p PendingPublication) error {
	if p.History != nil || p.Commit == nil || p.Commit.Purpose != PublicationPrivacyRewrite {
		return ErrDurableStorageRecovery
	}
	return s.settlePublicationEvidence(id, p, true)
}

func (s *Store) settlePublicationEvidence(id string, p PendingPublication, privacy bool) error {
	owed, err := s.hasPublicationEvidence(id)
	if err != nil || !owed {
		return err
	}
	return config.WithPublicationComposition(s.home, func(composition config.PublicationCompositionGuard) (err error) {
		g, err := composition.Storage(s.home)
		if err != nil {
			return err
		}
		raw, release, err := s.rootedPublicationBytes(g, evidencePath(id))
		if err != nil {
			return err
		}
		defer release()
		var evidence PublicationOriginalEvidence
		if err = closedPublicationDecode(raw, &evidence); err != nil {
			return err
		}
		if err = s.validateOriginalEvidence(evidence, id); err != nil {
			return err
		}
		publishedRaw, closePublished, err := s.rootedPublicationBytes(g, filepath.Join("published", id+".json"))
		if err != nil {
			return err
		}
		defer closePublished()
		var published publishedState
		if err = s.decodePublicationOwned(s.durableContext(), publishedRaw, &published); err != nil {
			return err
		}
		if migrationSelectingStateChanged(published, p, evidence) {
			return ErrDurableStorageRecovery
		}
		if p.JournalVersion != 5 || p.Phase != PublicationReady || p.ValidatePublication() != nil || evidence.Link.Target != evidenceTarget(p, nil) {
			return ErrDurableStorageRecovery
		}
		evidence.MigrationOrigin = nil
		if privacy {
			evidence.PrivacyOrigin = nil
		}
		if evidence.PrivacyOrigin != nil {
			evidence.SHA256 = originalEvidenceSHA(evidence)
			return s.writeDurableGuard(s.durableContext(), g, evidencePath(id), evidence)
		}
		home, err := g.RootedHome(s.home)
		if err != nil {
			return err
		}
		dir, err := privateDirectory(home.Root, filepath.Dir(evidencePath(id)), false)
		if err != nil {
			return err
		}
		defer func() { err = errors.Join(err, s.closePublicationRoot(dir)) }()
		file, err := dir.Open(".")
		if err != nil {
			return err
		}
		entries, e := file.ReadDir(2)
		if errors.Is(e, io.EOF) {
			e = nil
		}
		if e = errors.Join(e, s.closePublicationFile(file)); e != nil {
			return e
		}
		if len(entries) != 1 || entries[0].Name() != "original.json" {
			return ErrDurableStorageRecovery
		}
		if err = g.CheckHome(s.home); err != nil {
			return err
		}
		if err = dir.Remove("original.json"); err != nil {
			return err
		}
		syncDir, err := dir.Open(".")
		if err != nil {
			return err
		}
		return errors.Join(syncDir.Sync(), s.closePublicationFile(syncDir))
	})
}

// Evidence is exactly one original control file. Anonymous siblings cannot be
// interpreted or ignored by an owning replay or a native eligibility guard.
func (s *Store) validatePublicationEvidenceDirectory(root *os.Root, id string) (err error) {
	reservation := s.readReservation()
	const charge = 64 << 10
	if s.resourceBudget != nil && !reservation.Reserve(charge) {
		return errStateBudget
	}
	if s.resourceBudget != nil {
		defer reservation.Release(charge)
	}
	dir, err := privateDirectory(root, filepath.Join("publication-evidence", id), false)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, dir.Close()) }()
	file, err := dir.Open(".")
	if err != nil {
		return err
	}
	entries, readErr := file.ReadDir(2)
	if errors.Is(readErr, io.EOF) {
		readErr = nil
	}
	err = errors.Join(readErr, s.closePublicationFile(file))
	if err != nil {
		return err
	}
	if len(entries) != 1 || entries[0].Name() != "original.json" {
		return ErrDurableStorageRecovery
	}
	return nil
}

func (s *Store) closePublicationFile(file *os.File) error {
	if s.publicationFileClose != nil {
		return s.publicationFileClose(file)
	}
	return file.Close()
}

func originalEvidenceEnvelopeInvalid(e PublicationOriginalEvidence, id string) bool {
	return e.Version != 2 || e.Kind != OriginalEvidence || e.SessionID != id || e.Link.Version != 1 || !validEvidenceTarget(e.Link.Target) || !validEvidenceTarget(e.Link.PreviousTarget) || e.Link.PreviousEvidenceSHA256 != "" && !validPublicationDigest(e.Link.PreviousEvidenceSHA256) || e.SHA256 != originalEvidenceSHA(e) || e.MigrationOrigin == nil && e.PrivacyOrigin == nil
}

func (e PublicationOriginalEvidence) validateRole(id string, i int, role *PublicationOriginalRole) error {
	var pending PendingPublication
	if err := json.Unmarshal(role.Raw, &pending); err != nil {
		return err
	}
	if pending.Bundle.ArchiveSessionID != id {
		return ErrDurableStorageRecovery
	}
	rebuilt, err := originalRole(pending, role.Raw, e.DestinationID, e.AdmissionContext)
	if err != nil {
		return err
	}
	if rebuilt.Kind != role.Kind || rebuilt.FrozenContextSHA256 != role.FrozenContextSHA256 || rebuilt.MetadataSHA256 != role.MetadataSHA256 || rebuilt.InputMapSHA256 != role.InputMapSHA256 || rebuilt.ProtectionMapSHA256 != role.ProtectionMapSHA256 || rebuilt.FrozenContextSHA256 != e.OwnerSHA256 {
		return ErrDurableStorageRecovery
	}
	if i == 0 && pending.JournalVersion != 0 {
		return ErrDurableStorageRecovery
	}
	if i == 1 && (pending.Commit == nil || pending.ValidatePublication() != nil) {
		return ErrDurableStorageRecovery
	}
	return nil
}

func migrationSelectingStateChanged(published publishedState, p PendingPublication, evidence PublicationOriginalEvidence) bool {
	return published.Commit == nil || p.Commit == nil || published.Commit.MetadataSHA256 != p.Commit.MetadataSHA256 || published.Commit.SourceSetSHA256 != p.Commit.SourceSetSHA256 || published.Commit.PayloadSetSHA256 != p.Commit.PayloadSetSHA256 || evidence.DestinationID != p.Commit.DestinationID || evidence.AdmissionContext != p.Commit.AdmissionContext
}

func (s *Store) closePublicationRoot(root *os.Root) error {
	if s.publicationRootClose != nil {
		return s.publicationRootClose(root)
	}
	return root.Close()
}

// replacesReadyPrivacyOriginal distinguishes a new privacy successor from a
// checkpoint within its immutable preparation. Preparing journals have no ready
// commit and remain protected by their existing original-evidence roles.
func replacesReadyPrivacyOriginal(next, previous PendingPublication) bool {
	return next.Preparation != nil && next.Preparation.Purpose == PublicationPrivacyRewrite &&
		previous.JournalVersion == 5 && previous.Phase == PublicationReady && previous.Commit != nil &&
		previous.Preparation != nil && previous.Preparation.SHA256 != next.Preparation.SHA256
}
