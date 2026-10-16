package muse

import (
	"context"
	"reflect"
	"sort"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// Parser derives only qualified own visible conversation from retained evidence.
// It never opens a native file or infers missing accounting.
type Parser struct{}

// Version identifies the Muse retained analysis policy.
func (Parser) Version() string { return "0.2.0" }

// Parse interprets the complete validated muse-jsonl v1 evidence.
func (Parser) Parse(ctx context.Context, b archive.SourceBundle) (archive.Analysis, error) {
	rows, e := retainedRows(ctx, b)
	if e != nil {
		return archive.Analysis{}, e
	}
	for i, r := range rows {
		if e = ctx.Err(); e != nil {
			return archive.Analysis{}, e
		}
		rows[i], e = privacyWithGaps(r, i+1, func(string, int, string) {})
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
	if e = validateRootRows(rows); e != nil {
		return archive.Analysis{}, e
	}
	view, e := conversation(rows)
	if e != nil {
		return archive.Analysis{}, e
	}
	unavailable := archive.Availability{State: archive.AvailabilityUnavailable, Reason: archive.AvailabilityReasonNotRecorded}
	a := archive.Analysis{View: view, Facts: archive.NativeFacts{NativeStartedAt: time.UnixMicro(rows[0].Timestamp).UTC(), EarliestRecordAt: time.UnixMicro(rows[0].Timestamp).UTC(), TokenScopeUnknown: true}, Observability: archive.Observability{StructuredCounts: archive.Availability{State: archive.AvailabilityAvailable}, ToolCalls: unavailable, FilesTouched: unavailable, GitActivity: unavailable, Compactions: unavailable, ToolErrors: unavailable, ToolResults: unavailable}}
	for _, r := range rows {
		if r.Type == payloadRuntimeSessionMetadata && a.Facts.WorkspaceRoot == "" {
			a.Facts.WorkspaceRoot = str(r.Fields["workspace_root"])
		}
		if r.Type == payloadSessionNameChanged {
			a.Facts.Name = archive.CollapseSessionTitle(str(r.Fields["new_name"]))
		}
	}
	a.Facts.IdentityConflict = b.NativeSessionID != "" && b.NativeSessionID != rows[0].Stream.ID
	return a, nil
}

func conversation(rows []retained) (archive.NormalizedView, error) {
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
		ts := time.UnixMicro(r.Timestamp).UTC()
		if ts.After(view.LatestRecordAt) {
			view.LatestRecordAt = ts
		}
		f := r.Fields
		switch r.Type {
		case payloadRuntimeUserIntentAccepted:
			id := str(f["intent_id"])
			if id == "" {
				return view, errFormat
			}
			if _, ok := accepted[id]; ok {
				return view, errFormat
			}
			accepted[id] = r
		case payloadRuntimeSession:
			if str(f["kind"]) != "run" {
				continue
			}
			run := str(f["run_id"])
			switch runtimeEvent(str(f["event_kind"])) {
			case eventStarted:
				if run == "" || str(f["source_run_record_id"]) == "" {
					return view, errFormat
				}
				if _, ok := started[run]; ok {
					return view, errFormat
				}
				started[run] = i
				sourceRecords[runIdentity{run, str(f["source_run_record_id"])}] = true
				lastSequence[run] = num(f["source_run_record_sequence"])
			case eventAssistantMessageCommitted:
				_, ok := started[run]
				if !ok || !joined[run] {
					return view, errFormat
				}
				if num(f["source_run_record_sequence"]) <= lastSequence[run] || sourceRecords[runIdentity{run, str(f["source_run_record_id"])}] {
					return view, errFormat
				}
				key := runIdentity{run, str(f["message_id"])}
				if str(f["message_id"]) == "" || str(f["response_id"]) == "" || messages[key] {
					return view, errFormat
				}
				messages[key] = true
				sourceRecords[runIdentity{run, str(f["source_run_record_id"])}] = true
				lastSequence[run] = num(f["source_run_record_sequence"])
				view.Turns = append(view.Turns, archive.NormalizedTurn{RecordIndex: i, Role: "assistant", Kind: archive.TurnKindAssistant, Text: str(f["text"]), ID: r.ID, MessageID: str(f["message_id"]), TurnID: run, Timestamp: ts.Format(time.RFC3339Nano)})
			case eventAccepted, eventAssistantToolCallsCommitted, eventCancelled, eventCompleted, eventContextBlockDiagnostic, eventContextBlockUpdated, eventFailed, eventGoalUsageAttribution, eventHookRunStarted, eventHookRunTerminal, eventModelCompleted, eventModelInputTraceRecorded, eventModelRequestConfigured, eventModelResponseCreated, eventProposed, eventProviderRequestOptionsConfigured, eventResourceUsageSampled, eventScheduled, eventSideEffectIntent, eventStatus, eventTaskStreamLinked, eventTerminal, eventToolResultBatchCommitted:
				// Known optional facts do not create visible conversation turns.

			}
		case payloadRuntimeUserIntentMaterialized:
			turn, run, e := materializedTurn(r, rows, accepted, started, materialized, consumed)
			if e != nil {
				return view, e
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
		return view, errFormat
	}
	sort.Slice(view.Turns, func(i, j int) bool { return view.Turns[i].RecordIndex < view.Turns[j].RecordIndex })
	return view, nil
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
