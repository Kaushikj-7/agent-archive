package cli

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestNativeLatestIgnoresOnlyStrictlyOlderUnverifiedHistory(t *testing.T) {
	t.Parallel()
	for _, age := range []time.Duration{time.Hour, 0, -time.Hour} {
		t.Run(age.String(), func(t *testing.T) {
			t.Parallel()
			f := newNativeFixture(t)
			f.add(t, "claude", "native-selected", "Newest verified work", 0)
			bad := f.add(t, "claude", "native-unverified", "Unknown work", age)
			must(t, os.WriteFile(bad, []byte("{}\n"), 0600))
			at := f.at.Add(-age)
			must(t, os.Chtimes(bad, at, at))
			out, stderr, code := runHandoff(t, f.env, "--latest", "--harness", "claude")
			if age > 0 {
				if code != 0 || !strings.Contains(out, "Newest verified work") {
					t.Fatalf("older history blocked latest: code=%d stderr=%s", code, stderr)
				}
			} else if code != 1 || out != "" || !strings.Contains(stderr, "complete local") {
				t.Fatalf("unknown newer or tied history accepted: code=%d stderr=%s", code, stderr)
			}
		})
	}
}

func TestNativeLatestChecksUncertaintyAfterSkippingTheCurrentSession(t *testing.T) {
	t.Parallel()
	f := newNativeFixture(t)
	f.add(t, "claude", "native-current", "Current work", 0)
	f.add(t, "claude", "native-older", "Older verified work", 2*time.Hour)
	bad := f.add(t, "claude", "native-unverified", "Unknown work", time.Hour)
	must(t, os.WriteFile(bad, []byte("{}\n"), 0600))
	at := f.at.Add(-time.Hour)
	must(t, os.Chtimes(bad, at, at))
	f.env.LookupEnv = agentEnv(map[string]string{"CLAUDE_CODE_SESSION_ID": "native-current"})
	out, stderr, code := runHandoff(t, f.env, "--latest", "--harness", "claude")
	if code != 1 || out != "" || !strings.Contains(stderr, "complete local") {
		t.Fatalf("current session hid uncertain newer history: code=%d stderr=%s", code, stderr)
	}
}
