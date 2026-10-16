package nativecodec

import (
	"encoding/json"
	"io"
	"strings"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/codexmeta"
)

func codexCaptureMetadata(line []byte, filename string) error {
	var record struct {
		Payload codexmeta.CodexMeta `json:"payload"`
	}
	if json.Unmarshal(line, &record) != nil {
		return &archive.FilterError{Reason: "invalid Codex metadata"}
	}
	m := record.Payload
	facts, outcome := m.Relationships()
	if outcome != "" {
		return &archive.FilterError{Reason: string(outcome)}
	}
	if rollout := codexmeta.RolloutID(filename); rollout != "" && !strings.EqualFold(rollout, m.ID) {
		return archive.ErrRelatedHistory
	}
	if facts.Child || facts.ForkID != "" || facts.HistoryBase != nil {
		return archive.ErrRelatedHistory
	}
	if m.HistoryMode != "" && m.HistoryMode != codexmeta.CodexHistoryLegacy && m.HistoryMode != codexmeta.CodexHistoryPaginated {
		return &archive.FilterError{Reason: "unsupported Codex history"}
	}
	return nil
}

// FilterCodexRetainedEncodedJSONL borrows bounded encoding scratch for retained records.
func FilterCodexRetainedEncodedJSONL(r io.Reader, encoder func(map[string]any) ([]byte, error), beforeRecord func(int) (func(), error), boundary archive.CaptureBoundary) (archive.FilteredTranscript, error) {
	return filterJSONLEncoded(r, "codex-jsonl", map[string]bool{"session_meta": true, "turn_context": true, "response_item": true, "event_msg": true, "message": true, "token_usage_record": true}, nil, nil, encoder, beforeRecord, boundary)
}

// FilterCodexCaptureEncodedJSONL keeps the capture proof while borrowing record resources.
func FilterCodexCaptureEncodedJSONL(r io.Reader, filename string, encoder func(map[string]any) ([]byte, error), beforeRecord func(int) (func(), error), boundary archive.CaptureBoundary) (archive.FilteredTranscript, error) {
	return filterJSONLEncoded(r, "codex-jsonl", map[string]bool{"session_meta": true, "turn_context": true, "response_item": true, "event_msg": true, "message": true, "token_usage_record": true}, nil, func(line []byte) error { return codexCaptureMetadata(line, filename) }, encoder, beforeRecord, boundary)
}

// InspectCodexCaptureEncodedJSONL runs the capture loop with a synchronous safe
// object observer. Encoded rows are counted exactly and discarded after use.
func InspectCodexCaptureEncodedJSONL(r io.Reader, filename string, inspect func(map[string]any), encoder func(map[string]any) ([]byte, error), beforeRecord func(int) (func(), error), boundary archive.CaptureBoundary) (archive.FilteredTranscript, error) {
	return filterJSONLWithOutput(r, "codex-jsonl", map[string]bool{"session_meta": true, "turn_context": true, "response_item": true, "event_msg": true, "message": true, "token_usage_record": true}, nil, func(line []byte) error { return codexCaptureMetadata(line, filename) }, encoder, beforeRecord, inspect, boundary)
}
