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
func (Parser) Version() string { return "0.7.0" }

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
	families := newFamilyState(&tools)
	view := archive.NormalizedView{}
	accepted := map[string]retained{}
	started := map[string]int{}
	joined := map[string]bool{}
	messages := map[runIdentity]bool{}
	sourceRecords := map[runIdentity]bool{}
	lastSequence := map[string]int64{}
	materialized := map[string]bool{}
	consumed := map[string]bool{}
	children := map[string]bool{}
	workflowSteps := map[string]workflowChildStep{}
	inbox := inboxOrigins(rows)
	for i, r := range rows {
		if r.Version == 0 {
			continue
		}
		if err := families.observeRow(r); err != nil {
			return view, nil, err
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
			if err := acceptIntent(r, accepted); err != nil {
				return view, nil, err
			}
		case payloadRuntimeSession:
			if str(f["kind"]) != "run" {
				continue
			}
			run := str(f["run_id"])
			switch runtimeEvent(str(f["event_kind"])) {
			case eventStarted:
				if !newConversationRun(f, run, i, started, inbox, joined, materialized) {
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
				if observeAssistantMessage(r, i, ts, joined, lastSequence, sourceRecords, messages, &view) != nil {
					return view, nil, errFormat
				}
			case eventModelCompleted:
				if observeCompletion(r, joined, lastSequence, sourceRecords, &observations) != nil {
					return view, nil, errFormat
				}
			case eventSkillReadObserved, eventChildSessionLinked, eventWorkflowChildLifecycle:
				if observeRunEvidence(r, children, workflowSteps, joined, lastSequence, sourceRecords, &view) != nil {
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

		case payloadRunModelConfigured, payloadRuntimeCommandIntakeReceived, payloadRuntimeCommandIntakeSessionNameReceived, payloadRuntimeCommandIntakeSettled, payloadRuntimeRetainedFact, payloadRuntimeSessionCaptureSelection, payloadRuntimeSessionMetadata, payloadRuntimeSessionPermissionFormatDeclared, payloadRuntimeSessionPermissionProfileCommitted, payloadRuntimeSessionRouteFacts, payloadRuntimeSessionTask, payloadRuntimeSessionStartIntakeV3, payloadSessionEnd, payloadSessionNameChanged, payloadSessionOpenedObserved, payloadSessionResumed, payloadSessionLoginCompleted, payloadSessionStarted:
			// These known variants keep the existing non-special-case behavior.
		}
	}
	if len(accepted) != len(consumed) || len(started) != len(materialized) {
		return view, nil, errFormat
	}
	sort.Slice(view.Turns, func(i, j int) bool { return view.Turns[i].RecordIndex < view.Turns[j].RecordIndex })
	return view, observations.Summary(), nil
}

func observeAssistantMessage(r retained, index int, ts time.Time, joined map[string]bool, last map[string]int64, seen, messages map[runIdentity]bool, view *archive.NormalizedView) error {
	f := r.Fields
	run := str(f["run_id"])
	if observeConversationSource(f, run, joined, last, seen) != nil {
		return errFormat
	}
	key := runIdentity{run, str(f["message_id"])}
	if str(f["message_id"]) == "" || str(f["response_id"]) == "" || messages[key] {
		return errFormat
	}
	messages[key] = true
	view.Turns = append(view.Turns, archive.NormalizedTurn{RecordIndex: index, Role: "assistant", Kind: archive.TurnKindAssistant, Text: str(f["text"]), ID: r.ID, MessageID: str(f["message_id"]), TurnID: run, Timestamp: ts.Format(time.RFC3339Nano)})
	return nil
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

// observeRunEvidence accepts a run's own native reports that create no
// conversation turn: skill reads, child links and workflow child steps.
func observeRunEvidence(r retained, children map[string]bool, workflowSteps map[string]workflowChildStep, joined map[string]bool, last map[string]int64, seen map[runIdentity]bool, view *archive.NormalizedView) error {
	ek := runtimeEvent(str(r.Fields["event_kind"]))
	if ek == eventChildSessionLinked {
		return observeChildLink(r, children, joined, last, seen)
	}
	if ek == eventWorkflowChildLifecycle {
		return observeWorkflowChild(r, children, workflowSteps, joined, last, seen)
	}
	return observeSkillRead(r, joined, last, seen, view)
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

// newConversationRun accepts a run's start at index i. A user turn's run keeps
// its prompt and is joined when its intent materializes. A run an inbox
// delivery started keeps no prompt, needs its earlier origin fact, and is
// joined and accounted for as it starts, with no user turn.
func newConversationRun(f map[string]any, run string, i int, started map[string]int, inbox map[string]int, joined, materialized map[string]bool) bool {
	_, exists := started[run]
	_, prompted := f["prompt"]
	origin, fromInbox := inbox[run]
	if !conversationSourceShape(f) || exists || prompted == fromInbox || fromInbox && (origin < 0 || origin > i) {
		return false
	}
	if fromInbox {
		joined[run], materialized[run] = true, true
	}
	return true
}

// inboxOrigins indexes the runs inbox deliveries started by the position of
// their origin fact; a run with two origins is marked -1 and cannot start.
func inboxOrigins(rows []retained) map[string]int {
	out := map[string]int{}
	for i, r := range rows {
		if r.Type != payloadRuntimeRetainedFact || !inboxOrigin(r) {
			continue
		}
		run := str(r.Fields["run_id"])
		if _, twice := out[run]; twice {
			out[run] = -1
			continue
		}
		out[run] = i
	}
	return out
}

func acceptIntent(r retained, accepted map[string]retained) error {
	id := str(r.Fields["intent_id"])
	if id == "" {
		return errFormat
	}
	if _, exists := accepted[id]; exists {
		return errFormat
	}
	accepted[id] = r
	return nil
}
