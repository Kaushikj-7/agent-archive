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
    "comparable_group": "eight-workers-complete-parent-v2",
    "event_at": "2026-10-14T00:00:00Z",
    "required_jobs": {
      "Test": ["tier-plan", "source-audit", "linux-short (0)", "linux-short (1)", "linux-short (2)", "linux-short (3)", "linux-short (4)", "linux-short (5)", "linux-short (6)", "linux-short (7)", "linux-full", "script-tests", "lint", "cross-build", "macos-smoke", "linux-race", "Git identity (runner-current)", "Git identity (upstream-2.30.2)", "Git identity (upstream-2.39.2)"],
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

The version-2 shard catalog discovers compiled Test/Fuzz/Example parents for CLI,
catalog, statshtml and state in the exact race-short configuration. Light packages
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

The initial version-2 qualification used four standard Linux workers; the current
topology uses eight standard Linux workers. Use distinct comparison groups for four/eight workers and
for the earlier whole-package scheduler. Keep prior run inventories unchanged.
The eight-worker inventory must explicitly add `linux-short (4)` through
`linux-short (7)` and its comparison group must change to
`eight-workers-complete-parent-v2`. To roll back concurrency, change matrix,
WORKERS, runner/reconciler arguments and workflow contract tests together to four;
both four and eight are supported by the catalog and cache guards. Keep the
stable `linux-race` aggregate required check. Do not mix schema-less historical
manifests into live reconciliation; only the explicit historical hint-import
interface accepts those old records.

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
