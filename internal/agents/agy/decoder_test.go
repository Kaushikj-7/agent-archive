package agy

import (
	"context"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
)

func TestDecoderRecognizesDocumentedAntigravityPayload(t *testing.T) {
	d := Decoder()
	payload := map[string]any{
		"hook_event_name": "SessionStart",
		"conversationId":  "12345678-1234-4234-8234-123456789abc",
		"transcriptPath":  "/home/user/.gemini/antigravity-cli/brain/12345678-1234-4234-8234-123456789abc/.system_generated/logs/transcript.jsonl",
		"workspacePaths":  []any{"/home/user/project"},
		"modelName":       "gemini-2.5-pro",
		"invocationNum":   float64(0),
	}
	now := time.Now()
	events, err := d.Decode(context.Background(), agentapi.HookInput{Payload: payload, ObservedAt: now})
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if len(events) == 0 {
		t.Fatal("Decode() returned 0 events for documented SessionStart payload")
	}
	ev := events[0]
	if ev.Kind != agentapi.EventStart || ev.Session.NativeID != "12345678-1234-4234-8234-123456789abc" {
		t.Fatalf("unexpected event: %#v", ev)
	}
	if ev.Session.Agent != agentmeta.Agy {
		t.Fatalf("expected agent agy, got %v", ev.Session.Agent)
	}
	if ev.ProjectRoot != "/home/user/project" {
		t.Fatalf("expected project root /home/user/project, got %q", ev.ProjectRoot)
	}
	if ev.Start.Kind != agentapi.FreshExplicit || ev.Start.Reason != agentapi.FreshnessExplicitStart {
		t.Fatalf("expected FreshExplicit for invocationNum 0, got %#v", ev.Start)
	}
	if ev.Deferred != agentapi.DeferredStart {
		t.Fatalf("expected DeferredStart, got %v", ev.Deferred)
	}
	if ev.Locator != agentapi.LocatorReplaceFile {
		t.Fatalf("expected LocatorReplaceFile, got %v", ev.Locator)
	}
	if ev.Source.Path != "/home/user/.gemini/antigravity-cli/brain/12345678-1234-4234-8234-123456789abc/.system_generated/logs/transcript.jsonl" {
		t.Fatalf("unexpected transcript path: %q", ev.Source.Path)
	}
	if len(ev.Evidence) != 1 {
		t.Fatalf("expected 1 evidence item, got %d", len(ev.Evidence))
	}
	if ev.Evidence[0].Kind != archive.EvidenceKindLifecycleHook {
		t.Fatalf("expected EvidenceKindLifecycleHook, got %v", ev.Evidence[0].Kind)
	}
	if ev.Evidence[0].Payload["event_name"] != "SessionStart" {
		t.Fatalf("expected event_name SessionStart, got %v", ev.Evidence[0].Payload["event_name"])
	}
	if ev.Evidence[0].Payload["model"] != "gemini-2.5-pro" {
		t.Fatalf("expected model gemini-2.5-pro, got %v", ev.Evidence[0].Payload["model"])
	}
}

func TestDecoderDistinguishesResumedSession(t *testing.T) {
	d := Decoder()

	// invocationNum as float64 > 0
	payloadFloat := map[string]any{
		"hook_event_name": "SessionStart",
		"conversationId":  "12345678-1234-4234-8234-123456789abc",
		"transcriptPath":  "/path/to/transcript.jsonl",
		"workspacePaths":  []any{"/work"},
		"invocationNum":   float64(3),
	}
	events, err := d.Decode(context.Background(), agentapi.HookInput{Payload: payloadFloat, ObservedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) == 0 {
		t.Fatal("expected at least 1 event")
	}
	if events[0].Start.Kind != agentapi.FreshContinuation || events[0].Start.Reason != agentapi.FreshnessContinuation {
		t.Fatalf("expected FreshContinuation for invocationNum 3, got %#v", events[0].Start)
	}

	// invocationNum as int > 0
	payloadInt := map[string]any{
		"hook_event_name": "SessionStart",
		"conversationId":  "12345678-1234-4234-8234-123456789abc",
		"transcriptPath":  "/path/to/transcript.jsonl",
		"workspacePaths":  []any{"/work"},
		"invocationNum":   int(5),
	}
	events, err = d.Decode(context.Background(), agentapi.HookInput{Payload: payloadInt, ObservedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) == 0 {
		t.Fatal("expected at least 1 event")
	}
	if events[0].Start.Kind != agentapi.FreshContinuation || events[0].Start.Reason != agentapi.FreshnessContinuation {
		t.Fatalf("expected FreshContinuation for invocationNum int(5), got %#v", events[0].Start)
	}

	// isResume == true
	payloadResume := map[string]any{
		"hook_event_name": "SessionStart",
		"conversationId":  "12345678-1234-4234-8234-123456789abc",
		"transcriptPath":  "/path/to/transcript.jsonl",
		"workspacePaths":  []any{"/work"},
		"isResume":        true,
	}
	events, err = d.Decode(context.Background(), agentapi.HookInput{Payload: payloadResume, ObservedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) == 0 {
		t.Fatal("expected at least 1 event")
	}
	if events[0].Start.Kind != agentapi.FreshContinuation || events[0].Start.Reason != agentapi.FreshnessContinuation {
		t.Fatalf("expected FreshContinuation for isResume=true, got %#v", events[0].Start)
	}

	// source == "resume"
	payloadSourceResume := map[string]any{
		"hook_event_name": "SessionStart",
		"conversationId":  "12345678-1234-4234-8234-123456789abc",
		"transcriptPath":  "/path/to/transcript.jsonl",
		"workspacePaths":  []any{"/work"},
		"source":          "resume",
	}
	events, err = d.Decode(context.Background(), agentapi.HookInput{Payload: payloadSourceResume, ObservedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) == 0 {
		t.Fatal("expected at least 1 event")
	}
	if events[0].Start.Kind != agentapi.FreshContinuation || events[0].Start.Reason != agentapi.FreshnessContinuation {
		t.Fatalf("expected FreshContinuation for source=resume, got %#v", events[0].Start)
	}

	// startup / missing invocationNum -> FreshExplicit
	payloadStartup := map[string]any{
		"hook_event_name": "SessionStart",
		"conversationId":  "12345678-1234-4234-8234-123456789abc",
		"transcriptPath":  "/path/to/transcript.jsonl",
		"workspacePaths":  []any{"/work"},
		"source":          "startup",
	}
	events, err = d.Decode(context.Background(), agentapi.HookInput{Payload: payloadStartup, ObservedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) == 0 {
		t.Fatal("expected at least 1 event")
	}
	if events[0].Start.Kind != agentapi.FreshExplicit || events[0].Start.Reason != agentapi.FreshnessExplicitStart {
		t.Fatalf("expected FreshExplicit for source=startup, got %#v", events[0].Start)
	}
}

func TestDecoderPreInvocation(t *testing.T) {
	d := Decoder()
	payload := map[string]any{
		"hook_event_name": "PreInvocation",
		"conversationId":  "session-pre-1",
		"workspacePaths":  []any{"/work/pre"},
		"transcriptPath":  "/work/pre/transcript.jsonl",
		"modelName":       "gemini-2.5-flash",
	}
	events, err := d.Decode(context.Background(), agentapi.HookInput{Payload: payload, ObservedAt: time.Now()})
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	ev := events[0]
	if ev.Kind != agentapi.EventTurnStart {
		t.Fatalf("expected EventTurnStart, got %v", ev.Kind)
	}
	if ev.Reason != "preinvocation" || ev.NativeEvent != "PreInvocation" {
		t.Fatalf("unexpected reason/nativeEvent: %s / %s", ev.Reason, ev.NativeEvent)
	}
	if ev.ProjectRoot != "/work/pre" {
		t.Fatalf("expected project root /work/pre, got %q", ev.ProjectRoot)
	}
	if len(ev.Evidence) != 1 || ev.Evidence[0].Kind != archive.EvidenceKindLifecycleHook {
		t.Fatalf("expected 1 EvidenceKindLifecycleHook, got %#v", ev.Evidence)
	}
	if ev.Evidence[0].Payload["event_name"] != "PreInvocation" {
		t.Fatalf("expected event_name PreInvocation, got %v", ev.Evidence[0].Payload["event_name"])
	}
	if ev.Evidence[0].Payload["model"] != "gemini-2.5-flash" {
		t.Fatalf("expected model gemini-2.5-flash, got %v", ev.Evidence[0].Payload["model"])
	}
}

func TestDecoderPostInvocation(t *testing.T) {
	d := Decoder()
	payload := map[string]any{
		"hook_event_name":        "PostInvocation",
		"conversationId":         "session-post-1",
		"workspacePaths":         []any{"/work/post"},
		"transcriptPath":         "/work/post/transcript.jsonl",
		"modelName":              "gemini-2.5-pro",
		"last_assistant_message": "Completed task.",
	}
	events, err := d.Decode(context.Background(), agentapi.HookInput{Payload: payload, ObservedAt: time.Now()})
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	ev := events[0]
	if ev.Kind != agentapi.EventResponse {
		t.Fatalf("expected EventResponse, got %v", ev.Kind)
	}
	if len(ev.Evidence) != 2 {
		t.Fatalf("expected 2 evidence items for PostInvocation, got %d", len(ev.Evidence))
	}
	hasFinal := false
	hasLifecycle := false
	for _, e := range ev.Evidence {
		if e.Kind == archive.EvidenceKindFinalResponse {
			hasFinal = true
			if e.Payload["text"] != "Completed task." {
				t.Fatalf("expected text 'Completed task.', got %v", e.Payload["text"])
			}
			if e.Payload["model"] != "gemini-2.5-pro" {
				t.Fatalf("expected model gemini-2.5-pro, got %v", e.Payload["model"])
			}
			if e.Payload["event_name"] != "PostInvocation" {
				t.Fatalf("expected event_name PostInvocation, got %v", e.Payload["event_name"])
			}
		}
		if e.Kind == archive.EvidenceKindLifecycleHook {
			hasLifecycle = true
			if e.Payload["model"] != "gemini-2.5-pro" {
				t.Fatalf("expected model gemini-2.5-pro, got %v", e.Payload["model"])
			}
			if e.Payload["event_name"] != "PostInvocation" {
				t.Fatalf("expected event_name PostInvocation, got %v", e.Payload["event_name"])
			}
		}
	}
	if !hasFinal || !hasLifecycle {
		t.Fatalf("missing expected evidence kinds: hasFinal=%v, hasLifecycle=%v", hasFinal, hasLifecycle)
	}
}

func TestDecoderStop(t *testing.T) {
	d := Decoder()
	payload := map[string]any{
		"hook_event_name": "Stop",
		"conversationId":  "session-stop-1",
		"workspacePaths":  []any{"/work/stop"},
		"modelName":       "gemini-2.5-pro",
	}
	events, err := d.Decode(context.Background(), agentapi.HookInput{Payload: payload, ObservedAt: time.Now()})
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	ev := events[0]
	if ev.Kind != agentapi.EventStop {
		t.Fatalf("expected EventStop, got %v", ev.Kind)
	}
	if len(ev.Evidence) != 1 || ev.Evidence[0].Kind != archive.EvidenceKindLifecycleHook {
		t.Fatalf("expected 1 EvidenceKindLifecycleHook, got %#v", ev.Evidence)
	}
	if ev.Evidence[0].Payload["event_name"] != "Stop" {
		t.Fatalf("expected event_name Stop, got %v", ev.Evidence[0].Payload["event_name"])
	}
	if ev.Evidence[0].Payload["model"] != "gemini-2.5-pro" {
		t.Fatalf("expected model gemini-2.5-pro, got %v", ev.Evidence[0].Payload["model"])
	}
}

func TestDecoderDiagnosticProject(t *testing.T) {
	d := Decoder()

	// workspacePaths as []any
	p1 := d.DiagnosticProject(agentapi.HookInput{Payload: map[string]any{"workspacePaths": []any{"/project/workspace"}}})
	if p1 != "/project/workspace" {
		t.Fatalf("got %q, want /project/workspace", p1)
	}

	// workspacePaths as []string
	p2 := d.DiagnosticProject(agentapi.HookInput{Payload: map[string]any{"workspacePaths": []string{"/project/str"}}})
	if p2 != "/project/str" {
		t.Fatalf("got %q, want /project/str", p2)
	}

	// workspace_roots
	p3 := d.DiagnosticProject(agentapi.HookInput{Payload: map[string]any{"workspace_roots": []any{"/project/roots"}}})
	if p3 != "/project/roots" {
		t.Fatalf("got %q, want /project/roots", p3)
	}

	// cwd
	p4 := d.DiagnosticProject(agentapi.HookInput{Payload: map[string]any{"cwd": "/project/cwd"}})
	if p4 != "/project/cwd" {
		t.Fatalf("got %q, want /project/cwd", p4)
	}

	// empty payload
	p5 := d.DiagnosticProject(agentapi.HookInput{Payload: nil})
	if p5 != "" {
		t.Fatalf("got %q, want empty", p5)
	}
}

func TestDecoderAlternateKeysAndMissingIdentity(t *testing.T) {
	d := Decoder()

	// alternate keys: conversation_id, transcript_path, event, model
	payload := map[string]any{
		"event":           "SessionStart",
		"conversation_id": "conv-alt-1",
		"transcript_path": "/path/alt.jsonl",
		"cwd":             "/work/alt",
		"model":           "gemini-2.5-pro",
		"invocationNum":   float64(0),
	}
	events, err := d.Decode(context.Background(), agentapi.HookInput{Payload: payload, ObservedAt: time.Now()})
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Session.NativeID != "conv-alt-1" {
		t.Fatalf("expected conv-alt-1, got %q", events[0].Session.NativeID)
	}
	if events[0].Source.Path != "/path/alt.jsonl" {
		t.Fatalf("expected /path/alt.jsonl, got %q", events[0].Source.Path)
	}
	if events[0].ProjectRoot != "/work/alt" {
		t.Fatalf("expected /work/alt, got %q", events[0].ProjectRoot)
	}
	if events[0].Evidence[0].Payload["model"] != "gemini-2.5-pro" {
		t.Fatalf("expected model gemini-2.5-pro, got %v", events[0].Evidence[0].Payload["model"])
	}

	// alternate keys: session_id, hookEventName
	payload2 := map[string]any{
		"hookEventName": "Stop",
		"session_id":    "session-alt-2",
		"cwd":           "/work/alt2",
	}
	events, err = d.Decode(context.Background(), agentapi.HookInput{Payload: payload2, ObservedAt: time.Now()})
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if len(events) != 1 || events[0].Session.NativeID != "session-alt-2" {
		t.Fatalf("unexpected events: %#v", events)
	}

	// alternate keys: sessionId
	payload3 := map[string]any{
		"hook_event_name": "PreInvocation",
		"sessionId":       "session-alt-3",
	}
	events, err = d.Decode(context.Background(), agentapi.HookInput{Payload: payload3, ObservedAt: time.Now()})
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if len(events) != 1 || events[0].Session.NativeID != "session-alt-3" {
		t.Fatalf("unexpected events: %#v", events)
	}

	// Missing identity should return error
	missingID := map[string]any{
		"hook_event_name": "SessionStart",
		"workspacePaths":  []any{"/work"},
	}
	_, err = d.Decode(context.Background(), agentapi.HookInput{Payload: missingID, ObservedAt: time.Now()})
	if err == nil {
		t.Fatal("expected error for missing identity, got nil")
	}

	// Unknown event returns 0 events and no error
	unknownEvent := map[string]any{
		"hook_event_name": "UnknownEvent",
		"conversationId":  "conv-1",
	}
	events, err = d.Decode(context.Background(), agentapi.HookInput{Payload: unknownEvent, ObservedAt: time.Now()})
	if err != nil {
		t.Fatalf("unexpected error for unknown event: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("expected 0 events for unknown event, got %d", len(events))
	}
}
