package archive

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"
)

// HistorySourceSchemaVersion retains self-contained Codex revision evidence.
const HistorySourceSchemaVersion = 3

// HistoryMetadataSchemaVersion adds bounded preserved revision references.
const HistoryMetadataSchemaVersion = 2

// MaxHistorySpans bounds source graph and retained manifest complexity.
const MaxHistorySpans = 64

// MaxHistoryRecords bounds retained record ordinal metadata.
const MaxHistoryRecords = 1_000_000

var historyID = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// SourceHistory identifies one selected physical revision and logical ownership.
// OwnStart is absent for ordinary children, including children without copied history.
type SourceHistory struct {
	ActiveRolloutID string        `json:"active_rollout_id"`
	ThreadID        string        `json:"thread_id"`
	OwnStart        *uint64       `json:"own_start_ordinal,omitempty"`
	Spans           []HistorySpan `json:"spans"`
}

// HistorySpan maps a retained interval to its original physical raw ordinals.
type HistorySpan struct {
	RolloutID    string `json:"rollout_id"`
	ThreadID     string `json:"thread_id"`
	FirstRecord  int    `json:"first_record"`
	EndRecord    int    `json:"end_record"`
	StartOrdinal uint64 `json:"start_ordinal"`
	EndOrdinal   uint64 `json:"end_ordinal_exclusive"`
}

// Validate checks bounded contiguous retained coverage, without trusting ownership flags.
func (h *SourceHistory) Validate(thread string, count int) error {
	if h == nil || h.ThreadID != thread || !historyID.MatchString(thread) || !historyID.MatchString(h.ActiveRolloutID) || len(h.Spans) == 0 || len(h.Spans) > MaxHistorySpans || count < 0 || count > MaxHistoryRecords {
		return errors.New("invalid history identity or bounds")
	}
	next := 0
	var end uint64
	seen := map[string]bool{}
	for i, s := range h.Spans {
		if !historyID.MatchString(s.RolloutID) || !historyID.MatchString(s.ThreadID) || seen[s.RolloutID] || s.FirstRecord != next || s.EndRecord < s.FirstRecord || s.EndRecord > count || s.EndOrdinal < s.StartOrdinal || (i > 0 && s.StartOrdinal != end) {
			return errors.New("invalid history span")
		}
		seen[s.RolloutID] = true
		next = s.EndRecord
		end = s.EndOrdinal
	}
	last := h.Spans[len(h.Spans)-1]
	if next != count || last.RolloutID != h.ActiveRolloutID || last.ThreadID != thread || (h.OwnStart != nil && *h.OwnStart > end) {
		return errors.New("incomplete history coverage")
	}
	return nil
}

// SpanAt returns the bounded physical interval containing a retained record.
func (h *SourceHistory) SpanAt(index int) (HistorySpan, bool) {
	if h != nil {
		for _, s := range h.Spans {
			if index >= s.FirstRecord && index < s.EndRecord {
				return s, true
			}
		}
	}
	return HistorySpan{}, false
}

// OwnRecord excludes other threads and explicitly inherited copied prefixes.
func (b SourceBundle) OwnRecord(index int) bool {
	if b.History == nil {
		return true
	}
	s, ok := b.History.SpanAt(index)
	return ok && s.ThreadID == b.NativeSessionID && index < len(b.Ordinals) && (b.History.OwnStart == nil || b.Ordinals[index] >= *b.History.OwnStart)
}

// ValidateHistory checks schema/manifest/ordinal alignment before any derivation.
func (b SourceBundle) ValidateHistory() error {
	if b.SchemaVersion != HistorySourceSchemaVersion {
		if b.History != nil || len(b.Ordinals) > 0 {
			return errors.New("history requires source schema 3")
		}
		return nil
	}
	if b.Capture.Harness.Name != "codex" || len(b.NativeText) > 0 || len(b.Ordinals) != len(b.NativeRecords) {
		return errors.New("invalid history source shape")
	}
	if err := b.History.Validate(b.NativeSessionID, len(b.NativeRecords)); err != nil {
		return err
	}
	for _, s := range b.History.Spans {
		var prior uint64
		for i := s.FirstRecord; i < s.EndRecord; i++ {
			n := b.Ordinals[i]
			if n < s.StartOrdinal || n >= s.EndOrdinal || (i > s.FirstRecord && n <= prior) {
				return errors.New("invalid retained history ordinal")
			}
			prior = n
		}
	}
	return nil
}

// RevisionReference preserves a meaningful previous native revision in this session.
type RevisionReference struct {
	RevisionID string          `json:"revision_id"`
	CapturedAt time.Time       `json:"captured_at"`
	Source     SourceReference `json:"source"`
	// Optional legacy-compatible provenance is checked against retained bytes.
	SourceSchemaVersion int    `json:"source_schema_version,omitempty"`
	FilterVersion       string `json:"filter_version,omitempty"`
}

// RevisionHistory identifies the active native revision and its preserved alternatives.
type RevisionHistory struct {
	CurrentRevision string              `json:"current_revision"`
	Preserved       []RevisionReference `json:"preserved,omitempty"`
}

// SourceReferences validates and returns active and preserved object pointers.
func (m *Metadata) SourceReferences() ([]SourceReference, error) {
	if err := m.ValidateSourceReference(); err != nil {
		return nil, err
	}
	out := []SourceReference{m.SourceBundle}
	if m.History != nil {
		for _, r := range m.History.Preserved {
			out = append(out, r.Source)
		}
	}
	return out, nil
}

// SourceSetDigest identifies the complete validated active and preserved set.
// Order and capture provenance are included so an active selection change
// cannot reuse a receipt for an older historical view.
func (m *Metadata) SourceSetDigest() (string, error) {
	if _, err := m.SourceReferences(); err != nil {
		return "", err
	}
	data, err := json.Marshal(struct {
		Active     SourceReference  `json:"active"`
		CapturedAt time.Time        `json:"captured_at"`
		History    *RevisionHistory `json:"history,omitempty"`
	}{m.SourceBundle, m.CapturedAt, m.History})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func (m *Metadata) validateRevisionHistory() error {
	if m.SchemaVersion != HistoryMetadataSchemaVersion {
		if m.History != nil {
			return errors.New("history requires metadata schema 2")
		}
		return nil
	}
	h := m.History
	if h == nil || m.Harness.Name != "codex" || !historyID.MatchString(h.CurrentRevision) || len(h.Preserved) > MaxPreservedRevisions {
		return errors.New("invalid revision history")
	}
	seen := map[string]bool{h.CurrentRevision: true}
	keys := map[string]bool{}
	refs := []SourceReference{m.SourceBundle}
	for _, r := range h.Preserved {
		if !historyID.MatchString(r.RevisionID) || seen[r.RevisionID] || r.CapturedAt.IsZero() || (r.SourceSchemaVersion != 0 && r.SourceSchemaVersion != SourceSchemaVersion && r.SourceSchemaVersion != HistorySourceSchemaVersion) {
			return errors.New("invalid preserved revision")
		}
		seen[r.RevisionID] = true
		refs = append(refs, r.Source)
	}
	for _, r := range refs {
		key, err := SourceObjectKey(SourceBundle{SchemaVersion: SourceSchemaVersion, ArchiveSessionID: m.SessionID, NativeSessionID: m.NativeSessionID, ProjectID: m.ProjectID, Capture: SourceCapture{Harness: m.Harness, AdapterName: "codex", CapturedAt: m.CapturedAt}}, r.SHA256)
		if err != nil || r.Key != key || keys[r.Key] || r.CompressedBytes <= 0 {
			return fmt.Errorf("invalid history source reference")
		}
		keys[r.Key] = true
	}
	return nil
}

// MeaningfulCapturedAt returns the latest archive observation in the complete
// retained source set. Derivation and native timestamps never contribute age.
func (m *Metadata) MeaningfulCapturedAt() (time.Time, error) {
	if _, err := m.SourceReferences(); err != nil {
		return time.Time{}, err
	}
	at := m.CapturedAt
	if m.History != nil {
		for _, revision := range m.History.Preserved {
			if revision.CapturedAt.After(at) {
				at = revision.CapturedAt
			}
		}
	}
	return at, nil
}

// ErrHistoryMutationPending protects readable history until revision lifecycle support lands.
var ErrHistoryMutationPending = errors.New("codex history mutation requires revision lifecycle support")

// CheckHistoryMutation is the temporary reader-before-writer publication fence.
func CheckHistoryMutation(b SourceBundle, m Metadata) error {
	if b.History != nil || b.SchemaVersion == HistorySourceSchemaVersion || m.History != nil || m.SchemaVersion == HistoryMetadataSchemaVersion {
		return ErrHistoryMutationPending
	}
	return nil
}
