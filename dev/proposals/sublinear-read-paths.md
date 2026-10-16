# Sublinear read paths: implementation plan

Status: proposed 2026-10-15; nothing implemented. This plan takes the
remaining read-path work from [CLI performance](cli-performance.md) PRs 7 and 9
and [archive listing at scale](listing-at-scale.md) phase 4 down to concrete
structs and per-PR acceptance gates. It does not change any output, search
rule, retention rule or source verification. Where this plan and code differ,
code and the [catalog migration and query contract](../catalog/migration-and-query.md)
remain current behavior.

## Problem

No default read path is sublinear in the number of sessions N:

- Bounded `list` reads only L metadata bodies, but every run discovers and
  parses Theta(N) listing headers (~654 B of keys, ~4.7 KB allocation churn
  and ~20 allocations per session at any `--limit`), then sorts all matches
  before truncating (`selectListingRevisions`, internal/reader/list_index.go).
- The warm local SQLite catalog avoids body downloads, but its CLI caller
  pages through every candidate and limits last
  (internal/cli/session_catalog.go `catalogSessionsInContext`), and the reader
  unmarshals each scanned row's summary before filtering
  (`fillCatalogPage`, internal/reader/session_catalog.go).
- `show`/`handoff` with an exact ID are O(1) probes, but a short ID or topic
  words are Theta(N) cold (every body) and Theta(N) warm (every summary).
- Cold exhaustive reads are dominated by per-sidecar cache-file writes
  (80.5% cumulative CPU in `MetadataCache.put` at N=50k in the 2026-10-15
  synthetic profile), not by JSON decoding.

Synthetic baseline (M5 Pro, go1.27.2, `storagetest` in-memory store, minimal
~0.5 KB sidecars; lower bounds that exclude provider latency): bounded warm
`list --limit 50` 136 ms at N=50,000 in-process; exhaustive `--limit 0` cold
8.3 s; `show` short-ID 147.5 ms at N=800 versus 0.099 ms exact. Real S3/R2
listing latency at these sizes is unmeasured and is additive.

## What already exists (do not rebuild)

- `catalog.Snapshot.Query` over the identity/capture/activity/project trees
  already answers eligible catalog-v4 queries in O(log N + returned rows)
  with no canonical LIST; see the query table in
  [migration-and-query](../catalog/migration-and-query.md).
- `catalog.Snapshot.Delta` (internal/catalog/delta.go) plus
  `SQLiteSessionCatalog.RefreshRemote` (internal/reader/remote_catalog.go)
  already reconcile a verified prior-root delta into the local SQLite
  catalog. The "persisted delta catalog" therefore exists for catalog-v4
  destinations; the remaining work is bounding the queries that still fall
  back to complete summaries, not a second manifest format.
- Production catalog-v4 activation stays behind the
  [provider qualification gate](../catalog/provider-qualification.md). This
  plan does not change that gate, any protocol number, or the default
  legacy v3 format.

Consequence: for legacy v3 destinations, header discovery stays Theta(N)
under this plan (with a much smaller constant); only migration to catalog-v4
removes it. Building a parallel delta manifest for legacy destinations is
explicitly rejected below.

## Complexity targets

| Operation | Today | Target after this plan |
|---|---|---|
| `list --limit L` eligible filters, warm catalog | Theta(N) summary decodes + sort | O(log N + L) rows decoded |
| `list` model/skill/coverage filters | Theta(N) | Theta(N) retained (summary predicates), no body reads |
| `show` short-ID / prefix | Theta(N) bodies cold | O(log N + matches); at most 2 final candidates decoded |
| `show`/`handoff` topic words | Theta(N) | Bounded overscan pages until 2 final matches or exhaustion |
| Cold exhaustive list/show | Theta(N) bodies + Theta(N) cache prunes | Theta(N) bodies; zero per-put directory scans |
| Catalog-v4 text/prefix fallback | All rows loaded via `lazyCatalogRows` | Delta-synced local SQLite query; no full row materialization |

Eligibility is conservative: any predicate the bounded path cannot represent
exactly keeps today's complete path and result. Fallback is never an error.

## PR order

Each row is one PR, branched from its listed dependencies and measured alone.

| PR | Scope | Depends on |
|---|---|---|
| 1 | Bounded local catalog queries (SQL pushdown) | Main |
| 2 | Batched metadata-cache writes | Main |
| 3 | Bounded show/handoff candidate resolution | 1 |
| 4 | Generalized bounded catalog-v4 selection | 1 |
| 5 | Scale benchmarks and operation-count gates | 1–4 (benchmarks may land per PR) |

PRs 1 and 2 change no persisted archive format and need no entry in
[versions](../maintainers/versions.md): the SQLite catalog and metadata cache
are private, disposable local state. PR 4 must not allocate a new catalog
protocol or schema number; if the existing index key encodings cannot express
a range below, that range is dropped from PR 4 and needs its own proposal.

## PR 1: Bounded local catalog queries (SQL pushdown)

Goal: a warm eligible `list --limit L` decodes O(L) summaries, not N.

**Schema (private SQLite, internal/reader/session_catalog.go).** The
`sessions` table gains typed selection columns beside the existing
`summary` BLOB, populated from `SearchSummary` at insert time in
`Refresh` (currently `INSERT OR REPLACE INTO sessions VALUES(?,?,?,?,?,?,?,?,?,?)`):

```sql
harness TEXT NOT NULL, project_id TEXT NOT NULL, repo_key TEXT NOT NULL,
parent_id TEXT NOT NULL, native_child INTEGER NOT NULL, replay INTEGER NOT NULL
```

New indexes: `sessions_lowerid(lowerid)`,
`sessions_repo_activity(repo_key, activity DESC, capture DESC, key)`,
`sessions_project_activity(project_id, activity DESC, capture DESC, key)`,
`sessions_harness_activity(harness, activity DESC, capture DESC, key)`.
`catalogRecord` gains the same six fields, and `catalogRecordChecksum` must
cover them (the struct contract already requires every selection field in the
checksum). Layout migration uses `PRAGMA user_version`: on open, if the value
is not the expected layout version, drop `sessions`, `catalog_state` and
`remote_root` and recreate them incomplete. Dropping is safe because the
catalog is rebuilt from verified discovery and is never an authority.

**Query path.** `catalogWhere` stops using `json_extract` for harness,
top-level and replay predicates and uses the typed columns; capture bounds
already use columns. `CatalogQuery` gains one field:

```go
type CatalogQuery struct {
	Metadata MetadataQuery // Limit now honored for bounded queries
	Words    []string      // unchanged: safe superset via the search column
	IDPrefix string        // new: lower-cased session-ID prefix, "" = none
	Cursor   string
}
```

`IDPrefix` compiles to an indexed range on `lowerid`
(`lowerid >= ? AND lowerid < ?` with the prefix upper bound), never `LIKE`.
`Query` becomes two-phase for bounded queries: phase 1 selects
`key, etag, hash, summary_hash` plus typed columns (not the `summary` or
`search` blobs) in query order with the SQL predicates; phase 2 loads and
checksum-validates summaries only for candidate keys, then applies the
existing exact `matches` and the caller's final matcher. Model/skill/coverage
predicates (`catalogRequiresSummaryFilter`) and `IncludeRootChildren` keep
the current full-scan `fillCatalogPage` behavior in this PR.

**Caller (internal/cli/session_catalog.go).** `catalogSessionsInContext`
gains a bound and stops paging: it sets `query.Metadata.Limit`, accumulates
only final matches, and requests overscan pages of
`max(4*remaining, 64)` capped at 1024 until the limit is filled or the
candidate set is exhausted. A new `listBoundedEligible(opts, full) bool`
next to `readListCandidates` returns true only when every post-filter the CLI
would apply (scope, origin, top-level) is represented in the query; browsing,
explicit `--limit 0`, and any unrepresented predicate keep the complete path.
`sortByActivity` is not re-run on pages already returned in activity order.

Pass: warm eligible list output byte-identical to the complete path on the
existing oracle fixtures, including tie breaks; a recording-store test shows
summary decodes bounded by page size, not N; stale cursors still fail with
`ErrStaleCatalogCursor`.

## PR 2: Batched metadata-cache writes

Goal: cold exhaustive reads stop paying a directory scan per cached sidecar.

`MetadataCache.putVerified` (internal/reader/cache.go) currently calls
`pruneVersions(dir, current)` after every successful `writeCacheFile`, and
`pruneVersions` re-reads the key directory. Remove the call from the put
path. Move pruning into the existing bounded maintenance sweep
(`cacheMaintenanceDirectoryBudget = 64`, internal/reader/cache_maintenance.go):
when the sweep visits a key directory holding more than one validator file,
keep the most recently modified and remove the rest. Keeping an arbitrary
subset is correctness-safe because `get` opens exactly one ETag-named file
and any miss costs one refetch; modes (0600 file, 0700 directory) and symlink
checks are unchanged. No cache layout change, so old and new binaries can
share a cache directory in either order.

Pass: profile of cold exhaustive N=50k shows `pruneVersions` gone from the
put path; cache hit/miss behavior and deleted-session eviction tests pass
unchanged; interrupted sweeps leave only `.pending-` debris the existing
stale-temp cleanup already removes. Re-profile before considering a packed
single-file cache; that is out of scope here.

## PR 3: Bounded show/handoff candidate resolution

Goal: resolving one session never decodes the archive.

`resolveShowQuery` (internal/cli) classifies the query before reading
candidates: exact ID keeps today's per-harness probes; a short ID or ID
prefix uses PR 1's `IDPrefix` query; topic words use the existing `Words`
superset. Both bounded forms page with the overscan rule from PR 1 and stop
after **two** final matches, which is sufficient to preserve today's
ambiguity errors exactly. Cases the bounded query cannot represent
(configured project labels that only exist at query time, scope tiers,
unlabeled-name gaps in `catalogSearch`) detect that from the query shape and
use today's complete candidate path. The selected session's metadata is
still loaded and verified exactly as today (`selectedShowMetadata`,
`ReadCachedMetadata` rules in the catalog contract); summaries never become
body authority.

`handoff` reuses the same candidate API for its title-word and picker-search
paths; its bare picker already takes a bounded page and is unchanged. The
local-transcript label reads (at most 50 files) stay bounded and unchanged.

Pass: cold and warm operation-count tests (`BenchmarkCommandReads` cases and
new PR 5 cases) show short-ID resolution performing O(matches) GETs after
catalog warm, ambiguity and not-found output unchanged, and an ID prefix
never outranking a text match it does not also satisfy.

## PR 4: Generalized bounded catalog-v4 selection

Goal: on catalog-v4 destinations, remove the remaining complete-summary
fallbacks from `list`/`show` hot paths without any protocol change.

Today `selectCatalogMetadata` (internal/reader/remote_catalog.go) takes the
bounded tree path only for a single indexed predicate
(`boundedCatalogSelection`); everything else loads all rows through
`lazyCatalogRows` (pages of 1000 over `IdentityIndex`, fully materialized)
and re-derives listing revisions in memory. `CatalogSummaries` does the same
for complex CLI matchers.

Changes, all reader-side over existing wire types
(`catalog.CatalogHead`, `catalog.Query{Index, Lower, Upper, Reverse}`,
`catalog.Row`, `catalog.Delta`):

1. **ID-prefix ranges.** Expand `boundedCatalogSelection` to accept an
   ID-prefix query. Identity tree keys are canonical session keys, so a
   prefix on the session ID is one contiguous `IdentityIndex` range per
   harness: `Lower = sessions/<harness>/<prefix>`,
   `Upper =` the same with the prefix's exclusive upper bound.
   `selectBoundedCatalog` queries each harness range with the caller's limit
   and merges at most that many rows per harness; bodies load through the
   existing `readCatalogRow` verification.
2. **Fallback through the delta-synced local catalog.** Where a local
   `SQLiteSessionCatalog` is available, `CatalogSummaries` and the
   complete-summary fallback stop calling `catalogRows`/`lazyCatalogRows`:
   they call `RefreshRemote` (verified delta against the persisted
   `remote_root`, complete rebuild only when the prior root is unknown) and
   then PR 1's pushed-down `Query`. Text search stays a complete-summary
   operation semantically, but its cost becomes O(changed) remote work plus
   a local indexed scan, with no canonical LIST and no all-rows slice.
   `--no-cache` keeps today's in-memory complete fallback.
3. **No new authority.** Cursors stay bound to the snapshot root and refresh
   nonce exactly as the catalog contract requires; a changed head mid-query
   still returns the existing refresh error. Counter-only leaf changes keep
   invalidating local summary generations.

Pass: on a synthetic catalog-v4 destination (in-memory qualified store),
prefix and text queries perform zero canonical LISTs and fetch tree nodes
proportional to changed subtrees on the second run; results equal the
exhaustive oracle; the query table in
[migration-and-query](../catalog/migration-and-query.md) is updated in the
same PR. Provider qualification and activation remain HOLD and untouched.

## PR 5: Scale benchmarks and operation-count gates

Extend the existing harnesses rather than adding new ones
(`storagetest.SeedArchive`/`ScaleCases`, `BenchmarkReaderScale`,
`BenchmarkCommandScale`, `BenchmarkCommandReads`):

- `BenchmarkCatalogListScale`: text-full, model, skill and word queries at
  N=800/10k/50k, warm and 1%-changed, through the real CLI. Asserts via the
  measured store that eligible bounded queries perform 0 body GETs warm and
  decode at most the overscan page.
- `BenchmarkShowResolveScale`: exact, short-ID and topic resolution at the
  same scales, cold and warm catalog.
- Gate on operation counts, bytes and allocations (reproducible), report
  ms/op ungated; the 2026-10-15 runs showed wall time varies up to ~1.9x
  under contention while counts reproduce exactly. Default CI keeps N=800;
  the full matrix stays behind `AGENT_ARCHIVE_PERF=1` per the testing guide.

## Correctness and privacy constraints

- The exhaustive reader remains the selection oracle for every PR; each new
  bounded path ships with differential tests against it, including ordering
  ties, replay and native-child handling, and missing/corrupt rows.
- No predicate may move into SQL or a tree range unless it is exact there.
  Unicode case folding stays in Go (`catalogSearch` exists because SQLite
  `lower()` is ASCII-only); SQL and tree ranges only ever produce supersets.
- Summaries, catalogs and caches keep today's contents ceiling: no titles in
  object keys, no transcripts or source references in `SearchSummary`, local
  files stay 0600/0700. Nothing in this plan uploads or drops new content,
  so no filter/adapter/parser version bump or privacy changelog entry is
  required; if an implementation discovers it must derive a new persisted
  field, it stops and follows [versions](../maintainers/versions.md) first.
- Validation is not a performance knob: SHA-256, ETag and
  `ValidateMetadata` checks on selected bodies are unchanged in every PR.

## Rejected alternatives

- **A new delta manifest for legacy v3 destinations.** It would duplicate
  catalog-v4's head/tree/delta machinery under a second format with its own
  writer fencing. Legacy destinations get PRs 1–3; sublinear discovery is
  the reason to migrate them.
- **TTL-cached bucket listings.** Violates the cache-never-authority rule
  and can hide just-captured or just-deleted sessions.
- **Skipping body validation on warm paths.** Small warm-path share, and it
  is the integrity boundary for selected metadata.
- **More listing/read concurrency.** Ranges already run 16-way and body
  reads 8-way; more only risks provider throttling.
- **Streaming transcript decode and render-fit changes** (the Theta(S)
  findings for `show --transcript` and `handoff`, ~13xS peak RSS, hard cap
  at 128 MiB uncompressed) are real but orthogonal to N; they need their own
  proposal against the privacy filter contract.

## Documentation on landing

Each PR updates, in the same change: this proposal's status,
[the catalog query contract](../catalog/migration-and-query.md) (PR 4),
[the listing contract](../maintainers/list-performance.md), user docs only
if visible behavior changes (none planned), and CHANGELOG.md. When PRs 1–4
have landed, move this file to `implemented/` per the proposals index rules
and record the matched benchmark evidence with exact source SHAs.
