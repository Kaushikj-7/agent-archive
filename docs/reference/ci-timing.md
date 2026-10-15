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
  `total_count`. The reporter rejects incomplete paginated snapshots and future
  job attempts; evidenced carried-forward allocations remain distinguishable. An empty API result is missing evidence, not zero usage.
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
      "Test": ["source-audit", "linux-short (0)", "linux-short (1)", "linux-short (2)", "linux-short (3)", "linux-short (4)", "linux-short (5)", "linux-short (6)", "linux-short (7)", "linux-full", "script-static", "script-install", "script-purge (0)", "script-purge (1)", "lint", "cross-linux-arm64", "cross-darwin-nocgo", "cross-linux-release", "cross-build", "macos-smoke", "linux-race", "Git identity (all versions)"],
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

Allowed cohorts: `unknown` (unclassified and never qualifying), `source-only`, `cli-catalog`, `native-platform`,
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

## Verified schema 3 hint refresh

`go_shard_hint_sources.json` pins Test run `38670278594`, attempt 1, actual
source `bb84d9fd58b0665fde9b201050b80a19e68920b0`, tree
`af4e8d922b9628cf40a94f967012667e01937613` (head 98fe), and nine
API-digest-verified worker/audit archives. Complete compiled discovery and raw
body reconciliation passed for 84 packages and 3154 disjoint units: 2203 CLI parents,
128 catalog, 414 collector, 276 state, 54 statshtml parents and 79 whole packages.
All 3075 selected parents and runtime descendants, every worker's fresh own-vet,
independent plain audit, exact source/compiler and original commands were verified.
The 12336 actual body records include descendants; they are not 3154 top-level units.

Regenerated package, CLI and selected-parent weights use those actual successful
lifecycles, replacing historical c48ff advice and unmeasured collector defaults.
This remains one mixed trusted/exact compilation observation, not a fully warm
cohort or p95. Dynamic compiled discovery remains coverage authority; new units
receive 1000 ms defaults and cannot disappear when advice changes.

The committed request retains exact per-worker settings/full contexts, measured
Git body catalog bytes, hashes of 57 measured Python helpers, canonical catalog,
worker/audit JSON and API/log/archive provenance. Fetch the exact measured Git
commit, retain `workers/` and `audit/` under one artifact root, and reproduce:

```sh
python3 -B scripts/generate_go_shard_hints.py \
  --request scripts/go_shard_hint_sources.json \
  --artifacts-root /path/to/verified-artifacts \
  --output /tmp/reproduced-go-shard-hints.json
```

Use the recorded measured helper bytes if a future generator contract differs.
Never alter receipts or weaken live schema/context checks to import older data.
Two independent generations of this request must reproduce identical bytes.

Measured worker totals were 164.330, 176.164, 141.772, 170.516, 146.509,
255.491, 203.196 and 220.431 seconds, before additional job setup/upload costs.
CLI and ordinary groups overlap; selected-parent lifecycle intervals include
parallel descendant waits and lazy fixture work. Their sums are scheduling advice,
not wall forecasts. The 96-row catalog fixture can repeat in each package process.
The CLI parent
`TestBoundedArchiveScopeContinuationsAreIndependentAndSelectionPinned` alone
reported 93.16 seconds on worker 7, exceeding the 90 second investigation threshold.
That indivisible parent, repeated fixture/bootstrap costs, long ordinary groups
and queues remain measured obstacles to 150 seconds. Further optimization needs
its own bounded evidence and review; this refresh does not add workers, change
CPU allocation, defer tests or shorten coverage/deadlines.

## Lane-aware collector trial

Catalog schema 3 identifies `independent-lanes-lpt-v1`. The existing generic LPT
helper ranks CLI and ordinary units separately, then pairs higher ordinary
advice with lower CLI advice. Sparse lanes may have empty bins, but the combined
worker catalog remains nonempty, exhaustive and disjoint. Initial fixed-cost
advice is included once on the ordinary side. Stale/absent hints cannot remove
units. The initial trial used 1000 ms collector defaults; its exact hosted compiler
listing now establishes 414 Test/Fuzz/Example parents, and the verified lifecycle
refresh above replaces those defaults. Future unknown parents still use 1000 ms.

Collector is the only added selected package; reader remains a whole-package
obligation. Every selected parent retains all runtime descendants. Per-worker
package grouping still runs TestMain/lazy fixture work once per package process,
including the real 96-row catalog seed whenever a selected parent needs it.
Ordinary groups stay sequential while CLI overlaps with the existing fixed 2+2
CPU budget; native Git observers remain isolated before overlap. Race/count1,
assertions, named skips/restorations, source/fullcompiler/every-worker vet and
independent plain audit contracts and all test deadlines are unchanged.

Producer, raw-log verifier, aggregate, hint generator and unrestricted comparator
use schema 3 together. Old schema 2 live catalogs are rejected. Historical schema 2
advice retains its pinned source and immutable import provenance; its assignment
is not reused as live coverage. The final candidate still requires a fresh normal eight-worker trial and
the same-source unrestricted reference before accepting the new algorithm. The
initial trial supplies the verified compiled/body logs for the hint refresh; its
old successful checks cannot qualify the later hint/base integration. Compare
actual lane/group/bootstrap/listing/restore/upload/aggregate durations and total
runner-minutes after verified warm compilation. Advisory balance alone is not
150-second success; CPU oversubscription and further test deferral are outside
this trial.


## Bounded manual native smoke references

`Extended` has an explicit `native-smoke-reference` choice, excluded from `all`,
normal PR/push runs and schedules. It uses one standard `macos-14` host per
manual dispatch and the same native producer/verifier. Inputs `expected_source`,
`native_smoke_mode` (`normal`/`sensitive`) and `native_smoke_cache`
(`cold`/`trusted-seed`) must identify the exact workflow/checkout SHA and cohort.
A moved ref fails; do not override `GITHUB_SHA` or forge a PR tier plan.

Run the four cases sequentially after the trusted main seed exists, using the
same immutable source. Cold proves both owned caches empty before the normal
module download/verification prelude and performs no Actions restore. A trusted
seed request requires the composite's **final verified** state `trusted`;
`miss` and `corrupt-rebuilt` fail warm qualification before bodies. Each job
starts independently; it does not inherit a previously executed cohort's build
objects. The installed toolchain/SDK are standard hosted provisioning, so cold
here describes product compilation/module caches, not physically cold hardware.

Ordinary PR native artifacts now retain `cache-observation.json` from the same
final restore-state output, bound to source, module identity and canonical
compiler/SDK context. A service download/cache hit alone does not prove trusted
reuse, nor does a verified seed imply every current-source compile action hits.

The manual artifact name includes mode, cache request and attempt, with seven-day
retention. Its `reference.json` is explicitly outside required PR gates and p95
traffic. `producer_seconds` includes native source/compiler guards and evidence
verification; phase durations include their compilation. Setup/module/cache and
upload costs remain visible in workflow job/step timestamps. Manual workflow
wall time lacks the PR tier-plan/dependency path and must not stand in for full
PR-to-green latency.

After collecting four actual artifacts, use:

```sh
python3 -B scripts/native_smoke_reference.py compare \
  --reference cold-normal/reference.json --reference cold-sensitive/reference.json \
  --reference trusted-normal/reference.json --reference trusted-sensitive/reference.json \
  --output native-comparison.json
```

This metadata comparison requires all four successful distinct cohorts and exact
source/tree/full canonical context equality, including runner image. Different
images or sources remain separate unqualified comparisons. It cannot replace
raw native body verification, required CI or ordinary-traffic tail measurements.
Keep manual head and PR synthetic merge identities distinct; any tree-equivalence
claim requires independent Git evidence and does not relabel their source SHAs.

## Initial hosted observations and remaining qualification

Normal PR Test 38670278594 and Levenshtein 38670278618 succeeded on bb84/tree af4e
with all 28 production jobs. Actual allocated execution was 52.100 runner-minutes,
versus 54.183333 for the refreshed baseline (3.845% lower). This single source/context
observation is not a matched causal saving or a steady-state guarantee. It does
not breach the 20% growth investigation threshold. Its run-created-to-required-
completion lower bound was 1479 seconds, including long admission/dependency waits;
the initiating event timestamp and qualified global accelerator state are unknown.
The 150 second goal remains unmet. Short workers 173–290 seconds, native smoke 161,
lint 166 and shared verify 186 seconds also exceed 150 without queueing.

The repaired manual checker experiment 38670998722 on immutable 98fe/tree af4e
completed all six fresh checks at concurrency 4/2/2/4 in 35.884/36.705/37.482/
38.665 seconds after a 109.871 second warmup. Jobs 2 median 37.094 versus jobs 4 median
37.274 seconds differs by 0.48%, below observed ordering variation. Production
**jobs 4 remains unchanged**. These variants have identical verified compilation
copies, fresh result/tool/XDG state and current scans; their extra target Go/vet
warming differs from the normal trusted seed. No normal-PR speedup follows from
comparing those timings, and two samples per setting do not establish a p95.

Keep one-off and postmerge overhead visible: native manual qualification 12.783
runner-minutes; failed checker attempt 9.283333 and repaired attempt 10.716667
(total 20.000000); first cold three-main-seed campaign 10.433333 minutes and
996186513 compressed upload bytes; later mixed seed campaign 9.383333 minutes and
516064567 compressed upload bytes plus 479992807 restored bytes. These are separate
from the normal 52.100 minutes. Raw payload inventories differ from compressed
transfer/storage bytes; exact-worker raw size is unknown in the available logs.

Recurring arithmetic is PR minutes plus main-seed campaigns per PR times seed
minutes per campaign. Illustratively, one cold seed campaign per PR gives
62.533333 minutes (+15.41% versus baseline); one observed mixed campaign gives
61.483333 minutes (+13.47%). Actual main-pushes-per-PR, new context churn and
trusted-hit seed frequency must be measured; all-warm seed campaign cost is
unknown. These scenarios do not accept increased spend or hide manual oneoffs.
Standard runners and the 10 GB cache limit remain unchanged.

Final source integration/hints still require independent review, current CI and
an exact same-source unrestricted reference. Qualification then needs at least
20 unique comparable normal-traffic observations with actual event timestamps,
final job inventory, queue/setup/restore/upload/aggregate costs and distinct
platform/fork/source/dependency/cold/trusted cohorts. Manual experiments and
historical source checks cannot satisfy that gate.

## Verified cache payload observations

`ci_cache.py verify` logs one `compilation-cache-observation:` JSON record only
after final module/shared-module verification and source/compiler guards. Its
`raw_payload_bytes` is the accepted manifest's already-validated uncompressed
payload inventory, excluding manifest metadata. A miss, corrupt restore or cold
fallback reports null bytes; discarded trusted bytes never remain in the final
observation. `complete` logs the existing captured size as `ready-for-save` after
its final guard, or null as `not-captured` when publication is declined. Ready
means prepared for the later cache service save, not confirmation of an upload.
These records add no cache scan, hash pass, compiler probe or Go command.

Keep raw payload bytes separate from compressed archive transfer/storage bytes
and API cache inventories. A key hit alone does not prove that expensive test,
vet or audit compilation was avoided. Report the three trusted-main seed jobs'
actual runner-minutes and measured payload/transfer/storage bytes separately
from PR Test+Levenshtein usage. Disclose amortization assumptions, including main
pushes per PR and new-context cold versus trusted-hit seed runs. Do not hide
postmerge seed overhead inside claimed PR savings or infer billed bytes from raw
payload size. Existing standard-runner and10 GB cache policies remain unchanged.

Runner-minute reports distinguish a job's API timestamp interval from runner
allocation. An explicitly unallocated cancelled/skipped job (`runner_id: 0`,
empty runner name and no steps, or explicit null allocation on a skipped job)
contributes zero runner seconds even when the API supplies cancellation placeholder
timestamps; valid intervals stay visible as job `seconds`. Invalid or missing
unallocated intervals become null diagnostics with the original `started_at`
and `completed_at` retained. Optional skipped placeholders do not invalidate an
otherwise qualifying sample. Allocated or unknown jobs still reject malformed
or backwards timestamps. Allocated cancelled/failed jobs retain elapsed usage.
Unknown allocation or missing allocated-job timestamps makes the total null and
`runner_minutes_complete` false; `known_runner_minutes` is only the observed
subtotal. Older successful snapshots without allocation metadata retain an
explicit `legacy-success-timestamps` estimate. Required unsuccessful jobs still
exclude the observation from latency qualification.

## Ancillary runner demand

The Git compatibility job uses one runner and one verified compilation restore
for three sequential versions: exact upstream 2.30.2, exact upstream 2.39.2 and
the runner's original Git. Every version executes both original count-one test
commands with GOMAXPROCS=2 and Go package parallelism 2, in separate test
processes, PATH/install/configuration/temp directories. A version failure does
not suppress the other versions. The `git-identity-versions` artifact retains
all three receipts and raw logs for seven days. Installations, HOME/XDG state
and test temporary files live outside that artifact root and are removed after
each version's process group stops; they are not retained diagnostics. A single
green job without complete version evidence is insufficient qualification. The
normal production inventory has 24 jobs rather than the prior 28 (Git consolidation removes two;
local planning and final script aggregation each remove one), with version coverage unchanged.

In normal run 38682572380, the earlier three jobs allocated 112 seconds in
aggregate, including 39 seconds in their test steps. Sharing setup should reduce
runner demand and duplicated work, but a 60–75 second combined warm job remains
a planning estimate until the new topology is measured. Separate cold pinned
Git compilation from verified cache restores. Do not compare a historical
three-job cohort with the new one as if only elapsed test work changed.

The Linux final gate still validates every worker's exact source, full compiler
context, vet receipt, independent source audit, exhaustive raw body evidence and
focused/full tier evidence. It does not install Go: those validators do not
execute Go or compare the aggregate machine's unrelated compiler context.
Both validators were replayed against actual source-bound f69a1bc artifacts from
run 38682572380 with Go blocked. They passed, while compiler/receipt/source/
command/terminal/missing-evidence tampering failed. This establishes removal of
unused installation; it does not qualify a later source revision or predict the
entire final gate's speedup.

Mac smoke keeps its current build/discovery/package/CLI/sensitive phase timing,
serial native resource isolation and all assertions. Profile those phases before
changing them. The three cross-build lanes retain all targets and release
binary validation: their observed body steps alone totaled 166 seconds, so
serializing all three would not support the 150-second objective.

The `linux-race` and `cross-build` job gates use
`${{ !cancelled() }}`: prerequisite failures/skips still reach their fail-closed
verdicts, but superseded workflows can release runners and their per-PR
concurrency group. Step-level evidence uploads and cleanup retain `always()`.
A canceled run never satisfies required CI. During this campaign, Test run
38689881223 explicitly waited for superseded run 38689498661, whose
`always()` gates continued after cancellation. This is an observed dependency
on cancellation completion, not a measured runner-queue duration or quantified
latency saving. See GitHub's
[status-function guidance](https://docs.github.com/en/actions/reference/workflows-and-actions/expressions#always).
## Throughput and contention

The reporter includes every saved job in Test and Levenshtein, including builds,
script lanes, audits and aggregate jobs outside the required inventory. Do not
report the test-shard count as the pipeline's total Linux demand. Explicit Linux
or Ubuntu runner labels identify Linux; unknown labels leave Linux totals and
peaks incomplete. `linux_runner_minutes` is allocated elapsed usage, not an
invoice. `peak_simultaneous_linux_jobs` counts overlapping allocated intervals
across both workflows, with interval ends before starts at the same instant.
Legacy timestamp-only successes can estimate usage but cannot establish actual
allocation peaks. Partial inventories publish known subtotals and null totals.

Job/run/repository IDs, attempts, source, allocation evidence and raw step timing
identity remain in output. `resources_across_snapshots` deduplicates repeated
physical allocations across observations; individual sample totals still describe
that observation. A targeted rerun can give a carried-forward successful job a
new API ID. Deduplication of those aliases requires the same run, source, job
name, positive runner identity, exact interval, labels and step evidence, plus a
later attempt created after the allocation completed. Both records need API job
IDs and a timed successful executed step within the allocation interval. Alias IDs remain visible.
A runner ID alone never establishes an alias. Conflicting snapshots for the same
API job identity fail reporting. Inconsistent job/run/source/workflow/attempt or
repository identities make inventories incomplete, so authoritative resource
and batch completion totals remain null. Raw job identities and their enclosing
snapshot identities remain separately visible; an inconsistent run ID is never
rewritten to create a false physical-allocation alias. Live workflows cannot
establish complete inventories because more jobs can still appear. Malformed
raw job/step timestamps remain visible with `timestamp_issues`; invalid job
intervals supply no usage or required-completion timestamp, and malformed timing
evidence cannot establish carried-forward allocation aliases. Explicit evidence
of no allocation still contributes zero; null runner fields on canceled jobs
remain unknown. Valid job allocation intervals can supply known usage even when
step or creation timestamps are malformed. Collect **all attempt views**, including failed
and canceled attempts, to account for total campaign usage; the latest view alone
can omit work. Older carried-forward job attempts are allowed; future attempts
are invalid. Auxiliary campaigns such as compilation seeds can use
`scope: "auxiliary-campaign"` and their own explicit workflow inventories; they
report usage but cannot qualify production PR latency.

`dependency_wait_seconds` and `eligible_to_start_seconds` are null by default.
REST job creation/start timestamps do not establish scheduler readiness. Only
independently retained telemetry can populate the optional sample field:

```json
"job_readiness": {
  "123456": {
    "source": "retained scheduler receipt path or identifier",
    "dependencies_ready_at": "2026-10-14T00:00:04Z",
    "eligible_at": "2026-10-14T00:00:06Z"
  }
}
```

The key is the API job ID. Dependency wait is workflow creation to dependency
readiness; eligible-to-start is eligibility to runner start. They need not add up
to workflow-created start delay: other orchestration can intervene. Either can
remain unknown. Preserve original telemetry, its collection method and meaning;
do not invent eligibility by subtracting preceding job intervals. Even observed
eligible-to-start time is not proof that capacity alone caused the delay.

Add a top-level `batches` array to compare simultaneous PR observations:

```json
"batches": [{"id": "two-active-prs", "sample_ids": ["pr-one", "pr-two"]}]
```

Batch output reports first initiating event to last required completion, total
physical usage and Linux peak across the selected full pipelines. When an event
timestamp is unavailable it instead publishes a workflow-created batch lower
bound. Arrival spacing is included in the span. Reusing a workflow run in two
batch PR observations is rejected. Missing expected workflow inventories leave
batch completion unknown, even if all supplied jobs succeeded. Preserve per-PR
turnaround alongside batch span; neither alone demonstrates causal savings from a topology change.

## Bounded manual four/eight-worker qualification

`Linux worker qualification` is a **shard-topology-only diagnostic**. It is not a
PR-equivalent campaign, omits the Mac/build/script/shared-verification lanes, and
never replaces Test or Levenshtein checks. Production remains eight workers.
The workflow is manual only, uses standard Linux runners, read-only permissions,
restore-only verified trusted compilation seeds and seven-day artifacts. Each
run has an independent concurrency group; it cannot cancel another PR or
qualification run. Existing production cancellation remains scoped by event and
PR/ref so a superseded revision does not cancel unrelated PRs.

Select a repository branch containing the workflow, provide its full current
commit as `expected_source`, and choose `workers: '4'` or `'8'`. Plan and every
role reject a different event/repository, checkout SHA or workflow-definition
SHA. The plan emits only a validated four/eight matrix. Never pass a mutable
source ref as the expected SHA. Workers execute the existing discovered
race-short bodies with the same per-runner CPU policy, assertions, scales,
seeds, count and deadlines. An independent full-source audit owns plain vet;
reconciliation requires every role and exhaustive current-source worker union,
raw body evidence and compiler/vet receipts. No caches or test verdicts are
published. The guarded manual audit calls the existing audit body directly and
retains the real manual event; it does not downgrade the general manual suite
CLI's full-tier policy.

For an initial comparison, run one four-worker and one eight-worker diagnostic
**sequentially on the same immutable source and toolchain**. Retain cache-state,
runner image, command/source/coverage receipts and all job snapshots. Alternate
ordering if a repeat is justified. A bounded overlap experiment can run two
same-configuration diagnostics together, after reserving an otherwise quiet
experiment window; do not launch multiple expensive campaigns alongside normal
implementation qualification. Report experiment allocation separately. No twenty
synthetic runs are needed for a percentile chart.

Diagnostic timing input uses `scope: "linux-shard-topology-only"`, `worker_count`
4 or 8, and an explicit `Linux worker qualification` inventory (`plan`, all exact
API worker names, `audit`, `reconcile`). Resource totals can be complete, but
these samples cannot qualify production latency or p95. Compare the existing
same-source unrestricted reference with both worker sets using
`run_pr_reference.py compare` when establishing coverage equivalence; reuse a
reference only when its exact product source and compiler contract match.

A four/eight production choice additionally requires **full Test plus
Levenshtein** comparison under overlapping PR activity. Shard-only speedups or
peak counts cannot establish full-PR batch turnaround. Prefer the smallest
configuration that meets the agreed throughput/resource tradeoff; do not claim
four halves usage or eight halves latency. Review recurring seed cost separately
and preserve unknown event/cache states. Validate a chosen production policy
against at least twenty comparable normal observations rather than creating
those observations artificially.


## Full PR topology and contention qualification

Ordinary PRs retain **eight** Linux test workers. Four workers are an experiment,
not an adopted cost saving. Both configurations execute the same complete-parent
race/count/CPU/deadline policy, owned plain vet, independent source audit and
exhaustive raw-evidence reconciliation. Ancillary Test and Levenshtein lanes remain.

For same-repository PRs only, a captured head branch of
`ci-qualification/workers-4/<campaign>/<replicate>` selects four; the equivalent
`workers-8` branch explicitly selects eight. Campaign/replicate components contain
1–40 ASCII letters/digits/hyphens, beginning with a letter/digit. Matching is case
insensitive, agreeing with GitHub expressions. A malformed reserved same-repository
branch fails closed. A fork branch cannot select an override. Labels are descriptive
only; no `labeled`/`unlabeled` triggers or mutable API metadata controls workers.

The fixed matrix, worker cache count/argv and aggregate count are guarded against
the same captured event policy. After local-plan integration the full expected
Test plus Levenshtein inventory is 24 jobs with eight workers and 20 with four
(including two Mac jobs, so Linux inventory is 22/18);
Git compatibility remains all three version receipts inside one job. Guard receipts record real PR/head/base/ordered
merge parents/tree/workflow identities. Different merge commits remain different
SHAs even if independent Git evidence establishes identical trees. Every topology
must reconcile every worker plus current source audit; `linux-race` stays the
required stable verdict. No extra planning runner or global PR serialization is
introduced. Supersession cancellation is still scoped to each PR, so four
qualification PRs sharing a head commit do not cancel one another.

After component integration, fresh independent review and meaningful local checks,
the initial bounded experiment is the substantive implementation PR as one
8-worker member, one 8-worker qualification sibling, and two 4-worker siblings.
Confirm the remote actually supports these distinct head branches/PR identities.
A root-owned fixed qualification base branch may point to the implementation
PR first observed base B0 for the three siblings. Never advance or merge that
base branch; actual implementation PR still targets main. Preserve the different
base ref names as real metadata. Keep head commit, base and full tested tree equal where Git permits; retain actual
workflow/source SHAs, compiler/image/cache cohorts and all raw coverage evidence.
Begin each same-configuration pair close together, wait for both full pipelines
before the next configuration, and report actual arrival spacing/background
traffic. A completed nonoverlapping run is not a simultaneous batch member.
The implementation PR's own exact candidate/checks alone establish its readiness.
The existing revive lint command keeps its policy and now records its actual
resolved `origin/$BASE_REF` SHA and merge-base endpoints before execution. Require
identical selected diff endpoints, tested trees and policy across the comparison;
a moving main ref can differ from the fixed sibling base. If that invalidates
the implementation PR as an 8-worker member, mark the comparison inconclusive,
retain validated eight and stop; do not automatically purchase more full runs.
Root may later authorize a justified bounded follow-up with actual evidence/cost.
Cache branch visibility/hits are separate cohorts, unknown until observed.
Close every qualification-only sibling **after terminal evidence collection and
before merging the implementation PR**, so a shared head does not cause GitHub to
mark those experiment PRs merged automatically. Never merge an experiment sibling.

The initial budget is four complete full pipelines total, three incremental ones.
The earlier 41.88 Linux runner-minute observation implies approximately 125.64
incremental Linux runner-minutes plus Mac, solely as a planning envelope before
these optimizations. Record actual all-attempt allocation and Mac/manual/seed costs
separately. Do not run a redundant shard-only campaign when full audited runs supply
that evidence. A failed guard/code check stops the experiment until repaired and
reviewed. Reverse-order repeats require a new recorded budget justified by initial
results, not automatic retries. Two batches cannot establish p95 or a causal queue
saving. Keep production eight when full-pipeline resource/turnaround comparisons
are inconclusive; a default change requires new review and current CI.

## Partial live summaries and complete offline collection

The existing final Linux aggregate emits a bounded **partial** advisory summary
using `collect_ci_timing.py`. Only that job gets `actions: read` in addition to
contents read. It makes GET requests solely to `api.github.com` for this repository,
rejects redirects, caps pages/requests/response size and uses short network
timeouts. The diagnostic step has its own one-minute upper bound and runs only
after successful authoritative checks; its failure cannot override their verdict.
There is no extra reporting runner, privileged `workflow_run`, artifact execution,
new scheduler, secret or write permission. Restricted fork tokens may make this
snapshot unavailable without weakening any test.

The current collector job has no final allocation end while it is executing, and
the paired workflow may be pending. Summary values are known allocation subtotals
and observed peaks, not final totals. Whole-pipeline completion, own final usage,
source/cache cohort and event timestamp remain unknown unless actually evidenced.
The live snapshot never qualifies p95. Its JSON includes collection time, raw API
snapshots and topology provenance; the step summary exposes the known subtotals.
`unknown` source cohort and unknown cache state are explicitly nonqualifying.

Finalize actual completed normal runs with explicit paired run IDs and a retained
source-verified topology receipt from the guard logs:

```sh
GH_TOKEN="$READ_ONLY_ACTIONS_TOKEN" python3 -B scripts/collect_ci_timing.py \
  --topology /path/to/worker-topology.json \
  --test-run 123 --levenshtein-run 124 \
  --cohort source-only --cache-state mixed \
  --output /path/to/pr-observation.json
```

The token is read from the environment, never an argument or diagnostic value.
This command provisions no runners. The adapter reuses `report_ci_timing.py`,
retains every earlier attempt as auxiliary usage, paginates completely and preserves
strict physical-allocation deduplication. Pairing checks repository, workflow path,
event, PR number, head repository/branch/SHA and base SHA: sharing a head SHA is not
sufficient. Missing PR association without authenticated retained identity, or ambiguous live peers, remains unavailable;
explicit offline run IDs still must satisfy the exact context. Missing expected
jobs cannot disappear by deriving inventory from the measured run.

Use `--experiment` on the implementation PR when it participates in the deliberate
batch; reserved-prefix receipts already identify experiment siblings. Intentional
experiments are excluded from normal-traffic percentile qualification. `--event-at`
is permitted only with retained initiating-event telemetry. For a captured genuine
opened event, use PR creation only after proving it is that exact initiating event;
never substitute it for synchronization/reopen events. Otherwise omit the argument:
the terminal report retains only run-created lower-bound latency. Do not fabricate
normal traffic to fill twenty observations. At least twenty unique comparable
actual normal observations with adequate event/cache/source evidence are needed
before the existing reporter can publish a provisional p95.

This work makes no hosted elapsed-time claim from the separate model-family lookup
optimization: it reduced isolated lookup CPU work, while both local elapsed pairs
were slower. Four workers, 150 seconds and steady-state savings remain measurement
decisions, not assumptions embedded in this control.


Consumers resolve their own deterministic plan using the existing planner,
full Git history and exact event/source proof immediately after checkout. PR
planning refuses missing-history/dirty-source fallback before tests execute.
The standalone `tier-plan` job is removed: Mac, full extras and both purge
workers can become eligible without waiting for a separate Linux planner.
`linux-full` still publishes `ci-tier-plan`; final verification independently
recomputes and compares it, checks Linux extras and binds native extras to the
same plan. Purge receipts must match the independently selected tier and
complete method/case union.

This makes five local planner invocations/full-history checkouts instead of one
standalone planner, adding repeated Git/selection work. Earlier full/purge
eligibility can also increase the initial ready burst by two Linux roles net
of the removed planner (one full plus two purge workers, minus one planner);
For the current eight-shard PR topology, initially eligible Test Linux roles
increase from 16 to 18; including the two shared-verifier Linux roles gives
18 to 20. These are static eligibility counts, not observed simultaneous
allocations. Mac eligibility moves earlier in its separate pool. Measure full-pipeline peak,
batch turnaround and allocated minutes before claiming a queue or cost benefit.
No shard waits on a planner, and no capacity or test cadence increases.


### Qualification caveat after local plan integration

Before final script-verdict consolidation, removing the standalone tier planner
reduced total full-pipeline jobs from 26 to 25 at eight workers, but statically initially eligible Linux Test jobs increase
from 16 to 18, or 18 to 20 including Levenshtein. That intermediate revision used six actual consumer plan
invocations and full-history fetches to replace one planner allocation; purge has
two physical consumers. Current event/source guards and aggregate canonical,
focused, native and purge proof checks remain. Lower total job count is not
proof of a lower initial burst, fewer runner-minutes, or less queueing. Record
these costs and actual overlap in the full-pipeline comparison before adopting
four or claiming throughput improvement.


### Captured base versus actual tested merge base

A normal PR may be tested by GitHub on a newer descendant of the base SHA retained
in its triggering event. This is accepted only with exactly two actual merge
parents, the second equal to the captured head, and real full-history Git proof
that `event_base` is an ancestor of the actual first parent `tested_base`. Every
receipt retains these two distinct values plus actual source/tree/ordered parents;
it never rewrites the captured event base. Unrelated/reversed/wrong-head parents,
missing commits or shallow history fail the guard. Source-audit now explicitly
fetches full history for that proof, an additional fetch cost to include in usage.

The collector's API PR association base is checked against `event_base`, not
silently against `parents[0]`. The aggregate's independent plan and native/cache
source guards similarly retain the authentic identities. Accepting this ordinary
GitHub base race does not qualify differing campaign content: the four/eight
experiment still independently requires comparable actual tested trees, policies,
compiler/cache/image cohorts and runtime lint selected diff endpoints. Different
source SHAs remain different. The main integration also preserves new product
and test bodies; older coverage or cost artifacts are not relabeled for this tree.
Tier-plan provenance retains `base` as the recorded PR-event base and
`tested_base` as the actual first parent of the tested merge. GitHub may build
against a newer descendant of the event base. Planning requires exactly two
parents, the exact event head second, and proof that the event base is an
ancestor of `tested_base`, with complete history and unchanged tracked source.
Path selection still conservatively compares the recorded event base to both
the PR head and tested source, including base advances. Candidate builders and
throughput qualification must use actual merge parents and keep recorded event
identity separate; forged, reversed or unrelated parent identities fail closed.


The required `linux-race` job directly requires static scripts, installer scripts
and both purge workers, alongside its existing Go/native/audit prerequisites.
It runs the unchanged purge artifact download and current-source method/case
union verifier before its Go evidence checks. The standalone `script-tests`
job is removed, eliminating a second aggregate admission and duplicate local
plan/history checkout while keeping every script body and purge receipt check.
Initial ready-role counts stay unchanged by this consolidation; it removes a
later dependent job, not a producer. Measure current CI before attributing a
specific latency saving to this scheduling change.


### Current final-gate inventory

The standalone unrequired `script-tests` verdict runner is removed; final
`linux-race` directly requires static/install/purge roles and retains the complete,
unconditional current-source purge method/case union before Go evidence checks.
The five existing local planner consumers (including two physical purge jobs)
retain canonical/native/extra source proof. Required check names and all bodies
remain intact. Full inventories are 24 jobs at eight workers / 20 at four, with
two Mac jobs included (22/18 Linux). This removes one allocation/history fetch
and sequential verdict boundary; initially eligible Linux jobs remain 20/16
including Levenshtein, so no reduced initial burst or measured queue benefit is
claimed. Actual source-bound runtime, cache cohorts and full-pipeline comparison
remain necessary; prior 28-job source evidence cannot certify this new tree.

## Durable per-run identity and performance history

The existing final linux-race aggregate and shared verify jobs now retain a small
`ci-run-identity-{Test|Levenshtein}-{attempt}` artifact. Its sole `identity.json`
member binds the real event, repository, PR, head/base refs, captured base, tested
merge source/tree/ordered parents, workflow source/path, run ID and attempt.
The shared workflow validates its own workflow path, not Test's path. Production
worker count, schedules, test selection and token permissions are unchanged.
Artifacts keep the existing seven-day retention. To preserve observations longer,
collect their small receipts during that window; this change adds no scheduler.

GitHub can remove `pull_requests` associations from completed runs after merge.
Explicit run IDs alone still cannot authorize pairing an empty-association run.
Supply its original downloaded identity ZIP using:

```sh
python3 -B scripts/collect_ci_timing.py \
  --topology worker-topology.json --test-run 123 --levenshtein-run 124 \
  --identity-archive 456:/path/to/test-identity.zip \
  --identity-archive 457:/path/to/shared-identity.zip \
  --output observation.json
```

Use the same environment token described above. The collector fetches metadata
for each artifact ID from the fixed repository API, checks its owning run,
expected name/attempt, expiry and SHA-256 digest against the exact ZIP bytes,
then compares the complete retained identity. It independently resolves the
actual merge commit/tree/parents through the Git API, and proves ancestry when
the tested base advanced beyond the captured base. Missing, expired, mismatched,
wrong-attempt or modified receipts fail closed. Existing nonempty contradictory
PR associations cannot be overridden by a receipt. Preserve earlier-attempt ZIPs
as well when those API associations are absent. There is no fallback for older
runs that did not retain authenticated identity. Identical trees do not imply
identical tested source SHAs.

Normalize existing producer evidence without running tests:

```sh
python3 -B scripts/normalize_ci_timing.py --worker go-shard-0 \
  --topology worker-topology.json --output worker-timing.json
python3 -B scripts/normalize_ci_timing.py --native native-full-tier \
  --output native-timing.json
python3 -B scripts/ci_timing_history.py --observation worker-timing.json \
  --pipeline-observation observation.json --output baseline-candidate.json
```

`worker-timing` schema 1 has exact `source`, `run_id`, `run_attempt`, `shard`,
`compiled_inventory_sha256`, `configuration_sha256`, `cohort`, `success`,
`observation_kind`, `phases`, `tests` and `fixture_metrics`. Test rows identify
`package::TopLevelTest`, family/package, lifecycle `parent_wall_seconds`, Go
`own_elapsed_seconds`, terminal status and timing issues. Setup remains
`unattributed`: the normalizer never subtracts overlapping fixture measurements
or calls Go Elapsed CPU time. Fixture output uses `CI_FIXTURE_METRIC_V1` JSON:
version 1, fixture, phase, elapsed_ns and a dictionary of nonnegative operation
counts. Each emitted sample retains its exact child test and top-level owner.
These metrics may overlap across tests and are not additive.

Shared verification retains `shared-verification-timing` schema 1 from its same
fresh invocation, including total elapsed and per-check measured duration,
execution duration when reported, status and cache state. Checks overlap; their
sum is not wall time. Raw checker stderr, scanner output, staged source and
caches are not copied into the artifact. The command's failure remains a
failure; malformed/missing timing output fails rather than claiming zero.

The bounded history helper groups comparable compiled-inventory/configuration/cache/image cohorts,
deduplicates physical workers, rejects conflicting duplicate observations and
requires matching terminal full-pipeline collector evidence and excludes failed, experimental, unknown-classification or unknown-cohort samples
from baseline candidates. It preserves measured medians and sample counts, not
inferred percentiles. Its output is explicitly a **review candidate**, never
an automatically promoted allowance. A single worker receipt cannot certify
whole-pipeline success; the candidate builder recomputes the paired snapshot report
and requires the complete fixed production inventory and successful workflows.
Candidate eligibility revalidates the paired workflow association and canonical
run/job identities. Every required job needs a positive API job/runner ID, a
nonblank runner name and successful executed-step evidence within its allocation.
Legacy or unknown allocations remain diagnostic. Each shard must match a
successful physical job's actual attempt, including
successful jobs carried forward into a later workflow attempt. Blank or missing
cache/image cohorts cannot supply a candidate.
The required aggregate must also reconcile coverage before any policy uses it. The future required performance
gate owns approval and trusted-main promotion. Queue eligibility remains unknown
without independent telemetry, and runner allocation remains accounted by the
existing full-pipeline reporter rather than by summing test durations.

## Required test-cost gate

The existing required `linux-race` aggregate runs the performance gate after
source/coverage reconciliation. It adds no worker and executes no Go. For a PR,
it extracts the enforcer, imports, and policy from the tested merge's **first
parent**, rather than executing the PR's proposed policy. Removing or changing
that policy in a PR therefore cannot authorize that PR's increased allowance.
The sole installation bootstrap is a tested first parent without an enforcer.
Once main contains the enforcer, absent/invalid policy or missing evidence fails.
There is no disabled or permanent warning-only mode.

`ci_performance_policy.json` contains a reviewed individual top-level-parent
inventory, including tests in whole-package shard units. This inventory is
separate from measured baseline timings: existing tests with fewer than three
verified comparable observations are explicitly reported as insufficiently
calibrated, not assigned invented historical durations. Comparison cohorts bind
compiler/settings, runner image, and cache state. Different/unknown cohorts do
not manufacture a regression comparison. Exact compiled inventories remain
source-bound admission evidence, while comparison keys use configuration and
cohort: adding a new parent does not erase the measured history of surviving
parents. New identities receive only the new-test rules. A previously skipped
parent with no actual admitted measurement also receives the new-cost time and
family caps when it first passes. Continuing legitimate skips remain unmeasured;
one or two real observations retain the insufficient-calibration rule.

A new parent warns above 10 seconds and fails above 30 seconds. Measured cost is
the greater of its own terminal Go Elapsed and its slowest descendant's terminal
Elapsed. Active lifecycle wall is reported separately because parallel slot
contention and descendant waiting can inflate it. A new parallel family with
more than 100 parallel descendants or more than 30 seconds of summed parallel
terminal Elapsed also requires an explicit family budget. That sum is a
**nonadditive review indicator**, not CPU time or end-to-end elapsed time; nested
parallel intervals can overlap. Small cheap parallel tests need no annotation.
Existing tests fail when their comparable measured cost exceeds both 1.5 times
the baseline median and the median plus 10 seconds. Baselines require at least
three eligible observations before this relative rule activates. These rules do
not promise statistical confidence from three observations; they are reviewed
admission policy, with source-bound evidence and an explicit override.

Deterministic `work_budgets` bind a test, fixture, phase, and operation counter.
Missing configured parents, required phases or counters, and excess work fail
independently of time. Repeated metrics for that exact phase are accumulated; parent and child phases are never
summed or subtracted as disjoint durations. `parallel_budgets` can explicitly
bound a family's observed descendant count and elapsed-work indicator; these
explicit limits remain enforced after the family enters the baseline.

The initial policy records Test run `38775545530`, attempt 1, tested source
`412cec705ce4b6c539f18e006f3b5c1a41b69d7a` and its paired successful
Levenshtein run `38775545598`. It contains 5,927 actual parent identities,
including 57 legitimate skips without invented durations, and 5,870 measured
parents split across two configuration cohorts. Each measurement has one
physical observation. The pipeline hash and inventory hash are retained in the
policy. Missing event-time telemetry prevents a queue-eligibility claim; it does
not erase the independently proven successful physical worker observations.

The first fixture-counter producer change must retain its constructor ceilings
and emit its exact test/fixture/phase/operation identities before a reviewed
policy adds corresponding `work_budgets`. That integration must use actual
receipts and preserve missing-counter failure; an empty initial work-budget
list is not evidence that deterministic fixture-growth protection is complete.
Continue reducing repeated setup and checking critical-path and total runner
allocation with comparable ordinary CI evidence. This initial baseline makes
no end-to-end speedup claim and does not authorize extra benchmark campaigns.

### Exceptions and hosting protection

Proposed exceptions live in `scripts/ci_performance_exceptions.json`. Each entry
has exactly one `test_id` or package `family`, `maximum_seconds`, nonempty
`reason` and `owner`, and required ISO-date `expires`. Large parallel-family
exceptions additionally need `maximum_parallel_descendants` and
`maximum_parallel_elapsed_work_seconds`. Overlapping selectors are rejected.
An exception cannot override missing evidence, source/coverage checks, correctness
failures, or deterministic operation budgets.

An exception used by a run requires the latest submitted review by a designated
maintainer to be APPROVED, from a human other than the PR author, with
`review.commit_id` equal to the live/event PR head. Dismissed, stale, bot, author,
and unlisted-user reviews do not authorize it. Pushes invalidate that approval.
Approve the revision, then rerun CI; a rerun or label alone grants nothing. There
is no self-referential head SHA in the exception file. The bounded decision
artifact records the exact source, trusted policy digest, exception and review.
Unsubmitted pending reviews do not replace a submitted decision. Review submission
time determines the latest review, with review ID breaking equal-time ties.
Expired exceptions fail even when unused, so remove them promptly.

The enforcer protects the budget decision **when the required workflow runs**.
CODEOWNERS text alone does not prevent a PR changing the workflow to omit the
enforcer. For example, deleting the performance step in
`.github/workflows/test.yml` can leave the same `linux-race` check name green
without executing the trusted-base runner. A required status name alone cannot
detect that change. Hosting rules must independently protect the workflow/enforcer/policy
and required checks, enforce review by the appropriate maintainers, invalidate
stale approvals, and ensure the validated merge candidate is the candidate
merged. A repository with only one maintainer cannot obtain that maintainer's
non-author approval on their own PR. Do not claim tamper-proof enforcement until
those hosting requirements are configured and verified. An exception remains
blocked when no eligible independent approver exists.

### Reviewed baseline admission

After the normal production pair completes successfully, retain its validated
terminal collector JSON, exact topology, and complete worker artifacts. Prepare
a review candidate without rerunning tests:

```sh
python3 -B scripts/admit_ci_performance_baseline.py \
  --policy scripts/ci_performance_policy.json \
  --topology /path/to/worker-topology.json \
  --pipeline /path/to/terminal-pipeline.json \
  --workers /path/to/shard-evidence \
  --output /path/to/policy-candidate.json
```

Review the original authenticated artifact provenance, source/normal-production
identity, complete compiled/executed inventory, and any allowances before
committing that candidate. The command does not promote it or grant approval.
It records real initial observations without fabricating additional sample size.
Multiple comparable successful observations are required to establish relative
budgets; updating existing baselines must preserve their reviewed sample
provenance. The live gate and admission CLI independently recheck catalog version, exact
source/tree, catalog digest, complete assignment, compiled discovery and selected
raw parent bodies. Ordinary
whole-package receipts do not contain separate individual compiled listings;
review the original authenticated ZIP and digest proof as well. Experimental,
failed, partial, truncated or source-mismatched inputs are not admitted. No
scheduled benchmark campaign is required: use ordinary CI traffic.

For additional comparable normal runs, repeat
`--history-sample TOPOLOGY PIPELINE WORKERS`. Each sample undergoes the same
full-pipeline validation. Physical run/attempt/shard duplicates cannot increase
its observation count, and removed historical parents are excluded from the
current inventory. The candidate retains sample identities and pipeline hashes
so a reviewer can inspect the original evidence. Policy validation rejects absent or
duplicate sample provenance, unknown/empty measurement cohorts, and per-parent
observation counts exceeding the retained physical sample inventory.

## Linux compiled discovery and scheduling advice

The schema-4 parent catalog uses one `go test -race -short -count=1 -json -p 1
-list '^(Test|Fuzz|Example)'` invocation for CLI and discovered heavy packages.
This removes one Go invocation and repeated dependency-graph loading per worker;
it does not reuse a test process across packages, disable default vet, change
TestMain, or cache a test verdict. Package metadata still comes from the separate
race-aware `go list`. The combined command serializes package builds as well as
package execution; cold-cache performance needs observation rather than assuming
that fewer commands always means a faster worker.

`listing-combined.json` is the primary compiled-list receipt. `listing.json` and
`listing-parents.json` are projections of its Go events, not separate invocations.
The coverage verifier requires the primary receipt and exact projected event
streams. The measured phase is `compiled_listing_race_build_testmain_seconds`;
do not synthesize independent CLI/heavy durations or add them to this phase.
Current reconciliation rejects older catalog schemas. The offline timing-hint
importer explicitly accepts schema 3 for existing historical observations.

Each worker manifest includes `hint_diagnostics`: compiled units without a
weight, stale hints, and the unchanged 1000ms unknown-unit fallback. These are
advisory diagnostics, not a test allowlist. New tests and fuzz/example parents
remain in the exhaustive compiled partition. Refresh weights using
`generate_go_shard_hints.py` and verified comparable successful samples; review
the generated provenance and changes rather than copying local macOS timings
into Linux weights. Fixture metrics are non-additive: do not charge a shared
seed's first-caller cost to every test or add it again to parent wall time.
