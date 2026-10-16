# Listing performance target

Provider-page measurement work extends the existing synthetic stores and
command benchmarks with separate LIST-page and per-key delay, transferred and
discarded header bytes, and GET delay. Deterministic operation counts are the
regression gates; modeled time is supplementary. Content-free trace counters
separate local row validation, summary decoding and cache inventory from remote
requests. The historical baselines below retain their original constants and
source pins; new modeled runs do not revise that evidence or qualify live R2
performance. See the [read-path plan](../proposals/sublinear-read-paths.md).

`BenchmarkCommandReadPaths` covers the six list/show/handoff read paths at
1,800 sessions by default, with 10k/25k/50k behind `AGENT_ARCHIVE_PERF=1`.
Cold, warm and one-percent-changed fixtures reset to the same exact object
snapshot for each iteration; `--no-cache` cases apply to list, which supports
that flag. Local trace accounting uses a separate untimed invocation so trace
recording does not inflate the benchmark's time and allocation measurements.
Ordinary tests retain 1,800-session command-result and operation-budget gates.
Picker checks validate every advertised visible row's identity, revision and
order, including the initial 50-row window marker. Selected show/handoff JSON
checks a changed output-visible revision. Handoff picker homes contain one
synthetic registration per archived session; their missing native files remain
inside those homes. Successful registration counts and attempted reads (where
available) are reported separately. Cache eviction spans include inventory and
filesystem deletion, counting only successful removals.
`AGENT_ARCHIVE_READ_LATENCY=1` enables a modeled timing replay: 200 ms per page,
400 microseconds per transferred key, and 160 ms per GET. These values are one
explicit sensitivity decomposition within the observed full-page and GET ranges,
not a calibration of the provider's page overhead and per-key cost.

`BenchmarkReadPathListingPolicies` explicitly separates the legacy topology
from post-PR1 projections. The legacy full-warm comparison uses sidecar-based
canonical ranges in their own sixteen-slot pool, with independent sequential
v2/v3 listings and strict-beyond boundary overread. The post-PR1 comparisons
use one, eight, sixteen or page-sized ranges across a shared sixteen-slot pool
and inclusive boundary termination. These labels describe the modeled policy;
post-PR1 results do not establish behavior on a pre-fix binary. Its page/key
delay decompositions are sensitivity assumptions:
the observed full-page timings do not calibrate small-page latency. Qualified
catalog root/delta scale accounting accompanies the catalog-reader work;
mixed and maximum-body RSS/concurrency trials remain a separate measured step.

On the default legacy destination format, ordinary noninteractive
`list --limit N` and `list --json --limit N`
use session-addressed revision-qualified `listing/v3/` entries; existing
`listing/v2/` summaries remain readable. A complete healthy archive reads
at most N canonical metadata bodies on a cold run, or N selected cache files
on a warm run, and no transcript bodies. Both the 10,000 and 20,000 session
fixtures assert the default 50-body budget against the exhaustive reader.
Warm unchanged selections perform no remote metadata GETs. Normal metadata
refreshes share the same total body budget: a stale cached revision is never
opened before downloading its replacement. Cache paths encode the canonical
key and a SHA-256 of the opaque validator. Changed-key writes prune stale
versions; each command also maintains at most 64 cache directories, resuming
from a private checkpoint and using file modification times for deferred
cleanup. Cache opening skips session directories. Flat legacy cache files are
disposable cold misses. Fresh complete listings still evict absent keys. Cache
key limits, private file handling and corruption fallback remain unchanged.

The budget covers bodies, not discovery: fresh canonical and both v2/v3 index LIST
headers are enumerated on every query, proportional to archive size. Text
uses activity time, excludes subagents before limiting, and obtains child
counts from covered summaries. JSON preserves capture-time order. Equal
capture times use harness and session identity as deterministic tie breakers. Implicit repository scope matches RepoKey
across clones and falls back to local ProjectIDs for legacy sidecars. Empty
scopes choose the existing all-project fallback before reading bodies.

Named explicit project scope, origin, model, skill and completeness queries
retain complete selection, as does `--limit 0`. Search and interactive browsing
can reuse the private typed summary catalog and load selected details; initial
summary construction and complete fresh-header checks still grow with archive
size. A missing,
stale, unsupported or damaged index produces an explicit compatibility-scan
diagnostic and complete legacy results. A selected object deleted or changed
during a bounded query fails explicitly, suggesting retry or `--limit 0`;
it never refills indefinitely. Exact counts describe discovered matching
identities, not validation of every unselected sidecar.

The opt-in catalog-v4 implementation landed in
[#460](https://github.com/wangjohn/agent-archive/pull/460). Eligible queries use
bounded tree paths and selected immutable metadata without canonical LIST;
complex predicates and scope tiers use the complete summary fallback. This is
a separate destination format, not a change to default legacy discovery costs.
See [catalog migration and query](../catalog/migration-and-query.md) for query
eligibility, pinned read views and migration restrictions. Production S3/R2
construction and migration remain refused until reviewed provider atomicity,
same-version timestamp/clock and cutover authority qualification exists; see
the [provider release gate](../catalog/provider-qualification.md). Synthetic
controls and merged source do not qualify live activation, full-load or matched
performance acceptance.

## Revision protocol

Canonical metadata is authoritative. Keys are
`listing/v3/<harness>/<id>/<19-digit reverse Unix nanoseconds>/<summary>`.
Legacy v2 keys put the reverse timestamp before harness/id. Both forms
encode the same validated summaries; v3 needs no separate cleanup pointer.
The summary is canonical JSON encoded as unpadded base64url. New v3 writers
split it into consecutive 255-byte path components (only the last may be
shorter), preserving one empty immutable object and the session cleanup prefix.
Separators count toward S3's 1,024-byte key bound. Identity components must
also fit 255 bytes. Readers retain earlier single-component v2/v3 summaries;
rebuild and ordinary v3 repair publish the bounded encoding before retiring
old hints. Segmented summaries with empty or noncanonical boundaries are
refused. Earlier v3 readers reject these segmented hints and use their
existing complete compatibility scan; they cannot provide indexed listing
for newly segmented entries. The summary contains the
fresh publication nonce (`n`), opaque provider ETag (`v`), SHA-256 of canonical bytes (`h`), activity timestamp
(`a`), optional parent ID (`p`), replay marker (`r`), ProjectID (`j`) and RepoKey (`k`). No source or skill
content is included. Unsupported/noncanonical summaries and keys exceeding
S3's 1,024-byte key bound are refused. Activity is EndedAt when present,
otherwise StartedAt for imports, otherwise CapturedAt.

Each live canonical header must have an entry matching exact key and opaque
ETag before any body is selected. No ready marker can establish coverage.
The selected response returns bytes and ETag together; its ETag, SHA-256,
canonical identity, schema and summary must match. Cache bytes are checked
against the same entry digest. ETags are never decoded as content hashes on
this path. Stores without response-bound validators use the exhaustive path.
S3 and R2 use their GetObject ETag; fake HTTP and opaque-validator tests cover
the protocol. Real provider acceptance remains a release acceptance task.

Publication journals a destination-bound per-session repair intent before
canonical commit. Successful capture survives an auxiliary failure; its
warning and repair intent remain visible. Collector passes retry at most 32
intents, rotating the cursor so a failing group cannot starve later intents.
Entries are empty immutable objects addressed directly under each session.
A v3 repair snapshots only that session's hint headers before publishing its
fresh identity, confirms the canonical validator, then retires at most 32
snapshot candidates. It leaves its intent until cleanup completes. One
normal publication reads one response-bound canonical metadata body and no
auxiliary bodies; it writes one hint rather than a pointer/hint pair. Delayed
writes and crash retries remain discoverable directly from session headers.
Concurrent snapshots cannot each include the other's later fresh identity,
so one current survivor remains even for equivalent writers or reused
validators. No logical counter or clock is needed.

`list --rebuild-index` snapshots legacy v2 headers and pointers before fresh
v3 publication and retires a combined maximum of 32 v2/v3 candidates per
session repair. Each v3 candidate needs one DELETE; legacy pairs need up to
two DELETEs (at most 64 calls for 32 candidates). Legacy partial pointers
can require at most one body GET per candidate; known hint keys require none. It reclaims legacy pointerless hints while canonical metadata
remains live. Repeating a partial rebuild resumes cleanup, including malformed
or canonical-absent hints (up to 32 per cleanup slice). It validates canonical
response bytes and writes/deletes only auxiliary objects; transcripts and
canonical metadata are unchanged. Failure reports completed metadata entries.
Legacy partial pointers are inspected only within that same candidate budget;
a corrupt pointer never authorizes deleting another session's claimed hint.

Retention and undo delete canonical discovery first, then the session's v3
entries and sources. They also enumerate legacy v2 hint headers to reclaim
pointerless artifacts, with a combined 32-candidate v2/v3 cleanup slice that
can be retried. Auxiliary failure never postpones source deletion. Archive-wide
legacy header enumeration is confined to explicit rebuild and deletion; normal
v3 publication never scans the full hint archive. Delayed old v2 writers may
recreate legacy artifacts, which remain discoverable by later explicit sweeps.
Equivalent current summaries are deduplicated across v2 and v3; conflicting
claims for one validator require compatibility scanning. A v2-only reader
cannot prove coverage for v3-only sessions and uses its exhaustive fallback.
Concurrent writers require no global lock. A concurrent rewrite invalidates
fresh coverage and produces compatibility scanning or a bounded-query error.

The benchmark in `internal/reader/list_index_bench_test.go` compares the cold
full scan, indexed cold selection and a real selected metadata cache. Timing
is supplementary; deterministic operation counts are the regression gate.

## Full scans

On the default legacy format, complete discovery still enumerates fresh headers.
Unlimited listing and complete analytics select the full required universe;
search and browsers may reuse validated summaries before loading selected
details. A missing index retains complete compatibility scanning. On R2 a
listing page of 1,000 keys takes
0.5–0.9 s, and one listing's pages are strictly sequential. So since the
[listing-at-scale](../proposals/listing-at-scale.md) phase 1 change, the scan is
split into contiguous key ranges listed concurrently. Sessions and both
listing-index prefixes share at most sixteen in-flight listings. After a
complete listing, each prefix saves private complete-key hints for roughly
1,000-key ranges; prefixes with at most 1,000 keys stay sequential. This policy
accounts for full provider pages transferred past range boundaries rather than
assuming that sixteen short returned ranges transfer sixteen short responses.
An exact inclusive range boundary terminates without fetching another page.

Hints split the complete keyspace and never prove existence or absence. Stale,
deleted or empty boundaries preserve complete ordered results; missing or
damaged hints use the existing sidecar-based planner or sequential fallback.
Unknown harnesses and non-hex identities remain covered. Each hint file is
0600, atomically replaced and bounded to `15 + ceil(keys/1000)` boundaries.
`--no-cache` retains complete sequential discovery. These changes reduce
constant factors; fresh legacy discovery still grows with archive size and
does not establish the catalog-v4 scale targets.

## Synthetic baseline recorded 2026-10-12

Measured source: `3878e73f43a2c372a63773b6ba0b436057606d5f`, Go 1.27.2,
Linux/amd64, AMD EPYC 9V74, benchmark concurrency setting 5. The source includes
normal merges of main `e1436e35` and compiler regression parent `d8d0f2cf`.
The [benchmark output transcript](listing-baseline-2026-10-12.txt), with CPU-label
trailing spaces removed from the retained original raw source, records the full
27 reader and 27 CLI.Run scale cases (800/10k/50k, limit 1/50/all,
cold/warm/changed), latency, first-screen and command-read probes, and the
21-case historical listing matrix below. Later evidence/documentation commits
do not change the measured benchmark source. These are synthetic single-run
measurements on a shared host: local wall times can include unrelated contention
and establish no paired speed comparison, idle-host threshold, live-provider,
real-terminal or platform acceptance. Counts are the primary baseline.

```sh
AGENT_ARCHIVE_PERF=1 go test ./internal/reader ./internal/cli ./internal/storage/storagetest \
  -run 'Test(HistoricalListingModel|MeasuredStore|ListRevision|ListRecent|IndexedRefresh)' \
  -bench '^Benchmark(ReaderScale|CommandScale|ReaderLatency|CommandLatency|UsableTerminalScreen|CommandReads|ListingHistoricalModel)$' \
  -benchtime=1x -count=1 -timeout=15m
```

The historical fixtures contain exactly 85% children, one current source plus
1.5 superseded source snapshots/session, and complete current v3 summaries.
Full scans return every session; indexed reads select 50 top-level sessions.
Sequential mode deliberately hides `RangeLister`; ranged cold starts without
boundaries and therefore retains sequential LIST behavior. Warm full scans
prepare all sidecars; warm indexed reads prepare only the selected 50.
Fixture creation and warmup occur outside measured time. Every measured
operation includes opening the private disk cache. No transcript is read.

The historical latency model uses 1,000 keys/page, 100 ms/request and
0.45 ms/listed key, derived from the original design's historical assumptions.
It transfers the complete final page of a range, including headers beyond its
boundary, and counts empty LIST requests. Ranges are scheduled deterministically
in key order into 16 ideal slots; exhaustive GETs use eight ideal slots and the
current selected reader uses serial GETs. LIST services precede GET services.
This virtual scheduling replaces scaled sleeps: it reports a reproducible
projection and does not simulate local/network overlap, connection setup,
retries, provider throttling or actual transport. GET body bytes, local runtime,
allocations and observed peak GET concurrency are separately measured. Header
bytes estimate key+ETag lengths plus 16 bytes for size/time, excluding XML/HTTP/TLS.
The model replays a pinned sorted header snapshot to avoid repeated whole-map
MemoryStore sorting for every range; local numbers remain harness costs.

| Sessions | Read/cache | GETs | Modeled LIST pages | Modeled wire headers | Modeled ms | Local ms | Cache open ms |
| --- | --- | --- | --- | --- | --- | --- | --- |
| 1,000 | sequential/cold | 1,000 | 4 | 3,500 | 14,475 | 327.0 | 0.07 |
| 1,000 | sequential/warm | 0 | 4 | 3,500 | 1,975 | 40.7 | 10.99 |
| 1,000 | ranged/cold | 1,000 | 4 | 3,500 | 14,475 | 273.7 | 0.06 |
| 1,000 | ranged/warm | 0 | 5 | 4,703 | 550 | 38.3 | 10.89 |
| 1,000 | indexed/cold | 50 | 6 | 4,500 | 7,625 | 15.7 | 0.06 |
| 1,000 | indexed/warm | 0 | 6 | 4,500 | 2,625 | 8.7 | 0.49 |
| 10,000 | sequential/cold | 10,000 | 35 | 35,000 | 144,250 | 2,663.1 | 0.08 |
| 10,000 | sequential/warm | 0 | 35 | 35,000 | 19,250 | 332.7 | 109.46 |
| 10,000 | ranged/cold | 10,000 | 35 | 35,000 | 144,250 | 1,220.7 | 0.06 |
| 10,000 | ranged/warm | 0 | 50 | 49,703 | 2,200 | 339.8 | 106.24 |
| 10,000 | indexed/cold | 50 | 46 | 45,000 | 29,850 | 74.5 | 0.06 |
| 10,000 | indexed/warm | 0 | 46 | 45,000 | 24,850 | 79.8 | 0.47 |
| 100,000 | sequential/cold | 100,000 | 350 | 350,000 | 1,442,500 | 24,916.6 | 0.55 |
| 100,000 | sequential/warm | 0 | 350 | 350,000 | 192,500 | 3,728.0 | 1,157.99 |
| 100,000 | ranged/cold | 100,000 | 350 | 350,000 | 1,442,500 | 24,741.8 | 0.07 |
| 100,000 | ranged/warm | 0 | 500 | 499,703 | 17,600 | 4,217.8 | 1,136.62 |
| 100,000 | indexed/cold | 50 | 451 | 450,000 | 252,600 | 778.7 | 0.09 |
| 100,000 | indexed/warm | 0 | 451 | 450,000 | 247,600 | 881.2 | 0.60 |

Isolated JSON decode of all canonical bodies (no cache I/O or transport):

| Sessions | Local decode ms |
| --- | --- |
| 1,000 | 4.8 |
| 10,000 | 34.5 |
| 100,000 | 486.6 |

The raw output includes per-case GET bytes, modeled header bytes, returned
header counts, allocations and peak GET concurrency. Warm selections and warm
full scans retain zero remote GETs. At 100k, a warm full-scan cache opening
is measurable separately from JSON decoding. Ranged warm reduces projected
LIST wait while increasing modeled request pages/end-page headers. Current
indexed reads retain the exact 50-body bound but still enumerate canonical and
v3 summary headers, so modeled header discovery grows with archive size.
These results complete the original phase-3 fixture/measurement matrix in the
baseline PR; qualified production phase-4 activation remains open. The default
legacy path still has proportional header cost, while the later opt-in catalog
implementation has the separate gates described above. The original plan’s
662-session live timings and older pinned compiler/source baselines stay
historical evidence and are not measurements of this source commit.

The recorded raw samples used the historical discard-writer harness. This
candidate captures and validates command JSON; freeze that repaired harness on
both comparison sources before accepting a performance improvement. These
historical samples do not qualify the repaired harness on current main.

## Scoped interactive catalog windows

Supported ordinary repository windows use complete scoped catalog ranges rather
than legacy header enumeration. Browse and handoff eligibility are separate;
known-zero prompt archives remain excluded from handoff without candidate scans.
Selection hydrates an immutable verified body; row projections do not authorize
sources. Named-project/advanced predicates and legacy destinations retain complete
compatibility costs. Interactive Older fetches another remote page and search
initializes the complete archive universe on demand.

The [real CLI qualification protocol](picker-qualification/README.md) measures
10k/100k first-frame and Older work, with a fixed relevant repository population,
complete bulk trees and actual production construction equivalence. It separates
returned payload/header estimates from actual provider wire bytes, archive work
from native R, latency/allocations from sampled runtime heap, and source completion
from production qualification. The
[2026-10-13 paired results](picker-qualification/results-20261013.md) completed all 16
processes / 104 cases using identical repaired harness/logical fixture hashes on
pinned protocol10 baseline and protocol11 candidate. Every supported measured
phase read zero metadata/source bodies and LISTs; actual caller head/coordinator
work is included. Sampled heap includes fixture residency and process RSS includes
construction/setup, which has a larger footprint for the complete candidate fixture.
Review, hosted gates and production qualification remain separate; historical
baseline samples above remain historical.
