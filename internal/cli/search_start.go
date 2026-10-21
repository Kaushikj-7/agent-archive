package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/search"
	"github.com/wangjohn/agent-archive/internal/setupjournal"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

// setSearchIndexing saves an indexing answer. Yes goes through the same setup
// transaction as setup --yes --search-indexing on, whose review reaches out
// only when it fails. No changes nothing but the saved answer, under the setup
// lock, so it needs no storage check or network.
func (e Env) setSearchIndexing(on bool, out io.Writer) bool {
	if !on {
		if err := e.declineSearchIndexing(); err != nil {
			terminal.Printf(out, "%s %v. The answer was not saved; search asks again next time.\n", styleFor(out).warnMark(), err)
			return false
		}
		return true
	}
	var review bytes.Buffer
	if code := runSetupCommand([]string{"--yes", "--search-indexing", string(searchIndexingOn)}, strings.NewReader(""), &review, &review, e); code != 0 {
		_, _ = out.Write(review.Bytes())
		return false
	}
	return true
}

// searchIndexingDeclined reports an explicit No to search indexing that is
// still in force: the saved answer, or lifecycle protection with indexing off,
// which an earlier on-then-off left without the declined flag.
func searchIndexingDeclined(cfg config.Config) bool {
	return !cfg.SearchIndexing && (cfg.SearchIndexingDeclined || cfg.SearchLifecycleProtection)
}

// searchAnswerLockWait bounds how long a No waits for another configuration
// writer, such as a sync pass, to finish.
var searchAnswerLockWait = 5 * time.Second

func (e Env) declineSearchIndexing() error {
	home, err := e.readHome()
	if err != nil {
		return err
	}
	release, err := local.NamedLock(home, "setup.lock")
	if err != nil {
		return errors.New("setup is running; answer again when it finishes")
	}
	defer release()
	// Every configuration writer, pause and resume included, holds the
	// collector lock, so the answer is saved over the latest configuration.
	unlock, err := lockCollectorWait(home, "search indexing answer", e.now(), searchAnswerLockWait)
	if err != nil {
		return fmt.Errorf("%s holds the collector lock", lockHolder(home))
	}
	defer unlock()
	if setupjournal.TransactionPending(home) {
		return errors.New(recoveryPending(home))
	}
	return config.DeclineSearchIndexing(home)
}

// searchIndexMissing reports whether no usable local index exists yet,
// counting an index whose first creation was interrupted as missing.
func searchIndexMissing(dir, namespace string) bool {
	index, err := search.OpenRead(context.Background(), dir, namespace)
	if err == nil {
		_ = index.Close()
	}
	return os.IsNotExist(err) || errors.Is(err, search.ErrUninitialized)
}

// offerSearchIndexing asks once, on a terminal, before the first local index
// exists, whether to build it in the background. Either answer is saved;
// after No, terminal searches read the bucket directly (see
// runDirectSearch), and a failed offer leaves the search to run on what is
// indexed. It reports whether indexing was turned on now.
func offerSearchIndexing(p *prompter, dir, namespace string, cfg config.Config, env searchCommandDependencies) (bool, error) {
	stdout := p.out
	if cfg.SearchIndexing || searchIndexingDeclined(cfg) || !cfg.Archive.Enabled || cfg.Paused {
		return false, nil
	}
	if !searchIndexMissing(dir, namespace) {
		return false, nil
	}
	terminal.Println(stdout, "Search keeps a private, searchable copy of your archived conversations on this machine.")
	terminal.Println(stdout, p.style.dim("The background job updates it about once a minute and pauses with capture."))
	answer, err := p.guidedChoice(promptModel{Question: "Build the search index in the background?", Default: "yes", Receipt: "Search index:", Primary: []option{{"yes", "Yes, index in the background"}, {"no", "No, search the bucket directly each time (slow)"}}})
	if err != nil {
		return false, err
	}
	on := answer == "yes"
	if !env.setSearchIndexing(on, stdout) {
		// A failed No has said search asks again; a failed Yes printed
		// setup's review, so name the setup that retries it.
		if on {
			terminal.Println(stdout, "The answer was not saved. To retry, run:")
			terminal.Println(stdout, p.style.cmd("agent-archive setup --yes --search-indexing on"))
		}
		return false, nil
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
	if coverage != nil && coverage.Known > 0 && coverage.Indexed >= coverage.Known {
		terminal.Printf(stdout, "%s Background indexing is on; %s indexed.\n", p.style.okMark(), sessionsText(coverage.Known))
		return nil
	}
	if coverage != nil && coverage.Known > 0 {
		terminal.Printf(stdout, "%s Background indexing is on; %d of %d sessions indexed, the rest within minutes.\n", p.style.okMark(), coverage.Indexed, coverage.Known)
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
// refresh, JSON or piped output. It leaves its prompter in opts.prompt, open,
// for a direct search's later question; the caller closes it.
func startInteractiveSearch(home, dir string, cfg *config.Config, opts *contentSearchOptions, stdout, stderr io.Writer, env searchCommandDependencies) int {
	if !opts.hasQuery || opts.refresh || opts.json || opts.stdin == nil || !browseInteractive(env, opts.stdin, stdout) {
		return 0
	}
	p := newPrompter(opts.stdin, stdout)
	opts.prompt = p
	namespace, _ := contentSearchLocation(home, *cfg)
	enabled, err := offerSearchIndexing(p, dir, namespace, *cfg, env)
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
