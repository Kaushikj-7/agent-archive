# Read paths at realistic scale: implementation plan

Status: proposed 2026-10-15, revised 2026-10-16 after real-command
measurements and design review. PR 6 measurement infrastructure is implemented;
the optimization and provider-activation work remains pending. This plan takes the
remaining read-path work from [CLI performance](cli-performance.md) PRs 7 and 9 and
[archive listing at scale](listing-at-scale.md) phase 4 down to per-PR scope
and acceptance gates. It does not change any output, search rule, retention
rule or source verification. Where this plan and code differ, code and the
[catalog migration and query contract](../catalog/migration-and-query.md)
remain current behavior.

## Problem

`list`, `show <short-id>` and `handoff` are bounded by remote round trips, not
local CPU, and on the default legacy v3 format every run lists every key under
`sessions/`. That listing grows with the archive, and archives grow quickly
because subagents are sessions too.

### Measured on a real archive (2026-10-16)

Installed binary `dev-6c1d2437da58`, `AGENT_ARCHIVE_TRACE=1`, one R2
destination in legacy v3 format, warm local caches, M-series laptop on home
broadband. The archive holds 1,819 sessions (sidecars) and 5,152 keys under
`sessions/`, which is 2.8 keys per session. Interactive runs used a PTY with a
clean environment. Each figure is the range over two or three runs.

| Command | Wall time | Remote | Local |
|---|---|---|---|
| `list --limit 50`, piped (`ListRecent`) | 1.06–1.64 s | `sessions/` LIST, 10 ranges: 0.9–1.07 s. `listing/v3/` LIST, unranged, second page ends at about 1.3 s | 0.15 s user CPU |
| `show <full-id>` | 0.37 s | 4 parallel harness GETs (0.21 s), then 1 sequential GET (0.15 s) | <10 ms |
| `show <8-hex short id>`, warm | 1.23 s | `sessions/` LIST 0.9 s, then 2 sequential GETs 0.24 s | catalog refresh 10 ms, query 27 ms (1,818 summaries) |
| `show <short id>`, 52 sidecars changed since last run | 2.34 s | LIST 0.94 s, then 52 GETs at 8-way 1.16 s, then 1 GET 0.11 s | query 25 ms |
| `handoff` picker, time to first draw | 1.33–2.18 s | `sessions/` LIST 0.77–1.26 s | registrations 42–703 ms (1,721 files, serial, before the LIST). Cached sidecar bodies 82 ms. Headers 94 ms |

Observations the original draft did not have:

1. **Local CPU is about 10% of wall time.** The SQLite query over every
   summary is 25 ms here. The bounded-decode work in the original draft saves
   at most that on the measured archive.
2. **The listing is unbalanced.** Ranges are planned at one boundary per 200
   cached sidecars (`rangeSidecars`, internal/reader/list_ranges.go). That
   makes 10 ranges here, not the 16 the concurrency allows, and because key
   density per session varies, the longest single range request (0.9–1.25 s)
   is the whole LIST cost.
3. **`listing/v2/` and `listing/v3/` are listed unranged.** That is
   `store.List` in `listHeaderGroups`, internal/reader/header_snapshot.go. The
   v3 prefix holds roughly one key per live sidecar revision, so the piped and
   agent-facing `list` pays sequential 1,000-key pages that grow with N.
4. **Local work runs before or after the network instead of beside it.** The
   handoff picker reads every registration file before it starts the LIST.
   `show` waits for the LIST before its selected-body GET, and does child GETs
   only after that.
5. **Handoff never uses the SQLite catalog.** `loadSessionsForBrowseContext`
   excludes it (internal/cli/list_browse.go), so the picker decodes every
   cached sidecar body. `archiveByID` reopens the store after a pick
   (internal/cli/handoff.go), which repeats the Keychain read and TLS setup.
6. **Changed sidecars are a normal warm-run cost.** Active sessions rewrite
   their sidecars, so tens of GETs per run are common, at 8-way concurrency
   (`listConcurrency`, internal/reader/reader.go).

### Realistic scale

The measured archive gained 1,161 sessions in October 1–16, about 70 a day,
most of them subagents. With Keep forever or sufficiently long retention, that
rate reaches about 10k sessions in five months and 25k in a year. 50k is two
years, or one year with a second active machine. With the default 90-day
retention and successful cleanup, mostly short-lived sessions instead approach
about 6,300 per machine; renewed activity and sessions whose cleanup cannot
complete can increase that inventory. See [retention configuration](../../docs/reference/configuration.md).
Use 10k, 25k and 50k as explicit long-retention/multi-machine scenarios, and
100k as the stress ceiling. At the observed 2.8 keys per session these are
28k, 70k, 140k and 280k keys under `sessions/`; that ratio also needs checking
under sustained publication and retention.

The original projections below are estimates, not measured command results.
They assume 0.4–1.0 s per 1,000-key LIST page and linear local costs. The
balanced column is an idealized single-prefix estimate: it excludes discarded
page suffixes, range-boundary overread, competition among all three prefixes,
connection setup, retries and provider limits. Its small-archive estimate
also assumes smaller responses are faster, which requires measurement.

| Sessions | Idealized `sessions/` LIST (balanced 16-way) | Today's planner | Unranged `listing/v3/` | Handoff local (bodies + registrations) |
|---|---|---|---|---|
| 1.8k | 0.15–0.35 s | 0.8–1.3 s (measured) | about 1.0 s (measured) | 0.12–0.8 s |
| 10k | 0.8–2 s | 2–3 s | 4–10 s | 0.7–4 s |
| 25k | 2–5 s | 5–7 s | 10–25 s | 1.7–10 s |
| 50k | 4–9 s | 10–14 s | 20–50 s | 3.5–20 s |

The conclusion shapes the PR order:

- Constant-factor work on the legacy format (balanced and ranged listing,
  overlap, the SQLite catalog for handoff, fewer sequential GETs) should
  materially reduce latency. About 0.4–0.6 s at the measured size is a
  hypothesis for matched trials, not an established outcome.
- Legacy discovery still proves freshness by listing every key. Constant-factor
  work alone does not establish the desired latency at 25k–50k.
- Removing full remote enumeration needs a change token: catalog-v4's verified
  head and delta, already in source behind the
  [provider qualification gate](../catalog/provider-qualification.md).
  Qualifying R2 is the critical path for scale. It is operator work, and this
  plan does not change the gate.
- Once the LIST is gone, profile local refresh, query and registration work
  separately. Existing `RefreshRemote` still inventories cached bodies and
  checksums every local summary, even with an unchanged root. Query pushdown
  alone cannot make end-to-end warm work sublinear. The rough summary-query
  extrapolation is 0.7 s at 50k; it is not a refresh measurement.

## Targets

Warm runs, single R2 destination. These are aspirational wall-time goals
informed by the traces, pending matched measurements at each size. They are
not guarantees derived from request counts. CI gates use operation counts
(see PR 6); record whether each goal was met, missed or not measured.

| Command | Sessions | Legacy v3 after Track A | Catalog-v4 after Track B |
|---|---|---|---|
| `handoff` picker first draw, interactive `list`, bare `show` | 1.8k / 10k / 50k | ≤ 0.5 s / ≤ 1.5 s / not met | ≤ 0.5 s at all three |
| `show <short id>` | same | ≤ 0.6 s / ≤ 1.5 s / not met | ≤ 0.5 s |
| piped `list --limit 50` | same | ≤ 0.6 s / ≤ 1.5 s / not met | ≤ 0.5 s |
| changed-body hydration for K sidecars | any | ⌈K/C⌉ GET waves at admitted concurrency C | same, when bodies are required |

"Not met" means Track A does not claim the large-scale goal. Track B's flat
target additionally requires acceptable local refresh, query and registration
costs. For 52 small changed bodies, raising C from 8 to 16 reduces seven waves
to four; it does not halve total command latency. Large-body byte budgets can
reduce admitted concurrency.

## What already exists (do not rebuild)

- **Range listing.** `listObjects`/`planRanges`/`listRanges` and the
  `storage.RangeLister` `StartAfter` ranges (internal/reader/list_ranges.go,
  internal/storage/s3.go). PR 1 changes planning and boundary termination,
  reusing the existing mechanism.
- **ID-prefix lookup.** `FindMetadataPrefix` (internal/reader/metadata_prefix.go)
  reads bodies only for ID candidates when no additional text matcher is
  supplied; the current show fallback supplies a matcher that admits all rows.
  The catalog's `Words` predicate
  already includes `instr(lowerid,?) = 1`
  (`catalogQueryPredicates`, internal/reader/session_catalog.go).
  `readShowCandidates` passes `nil` words today
  (internal/cli/session_catalog.go). Before wiring words through, repair native-ID
  coverage in the candidate projection. `instr` and its surrounding text/label
  alternatives do not become indexed prefix lookups by adding a B-tree.
- **SQL limits.** `SQLiteSessionCatalog.Query` already pushes `LIMIT/OFFSET`
  into SQL when no summary-only predicate is present. The CLI caller never sets
  `Metadata.Limit`.
- **Stale cache versions.** The maintenance sweep already keeps only the
  newest validator per key directory (`maintainDirectory`,
  internal/reader/cache_maintenance.go).
- **Change tokens.** `catalog.Snapshot.Query`/`Delta` and
  `SQLiteSessionCatalog.RefreshRemote` (internal/catalog/delta.go,
  internal/reader/remote_catalog.go) are the persisted delta catalog for
  catalog-v4 destinations. No second manifest format is needed.
- **Traces.** `AGENT_ARCHIVE_TRACE` spans per request (internal/storage/http_trace.go).
  Every measurement above came from it, and each PR's evidence should too.
- **Pagination model.** `historicalListingModel`
  (internal/reader/listing_model_bench_test.go) already models range-end overread
  and transferred page suffixes. Extend it alongside `MeasuredStore` and the
  existing command benchmarks; preserve its historical controls.

## PR order

PR numbers are stable scope identifiers, not landing order. Land PR 6 first,
then prioritize PR 1, PR 2 and PR 3 using matched results. PR 4 is a measured
follow-up. Run provider qualification in parallel; reader work need not wait
for production activation. Measure each change against its base with the trace
recipe below and PR 6's model.

| PR | Track | Scope | Depends on |
|---|---|---|---|
| 1 | A | Provider-page-correct balanced listing across all three header prefixes | 6 |
| 2 | A | Overlap local work with discovery; drop repeated opens | 6 |
| 3 | A | Handoff on SQLite; complete candidate predicates before query wiring | 6 |
| 4 | A | Small cache fixes and memory-bounded GET concurrency trial | 6; measured need |
| 5a | B | Provider evidence, reviewed capability enablement, then writer cutover as separate deliverables | Existing provider and migration gates |
| 5b | B | Catalog reader reuse; explicit local refresh accounting | 6; production use requires 5a |
| 6 | A/B | Latency-modeled command benchmarks and operation-count gates | Main; lands first |
| 7 | B | Measured local refresh/query improvements | 5b; measured need |

PRs 1–4, 6 and 7 change no persisted archive format and need no entry in
[versions](../maintainers/versions.md). The SQLite catalog, metadata cache and
any range-plan file are private, disposable local state. PR 5 changes no
protocol or schema number. If the existing catalog-v4 encodings cannot express
something below, that part is dropped and needs its own proposal.

## PR 1: Balanced listing across all three header prefixes

Goal: reduce the measured critical path while accounting for actual provider
pages, bytes and the shared concurrency budget.

- Plan ranges by expected key count, not sidecar count. Let `planRanges` take
  boundaries from a range plan saved after the previous complete listing of
  that prefix. Start with up to 16 balanced ranges; above 16k keys, trial
  approximately page-sized ranges. Choose fewer ranges for small prefixes
  when extra requests and discarded page suffixes outweigh overlap. Record
  the winning policy in PR 6's model rather than requiring 16 ranges per prefix.
  Store hints in the metadata cache directory (0600, atomic replace, at most
  `15 + ⌈keys/1,000⌉` boundaries per prefix).
  - Boundaries only split work. Ranges still cover the whole key space, so a
    stale or missing plan can make a listing slower but never incomplete. The
    plan is a hint, never authority.
  - With no plan, use available sidecar-based boundaries or the complete
    sequential listing. `--no-cache` has no cached boundaries. Trial static
    splits only if measurements justify them; all ranges must cover unknown
    harnesses and non-hex IDs as well as built-in identities.
- Fix `S3Store.ListRange` to stop after retaining `key == through`. Today
  it waits for `key > through`, so an exact 1,000-key range can read a second
  page. Keep the `>` exit for stale boundaries no longer present. Retain
  full-page transfer accounting even when only a short range is returned.
- Run `listing/v2/` and `listing/v3/` discovery through the same `listObjects`
  ranging with their own plans. All three prefixes share one 16-slot limit, so
  total concurrency does not grow.
- Keep `rangeConcurrency = 16`. Raising it is a separate, measured change
  against R2 throttling and Class A operation cost; it is out of scope here.

Pass:
- Listings are byte-identical, including order, to the unranged `List` for
  random plans, stale plans, empty plans, keys inserted at range edges, and
  ranges that become empty.
- Test exact page-edge boundaries, absent boundaries and the final range with
  S3-compatible pagination. Count requests and transferred keys, not only
  returned keys. The existing 1,000-key policy can otherwise take roughly
  twice the ideal wave count above 16k keys.
- PR 6 reports total pages, discarded bytes and critical-path waves across
  all three prefixes sharing 16 slots. Compare both balanced and skewed cases.
- Matched traces show a command-level improvement and report whether the
  reference goals (longest range below 0.4 s, piped list below 0.6 s) were met.

## PR 2: Overlap local work with discovery; drop repeated opens

The source change is implemented, with deterministic synthetic controls for
discovery/scope overlap, cancellation and joins, registration worker bounds and
error precedence, and one storage open after a handoff pick. Independent review
also covers the no-catalog fallback and native activity eligibility. Scoped
activity stays after archive discovery when published scope facts determine
admission; only all-project activity can safely move earlier. Local-first
handoff query tiers retain zero archive requests when they resolve locally.
Existing verified selected-body reuse and parent/child dependencies remain in
place. Real command traces and wall-time goals remain unmeasured.

Goal: wall time is max(network, local), not their sum.

- **Start header discovery first.** It should begin as soon as the store is
  open, concurrently with:
  - `LoadRegistrations`
  - `collector.LastActivities`
  - local label reads
  - `scopeFor`'s git call
  - SQLite open and maintenance
  - the second `config.Load`

  Join before selection. Errors keep today's precedence and messages.
- **Load registrations concurrently.** Use a bounded worker pool (8) instead of
  one serial loop (internal/state/store.go). The picker needs only identity,
  harness and transcript fields. A registration index is out of scope until
  this measures as still significant.
- **Reuse the open store.** `handoffResolver.archiveByID` takes the store the
  picker or query path already opened, so a pick costs no second Keychain read
  or TLS handshake.
- **Reuse `show`'s selected body.** Reuse the body `Refresh` verified in this
  run when available. Otherwise fetch the parent before scheduling linked-child
  reads: `SearchSummary` omits `LinkedSessions`, so those identities are not
  yet known. Start child reads promptly once the verified parent is available;
  do not add link projections solely to force a single-wave gate.

Pass:
- The handoff, interactive list and short-ID show traces show local spans
  overlapping the LIST span.
- `archiveByID` after a pick performs zero store opens (counting `Env`).
- A parent already verified during refresh needs no duplicate GET. A cold or
  stale selected body is fetched before its children, with no extra serial work
  between those dependency waves.
- Output and errors are unchanged on the existing command tests.

## PR 3: Handoff on the SQLite catalog; show and handoff candidate wiring

Goal: handoff uses summary projections on an unchanged warm run, and query
predicates reduce candidate decoding without dropping valid matches. Changed
rows may still require body hydration; unique final matches do not by themselves
prove a page-sized candidate bound.

- **Handoff on the catalog.**
  - Add the fields handoff's source and replay policy reads to `SearchSummary`
    (private local state; populated in `summarize`; covered by
    `catalogRecordChecksum`).
  - Bump the catalog's private layout with `PRAGMA user_version`. On mismatch,
    drop and rebuild incomplete. The catalog is rebuilt from verified
    discovery and is never authority.
  - Remove the `catalogBrowseCommand` exclusion. The picked session's body is
    still loaded and verified exactly as today before any handoff output.
- **Repair candidate coverage before passing queries through.** Extend the
  `CatalogQuery.Words` superset to cover eligible distinct `NativeSessionID`
  values as well as archive IDs. Today `catalogSearch` omits native IDs, and
  `lowerid` contains only the archive ID; a labeled row matching only its native
  ID can be lost. Match `sessionFields.ids` eligibility, including exclusion of
  colon-containing subagent IDs. Cover the new private selection data with
  `catalogRecordChecksum` and the layout rebuild.
  - Then pass the user's parsed words from `readShowCandidates` and the handoff
    query path. Go's `sessionQuery` keeps final matching, tiers and ambiguity.
    Preserve query-time project labels: unlabeled rows remain candidates unless
    an equally complete label-aware predicate replaces that fallback.
  - A second final match proves ambiguity but is not the output. `show` and
    `handoff` print the complete match count and ordered candidate rows, and
    interactive pickers offer every match. After a second match, continue the
    candidate query to exhaustion or use the complete path. Never report a
    two-row prefix as the full result. A bounded or count-only ambiguity view
    needs its own output contract.
  - Do not add `sessions_lowerid(lowerid)` merely for `instr(lowerid,?) = 1`.
    Its OR branches over substring search and unlabeled rows still require a
    scan. Add an index only with `EXPLAIN QUERY PLAN` and measured evidence of
    a useful query, such as an explicit lower/upper ID range.
  - Handoff may use an indexed exact/short-ID stage only if it preserves
    `exactIDWins`, including archive-ID precedence over native IDs. Show uses
    `searchShowSessions`, which combines ID and text matches before scope tiers;
    it cannot substitute an ID-only lookup. Keep the complete candidate path
    until an optimization covers all those alternatives.
  - Harness filtering uses a `key` range on the existing primary key, not
    `json_extract`.
- **Bound bounded lists.** `catalogSessionsInContext` sets
  `Metadata.Limit` for non-browsing bounded lists whose predicates are all
  exact in SQL, including scope, origin and child/replay rules applied by the
  caller. Carry repo/project scope into the query if needed; otherwise retain
  the complete path. It does not loop pages it will discard. Existing exact
  `Total` counts can still scan a full matching range; limiting decoded rows
  is not a promise of sublinear total query work.

Pass:
- Results are identical to the exhaustive oracle, including ties, replays,
  native children, distinct native IDs, archive/native ID collisions, unlabeled
  configured names, PR-number words and scoped results outside an initial page.
- A recording test shows handoff's picker decoding zero metadata bodies on an
  unchanged warm run. Record summaries scanned and decoded separately, including
  many unlabeled rows with one final short-ID match. Do not impose a one-page
  bound where proving uniqueness requires exhaustive candidate matching.
- Ambiguity with three or more matches preserves the exact count, the ordered
  candidate rows and every interactive choice.
- Stale cursors still return `ErrStaleCatalogCursor`.

## PR 4: Small cache and selected-read fixes

- `MetadataCache.putVerified` skips `pruneVersions` when its `os.Mkdir` just
  created the key directory. A fresh directory has nothing to prune. Rewritten
  keys keep today's prune, and the sweep remains the backstop. No layout change.
- Trial 16-way GETs for small changed sidecars against the current 8-way limit.
  `listConcurrency` also controls catalog hydration, whose immutable metadata
  bodies may reach 32 MiB: 16 concurrent bodies can mean 512 MiB of raw input
  before decoding. Keep large-body reads on a conservative separate limit or
  apply a byte budget, reusing the existing selected-read scheduler. Do not
  raise the shared constant solely to match LIST concurrency.
- Gate the trial on PR 6's model, synthetic mixed/maximum-size bodies, peak
  memory measurements and repeated authorized traces with ≥ 50 changed small
  sidecars. Keep 8 if gains are negligible or retries/throttling increase.

Pass:
- A cold N=50k synthetic profile shows `pruneVersions` gone from fresh puts.
- Cache hit, miss and eviction tests are unchanged.
- The model records seven-to-four waves for 52 small bodies at 8 versus 16,
  with output unchanged. Large-body admission stays within its documented byte
  budget; report peak RSS, bytes, retries and wall time as well as wave count.

## PR 5: Catalog-v4 qualification and reader work

Goal: qualified warm reads avoid `sessions/` LIST and reuse verified catalog
deltas. Account separately for activation/head reads, tree-node reads, local
refresh, summary selection and registration work. Reusing `RefreshRemote`
alone does not make the complete command proportional to the delta.

- **5a: qualification, enablement and cutover are separate deliverables.**
  Record repeated `_catalog probe --isolated` evidence for the user's R2 endpoint, and the
  clock and LastModified evidence, as the
  [provider qualification gate](../catalog/provider-qualification.md)
  requires, against an explicitly authorized isolated target. Reviewed evidence
  must precede a capability implementation granting production qualification.
  Writer admission, migration and cutover then follow their own rules and
  acceptance gates. A successful probe alone neither enables the capability
  nor completes cutover. This plan changes no gate, protocol number or default.
- **5b: the reader can proceed behind the existing qualification gate.**
  - **ID prefixes.** Trial an `IdentityIndex` range for an explicitly supplied
    harness where archive-ID-only selection is semantically sufficient. Without
    a harness, use the complete delta-synced local catalog: harness names are
    open-ended and the wire format has no complete harness inventory. Do not
    enumerate only built-ins or drop native-ID/text alternatives.
  - **Text and complex filters through the local catalog.**
    `CatalogSummaries` and the complete-summary fallback stop materializing
    every row through `lazyCatalogRows`/`catalogRows`. They call
    `RefreshRemote`, which runs a verified delta against the persisted
    `remote_root`, retains existing rebuild/repair behavior when required, and
    then the local `Query`. The CLI already uses this path for some candidates;
    extend shared orchestration rather than adding another sync mechanism.
  - **Local refresh accounting.** Measure unchanged-root and small-delta refresh
    at 1.8k/10k/25k/50k. `remoteCachedRows` checksums every SQLite row and cache
    maintenance/inventory still visits the root directory. Record rows read,
    bytes hashed, directory entries visited, evictions and registration reads.
    If this prevents the latency target, PR 7 must address refresh as well as
    query work; do not call the end-to-end path sublinear while these scans remain.
  - **`--no-cache`** keeps today's in-memory complete fallback.
  - **Cursors** stay bound to the snapshot root and refresh nonce. A changed
    head mid-query returns the existing refresh error.

Pass:
- On a synthetic qualified store, warm prefix and text queries perform zero
  canonical LISTs. Report delta tree reads separately from selection and
  validation reads; record unchanged-root and changed-subtree costs.
- Results equal the exhaustive oracle.
- The [migration-and-query](../catalog/migration-and-query.md) query table is
  updated in the same PR.
- 5b may merge before 5a because it is reachable only on qualified destinations.
  Track B targets count as met only after qualified enablement, cutover and
  measured end-to-end command acceptance, including remaining local work.

## PR 6: Latency-modeled command benchmarks and operation-count gates

Initial measurement code revision: `b441ed88`. Review follow-ups repair the
legacy-versus-post-PR1 scheduling labels, eviction timing, picker result oracle,
synthetic registration inventory/counters and output-visible changed handoffs.
The original observed-size 20-case command matrix passed with exact Go 1.27.2,
alongside synthetic S3 provider-page conformance. The current matrix adds four
scoped late-match cases (24 total), with ordinary 1,800-session operation/output
gates for every applicable cold, warm, changed and no-cache state. Historical
20-case logs describe the original matrix rather than this expansion. Independent source
and documentation review found no unresolved blockers. Historical controls
remain unchanged. Local row counts describe application-level validated and
decoded candidates, not SQLite engine scan or exact-count work. Qualified
catalog root/delta scale accounting accompanies PR 5b; mixed/maximum-body RSS
trials remain conditional PR 4b work. No live-provider latency is qualified.

Land this first. Extend `storagetest.MeasuredStore`, `historicalListingModel`
and the existing command benchmarks rather than adding a harness. Preserve
historical results with their original constants and source pins.

- **Model what was measured.**
  - Each actual provider LIST page costs `ListPageDelay + keys × ListKeyDelay`,
    including transferred keys discarded outside a range. `MeasuredStore`'s
    per-call delay alone is insufficient; retain the historical pagination
    model's overread behavior and verify it against a synthetic S3 endpoint.
  - Each GET costs `GetDelay`.
  - Concurrent requests overlap within the actual shared slot/byte budgets.
    Model all three listing prefixes together, including empty-prefix requests,
    range dispatch, exact and absent boundaries, and the corrected early exit.
  - Defaults come from the 2026-10-16 traces: 1,000-key pages at 0.4–1.0 s,
    GETs at 0.11–0.21 s. Calibrate page overhead and per-key cost separately;
    these observations do not establish that a small page costs 0.15 s.
  - Seed 2.8 keys per session with uneven per-session key counts, so range
    imbalance is reproducible.
- **Cover the commands people run.** Interactive `list`, bare `show`, the
  `handoff` picker, `show <short id>`, `handoff <query>` and piped
  `list --limit 50`, at 1.8k, 10k, 25k and 50k sessions. Run warm, and warm
  with 1% changed. Include cold/no-cache runs, unchanged catalog roots,
  small deltas, distinct native IDs, unknown harnesses, many unlabeled rows,
  scoped matches outside an initial page and large metadata bodies. Keep
  remote calls, local inventory/refresh and query decoding as separate metrics.
- **Gate counts, report time.** CI gates operation counts, critical-path page
  waves and bytes with deterministic assertions. Report allocations and peak
  RSS, using measured regression budgets where stable rather than exact
  allocator counts. Modeled and real wall time are reported but not gated.
  CI keeps 1.8k. The full matrix stays behind
  `AGENT_ARCHIVE_PERF=1`.
- **Real-command evidence.** Synthetic command runs use injected environments,
  temporary homes and fake stores per the [testing guide](../contributing/testing.md).
  Real-account traces require explicit operator authorization; this proposal
  does not authorize reading real settings, Keychains or buckets. The installed,
  trusted binary can supply a baseline, but cannot measure an uninstalled change.
  Compare the exact baseline and candidate binaries against the same authorized
  synthetic destination, with cache state, retention, fixture size and machine
  held constant. Record source SHAs, concurrency and credentials/client setup
  time; use repeated interleaved trials and report median and tail latency.
  Run `AGENT_ARCHIVE_TRACE=1`, retaining only content-free counts/timings.
  Interactive trials use a PTY and cancel with Esc; retain no terminal transcript,
  session IDs or titles. Record unmet targets alongside improvements.

## PR 7: Measured local refresh and query improvements

After PR 5b removes the LIST from the relevant path, use PR 6 to identify the
remaining critical work. Query time above 100 ms at 50k triggers query work;
refresh or registration costs that prevent the command target need their own
measured follow-up. Split distinct changes into focused PRs.

- Add typed columns only for predicates that PR 6 shows on the critical path,
  such as `repo_key` and `project_id` for scoped pickers, with the matching
  `(column, activity DESC, capture DESC, key)` index.
- Read candidate keys first. Decode and checksum summaries only for the page.
- Any predicate SQL cannot represent exactly keeps the current Go filter.
- Account for `Query`'s exact `Total` count: `LIMIT` does not bound counting a
  matching index range. Preserve output counts where required; a bounded query
  API may omit an unused count only after checking its consumers and cursor
  contract. Do not advertise O(log N + L) from page-sized decoding alone.
- If refresh dominates, design an unchanged-root/incremental-integrity path
  with explicit corruption detection and repair semantics before changing the
  complete validation scan. Do not simply skip checks because a root matches.
  Keep selected-row checksums and authoritative body validation unchanged.
- If registration inventory dominates after overlap and bounded concurrency,
  evaluate a private index or candidate-driven reads against the native-session
  selection contract. Do not introduce an index solely from extrapolation.

Pass: identical oracle results and corruption/repair behavior; candidate-page
decoding bounds only for eligible exact queries. Record refresh scans, count
scans and registration reads separately, and show matched command-level gains.

## Correctness and privacy constraints

- The exhaustive reader remains the selection oracle for every PR. Each new
  bounded path ships with differential tests against it, including ordering
  ties, replay and native-child handling, and missing or corrupt rows.
- SQL or tree candidate predicates must be complete supersets; truncation and
  final counts require exact predicates or complete final matching. Unicode
  case folding stays in Go. Archive/native identity precedence, show's ID/text
  union, configured labels and unknown harnesses remain part of the oracle.
- Range plans contain object keys the reader already lists and caches. They
  add no titles or content to disk. Summaries, catalogs and caches keep
  today's contents ceiling, and local files stay 0600/0700.
- Nothing in this plan uploads or drops new content. No filter, adapter or
  parser version bump or privacy changelog entry is required. If an
  implementation finds it must derive a new persisted field, it stops and
  follows [versions](../maintainers/versions.md) first.
- Validation is not a performance knob. SHA-256, ETag and `ValidateMetadata`
  checks on selected bodies are unchanged in every PR.

## Rejected alternatives

- **A change marker or delta manifest for legacy v3.** Readers could trust it
  only if every writer, including older binaries on other machines, maintained
  it. That is the writer-barrier problem catalog-v4 already solves, under a
  second format. Scale comes from qualifying catalog-v4.
- **TTL-cached bucket listings.** They violate the cache-never-authority rule
  and can hide just-captured or just-deleted sessions. A picker that draws
  local rows first and reconciles after the LIST is a promising separate UX
  investigation: measure time to first draw separately from time to fresh
  results, label provisional rows, and verify fresh authority before selection.
  It is user-visible behavior and needs its own proposal before implementation.
- **Skipping body validation on warm paths.** The time saved is small, and it
  is the integrity boundary for selected metadata.
- **Raising LIST concurrency beyond 16 in this plan.** Balance comes first
  (PR 1). More slots are a separate measured change against R2 throttling and
  per-operation cost.
- **Bounded-decode work before the network work.** The measured local query is
  25 ms against a roughly 1 s LIST. PR 7 waits until it is on the critical
  path.
- **Streaming transcript decode and render-fit changes.** These are the
  Theta(S) findings for `show --transcript` and `handoff`: about 13×S peak RSS
  and a hard cap at 128 MiB uncompressed. They are real but orthogonal to N,
  and need their own proposal against the privacy filter contract.

## Documentation on landing

Each PR updates, in the same change:
- this proposal's status
- [the listing contract](../maintainers/list-performance.md) (PRs 1, 6)
- [the catalog query contract](../catalog/migration-and-query.md) (PR 5b)
- user docs, only if visible behavior changes (none planned)
- CHANGELOG.md

When Track A and PR 5b have landed, reconcile any measured PR 7 follow-ups and
move this file to `implemented/` per the proposals index rules, with matched
trace evidence and exact source SHAs. Keep incomplete qualification, enablement,
cutover and latency acceptance explicitly visible; landed source is not evidence
of production activation or a met performance target.
