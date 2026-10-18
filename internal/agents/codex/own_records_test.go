package codex

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/codexmeta"
)

type ownRecordJSONField string

const (
	ownRecordJSONFieldExported ownRecordJSONField = "OwnRecords"
	ownRecordJSONFieldSnake    ownRecordJSONField = "own_records"
)

func TestAdmissionOwnRecordsDescribeOnlyValidatedNativeSnapshot(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		own      []map[string]any
		boundary bool
		tail     bool
		want     agentapi.OwnRecordState
	}{
		{name: "inherited_only", boundary: true, want: agentapi.OwnRecordsNone},
		{name: "own_prompt", boundary: true, own: []map[string]any{ownRecordPrompt()}, want: agentapi.OwnRecordsPresent},
		{name: "own_control", boundary: true, own: []map[string]any{{"type": "turn_context", "payload": map[string]any{"model": "synthetic"}}}, want: agentapi.OwnRecordsPresent},
		{name: "unknown_own_record", boundary: true, own: []map[string]any{{"type": "synthetic_future_record", "payload": map[string]any{}}}, want: agentapi.OwnRecordsPresent},
		{name: "missing_native_boundary"},
		{name: "uncommitted_own_tail", boundary: true, tail: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			extra := map[string]any{"parent_thread_id": threadA, "session_id": threadA}
			if test.boundary {
				extra["subagent_history_start_ordinal"] = 3
			}
			ref, _ := historyFile(t, root, threadB, threadB, 0, extra)
			appendHistoryRows(t, ref, 1, ownRecordTask(threadA), ownRecordPrompt())
			appendHistoryRows(t, ref, 3, test.own...)
			if test.tail {
				file, err := os.OpenFile(ref.Path, os.O_APPEND|os.O_WRONLY, 0600)
				if err != nil {
					t.Fatal(err)
				}
				_, err = file.WriteString(`{"type":"response_item","ordinal":3,"payload":{"type":"message","role":"user","content":[]}}`)
				if err = errors.Join(err, file.Close()); err != nil {
					t.Fatal(err)
				}
			}
			lookup := &historyLookup{thread: agentapi.CodexRolloutSet{Current: &ref, Revision: "one"}}
			pass := historyPass(t, root, lookup)
			snapshot, err := pass.Read(t.Context(), ref, agentapi.ReadLimits{})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = snapshot.Close() }()
			evidence, err := snapshot.(agentapi.SourceAdmissionEvidence).AdmissionEvidence(t.Context(), agentapi.SourceAdmission{NativeID: threadB, Cwd: root})
			if err != nil || evidence.OwnRecords != test.want {
				t.Fatalf("own records = %q, want %q: %v", evidence.OwnRecords, test.want, err)
			}
			if test.boundary && evidence.Task.Seen {
				t.Fatal("inherited task became own task evidence", evidence.Task)
			}
			encoded, err := json.Marshal(evidence)
			if err != nil {
				t.Fatal(err)
			}
			var serialized any
			if err := json.Unmarshal(encoded, &serialized); err != nil {
				t.Fatal(err)
			}
			var checkOwnRecordKeys func(any)
			checkOwnRecordKeys = func(value any) {
				switch value := value.(type) {
				case map[string]any:
					for key, child := range value {
						if field := ownRecordJSONField(key); field == ownRecordJSONFieldExported || field == ownRecordJSONFieldSnake {
							t.Errorf("ephemeral own-record field %q entered serialized proof", key)
						}
						checkOwnRecordKeys(child)
					}
				case []any:
					for _, child := range value {
						checkOwnRecordKeys(child)
					}
				}
			}
			checkOwnRecordKeys(serialized)
			if err := snapshot.Close(); err != nil {
				t.Fatal(err)
			}
			if err := pass.Close(); err != nil {
				t.Fatal(err)
			}
			if used, _ := pass.(*relatedSourcePass).env.ReadBudget.Charged(); used != 0 {
				t.Fatal("own-record observation retained source charges", used)
			}
		})
	}
}

func TestAdmissionOwnRecordsRequireCurrentNativeBoundary(t *testing.T) {
	t.Parallel()
	for _, retainedOnly := range []bool{false, true} {
		t.Run(map[bool]string{false: "native", true: "retained_only"}[retainedOnly], func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			extra := map[string]any{"parent_thread_id": threadA, "session_id": threadA}
			if !retainedOnly {
				extra["subagent_history_start_ordinal"] = 3
			}
			ref, _ := historyFile(t, root, threadB, threadB, 0, extra)
			appendHistoryRows(t, ref, 1, ownRecordTask(threadA), ownRecordPrompt())
			lookup := &historyLookup{thread: agentapi.CodexRolloutSet{Current: &ref, Revision: "one"}}
			pass := historyPass(t, root, lookup)
			snapshot, err := pass.Read(t.Context(), ref, agentapi.ReadLimits{})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = snapshot.Close() }()
			binding, err := snapshot.(agentapi.SourceAdmissionFacts).AdmissionFacts(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			boundary := uint64(3)
			binding.Codex.OwnStart = &boundary
			admission := agentapi.SourceAdmission{NativeID: threadB, Binding: &binding}
			reader := snapshot.(agentapi.SourceAdmissionEvidence)
			evidence, err := reader.AdmissionEvidence(t.Context(), admission)
			want := agentapi.OwnRecordsNone
			if retainedOnly {
				want = ""
			}
			if err != nil || evidence.OwnRecords != want || evidence.Task.Seen {
				t.Fatal("native/retained boundary observation changed ownership", evidence, err)
			}
			raw, err := os.ReadFile(ref.Path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(ref.Path, bytes.Replace(raw, []byte("Synthetic record evidence"), []byte("Synthetic changed evidence"), 1), 0600); err != nil {
				t.Fatal(err)
			}
			rewritten, err := reader.AdmissionEvidence(t.Context(), admission)
			if !agentapi.HasFailure(err, agentapi.Changed) || rewritten.OwnRecords != "" {
				t.Fatal("rewritten prefix retained a positive diagnostic", rewritten.OwnRecords, err)
			}
			if err := os.WriteFile(ref.Path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			lookup.changed = true
			changed, err := reader.AdmissionEvidence(t.Context(), admission)
			if !agentapi.HasFailure(err, agentapi.Changed) || changed.OwnRecords != "" {
				t.Fatal("changed current selection retained a positive diagnostic", changed.OwnRecords, err)
			}
			if err := snapshot.Close(); err != nil {
				t.Fatal(err)
			}
			closed, err := reader.AdmissionEvidence(t.Context(), admission)
			if !errors.Is(err, agentapi.ErrClosed) || closed.OwnRecords != "" {
				t.Fatal("closed source retained a positive diagnostic", closed.OwnRecords, err)
			}
			if err := pass.Close(); err != nil {
				t.Fatal(err)
			}
			if used, _ := pass.(*relatedSourcePass).env.ReadBudget.Charged(); used != 0 {
				t.Fatal("invalidated own-record evidence retained source charges", used)
			}
		})
	}
}

func TestAdmissionOwnRecordsCannotBypassRequiredHistoryEvidence(t *testing.T) {
	t.Parallel()
	for _, missingBase := range []bool{false, true} {
		t.Run(map[bool]string{false: "boundary_beyond_end", true: "missing_base"}[missingBase], func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			extra := map[string]any{"parent_thread_id": threadA, "session_id": threadA, "subagent_history_start_ordinal": 4}
			if missingBase {
				extra["history_base"] = codexmeta.CodexHistoryPosition{RolloutID: threadA, EndOrdinal: 3, EndByteOffset: 1}
			}
			start := uint64(0)
			if missingBase {
				start = 3
			}
			ref, _ := historyFile(t, root, threadB, threadB, start, extra)
			if !missingBase {
				appendHistoryRows(t, ref, 1, ownRecordTask(threadA), ownRecordPrompt())
			}
			lookup := &historyLookup{thread: agentapi.CodexRolloutSet{Current: &ref, Revision: "one"}}
			pass := historyPass(t, root, lookup)
			snapshot, err := pass.Read(t.Context(), ref, agentapi.ReadLimits{})
			if err == nil || snapshot != nil {
				t.Fatal("incomplete history acquired diagnostic authority", snapshot, err)
			}
			var refusal *agentapi.SourceError
			if missingBase && (!errors.As(err, &refusal) || refusal.Cause != agentapi.CauseHistoryBaseUnavailable) {
				t.Fatal("required base refusal changed", err)
			}
			if err := pass.Close(); err != nil {
				t.Fatal(err)
			}
			if used, _ := pass.(*relatedSourcePass).env.ReadBudget.Charged(); used != 0 {
				t.Fatal("refused history retained source charges", used)
			}
		})
	}
}

func TestAdmissionOwnRecordObservationIsLimitedToCapturedSnapshot(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	ref, _ := historyFile(t, root, threadB, threadB, 0, map[string]any{"parent_thread_id": threadA, "session_id": threadA, "subagent_history_start_ordinal": 1})
	lookup := &historyLookup{thread: agentapi.CodexRolloutSet{Current: &ref, Revision: "one"}}
	pass := historyPass(t, root, lookup)
	snapshot, err := pass.Read(t.Context(), ref, agentapi.ReadLimits{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = snapshot.Close() }()
	appendHistoryRows(t, ref, 1, ownRecordTask(threadB), ownRecordPrompt())
	admission := agentapi.SourceAdmission{NativeID: threadB, Cwd: root}
	observed, err := snapshot.(agentapi.SourceAdmissionEvidence).AdmissionEvidence(t.Context(), admission)
	if err != nil || observed.OwnRecords != agentapi.OwnRecordsNone || observed.Task.Seen {
		t.Fatal("captured snapshot absorbed a later append", observed, err)
	}
	if err := snapshot.Close(); err != nil {
		t.Fatal(err)
	}
	if err := pass.Close(); err != nil {
		t.Fatal(err)
	}
	if used, _ := pass.(*relatedSourcePass).env.ReadBudget.Charged(); used != 0 {
		t.Fatal("captured observation retained source charges", used)
	}
	fresh := historyPass(t, root, lookup)
	current, err := fresh.Read(t.Context(), ref, agentapi.ReadLimits{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = current.Close() }()
	observed, err = current.(agentapi.SourceAdmissionEvidence).AdmissionEvidence(t.Context(), admission)
	if err != nil || observed.OwnRecords != agentapi.OwnRecordsPresent || !observed.Task.ValidNativeCreation(observed.Binding.Codex.NativeCreatedAt) {
		t.Fatal("fresh snapshot retained a permanent no-record conclusion", observed, err)
	}
	if err := current.Close(); err != nil {
		t.Fatal(err)
	}
	if err := fresh.Close(); err != nil {
		t.Fatal(err)
	}
	if used, _ := fresh.(*relatedSourcePass).env.ReadBudget.Charged(); used != 0 {
		t.Fatal("fresh observation retained source charges", used)
	}
}

func ownRecordTask(id string) map[string]any {
	return map[string]any{"type": "event_msg", "payload": map[string]any{"type": "task_started", "turn_id": id, "started_at": "2026-10-01T12:02:00Z"}}
}

func ownRecordPrompt() map[string]any {
	return map[string]any{"type": "response_item", "payload": map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "Synthetic record evidence"}}}}
}
