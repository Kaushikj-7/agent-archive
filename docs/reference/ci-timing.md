# CI latency and usage observations

`python3 -B scripts/report_ci_timing.py observations.json --output timing.json`
produces a diagnostic report from saved GitHub REST API snapshots. It performs
no network requests, provisions no runners, and cannot satisfy a required check.
Use ordinary engineering runs; do not dispatch twenty runs just to fill a chart.

For each tested PR revision, save:

- `GET /repos/wangjohn/agent-archive/actions/runs/{run_id}` for **both** Test and
  Levenshtein. Keep their IDs and `run_attempt`; do not combine different revisions.
- `GET /repos/wangjohn/agent-archive/actions/runs/{run_id}/attempts/{attempt}/jobs?per_page=100`
  for each run. Follow pagination and merge `jobs`, retaining the original
  `total_count`. The reporter rejects incomplete paginated snapshots and mixed
  run attempts. An empty API result is missing evidence, not zero usage.
- The initiating PR event's actual timestamp if available from independently
  retained event telemetry. GitHub workflow `created_at` is **not** that timestamp.
  Without it the reporter publishes only a run-created lower bound and does not
  qualify the end-to-end sample. Do not substitute PR creation or commit time.
- Optional `GET /repos/wangjohn/agent-archive/actions/caches?per_page=100` and
  run artifact inventory snapshots. Follow pagination. Store their collection
  timestamps; inventories describe retained bytes at that instant, not upload
  traffic or a bill. Missing inventory is unknown, never zero.

Use the existing GitHub connector, or an authenticated API client with read-only
Actions access. No workflow needs new token permissions or a privileged
`workflow_run` consumer. This is deliberately an offline collection/reporting
interface so processing untrusted PR artifacts cannot populate trusted caches.

## Input

```json
{
  "schema": 1,
  "samples": [{
    "id": "pr-123-revision-abc-attempt-1",
    "source": "API-head-sha-shared-by-both-workflows",
    "cohort": "source-only",
    "cache_state": "warm",
    "comparable_group": "eight-workers-lane-aware-parent-v3",
    "event_at": "2026-10-14T00:00:00Z",
    "required_jobs": {
      "Test": ["tier-plan", "source-audit", "linux-short (0)", "linux-short (1)", "linux-short (2)", "linux-short (3)", "linux-short (4)", "linux-short (5)", "linux-short (6)", "linux-short (7)", "linux-full", "script-static", "script-install", "script-purge (0)", "script-purge (1)", "script-tests", "lint", "cross-linux-arm64", "cross-darwin-nocgo", "cross-linux-release", "cross-build", "macos-smoke", "linux-race", "Git identity (runner-current)", "Git identity (upstream-2.30.2)", "Git identity (upstream-2.39.2)"],
      "Levenshtein": ["verify", "native-release-vulnerabilities"]
    },
    "workflows": [{"run": {}, "jobs": {"total_count": 0, "jobs": []}}]
  }]
}
```

Replace `run`/`jobs` with the complete saved API objects for each workflow.
The example inventory illustrates the current topology; copy **actual exact API
job names** from a known complete run, reconcile them with the current workflows
and required check policy, and commit that inventory with each measurement
campaign. Never infer the expected inventory from the very run being assessed:
a missing job would otherwise disappear from expectations. Intentional skipped
jobs outside the declared PR inventory may remain in the snapshot; every declared
job must finish successfully. Successful workflow conclusion alone is insufficient.
Qualification always requires both Test and Levenshtein inventories and snapshots;
a historical single-workflow snapshot can be reported but cannot qualify. The
initiating event timestamp must precede or equal both workflow creation times.

`source` is the API head revision, not necessarily `GITHUB_SHA` (the tested merge
commit). Existing worker manifests and source proofs retain the tested checkout
identity. Keep those artifacts with the observation; this report does not replace
source verification or coverage reconciliation.

Allowed cohorts: `source-only`, `cli-catalog`, `native-platform`,
`dependency-toolchain`, `fork-cold-cache`. Assign conservatively from changed
paths and the existing tier plan; don't silently mix sensitive platform changes
into the small-source cohort. Use a new `comparable_group` when worker counts,
compiler/flags, required coverage, or scheduling policy change.

Cache state is `warm`, `cold`, `mixed`, or `unknown`. Declare warm only with
actual cache-hit evidence from all applicable accelerators, including tool and
native compilation caches. Unsupported cache paths are not hits. Existing cold
compiler/tool jobs mean the entire run is not fully warm. Unknown state prevents
qualification; cold and mixed observations remain separate groups. Cache
inventory can be included as `cache_inventory`; artifact inventory as
`artifact_inventory`. Preserve raw receipts as `worker_evidence` references.

## Reading the report

The maximum required job completion across **both** workflows determines elapsed
time. Job starts minus workflow creation include dependency waits, scheduling,
and queues; the API cannot separate these, so the field explicitly says
`run_created_to_job_start_seconds`. Actual runner-minutes sum all job intervals,
including jobs outside the required inventory, not just the longest branch.
They are not rounded billed minutes. Failed/cancelled/skipped required jobs,
missing workflows/jobs/timestamps, and source mismatches cannot qualify.

Step timestamps expose checkout/setup, restore/module verification, installation,
evidence upload, and aggregate verdict costs. Automatic step categories are only
reading aids; raw names and conclusions remain visible. Go's `go test` combines
compilation and execution; do not mislabel its whole invocation as body time.
The existing shard phases separately expose discovery and CLI compilation/listing.
Tier commands print batch wall time and per-command durations; overlapping
commands are explicitly marked and their durations must not be added.

The macOS smoke artifact keeps `tier.json`, `native-build.log`, compiled
`native-list.jsonl`, and one raw JSON stream per actual native test invocation.
`tier.json`'s `native_smoke.phase_seconds` separates the whole production build,
compiled discovery, unrestricted native packages, mandatory CLI parents, and
conditional sensitive CLI/catalog regressions. These are serial phase durations,
including any compilation inside that phase. Failed phases retain their elapsed
time without a successful source/coverage receipt. Base and conditional CLI
commands keep separate package lifetimes and their original 10/20-minute
budgets; sharing discovery does not pool deadlines or skip a native test body.
Hosted smoke receipts use the same full compiler, toolchain, SDK and runner image
identity as the verified compilation cache. Pre/post identities must match the
current canonical host; restored compilation never replaces fresh body evidence.
The unrestricted native command must yield all seven package PASS streams and
actual body lifecycles. Selected CLI/catalog parents are independently reconciled
against their compiled listing and actual parent/child terminals. Established ordinary capability/Keychain opt-in skips
remain explicit; selected CLI/catalog parents and all observed children must pass.

Outside GitHub Actions, `./scripts/test_macos_smoke.sh --output native-tier-results`
selects explicit local diagnostics. It runs the same build and native bodies,
including dirty local source, but labels its evidence `local-diagnostic-only`
and emits no hosted compiler/source qualification receipt. A hosted event cannot
use that mode or accept its receipt as native qualification. Direct local tier
invocations may use `--native-local --tier pr --suite macos`; the local guard
checks Darwin/cgo without claiming a GitHub runner image.

Append `--evidence path/to/go-shard-0` (repeat for each worker) to summarize the
existing manifests, flags/compiler context, runner image/cache observation,
phases, slow package terminals, and complete top-level parent trees. Parent
`seconds` uses lifecycle timestamps from `cont` to the terminal, or `run` to
the terminal for an unpaused parent, including descendant waits after that start.
Time paused before `cont` is excluded. Missing timestamps, missing terminals,
paused parents without `cont`, and malformed lifecycles yield null wall timing
with diagnostic issues. Each invocation remains separate. `own_elapsed_seconds`
retains Go's terminal `Elapsed`, which can omit waits for parallel descendants;
it is not a complete-parent wall estimate. The report does not
reinterpret these artifacts as a coverage verdict. Cache bytes and tool
installation are measured from inventory/API step data, not inferred from hits.

Ten unique qualifying runs in a comparable cohort permit directional comparison.
Twenty permit a **provisional**, nearest-rank p95. Attempts of the same workflow
run do not increase sample size, even when paired with a different counterpart
workflow run; all attempts remain visible. Within each comparison group the first
qualifying sample is retained only if neither workflow run was already selected.
`selected_sample_ids` identifies the observations used for the percentile. A small
successful sample never proves the 150-second goal. Failure frequency, exclusions and reruns
must accompany the latency report. Cold, fork and platform cohorts need their own
qualification; a passing warm source-only cohort says nothing about their p95.

Qualification is pending until normal traffic supplies the necessary observations.
Keep the prior ~47.1 runner-minute snapshot separate from newer per-worker-vet
coverage; it is historical context, not an equivalent performance comparison.
Investigate total runner-minute growth over 20% against a genuinely comparable
baseline. Keep standard runners, seven-day diagnostic artifacts and the existing
10 GB cache capacity policy; reporting must not enable paid capacity.

## Complete-parent scheduler topology

The version-3 shard catalog discovers compiled Test/Fuzz/Example parents for CLI,
catalog, collector, statshtml and state in the exact race-short configuration. Light packages
remain whole-package obligations, including no-test compilation and TestMain.
The catalog records source/tree proof, discovery configuration, explicit unit IDs,
worker count and exhaustive disjoint assignments. Unknown names are automatically
included; hints cannot remove coverage. Selected descendants remain under their
complete top-level parent. Per-worker whole-source vet retains its own full-context
receipt and independent plain audit coverage.

Each worker groups its assigned parents into one invocation per package. An
ordinary lane sequences those package groups and whole light packages; a separate
CLI lane can overlap it with an explicit CPU budget. Git observer tests execute
first in isolation. Invocation duration excludes waiting for a predecessor, and
parent timing uses the same cont-to-terminal/run-to-terminal lifecycle helper as
the diagnostic reporter. Include `parents-*.json` when inspecting raw body evidence.
Parallel-parent intervals include descendant waits and contention; they are
scheduling advice, not additive CPU time or promised wall time. Catalog seeding
may repeat across workers and must be measured in hosted trials.

The initial version-2 qualification used four standard Linux workers; the later
topology uses eight standard Linux workers. Use distinct comparison groups for four/eight workers and
for the earlier whole-package scheduler. Keep prior run inventories unchanged.
The eight-worker inventory must explicitly add `linux-short (4)` through
`linux-short (7)` and its comparison group must change to
`eight-workers-complete-parent-v2` for that historical algorithm. The current
lane-aware collector trial uses `eight-workers-lane-aware-parent-v3`. To roll back
concurrency, change matrix,
WORKERS, runner/reconciler arguments and workflow contract tests together to four;
both four and eight are supported by the catalog and cache guards. Keep the
stable `linux-race` aggregate required check. Do not mix schema-less historical
manifests into live reconciliation; only the explicit historical hint-import
interface accepts those old records.

## Shared checker scheduling experiment

Production shared verification stays at `--jobs 4`. Use the existing
**Levenshtein → Run workflow** control and opt into `benchmark-checker-jobs`
only when a concrete source revision is ready for a scheduling experiment.
The input defaults to false; PRs, main pushes and daily scans never start the
extra benchmark worker. It uses one standard `ubuntu-24.04` worker for at most
30 minutes, with seven-day diagnostic artifacts. Ordinary verification and the
separate release-compiler vulnerability scan still run normally.

The helper `scripts/benchmark_shared_verify.py` uses the same immutable shared
policy, exact compiler context and tracked-input native staging as PR checks.
It requires the complete six-check pre-merge inventory, performs one successful
warmup, then runs `4, 2, 2, 4` checks concurrently in that order. Each invocation
runs the whole policy; these numbers are concurrency limits, not selected checks.
Each measured invocation starts from a separate byte-verified copy of the same
warm **compilation** snapshot. Snapshot copying and verification are recorded
outside the checker interval. The task-local snapshot is capped at 3 GiB and
removed at completion; it is never uploaded or published as a repository cache.

Result caches, scanner/tool output and XDG directories start empty for every
invocation. Explicit `rerun_checks` also requests fresh analysis under the pinned
interface. Vulnerability scans use their current database. Go may reuse its
source/action-keyed compilation objects; previous-source checker results cannot
satisfy this experiment. Full plans, raw reports, stderr, exact source/compiler
identity, baseline bytes, per-check timings and the paired median ratio are saved
in `checker-concurrency-benchmark`. An incomplete, failed, missing, duplicate or
cached result invalidates the observation. Keep failed diagnostics when assessing
whether a concurrency setting is useful.

This is a small diagnostic with two observations per variant, **not** a latency
qualification or a reason to change production automatically. Compare body time,
RSS/CPU contention if separately observed, and repeat on another representative
source only when the result warrants it. Warm compilation does not imply warm
Staticcheck facts: analysis/results deliberately start fresh. Native Darwin
lint/deadcode, Linux lint/vet, new exported comments, exact-source audit/vet and
release-compiler scanning remain ordinary required checks; the experiment does
not replace any of them. Do not extrapolate its Linux result to macOS bodies.

After final topology integration, freeze a new independently checked job
inventory and comparison group before collecting normal-traffic observations.
Include all eight Linux workers, every split build/script/purge lane and their
stable aggregates; manually dispatched diagnostics do not enter that cohort.
Record fork/cache/platform/dependency cohorts separately and retain failed runs
and unknown cache states. The refreshed equivalent-coverage baseline is Test
run `38610109893` plus Levenshtein run `38610109924`: approximately **54.18
actual runner-minutes** and a **463-second workflow-created lower bound**.
The earlier 47.1-minute example predates the per-worker vet requirements. A
20% runner-minute increase is an investigation trigger, even when standard
public-repository compute is free. Preserve the 10 GB repository cache setting;
LRU eviction and cold fallbacks are preferable to enabling paid storage.

Qualification remains pending until 20 unique successful comparable observations
with actual initiating event timestamps support the declared warm-cohort p95.
Queueing, cache restore/upload and aggregate completion belong in that latency;
a checker-only median or a synthetic manual run cannot establish the 150-second
end-to-end goal.

### Optional unrestricted same-source reference

For a bounded coverage comparison, select Extended's manual `pr-reference` suite
and provide `reference_source` as the full immutable **tested merge commit** from
the eight-worker artifacts. Select a workflow ref containing this helper and mode;
the job checks out the exact reference source, verifies its tracked source/tree and
compiler before/after execution, and records the workflow ref SHA separately.
Blank/mutable refs fail before checkout. Other Extended suites and schedules do
not include this job; it is never a new PR gate or a recurring full campaign.

The reference compiles the whole listing and executes unrestricted-name
`go test -race -short -count=1 -json -timeout 20m -p 1 ./...`. No `-run`, `-skip`,
parent shard selection or timing hint is used. Package concurrency1 isolates the
native Git observer package and does not reduce bodies. Committed fuzz seeds run;
mutation campaigns and exhaustive non-short scales retain their separate policy.
The race source-audit placeholder records the same owned skip as PR workers;
comparison separately verifies the real plain source audit and all worker own-vet
receipts, plus a fresh reference own-vet receipt. Seven-day artifacts retain raw
listing/body streams, contexts, source proof and failed receipts.

After verifying artifact archive/API digests and fetching the exact tested commit,
run `python3 -B scripts/run_pr_reference.py compare --reference <reference-dir>
--workers <worker-root> --audit <audit-dir> --output <comparison.json>` on one line.
This invokes canonical worker reconciliation then compares all compiled parent and
package obligations against actual unrestricted terminals. Short/platform skip
identities must match in both directions; raw reasons remain in each body receipt.
An extra/missing skip is a failure to investigate, not automatic permission to
accept skipped coverage. Compiler/flags/tool/OS identity must match using the
existing ImageVersion-only compatibility projection; raw full contexts remain
verified. The final reference must use the same source SHA/tree as the worker
artifacts, not merely a related branch head. This qualification is correctness
evidence, not a runner-time or150-second benchmark.

## Post-fixture eight-worker hint refresh

`go_shard_hint_sources.json` now pins Test run `38643152910`, attempt 1,
source `c48ffb13008afd1751b50248fef198a7072474b7`, tree
`e8f06a9fcf48627539077377fdc4d78921872598`, and nine API-digest-verified
worker/audit archives. This is one mixed prefix/miss observation. Regenerated
package, CLI and heavy-parent weights replace the earlier pre-fixture estimates;
the new geometry scanner parent is measured rather than assigned a default.
Current compiled discovery and successful body receipts remain coverage authority.

This historical sample predates the compiler-context schema change. Reproduction
must use the **immutable measured source's** Python generator/reconciler modules,
whose hashes are recorded in the request attribution, with access to that exact
Git source and body catalog. Export its `scripts/*.py` into a task-owned directory;
load that directory first in an isolated Python process, point the imported
runner/tier `ROOT` at the Git checkout containing the measured commit, and call
that unchanged generator's `main` with the committed request, artifact root and
output. The artifact root contains `workers/` and `audit/`. This uses the existing
historical import interface to validate its recorded schema; do not bypass the
live context validator, alter receipts, or relabel the old context as current.
Keep the request, raw artifacts and pinned historical source together. Future
warm data should use the then-current producer and matching verifier.

Hint equality is not proof of balanced wall time. In this sample, a prefix-hit
worker's ordinary lane took 298.78 seconds, including whole collector 156.352
and reader 47.122 seconds, while its overlapping CLI took 107.03 seconds and
own-vet 1.20 seconds. Cold cache fixes alone cannot eliminate that body lane.
Global summed CLI/ordinary parent weights can hide unequal lanes; complete-parent
wall intervals may overlap, and lazy once-per-process catalog fixture costs can
move between selected parents. Retain package bootstrap and lane timings when
choosing further changes. Production allocation/CPU policy stays unchanged here;
any lane-aware balancing or additional selected packages needs separate measured
hosted comparison, unchanged coverage reconciliation and review.


## Lane-aware collector trial

Catalog schema 3 identifies `independent-lanes-lpt-v1`. The existing generic LPT
helper ranks CLI and ordinary units separately, then pairs higher ordinary
advice with lower CLI advice. Sparse lanes may have empty bins, but the combined
worker catalog remains nonempty, exhaustive and disjoint. Initial fixed-cost
advice is included once on the ordinary side. Stale/absent hints cannot remove
units; new compiled collector parents initially use the explicit 1000ms default.
Those unmeasured defaults are not a claim that collector contains414 compiled
parents or that each parent takes one second. The hosted exact compiler listing
will establish its actual Test/Fuzz/Example inventory before any selected body.

Collector is the only added selected package; reader remains a whole-package
obligation. Every selected parent retains all runtime descendants. Per-worker
package grouping still runs TestMain/lazy fixture work once per package process,
including the real96-row catalog seed whenever a selected parent needs it.
Ordinary groups stay sequential while CLI overlaps with the existing fixed2+2
CPU budget; native Git observers remain isolated before overlap. Race/count1,
assertions, named skips/restorations, source/fullcompiler/every-worker vet and
independent plain audit contracts and all test deadlines are unchanged.

Producer, raw-log verifier, aggregate, hint generator and unrestricted comparator
use schema3 together. Old schema2 live catalogs are rejected. Historical schema2
advice retains its pinned source and immutable import provenance; its assignment
is not reused as live coverage. Verify the normal eight-worker trial against the
same-source unrestricted reference before accepting the new algorithm. Refresh
collector weights only from those verified current compiled/body logs. Compare
actual lane/group/bootstrap/listing/restore/upload/aggregate durations and total
runner-minutes after verified warm compilation. Advisory balance alone is not
150-second success; CPU oversubscription and further test deferral are outside
this trial.
