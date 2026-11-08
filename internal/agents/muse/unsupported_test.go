package muse

import (
	"bytes"
	"encoding/json"
	"errors"
	"maps"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/testutil/golden"
)

const unsupportedSentinel = "PRIVATE_UNSUPPORTED_RECORD_SENTINEL"

// withRecordBeforeEnd inserts a native record before the synthetic session end,
// renumbering the end record so the session stays contiguous.
func withRecordBeforeEnd(t *testing.T, payloadType string, payload map[string]any) ([]byte, int64) {
	t.Helper()
	lines := bytes.Split(bytes.TrimSpace(readSynthetic(t, "synthetic-exec-conversation.jsonl")), []byte("\n"))
	var end map[string]any
	if json.Unmarshal(lines[len(lines)-1], &end) != nil || str(end["payload_type"]) != string(payloadSessionEnd) {
		t.Fatal("synthetic session end changed")
	}
	extra := maps.Clone(end)
	extra["id"] = "00000000-0000-4000-8000-0000000000e1"
	extra["causation_id"] = nil
	extra["payload_type"], extra["payload_schema_version"], extra["payload"] = payloadType, 1, payload
	seq, _ := end["sequence"].(float64)
	at, _ := end["recorded_at"].(float64)
	end["sequence"], end["recorded_at"] = seq+1, at+1
	a, _ := json.Marshal(extra)
	b, _ := json.Marshal(end)
	lines = append(lines[:len(lines)-1], a, b)
	return append(bytes.Join(lines, []byte("\n")), '\n'), int64(len(lines) - 1)
}

func TestUnknownRecordFamilyIsOmittedWithAGap(t *testing.T) {
	t.Parallel()
	raw, line := withRecordBeforeEnd(t, "session.invented_future.observed", map[string]any{"kind": "invented_future", "record": map[string]any{"branch": unsupportedSentinel}})
	out, err := filterBytes(t, raw, agentapi.ReadLimits{})
	if err != nil {
		t.Fatal(err)
	}
	assertOmittedAt(t, out, line)
	retained := bytes.Join(out.Records, nil)
	for _, private := range []string{unsupportedSentinel, "invented_future"} {
		if bytes.Contains(retained, []byte(private)) {
			t.Fatalf("omitted record leaked %q", private)
		}
	}
	b := bundleFor(t, out)
	if _, err := (Parser{}).Parse(t.Context(), b); err != nil {
		t.Fatal(err)
	}
	again, err := (Filter{}).Refilter(t.Context(), b, time.Time{})
	if err != nil || !bytes.Equal(bytes.Join(again.Records, nil), retained) || !slices.Equal(again.Gaps, out.Gaps) {
		t.Fatalf("refilter changed the omission: %v", err)
	}
}

func TestUnknownAuthorityRecordStillRefusesTheSession(t *testing.T) {
	t.Parallel()
	for _, family := range []string{"session.permission_grant.recorded", "subagent.control.resume_context_recorded", "runtime.workflow_digest.observed", "session.login.completed"} {
		raw, line := withRecordBeforeEnd(t, family, map[string]any{"kind": "invented", "record": map[string]any{"value": unsupportedSentinel}})
		out, err := filterBytes(t, raw, agentapi.ReadLimits{})
		var refused *agentapi.NativeRefusalError
		if !errors.As(err, &refused) || refused.Reason != agentapi.RefusalUnsupportedRecord || refused.Line != line || len(out.Records) != 0 {
			t.Fatalf("%s: authority-bearing record was not refused: %v", family, err)
		}
	}
	// Run events naming an approval or a link to another session are
	// authority-bearing too: lineage is never omitted.
	for _, event := range []string{"approval_invented", "memory_reminder_child_session_linked", "workflow_child_lifecycle"} {
		raw, _ := withRecordBeforeEnd(t, string(payloadRuntimeSession), map[string]any{"kind": "run", "run_id": "00000000-0000-4000-8000-0000000000e2", "event": map[string]any{"kind": event}})
		if _, err := filterBytes(t, raw, agentapi.ReadLimits{}); !errors.Is(err, errFormat) {
			t.Fatalf("unknown %s event was not refused: %v", event, err)
		}
	}
}

func TestUnqualifiedAuthorityFamilyStillRefusesTheSession(t *testing.T) {
	t.Parallel()
	// A supported identity family in a shape the adapter cannot qualify.
	raw, _ := withRecordBeforeEnd(t, string(payloadSessionResumed), map[string]any{"kind": "session_resumed", "record": map[string]any{"schema_version": 2, "session_id": "00000000-0000-7000-8000-000000000001"}})
	if _, err := filterBytes(t, raw, agentapi.ReadLimits{}); !errors.Is(err, errFormat) {
		t.Fatalf("unqualified identity record was not refused: %v", err)
	}
}

func TestUnknownMembersOfIdentityRecordsAreDropped(t *testing.T) {
	t.Parallel()
	raw := readSynthetic(t, "synthetic-exec-conversation.jsonl")
	want, err := filterBytes(t, raw, agentapi.ReadLimits{})
	if err != nil {
		t.Fatal(err)
	}
	old := []byte(`"resume":false}`)
	if bytes.Count(raw, old) != 1 {
		t.Fatal("synthetic session opening changed")
	}
	raw = bytes.Replace(raw, old, []byte(`"resume":false,"workspace_kind":"`+unsupportedSentinel+`"}`), 1)
	out, err := filterBytes(t, raw, agentapi.ReadLimits{})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.EqualFunc(out.Records, want.Records, bytes.Equal) || bytes.Contains(bytes.Join(out.Records, nil), []byte(unsupportedSentinel)) {
		t.Fatal("dropped member changed or reached retained evidence")
	}
	// The session opening is logical record 4: the frame's two rows, metadata,
	// then the opening.
	if !slices.Contains(out.Gaps, archive.CaptureGap{Code: string(privacyUnknownFieldOmitted), Record: 4, Detail: privacyGapDetail(string(privacyUnknownFieldOmitted))}) {
		t.Fatalf("dropped member has no gap: %+v", out.Gaps)
	}
	b := bundleFor(t, out)
	again, err := (Filter{}).Refilter(t.Context(), b, time.Time{})
	if err != nil || !slices.Equal(again.Gaps, out.Gaps) {
		t.Fatalf("refilter lost the dropped-member gap: %v", err)
	}
}

func TestOmissionRowsAreClosed(t *testing.T) {
	t.Parallel()
	raw, _ := withRecordBeforeEnd(t, "session.invented_future.observed", map[string]any{"kind": "invented_future"})
	out, err := filterBytes(t, raw, agentapi.ReadLimits{})
	if err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(map[string]any){
		"live-only marker": func(m map[string]any) { m["marker"] = "omitted_live_only" },
		"other class":      func(m map[string]any) { object(m["omitted_record"])["omission_class"] = "task_tool_delta_v1" },
		"family carried": func(m map[string]any) {
			object(m["omitted_record"])["payload_type"] = "session.invented_future.observed"
		},
	} {
		b := bundleFor(t, out)
		changed := 0
		for i, m := range b.NativeRecords {
			if m["marker"] == unsupportedMarker {
				change(b.NativeRecords[i])
				changed++
			}
		}
		if changed != 1 {
			t.Fatal("synthetic omission row missing")
		}
		if _, err := (Parser{}).Parse(t.Context(), b); err == nil {
			t.Fatalf("%s: altered omission row parsed", name)
		}
	}
	// Native input cannot write an adapter omission row of its own.
	native := []byte(`{"retained_marker":"` + unsupportedMarker + `","schema_version":1,"stream":{"kind":"session","id":"00000000-0000-7000-8000-000000000001"},"position":{"id":"00000000-0000-4000-8000-0000000000e3","sequence":9},"omitted_record":{"record_type":"event","durability":"durable","payload_type":"","payload_schema_version":0,"payload_kind":"","omission_class":"unsupported_record_v1"}}`)
	if _, err := marker(native, position{Line: 8}); !errors.Is(err, errFormat) {
		t.Fatal("native marker claimed an adapter omission")
	}
}

func TestUnsupportedRecordsGolden(t *testing.T) {
	t.Parallel()
	raw := readSynthetic(t, "synthetic-unsupported-records.jsonl")
	out, err := filterBytes(t, raw, agentapi.ReadLimits{})
	if err != nil {
		t.Fatal(err)
	}
	retained := append(bytes.Join(out.Records, []byte("\n")), '\n')
	if bytes.Contains(retained, []byte("invented")) {
		t.Fatal("unsupported member or record retained")
	}
	golden.Check(t, filepath.Join("testdata", "synthetic-unsupported-records.golden.jsonl"), retained)
}
