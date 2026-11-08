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

	t.Run("conformance exact ANTIGRAVITY_SESSION_ID", func(t *testing.T) {
		agenttest.RuntimeConformance(t, RuntimeDetector{}, "ANTIGRAVITY_SESSION_ID", true, expectedCleanup)
	})

	t.Run("conformance exact AGY_SESSION_ID", func(t *testing.T) {
		agenttest.RuntimeConformance(t, RuntimeDetector{}, "AGY_SESSION_ID", true, expectedCleanup)
	})

	t.Run("conformance exact ANTIGRAVITY_CONVERSATION_ID", func(t *testing.T) {
		agenttest.RuntimeConformance(t, RuntimeDetector{}, "ANTIGRAVITY_CONVERSATION_ID", true, expectedCleanup)
	})

	t.Run("conformance fallback ANTIGRAVITY_AGENT", func(t *testing.T) {
		agenttest.RuntimeConformance(t, RuntimeDetector{}, "ANTIGRAVITY_AGENT", false, expectedCleanup)
	})

	t.Run("conformance fallback AGY_AGENT", func(t *testing.T) {
		agenttest.RuntimeConformance(t, RuntimeDetector{}, "AGY_AGENT", false, expectedCleanup)
	})

	t.Run("conformance fallback ANTIGRAVITY_CLI", func(t *testing.T) {
		agenttest.RuntimeConformance(t, RuntimeDetector{}, "ANTIGRAVITY_CLI", false, expectedCleanup)
	})
}

func TestRuntimePrecedence(t *testing.T) {
	t.Parallel()
	d := RuntimeDetector{}

	t.Run("session ID takes precedence over fallback", func(t *testing.T) {
		env := agentapi.RuntimeEnvironment{
			LookupEnv: func(key string) (string, bool) {
				switch key {
				case "ANTIGRAVITY_SESSION_ID":
					return "session-123", true
				case "ANTIGRAVITY_AGENT":
					return "1", true
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

	t.Run("ANTIGRAVITY_SESSION_ID takes precedence over AGY_SESSION_ID", func(t *testing.T) {
		env := agentapi.RuntimeEnvironment{
			LookupEnv: func(key string) (string, bool) {
				switch key {
				case "ANTIGRAVITY_SESSION_ID":
					return "primary-id", true
				case "AGY_SESSION_ID":
					return "secondary-id", true
				default:
					return "", false
				}
			},
		}
		got := d.Detect(env)
		want := agentapi.RuntimeObservation{NativeID: "primary-id", PresenceKey: "ANTIGRAVITY_SESSION_ID"}
		if got != want {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	})

	t.Run("AGY_SESSION_ID takes precedence over ANTIGRAVITY_CONVERSATION_ID", func(t *testing.T) {
		env := agentapi.RuntimeEnvironment{
			LookupEnv: func(key string) (string, bool) {
				switch key {
				case "AGY_SESSION_ID":
					return "agy-id", true
				case "ANTIGRAVITY_CONVERSATION_ID":
					return "conv-id", true
				default:
					return "", false
				}
			},
		}
		got := d.Detect(env)
		want := agentapi.RuntimeObservation{NativeID: "agy-id", PresenceKey: "AGY_SESSION_ID"}
		if got != want {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	})

	t.Run("fallback ANTIGRAVITY_AGENT takes precedence over AGY_AGENT", func(t *testing.T) {
		env := agentapi.RuntimeEnvironment{
			LookupEnv: func(key string) (string, bool) {
				switch key {
				case "ANTIGRAVITY_AGENT":
					return "1", true
				case "AGY_AGENT":
					return "1", true
				default:
					return "", false
				}
			},
		}
		got := d.Detect(env)
		want := agentapi.RuntimeObservation{PresenceKey: "ANTIGRAVITY_AGENT", ProjectLatest: true}
		if got != want {
			t.Fatalf("got %+v, want %+v", got, want)
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
