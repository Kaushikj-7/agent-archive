package muse

import (
	"slices"
	"strings"
)

// unsupportedMarker classifies an adapter-written omission row: a well-formed
// native record the adapter does not support and leaves out of the archive.
const unsupportedMarker = "unsupported_record_omitted"

// unsupportedRecord is fixed so an omission row never carries any member of
// the record it replaces, its family name included.
var unsupportedRecord = omittedRecord{"event", "durable", "", 0, "", "unsupported_record_v1"}

// authorityFamilies are the supported record families whose records establish
// this session's identity, start, workspace or permissions. One the adapter
// cannot qualify refuses the session instead of being omitted.
var authorityFamilies = []payloadType{
	payloadRuntimeSessionPermissionFormatDeclared,
	payloadRuntimeSessionPermissionProfileCommitted,
	payloadRuntimeSessionMetadata,
	payloadSessionOpenedObserved,
	payloadRuntimeSessionCaptureSelection,
	payloadRuntimeCommandIntakeReceived,
	payloadRuntimeCommandIntakeSessionNameReceived,
	payloadRuntimeCommandIntakeSettled,
	payloadRuntimeSessionStartIntakeV3,
	payloadSessionStarted,
	payloadSessionResumed,
	payloadSessionLoginCompleted,
	payloadSessionEnd,
	payloadSessionNameChanged,
	payloadRuntimeSessionRouteFacts,
	payloadRuntimeUserIntentAccepted,
	payloadRuntimeUserIntentMaterialized,
	payloadRunModelConfigured,
	payloadRuntimeRetainedFact,
	payloadApprovalWaitStarted,
	payloadApprovalWaitTerminal,
}

// authorityWords mark a record family, kind or event as authority-bearing when
// the adapter does not know it: permission and approval, identity of this
// session and its lineage to other sessions, and checksums or proofs. Matching is by whole word of the
// native schema names, never of record content.
var authorityWords = []string{
	"permission", "permissions", "approval", "approvals", "policy", "sandbox", "scope", "grant", "denied",
	"identity", "owner", "subagent", "child", "children", "lineage", "linked", "attested", "attest", "bound", "spawn", "spawned", "parent", "login", "auth", "credential", "credentials",
	"checksum", "digest", "hash", "proof", "fingerprint", "signature", "signed",
}

// omittable reports whether a well-formed record whose payload the adapter
// cannot qualify may be left out of the archive. Records of the authority
// families, the runtime agent tree root, and unknown records whose schema names
// carry an authority word still refuse the whole session.
func omittable(r retained, p map[string]any) bool {
	if r.Position.Line < 2 || slices.Contains(authorityFamilies, r.Type) {
		return false
	}
	kind := str(p["kind"])
	if r.Type == payloadRuntimeSession && (kind == string(roleAgentTreeInitialized) || r.PayloadSchema != 1) {
		return false
	}
	names := []string{string(r.Type), kind, str(object(p["event"])["kind"]), str(object(p["record"])["kind"])}
	for _, name := range names {
		for word := range strings.FieldsFuncSeq(name, func(c rune) bool { return c == '.' || c == '_' || c == '/' || c == '-' }) {
			if slices.Contains(authorityWords, word) {
				return false
			}
		}
	}
	return true
}

// unsupportedRow replaces an omittable record with a content-free omission row
// that keeps only its stream, native ID and sequence, so the session's record
// order and identity checks still cover it.
func unsupportedRow(r retained) logicalRow {
	return logicalRow{omission: &nativeOmission{Version: 2, Kind: nativeOmissionKind, Position: r.Position, Stream: r.Stream, Native: nativePosition{ID: r.ID, Sequence: r.Sequence}, Schema: 1, Marker: unsupportedMarker, Omitted: unsupportedRecord}}
}

// omittedEffects tracks tool effect starts so the records that join them follow
// them: a terminal of an omitted start is omitted, and so is a task's private
// output body when no retained start opened that task.
type omittedEffects struct {
	effects map[effectKey]bool
	started map[taskKey]bool
}

// add records a tool effect start, retained or omitted.
func (o *omittedEffects) add(r retained, p map[string]any, omitted bool) {
	if r.Type != payloadToolBatchStarted {
		return
	}
	if o.effects == nil {
		o.effects, o.started = map[effectKey]bool{}, map[taskKey]bool{}
	}
	run, rec := str(p["run_id"]), object(p["record"])
	if omitted {
		o.effects[effectKey{run, str(rec["effect_id"])}] = true
	} else {
		o.started[taskKey{run, str(rec["task_id"])}] = true
	}
}

// covers reports a record that can only join an omitted or absent start.
func (o *omittedEffects) covers(r retained, p map[string]any) bool {
	run := str(p["run_id"])
	switch {
	case r.Type == payloadToolBatchTerminal:
		return o.effects[effectKey{run, str(object(p["record"])["effect_id"])}]
	case r.Type == payloadRuntimeSession && str(p["kind"]) == string(roleTask) && str(object(p["event"])["kind"]) == "output":
		return !o.started[taskKey{run, str(p["task_id"])}]
	}
	return false
}

// unsupportedTool reports a well-formed tool effect start that joins a
// committed call consistently but for a tool the adapter does not support: not
// a read, with no approval for its task or call. A well-formed effect that
// contradicts its call, its task or an approval still refuses the session.
func (s *nativeBodyJoins) unsupportedTool(r retained, p map[string]any) bool {
	if r.Type != payloadToolBatchStarted || !omittable(r, p) || s.approvedEffect(r, p) {
		return false
	}
	rec := object(p["record"])
	run, task, call, name := str(p["run_id"]), str(rec["task_id"]), str(rec["call_id"]), str(rec["tool_name"])
	ref, exists := s.tools.calls[toolCallIdentity{r.Stream.ID, run, call}]
	return exists && ref.record < int(r.Sequence-1) && !ref.nativeRead && !ref.nativeBroadRead && s.taskIDs[taskKey{run, task}] && name != "" && name != "read_file" && name != "read_skill"
}

// approvedEffect reports a tool effect start whose task or call has an
// approval request. Approval joins are permission authority, so such a start
// is never omitted.
func (s *nativeBodyJoins) approvedEffect(r retained, p map[string]any) bool {
	rec := object(p["record"])
	return r.Type == payloadToolBatchStarted && s.pendingIndex.overlaps(str(p["run_id"]), str(rec["task_id"]), str(rec["call_id"]))
}
