package cli

import (
	"bytes"
	"strings"
	"testing"
)

// Every command taking --harness rejects an unknown app with one tidy line
// and exit 2, before touching storage.
func TestHarnessFlagRejectsUnknownApps(t *testing.T) {
	t.Parallel()
	env, _, _ := publishedFixture(t)
	for _, args := range [][]string{
		{"list", "--harness", "nope"},
		{"list", "--json", "--harness", "nope"},
		{"show", "some-id", "--harness", "nope"},
		{"handoff", "some-id", "--harness", "nope"},
		{"backfill", "--dry-run", "--harness", "nope"},
	} {
		var out, errOut bytes.Buffer
		code := Run(args, nil, &out, &errOut, env)
		want := "agent-archive: " + args[0] + `: --harness must be claude, codex, cursor, muse, or agy, not "nope"; run agent-archive ` + args[0] + " --help\n"
		if code != 2 || errOut.String() != want || out.Len() != 0 {
			t.Errorf("%v: exit=%d stdout=%q stderr=%q, want exit 2 and %q", args, code, out.String(), errOut.String(), want)
		}
	}
}

// "claude-code" names Claude, as hooks and backfill already accept, and
// filters as "claude" does rather than matching nothing.
func TestHarnessFlagAcceptsCanonicalAliases(t *testing.T) {
	t.Parallel()
	cases := []struct {
		value string
		want  string
	}{
		{value: "", want: ""},
		{value: "claude", want: "claude"},
		{value: "claude-code", want: "claude"},
		{value: " Codex ", want: "codex"},
		{value: "cursor", want: "cursor"},
	}
	for _, tc := range cases {
		if got, ok := harnessFlagWithCatalog(productionAgents.Catalog(), tc.value); !ok || got != tc.want {
			t.Errorf("harnessFlag(%q) = %q, %v; want %q", tc.value, got, ok, tc.want)
		}
	}
	env, _, id := publishedFixture(t)
	var byName, byAlias, errOut bytes.Buffer
	if code := Run([]string{"list", "--harness", "codex"}, nil, &byName, &errOut, env); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(byName.String(), id[:min(8, len(id))]) {
		t.Fatalf("list --harness codex:\n%s", byName.String())
	}
	if code := Run([]string{"list", "--harness", "CODEX"}, nil, &byAlias, &errOut, env); code != 0 || byAlias.String() != byName.String() {
		t.Fatalf("list --harness CODEX differs: exit=%d\n%s", code, byAlias.String())
	}
}
