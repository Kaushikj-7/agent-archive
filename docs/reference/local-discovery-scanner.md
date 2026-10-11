# Local discovery scanner

This scanner backs automatic Codex discovery configured through
[setup consent](../getting-started/setup.md#automatic-codex-discovery). Only
compatible recorded Codex formats qualify; Claude Code and Cursor retain their existing
hook paths. Discovery does not claim originating local execution.

A scheduled collector observes approved active and archived rollout roots before
opening remote storage. A source scan has a five-second budget and at most 256 metadata
probes, with durable enumeration cursors and a bounded 8,192-entry metadata cache.
Up to 256 admission retries are retained separately from enumeration, with at
most 64 revisited per pass and half the probe budget reserved for forward
coverage. A conflicting source cannot pin a directory cursor; retry overflow
is reported and revisited by filesystem reconciliation. Unavailable directories
remain pending and retry before the backlog on a later pass; a failed directory
is attempted at most once per pass while other directories continue.
Native date directories and compatible settled SQLite indexes prioritize work;
those hints never establish native creation time or authorize capture. Live WAL
indexes fall back to filesystem enumeration without creating native side files.

Every candidate needs supported metadata, a native first task, an included
project, and original creation within current project and destination consent.
Recognizable inherited or imported histories reject. A recent copy with otherwise
indistinguishable supported native metadata is eligible under the revised source
contract; file arrival is not proof that execution originated on this machine.
Old history remains old when copied or resumed.

Registrations remain authoritative. A missing identity lookup requests the
existing registration census, then defers admission until that census restores
an owner or records explicit absence. Census enumeration and source reads run
outside the hooks lock. A completed census also checks bounded directory presence:
if both derived index directories are empty while registrations remain, it
rebuilds ownership before scanning, including when startup recreated the empty
directories. This preserves admitted continuations outside a newer start window
without requesting recovery for every old unknown source. Arbitrary individual
index loss while other entries survive still requires a recovery signal.
The census currently enumerates registrations in memory;
it is deadline-aware but has no fixed entry or memory cap. The CLI gives the whole
recovery stage a child context of up to half its remaining soft collection budget,
separate from the scanner's five-second deadline. A separate application allowance
of at most four seconds starts after complete census validation and fingerprinting;
index application resumes across passes.
an inventory that cannot finish within the child context restarts on the next
pass and cannot certify absence. The proposed progress-preserving recovery and
working-state targets still require qualification for the supported inventory size.
This phase does not
claim universal two-pass latency or release-machine resource acceptance.

Collector publication reopens discovery files within their approved root and
checks native identity, original creation, working directory, and admitted
producer version, originator, and execution source against the
registration before filtering. Existing origins, destination attribution,
admission time, and removal records are preserved. Discovery never supplies
a source locator to an existing hook or import registration; their existing
capture paths establish those locators. Scanner unit tests inject adapter
support within the scanner package; activation integration tests use the real
format checks. There is no production support override, and synthetic
publication/readback is not desktop GUI acceptance.

## Codex format compatibility

Compatibility comes from each session's recorded metadata and first task, not
an executable version discovered on PATH or bundled with an app. There is no
release allowlist or minimum-version gate. A prerelease or unknown version can
qualify under either supported profile:

| Profile | Required history representation |
| --- | --- |
| `codex_jsonl_legacy` | Line-delimited `session_meta` plus a native first task; `history_mode` absent or `legacy`. |
| `codex_jsonl_paginated` | The same evidence with `history_mode: paginated`; no inherited history base or subagent boundary. Ordinals do not authorize capture. |

Both profiles require a matching UUID filename/header identity, an absolute
bounded cwd, a creation timestamp, a local `cli`, `exec`, or `vscode` source tag,
and nonempty bounded producer metadata. Versions are diagnostic tokens (letters,
digits, `.`, `-`, `+`, `_`), not ordered release numbers. Originator names are
client-controlled metadata; a new name alone does not establish or defeat format
compatibility. The first `task_started` (or `turn_started` alias) must have a UUID
turn ID and valid `started_at` (Unix seconds or RFC3339). Missing/null
`root_turn_id` is compatible with older producers; a present value must match the
turn ID. Integer task timestamps may precede fractional creation timestamps by
less than a second. Long idle time before the first task does not renew consent.

Known imports, forks, parent/history-base/subagent markers, non-user thread
sources and unknown execution tags still reject. A rejected first task cannot
be repaired by a later native resume. Compressed/referenced or unknown history
modes report `unsupported_history`; missing/malformed producer metadata reports
`unsupported_producer` or `invalid_metadata`; missing first-task records remain
`incomplete_metadata`. Invalid or imported first-task evidence reports
`inherited_history`. An explicit null, empty or non-string history mode is
`invalid_metadata`, not the absent-field legacy default. Each rejected observation is independent: other compatible
records continue through authorization and publication.

Status's optional `observed_formats` contains up to 16 combinations of profile,
recorded producer version, source tag and evidence label, with observation counts.
Additional combinations increment `format_summary_overflow` without blocking
capture. `runtime_tested` records previously executed 0.159.3 CLI/app-server
probe evidence; `source_inspected` records pinned source evidence, including
0.150.0, 0.155.0, its inspected alphas and 0.160.0. A different version or client
is `compatible_untested`. These labels describe evidence, never permission,
publication, or desktop GUI acceptance. Each admitted registration and source
bundle retains its original producer version; current executable detection is
separate. Profile changes invalidate the private scanner cache and trigger
bounded reprobes; they never rewrite consent or native identity ownership.

Unknown additive fields are tolerated. Structural compatibility cannot prove
unknown future semantics, detect every imported/recently copied native file, or
guarantee that future transcript bodies remain readable. Bodies still pass
through the existing filter/collector rules, which may reject or omit unfamiliar
content. Successful publication and read-back verify the retained archive,
not completeness for unknown representations. The [pinned evidence](../../dev/proposals/implemented/local-session-discovery-evidence.md)
and synthetic fixtures document what was actually inspected and exercised.

## Measured operating envelope

A synthetic Linux Go 1.27.1 run exercised 1,000, 10,000 and 100,000 source files
in flat, native-date, settled-index and live-WAL layouts. Each case ran once;
passes ran consecutively to measure work, without scheduler sleeps. Fresh bursts
in these warmed fixtures required two scheduled recovery-before-scan stages.
Measured accumulated task time was 97–456 ms. This is a measured fixture result,
not a guarantee for all directory layouts, histories, cache states or machines.
The bounded cache still causes historical metadata rereads: the filesystem warm
cases probed 50–256 headers in the measured first stage.

Cold filesystem coverage required 4, 40 and 392 stages at those sizes. Settled
indexed coverage required up to 578 stages at 100,000 files in the dated layout.
The default Linux and macOS schedules run every 60 seconds. Thus 392 or 578
stages imply roughly 6.5 or 9.6 hours of scheduled cold coverage, despite only
54–57 seconds of accumulated work in the 100,000-file filesystem benchmarks.
Manual runs or changed schedules have different elapsed times. Source hints
prioritize recent work; they do not promise immediate exhaustive cold coverage.

A fresh unknown identity also needs a complete authoritative registration
census. With 1,000, 10,000 and 100,000 existing registrations, the measured
recovery-plus-admission task took 82 ms, 543 ms and 5.33 seconds respectively.
The 100,000-registration task allocated 1.125 GB in total across its execution;
process maximum resident memory at that point was approximately 82 MB. Total
allocation and process high-water memory are different measurements, and these
results do not establish release-machine resource or isolated lock acceptance.
Census work is outside the five-second source-scan budget and runs before
publication, so large inventories can delay publication even when source
observation itself is bounded. This implementation retains the existing census
architecture rather than asserting a fixed memory or time limit for it.

## Blanket policy integration

Codex scope is independent of the ingress. All mode shares a single consent
window across physical projects; discovery additionally checks its own approved
source window. Resolving a candidate never saves configuration. New projects
remain separate registration/archive identities, even below a configured parent.
Fresh hook starts use the same permission windows, while retained hook intents
replay their original observation and freshness evidence. Neither resuming a
session nor finding its file later creates a new historical permission.

Physical resolution has a fresh pass-local cache of at most 1,024 cwd facts,
64 ancestors, 4 KiB per Git metadata file, at most 1,024 accounted metadata
operations and 256 KiB aggregate Git bytes. Canonicalization counts each
`lstat`/`readlink`; other counted operations include directory checks and Git
metadata opens/stat/read work. These are resolver limits, not total scan I/O.
Exhaustion retains an admission retry with `project_budget_exhausted`; it never
invents a nonGit fallback for an unresolved Git/worktree mapping. Cache facts
expire each pass, and every reuse validates the observed file/directory
identities, missing Git markers and regular-file fingerprints within the same
aggregate budget. Metadata-operation counts cover explicit lstat/readlink,
open/fstat and bounded metadata-file read calls, rather than hidden
canonicalization stats or transcript/index I/O. Source probe, directory cursor and rotating retry budgets
remain unchanged, and complete identity census still governs unknown IDs.
Deferred hook replay shares the same aggregate project metadata allowance.
A small atomic scheduling cursor rotates that work across retained intents so
interrupted leading starts cannot indefinitely starve later starts. It conveys
no permission or identity authority; every replay checks the original evidence
and current policy, and expired or revoked intents are removed even when their
working directory is no longer available.

When a scan refuses a top-level session as `start_not_authorized` and it
started within 8 days before the earliest discovery start floor
(`config.DiscoveryStartFloor`), the scan records it in
`discovery-refused-starts.json` once it passes the same native admission
proof an admitted session needs (at most 16 proofs per pass; a provisional or
rejected session is not recorded): native ID, start time and rollout path, at
most 256 sessions, written atomically and only when it changed. A later scan
that reaches the session and admits it, finds it archived or removed (a
removal lookup that fails keeps it), or finds its project excluded drops it,
as does a scan that finds its rollout file gone. Status reads this file instead of the catalog for the pre-setup line
([setup](../getting-started/setup.md#after-setup)), and checks each session
against registrations and removal records itself, so an import clears the line
without waiting for a scan.

`discovery-health.json` is an atomically written, versioned summary capped at
16 KiB. It contains aggregate counts, bounded observed format/version summaries,
stable diagnostic codes and attempt/reconciliation timestamps, with no cwd, project sample, native ID or transcript
body. Status reads this summary without opening the full source catalog, Git,
SQLite or storage. Missing/corrupt/oversized summaries are uncertainty; freshness
must be evaluated against the caller's clock. Scan evidence remains distinct
from registration, queued publication, upload and verified readback.
