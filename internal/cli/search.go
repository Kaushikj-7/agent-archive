package cli

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/search"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

type searchCommandDependencies interface {
	readOnlyStoreDependencies
	scopeDependencies
	readSignalSource
	now() time.Time
	newCommandFlags(string, io.Writer) *commandFlags
}

type contentSearchOptions struct {
	query    search.Query
	refresh  bool
	session  string
	read     string
	offset   int
	maxBytes int
	project  string
	all      bool
	hasQuery bool
}

func runSearchCommand(args []string, stdout, stderr io.Writer, env searchCommandDependencies) int {
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
	refresh := fs.Bool("refresh", false, "index exactly --session from verified archive sources")
	session := fs.String("session", "", "exact archive session ID; requires --refresh and --harness")
	read := fs.String("read", "", "read a source-bound reference from a search result")
	offset := fs.Int("offset", -1, "with --read, absolute byte offset (default passage start)")
	maxBytes := fs.Int("max-bytes", 4096, "with --read, maximum text bytes (4–8192)")
	text, given, ok := fs.parseWithOptionalArgument(args)
	if !ok {
		return 2
	}
	canonical, ok := harnessFlagWithCatalog(catalogFor(env), *harness)
	if !ok {
		return fs.usageError("%s", harnessFlagError(*harness))
	}
	opts := contentSearchOptions{query: search.Query{Text: text, Harness: canonical, Limit: *limit, Cursor: *cursor}, refresh: *refresh, session: *session, read: *read, offset: *offset, maxBytes: *maxBytes, project: *project, all: *all, hasQuery: given}
	if *all && *project != "" {
		return fs.usageError("--project and --all-projects cannot be combined")
	}
	if *refresh != (*session != "") || (*refresh && canonical == "") {
		return fs.usageError("indexing requires --refresh --session ID --harness NAME")
	}
	if *read != "" && (given || *refresh || *session != "") {
		return fs.usageError("--read cannot be combined with a query or refresh")
	}
	if !given && !*refresh && *read == "" {
		return fs.usageError("provide a query, --refresh --session ID --harness NAME, or --read REF")
	}
	if given {
		if _, err := search.MatchExpression(text); err != nil {
			return fs.usageError("%v", err)
		}
	}
	if *limit < 1 || *limit > 100 {
		return fs.usageError("--limit must be between 1 and 100")
	}
	if *maxBytes < 4 || *maxBytes > 8192 || *offset < -1 {
		return fs.usageError("--max-bytes must be 4–8192 and --offset at least 0")
	}
	if *read == "" && (*offset != -1 || *maxBytes != 4096) {
		return fs.usageError("--offset and --max-bytes require --read")
	}
	if code := contentSearchFilters(fs, &opts, *replay, *children, *since, *until, env.now()); code != 0 {
		return code
	}
	return executeContentSearch(opts, stdout, stderr, env)
}

func executeContentSearch(opts contentSearchOptions, stdout, stderr io.Writer, env searchCommandDependencies) int {
	home, err := env.readHome()
	if err != nil {
		return searchCommandError(stderr, err)
	}
	cfg, found, err := config.Load(home)
	if err != nil {
		return searchCommandError(stderr, err)
	}
	if !found {
		terminal.Println(stderr, notSetUpMessage)
		return 1
	}
	namespace, dir := contentSearchLocation(home, cfg)
	ctx, stop := readCommandContext(env)
	defer stop()
	if opts.read != "" {
		return readSearchReference(ctx, env, cfg, namespace, opts, stdout, stderr)
	}
	scope, err := scopeFor(env, opts.project, opts.all)
	if err != nil {
		return searchCommandError(stderr, err)
	}
	opts.query.Scope = search.Scope{Restricted: scope.narrowed(), Named: scope.Dir == "", RepoKey: scope.RepoKey, ProjectName: scope.Label, ProjectIDs: scope.ProjectIDs}
	if !opts.refresh {
		if _, err = os.Lstat(filepath.Join(dir, "content.sqlite")); os.IsNotExist(err) {
			terminal.Println(stdout, "Search coverage is incomplete: no local content index. Index a session with search --refresh --session ID --harness NAME.")
			return 0
		} else if err != nil {
			return searchCommandError(stderr, err)
		}
	}
	index, err := search.Open(ctx, dir, namespace)
	if err != nil {
		return searchCommandError(stderr, err)
	}
	defer func() { _ = index.Close() }()
	if opts.refresh {
		if err = refreshSearchSession(ctx, env, cfg, index, opts, stdout, stderr); err != nil {
			return searchCommandError(stderr, err)
		}
	}
	if !opts.hasQuery {
		return 0
	}
	page, err := index.Search(ctx, opts.query)
	if err != nil {
		return searchCommandError(stderr, err)
	}
	return printSearchPage(stdout, stderr, page)
}

func contentSearchLocation(home string, cfg config.Config) (string, string) {
	namespace := strings.Join([]string{cfg.DestinationID(), cfg.MachineID, cfg.DestinationSince.UTC().Format(time.RFC3339Nano), "filter-" + archive.FilterVersion}, "/")
	sum := sha256.Sum256([]byte(namespace))
	return namespace, filepath.Join(home, "search", hex.EncodeToString(sum[:]))
}

func refreshSearchSession(ctx context.Context, env searchCommandDependencies, cfg config.Config, index *search.Store, opts contentSearchOptions, stdout, stderr io.Writer) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	release, err := index.LockWork(ctx)
	if err != nil {
		return err
	}
	defer release()
	objects, err := env.openStore(cfg)
	if err != nil {
		return err
	}
	key, err := archive.MetadataObjectKey(opts.query.Harness, opts.session)
	if err != nil {
		return err
	}
	metadata, err := reader.ReadMetadata(ctx, objects, key)
	if err != nil {
		return err
	}
	if metadata.SessionID != opts.session || metadata.Harness.Name != opts.query.Harness {
		return errors.New("search metadata disagrees with requested session")
	}
	if err = index.Observe(ctx, metadata, parsersFor(env), env.now(), 0); err != nil {
		return err
	}
	progress, err := index.Run(ctx, objects, parsersFor(env), env.now(), search.Budget{Sources: 64, Sessions: 64, Limits: reader.Limits{}})
	if err != nil {
		return err
	}
	terminal.Printf(stdout, "Observed %s; indexed %d work item(s), %d passage/title writes; %d pending, %d failed.\n", metadata.SessionID, progress.Committed, progress.Writes.Inserted, progress.Pending, progress.Failed)
	for _, gap := range progress.Gaps {
		terminal.Printf(stderr, "Search coverage gap for revision %s: %s\n", gap.RevisionID, gap.Reason)
	}
	if progress.Failed > 0 {
		return errors.New("search refresh has failed work; retry after backoff or inspect coverage")
	}

	return nil
}

func readSearchReference(ctx context.Context, env searchCommandDependencies, cfg config.Config, namespace string, opts contentSearchOptions, stdout, stderr io.Writer) int {
	var ref search.Reference
	if len(opts.read) > 16384 {
		return searchCommandError(stderr, errors.New("search reference is too large"))
	}
	b, err := base64.RawURLEncoding.DecodeString(opts.read)
	if err != nil {
		return searchCommandError(stderr, errors.New("invalid search reference"))
	}
	if err = json.Unmarshal(b, &ref); err != nil {
		return searchCommandError(stderr, errors.New("invalid search reference"))
	}
	if ref.Source.Destination != namespace {
		return searchCommandError(stderr, errors.New("search reference belongs to another destination/privacy context"))
	}
	objects, err := env.openStore(cfg)
	if err != nil {
		return searchCommandError(stderr, err)
	}
	offset := opts.offset
	if offset == -1 {
		offset = ref.Start
	}
	result, err := search.ReadPassage(ctx, objects, parsersFor(env), ref, offset, opts.maxBytes, reader.Limits{})
	if err != nil {
		return searchCommandError(stderr, err)
	}
	// DisplayJSON escapes terminal controls while retaining newlines in the quoted
	// text, preserving a precise byte slice without raw terminal instructions.
	text, _ := json.Marshal(result.Text)
	if err = writeOutput(stdout, append(archive.DisplayJSON(text), '\n')); err != nil {
		return searchCommandError(stderr, err)
	}
	terminal.Printf(stderr, "Passage bytes %d–%d; next offset %d (0 means complete).\n", result.Start, result.End, result.Next)
	return 0
}

func printSearchPage(stdout, stderr io.Writer, page search.Page) int {
	for _, hit := range page.Hits {
		terminal.Printf(stdout, "%s · %s · %s\n  %s\n  %d matching passage(s)\n", hit.SessionID, hit.Harness, hit.Title, hit.Snippet, hit.MatchCount)
		if hit.Reference != nil {
			b, err := json.Marshal(hit.Reference)
			if err != nil {
				return searchCommandError(stderr, err)
			}
			terminal.Printf(stdout, "  ref: %s\n", base64.RawURLEncoding.EncodeToString(b))
		}
		for _, gap := range hit.Gaps {
			terminal.Printf(stdout, "  coverage gap: %s\n", gap.Reason)
		}
	}
	terminal.Printf(stdout, "%d result(s). Search coverage is incomplete: only explicitly indexed sessions; archive changes have not been reconciled.\n", len(page.Hits))
	if page.Next != "" {
		terminal.Printf(stdout, "Next cursor: %s\n", page.Next)
	}
	return 0
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
