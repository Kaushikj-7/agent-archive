# Muse Q0 qualification

Current implementation: see the canonical plan’s
[delivered local subset and residual ownership](../../proposals/muse-integration.md#delivered-local-subset-and-residual-ownership).
Strict qualified local source/filter, retained parsing and handoff are implemented;
public capture/first-owner admission remain unavailable. Qualified nonempty
enriched sources can expose observed committed invocation counts; this does not
qualify approval-free read sessions, unknown-ID startup, or full parity.

Historical Q0 status: **reproducibility delivered; authority gates OPEN; capture unavailable**.
This campaign supports the [integration spec](../../proposals/muse-integration.md)
at source base `37609182169b658c380e9113888b0cd11d387c05`. It does not implement
an adapter, setup, publication, retained filter or capture permission.

## Run safely

Use Python 3.9+ on macOS or Linux with a PTY. The runner accepts only these
exact **macOS x86_64** binaries, verifies `--version` in a private environment,
and records the selected producer, binary hash and actual runner hash. Supply
the direct binary yourself; the script does not download, install or invoke an
auto-updating launcher. Native Linux/arm64 are not qualified.

| Producer | Binary SHA-256 | Recorded native scope |
| --- | --- | --- |
| 1.4.8-R6256.1 | `94ea8fd1c73664c22e5c6157d8706945b667b11bd1c7c51691811e79f17dfcf1` | Historical six-case matrix and corpus in [results.json](results.json); original runner hashes preserved. |
| 1.4.9-R6297.1 | `4c6ed04ccd5a453bd7b38799aeb9eb290b8635b32c95b5a35e6950a2f20446cb` | Supplemental `exec-utc` only in [results-1.4.9.json](results-1.4.9.json); 22 assertions passed on the final runner. |

The historical 1.4.8 stable MSP schema fingerprint is
`sha256:4e3200c3c9dc0cd7919d11022630d43f9bc1c5cdda375381e90b0183b5665069`.
Offline schema/SDK claims are supporting evidence, not a retained-file contract.

```sh
python3 -B scripts/test_muse_qualification.py
python3 -B scripts/muse_qualification.py \
  --binary /path/to/muse-1.4.8-R6256.1 \
  --output /tmp/new-disposable-muse-q0-run
# Reproduce only the supplemental 1.4.9 lane:
python3 -B scripts/muse_qualification.py \
  --binary /path/to/muse-1.4.9-R6297.1 \
  --output /tmp/new-disposable-muse-149-run --cases exec-utc
```

`--output` must not exist. It contains native logs and local export documents;
**never add that directory to Git or upload it**. The runner constructs an
environment rather than copying the caller's: private HOME, XDG config/data/
cache/state, temp and archive roots, system PATH, no inherited proxies/provider
keys, explicit `TBH_CREDENTIAL_BACKEND=file` and a dummy loopback-only key. A
temporary settings file binds the only configured provider to `127.0.0.1` on an
ephemeral port. No real provider, personal settings, Keychain, scheduler, bucket
or paid request is needed. The endpoint discards incoming model context; it
does not record raw requests. Write/shell tools are disabled; the tool case
reads one synthetic artifact under ordinary permissions. Hooks keep only
bounded lifecycle IDs, cwd/source/reason and root/thread IDs, dropping text and
arbitrary environment. No permission-bypass flags are used.

Native processes have independent sessions, command/turn deadlines and
TERM/KILL process-group cleanup, including after success, exceptions, Ctrl-C and SIGTERM. The CLI converts
SIGTERM into an unsuccessful exit that unwinds cleanup; interrupted cases are
recorded as failures. MSP drains/reaps its readers; the HTTP server shuts down and joins.
Captured exec/export and MSP streams are capped at 8 MiB each; MSP lines at
1 MiB and pending messages at 1024. A breached cap fails the case and kills
the owned group. PTY capture retains its 1 MiB cap. These are harness stream
limits, not future archive-format limits. Local native logs, exports and corpus
inputs are read with a strict 32 MiB per-file cap; hook evidence is capped at
1 MiB. Oversized evidence fails before decoding and is never silently truncated.
These acquisition caps do not limit how much native scratch data a producer can
write before its process deadline.
Output roots intentionally remain for local inspection. The PTY supports a
fixed window size and cursor-position response; its full-turn attempt uses
synthetic bracketed paste. Cleanup is not proof of graceful SessionEnd.
`--cases` selects named independent cases when diagnosing a failure. Each lane checks retained own identity, exact prompts/replies, model usage and
tool pairing; exec also checks that resume appended a second opening and turn.
Command acknowledgement alone cannot establish a retained turn. Any failed
case or false assertion makes the command exit nonzero. A successful runner
means its stated observations reproduced, not that authority/release gates pass.

## Evidence and corpus

[results.json](results.json) records reviewed observations, failed attempts,
exact input provenance and gate dispositions. IDs, timestamps, automatic names,
ports, hashes and durations naturally change on another fresh run; assertions
and provenance rules must reproduce. Do not compare only the command exit code.
Native `export` exits **0 with empty stderr for rejected lines**. The runner
requires zero process exit without timeout, a written document, and matching
`diagnostics.unparseable_lines` in the export, separately from the event array
(a rejected line is itself an export event).

The [fixture directory](../../../internal/agents/muse/testdata) contains complete
root/resume, own tool/resume, MSP-root, fork and TUI synthetic sessions through
owned-process cleanup. Every physical
line/logical child position is represented; records are not cherry-picked out
of a larger conversation. Each manifest pins raw and sanitized hashes, build,
provider, source base, exact own prompts/text/run IDs/tool pairing/usage and all
body omissions. The exact permission frame is a separate reviewed native byte
vector. All corpus conversation/tool content is synthetic; native context,
permission snapshots, environmental diagnostics and provider/hidden reasoning
bodies are omitted from the derived sessions.

`scripts/muse_corpus.py` makes this allowlisted review projection; it requires
nonempty paired own controlled prompts/replies and refuses an existing output
file or companion manifest. Use it only
on this harness's synthetic logs, inspect every output value, then add fixtures
deliberately. It preserves native IDs, sequences, timestamps and positions,
replaces disposable root spellings with `/synthetic/muse-q0`, and records each
payload omission. It does **not** produce the future `muse-jsonl` archive format.
Sanitized data is derived and cannot certify native integrity; no recomputed or
copied checksum is attached. The helper is not the production privacy filter.

The exact paths in [retained-allowlist.json](retained-allowlist.json) freeze the
maximum reviewed Q0 field set for these native types; [contracts.md](contracts.md)
freezes the shared signatures and admission/compatibility invariants for PR2.
Unobserved fields/types need new evidence and privacy review before retention.

## Supplemental 1.4.9 observation

The final dual-pin runner passed all 22 `exec-utc` assertions: exact synthetic
own prompts/replies/usage and stream identity, fresh/resume openings, unchanged
append prefix, valid copied log, and nine permission/ordinary mutation cases.
Exactly two requests went to the synthetic loopback endpoint. The metadata-only
[results supplement](results-1.4.9.json) pins final source and raw report hashes;
native vectors and exports remain scratch-only. All owned native processes were
reaped and the endpoint thread joined. The first sandboxed listener attempt was
refused before creating a session; its report hash and the initial scratch-pin
retry are recorded separately rather than relabeled as final-run evidence.

No retained framing or opening shape difference was observed against the matched
1.4.8 exec lane. All 15 exact historical permission/ordinary vectors also retained
the same export diagnostics under 1.4.9. Exact permission frames decode; tested
framing/checksum mutations are rejected. Ordinary schema 999 is still accepted
by export, so the collector must enforce its own supported-version policy.
Those historical exporter observations did not establish independent checksum
proof or original root-creation authority. Copied bytes preserve the same identity and
opening observation. No new MSP, PTY, tool-turn, child/fork/checkpoint, date-zone,
platform, real-accounting, storage or capture qualification is implied.

The official [1.4.9 SDK mirror](https://github.com/meta-models/muse-code-sdk/tree/bb863c9071d42d9f67fcc3561690425646f727c8)
still [excludes the Rust workspace](https://github.com/meta-models/muse-code-sdk/blob/bb863c9071d42d9f67fcc3561690425646f727c8/scripts/sdk-source-closure.json#L24)
that would define retained transaction integrity. Its
[Session declaration](https://github.com/meta-models/muse-code-sdk/blob/bb863c9071d42d9f67fcc3561690425646f727c8/schema/msp/msp.d.ts#L1688)
also distinguishes a top-level containing home from the immediate spawning
session; no child-graph authority follows from this root-only campaign.
The later [permission checksum qualification](permission-integrity.md) independently
recovers the pinned v1 byte recipe. The [initial-root contract](root-creation.md)
qualifies reserved logical-start semantics for strict observed 1.4.9 roots. The complete production validator
and admission gates remain unimplemented and capture unavailable.

## Historical Q0 support and refusal matrix

The dispositions below describe the pinned Q0 campaign, not current decoder
implementation status. Later local support is described by the current-status
link above; original reports, campaign observations, corpus and hashes stay frozen.

| Lane | Qualified observation | Gate / safe production behavior |
| --- | --- | --- |
| Permission transaction v1 | [Pinned checksum recipe](permission-integrity.md) independently matches nine native held-out frames and a fresh final-source run; exporter diagnostics remain separate. | Complete native grammar, permission profiles and unsupported transaction shapes remain **OPEN**. No production decoder or capture capability is enabled. |
| Ordinary schema-1 envelopes | Fresh/resume activity and usage observed; no enforced native checksum on tested ordinary envelope. | Own structural/version/sequence/identity validation required. Export even accepts schema 999, so it is not a version authority. Complete initial-transaction validation still blocks whole native capture. |
| Initial ordinary root | [Reserved logical-start semantics](root-creation.md) qualified for strict observed 1.4.9 exec/MSP prefixes, coherent identity and initial workspace. Opening corroborates startup and is later than logical birth. | Complete prefix/negative validator, same-descriptor checks, physical-source uniqueness and consent admission remain **unimplemented**. Unknown child/fork/checkpoint lanes refuse; no capture enablement. |
| Resume and exact copy | Original prefix and logical start continue; MSP resume can omit a new opened(true). Copies/relocation preserve logical identity and time. | No new owner, consent window or original physical-copy uniqueness follows from copied bytes or late observations. |
| Folder dating | Process-local dated folder differs from UUIDv7 UTC date in tested UTC−12 boundary. | Dates are locator hints only; UTC/local/adjacent-day plus bounded fallback remains required. Skew/all zones not established. |
| MSP fork | Own turn completes; timestamp-less fork prefix contains inherited prompt/answer and an omitted-live-only marker. | **OPEN branch creation**. Copied content is excluded from own activity; fork does not imply delegated child. |
| PTY | Startup and one synthetic multiline turn retained an own prompt and committed assistant response. | No manual/default compaction, failure/signals matrix or graceful-exit qualification inferred. |
| Children/observers/checkpoints | Historical leads only in the spec; not rerun by this root campaign. | **OPEN**. No child coverage or checkpoint frontier support. Refuse unsupported framing/checkpoint; preserve previous publication. |
| Real usage, platforms, publication | Controlled usage only, macOS x86_64 only, no archive PUT. | Billing/cache overlap, Linux/arm64, storage/read-back and final release acceptance remain **OPEN**. |

Missing/partial final frames and temporary file changes must remain pending;
invalid integrity, contradictory identity and unsupported authoritative formats
must refuse. An unknown optional body can be dropped with bounded explicit
coverage. None of these future production behaviors is implemented here.

Local static inspection recovered the exact permission v1 checksum encoding,
then independent Python recomputation matched native known answers. The
[checksum supplement](permission-integrity.md) records the formula, bounded
checks and final-source campaign. The [initial-root contract](root-creation.md)
records the separate reserved-start dataflow and observed lane discriminator.
Complete grammar, same-handle validation, uniqueness and consent admission remain
implementation gates. Historical
[1.4.8 results](results.json), [1.4.9 results](results-1.4.9.json), corpus manifests
and their original runner hashes remain unchanged. A matching unkeyed checksum
is not authentication or permission to retain a source. Skipping transactions
or shelling out to `muse export` in the collector is not a substitute for a
qualified native decoder.

Q0 also corrected the spec's downgrade assumption. An old JSON rewrite can drop
`native_binding` while leaving `TranscriptPath`; current non-Codex admission is
empty. New durable authority therefore requires writer protection/preservation
and actual old-entrypoint tests, not an optimistic return-to-pending claim.

## Validation scope

Offline tests run in ordinary CI without native Muse. They use bounded synthetic
loopback listeners for the opt-in tool controls and verify
isolation, timeout/group cleanup, export diagnostic handling, complete corpus
hashes/assertions, rejection of missing/mismatched own turns, SIGTERM cleanup,
empty-corpus/manifest preservation, stream/queue flood and oversized-file refusal,
removal of context sentinels, independent checksum known answers and byte binding.
The artificial checksum objects in the tests qualify only the byte recipe, not
native transaction grammar. The spec's §7 extension-point
commands were rerun on the current base; see results for actual commands/outcomes.
They do not validate any unimplemented Muse adapter. Native campaigns are opt-in
local qualification, not automatically run against developer machines in CI.

## Runtime presence (independent of capture)

[Runtime presence evidence](runtime-presence.md) qualifies a fresh root shell on
1.4.9 for noninteractive behavior and environment cleanup only. It does not
qualify that producer for retained decoding or capture, current-session identity,
resume, children or monitors. The authority gates above remain open.

## Production local v1 policy

The [versioned local v1 allowlist](retained-allowlist-local-v1.json) and
[inner schema](../../../schemas/muse-retained-v1.schema.json) define the exact
implemented `muse-jsonl` projection. They explicitly add minimal permission,
mode-specific startup and agent-tree authority facts. The original
[Q0 allowlist](retained-allowlist.json), projector, five derived manifests and
corpus retain their original bytes/hashes. Their broader ceiling does not claim
production accounting or native integrity validity of redacted corpus bytes.

## Offline tool-status campaign controls

Three additional cases are opt-in only: `exec-read-success`,
`exec-read-missing` and `exec-read-two`. They prepare one bounded regular
synthetic file, a guaranteed absent path inside the owned workspace, or two
separate regular files respectively. The third emits two independent call IDs
and item IDs in one provider response. It is a multiple-call control;
actual parallel execution and native cancellation remain **unavailable**.
No new native campaign ran to add these controls. Historical cases, binary pins,
corpus and result files retain their original provenance.

Each case admits at most four connections and four requests, including up to
two model-catalog requests (exact zero-body GET `/models` or
`/muse-code/models`) and exactly two POST `/responses` transitions. The two catalog
routes share one counter and response. Query-bearing targets, other aliases,
other methods and nonzero GET bodies refuse; no URL normalization is applied. Headers are
limited to 8 KiB, each body to 256 KiB and cumulative input to 1 MiB. Advertised
lengths are checked before body allocation; transfer encoding, duplicate
headers, malformed requests, unexpected routes/transitions and wrong result
IDs/counts refuse. Each handler has a two-second absolute read/write deadline;
the case has 45 seconds. Context is discarded. The fixed tool arguments use
only owned paths with offset 1 and limit 10; fixtures are regular and at most
1 KiB each. Writes, shell, reminders and foreign personal context stay disabled.
Captured streams and acquired native files retain the existing byte caps.
Native acquisition refuses symlinks, nonregular files and paths outside the
case root before decoding. A second session log prevents a passing receipt.

The owned subprocess group is checked at least between bounded stream polls;
a detected membership above 16 or an unavailable monitor aborts the case.
This is a detection threshold and cleanup deadline, **not hard fork prevention**.
macOS uses the scoped process-group libproc listing with a fixed buffer. Linux
streams at most 4096 catalog entries and 2048 stat identity headers (4 KiB each)
within 250 ms per check; unreadable or incomplete monitoring refuses. No argv,
environment, user-wide limits or unrelated process signals are used. Cleanup
sends TERM and, if still needed, KILL only to the present recorded owned group,
retains signal failures, reaps its leader and checks group absence within three
seconds, within one eight-second cleanup budget. When catalog monitoring is
unavailable, an exact owned-group signal-zero query can prove positive presence
for cleanup signals; it never supplies the final complete absence proof, and
monitor failure keeps the case unsuccessful. A failed TERM does not skip KILL.
Signal failures stay in the private receipt and require an independent complete
absence proof. Endpoint refusal stops the command;
cleanup closes every tracked socket and joins the server and all handler
threads, including interrupted and stalled reads. Rejected excess connections
acquire no handler or body. These bounds do not prevent a producer writing
scratch files before termination.

New case receipts expose fixed structural counts, recognized shape/state enums
and internal identity/index/batch/order/uniqueness booleans. Reordered results
with valid index associations are accepted and separately reported. Receipts do
not contain actual tool names, IDs, arguments, paths, model labels, counters or
result bodies. The endpoint also records one first-refusal code from a closed
set: `route`, `request-shape`, `input-shape`, `tool-declaration`, `transition`,
`association`, `limit`, `deadline`, `io` or `unclassified` (`none` before refusal).
The first code is retained under the server lock; later refusals cannot replace
it. Unknown exceptions map to `unclassified`, without their text or class names.
Separate cleanup-failure visibility remains available. Route observations are
fixed booleans for the two exact catalog GET routes and the existing response
POST route only; unknown targets, queries, headers and bodies are never copied.
The controlled provider-call echo accepts the existing exact `read_file` form
and exact `muse.read_file` only when the `namespace` member is absent. A qualified
name with any namespace member, unknown prefix/suffix/tool, wrong controlled
arguments or mismatched call/result associations still refuses. This finite
provider API representation does not change native retained names or status;
there is no general namespace stripping.
These fixture compatibilities and diagnostics add no native tool/status qualification.
Success output is compared privately with the fixed synthetic
file; missing-file prose is never interpreted as typed failure. Typed status,
parallelism and cancellation remain unavailable pending a separately reviewed
local native campaign. Process exit and exporter acceptance cannot qualify them.
The existing retained-turn success fixture helper keeps its historical purpose.

Disposable case roots retain private bounded stdout/stderr, process acquisition
receipts and hashes, command output and native logs for later attribution. These
files may contain native context and must never be committed or uploaded. Failed,
interrupted, empty and refused attempts retain unsuccessful report entries.
The later campaign must pin its actual runner/source/binary and private evidence
hashes; offline passing tests are not native status evidence or production parser,
privacy, wire-format, capture or accounting qualification.
