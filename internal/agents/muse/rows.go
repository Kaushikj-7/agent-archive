package muse

import (
	"encoding/json"
	"reflect"
)

// logicalRow is a closed tagged union. Ordinary serialization is unchanged.
type logicalRow struct {
	ordinary *retained
	omission *nativeOmission
}

type nativePosition struct {
	ID       string `json:"id"`
	Sequence int64  `json:"sequence"`
}

type omittedRecord struct {
	RecordType    string `json:"record_type"`
	Durability    string `json:"durability"`
	PayloadType   string `json:"payload_type"`
	PayloadSchema int64  `json:"payload_schema_version"`
	PayloadKind   string `json:"payload_kind"`
	Class         string `json:"omission_class"`
}

type nativeOmission struct {
	Version  int            `json:"v"`
	Kind     omissionKind   `json:"kind"`
	Position position       `json:"pos"`
	Stream   stream         `json:"stream"`
	Native   nativePosition `json:"position"`
	Schema   int64          `json:"schema"`
	Marker   string         `json:"marker"`
	Omitted  omittedRecord  `json:"omitted_record"`
}

var taskDelta = omittedRecord{"status", "ephemeral", "runtime.session", 1, "task", "task_tool_delta_v1"}

func ordinaryRow(r retained) logicalRow { return logicalRow{ordinary: &r} }

func (r logicalRow) valid() bool { return (r.ordinary != nil) != (r.omission != nil) }

func (r logicalRow) MarshalJSON() ([]byte, error) {
	if !r.valid() {
		return nil, errFormat
	}
	if r.ordinary != nil {
		return json.Marshal(r.ordinary)
	}
	return json.Marshal(r.omission)
}

func (r logicalRow) identity() (stream, string, int64, position) {
	if r.ordinary != nil {
		o := r.ordinary
		return o.Stream, o.ID, o.Sequence, o.Position
	}
	o := r.omission
	return o.Stream, o.Native.ID, o.Native.Sequence, o.Position
}

func marker(raw []byte, pos position) (logicalRow, error) {
	m, _, e := decodeObject(raw)
	if e != nil || !keys(m, "retained_marker", "schema_version", "stream", "position", "omitted_record") {
		return logicalRow{}, errFormat
	}
	o := nativeOmission{Version: 2, Kind: nativeOmissionKind, Position: pos, Schema: num(m["schema_version"]), Marker: str(m["retained_marker"])}
	b, _ := json.Marshal(m["stream"])
	if json.Unmarshal(b, &o.Stream) != nil {
		return logicalRow{}, errFormat
	}
	b, _ = json.Marshal(m["position"])
	if json.Unmarshal(b, &o.Native) != nil {
		return logicalRow{}, errFormat
	}
	b, _ = json.Marshal(m["omitted_record"])
	if json.Unmarshal(b, &o.Omitted) != nil {
		return logicalRow{}, errFormat
	}
	if !keys(object(m["stream"]), "kind", "id") || !keys(object(m["position"]), "id", "sequence") || !keys(object(m["omitted_record"]), "record_type", "durability", "payload_type", "payload_schema_version", "payload_kind", "omission_class") || !o.valid() {
		return logicalRow{}, errFormat
	}
	return logicalRow{omission: &o}, nil
}

func (o nativeOmission) valid() bool {
	_, a := uuid(o.Stream.ID)
	_, b := uuid(o.Native.ID)
	return o.Version == 2 && o.Kind == nativeOmissionKind && o.Schema == 1 && o.Marker == "omitted_live_only" && o.Omitted == taskDelta && o.Stream.Kind == streamSession && a && b && o.Native.Sequence > 2 && o.Native.Sequence <= exactMax && o.Position.Child == nil && o.Position.Line > 1
}

func decodeLogical(raw []byte) (logicalRow, error) {
	m, _, e := decodeObject(raw)
	if e != nil {
		return logicalRow{}, e
	}
	if num(m["v"]) == 2 {
		if !keys(m, "v", "kind", "pos", "stream", "position", "schema", "marker", "omitted_record") || !keys(object(m["pos"]), "line", "child") || !keys(object(m["stream"]), "kind", "id") || !keys(object(m["position"]), "id", "sequence") || !keys(object(m["omitted_record"]), "record_type", "durability", "payload_type", "payload_schema_version", "payload_kind", "omission_class") {
			return logicalRow{}, errFormat
		}
		var o nativeOmission
		if json.Unmarshal(raw, &o) != nil || !o.valid() {
			return logicalRow{}, errFormat
		}
		return logicalRow{omission: &o}, nil
	}
	if !keys(m, "v", "pos", "id", "stream", "seq", "ts_us", "schema", "type", "payload_schema", "record_type", "durability", "causation_id", "fields") || !keys(object(m["pos"]), "line", "child") || !keys(object(m["stream"]), "kind", "id") {
		return logicalRow{}, errFormat
	}
	var o retained
	if json.Unmarshal(raw, &o) != nil {
		return logicalRow{}, errFormat
	}
	o.Fields = object(m["fields"])
	if !validFields(o) {
		return logicalRow{}, errFormat
	}
	return ordinaryRow(o), nil
}

// ordinarySlots is an indexed view for the existing startup/conversation graph.
// Empty slots cannot satisfy authority or contribute activity; they are never serialized.
func ordinarySlots(rows []logicalRow) []retained {
	out := make([]retained, len(rows))
	for i, r := range rows {
		if r.ordinary != nil {
			out[i] = *r.ordinary
		}
	}
	return out
}

func validateLogical(rows []logicalRow) error {
	if len(rows) < 4 || !initialAuthority(ordinarySlots(rows)) {
		return errFormat
	}
	slots := ordinarySlots(rows)
	s := rootState{owner: slots[0].Stream.ID, start: slots[0].Timestamp, ids: map[string]bool{}}
	for i, r := range rows {
		if !r.valid() {
			return errFormat
		}
		if r.ordinary != nil {
			if !s.envelope(*r.ordinary, i) || !validFields(*r.ordinary) || s.authority(slots, i) != nil {
				return errFormat
			}
		} else {
			st, id, seq, pos := r.identity()
			if !r.omission.valid() || st.ID != s.owner || seq != int64(i+1) || pos.Line != int64(i) || s.ids[id] {
				return errFormat
			}
			s.ids[id] = true
		}
	}
	if s.original != 1 || s.workspace == "" || !startupPrefix(slots) {
		return errFormat
	}
	if _, e := conversation(slots); e != nil {
		return e
	}
	return nil
}

func privacyLogical(r logicalRow, n int, add func(string, int, string)) (logicalRow, error) {
	if !r.valid() {
		return logicalRow{}, errFormat
	}
	if r.ordinary != nil {
		o, e := privacyWithGaps(*r.ordinary, n, add)
		return ordinaryRow(o), e
	}
	// Marker strings are all fixed classification or canonical UUIDs. Apply the
	// identical shared string policy and refuse any identity/classification change.
	raw, _ := json.Marshal(r)
	safe, e := privacyMarker(raw, n, add)
	if e != nil {
		return logicalRow{}, e
	}
	next, e := decodeLogical(safe)
	if e != nil || !reflect.DeepEqual(r, next) {
		return logicalRow{}, errFormat
	}
	return next, nil
}

func decodeNativeRows(raw []byte, line int64, joins *nativeBodyJoins) ([]logicalRow, int64, error) {
	if line == 1 {
		children, e := frame(raw, line)
		if e != nil {
			return nil, 0, e
		}
		out := make([]logicalRow, 0, 2)
		for _, r := range children {
			out = append(out, ordinaryRow(r))
		}
		return out, 0, nil
	}
	m, _, e := decodeObject(raw)
	if e != nil {
		return nil, 0, e
	}
	if _, present := m["retained_marker"]; present {
		r, e := marker(raw, position{Line: line})
		return []logicalRow{r}, 0, e
	}
	r, e := envelope(raw, position{Line: line})
	if e != nil {
		return nil, 0, e
	}
	p := object(m["payload"])
	if joins.skillContent != nil {
		if e = joins.skillContent.capture(r, p); e != nil {
			return nil, 0, e
		}
	}
	if e = joins.observe(r, p); e != nil {
		return nil, 0, e
	}
	charge := int64(0)
	if enrichedTools(r.Fields) && str(r.Fields["event_kind"]) == string(eventAssistantToolCallsCommitted) {
		// Bounded private IDs, map entries, refs and batch slots outlive physical decoding.
		charge = 1024 + int64(len(r.Fields["tool_calls"].([]any)))*1024
	}
	if nativeNewFamily(r, p) || r.Sequence == 3 && r.Type == payloadRuntimeSessionMetadata {
		charge = int64(len(raw)) * 128
	}
	return []logicalRow{ordinaryRow(r)}, charge, nil
}

type omissionKind string

const nativeOmissionKind omissionKind = "native_omission"
