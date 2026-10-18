package muse

import "github.com/wangjohn/agent-archive/internal/archive"

// The native digest hashes original file bytes. Validate its exact observed
// grammar, then erase it: shared skill snapshots hash privacy-filtered bytes.
// Retaining the original digest could expose a guess at a redacted credential.
func projectSkillRead(r retained, p, event map[string]any) (map[string]any, error) {
	if r.Causation != nil || !keys(p, "kind", "run_id", "source_run_record_id", "source_run_record_sequence", "event") || !keys(event, "kind", "skill_id", "evidence_kind", "evidence_hash", "observed_at_sequence") || !digest(event["evidence_hash"]) {
		return nil, errFormat
	}
	f := map[string]any{"kind": p["kind"], "event_kind": event["kind"]}
	copyFields(f, p, "run_id", "source_run_record_id", "source_run_record_sequence")
	copyFields(f, event, "skill_id", "evidence_kind", "observed_at_sequence")
	r.Fields = f
	if !skillReadFields(r) {
		return nil, errFormat
	}
	return f, nil
}

const skillContentEvidence = "marker_free_skill_content_v1"

var skillReadFieldNames = []string{"kind", "event_kind", "run_id", "source_run_record_id", "source_run_record_sequence", "skill_id", "evidence_kind", "observed_at_sequence"}

func skillReadNames(f map[string]any) []string {
	if _, present := f["sha256"]; present {
		return append(append([]string(nil), skillReadFieldNames...), "sha256", "hash_evidence")
	}
	return skillReadFieldNames
}

func skillReadFields(r retained) bool {
	f := r.Fields
	shape := keys(f, skillReadNames(f)...)
	if _, present := f["sha256"]; present {
		shape = shape && str(f["hash_evidence"]) == skillContentEvidence && digest("sha256:"+str(f["sha256"]))
	}
	return r.PayloadSchema == 1 && r.Causation == nil && shape && str(f["kind"]) == "run" && str(f["event_kind"]) == string(eventSkillReadObserved) && conversationSourceShape(f) && skillPortableID(str(f["skill_id"])) && str(f["evidence_kind"]) == "read_skill_tool" && integer(f["observed_at_sequence"], 1) && num(f["observed_at_sequence"]) == num(f["source_run_record_sequence"])
}

func observeSkillRead(r retained, joined map[string]bool, last map[string]int64, seen map[runIdentity]bool, view *archive.NormalizedView) error {
	if !skillReadFields(r) || observeConversationSource(r.Fields, str(r.Fields["run_id"]), joined, last, seen) != nil {
		return errFormat
	}
	// The explicit native harness read report grants this positive fact.
	// Only the separately finalized safe-content producer variant grants a hash.
	view.NativeSkillUses = append(view.NativeSkillUses, archive.SkillUse{Name: str(r.Fields["skill_id"]), SHA256: str(r.Fields["sha256"]), Evidence: archive.SkillUseEvidenceNativeInvocation})
	return nil
}
