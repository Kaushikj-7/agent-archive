// Package muse interprets qualified Muse runtime presence without capture authority.
package muse

import (
	"strings"

	"github.com/wangjohn/agent-archive/internal/agentapi"
)

// RuntimeDetector observes presence through an injected environment getter.
type RuntimeDetector struct{}

// Detect reports presence without claiming current-thread or project identity.
func (RuntimeDetector) Detect(env agentapi.RuntimeEnvironment) agentapi.RuntimeObservation {
	for _, key := range []string{"MUSE_THREAD_ID", "MUSE_SESSION_ID"} {
		if value, set := env.LookupEnv(key); set && strings.TrimSpace(value) != "" {
			return agentapi.RuntimeObservation{PresenceKey: key}
		}
	}
	return agentapi.RuntimeObservation{}
}

// SessionEnvironmentKeys declares session state to remove from other-agent launches.
func (RuntimeDetector) SessionEnvironmentKeys() []string {
	return []string{"MUSE_THREAD_ID", "MUSE_SESSION_ID"}
}
