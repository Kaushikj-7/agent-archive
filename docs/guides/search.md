# Search conversation text

Content search finds indexed titles and visible user/assistant messages in
supported filtered retained sources, including supported preserved revisions.
It keeps private local searchable text. Ordinary queries read only an existing
local index; they never discover archives, download sources or index content.
A missing index returns partial empty results without creating files.

```sh
agent-archive search --refresh
agent-archive search "upload checksum" --json
agent-archive search '"checksum mismatch"' --all-projects
agent-archive search 'check*' --project my-project --since 7d
agent-archive search --refresh --session SESSION_ID --harness codex
```

Search defaults to the current repository/project using existing worktree-aware
repository identity and configured project fallback. Broaden with --all-projects;
empty project results never broaden automatically. Replays and child sessions
are hidden by default; --replays include and --children include reveal them.
Scope/date/harness/replay/child filters apply before ranking and limiting.
Namespace refresh discovery is independent of the query's project filters.

Words are ANDed within one message passage. Quotes match phrases. Only an
explicit trailing * enables prefix matching. Case is ignored; accents remain
significant. Punctuation splits words, including filenames and error strings.
Queries allow at most 512 UTF-8 bytes and 32 terms. This is keyword matching,
not substring or semantic search. Messages use 8KiB passages with about 1KiB
overlap; very long tokens/phrases across that overlap can be missed. Results
are grouped by session; match_count counts matching passages, including overlap.
Title-only results have no reference; use show with the exact session ID.

```sh
agent-archive search --read REF --max-bytes 4096 --json
agent-archive search --read REF --offset NEXT_OFFSET --max-bytes 4096 --json
```

References bind exact source membership, bytes, interpretation and message
location, never rendered line numbers. Reads never silently substitute newer
content. A next_offset of null means that referenced passage is complete.
Each context slice identifies its own reference and raw UTF-8 offsets.
The existing plain read prints an escaped quoted string and continuation on
stderr. --limit defaults to 20 (1–100); --cursor continues the same query/filters
until an index write invalidates its generation and requires a restart.

Tools, reasoning, attachments, generated context and copied other-thread history
are excluded according to parser visibility and ownership rules. Unknown privacy
versions, versions older than 6 or newer than this binary, unsupported formats and
unreadable sources remain explicit coverage gaps. No source cache is retained
merely for search. Changed source snapshots are parsed in full, while unchanged
sources and display-only changes reuse projections. Temporary failures retain
backoff; oversized work discards staging and blocks until its desired state changes.

The private index is under AGENT_ARCHIVE_HOME/search/, partitioned by archive
destination, installation, destination activation and privacy version. It is
separate from the metadata cache. No automatic indexing job is installed by these
commands. Uninstall retains local archive data. To discard searchable text,
disable automatic indexing with `setup --yes --search-indexing off`, stop explicit
refresh commands, and remove that search directory; archive sources remain.
See [privacy](../security/privacy.md) and
[engine contracts](../../dev/proposals/content-search-engine.md).

## Bounded JSON, catch-up and context

`search "words" --json` writes one `schema_version: 1` document, described by
[the search output schema](../../schemas/search-output.schema.json). Result and
context text contain conversation content. stdout holds the document; diagnostics
use stderr. Cached queries return success even with missing or partial coverage.
Outside a known project, choose `--project NAME` or `--all-projects` explicitly.
An older local schema requires explicit `search --refresh`; cached readers never
create an index or acquire SQLite writer admission to upgrade one.

`search --refresh --wait 2m` runs successive bounded passes. Namespace passes
normally process at most four sources and sixteen sessions, with a 30-second
outer budget and a 20-second discovery slice. Named refresh retains its maximum
64-source pass. Wait accepts an explicit duration from 1s through 10m; throughput
and archive size determine progress, so the duration does not promise catch-up.
Known failures, source gaps and incomplete authority cannot count as completion.
An idle legacy listing that cannot prove coverage stops with
`coverage_incomplete`; use bounded `--refresh` for best-effort progress there.
Wait expiry returns `wait_timeout`, exit 1, and preserves progress for retry.
Named waits check their target independently of other pending sessions.

`search --refresh --rebuild` atomically discards derived local state and starts
bounded catch-up; it preserves archive sources and invalidates old cursors.
This clears searchable state, rather than securely erasing SQLite/WAL bytes.
Refresh-only and exact-reference reads can run outside a project. Explicit
refresh remains allowed while automatic collection is paused. Index presence
never implies consent to future automatic indexing.

Discovery rotates first access to the unchanged pass budget among known-session
checks, recent publications and historical catch-up. Each class gets first access
within three passes. The four recent entries also rotate their starting entry,
so one oversized or timed-out body cannot hide its neighbors; each gets a first
recent-body opportunity within twelve passes while that recent set remains stable.
A fitting large metadata body can use the whole remaining pass budget on its
turn. Metadata reads reserve500ms of the existing discovery deadline for recording
failed-item progress (a fresh20-second pass therefore allows at most roughly19.5
seconds for one metadata read, less earlier work). Parent cancellation or stale
catalog authority still stops immediately. These are finite work limits, not
remote latency guarantees.

Catch-up checkpoints survive interruption and process restart. New catalog roots
resume by physical identity key, including tombstones; new records behind that
checkpoint are found in a later sweep. Partial or moving-root discovery never
proves deletion or complete coverage. Exact fresh authority checks remove known
sessions confirmed absent. Legacy paged archives report partial coverage even
when a listing walk finishes, since those pages do not form an immutable snapshot.
Stores without bounded listing report an explicit discovery error. Unreadable
metadata reports an error and keeps coverage partial while other identities
continue to advance; the failed identity is retried in a later sweep.

Use `search --read REF --before 1 --after 1 --json` for neighboring visible
messages from that exact verified source. `--max-bytes` limits each raw UTF-8
text slice; `--max-output-bytes` limits the complete escaped stdout document,
including metadata and newline (default 256KiB, maximum 1MiB). Context may shrink
text to fit and supplies exact raw byte continuation offsets. Escaping never
changes offsets or opaque references. A result page too large for the output
budget fails before stdout with `output_too_large`; lower `--limit` or increase
the budget. It never drops hits while moving the cursor beyond them.

Exit 2 means invalid command syntax or incompatible options; exit 1 means an
operational failure, stale cursor/source, unavailable interpretation, or an
incomplete wait. JSON operational errors have a stable code and bounded message.
Changed/deleted/unsupported/unavailable source states are distinct. A changed
cursor requires restarting the same query; no transaction survives user think
time. Unknown matching totals are null, not zero. Coverage counters describe
locally known namespace sessions, independently of the query's project filters.

Explicit refresh upgrades older local search databases under its bounded pass
budget, including a one-time local coverage-index build proportional to installed
session rows. Cached queries require the current schema and request explicit
refresh instead of running migration. Schema3 upgrades to schema4 by adding a
finite discovery-turn value while preserving existing identity/root checkpoints.
An interrupted migration rolls back; bounded identity validation resumes during
explicit refresh. Legacy metadata retry resumes by logical key within a physical
page without treating that mutable page as snapshot authority.

When continuing a cursor with relative date filters such as --since 7d, use the
absolute since/until timestamps reported in JSON filters. Relative ages resolve
again on each invocation and otherwise change the query identity. After a failed
refresh whose observation could not be read, coverage is null; successful queries
against a missing index instead report known local zero counts and index_present
false. Unknown matching totals always remain null.

## Browse matches on a terminal

When stdin and stdout are terminals, a content query opens the shared session
browser. JSON and piped output stay bounded ordinary output. The original FTS
query remains fixed, including phrase spacing. Metadata `/` filtering is disabled
in this mode. Results preserve engine relevance and the requested scope; no
project regrouping or automatic scope widening occurs.

Each row shows the best snippet and any known other indexed title/body matches.
These are indexed passages, including overlap, rather than text occurrences.
Enter a listed number or unique short ID to open its exact source-bound context.
The number resolves the full harness/session identity even when short IDs collide.
A title match has no body reference and explains that instead of opening an
unrelated transcript. In details, `n`/`p` navigate matching **body** passages,
`m` follows the displayed raw-byte continuation in the current message, arrows
and page keys scroll the current bounded text, `b` returns, and `q` exits.
The line fallback accepts the same letter actions and pgup/pgdn, up/down,
home/end to scroll the current text. Long unbroken Unicode text wraps into
physical rows so its tail remains reachable on narrow terminals.

The list's `n`/`p` or page keys page already loaded results. `o` requests the next
local indexed result page; `r` explicitly reruns the original query. At most
three result pages (each bounded by --limit, up to 100) are retained. Match
navigation retains one page of at most 20 references and uses forward/reverse
keyset cursors. Context retains only the current bounded selected/neighbor text.
Painting, scrolling and resizing never fetch metadata/sources or refresh the
index. An index write invalidates local paging: results remain usable, and an
explicit rerun resets navigation rather than appending a different ranking.
Changed, deleted or unavailable exact sources leave an explanatory notice and
the usable result list.

`h` is labelled **Hand off current session**. It closes browser reads and restores
the terminal before invoking the ordinary handoff command with the selected
full harness/session ID and archive source. Existing repository, destination and
launch gates apply. It hands off the authorized current session, which may differ
from a historical search match. Searching does not authorize indexing, setup,
refresh or rebuild.
## Automatic indexing and publication hints

Enable automatic indexing on this machine with
`agent-archive setup --yes --search-indexing on`; use `off` to disable it.
The worker may create a fresh index or retry an interrupted empty initial
creation. An existing old or future schema requires explicit refresh with a
supported binary; automatic passes and hints never migrate it.
Omitting this flag preserves the local choice. Fresh setup, pairing and upgrades
leave automatic indexing off. Setup installs a separate search job on a
one-minute advisory cadence. A large archive takes multiple bounded passes;
there is no freshness deadline. Pause waits briefly for an active pass or refuses
before saving. It stops automatic indexing and reconciliation; explicit
`search --refresh` remains allowed while paused and never changes consent.

After collection, verification, retention and machine registration release the
collector lock, a bounded local hint can mark harness/session identities for
recheck. The hint performs no archive GET, parsing or indexing. It drops when a
worker/lifecycle change is busy, the cache is absent or old, or the captured
namespace no longer matches. Periodic reconciliation recovers missed hints.

Coverage reports `pending_rechecks` separately from pending extraction work.
Hints preserve the real last metadata revision and do not invent a new desired
version. Historical discovery time (`last_reconciled`) remains unchanged by a
hint. `last_observed` is the maximum retained settled identity observation or
discovery time, so it can conservatively decrease while identities await recheck.
Identity-check completion stays false until those identities have
been authoritatively rechecked. A normal known-identity sweep either observes
current metadata (including an unchanged revision) or forgets a confirmed absence.
It keeps its fair cursor; an identity inserted behind it waits for the next cycle.
The last observed timestamp is not a historical high-water mark or proof that
every identity has already been checked at that time.

Disabling retains the cache for offline queries. `uninstall --delete-local-data`
removes it and its search logs. The permanent lifecycle writer fence remains
once enabled. Older binaries refuse this configuration during ordinary setup,
refresh and uninstall, and normal recovery rejects the two-job journal before
changing jobs or configuration. Use a current binary for recovery. An older
binary's explicit `setup --abandon-recovery` escape hatch can still rename an
unreadable journal aside, removing the pending-journal gate while leaving
application files and jobs as they are.
