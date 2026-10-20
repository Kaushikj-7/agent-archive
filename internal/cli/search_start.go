package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/search"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

// setSearchIndexing saves an indexing answer through the same setup
// transaction as setup --yes --search-indexing on|off. Setup's review goes to
// out only when it fails, since the person answered one question.
func (e Env) setSearchIndexing(on bool, out io.Writer) bool {
	mode := searchIndexingOff
	if on {
		mode = searchIndexingOn
	}
	var review bytes.Buffer
	if code := runSetupCommand([]string{"--yes", "--search-indexing", string(mode)}, strings.NewReader(""), &review, &review, e); code != 0 {
		_, _ = out.Write(review.Bytes())
		return false
	}
	return true
}

// offerSearchIndexing asks once, on a terminal, before the first local index
// exists, whether to build it in the background. Either answer is saved;
// a declined or failed offer leaves the search to run on what is indexed.
// It reports whether indexing was turned on now.
func offerSearchIndexing(p *prompter, dir string, cfg config.Config, env searchCommandDependencies) (bool, error) {
	stdout := p.out
	// Lifecycle protection without indexing is an earlier explicit off.
	declined := cfg.SearchIndexingDeclined || cfg.SearchLifecycleProtection
	if cfg.SearchIndexing || declined || !cfg.Archive.Enabled || cfg.Paused {
		return false, nil
	}
	if _, err := os.Lstat(filepath.Join(dir, "content.sqlite")); !errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	terminal.Println(stdout, "Search keeps a private, searchable copy of your archived conversations on this machine.")
	terminal.Println(stdout, p.style.dim("The background job updates it about once a minute and pauses with capture."))
	answer, err := p.guidedChoice(promptModel{Question: "Build the search index in the background?", Default: "yes", Receipt: "Search index:", Primary: []option{{"yes", "Yes, index in the background"}, {"no", "No, I'll run the refresh myself"}}})
	if err != nil {
		return false, err
	}
	on := answer == "yes"
	if !env.setSearchIndexing(on, stdout) {
		terminal.Println(stdout, "The answer was not saved. To retry, run:")
		terminal.Println(stdout, p.style.cmd("agent-archive setup --yes --search-indexing "+map[bool]string{true: "on", false: "off"}[on]))
		return false, nil
	}
	if !on {
		terminal.Println(stdout, "To build or update the index yourself, run:")
		terminal.Println(stdout, p.style.cmd("agent-archive search --refresh --wait 10m"))
	}
	return on, nil
}

// indexFirstPass indexes for up to 30 seconds right after indexing is turned
// on, so the first search finds something. The new background job may start
// at once and hold the index; the wait retries until it lets go. An interrupt
// ends the command; any other failure is left to the background job.
func indexFirstPass(home string, cfg config.Config, opts contentSearchOptions, p *prompter, env searchCommandDependencies) error {
	stdout := p.out
	ctx, stop := readCommandContext(env)
	defer stop()
	spin := p.style.spin(stdout, "Indexing your first sessions")
	pass := opts
	pass.refresh, pass.wait, pass.session = true, 30*time.Second, ""
	_, coverage, err := refreshContentSearch(ctx, home, cfg, env, pass)
	spin.stop()
	if errors.Is(err, context.Canceled) {
		return errors.New("interrupted")
	}
	if coverage != nil && coverage.Known > 0 {
		terminal.Printf(stdout, "%s Background indexing is on. %d of %d known sessions indexed so far; the rest fill in over the next few minutes.\n", p.style.okMark(), coverage.Indexed, coverage.Known)
		return nil
	}
	terminal.Printf(stdout, "%s Background indexing is on. Results fill in over the next few minutes.\n", p.style.okMark())
	return nil
}

// promptSearchQuery asks for the words bare search runs on a terminal.
func promptSearchQuery(p *prompter, opts contentSearchOptions) (string, error) {
	scope := "Searching all projects."
	if opts.project != "" {
		scope = "Searching project " + archive.DisplayLine(opts.project) + "."
	}
	return p.guidedText(promptModel{Question: "Search for:", Label: "Search", Helpers: []string{`All words must match; "quotes" match a phrase and a trailing * matches a prefix.`, scope}, Validate: func(value string) error {
		if value == "" {
			return errors.New("enter words to search for")
		}
		_, err := search.MatchExpression(value)
		return err
	}})
}

// startInteractiveSearch runs the terminal screens before a query: the
// indexing offer, then the query itself for bare search. It asks nothing for
// refresh, JSON or piped output.
func startInteractiveSearch(home, dir string, cfg *config.Config, opts *contentSearchOptions, stdout, stderr io.Writer, env searchCommandDependencies) int {
	if !opts.hasQuery || opts.refresh || opts.json || opts.stdin == nil || !browseInteractive(env, opts.stdin, stdout) {
		return 0
	}
	p := newPrompter(opts.stdin, stdout)
	defer p.close()
	enabled, err := offerSearchIndexing(p, dir, *cfg, env)
	if err != nil {
		return searchCommandError(stderr, err)
	}
	if reloaded, found, err := config.Load(home); err == nil && found {
		*cfg = reloaded
	}
	if enabled {
		if err := indexFirstPass(home, *cfg, *opts, p, env); err != nil {
			return searchCommandError(stderr, err)
		}
	}
	if !opts.ask {
		return 0
	}
	text, err := promptSearchQuery(p, *opts)
	if err != nil {
		return searchCommandError(stderr, err)
	}
	opts.query.Text = text
	return 0
}
