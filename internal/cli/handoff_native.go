package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/nativesessions"
	"github.com/wangjohn/agent-archive/internal/terminal"
	"github.com/wangjohn/agent-archive/internal/transcriptio"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const nativeReadBudget int64 = 64 * 1024 * 1024

const nativeWindowBytes int64 = 256 * 1024

const nativePreviewBatch = 50

func (e Env) nativeFiles() nativesessions.FileSystem {
	if e.nativeFS != nil {
		return e.nativeFS
	}
	return nativesessions.OS{}
}

func (e Env) nativeRoots(harness string) ([]nativesessions.StoreRoot, error) {
	if e.nativeStoreRoots != nil {
		var roots []nativesessions.StoreRoot
		for _, r := range e.nativeStoreRoots {
			if harness == "" || r.Harness == harness {
				roots = append(roots, r)
			}
		}
		return roots, nil
	}
	home, err := e.userHomeDir()
	if err != nil {
		return nil, err
	}
	locations := e.nativeSessionDirectories(home, config.Config{})
	var roots []nativesessions.StoreRoot
	for _, name := range e.agentRegistry().NativeHeaderAgents() {
		if harness != "" && harness != name {
			continue
		}
		provider, _ := e.agentRegistry().LookupNativeHeaders(name)
		dirs := locations[name]
		roots = append(roots, provider.Roots(agentapi.NativeLocations{UserHome: home, Directories: dirs}, agentapi.DiscoveryHandoff)...)
	}

	return roots, nil
}

// nativeBrowserSignals shares one signal owner with discovery and preview jobs.
type nativeBrowserSignals struct {
	sessionBrowserDependencies
	signals <-chan os.Signal
}

func (n nativeBrowserSignals) interrupts() (<-chan os.Signal, func()) { return n.signals, func() {} }

func nativeCommandContext(env sessionBrowserDependencies) (context.Context, <-chan os.Signal, func()) {
	ctx, cancel := context.WithCancel(context.Background())
	signals, stop := env.interrupts()
	browser := make(chan os.Signal, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		select {
		case sig, ok := <-signals:
			if ok {
				cancel()
				browser <- sig
			}
		case <-ctx.Done():
		}
	}()
	return ctx, browser, func() { cancel(); stop(); <-done }
}

type nativePreviewCatalog struct {
	previews   agentapi.PreviewsLookup
	ctx        context.Context
	files      nativesessions.FileSystem
	sources    agentapi.SourcesLookup
	candidates []nativesessions.Candidate
	next       int
	reserved   int64
	rows       []listRow
	stderr     io.Writer
	exhausted  bool
	now        time.Time
}

func (n *nativePreviewCatalog) load() (bool, error) {
	if err := n.ctx.Err(); err != nil {
		return false, err
	}
	if n.exhausted {
		return false, nil
	}
	type job struct {
		index int
		c     nativesessions.Candidate
	}
	type answer struct {
		index int
		p     collector.TranscriptPreview
		err   error
	}
	var batch []job
	for n.next < len(n.candidates) && len(batch) < nativePreviewBatch {
		c := n.candidates[n.next]
		cost := min(c.Stamp.Size, nativeWindowBytes)
		if c.Stamp.Size > nativeWindowBytes {
			cost += min(c.Stamp.Size, nativeWindowBytes) + 1
		}
		if cost > nativeReadBudget-n.reserved {
			n.exhausted = true
			terminal.Println(n.stderr, "handoff: cumulative local read budget exhausted; remaining verified sessions are shown by native ID without preview labels")
			break
		}
		n.reserved += cost
		batch = append(batch, job{n.next, c})
		n.next++
	}
	jobs := make(chan job, 2)
	results := make(chan answer, 2)
	var workers sync.WaitGroup
	for range 2 {
		workers.Go(func() {
			for j := range jobs {
				p, err := previewNativeCandidate(n.ctx, n.files, j.c, n.sources, n.previews)
				a := answer{index: j.index, p: p, err: err}
				select {
				case results <- a:
				case <-n.ctx.Done():
					return
				}
			}
		})
	}
	go func() {
		defer close(jobs)
		for _, j := range batch {
			select {
			case jobs <- j:
			case <-n.ctx.Done():
				return
			}
		}
	}()
	go func() { workers.Wait(); close(results) }()
	got := map[int]answer{}
	for a := range results {
		got[a.index] = a
	}
	if err := n.ctx.Err(); err != nil {
		return false, err
	}
	partial, unavailable := 0, 0
	for _, j := range batch {
		a := got[j.index]
		c := j.c
		title := a.p.Name
		if title == "" {
			title = a.p.Title
		}
		if title == "" {
			title = c.NativeID
		}
		row := n.identityRow(j.index)
		row.Title = archive.DisplayLine(title)
		row.fields = sessionFields{Name: a.p.Name, Title: a.p.Title, Branch: a.p.Branch, Project: row.Project, Harness: c.Ref.Harness, SessionID: c.NativeID}
		if a.err != nil {
			row.Hint = "preview unavailable"
			unavailable++
		} else if !a.p.NameComplete {
			row.Hint = "preview partial"
			partial++
		}
		n.rows = append(n.rows, row)
	}
	if partial > 0 || unavailable > 0 {
		terminal.Printf(n.stderr, "handoff: this preview batch has %d partial labels and %d unavailable labels; word searches may miss uninspected text\n", partial, unavailable)
	}
	if n.exhausted {
		for i := n.next; i < len(n.candidates); i++ {
			row := n.identityRow(i)
			row.Hint = "label not inspected"
			n.rows = append(n.rows, row)
		}
	}
	terminal.Printf(n.stderr, "handoff: labels inspected for %d of %d discovered local sessions; word searches cover only these loaded previews\n", n.next, len(n.candidates))
	return n.next < len(n.candidates) && !n.exhausted, nil
}

// identityRow uses only verified discovery facts; it never reads a transcript.
func (n *nativePreviewCatalog) identityRow(index int) listRow {
	c := n.candidates[index]
	row := listRow{Index: len(n.rows) + 1, selectionKey: fmt.Sprintf("native-%d", index), SessionID: c.NativeID, ShortID: shortSessionID(c.NativeID), HarnessKey: c.Ref.Harness, Harness: c.Ref.Harness, Title: c.NativeID, Project: archive.DisplayLine(filepath.Base(c.Directory)), ProjectID: c.Directory}
	row.setActive(n.now, c.ModifiedAt)
	row.fields = sessionFields{Project: row.Project, Harness: c.Ref.Harness, SessionID: c.NativeID}
	return row
}

func nativeIdentity(candidates []nativesessions.Candidate, query, harness string) (*nativesessions.Candidate, error) {
	var exact, prefix []nativesessions.Candidate
	for _, c := range candidates {
		if harness != "" && c.Ref.Harness != harness {
			continue
		}
		if c.NativeID == query {
			exact = append(exact, c)
		} else if len(query) >= minIDPrefixWord && strings.HasPrefix(strings.ToLower(c.NativeID), strings.ToLower(query)) {
			prefix = append(prefix, c)
		}
	}
	matches := exact
	if len(matches) == 0 {
		matches = prefix
	}
	if len(matches) > 1 {
		return nil, errors.New("local native identity is ambiguous or conflicts across transcripts; specify --harness or use --file PATH --harness NAME")
	}
	if len(matches) == 1 {
		return &matches[0], nil
	}
	return nil, nil
}

func nativeCurrent(candidates []nativesessions.Candidate, harness string, env currentSessionDependencies) (*nativesessions.Candidate, error) {
	var found *nativesessions.Candidate
	for _, observation := range runtimeObservations(env) {
		if harness != "" && harness != string(observation.Agent) {
			continue
		}
		value := observation.NativeID
		if value == "" {
			continue
		}
		// Current identity is exact; short prefixes never identify a running agent.
		var matches []nativesessions.Candidate
		for _, c := range candidates {
			if c.Ref.Harness == string(observation.Agent) && c.NativeID == value {
				matches = append(matches, c)
			}
		}
		if len(matches) > 1 || len(matches) == 1 && found != nil {
			return nil, errors.New("current local native identity is ambiguous; name a session explicitly")
		}
		if len(matches) == 1 {
			c := matches[0]
			found = &c
		}
	}
	return found, nil
}

func resolveNativeHandoff(opts handoffOptions, interactive bool, input *typedInput, stdout, stderr io.Writer, env nativeHandoffDependencies) (handoffTarget, bool, int) {
	fail := func(err error) (handoffTarget, bool, int) {
		terminal.Printf(stderr, "agent-archive: handoff: %v\n", err)
		return handoffTarget{}, false, 1
	}
	if opts.source == "archive" {
		return fail(errors.New("archive access requires setup; native local sessions are available with --source local"))
	}
	if opts.harness == archive.HarnessCursor {
		return fail(errors.New("automatic local discovery searches Claude Code and Codex; use --file PATH --harness cursor for a Cursor transcript"))
	}
	pruneNativeHandoffs(env.tempDir(), env.now())
	ctx, signals, stop := nativeCommandContext(env)
	defer stop()
	dir := opts.project
	var err error
	if dir == "" {
		dir, err = env.workingDir()
	}
	if err != nil {
		return fail(err)
	}
	if !opts.allProjects {
		dir, err = filepath.Abs(dir)
		if err != nil {
			return fail(err)
		}
		canonical, resolveErr := env.nativeFiles().EvalSymlinks(dir)
		if resolveErr != nil {
			return fail(errors.New("before setup --project needs an existing directory, not a configured project label"))
		}
		dir = canonical
		if info, e := env.nativeFiles().Lstat(dir); e != nil || !info.IsDir() {
			return fail(errors.New("before setup --project needs an existing directory, not a configured project label"))
		}
	}
	roots, err := env.nativeRoots(opts.harness)
	if err != nil {
		return fail(err)
	}
	apps := ""
	if d, ok := catalogFor(env).Lookup(opts.harness); ok {
		apps = d.DisplayName
	}
	if apps == "" {
		apps = "Claude Code and Codex"
	}

	terminal.Printf(stderr, "handoff: local source; automatic archiving is not configured. Searching %s native stores.\n", apps)
	result, err := nativesessions.Discover(ctx, registryFor(env), env.nativeFiles(), roots, nativesessions.Scope{Directories: []string{dir}, All: opts.allProjects}, nativesessions.Limits{Files: 10000, HeaderBytes: nativeWindowBytes, RecordBytes: nativeWindowBytes, TotalBytes: nativeReadBudget, Workers: 2})
	if err != nil {
		return fail(err)
	}
	if !result.Coverage.IdentityComplete {
		terminal.Printf(stderr, "handoff: local discovery incomplete (%s); loading labels cannot recover uninspected identities\n", result.Coverage.Reason)
	}
	candidates := result.Candidates
	if len(candidates) == 0 {
		return fail(errors.New("no verified local sessions in the selected checkout; use --all-projects to search other projects, or --file PATH --harness NAME"))
	}
	var selected *nativesessions.Candidate
	if opts.latest && !result.Coverage.IdentityComplete {
		terminal.Println(stderr, "handoff: --latest requires complete local identity and checkout discovery; select a known native ID explicitly")
		if !interactive {
			for i, c := range candidates {
				if i >= handoffCandidateLimit {
					terminal.Printf(stderr, "handoff: showing %d of %d verified candidates; use the terminal picker for the bounded catalog\n", handoffCandidateLimit, len(candidates))
					break
				}
				terminal.Printf(stderr, "  agent-archive handoff %s --harness %s --source local --project %s\n", shellQuote(c.NativeID), shellQuote(c.Ref.Harness), shellQuote(c.Directory))
			}
			return handoffTarget{}, false, 1
		}
		// Refused latest must reach an explicit picker even when --to and a
		// current-session identity are present.
	} else {
		selected, err = selectKnownNative(opts, result, interactive, env)
	}
	if err != nil {
		return fail(err)
	}
	if selected == nil {
		var picked bool
		var code int
		selected, picked, code, err = chooseNativePreviews(ctx, opts, result, interactive, signals, input, stdout, stderr, env)
		if err != nil {
			return fail(err)
		}
		if code != 0 || !picked {
			return handoffTarget{}, false, code
		}
	}
	// Duplicate identities cannot become safe merely by choosing a numbered row.
	if _, e := nativeIdentity(candidates, selected.NativeID, selected.Ref.Harness); e != nil {
		return fail(e)
	}
	target, err := handoffFromNative(ctx, *selected, env.nativeFiles(), env)
	if err != nil {
		return fail(err)
	}
	if opts.latest && result.Coverage.IdentityComplete {
		target.describe = fmt.Sprintf("local %s (%s), newest local transcript modification time %s", selected.NativeID, selected.Ref.Harness, selected.ModifiedAt.Format("2006-01-02 15:04:05 MST"))
	}
	return target, true, 0
}

func nativeRowIndex(row listRow, candidates []nativesessions.Candidate) int {
	for i := range candidates {
		if row.selectionKey == fmt.Sprintf("native-%d", i) {
			return i
		}
	}
	return -1
}

func handoffFromNative(ctx context.Context, c nativesessions.Candidate, files nativesessions.FileSystem, clock handoffFileDependencies) (_ handoffTarget, resultErr error) {
	pass, snapshot, s, err := openNativeSource(ctx, files, c, registryFor(clock))
	if err != nil {
		return handoffTarget{}, err
	}
	defer func() { resultErr = errors.Join(resultErr, snapshot.Close(), pass.Close()) }()
	if !c.Stamp.SameFile(s.Stamp()) {
		return handoffTarget{}, transcriptio.ErrChanged
	}
	h, _, err := nativesessions.InspectNative(ctx, registryFor(clock), s, c.Ref, nativeWindowBytes, nativeWindowBytes)
	if err != nil {
		return handoffTarget{}, err
	}
	dir, e := files.EvalSymlinks(h.Directory)
	if e != nil || h.IdentityMismatch || h.NativeID != c.NativeID || dir != c.Directory {
		return handoffTarget{}, errors.New("selected local transcript identity or checkout changed; discover again")
	}
	filtered, adapter, err := collector.FilterTranscriptSnapshot(ctx, s, c.Ref.Harness, c.StartedAt, collector.DefaultMaxTranscriptBytes, registryFor(clock))
	if err != nil {
		return handoffTarget{}, err
	}
	if c.StartedAt.IsZero() {
		c.StartedAt = c.ModifiedAt
	}
	sum := sha256.Sum256([]byte(c.Ref.Harness + "\x00" + c.NativeID))
	reg := archive.SessionRegistration{ArchiveSessionID: hex.EncodeToString(sum[:16]), NativeSessionID: c.NativeID, ProjectRoot: c.Directory, ProjectID: "local", Harness: archive.Harness{Name: c.Ref.Harness}, SessionStartedAt: c.StartedAt}
	bundle, err := archive.NewSourceBundle(reg, adapter, filtered, clock.now().UTC(), nil)
	if err != nil {
		return handoffTarget{}, err
	}
	target, err := analyzeTarget(ctx, parsersFor(clock), handoffTarget{bundle: bundle, source: "local", native: &c, startedAt: c.StartedAt, lastActivityAt: s.Stamp().ModifiedAt})
	if err != nil {
		return handoffTarget{}, err
	}
	if target.analysis.Facts.IdentityConflict {
		return handoffTarget{}, errors.New("selected local transcript has conflicting native identity; use --file PATH --harness NAME to inspect it explicitly")
	}
	return target, nil
}

// Native headers are bounded; the complete selected record must not introduce
// another top-level identity beyond that inspected window.

func selectKnownNative(opts handoffOptions, result nativesessions.Result, interactive bool, env currentSessionDependencies) (*nativesessions.Candidate, error) {
	candidates := result.Candidates
	if opts.latest {
		if !result.Coverage.IdentityComplete {
			return nil, errors.New("--latest requires complete local identity and checkout discovery; select a known native ID explicitly")
		}
		observations := runtimeObservations(env)
		for _, c := range candidates {
			if !isCurrentNative(c, observations) {
				return &c, nil
			}
		}
		return nil, errors.New("no other local session in the selected checkout")
	}
	if opts.sessionID != "" {
		return nativeIdentity(candidates, opts.sessionID, opts.harness)
	}
	if opts.to != "" {
		selected, err := nativeCurrent(candidates, opts.harness, env)
		if err != nil {
			return nil, err
		}
		if selected == nil && !interactive {
			return nil, errors.New("current native session could not be identified; " + noCurrentSessionMessage)
		}
		return selected, nil
	}
	return nil, nil
}

func isCurrentNative(c nativesessions.Candidate, observations []agentapi.AgentRuntime) bool {
	for _, observation := range observations {
		if observation.NativeID != "" && string(observation.Agent) == c.Ref.Harness && observation.NativeID == c.NativeID {
			return true
		}
	}
	return false
}

func chooseNativePreviews(ctx context.Context, opts handoffOptions, result nativesessions.Result, interactive bool, signals <-chan os.Signal, input *typedInput, stdout, stderr io.Writer, env nativeHandoffDependencies) (*nativesessions.Candidate, bool, int, error) {
	candidates := result.Candidates
	var selected *nativesessions.Candidate
	n := &nativePreviewCatalog{previews: previewsFor(env), sources: registryFor(env), ctx: ctx, files: env.nativeFiles(), candidates: candidates, reserved: result.Coverage.ReservedBytes, stderr: stderr, now: env.now()}
	more, e := n.load()
	if e != nil {
		return nil, false, 1, e
	}
	query := opts.sessionID
	if query != "" {
		var matches []listRow
		q := parseSessionQuery(query)
		for _, row := range n.rows {
			if q.matches(row.fields) {
				matches = append(matches, row)
			}
		}
		if len(matches) == 1 {
			index := nativeRowIndex(matches[0], candidates)
			selected = &candidates[index]
		} else if !interactive {
			for i, row := range matches {
				if i >= handoffCandidateLimit {
					break
				}
				terminal.Printf(stderr, "  local %s (%s): %s\n", row.SessionID, row.HarnessKey, row.Title)
			}
			return nil, false, 1, fmt.Errorf("%d matches in %d loaded local previews; search is bounded, not exhaustive; name a native ID or use the terminal picker to load older sessions", len(matches), n.next)
		}
	}
	if selected == nil {
		if !interactive {
			return nil, false, 1, errors.New(noSelectorMessage)
		}
		format := listFormatOptions{Now: env.now(), Numbered: true, Style: styleFor(stdout), NarrowHint: "Native local IDs: agent-archive handoff NATIVE_ID --harness NAME --source local."}
		choices := rowChoices(n.rows, format)
		var older *browserLoadAction
		if more {
			older = &browserLoadAction{Label: "Load older sessions (50)", Load: func() (bool, error) {
				stop := startActivity(stdout, "Inspecting older local previews…")
				defer stop()
				more, e := n.load()
				choices.rowsFor = func(sessionScope) scopeView {
					return scopeView{rows: n.rows, total: len(candidates), truncated: n.next < len(candidates)}
				}
				choices.built = [2]*scopeChoice{}
				return more, e
			}}
		}
		row, picked, code := runBrowser(ctx, nativeBrowserSignals{env, signals}, input.prompter(stdout), stdout, stderr, browserSpec{Mode: pickSession, Verb: "Hand off local", Choices: choices, Query: query, Command: "handoff", LoadOlder: older, Notice: "Only loaded local previews are searched; use --all-projects to broaden checkout scope."})
		if code != 0 || !picked {
			return nil, false, code, nil
		}
		index := nativeRowIndex(row, candidates)
		if index < 0 {
			return nil, false, 1, errors.New("unknown local selection")
		}
		selected = &candidates[index]
	}
	return selected, true, 0, nil
}

func previewNativeCandidate(ctx context.Context, files nativesessions.FileSystem, c nativesessions.Candidate, sources agentapi.SourcesLookup, previews agentapi.PreviewsLookup) (_ collector.TranscriptPreview, resultErr error) {
	pass, snapshot, s, err := openNativeSource(ctx, files, c, sources)
	if err != nil {
		return collector.TranscriptPreview{}, err
	}
	defer func() { resultErr = errors.Join(resultErr, snapshot.Close(), pass.Close()) }()
	if !c.Stamp.SameFile(s.Stamp()) || s.Stamp().Size != c.Stamp.Size {
		return collector.TranscriptPreview{}, transcriptio.ErrChanged
	}
	if previews == nil {
		return collector.TranscriptPreview{}, errors.New("preview unavailable")
	}
	preview, ok := previews.LookupPreview(c.Ref.Harness)
	if !ok {
		return collector.TranscriptPreview{}, errors.New("preview unavailable")
	}
	return collector.PreviewTranscript(ctx, s, preview, collector.PreviewLimits{HeadBytes: nativeWindowBytes, TailBytes: nativeWindowBytes, RecordBytes: nativeWindowBytes}, c.NativeID)
}

// Each browser worker owns its own serial pass; no shared mutable provider resource enters the pool.
func openNativeSource(ctx context.Context, files nativesessions.FileSystem, c nativesessions.Candidate, sources agentapi.SourcesLookup) (agentapi.SourcePass, agentapi.SourceSnapshot, agentapi.FileInput, error) {
	p, _, ok := sources.LookupSources(c.Ref.Harness)
	if !ok {
		return nil, nil, nil, errors.New("native source integration unavailable")
	}
	pass, err := p.OpenPass(ctx, agentapi.SourceEnvironment{Files: files, Policy: transcriptio.OpenPolicy{RejectSymlinks: true, Root: c.Ref.Store}})
	if err != nil {
		return nil, nil, nil, err
	}
	snapshot, err := pass.Read(ctx, agentapi.SourceRef{Path: c.Ref.Path}, agentapi.ReadLimits{})
	if err != nil {
		return nil, nil, nil, errors.Join(err, pass.Close())
	}
	return pass, snapshot, snapshot.Input().File, nil
}
