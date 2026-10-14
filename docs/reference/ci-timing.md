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
    "comparable_group": "four-workers-per-worker-vet-v1",
    "event_at": "2026-10-14T00:00:00Z",
    "required_jobs": {
      "Test": ["tier-plan", "source-audit", "linux-short (0)", "linux-short (1)", "linux-short (2)", "linux-short (3)", "linux-full", "script-tests", "lint", "cross-build", "macos-smoke", "linux-race", "Git identity (runner-current)", "Git identity (upstream-2.30.2)", "Git identity (upstream-2.39.2)"],
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
elapsed times include descendants and parallel waits. The report does not
reinterpret these artifacts as a coverage verdict. Cache bytes and tool
installation are measured from inventory/API step data, not inferred from hits.

Ten unique qualifying runs in a comparable cohort permit directional comparison.
Twenty permit a **provisional**, nearest-rank p95. Attempts of the same workflow
run do not increase sample size; all attempts remain visible. A small successful
sample never proves the 150-second goal. Failure frequency, exclusions and reruns
must accompany the latency report. Cold, fork and platform cohorts need their own
qualification; a passing warm source-only cohort says nothing about their p95.

Qualification is pending until normal traffic supplies the necessary observations.
Keep the prior ~47.1 runner-minute snapshot separate from newer per-worker-vet
coverage; it is historical context, not an equivalent performance comparison.
Investigate total runner-minute growth over 20% against a genuinely comparable
baseline. Keep standard runners, seven-day diagnostic artifacts and the existing
10 GB cache capacity policy; reporting must not enable paid capacity.
