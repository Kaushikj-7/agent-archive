package muse

import (
	"bytes"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/testutil/golden"
)

const (
	syntheticChild    = "00000000-0000-7000-8000-0000000000c1"
	syntheticRun      = "33333333-3333-4333-8333-333333333333"
	syntheticTask     = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	childLinkSentinel = "PRIVATE_REMINDER_AGENT_SENTINEL"
)

// childLinkEvent is a synthetic link from the synthetic omissions session's own
// run and task to a child session.
func childLinkEvent(owner string) map[string]any {
	return map[string]any{
		"kind": "memory_reminder_child_session_linked", "child_session_id": syntheticChild, "generation_id": json.Number("0"),
		"parent_run_id": syntheticRun, "parent_session_id": owner, "reminder_agent_id": childLinkSentinel,
		"task_id": syntheticTask, "task_stream": map[string]any{"kind": "task", "id": syntheticTask},
	}
}

// withChildLinks inserts one run record per event before the synthetic
// omissions session's end, renumbering the end so the session stays
// contiguous. change may edit each inserted envelope.
func withChildLinks(t *testing.T, change func(m map[string]any), events ...func(owner string) map[string]any) []byte {
	t.Helper()
	lines := bytes.Split(bytes.TrimSpace(readSynthetic(t, "synthetic-native-omissions.jsonl")), []byte("\n"))
	end, _, err := decodeObject(lines[len(lines)-1])
	if err != nil || str(end["payload_type"]) != string(payloadSessionEnd) {
		t.Fatal("synthetic session end changed")
	}
	owner := str(object(end["stream"])["id"])
	seq, at := num(end["sequence"]), num(end["recorded_at"])
	lines = lines[:len(lines)-1]
	for i, event := range events {
		m := maps.Clone(end)
		m["id"] = "00000000-0000-4000-8000-0000000000d" + string(rune('0'+i))
		m["causation_id"] = nil
		m["sequence"], m["recorded_at"] = seq+int64(i), at+int64(i)
		m["payload_type"], m["payload_schema_version"] = string(payloadRuntimeSession), 1
		m["payload"] = map[string]any{"kind": "run", "run_id": syntheticRun, "source_run_record_id": "00000000-0000-4000-8000-0000000000f" + string(rune('0'+i)), "source_run_record_sequence": 4 + i, "event": event(owner)}
		if change != nil {
			change(m)
		}
		raw, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, raw)
	}
	end["sequence"], end["recorded_at"] = seq+int64(len(events)), at+int64(len(events))
	raw, err := json.Marshal(end)
	if err != nil {
		t.Fatal(err)
	}
	return append(bytes.Join(append(lines, raw), []byte("\n")), '\n')
}

// filterInStore filters raw as session.jsonl in a native session directory,
// creating the given child logs beside it.
func filterInStore(t *testing.T, raw []byte, childLogs ...string) (archive.FilteredTranscript, error) {
	t.Helper()
	dir := t.TempDir()
	for _, rel := range childLogs {
		path := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(dir, "session.jsonl")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	pass, err := (SourceProvider{}).OpenPass(t.Context(), agentapi.SourceEnvironment{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := pass.Close(); err != nil {
			t.Error(err)
		}
	}()
	snap, err := pass.Read(t.Context(), agentapi.SourceRef{Path: path}, agentapi.ReadLimits{})
	if err != nil {
		return archive.FilteredTranscript{}, err
	}
	defer func() {
		if err := snap.Close(); err != nil {
			t.Error(err)
		}
	}()
	return (Filter{}).Filter(t.Context(), snap.Input(), agentapi.FilterContext{StartedAt: time.Now()})
}

func withLogPath(owner string) map[string]any {
	e := childLinkEvent(owner)
	e["child_session_log_path"] = childLogPath(syntheticChild)
	return e
}

func TestVerifiedChildLinkRetainsOnlyTheChildID(t *testing.T) {
	t.Parallel()
	raw := withChildLinks(t, nil, childLinkEvent)
	// The native input is also a fuzz seed.
	golden.Check(t, filepath.Join("testdata", "synthetic-child-link.jsonl"), raw)
	out, err := filterBytes(t, raw, agentapi.ReadLimits{})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(out.NativeChildIDs, []string{syntheticChild}) {
		t.Fatalf("verified child IDs = %v", out.NativeChildIDs)
	}
	retained := append(bytes.Join(out.Records, []byte("\n")), '\n')
	// The fixture's own task records keep their task ID; the link keeps none of its members but the child ID.
	for _, private := range []string{childLinkSentinel, "generation_id", "parent_session_id", "parent_run_id", "task_stream", "reminder_agent_id"} {
		if bytes.Contains(retained, []byte(private)) {
			t.Fatalf("child link retained %q", private)
		}
	}
	golden.Check(t, filepath.Join("testdata", "synthetic-child-link.golden.jsonl"), retained)
	b := bundleFor(t, out)
	if _, err := (Parser{}).Parse(t.Context(), b); err != nil {
		t.Fatal(err)
	}
	again, err := (Filter{}).Refilter(t.Context(), b, time.Time{})
	if err != nil || !slices.EqualFunc(again.Records, out.Records, bytes.Equal) || !slices.Equal(again.NativeChildIDs, out.NativeChildIDs) {
		t.Fatalf("refilter changed the child link: %v", err)
	}
}

func TestChildLinkNamingALogNeedsTheRealChildFile(t *testing.T) {
	t.Parallel()
	raw := withChildLinks(t, nil, withLogPath)
	out, err := filterInStore(t, raw, childLogPath(syntheticChild))
	if err != nil || !slices.Equal(out.NativeChildIDs, []string{syntheticChild}) {
		t.Fatalf("link to an existing child log refused: %v", err)
	}
	if bytes.Contains(bytes.Join(out.Records, nil), []byte("subagent/")) {
		t.Fatal("child log path retained")
	}
	if _, err := filterInStore(t, raw); !errors.Is(err, errFormat) {
		t.Fatalf("link to a missing child log accepted: %v", err)
	}
	// A directory where the log should be is not a child log.
	if _, err := filterInStore(t, raw, childLogPath(syntheticChild)+"/nested"); !errors.Is(err, errFormat) {
		t.Fatalf("directory accepted as a child log: %v", err)
	}
	other := withChildLinks(t, nil, func(owner string) map[string]any {
		e := withLogPath(owner)
		e["child_session_log_path"] = "subagent/00000000-0000-7000-8000-0000000000c2/session.jsonl"
		return e
	})
	if _, err := filterInStore(t, other, "subagent/00000000-0000-7000-8000-0000000000c2/session.jsonl"); !errors.Is(err, errFormat) {
		t.Fatalf("link naming another child's log accepted: %v", err)
	}
	// Input that cannot check child logs cannot verify a named one.
	budget := agentapi.NewNativeReadBudget(agentapi.PassReadBudget)
	_, release, err := (Filter{}).FilterLeased(t.Context(), agentapi.NativeInput{File: memoryInput{bytes.NewReader(raw)}}, agentapi.FilterContext{}, budget)
	release()
	if !errors.Is(err, errFormat) {
		t.Fatalf("unverifiable child log accepted: %v", err)
	}
}

func TestUnverifiedChildLinksRefuseTheSession(t *testing.T) {
	t.Parallel()
	edit := func(change func(e map[string]any)) func(string) map[string]any {
		return func(owner string) map[string]any {
			e := childLinkEvent(owner)
			change(e)
			return e
		}
	}
	other := "00000000-0000-7000-8000-0000000000c9"
	cases := map[string][]byte{
		"another parent": withChildLinks(t, nil, edit(func(e map[string]any) { e["parent_session_id"] = other })),
		"another run":    withChildLinks(t, nil, edit(func(e map[string]any) { e["parent_run_id"] = other })),
		"unknown task": withChildLinks(t, nil, edit(func(e map[string]any) {
			e["task_id"], e["task_stream"] = other, map[string]any{"kind": "task", "id": other}
		})),
		"task stream mismatch": withChildLinks(t, nil, edit(func(e map[string]any) { e["task_stream"] = map[string]any{"kind": "task", "id": other} })),
		"self as child":        withChildLinks(t, nil, edit(func(e map[string]any) { e["child_session_id"] = e["parent_session_id"] })),
		"child not an ID":      withChildLinks(t, nil, edit(func(e map[string]any) { e["child_session_id"] = "child" })),
		"negative generation":  withChildLinks(t, nil, edit(func(e map[string]any) { e["generation_id"] = json.Number("-1") })),
		"empty reminder agent": withChildLinks(t, nil, edit(func(e map[string]any) { e["reminder_agent_id"] = "" })),
		"unknown member":       withChildLinks(t, nil, edit(func(e map[string]any) { e["future"] = true })),
		"missing member":       withChildLinks(t, nil, edit(func(e map[string]any) { delete(e, "generation_id") })),
		"caused":               withChildLinks(t, func(m map[string]any) { m["causation_id"] = "00000000-0000-4000-8000-000000000005" }, childLinkEvent),
		"linked twice":         withChildLinks(t, nil, childLinkEvent, childLinkEvent),
		"run of no turn": withChildLinks(t, func(m map[string]any) {
			object(m["payload"])["run_id"] = other
		}, edit(func(e map[string]any) { e["parent_run_id"] = other })),
	}
	for name, raw := range cases {
		out, err := filterBytes(t, raw, agentapi.ReadLimits{})
		if err == nil || len(out.Records) != 0 {
			t.Errorf("%s: unverified child link was not refused", name)
		}
	}
}

func TestRetainedChildLinksAreClosed(t *testing.T) {
	t.Parallel()
	second := func(owner string) map[string]any {
		e := childLinkEvent(owner)
		e["child_session_id"] = "00000000-0000-7000-8000-0000000000c2"
		return e
	}
	out, err := filterBytes(t, withChildLinks(t, nil, childLinkEvent, second), agentapi.ReadLimits{})
	if err != nil || len(out.NativeChildIDs) != 2 {
		t.Fatal(err)
	}
	for name, change := range map[string]func(b *archive.SourceBundle, i int){
		"self as child": func(b *archive.SourceBundle, i int) {
			object(b.NativeRecords[i]["fields"])["child_session_id"] = str(object(b.NativeRecords[i]["stream"])["id"])
		},
		"extra member": func(b *archive.SourceBundle, i int) { object(b.NativeRecords[i]["fields"])["task_id"] = syntheticTask },
		"linked twice": func(b *archive.SourceBundle, i int) {
			object(b.NativeRecords[i+1]["fields"])["child_session_id"] = syntheticChild
		},
	} {
		b := bundleFor(t, out)
		found := -1
		for i, m := range b.NativeRecords {
			if found < 0 && str(object(m["fields"])["event_kind"]) == string(eventChildSessionLinked) {
				found = i
			}
		}
		if found < 0 {
			t.Fatal("retained child link missing")
		}
		change(&b, found)
		if _, err := (Parser{}).Parse(t.Context(), b); err == nil {
			t.Errorf("%s: altered retained child link parsed", name)
		}
	}
}
