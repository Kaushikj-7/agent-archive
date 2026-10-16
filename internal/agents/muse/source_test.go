package muse

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/testutil/golden"
)

func readSynthetic(t *testing.T, name string) []byte {
	t.Helper()
	raw, e := os.ReadFile(filepath.Join("testdata", name))
	if e != nil {
		t.Fatal(e)
	}
	return raw
}

func filterBytes(t *testing.T, raw []byte, limits agentapi.ReadLimits) (archive.FilteredTranscript, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "misleading-name.jsonl")
	if e := os.WriteFile(path, raw, 0600); e != nil {
		t.Fatal(e)
	}
	defer func() {
		if err := os.Remove(path); err != nil {
			t.Error(err)
		}
	}()
	pass, e := (SourceProvider{}).OpenPass(t.Context(), agentapi.SourceEnvironment{})
	if e != nil {
		t.Fatal(e)
	}
	defer func() {
		if e := pass.Close(); e != nil {
			t.Error(e)
		}
	}()
	snap, e := pass.Read(t.Context(), agentapi.SourceRef{Path: path}, limits)
	if e != nil {
		return archive.FilteredTranscript{}, e
	}
	defer func() {
		if e := snap.Close(); e != nil {
			t.Error(e)
		}
	}()
	return (Filter{}).Filter(t.Context(), snap.Input(), agentapi.FilterContext{Limits: limits, StartedAt: time.Now()})
}

func bundleFor(t *testing.T, out archive.FilteredTranscript) archive.SourceBundle {
	t.Helper()
	reg := archive.SessionRegistration{ArchiveSessionID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", NativeSessionID: out.SessionIDs[0], ProjectID: strings.Repeat("b", 64), ProjectRoot: "/work/synthetic-muse", Harness: archive.Harness{Name: "muse"}, TranscriptPath: "/unavailable/native.jsonl", SessionStartedAt: time.Unix(2000000000, 0)}
	b, e := archive.NewSourceBundle(reg, Filter{}, out, time.Unix(2000000100, 0), nil)
	if e != nil {
		t.Fatal(e)
	}
	raw, e := json.Marshal(b)
	if e != nil {
		t.Fatal(e)
	}
	if e = json.Unmarshal(raw, &b); e != nil {
		t.Fatal(e)
	}
	return b
}

func TestSyntheticOrdinaryRootsRetainUsefulConversation(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"synthetic-exec-empty", "synthetic-exec-conversation", "synthetic-msp-empty", "synthetic-msp-conversation"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			out, e := filterBytes(t, readSynthetic(t, name+".jsonl"), agentapi.ReadLimits{})
			if e != nil {
				t.Fatal(e)
			}
			if !out.NativeStartAt.Equal(time.UnixMicro(1792083855882704)) || !out.NativeStartComplete {
				t.Fatalf("wrong reserved start: %+v", out)
			}
			b := bundleFor(t, out)
			a, e := (Parser{}).Parse(t.Context(), b)
			if e != nil {
				t.Fatal(e)
			}
			count := 0
			if strings.HasSuffix(name, "conversation") {
				count = 2
			}
			if len(a.View.Turns) != count || a.View.Tokens.Input != nil || a.Observability.ToolResults.Available() || a.Facts.WorkspaceRoot != "/work/synthetic-muse" {
				t.Fatalf("analysis %+v", a)
			}
			safe, e := (Filter{}).Refilter(t.Context(), b, time.Time{})
			if e != nil {
				t.Fatal(e)
			}
			if !reflect.DeepEqual(out.Records, safe.Records) {
				t.Fatal("refilter changed retained records")
			}
			if !(Filter{}).EvidenceExtends(b, b) {
				t.Fatal("valid equality refused")
			}
			golden.Check(t, filepath.Join("testdata", name+".golden.jsonl"), append(bytes.Join(out.Records, []byte("\n")), '\n'))
			h, e := archive.BuildHandoffWithAnalysis(b, a, nil, archive.HandoffOptions{StartedAt: time.Unix(2000000000, 0)})
			if e != nil {
				t.Fatal(e)
			}
			if h.Session.StartedAt == nil || !h.Session.StartedAt.Equal(out.NativeStartAt) {
				t.Fatalf("handoff start: %+v", h.Session)
			}
		})
	}
}

func TestNativeAuthorityAndFramingRefusals(t *testing.T) {
	t.Parallel()
	raw := readSynthetic(t, "synthetic-exec-conversation.jsonl")
	cases := map[string][]byte{
		"missing newline":            bytes.TrimSuffix(raw, []byte("\n")),
		"unknown version":            bytes.Replace(raw, []byte(`"frame_schema_version":1`), []byte(`"frame_schema_version":2`), 1),
		"wrong ordinal":              bytes.Replace(raw, []byte(`"outer_log_ordinal":1`), []byte(`"outer_log_ordinal":2`), 1),
		"wrong hash":                 bytes.Replace(raw, []byte(`"content_sha256":"sha256:`), []byte(`"content_sha256":"sha256:0`), 1),
		"duplicate key":              append([]byte(`{"retained_frame":"session_permission_transaction",`), raw[1:]...),
		"unknown authority":          bytes.Replace(raw, []byte(`"payload_type":"runtime.session.metadata"`), []byte(`"payload_type":"runtime.session.future"`), 1),
		"fork":                       append([]byte("{\"payload_type\":\"session.fork.created\"}\n"), raw...),
		"contradictory opening":      bytes.Replace(raw, []byte(`"resume":false`), []byte(`"resume":true`), 1),
		"bad join":                   bytes.Replace(raw, []byte(`"run_started_source_record_id":"44444444-4444-4444-8444-444444444444"`), []byte(`"run_started_source_record_id":"bad-join"`), 1),
		"foreign owner":              bytes.Replace(raw, []byte(`"source_session_id":"00000000-0000-7000-8000-000000000001"`), []byte(`"source_session_id":"99999999-9999-4999-8999-999999999999"`), 1),
		"reused typed source record": bytes.Replace(raw, []byte(`"source_run_record_id":"66666666-6666-4666-8666-666666666666"`), []byte(`"source_run_record_id":"44444444-4444-4444-8444-444444444444"`), 1),
		"invalid UTF8":               append(append([]byte{}, raw[:len(raw)-1]...), 0xff, '\n'),
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if out, e := filterBytes(t, input, agentapi.ReadLimits{}); e == nil || len(out.Records) > 0 {
				t.Fatalf("accepted malformed input: %v", e)
			}
		})
	}
	for _, limit := range []agentapi.ReadLimits{{RawBytes: 1}, {RecordBytes: 1}, {Records: 2}, {FilteredBytes: 1}} {
		if _, e := filterBytes(t, raw, limit); e == nil {
			t.Fatalf("ignored limit %+v", limit)
		}
	}
}

func TestRetainedSchemaIdentityAndExtensionRefusals(t *testing.T) {
	t.Parallel()
	out, e := filterBytes(t, readSynthetic(t, "synthetic-exec-conversation.jsonl"), agentapi.ReadLimits{})
	if e != nil {
		t.Fatal(e)
	}
	b := bundleFor(t, out)
	for _, mutate := range []func(*archive.SourceBundle){
		func(b *archive.SourceBundle) { b.NativeRecords[0]["v"] = 2 },
		func(b *archive.SourceBundle) { b.NativeRecords[0]["raw_record_json"] = "private" },
		func(b *archive.SourceBundle) { b.NativeRecords[3]["seq"] = 1 },
		func(b *archive.SourceBundle) { b.NativeRecords[3]["ts_us"] = float64(1 << 53) },
		func(b *archive.SourceBundle) { b.NativeRecords[3]["pos"].(map[string]any)["child"] = 0 },
		func(b *archive.SourceBundle) { b.NativeRecords[3]["fields"].(map[string]any)["session_id"] = "other" },
	} {
		raw, _ := json.Marshal(b)
		var changed archive.SourceBundle
		if json.Unmarshal(raw, &changed) != nil {
			t.Fatal("clone")
		}
		mutate(&changed)
		if _, e := (Filter{}).Refilter(t.Context(), changed, time.Time{}); e == nil {
			t.Fatal("invalid retained accepted")
		}
		if (Filter{}).EvidenceExtends(b, changed) {
			t.Fatal("changed evidence extends")
		}
	}
}

func TestSharedPrivacyAndTypedOrigin(t *testing.T) {
	t.Parallel()
	raw := readSynthetic(t, "synthetic-exec-conversation.jsonl")
	raw = bytes.Replace(raw, []byte("Explain the synthetic parser."), []byte("API key: sk-proj-abcdefghijklmnopqrstuvwxyz1234567890"), 1)
	out, e := filterBytes(t, raw, agentapi.ReadLimits{})
	if e != nil {
		t.Fatal(e)
	}
	if bytes.Contains(bytes.Join(out.Records, nil), []byte("abcdefghijklmnopqrstuvwxyz1234567890")) || bytes.Contains(bytes.Join(out.Records, nil), []byte("PRIVATE CONTEXT")) {
		t.Fatal("private body retained")
	}
	for _, pair := range [][2]string{{`"surface":"main"`, `"surface":"notification"`}, {`"kind":"chat"`, `"kind":"agent"`}} {
		input := bytes.Replace(readSynthetic(t, "synthetic-exec-conversation.jsonl"), []byte(pair[0]), []byte(pair[1]), 1)
		if out, e := filterBytes(t, input, agentapi.ReadLimits{}); e == nil || len(out.Records) != 0 {
			t.Fatalf("unsupported origin produced a successful artifact: %v", e)
		}
	}
}

func TestCaptureRefusedBeforeNativeReadAndCancelled(t *testing.T) {
	t.Parallel()
	for _, env := range []agentapi.SourceEnvironment{{RequireConfinedHistory: true}, {LegacyUnboundRegistration: true}, {NativeRoot: &archive.NativeRoot{}}} {
		if p, e := (SourceProvider{}).OpenPass(t.Context(), env); e == nil || p != nil {
			t.Fatal("capture context accepted")
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, e := (SourceProvider{}).OpenPass(ctx, agentapi.SourceEnvironment{}); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
}

func TestTinyBudgetRefusesAndReleases(t *testing.T) {
	t.Parallel()
	budget := agentapi.NewNativeReadBudget(1)
	out, release, e := (Filter{}).RefilterLeased(t.Context(), archive.SourceBundle{Capture: archive.SourceCapture{Harness: archive.Harness{Name: "muse"}, SourceFormat: "muse-jsonl"}, NativeRecords: []map[string]any{{"v": 1}}}, time.Time{}, budget)
	release()
	if e == nil || len(out.Records) != 0 {
		t.Fatal("tiny refilter budget accepted")
	}
	if !budget.Reservation().Reserve(1) {
		t.Fatal("budget leaked")
	}
}

func TestStrictJSONStringsKeysDepthAndNumbers(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{`{"id":"\ud800"}`, `{"\udc00":1}`, `{"a":{"b":1,"b":2}}`, `{"n":9007199254740992}`, `{"n":1.0}`, `{"a":"\ud800x"}`, `{"a":"\udc00\ud800"}`, strings.Repeat(`{"a":`, maxDepth+2) + `1` + strings.Repeat(`}`, maxDepth+2)} {
		if _, _, e := decodeObject([]byte(raw)); e == nil {
			t.Fatalf("ambiguous JSON accepted: %s", raw)
		}
	}
	if _, _, e := decodeObject([]byte(`{"encoding_only":"\ud83d\ude00"}`)); e != nil {
		t.Fatal("paired Unicode encoding rejected")
	}
	// An escaped surrogate nested inside record_json is checked again after
	// the outer string has been decoded, before hashing or child identity use.
	input := bytes.Replace(readSynthetic(t, "synthetic-exec-empty.jsonl"), []byte(`Invented bounded fixture`), []byte(`\ud800`), 1)
	if _, e := filterBytes(t, input, agentapi.ReadLimits{}); e == nil {
		t.Fatal("invalid decoded child string accepted")
	}
	out, e := filterBytes(t, readSynthetic(t, "synthetic-exec-empty.jsonl"), agentapi.ReadLimits{})
	if e != nil {
		t.Fatal(e)
	}
	b := bundleFor(t, out)
	b.NativeRecords[0]["id"] = "\xff"
	if _, e := (Parser{}).Parse(t.Context(), b); e == nil {
		t.Fatal("invalid retained identity normalized")
	}
}

func TestRetainedExtensionNeedsUnchangedPrefixAndNativeLoss(t *testing.T) {
	t.Parallel()
	empty, e := filterBytes(t, readSynthetic(t, "synthetic-exec-empty.jsonl"), agentapi.ReadLimits{})
	if e != nil {
		t.Fatal(e)
	}
	grown, e := filterBytes(t, readSynthetic(t, "synthetic-exec-conversation.jsonl"), agentapi.ReadLimits{})
	if e != nil {
		t.Fatal(e)
	}
	a, b := bundleFor(t, empty), bundleFor(t, grown)
	if !(Filter{}).EvidenceExtends(a, b) || (Filter{}).EvidenceExtends(b, a) {
		t.Fatal("prefix extension reversed")
	}
	// bundleFor points at an absent native path. Refilter and Parse still derive
	// the complete original own conversation through actual serialized maps.
	out, e := (Filter{}).Refilter(t.Context(), b, time.Time{})
	if e != nil {
		t.Fatal(e)
	}
	analysis, e := (Parser{}).Parse(t.Context(), bundleFor(t, out))
	if e != nil || len(analysis.View.Turns) != 2 {
		t.Fatalf("native-loss analysis: %+v %v", analysis, e)
	}
	b.NativeRecords[5]["fields"].(map[string]any)["prompt"] = "An edited visible suffix"
	if !(Filter{}).EvidenceExtends(a, b) {
		t.Fatal("unchanged initial prefix refused")
	}
	if (Filter{}).EvidenceExtends(bundleFor(t, grown), b) {
		t.Fatal("edited suffix equality accepted")
	}
}

func TestLocalResumesPreserveOriginalOwnerStartAndWorkspace(t *testing.T) {
	t.Parallel()
	for _, lane := range []string{"exec", "msp"} {
		t.Run(lane, func(t *testing.T) {
			t.Parallel()
			raw := readSynthetic(t, "synthetic-"+lane+"-empty.jsonl")
			lines := bytes.Split(bytes.TrimSpace(raw), []byte("\n"))
			var last map[string]any
			if e := json.Unmarshal(lines[len(lines)-1], &last); e != nil {
				t.Fatal(e)
			}
			seq := int64(last["sequence"].(float64))
			owner := "00000000-0000-7000-8000-000000000001"
			command := "77777777-7777-4777-8777-777777777777"
			appendRow := func(typ string, version int, payload map[string]any, cause any) {
				seq++
				m := map[string]any{"schema_version": 1, "id": fmt.Sprintf("88888888-8888-4888-8888-%012d", seq), "stream": map[string]any{"kind": "session", "id": owner}, "sequence": seq, "recorded_at": int64(1792083855882704) + seq*10000000, "record_type": "event", "durability": "durable", "causation_id": cause, "payload_type": typ, "payload_schema_version": version, "payload": payload}
				encoded, e := json.Marshal(m)
				if e != nil {
					t.Fatal(e)
				}
				raw = append(raw, append(encoded, '\n')...)
			}
			metadata := map[string]any{"workspace_root": "/work/synthetic-muse", "provider_id": "synthetic", "model_id": "synthetic-model", "tool_surface_version": "2"}
			if lane == "exec" {
				appendRow("session.opened.observed", 1, map[string]any{"kind": "session_opened", "record": map[string]any{"schema_version": 1, "session_id": owner, "resume": true}}, nil)
			} else {
				st := map[string]any{"kind": "session", "id": owner}
				appendRow("runtime.command_intake.received", 2, map[string]any{"kind": "command_intake", "record": map[string]any{"kind": "received", "command_id": command, "session_stream": st, "command": map[string]any{"kind": "session_start", "background_tasks": "keep", "session_metadata": metadata}}}, command)
				appendRow("runtime.session_start.intake_v3", 3, map[string]any{"kind": "session_start_intake_v3", "command_id": command, "command_payload_fingerprint": "opaque-synthetic", "payload": map[string]any{"schema_version": 3, "session_stream": st, "background_tasks": "keep", "session_metadata": metadata, "activation_source": "resume"}}, command)
				appendRow("session.started", 1, map[string]any{"kind": "session_started", "command_id": command, "previous_session_stream": nil, "background_tasks": "keep"}, command)
				appendRow("runtime.session.metadata", 1, map[string]any{"kind": "metadata", "record": metadata}, nil)
				appendRow("session.resumed", 1, map[string]any{"kind": "session_resumed", "record": map[string]any{"schema_version": 1, "session_id": owner}}, nil)
			}
			out, e := filterBytes(t, raw, agentapi.ReadLimits{})
			if e != nil || len(out.SessionIDs) != 1 || out.SessionIDs[0] != owner || !out.NativeStartAt.Equal(time.UnixMicro(1792083855882704)) {
				t.Fatalf("resume: %+v %v", out, e)
			}
			// Later own metadata cannot replace the original project binding.
			metadata["workspace_root"] = "/work/different"
			appendRow("runtime.session.metadata", 1, map[string]any{"kind": "metadata", "record": metadata}, nil)
			if _, e := filterBytes(t, raw, agentapi.ReadLimits{}); e == nil {
				t.Fatal("resume moved workspace")
			}
		})
	}
}

func TestLeasesReleaseOnSuccessFailureAndCancellation(t *testing.T) {
	t.Parallel()
	raw := readSynthetic(t, "synthetic-exec-conversation.jsonl")
	for _, mode := range []string{"success", "failure", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			input := raw
			if mode == "failure" {
				input = bytes.TrimSuffix(raw, []byte("\n"))
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if mode == "cancel" {
				cancel()
			}
			budget := agentapi.NewNativeReadBudget(8 << 20)
			end := budget.BeginScope(8 << 20)
			_, release, e := (Filter{}).FilterLeased(ctx, agentapi.NativeInput{File: memoryInput{bytes.NewReader(input)}}, agentapi.FilterContext{}, budget)
			if mode == "success" && e != nil {
				t.Fatal(e)
			}
			if mode != "success" && e == nil {
				t.Fatal("expected refusal")
			}
			end()
			release()
			release()
			if used, _ := budget.Charged(); used != 0 {
				t.Fatalf("lease leaked %d bytes", used)
			}
		})
	}
}

func TestIncompleteOwnGraphRefusesAndActiveQualifiedPromptIsExplicit(t *testing.T) {
	t.Parallel()
	raw := readSynthetic(t, "synthetic-exec-conversation.jsonl")
	lines := bytes.Split(bytes.TrimSpace(raw), []byte("\n"))
	for _, count := range []int{4, 5} { // accepted only, then accepted+started
		input := append(bytes.Join(lines[:count], []byte("\n")), '\n')
		if out, e := filterBytes(t, input, agentapi.ReadLimits{}); e == nil || len(out.Records) != 0 {
			t.Fatal("unresolved intent/run became zero-prompt success")
		}
	}
	input := append(bytes.Join(lines[:6], []byte("\n")), '\n')
	out, e := filterBytes(t, input, agentapi.ReadLimits{})
	if e != nil {
		t.Fatal(e)
	}
	a, e := (Parser{}).Parse(t.Context(), bundleFor(t, out))
	if e != nil || len(a.View.Turns) != 1 || a.View.Turns[0].Kind != archive.TurnKindHumanPrompt {
		t.Fatalf("active prompt: %+v %v", a, e)
	}
	found := false
	for _, gap := range out.Gaps {
		found = found || gap.Code == "muse_response_pending"
	}
	if !found {
		t.Fatal("pending response availability missing")
	}
}

func TestTaskStartsAndTerminalCopiesAreNotVisibleConversation(t *testing.T) {
	t.Parallel()
	raw := readSynthetic(t, "synthetic-exec-empty.jsonl")
	appendRow := func(payload map[string]any, typ string, seq int) {
		row := map[string]any{"schema_version": 1, "id": fmt.Sprintf("99999999-9999-4999-8999-%012d", seq), "stream": map[string]any{"kind": "session", "id": "00000000-0000-7000-8000-000000000001"}, "sequence": seq, "recorded_at": int64(1792083855882704) + int64(seq)*1000000, "record_type": "event", "durability": "durable", "causation_id": nil, "payload_type": typ, "payload_schema_version": 1, "payload": payload}
		encoded, e := json.Marshal(row)
		if e != nil {
			t.Fatal(e)
		}
		raw = append(raw, append(encoded, '\n')...)
	}
	appendRow(map[string]any{"kind": "task", "run_id": "synthetic-task-run", "task_id": "synthetic-task", "event": map[string]any{"kind": "started", "task_id": "synthetic-task"}}, "runtime.session", 5)
	appendRow(map[string]any{"task_id": "synthetic-task", "event": map[string]any{"kind": "started", "task_id": "synthetic-task"}}, "runtime.session.task", 6)
	appendRow(map[string]any{"kind": "run", "run_id": "synthetic-task-run", "source_run_record_id": "synthetic-terminal", "source_run_record_sequence": 1, "event": map[string]any{"kind": "terminal", "text": "A terminal echo must not become a second assistant message"}}, "runtime.session", 7)
	out, e := filterBytes(t, raw, agentapi.ReadLimits{})
	if e != nil {
		t.Fatal(e)
	}
	a, e := (Parser{}).Parse(t.Context(), bundleFor(t, out))
	if e != nil || len(a.View.Turns) != 0 || bytes.Contains(bytes.Join(out.Records, nil), []byte("terminal echo")) {
		t.Fatalf("task/terminal counted: %+v %v", a, e)
	}
}

func TestRetainedUnknownNullFieldsAndUnsupportedOwnerRefuse(t *testing.T) {
	t.Parallel()
	out, e := filterBytes(t, readSynthetic(t, "synthetic-exec-empty.jsonl"), agentapi.ReadLimits{})
	if e != nil {
		t.Fatal(e)
	}
	base := bundleFor(t, out)
	for _, change := range []func(*archive.SourceBundle){
		func(b *archive.SourceBundle) { b.NativeRecords[0]["fields"].(map[string]any)["future_authority"] = nil },
		func(b *archive.SourceBundle) { b.NativeRecords[0]["pos"].(map[string]any)["future_authority"] = nil },
		func(b *archive.SourceBundle) { b.NativeRecords[0]["stream"].(map[string]any)["future_authority"] = nil },
		func(b *archive.SourceBundle) { b.NativeChild = true },
		func(b *archive.SourceBundle) { b.ParentSessionID = "other-owner" },
	} {
		encoded, _ := json.Marshal(base)
		var b archive.SourceBundle
		if e := json.Unmarshal(encoded, &b); e != nil {
			t.Fatal(e)
		}
		change(&b)
		if _, e := (Filter{}).Refilter(t.Context(), b, time.Time{}); e == nil {
			t.Fatal("unsupported retained authority accepted")
		}
		if _, e := (Parser{}).Parse(t.Context(), b); e == nil {
			t.Fatal("parser accepted unsupported retained authority")
		}
	}
	for _, change := range []func(*archive.SourceBundle){func(b *archive.SourceBundle) { b.ArchiveSessionID = "different-owner" }, func(b *archive.SourceBundle) { b.ProjectID = "different-project" }} {
		b := base
		change(&b)
		if (Filter{}).EvidenceExtends(base, b) {
			t.Fatal("cross-owner prefix accepted")
		}
	}
}

func TestRetainedLogicalStartRequiresPositiveTimestamp(t *testing.T) {
	t.Parallel()
	out, e := filterBytes(t, readSynthetic(t, "synthetic-exec-empty.jsonl"), agentapi.ReadLimits{})
	if e != nil {
		t.Fatal(e)
	}
	for _, timestamp := range []any{nil, 0, -1} {
		b := bundleFor(t, out)
		for i := range 2 {
			b.NativeRecords[i]["ts_us"] = timestamp
		}
		if _, e := (Parser{}).Parse(t.Context(), b); e == nil {
			t.Errorf("parser accepted invalid logical start %v", timestamp)
		}
		if _, e := (Filter{}).Refilter(t.Context(), b, time.Time{}); e == nil {
			t.Errorf("refilter accepted invalid logical start %v", timestamp)
		}
		if (Filter{}).EvidenceExtends(b, b) {
			t.Errorf("invalid logical start %v certified retained equality", timestamp)
		}
	}
}

type countedContext struct {
	context.Context
	calls  int
	stop   int
	cancel context.CancelFunc
}

func (c *countedContext) Err() error {
	c.calls++
	if c.stop > 0 && c.calls == c.stop {
		c.cancel()
	}
	return c.Context.Err()
}

func TestRetainedPrivacyCancellationAndBudgetKinds(t *testing.T) {
	t.Parallel()
	raw := readSynthetic(t, "synthetic-exec-conversation.jsonl")
	out, e := filterBytes(t, raw, agentapi.ReadLimits{})
	if e != nil {
		t.Fatal(e)
	}
	b := bundleFor(t, out)
	baseline := &countedContext{Context: t.Context()}
	if _, e := retainedRows(baseline, b); e != nil {
		t.Fatal(e)
	}
	for _, parser := range []bool{false, true} {
		ctx, cancel := context.WithCancel(t.Context())
		observed := &countedContext{Context: ctx, stop: baseline.calls + 1, cancel: cancel}
		if parser {
			_, e = (Parser{}).Parse(observed, b)
		} else {
			_, e = (Filter{}).Refilter(observed, b, time.Time{})
		}
		cancel()
		if !errors.Is(e, context.Canceled) {
			t.Fatalf("privacy cancellation became success/size failure: %v", e)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	budget := agentapi.NewNativeReadBudget(8 << 20)
	_, release, e := (Filter{}).RefilterLeased(ctx, b, time.Time{}, budget)
	release()
	if !errors.Is(e, context.Canceled) {
		t.Fatalf("cancelled preflight became size failure: %v", e)
	}
	if used, _ := budget.Charged(); used != 0 {
		t.Fatal("cancelled refilter leaked")
	}
	for _, size := range []int64{1, 16384} {
		budget := agentapi.NewNativeReadBudget(size)
		_, release, e := (Filter{}).FilterLeased(t.Context(), agentapi.NativeInput{File: memoryInput{bytes.NewReader(raw)}}, agentapi.FilterContext{}, budget)
		release()
		if !errors.Is(e, agentapi.ErrReadBudget) || errors.Is(e, archive.ErrRecordTooLarge) {
			t.Fatalf("shared pressure misclassified: %v", e)
		}
		if used, _ := budget.Charged(); used != 0 {
			t.Fatal("pressure leaked")
		}
	}
	budget = agentapi.NewNativeReadBudget(1)
	_, release, e = (Filter{}).RefilterLeased(t.Context(), b, time.Time{}, budget)
	release()
	if !errors.Is(e, agentapi.ErrReadBudget) || errors.Is(e, archive.ErrRecordTooLarge) {
		t.Fatalf("retained pressure misclassified: %v", e)
	}
}

func TestOrdinaryRootWithoutStartupModelKeepsIdentityUnknown(t *testing.T) {
	t.Parallel()
	raw := readSynthetic(t, "synthetic-exec-empty.jsonl")
	var metadata map[string]any
	lines := bytes.Split(raw, []byte("\n"))
	if e := json.Unmarshal(lines[1], &metadata); e != nil {
		t.Fatal(e)
	}
	record := metadata["payload"].(map[string]any)["record"].(map[string]any)
	delete(record, "model_id")
	lines[1], _ = json.Marshal(metadata)
	out, e := filterBytes(t, bytes.Join(lines, []byte("\n")), agentapi.ReadLimits{})
	if e != nil {
		t.Fatal(e)
	}
	b := bundleFor(t, out)
	if _, present := b.NativeRecords[2]["fields"].(map[string]any)["model_id"]; present {
		t.Fatal("invented model")
	}
	a, e := (Parser{}).Parse(t.Context(), b)
	if e != nil || !a.Facts.NativeStartedAt.Equal(time.UnixMicro(1792083855882704)) {
		t.Fatalf("logical start lost: %v", e)
	}
	h, e := archive.BuildHandoffWithAnalysis(b, a, nil, archive.HandoffOptions{})
	if e != nil {
		t.Fatal(e)
	}
	rendered, e := json.Marshal(h)
	if e != nil || !bytes.Contains(rendered, []byte("model_identity_unavailable")) {
		t.Fatalf("unknown model not explicit: %v", e)
	}
	if _, e := (Filter{}).Refilter(t.Context(), b, time.Time{}); e != nil {
		t.Fatal(e)
	}
	for _, bad := range []any{nil, 123, false} {
		record["model_id"] = bad
		lines[1], _ = json.Marshal(metadata)
		if out, e := filterBytes(t, bytes.Join(lines, []byte("\n")), agentapi.ReadLimits{}); e == nil || len(out.Records) != 0 {
			t.Fatal("non-string model accepted")
		}
		b.NativeRecords[2]["fields"].(map[string]any)["model_id"] = bad
		if _, e := (Parser{}).Parse(t.Context(), b); e == nil {
			t.Fatal("retained non-string model accepted")
		}
	}
}
