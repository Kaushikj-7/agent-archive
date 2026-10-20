package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/config"
)

func TestBareSearchOnTerminalAsksForQueryThenBrowses(t *testing.T) {
	env, _, id := prepareSearchBrowser(t)
	out, errOut, code := runOnTerminal(t, env, newFakeKeys("q"), "\nvisible\n", "search")
	if code != 0 {
		t.Fatalf("%d %s %s", code, errOut, out)
	}
	for _, want := range []string{"Search for:", "Searching all projects.", "enter words to search for", shortSessionID(id)} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q: %s", want, out)
		}
	}
	if strings.Contains(out, "Build the search index") {
		t.Fatalf("offered indexing with an index present: %s", out)
	}
}

func TestSearchOnTerminalOffersIndexingOnceBeforeFirstIndex(t *testing.T) {
	t.Parallel()
	for _, accept := range []bool{true, false} {
		t.Run(map[bool]string{true: "accept", false: "decline"}[accept], func(t *testing.T) {
			t.Parallel()
			home, userHome, project := t.TempDir(), t.TempDir(), t.TempDir()
			env := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Now())
			setupYes(t, env, "", 0, "--yes", "--provider", "s3", "--bucket", "synthetic", "--aws-profile", "test", "--region", "us-east-1", "--apps", "claude", "--project", project)
			answer := map[bool]string{true: "\n", false: "2\n"}[accept]
			out, errOut, code := runOnTerminal(t, env, newFakeKeys("q"), answer+"checksum\n", "search")
			if code != 0 {
				t.Fatalf("%d %s %s", code, errOut, out)
			}
			cfg, _, err := config.Load(home)
			if err != nil || cfg.SearchIndexing != accept || cfg.SearchIndexingDeclined == accept {
				t.Fatalf("indexing %v declined %v want %v: %v\n%s", cfg.SearchIndexing, cfg.SearchIndexingDeclined, accept, err, out)
			}
			defined := env.jobDefinition(userHome, env.installation(home, userHome).searchRole().ref()).Defined
			// Declining searches the (empty) bucket directly instead of
			// pointing at a manual refresh.
			want := map[bool]string{true: "Background indexing is on.", false: "No archived sessions to search in all projects."}[accept]
			if defined != accept || !strings.Contains(out, "Build the search index in the background?") || !strings.Contains(out, want) || !strings.Contains(out, "Search for:") || strings.Contains(out, "search --refresh") {
				t.Fatalf("job %v: %s", defined, out)
			}
			out, errOut, code = runOnTerminal(t, env, newFakeKeys("q"), "checksum\n", "search")
			if code != 0 || strings.Contains(out, "Build the search index") {
				t.Fatalf("asked again after answering: %d %s %s", code, errOut, out)
			}
		})
	}
}
