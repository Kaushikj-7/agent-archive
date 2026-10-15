package muse

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
)

// Filter validates native authority before projecting any safe retained output.
type Filter struct{}

// Name is the canonical native integration identity.
func (Filter) Name() string { return "muse" }

// Version identifies the current privacy projection.
func (Filter) Version() string { return "0.19.0" }

// LeasedFilterFor refuses decorators which may change filtering semantics.
func (Filter) LeasedFilterFor(a archive.Adapter) bool { _, ok := a.(Filter); return ok }

// Filter applies the bounded native decoder and shared privacy policy.
func (f Filter) Filter(ctx context.Context, in agentapi.NativeInput, c agentapi.FilterContext) (archive.FilteredTranscript, error) {
	out, release, e := f.FilterLeased(ctx, in, c, c.Limits.ReadBudget)
	release()
	return out, e
}

// FilterLeased transfers retained memory ownership to the caller.
func (Filter) FilterLeased(ctx context.Context, in agentapi.NativeInput, c agentapi.FilterContext, budget *agentapi.NativeReadBudget) (out archive.FilteredTranscript, release func(), err error) {
	reservation := budget.Reservation()
	held := int64(0)
	release = func() { reservation.Release(held); held = 0 }
	defer func() {
		if err != nil {
			release()
		}
	}()
	if in.File == nil || in.Records != nil {
		return out, release, errFormat
	}
	rawLimit := c.Limits.RawBytes
	if rawLimit <= 0 {
		rawLimit = archive.MaxRawTranscriptBytes
	}
	recordLimit := c.Limits.RecordBytes
	if recordLimit <= 0 {
		recordLimit = archive.MaxRecordBytes
	}
	recordLimit = min(recordLimit, archive.MaxRecordBytes)
	if in.File.Length() > rawLimit {
		return out, release, archive.ErrRecordTooLarge
	}
	if !reservation.Reserve(4096) {
		return out, release, errBudget
	}
	held += 4096
	reader := bufio.NewReaderSize(io.NewSectionReader(in.File, 0, in.File.Length()), 4096)
	rows := []retained{}
	gaps := privacyGaps{}
	line := int64(0)
	encodedTotal := int64(0)
	for {
		if err = ctx.Err(); err != nil {
			return out, release, err
		}
		raw, charge, readErr := physicalLine(ctx, reader, recordLimit, reservation)
		held += charge
		if readErr != nil {
			reservation.Release(charge)
			held -= charge
			if errors.Is(readErr, io.EOF) {
				break
			}
			return out, release, readErr
		}

		line++
		raw = raw[:len(raw)-1]
		if len(raw) == 0 {
			reservation.Release(charge)
			held -= charge
			return out, release, errFormat
		}
		var decoded []retained
		if line == 1 {
			decoded, err = frame(raw, line)
		} else {
			var r retained
			r, err = envelope(raw, position{Line: line})
			decoded = []retained{r}
		}
		if err == nil {
			for _, r := range decoded {
				var safe retained
				safe, err = privacyWithGaps(r, len(rows)+1, gaps.add)
				if err != nil {
					break
				}
				encoded, _ := json.Marshal(safe)
				encodedTotal += int64(len(encoded))
				ceiling := c.Limits.FilteredBytes
				if ceiling <= 0 {
					ceiling = archive.MaxRecordBytes
				}
				if encodedTotal > ceiling {
					err = archive.ErrRecordTooLarge
					break
				}
				n := int64(len(encoded))*8 + 512
				if !reservation.Reserve(n) {
					err = errBudget
					break
				}
				held += n
				rows = append(rows, safe)
			}
		}
		reservation.Release(charge)
		held -= charge
		if err != nil {
			return out, release, err
		}
		if c.Limits.Records > 0 && len(rows) > c.Limits.Records {
			return out, release, archive.ErrRecordTooLarge
		}
	}
	if err = in.File.Check(); err != nil {
		return out, release, err
	}
	if err = validateRows(rows); err != nil {
		return out, release, err
	}
	out, err = filtered(rows, c.Limits, gaps.list())
	return out, release, err
}

func privacyWithGaps(r retained, record int, addGap func(string, int, string)) (retained, error) {
	original := r
	// Every retained string, including keys and typed identity, passes the shared
	// policy. Changed authority is refused rather than coalescing redacted owners.
	raw, _ := json.Marshal(r)
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return r, errFormat
	}
	var strip func(any) any
	strip = func(v any) any {
		switch x := v.(type) {
		case string:
			removed, s := archive.StripInjectedContext(x)
			if removed {
				addGap("hidden_instruction_omitted", record, "")
			}
			return s
		case map[string]any:
			for k, v := range x {
				x[k] = strip(v)
			}
		case []any:
			for i, v := range x {
				x[i] = strip(v)
			}
		}
		return v
	}
	safe, ok := archive.SanitizeValue(strip(value), &archive.PrivacyState{ExtraAllowed: retainedKeys, Record: record, AddGap: addGap})
	if !ok {
		return r, errFormat
	}
	raw, e := json.Marshal(safe)
	if e != nil {
		return r, errFormat
	}
	r = retained{}
	if e = json.Unmarshal(raw, &r); e != nil {
		return r, errFormat
	}
	// Restore exact-number map values through the same strict JSON decoder.
	m, _, e := decodeObject(raw)
	if e != nil {
		return r, e
	}
	r.Fields = object(m["fields"])
	if r.ID != original.ID || r.Stream != original.Stream || !reflect.DeepEqual(r.Causation, original.Causation) {
		return r, errFormat
	}
	for k, v := range original.Fields {
		switch fieldName(k) {
		case fieldPrompt, fieldText, fieldNewName, fieldPreviousName:
		case fieldSchemaVersion, fieldPermissionEpoch, fieldEnvelopeSequence, fieldSourceRunRecordSequence, fieldResume:
			fallthrough
		default:
			if !reflect.DeepEqual(v, r.Fields[k]) {
				return r, errFormat
			}
		}
	}
	return r, nil
}

func filtered(rows []retained, limits agentapi.ReadLimits, gaps []archive.CaptureGap) (archive.FilteredTranscript, error) {
	out := archive.FilteredTranscript{Format: "muse-jsonl", SessionIDs: []string{rows[0].Stream.ID}, NativeStartAt: time.UnixMicro(rows[0].Timestamp).UTC(), NativeStartComplete: true, LocalIdentity: archive.NativeSessionIdentity{ID: rows[0].Stream.ID}, Gaps: []archive.CaptureGap{{Code: "muse_local_conversation_only", Detail: "Own visible conversation only"}, {Code: "usage_cache_reasoning_cost_unavailable"}, {Code: "tool_facts_unavailable"}, {Code: "model_switches_unavailable"}, {Code: "child_coverage_unavailable"}}}
	out.Gaps = append(out.Gaps, gaps...)
	for _, r := range rows {
		if r.Type == payloadRuntimeSessionMetadata {
			if _, present := r.Fields["model_id"]; !present {
				out.Gaps = append(out.Gaps, archive.CaptureGap{Code: "model_identity_unavailable", Detail: "Startup metadata does not identify a model"})
			}
			break
		}
	}
	if pendingResponse(rows) {
		out.Gaps = append(out.Gaps, archive.CaptureGap{Code: "muse_response_pending", Detail: "A qualified own prompt has no committed assistant response yet"})
	}
	var total int64
	for _, r := range rows {
		b, e := json.Marshal(r)
		if e != nil {
			return out, e
		}
		total += int64(len(b))
		ceiling := limits.FilteredBytes
		if ceiling <= 0 {
			ceiling = archive.MaxRecordBytes
		}
		if total > ceiling {
			return archive.FilteredTranscript{}, archive.ErrRecordTooLarge
		}
		out.Records = append(out.Records, b)
	}
	out.Boundary = archive.CaptureBoundary{RetainedRecords: len(rows), RetainedBytes: int(total)}
	return out, nil
}

func retainedRows(ctx context.Context, b archive.SourceBundle) ([]retained, error) {
	if b.NativeChild || b.ParentSessionID != "" || b.SchemaVersion != archive.SourceSchemaVersion || b.Capture.Harness.Name != "muse" || b.Capture.AdapterName != "muse" || b.Capture.SourceFormat != "muse-jsonl" || len(b.NativeText) != 0 || b.History != nil {
		return nil, errFormat
	}
	rows := []retained{}
	total := 0
	for _, m := range b.NativeRecords {
		if e := ctx.Err(); e != nil {
			return nil, e
		}
		if !validRetainedStrings(m, 0) {
			return nil, errFormat
		}
		if _, e := agentmeta.JSONWireBound(ctx, m, int64(archive.MaxRecordBytes-total)); e != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, archive.ErrRecordTooLarge
		}
		raw, e := json.Marshal(m)
		if e != nil {
			return nil, errFormat
		}
		total += len(raw)
		if total > archive.MaxRecordBytes {
			return nil, archive.ErrRecordTooLarge
		}
		obj, _, e := decodeObject(raw)
		if e != nil || !keys(obj, "v", "pos", "id", "stream", "seq", "ts_us", "schema", "type", "payload_schema", "record_type", "durability", "causation_id", "fields") {
			return nil, errFormat
		}
		if !keys(object(obj["pos"]), "line", "child") || !keys(object(obj["stream"]), "kind", "id") {
			return nil, errFormat
		}
		var r retained
		if json.Unmarshal(raw, &r) != nil {
			return nil, errFormat
		}
		r.Fields = object(obj["fields"])
		if !validFields(r) {
			return nil, errFormat
		}
		rows = append(rows, r)
	}
	if len(rows) > 0 && b.NativeSessionID != "" && b.NativeSessionID != rows[0].Stream.ID {
		return nil, errFormat
	}
	return rows, nil
}

// Refilter revalidates and reapplies privacy without reopening native sources.
func (f Filter) Refilter(ctx context.Context, b archive.SourceBundle, at time.Time) (archive.FilteredTranscript, error) {
	return f.RefilterBounded(ctx, b, at, agentapi.ReadLimits{})
}

// RefilterBounded enforces output ceilings before returning retained evidence.
func (Filter) RefilterBounded(ctx context.Context, b archive.SourceBundle, _ time.Time, limits agentapi.ReadLimits) (archive.FilteredTranscript, error) {
	if limits.Records > 0 && len(b.NativeRecords) > limits.Records {
		return archive.FilteredTranscript{}, archive.ErrRecordTooLarge
	}
	rows, e := retainedRows(ctx, b)
	if e != nil {
		return archive.FilteredTranscript{}, e
	}
	if limits.Records > 0 && len(rows) > limits.Records {
		return archive.FilteredTranscript{}, archive.ErrRecordTooLarge
	}
	gaps, e := retainedPrivacyGaps(ctx, b, len(rows))
	if e != nil {
		return archive.FilteredTranscript{}, e
	}
	for i, r := range rows {
		if e = ctx.Err(); e != nil {
			return archive.FilteredTranscript{}, e
		}
		rows[i], e = privacyWithGaps(r, i+1, gaps.add)
		if e != nil {
			return archive.FilteredTranscript{}, e
		}
	}
	if e = validateRows(rows); e != nil {
		return archive.FilteredTranscript{}, e
	}
	return filtered(rows, limits, gaps.list())
}

// RefilterLeased reserves retained input expansion and output ownership.
func (f Filter) RefilterLeased(ctx context.Context, b archive.SourceBundle, at time.Time, budget *agentapi.NativeReadBudget) (out archive.FilteredTranscript, release func(), err error) {
	reservation := budget.Reservation()
	held := int64(0)
	release = func() { reservation.Release(held); held = 0 }
	defer func() {
		if err != nil {
			release()
		}
	}()
	for _, r := range b.NativeRecords {
		size, e := agentmeta.JSONWireBound(ctx, r, archive.MaxRecordBytes)
		if e != nil {
			if ctx.Err() != nil {
				return out, release, ctx.Err()
			}
			return out, release, archive.ErrRecordTooLarge
		}
		n := size*128 + 512
		if !reservation.Reserve(n) {
			return out, release, errBudget
		}
		held += n
	}
	out, err = f.Refilter(ctx, b, at)
	return out, release, err
}

// EvidenceExtends requires an unchanged validated retained prefix. Equality does
// not certify physical continuity or admission of a native copy.
func (Filter) EvidenceExtends(a, b archive.SourceBundle) bool {
	if a.ArchiveSessionID != b.ArchiveSessionID || a.ProjectID != b.ProjectID || a.NativeSessionID != b.NativeSessionID || a.Capture.Harness != b.Capture.Harness || a.Capture.AdapterName != b.Capture.AdapterName || a.Capture.AdapterVersion != b.Capture.AdapterVersion || a.Capture.FilterVersion != b.Capture.FilterVersion || a.Capture.SourceFormat != b.Capture.SourceFormat {
		return false
	}
	left, e := retainedRows(context.Background(), a)
	if e != nil || validateRows(left) != nil {
		return false
	}
	right, e := retainedRows(context.Background(), b)
	if e != nil || validateRows(right) != nil || len(right) < len(left) {
		return false
	}
	for i := range left {
		x, _ := json.Marshal(left[i])
		y, _ := json.Marshal(right[i])
		if !bytes.Equal(x, y) {
			return false
		}
	}
	return true
}

var _ agentapi.TranscriptFilter = Filter{}

var _ agentapi.LeasedTranscriptFilter = Filter{}

var _ agentapi.LeasedTranscriptRefilter = Filter{}

var _ agentapi.TranscriptParser = Parser{}

var retainedKeys = func() map[string]bool {
	keys := strings.Fields("v pos line child id stream kind seq ts_us schema type payload_schema record_type durability causation_id fields schema_version format permission_epoch cause workspace_root provider_id model_id tool_surface_version session_id resume selection publication_id command_id command_kind activation_source cwd intent_id source_session_id surface semantic_kind envelope_record_id envelope_sequence run_id run_started_session_record_id run_started_source_record_id outcome_kind new_name previous_name source operation_id root_agent_id root_session_id event_kind source_run_record_id source_run_record_sequence prompt message_id response_id text")
	out := map[string]bool{}
	for _, key := range keys {
		out[key] = true
	}
	return out
}()

func physicalLine(ctx context.Context, reader *bufio.Reader, limit int64, reservation *agentapi.NativeReadReservation) (raw []byte, charge int64, err error) {
	for {
		if err = ctx.Err(); err != nil {
			return raw, charge, err
		}
		fragment, e := reader.ReadSlice('\n')
		if int64(len(raw)+len(fragment)) > limit {
			return raw, charge, archive.ErrRecordTooLarge
		}
		// Reserve before copying or expanding JSON maps and nested child strings.
		n := int64(len(fragment)) * 128
		if !reservation.Reserve(n) {
			return raw, charge, errBudget
		}
		charge += n
		raw = append(raw, fragment...)
		if errors.Is(e, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(e, io.EOF) && len(raw) == 0 {
			return raw, charge, io.EOF
		}
		if e != nil {
			return raw, charge, errFormat
		}
		return raw, charge, nil
	}
}

func pendingResponse(rows []retained) bool {
	pending := map[string]bool{}
	for _, r := range rows {
		if r.Type != payloadRuntimeSession || str(r.Fields["kind"]) != "run" {
			continue
		}
		run := str(r.Fields["run_id"])
		switch runtimeEvent(str(r.Fields["event_kind"])) {
		case eventStarted:
			pending[run] = true
		case eventAssistantMessageCommitted:
			delete(pending, run)

		case eventAccepted, eventAssistantToolCallsCommitted, eventCancelled, eventCompleted, eventContextBlockDiagnostic, eventContextBlockUpdated, eventFailed, eventGoalUsageAttribution, eventHookRunStarted, eventHookRunTerminal, eventModelCompleted, eventModelInputTraceRecorded, eventModelRequestConfigured, eventModelResponseCreated, eventProposed, eventProviderRequestOptionsConfigured, eventResourceUsageSampled, eventScheduled, eventSideEffectIntent, eventStatus, eventTaskStreamLinked, eventTerminal, eventToolResultBatchCommitted:
			// These known variants keep the existing non-special-case behavior.
		}
	}
	return len(pending) > 0
}

var errBudget = agentapi.ReadBudgetLimit(errors.New("muse decoding or retained output exceeds shared memory budget"))

// privacyGaps keeps fixed, content-free provenance with the first affected logical
// record. One entry per shared code bounds it independently of transcript size.
type privacyGaps map[string]int

func privacyGapDetail(code string) string {
	switch code {
	case "binary_content_omitted":
		return "binary content omitted"
	case "content_truncated":
		return "content truncated"
	case "hidden_instruction_omitted":
		return "injected instruction block omitted"
	case "hidden_or_unknown_nested_content_omitted":
		return "nested content omitted"
	case "record_without_allowed_fields_omitted":
		return "record omitted"
	case "sensitive_content_redacted":
		return "content redacted"
	case "sensitive_or_hidden_field_omitted":
		return "sensitive or hidden field omitted"
	case "unknown_field_omitted":
		return "unknown field omitted"
	case "unsupported_value_omitted":
		return "value omitted"
	}
	return ""
}

func (g privacyGaps) add(code string, record int, _ string) {
	if privacyGapDetail(code) == "" || record < 1 {
		return
	}
	if prior, exists := g[code]; !exists || record < prior {
		g[code] = record
	}
}

func (g privacyGaps) list() []archive.CaptureGap {
	out := make([]archive.CaptureGap, 0, len(g))
	for code, record := range g {
		out = append(out, archive.CaptureGap{Code: code, Record: record, Detail: privacyGapDetail(code)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Code < out[j].Code })
	return out
}

func retainedPrivacyGaps(ctx context.Context, b archive.SourceBundle, records int) (privacyGaps, error) {
	gaps := privacyGaps{}
	for _, gap := range b.Capture.Gaps {
		if e := ctx.Err(); e != nil {
			return nil, e
		}
		if gap.Record <= records {
			gaps.add(gap.Code, gap.Record, "")
		}
	}
	return gaps, nil
}
