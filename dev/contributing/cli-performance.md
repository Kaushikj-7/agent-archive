# CLI performance baselines

The synthetic benchmarks measure current reader/command work without real
credentials, transcripts, native applications or a bucket. They change no
production command behavior. Run from the repository root with the toolchain in go.mod (Go 1.27.2):

```sh
AGENT_ARCHIVE_PERF=1 go test ./internal/reader ./internal/cli -run '^$' -bench 'Benchmark(ReaderScale|CommandScale)$' -benchtime=1x -count=2
go test ./internal/reader ./internal/cli -run '^$' -bench 'Benchmark(ReaderLatency|CommandLatency|UsableTerminalScreen|CommandReads)$' -benchtime=1x -count=2
```

Use a private `GOCACHE` and `GOTMPDIR` when running from an isolated checkout.
Run timings serially on an otherwise idle host. These benchmarks report costs;
they do not enforce wall-clock thresholds in CI. `-benchtime=1x` keeps the full
50k filesystem matrix practical; repeat with `-count` to see timing variance.

`BenchCase` covers 800, 10k and 50k sessions, limits 1, 50 and 0 (all), and
cold/warm/changed caches. Synthetic metadata uses pinned timestamps and 100
projects, one placeholder source header per session, and complete v3 revision
summaries. No source is downloaded. Cold cases start with an empty private
cache. Warm cases prepare precisely the selected cache. Changed cases warm the
original revision, then replace every sidecar and add its new revision summary;
the old summaries remain, as they may after publication. Every iteration starts
from the same two generations. Fixture construction, warmup and mutation are
outside timed/allocation measurements.

Reader and command scale operations include opening the metadata cache. Reader
cases additionally report `cacheOpen-ns/op`; their total includes that span.
Command cases call `Run` with injected `Env` and include flags, config loading,
store opening, cache work, discovery, downloads and JSON rendering to an in-memory buffer with complete-output validation. They exclude OS process execution, credential lookup and real network
transport. Limits select by capture time; limit 0 exercises exhaustive fallback.

`MeasuredStore` retains versioned GET, bounded GET, paged/ranged LIST and Stat
capabilities. LIST/GET counters count attempted calls, including failed or
cancelled reads; GET bytes count returned body bytes. LIST header bytes are key
and ETag lengths plus 16 bytes for size/time per object, an explicit estimate
that excludes provider serialization, HTTP and TLS. Peak reads includes both
LIST and GET; Stat is forwarded but not counted. Delay applies independently to
each LIST/GET and honors cancellation. Small latency cases use 80 sessions,
limit 50 and 1 ms/request; they separate injected waits from zero-delay CPU and
filesystem scale costs. MemoryStore sorting/copying costs remain in both.

The command-read cases cover an exact summary, a parent with 50 direct children,
a short ID summary and one-day stats over 800 synthetic sessions. The stats
case spreads capture times evenly over 90 days (about 1/90 in the current
window), 100 projects, and one gpt-5 token attribution per session with varying
input/output counts. It includes no parent relationships or skill/tool usage;
those richer compute workloads have separate stats benchmarks. The listing
matrix itself spans less than 14 hours and has no token/model counts. The first
usable screen case covers cold interactive stats on an injected 80x24 terminal;
it records the completed first full-frame write and then quits through fake
keys. It includes data loading, stats computation and rendering, but does not
measure emulator painting or process startup. Browser list startup, real
provider latency, live archive targets and platform acceptance require separate
validation; no synthetic run establishes them.

The initial baseline uses production main at `d914dfb2391dd5f8b18b2d182b17bf6f4269e256`
(2026-10-07), including the history/lazy-inventory changes. Compare later PRs on
the same fixtures and machine. Warm list-50 should retain zero GETs; bounded
list-50 reads 50 selected sidecars, while narrow stats and short-ID lookup
currently require exhaustive bodies. Timing/allocation snapshots are supporting
local evidence, not provider guarantees. The [implementation plan](../proposals/cli-performance.md)
tracks subsequent optimizations.

Initial local execution recorded content-free counters on native Darwin/amd64.
Bounded cold/changed list-50 used 50 GETs; warm used zero. Exact show used four
attempted GETs (including unavailable harness probes). The short-ID path used
804 GETs and two LISTs for 800 sessions. Cold stats and its first usable screen
used 800 GETs. The full matrix was attempted, but repeated exhaustive cache
cleanup became prohibitively slow in `unlinkat`; the partial 10k exhaustive
warm sample still showed zero GETs and 50 ranged LISTs. Full 10k/50k cache
coverage remained pending after that initial run. Concurrent local validation
caused material timing variance, so these wall-time samples are not accepted as
controlled baselines.
Use the commands above on an idle host before assessing speed improvements.
`BenchmarkReaderExhaustiveNoCache` supplies a cheap additional cardinality/read
check at 10k and 50k without claiming filesystem-cache coverage.

A subsequent run at the same pinned head completed all 27 reader and 27 command
scale cases, including 10k/50k exhaustive cold/warm/changed disk caches, plus
latency, first-screen and command-read cases. This closes the matrix execution
coverage gap. Unrelated CPU race tests and temporary cleanup overlapped that
run, so its wall times still carry an external-contention limitation. They do
not establish idle-host, live-provider or real-terminal acceptance.

The 10k/50k scale fixtures and 10k/100k historical model require
`AGENT_ARCHIVE_PERF=1`. The default scale run retains the 800-session matrix.
The historical model's full measured matrix and assumptions are recorded in
[listing performance](../maintainers/list-performance.md). Its virtual latency
is a historical cost projection, not a live R2 measurement or benchmark wall time.

The reconciled measurement at source `3878e73f43a2c372a63773b6ba0b436057606d5f`
uses Go 1.27.2 and records the complete original matrix plus the historical
phase-3 matrix. See [the recorded results](../maintainers/list-performance.md#synthetic-baseline-recorded-2026-10-12)
and [raw output](../maintainers/listing-baseline-2026-10-12.txt). The older
`d914dfb2` compiler/head results above remain historical; they were not relabeled
as measurements of the reconciled source. Local single-run timings retain a
shared-host contention limitation; request/body counts are primary.

Command result validation captures the same measured invocation and runs after
stopping the timer, without additional storage reads. Buffer writes and allocations
are measured. Freeze this repaired harness on both baseline and candidate; older
discard-writer samples are not equivalent. Preserve exact production/harness
commits, fixture hashes, toolchain/build settings and raw samples. Controlled
matched measurements of this repaired harness remain pending.

## Bounded matched command harness

`BenchmarkMatchedCLI` measures the same captured `CLI.Run` invocation that its
validators inspect after timing. Its default fixture has 800 synthetic sessions;
`AGENT_ARCHIVE_MATCHED_LARGE=1` adds 10,000. It covers actual default list-50 and
text and JSON, cold and warm, exact/short-ID show, a parent with 50 direct
children, one-shot JSON stats at 1/7/30/90 days, and cold interactive stats at
7/30/90 days on an injected 80x24 terminal. Stats has 400 days of token metadata
and 100 projects. There are no transcripts or provider calls.

```sh
go test ./internal/cli -run '^TestMatchedPerformanceRejectsPartialOutput$' -count=1
go test ./internal/cli -run '^$' -bench '^BenchmarkMatchedCLI$' -benchtime=1x -count=3
AGENT_ARCHIVE_MATCHED_LARGE=1 go test ./internal/cli -run '^$' -bench '^BenchmarkMatchedCLI/sessions=10000/' -benchtime=1x -count=3
```

List validation checks all selected metadata, ordering, counts and truncation.
Show validation checks every metadata field and linked availability. One-shot
stats validates the full JSON against exhaustive computation over the fixture;
each interactive frame checks every visible body row against exhaustive
computation and checks its shape/window/quit footer. Diagnostics fail validation.
Validation and exhaustive oracle work are outside measured time and allocations;
buffer capture is inside. Fixture, all stored key/body bytes, expected JSON and measured output SHA-256 fingerprints are
printed with verbose benchmark output, so a baseline/candidate comparison can
identify schema or semantic changes before interpreting its timings.

Freeze this file byte-for-byte on both source pins, with identical seed/store
helpers, compiler/build settings, invocation counts and fixture hashes. Any
adapter changes must be recorded separately and reviewed; never move production
optimizations into the control. Run only after correctness validation succeeds
on both sources, on an otherwise quiet host, with one benchmark process at a
time. Keep raw samples and exact source/harness hashes. These zero-delay samples
include MemoryStore sorting/copying and private disk-cache I/O. They measure
in-process startup and command work, not OS process execution or real-provider
latency. `B/op` and `allocs/op` cover the timed operation; external maximum RSS
covers the whole worker, including fixtures, validation and cleanup, and cannot
be called per-command memory.

The common harness adapter pins LIST header timestamps and exposes the same
bounded-versioned GET protocol on both sources, using the existing measured
versioned getter exactly once. It checks the limit after copying tiny synthetic
bodies, unlike a provider's admission check; no oversized body case is measured.
The older wrapper lacked this counted method. No catalog is seeded or enabled,
and no optimized production reader or stats code is copied into the baseline.

The seed's randomized revision nonces are replaced by deterministic 26-character
hash-derived fixture identities. The common harness retains every summary and
its capture-time prefix, re-encodes the public v3 JSON in 255-byte path
components, and validates the result through each source's `PutRevision`.
Any protocol incompatibility fails fixture setup. This fixture-only replacement
makes the full stored key/body fingerprint reproducible, without normalizing
away output differences.
