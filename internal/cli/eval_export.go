package cli

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/backfill"
	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

// defaultEvalMaxBytes bounds one exported record, like handoff's default.
const defaultEvalMaxBytes = 120000

// maxEvalWorkers caps --workers' default: the export is bound by parsing and
// filtering, and more than this mostly adds memory.
const maxEvalWorkers = 8

// maxEvalInputLine is the longest line --ids-from reads.
const maxEvalInputLine = 64 << 10

// runEvalCommand implements `agent-archive eval`, whose one action is export.
func runEvalCommand(args []string, stdin io.Reader, stdout, stderr io.Writer, env Env) int {
	if len(args) > 0 && strings.HasPrefix(args[0], "-") {
		fs := env.newCommandFlags("eval", stderr)
		if !fs.parseFlagsOnly(args) {
			return 2
		}
	}
	if len(args) == 0 {
		terminal.Println(stderr, "agent-archive: eval: choose export; run agent-archive eval --help")
		return 2
	}
	switch args[0] {
	case "export":
		return runEvalExport(args[1:], stdin, stdout, stderr, env)
	default:
		terminal.Printf(stderr, "agent-archive: eval: unknown action %q; run agent-archive eval --help\n", args[0])
		return 2
	}
}

// evalExportOptions is the validated command line of eval export.
type evalExportOptions struct {
	ids      []string
	idsFrom  bool
	file     string
	scan     bool
	filters  backfill.Filters
	harness  string
	detail   archive.EvalExportDetail
	maxBytes int
	workers  int
}

// evalInput is one session to export: an archive session ID, or a local
// transcript (path, and the app that wrote it when known).
type evalInput struct {
	archiveID string
	path      string
	harness   string
	// nativeID, projectRoot and startedAt are what --scan's discovery
	// resolved for a local transcript; empty for one named directly.
	nativeID    string
	projectRoot string
	startedAt   time.Time
	// given is the input as the caller wrote it, for an error record.
	given string
}

// runEvalExport implements `agent-archive eval export`: one JSON Lines record
// per session, each built only from filtered data (dev/specs/eval-export.md).
// Sessions come from the archive (IDs as arguments or on stdin with
// --ids-from -) or from transcript files on this machine (--file, --scan, or
// paths on stdin), which needs no setup and never creates the data
// directory. It never prompts, pages, or writes anything but stdout and
// stderr. With more than one worker, records are written as each session
// finishes. A session that cannot be exported is an error record on its own
// line; the others are still written, and the exit code is 1.
func runEvalExport(args []string, stdin io.Reader, stdout, stderr io.Writer, env Env) int {
	opts, code := evalExportOptionsFromArgs(args, stderr, env)
	if code != 0 {
		return code
	}
	if opts.idsFrom && env.isTerminal(stdin) {
		// The list is a pipe or a file; a terminal would wait for a person.
		terminal.Println(stderr, "agent-archive: eval export: --ids-from - reads a pipe or a file, not a terminal; run agent-archive eval export --help")
		return 2
	}
	inputs, err := evalInputs(opts, stdin, env)
	if err != nil {
		terminal.Printf(stderr, "agent-archive: eval export: %v\n", err)
		return 1
	}
	var store storage.ObjectStore
	for _, input := range inputs {
		if input.archiveID == "" {
			continue
		}
		// Only an archive input needs setup; local ones never read the
		// data directory.
		var found bool
		if store, _, found, err = openReadOnlyStore(env); err != nil {
			terminal.Printf(stderr, "agent-archive: eval export: %v\n", err)
			return 1
		}
		if !found {
			terminal.Println(stderr, notSetUpMessage)
			return 1
		}
		break
	}
	ctx, stopReads := readCommandContext(env)
	defer stopReads()
	exporter := evalExporter{ctx: ctx, store: store, opts: opts, env: env}
	failed, err := exporter.run(inputs, stdout)
	if err != nil {
		terminal.Printf(stderr, "agent-archive: eval export: %v\n", err)
		return 1
	}
	if failed {
		return 1
	}
	return 0
}

// evalExportOptionsFromArgs parses eval export's flags, which may come before
// or after the session IDs.
func evalExportOptionsFromArgs(args []string, stderr io.Writer, env Env) (evalExportOptions, int) {
	fs := env.newCommandFlags("eval export", stderr)
	harness := fs.String("harness", "", "the app the sessions were captured with (codex, claude, cursor); needed with --file, and for an archive ID that exists under more than one")
	detail := fs.String("detail", string(archive.EvalExportDetailFull), "metadata (no conversation text; reads only metadata sidecars) or full (adds prompts, final response, edited files, feedback)")
	maxBytes := fs.Int("max-bytes", defaultEvalMaxBytes, "cut each record's longest texts to fit this many bytes (0 for no limit)")
	idsFrom := fs.String("ids-from", "", "read archive session IDs or transcript paths, one a line, from - (standard input)")
	file := fs.String("file", "", "export this transcript file on this machine (with --harness); needs no setup")
	scan := fs.Bool("scan", false, "export the transcripts found on this machine, as backfill finds them; needs no setup")
	var projects stringList
	fs.Var(&projects, "project", "with --scan, only sessions in this project directory; repeatable")
	since := fs.String("since", "", "with --scan, only sessions started on or after this local date, time, or age")
	until := fs.String("until", "", "with --scan, only sessions started on or before this local date, time, or age")
	// 0 is resolved here, not in the flag's default, so help and the CLI
	// reference are the same on every machine.
	workers := fs.Int("workers", 0, "export this many sessions at once, records written as each finishes (0: the number of CPUs, up to 8)")
	var ids []string
	rest := args
	for {
		if !fs.parse(rest) {
			return evalExportOptions{}, 2
		}
		if fs.NArg() == 0 {
			break
		}
		ids = append(ids, fs.Arg(0))
		rest = fs.Args()[1:]
	}
	opts := evalExportOptions{ids: ids, idsFrom: *idsFrom != "", file: *file, scan: *scan, maxBytes: *maxBytes, workers: *workers}
	if opts.workers == 0 {
		opts.workers = min(runtime.NumCPU(), maxEvalWorkers)
	}
	sources := 0
	for _, set := range []bool{len(ids) > 0 || opts.idsFrom, opts.file != "", opts.scan} {
		if set {
			sources++
		}
	}
	switch {
	case sources == 0:
		return evalExportOptions{}, fs.usageError("name session IDs, --ids-from -, --file PATH, or --scan")
	case sources > 1:
		return evalExportOptions{}, fs.usageError("session IDs (or --ids-from), --file, and --scan are mutually exclusive")
	case len(ids) > 0 && opts.idsFrom:
		return evalExportOptions{}, fs.usageError("name session IDs as arguments or with --ids-from -, not both")
	case opts.idsFrom && *idsFrom != "-":
		return evalExportOptions{}, fs.usageError("--ids-from reads only - (standard input)")
	case !opts.scan && (len(projects) > 0 || *since != "" || *until != ""):
		return evalExportOptions{}, fs.usageError("--project, --since, and --until apply only to --scan")
	case *maxBytes < 0:
		return evalExportOptions{}, fs.usageError("--max-bytes must be 0 or more")
	case *workers < 0:
		return evalExportOptions{}, fs.usageError("--workers must be 0 (automatic) or more")
	}
	canonical, ok := harnessFlagWithCatalog(fs.catalog, *harness)
	if !ok {
		return evalExportOptions{}, fs.usageError("%s", harnessFlagError(*harness))
	}
	if opts.file != "" && canonical == "" {
		return evalExportOptions{}, fs.usageError("--file requires --harness (claude, codex, or cursor)")
	}
	opts.harness = canonical
	opts.detail = archive.EvalExportDetail(*detail)
	if opts.detail != archive.EvalExportDetailMetadata && opts.detail != archive.EvalExportDetailFull {
		return evalExportOptions{}, fs.usageError("--detail must be metadata or full, not %q", *detail)
	}
	if opts.scan {
		sinceDay, err := backfillDay(*since, env.now())
		if err != nil {
			return evalExportOptions{}, fs.usageError("--since: %v", err)
		}
		untilDay, err := backfillDay(*until, env.now())
		if err != nil {
			return evalExportOptions{}, fs.usageError("--until: %v", err)
		}
		opts.filters = backfill.Filters{Projects: projects, Since: sinceDay, Until: untilDay}
		if canonical != "" {
			opts.filters.Harnesses = []string{canonical}
		}
		if err := opts.filters.Validate(env.agentRegistry()); err != nil {
			return evalExportOptions{}, fs.usageError("%v", err)
		}
	}
	return opts, 0
}

// evalInputs lists what to export, in order: the IDs given, the lines of
// stdin, the one file, or what --scan finds.
func evalInputs(opts evalExportOptions, stdin io.Reader, env Env) ([]evalInput, error) {
	switch {
	case opts.file != "":
		return []evalInput{{path: opts.file, harness: opts.harness, given: opts.file}}, nil
	case opts.scan:
		return scanEvalInputs(opts, env)
	case opts.idsFrom:
		return readEvalInputs(stdin, opts.harness)
	}
	inputs := make([]evalInput, len(opts.ids))
	for i, id := range opts.ids {
		inputs[i] = evalInput{archiveID: id, given: id}
	}
	return inputs, nil
}

// readEvalInputs reads --ids-from's lines: an absolute path is a local
// transcript, anything else an archive session ID. Blank lines are skipped.
func readEvalInputs(stdin io.Reader, harness string) ([]evalInput, error) {
	var inputs []evalInput
	scanner := bufio.NewScanner(stdin)
	scanner.Buffer(nil, maxEvalInputLine)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		switch {
		case line == "":
		case filepath.IsAbs(line):
			inputs = append(inputs, evalInput{path: line, harness: harness, given: line})
		default:
			inputs = append(inputs, evalInput{archiveID: line, given: line})
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read --ids-from: %w", err)
	}
	return inputs, nil
}

// scanEvalInputs finds this machine's transcripts with backfill's discovery,
// as if nothing were archived and nothing configured, and keeps the ones
// backfill would import: a session it would skip (run from the home folder
// or a temporary one, a transcript it cannot read safely, no project) is
// skipped here too. Cursor chats only in Cursor's database are not read: they
// have no file to name them by.
func scanEvalInputs(opts evalExportOptions, env Env) ([]evalInput, error) {
	userHome, err := env.userHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolve home: %w", err)
	}
	benv := env.backfillEnvironment(userHome, config.Config{})
	benv.CursorDatabase = nil
	plan, err := backfill.BuildPlan(context.Background(), benv, noArchivedSessions{}, config.Config{}, opts.filters)
	if err != nil {
		return nil, err
	}
	var inputs []evalInput
	for _, c := range plan.Candidates {
		if c.Skip != "" || c.TranscriptPath == "" || c.SourceKind != archive.SourceKindFile {
			continue
		}
		inputs = append(inputs, evalInput{path: c.TranscriptPath, harness: c.Harness, nativeID: c.NativeSessionID, projectRoot: c.ProjectRoot, startedAt: c.StartedAt, given: c.TranscriptPath})
	}
	return inputs, nil
}

// noArchivedSessions is backfill's archive state for a scan: nothing is
// archived, so every session found is a candidate.
type noArchivedSessions struct{}

func (noArchivedSessions) Classify(string, string) (backfill.SkipReason, error) { return "", nil }

// evalExporter exports inputs with a pool of workers and writes each record
// whole, as its session finishes.
type evalExporter struct {
	ctx   context.Context
	store storage.ObjectStore
	opts  evalExportOptions
	env   Env

	mu     sync.Mutex
	failed bool
	err    error
}

// run exports every input and reports whether any became an error record,
// or the first error writing the output.
func (x *evalExporter) run(inputs []evalInput, stdout io.Writer) (failed bool, err error) {
	ctx, cancel := context.WithCancel(x.ctx)
	defer cancel()
	x.ctx = ctx
	jobs := make(chan evalInput)
	var wg sync.WaitGroup
	for range min(x.opts.workers, max(len(inputs), 1)) {
		wg.Go(func() {
			for input := range jobs {
				if ctx.Err() != nil {
					return
				}
				x.write(stdout, x.export(input))
				x.mu.Lock()
				if x.err != nil {
					cancel()
				}
				x.mu.Unlock()
			}
		})
	}
feed:
	for _, input := range inputs {
		select {
		case <-ctx.Done():
			break feed
		case jobs <- input:
		}
	}
	close(jobs)
	wg.Wait()
	if x.err == nil {
		x.err = ctx.Err()
	}
	return x.failed, x.err
}

// write writes one record under the lock, so lines never interleave.
func (x *evalExporter) write(w io.Writer, record any) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if _, isError := record.(archive.EvalExportError); isError {
		x.failed = true
	}
	if x.err == nil {
		x.err = writeEvalRecord(w, record)
	}
}

// export is one input's record.
func (x *evalExporter) export(input evalInput) any {
	if input.archiveID != "" {
		return exportArchivedSession(x.ctx, x.env.agentRegistry(), x.store, input.archiveID, x.opts)
	}
	return x.exportLocal(input)
}

// exportArchivedSession is the record of one archived session: an
// archive.EvalExport, or an archive.EvalExportError saying why not. Only a
// full archive session ID is accepted, never a title or a prefix, so an
// export is always of exactly the session named. At metadata detail only the
// sidecar is read; at full detail the source bundle is read and verified
// against it, as show --transcript does.
func exportArchivedSession(ctx context.Context, parsers agentapi.ParsersLookup, store storage.ObjectStore, id string, opts evalExportOptions) any {
	fail := func(code archive.EvalErrorCode, message string) any {
		record := archive.NewEvalExportError(archive.EvalExportSourceArchive, id, code, message)
		if isArchiveSessionID(id) {
			record.SessionID = id
		}
		return record
	}
	if !isArchiveSessionID(id) {
		return fail(archive.EvalErrorNotFound, "not an archive session ID (32 lowercase hex digits); see agent-archive list --json")
	}
	key, err := locateMetadataKey(ctx, store, opts.harness, id)
	if err != nil {
		if strings.Contains(err.Error(), "more than one harness") {
			return fail(archive.EvalErrorAmbiguous, err.Error())
		}
		if strings.Contains(err.Error(), "no archived session") {
			return fail(archive.EvalErrorNotFound, "no archived session with this ID")
		}
		return fail(archive.EvalErrorReadFailed, "the session metadata could not be read or verified")
	}
	if opts.detail == archive.EvalExportDetailMetadata {
		metadata, err := reader.ReadMetadata(ctx, store, key)
		if errors.Is(err, storage.ErrNotFound) {
			return fail(archive.EvalErrorNotFound, "no archived session with this ID")
		}
		if err != nil {
			return fail(archive.EvalErrorReadFailed, "the session metadata could not be read or verified")
		}
		if actualKey, err := archive.MetadataObjectKey(metadata.Harness.Name, metadata.SessionID); err != nil || actualKey != key {
			return fail(archive.EvalErrorReadFailed, "the session metadata does not match the requested identity")
		}
		record := archive.EvalExportFromMetadata(metadata, archive.EvalExportSourceArchive)
		if err := archive.ValidateEvalExport(record); err != nil {
			return fail(archive.EvalErrorReadFailed, "the session metadata is not valid: "+err.Error())
		}
		return archive.FitEvalExport(record, opts.maxBytes)
	}
	metadata, bundle, err := reader.RefreshAndLoad(ctx, store, key, reader.Limits{})
	if errors.Is(err, storage.ErrNotFound) {
		return fail(archive.EvalErrorNotFound, "no archived session with this ID")
	}
	if err != nil {
		return fail(archive.EvalErrorReadFailed, "the session metadata or filtered source could not be read or verified")
	}
	if actualKey, err := archive.MetadataObjectKey(metadata.Harness.Name, metadata.SessionID); err != nil || actualKey != key {
		return fail(archive.EvalErrorReadFailed, "the session metadata does not match the requested identity")
	}
	if parsers == nil {
		return fail(archive.EvalErrorParseFailed, "the filtered source could not be parsed")
	}
	parser, ok := parsers.LookupParser(bundle.Capture.Harness.Name)
	if !ok {
		return fail(archive.EvalErrorParseFailed, "the filtered source could not be parsed")
	}
	analysis, err := agentapi.Analyze(ctx, parser, bundle)
	if err != nil {
		return fail(archive.EvalErrorParseFailed, "the filtered source could not be parsed")
	}
	record, err := archive.BuildEvalExportWithAnalysis(bundle, analysis, metadata, archive.EvalExportSourceArchive, archive.EvalExportDetailFull, archive.ParserInfo{Version: parser.Version()})
	if err != nil {
		return fail(archive.EvalErrorParseFailed, "the filtered source could not be parsed")
	}
	if err := archive.ValidateEvalExport(record); err != nil {
		return fail(archive.EvalErrorReadFailed, "the session metadata is not valid: "+err.Error())
	}
	return archive.FitEvalExport(record, opts.maxBytes)
}

// exportLocal is the record of a transcript file: filtered as the collector
// filters it (collector.FilterTranscriptFile), as handoff --file does, then
// summarized with the running parser. Nothing is read from or written to the
// data directory.
func (x *evalExporter) exportLocal(input evalInput) any {
	fail := func(path string, code archive.EvalErrorCode, message string) any {
		record := archive.NewEvalExportError(archive.EvalExportSourceLocal, input.given, code, message)
		record.TranscriptPath = path
		return record
	}
	path, err := filepath.Abs(input.path)
	if err != nil {
		return fail("", archive.EvalErrorReadFailed, err.Error())
	}
	path = filepath.Clean(path)
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return fail(path, archive.EvalErrorNotFound, "no such transcript file")
	}
	if err != nil {
		return fail(path, archive.EvalErrorReadFailed, err.Error())
	}
	if !info.Mode().IsRegular() {
		return fail(path, archive.EvalErrorReadFailed, "not a regular file")
	}
	harness := input.harness
	if harness == "" {
		if harness = x.transcriptHarness(path); harness == "" {
			return fail(path, archive.EvalErrorUnknownHarness, "not in a folder Claude Code, Codex, or Cursor keeps transcripts in; pass --harness")
		}
	}
	freshStart := input.startedAt
	if freshStart.IsZero() {
		freshStart = info.ModTime()
	}
	filtered, adapter, err := x.filterLocal(harness, path, freshStart)
	if err != nil {
		return fail(path, archive.EvalErrorReadFailed, "the transcript could not be read safely or filtered; check its format and file permissions")
	}
	nativeID := input.nativeID
	// Discovery admits a raw native identity; retained records supply the
	// sanitized identity for output. Text-only transcripts retain discovery's
	// identity because they have no structured records to sanitize.
	if nativeID == "" || len(filtered.Records) > 0 {
		nativeID = transcriptSessionID(adapter, path, filtered)
	}
	sum := sha256.Sum256([]byte(path))
	root := input.projectRoot
	registrationRoot := root
	if registrationRoot == "" {
		registrationRoot = filepath.Dir(path)
	}
	reg := archive.SessionRegistration{
		ArchiveSessionID: "file-" + hex.EncodeToString(sum[:])[:16], NativeSessionID: nativeID,
		ProjectID: "file", ProjectRoot: registrationRoot, Harness: archive.Harness{Name: adapter.Name()},
		SessionStartedAt: info.ModTime(),
	}
	now := x.env.now().UTC()
	bundle, err := archive.NewSourceBundle(reg, adapter, filtered, now, nil)
	if err != nil {
		return fail(path, archive.EvalErrorReadFailed, "the filtered transcript could not form a source bundle")
	}
	startedAt := input.startedAt
	if !filtered.NativeStartAt.IsZero() {
		startedAt = filtered.NativeStartAt
	}
	parser, _ := x.env.agentRegistry().LookupParser(bundle.Capture.Harness.Name)
	analysis, parseErr := agentapi.Analyze(x.ctx, parser, bundle)
	var parserVersion string
	if parser != nil {
		parserVersion = parser.Version()
	}
	parserInfo := archive.ParserInfo{Version: parserVersion}
	record, err := archive.BuildLocalEvalExportWithAnalysis(bundle, analysis, parseErr, archive.LocalTranscript{Path: path, ProjectRoot: root, StartedAt: startedAt, Now: now}, x.opts.detail, parserInfo)
	if err != nil {
		return fail(path, archive.EvalErrorParseFailed, "the filtered transcript could not be parsed")
	}
	return archive.FitEvalExport(record, x.opts.maxBytes)
}

// filterLocal owns a provider pass and snapshot for this worker and closes
// both on every outcome. Filtering sees the pool's cancellation context.
func (x *evalExporter) filterLocal(harness, path string, startedAt time.Time) (out archive.FilteredTranscript, adapter archive.Adapter, resultErr error) {
	provider, _, ok := x.env.agentRegistry().LookupSources(harness)
	if !ok {
		return out, nil, errors.New("native source integration unavailable")
	}
	pass, err := provider.OpenPass(x.ctx, agentapi.SourceEnvironment{})
	if err != nil {
		return out, nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, pass.Close()) }()
	snapshot, err := pass.Read(x.ctx, agentapi.SourceRef{Kind: archive.SourceKindFile, Path: path}, agentapi.ReadLimits{RawBytes: collector.DefaultMaxRawTranscriptBytes, RecordBytes: archive.MaxRecordBytes})
	if err != nil {
		return out, nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, snapshot.Close()) }()
	file := snapshot.Input().File
	if file == nil {
		return out, nil, errors.New("native file input required")
	}
	return collector.FilterTranscriptSnapshot(x.ctx, file, harness, startedAt, collector.DefaultMaxTranscriptBytes, x.env.agentRegistry())
}

// transcriptSessionID consumes the integration's sanitized identity facts and
// optional filename policy. The generic fallback preserves original spelling.
func transcriptSessionID(adapter archive.Adapter, path string, filtered archive.FilteredTranscript) string {
	identity := filtered.LocalIdentity
	if resolver, ok := adapter.(agentapi.LocalIdentityResolver); ok {
		identity = resolver.LocalIdentity(filtered, filepath.Base(path))
	}
	if identity.ID != "" {
		return identity.ID
	}
	return strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
}

// transcriptHarness infers one owner from declared native path recognition ports.
// Ambiguous or unavailable locations require an explicit --harness.
func (x *evalExporter) transcriptHarness(path string) string {
	home, _ := x.env.userHomeDir()
	dirs := x.env.nativeSessionDirectories(home, config.Config{})
	registry := x.env.agentRegistry()
	var owner string
	for _, name := range registry.NativePathAgents() {
		provider, _ := registry.LookupNativePaths(name)
		recognizer, ok := provider.(agentapi.TranscriptPathRecognizer)
		if !ok {
			continue
		}
		observed := agentapi.NativePathEnvironment{Locations: agentapi.NativeLocations{UserHome: home, Directories: dirs[name]}, OperatingSystem: x.env.operatingSystem(), Getenv: x.env.getenv}
		if !recognizer.RecognizesTranscriptPath(observed, path) {
			continue
		}
		if owner != "" && owner != name {
			return ""
		}
		owner = name
	}
	return owner
}

// writeEvalRecord writes one record as a line of JSON. Its strings come from
// filtered data, so the line goes through archive.DisplayJSON: a C1 control or
// bidi override is written as a \u escape, never raw.
func writeEvalRecord(w io.Writer, record any) error {
	data, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("encode record: %w", err)
	}
	data = append(archive.DisplayJSON(data), '\n')
	n, err := w.Write(data)
	if err != nil {
		return fmt.Errorf("write record: %w", err)
	}
	if n != len(data) {
		return fmt.Errorf("write record: %w", io.ErrShortWrite)
	}
	return nil
}
