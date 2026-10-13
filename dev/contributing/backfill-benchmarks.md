# Backfill command measurements

Run `python3 -B scripts/test_backfill_measurement.py` to verify sample matching
and runner environment isolation. Comparisons require unique matched sample
indices, the same host, CPU topology, and Go execution parallelism.

The synthetic fixture under `internal/cli/backfill_benchmark_test.go` drives
`Run` through a complete rendered dry-run or foreground confirmed import.
It never uses a real home, native store, credential store, scheduler or bucket.
The fixed provider is an in-memory test store: timings describe complete local
application work, not live S3/R2 throughput.

Build the CLI test executable outside the measured region, then run explicit
samples. Keep original and candidate binaries, source commits and harness commit
with the raw logs. Apply any harness-only repairs identically to both sources.

```sh
go test -c -o /tmp/backfill-original.test ./internal/cli
python3 scripts/measure_backfill.py /tmp/backfill-original.test /tmp/backfill-original --samples 5
python3 scripts/measure_backfill.py /tmp/backfill-candidate.test /tmp/backfill-candidate --samples 5
python3 scripts/compare_backfill.py /tmp/backfill-original/samples.jsonl /tmp/backfill-candidate/samples.jsonl
```

The clock is September 23, 2026 in California. Metadata headers occupy exactly
1,024 bytes before the newline, including trailing JSON whitespace. Fixture version 6 has:

- Recent and old corpora: 1,500 files, sizes cycling through 256 KiB, 256 KiB,
  512 KiB, 1 MiB, 1 MiB, 1 MiB, 1 MiB, 2 MiB, 4 MiB, 8 MiB, plus small records.
  One-third carry retained assistant explanations; the rest carry tool stdout.
  Recent selects 1,350; old selects 150. Both run planning and complete imports.
- Large: one file with at least 210 MiB tool stdout and 31 small files.
  Its 1-MiB output strings exceed the normal per-string privacy cap; retained
  output fits the unchanged production transcript limit. All 32 sessions must
  be admitted, imported and verified. Version 5 used 8-KiB output strings whose
  retained total exceeded that limit and correctly refused the large source.
- Related: one old parent and four recent self-contained forks sharing context.
- Ancestry: one old parent and four recent children whose native history refers
  to the parent's captured prefix. Uploaded history manifests are verified.
- Tool-heavy and retained-heavy: 32 files with at least 4 MiB of each content
  shape, to distinguish privacy work from retained-output accumulation.

`--small` scales corpus sizes for functional smoke checks. Large corpora run
only through explicit measurements or Go benchmarks. `--scenario` selects an
individual fixture. `--state warm` primes traced dry-runs outside the timer. Cache-aware ordinary corpora must
prove all files hit the cache without adapter/filter reads, retrying at most
eight times if the bounded writer dropped first-pass observations. Original
binaries without cache counters prime filesystem pages only; results record
cache support, hit count and prime attempts separately. Import
archive/config state is fresh, so actual imports still publish. `--state changed`
primes then appends one safe record to one file. Filesystem-page warmth is
separate from inspection-cache warmth. `--trace` enables diagnostic metrics;
compare tracing overhead separately from disabled-trace optimization runs.

Each subprocess creates fixtures before the `Run` timer. Every confirmed sample
uses fresh registration/archive state, then reads uploaded metadata and source
bundles for the expected identities and verifies completed batch records.
Normalized per-session hashes include records, text, history, history ordinals, start provenance,
origin and source format. Hashes omit generated archive IDs and normalize the
disposable project path. Only compact hashes survive verification.

The JSON records command elapsed time, allocation and GC deltas, actual command
CPU from `getrusage`, RSS observed at command return, and whole-subprocess CPU
and peak RSS. Command-return RSS includes fixture construction but excludes
subsequent artifact verification. Whole-subprocess resources include both.
Linux RSS is converted from KiB; macOS reports bytes. Neither allocations nor
native budget accounting substitute for these process memory measurements.
Bounded checkpoint first/last offsets expose admission/staging/registration and
upload progress on the original production code. Enabled tracing adds bounded
aggregate stages and header/hash/filter/prefix-verification byte counts; these
count instrumented provider/codec reads, not a complete filesystem I/O ledger.
Filter input includes the native scanner's framing preflight. Capacity drains
are collected as total time/pass counts, not one span per session.

The supplemental planner benchmark uses counted discovery/provider file reads:

```sh
go test -run '^$' -bench '^BenchmarkBuildPlan$' -benchtime 1x -benchmem ./internal/backfill
```

It holds the planning worker count at one to match the CLI's native lookup
ownership. The library default worker count and 128 MiB byte scheduling budget
remain unchanged. CPU/RSS comparisons use plain builds and at least five matched
samples on one otherwise idle host. Record medians and range, input/count/artifact
equivalence, and stage costs. A faster checking stage alone does not establish a
faster completed backfill.
