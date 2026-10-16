package muse

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
)

func inspectSynthetic(t *testing.T, raw []byte) (agentapi.NativeHeader, error) {
	t.Helper()
	return (NativeHeaders{}).InspectHeader(agentapi.NativeHeaderRequest{Purpose: agentapi.DiscoveryHandoff, Path: filepath.Join(string(filepath.Separator), "store", "00000000-0000-7000-8000-000000000001", "session.jsonl"), Scan: func(visit func([]byte) bool) error {
		for line := range bytes.SplitSeq(raw, []byte("\n")) {
			if len(line) != 0 && !visit(line) {
				break
			}
		}
		return nil
	}})
}

func TestNativeHeaderCorroboratesOnlySupportedStartup(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"synthetic-exec-conversation.jsonl", "synthetic-msp-conversation.jsonl"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			raw := readSynthetic(t, name)
			var h agentapi.NativeHeader
			err := (func() error {
				rows, err := frame(bytes.Split(raw, []byte("\n"))[0], 1)
				if err != nil {
					return err
				}
				h, err = (NativeHeaders{}).InspectHeader(agentapi.NativeHeaderRequest{Purpose: agentapi.DiscoveryHandoff, Path: filepath.Join(string(filepath.Separator), "store", rows[0].Stream.ID, "session.jsonl"), Scan: func(visit func([]byte) bool) error {
					for line := range bytes.SplitSeq(raw, []byte("\n")) {
						if len(line) > 0 && !visit(line) {
							break
						}
					}
					return nil
				}})
				return err
			})()
			if err != nil || h.Directory != "/work/synthetic-muse" || !h.StartedAt.Equal(time.UnixMicro(1792083855882704)) {
				t.Fatalf("header %+v: %v", h, err)
			}
		})
	}
}

func TestNativeHeaderRefusesUnsupportedAndIncompletePrefixes(t *testing.T) {
	t.Parallel()
	raw := readSynthetic(t, "synthetic-exec-conversation.jsonl")
	lines := bytes.Split(raw, []byte("\n"))
	for name, input := range map[string][]byte{"missing frame": bytes.Join(lines[1:], []byte("\n")), "cut prefix": bytes.Join(lines[:2], []byte("\n")), "bad checksum": bytes.Replace(raw, []byte(`"content_sha256":"sha256:`), []byte(`"content_sha256":"sha256:0`), 1), "wrong schema": bytes.Replace(raw, []byte(`"frame_schema_version":1`), []byte(`"frame_schema_version":2`), 1)} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if h, err := inspectSynthetic(t, input); err == nil || h.NativeID != "" {
				t.Fatalf("accepted %+v %v", h, err)
			}
		})
	}
	for _, purpose := range []agentapi.DiscoveryPurpose{agentapi.DiscoveryImport, agentapi.DiscoveryProjects, agentapi.DiscoveryBoundedProjects} {
		if _, err := (NativeHeaders{}).InspectHeader(agentapi.NativeHeaderRequest{Purpose: purpose, Scan: func(func([]byte) bool) error { t.Fatal("scanned unsupported purpose"); return nil }}); err == nil {
			t.Fatal("accepted purpose")
		}
	}
}

func TestPreviewDoesNotInferHumanPromptAndMarksUnknownPartial(t *testing.T) {
	t.Parallel()
	raw := readSynthetic(t, "synthetic-exec-conversation.jsonl")
	for line := range bytes.SplitSeq(raw, []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		p, err := (Previewer{}).PreviewRecord(t.Context(), line)
		if err != nil || p.Title != "" || p.Kind == archive.TurnKindHumanPrompt || p.Command != "" {
			t.Fatalf("unsafe preview %+v: %v", p, err)
		}
	}
	p, err := (Previewer{}).PreviewRecord(t.Context(), []byte(`{"unknown":"secret"}`))
	if err != nil || len(p.Gaps) != 1 || p.Gaps[0].Code != "preview_partial" || strings.Contains(p.Name, "secret") {
		t.Fatalf("unknown preview %+v: %v", p, err)
	}
}

func TestNativeHeaderIsOnlyNormalizedScanCorroboration(t *testing.T) {
	t.Parallel()
	raw := readSynthetic(t, "synthetic-exec-conversation.jsonl")
	for _, prefix := range []string{"\n", " "} {
		hint, err := (NativeHeaders{}).InspectHeader(agentapi.NativeHeaderRequest{Purpose: agentapi.DiscoveryHandoff, Path: filepath.Join(string(filepath.Separator), "store", "00000000-0000-7000-8000-000000000001", "session.jsonl"), Scan: func(visit func([]byte) bool) error {
			for line := range bytes.SplitSeq(append([]byte(prefix), raw...), []byte("\n")) {
				line = bytes.TrimSpace(line)
				if len(line) > 0 && !visit(line) {
					break
				}
			}
			return nil
		}})
		if err != nil || hint.NativeID == "" {
			t.Fatalf("normalized hint %+v %v", hint, err)
		}
		if _, err := filterBytes(t, append([]byte(prefix), raw...), agentapi.ReadLimits{}); err == nil {
			t.Fatal("hint bypassed strict whole-file framing")
		}
	}
}

func TestPreviewNamesUseQualifiedOwnerEnumAndPrivacy(t *testing.T) {
	t.Parallel()
	lines := bytes.Split(readSynthetic(t, "synthetic-exec-conversation.jsonl"), []byte("\n"))
	var record map[string]any
	if err := json.Unmarshal(lines[1], &record); err != nil {
		t.Fatal(err)
	}
	record["payload_type"] = "session.name.changed"
	record["payload_schema_version"] = 1
	payload := map[string]any{"session_id": "00000000-0000-7000-8000-000000000001", "new_name": "Safe generated name", "previous_name": nil, "source": "automatic", "operation_id": "00000000-0000-4000-8000-000000000003"}
	record["payload"] = payload
	raw, _ := json.Marshal(record)
	p, err := (Previewer{}).PreviewRecord(t.Context(), raw)
	if err != nil || p.Name != "Safe generated name" || p.NameSource != archive.SessionNameGenerated || p.NameNativeID != payload["session_id"] {
		t.Fatalf("name %+v %v", p, err)
	}
	acc := archive.PreviewAccumulator{NativeID: "00000000-0000-7000-8000-000000000099"}
	acc.AddFacts(p, true)
	if acc.Labels.Name != "" {
		t.Fatal("cross-owner preview name")
	}
	payload["source"] = "unknown"
	raw, _ = json.Marshal(record)
	p, _ = (Previewer{}).PreviewRecord(t.Context(), raw)
	if p.Name != "" || len(p.Gaps) == 0 {
		t.Fatal("unknown enum accepted")
	}
	payload["source"] = "automatic"
	payload["new_name"] = "<system-reminder>HIDDEN_CONTEXT_SENTINEL</system-reminder>Visible name"
	raw, _ = json.Marshal(record)
	p, _ = (Previewer{}).PreviewRecord(t.Context(), raw)
	if strings.Contains(p.Name, "HIDDEN_CONTEXT_SENTINEL") || !strings.Contains(p.Name, "Visible name") {
		t.Fatalf("unsafe name %+v", p)
	}
}

func TestNativeHeaderRejectsContradictoryStartupAndMismatchedParent(t *testing.T) {
	t.Parallel()
	raw := readSynthetic(t, "synthetic-exec-conversation.jsonl")
	for _, input := range [][]byte{bytes.Replace(raw, []byte(`"workspace_root":"/work/synthetic-muse"`), []byte(`"workspace_root":"relative"`), 1), bytes.Replace(raw, []byte(`"resume":false`), []byte(`"resume":true`), 1), bytes.Replace(raw, []byte(`"session_id":"00000000-0000-7000-8000-000000000001"`), []byte(`"session_id":"00000000-0000-7000-8000-000000000099"`), 1)} {
		if h, err := inspectSynthetic(t, input); err == nil || h.NativeID != "" {
			t.Fatalf("contradictory header %+v %v", h, err)
		}
	}
	_, err := (NativeHeaders{}).InspectHeader(agentapi.NativeHeaderRequest{Purpose: agentapi.DiscoveryHandoff, Path: "/store/unrelated/session.jsonl", Scan: func(visit func([]byte) bool) error {
		for line := range bytes.SplitSeq(raw, []byte("\n")) {
			if !visit(line) {
				break
			}
		}
		return nil
	}})
	if err == nil {
		t.Fatal("parent identity mismatch accepted")
	}
}
