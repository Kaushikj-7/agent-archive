# CLI performance implementation plan

Status: historical design with partially integrated source. Historical measurements were recorded at
`3878e73f43a2c372a63773b6ba0b436057606d5f`; this repaired harness is
reconciled with main `52a9eb2c8ddb58cbcb539364c02e43b20d053714`.
Freeze the same repaired harness on both production baseline and optimization
candidate before comparing them. Historical raw samples do not establish
matched-harness or current-main performance acceptance.
The original baseline/design was prepared at `a21bc752` on 2026-10-07.
Default legacy v3 summary authority, exact coverage and selected-body budgets
remain the contract. Later source integration in #460 adds the opt-in catalog-v4
format; these baselines do not activate it or qualify production providers.
Cover all five areas from the performance review: concurrent
reads, listing and cache cost, query selection, bounded discovery, and stats
computation. Preserve output, search rules, retention, and source verification.
The original API designs below are historical proposals; the current contracts
and source status are linked here.

## Current reconciliation

Reconciled against main `4a2ca72a` (2026-10-12). Closed original PRs were
source-superseded rather than merged unchanged. Their historical measurements
do not establish current-main performance acceptance.

| Original work | Integrated replacement | Remaining work |
| --- | --- | --- |
| #390 compiler/verifier regressions | #449 and supplementary native scan #467 | Historical eight-check protection identities are not the current six-check configuration. |
| #376 command/scale baselines | #451 output validation/model and #458 plan status | Fresh matched repaired-harness measurements. |
| #379 selected reads, #381 discovery, #377 cache maintenance, #385 search/browser | #460 | Native/provider/operator, full-size and matched-performance qualification. |
| #382 concurrent show/lookup | #460, original latency benchmark #465, release note #468 | Matched output/performance qualification. |
| #384 transactional catalog and #391 migration/bounded reads | #460, preserved measured/year-window controls #464/#466 | Provider atomicity/clock, durable global writer barriers and cutover qualification; full-load/peak-memory acceptance. |
| #383 stats selection | Source-superseded by #460; original year-window benchmark preserved in #466 | Full-window and matched-performance qualification remain open. |
| #375 prepared stats, #380 ranking | #375 reconciled onto current main as an open draft; surviving ranking work has standalone draft #470 while original #380 remains open | Complete remaining unique-source/control accounting and integration, fresh independent review and current-head CI. Draft source and old-base mergeability do not establish readiness or matched performance. |

The [default listing contract](../maintainers/list-performance.md) now
distinguishes legacy header enumeration and private summary reuse from
[opt-in catalog tree reads](../catalog/migration-and-query.md). Production
S3/R2 construction and migration remain refused under the
[provider release gate](../catalog/provider-qualification.md). The recorded
120-second paired full-load timeouts and 180-second failure remain HOLD;
source integration and green hosted checks do not resolve those gates.

[#469](https://github.com/wangjohn/agent-archive/pull/469) additionally supplies
the opt-in ordinary-ready selecting authority under the distinct
`publication-composition-v12` fence. It does not complete the broader historical,
private, staged publication or legacy-provenance work in #352/#356/#406, qualify
providers, or supply a fragment consumer. The [fragment foundation](../specs/codex-fragments.md)
is a proposed schema only; decoder/privacy-context consumer, lifecycle/import
and the contribution/enrichment journal for complete repair and exact undo
remain dependent work.

The remaining sections preserve the original proposed implementation sequence.

The goal is to make common commands fast as the archive grows. First, reduce
serial network waits and repeated local work. Then, read only the metadata that
a query needs. Finally, add a remote catalog that can avoid a full object scan.
Keep these changes separate so each improvement can ship and be measured alone.

## PR order

Each row is one PR. Start each branch from its listed dependencies. Merge each
dependency before its child; reconcile the child with normal history-preserving
merges. Independent rows
can proceed separately. Do not put the full program on one branch.

| PR | Scope | Depends on |
| --- | --- | --- |
| 1 | Command and scale benchmarks | Main |
| 2 | Concurrent selected metadata reads | 1 |
| 3 | Concurrent show reads and lookup reuse | 2 |
| 4 | Parallel header discovery and fallback reuse | 2 |
| 5 | Bounded cache maintenance | 1 |
| 6 | Stats window selection and ID prefix lookup | 4 |
| 7 | Local search catalog and paged browser | 5, 6 |
| 8 | Transactional remote catalog publication | 4 |
| 9 | Catalog migration and bounded reader | 7, 8 |
| 10 | Prepared stats units and model lookup reuse | 1 |
| 11 | Stats allocation and project ranking | 10 |

Deliver PRs 2–7 before activating the new remote format. PRs 10–11 can proceed
without the reader changes. PR 9 is the only remote format cutover.

## PR 1 Command and scale benchmarks

This PR makes runtime cost visible and repeatable. It measures complete user
commands through injected `CLI.Run`, including config/store initialization and
cache work. OS process startup is excluded; synthetic waits are explicit. It does not change
command speed. Its results will show whether later PRs reduce work or only move
it to another step; provider latency must remain separate from local CPU cost.

Add command benchmarks under `internal/cli` and reader benchmarks under
`internal/reader`. Use `cli.Env`, existing `storagetest.MemoryStore`, and a
`MeasuredStore` wrapper. Record LIST/GET count, bytes, peak concurrency, time,
and allocations. Include cache opening and time to the first usable screen.

Define `ReadMetrics{Lists, Gets, Bytes, PeakReads}` and
`BenchCase{Sessions, Limit, CacheState, Delay}`. Test 800, 10k, and 50k sessions;
limits 1, 50, and all; cold, warm, and changed caches. Keep latency tests small.
Pass: repeatable fixtures, content-free results, and the current baseline.

## PR 2 Concurrent selected metadata reads

This PR reduces the wait for a list whose metadata is not cached. It reads
independent sessions at the same time instead of waiting for each GET to finish.
Cold and changed caches gain the most; a warm list still pays for header
discovery. Limit concurrency to protect the provider and preserve deterministic
results when reads fail.

Change `reader.ListRecent`. Add private
`readSelected(ctx, storage.VersionedGetter, []listingindex.Revision, ListOptions)`.
Use eight workers and result slots in selection order. Reuse existing cache,
ETag, SHA-256, and `Revision.ValidateMetadata` checks.

Define `selectedRead{Metadata archive.Metadata, Cached bool, Err error}`. Report
the first error in selection order. Stop dispatch after failure; join all workers.
Invoke observers safely. Pass: 50 cold GETs overlap; warm GET count is zero;
ordering, validator races, and cancellation match the current contract.

## PR 3 Concurrent show reads and lookup reuse

This PR makes session summaries faster, especially for parents with many
subagents. It checks children concurrently and reuses the parent metadata found
during lookup. Runtime should depend on batches of requests rather than one
network wait per child. Missing children and ambiguous IDs must produce the
same results as today.

Change `ResolveLinkedSessions` to use eight workers and retain link order.
Preserve each existing `LinkedState`. Parallelize known-harness probes in
`MetadataFinder`; retain ambiguity detection.

Add `MetadataLookup{Key string, Metadata archive.Metadata}` and
`MetadataFinder.FindMetadata`. Keep `FindMetadataKeys` as a compatibility wrapper.
Carry the lookup through `showLookup`; do not fetch the selected parent again.
Pass: one parent read; bounded child reads; unchanged summary and failure states.

## PR 4 Parallel header discovery and fallback reuse

This PR reduces the network delay before a list can select its rows. It overlaps
independent listings and reuses work when the index cannot answer the query.
Both cold and warm commands benefit. Discovery still grows with archive size;
parallel ranges can also increase request count and transferred bytes. Measure
that cost before increasing concurrency.

Add private `HeaderSnapshot{Canonical []storage.Object, Revisions
map[RevisionID]listingindex.Revision}` and `RevisionID{Key, ETag string}`.
Add `discoverHeaders(ctx, store, prefix, filter, cache)`.

Use existing `RangeLister`, `listObjects`, and range limits for canonical keys.
Overlap canonical, v2, and v3 listing. Narrow v3 by harness. Reuse canonical
headers in exhaustive fallback; do not LIST them again. Keep legacy coverage
checks. Pass: output equals the exhaustive reader; gaps cancel safely; measure
request and byte increases as well as latency reductions.

## PR 5 Bounded cache maintenance

This PR removes archive-wide housekeeping from cache startup. Each command
does a small amount of cleanup instead of visiting every session directory
before reading. Large warm caches gain the most. Freshness checks and deletion
eviction remain required, so this PR bounds maintenance cost rather than all
cache work.

Keep the current metadata file layout. Add `CacheMaintenance{Cursor string,
LastSweep time.Time}` and private `maintain(ctx, maxDirs)`.
Open the cache without visiting every session directory. Sweep at most 64
directories per command. Save maintenance state atomically; treat errors as misses.

Evict absent keys after fresh discovery. Remove old validator files on a changed
key or during maintenance. Keep private modes and symlink checks. Pass: cache
opening does not scale with entry count; interrupted cleanup is safe; a deleted
session cannot reappear through the cache. Measure eviction separately.

## PR 6 Stats window selection and ID prefix lookup

This PR reduces metadata downloads for narrow stats windows and short-ID
lookups. It selects candidates from fresh headers before reading bodies and
removes duplicate discovery passes. The gain is largest when most sessions
are outside the query. Header scans remain; comparison periods, child
accounting, and text matches must remain complete.

Add `reader.MetadataQuery{Filter Filter, Limit int, Order QueryOrder,
TopLevelOnly bool}` and `SelectMetadata(ctx, store, query, opts)`.
Keep `ListRecent` as a wrapper. Permit unlimited indexed selection for supported
filters. Select From/To before body reads. Keep unsupported-filter fallback.

Use this API in stats for the current and comparison periods. Preserve existing
capture-time and parent/child rules. Add `FindMetadataPrefix` over fresh canonical
key headers; fetch candidate bodies only. Skip exact-ID probes for short IDs.
Keep title, PR-number, and scope search rules. Combine ID candidates with text
matches; an ID prefix must not take precedence over other matches.
Pass: a future window reads zero bodies; stats equals the exhaustive oracle;
short-ID search performs one discovery pass and preserves ambiguous results.

## PR 7 Local search catalog and paged browser

This PR makes repeated search and browsing use a local summary index. It loads
bodies for selected rows instead of decoding the full archive for each view.
Warm commands gain the most; the first catalog build still reads all required
metadata. Remote freshness checks remain until PR 9. Search completeness,
private data handling, and concurrent database access are the main constraints.

Add `reader.SessionCatalog` with `Refresh(ctx, HeaderSnapshot)` and
`Query(ctx, CatalogQuery) (CatalogPage, error)`. Use existing `modernc.org/sqlite`
through `database/sql`. Store private metadata summaries only. Keep metadata
bodies in the existing cache; store no transcripts.

Define `CatalogQuery{Metadata MetadataQuery; Words []string; Cursor string}`,
`CatalogRow{Key, ETag, Hash string; Summary SearchSummary}`, and
`CatalogPage{Rows []CatalogRow; Next string; Total int; Complete bool}`.
`SearchSummary` holds name, title, branch, PR numbers, and the typed metadata
fields used for identity, scope, activity, origin, model, skill, and coverage
filters. Apply configured project labels at query time. Reconcile changed and
deleted rows in one transaction.

Use SQL to select a safe candidate superset. Apply existing `sessionQuery` rules
for the final match. Page list and bare-show browsers; support global search and
scope changes. Bind cursors to the catalog generation and query. Pass: searches
equal the exhaustive oracle; stale cursors fail; cold rebuild is cancellable;
warm browsing does not decode every body. Native names use published metadata.

## PR 8 Transactional remote catalog publication

This PR builds the writer protocol needed to remove full object scans from
reads. A single catalog commit makes each publication or deletion visible as
one consistent change. It does not speed up default legacy reads yet. Writers
gain storage work and can contend on the shared head; provider support, retry
safety, and crash recovery must be proven before migration.

Add `ArchiveFormat` to destination configuration; default to legacy.
Add `internal/catalog` for an opt-in v4 archive format. Make one versioned head
the commit point. Store metadata and tree nodes at immutable revision keys.
Use copy-on-write trees for identity, capture time, activity, and project order.

Define `ObjectRef{Key, SHA256 string}`, `CatalogHead{Schema, Generation uint64;
Epoch string; Identity, Capture, Activity, Project ObjectRef}`, and
`CatalogMutation{ID, SessionKey, ExpectedRevision string; Next *CatalogEntry}`.
`Next == nil` deletes an entry. `CatalogEntry` holds a metadata reference and
the query summary. Bound node size; keep pointers in internal nodes and entries
in leaves. Store no titles in object keys.

Add optional `storage.ConditionalPutter.PutConditional(ctx, key, data,
PutCondition) (etag string, err error)`. `PutCondition` has `MatchETag` or
`CreateOnly`. Use SDK `s3.PutObjectInput.IfMatch` and `IfNoneMatch`. Normalize
conflicts to `ErrPreconditionFailed`. Prove atomic behavior on S3 and R2 before
enabling the format; refuse unsupported providers.

Add `catalog.Commit(ctx, mutation)`. Upload immutable objects, then replace the
head conditionally. Rebase conflicts for other sessions. Reject a changed
ExpectedRevision for the same session. Give every revision a unique identity
to prevent ABA retries. Persist frozen mutation IDs in `PendingPublication`.
Resolve lost responses before retry; do not overwrite a newer publication.

Connect collector publication and `DeleteWholeSession` to this commit path.
Commit deletion before source removal. Limit snapshot lifetime to ten minutes;
retain old roots and their sources for that period. Garbage collection must also
retain preserved history references. Pass: concurrent commits lose no entries;
crash, conflict, lost-response, deletion, and history-fence tests pass.

## PR 9 Catalog migration and bounded reader

This PR activates the new catalog after a controlled migration. Supported
queries read a fresh head and the relevant tree paths, so discovery cost grows
mainly with the result set rather than the full archive. Initial migration and
local catalog rebuilds still require full work. Writer coordination, rollback,
retention, and cursor refresh are release conditions.

Add an explicit `agent-archive migrate --format catalog-v4` operation. Create a
new destination prefix, or a separate bucket when permissions cannot isolate
prefixes. Require all writers to support v4; revoke old write credentials and
keep the old destination read-only. Never assert v4 completeness for a mixed
legacy destination. Verify the writer cutover before activation.

Define `CatalogMigration{Source, Destination, Phase, Cursor, ExpectedHead}`.
Copy and verify live source references and metadata. Build trees in bounded
batches. Resume from durable checkpoints. Compare the catalog with an exhaustive
source inventory after writers stop, then activate. Preserve retention dates.

Add `catalog.OpenSnapshot` and `Snapshot.Query(query, cursor, limit)`.
Read one fresh head per request; fetch selected tree paths and bodies. Cache
immutable nodes by hash. Bind cursors to the root; refresh the view if the head
changes or the cursor expires. Feed changed leaves into `SessionCatalog`;
rebuild when its prior root is unavailable. Use these APIs for list, show, stats,
search, and browser discovery. Retain the legacy reader for old destinations.
Pass: warm supported queries use O(log N + results) tree work; no canonical
LIST; migration, rollback, retention, and concurrent-reader tests pass.

## PR 10 Prepared stats units and model lookup reuse

This PR reduces CPU work in stats and when the user changes the time window.
It prepares session relationships and model costs once, then reuses them for
each view. The gain is largest for large archives and repeated window changes;
remote read time is unchanged. Rebuild prepared data when prices or the
timezone change, and keep it immutable between views.

Add `stats.Prepare([]archive.Metadata, PrepareOptions) *Prepared` and
`(*Prepared).Compute(Options) Stats`. `PrepareOptions` contains location and
prices; `Prepared` holds immutable units. Keep `stats.Compute` as a wrapper.

Use per-prepare maps from raw model ID to normalized ID and price. Build root
relationships and member totals once. Replace `statsInputs.sessions` with the
prepared data. Window changes reuse it; price or timezone changes rebuild it.
Pass: outputs are identical; repeated window changes perform no normalization
or root resolution; the 50k benchmark shows lower time and allocations.

## PR 11 Stats allocation and project ranking

This PR reduces temporary memory use and the cost of choosing top projects.
It should reduce allocation and garbage collection pressure at high session
and project counts. Bounded ranking helps top-N output; views that require all
rows still need a full sort. Exact ties, sum order, and unknown values must
remain stable.

Reduce temporary maps and slices in `memberUsage` and `unit.finish`. Reuse
per-call scratch storage; do not use global mutable pools. Add private
`projectHeap` for bounded top-N selection. Use the existing comparator and full
sort when `AllRows` is true. Keep sum order and unknown-value rules unchanged.
Pass: top-N equals the full-sort prefix, including ties; all stats fixtures
match; the high-project-cardinality benchmark allocates less memory.

## Shared release checks

Run focused tests, `go test -race` for concurrent packages, then repository CI.
Use exhaustive-reader results as the selection oracle. Test missing objects,
changed ETags, invalid schemas, partial writes, and interrupted commands.
Recheck the Codex history lifecycle fence on publication and deletion changes.

For PRs 2–7, measure real cold list-50, large-parent show, short-ID show, narrow
stats, and terminal startup. Initial targets are cold list-50 below 3 seconds
and the reviewed parent below 1 second on the same archive. Treat these as
acceptance targets, not provider guarantees. For PRs 8–9, gate release on live
provider semantics and migration tests. Update this plan as each PR lands.

This plan implements and extends the remaining work in
[Archive listing at scale](listing-at-scale.md). Native session naming remains
a separate proposal; refresh catalog rows when published metadata changes.
