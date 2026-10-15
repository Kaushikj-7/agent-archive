package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/search"
	"github.com/wangjohn/agent-archive/internal/setupjournal"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

func executeContentSearch(opts contentSearchOptions, stdout, stderr io.Writer, env searchCommandDependencies) int {
	kind := contentSearchResults
	if opts.read != "" {
		kind = contentSearchContextRead
	} else if !opts.hasQuery {
		kind = contentSearchRefreshOnly
	}
	doc := contentSearchDocument{SchemaVersion: 1, Kind: kind, OK: true, Results: []contentSearchHit{}, Query: opts.query.Text, Limit: opts.query.Limit}

	home, err := env.readHome()
	if err != nil {
		setSearchError(&doc, err)
		return writeContentSearch(doc, opts, stdout, stderr)
	}
	cfg, found, err := config.Load(home)
	if err == nil && !found {
		err = errors.New(strings.TrimSpace(notSetUpMessage))
	}
	if err != nil {
		setSearchError(&doc, err)
		return writeContentSearch(doc, opts, stdout, stderr)
	}
	namespace, dir := contentSearchLocation(home, cfg)
	ctx, stop := readCommandContext(env)
	defer stop()
	if opts.read != "" {
		return readSearchContext(ctx, env, cfg, namespace, opts, doc, stdout, stderr)
	}
	if opts.hasQuery {
		f := &contentSearchFiltersDocument{Harness: opts.query.Harness, Replays: string(opts.query.Replay), Children: string(opts.query.Children)}
		if f.Replays == "" {
			f.Replays = "include"
		}
		if f.Children == "" {
			f.Children = "include"
		}
		if !opts.query.From.IsZero() {
			f.Since = &opts.query.From
		}
		if !opts.query.To.IsZero() {
			f.Until = &opts.query.To
		}
		doc.Filters = f
	}

	if opts.hasQuery {
		scope, e := scopeFor(env, opts.project, opts.all)
		if e != nil {
			setSearchError(&doc, e)
			return writeContentSearch(doc, opts, stdout, stderr)
		}
		if !opts.all && !scope.narrowed() {
			setSearchError(&doc, searchFailure("scope_required"))
			doc.Error.Message = "choose --project or --all-projects outside a known project"
			return writeContentSearch(doc, opts, stdout, stderr)
		}
		opts.query.Scope = search.Scope{Restricted: scope.narrowed(), Named: scope.Dir == "", RepoKey: scope.RepoKey, ProjectName: scope.Label, ProjectIDs: scope.ProjectIDs}
		doc.Scope = &contentSearchScope{scope.Label, opts.all, append([]string{}, scope.ProjectIDs...), scope.RepoKey}
	}
	if opts.refresh {
		doc.Refresh, doc.Coverage, err = refreshContentSearch(ctx, home, cfg, env, opts)
		setSearchError(&doc, err)
	}
	if opts.hasQuery {
		queryCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		index, e := search.OpenRead(queryCtx, dir, namespace)
		if os.IsNotExist(e) {
			doc.Coverage = contentSearchCoverageOf(search.Observation{}, false)
		} else if e != nil {
			setSearchError(&doc, e)
		} else {
			page, e := index.Search(queryCtx, opts.query)
			if e == nil && !opts.json && opts.stdin != nil && browseInteractive(env, opts.stdin, stdout) {
				if doc.Error != nil {
					searchCommandError(stderr, errors.New(doc.Error.Message))
				}
				row, picked, input, code := runContentSearchBrowser(ctx, env, cfg, namespace, index, page, opts, stdout, stderr)
				_ = index.Close()
				if code != 0 || !picked {
					return contentSearchBrowserExit(code, doc.Error)
				}
				// The search store and all browser reads are closed before handing
				// signal and prompt ownership to the ordinary handoff pipeline.
				stop()
				code = runHandoffWithInput([]string{row.SessionID, "--harness", row.HarnessKey, "--source", "archive"}, opts.stdin, stdout, stderr, env, input)
				return contentSearchBrowserExit(code, doc.Error)
			}
			_ = index.Close()
			if e != nil {
				setSearchError(&doc, e)
			} else {
				setSearchPage(&doc, page)
			}
		}
	}
	return writeContentSearch(doc, opts, stdout, stderr)
}

// A cached browser or handoff success cannot erase a requested refresh failure.
func contentSearchBrowserExit(code int, failure *contentSearchError) int {
	if code == 0 && failure != nil {
		return 1
	}
	return code
}

// runContentSearchPass owns the stable home lifecycle lock before opening an
// index or rechecking configuration. S5 shares this lock with lifecycle changes.
type contentSearchPass struct {
	Discovery   search.Discovery
	Progress    search.Progress
	Observation search.Observation
	Complete    bool
	Observed    bool
	Rebuilt     bool
}

func runContentSearchPass(ctx context.Context, home string, expected config.Config, env searchCommandDependencies, opts contentSearchOptions) (contentSearchPass, error) {
	var result contentSearchPass
	var discovery search.Discovery
	var progress search.Progress
	var observation search.Observation
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	release, err := local.NamedLock(home, "search-worker.lock")
	if errors.Is(err, local.ErrBusy) {
		err = search.ErrWorkBusy
	}
	if err != nil {
		return result, err
	}
	defer release()
	if err = ctx.Err(); err != nil {
		return result, err
	}
	cfg, err := loadSearchPassConfig(home, expected, opts.automatic)
	if err != nil {
		return result, err
	}

	if opts.automatic {
		_ = local.TrimLog(filepath.Join(home, "search-error.log"), errorLogMaxBytes, errorLogKeepBytes)
		_ = local.TrimLog(filepath.Join(home, "search.log"), errorLogMaxBytes, errorLogKeepBytes)
	}
	namespace, dir := contentSearchLocation(home, cfg)
	if opts.automatic {
		if err = admitAutomaticSearch(ctx, dir, namespace); err != nil {
			return result, err
		}
	}
	index, err := search.Open(ctx, dir, namespace)
	if err != nil {
		return result, err
	}
	defer func() { _ = index.Close() }()
	if opts.rebuild {
		if err = index.Rebuild(ctx); err != nil {
			return result, err
		}
		result.Rebuilt = true
	}
	objects, err := env.openStore(cfg)
	if err != nil {
		return result, err
	}
	sources, sessions := 0, 0
	if opts.session != "" {
		key, e := archive.MetadataObjectKey(opts.query.Harness, opts.session)
		if e != nil {
			return result, e
		}
		metadata, e := search.ReadCurrentMetadata(ctx, objects, key)
		if e != nil {
			if errors.Is(e, search.ErrDeleted) {
				if err = index.ForgetSession(ctx, opts.query.Harness, opts.session); err != nil {
					return result, err
				}
				result.Observation, err = index.Observation(ctx)
				result.Observed = err == nil
			}
			return result, e
		}
		if metadata.SessionID != opts.session || metadata.Harness.Name != opts.query.Harness {
			return result, errors.New("search metadata disagrees with requested session")
		}
		if e = index.Observe(ctx, metadata, parsersFor(env), env.now(), 0); e != nil {
			return result, e
		}
		sources, sessions = 64, 64
	} else {
		discovery, err = index.Reconcile(ctx, objects, parsersFor(env), env.now(), search.ReconcileBudget{})
	}
	var runErr error
	progress, runErr = index.Run(ctx, objects, parsersFor(env), env.now(), search.Budget{Sources: sources, Sessions: sessions, VerifyCurrent: true})
	if runErr != nil {
		err = runErr
	}
	observation, readErr := index.Observation(ctx)
	result.Observed = readErr == nil
	if readErr != nil && err == nil {
		err = readErr
	}
	complete := observation.Complete
	if opts.session != "" {
		complete, readErr = index.TargetComplete(ctx, opts.query.Harness, opts.session)
		if readErr != nil && err == nil {
			err = readErr
		}
	}
	if err == nil && opts.session == "" && progress.Failed > 0 {
		err = searchFailure("refresh_failed")
	}
	// Historical coverage can remain complete when this refresh attempt fails.
	result.Discovery, result.Progress, result.Observation, result.Complete = discovery, progress, observation, complete && err == nil
	return result, err
}

// admitAutomaticSearch permits fresh initialization, including a first Open
// interrupted before its atomic schema transaction. Existing versions only
// migrate through an explicit public refresh/rebuild.
func admitAutomaticSearch(ctx context.Context, dir, namespace string) error {
	index, err := search.OpenRead(ctx, dir, namespace)
	if os.IsNotExist(err) || errors.Is(err, search.ErrUninitialized) {
		return nil
	}
	if err != nil {
		return err
	}
	return index.Close()
}

func loadSearchPassConfig(home string, expected config.Config, automatic bool) (config.Config, error) {
	if setupjournal.TransactionPending(home) {
		if automatic {
			return config.Config{}, errAutomaticSearchSkipped
		}
		return config.Config{}, errors.New(recoveryPending(home))
	}
	cfg, found, err := config.Load(home)
	if err != nil {
		return config.Config{}, err
	}
	if automatic && (!found || !cfg.SearchIndexing || !cfg.Archive.Enabled || cfg.Paused) {
		return config.Config{}, errAutomaticSearchSkipped
	}
	namespace, _ := contentSearchLocation(home, cfg)
	expectedNamespace, _ := contentSearchLocation(home, expected)
	if !found || namespace != expectedNamespace {
		if automatic {
			return config.Config{}, errAutomaticSearchSkipped
		}
		return config.Config{}, errors.New("search configuration changed; retry command")
	}
	return cfg, nil
}

func refreshContentSearch(ctx context.Context, home string, cfg config.Config, env searchCommandDependencies, opts contentSearchOptions) (*contentSearchRefresh, *contentSearchCoverage, error) {
	started := time.Now()
	scope := "namespace"
	if opts.session != "" {
		scope = "session"
	}
	r := &contentSearchRefresh{Session: opts.session, Harness: opts.query.Harness, Scope: scope, Rebuild: opts.rebuild, Stopped: "pass_complete"}

	duration := 30 * time.Second
	if opts.wait > 0 {
		duration = opts.wait
	}
	ctx, cancel := context.WithTimeout(ctx, duration)
	defer cancel()
	var coverage *contentSearchCoverage
	for {
		result, err := runContentSearchPass(ctx, home, cfg, env, opts)
		d, p, o, complete := result.Discovery, result.Progress, result.Observation, result.Complete
		if result.Rebuilt {
			opts.rebuild = false
		}
		r.Passes++
		r.Loaded += p.Loaded
		r.Reused += p.Reused
		r.Bytes += p.CompressedBytes
		r.Committed += p.Committed
		r.Metadata += d.Metadata
		r.Nodes += d.Nodes
		r.VerificationChecks += p.MetadataChecks
		r.VerificationNodes += p.CatalogNodes
		r.VerificationBytes += p.MetadataBytes
		r.Completed = complete
		r.Elapsed = time.Since(started).Milliseconds()
		coverage = nil
		if result.Observed {
			coverage = contentSearchCoverageOf(o, true)
		}
		if opts.wait == 0 {
			return r, coverage, err
		}
		if complete && err == nil {
			r.Stopped = "complete"
			return r, coverage, nil
		}
		if errors.Is(ctx.Err(), context.Canceled) {
			r.Stopped = "interrupted"
			return r, coverage, ctx.Err()
		}
		if ctx.Err() != nil {
			r.Stopped = "wait_timeout"
			return r, coverage, searchFailure("wait_timeout")
		}
		if err != nil && !errors.Is(err, search.ErrWorkBusy) {
			r.Stopped = "refresh_failed"
			return r, coverage, err
		}
		if err == nil && o.Pending == 0 && !o.DiscoveryComplete && d.CompletedWalk {
			r.Stopped = "coverage_incomplete"
			return r, coverage, searchFailure("coverage_incomplete")
		}
		if err == nil && o.Pending == 0 && o.Unsupported > 0 {
			r.Stopped = "coverage_incomplete"
			return r, coverage, searchFailure("coverage_incomplete")
		}
		if errors.Is(err, search.ErrWorkBusy) || (p.Loaded == 0 && p.Committed == 0 && d.Metadata == 0 && d.Objects == 0) {
			timer := time.NewTimer(time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
			case <-timer.C:
			}
		}
	}
}

func readSearchContext(ctx context.Context, env searchCommandDependencies, cfg config.Config, namespace string, opts contentSearchOptions, doc contentSearchDocument, stdout, stderr io.Writer) int {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	ref := opts.reference
	if ref.Source.Destination != namespace {
		setSearchError(&doc, searchFailure("reference_context_mismatch"))
		doc.Error.Message = "search reference belongs to another destination/privacy context"
		return writeContentSearch(doc, opts, stdout, stderr)
	}
	objects, err := env.openStore(cfg)
	if err != nil {
		setSearchError(&doc, err)
		return writeContentSearch(doc, opts, stdout, stderr)
	}
	offset := opts.offset
	if offset < 0 {
		offset = ref.Start
	}
	out, err := search.ReadContext(ctx, objects, parsersFor(env), ref, offset, opts.maxBytes, opts.before, opts.after, reader.Limits{})
	if err != nil {
		setSearchError(&doc, err)
		return writeContentSearch(doc, opts, stdout, stderr)
	}
	slice := func(ref search.Reference, role string, c search.Context) contentSearchSlice {
		var next *int
		if c.Next != 0 {
			next = &c.Next
		}
		return contentSearchSlice{Reference: encodeSearchReference(ref), Role: role, Text: c.Text, Start: c.Start, End: c.End, Next: next, Complete: c.Next == 0}
	}
	c := &contentSearchContext{Selected: slice(ref, out.Role, out.Selected), Before: []contentSearchSlice{}, After: []contentSearchSlice{}}
	for _, v := range out.Before {
		c.Before = append(c.Before, slice(v.Reference, v.Role, v.Context))
	}
	for _, v := range out.After {
		c.After = append(c.After, slice(v.Reference, v.Role, v.Context))
	}
	doc.Context = c
	doc.Truncated = !c.Selected.Complete
	for _, v := range append(c.Before, c.After...) {
		doc.Truncated = doc.Truncated || !v.Complete
	}
	fitContentSearchContext(&doc, opts.outputBytes)
	if !opts.json {
		terminal.Printf(stderr, "Passage bytes %d–%d; next offset %d (0 means complete).\n", c.Selected.Start, c.Selected.End, func() int {
			if c.Selected.Next != nil {
				return *c.Selected.Next
			}
			return 0
		}())
	}
	return writeContentSearch(doc, opts, stdout, stderr)
}

func fitContentSearchContext(doc *contentSearchDocument, outputBytes int) {
	c := doc.Context
	// Shrink only raw UTF-8 text; encoded identity and offsets remain untouched.
	for {
		data, e := jsonDocument(doc)
		if e != nil {
			setSearchError(doc, e)
			break
		}
		if len(data) <= outputBytes {
			break
		}
		var largest *contentSearchSlice
		for _, v := range append(append([]*contentSearchSlice{&c.Selected}, slicePointers(c.Before)...), slicePointers(c.After)...) {
			if largest == nil || len(v.Text) > len(largest.Text) {
				largest = v
			}
		}
		if largest == nil || len(largest.Text) <= 4 {
			break
		}
		n := len(largest.Text) / 2
		for n > 0 && !utf8.RuneStart(largest.Text[n]) {
			n--
		}
		if n == 0 {
			_, n = utf8.DecodeRuneInString(largest.Text)
		}
		largest.Text = largest.Text[:n]
		largest.End = largest.Start + n
		next := largest.End
		largest.Next = &next
		largest.Complete = false
		doc.Truncated = true
	}
}

func slicePointers(values []contentSearchSlice) []*contentSearchSlice {
	out := make([]*contentSearchSlice, 0, len(values))
	for i := range values {
		out = append(out, &values[i])
	}
	return out
}

func terminalSearchSummary(out *bytes.Buffer, r *contentSearchRefresh, c *contentSearchCoverage) {
	label := "Observed archive slice"
	if r.Session != "" {
		label = "Observed " + archive.DisplayLine(r.Session)
	}
	if c == nil {
		fmt.Fprintf(out, "%s; coverage unavailable; %d indexed in this attempt.\n", label, r.Committed)
		return
	}
	fmt.Fprintf(out, "%s; %d indexed, %d pending, %d failed. Coverage is local to this machine.\n", label, r.Committed, c.Pending, c.Failed)
	printContentSearchCoverage(out, c)
}

func runSearchIndexCommand(args []string, stdout, stderr io.Writer, env searchCommandDependencies) int {
	if len(args) != 1 {
		return runSearchIndexPass(args, stdout, stderr, env)
	}
	if isHelpFlag(args[0]) {
		if err := writeOutput(stdout, []byte("Usage: agent-archive _search-index\n\nRun one bounded search indexing pass when local indexing consent is enabled.\n")); err != nil {
			return searchCommandError(stderr, err)
		}
		return 0
	}
	return runSearchIndexPass(args, stdout, stderr, env)
}

var errAutomaticSearchSkipped = errors.New("automatic search indexing skipped")

func runSearchIndexPass(args []string, _ io.Writer, stderr io.Writer, env searchCommandDependencies) int {
	fs := env.newCommandFlags("_search-index", stderr)
	if !fs.parseFlagsOnly(args) {
		return 2
	}
	home, err := env.readHome()
	if err != nil {
		return searchCommandError(stderr, err)
	}
	cfg, found, err := config.Load(home)
	if err != nil {
		return searchCommandError(stderr, err)
	}
	if !found || !cfg.SearchIndexing || !cfg.Archive.Enabled || cfg.Paused {
		return 0
	}
	ctx, stop := readCommandContext(env)
	defer stop()
	_, err = runContentSearchPass(ctx, home, cfg, env, contentSearchOptions{automatic: true, refresh: true})
	if errors.Is(err, search.ErrWorkBusy) || errors.Is(err, errAutomaticSearchSkipped) {
		return 0
	}
	if err != nil {
		return searchCommandError(stderr, err)
	}
	return 0
}
