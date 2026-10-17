package agentapi_test

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agents/claude"
	"github.com/wangjohn/agent-archive/internal/agents/cursor"
	"github.com/wangjohn/agent-archive/internal/agents/muse"
	"github.com/wangjohn/agent-archive/internal/archive"
)

func TestOrdinaryOwnedEvidenceUsesCompleteAdapterEvidence(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		filter agentapi.TranscriptFilter
	}{{"claude", claude.Filter{}}, {"cursor", cursor.Filter{}}, {"muse", muse.Filter{}}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, ok := tc.filter.(agentapi.RevisionEvidence); ok {
				t.Fatal("fixture unexpectedly has graph revision evidence")
			}
			previous := archive.SourceBundle{SchemaVersion: archive.SourceSchemaVersion, ArchiveSessionID: "synthetic", NativeSessionID: "native", ProjectID: "project", Capture: archive.SourceCapture{Harness: archive.Harness{Name: tc.name}, AdapterName: tc.name, AdapterVersion: tc.filter.Version(), FilterVersion: archive.FilterVersion, SourceFormat: tc.name + "-jsonl"}, NativeRecords: []map[string]any{{"type": "assistant", "message": map[string]any{"role": "assistant", "content": "retained"}}}, NativeText: []archive.TextTranscript{{Format: "text", Content: "retained text"}}}
			candidate := previous
			candidate.NativeRecords = append(append([]map[string]any{}, previous.NativeRecords...), map[string]any{"type": "assistant", "message": map[string]any{"role": "assistant", "content": "continued"}})
			candidate.NativeText = []archive.TextTranscript{{Format: "text", Content: "retained text continued"}}
			if tc.name == "muse" {
				data, err := os.ReadFile("../agents/muse/testdata/synthetic-exec-conversation.golden.jsonl")
				if err != nil {
					t.Fatal(err)
				}
				candidate.NativeRecords = nil
				for line := range bytes.SplitSeq(bytes.TrimSpace(data), []byte("\n")) {
					var record map[string]any
					if err := json.Unmarshal(line, &record); err != nil {
						t.Fatal(err)
					}
					candidate.NativeRecords = append(candidate.NativeRecords, record)
				}
				candidate.NativeText, previous.NativeText = nil, nil
				previous.NativeSessionID, candidate.NativeSessionID = "00000000-0000-7000-8000-000000000001", "00000000-0000-7000-8000-000000000001"
				previous.NativeRecords = candidate.NativeRecords[:len(candidate.NativeRecords)-1]
			}
			if !tc.filter.EvidenceExtends(previous, candidate) {
				t.Fatal("invalid actual-adapter prefix fixture")
			}
			if !agentapi.OwnedEvidenceCovered(tc.filter, previous, candidate) {
				t.Fatal("valid ordinary retained prefix refused")
			}
			foreign := candidate
			foreign.NativeSessionID = "foreign"
			if agentapi.OwnedEvidenceCovered(tc.filter, previous, foreign) {
				t.Fatal("foreign native owner accepted")
			}
			changed := candidate
			changed.NativeRecords = append([]map[string]any{}, candidate.NativeRecords...)
			changed.NativeRecords[0] = map[string]any{"type": "unrelated"}
			if agentapi.OwnedEvidenceCovered(tc.filter, previous, changed) {
				t.Fatal("changed retained record accepted")
			}
			if tc.name != "muse" {
				changed = candidate
				changed.NativeText = []archive.TextTranscript{{Format: "text", Content: "unrelated text"}}
				if agentapi.OwnedEvidenceCovered(tc.filter, previous, changed) {
					t.Fatal("changed retained text accepted")
				}
			}
			candidate.History = &archive.SourceHistory{ActiveRolloutID: "native"}
			if agentapi.OwnedEvidenceCovered(tc.filter, previous, candidate) {
				t.Fatal("ordinary adapter granted graph-history authority")
			}
		})
	}
}
