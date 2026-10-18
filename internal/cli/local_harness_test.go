package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestLocalListAndShowCanonicalHarnessAliases(t *testing.T) {
	t.Parallel()
	for _, harness := range []string{"muse", "claude"} {
		t.Run(harness, func(t *testing.T) {
			t.Parallel()
			var env Env
			var id string
			var aliases []string
			if harness == "muse" {
				var data, workspace string
				env, data, workspace, _ = museNativeEnv(t)
				id = museSyntheticID
				writeMuseNative(t, data, id, museNativeBytes(t, "exec", id, workspace))
				aliases = []string{"muse-code", "MUSE", " MUSE-CODE "}
			} else {
				fixture := newNativeFixture(t)
				id = "local-alias-session"
				fixture.add(t, harness, id, "Alias prompt", time.Hour)
				env = fixture.env
				aliases = []string{"claude-code", "CLAUDE", " CLAUDE-CODE "}
			}
			for _, command := range []string{"list", "show"} {
				args := []string{command}
				if command == "show" {
					args = append(args, id)
				}
				args = append(args, "--source", "local", "--json", "--harness")
				var canonical, problems bytes.Buffer
				if code := Run(append(args, harness), nil, &canonical, &problems, env); code != 0 {
					t.Fatalf("%s canonical: %d: %s", command, code, problems.String())
				}
				if !strings.Contains(canonical.String(), id) {
					t.Fatal("canonical command lost native identity")
				}
				for _, alias := range aliases {
					var out, errOut bytes.Buffer
					if code := Run(append(args, alias), nil, &out, &errOut, env); code != 0 || out.String() != canonical.String() {
						t.Fatalf("%s alias %q differs: %d: %s", command, alias, code, errOut.String())
					}
				}
				var out, errOut bytes.Buffer
				if code := Run(append(args, "unknown-native-app"), nil, &out, &errOut, env); code != 1 || out.Len() != 0 || !strings.Contains(errOut.String(), harnessFlagError("unknown-native-app")) {
					t.Fatalf("%s invalid harness: %d: %s", command, code, errOut.String())
				}
			}
		})
	}
}
