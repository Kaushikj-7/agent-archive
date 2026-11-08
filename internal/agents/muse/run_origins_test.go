package muse

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/testutil/golden"
)

const (
	inboxRun         = "77777777-7777-4777-8777-777777777777"
	inboxSentinel    = "PRIVATE_INBOX_SENTINEL"
	workflowSentinel = "PRIVATE_WORKFLOW_SENTINEL"
)

// originRecords builds, in order, an inbox-delivery origin fact, the run it
// starts with an assistant reply, and a workflow-engine fact.
func originRecords() []map[string]any {
	session := map[string]any{"kind": "session", "id": syntheticOwner}
	record := func(payloadType string, payload map[string]any) map[string]any {
		return map[string]any{"schema_version": 1, "stream": session, "record_type": "event", "durability": "durable", "causation_id": nil, "payload_type": payloadType, "payload_schema_version": 1, "payload": payload}
	}
	fact := func(innerType string, body map[string]any) map[string]any {
		return record(string(payloadRuntimeRetainedFact), map[string]any{"kind": "retained_runtime_fact", "run_id": inboxRun, "record": map[string]any{"payload_type": innerType, "payload_schema_version": 1, "payload": body}})
	}
	run := func(source string, sequence int, event map[string]any) map[string]any {
		return record(string(payloadRuntimeSession), map[string]any{"kind": "run", "run_id": inboxRun, "source_run_record_id": source, "source_run_record_sequence": sequence, "event": event})
	}
	return []map[string]any{
		fact("inbox_delivery.run_origin", map[string]any{"client_id": inboxSentinel}),
		run("88888888-8888-4888-8888-888888888881", 1, map[string]any{"kind": "started", "prompt": inboxSentinel + " delivered text"}),
		run("88888888-8888-4888-8888-888888888882", 2, map[string]any{"kind": "assistant_message_committed", "message_id": "inbox-message", "response_id": "inbox-response", "text": "Handled the delivery."}),
		fact("workflow.capacity_eligible", map[string]any{"root_agent_id": workflowSentinel, "workflow_run_binding": map[string]any{"sha256": workflowSentinel, "byte_length": 1}, "prompt": workflowSentinel}),
	}
}

// withOriginRecords inserts records before the synthetic session end, giving
// each the next sequence and timestamp. change may edit the records first.
func withOriginRecords(t *testing.T, change func([]map[string]any)) []byte {
	t.Helper()
	lines := bytes.Split(bytes.TrimSpace(readSynthetic(t, "synthetic-exec-conversation.jsonl")), []byte("\n"))
	end, _, err := decodeObject(lines[len(lines)-1])
	if err != nil || str(end["payload_type"]) != string(payloadSessionEnd) {
		t.Fatal("synthetic session end changed")
	}
	records := originRecords()
	if change != nil {
		change(records)
	}
	seq, at := num(end["sequence"]), num(end["recorded_at"])
	out := lines[:len(lines)-1]
	for i, m := range records {
		m["id"] = "00000000-0000-4000-8000-0000000000a" + string(rune('0'+i))
		m["sequence"], m["recorded_at"] = seq+int64(i), at+int64(i)
		raw, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, raw)
	}
	end["sequence"], end["recorded_at"] = seq+int64(len(records)), at+int64(len(records))
	raw, err := json.Marshal(end)
	if err != nil {
		t.Fatal(err)
	}
	return append(bytes.Join(append(out, raw), []byte("\n")), '\n')
}

func TestInboxStartedRunKeepsItsReplyButNoPromptAndWorkflowFactsAreOmitted(t *testing.T) {
	t.Parallel()
	raw := withOriginRecords(t, nil)
	// The native input is also a fuzz seed.
	golden.Check(t, filepath.Join("testdata", "synthetic-run-origins.jsonl"), raw)
	out, err := filterBytes(t, raw, agentapi.ReadLimits{})
	if err != nil {
		t.Fatal(err)
	}
	retained := append(bytes.Join(out.Records, []byte("\n")), '\n')
	for _, private := range []string{inboxSentinel, workflowSentinel, "workflow.", "client_id"} {
		if bytes.Contains(retained, []byte(private)) {
			t.Fatalf("retained %q", private)
		}
	}
	golden.Check(t, filepath.Join("testdata", "synthetic-run-origins.golden.jsonl"), retained)
	if !slices.Contains(out.Gaps, archive.CaptureGap{Code: "muse_workflow_facts_omitted", Detail: "Workflow engine facts were left out"}) {
		t.Fatalf("workflow omission has no gap: %+v", out.Gaps)
	}
	assertOmittedWorkflowFact(t, out)
	b := bundleFor(t, out)
	a, err := (Parser{}).Parse(t.Context(), b)
	if err != nil {
		t.Fatal(err)
	}
	users, replies := 0, 0
	for _, turn := range a.View.Turns {
		switch {
		case turn.Role == "user":
			users++
			if bytes.Contains([]byte(turn.Text), []byte(inboxSentinel)) {
				t.Fatal("inbox delivery became a user turn")
			}
		case turn.Text == "Handled the delivery.":
			replies++
		}
	}
	if users != 1 || replies != 1 {
		t.Fatalf("turns: %d user, %d inbox replies", users, replies)
	}
	again, err := (Filter{}).Refilter(t.Context(), b, time.Time{})
	if err != nil || !slices.EqualFunc(again.Records, out.Records, bytes.Equal) || !slices.Equal(again.Gaps, out.Gaps) {
		t.Fatalf("refilter changed the origins: %v", err)
	}
}

func assertOmittedWorkflowFact(t *testing.T, out archive.FilteredTranscript) {
	t.Helper()
	for _, raw := range out.Records {
		row, err := decodeLogical(raw)
		if err != nil {
			t.Fatal(err)
		}
		if row.omission != nil && row.omission.Omitted == workflowFactRecord {
			return
		}
	}
	t.Fatal("workflow fact has no omission row")
}

func TestInboxOriginsThatDoNotQualifyRefuseTheSession(t *testing.T) {
	t.Parallel()
	body := func(rs []map[string]any) map[string]any {
		return object(object(object(rs[0]["payload"])["record"])["payload"])
	}
	for name, change := range map[string]func([]map[string]any){
		"unknown origin member": func(rs []map[string]any) { body(rs)["sender"] = inboxSentinel },
		"client not text":       func(rs []map[string]any) { body(rs)["client_id"] = true },
		"origin after start":    func(rs []map[string]any) { rs[0], rs[1] = rs[1], rs[0] },
		"run never started":     func(rs []map[string]any) { object(rs[0]["payload"])["run_id"] = "99999999-9999-4999-8999-999999999999" },
		"unknown run origin": func(rs []map[string]any) {
			object(object(rs[0]["payload"])["record"])["payload_type"] = "scheduler.run_origin"
		},
	} {
		if out, err := filterBytes(t, withOriginRecords(t, change), agentapi.ReadLimits{}); err == nil || len(out.Records) != 0 {
			t.Errorf("%s: unqualified inbox origin admitted", name)
		}
	}
}

func TestRetainedInboxStartsAreClosed(t *testing.T) {
	t.Parallel()
	out, err := filterBytes(t, withOriginRecords(t, nil), agentapi.ReadLimits{})
	if err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(b *archive.SourceBundle){
		// A prompt on an inbox-started run would be a user turn's text.
		"prompt restored": func(b *archive.SourceBundle) {
			for _, m := range b.NativeRecords {
				f := object(m["fields"])
				if str(f["run_id"]) == inboxRun && str(f["event_kind"]) == string(eventStarted) {
					f["prompt"] = "restored"
				}
			}
		},
		// A user run cannot start without its prompt.
		"origin removed": func(b *archive.SourceBundle) {
			for _, m := range b.NativeRecords {
				f := object(m["fields"])
				if str(f["origin_kind"]) == string(originInboxDelivery) {
					f["run_id"] = "99999999-9999-4999-8999-999999999999"
				}
			}
		},
	} {
		b := bundleFor(t, out)
		change(&b)
		if _, err := (Parser{}).Parse(t.Context(), b); err == nil {
			t.Errorf("%s: altered inbox start parsed", name)
		}
	}
}
