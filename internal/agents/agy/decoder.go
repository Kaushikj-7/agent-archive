package agy

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
)

// HookDecoder interprets Antigravity lifecycle payloads into archive lifecycle events.
type HookDecoder struct{}

var (
	_ agentapi.HookDecoder           = HookDecoder{}
	_ agentapi.HookDiagnosticDecoder = HookDecoder{}
	_ agentapi.LegacyHookDecoder     = HookDecoder{}
)

// Decoder returns an Antigravity hook decoder.
func Decoder() HookDecoder {
	return HookDecoder{}
}

func firstString(payload map[string]any, keys ...string) string {
	if payload == nil {
		return ""
	}
	for _, key := range keys {
		if v, ok := payload[key].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

func projectRoot(payload map[string]any) string {
	if payload == nil {
		return ""
	}
	if paths, ok := payload["workspacePaths"].([]any); ok {
		for _, v := range paths {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
	}
	if paths, ok := payload["workspacePaths"].([]string); ok {
		for _, s := range paths {
			if s != "" {
				return s
			}
		}
	}
	if roots, ok := payload["workspace_roots"].([]any); ok {
		for _, v := range roots {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
	}
	if roots, ok := payload["workspace_roots"].([]string); ok {
		for _, s := range roots {
			if s != "" {
				return s
			}
		}
	}
	if cwd := firstString(payload, "cwd"); cwd != "" {
		return cwd
	}
	return ""
}

func isResumedSession(payload map[string]any) bool {
	if payload == nil {
		return false
	}
	if b, ok := payload["isResume"].(bool); ok && b {
		return true
	}
	if s, ok := payload["source"].(string); ok && strings.EqualFold(strings.TrimSpace(s), "resume") {
		return true
	}
	switch v := payload["invocationNum"].(type) {
	case float64:
		if v > 0 {
			return true
		}
	case float32:
		if v > 0 {
			return true
		}
	case int:
		if v > 0 {
			return true
		}
	case int64:
		if v > 0 {
			return true
		}
	case int32:
		if v > 0 {
			return true
		}
	case json.Number:
		if n, err := v.Int64(); err == nil && n > 0 {
			return true
		}
		if f, err := v.Float64(); err == nil && f > 0 {
			return true
		}
	}
	return false
}

// DiagnosticProject interprets project root facts independently of event decoding.
func (HookDecoder) DiagnosticProject(input agentapi.HookInput) string {
	return projectRoot(input.Payload)
}

func (d HookDecoder) evidence(kind archive.SupplementalEvidenceKind, name, provenance string, payload map[string]any, observedAt time.Time) archive.SupplementalEvidence {
	if observedAt.IsZero() {
		observedAt = time.Now().UTC()
	}
	out := map[string]any{"event_name": name}
	if model := firstString(payload, "modelName", "model"); model != "" {
		out["model"] = model
	}
	for _, key := range []string{"message_id", "turn_id", "agent_id"} {
		if v := firstString(payload, key); v != "" {
			out[key] = v
		}
	}
	if kind == archive.EvidenceKindFinalResponse {
		if text := firstString(payload, "last_assistant_message", "text"); text != "" {
			out["text"] = text
		}
	}
	return archive.SupplementalEvidence{
		Kind:       kind,
		ObservedAt: observedAt,
		Provenance: provenance,
		Payload:    out,
	}
}

// Decode interprets Antigravity lifecycle events.
func (d HookDecoder) Decode(ctx context.Context, input agentapi.HookInput) ([]agentapi.LifecycleEvent, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	payload := input.Payload
	if payload == nil {
		return nil, nil
	}

	name := firstString(payload, "hook_event_name", "event", "hookEventName")
	var kind agentapi.EventKind
	switch strings.ToLower(name) {
	case "sessionstart":
		kind = agentapi.EventStart
	case "preinvocation":
		kind = agentapi.EventTurnStart
	case "postinvocation":
		kind = agentapi.EventResponse
	case "stop":
		kind = agentapi.EventStop
	default:
		return nil, nil
	}

	id := firstString(payload, "conversationId", "conversation_id", "session_id", "sessionId")
	if id == "" {
		return nil, errors.New("hook identity unavailable")
	}

	transcriptPath := firstString(payload, "transcriptPath", "transcript_path")
	root := projectRoot(payload)
	reason := strings.ToLower(name)
	provenance := "hook:" + string(agentmeta.Agy) + ":" + reason

	session := agentapi.NativeSession{
		Agent:    agentmeta.Agy,
		NativeID: id,
	}
	source := agentapi.SourceRef{Path: transcriptPath}

	event := agentapi.LifecycleEvent{
		Kind:        kind,
		Session:     session,
		ProjectRoot: root,
		Reason:      reason,
		NativeEvent: name,
		Source:      source,
	}

	switch kind {
	case agentapi.EventStart:
		event.Locator = agentapi.LocatorReplaceFile
		event.Deferred = agentapi.DeferredStart
		if isResumedSession(payload) {
			event.Start = agentapi.StartEvidence{
				Kind:   agentapi.FreshContinuation,
				Reason: agentapi.FreshnessContinuation,
			}
		} else {
			event.Start = agentapi.StartEvidence{
				Kind:   agentapi.FreshExplicit,
				Reason: agentapi.FreshnessExplicitStart,
			}
		}
		event.Evidence = []archive.SupplementalEvidence{
			d.evidence(archive.EvidenceKindLifecycleHook, name, provenance, payload, input.ObservedAt),
		}

	case agentapi.EventTurnStart:
		event.Evidence = []archive.SupplementalEvidence{
			d.evidence(archive.EvidenceKindLifecycleHook, name, provenance, payload, input.ObservedAt),
		}

	case agentapi.EventResponse:
		event.Evidence = []archive.SupplementalEvidence{
			d.evidence(archive.EvidenceKindFinalResponse, name, provenance, payload, input.ObservedAt),
			d.evidence(archive.EvidenceKindLifecycleHook, name, provenance, payload, input.ObservedAt),
		}

	case agentapi.EventStop:
		event.Evidence = []archive.SupplementalEvidence{
			d.evidence(archive.EvidenceKindLifecycleHook, name, provenance, payload, input.ObservedAt),
		}
	}

	return []agentapi.LifecycleEvent{event}, nil
}

// DecodeLegacy translates old admission intent records into lifecycle events without re-inspecting sources.
func (d HookDecoder) DecodeLegacy(old agentapi.AdmissionIntent) ([]agentapi.LifecycleEvent, error) {
	payload := map[string]any{
		"hook_event_name": old.Event,
		"conversationId":  old.NativeSessionID,
		"cwd":             old.ProjectRoot,
		"transcriptPath":  old.TranscriptPath,
	}
	events, err := d.Decode(context.Background(), agentapi.HookInput{Payload: payload, ObservedAt: old.ObservedAt})
	if err != nil {
		return nil, err
	}
	var retained []agentapi.LifecycleEvent
	for _, event := range events {
		switch event.Deferred {
		case agentapi.DeferredStart:
			event.Start = agentapi.StartEvidence{Kind: agentapi.FreshExplicit, Reason: agentapi.FreshnessRetainedProof}
			retained = append(retained, event)
		case agentapi.DeferredFollowup:
			retained = append(retained, event)
		case agentapi.DeferredNone:
		}
	}
	return retained, nil
}
