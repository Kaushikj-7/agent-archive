package archive

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// MaxPreservedRevisions bounds selected revision references independently of graph spans.
const MaxPreservedRevisions = 64

// PublicationIdentity validates the complete reference set and hashes canonical
// metadata plus private destination, admission, policy and mutation context.
// Preserved-reference order does not affect identity; every other metadata field does.
func PublicationIdentity(raw []byte, destination, admission, policy, purpose string) (string, []SourceReference, error) {
	var m Metadata
	if err := json.Unmarshal(raw, &m); err != nil {
		return "", nil, err
	}
	refs, err := m.SourceReferences()
	if err != nil {
		return "", nil, err
	}
	if strings.TrimSpace(m.NativeSessionID) == "" || strings.TrimSpace(m.ProjectID) == "" || strings.TrimSpace(m.MachineID) == "" || m.StartedAt.IsZero() || m.CapturedAt.IsZero() || m.MetadataDerivedAt.IsZero() {
		return "", nil, errors.New("publication metadata identity is incomplete")
	}
	if _, err := MetadataObjectKey(m.Harness.Name, m.SessionID); err != nil {
		return "", nil, err
	}
	prefix := "sessions/" + m.Harness.Name + "/" + m.SessionID + "/source."
	seen := map[string]bool{}
	for _, r := range refs {
		if !isLowerHexSHA256(r.SHA256) || r.CompressedBytes <= 0 || r.Key != prefix+r.SHA256+".jsonl.gz" || seen[r.Key] {
			return "", nil, errors.New("publication source reference is invalid or duplicated")
		}
		seen[r.Key] = true
	}
	if m.History != nil {
		slices.SortFunc(m.History.Preserved, func(a, b RevisionReference) int { return strings.Compare(a.RevisionID, b.RevisionID) })
	}
	canonical, err := json.Marshal(struct {
		Metadata    Metadata `json:"metadata"`
		Destination string   `json:"destination"`
		Admission   string   `json:"admission"`
		Policy      string   `json:"policy"`
		Purpose     string   `json:"purpose"`
	}{Metadata: m, Destination: destination, Admission: admission, Policy: policy, Purpose: purpose})
	if err != nil {
		return "", nil, err
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), refs, nil
}

// ValidateRevisionTransition refuses eviction and append-snapshot preservation.
// Same-revision replacement needs retained filtered prefix and ordinal consistency.
// These checks are necessary, not provider proof of raw prefix continuity.
// Privacy replacement is deliberately outside this selection-only contract.
func ValidateRevisionTransition(prior, next Metadata, previous, candidate SourceBundle) error {
	if prior.History == nil && next.History == nil {
		return nil
	}
	if prior.History == nil || next.History == nil {
		return errors.New("revision transition requires complete prior selection")
	}
	if prior.SessionID != next.SessionID || prior.NativeSessionID != next.NativeSessionID || prior.ProjectID != next.ProjectID || prior.Harness != next.Harness {
		return errors.New("revision transition changes ownership")
	}
	retained := map[string]RevisionReference{}
	for _, r := range next.History.Preserved {
		retained[r.RevisionID] = r
	}
	for _, r := range prior.History.Preserved {
		if r.RevisionID == next.History.CurrentRevision {
			if r.Source != next.SourceBundle || !r.CapturedAt.Equal(next.CapturedAt) {
				return errors.New("promoted revision must retain its exact source and capture time")
			}
			continue
		}
		if got, ok := retained[r.RevisionID]; !ok || got.Source != r.Source || !got.CapturedAt.Equal(r.CapturedAt) {
			return errors.New("revision transition cannot discard preserved evidence")
		}
	}
	if prior.History.CurrentRevision != next.History.CurrentRevision {
		r, ok := retained[prior.History.CurrentRevision]
		if !ok || r.Source != prior.SourceBundle || !r.CapturedAt.Equal(prior.CapturedAt) {
			return errors.New("revision switch must preserve the exact previous selection; at 64 preserved revisions stop and retain the committed archive")
		}
		return nil
	}
	if len(next.History.Preserved) != len(prior.History.Preserved) {
		return errors.New("same revision cannot preserve append snapshots")
	}
	if prior.SourceBundle == next.SourceBundle {
		return nil
	}
	if previous.ArchiveSessionID != prior.SessionID || previous.NativeSessionID != prior.NativeSessionID || previous.ProjectID != prior.ProjectID || candidate.ArchiveSessionID != next.SessionID || candidate.NativeSessionID != next.NativeSessionID || candidate.ProjectID != next.ProjectID {
		return errors.New("continuity bundles differ from selected ownership")
	}
	return retainedRevisionContinues(previous, candidate)
}

func retainedRevisionContinues(previous, candidate SourceBundle) error {
	if previous.History == nil || candidate.History == nil || previous.ValidateHistory() != nil || candidate.ValidateHistory() != nil || previous.History.ActiveRolloutID != candidate.History.ActiveRolloutID || previous.History.ThreadID != candidate.History.ThreadID || (previous.History.OwnStart == nil) != (candidate.History.OwnStart == nil) || (previous.History.OwnStart != nil && *previous.History.OwnStart != *candidate.History.OwnStart) || previous.Capture.SourceFormat != candidate.Capture.SourceFormat || previous.Capture.FilterVersion != candidate.Capture.FilterVersion || previous.Capture.AdapterVersion != candidate.Capture.AdapterVersion || len(previous.NativeRecords) > len(candidate.NativeRecords) {
		return errors.New("same revision update requires retained continuity evidence")
	}
	return retainedRevisionContentContinues(previous, candidate)
}

func retainedRevisionContentContinues(previous, candidate SourceBundle) error {
	if len(previous.History.Spans) != len(candidate.History.Spans) {
		return errors.New("same revision span selection changed")
	}
	for i, span := range previous.History.Spans {
		nextSpan := candidate.History.Spans[i]
		if span.RolloutID != nextSpan.RolloutID || span.ThreadID != nextSpan.ThreadID || span.FirstRecord != nextSpan.FirstRecord || span.StartOrdinal != nextSpan.StartOrdinal || span.EndRecord > nextSpan.EndRecord || span.EndOrdinal > nextSpan.EndOrdinal {
			return errors.New("same revision span identity changed")
		}
	}
	for i, record := range previous.NativeRecords {
		a, err := json.Marshal(record)
		if err != nil {
			return err
		}
		b, err := json.Marshal(candidate.NativeRecords[i])
		if err != nil || string(a) != string(b) || previous.Ordinals[i] != candidate.Ordinals[i] {
			return fmt.Errorf("same revision update changes retained record %d", i)
		}
	}
	return nil
}
