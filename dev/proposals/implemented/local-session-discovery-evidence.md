# Local session discovery: source evidence and decision

This records the bounded source investigation behind the revised
[discovery proposal](local-session-discovery.md). It describes source-format
support and its limits; it does not establish released desktop acceptance or
announce feature availability.

## Capture decision

Capture consent applies to supported session records in approved local Codex
homes, explicitly approved Codex-only capture scope, and the selected destination.
Scope is included-projects by default or an explicit all-current-and-future-projects
choice with configured exceptions; discovery ingress is separate. Unknown native
IDs require original creation within effective scope/source permission and an
unpaused interval, including applicable forward-only exclusion-lift barriers.
Fresh hooks retain their own supported fresh-start evidence. Scope changes never
rewrite known native-ID ownership, origin, start, admission or destination.
Excluded projects, invalid metadata, identity conflicts, removal records, and
identifiable unsupported imports or inherited histories remain ineligible.
All publication uses the existing privacy filter and read-back verification.

An indistinguishable recent copy is eligible under those same rules. Copying an
old session does not change its native start, and a copy of an already registered
session does not create a second archive identity. The decision accepts the
recent-copy limitation; it does not assume that metadata proves local execution.
The frequency of copying or synchronizing recent sessions has not been measured.

## Evidence boundary

The investigation uses public upstream source and disposable synthetic probes,
not user transcripts, settings, credentials, or storage. Source presence, file
appearance times, and reconstructed database entries establish availability on
this computer, not where the task executed. Native first-task metadata can
identify known importer shapes without establishing universal provenance.

## Pinned observations

The 2026-10-02 follow-up was bounded to 30 minutes of public source inspection
and existing disposable-probe evidence. It added no runtime probe. The inspected
release was [Codex `rust-v0.159.3`](https://github.com/openai/codex/tree/01fc69f4026735edfdf6789820549727a4867b11),
commit `01fc69f4026735edfdf6789820549727a4867b11`. The previously executed Linux
binary's SHA-256 was
`8bf204b36a2f6dd0dab73aa2f639892e67ef9ac8befccb4a05b1496ebf25c479`.

| Observation | Result and limit |
| --- | --- |
| Actual Linux TUI and exec tasks at 0.159.3 | `cli/codex-tui` and `exec/codex_exec` produced native first-task IDs and start metadata. These were CLI probes. |
| Synthetic stdio client named `Codex Desktop` | Produced `vscode/Codex Desktop` metadata. The harness selected that client name; this was not a desktop GUI probe. |
| Copy only a native rollout into a fresh Codex home, then initialize app-server without starting a task | Codex reconstructed matching SQLite metadata. The database entry does not prove originating execution. |
| Built-in external-agent importer | First imported task has `external-import-turn-N` and null root, even with fresh session metadata. Inspect the first task; a later native resume must not override it. |
| Native paginated history without inherited markers | Occurred in the existing native probes. Paginated alone is not an import/fork discriminator. |
| Diagnostic process logs and live `thread/started` events | Logs can be lost/pruned or arise from resume; events also cover forks/reviews and are not retrospectively available to a scheduled scanner. No reliable durable execution-host discriminator was established. |

The [protocol](https://github.com/openai/codex/blob/01fc69f4026735edfdf6789820549727a4867b11/codex-rs/protocol/src/protocol.rs),
[native producer](https://github.com/openai/codex/blob/01fc69f4026735edfdf6789820549727a4867b11/codex-rs/core/src/session/mod.rs),
and [imported task producer](https://github.com/openai/codex/blob/01fc69f4026735edfdf6789820549727a4867b11/codex-rs/external-agent-migration/src/sessions/export.rs)
provide source-specific classification evidence. Parent/fork/history-base,
non-user `thread_source`, and spawned-agent markers can supply conservative
exclusions. Absence of those markers supplies no proof of execution location.

The inspected successor [Codex `rust-v0.160.0`](https://github.com/openai/codex/tree/a956835d020762cb2b570053af06f643a11c0ecc)
peels to `a956835d020762cb2b570053af06f643a11c0ecc`. Exact-commit comparison found
its relevant protocol, rollout metadata/recorder, importer, initialization, and
state extraction unchanged. This is source compatibility for the inspected
fields; no 0.160.0 runtime or actual desktop was exercised.

## Format compatibility update (2026-10-05)

The implementation now recognizes metadata/first-task format profiles instead
of admitting exact producer/version tuples. Version differences alone, including
prereleases, do not block discovery. There is no unconditional `>=0.155` rule.
The earlier exact tuple list is retained only as evidence labels: the 0.159.3
CLI and synthetic app-server runtime probes remain distinct from public-source
inspection and from structurally compatible but untested producers.

Additional public source was inspected without reading private sessions or
changing local capture settings:

| Public source | Observed format and limit |
| --- | --- |
| [0.100.0 protocol](https://github.com/openai/codex/blob/rust-v0.100.0/codex-rs/protocol/src/protocol.rs) | `TurnStartedEvent` lacks `started_at`. This shape cannot provide required first-task evidence and stays unsupported, even if an envelope timestamp or later native resume is available. This is a structural refusal, not a lower release cutoff. |
| [0.150.0 protocol](https://github.com/openai/codex/blob/3b3b4f8fb3f6403e72c2d0533ed0d2f309c59717/codex-rs/protocol/src/protocol.rs) | Legacy and paginated history modes, task start with `turn_id` and optional Unix-second `started_at`; no root-turn field in `TurnStartedEvent`. Source inspection only. |
| [0.155.0 protocol](https://github.com/openai/codex/blob/f0a1b8f0849d90960bc406b848f32e5a129b0457/codex-rs/protocol/src/protocol.rs) | Session/root identity, fork/parent/history-base/subagent markers, local source tags, legacy/paginated modes. Task wire name `task_started`, with `turn_started` alias; UUID turn evidence and Unix-second start are compatible without `root_turn_id`. Source inspection only. |
| [0.155.0-alpha.9.2 protocol](https://github.com/openai/codex/blob/4607249e430dac1c961df4dc615beae88e33cec8/codex-rs/protocol/src/protocol.rs) | Adds optional `root_turn_id`, documented to equal `turn_id` on root turns. This exact public tag matches the reported executable version, but does not establish which version produced any private session. Source inspection only. |
| [0.155.0 recorder](https://github.com/openai/codex/blob/f0a1b8f0849d90960bc406b848f32e5a129b0457/codex-rs/rollout/src/recorder.rs) | Writes producer version from the build and supplied originator into `session_meta`, retaining creation and history representation. These are record facts, independent of whichever executable is found later. |
| [0.155.0 native submission IDs](https://github.com/openai/codex/blob/f0a1b8f0849d90960bc406b848f32e5a129b0457/codex-rs/core/src/session/mod.rs) | `new_submission_id` creates UUIDv7 strings, exposed as turn IDs by app-server. The [task lifecycle](https://github.com/openai/codex/blob/f0a1b8f0849d90960bc406b848f32e5a129b0457/codex-rs/core/src/tasks/mod.rs) records native turn timing; neither file is execution-host proof. |
| [0.155.0 importer](https://github.com/openai/codex/blob/f0a1b8f0849d90960bc406b848f32e5a129b0457/codex-rs/external-agent-migration/src/sessions/export.rs) | Creates `external-import-turn-N` first-turn identities. A later native resume must not repair this negative evidence. |
| [0.155.0 client originator](https://github.com/openai/codex/blob/f0a1b8f0849d90960bc406b848f32e5a129b0457/codex-rs/login/src/auth/default_client.rs) | Originator can come from the hosting client or an override. It is not an execution-host attestation and is no longer a client-name allowlist. |

The [0.155.0-alpha.9 protocol](https://github.com/openai/codex/blob/rust-v0.155.0-alpha.9/codex-rs/protocol/src/protocol.rs) was also inspected and has the same relevant
optional-root/start shape as alpha.9.2. The 0.140.0 protocol spot check has the
older no-root task shape. Neither is new runtime evidence.

The supported profiles are `codex_jsonl_legacy` (absent or legacy history mode)
and `codex_jsonl_paginated`. They require the same bounded header, original
creation, matching native/filename identity, local source, producer metadata and
native first-task evidence as before. Older producers can qualify if they
actually record that evidence; missing task IDs or start timestamps do not get
reconstructed from file times, envelope times, a later task or the installed
version. Compressed/referenced/unknown histories require separate support.

Synthetic JSONL fixtures under `internal/sourcefacts/testdata` model 0.150 absent
history mode, 0.155 legacy with no root-turn field, and alpha.9.2 paginated with
root-turn evidence, plus the 0.100 first-task shape lacking its own timestamp.
Their identities, cwd, times and text are fabricated. Tests
exercise these shapes and compatible unknown releases/prereleases, malformed
metadata, inherited/imported/subagent forms, absent first-task evidence and
mixed records. Disposable scanner/collector tests exercise the production
adapter, native ownership/deduplication, consent failures, filtering, source and
metadata schema validation, publication and read-back. They do not run a Codex
GUI or turn source inspection into a runtime claim.

`runtime_tested`, `source_inspected` and `compatible_untested` are diagnostic
labels computed from observed session producer metadata. Unknown client names
and versions remain compatible when the format qualifies. Summaries retain up
to 16 bounded version/profile/source/evidence combinations, never transcript
body, cwd or native IDs; overflow is reported without blocking admission.
Registrations retain the exact producer version and collector revalidation
still checks it against the reopened source.

Unknown additive metadata fields are tolerated to survive ordinary upgrades.
This explicitly accepts a semantic uncertainty: an unknown producer could change
the meaning of familiar fields or introduce inheritance signals we do not yet
recognize. Structural compatibility cannot attest all future layouts, complete
provenance, desktop GUI behavior or publication success. The format classifier
continues to reject recognized unsupported forms, and source consent, pause,
exclusion, project/destination authorization, identity ownership, filtering and
publication/read-back remain separate requirements. No live discovery setting
is enabled by this change.

## Admission rules

Read the first task event within fixed metadata bounds. Reject known import
markers, malformed native IDs, unsupported source forms, and invalid start
metadata; never skip a bad first event in favor of a later native task. Header
creation time governs consent. A task may begin long after creation during an
idle TUI, so task delay is not an upper eligibility bound. Test timestamp
precision and future-skew handling explicitly.

These checks distinguish supported shapes and known inherited content. They do
not distinguish every recent copy. The bounded investigation does not claim to
have exhausted every possible Codex signal.

## Remaining release work

Source inspection and a mock client cannot substitute for an actual desktop
probe. Desktop onboarding with hooks absent or unapproved, disposable storage
publication/read-back, worktree and lifecycle cases, writer compatibility, and
release-machine performance remain acceptance gates in the proposal. Source
ambiguity alone is no longer a gate. Keep activation separate from independently
reviewable foundations, scanning, and onboarding changes.
