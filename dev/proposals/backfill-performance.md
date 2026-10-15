# Backfill transcript inspection performance

Status: proposed implementation plan, 2026-10-13. Based on checkout
`47ac23d5`; no production optimization or matched performance acceptance is
claimed. This covers local backfill planning, complementing
[CLI performance](cli-performance.md) and
[historical backfill recovery](historical-backfill-recovery.md).

## Outcome and priorities

Make `backfill --since 7d --harness codex` faster by doing less transcript work,
with bounded memory and no increase in the default worker count. Preserve
candidate decisions, privacy filtering, native history ownership, source-change
detection, recovery evidence, duplicate precedence, and import admission.

Prioritize:

1. Streaming inspection summaries: reduce allocation and retained memory on
   the first run, without replacing the privacy codec with a second parser.
2. Verified inspection-summary reuse: avoid JSON decoding and redaction on
   repeated planning of unchanged, unarchived sessions.
3. Early date exclusion: avoid processing ordinary sessions whose authoritative
   header start is outside the selected range.

Delivery order differs from impact ranking: establish a small measurement
baseline and bounded aggregate tracing first, then streaming inspection,
then verified reuse and date exclusion from successful inspection evidence.
A header-only date exclusion is deferred: an old malformed body or later native
metadata can leave the current candidate without a start and with a refusal
reason. A header timestamp alone cannot preserve those candidate fields. Repeated-read consolidation is conditional on profiling.

The previous investigation observed 1,511 files / 3.49 GiB in the default Codex
stores, with 1,352 header starts inside that run's seven-day window. Only
0.44 GiB was outside the window. These are inventory observations, not runtime
measurements; early dates alone will not address most of that workload.

## Current execution and implementation seams

- [Planning](../../internal/backfill/plan.go) discovers sources, resolves
  projects and recovery, classifies archive state, filters selected transcripts,
  then applies final dates. `selectAdapterWork` does not reject known old starts.
- The CLI sets planning workers to **one** because its native lookup and source
  ownership are serial. The library `defaultWorkers` uses half the logical CPUs,
  clamped to 2–8 when the caller does not override it. The 128 MiB
  `byteBudget` reserves raw file size; a larger file runs alone. This is a
  scheduling bound, not a process RSS bound.
- `runAdapter` builds a complete filtered transcript and retains only compact
  inspection facts. [Import inspection](../../internal/agents/importconfig/config.go)
  may decode encoded records again to identify conversation.
- [The codec](../../internal/agents/nativecodec/adapters.go) decodes records,
  sanitizes objects/text, serializes safe rows, and retains encoded output.
- [Ordinary Codex snapshots](../../internal/agents/codex/related_source.go)
  hash the prefix before filtering. [Prefix verification](../../internal/transcriptio/snapshot.go)
  reads and hashes it again. Additional admission checks can invoke verification;
  measure the actual passes rather than assume one fixed count per session.
- Reuse [existing tracing](../../internal/cli/trace.go), native read budgets,
  and native provider ports. Keep agent-specific rules inside integrations.

## PR 1: Baseline, bounded tracing, and date evidence contract

Dependency: current main. Record the unoptimized baseline before implementing
date exclusion or any other optimization. PR 1 records the existing old-body
error and late-metadata contract; it does not enable a header-only shortcut.

### Measurement

The first implementation task is a repeatable whole-command benchmark under
`internal/cli`. Freeze this harness and session corpus, run it on the baseline
commit, and save the results before changing production code. Planning-only
benchmarks below supplement this benchmark; they cannot establish that actual
backfill completion became faster.

Run the same fixed session sets in two modes:

- **Plan:** `backfill --dry-run --since 7d --harness codex`, timed from command
  entry through the complete rendered plan and command exit.
- **Complete backfill:** `backfill --yes --since 7d --harness codex`, timed
  from command entry through discovery, inspection, admission, registration,
  collection, upload to a disposable deterministic test provider, batch
  completion and command exit. Do not use `--background`, which returns before
  the work is complete. Validate completed registrations and stored artifacts,
  including expected skip reasons and counts, rather than accepting exit alone.

Construct fixtures and build binaries outside the timed region. Freeze the
clock, timezone, source files, config, provider behavior and initial archive
state. Restore that initial state before every timed import so later runs do
not become cheap already-archived no-ops. For a warm inspection-cache import,
prime the disposable summary cache separately, then restore archive/config
state while preserving only that cache. Separate unchanged repeated planning
from repeated full imports; each scenario must do its specified work.

Report total elapsed seconds, successful sessions backfilled per second, input
bytes, discovered/selected/imported/skipped counts, planning/checking/import
stage times, CPU time and peak RSS. Identify test-provider timings explicitly:
they measure complete application work under fixed provider behavior, not live
S3/R2 throughput. If provider-latency scenarios are added, keep them separate
and identical on both commits.

Extend `AGENT_ARCHIVE_TRACE` with bounded aggregate backfill metrics: time by
stage, selected/skipped session counts, bytes read by purpose (header, hash,
filter, verification), adapter invocation count, worker occupancy, and byte-budget
wait time. Do not emit source paths, native IDs, record contents, or per-record
events. Aggregate locally in workers and merge at completion; disabled tracing
must avoid hot-loop timing and event allocation.

Add deterministic end-to-end planning benchmarks under `internal/backfill`,
using existing discovery/provider injection and counted file reads. Include:

| Fixture | Purpose |
| --- | --- |
| 1,500 mostly recent files, mixed record sizes | Representative first-run workload |
| 1,500 mostly old files, narrow date window | Date exclusion effectiveness |
| One 200+ MiB file and many small files | Budget waits and progress stalls |
| Tool-output-heavy and retained-text-heavy inputs | Separate decode, redaction and allocation costs |
| Parent with several children sharing history | Dependencies outside the date range |
| Repeated unchanged run, followed by one changed file | Future cache acceptance |

Use small equivalents for ordinary tests; large fixtures belong in explicit
benchmarks. Pin fixture seeds, worker count and toolchain for comparisons.
Measure end-to-end wall time, CPU time, allocated bytes, read bytes/passes,
GC activity, and subprocess peak RSS. Allocations and the resource ledger alone
do not establish peak memory. Run resource comparisons without the race detector.

### Deferred date exclusion

Observed header facts alone do not attest complete body validity or the absence
of later relationship metadata. First preserve and test the existing candidate
fields and skip reasons, then use a verified successful inspection summary in
PR 3 to establish safe eligibility. Until then, ordinary old files retain the
full adapter path. The optional capability below is added only when exercised
by such a proof; PR 1 adds no unused API scaffolding.


Add an optional integration-owned capability for an authoritative planning start
from observed header facts. Codex can initially supply this only for ordinary,
unambiguous sources whose final import start uses the same header timestamp.
Unknown or unsupported starts retain the current path.

Apply the shortcut after complete inventory, recovery preparation and duplicate
grouping, before adapter selection. Keep out-of-range files in the inventory:
they may still establish clone conflicts, exclusions or ancestor dependencies.

Initially bypass the shortcut for duplicates, proposed recovery witnesses,
children/forks/related history, identity uncertainty, and changed sources. Preserve
source checks, archive-state precedence, project facts, start provenance, and
the local-day semantics of `--since`/`--until`. Do not prune date-named folders,
use modification time as creation time, or extrapolate an ordinary rollout's
header rule to a logical thread spanning multiple files.

Acceptance: eligible old files invoke no full adapter; recent imports and
ineligible cases match the existing implementation. Differential tests must
compare candidate fields and skip reasons, including old malformed bodies and
metadata appearing later in a file. Resolve any observable discrepancy before
widening shortcut eligibility. Retain normal dependency reads for old ancestors.

## PR 2: Stream filtered inspection into a compact summary

Dependency: PR 1. Highest first-run resource opportunity; speedup is measured,
not assumed, because privacy redaction still runs.

Introduce an optional inspection capability through `agentapi`, implemented
initially by Codex. Backfill uses it when supported and retains full filtering
as the compatibility fallback for other integrations.

Refactor the existing codec's record loop to accept a bounded output sink. The
collector's sink retains records as today. The planning sink consumes each safe
record and keeps only import facts: conversation presence, identity observations,
start evidence, output byte/record counts, and required refusal/history facts.
Keep the native provider snapshot and admission evidence as the source of Codex
bindings; the summary does not invent or authorize ownership.

Both sinks must share decoding, metadata validation, privacy transformations,
record selection, size accounting, and errors. Observe conversation after
sanitization using integration-owned rules. Read through the required complete
boundary; finding the first user message must not stop validation of later
metadata, oversized records, source changes or refusals.

Implement in two steps within this PR:

1. Encode each safe record with the existing encoder and discard it after
   summary/accounting. This removes whole-transcript accumulation and a second
   conversation decode, while preserving exact encoded size semantics.
2. Eliminate encoding only if profiles justify it and an exact counting writer
   shares the serialization rules. An approximate JSON size estimate cannot
   replace the output-limit contract. This step may be deferred independently.

Keep retained-byte/record limits active even when no output is retained. A
summary path must refuse anything the full path would refuse. Charge scanner,
decoded-record and encoding scratch through existing resource mechanisms; bound
summary sets explicitly. Never retain raw strings/maps in a long-lived summary.

Acceptance: full-filter and summary modes produce identical import decisions
across the golden/compatibility suite, late malformed metadata, unknown records,
limits, truncation, appends, rewrites and related histories. Large outputs no
longer accumulate `FilteredTranscript.Records` during planning. Proposed target:
at least 50% fewer allocated bytes on the retained-output-heavy fixture, with no
material CPU or wall-time regression on small sessions. Record actual gains on
the representative mixed fixture before claiming a user-facing speedup.

## PR 3: Bounded, content-verified inspection summary cache

Dependency: PR 2. Highest repeat-run opportunity; already archived sessions are
already skipped, so measure repeated dry runs and interrupted/unconfirmed plans.

Start with ordinary Codex files only. Store intrinsic inspection facts, not
final candidate eligibility, resolved projects, archive status, permissions or
recovery proofs. Recompute those mutable decisions on every run.

Key reuse by agent/source identity, verified captured-content digest and boundary,
adapter/filter/inspection versions, and applicable limits. Validate cached data
with a small versioned schema. File identity/size/mtime can shortlist entries,
but matching stamps alone must not authorize positive reuse: same-size rewrites
and restored timestamps must invalidate the result.

Obtain the digest from the source provider's verified snapshot preparation so
cache hits reuse existing hash work. Expose a narrow provider-owned content
proof if necessary; do not treat a stamp-based `SourceSignature` as a content
digest. Keep end-of-use source verification. Cache hits still read/hash bytes;
their benefit is avoiding decode, sanitization, serialization and allocations.

Begin with successful, complete inspections only. Source changes, cancellation,
incomplete history, unavailable evidence and transient errors are misses and
are never persisted as stable verdicts. Changed files initially receive a full
inspection; incremental append inspection is outside this plan.

Use a dedicated disposable local cache with private file permissions, atomic
updates and bounded work: proposed caps are 10,000 entries / 16 MiB on disk,
with paged lookups rather than loading all entries. A small bounded writer
queue or batches must not accumulate all session results. Cache failure falls
back to inspection and cannot block planning or change import decisions.

Backfill's existing `BuildPlan` contract says it writes nothing. Preserve that
by injecting cache reads and emitting compact observations to a caller-owned
cache writer outside the pure planner. Document the command-level disposable
cache write; `--dry-run` must still make no registration, config or archive
changes. Do not silently redefine the planner contract.

Acceptance: an unchanged supported second run performs zero full adapter scans;
one changed file causes one full scan. Compare complete candidates with cache
disabled, cold, warm, corrupt and evicted. Test version/limit invalidation,
same-size rewrites, restored timestamps, changed permissions, missing/replaced
files, concurrent cache writers and cancellation. Proposed target: at least
50% lower checking-stage wall time on the unchanged mixed fixture; report total
planning time separately, since discovery and recovery remain necessary.

## Conditional PR 4: Consolidate redundant read passes

Dependency: PR 2 and PR 3 measurements. Proceed only if hashing/verification
reads remain a substantial cost; do not spend this milestone on a minor share.

For ordinary uncached files, investigate computing the baseline digest while
the verified filter stream consumes bytes, followed by independent prefix
verification. For cache hits, retain the provider's verified digest path. Reuse
verification only within a tightly defined operation where no intervening read
or ownership change invalidates it.

Specify the byte boundary and proof lifetime before changing APIs. Test same-size
rewrites during and between reads, partial records, appends, replacement, links,
and source changes between planning and registration. Existing stat equality is
not a substitute for content verification. First ship ordinary-file support;
ancestor prefix caching/history proofs need separate analysis.

Acceptance: counted reads show a removed redundant pass, decisions match, and
rewrite/containment tests pass. Keep the optimization only if paired benchmarks
show useful elapsed-time or I/O reduction without higher peak memory.

## Delivery and verification

### Required final benchmark

After the final optimization lands in the candidate, rerun the exact frozen
whole-command harness and session sets against both the original baseline and
the final candidate on the same host. Apply harness-only changes to both if
repairs are needed; never compare results from different harnesses. Run at least
five paired baseline/candidate samples for each required scenario:

| Scenario | Required comparisons |
| --- | --- |
| Mostly recent mixed-size corpus | Cold summary-cache plan and complete backfill |
| Mostly old corpus | Cold summary-cache plan and complete backfill |
| Large file alongside small sessions | Complete backfill, CPU and peak RSS |
| Related histories with old ancestors | Complete backfill and artifact equivalence |
| Unchanged and one-file-changed corpus | Warm-cache plan and independently reset complete backfill |

Publish a before/after table containing session count, input bytes, median total
seconds, spread, sessions/second, CPU seconds, peak RSS and speedup. Also show
stage timings so faster checking cannot hide slower registration or collection.
Verify equivalent imported session sets, normalized artifact contents and final
batch completion. Save raw samples and exact source/harness/fixture versions
with the report. Any end-to-end regression must be explained and addressed
before accepting the implementation; a checking-stage improvement alone is not
success. This final benchmark/report is a required deliverable, not an optional
follow-up.

Each PR records its baseline/candidate SHAs, fixture version, worker count,
toolchain, host, cold/warm state, raw samples and output equivalence. Use at least
five paired runs and report medians plus spread. Warm filesystem pages and warm
inspection summaries are distinct states. Initial performance targets above are
engineering targets, not measured results or cross-machine guarantees.

Run focused differential and operation-count tests, then affected package race
tests and the repository checks in [Testing](../contributing/testing.md).
Run full lifecycle/recovery regression tests where native provider behavior
changes. Fixtures stay synthetic and read/write only disposable test stores.

Preserve the CLI planning override of one worker, library worker defaults, and
scheduling budgets across the first three
PRs so gains can be attributed to less work. Trace overhead target: below 2%
when enabled on the representative fixture, negligible when disabled. No
material increase in CPU time or peak RSS for any accepted optimization.

Defer additional workers, a replacement memory-aware scheduler, cross-session
ancestor pass sharing, persistent filtered transcripts, incremental append
parsing, and a remote catalog. Revisit scheduling only after streaming inspection
shows the actual working-memory shape and an explicit aggregate reservation can
replace raw-size scheduling safely. A progress UI improvement can ship later;
it does not make transcript processing faster.
