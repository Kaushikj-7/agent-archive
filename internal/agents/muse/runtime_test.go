package muse

import (
	"reflect"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	_ "github.com/wangjohn/agent-archive/internal/testutil/golden"
)

func TestRuntimePresenceDoesNotInterpretIdentity(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		vars map[string]string
		key  string
	}{
		{"unset", nil, ""},
		{"empty", map[string]string{"MUSE_THREAD_ID": "", "MUSE_SESSION_ID": " \t\n"}, ""},
		{"thread", map[string]string{"MUSE_THREAD_ID": "opaque-not-a-uuid"}, "MUSE_THREAD_ID"},
		{"session", map[string]string{"MUSE_SESSION_ID": "session/root"}, "MUSE_SESSION_ID"},
		{"thread preferred", map[string]string{"MUSE_THREAD_ID": "child", "MUSE_SESSION_ID": "root"}, "MUSE_THREAD_ID"},
		{"blank thread", map[string]string{"MUSE_THREAD_ID": " \t", "MUSE_SESSION_ID": "root"}, "MUSE_SESSION_ID"},
		{"opaque unicode", map[string]string{"MUSE_THREAD_ID": "  会話/child  "}, "MUSE_THREAD_ID"},
		{"nonidentity bytes", map[string]string{"MUSE_THREAD_ID": "\xff"}, "MUSE_THREAD_ID"},
		{"configuration only", map[string]string{"MUSE_NO_AUTO_UPDATE": "1", "MUSE_CONFIG_DIR": "/config"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := (RuntimeDetector{}).Detect(agentapi.RuntimeEnvironment{LookupEnv: func(key string) (string, bool) {
				value, ok := tc.vars[key]
				return value, ok
			}})
			if want := (agentapi.RuntimeObservation{PresenceKey: tc.key}); got != want {
				t.Fatalf("observation %+v; want %+v", got, want)
			}
		})
	}
}

func TestRuntimeCleanupDeclaresOnlySessionKeys(t *testing.T) {
	t.Parallel()
	detector := RuntimeDetector{}
	want := []string{"MUSE_THREAD_ID", "MUSE_SESSION_ID"}
	got := detector.SessionEnvironmentKeys()
	if !reflect.DeepEqual(got, want) {
		t.Fatal(got)
	}
	got[0] = "mutated"
	if !reflect.DeepEqual(detector.SessionEnvironmentKeys(), want) {
		t.Fatal("cleanup keys shared with caller")
	}
}
