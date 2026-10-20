package cli

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
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
			want := map[bool]string{true: "Background indexing is on.", false: "agent-archive search --refresh --wait 10m"}[accept]
			if defined != accept || !strings.Contains(out, "Build the search index in the background?") || !strings.Contains(out, want) || !strings.Contains(out, "Search for:") {
				t.Fatalf("job %v: %s", defined, out)
			}
			out, errOut, code = runOnTerminal(t, env, newFakeKeys("q"), "checksum\n", "search")
			if code != 0 || strings.Contains(out, "Build the search index") {
				t.Fatalf("asked again after answering: %d %s %s", code, errOut, out)
			}
		})
	}
}

func TestFirstIndexPassWaitsForBackgroundJobThenIndexes(t *testing.T) {
	env, _, _ := publishedFixture(t)
	home, _ := env.readHome()
	cfg, _, err := config.Load(home)
	must(t, err)
	release, err := local.NamedLock(home, "search-worker.lock")
	must(t, err)
	go func() {
		time.Sleep(200 * time.Millisecond)
		release()
	}()
	var out bytes.Buffer
	if err := indexFirstPass(home, cfg, contentSearchOptions{}, newPrompter(strings.NewReader(""), &out), env); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "1 of 1 known sessions indexed") {
		t.Fatalf("busy job was not waited out: %s", out.String())
	}
}

func TestFirstIndexPassStopsOnInterrupt(t *testing.T) {
	env, _, _ := publishedFixture(t)
	home, _ := env.readHome()
	cfg, _, err := config.Load(home)
	must(t, err)
	release, err := local.NamedLock(home, "search-worker.lock")
	must(t, err)
	defer release()
	signals := make(chan os.Signal, 1)
	signals <- os.Interrupt
	env.Interrupts = func() (<-chan os.Signal, func()) { return signals, func() {} }
	var out bytes.Buffer
	if err := indexFirstPass(home, cfg, contentSearchOptions{}, newPrompter(strings.NewReader(""), &out), env); err == nil || err.Error() != "interrupted" {
		t.Fatalf("interrupt ignored: %v %s", err, out.String())
	}
}

func TestSearchOnTerminalKeepsEarlierDisabledIndexingChoice(t *testing.T) {
	t.Parallel()
	home, userHome, project := t.TempDir(), t.TempDir(), t.TempDir()
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Now())
	setupYes(t, env, "", 0, "--yes", "--provider", "s3", "--bucket", "synthetic", "--aws-profile", "test", "--region", "us-east-1", "--apps", "claude", "--project", project)
	setupYes(t, env, "", 0, "--yes", "--search-indexing", "on")
	setupYes(t, env, "", 0, "--yes", "--search-indexing", "off")
	// A configuration written before the decline was recorded.
	cfg, _, err := config.Load(home)
	must(t, err)
	cfg.SearchIndexingDeclined = false
	must(t, config.Save(home, cfg))
	out, errOut, code := runOnTerminal(t, env, newFakeKeys("q"), "checksum\n", "search")
	if code != 0 || strings.Contains(out, "Build the search index") {
		t.Fatalf("asked despite earlier off: %d %s %s", code, errOut, out)
	}
}

func TestSearchQueryPromptEscapesProjectName(t *testing.T) {
	var out bytes.Buffer
	p := newPrompter(strings.NewReader("word\n"), &out)
	if _, err := promptSearchQuery(p, contentSearchOptions{project: "evil\n\x1b[2Jname"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "\x1b[2J") || strings.Contains(out.String(), "evil\n") {
		t.Fatalf("project name not escaped: %q", out.String())
	}
}

func TestMissingIndexWhilePausedAsksForRefresh(t *testing.T) {
	env, _, _ := publishedFixture(t)
	home, _ := env.readHome()
	cfg, _, err := config.Load(home)
	must(t, err)
	must(t, config.SetSearchIndexing(&cfg, true))
	cfg.Paused = true
	must(t, config.Save(home, cfg))
	var out, errOut bytes.Buffer
	if code := Run([]string{"search", "visible"}, nil, &out, &errOut, env); code != 0 {
		t.Fatalf("%d %s %s", code, &out, &errOut)
	}
	if strings.Contains(out.String(), "background indexing is building it") || !strings.Contains(out.String(), "Run search --refresh") {
		t.Fatalf("paused indexing reported as running: %s", out.String())
	}
}
