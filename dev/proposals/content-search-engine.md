# Conversation search engine contracts and baseline

This records the S1 engine slice of [the incremental indexing plan](content-search.md).
The minimal explicit CLI is described in [the search guide](../../docs/guides/search.md);
this is not a statement that archive coverage is complete.

## Boundaries

`internal/search` contains a pure projection over `archive.Analysis`, a concrete
SQLite store, and explicit reader/parser composition. It has no scheduler,
collector dependency, default home lookup, registration, or automatic work.
`LoadProjection` uses `reader.LoadSource` or `reader.LoadRevision`, preserving
compressed/decompressed limits, checksum, identity, history and provenance checks.
`LoadSession` includes active then preserved sources and fails on unreadable
sources so a caller can retry instead of claiming completeness.

`Extract` uses normalized `TurnKind`, role, record index and presentation text.
It excludes tool results, reasoning, generated context, notifications, compact
summaries, shell output and sidechain records according to the existing parser.
It does not interpret native JSON. Native event echoes without authored message
roles are absent in the parser; identical text with the same native message ID
is deduplicated. Identical prompts with distinct identities remain distinct.
Copied other-thread Codex prefixes follow the parser's `OwnRecord` policy and
are not attributed to the child's own conversation. Preserved alternatives are
indexed; overlapping physical rollout/thread/ordinal records with identical
role/text/chunk bounds are deduplicated within their session, preferring active.
There is no cross-session deduplication. Text-only sources and missing/failed
parsers return explicit coverage gaps. Sources with unknown privacy versions,
versions newer than this binary, or versions older than 6 report a
`historical_visibility` gap and contribute no message text; their visibility
rules cannot be interpreted safely.

Each reference binds destination/privacy context, session, source key/checksum/
size, revision membership, parser/filter/schema/extractor versions, retained
record position, and byte bounds in presentation text. Source reuse keys include
all this ownership/interpretation provenance and exclude display metadata.
Passages contain at most 8192 UTF-8 bytes with approximately 1024 bytes of overlap.
An unusually long token or phrase spanning more than that overlap may not match
across a passage boundary. Terms must match within one passage; unrelated words
in separate messages do not combine into a match.

`ReadPassage` rechecks metadata membership, loads verified bytes and resolves the
same extractor/parser provenance. It distinguishes changed, deleted, unsupported
and unavailable sources; reports exact byte offsets and continuation; and never
substitutes a different source version. Title-only results have no passage
reference. CLI callers can open their selected session rather than invent a text
location. The reader returns presentation text; terminal consumers must apply
shared display escaping before rendering it.

## Storage and matching

The store is separate from `SessionCatalog`, in a caller-selected private
0700 directory with 0600 database files. Its destination identifier must include
installation/privacy context. Opening the directory under another context fails.
The content index is disposable; schema mismatches require an explicit rebuild.
The eager explicit-session refresh caps aggregate compressed inputs and retained
passage text at 32MiB and 100,000 passages, processing one source at a time; each individual source
still has reader decompression limits. This is a finite initial CLI operation,
not a durable catch-up worker. WAL writes use a 100ms SQLite busy timeout and a 256-page auto-checkpoint. There
is one connection per store handle; downloads and parsing happen before writes.
Normal queries never read the archive. Database transactions end within a call,
including during pagination. WAL size and contention under workers remain S2/S4
acceptance, not a claim based on SQLite settings alone.

SQLite FTS5 is available in the existing `modernc.org/sqlite` driver. `unicode61`
uses case folding without stemming or accent folding. Punctuation splits tokens:
`foo_bar.go` is a phrase of `foo`, `bar`, `go`, not byte-substring matching.
CJK text follows Unicode61 tokenization rather than language-specific segmentation.
Whitespace-separated terms are ANDed; quotes create phrases; explicit trailing
`*` enables prefix matching, including on a phrase's final token. Raw FTS
operators, column qualifiers and negative syntax are quoted literal input.
Queries are at most 512 UTF-8 bytes and 32 clauses. Empty/punctuation-only terms,
unterminated phrases and interior wildcards fail with an input error.

Scope (project, harness, inclusive capture dates, replay and child membership)
is applied before session grouping and final limiting. Best match ordering is:
title before body, FTS BM25 relevance, user before assistant on equal relevance,
capture recency, session ID. Equal passages use their indexed row identity.
One result per session reports matching passage count (not occurrence count;
long-message overlap can increase this count). Only selected hits compute
snippets; snippets use shared display escaping and at most 512 runes.

Queries have a five-second context deadline to bound database read-view lifetime.
A timed-out query returns an error, never a silently truncated success.
Queries are exhaustive within the local index; there is no hidden candidate cap.
Output limits (1–100) do not bound FTS traversal or grouped ranking. Query plans
materialize scoped FTS scores and use temporary B-trees for session ranking and
final order. A common-word query therefore grows with the matching corpus.

Pagination is deterministic keyset ordering bound to the query and index
generation. Any committed replacement expires cursors with
`ErrCursorChanged`. No read transaction survives user think time. Continuous
index writes can require restart; stable cross-generation pagination is not
promised. Engine-only pages explicitly report unreconciled coverage; S3 owns
actual observation time, pending/failed counts and completeness.

## Reproduce the performance evidence

All fixtures are synthetic, use disposable directories and in-memory archives,
and never inspect real agent applications, a native home or a bucket.

```sh
go test -race -count=1 ./internal/search
go test -v ./internal/search -run TestScopedLiteralSearchAndSnippets
go test ./internal/search -run '^$' -bench . -benchmem -benchtime=3x -count=1
```

Initial local measurements: Intel Core i7-8559U, Darwin/amd64, Go 1.27.2,
`GOMAXPROCS=8`, 2026-10-14. The machine was not otherwise isolated. Observed
ranges across runs rather than guaranteed service levels:

| Workload | Observed time | Allocation / work |
| --- | --- | --- |
| Verify/decode/parse/project 1000 short native records | 19–66 ms | About 3.2 MB allocated, exactly 1 bounded GET and 1 parse |
| Warm common-word query, 2000 sessions × 10 passages | 280–481 ms | About 84–87 KB allocated, zero source GETs |
| Same common word scoped to 1% of projects | 52–145 ms | About 77–80 KB allocated, zero source GETs |
| Rare term, same corpus | 1.4–1.6 ms | About 11–12 KB allocated, zero source GETs |
| Cold local indexing, 250 sessions × 20 passages | 0.91–0.96 s | Exactly 5000 body FTS inserts; about 29 MB allocated |
| Local disk after that cold workload | 3.66 MB | Database + WAL + SHM, no downloaded source cache |

These tiny repetitive messages compress heavily: the verified-source benchmark
reads 1117 compressed bytes representing roughly 137 KB of native JSON. It is
not representative of large tool-heavy conversations or network latency. Cold
index measurements start with projected passages and exclude remote download
and parsing. Allocation totals are not peak RSS. Setup/corpus construction is
outside warm-query timings. Timings from three iterations have substantial host
noise; repeat with a quiet host before comparing optimizations.

Reference-host advisory targets for dependent PRs: under 750ms for this broad
20k-passage query, under 250ms for its selective scope, under 10ms for a rare
term, and under 2s for this 5k-passage cold index. They are investigation triggers,
not brittle CI wall-clock assertions. Deterministic regressions pin zero query
GETs, source verification, unchanged-body FTS avoidance, scoped grouping,
selected-only snippets and complete retained alternatives. S2 must measure growing
session lifetime amplification and worker fairness; S3 discovery accounting;
S4 CLI startup and pagination under writes; S5 capture/lock interference.

## Bounded worker evidence (S2)

Production refresh now uses `Store.Observe` and `Store.Run`; eager `LoadSession`
and `Replace` remain only as test fixture conveniences. One source is loaded at
once. Desired/indexed source identity is separate from searchable metadata, so
metadata-only refresh does not deserialize cached passage text. A changed source
can update exact references without replacing equal FTS text. Intermediate
revisions coalesce, while a short transaction rejects superseded generations.
Source projections are staged in the same private search database; full source
bundles are not cached. This adds a second local copy of projected message text
alongside FTS, bounded per current session membership and discarded on rebuild.
Legacy preserved sidecars may omit privacy/schema provenance that verified source
bytes supply. Their unchanged reader-bound requests reuse certified projections;
new unknown requests still invalidate old text and verify source provenance.

Reproduce deterministic work counts and the small lifetime workload with:

```sh
go test ./internal/search -run Worker -count=1
go test ./internal/search -run '^$' -bench Worker -benchmem -benchtime=3x -count=1
```

On the same non-isolated Intel Mac / Go 1.27.2, final three-iteration timings
were about 23.4 ms per true display-only metadata refresh, zero source GETs and
zero FTS writes. Ten growing synthetic publications indexed separately measured
about 778 ms total, ten GETs / parses, 3443 compressed bytes and eleven FTS inserts
(one title plus ten messages). Observing the same ten revisions before one worker
pass measured about 36.4 ms, one GET / parse, 373 compressed bytes and the same
eleven inserts. Full parsing processes 55 cumulative message records in the first
workload versus ten when coalesced. This demonstrates work reduction, not constant
lifetime parsing cost; changed snapshots still parse in full. The final metadata
path allocates about 38 KB/op; this workload does not grow its message payload.

Earlier runs observed roughly 1.5 ms metadata refresh, 28 ms separate lifetime
work and 8 ms coalesced work. This large host variability makes these timings
unsuitable for comparing small optimizations or setting an SLA. Counted work is
the reliable evidence; repeat timings on a quiet host. No peak-RSS or realistic
large-history disk-growth claim follows from these tiny fixtures.

Fixtures cover source staging and restart, stale-generation rejection, unchanged
preserved-source reuse, metadata-only work, equal-text reference updates, retry
backoff, finite oversized units, privacy invalidation, cancellation, busy writer
recovery and atomic failure of the final indexed-state transaction. Timing values
are advisory; operation counts are the deterministic contract. These tiny local
sources do not model real network delay, peak RSS, or large tool-heavy histories.

Composition must acquire `Store.LockWork(ctx)` before authoritative metadata
reads, then hold it through `Observe` and `Run`, releasing it with `defer` on
success, cancellation and failure. It is the existing nonblocking `local.NamedLock`
in the private physical index directory, named `search-worker.lock`. Busy
attempts return `ErrWorkBusy` before any archive read. There is no nested Run
lock, durable lease or worker framework. Cached queries and content-free hints
remain independent; callers must never acquire this lock while holding a
collector/publication lock. Future reconciliation/scheduling must use the same
boundary so observations cannot arrive out of order and overlapping processes
cannot each start source reads. Index purge must stop workers before removing
its directory/lock inode, like other retained archive state.

Reuse also binds all of reader's ownership checks: archive/native session,
project identity, parent session, exact active harness producer facts and capture
time. Preserved sources use their own capture time and harness-name check, as
reader does. These fields are verification provenance; changing them requires
verification even if bytes/checksum stay equal. Display-only updates such as
RepoKey/title/project labels can take the metadata fast path. Tests mutate each
identity fact and require the verified reader to reject mismatches rather than
accepting a cached projection. Initial work timestamps preserve FIFO fairness
against a continuing arrival of completed sessions; completion priority breaks
ties without indefinitely starving older active work.

## Bounded archive reconciliation

`search --refresh` runs one explicit namespace-wide discovery/indexing pass;
ordinary queries keep their current-project scope and perform no discovery or
remote reads. Named session refresh still works. Production composition holds
`Store.LockWork` across metadata reads, `Observe` and `Run`, and enables
`Budget.VerifyCurrent`. That option opens fresh authority after source loading,
including when the caller supplied an older catalog read view, before a
projection can install. Source parsing and projection preparation stay outside
SQLite write transactions.

Discovery has a 20-second child deadline within the command's 30-second pass.
Discovery errors are reported while already-known source work receives the
remaining time. Defaults process 32 physical identities/objects, up to four
recent catalog entries, eight exact known-identity checks, 512 nodes and 16 MiB
of discovery record bytes. Catalog bytes include head/coordinator reads and
cached node visits. Source indexing separately allows four sources, sixteen
session attempts and 32 MiB of referenced compressed bytes, with 512 catalog
nodes/16 MiB for fresh pre-install authority. Only one source is in flight.
Pass exhaustion remains retryable; it is distinct from an individually
oversized source. Source decompression limits are unchanged.

Catalog catch-up persists physical identity keys, including tombstones, with
root/mode/interpretation identity. It never persists snapshot-authenticated
cursors. A walk across changing roots is discovery without a complete-snapshot
claim. Exact fresh identity checks establish removals independently. Stable-root
walk coverage, old local identity validation, pending/failed indexing and
retained-source gaps are separate facts. Older schema1/schema2 identities seed
through bounded local keyset pages, including after restart; opening an upgraded
index never eagerly scans remote storage or hydrates local identities. The
ordinary one-time SQLite coverage-index migration does scan existing session
rows locally; its cost is proportional to row count. Normal query open and
execution share a five-second deadline, and refresh open plus work shares thirty
seconds. Initialization cancellation rolls back schema changes for safe retry.
Source provenance may be recovered
from verified preserved bytes when old sidecars omit it, never from active
revision metadata.

Legacy discovery counts all physical listed objects, including source-only
pages. It requires `PageLister`, restarts failed opaque continuations safely,
and never represents mutable listing pages as an immutable snapshot. Corruption,
timeout, stale snapshots and partial listing do not establish absence. Unreadable
metadata bodies do not pin logical discovery or validation checkpoints: failures
remain visible and suppress completion while later identities progress, and the
next sweep retries them. Parent cancellation, stale authority and structural/pass budget failures
stop without skipping an uncertain identity. A failed bounded metadata unit can
checkpoint its known logical key for retry in the next sweep. Network
changes after the last local observation remain unknown until another refresh.

Reproduce discovery costs separately from source work:

```sh
go test -v ./internal/search -run TestReconcileDefaultPassCatchupOperations
go test ./internal/search -run '^$' -bench BenchmarkReconcileColdAndWarm -benchmem -benchtime=3x
```

Default source count is deliberately conservative rather than a scheduling SLA.
Even small archives can need many bounded passes; a future one-minute worker tick
would translate each additional pass into scheduling lag. S4/S5 composition must
choose manual catch-up and scheduled budgets using measured costs. This change
adds neither concurrency nor a scheduler.

Fairness also covers bodies that exhaust a complete pass: schema4 stores one
finite discovery turn in the existing discovery row. Known checks, recent reads
and historical reads rotate first access to the unchanged512-node/16MiB/20-second
aggregate budget (at most three passes per class). The recent starting row rotates
across its four selected entries (at most twelve passes for a stable recent set).
Later work borrows the actual remainder. There is no smaller permanent per-body
size cap: a14MiB verified metadata fixture still hydrates with the default16MiB
pass. Oversized bodies, including streaming bodies with absent or false declared
length, remain reported failures and partial coverage while unrelated identities
advance. Legacy budget failures checkpoint within the current physical page using
the existing logical key; the next sweep retries skipped bodies.

Metadata-only reads reserve500ms inside the existing discovery deadline for
persisting a failed immutable key. A fresh pass therefore permits roughly19.5s
for a single metadata read, reduced by preceding work. Only child metadata timeout
while the parent remains live is an item failure; parent cancellation, stale
snapshots and structural errors still stop without skipping an uncertain key.
This is a work budget, not a service-time guarantee. Schema3-to4 adds the finite
turn without resetting durable progress; schema1/2 logical seeding stays bounded.

S3 bounded reads reject a known oversized response length before allocating its
body. Missing or misleading lengths still use the bounded actual read and retain
partial/error bytes solely for accounting; reader integrity/identity checks reject
errors before decoding/installing anything. Discovery reserves the getter's
one-byte overflow probe inside its total byte ceiling and charges returned error
bytes before classifying exhaustion. No HTTP length acts as content authority.

Post-repair three-iteration cold/warm discovery retained the operation counts
above: cold40metadata GETs/5nodes/212,634accounted bytes, warm0metadata GETs/
16nodes/253,894bytes. Advisory plain timings were54.88ms/8.41ms with2,491,981/
1,143,282allocated bytes per operation. The40-session default catch-up again
needed2discovery passes and10indexing passes with identical published operation
counts (plain advisory220.69ms). These fixtures do not model real remote latency.

Measured on Go1.27.2, a shared Intel i7-8559U Mac and a synthetic in-memory
40-session archive with zero simulated remote latency:

- Defaults discovered the archive in two passes and indexed it in ten. Discovery
  used four pages, forty identity entries/metadata GETs, sixty-four exact known
  checks,119head reads,10coordinator reads,137node visits and2,450,451accounted
  record bytes. Source work used40GETs/parses and12,772compressed bytes;
  fresh pre-install checks separately used40checks,80nodes and1,514,248bytes.
  The prior advisory non-race elapsed catch-up was614ms, excluding archive
  construction; the post-review race fixture measured4.25s and includes the
  final warm-view head checks. These timing modes are not comparable.
- The earlier three-iteration cold discovery with an explicit50-entry budget used40metadata
  GETs,5nodes and212,634bytes per operation, roughly107ms and2.50MB allocated.
  Post-review three-iteration warm default discovery used0metadata GETs,16node
  visits and253,894accounted bytes, roughly8.46ms and1.14MB allocated. Repeated cached node decoding counts
  toward work bytes, so these figures are not HTTP payload measurements.

These tiny fixtures establish operation counts and resumability, not archive-size
or latency guarantees. Forty sessions at four sources per pass already need ten
passes; a one-minute schedule would add roughly ten minutes before remote
latency, large sources, backoff or coalescing. Larger manual catch-up budgets
remain a composition decision, with the one-source-at-a-time constraint intact.

Local upgrade/startup measurements used lightweight session rows with no source
payloads, three iterations per case, on the same Go1.27.2 Mac. Schema2-to3 open
with20,000rows took6.20ms versus2.42ms for an already-migrated warm open;
100,000rows took26.64ms versus2.35ms warm. These are advisory local SQLite
measurements, not bounds on real transcript databases. A20,000-row race fixture
also canceled initialization with a one-millisecond caller deadline, verified
schema2 remained uncommitted and retried successfully with all rows retained;
identity seeding remained pending. Reproduce with
`go test ./internal/search -run '^$' -bench BenchmarkSearchSchemaMigrationAndWarmOpen -benchmem -benchtime=3x`.
