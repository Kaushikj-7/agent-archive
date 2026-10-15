# Search conversation text

Content search finds words in indexed titles and visible user/assistant messages.
It keeps a private local copy of that text. This initial version indexes sessions
only when explicitly requested; it does not install background indexing or
change capture, uploads, `list`, `show`, or `handoff`.

Run a bounded archive refresh to discover sessions from every project, or find
a session ID using `agent-archive list` and refresh that exact session. Refresh
includes supported preserved native revisions:

```sh
agent-archive search --refresh
agent-archive search --refresh --session SESSION_ID --harness codex
agent-archive search "upload checksum"
agent-archive search '"checksum mismatch"' --all-projects
agent-archive search 'check*' --project my-project --since 7d
```

Normal queries read the local index and do not open remote storage or refresh
history. A missing index reports incomplete coverage without creating one.
Results are grouped by session with a matching excerpt. They search the current
repository using the same repository/name/project fallback as `list`; use
`--all-projects` to broaden. Replay and child sessions are hidden by default;
use `--replays include` or `--children include` to include them.

Words are ANDed within one message passage. Quotes match a phrase. Only an
explicit trailing `*` enables prefix matching. Case is ignored; accents are
significant. Punctuation splits words, so filenames and error strings match
word sequences rather than exact byte substrings. This is keyword search;
terms with similar meaning do not automatically match. Queries have a 512-byte
limit. Messages are chunked into 8KiB passages with approximately 1KiB overlap;
very long tokens/phrases across that overlap can be missed.

A body match includes an opaque `ref:` token. Read its exact source text with:

```sh
agent-archive search --read REF --max-bytes 4096
agent-archive search --read REF --offset NEXT_OFFSET --max-bytes 4096
```

Reads verify current source membership and bytes. They print a quoted escaped
text string, plus byte offsets and a continuation offset on stderr. Offset 0 in
the continuation means complete. References can expire after updates, deletion,
privacy/extractor changes or a destination switch; refresh the session and search
again. Title-only results have no passage reference; use `show SESSION_ID`.

`--limit` defaults to 20 and accepts 1–100. A result page may provide a
`Next cursor`; pass it with the same query/filters using `--cursor`. Index writes
expire cursors and require restarting pagination. No long database transaction
is held while you review a page.

Coverage is explicitly partial: sessions not indexed, sources not readable by the
parser, and changes on other machines may be absent. Refreshing a session reads
its latest archive sources; querying does not prove remote freshness. Tools,
reasoning, attachments, generated harness context and copied other-thread history
are excluded according to the existing parser's ownership/visibility rules.
Sources with privacy filters older than version 6, newer than this binary, or
unknown versions report a coverage gap because their visibility rules cannot be
interpreted safely.

The index is stored under `AGENT_ARCHIVE_HOME/search/` (or the normal private
data directory), separate from the metadata cache. It is partitioned by archive
destination, installation, destination activation and current privacy version.
Files are private to the owner. Explicit refresh works while capture is paused.
Uninstall retains this local data, like other archive state. To discard all
local searchable text, stop search commands and remove that `search` directory;
this does not delete archived sources. No downloaded source cache is retained.
See [privacy](../security/privacy.md) for what archive filtering retains and
[engine contracts](../../dev/proposals/content-search-engine.md) for measured
performance, tokenization and history limitations.

Explicit refresh records desired state in the same search database and executes
bounded indexing work. Completed sources survive interruption and are reused on
retry. Unchanged sources, including retained alternatives, need no new download
or parse; changing only titles or display/filter metadata avoids source reads.
Body passages are rewritten only when their projection changes. Changed source
snapshots are still parsed in full; indexing is not an append-only parser.

Refresh reports pending and failed work. Temporary failures use persisted
exponential backoff (one second up to 256 seconds); retry after that delay.
Oversized work discards staged projections and stays blocked until its desired
snapshot changes. A pass reads at most 64 sources and 32 MiB compressed data. Its
30-second deadline covers authoritative metadata reads and indexing, so sessions
with many retained revisions may require another refresh. Each pass can also
advance previously observed pending sessions. `search --refresh` discovers an archive slice across all projects; it does not
broaden the current-project scope of subsequent queries. A pass reserves up to
20 seconds for discovery and the remainder of its 30-second deadline for pending
indexing, including when discovery fails. Its defaults process up to 32 physical
identities/objects, 4 recent catalog entries, 8 exact known-session checks,
512 catalog nodes, and 16 MiB of discovery bytes, then up to 4 source downloads.
Catalog metadata and pre-install verification have separate finite budgets.
Run another explicit refresh to resume catch-up; no scheduler is installed.

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

Coverage distinguishes discovery of a stable observed catalog snapshot from
pending, failed, and unsupported retained-source work. Last observation is local
to this machine: unobserved remote deletions/privacy changes remain unknown until
refresh. Observing stricter privacy, changed interpretation, or removed retained
membership invalidates prior text before replacement. Indexing rechecks fresh
authority after source loading and before install. These checks do not establish
perpetual freshness after their observation. Normal queries remain network-free.

Opening an older local search database builds a local coverage index once; this
work scales with indexed session rows and reads no archive sources. Normal query
initialization and execution share a five-second deadline. Explicit refresh
initialization, discovery and indexing share a thirty-second deadline. An
interrupted schema migration rolls back and can be retried on the next open;
identity validation still advances separately through bounded refresh passes.
Schema3 indexes upgrade to schema4 by adding one finite discovery-turn value;
existing identity/root checkpoints are preserved. A failed legacy metadata read
can resume within its physical page by logical key without treating that mutable
page as snapshot authority.
