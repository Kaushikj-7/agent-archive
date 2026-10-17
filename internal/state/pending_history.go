package state

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"syscall"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
)

const pendingHistoryVersion = 1

const maxPendingHistoryBytes = 128 << 20

// One batch exceeds the largest live final-plus-input set, so retries progress.
const pendingStageCleanupBatch = 2*archive.MaxHistorySpans + 3

var stagedSourceName = regexp.MustCompile(`^[0-9a-f]{64}\.gz$`)

var stagedTempName = regexp.MustCompile(`^\.pending-([0-9]+|[0-9a-f]{32})$`)

// PendingHistory freezes one complete reference-set replacement. Source payloads
// are staged individually before this descriptor, outside the journal JSON.
type PendingHistory struct {
	// MaintenanceOwed survives exact committed acknowledgement under a stronger policy.
	MaintenanceOwed bool `json:"maintenance_owed,omitempty"`
	// Preparing performs private sequential privacy work before any remote write.
	FilterVersion          string          `json:"filter_version,omitempty"`
	AdapterVersion         string          `json:"adapter_version,omitempty"`
	PreparedAt             time.Time       `json:"prepared_at,omitzero"`
	Preparing              bool            `json:"preparing,omitempty"`
	PrivacyCursor          int             `json:"privacy_cursor,omitempty"`
	Inputs                 []HistoryInput  `json:"inputs,omitempty"`
	Version                int             `json:"version"`
	ExpectedMetadataSHA256 string          `json:"expected_metadata_sha256,omitempty"`
	Sources                []PendingSource `json:"sources,omitempty"`
	Retired                []RetiredSource `json:"retired,omitempty"`
}

// HistoryInput freezes the capture facts needed to read an earlier reference
// while the next metadata document is being prepared under newer privacy rules.
type HistoryInput struct {
	// ParentSessionID freezes this immutable input's header authority separately
	// from the output envelope. Nil retains pre-provenance journal compatibility;
	// a pointer to an empty string positively records an unresolved parent.
	NativeChild         *bool                   `json:"native_child,omitempty"`
	ParentSessionID     *string                 `json:"parent_session_id,omitempty"`
	SourceSchemaVersion int                     `json:"source_schema_version,omitempty"`
	Reference           archive.SourceReference `json:"reference"`
	RevisionID          string                  `json:"revision_id"`
	CapturedAt          time.Time               `json:"captured_at"`
	FilterVersion       string                  `json:"filter_version"`
	AdapterVersion      string                  `json:"adapter_version,omitempty"`
}

// PendingSource identifies an immutable owned stage by checksum-derived name.
type PendingSource struct {
	Reference archive.SourceReference `json:"reference"`
	Name      string                  `json:"name"`
}

// RetiredSource retains every cleanup obligation until acknowledgement succeeds.
type RetiredSource struct {
	RetiredAt        time.Time               `json:"retired_at"`
	Reference        archive.SourceReference `json:"reference"`
	PrivacySensitive bool                    `json:"privacy_sensitive,omitempty"`
}

// ValidateHistoryBudgeted reserves the independent ephemeral metadata view used
// by journal validation. The input bytes retain their original caller ownership.
func (owned *PendingPublication) ValidateHistoryBudgeted(id string, budget *agentapi.NativeReadBudget) error {
	p := *owned
	reservation := budget.Reservation()
	if p.History == nil {
		return nil
	}
	n := int64(len(p.MetadataBytes))
	if p.Preparation != nil {
		for _, input := range p.Preparation.Inputs {
			if h := input.HookObservations; h != nil {
				if len(h.Body) > 32<<20 {
					return ErrDurableStorageCapacity
				}
				n += 8 * int64(len(h.Body))
			}
		}
	}
	if !reservation.Reserve(n) {
		return errStateBudget
	}
	defer reservation.Release(n)
	return p.ValidateHistory(id)
}

// ValidateHistory refuses future journals and references outside this session.
func (owned *PendingPublication) ValidateHistory(id string) error {
	p := *owned
	if p.History == nil {
		return nil
	}
	if p.History.Version != pendingHistoryVersion && (p.JournalVersion != 5 || p.History.Version != 2) {
		return errors.Join(ErrDurableStorageRecovery, errors.New("pending history requires a newer writer"))
	}
	if err := validatePredecessorSHA(p.History.ExpectedMetadataSHA256); err != nil {
		return err
	}

	var m archive.Metadata
	if err := json.Unmarshal(p.MetadataBytes, &m); err != nil {
		return err
	}
	refs, err := m.SourceReferences()
	if err != nil {
		return err
	}
	expectedKey, keyErr := archive.MetadataObjectKey(m.Harness.Name, id)
	if keyErr != nil || expectedKey != p.MetadataKey {
		return errors.New("pending history metadata key mismatch")
	}
	if m.SessionID != id || m.SourceBundle != p.SourceReference() {
		return errors.New("pending history identity mismatch")
	}
	known := map[archive.SourceReference]bool{}
	for _, r := range refs {
		known[r] = true
	}
	if err := p.validateHistoryInputs(m); err != nil {
		return err
	}
	// A stronger successor can owe original, prepared and acknowledged refs.
	if len(p.History.Sources) > archive.MaxHistorySpans+1 || len(p.History.Retired) > 3*(archive.MaxHistorySpans+1) {
		return errors.New("pending history exceeds source limit")
	}
	size := 0
	seen := map[string]bool{}
	for _, stage := range p.History.Sources {
		r := stage.Reference
		if !stagedSourceName.MatchString(stage.Name) || stage.Name != r.SHA256+".gz" || !known[r] || seen[stage.Name] {
			return errors.New("invalid pending history stage")
		}
		seen[stage.Name] = true
		if r.CompressedBytes > maxPendingHistoryBytes-size {
			return errors.New("pending history exceeds byte limit")
		}
		size += r.CompressedBytes
	}
	for _, retired := range p.History.Retired {
		prior := m
		prior.SchemaVersion = archive.MetadataSchemaVersion
		prior.History = nil
		prior.SourceBundle = retired.Reference
		if prior.ValidateSourceReference() != nil || known[retired.Reference] || (retired.RetiredAt.IsZero() && !p.History.Preparing) {
			return errors.New("invalid retired history reference")
		}
	}
	return nil
}

func (s *Store) checkPendingHistoryVersion(id string) error {
	var header struct {
		History *struct {
			Version int `json:"version"`
		} `json:"history"`
	}
	if err := s.readBudgeted(s.pendingPath(id), &header, false); err == nil {
		if header.History != nil && header.History.Version != pendingHistoryVersion && header.History.Version != 2 {
			return errors.Join(ErrDurableStorageRecovery, errors.New("pending history requires a newer writer"))
		}
	}
	// Protected damage is classified by LoadPending without discarding evidence.
	return nil
}

func validatePendingHistorySource(id, name string) error {
	if !safeFileComponent(id) || !stagedSourceName.MatchString(name) {
		return errors.New("invalid history stage identity")
	}
	return nil
}

// StagePendingSource durably freezes bytes before the journal references them.
func (s *Store) StagePendingSource(id string, ref archive.SourceReference, data []byte) (PendingSource, error) {
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != ref.SHA256 || len(data) != ref.CompressedBytes || len(data) > maxPendingHistoryBytes {
		return PendingSource{}, errors.New("invalid staged source size")
	}
	name := ref.SHA256 + ".gz"
	err := validatePendingHistorySource(id, name)
	if err != nil {
		return PendingSource{}, err
	}
	if err := config.WithDurableStorage(s.home, func(g config.DurableStorageGuard) error {
		return s.stagePendingSourceGuard(g, id, ref, data)
	}); err != nil {
		return PendingSource{}, err
	}
	return PendingSource{Reference: ref, Name: name}, nil
}

// StagePublicationSource freezes a protocol2 input after the composition floor.
func (s *Store) StagePublicationSource(id string, ref archive.SourceReference, data []byte) (PendingSource, error) {
	if err := validatePendingHistorySource(id, ref.SHA256+".gz"); err != nil {
		return PendingSource{}, err
	}
	if len(data) != ref.CompressedBytes || len(data) > maxPendingHistoryBytes || publicationSHA256(data) != ref.SHA256 {
		return PendingSource{}, ErrDurableStorageRecovery
	}
	err := config.WithPublicationComposition(s.home, func(g config.PublicationCompositionGuard) error {
		storage, e := g.Storage(s.home)
		if e != nil {
			return e
		}
		return s.stagePendingSourceGuard(storage, id, ref, data)
	})
	return PendingSource{Reference: ref, Name: ref.SHA256 + ".gz"}, err
}

// ReadPendingSource reads only the journal's bounded private checksum stage.
func (s *Store) ReadPendingSource(id string, stage PendingSource) ([]byte, error) {
	err := validatePendingHistorySource(id, stage.Name)
	if err != nil {
		return nil, err
	}
	home, err := local.OpenRootedHome(s.home)
	if err != nil {
		return nil, err
	}
	defer func() { _ = home.Close() }()
	dir, err := privateDirectory(home.Root, filepath.Join("sessions", id, "pending-sources"), false)
	if err != nil {
		return nil, err
	}
	defer func() { _ = dir.Close() }()
	info, err := dir.Lstat(stage.Name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() != int64(stage.Reference.CompressedBytes) || info.Size() > maxPendingHistoryBytes {
		return nil, errors.New("invalid history stage size or type")
	}
	f, err := dir.OpenFile(stage.Name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, errors.New("history stage changed while opening")
	}

	data, err := io.ReadAll(io.LimitReader(f, int64(stage.Reference.CompressedBytes)+1))
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	if len(data) != stage.Reference.CompressedBytes || hex.EncodeToString(sum[:]) != stage.Reference.SHA256 {
		return nil, errors.New("history stage checksum mismatch")
	}
	return data, nil
}

func validatePredecessorSHA(value string) error {
	if value == "" {
		return nil
	}
	checksum, err := hex.DecodeString(value)
	if err != nil || len(checksum) != sha256.Size {
		return errors.New("invalid frozen predecessor checksum")
	}
	return nil
}

func (owned *PendingPublication) validateHistoryInputs(m archive.Metadata) error {
	p := *owned
	if p.History.PrivacyCursor < 0 || p.History.PrivacyCursor > len(p.History.Inputs) || len(p.History.Inputs) > archive.MaxHistorySpans+1 || p.History.Preparing && p.Attempted {
		return errors.New("invalid history preparation progress")
	}
	seen := map[string]bool{}
	for _, input := range p.History.Inputs {
		if len(input.AdapterVersion) > 4096 || seen[input.RevisionID] || input.SourceSchemaVersion != 0 && input.SourceSchemaVersion != archive.SourceSchemaVersion && input.SourceSchemaVersion != archive.HistorySourceSchemaVersion {
			return errors.New("invalid frozen input provenance")
		}
		seen[input.RevisionID] = true
		member := m.History != nil && m.History.CurrentRevision == input.RevisionID && m.CapturedAt.Equal(input.CapturedAt)
		if m.History != nil {
			for _, revision := range m.History.Preserved {
				member = member || revision.RevisionID == input.RevisionID && revision.CapturedAt.Equal(input.CapturedAt)
			}
		}
		if !member {
			return errors.New("frozen input does not belong to the complete manifest")
		}
		prior := m
		prior.History = &archive.RevisionHistory{CurrentRevision: input.RevisionID}
		prior.SourceBundle = input.Reference
		prior.CapturedAt = input.CapturedAt
		prior.FilterVersion = input.FilterVersion
		if prior.ValidateSourceReference() != nil || input.CapturedAt.IsZero() || input.FilterVersion == "" {
			return errors.New("invalid history preparation input")
		}
	}
	return nil
}

// SweepPendingSources removes a bounded slice of abandoned private stages under
// collector ownership. Unknown/damaged journals fail closed; every live stage
// and original preparation input remains protected.
func (s *Store) SweepPendingSources(id string) error {
	if owed, err := s.hasPublicationEvidence(id); err != nil || owed {
		return errors.Join(ErrDurableStorageRecovery, err)
	}
	pending, found, err := s.LoadPending(id)
	if err != nil {
		return err
	}
	protected := map[string]bool{}
	if found && pending.History != nil {
		for _, stage := range pending.History.Sources {
			protected[stage.Name] = true
		}
		for _, input := range pending.History.Inputs {
			protected[input.Reference.SHA256+".gz"] = true
		}
	}
	return s.removePendingSources(id, protected)
}

func (s *Store) removePendingSources(id string, protected map[string]bool) error {
	if owed, err := s.hasPublicationEvidence(id); err != nil || owed {
		return errors.Join(ErrDurableStorageRecovery, err)
	}
	if !safeFileComponent(id) {
		return errors.New("invalid history stage identity")
	}
	home, err := local.OpenRootedHome(s.home)
	if err != nil {
		return err
	}
	defer func() { _ = home.Close() }()
	dir, err := privateDirectory(home.Root, filepath.Join("sessions", id, "pending-sources"), false)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	f, err := dir.Open(".")
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	// Read at most one bounded batch, including the sentinel. No recursive removal.
	entries, err := f.ReadDir(pendingStageCleanupBatch)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	for _, entry := range entries {
		if !stagedSourceName.MatchString(entry.Name()) && !stagedTempName.MatchString(entry.Name()) {
			return errors.New("unsafe history stage entry")
		}
		if protected[entry.Name()] {
			continue
		}
		info, err := dir.Lstat(entry.Name())
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return errors.New("unsafe history stage type")
		}
		if err := dir.Remove(entry.Name()); err != nil {
			return err
		}
	}
	if len(entries) > 0 {
		if err := f.Sync(); err != nil {
			return err
		}
	}
	if len(entries) == pendingStageCleanupBatch {
		return errors.New("history stage cleanup remains pending")
	}
	if len(protected) == 0 {
		if err := home.Root.Remove(filepath.Join("sessions", id, "pending-sources")); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		parent, err := privateDirectory(home.Root, filepath.Join("sessions", id), false)
		if err != nil {
			return err
		}
		defer func() { _ = parent.Close() }()
		d, err := parent.Open(".")
		if err != nil {
			return err
		}
		return errors.Join(d.Sync(), d.Close())
	}
	return nil
}
