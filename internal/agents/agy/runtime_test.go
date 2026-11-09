package agy

import (
	"slices"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/testutil/agenttest"
)

func TestRuntimeConformance(t *testing.T) {
	t.Parallel()
	expectedCleanup := []string{
		"ANTIGRAVITY_SESSION_ID",
		"AGY_SESSION_ID",
		"ANTIGRAVITY_CONVERSATION_ID",
		"ANTIGRAVITY_AGENT",
		"AGY_AGENT",
		"ANTIGRAVITY_CLI",
	}

	agenttest.RuntimeConformance(t, RuntimeDetector{}, "ANTIGRAVITY_SESSION_ID", true, expectedCleanup)
}

func TestRuntimePrecedence(t *testing.T) {
	t.Parallel()
	d := RuntimeDetector{}

	t.Run("session ID detected", func(t *testing.T) {
		env := agentapi.RuntimeEnvironment{
			LookupEnv: func(key string) (string, bool) {
				switch key {
				case "ANTIGRAVITY_SESSION_ID":
					return "session-123", true
				default:
					return "", false
				}
			},
		}
		got := d.Detect(env)
		want := agentapi.RuntimeObservation{NativeID: "session-123", PresenceKey: "ANTIGRAVITY_SESSION_ID"}
		if got != want {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	})

	t.Run("cleanup keys do not trigger detection", func(t *testing.T) {
		for _, key := range []string{"AGY_SESSION_ID", "ANTIGRAVITY_CONVERSATION_ID", "ANTIGRAVITY_AGENT", "AGY_AGENT", "ANTIGRAVITY_CLI"} {
			env := agentapi.RuntimeEnvironment{
				LookupEnv: func(k string) (string, bool) {
					return "1", k == key
				},
			}
			got := d.Detect(env)
			if got != (agentapi.RuntimeObservation{}) {
				t.Fatalf("expected empty observation for cleanup key %s, got %+v", key, got)
			}
		}
	})

	t.Run("nil LookupEnv returns empty observation", func(t *testing.T) {
		got := d.Detect(agentapi.RuntimeEnvironment{})
		if got != (agentapi.RuntimeObservation{}) {
			t.Fatalf("got %+v, want empty", got)
		}
	})

	t.Run("keys list immutability", func(t *testing.T) {
		keys1 := d.SessionEnvironmentKeys()
		keys1[0] = "MODIFIED"
		keys2 := d.SessionEnvironmentKeys()
		if slices.Contains(keys2, "MODIFIED") {
			t.Fatal("SessionEnvironmentKeys returned mutable shared slice")
		}
	})
}
