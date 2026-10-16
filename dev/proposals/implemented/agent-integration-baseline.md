# Integration performance reference

Production reference: `98f8bd0898abc8f22e4d67d567cf11ae2b88ee2f`.
Original current base: `c85d239f09a3ae963f59e7023153a7a7f3ee7539`.
Integrated main: `12c14b1adc7bfa9ded544ce6e0bc061f97cdf161`; measured
current tree at `ff6d2506fd2512067e24f51e76b4cd5557493bab` plus the recorded
benchmark/lint-only working changes (see `provenance.json`).
Both run on this Linux amd64 host (Xeon Platinum 8573C, Go 1.27.1,
benchmark GOMAXPROCS 5). The baseline uses test-only instrumentation from this
phase: the five new benchmark files and testing.TB fixture helper and expanded `scripts/measure-hook.py`,
not production patches. All content is synthetic, with temporary data homes,
injected repository lookup and fake object storage. Raw results are in
`agent-integration-baseline-data/`. Both references and longer rechecks are committed below. This is an honest
phase-entry baseline; the unresolved historical timing difference means the
proposal’s full relative performance gate is **not** declared resolved.

Live integration target is `0ffa628e84a2b4e6265ffa2ff481788269027567`.
It includes #277's collector file reads through `transcriptio` and extracted
`nativesessions` helpers, plus #280's native handoff discovery, bounded filtered
previews, shared record filtering and cancellable boundary probes. It is not
the measured current reference above.
The native additions entered at `d05defabb8ee1d7f268ee854882ca98dcfc0bde5`;
target `4add976435af4b91d1dfbd31b3c2496316a0d3f4` also includes #257's shared
archived no-prompt handoff selection policy. Latest main additionally releases
unused discovery reservations, checks cancellation during checkout scoping and
preview loading, keeps verified identity-only rows after preview-budget exhaustion,
and refuses automatic latest/current selection when discovery is incomplete.
None of these later targets was timed for this historical reference.
Latest main also includes filter15/adapter0.15.0 pairing-bundle redaction,
informational machine registration with independent publication retries,
bounded portable project matching and revised storage creation/confirmation.
Their upstream production changes and golden fixtures are preserved. Historical
98/ff6 timings and immutable instrumentation still use their recorded versions;
this update neither regenerates raw samples nor claims performance parity for
the newer filter or machine-publication workloads.
The core-production equality claim below applies only to the pinned historical
measurement at `ff6d250`, not to this newer target. Later hot-path comparisons
must keep pinned-98 and recorded-current results and additionally measure the
updated target with matching instrumentation/workload counts; no numbers here
are relabeled as measurements of that target.

The native-handoff target also has distinct workloads: bounded identity discovery,
head/tail preview batches, read-free browser filtering/redraw, and one full filter
for a selected source. Its tests and benchmark are target-specific evidence;
the historical analysis-consumer benchmark does not measure previews, native
discovery or launch-file cleanup. Future affected phases must preserve counted
read/batch/filter budgets and separately measure these real callers on the same
host/toolchain. No new target timing comparison was run for this context update.

## Reproduction

Create a detached checkout at the exact reference above; copy
`internal/capture/hook_bench_test.go`,
`internal/collector/settled_scan_bench_test.go`,
`internal/archive/analysis_consumers_bench_test.go`,
`internal/archive/record_limits_bench_test.go`,
`internal/collector/cursor_scan_bench_test.go`, the testing.TB helper in
`internal/collector/source_test.go`, and
`scripts/measure-hook.py` from recorded snapshot
`394d3c1fa2571c4e7d9673a0319987816bf492bb` into it. For these historical
measurements, take all copied files from that snapshot, not a moving branch:
later harness repairs must not silently replace the recorded instrumentation.
Preserve and record their
SHA-256 hashes with each comparison (matched in `provenance.json`). Run both revisions serially on the same
host/toolchain, outside correctness/lint compilation or other benchmarks:

```sh
export GOMAXPROCS=5
go test -run '^$' -bench 'BenchmarkScan(SettledRegistrations|Large|ChangedCursorChats)' -benchtime=1x -count=3 ./internal/collector
go test -run '^$' -bench BenchmarkHookLocalEffects -benchtime=10x -count=3 ./internal/capture
go test -run '^$' -bench 'BenchmarkAnalysisConsumers|BenchmarkCursorTextFilterFiveMegabytes' -benchtime=10x -count=3 ./internal/archive
go test -run '^$' -bench BenchmarkFilterRecordLimits -benchtime=1x -count=3 ./internal/archive
go test -v -run 'TestLargeRecordMemoryCeiling|TestCursorSQLite|TestParserUpgradeOverAnUnchangedTranscriptDoesNotReadIt|TestParserUpgradeWithNewContentReadsTheTranscriptOnce|TestCursorText.*Limit|TestRecordOverTheLimit' ./internal/archive ./internal/collector
go build -o /absolute/temporary/path/agent-archive ./cmd/agent-archive
TMPDIR=/var/tmp python3 scripts/measure-hook.py /absolute/temporary/path/agent-archive
```

The longer paired recheck commands were:

```sh
go test -run '^$' -bench 'BenchmarkScanChangedCursorChats|BenchmarkScanLargeGrowingSession' -benchtime=5x -count=5 ./internal/collector
go test -run '^$' -bench 'BenchmarkHookLocalEffects/(fresh|stop)$' -benchtime=100x -count=5 ./internal/capture
go test -run '^$' -bench 'BenchmarkFilterRecordLimits/near-limit$' -benchtime=5x -count=5 ./internal/archive
```

Use identical fixed iteration counts for both sides of every pair. Cursor
chat history grows each iteration: the initial chat has one message, and
measured iterations have 2 through N+1 messages per chat (2 at 1x, 2–6 at 5x).
The short and longer Cursor samples therefore measure different workloads;
only baseline/current samples with matching N can be compared. The existing
large-growing-file benchmark also appends one record each iteration, starting
with 2,000 records. Fresh hooks accumulate new registrations; local stop and
subagent benchmarks repeat the same identity and timestamp, characterizing
duplicate/coalesced delivery rather than distinct turns or children. Pin N for
those comparisons too; default time-based Go calibration is not equivalent.

This managed sandbox adds a read-only `.git` sentinel to `/tmp`, causing the
real CLI data-home guard to refuse that location. Subprocess measurements used
an approved isolated `/var/tmp` run. The guard was not patched or disabled.
The subprocess harness creates private HOME/AWS paths, never installs hooks or
contacts storage, and checks that synthetic registration/candidate effects
occur before timing. Subprocess fresh is Claude; local fresh is Codex.
The recorded script checked registration and child candidate effects, but its
stop check was empty. The repaired current script additionally requires an
urgent request containing the stop reason; `python3 scripts/test_measure_hook.py`
checks refusal of silent/deferred stop no-ops, successful measurement and
temporary-home cleanup with fake subprocesses. Historical stop samples remain
latency evidence with that validation limitation; they are not retroactively
claimed to satisfy the stronger guard.

## Paired measurements and attribution

Per-metric medians of three samples (1x collector, 10x analysis/text/hook,
1x near/over-limit). Each baseline and current run was serial after all
correctness/lint/compiler work finished. The first current collector run had
overlapping subprocess work; its raw `collector-current-overlapping.txt` is
retained for transparency and excluded from these comparisons.

| Scenario | 98 reference | Current reference | Current bytes/op | Current allocs/op |
| --- | --- | --- | --- | --- |
| 1k settled registrations | 30.68 ms | 28.19 ms | 5,519,824 | 43,363 |
| 10k settled registrations | 304.04 ms | 290.51 ms | 57,354,216 | 430,767 |
| Large unchanged file with hook request | 246.64 ms | 254.77 ms | 39,735,920 | 194,985 |
| Labels + metadata + transcript + handoff | 49.41 µs | 45.62 µs | 7,704 | 108 |
| Five-MiB Cursor text filter | 270.59 ms | 269.59 ms | 48,124,180 | 61,364 |
| Over 64-MiB record (refused) | 95.82 ms | 93.50 ms | 201,318,448 | 120 |

Short samples moved by >10% for changed Cursor chats, growing files and a
near-limit record. Rechecked serially with 5 iterations/sample and 5 samples
for those cases, and 100 iterations/sample ×5 for fresh/stop local hooks:

| Recheck | 98 reference | Current reference | Runtime delta | Current bytes/op | Current allocs/op |
| --- | --- | --- | --- | --- | --- |
| 100 changed Cursor chats | 338.66 ms | 334.20 ms | -1.3% | 123,136,340 | 204,734 |
| Large growing file | 350.46 ms | 418.32 ms | **+19.4%** | 70,712,556 | 295,906 |
| Near 64-MiB record (accepted) | 195.71 ms | 209.71 ms | +7.2% | 201,304,408 | 208 |
| Codex fresh local effects | 328.38 µs | 289.38 µs | -11.9% | 37,672 | 323 |
| Codex stop local effects | 115.06 µs | 133.43 µs | +16.0% | 22,430 | 189 |

The growing-file historical difference remains unresolved, with stable counted
work and allocations. Production files in archive/collector/state/cursorstore
are identical between the historical reference and the current measurement
(`provenance.json` enumerates differences); capture/config dependencies changed
between them. This does **not** attribute the entire latency movement to those
dependencies or prove noise. Keep both references and all samples; independent
review and the relevant hot-path phase must assess any additional measurement
or justification. Current-reference measurements help isolate extraction costs
but do not replace the proposal’s pinned-98 comparison gate.

Commit `8cd1c5e` (#262), already in the original current base, added the
pre-lock capture-window configuration read and retained the post-lock read.
Local fresh/stop hooks accordingly go from 313/179 to 323/189 allocations;
stop bytes rise 20,198→22,430. That correctness work also lets disabled/paused
hooks exit before taking the lock: 27→15 allocations in the short samples.
These are measured pre-refactor differences, not a reason to remove generation
checks. Phase 0 adds no hook/source/collector behavior or production abstraction;
its inherited clipboard lint fix only makes the existing Unknown return explicit.

For the 32-MiB dropped-field record, both isolated references show sampled peak
heap **144.1 MiB** above baseline, **160.1 MiB** allocated, 358 bytes retained.
Sampled peak is not an exact maximum; an earlier 128.0-MiB sample is retained
in the initial gate log. Near-limit fixture construction is untimed and its
64-MiB boundary tests exercise real production limits, not lowered test limits.
Database native values are still materialized before collector size validation.

Subprocess results: 100 samples per scenario/mode, private HOME/data home.
The native fresh scenario is Claude, local fresh benchmark is Codex. Full
paused/disabled/ignored distributions and maxima are in both JSON files.

| Enabled subprocess | 98 p50 / p95 | Current p50 / p95 |
| --- | --- | --- |
| Claude fresh | 24.64 / 36.99 ms | 25.05 / 34.73 ms |
| Codex stop | 19.28 / 26.90 ms | 22.80 / 48.90 ms |
| Claude subagent stop | 20.28 / 27.40 ms | 20.82 / 32.06 ms |

These include subprocess startup and filesystem work, not installed-agent or
remote capture. They are below existing hook budgets; broad distribution
variation is recorded rather than turned into a CI timing assertion.

## Counted gate inventory

| Proposal scenario | Executable evidence / preserved exception |
| --- | --- |
| Disabled/paused/ignored | `BenchmarkHookLocalEffects`: zero repository probes; actual pipeline has no source pass/database/version operation. New registry must preserve this before lookup construction. |
| Fresh/stop/subagent | Local-effect allocations and subprocess distributions; existing `hook_registration_race_test.go`, `admission_generation_test.go`, `hook_lock_wait_test.go`. New migration must eliminate current corrupt-index registration scan from hooks. |
| 1k/10k unchanged | `BenchmarkScanSettledRegistrations`: zero filters, bundle decodes and remote Get; existing `TestUnchangedSessionsCostNoWritesAndStayFast` pins local writes. |
| One changed amid unchanged | New `TestOneChangedFileAmidSettledSessionsFiltersOnce`: 32 sessions, 31 skips, one filter/publication, zero remote Get. |
| Parser-only upgrade | `TestParserUpgradeOverAnUnchangedTranscriptDoesNotReadIt` and `TestParserUpgradeWithNewContentReadsTheTranscriptOnce`: no unchanged native filter, once if changed. |
| Many Cursor chats | `TestCursorSQLiteOneSnapshotPerPass`: four changed chats, one copy; Cursor reader tests cover failed attempts and cleanup. |
| Large/near-limit | `TestLargeRecordMemoryCeiling`, record/text limits and five-MiB tests; ordinary allocation bounds and refusal semantics retained. |
| Deterministic/transient failure | `TestCursorSQLiteFailuresCostNoCopies`: missing/unsafe/size across three passes; cursorstore lock/changed-read fixtures preserve retry failures. |
| Multiple derived consumers | `BenchmarkAnalysisConsumers`: current repeated traversal baseline. One Analysis is a future counted phase-5 gate, not current behavior. |

Ordinary unchanged scans differ from deliberate file hook requests, which
refilter even when stat matches. Cursor text is always reread while present,
because stat equality cannot prove unchanged text; parser-only rederivation
can reuse safe published evidence. Do not apply the zero-filter gate to these
exceptions. Cursor signatures read per-chat native rows in place and never use
global database mtime or force a snapshot for unchanged-only passes.

Current architecture has no integration registry/pass/Analysis instrumentation.
Their operation counters must be introduced alongside real callers in later
phases, rather than claimed proven here. Relative >10% latency/peak regressions
require isolated reproduction and correction/justification; existing hook
absolute lock/read budgets still apply. macOS and real-systemd checks require
CI; this Linux reference cannot claim them locally.

## Historical verification record and limitations

Capture/state/collector/archive/doclinks ordinary characterization suites pass.
Final focused race run on current tree passes capture/collector/state/doclinks;
merged-main clipboard/StatsHTML selected race tests pass. Vet, blocking
golangci-lint, new-code revive and Linux deadcode pass (85 existing allowlisted
findings, zero unexpected). Six required Python script suites pass, including
18 fake bucket-purge tests. Pinned Levenshtein lint and final companion-link
verification are recorded in the implementer report. CI owns macOS, full final
merge-candidate race/fuzz, cross-build and real-systemd checks.

The initial full race run **failed**, and its tree changed while runtime docs
were read; it is not final-head green evidence. Failures: CLI commands/data-home
and project-scope fixtures plus local Home fixtures saw the sandbox’s `/tmp/.git`
marker; `TestRelativeLinksResolve` saw the companion before it existed;
`TestHookFilesAreWrittenThroughSymlinks` inherited a restrictive umask;
`TestDeepSubagentChainIsWalkedOnce/deepest_first` exceeded its wall budget
under load; `TestPlanRetireesReadsEachAliasAsFound` also failed initially.

Selected failed cases all passed at the integrated current head with umask022
and a private `TMPDIR=/var/tmp/agent-archive-cli-test-recheck-*` parent. The
existing CLI TestMain honors that prefix and nests its own temp home there,
avoiding its usual forced `/tmp` parent. An unsandboxed attempt without that
TMPDIR still failed the marker cases. Neither attempt changed tests or guards.
These are failed-case reruns, not a second complete race-suite pass. Exact
selectors, outcomes and original failure evidence are retained in the run’s
implementer report; required full final CI must still pass before merge.
