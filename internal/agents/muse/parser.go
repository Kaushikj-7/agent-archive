package muse

import (
	"context"
	"reflect"
	"sort"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// Parser derives qualified own conversation and input/output observations from retained evidence.
// It never opens a native file or infers missing accounting.
type Parser struct{}

// Version identifies the Muse retained analysis policy.
func (Parser) Version() string { return "0.5.0" }

// Parse interprets the complete validated Muse ordinary/omission union.
func (Parser) Parse(ctx context.Context, b archive.SourceBundle) (archive.Analysis, error) {
	rows, e := retainedRows(ctx, b)
	if e != nil {
		return archive.Analysis{}, e
	}
	for i, r := range rows {
		if e = ctx.Err(); e != nil {
			return archive.Analysis{}, e
		}
		rows[i], e = privacyLogical(r, i+1, func(string, int, string) {})
		if e != nil {
			return archive.Analysis{}, e
		}
		// A retained parser has no provenance sink. Require privacy refresh
		// rather than silently changing untrusted retained conversation. Shared
		// rules may report already-redacted text, so compare actual retained bytes.
		if !reflect.DeepEqual(rows[i], r) {
			return archive.Analysis{}, errFormat
		}
	}
	if e = validateLogical(rows); e != nil {
		return archive.Analysis{}, e
	}
	if !validOmissionGaps(rows, b.Capture.Gaps) {
		return archive.Analysis{}, errFormat
	}
	slots := ordinarySlots(rows)
	view, observations, e := conversationObserved(slots)
	if e != nil {
		return archive.Analysis{}, e
	}
	unavailable := archive.Availability{State: archive.AvailabilityUnavailable, Reason: archive.AvailabilityReasonNotRecorded}
	toolCalls := unavailable
	if toolCountAvailable(slots, view) {
		toolCalls = archive.Availability{State: archive.AvailabilityAvailable}
	}
	a := archive.Analysis{View: view, Facts: archive.NativeFacts{NativeStartedAt: time.UnixMicro(slots[0].Timestamp).UTC(), EarliestRecordAt: time.UnixMicro(slots[0].Timestamp).UTC(), TokenScopeUnknown: false}, Observability: archive.Observability{StructuredCounts: archive.Availability{State: archive.AvailabilityAvailable}, ToolCalls: toolCalls, FilesTouched: unavailable, GitActivity: unavailable, Compactions: unavailable, ToolErrors: unavailable, ToolResults: unavailable}}
	for _, r := range slots {
		if r.Type == payloadRuntimeSessionMetadata && a.Facts.WorkspaceRoot == "" {
			a.Facts.WorkspaceRoot = str(r.Fields["workspace_root"])
		}
		if r.Type == payloadSessionNameChanged {
			a.Facts.Name = archive.CollapseSessionTitle(str(r.Fields["new_name"]))
		}
	}
	if observations != nil && !observationGapsPresent(b.Capture.Gaps) {
		return archive.Analysis{}, errFormat
	}
	a.Facts.TokenScopeUnknown = observations == nil
	a.TokenObservations = observations
	a.Facts.IdentityConflict = b.NativeSessionID != "" && b.NativeSessionID != slots[0].Stream.ID
	return a, nil
}

func conversation(rows []retained) (archive.NormalizedView, error) {
	view, _, err := conversationObserved(rows)
	return view, err
}

func conversationObserved(rows []retained) (archive.NormalizedView, *archive.TokenObservationSummary, error) {
	var observations archive.ObservationAccumulator
	var tools toolObservations
	view := archive.NormalizedView{}
	accepted := map[string]retained{}
	started := map[string]int{}
	joined := map[string]bool{}
	messages := map[runIdentity]bool{}
	sourceRecords := map[runIdentity]bool{}
	lastSequence := map[string]int64{}
	materialized := map[string]bool{}
	consumed := map[string]bool{}
	for i, r := range rows {
		if r.Version == 0 {
			continue
		}
		ts := time.UnixMicro(r.Timestamp).UTC()
		if ts.After(view.LatestRecordAt) {
			view.LatestRecordAt = ts
		}
		f := r.Fields
		switch r.Type {
		case payloadApprovalWaitStarted, payloadApprovalWaitTerminal, payloadToolBatchStarted, payloadToolBatchTerminal:
			// Omitted private effects do not create conversation facts.
		case payloadRuntimeUserIntentAccepted:
			id := str(f["intent_id"])
			if id == "" {
				return view, nil, errFormat
			}
			if _, ok := accepted[id]; ok {
				return view, nil, errFormat
			}
			accepted[id] = r
		case payloadRuntimeSession:
			if str(f["kind"]) != "run" {
				continue
			}
			run := str(f["run_id"])
			switch runtimeEvent(str(f["event_kind"])) {
			case eventStarted:
				if !newConversationRun(f, run, started) {
					return view, nil, errFormat
				}
				started[run] = i
				sourceRecords[runIdentity{run, str(f["source_run_record_id"])}] = true
				lastSequence[run] = num(f["source_run_record_sequence"])
			case eventReasoningCommitted:
				if observeConversationSource(f, run, joined, lastSequence, sourceRecords) != nil {
					return view, nil, errFormat
				}
			case eventAssistantMessageCommitted:
				if observeConversationSource(f, run, joined, lastSequence, sourceRecords) != nil {
					return view, nil, errFormat
				}
				key := runIdentity{run, str(f["message_id"])}
				if str(f["message_id"]) == "" || str(f["response_id"]) == "" || messages[key] {
					return view, nil, errFormat
				}
				messages[key] = true
				view.Turns = append(view.Turns, archive.NormalizedTurn{RecordIndex: i, Role: "assistant", Kind: archive.TurnKindAssistant, Text: str(f["text"]), ID: r.ID, MessageID: str(f["message_id"]), TurnID: run, Timestamp: ts.Format(time.RFC3339Nano)})
			case eventModelCompleted:
				if observeCompletion(r, joined, lastSequence, sourceRecords, &observations) != nil {
					return view, nil, errFormat
				}
			case eventAssistantToolCallsCommitted, eventToolResultBatchCommitted:
				if observeCommittedTools(r, i, joined, lastSequence, sourceRecords, &tools, &view) != nil {
					return view, nil, errFormat
				}
			case eventAccepted, eventCancelled, eventCompleted, eventContextBlockDiagnostic, eventContextBlockUpdated, eventFailed, eventGoalUsageAttribution, eventHookRunStarted, eventHookRunTerminal, eventModelInputTraceRecorded, eventModelRequestConfigured, eventModelResponseCreated, eventProposed, eventProviderRequestOptionsConfigured, eventResourceUsageSampled, eventScheduled, eventSideEffectIntent, eventStatus, eventTaskStreamLinked, eventTerminal:
				// Known optional facts do not create visible conversation turns.

			}
		case payloadRuntimeUserIntentMaterialized:
			turn, run, e := materializedTurn(r, rows, accepted, started, materialized, consumed)
			if e != nil {
				return view, nil, e
			}
			if turn != nil {
				joined[run] = true
				view.Turns = append(view.Turns, *turn)
			}

		case payloadRunModelConfigured, payloadRuntimeCommandIntakeReceived, payloadRuntimeCommandIntakeSessionNameReceived, payloadRuntimeCommandIntakeSettled, payloadRuntimeRetainedFact, payloadRuntimeSessionCaptureSelection, payloadRuntimeSessionMetadata, payloadRuntimeSessionPermissionFormatDeclared, payloadRuntimeSessionPermissionProfileCommitted, payloadRuntimeSessionRouteFacts, payloadRuntimeSessionTask, payloadRuntimeSessionStartIntakeV3, payloadSessionEnd, payloadSessionNameChanged, payloadSessionOpenedObserved, payloadSessionResumed, payloadSessionStarted:
			// These known variants keep the existing non-special-case behavior.
		}
	}
	if len(accepted) != len(consumed) || len(started) != len(materialized) {
		return view, nil, errFormat
	}
	sort.Slice(view.Turns, func(i, j int) bool { return view.Turns[i].RecordIndex < view.Turns[j].RecordIndex })
	return view, observations.Summary(), nil
}

func materializedTurn(r retained, rows []retained, accepted map[string]retained, started map[string]int, materialized, consumed map[string]bool) (*archive.NormalizedTurn, string, error) {
	f := r.Fields
	intent := str(f["intent_id"])
	a, ok := accepted[intent]
	if !ok || a.ID != str(f["envelope_record_id"]) || a.Sequence != num(f["envelope_sequence"]) {
		return nil, "", errFormat
	}
	run := str(f["run_id"])
	idx, ok := started[run]
	if !ok || materialized[run] || consumed[intent] {
		return nil, "", errFormat
	}
	materialized[run] = true
	consumed[intent] = true
	s := rows[idx]
	if s.ID != str(f["run_started_session_record_id"]) || str(s.Fields["source_run_record_id"]) != str(f["run_started_source_record_id"]) || a.Sequence >= s.Sequence {
		return nil, "", errFormat
	}
	// The observed run-origin fact is empty. Only typed main/chat acceptance and
	// the materialization graph qualify an own top-level visible prompt.
	if str(a.Fields["surface"]) != "main" || str(a.Fields["semantic_kind"]) != "chat" {
		return nil, run, errFormat
	}
	turn := archive.NormalizedTurn{RecordIndex: idx, Role: "user", Kind: archive.TurnKindHumanPrompt, Text: str(s.Fields["prompt"]), ID: s.ID, TurnID: run, Timestamp: time.UnixMicro(s.Timestamp).UTC().Format(time.RFC3339Nano)}
	return &turn, run, nil
}

// runIdentity keeps opaque identifiers in separate typed fields without delimiter normalization.
type runIdentity struct {
	run    string
	record string
}

func observeConversationSource(f map[string]any, run string, joined map[string]bool, last map[string]int64, seen map[runIdentity]bool) error {
	key := runIdentity{run, str(f["source_run_record_id"])}
	seq := num(f["source_run_record_sequence"])
	if !conversationSourceShape(f) || !joined[run] || seq <= last[run] || seen[key] {
		return errFormat
	}
	seen[key] = true
	last[run] = seq
	return nil
}

func conversationSourceShape(f map[string]any) bool {
	return identifier(f["run_id"]) && identifier(f["source_run_record_id"]) && integer(f["source_run_record_sequence"], 1)
}

func observeCompletion(r retained, joined map[string]bool, last map[string]int64, seen map[runIdentity]bool, observations *archive.ObservationAccumulator) error {
	f := r.Fields
	if !completionEnriched(f) {
		return nil
	}
	if !completionFields(r) || observeConversationSource(f, str(f["run_id"]), joined, last, seen) != nil {
		return errFormat
	}
	return observations.Add(str(f["model"]), num(f["input_tokens"]), num(f["output_tokens"]))
}

func newConversationRun(f map[string]any, run string, started map[string]int) bool {
	_, exists := started[run]
	return conversationSourceShape(f) && !exists
}
