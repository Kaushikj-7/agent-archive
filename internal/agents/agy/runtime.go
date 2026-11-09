package agy

import (
	"strings"

	"github.com/wangjohn/agent-archive/internal/agentapi"
)

// RuntimeDetector observes native session evidence through an injected getter.
type RuntimeDetector struct{}

var _ agentapi.RuntimeDetector = RuntimeDetector{}

// Detect returns exact identity or native project-presence fallback.
func (RuntimeDetector) Detect(env agentapi.RuntimeEnvironment) agentapi.RuntimeObservation {
	if env.LookupEnv == nil {
		return agentapi.RuntimeObservation{}
	}
	value, set := env.LookupEnv("ANTIGRAVITY_SESSION_ID")
	value = strings.TrimSpace(value)
	if !set || value == "" {
		return agentapi.RuntimeObservation{}
	}
	return agentapi.RuntimeObservation{NativeID: value, PresenceKey: "ANTIGRAVITY_SESSION_ID"}
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
