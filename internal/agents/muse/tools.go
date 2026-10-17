package muse

import "github.com/wangjohn/agent-archive/internal/archive"

// opaqueToolID is a closed, bounded identity domain, separate from native UUIDs.
func opaqueToolID(v any) bool {
	s, ok := v.(string)
	if !ok || len(s) < 1 || len(s) > 128 {
		return false
	}
	for i, c := range []byte(s) {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || i > 0 && (c == '_' || c == '-') {
			continue
		}
		return false
	}
	return true
}

func toolEvent(f map[string]any) bool {
	return str(f["kind"]) == "run" && (str(f["event_kind"]) == string(eventAssistantToolCallsCommitted) || str(f["event_kind"]) == string(eventToolResultBatchCommitted))
}

func enrichedTools(f map[string]any) bool { return toolEvent(f) && len(f) != 2 }

func toolFields(r retained) bool {
	f := r.Fields
	if r.Causation != nil || !conversationSourceShape(f) {
		return false
	}
	if str(f["event_kind"]) == string(eventAssistantToolCallsCommitted) {
		if !keys(f, "kind", "event_kind", "run_id", "source_run_record_id", "source_run_record_sequence", "message_id", "tool_calls") || !opaqueToolID(f["message_id"]) {
			return false
		}
		items, ok := f["tool_calls"].([]any)
		seen := map[string]bool{}
		if !ok || len(items) == 0 {
			return false
		}
		for _, value := range items {
			item := object(value)
			id := str(item["call_id"])
			if !keys(item, "call_id") || !opaqueToolID(item["call_id"]) || seen[id] {
				return false
			}
			seen[id] = true
		}
		return true
	}
	if !keys(f, "kind", "event_kind", "run_id", "source_run_record_id", "source_run_record_sequence", "batch_id", "results") || !opaqueToolID(f["batch_id"]) {
		return false
	}
	items, ok := f["results"].([]any)
	seen := map[int64]bool{}
	if !ok || len(items) == 0 {
		return false
	}
	for _, value := range items {
		item := object(value)
		index := num(item["tool_call_index"])
		if !keys(item, "tool_call_index", "tool_call_id") || !integer(item["tool_call_index"], 0) || !opaqueToolID(item["tool_call_id"]) || seen[index] {
			return false
		}
		seen[index] = true
	}
	return true
}

func projectTools(r retained, p, event map[string]any) (map[string]any, error) {
	if r.Causation != nil || !keys(p, "kind", "run_id", "source_run_record_id", "source_run_record_sequence", "event") {
		return nil, errFormat
	}
	f := map[string]any{"kind": "run", "event_kind": event["kind"]}
	copyFields(f, p, "run_id", "source_run_record_id", "source_run_record_sequence")
	call := str(event["kind"]) == string(eventAssistantToolCallsCommitted)
	array, batch := "results", "batch_id"
	if call {
		array, batch = "tool_calls", "message_id"
		if !keys(event, "kind", "message_id", "response_id", "tool_calls") || !boundedToolString(event["response_id"]) {
			return nil, errFormat
		}
	} else if !keys(event, "kind", "batch_id", "results") {
		return nil, errFormat
	}
	items, ok := event[array].([]any)
	if !ok || len(items) == 0 {
		return nil, errFormat
	}
	out := make([]any, 0, len(items))
	ids := map[string]bool{}
	for _, value := range items {
		item := object(value)
		if call {
			if !keys(item, "id", "call_id", "name", "args") || !boundedToolString(item["id"]) || !boundedToolString(item["name"]) || !boundedToolString(item["args"]) || ids[str(item["id"])] {
				return nil, errFormat
			}
			ids[str(item["id"])] = true
			out = append(out, map[string]any{"call_id": item["call_id"]})
		} else {
			if !keys(item, "tool_call_index", "tool_call_id", "text") || !boundedToolString(item["text"]) {
				return nil, errFormat
			}
			out = append(out, map[string]any{"tool_call_index": item["tool_call_index"], "tool_call_id": item["tool_call_id"]})
		}
	}
	f[batch], f[array] = event[batch], out
	r.Fields = f
	if !toolFields(r) {
		return nil, errFormat
	}
	return f, nil
}

func boundedToolString(v any) bool { s, ok := v.(string); return ok && len(s) <= maxString }

// toolBatchIdentity keeps owner, materialized run and message in distinct domains.
type toolBatchIdentity struct {
	session string
	run     string
	message string
}

type toolCallIdentity struct {
	session string
	run     string
	call    string
}

type toolBatch struct {
	calls    []int
	resulted bool
}

type toolObservations struct {
	batches map[toolBatchIdentity]*toolBatch
	calls   map[toolCallIdentity]toolCallRef
}

// toolCallRef contains no native payload, arbitrary name, or argument data.
type toolCallRef struct {
	record     int
	normalized int
	nativeRead bool
}

func (t *toolObservations) registerCalls(r retained, index int, view *archive.NormalizedView, native map[string]any) error {
	if t.batches == nil {
		t.batches = map[toolBatchIdentity]*toolBatch{}
		t.calls = map[toolCallIdentity]toolCallRef{}
	}
	f := r.Fields
	key := toolBatchIdentity{r.Stream.ID, str(f["run_id"]), str(f["message_id"])}
	if t.batches[key] != nil {
		return errFormat
	}
	items := f["tool_calls"].([]any)
	var private []any
	if native != nil {
		private, _ = object(native["event"])["tool_calls"].([]any)
		if len(private) != len(items) {
			return errFormat
		}
	}
	batch := &toolBatch{}
	for i, value := range items {
		id := str(object(value)["call_id"])
		call := toolCallIdentity{key.session, key.run, id}
		if _, present := t.calls[call]; present {
			return errFormat
		}
		if native != nil {
			item := object(private[i])
			if item["call_id"] != id {
				return errFormat
			}
		}
		normalized := -1
		if view != nil {
			normalized = len(view.ToolCalls)
			// Private names/actions never enter narrative listing.
			view.ToolCalls = append(view.ToolCalls, archive.NormalizedToolCall{RecordIndex: index, CallID: id})
		}
		ref := toolCallRef{record: index, normalized: normalized, nativeRead: native != nil && str(object(private[i])["name"]) == "read_file"}
		t.calls[call] = ref
		batch.calls = append(batch.calls, ref.normalized)
	}
	t.batches[key] = batch
	return nil
}

func (t *toolObservations) observe(r retained, index int, view *archive.NormalizedView) error {
	f := r.Fields
	if str(f["event_kind"]) == string(eventAssistantToolCallsCommitted) {
		return t.registerCalls(r, index, view, nil)
	}
	key := toolBatchIdentity{r.Stream.ID, str(f["run_id"]), str(f["batch_id"])}
	batch := t.batches[key]
	if batch == nil || batch.resulted {
		return errFormat
	}
	batch.resulted = true
	for _, value := range f["results"].([]any) {
		item := object(value)
		n := num(item["tool_call_index"])
		if n >= int64(len(batch.calls)) {
			return errFormat
		}
		call := &view.ToolCalls[batch.calls[n]]
		if call.CallID != str(item["tool_call_id"]) || call.ResultRecordIndex != nil {
			return errFormat
		}
		call.ResultRecordIndex = new(index)
	}
	return nil
}

func toolCountAvailable(rows []retained, view archive.NormalizedView) bool {
	if len(view.ToolCalls) == 0 {
		return false
	}
	for _, r := range rows {
		if r.Type == payloadRuntimeSession && toolEvent(r.Fields) && !enrichedTools(r.Fields) {
			return false
		}
	}
	return true
}

func observeCommittedTools(r retained, index int, joined map[string]bool, last map[string]int64, seen map[runIdentity]bool, tools *toolObservations, view *archive.NormalizedView) error {
	if !enrichedTools(r.Fields) {
		return nil
	}
	if !toolFields(r) || observeConversationSource(r.Fields, str(r.Fields["run_id"]), joined, last, seen) != nil {
		return errFormat
	}
	return tools.observe(r, index, view)
}
