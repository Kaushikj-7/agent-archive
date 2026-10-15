# Muse Q0 qualification

Status: **reproducibility delivered; authority gates OPEN; capture unavailable**.
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

## Support and refusal matrix

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

Offline tests run in ordinary CI without native Muse or a listener and verify
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
