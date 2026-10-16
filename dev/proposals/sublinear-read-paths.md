# Read paths at realistic scale: implementation plan

Status: proposed 2026-10-15, revised 2026-10-16 after real-command
measurements; nothing implemented. This plan takes the remaining read-path work
from [CLI performance](cli-performance.md) PRs 7 and 9 and
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
most of them subagents. One heavy user therefore reaches about 10k sessions in
five months and 25k in a year. 50k is two years, or one year with a second
active machine. 100k is the stress ceiling. At 2.8 keys per session these are
28k, 70k, 140k and 280k keys under `sessions/`.

Projected warm cost with today's code. The constants come from the table
above: R2 serves about one 1,000-key LIST page per 0.4–1.0 s per connection,
ranges are 16-way, and local costs scale linearly from the measured values.

| Sessions | `sessions/` LIST (balanced 16-way) | Today's planner | Unranged `listing/v3/` | Handoff local (bodies + registrations) |
|---|---|---|---|---|
| 1.8k | 0.15–0.35 s | 0.8–1.3 s (measured) | about 1.0 s (measured) | 0.12–0.8 s |
| 10k | 0.8–2 s | 2–3 s | 4–10 s | 0.7–4 s |
| 25k | 2–5 s | 5–7 s | 10–25 s | 1.7–10 s |
| 50k | 4–9 s | 10–14 s | 20–50 s | 3.5–20 s |

The conclusion shapes the PR order:

- Constant-factor work on the legacy format (balanced and ranged listing,
  overlap, the SQLite catalog for handoff, fewer sequential GETs) brings the
  measured archive to about 0.4 s and keeps 10k usable.
- No legacy-format change keeps `list` or the picker interactive at 25k–50k.
  Each run must prove freshness by listing every key.
- Warm reads that do not grow with N need a change token: catalog-v4's verified
  head and delta, already in source behind the
  [provider qualification gate](../catalog/provider-qualification.md).
  Qualifying R2 is the critical path for scale. It is operator work, and this
  plan does not change the gate.
- Once the LIST is gone, local decode is the next linear term: about 0.7 s of
  summaries at 50k, plus Θ(N) registration and body reads. The SQL pushdown
  from the original draft then matters, so it is kept, smaller, after the
  network work.

## Targets

Warm runs, single R2 destination. These are wall-time goals from
real-command traces on the reference machine. CI gates use operation counts
(see PR 6).

| Command | Sessions | Legacy v3 after Track A | Catalog-v4 after Track B |
|---|---|---|---|
| `handoff` picker first draw, interactive `list`, bare `show` | 1.8k / 10k / 50k | ≤ 0.5 s / ≤ 1.5 s / not met | ≤ 0.5 s at all three |
| `show <short id>` | same | ≤ 0.6 s / ≤ 1.5 s / not met | ≤ 0.5 s |
| piped `list --limit 50` | same | ≤ 0.6 s / ≤ 1.5 s / not met | ≤ 0.5 s |
| any command after K changed sidecars | any | adds about ⌈K/16⌉ GET waves | same |

"Not met" is deliberate. It is the case for migrating, not a gap this plan
hides.

## What already exists (do not rebuild)

- **Range listing.** `listObjects`/`planRanges`/`listRanges` and the
  `storage.RangeLister` `StartAfter` ranges (internal/reader/list_ranges.go,
  internal/storage/s3.go). PR 1 changes planning, not the mechanism.
- **ID-prefix lookup.** `FindMetadataPrefix` (internal/reader/metadata_prefix.go)
  already reads bodies only for ID candidates. The catalog's `Words` predicate
  already includes `instr(lowerid,?) = 1`
  (`catalogQueryPredicates`, internal/reader/session_catalog.go).
  `readShowCandidates` passes `nil` words today
  (internal/cli/session_catalog.go). Wiring, not new resolution logic.
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

## PR order

Each row is one PR, measured alone against its base with the trace recipe
below and PR 6's model.

| PR | Track | Scope | Depends on |
|---|---|---|---|
| 1 | A | Balanced listing across all three header prefixes | Main |
| 2 | A | Overlap local work with discovery; drop repeated opens | Main |
| 3 | A | Handoff on the SQLite catalog; show and handoff candidate wiring | Main |
| 4 | A | Small cache and selected-read fixes | Main |
| 5 | B | Catalog-v4 R2 qualification evidence, then the bounded catalog reader | Provider gate |
| 6 | A/B | Latency-modeled command benchmarks and operation-count gates | Main (lands first or with PR 1) |
| 7 | B | SQL pushdown for local summary queries | 5 |

PRs 1–4, 6 and 7 change no persisted archive format and need no entry in
[versions](../maintainers/versions.md). The SQLite catalog, metadata cache and
any range-plan file are private, disposable local state. PR 5 changes no
protocol or schema number. If the existing catalog-v4 encodings cannot express
something below, that part is dropped and needs its own proposal.

## PR 1: Balanced listing across all three header prefixes

Goal: the LIST costs one page wave per 16 × 1,000 keys, not one long range.

- Plan ranges by expected key count, not sidecar count. Let `planRanges` take
  boundaries from a range plan saved after the previous complete listing of
  that prefix: every ⌈keys/16⌉-th key, or every 1,000th key when there are
  more than 16k. Store it in the metadata cache directory (0600, atomic
  replace, bounded size: at most one boundary per 1,000 keys).
  - Boundaries only split work. Ranges still cover the whole key space, so a
    stale or missing plan can make a listing slower but never incomplete. The
    plan is a hint, never authority.
  - With no plan (first run, `--no-cache`), use today's sidecar-based
    boundaries. Below 400 cached sidecars, split `sessions/<harness>/` by the
    first hex digit of the session ID.
- Run `listing/v2/` and `listing/v3/` discovery through the same `listObjects`
  ranging with their own plans. All three prefixes share one 16-slot limit, so
  total concurrency does not grow.
- Keep `rangeConcurrency = 16`. Raising it is a separate, measured change
  against R2 throttling and Class A operation cost; it is out of scope here.

Pass:
- Listings are byte-identical, including order, to the unranged `List` for
  random plans, stale plans, empty plans, keys inserted at range edges, and
  ranges that become empty.
- PR 6's model shows the critical path at ⌈keys / (16 × 1,000)⌉ page waves.
- On the reference archive, a trace shows the longest range request under
  0.4 s and piped `list` under 0.6 s.

## PR 2: Overlap local work with discovery; drop repeated opens

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
- **Overlap `show`'s fetches.** `show` issues the selected sidecar GET and its
  linked-child GETs together once the selection is known, and reuses the body
  `Refresh` verified in this run when there is one.

Pass:
- The handoff, interactive list and short-ID show traces show local spans
  overlapping the LIST span.
- `archiveByID` after a pick performs zero store opens (counting `Env`).
- The selected-body and child GETs start in one wave.
- Output and errors are unchanged on the existing command tests.

## PR 3: Handoff on the SQLite catalog; show and handoff candidate wiring

Goal: no command decodes N bodies on a warm run, and resolution reads at most
a page of summaries.

- **Handoff on the catalog.**
  - Add the fields handoff's source and replay policy reads to `SearchSummary`
    (private local state; populated in `summarize`; covered by
    `catalogRecordChecksum`).
  - Bump the catalog's private layout with `PRAGMA user_version`. On mismatch,
    drop and rebuild incomplete. The catalog is rebuilt from verified
    discovery and is never authority.
  - Remove the `catalogBrowseCommand` exclusion. The picked session's body is
    still loaded and verified exactly as today before any handoff output.
- **Pass the query through.** `readShowCandidates` and the handoff query path
  pass the user's words to `CatalogQuery.Words`. That uses the existing
  `lowerid` prefix and search-column superset in SQL. Go's `sessionQuery` keeps
  the final match, tiers and ambiguity rules.
  - A second final match proves ambiguity but is not the output. `show` and
    `handoff` print the complete match count and ordered candidate rows, and
    interactive pickers offer every match. After a second match, continue the
    candidate query to exhaustion or use the complete path. Never report a
    two-row prefix as the full result. A bounded or count-only ambiguity view
    needs its own output contract.
  - Add one index, `sessions_lowerid(lowerid)`.
  - Harness filtering uses a `key` range on the existing primary key, not
    `json_extract`.
- **Bound bounded lists.** `catalogSessionsInContext` sets
  `Metadata.Limit` for non-browsing bounded lists whose predicates are all
  exact in SQL. It does not loop pages it will discard.

Pass:
- Results are identical to the exhaustive oracle, including ties, replays,
  native children, unlabeled names and PR-number words.
- A recording test shows handoff's picker decoding zero bodies on a warm run,
  and a unique short-ID show decoding at most one SQL page of summaries.
- Ambiguity with three or more matches preserves the exact count, the ordered
  candidate rows and every interactive choice.
- Stale cursors still return `ErrStaleCatalogCursor`.

## PR 4: Small cache and selected-read fixes

- `MetadataCache.putVerified` skips `pruneVersions` when its `os.Mkdir` just
  created the key directory. A fresh directory has nothing to prune. Rewritten
  keys keep today's prune, and the sweep remains the backstop. No layout change.
- Raise selected sidecar GET concurrency (`listConcurrency`) from 8 to 16, to
  match listing. Gate it on the PR 6 model and one real trace with ≥ 50 changed
  sidecars. Revert it if the provider throttles.

Pass:
- A cold N=50k synthetic profile shows `pruneVersions` gone from fresh puts.
- Cache hit, miss and eviction tests are unchanged.
- The changed-sidecar wave count halves in the model.

## PR 5: Catalog-v4 on R2 — qualification first, then the bounded reader

Goal: a warm run on a qualified destination performs one head read plus work
proportional to what changed. It performs no `sessions/` LIST.

- **Step 1 is operator work, not code.** Record repeated
  `_catalog probe --isolated` evidence for the user's R2 endpoint, and the
  clock and LastModified evidence, as the
  [provider qualification gate](../catalog/provider-qualification.md)
  requires. Writer cutover follows its own rules. This plan does not change the
  gate, protocol numbers or the default format.
- **Step 2 is the reader.**
  - **ID prefixes.** `boundedCatalogSelection` accepts ID-prefix queries as one
    `IdentityIndex` range per harness.
  - **Text and complex filters through the local catalog.**
    `CatalogSummaries` and the complete-summary fallback stop materializing
    every row through `lazyCatalogRows`/`catalogRows`. They call
    `RefreshRemote`, which runs a verified delta against the persisted
    `remote_root` and a complete rebuild only when the prior root is unknown,
    and then the local `Query`.
  - **`--no-cache`** keeps today's in-memory complete fallback.
  - **Cursors** stay bound to the snapshot root and refresh nonce. A changed
    head mid-query returns the existing refresh error.

Pass:
- On a synthetic qualified store, warm prefix and text queries perform zero
  canonical LISTs, and tree-node reads track changed subtrees.
- Results equal the exhaustive oracle.
- The [migration-and-query](../catalog/migration-and-query.md) query table is
  updated in the same PR.
- Step 2 may merge before step 1 because it is reachable only on qualified
  destinations. Track B targets count as met only with step 1's evidence.

## PR 6: Latency-modeled command benchmarks and operation-count gates

Extend `storagetest.MeasuredStore` and the existing command benchmarks rather
than adding a harness.

- **Model what was measured.**
  - Each LIST *page* costs `ListPageDelay + keys × ListKeyDelay`. Today one
    delay is charged per `List` call, whatever its key count.
  - Each GET costs `GetDelay`.
  - Concurrent requests overlap.
  - Defaults come from the 2026-10-16 traces: 1,000-key pages at 0.4–1.0 s,
    GETs at 0.11–0.21 s.
  - Seed 2.8 keys per session with uneven per-session key counts, so range
    imbalance is reproducible.
- **Cover the commands people run.** Interactive `list`, bare `show`, the
  `handoff` picker, `show <short id>`, `handoff <query>` and piped
  `list --limit 50`, at 1.8k, 10k, 25k and 50k sessions. Run warm, and warm
  with 1% changed.
- **Gate counts, report time.** CI gates operation counts, critical-path page
  waves, bytes and allocations, which reproduce exactly. Modeled and real wall
  time are reported but not gated. CI keeps 1.8k. The full matrix stays behind
  `AGENT_ARCHIVE_PERF=1`.
- **Real-command recipe.** Use the installed, trusted binary so fresh builds
  trigger no Keychain prompts. Run `AGENT_ARCHIVE_TRACE=1` and keep only span
  lines. Run interactive commands under `script -q /dev/null env -i HOME=…
  PATH=… TERM=…` and cancel with Esc. Record session and key counts,
  never IDs or titles.

## PR 7: SQL pushdown for local summary queries

Only after PR 5 has removed the LIST from the hot path, and only if PR 6 shows
local query time above 100 ms at 50k.

- Add typed columns only for predicates that PR 6 shows on the critical path,
  such as `repo_key` and `project_id` for scoped pickers, with the matching
  `(column, activity DESC, capture DESC, key)` index.
- Read candidate keys first. Decode and checksum summaries only for the page.
- Any predicate SQL cannot represent exactly keeps the current Go filter.

Pass: identical oracle results, and decodes bounded by the page size in a
recording test.

## Correctness and privacy constraints

- The exhaustive reader remains the selection oracle for every PR. Each new
  bounded path ships with differential tests against it, including ordering
  ties, replay and native-child handling, and missing or corrupt rows.
- No predicate moves into SQL or a tree range unless it is exact there.
  Unicode case folding stays in Go. SQL and tree ranges only produce supersets.
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
  local rows first and reconciles after the LIST is not rejected, but it is
  user-visible behavior and needs its own UX proposal.
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
- [the catalog query contract](../catalog/migration-and-query.md) (PR 5)
- user docs, only if visible behavior changes (none planned)
- CHANGELOG.md

When Track A and PR 5 have landed, move this file to `implemented/` per the
proposals index rules, with matched trace evidence and exact source SHAs.
