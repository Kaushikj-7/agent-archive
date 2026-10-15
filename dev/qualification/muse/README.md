# Muse Q0 qualification

Status: **reproducibility delivered; authority gates OPEN; capture unavailable**.
This campaign supports the [integration spec](../../specs/muse-integration.md)
at source base `37609182169b658c380e9113888b0cd11d387c05`. It does not implement
an adapter, setup, publication, retained filter or capture permission.

## Run safely

Use Python 3.9+ on macOS or Linux with a PTY. The currently pinned native input
is **macOS x86_64 Muse 1.4.8-R6256.1**, binary SHA-256
`94ea8fd1c73664c22e5c6157d8706945b667b11bd1c7c51691811e79f17dfcf1`.
The runner rejects any different binary. Supply it yourself; the script does
not download or install software. Native Linux/arm64 are not qualified by this
campaign. The stable MSP schema fingerprint is
`sha256:4e3200c3c9dc0cd7919d11022630d43f9bc1c5cdda375381e90b0183b5665069`.
Offline schema/SDK claims are supporting evidence, not a retained-file contract.

```sh
python3 -B scripts/test_muse_qualification.py
python3 -B scripts/muse_qualification.py \
  --binary /path/to/muse-1.4.8-R6256.1 \
  --output /tmp/new-disposable-muse-q0-run
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
checks `diagnostics.unparseable_lines` in the export, separately from process
exit and from the event array (a rejected line is itself an export event).

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

## Support and refusal matrix

| Lane | Qualified observation | Gate / safe production behavior |
| --- | --- | --- |
| Permission transaction v1 | Exact bytes accepted; selected mutations rejected by native export. | **OPEN integrity encoding**. No independent preimage/encoding contract; refuse before retention. |
| Ordinary schema-1 envelopes | Fresh/resume activity and usage observed; no enforced native checksum on tested ordinary envelope. | Own structural/version/sequence/identity validation required. Export even accepts schema 999, so it is not a version authority. Initial transaction still blocks whole native capture. |
| Fresh root, resume and exact copy | Fresh `session.opened.observed` false, resume true, append prefix preserved; exact copied log exports identically. | **OPEN original-creation authority**. Copied permission/opened times cannot create another owner or reset birth. No uniqueness/relocation authority inferred. |
| Folder dating | Process-local dated folder differs from UUIDv7 UTC date in tested UTC−12 boundary. | Dates are locator hints only; UTC/local/adjacent-day plus bounded fallback remains required. Skew/all zones not established. |
| MSP fork | Own turn completes; timestamp-less fork prefix contains inherited prompt/answer and an omitted-live-only marker. | **OPEN branch creation**. Copied content is excluded from own activity; fork does not imply delegated child. |
| PTY | Startup and one synthetic multiline turn retained an own prompt and committed assistant response. | No manual/default compaction, failure/signals matrix or graceful-exit qualification inferred. |
| Children/observers/checkpoints | Historical leads only in the spec; not rerun by this root campaign. | **OPEN**. No child coverage or checkpoint frontier support. Refuse unsupported framing/checkpoint; preserve previous publication. |
| Real usage, platforms, publication | Controlled usage only, macOS x86_64 only, no archive PUT. | Billing/cache overlap, Linux/arm64, storage/read-back and final release acceptance remain **OPEN**. |

Missing/partial final frames and temporary file changes must remain pending;
invalid integrity, contradictory identity and unsupported authoritative formats
must refuse. An unknown optional body can be dropped with bounded explicit
coverage. None of these future production behaviors is implemented here.

Blind hashing of JSON metadata subsets/child concatenations did not reproduce
the permission digest. Available SDK source is a selected mirror without the
retained Rust implementation. Native diagnostic strings mention closed grammar,
physical ordinals, 1–4096 children, 32 MiB/child, 48 MiB aggregate and 64 MiB/frame,
but do not define canonical encoding. These are investigation leads, not tested
archive limits. The productive next input is a producer checksum preimage/
encoding contract with reference vectors; skipping transactions or shelling out
to `muse export` in the collector is not an acceptable substitute.

Q0 also corrected the spec's downgrade assumption. An old JSON rewrite can drop
`native_binding` while leaving `TranscriptPath`; current non-Codex admission is
empty. New durable authority therefore requires writer protection/preservation
and actual old-entrypoint tests, not an optimistic return-to-pending claim.

## Validation scope

Offline tests run in ordinary CI without native Muse or a listener and verify
isolation, timeout/group cleanup, export diagnostic handling, complete corpus
hashes/assertions, rejection of missing/mismatched own turns, SIGTERM cleanup,
empty-corpus/manifest preservation, stream/queue flood and oversized-file refusal and removal of context sentinels. The spec's §7 extension-point
commands were rerun on the current base; see results for actual commands/outcomes.
They do not validate any unimplemented Muse adapter. Native campaigns are opt-in
local qualification, not automatically run against developer machines in CI.
