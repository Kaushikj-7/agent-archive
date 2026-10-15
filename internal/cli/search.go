package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/search"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

type searchCommandDependencies interface {
	handoffCommandDependencies
	now() time.Time
	newCommandFlags(string, io.Writer) *commandFlags
}

type contentSearchOptions struct {
	stdin       io.Reader
	automatic   bool
	query       search.Query
	refresh     bool
	session     string
	read        string
	offset      int
	maxBytes    int
	project     string
	all         bool
	hasQuery    bool
	json        bool
	rebuild     bool
	wait        time.Duration
	outputBytes int
	before      int
	after       int
	reference   search.Reference
}

func runSearchCommand(args []string, stdin io.Reader, stdout, stderr io.Writer, env searchCommandDependencies) int {
	fs := env.newCommandFlags("search", stderr)
	harness := fs.String("harness", "", "session harness")
	project := fs.String("project", "", "directory or configured project name")
	all := fs.Bool("all-projects", false, "search every project's indexed sessions")
	since := fs.String("since", "", "inclusive capture date, RFC3339 time or age")
	until := fs.String("until", "", "inclusive capture date, RFC3339 time or age")
	replay := fs.String("replays", "hide", replaysFlagUsage)
	children := fs.String("children", "hide", "hide, include or only child sessions")
	limit := fs.Int("limit", 20, "maximum returned sessions (1–100)")
	cursor := fs.String("cursor", "", "continue an unchanged index/query page")
	refresh := fs.Bool("refresh", false, "run one bounded namespace-wide archive refresh, or index --session")
	session := fs.String("session", "", "exact archive session ID; requires --refresh and --harness")
	read := fs.String("read", "", "read a source-bound reference from a search result")
	offset := fs.Int("offset", -1, "with --read, absolute byte offset (default passage start)")
	maxBytes := fs.Int("max-bytes", 4096, "with --read, maximum text bytes (4–8192)")
	jsonMode := fs.Bool("json", false, "print one bounded JSON document")
	rebuild := fs.Bool("rebuild", false, "with --refresh, rebuild derived local content")
	wait := fs.Duration("wait", 0, "with --refresh, wait 1s–10m for observed coverage")
	outputBytes := fs.Int("max-output-bytes", 262144, "maximum escaped document bytes, 4096–1048576")
	before := fs.Int("before", 0, "with --read, preceding visible messages, 0–2")
	after := fs.Int("after", 0, "with --read, following visible messages, 0–2")
	text, given, ok := fs.parseWithOptionalArgument(args)
	if !ok {
		return 2
	}
	canonical, ok := harnessFlagWithCatalog(catalogFor(env), *harness)
	if !ok {
		return fs.usageError("%s", harnessFlagError(*harness))
	}
	opts := contentSearchOptions{query: search.Query{Text: text, Harness: canonical, Limit: *limit, Cursor: *cursor}, refresh: *refresh, session: *session, read: *read, offset: *offset, maxBytes: *maxBytes, project: *project, all: *all, hasQuery: given, json: *jsonMode, rebuild: *rebuild, wait: *wait, outputBytes: *outputBytes, before: *before, after: *after}

	seen := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { seen[f.Name] = true })
	if code := validateContentSearchBounds(fs, &opts, seen); code != 0 {
		return code
	}
	if code := validateContentSearchRead(fs, &opts, seen); code != 0 {
		return code
	}
	if code := validateContentSearchModes(fs, &opts, seen); code != 0 {
		return code
	}
	if code := validateContentSearchIdentities(fs, &opts); code != 0 {
		return code
	}
	if code := contentSearchFilters(fs, &opts, *replay, *children, *since, *until, env.now()); code != 0 {
		return code
	}
	opts.stdin = stdin
	return executeContentSearch(opts, stdout, stderr, env)
}

func contentSearchLocation(home string, cfg config.Config) (string, string) {
	namespace := strings.Join([]string{cfg.DestinationID(), cfg.MachineID, cfg.DestinationSince.UTC().Format(time.RFC3339Nano), "filter-" + archive.FilterVersion}, "/")
	sum := sha256.Sum256([]byte(namespace))
	return namespace, filepath.Join(home, "search", hex.EncodeToString(sum[:]))
}

func searchCommandError(stderr io.Writer, err error) int {
	terminal.Printf(stderr, "agent-archive: search: %v\n", err)
	return 1
}

func contentSearchFilters(fs *commandFlags, opts *contentSearchOptions, replay, children, since, until string, now time.Time) int {
	for _, flag := range []struct {
		value  string
		target *search.Inclusion
	}{{replay, &opts.query.Replay}, {children, &opts.query.Children}} {
		parsed, valid := replayFilterFlag(replaysFlag(flag.value))
		if !valid {
			return fs.usageError("--replays and --children accept hide, include or only")
		}
		*flag.target = search.Inclusion(parsed)
	}
	for _, flag := range []struct {
		value  string
		target *time.Time
	}{{since, &opts.query.From}, {until, &opts.query.To}} {
		if flag.value != "" {
			at, err := parseSince(flag.value, now)
			if err != nil {
				return fs.usageError("date filter: %v", err)
			}
			*flag.target = at
		}
	}
	if !opts.query.From.IsZero() && !opts.query.To.IsZero() && opts.query.From.After(opts.query.To) {
		return fs.usageError("--since must not follow --until")
	}
	return 0
}

func validateContentSearchBounds(fs *commandFlags, opts *contentSearchOptions, seen map[string]bool) int {
	if opts.outputBytes < 4096 || opts.outputBytes > 1048576 {
		return fs.usageError("--max-output-bytes must be 4096–1048576")
	}
	if seen["wait"] && (!opts.refresh || opts.wait < time.Second || opts.wait > 10*time.Minute) {
		return fs.usageError("--wait requires --refresh and a duration 1s–10m")
	}
	if opts.rebuild && (!opts.refresh || opts.session != "" || opts.query.Cursor != "") {
		return fs.usageError("--rebuild requires namespace --refresh without --session or --cursor")
	}
	if opts.query.Cursor != "" && opts.refresh {
		return fs.usageError("--cursor cannot be combined with refresh")
	}
	if opts.before < 0 || opts.before > 2 || opts.after < 0 || opts.after > 2 {
		return fs.usageError("--before and --after must be 0–2")
	}
	if opts.read == "" && (seen["before"] || seen["after"]) {
		return fs.usageError("--before and --after require --read")
	}
	if opts.query.Limit < 1 || opts.query.Limit > 100 {
		return fs.usageError("--limit must be between 1 and 100")
	}
	if opts.maxBytes < 4 || opts.maxBytes > 8192 || opts.offset < -1 {
		return fs.usageError("--max-bytes must be 4–8192 and --offset at least 0")
	}
	if opts.read == "" && (seen["offset"] || seen["max-bytes"]) {
		return fs.usageError("--offset and --max-bytes require --read")
	}

	return 0
}

func validateContentSearchRead(fs *commandFlags, opts *contentSearchOptions, seen map[string]bool) int {
	if opts.read != "" {
		for _, name := range []string{"project", "all-projects", "harness", "since", "until", "replays", "children", "limit", "cursor", "session", "refresh", "wait", "rebuild"} {
			if seen[name] {
				return fs.usageError("--%s cannot be combined with --read", name)
			}
		}
		var err error
		opts.reference, err = decodeSearchReference(opts.read)
		if err != nil {
			return fs.usageError("%v", err)
		}
	}
	if opts.read != "" {
		offset := opts.offset
		if offset < 0 {
			offset = opts.reference.Start
		}
		if offset < opts.reference.Start || offset > opts.reference.End {
			return fs.usageError("--offset is outside referenced passage")
		}
	}

	return 0
}

func validateContentSearchModes(fs *commandFlags, opts *contentSearchOptions, seen map[string]bool) int {
	if !opts.hasQuery && opts.read == "" {
		for _, name := range []string{"project", "all-projects", "since", "until", "replays", "children", "limit", "cursor"} {
			if seen[name] {
				return fs.usageError("--%s requires a query", name)
			}
		}
		if opts.query.Harness != "" && opts.session == "" {
			return fs.usageError("--harness requires a query or --session")
		}
	}
	if opts.all && opts.project != "" {
		return fs.usageError("--project and --all-projects cannot be combined")
	}
	if (opts.session != "" && !opts.refresh) || (opts.session != "" && opts.query.Harness == "") {
		return fs.usageError("named indexing requires --refresh --session ID --harness NAME")
	}
	if opts.read != "" && (opts.hasQuery || opts.refresh || opts.session != "") {
		return fs.usageError("--read cannot be combined with a query or refresh")
	}
	if !opts.hasQuery && !opts.refresh && opts.read == "" {
		return fs.usageError("provide a query, --refresh, or --read REF")
	}

	return 0
}

func validateContentSearchIdentities(fs *commandFlags, opts *contentSearchOptions) int {
	if opts.query.Cursor != "" {
		if err := search.ValidateCursor(opts.query.Cursor); err != nil {
			return fs.usageError("%v", err)
		}
	}
	if opts.session != "" {
		if _, err := archive.MetadataObjectKey(opts.query.Harness, opts.session); err != nil {
			return fs.usageError("invalid session ID: %v", err)
		}
	}
	if opts.hasQuery {
		if _, err := search.MatchExpression(opts.query.Text); err != nil {
			return fs.usageError("%v", err)
		}
	}

	return 0
}
