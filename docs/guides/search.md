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
stop search commands and remove that search directory; archive sources remain.
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

Checksum-verified sources in unsupported immutable formats (including legacy
source schema 1) become explicit `unsupported_source_schema` coverage gaps.
They are not downloaded repeatedly; a changed source or extraction contract
rechecks them. Corrupt, unverified and transiently unreadable sources remain
retryable failures. Unsupported content is never indexed.
