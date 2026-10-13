# Scoped picker qualification

The picker qualification harness calls real `CLI.Run` for bare handoff,
interactive list and bare show. It measures a completed first 50-row screen and
an appended Older screen against the independent exhaustive repository/order
oracle. Only then does it send quit. A spinner, partial frame or eager in-memory
Older action does not establish the candidate's bounded startup acceptance.

A separate small real-CLI directory-scope control exercises32 accepted ranges
and33 complete compatibility ranges for list/handoff, including repository-plus-
project ranges, duplicate/reordered overlapping project activations and an
observable newest root in the last ProjectID. Scope comes from saved config and
`scopeFor`. Supported output forbids non-Project tree initialization; unsupported
output must show complete compatibility work. The exhaustive frame oracle also
rejects an omitted final scope identity. This control qualifies routing at the
range ceiling; the paired scale benchmark keeps its fixed two-range query.
Default 1400 CLI/native cases share one complete immutable seed; boundary cases
share a second complete33-membership seed. Each command uses a fresh home, Env,
request cache and meter. Setup durations are logged separately. The actual140
production-construction equivalence control remains independent.

Run using Go 1.27.2 and isolated caches, synthetic home and native files:

```sh
go test -p 2 ./internal/cli -run '^TestPickerQualification' -count=1 -timeout=5m
AGENT_ARCHIVE_PERF=1 go test -p 2 ./internal/cli -run '^TestPickerQualificationRealCLIPhases$' -count=1 -timeout=20m
AGENT_ARCHIVE_PERF=1 go test -p 2 ./internal/cli -run '^$' -bench '^BenchmarkPickerQualification$' -benchtime=1x -count=2 -timeout=30m
AGENT_ARCHIVE_PERF=1 go test -p 2 ./internal/cli -run '^$' -bench '^BenchmarkPickerQualificationNative$' -benchtime=1x -count=2 -timeout=30m
```

Set `GOMAXPROCS=2`; use the same compiler, build flags, host and serial scheduling
on both sources. `AGENT_ARCHIVE_PICKER_SIZE=10000` or `100000` restricts the main
benchmark to one fixture size for isolated process-memory observation. No test
reads real homes, native settings, providers or credentials.

The production baseline is
`a187e1b883924fd36c9d6700606a5c2756fc2b6f`, merged protocol-contract source before
transactional picker implementation. Create a separate clean checkout at this
pin, then run `python3 scripts/prepare_picker_qualification.py /path/to/baseline`
from the final candidate. The script copies byte-identical common fixture and
terminal measurement/oracle files, printing their SHA256. It uses the reviewed
protocol 10 adapter in this directory. The baseline retains its existing full
archive UI path; the final candidate uses complete protocol 11 indexes over the
same logical metadata/source universe. Neither is measured on a format it refuses.
Protocol10 setup first performs one real public tombstone commit, asserts its
nonzero head/epochs/receipt and absence of metadata/source bodies, then activates
the coordinator. Protocol11 setup uses the real empty scoped-upgrade constructor.
The bulk fixture replaces visible roots in both cases. This receipt/root-authority
format difference is explicit, while all logical metadata and source body bytes
are identical. Both setup/construction paths are excluded from phase metrics.
Record both production and harness revisions, adapter differences and fixture
hashes. `scripts/measure_picker_qualification.py` checks the shared harness hashes,
builds each test binary once, then serially pairs both sources with alternating
run order. Its manifest pins source, binary, adapter and harness identities and
retains process high-water RSS separately from sampled command heap. A dedicated
`measure_picker_process.py` wrapper uses kernel `wait4` rusage for each waited
child (Linux KiB; macOS bytes normalized to KiB), not cumulative parent child
usage or sampled RSS. It preserves wall time and exit status including failures. Pass explicit
`--baseline`, `--candidate`, `--go` and `--output` paths plus isolated cache/temp
environment variables. Do not reuse historical JSON/discard-writer samples as this baseline.

The fixture is 15% roots and 85% children. The first 1,200 physical identities
contain a fixed relevant repository population;10k→100k adds unrelated data.
Known/missing/conflicting repository keys, replay and unknown/zero/positive prompt
facts exercise disjoint membership classes. Every identity has its own full
immutable metadata body and owned synthetic source placeholder. All four trees,
child ranges and canonical/picker memberships are populated. A bulk builder
writes balanced32-way immutable nodes once, asserting the128KiB limit; ordinary
per-entry COW construction would retain millions of dead nodes at full scale.
The separate 140-entry test compares every tree leaf/semantic value/body reference
against actual legacy publication plus guarded production upgrade, checks receipt
root preservation and hydrated pages against the raw identity order. Scale
fixture construction is excluded from command cost. This does not benchmark
upgrade throughput, distributed mutation, GC or production migration.

The injected terminal starts at120 × 70 to display an entire 50-row page and
reports120 × 120 after Older, so the appended 100-row prefix and stable numbering
can be validated in one complete frame. Both sources see the same geometry.
This measures command loading/rendering/byte writes, excluding process startup,
terminal emulator paint and network transport. Input waits for actual complete
frames. Output validation occurs after the measured invocation with no new GETs.
Elapsed and allocation metrics are recorded independently for first frame and
Older; standard benchmark ns/op is unused. Runtime sampling is identical on both
sources and adds some instrumentation cost.

GET/LIST/Stat counts include attempted calls. Head, coordinator, tree, metadata
and source GETs are separate. `PayloadBytes` is actual returned store body bytes;
`EstimatedListBytes` is key+ETag lengths plus 16 bytes/header, excluding provider
XML/HTTP/TLS. These are not measured production wire bytes. No first-page or
Older metadata/source GETs or LISTs are allowed for the supported candidate path
without native-local work. Node limits derive from the finite query's two ranges,
page/lookahead, measured tree height and128KiB node maximum, with distinct CLI
setup authority overhead. Per-logical-reader operation budgets remain8192 node
visits/32MiB; full CLI costs include every caller observation.

For this fixed, nonempty fixture, let H be its maximum actual tree height and
R the supplied native registration count. One page is conservatively bounded by
D = 2 × H × (51 + 1 + 4) node visits: two disjoint ranges, at most51 selected
candidates plus lookahead, and four count/boundary paths per range. With R = 0,
First consumes the adapter prefetch and Older needs one remote page. With R > 0,
the model's refill ceiling is2 + ceil(R/50) pages per operation; First also owns
the prefetch, ceil(R/100) exact identity batches and at most R × H identity-node
visits. Older reuses that classification. These are conservative path bounds,
not forecasts of latency or typical node reads.

Let A be that phase's page ceiling, B its identity-batch ceiling and I its
identity count (B = I = 0 for Older). The enforced limits are A × D + I × H
node GETs, A + B coordinator GETs, and A + B + 1 head GETs, with one additional
head allowance for handoff Older's entry validation. Opening supplies the first
page's head/coordinator pair. The extra head allowance covers a model return
from its preloaded or cached buffer; it checks current authority even without a
new archive page. The two allowances can overlap conservatively and every
actual call remains metered. Every Older return must observe a fresh head. The
R = 0 case additionally requires the coordinator observation from its actual
archive page; native cases may consume only a local/cached buffer and correctly
perform a head-only continuation check with zero new coordinator reads. Tree
bytes are bounded by
A × min(D ×128KiB,32MiB) + min(I × H ×128KiB,B ×32MiB).
Add each allowed head's16KiB and each allowed coordinator's4MiB to obtain the
whole-phase payload ceiling. These limits include worst-case authority body
sizes; the measured fixture bytes are reported separately. They neither promise
worst-case fast output nor qualify production transport. Native filesystem
activity, stat and label bytes have their own counts and budget below.

Cold runs remove private cache state. Warm runs first execute the same two-page
command outside measurement; each measured invocation still owns a new request
view. Changed runs warm the original generation, then use a deterministic new
complete catalog with every capture/title updated. Node caches are command-local;
"warm" does not imply cross-command immutable node reuse. NativeR = 20 / R = 120 cases
hold archive contents fixed and retain real activity-provider/file behavior while
metering the provider's filesystem. Activity-reference counts are refs supplied
to the actual provider; native-stat counts are attempted injected `Lstat` calls
and open counts are successful wrapped native-file opens. Bytes count descriptor
reads. Internal syscalls performed by `EvalSymlinks` are not individually metered.
These counters are separate from archive work, including first-frame and Older
phase deltas. Archived registration cases observe native activity without label
body reads; local-only cases materialize synthetic prompt labels under the same
per-operation attempt budget. UnlimitedR is not constant cost;
archive association uses bounded batches.

`SampledPeakHeapInuse` is the maximum 1 ms-sampled runtime heap-in-use during the
command, with initial fixture-resident heap reported alongside it. It is not
RSS, total allocations or an exact peak. For process high-water RSS, run one
source/size in an isolated process with the same fixture-residency conditions and
retain the OS/process measurement separately; build/fixture allocation can affect
process high-water and cannot be labeled command peak. Report incomplete,
timeout or skipped100k cases as NOT RUN/FAIL. Synthetic counts do not qualify
provider latency or real-platform lifecycle behavior.

Implementation controls and matched measurements are tracked in
[the bounded picker plan](../../proposals/bounded-repository-picker.md).
[Matched results](results-20261013.md) pin the measured candidate, baseline,
harness/adapter/binary hashes and all 16 completed processes / 104 cases.
`python3 scripts/summarize_picker_qualification.py OUTPUT` verifies completeness and
fixture pairing while retaining every raw phase observation. Independent PR7
review and required hosted checks still gate completion.
ProductionS3/R2 CAS/clock evidence, endpoint-scoped capabilities, credential
cutover authority and multi-machine lifecycle remain
[release gates](../../catalog/provider-qualification.md).
