package agentapi

import "github.com/wangjohn/agent-archive/internal/archive"

// OwnedEvidenceCovered preserves the native meaningful-record and owned ordinal
// comparison used by retained reconciliation and privacy transformation.
func OwnedEvidenceCovered(adapter TranscriptFilter, previous, candidate archive.SourceBundle) bool {
	if previous.Capture.FilterVersion != candidate.Capture.FilterVersion || previous.Capture.AdapterVersion != candidate.Capture.AdapterVersion || previous.Capture.SourceFormat != candidate.Capture.SourceFormat {
		return false
	}
	evidence, ok := adapter.(RevisionEvidence)
	if previous.History == nil && candidate.History == nil && !ok {
		// Ordinary adapters compare their complete retained records and text.
		// Owned ordinals are required only for graph-history reconciliation.
		return previous.NativeSessionID == candidate.NativeSessionID && adapter.EvidenceExtends(previous, candidate)
	}
	if !ok {
		return false
	}
	if previous.History == nil || candidate.History == nil {
		if retainedRevisionID(previous) != retainedRevisionID(candidate) {
			return false
		}
		return adapter.EvidenceExtends(OwnedRevisionProjection(evidence, previous), OwnedRevisionProjection(evidence, candidate))
	}
	j := 0
	for i := range previous.NativeRecords {
		if !previous.OwnRecord(i) || !evidence.MeaningfulRevisionRecord(previous, i) {
			continue
		}
		ordinal := retainedRevisionOrdinal(previous, i)
		for j < len(candidate.NativeRecords) && (retainedRevisionOrdinal(candidate, j) < ordinal || !candidate.OwnRecord(j) || !evidence.MeaningfulRevisionRecord(candidate, j)) {
			j++
		}
		if j == len(candidate.NativeRecords) || retainedRevisionOrdinal(candidate, j) != ordinal {
			return false
		}
		left, right := previous, candidate
		left.History, right.History = nil, nil
		left.Ordinals, right.Ordinals = nil, nil
		left.NativeRecords, right.NativeRecords = previous.NativeRecords[i:i+1], candidate.NativeRecords[j:j+1]
		left.NativeText, right.NativeText = nil, nil
		if !adapter.EvidenceExtends(left, right) {
			return false
		}
	}
	return true
}

func retainedRevisionID(b archive.SourceBundle) string {
	if b.History != nil {
		return b.History.ActiveRolloutID
	}
	return b.NativeSessionID
}

func retainedRevisionOrdinal(b archive.SourceBundle, i int) uint64 {
	if b.History != nil {
		return b.Ordinals[i]
	}
	// Both callers supply a slice index. Refuse negative input explicitly so
	// conversion cannot wrap; every nonnegative int fits in uint64.
	if i < 0 {
		return 0
	}
	return uint64(i)
}

// OwnedRevisionProjection selects meaningful owned records for native continuity comparison.
func OwnedRevisionProjection(evidence RevisionEvidence, bundle archive.SourceBundle) archive.SourceBundle {
	records := make([]map[string]any, 0, len(bundle.NativeRecords))
	for i, record := range bundle.NativeRecords {
		if bundle.OwnRecord(i) && evidence.MeaningfulRevisionRecord(bundle, i) {
			records = append(records, record)
		}
	}
	bundle.NativeRecords, bundle.History, bundle.Ordinals = records, nil, nil
	bundle.NativeText = nil
	return bundle
}
