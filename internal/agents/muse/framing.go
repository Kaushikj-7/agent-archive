package muse

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strings"
)

type position struct {
	Line  int64 `json:"line"`
	Child *int  `json:"child"`
}

type retained struct {
	Version       int            `json:"v"`
	Position      position       `json:"pos"`
	ID            string         `json:"id"`
	Stream        stream         `json:"stream"`
	Sequence      int64          `json:"seq"`
	Timestamp     int64          `json:"ts_us"`
	Schema        int64          `json:"schema"`
	Type          payloadType    `json:"type"`
	PayloadSchema int64          `json:"payload_schema"`
	RecordType    string         `json:"record_type"`
	Durability    string         `json:"durability"`
	Causation     *string        `json:"causation_id"`
	Fields        map[string]any `json:"fields"`
}

type stream struct {
	Kind streamKind `json:"kind"`
	ID   string     `json:"id"`
}

func uuid(s string) ([]byte, bool) {
	if len(s) != 36 || s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' {
		return nil, false
	}
	b, e := hex.DecodeString(strings.ReplaceAll(s, "-", ""))
	return b, e == nil && len(b) == 16 && s == strings.ToLower(s)
}

func canonical(raw []byte) bool {
	var b bytes.Buffer
	if json.Compact(&b, raw) != nil {
		return false
	}
	return bytes.Equal(b.Bytes(), raw)
}

func frame(raw []byte, line int64) ([]retained, error) {
	m, order, e := decodeObject(raw)
	if e != nil {
		return nil, e
	}
	if str(m["retained_frame"]) != "session_permission_transaction" || line != 1 || !canonical(raw) || !slices.Equal(order, []string{"retained_frame", "frame_schema_version", "outer_log_ordinal", "transaction_id", "children", "content_sha256"}) || num(m["frame_schema_version"]) != 1 || num(m["outer_log_ordinal"]) != 1 {
		return nil, errFormat
	}
	id, ok := uuid(str(m["transaction_id"]))
	if !ok {
		return nil, errFormat
	}
	children, ok := m["children"].([]any)
	if !ok || len(children) != 2 {
		return nil, errFormat
	}
	hash := sha256.New()
	hash.Write([]byte("muse.session_permission_transaction.v1\x00"))
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], 1)
	hash.Write(b[:])
	hash.Write(id)
	binary.BigEndian.PutUint32(b[:4], 2)
	hash.Write(b[:4])
	out := make([]retained, 0, 2)
	for i, v := range children {
		child := object(v)
		if !keys(child, "child_index", "record_json") || num(child["child_index"]) != int64(i) {
			return nil, errFormat
		}
		// Wrapper order and child JSON whitespace are native grammar independently
		// of the unkeyed checksum. Inspect original wrapper order below as well.
		s, ok := child["record_json"].(string)
		if !ok || !canonical([]byte(s)) {
			return nil, errFormat
		}
		binary.BigEndian.PutUint32(b[:4], uint32(i))
		hash.Write(b[:4])
		binary.BigEndian.PutUint64(b[:], uint64(len(s)))
		hash.Write(b[:])
		hash.Write([]byte(s))
		r, e := envelope([]byte(s), position{Line: line, Child: &i})
		if e != nil {
			return nil, e
		}
		out = append(out, r)
	}
	// The outer frame is compact; raw wrappers must use the closed field order.
	var wire struct {
		Children []json.RawMessage `json:"children"`
	}
	if json.Unmarshal(raw, &wire) != nil {
		return nil, errFormat
	}
	for _, r := range wire.Children {
		_, k, e := decodeObject(r)
		if e != nil || !slices.Equal(k, []string{"child_index", "record_json"}) {
			return nil, errFormat
		}
	}
	if str(m["content_sha256"]) != "sha256:"+hex.EncodeToString(hash.Sum(nil)) {
		return nil, errFormat
	}
	if out[0].Type != payloadRuntimeSessionPermissionFormatDeclared || out[1].Type != payloadRuntimeSessionPermissionProfileCommitted || out[0].Sequence != 1 || out[1].Sequence != 2 || out[0].Timestamp != out[1].Timestamp || out[0].Stream != out[1].Stream || out[0].Causation != nil || out[1].Causation != nil {
		return nil, errFormat
	}
	return out, nil
}

func envelope(raw []byte, pos position) (retained, error) {
	m, _, e := decodeObject(raw)
	if e != nil {
		return retained{}, e
	}
	if !keys(m, "schema_version", "id", "stream", "sequence", "recorded_at", "record_type", "durability", "causation_id", "payload_type", "payload_schema_version", "payload") {
		return retained{}, errFormat
	}
	st := object(m["stream"])
	if !keys(st, "kind", "id") || str(st["kind"]) != "session" {
		return retained{}, errFormat
	}
	id := str(m["id"])
	sid := str(st["id"])
	if _, ok := uuid(id); !ok {
		return retained{}, errFormat
	}
	if _, ok := uuid(sid); !ok {
		return retained{}, errFormat
	}
	var cause *string
	if m["causation_id"] != nil {
		s := str(m["causation_id"])
		if _, ok := uuid(s); !ok {
			return retained{}, errFormat
		}
		cause = &s
	}
	r := retained{Version: 1, Position: pos, ID: id, Stream: stream{streamSession, sid}, Sequence: num(m["sequence"]), Timestamp: num(m["recorded_at"]), Schema: num(m["schema_version"]), Type: payloadType(str(m["payload_type"])), PayloadSchema: num(m["payload_schema_version"]), RecordType: str(m["record_type"]), Durability: str(m["durability"]), Causation: cause}
	if r.Schema != 1 || r.Sequence < 1 || r.Timestamp < 1 || r.RecordType != "event" || r.Durability != "durable" {
		return retained{}, errFormat
	}
	r.Fields, e = project(r, object(m["payload"]))
	return r, e
}
