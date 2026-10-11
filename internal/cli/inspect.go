package cli

import (
	"context"
	"encoding/hex"
	"flag"
	"fmt"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

// archiveSessionsPrefix is the provider-relative prefix every published
// session lives under; see archive.MetadataObjectKey and
// archive.SourceObjectKey, which both produce "sessions/<harness>/<id>/...".
const archiveSessionsPrefix = "sessions"

// notSetUpMessage is what every read-only command prints, to stderr with
// exit 1 like sync and pause, when setup has never run: its output is often
// captured (claude "$(agent-archive handoff --latest)"), and must not pass
// the message off as a result. It is deliberately the same line `status`
// prints, so a first-time user gets one consistent answer.
const notSetUpMessage = "Not set up. Run `agent-archive setup` to get started."

// openReadOnlyStore loads configuration and opens the configured object
// store the same way a collector pass does (env.openStore), but without the
// machine lock or the pause check: `list` and `show` only read remote
// objects and never touch local collector state, so they may run alongside
// a scheduled `_collect` and while collection is paused. found is false,
// with a nil error, when setup has never run. cfg is meaningful only when
// found is true.
func openReadOnlyStore(env readOnlyStoreDependencies) (storage.ObjectStore, config.Config, bool, error) {
	// Read-only: before setup there is nothing to read, and no data
	// directory is created just to say so.
	home, err := env.readHome()
	if err != nil {
		return nil, config.Config{}, false, fmt.Errorf("resolve home: %w", err)
	}
	cfg, found, err := config.Load(home)
	if err != nil {
		return nil, config.Config{}, false, fmt.Errorf("load config: %w", err)
	}
	if !found {
		return nil, config.Config{}, false, nil
	}
	store, err := env.openStore(cfg)
	if err != nil {
		return nil, cfg, true, fmt.Errorf("open storage: %w", err)
	}
	return store, cfg, true, nil
}

// runListCommand implements `agent-archive list`. It reads only metadata
// sidecars (reader.ListMetadataWithOptions downloads no source bundle) and prints only
// metadata fields, so its output can never contain transcript content. A
// listing of every project uses the time-ordered index when complete and
// verifies each displayed sidecar live; full scans, which a scope needs,
// reuse the local metadata cache unless --no-cache.
// Text listings are capped by --limit (default 50; 0 for all) and, on a
// terminal, paged through $PAGER unless --no-pager, --json, or an interactive
// browse (stdin and stdout are both terminals).
func runListCommand(args []string, stdin io.Reader, stdout, stderr io.Writer, env listCommandDependencies) int {
	fs := env.newCommandFlags("list", stderr)
	harness := fs.String("harness", "", "only sessions from this harness (codex, claude, cursor)")
	model := fs.String("model", "", "only sessions that requested or observed this model")
	skill := fs.String("skill", "", "only sessions involving this skill (see --skill-usage)")
	skillSHA256 := fs.String("skill-sha256", "", "only sessions involving this exact lowercase skill SHA-256")
	skillUsage := fs.String("skill-usage", string(reader.SkillUsageUsed), "with --skill/--skill-sha256: used or available")
	since := fs.String("since", "", "only sessions captured at or after this date (2026-01-31), RFC 3339 time, or age (7d, 12h)")
	complete := fs.Bool("complete", false, "only sessions with complete parser coverage and no capture gaps")
	noCache := fs.Bool("no-cache", false, "download every metadata sidecar instead of reusing unchanged ones from the local metadata cache")
	rebuildIndex := fs.Bool("rebuild-index", false, "rebuild the time-ordered listing index from all live metadata sidecars")
	imported := fs.Bool("imported", false, "only sessions agent-archive backfill imported")
	hookCaptured := fs.Bool("hook-captured", false, "only sessions captured by hooks as they ran")
	replays := fs.String("replays", string(replaysHide), replaysFlagUsage)
	limit := fs.Int("limit", defaultListLimit, "show at most this many sessions, newest first (0 for all)")
	noPager := fs.Bool("no-pager", false, "print directly to the terminal; do not page through $PAGER")
	verbose := fs.Bool("verbose", false, "show full session IDs, absolute times, origin, parser, and all models/skills")
	jsonOut := fs.Bool("json", false, "print a versioned JSON document of the matching sessions' metadata")
	allProjects := fs.Bool("all-projects", false, "list every project's sessions, not only the current repository's")
	project := fs.String("project", "", "list this project's sessions: a directory, or a project name (default: the current directory's repository)")
	query, given, ok := fs.parseWithOptionalArgument(args)
	if !ok {
		return 2
	}
	q := parseSessionQuery(query)
	if given && q.empty() {
		return fs.usageError("the search words are empty")
	}
	if *project != "" && *allProjects {
		return fs.usageError("--project and --all-projects cannot be used together: --project lists one project, --all-projects lists every project")
	}
	opts, code := listOptionsFromFlags(fs, listFlagValues{
		harness: *harness, model: *model, skill: *skill, skillSHA256: *skillSHA256,
		skillUsage: *skillUsage, since: *since, complete: *complete,
		imported: *imported, hookCaptured: *hookCaptured, replays: replaysFlag(*replays), limit: *limit,
		noCache: *noCache, noPager: *noPager, verbose: *verbose, jsonOut: *jsonOut,
	}, env.now())
	if code != 0 {
		return code
	}
	store, cfg, found, err := openReadOnlyStore(env)
	if err != nil {
		terminal.Printf(stderr, "agent-archive: list: %v\n", err)
		return 1
	}
	if !found {
		terminal.Println(stderr, notSetUpMessage)
		return 1
	}
	if *rebuildIndex && !rebuildListingIndex(env, store, stderr) {
		return 1
	}
	scope, err := scopeFor(env, *project, *allProjects)
	if err != nil {
		terminal.Printf(stderr, "agent-archive: list: %v\n", err)
		return 1
	}
	browsing := !opts.jsonOut && browseInteractive(env, stdin, stdout)
	// Revision summaries support the implicit repository scope before limit.
	// Explicit project names, searches and interactive scope switching still
	// need exhaustive metadata. Text activity and child counts use summaries.
	full := listRequiresFullScan(opts, *project, scope, q, browsing)
	var stopList func()
	if !opts.jsonOut {
		stopList = startActivity(stdout, "Listing sessions…")
	} else {
		stopList = func() {}
	}
	listOpts := listingReadOptions(env, scope, opts, full, stderr)
	listLimit := opts.limit
	if full {
		listLimit = 0
	}
	listed, err := listRecentWithInterrupts(env, store, opts.filter, listLimit, listOpts)
	stopList()
	if err != nil {
		terminal.Printf(stderr, "agent-archive: list: %v\n", err)
		return 1
	}
	sessions := filterListOrigin(listed.Sessions, opts.imported, opts.hookCaptured)
	if !opts.jsonOut {
		// --json keeps the listing's order, newest capture first, which
		// the index's fast path can give without reading every session.
		sortByActivity(sessions)
	}
	labels := projectLabels(cfg)
	view := listViews{sessions: sessions, listed: listed, full: full, limit: opts.limit, jsonOut: opts.jsonOut, query: q,
		fields: func(m archive.Metadata) sessionFields { return fieldsOf(m, sessionProjectName(m, labels)) }}.view
	if opts.jsonOut {
		return printJSON(stdout, stderr, listJSON(scope, opts.limit, view))
	}
	format := listFormatOptions{
		Now: env.now(), Verbose: opts.verbose, Projects: labels, Style: styleFor(stdout),
		GroupByProject: true, Numbered: browsing, Children: listingChildren(listed, sessions, full),
	}
	words := strings.Join(strings.Fields(query), " ")
	choices := listChoices(scope, format, browsing, sessions, opts.limit, view, words)
	if choices.shown().holdsNothing() {
		if !q.empty() {
			terminal.Printf(stdout, "No archived sessions match %q.\n", queryLabel(query))
		} else {
			terminal.Println(stdout, "No archived sessions match.")
		}
		return 0
	}
	if browsing {
		_, _, code := runBrowser(context.Background(), env, newPrompter(stdin, stdout), stdout, stderr, browserSpec{Mode: browseSessions, Choices: choices, Query: words, Command: "list", Store: store, NoPager: opts.noPager})
		return code
	}
	if err := withPager(context.Background(), stdout, stderr, env, opts.noPager, func(w io.Writer) error {
		return printListTable(w, choices.shown())
	}); err != nil {
		terminal.Printf(stderr, "agent-archive: list: %v\n", err)
		return 1
	}
	return 0
}

// listChoices is what list shows of the archive: for a browser, every
// top-level session, opened with the words in its filter (the sessions the
// search finds decide only which scope it opens on and the note about matches
// elsewhere), and for a table, the sessions the search finds.
func listChoices(scope sessionScope, format listFormatOptions, browsing bool, sessions []archive.Metadata, limit int, view func(sessionScope) listView, words string) *scopeChoices {
	if browsing {
		return newScopeChoices(scope, format, false, browseSearchRows(archiveRows(sessions, limit, format), view, words))
	}
	return newScopeChoices(scope, format, true, func(s sessionScope) scopeView {
		v := view(s)
		return scopeView{rows: formatSessionRows(v.shown, format), total: v.total, truncated: v.truncated, hidden: v.hidden, note: v.note}
	})
}

// browseSearchRows is a browser's rows when it opens with words in its filter:
// the rows it would have without them, in the scope the search settles on. A
// scope the search finds nothing in counts as holding nothing, so the browser
// opens on all projects, and the note about matches elsewhere shows while the
// filter holds those words.
func browseSearchRows(rows scopeRowsFunc, found func(sessionScope) listView, words string) scopeRowsFunc {
	if words == "" {
		return rows
	}
	return func(s sessionScope) scopeView {
		v, hits := rows(s), found(s)
		v.nothing, v.searchNote, v.searchWords = len(hits.shown) == 0, hits.note, words
		return v
	}
}

// listView is what one scope lists.
type listView struct {
	shown []archive.Metadata
	// total counts what the scope holds before --limit, or is -1 when it was
	// not read to the end; truncated is set when --limit cut shown.
	total     int
	truncated bool
	// hidden is how many subagent sessions the table leaves out.
	hidden int
	// outside is how many more sessions match outside the scope.
	outside int
	// note is the footer's line about matches outside the scope.
	note string
}

// listViews builds the views of the listing a `list` run read, one per scope.
type listViews struct {
	// sessions are every session listed, newest first, subagents included.
	sessions []archive.Metadata
	listed   reader.RecentResult
	// full is set when sessions are every match, not the index's newest page.
	full    bool
	limit   int
	jsonOut bool
	query   sessionQuery
	fields  func(archive.Metadata) sessionFields
}

// view is what scope s lists. A search shows the first tier of the search
// that has a match, and only that tier's sessions in the scope, so a scope
// with none of them is empty and the caller moves to all projects. Without
// one, a table lists top-level sessions only, and --json every session.
func (v listViews) view(s sessionScope) listView {
	if !v.full {
		if s.narrowed() && v.listed.ScopeEmpty {
			return listView{}
		}
		return listView{shown: v.sessions, total: v.listed.TotalMatched, truncated: v.listed.TotalMatched > len(v.sessions), hidden: v.listed.Hidden, outside: v.listed.Outside}
	}

	switch {
	case !v.query.empty():
		res := searchSessions(v.sessions, v.query, s, v.fields)
		matches := res.matches
		if s.narrowed() && !res.inScope {
			matches = nil
		}
		shown, total, truncated := applyListLimit(matches, v.limit)
		return listView{shown: shown, total: total, truncated: truncated, outside: res.outside, note: res.outsideNote(s)}
	case !v.jsonOut:
		return topLevelView(v.sessions, s, v.limit)
	case v.full:
		scoped := s.filter(v.sessions)
		shown, total, truncated := applyListLimit(scoped, v.limit)
		return listView{shown: shown, total: total, truncated: truncated, outside: len(v.sessions) - len(scoped)}
	}
	shown, _, _ := applyListLimit(v.sessions, v.limit)
	if !v.listed.Complete {
		return listView{shown: shown, total: -1, truncated: true}
	}
	return listView{shown: shown, total: v.listed.TotalMatched, truncated: v.listed.TotalMatched > len(shown)}
}

// topLevelView is a scope's top-level sessions, cut to limit: subagents are
// left out before the limit counts, and tallied for the footer.
func topLevelView(sessions []archive.Metadata, s sessionScope, limit int) listView {
	scoped := s.filter(sessions)
	top := topLevelSessions(scoped)
	shown, total, truncated := applyListLimit(top, limit)
	return listView{shown: shown, total: total, truncated: truncated, hidden: len(scoped) - len(top)}
}

// listJSON builds the `list --json` document for a scope. Its rows are the
// scope's sessions, or every session when the scope is off or holds none (and
// the scope object says which).
func listJSON(scope sessionScope, limit int, view func(sessionScope) listView) listDocument {
	v := view(scope)
	fellBack, outside := false, 0
	if scope.narrowed() {
		if len(v.shown) == 0 {
			fellBack = true
			v = view(scope.everything())
		} else {
			outside = v.outside
		}
	}
	out := newListDocument(v.shown, limit, v.total, v.truncated)
	if scope.Label != "" {
		out.Scope = &listScope{Label: scope.Label, AllProjects: scope.All || fellBack, FellBack: fellBack, OutsideMatches: outside}
	}
	return out
}

// listFlagValues holds the parsed list flags before validation.
type listFlagValues struct {
	harness      string
	model        string
	skill        string
	skillSHA256  string
	skillUsage   string
	since        string
	complete     bool
	imported     bool
	hookCaptured bool
	replays      replaysFlag
	noCache      bool
	noPager      bool
	verbose      bool
	jsonOut      bool
	limit        int
}

// replaysFlag is a value of --replays.
type replaysFlag string

// The values of --replays, on list and stats. hide, the default, keeps the
// sessions a replay tool ran (archive.ReplayEnv) out of a person's history.
const (
	replaysHide    replaysFlag = "hide"
	replaysInclude replaysFlag = "include"
	replaysOnly    replaysFlag = "only"
)

// replaysFlagUsage is --replays' help.
const replaysFlagUsage = "sessions a replay tool ran (" + archive.ReplayEnv + " set): hide, include, or only"

// replayFilterFlag reads --replays; ok is false for any other value.
func replayFilterFlag(value replaysFlag) (reader.ReplayFilter, bool) {
	switch value {
	case replaysHide:
		return reader.ReplaysHidden, true
	case replaysInclude:
		return reader.ReplaysIncluded, true
	case replaysOnly:
		return reader.ReplaysOnly, true
	}
	return "", false
}

// listOptions is the validated list command configuration.
type listOptions struct {
	filter       reader.Filter
	skillUsage   reader.SkillUsage
	imported     bool
	hookCaptured bool
	noCache      bool
	noPager      bool
	verbose      bool
	jsonOut      bool
	limit        int
}

// listOptionsFromFlags validates list flags and builds the reader filter.
// On a usage error it returns a non-zero exit code.
func listOptionsFromFlags(fs *commandFlags, v listFlagValues, now time.Time) (listOptions, int) {
	if v.limit < 0 {
		return listOptions{}, fs.usageError("--limit must be 0 or more")
	}
	if v.imported && v.hookCaptured {
		return listOptions{}, fs.usageError("choose one of --imported and --hook-captured")
	}
	if v.replays == "" {
		v.replays = replaysHide
	}
	replays, ok := replayFilterFlag(v.replays)
	if !ok {
		return listOptions{}, fs.usageError("--replays must be hide, include, or only, not %q", v.replays)
	}
	canonical, ok := harnessFlagWithCatalog(fs.catalog, v.harness)
	if !ok {
		return listOptions{}, fs.usageError("%s", harnessFlagError(v.harness))
	}
	if v.skillSHA256 != "" && !validLowerSHA256(v.skillSHA256) {
		return listOptions{}, fs.usageError("--skill-sha256 must be exactly 64 lowercase hexadecimal characters")
	}
	// The value is checked before the --skill/--skill-sha256 requirement so
	// that a misspelled value is reported as the misspelling it is, rather
	// than as a missing companion flag.
	usage := reader.SkillUsage(v.skillUsage)
	switch usage {
	case reader.SkillUsageUsed, reader.SkillUsageAvailable:
	case reader.SkillUsageEligibleNoUse:
		return listOptions{}, fs.usageError("--skill-usage eligible_no_use is unsupported: current parsers cannot prove non-use")
	default:
		return listOptions{}, fs.usageError("--skill-usage must be used or available, not %q", v.skillUsage)
	}
	if usage != reader.SkillUsageUsed && v.skill == "" && v.skillSHA256 == "" {
		return listOptions{}, fs.usageError("--skill-usage requires --skill or --skill-sha256")
	}
	var from time.Time
	if v.since != "" {
		parsed, err := parseSince(v.since, now)
		if err != nil {
			return listOptions{}, fs.usageError("--since: %v", err)
		}
		from = parsed
	}
	return listOptions{
		filter: reader.Filter{
			Harness: canonical, Model: v.model, Skill: v.skill, SkillSHA256: v.skillSHA256,
			RequireCompleteCoverage: v.complete, SkillUsage: usage, From: from, Replays: replays,
		},
		skillUsage: usage, imported: v.imported, hookCaptured: v.hookCaptured,
		noCache: v.noCache, noPager: v.noPager, verbose: v.verbose, jsonOut: v.jsonOut, limit: v.limit,
	}, 0
}

// filterListOrigin keeps only imported or only hook-captured sessions when
// one of those flags is set.
func filterListOrigin(sessions []archive.Metadata, imported, hookCaptured bool) []archive.Metadata {
	if !imported && !hookCaptured {
		return sessions
	}
	kept := sessions[:0]
	for _, m := range sessions {
		if (m.Origin == archive.SessionOriginImport) == imported {
			kept = append(kept, m)
		}
	}
	return kept
}

// applyListLimit returns the newest-first prefix of sessions to show.
// total is the match count before the limit; truncated is set when cut short.
func applyListLimit(sessions []archive.Metadata, limit int) (shown []archive.Metadata, total int, truncated bool) {
	total = len(sessions)
	if limit > 0 && len(sessions) > limit {
		return sessions[:limit], total, true
	}
	return sessions, total, false
}

// newListDocument builds the versioned list --json document.
func newListDocument(sessions []archive.Metadata, limit, totalMatched int, truncated bool) listDocument {
	if sessions == nil {
		sessions = []archive.Metadata{}
	}
	var count *int
	if totalMatched >= 0 {
		count = &totalMatched
	}
	return listDocument{
		Version: listSchemaVersion, Sessions: sessions,
		Limit: limit, Returned: len(sessions), TotalMatchedKnown: totalMatched >= 0,
		TotalMatched: count, Truncated: truncated,
	}
}

// harnessFlag checks a --harness value and returns its canonical name, as
// archived metadata records it ("claude-code" is Claude). An empty value
// means no filter.
func harnessFlagWithCatalog(c agentmeta.Catalog, value string) (string, bool) {
	if c == nil {
		c = productionAgents.Catalog()
	}
	if value == "" {
		return "", true
	}
	if d, known := c.Lookup(value); known {
		return string(d.ID), true
	}
	return "", false
}

func harnessFlagError(value string) string {
	return fmt.Sprintf("--harness must be claude, codex, or cursor, not %q", value)
}

// listSchemaVersion versions the `list --json` document.
const listSchemaVersion = 4

// defaultListLimit is how many sessions `list` shows when --limit is omitted.
const defaultListLimit = 50

// listDocument is what `list --json` prints: each matching session's
// metadata sidecar, as `show` prints one, and never conversation content.
// Limit is the --limit value (0 means all). Returned is len(Sessions);
// TotalMatched is how many passed the filters before --limit. Truncated is
// set when Sessions is a prefix of the full match set.
type listDocument struct {
	Version           int                `json:"schema_version"`
	Sessions          []archive.Metadata `json:"sessions"`
	Limit             int                `json:"limit"`
	Returned          int                `json:"returned"`
	TotalMatched      *int               `json:"total_matched,omitempty"`
	TotalMatchedKnown bool               `json:"total_matched_known"`
	Truncated         bool               `json:"truncated,omitempty"`
	// Scope says what part of the archive the listing looked at; absent when
	// the working directory is in no project. Added within schema version 4.
	Scope *listScope `json:"scope,omitempty"`
}

// warnSkippedSidecar reports, on stderr, a metadata sidecar a listing left
// out because it does not validate (damaged, or written by a newer version
// of agent-archive), so stdout keeps its format while the gap is visible.
func warnSkippedSidecar(stderr io.Writer, command string) func(reader.SkippedSidecar) {
	if stderr == nil {
		return nil
	}
	return func(s reader.SkippedSidecar) {
		terminal.Printf(stderr, "agent-archive: %s: warning: skipped a session whose metadata could not be read: %s\n", command, archive.DisplayLine(s.Err.Error()))
	}
}

// listCache opens the disposable metadata cache `list` uses to skip
// downloading sidecars whose ETag has not changed. It holds metadata only.
// The cache is an optimization, so a data directory or cache that cannot be
// opened means an uncached listing, never a failed one.
func listCache(env metadataCacheDependencies, disabled bool) *reader.MetadataCache {
	if disabled {
		return nil
	}
	home, err := env.readHome()
	if err != nil {
		return nil
	}
	cache, err := reader.OpenMetadataCache(home)
	if err != nil {
		return nil
	}
	return cache
}

// sessionOrigin is list's ORIGIN column: imported for a session backfill
// imported, hook for one captured as it ran.
func sessionOrigin(m archive.Metadata) string {
	if m.Origin == archive.SessionOriginImport {
		return "imported"
	}
	// A replay is hook-captured too; its [replay] title mark says it is one.
	return "hook"
}

func validLowerSHA256(value string) bool {
	if len(value) != 64 || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

// runShowCommand implements `agent-archive show SESSION_ID`. By default it
// prints a readable summary of the session's metadata sidecar; --json prints
// the sidecar itself. Conversation content — read from the verified source
// bundle — is printed only when the user passes --transcript (or the
// deprecated --normalized) explicitly, or presses t in the browser, keeping
// the spec's rule that nothing prints transcript contents unless asked.
// With no SESSION_ID on a TTY, it opens the same session browser as list.
func runShowCommand(args []string, stdin io.Reader, stdout, stderr io.Writer, env showCommandDependencies) int {
	fs := env.newCommandFlags("show", stderr)
	harness := fs.String("harness", "", "the session's harness, if the same ID exists under more than one")
	transcript := fs.Bool("transcript", false, "also download and verify the source bundle, and print the conversation (this prints transcript content)")
	full := fs.Bool("full", false, "with --transcript, also print each tool call's trimmed result")
	normalized := fs.Bool("normalized", false, "deprecated: the same as --transcript --json")
	noPager := fs.Bool("no-pager", false, "print the summary or transcript directly; do not page through $PAGER")
	maxBytes := fs.Int("max-bytes", archive.DefaultHandoffMaxBytes, "with --transcript, the output limit in bytes; 0 means no limit")
	jsonOut := fs.Bool("json", false, "print the metadata sidecar as JSON (with --transcript, also the normalized view)")
	// Flags may follow SESSION_ID too (`show SESSION_ID --transcript`).
	sessionID, ok := fs.parseWithArgument(args)
	if !ok {
		return 2
	}
	canonical, ok := harnessFlagWithCatalog(catalogFor(env), *harness)
	if !ok {
		return fs.usageError("%s", harnessFlagError(*harness))
	}
	*harness = canonical
	if *normalized {
		terminal.Println(stderr, "agent-archive: show: --normalized is deprecated; use --transcript --json")
		*transcript, *jsonOut = true, true
	}
	if *full && !*transcript {
		return fs.usageError("--full needs --transcript")
	}
	if code := checkShowMaxBytes(fs, *maxBytes, *transcript); code != 0 {
		return code
	}
	if *full && *jsonOut {
		return fs.usageError("--full is for the readable transcript; --json always includes every retained tool result")
	}

	if sessionID == "" && !browseInteractive(env, stdin, stdout) {
		return fs.usageError("a SESSION_ID is required (see agent-archive list)")
	}
	if sessionID == "" && *transcript {
		if *normalized {
			return fs.usageError("--normalized needs a SESSION_ID; pick a session with show, then run show SESSION_ID --normalized")
		}
		return fs.usageError("--transcript needs a SESSION_ID; pick a session with show and press t, or run show SESSION_ID --transcript")
	}

	store, cfg, found, err := openReadOnlyStore(env)
	if err != nil {
		terminal.Printf(stderr, "agent-archive: show: %v\n", err)
		return 1
	}
	if !found {
		terminal.Println(stderr, notSetUpMessage)
		return 1
	}

	summary := summaryOptions{Now: env.now(), Style: styleFor(stdout), Projects: projectLabels(cfg), Hints: true}
	if sessionID == "" {
		return runBareShow(env, store, cfg, stdin, stdout, stderr, *harness, *jsonOut, *noPager)
	}
	lookup, code := resolveShowQuery(store, env, stdin, stdout, stderr, *harness, sessionID, summary.Projects, *noPager, *transcript || *jsonOut)
	if code != 0 {
		return code
	}
	if lookup.Cancelled {
		return 0
	}
	sessionID, *harness = lookup.SessionID, lookup.Harness
	ctx, stopReads := readCommandContext(env)
	defer stopReads()

	stopShow := startActivity(stdout, "Loading session…")
	key, err := locateMetadataKey(ctx, store, *harness, sessionID)
	if err != nil {
		stopShow()
		terminal.Printf(stderr, "agent-archive: show: %v\n", err)
		return 1
	}

	if !*transcript {
		metadata, err := reader.ReadMetadata(ctx, store, key)
		if err != nil {
			stopShow()
			terminal.Printf(stderr, "agent-archive: show: %v\n", err)
			return 1
		}
		view := metadataWithLinks(ctx, store, metadata)
		stopReads()
		stopShow()
		if *jsonOut {
			return printJSON(stdout, stderr, view)
		}
		if err := withPager(context.WithoutCancel(ctx), stdout, stderr, env, *noPager, func(w io.Writer) error {
			renderSessionSummary(w, view, summary)
			return nil
		}); err != nil {
			terminal.Printf(stderr, "agent-archive: show: %v\n", err)
			return 1
		}
		return 0
	}

	return printSessionTranscript(ctx, store, env, stdout, stderr, key, sessionID, stopShow, stopReads, sessionTranscriptOptions{
		summary: summary, full: *full, json: *jsonOut, normalized: *normalized, noPager: *noPager, maxBytes: *maxBytes,
	})
}

// runBareShow is `show` with no SESSION_ID on a terminal: the browser, or with
// --json one session picked in it and printed as its sidecar.
func runBareShow(env showCommandDependencies, store storage.ObjectStore, cfg config.Config, stdin io.Reader, stdout, stderr io.Writer, harness string, jsonOut, noPager bool) int {
	if jsonOut {
		row, selected, code := selectArchivedSession(env, store, cfg, stdin, stdout, stderr, harness, "show", "Show")
		if code != 0 || !selected {
			return code
		}
		ctx, stopReads := readCommandContext(env)
		defer stopReads()
		view, err := readSessionView(ctx, store, row.HarnessKey, row.SessionID)
		if err != nil {
			terminal.Printf(stderr, "agent-archive: show: %v\n", err)
			return 1
		}
		return printJSON(stdout, stderr, view)
	}
	choices, ok, code := findBrowseSessions(env, store, cfg, stdout, stderr, harness, "show")
	if !ok {
		return code
	}
	_, _, code = runBrowser(context.Background(), env, newPrompter(stdin, stdout), stdout, stderr, browserSpec{Mode: browseSessions, Choices: choices, Command: "show", Store: store, NoPager: noPager})
	return code
}

// checkShowMaxBytes reports a --max-bytes that is negative, or given
// without --transcript, as a usage error (exit code 2); otherwise it is 0.
func checkShowMaxBytes(fs *commandFlags, maxBytes int, transcript bool) int {
	if maxBytes < 0 {
		return fs.usageError("--max-bytes must be 0 or more")
	}
	given := false
	fs.Visit(func(f *flag.Flag) { given = given || f.Name == "max-bytes" })
	if given && !transcript {
		return fs.usageError("--max-bytes needs --transcript")
	}
	return 0
}

// sessionTranscriptOptions are the show flags that shape a transcript.
type sessionTranscriptOptions struct {
	summary    summaryOptions
	full       bool
	json       bool
	normalized bool
	noPager    bool
	maxBytes   int
}

// printSessionTranscript downloads and verifies the session's source bundle
// and prints its transcript: readable and paged, or with --json the sidecar
// and the normalized view. stopShow ends the loading activity line.
func printSessionTranscript(ctx context.Context, store storage.ObjectStore, env showCommandDependencies, stdout, stderr io.Writer, key, sessionID string, stopShow, stopReads func(), opts sessionTranscriptOptions) int {
	view, bundle, err := loadVerifiedSession(ctx, store, key)
	if err != nil {
		stopShow()
		flag := "--transcript"
		if opts.normalized {
			flag = "--normalized"
		}
		terminal.Printf(stderr, "agent-archive: show: %s\n", describeBundleError(err, sessionID, flag))
		return 1
	}
	home, err := env.readHome()
	if err != nil {
		stopShow()
		terminal.Printf(stderr, "agent-archive: show: resolve home: %v\n", err)
		return 1
	}
	pruneHandoffs(home, env.now())
	analysis, parseErr := analyzeSource(ctx, parsersFor(env), bundle)
	// Remote reads are complete; the pager owns interruption from here.
	stopReads()
	if opts.json {
		normalizedView, err := analysis.View, parseErr
		stopShow()
		if err != nil {
			terminal.Printf(stderr, "agent-archive: show: normalized view unavailable: %v\n", err)
			return 1
		}
		turns := make([]normalizedTurn, len(normalizedView.Turns))
		for i, turn := range normalizedView.Turns {
			turns[i] = normalizedTurn{NormalizedTurn: turn}
		}
		normalized := normalizedOutput{Turns: turns, ToolCalls: normalizedView.ToolCalls, ToolResults: normalizedView.ToolResults, HookFinals: normalizedView.HookFinals}
		if opts.maxBytes > 0 {
			if normalized, err = fitNormalizedToLimit(view, normalized, bundle, opts.maxBytes, home, stderr); err != nil {
				terminal.Printf(stderr, "agent-archive: show: %v\n", err)
				return 1
			}
		}
		data, err := normalizedDocuments(view, normalized)
		if err != nil {
			terminal.Printf(stderr, "agent-archive: show: %v\n", err)
			return 1
		}
		if err := writeOutput(stdout, data); err != nil {
			terminal.Printf(stderr, "agent-archive: show: write JSON: %v\n", err)
			return 1
		}
		return 0
	}
	var t archive.Transcript
	if parseErr != nil {
		err = parseErr
	} else {
		t, err = archive.BuildTranscriptWithAnalysis(bundle, analysis, archive.HandoffOptions{ToolResultLines: transcriptResultLines, ToolResultBytes: transcriptResultBytes})
	}
	stopShow()
	if err != nil {
		terminal.Printf(stderr, "agent-archive: show: normalized view unavailable: %v\n", err)
		return 1
	}
	render := transcriptOptions{summaryOptions: opts.summary, Full: opts.full}
	if opts.maxBytes > 0 {
		t, render = fitTranscriptToLimit(view, t, bundle, render, opts.maxBytes, home, stderr)
	}
	if err := withPager(context.WithoutCancel(ctx), stdout, stderr, env, opts.noPager, func(w io.Writer) error {
		renderTranscript(w, view, t, render)
		return nil
	}); err != nil {
		terminal.Printf(stderr, "agent-archive: show: %v\n", err)
		return 1
	}
	return 0
}

// Preserve the sidecar fields while exposing live link availability separately.
// The recorded link status is historical; retention can remove a child later.
//
// `linked_session_availability` is a CLI-only field printed beside the
// sidecar's own fields, so this object is deliberately not an instance of
// metadata.schema.json, which sets additionalProperties:false. The schema
// governs stored metadata objects; `show` renders a view of one, and keeping
// the sidecar's fields at the top level is what existing readers of this
// command already parse. Validate stored objects, not command output.
func metadataWithLinks(ctx context.Context, store storage.ObjectStore, metadata archive.Metadata) sessionView {
	return sessionView{metadata, reader.ResolveLinkedSessions(ctx, store, metadata)}
}

// normalizedOutput is the JSON shape `show --transcript --json` prints for
// archive.NormalizedView, which carries no JSON tags of its own.
// NativeSkillUses is left out: it is an intermediate deriveSkills folds into
// the metadata's skills_used, which the sidecar printed first already shows.
//
// Each tool call carries the tool's name, its retained arguments, and the
// result it was linked to (record index, error flag, and retained output
// size). tool_results lists the results themselves, including any the parser
// could not safely link to a call.
//
// Trimmed appears only when --max-bytes trimmed the view (see fitNormalized).
type normalizedOutput struct {
	Turns       []normalizedTurn                  `json:"turns"`
	ToolCalls   []archive.NormalizedToolCall      `json:"tool_calls"`
	ToolResults []archive.NormalizedToolResult    `json:"tool_results"`
	HookFinals  []archive.HookFinalReconciliation `json:"hook_finals"`
	Trimmed     *trimmedOutput                    `json:"trimmed,omitempty"`
}

// locateMetadataKey resolves an archive session ID to its metadata sidecar
// key. With a harness the key is derived directly (the caller's one Get, no
// listing); without one each known harness's key is read directly, the archive
// is listed only if none exists, and the same ID published under more than
// one harness is reported as ambiguous rather than guessed.
func locateMetadataKey(ctx context.Context, store storage.ObjectStore, harness, sessionID string) (string, error) {
	if harness != "" {
		key, err := archive.MetadataObjectKey(harness, sessionID)
		if err != nil {
			return "", err
		}
		return key, nil
	}
	keys, err := reader.FindMetadataKeys(ctx, store, archiveSessionsPrefix, sessionID)
	if err != nil {
		return "", err
	}
	switch len(keys) {
	case 0:
		return "", fmt.Errorf("no archived session %q (see `agent-archive list`)", sessionID)
	case 1:
		return keys[0], nil
	}
	harnesses := make([]string, 0, len(keys))
	for _, key := range keys {
		harnesses = append(harnesses, strings.Split(strings.TrimPrefix(key, archiveSessionsPrefix+"/"), "/")[0])
	}
	return "", fmt.Errorf("session %q exists under more than one harness (%s); pass --harness", sessionID, strings.Join(harnesses, ", "))
}

// printJSON prints value as indented JSON. Its strings come from bucket
// metadata, so the text goes through archive.DisplayJSON: a C1 control or
// bidi override is printed as a \u escape, never raw to the terminal.
func printJSON(stdout, stderr io.Writer, value any) int {
	data, err := jsonDocument(value)
	if err != nil {
		terminal.Printf(stderr, "agent-archive: %v\n", err)
		return 1
	}
	if err := writeOutput(stdout, data); err != nil {
		terminal.Printf(stderr, "agent-archive: write JSON: %v\n", err)
		return 1
	}
	return 0
}

// parseSince is list's --since: parseTimeArg with a date taken as midnight
// UTC, as metadata capture times are recorded.
func parseSince(value string, now time.Time) (time.Time, error) {
	return parseTimeArg(value, now, time.UTC)
}

// maxAgeDays bounds an age in days (about 270 years), far inside what a
// time.Duration can hold.
const maxAgeDays = 100000

// parseTimeArg reads the time forms every command's --since (and backfill's
// --until) accepts: a calendar date (2026-01-31, midnight in loc), an RFC
// 3339 time, or an age relative to now written as a Go duration (12h, 90m)
// or in whole days (7d).
func parseTimeArg(value string, now time.Time, loc *time.Location) (time.Time, error) {
	value = strings.TrimSpace(value)
	if t, err := time.Parse(time.RFC3339, value); err == nil {
		return t, nil
	}
	if t, err := time.ParseInLocation("2006-01-02", value, loc); err == nil {
		return t, nil
	}
	if daysText, ok := strings.CutSuffix(value, "d"); ok {
		// Past maxAgeDays a time.Duration would overflow.
		if days, err := strconv.Atoi(daysText); err == nil && days >= 0 && days <= maxAgeDays {
			return now.Add(-time.Duration(days) * 24 * time.Hour), nil
		}
	}
	if d, err := time.ParseDuration(value); err == nil && d >= 0 {
		return now.Add(-d), nil
	}
	return time.Time{}, fmt.Errorf("%q is not a date (2026-01-31), an RFC 3339 time, or an age (7d, 12h)", value)
}

// modelNames returns the distinct model names a session's metadata
// attributes to it, whichever side (request or response) reported them.
func modelNames(m archive.Metadata) []string {
	return distinctSorted(func(add func(string)) {
		for _, model := range m.Models {
			add(model.Attributes["gen_ai.request.model"])
			add(model.Attributes["gen_ai.response.model"])
		}
	})
}

func skillNames(m archive.Metadata) []string {
	return distinctSorted(func(add func(string)) {
		for _, skill := range m.SkillsUsed {
			add(skill.Name)
		}
	})
}

func distinctSorted(collect func(add func(string))) []string {
	seen := map[string]bool{}
	var out []string
	collect(func(name string) {
		if name != "" && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	})
	sort.Strings(out)
	return out
}

func listOrDash(names []string) string {
	if len(names) == 0 {
		return "-"
	}
	display := make([]string, len(names))
	for i, name := range names {
		display[i] = archive.DisplayLine(name)
	}
	return strings.Join(display, ",")
}

func listingChildren(listed reader.RecentResult, sessions []archive.Metadata, full bool) map[string]int {
	if !full && listed.Children != nil {
		return listed.Children
	}
	return childCounts(sessions)
}

// listRequiresFullScan keeps queries needing metadata predicates exhaustive.
func listRequiresFullScan(opts listOptions, project string, scope sessionScope, query sessionQuery, browsing bool) bool {
	return opts.limit == 0 || opts.imported || opts.hookCaptured || (project != "" && scope.narrowed()) || !query.empty() || browsing
}

// listingReadOptions builds selection and observation options for one CLI listing.
func listingReadOptions(env listCommandDependencies, scope sessionScope, opts listOptions, full bool, stderr io.Writer) reader.ListOptions {
	var bodyRead func(string, bool)
	if observer, ok := env.(interface{ listBodyObserver() func(string, bool) }); ok {
		bodyRead = observer.listBodyObserver()
	}
	var scopeMatch func(archive.Metadata) bool
	if scope.narrowed() && !full {
		scopeMatch = func(m archive.Metadata) bool { return scope.contains(m, nil) }
	}
	return reader.ListOptions{
		Cache: listCache(env, opts.noCache), Skipped: warnSkippedSidecar(stderr, "list"),
		ActivityOrder: !opts.jsonOut && !full, TopLevelOnly: !opts.jsonOut && !full,
		CompatibilityScan: func(reason string) { terminal.Printf(stderr, "agent-archive: list: %s.\n", reason) },
		BodyRead:          bodyRead, ScopeMatch: scopeMatch,
	}
}

// listRecentWithInterrupts releases the read listener before the browser or pager.
func listRecentWithInterrupts(env listCommandDependencies, store storage.ObjectStore, filter reader.Filter, limit int, opts reader.ListOptions) (reader.RecentResult, error) {
	ctx, stopReads := readCommandContext(env)
	defer stopReads()
	return reader.ListRecent(ctx, store, archiveSessionsPrefix, filter, limit, opts)
}

// rebuildListingIndex reports explicit index maintenance progress and resumable failure.
func rebuildListingIndex(env listCommandDependencies, store storage.ObjectStore, stderr io.Writer) bool {
	ctx, stopReads := readCommandContext(env)
	defer stopReads()
	terminal.Println(stderr, "agent-archive: list: rebuilding listing entries from live metadata…")
	count, err := reader.RebuildIndex(ctx, store, archiveSessionsPrefix)
	if err != nil {
		terminal.Printf(stderr, "agent-archive: list: rebuild index stopped after %d metadata entries: %v; rerun to resume\n", count, err)
		return false
	}
	terminal.Printf(stderr, "agent-archive: list: rebuilt %d metadata entries.\n", count)
	return true
}
