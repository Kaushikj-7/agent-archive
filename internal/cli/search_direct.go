package cli

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/search"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

// directSearchPause is how long a direct search reads the bucket before it
// asks whether to go on.
const directSearchPause = 2 * time.Minute

// directSearchRate is about how many sessions a minute a direct search
// reads, as an explicit refresh does. It is the basis of the warning's
// estimate only.
const directSearchRate = 50

func (e Env) directSearchBudget() time.Duration {
	if e.directSearchPause != nil {
		return e.directSearchPause()
	}
	return directSearchPause
}

// wantsDirectSearch reports whether a query reads the bucket directly: on a
// terminal (startInteractiveSearch left its prompter), after the indexing
// offer was declined, with no usable local index in dir (none, or one whose
// first creation was interrupted, as searchIndexMissing decides). JSON, piped
// output, refresh and cursors keep reporting the missing index.
func wantsDirectSearch(cfg config.Config, opts contentSearchOptions, dir, namespace string) bool {
	return searchIndexingDeclined(cfg) && opts.prompt != nil && opts.hasQuery && !opts.json && !opts.refresh && opts.query.Cursor == "" && searchIndexMissing(dir, namespace)
}

// directSearchCandidate applies the query's scope and filters to a session's
// metadata, as Search applies them to indexed sessions, so nothing outside
// them is downloaded.
func directSearchCandidate(m archive.Metadata, q search.Query, scope sessionScope) bool {
	included := func(want search.Inclusion, is bool) bool {
		return want == search.IncludeAll || (want == search.Only) == is
	}
	switch {
	case q.Harness != "" && m.Harness.Name != q.Harness:
		return false
	case !q.From.IsZero() && m.CapturedAt.Before(q.From), !q.To.IsZero() && m.CapturedAt.After(q.To):
		return false
	case !included(q.Replay, m.Replay != nil), !included(q.Children, m.NativeChild || m.ParentSessionID != ""):
		return false
	}
	return scope.contains(m, nil)
}

// directScanStop is why one stretch of a direct search ended.
type directScanStop string

const (
	directScanDone        directScanStop = "done"
	directScanPaused      directScanStop = "paused"
	directScanStopped     directScanStop = "stopped"
	directScanInterrupted directScanStop = "interrupted"
)

// directMatch is a session found to match while a direct search runs.
type directMatch struct {
	metadata archive.Metadata
	count    int
}

// directScan reads sessions newest first into a store held in memory. Only
// matching sessions keep their passages, for the results browser.
type directScan struct {
	index    *search.Store
	objects  storage.ObjectStore
	parsers  agentapi.ParsersLookup
	query    search.Query
	now      func() time.Time
	budget   time.Duration
	sessions []archive.Metadata
	total    int

	mu       sync.Mutex
	searched int
	matching int
	failed   int
	// matched and unread are the sessions searched that matched and that
	// could not be read, by harness/session, so narrowing keeps only the
	// counts of sessions in the narrowed scope.
	matched map[string]bool
	unread  map[string]bool
}

// directSessionKey identifies a session among those a direct search reads.
func directSessionKey(m archive.Metadata) string { return m.Harness.Name + "/" + m.SessionID }

// unreadable is how many sessions searched so far could not be read.
func (s *directScan) unreadable() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.failed
}

func (s *directScan) counts() (searched, matching int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.searched, s.matching
}

// session indexes one session through the same verified loading and parser
// visibility as background indexing, and reports its matches. Its text is
// dropped unless it matches.
func (s *directScan) session(ctx context.Context, m archive.Metadata) (count int, unread bool, err error) {
	at := s.now()
	if err = s.index.Observe(ctx, m, s.parsers, at, 0); err != nil {
		return 0, false, err
	}
	progress, err := s.index.Run(ctx, s.objects, s.parsers, at, search.Budget{Sources: 1000, Sessions: 1})
	if err != nil {
		return 0, false, err
	}
	if progress.Committed == 0 {
		// An unreadable source is counted, not retried: the next search
		// reads it again.
		return 0, true, s.index.ForgetSession(ctx, m.Harness.Name, m.SessionID)
	}
	count, err = s.index.SessionMatches(ctx, s.query, m.Harness.Name, m.SessionID)
	if err != nil {
		return 0, false, err
	}
	return count, false, s.index.DropSessionText(ctx, m.Harness.Name, m.SessionID, count > 0)
}

// run reads sessions until none is left or the budget is spent, sending each
// match on found. It returns when ctx ends too. The budget is measured from
// the wall clock; tests set it to zero or far beyond the fixture.
func (s *directScan) run(ctx context.Context, found chan<- directMatch) (directScanStop, error) {
	started := time.Now()
	for read := 0; ; read++ {
		s.mu.Lock()
		next := s.searched
		s.mu.Unlock()
		if next >= len(s.sessions) {
			return directScanDone, nil
		}
		// Each stretch reads at least one session, so going on always
		// makes progress.
		if read > 0 && time.Since(started) >= s.budget {
			return directScanPaused, nil
		}
		m := s.sessions[next]
		count, unread, err := s.session(ctx, m)
		select {
		case <-ctx.Done():
			// Esc or a signal: the session cut short is not counted.
			return directScanStopped, nil
		default:
		}
		if err != nil {
			return directScanStopped, err
		}
		s.mu.Lock()
		s.searched++
		if count > 0 {
			s.matching++
			s.matched[directSessionKey(m)] = true
		}
		if unread {
			s.failed++
			s.unread[directSessionKey(m)] = true
		}
		s.mu.Unlock()
		if count > 0 {
			select {
			case found <- directMatch{metadata: m, count: count}:
			case <-ctx.Done():
				return directScanStopped, nil
			}
		}
	}
}

// directResults is what a direct search found, for the results browser: the
// store in memory, which the browser closes, its first page, and the options
// as narrowed at a pause.
type directResults struct {
	index *search.Store
	page  search.Page
	opts  contentSearchOptions
}

// runDirectSearch searches the bucket itself when indexing was declined and
// no local index exists: sessions in scope, newest first, read into memory
// and matched there, pausing after a budget to ask whether to go on. It
// returns what was found for the results browser, or nil and the exit code
// when there is nothing to browse.
func runDirectSearch(ctx context.Context, cfg config.Config, namespace string, scope sessionScope, opts contentSearchOptions, stdout, stderr io.Writer, env searchCommandDependencies) (*directResults, int) {
	p := opts.prompt
	objects, err := env.openStore(cfg)
	if err != nil {
		return nil, searchCommandError(stderr, err)
	}
	spin := p.style.spin(stdout, "Listing the bucket's sessions")
	listed, err := reader.ListMetadataWithOptions(ctx, objects, archiveSessionsPrefix, reader.Filter{Harness: opts.query.Harness}, reader.ListOptions{Cache: listCache(env, false), Skipped: warnSkippedSidecar(stderr, "search")})
	spin.stop()
	if ctx.Err() != nil {
		return nil, directSearchInterrupted(stderr)
	}
	if err != nil {
		return nil, searchCommandError(stderr, err)
	}
	var sessions []archive.Metadata
	for _, m := range listed {
		if directSearchCandidate(m, opts.query, scope) {
			sessions = append(sessions, m)
		}
	}
	scopeLabel := "all projects"
	if !opts.all {
		scopeLabel = archive.DisplayLine(scope.Label)
	}
	if len(sessions) == 0 {
		terminal.Printf(stdout, "No archived sessions to search in %s.\n", scopeLabel)
		return nil, 0
	}
	index, err := search.OpenMemory(ctx, namespace)
	if err != nil {
		return nil, searchCommandError(stderr, err)
	}
	browsing := false
	defer func() {
		if !browsing {
			_ = index.Close()
		}
	}()
	scan := &directScan{index: index, objects: objects, parsers: parsersFor(env), query: opts.query, now: env.now, budget: env.directSearchBudget(), sessions: sessions, total: len(sessions), matched: map[string]bool{}, unread: map[string]bool{}}
	printDirectSearchWarning(p, cfg, scan)

	// The project the working directory is in, offered at a pause when the
	// search covers every project.
	var narrow sessionScope
	if opts.all && scope.Label != "" {
		narrow = scope.only()
	}
	for {
		why, err := scan.scan(ctx, p, stdout, env, scopeLabel)
		if why == directScanInterrupted {
			return nil, directSearchInterrupted(stderr)
		}
		if err != nil {
			return nil, searchCommandError(stderr, err)
		}
		searched, matching := scan.counts()
		unread := ""
		if n := scan.unreadable(); n > 0 {
			unread = " · " + countText(n) + " could not be read"
		}
		if why != directScanPaused {
			switch {
			case searched >= scan.total:
				terminal.Printf(stdout, "%s Searched all %s · %s%s\n", p.style.okMark(), sessionsText(scan.total), matchingSessionsText(matching), unread)
			case searched == 0:
				terminal.Printf(stdout, "%s Stopped before searching any of the %s\n", p.style.okMark(), sessionsText(scan.total))
			default:
				terminal.Printf(stdout, "%s Stopped after the newest %s of %s · %s%s\n", p.style.okMark(), countText(searched), sessionsText(scan.total), matchingSessionsText(matching), unread)
			}
			break
		}
		terminal.Printf(stdout, "%s Searched the newest %s of %s · %s%s\n", p.style.okMark(), countText(searched), sessionsText(scan.total), matchingSessionsText(matching), unread)
		choice, err := askDirectSearchPause(ctx, p, scan, narrow)
		if err != nil {
			return nil, searchCommandError(stderr, err)
		}
		if choice == directPauseResults {
			break
		}
		if choice == directPauseStop {
			return nil, 0
		}
		if choice == directPauseProject {
			scan.narrow(narrow)
			opts.all, scopeLabel = false, archive.DisplayLine(narrow.Label)
			opts.query.Scope = search.Scope{Restricted: true, Named: narrow.Dir == "", RepoKey: narrow.RepoKey, ProjectName: narrow.Label, ProjectIDs: narrow.ProjectIDs}
			narrow = sessionScope{}
		}
	}
	if _, matching := scan.counts(); matching == 0 {
		return nil, 0
	}
	queryCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	page, err := index.Search(queryCtx, opts.query)
	if err != nil {
		return nil, searchCommandError(stderr, err)
	}
	browsing = true
	return &directResults{index: index, page: page, opts: opts}, 0
}

// directSearchInterrupted ends a direct search that a signal interrupted.
func directSearchInterrupted(stderr io.Writer) int {
	terminal.Println(stderr, "agent-archive: search: interrupted")
	return 130
}

// narrow keeps the sessions in scope, those already searched first, and
// counts only those: a session searched before narrowing that is outside the
// scope is neither searched nor matching any more, as the results leave it
// out.
func (s *directScan) narrow(scope sessionScope) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var kept []archive.Metadata
	searched, matching, failed := 0, 0, 0
	for i, m := range s.sessions {
		if !scope.contains(m, nil) {
			continue
		}
		kept = append(kept, m)
		if i < s.searched {
			searched++
			if s.matched[directSessionKey(m)] {
				matching++
			}
			if s.unread[directSessionKey(m)] {
				failed++
			}
		}
	}
	s.sessions, s.total = kept, len(kept)
	s.searched, s.matching, s.failed = searched, matching, failed
}

// printDirectSearchWarning says what a direct search costs before it starts,
// and the command that makes the next search instant.
func printDirectSearchWarning(p *prompter, cfg config.Config, scan *directScan) {
	provider := "R2"
	if strings.EqualFold(strings.TrimSpace(cfg.Storage.Provider), credentials.ProviderS3) {
		provider = "S3"
	}
	terminal.Printf(p.out, "%s Without an index, search downloads sessions from your %s bucket, newest first. Nothing is kept on this machine.\n", p.style.warnMark(), provider)
	minutes := (scan.total + directSearchRate - 1) / directSearchRate
	terminal.Printf(p.out, "  It pauses after %s; across all %s a full search takes %s.\n", durationText(scan.budget), sessionsText(scan.total), aboutMinutes(minutes))
	terminal.Println(p.out, "  To search instantly next time, run:")
	terminal.Println(p.out, "  "+p.style.cmd("agent-archive setup --yes --search-indexing on"))
	terminal.Println(p.out)
}

// scan runs one stretch of the search: it reads keys for Esc while the
// sessions are read, keeps the progress line on stdout, and prints each
// match found through the prompter.
func (s *directScan) scan(ctx context.Context, p *prompter, stdout io.Writer, env searchCommandDependencies, scopeLabel string) (directScanStop, error) {
	var keys *keyInput
	if term, ok := env.openKeyTerminal(p.source); ok {
		// The keys answer Ctrl-Z while they are read; the prompter's own
		// stop handling, left from the questions before, stands aside so
		// the process is stopped once, with the terminal restored first.
		release := p.suspendPrompts(true)
		if keys = startKeys(term); keys == nil {
			release()
		} else {
			defer release()
		}
	}
	if keys != nil {
		defer keys.close()
	}
	hint := ""
	if keys != nil {
		hint = " · " + p.style.dim("Esc stop")
	}
	label := func() string {
		searched, matching := s.counts()
		return fmt.Sprintf("Searching the bucket · %s · %s of %s, newest first · %s so far%s", scopeLabel, countText(searched), sessionsText(s.total), matchingText(matching), hint)
	}
	spin := p.style.spinLabelEvery(stdout, label, spinnerInterval)
	defer func() { spin.stop() }()

	scanCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	found := make(chan directMatch)
	type outcome struct {
		why directScanStop
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		why, err := s.run(scanCtx, found)
		done <- outcome{why, err}
	}()
	escaped := false
	polling := make(chan struct{})
	close(polling)
	for {
		var poll <-chan struct{}
		if keys != nil {
			poll = polling
		}
		select {
		case m := <-found:
			spin.stop()
			printDirectMatch(p, m)
			spin = p.style.spinLabelEvery(stdout, label, spinnerInterval)
			continue
		case result := <-done:
			if ctx.Err() != nil {
				return directScanInterrupted, nil
			}
			if escaped && result.why == directScanDone {
				result.why = directScanStopped
			}
			return result.why, result.err
		case <-ctx.Done():
			cancel()
			<-done
			return directScanInterrupted, nil
		case <-poll:
		}
		k, err := keys.nextWait(100 * time.Millisecond)
		if err != nil {
			// Keys can no longer be read; Ctrl-C still interrupts.
			keys.close()
			keys = nil
			continue
		}
		if k.kind == keyEscape && !escaped {
			escaped = true
			cancel()
		}
	}
}

// printDirectMatch prints a matching session as it is found: its agent's
// dot, its title, and how many passages match.
func printDirectMatch(p *prompter, m directMatch) {
	dot, _ := agentLabel(m.metadata.Harness.Name)
	title := archive.DisplayLine(archive.DisplayTitle(m.metadata))
	if strings.TrimSpace(title) == "" {
		title = shortSessionID(m.metadata.SessionID)
	}
	if p.style.width > 20 {
		title = truncateVisible(title, p.style.width-16)
	}
	matches := "1 match"
	if m.count != 1 {
		matches = strconv.Itoa(m.count) + " matches"
	}
	terminal.Printf(p.out, "  %s %s  %s\n", p.style.agent(m.metadata.Harness.Name, dot), title, p.style.dim(matches))
}

// directPauseChoice is an answer to the question a direct search asks when
// its budget is spent.
type directPauseChoice string

const (
	directPauseKeep    directPauseChoice = "keep"
	directPauseProject directPauseChoice = "project"
	directPauseResults directPauseChoice = "results"
	directPauseStop    directPauseChoice = "stop"
)

// askDirectSearchPause asks how to go on once the budget is spent: keep
// going, narrow to the working directory's project when the search covers
// every project, show what was found, or stop. Signals end the process at
// the question, as at any other.
func askDirectSearchPause(ctx context.Context, p *prompter, scan *directScan, narrow sessionScope) (directPauseChoice, error) {
	options := []option{{string(directPauseKeep), "Keep going"}}
	if narrow.Label != "" {
		count := 0
		for _, m := range scan.sessions {
			if narrow.contains(m, nil) {
				count++
			}
		}
		options = append(options, option{string(directPauseProject), fmt.Sprintf("Only %s (%s)", archive.DisplayLine(narrow.Label), sessionsText(count))})
	}
	options = append(options, option{string(directPauseResults), "Show the results so far"}, option{string(directPauseStop), "Stop"})
	if owner := readPhase(ctx); owner != nil {
		owner.Pause()
		defer owner.Resume()
	}
	answer, err := p.guidedChoice(promptModel{Question: "Keep searching the bucket?", Default: string(directPauseKeep), Receipt: "Search:", Primary: options})
	return directPauseChoice(answer), err
}

// countText is n with its thousands grouped: 2,350.
func countText(n int) string {
	digits := strconv.Itoa(n)
	if n < 0 {
		return digits
	}
	var b strings.Builder
	for i, r := range digits {
		if i > 0 && (len(digits)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	return b.String()
}

func sessionsText(n int) string {
	if n == 1 {
		return "1 session"
	}
	return countText(n) + " sessions"
}

func matchingText(n int) string { return countText(n) + " matching" }

func matchingSessionsText(n int) string {
	if n == 1 {
		return "1 matching session"
	}
	return countText(n) + " matching sessions"
}

// durationText is a budget in whole minutes when it is one, else as Go
// prints it.
func durationText(d time.Duration) string {
	switch {
	case d == time.Minute:
		return "1 minute"
	case d > 0 && d%time.Minute == 0:
		return strconv.Itoa(int(d/time.Minute)) + " minutes"
	}
	return d.String()
}

// aboutMinutes is a rough length of time, rounded the way a person says it.
func aboutMinutes(minutes int) string {
	switch {
	case minutes <= 1:
		return "about a minute"
	case minutes < 50:
		return "about " + strconv.Itoa(minutes) + " minutes"
	case minutes < 90:
		return "about an hour"
	}
	return "about " + strconv.Itoa((minutes+30)/60) + " hours"
}
