package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/stats"
	"github.com/wangjohn/agent-archive/internal/statsfmt"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

// statsSchemaVersion versions the `stats --json` document.
const statsSchemaVersion = 1

// maxPricesFileBytes bounds a --prices file: a price table is a few
// kilobytes, and nothing else should be read as one.
const maxPricesFileBytes = 1 << 20

// statsDocument is what `stats --json` prints: the engine's Stats, with its
// fields at the top level, under a schema version and the filters that
// narrowed the sessions. It holds counts, model names, project names, skill
// and MCP server names and one session ID: never prompts, transcript text or
// paths. A null is unknown, never zero; see stats.Stats.
type statsDocument struct {
	Version     int          `json:"schema_version"`
	GeneratedAt time.Time    `json:"generated_at"`
	Filters     statsFilters `json:"filters"`
	stats.Stats
}

// statsFilters echoes the filters a stats run applied, so a document says
// which sessions it covers. A filter that was not set is left out.
type statsFilters struct {
	Harness string `json:"harness,omitempty"`
	Model   string `json:"model,omitempty"`
	// Origin is "imported" or "hook" when --imported or --hook-captured was
	// given.
	Origin string `json:"origin,omitempty"`
	// Replays is "include" or "only" when --replays was given one of them;
	// by default replay sessions are left out and the field is absent.
	Replays string `json:"replays,omitempty"`
}

// runStatsCommand implements `agent-archive stats`. Like list, it reads only
// metadata sidecars (never a source bundle), reuses the local metadata cache
// unless --no-cache, and prints only numbers, model names, project names and
// the names of skills and MCP servers, so its output can never contain
// prompts, transcript text or paths.
//
// The window is calendar days in the machine's time zone ending today:
// --days N (default 30), or from the local day --since names through today.
// The previous period of the same length and the last six months are read
// too, for the overview's changes and the month rank, but only the window is
// reported.
//
// With a terminal on both stdin and stdout, interaction on, and none of
// --json, --html, --view, --detail, --by or --no-pager, it opens the
// interactive screen (see statsBrowser) instead of printing the overview.
// That screen switches windows without reading again, so it reads what the
// longest of them and its previous period need.
func runStatsCommand(args []string, stdin io.Reader, stdout, stderr io.Writer, env statsCommandDependencies) int {
	fs := env.newCommandFlags("stats", stderr)
	harness := fs.String("harness", "", "only sessions from this harness (codex, claude, cursor)")
	model := fs.String("model", "", "only sessions that requested or observed this model")
	since := fs.String("since", "", "start the window on this local day: a date (2026-09-01), RFC 3339 time, or age (7d, 12h)")
	days := fs.Int("days", stats.DefaultDays, "the window's length in calendar days, ending today")
	by := fs.String("by", "", "also break the window down by day, week, month, or project")
	viewName := fs.String("view", "", "which screen to print: overview (the default), detail, projects, models, or agents")
	detail := fs.Bool("detail", false, "print the detail screen; the same as --view detail")
	pricesFile := fs.String("prices", "", "price the tokens from this JSON file's prices on top of the built-in table")
	imported, hookCaptured := fs.Bool("imported", false, "only sessions agent-archive backfill imported"),
		fs.Bool("hook-captured", false, "only sessions captured by hooks as they ran")
	replays := fs.String("replays", string(replaysHide), replaysFlagUsage)
	noCache := fs.Bool("no-cache", false, "download every metadata sidecar instead of reusing unchanged ones from the local metadata cache")
	noPager := fs.Bool("no-pager", false, "print directly to the terminal; do not page through $PAGER")
	jsonOut := fs.Bool("json", false, "print a versioned JSON document of the numbers")
	all := fs.Bool("all", false, "with --json, list every project, skill and MCP server instead of the top five of each")
	htmlFlags := addStatsHTMLFlags(fs)
	if !fs.parseFlagsOnly(args) {
		return 2
	}
	if code := htmlFlags.validate(fs, *jsonOut, env.isTerminal(stdout)); code != 0 {
		return code
	}
	if code := checkStatsAll(fs, *all, *jsonOut); code != 0 {
		return code
	}
	daysSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "days" {
			daysSet = true
		}
	})
	page, code := statsPageFromFlags(fs, *viewName, *detail, *by, *jsonOut || htmlFlags.html)
	if code != 0 {
		return code
	}
	loc := statsZone(env.now(), env)
	now := env.now().In(loc)
	windowDays, code := statsWindowDays(fs, *days, daysSet, *since, now)
	if code != 0 {
		return code
	}
	grouping := stats.GroupNone
	if *by != "" {
		g, ok := stats.ParseGrouping(*by)
		if !ok {
			return fs.usageError("--by must be day, week, month, or project, not %q", *by)
		}
		grouping = g
	}
	table, code := statsPriceTable(fs, *pricesFile)
	if code != 0 {
		return code
	}
	// listOptionsFromFlags checks --harness and --model as list does. Its
	// --since is list's (midnight UTC); stats reads a date as a local day, so
	// it is not passed and the window sets the filter's From below.
	opts, code := listOptionsFromFlags(fs, listFlagValues{
		harness: *harness, model: *model, skillUsage: string(reader.SkillUsageUsed),
		imported: *imported, hookCaptured: *hookCaptured, replays: replaysFlag(*replays), noCache: *noCache,
	}, now)
	if code != 0 {
		return code
	}
	screen := statsScreenWanted(fs, env, stdin, stdout, *by != "" || *jsonOut || htmlFlags.html || *noPager || *detail)
	windows, windowIndex := []int{windowDays}, 0
	if screen {
		windows, windowIndex = statsWindowCycle(windowDays)
	}
	opts.filter.From = statsFetchFrom(now, loc, slices.Max(windows))

	store, cfg, found, err := openReadOnlyStore(env)
	if err != nil {
		terminal.Printf(stderr, "agent-archive: stats: %v\n", err)
		return 1
	}
	if !found {
		terminal.Println(stderr, notSetUpMessage)
		return 1
	}
	// The page on standard output carries nothing but the page: no spinner.
	listed, code := readStatsSessions(stdout, stderr, env, store, opts, *jsonOut || htmlFlags.toStdout())
	if code != 0 {
		return code
	}
	sessions := filterListOrigin(listed.Sessions, opts.imported, opts.hookCaptured)
	// A page lists every project and skill and lets the screen cut them. The
	// JSON and the web page keep the engine's default lists, the top few by
	// spend or use, unless --json --all asks for every row.
	textPage := !*jsonOut && !htmlFlags.html
	computed := stats.Compute(sessions, stats.Options{
		Now: now, Days: windowDays, Location: loc, PriceTable: table, By: grouping, AllRows: textPage || *all, MCPServerNames: cfg.MCPServerNames,
	})
	filters := statsFiltersOf(opts)
	if *jsonOut {
		return printJSON(stdout, stderr, statsDocument{
			Version: statsSchemaVersion, GeneratedAt: now, Filters: filters, Stats: computed,
		})
	}
	if htmlFlags.html {
		return htmlFlags.write(stdout, stderr, computed, filters, now, statsEmptyMessage(computed, filters, len(sessions) > 0))
	}
	view := newStatsView(stdout, env)
	view.filters = filters
	// A window with nothing in it is still a screen when another window may
	// have something: w moves on. Nothing at all is the message below.
	if screen && len(sessions) > 0 {
		start := statsBrowserStart{
			inputs:  statsInputs{sessions: sessions, now: now, location: loc, prices: table, filters: filters, mcpServerNames: cfg.MCPServerNames},
			windows: windows, window: windowIndex, first: computed, view: view,
		}
		if code, ran := runStatsBrowser(env, stdin, stdout, stderr, start); ran {
			return code
		}
	}
	if computed.Coverage.Sessions == 0 {
		terminal.Println(stdout, view.wrap(statsEmptyMessage(computed, filters, len(sessions) > 0)))
		return 0
	}
	if err := withPager(context.Background(), stdout, stderr, env, *noPager, func(w io.Writer) error {
		return renderStats(w, computed, page, view)
	}); err != nil {
		terminal.Printf(stderr, "agent-archive: stats: %v\n", err)
		return 1
	}
	return 0
}

// checkStatsAll reports a usage error for --all without --json. The web page
// keeps its top lists by design, and the screens list what they can already:
// only the JSON has a short default to lift.
func checkStatsAll(fs *commandFlags, all, jsonOut bool) int {
	if all && !jsonOut {
		return fs.usageError("--all applies only to --json")
	}
	return 0
}

// readStatsSessions reads the sessions stats counts. On a terminal it shows
// a spinner with how many sidecars are read, since the first run downloads
// and caches the selected ones. Ctrl-C (or SIGTERM, SIGHUP or SIGQUIT) stops the read at
// once, leaves nothing on the screen and exits as the signal would have;
// the cache is only ever written by atomic renames, so an interrupted read
// leaves nothing damaged. A non-zero code is the command's exit code.
func readStatsSessions(stdout, stderr io.Writer, env statsCommandDependencies, store storage.ObjectStore, opts listOptions, jsonOut bool) (reader.RecentResult, int) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	signals, stopSignals := env.interrupts()
	defer stopSignals()
	interrupted := make(chan os.Signal, 1)
	watching := make(chan struct{})
	go func() {
		defer close(watching)
		select {
		case sig := <-signals:
			interrupted <- sig
			cancel()
		case <-ctx.Done():
		}
	}()

	var done, total atomic.Int64
	// A skipped session is reported once the spinner is gone: a warning
	// written while it runs would land on the spinner's line, and clearing
	// the line would then cut into it. The reader reports them from the
	// goroutine that called it, after every read has finished.
	var skipped []reader.SkippedSidecar
	listOpts := reader.ListOptions{
		Cache: listCache(env, opts.noCache), Skipped: func(s reader.SkippedSidecar) { skipped = append(skipped, s) },
		// Calls come from several goroutines, out of order: keep the highest.
		Progress: func(d, t int) {
			total.Store(int64(t))
			for cur := done.Load(); int64(d) > cur; cur = done.Load() {
				if done.CompareAndSwap(cur, int64(d)) {
					break
				}
			}
		},
	}
	stopReading := func() {}
	if !jsonOut {
		style := activityStyle(stdout)
		stopReading = style.spinLabelEvery(stdout, func() string {
			if n := total.Load(); n > 0 {
				return fmt.Sprintf("Reading sessions… %s of %s", statsfmt.CommaInt(done.Load()), statsfmt.CommaInt(n))
			}
			return "Reading sessions…"
		}, spinnerInterval).stop
	}
	listed, err := reader.SelectMetadata(ctx, store, archiveSessionsPrefix, reader.MetadataQuery{Filter: opts.filter, IncludeRootChildren: true}, listOpts)
	stopReading()
	// From here on a signal is the default one's again; one that arrived
	// while the read was finishing still stops the command, as it asked.
	stopSignals()
	cancel()
	<-watching
	select {
	case sig := <-interrupted:
		return reader.RecentResult{}, signalExitCode(sig)
	case sig := <-signals:
		return reader.RecentResult{}, signalExitCode(sig)
	default:
	}
	if warn := warnSkippedSidecar(stderr, "stats"); warn != nil {
		for _, s := range skipped {
			warn(s)
		}
	}
	if err != nil {
		terminal.Printf(stderr, "agent-archive: stats: %v\n", err)
		return reader.RecentResult{}, 1
	}
	return listed, 0
}

// statsFiltersOf is the filters a run applied, as the output echoes them.
func statsFiltersOf(opts listOptions) statsFilters {
	filters := statsFilters{Harness: opts.filter.Harness, Model: opts.filter.Model}
	switch opts.filter.Replays {
	case reader.ReplaysIncluded:
		filters.Replays = string(replaysInclude)
	case reader.ReplaysOnly:
		filters.Replays = string(replaysOnly)
	case reader.ReplaysHidden:
	}
	switch {
	case opts.imported:
		filters.Origin = "imported"
	case opts.hookCaptured:
		filters.Origin = "hook"
	}
	return filters
}

// statsScreenWanted is whether stats opens the interactive screen: stdin and
// stdout are terminals and interaction is on (Env.interactive), the terminal
// is not a dumb one (which cannot switch screens or place the cursor, so the
// escape sequences would print as text), and no flag asks for a printed page
// (printed is whether --by, --json, --html, --no-pager or --detail was given;
// --view is looked at here).
func statsScreenWanted(fs *commandFlags, env statsCommandDependencies, stdin io.Reader, stdout io.Writer, printed bool) bool {
	if printed || viewGiven(fs) || !env.interactive(stdin) || !env.interactive(stdout) {
		return false
	}
	term, _ := env.lookupEnv("TERM")
	return term != "dumb"
}

// viewGiven is whether --view was given, whatever its value.
func viewGiven(fs *commandFlags) bool {
	given := false
	fs.Visit(func(f *flag.Flag) { given = given || f.Name == "view" })
	return given
}

// statsPageFromFlags is the screen --view, --detail and --by ask for. --detail
// is --view detail, and the two together are refused. --by project is the
// projects screen, and --by day, week or month is a table under the detail
// screen; asking for another screen with them is refused rather than
// guessed at. --view and --detail print a screen, so they do not go with
// --json or --html.
func statsPageFromFlags(fs *commandFlags, viewName string, detail bool, by string, structured bool) (statsPage, int) {
	viewSet := viewGiven(fs)
	if viewSet && detail {
		return "", fs.usageError("choose one of --view and --detail")
	}
	if structured && (viewSet || detail) {
		return "", fs.usageError("--view and --detail choose a screen; --json and --html print the whole document")
	}
	page := pageOverview
	switch {
	case detail:
		page = pageDetail
	case viewSet:
		parsed, ok := parseStatsPage(viewName)
		if !ok {
			return "", fs.usageError("--view must be overview, detail, projects, models, or agents, not %q", viewName)
		}
		page = parsed
	}
	if structured || by == "" {
		return page, 0
	}
	byPage := pageDetail
	if by == string(stats.GroupProject) {
		byPage = pageProjects
	}
	switch {
	case !viewSet && !detail:
		return byPage, 0
	case page != byPage:
		return "", fs.usageError("--by %s is shown on the %s screen; drop --by or use --view %s", by, byPage, byPage)
	}
	return page, 0
}

// statsWindowDays is the window's length in calendar days: --days, or the
// days from the local day --since names through today, so the window is
// whole days and --since can never disagree with what the engine counts. A
// time or age selects from the start of its local day, as backfill's --since
// does. It returns a usage error's exit code when the flags conflict or are
// out of range.
func statsWindowDays(fs *commandFlags, days int, daysSet bool, since string, now time.Time) (int, int) {
	if since != "" && daysSet {
		return 0, fs.usageError("choose one of --days and --since")
	}
	if since == "" {
		if days < 1 || days > stats.MaxDays {
			return 0, fs.usageError("--days must be from 1 to %d", stats.MaxDays)
		}
		return days, 0
	}
	from, err := parseTimeArg(since, now, now.Location())
	if err != nil {
		return 0, fs.usageError("--since: %v", err)
	}
	span := calendarDaysBetween(from.In(now.Location()), now) + 1
	if span < 1 {
		return 0, fs.usageError("--since is in the future")
	}
	if span > stats.MaxDays {
		return 0, fs.usageError("--since reaches back %d days; the most is %d (--days)", span, stats.MaxDays)
	}
	return span, 0
}

// calendarDaysBetween is how many calendar days later b's date is than a's,
// both read in b's zone. It counts dates, never 24-hour spans, so a day with
// a daylight-saving change is still one day.
func calendarDaysBetween(a, b time.Time) int {
	ay, am, ad := a.In(b.Location()).Date()
	by, bm, bd := b.Date()
	start := time.Date(ay, am, ad, 0, 0, 0, 0, time.UTC)
	end := time.Date(by, bm, bd, 0, 0, 0, 0, time.UTC)
	return int(end.Sub(start).Hours() / 24)
}

// statsFetchFrom is the earliest capture time stats reads: the start of the
// previous period (as long as the window before it), or of the month five
// before this one for the month rank, whichever is earlier.
func statsFetchFrom(now time.Time, loc *time.Location, windowDays int) time.Time {
	year, month, day := now.In(loc).Date()
	previous := time.Date(year, month, day-(2*windowDays-1), 0, 0, 0, 0, loc)
	rank := time.Date(year, month-time.Month(stats.MonthsCompared-1), 1, 0, 0, 0, 0, loc)
	if rank.Before(previous) {
		return rank
	}
	return previous
}

// statsPriceTable is the price table to cost with: the built-in one, or with
// the entries of the --prices file applied on top. A file that cannot be
// read or is not a price table is a usage error, reported before any
// storage is touched.
func statsPriceTable(fs *commandFlags, path string) (stats.PriceTable, int) {
	if path == "" {
		return stats.PriceTable{}, 0
	}
	file, err := os.Open(path)
	if err != nil {
		return stats.PriceTable{}, fs.usageError("--prices: %v", err)
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, maxPricesFileBytes+1))
	if err != nil {
		return stats.PriceTable{}, fs.usageError("--prices: %v", err)
	}
	if len(data) > maxPricesFileBytes {
		return stats.PriceTable{}, fs.usageError("--prices: the file is larger than %d bytes, so it is not a price table", maxPricesFileBytes)
	}
	custom, err := stats.ParsePriceTable(data)
	if err != nil {
		return stats.PriceTable{}, fs.usageError("--prices: %v", err)
	}
	return stats.DefaultPriceTable().WithOverrides(custom), 0
}

// statsEmptyMessage says why there is nothing to show, and what to try.
// sawSessions is whether any session (of the previous period, say) was read.
func statsEmptyMessage(s stats.Stats, filters statsFilters, sawSessions bool) string {
	longer := ""
	switch {
	case s.Window.Days < 90:
		longer = " Try a longer window, for example agent-archive stats --days 90."
	case s.Window.Days < 365:
		longer = " Try a longer window, for example agent-archive stats --days 365."
	}
	return statsEmptyMessageWith(s, filters, sawSessions, longer)
}

// statsEmptyMessageWith is statsEmptyMessage ending in advice: the screen
// tells the person to press w, not to run the command again.
func statsEmptyMessageWith(s stats.Stats, filters statsFilters, sawSessions bool, advice string) string {
	span := fmt.Sprintf("in the last %d days (%s to %s)", s.Window.Days, s.Window.FirstDay, s.Window.LastDay)
	if s.Window.Days == 1 {
		span = fmt.Sprintf("today (%s)", s.Window.LastDay)
	}
	switch {
	case filters.Model != "":
		// The screen names models by family ("opus"); the filter is exact.
		return "No archived sessions match these filters " + span + ". --model takes a full model id (for example claude-opus-5), not a family name like opus." + advice
	case filters != statsFilters{}:
		return "No archived sessions match these filters " + span + "." + advice
	case sawSessions:
		return "No archived sessions were captured " + span + "." + advice
	}
	return "No archived sessions " + span + ". If you have just set up, sessions appear once an app session is captured (agent-archive status shows capture)." + advice
}

// statsZone is the time zone days, weeks and months are counted in: the
// clock's own. The machine's local zone is found by its name, from $TZ or
// where /etc/localtime points, so the output can say which zone it counted
// in (Go's time.Local is only called "Local"); when no name is known to be
// the same zone, it stays time.Local.
func statsZone(now time.Time, env interface{ lookupEnv(string) (string, bool) }) *time.Location {
	if now.Location() != time.Local {
		return now.Location()
	}
	tz, tzSet := env.lookupEnv("TZ")
	link := ""
	if !tzSet {
		link = zoneFileLink("/etc/localtime")
	}
	return resolveLocalZone(time.Local, now, tz, tzSet, link)
}

// maxZoneLinkHops bounds how far zoneFileLink follows a chain of links.
const maxZoneLinkHops = 8

// zoneFileLink is where the link at path points in the zone database: the
// first target on its chain of links that has a zoneinfo directory in it (a
// distribution may point /etc/localtime at another link, as NixOS does), or
// the last target when none does. It is empty when path is not a link.
func zoneFileLink(path string) string {
	target := ""
	for range maxZoneLinkHops {
		next, err := os.Readlink(path)
		if err != nil {
			return target
		}
		if !filepath.IsAbs(next) {
			next = filepath.Join(filepath.Dir(path), next)
		}
		target = next
		if strings.Contains(target, "zoneinfo/") {
			return target
		}
		path = target
	}
	return target
}

// resolveLocalZone names the zone local is, or returns local when it cannot
// be sure. Go reads $TZ when it is set (an unset one means /etc/localtime),
// so a set $TZ is the only name to try: when it does not resolve Go counts in
// UTC, and /etc/localtime is not consulted. A name is used only when it has
// the same offset as local at every season of the year, so a zone that merely
// shares today's offset is not mistaken for it.
func resolveLocalZone(local *time.Location, now time.Time, tz string, tzSet bool, localtimeLink string) *time.Location {
	var names []string
	if tzSet {
		names = append(names, strings.TrimPrefix(tz, ":"))
	} else {
		names = append(names, localtimeLink)
	}
	for _, name := range names {
		// A path to a zone file (a $TZ, or the link's target) names the zone
		// by what follows the zoneinfo directory.
		if _, rest, found := strings.Cut(name, "zoneinfo/"); found {
			name = rest
		}
		if name == "" || name == "Local" {
			continue
		}
		if named, err := time.LoadLocation(name); err == nil && sameZone(named, local, now) {
			return named
		}
	}
	return local
}

// sameZone is whether a and b have the same UTC offset at the start of every
// season of the years around now, which is what a zone's daylight-saving
// rules come to.
func sameZone(a, b *time.Location, now time.Time) bool {
	for year := now.Year() - 1; year <= now.Year(); year++ {
		for month := time.January; month <= time.December; month += 3 {
			at := time.Date(year, month, 1, 12, 0, 0, 0, time.UTC)
			_, offsetA := at.In(a).Zone()
			_, offsetB := at.In(b).Zone()
			if offsetA != offsetB {
				return false
			}
		}
	}
	return true
}
