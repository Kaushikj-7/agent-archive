package agy

import (
	"strings"

	"github.com/wangjohn/agent-archive/internal/agentapi"
)

// RuntimeDetector observes native session evidence through an injected getter.
type RuntimeDetector struct{}

var _ agentapi.RuntimeDetector = RuntimeDetector{}

var sessionIDKeys = []string{
	"ANTIGRAVITY_SESSION_ID",
	"AGY_SESSION_ID",
	"ANTIGRAVITY_CONVERSATION_ID",
}

var fallbackKeys = []string{
	"ANTIGRAVITY_AGENT",
	"AGY_AGENT",
	"ANTIGRAVITY_CLI",
}

// Detect returns exact identity or native project-presence fallback.
func (RuntimeDetector) Detect(env agentapi.RuntimeEnvironment) agentapi.RuntimeObservation {
	if env.LookupEnv == nil {
		return agentapi.RuntimeObservation{}
	}
	for _, key := range sessionIDKeys {
		val, set := env.LookupEnv(key)
		val = strings.TrimSpace(val)
		if set && val != "" {
			return agentapi.RuntimeObservation{NativeID: val, PresenceKey: key}
		}
	}
	for _, key := range fallbackKeys {
		val, set := env.LookupEnv(key)
		val = strings.TrimSpace(val)
		if set && val != "" {
			return agentapi.RuntimeObservation{PresenceKey: key, ProjectLatest: true}
		}
	}
	return agentapi.RuntimeObservation{}
}

// SessionEnvironmentKeys returns native session variables to strip on launch.
func (RuntimeDetector) SessionEnvironmentKeys() []string {
	return []string{
		"ANTIGRAVITY_SESSION_ID",
		"AGY_SESSION_ID",
		"ANTIGRAVITY_CONVERSATION_ID",
		"ANTIGRAVITY_AGENT",
		"AGY_AGENT",
		"ANTIGRAVITY_CLI",
	}
}
