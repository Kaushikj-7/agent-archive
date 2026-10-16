# First local handoff before bucket setup

> **Implemented in the local handoff changes.** Read-only discovery/I/O extraction
> and native preview/browser/launch integration follow the accepted design below.
> Synthetic filesystem and launcher coverage is included. Disposable actual
> Claude Code/Codex histories and current identity variables on macOS and Linux
> remain unverified; no live-platform acceptance is claimed. Prepared 2026-10-01.


## Goal

A person who has installed agent-archive can run `agent-archive handoff`
inside a project, choose an existing local conversation, and continue it in
another coding agent, without configuring a bucket or locating a transcript
file by hand.

“One-time” describes each operation: discovery and filtering happen on demand.
The command can be used repeatedly, but it installs no capture hooks, starts
no background collector, and maintains no durable local session archive.

## Current behavior and the missing piece

`handoff --file PATH --harness claude --to codex` already works before setup.
`handoffFromFile` in `internal/cli/handoff.go` filters the native transcript
and builds a source bundle in memory; the existing renderer and launcher can
then deliver it. The difficult part for a new user is finding that file.

The normal picker, title queries, and `--latest` depend on configured storage
and local session registrations. Native transcripts that predate setup do
not become picker candidates just because they exist on disk.

Backfill already discovers native transcripts and resolves their project and
session identities. Its discovery implementation is internal to the import
workflow; reuse requires separating read-only discovery from import policy,
planning, and registration.

The earlier [portability proposal](portable-handoff-and-onboarding.md)
excluded persistent local-only setup and additional storage backends. This
proposal preserves that scope decision and extends the existing pre-setup
file handoff with discovery and selection.

## Accepted design decisions

| Decision | Behavior |
| --- | --- |
| First-release harnesses | Claude Code and Codex automatic discovery; Cursor is a separate follow-up. Existing explicit-file support stays available. |
| Preview depth | Start with 50 bounded previews; load another 50 only on an explicit request for older candidates. Measure the batch size before finalizing it. |
| Current-session handoff | Match an exact native identity when available. Otherwise offer terminal selection or require an explicit command inside an agent; never substitute newest for current. |
| Latest local session | The eligible native transcript with the newest file modification time. Recorded timestamps are supporting detail, not a second ordering clock. |
| Abstraction scope | Extract shared discovery and verified reading primitives; preserve configured resolution. No generic backend, adapter registry, or session repository framework. |

The remaining work is verification and calibration, not reopening these scope
choices. Copying or restoring transcripts can change modification times;
document that limitation of local `--latest`.

## User experience

```text
$ agent-archive handoff

Local sessions · my-project · archiving not configured
▸ Fix OAuth callback tests      Claude Code    18 minutes ago
  Investigate token refresh     Codex          yesterday

Continue in:
▸ Codex
  Claude Code
  Save handoff to a file
```

Use the existing session browser and destination chooser; the screen above
illustrates behavior rather than specifying a second UI. Once selected,
filter the source and launch the destination through the current handoff
pipeline. State on stderr that the source is local and automatic archiving
is not configured. Do not interrupt the handoff with a setup offer. The
getting-started guide can introduce `setup` for backup and cross-machine use
after showing the local handoff recipe.

Recommended command behavior before setup:

| Command | Behavior |
| --- | --- |
| `handoff` | Browse native local sessions in the current checkout. |
| `handoff --latest --harness claude --to codex` | Select the eligible Claude transcript with the newest modification time in the checkout and launch Codex. |
| `handoff "OAuth" --to codex` | Apply the existing metadata word matcher to discovered, filtered session labels; disambiguate as today. |
| `handoff --to codex` inside an agent | Use an exactly identifiable current native session; otherwise report that it could not be identified. |
| `handoff --source local` | Use native discovery before setup. |
| `handoff --source archive` | Explain that archive access requires setup; do not discover local sessions instead. |
| `handoff --file PATH --harness NAME` | Keep the existing explicit-file path. |

Inside agents and pipes, preserve current noninteractive behavior: explicit
selection, bounded output, no picker, and no guessing on ambiguous queries.
The utility does not install skills before setup. Users invoke the terminal
command or explicitly ask their agent to run it.

## Scope and selection

Initial scope is Claude Code and Codex native transcripts on
supported macOS and Linux installations. Cursor discovery is deferred because
database-only chats and text transcripts require separate fidelity and
snapshot handling. The explicit-file path keeps its existing supported
harnesses. A first-run message should state which apps were searched.

- Default to the current checkout, including its subdirectories, using the
  recorded working directory and canonical paths. Do not search sibling
  worktrees solely because their current origin remotes match.
- Honor `--project DIR` as an explicit directory. A configured project label
  has no meaning before setup; explain that a directory is needed.
- `--all-projects` is the explicit way to broaden native discovery. A query
  with no checkout match reports that scope rather than automatically reading
  conversations from unrelated projects.
- Prefer top-level sessions. Keep subagent-only discovery outside v1.
- For `--latest`, order by file modification time consistently and label it
  as local transcript modification time. Apply the existing current-session skip
  rule when the person asks for another session.
- For direct `--to`, match an available harness session variable exactly to
  a discovered native identity. If the variable is unavailable or ambiguous,
  offer a picker on a terminal or require explicit selection inside an agent.
  Do not use latest as a substitute for current. Verify
  `CLAUDE_CODE_SESSION_ID` and `CODEX_THREAD_ID` in disposable interactive
  sessions before claiming automatic identity support; the current code
  comments say the Codex variable has not been observed.

Before setup, use native IDs qualified by harness as selectable identities.
Short IDs must disambiguate within the discovered candidates. These IDs are
not archive IDs: label them as local and emit a runnable local handoff recipe,
not a `show` command that requires an archive. Never persist a synthetic
registration merely to fit the existing picker interface.

After setup, preserve the current configured discovery, eligibility, and
archive resolution paths. This proposal must not make excluded projects or
unregistered native transcripts silently eligible in configured installs.
Setup later still requires explicit capture choices and backfill consent.

## Implementation

1. Extract a read-only native discovery component from backfill. It returns
   harness, native ID, transcript reference, recorded checkout, activity
   evidence, and coverage diagnostics. Keep import eligibility and state
   writes in backfill, and preserve its existing behavior with shared tests.
2. Add a pre-setup resolver selected only when configuration is absent.
   Present ephemeral candidates through the shared browser. Broaden its
   candidate/action abstraction where necessary instead of fabricating state
   files or bucket keys.
3. Read lightweight identity headers first. Filter transcripts on demand for
   names and first-prompt previews, and filter the selected transcript again
   for the full handoff. Revalidate identity and checkout when it is opened;
   fail clearly if the file disappeared or changed identity.
4. Reuse the source-bundle, rendering, byte-budget, launch, destination, and
   worktree paths. Preserve branch-difference and active-checkout warnings.
   A selected transcript's recorded directory is evidence; the destination
   still starts in the user's chosen checkout.

Inspect labels for the newest 50 in-scope native sessions initially. Offer
“Load older sessions” after the loaded batch; each explicit request previews
another 50. Report when more candidates exist. A word query searches only the
loaded preview window and says so; exact native-ID selection bypasses that
window but still verifies identity. A bounded query must never claim
exhaustive absence. `--latest` requires complete enumeration and identity/
checkout inspection, but does not require previewing every title.

## Engineering design

The declarations in this section are proposed Go API shapes, not existing
symbols. Keep three stages separate: enumerate file references, inspect
bounded identity/preview evidence, and load the selected full transcript.
Only the third stage builds a complete source bundle.
Package names and interface shapes are provisional: extract only primitives
that implementation actually shares. The filesystem test boundary and staged
read behavior are requirements; a particular new package layout is not.

### Package boundaries

| Owner | Responsibility | Dependencies it must not acquire |
| --- | --- | --- |
| `internal/nativesessions` (new) | Enumerate Claude/Codex stores; inspect identity and recorded cwd; checkout scoping; candidate ordering and coverage. | Config, state, storage, credentials, collector, CLI, terminal UI. |
| `internal/transcriptio` (new, small) | Open a verified regular file; expose a fixed read boundary; read bounded complete JSONL records from head/tail. | Archive policy, app discovery, setup, network, UI. |
| `internal/archive` | Extract filtered display labels from preview records using the same credential/instruction rules as full filtering. | Filesystem access or native-store traversal. |
| `internal/collector` | Adapt a verified snapshot to the existing transcript filter, and return preview/full filtered data without capture side effects. | Native discovery or picker behavior. |
| `internal/cli` | Choose pre-setup versus configured path; map candidates to browser rows; resolve the chosen reference; render and launch. | Import-policy logic in the new path. |
| `internal/backfill` | Consume shared discovery facts, then apply its existing import policy and registration workflow. | Pre-setup handoff selection policy. |

The small `transcriptio` package is justified by two real consumers: discovery
needs safe bounded reads, and collector needs the same handle for preview and
full filtering. Move the relevant regular-file opening/JSONL-boundary helpers
out of collector rather than implement another variant in discovery. Preserve
existing collector behavior: rejecting discovered symlinks is a stricter
option for native discovery, not a blanket change to hook/file inputs. Update
the architecture import/program-access guards alongside this extraction.

Move Claude/Codex directory-layout and header parsing from backfill into
`nativesessions`. Keep Cursor database discovery, `.git` worktree folding,
configured-project matching, and import skip precedence in backfill. A native
candidate's recorded cwd is a fact; a backfill project root is a policy
decision. Do not use `backfill.BuildPlan` or export its `Candidate` as the
handoff discovery API.

### Discovery and candidate types

Use concrete values and functions, with one small filesystem port. Do not
introduce an adapter registry or a generalized session repository interface
for two known file layouts.

```go
// internal/transcriptio
type File interface {
    io.ReaderAt
    io.Closer
    Stat() (fs.FileInfo, error)
}

// internal/nativesessions
type FileSystem interface {
    ReadDir(path string) ([]fs.DirEntry, error)
    Lstat(path string) (fs.FileInfo, error)
    OpenRegular(path string) (transcriptio.File, error)
    EvalSymlinks(path string) (string, error)
}

type StoreRoot struct {
    Harness string // canonical "claude" or "codex"
    Path    string // resolved app-store root; never a display label
}

type Ref struct {
    Harness string
    Path    string // process-local locator, never an archive object key
    Store   string // identifies the trusted discovery root
}

type Candidate struct {
    Ref        Ref
    NativeID   string
    Directory  string // recorded cwd, canonically resolved within this run
    StartedAt  time.Time
    ModifiedAt time.Time // sole ordering clock for local selection
    Stamp      transcriptio.Stamp // file identity, size, and modification time
}

type Scope struct {
    Directories []string // chosen checkout, raw/canonical forms
    All         bool
}

type Limits struct {
    Files       int   // maximum header-inspected file references
    HeaderBytes int64 // per-file header budget
    TailBytes   int64 // per-file activity/preview tail budget
    TotalBytes  int64 // aggregate discovery/preview read budget
    Previews    int   // maximum candidates whose text labels are inspected
    Workers     int
}

type Coverage struct {
    Enumerated, Inspected, InScope, Previewed, Skipped int
    IdentityComplete bool // all enumerated identities/scopes inspected
    PreviewComplete  bool // labels inspected for every in-scope candidate
    Complete bool // enumeration, identity, and previews all complete
    Reason   CoverageReason
}

func Discover(ctx context.Context, files FileSystem, roots []StoreRoot,
    scope Scope, limits Limits) (Result, error)

type Result struct {
    Candidates []Candidate
    Coverage   Coverage
    Issues     []Issue // bounded codes/counts, not raw transcript text
}

// Shared file-layout enumeration; backfill adds its own header/import policy.
// Returning false from visit stops enumeration and marks coverage incomplete.
func Walk(ctx context.Context, files FileSystem, roots []StoreRoot,
    maxFiles int, visit func(Ref) (bool, error)) (Coverage, error)
```

`transcriptio.Stamp`, `CoverageReason`, and `Issue` are small typed
values. Stamp retains a private file-identity comparison token as well as
size/mtime; do not assume size/mtime uniquely identify a file. None of these
types is a JSON persistence format. Constructors normalize roots and limits;
nil production dependencies are not silently replaced inside pure functions.
The CLI supplies resolved app locations and environment overrides once.
`Discover` consumes `Walk`, inspects identity and scope, and returns
compact candidates in modification-time order. It does not call collector or populate text
labels. The CLI preview stage adds its read/skip counts to coverage; backfill
consumes `Walk` directly to preserve its complete import-plan behavior.
Enumeration completion is separate from identity and label coverage: a title
budget must not invalidate a complete identity lookup, while `--latest` must
not mistake a complete top-50 preview for complete store discovery.

Within discovery, use unexported harness-specific functions for directory
layouts and header parsing. Filter harnesses before enumerating their stores.
Read directories in chunks through an internal enumerator if `ReadDir`'s
whole-directory allocation shows up in benchmarks; that change does not
require a public discovery framework.

Deduplicate identical references first. Different files claiming the same
`(harness, nativeID)` remain an identity conflict rather than having one
silently win by modification time. Direct selection reports that conflict
and suggests the explicit-file path. Cache canonical cwd resolution per run,
so a thousand sessions in one checkout do not cause a thousand symlink walks.
Do not run git per candidate or reuse backfill's repository-folding resolver.

### Bounded previews and one full read

The picker needs a filtered name/first prompt, branch, and activity evidence,
not a normalized conversation for every row. Add an explicit preview result
and a filter entry point accepting a verified snapshot:

```go
// internal/transcriptio
type Snapshot struct {
    // Private: open handle, file identity, and initial size boundary.
}
type Opener interface {
    Lstat(path string) (fs.FileInfo, error)
    OpenRegular(path string) (File, error)
    EvalSymlinks(path string) (string, error)
}
type OpenPolicy struct {
    RejectSymlinks bool
    Root string // optional resolved containment boundary for discovery
}
func Open(files Opener, path string, policy OpenPolicy) (*Snapshot, error)
func (s *Snapshot) Close() error
func (s *Snapshot) Stamp() Stamp

// internal/collector
type TranscriptPreview struct {
    NativeID string
    Name, Title, Branch string // already privacy-filtered
    RecordedActivity time.Time // optional supporting detail, never ranking
    NameComplete bool
    Gaps []archive.CaptureGap
}
func PreviewTranscript(ctx context.Context, snapshot *transcriptio.Snapshot,
    harness string, limits PreviewLimits) (TranscriptPreview, error)

func FilterTranscriptSnapshot(ctx context.Context,
    snapshot *transcriptio.Snapshot, harness string, startedAt time.Time,
    maxBytes int64) (archive.FilteredTranscript, archive.Adapter, error)
```

The signatures describe ownership, not permission to pass sliced JSONL to an
adapter that expects a whole conversation. Implement preview extraction over
complete records through shared record-filtering helpers in `archive`. Use
the same secret redaction and instruction exclusion rules; the CLI never
derives a label from raw header/body text. A truncated record is skipped and
reported, not parsed as a complete record. Bound both total bytes and single
record bytes, and keep no raw excerpt after preview returns.

Read an initial record window for identity/cwd/first prompt and a bounded
tail for recent recorded timestamps or a recent custom title. Tail readers
skip a leading partial record and an unfinished final line. The latest title
may lie outside those windows; use the filtered first prompt or native ID and
mark name coverage partial. Do not scan an entire file merely to recover a
pretty title. Missing recorded timestamps do not affect modification-time
ordering.

`PreviewLimits` contains explicit head/tail/record byte caps. Proposed starting
budgets: 256 KiB head, 256 KiB tail, 64 MiB aggregate preview/header bytes,
two workers, and 50 preview candidates per explicitly requested batch. Oversized records can therefore make
identity or labels unknown; report that honestly. Preserve backfill's current
8 MiB header cap and 16-record Codex metadata rule when migrating it. Do not
silently impose the handoff preview budgets on imports.

After selection, open a fresh snapshot, verify native identity and cwd, and
filter the complete transcript up to the captured file-size boundary with
existing collector limits. Do not reuse stale preview text as the full
handoff. Appending new records is normal: include complete appended records
visible when the selected snapshot opens. A replaced file or changed session
identity is an error. Close every descriptor on success, failure, and cancel.
The filter should not reopen the path after verification; using the same
handle avoids a check/read race. In-place rewriting detected during a read
requires a bounded retry or an explicit changed-source failure.

Keep `FilterTranscriptFile` as a compatibility wrapper around the new
snapshot path so existing callers and size-limit behavior remain intact.

### CLI integration without synthetic registrations

Today `runHandoffCommand` resolves to an archive ID before loading a target;
`resolveHandoffQuery`, `currentHandoffSession`, and `selectHandoffSession`
all assume registrations or configured storage. Add a native route before
those calls, after parsing flags and loading config exactly once:

```go
// internal/cli: internal result of selection, not a new backend API.
type handoffSelection struct {
    SessionID string // existing configured route
    Harness   string
    Native    *nativesessions.Candidate // pre-setup route only
}

func selectNativeHandoff(ctx context.Context, deps nativeHandoffDependencies,
    opts handoffOptions, input *typedInput,
    stdout, stderr io.Writer) (handoffSelection, bool, error)

func handoffFromNative(ctx context.Context, candidate nativesessions.Candidate,
    deps nativeTranscriptDependencies) (handoffTarget, error)
```

Constructors enforce that a selection holds either an existing session ID or
a native candidate. `--file` keeps its own explicit route. Missing config
enables native discovery; invalid config, incomplete setup/recovery, disabled
capture, or an inaccessible config do not get treated as a fresh installation.
`--source archive` refuses before discovery. Keep configured resolvers intact
in the first change rather than refactoring all sources into a polymorphic
backend.

`nativeHandoffDependencies` is a consumer-owned interface in
`read_dependencies.go`: filesystem/discovery construction, app-root lookup,
clock, current-session environment, working directory, and the existing
browser capabilities. It must expose no `openStore`, credential, capture, or
scheduler operation. Pure discovery, selection policy, and row formatting
should take values rather than the whole `Env`.

The browser already supports `pickSession` without a store. Add one private
`selectionKey string` to `listRow`, empty for existing callers. The native
caller maps opaque per-run keys to candidates; the browser returns the chosen
row, and the caller retrieves its candidate. `SessionID`/`fields.SessionID`
can contain the native ID for display/search, but the opaque key is never
parsed as an ID or fed to `MetadataObjectKey`. Do not add filesystem handles,
load callbacks, or `archive.SessionRegistration` to every browser row.

Use `rowChoices` for an already scoped native result set, with no automatic
fallback. `newScopeChoices` currently opens all projects on an empty scope,
which conflicts with this proposal. The first version can direct the user to
`--all-projects` instead of adding another browser scope-loading policy.
Once built, `/` filters in-memory rows only. It performs no filesystem reads
and cannot discover sessions beyond the declared preview window.
Add one caller-provided load-older action to picker mode, offered at the end
of the loaded candidates. Invoke it once on explicit activation, show progress,
then append rows while preserving existing selection keys and row numbers.
Do not attach filesystem reads to redraw, scrolling, or individual filter
keystrokes. Existing browser callers omit the action and retain their behavior.
If discovery itself was incomplete, say so; loading more labels cannot recover
file identities that the discovery budget never inspected.

Define identity selection before word matching: with `--harness`, a full
native ID or unique prefix addresses that harness; without it, require
uniqueness across supported harnesses. Prefixes retain the existing minimum
length. IDs must come from validated discovery, never become file paths, and
duplicates fail explicitly. Copyable recipes use `handoff NATIVE_ID --harness
NAME --source local`; the receiving prompt needs a native-source retrieval
recipe too. Adapt `launchHandoffPrompt`, which currently suggests registered
`show`/`handoff` commands, so it does not claim archive access for a native
selection. Keep synthetic source-bundle identity private to rendering and
out of user-facing retrieval commands.

### Ordering, limits, and performance guarantees

Use a deterministic order: file modification time descending, then harness,
native ID, and canonical reference. Directory/file-name order is not modification-time
order. Previewing only 50 candidates does not make the entire catalog free:
checkout scoping still needs bounded headers for candidate files, especially
for Codex histories spread across date directories.

Start with a 10,000-file enumeration/header-attempt cap in addition to the
aggregate byte budget. Stop traversal as well as reads at that cap; do not
first allocate an unbounded list of paths. Preserve counts and incompleteness
when a cap is reached; treat these
as proposed constants pending measurement, not new CLI flags. Explicit native
ID selection can use validated filename ID hints to narrow enumeration before
opening headers; it must still verify file contents. Ordinary discovery
stores compact facts only. Retain and sort the bounded in-scope candidate
catalog (`O(n log n)`, with n bounded by the 10,000-file cap), then preview
the first 50 and later batches on explicit request. This permits older-batch
loading without repeating discovery or losing tie-breaking stability. A
top-50-only heap would discard candidates the user subsequently needs, so it
is not the recommended implementation. Do not retain full records or
build/filter a `SourceBundle` for every session. Backfill continues
to retain the complete candidate set its plan needs via its own consumption
mode of the shared enumerator.

For `--latest`, choose the maximum modification time among eligible files
only when enumeration and identity/checkout inspection are complete. This
is a snapshot of local discovery, not a claim about the newest conversation
timestamp. If discovery is incomplete in a way that could hide a newer
candidate, refuse automatic `--latest` and offer the known candidates for
explicit selection. A preview-title limit alone does not cause refusal.
Neither a top-50 preview nor a date-folder shortcut proves that an
uninspected transcript is older. New files or appends after discovery are
normal; ordering is established at discovery time and the selected file is
revalidated at full-read time.

Use a fixed worker pool with bounded jobs/results queues. Reserve read bytes
before scheduling each preview; budget exhaustion yields a coverage reason.
Workers return immutable results, merged by one goroutine, so completion
order cannot change tie-breaking, errors, or which candidates get a budget.
No goroutine per file and no concurrent writes to shared slices/maps. Check
context between records and directory batches, and select on cancellation
when sending jobs/results. Use the command's cancellation context throughout;
do not copy current `context.Background()` calls into the native path.

The cost model is directory enumeration plus bounded header bytes, 50 bounded
previews per requested batch, then one full selected-transcript filter. Memory is
the capped compact candidate catalog, bounded per-worker buffers, and
the selected filtered bundle. Header parsing uses narrow typed JSON structs;
reuse scratch buffers only within a worker. Avoid `ReadFile`, whole-file
`json.Unmarshal`, regex over transcript bodies, global caches, and pooling
without allocation-profile evidence. Keep record text out of timing traces.

### Implementation sequence and performance checks

1. Extract shared file I/O and Claude/Codex discovery helpers, preserving
   backfill and collector tests. Add import-boundary guards for the new
   packages. No user-visible behavior change in this slice.
2. Add bounded preview extraction and pure native selection policy, including
   identity conflicts, coverage, deterministic modification-time ordering, and latest
   refusal. Verify that labels use the same privacy rules as full filtering.
3. Wire the missing-config branch, private row selection keys, native launch
   retrieval recipes, explicit older-batch loading, and temporary cleanup; update help and first-run docs.
   Existing configured calls continue through their original route.

Benchmark the enumerator/header parser/preview pipeline separately from
terminal drawing and process launch. Use 100, 1,000, and 10,000 file fixtures,
many sessions sharing one cwd, multiple stores, long/noisy records, and a
large transcript outside scope. Measure cold and warm filesystem runs,
time to first picker screen, bytes opened/read, peak concurrent descriptors,
allocations, and full-filter count. At 10,000 files the configured cap must
hold; beyond it coverage must become incomplete rather than scaling forever.

Instrument the read port in tests: a query/picker must perform zero full
filters before selection, changing `/` must perform zero new reads, and a
successful selection must perform one full filter in the no-race case.
Test canceled queues and handles for leaks, byte limits for adversarial
records, permutation-independent ordering, and backfill parity. Proposed
latency objective is a warm 1,000-session picker under one second on a
representative local SSD; report actual hardware and cold results before
adopting that as a release gate. Deterministic CI gates should assert I/O and
allocation bounds, not fragile wall-clock timings.

## Files, privacy, and cleanup

Discovery reads only known app stores and honors existing alternate app-home
locations. Reuse regular-file and identity checks; do not follow discovered
transcript symlinks outside those stores. Raw text must not reach the picker,
output, or saved handoff without the existing privacy filter.

Plain rendering creates no persistent archive or configuration. Explicit
`--output` writes the requested file with existing overwrite and permission
rules. Launching needs a private handoff file that survives until the new
agent reads it; reuse the private temporary launch-directory path before
setup. Do not delete it immediately after opening a terminal window.

Define utility-owned temporary-file cleanup independently of the collector:
subsequent local handoffs remove owned directories older than seven days in
the dedicated temporary namespace. Do not scan or delete unrelated temporary
files. Explain that this is best-effort cleanup and files can remain until a
later invocation or OS cleanup. A trimmed plain rendering retains the current
pre-setup behavior: no automatic full copy, with an explicit explanation and
the existing output options. Later `setup` does not import temporary handoffs.

Launching carries the same provider visibility as today's handoff: the
receiving coding agent reads the filtered conversation. Discovery itself
makes no storage requests and opens no storage credentials.

## Delivery and acceptance

Use the three implementation slices above: shared discovery/I/O extraction,
bounded previews and selection, then pre-setup browser/launch integration and
documentation. They introduce no public command or storage backend.

Required verification uses isolated fake app homes and fake launchers:

- No config, bucket, credentials, hooks, registrations, or scheduler are
  created by discovery, rendering, or selection.
- Claude and Codex histories can each be handed to the other agent through
  the picker, explicit native identity, words, and `--latest`.
- Checkout scoping, duplicate native IDs, current-session identification,
  malformed or changing files, missing apps, and incomplete scans produce
  truthful outcomes.
- No network or credential dependency is touched in the pre-setup path.
- Secrets and injected instruction fields do not reach labels or handoffs;
  temporary files use private permissions and survive asynchronous launch.
- Existing configured handoff, backfill, explicit-file rendering, and
  worktree behavior retain their tests and semantics.

Run a disposable live acceptance exercise with actual Claude Code and Codex
histories before claiming the first-run flow works on each supported platform.

## Remaining verification and calibration

1. Measure initial and older-batch preview costs on real histories; retain
   the 50-session starting batch unless measurements justify adjusting it.
   Enforce a per-command cumulative read budget across batches. Exhaustion
   disables further preview loading with an explanation and retains bounded,
   verified native-ID rows for explicit selection. Completed header inspection
   releases unused reservations by measured bytes before preview scheduling,
   rather than silently raising
   the budget; a new invocation starts a new bounded run.
2. Observe current-session environment variables in actual interactive
   Claude Code and Codex sessions. Test unavailable/ambiguous identity paths
   before documenting automatic current-session support.
3. Finalize Cursor discovery in a separate proposal/follow-up after checking
   database and text-transcript fidelity. It is not a first-release gate.
4. Confirm file/header/read budgets and the package extraction against
   benchmarks and existing architecture guards. Do not add abstraction or
   allocation optimizations without a demonstrated consumer or measured cost.
