# Muse Code integration and capability parity

Status: proposed; native validation, senior design review and additional qualification
performed 2026-10-14 against Muse 1.4.7 and 1.4.8.
No production Muse integration, configuration changes or archive publication has
been implemented. The target is
Meta's Muse Code CLI, including task, workflow and observer children.
Implementation requires approval; this document does not authorize coding.
[Design review](muse-plan-review.md) records the gaps corrected and remaining gates.
[Qualification/readiness](muse-qualification-readiness.md) provides the latest
results, durable frame excerpts and future subagent work packets.

## Objective and acceptance boundary

Add Muse as a fourth integration alongside Claude Code, Codex and Cursor. Reuse
the existing registration, privacy, collector, publication, read-back, retention,
reader, statistics and handoff policies. Native Muse vocabulary and storage
interpretation belong to `internal/agents/muse`, bound once through
`internal/agents/builtin` and consumed through narrow `agentapi` ports.

Parity means equivalent supported archive outcomes, not identical vendor
features or a claim that every existing integration has complete coverage.
Cursor child capture remains unavailable; Claude child coverage includes
synthetic fixtures; Codex child/history acceptance has its own gates. Use
[capture capabilities](../../docs/reference/capture-capabilities.md) as the
baseline. Adding Muse must not upgrade another agent's evidence claim.

The complete target includes:

- Fresh root capture and continuation for interactive, `exec` and `serve`
  sessions, preserving project, destination and original-start consent.
- Child transcripts and ancestry, distinguishing own activity, copied context,
  delegated tasks, workflow children and built-in observers.
- Metadata, titles, models, token usage, tool/MCP activity, skill evidence where
  proven, project/repository identity, and existing reader/export surfaces.
- Setup, refresh, inspection, removal, status, historical import and exact undo,
  bounded read-only local discovery, bidirectional handoff and replay classification.
- Existing privacy, crash recovery, publication ordering, byte-identical retry,
  read-back verification and retention guarantees.

Ship only capabilities whose gates pass. Unsupported operations remain absent
from the registry or explicitly unavailable in capability evidence. Missing
metrics must not become measured zero or silent empty results.

## Research evidence

### Observed build and procedure

Research on 2026-10-14 installed the official CLI in a temporary directory and
observed `Muse Code 1.4.7 (1.4.7-R6101.1)`. Native build metadata reported semver
`1.4.7` and SHA `d8c53d25bd`. The installer was inspected before execution;
`MUSE_NO_MODIFY_PATH=1` prevented profile/PATH changes. Configuration, data and
cache were isolated under `/private/tmp/agent-archive-muse-research`.

An echo session, its continuation, a loopback synthetic Meta-compatible provider
session, and a scripted workflow child supplied native observations. The
synthetic provider followed Meta's public SDK quickstart fixture pattern. No
real credentials, paid provider calls, repository mutations or S3/R2 writes were
used. The workflow child's model turn completed, but the workflow reported
failure because the synthetic text endpoint did not satisfy its completion-result
contract. The subsequent synthetic campaign supplied `submit_result` and passed;
real-provider workflow behavior remains unverified.

Primary references:

- [Meta: extending and automating](https://dev.meta.ai/docs/muse-code/extending).
- [Meta: audit and resume sessions](https://dev.meta.ai/docs/cookbook/audit-agent-sessions).
- [Meta: working with the agent](https://dev.meta.ai/docs/muse-code/interactive).
- [Developer docs: hook events](https://meta-models.github.io/muse-code-sdk/next/guides/plugins/reference/hook-events/).
- [Developer docs: settings-level hooks](https://meta-models.github.io/muse-code-sdk/next/guides/extend/hooks/).
- [Developer docs: skills](https://meta-models.github.io/muse-code-sdk/next/guides/extend/skills/).
- [Developer docs: durability profiles](https://meta-models.github.io/muse-code-sdk/next/guides/msp-concepts/durability-profiles/).
- [Official SDK and protocol fixtures](https://github.com/meta-models/muse-code-sdk).

The developer preview hook documentation described checks against 1.3.0; it is
not live proof for 1.4.7. Pin new fixtures and acceptance records to their actual
CLI build and native format. Temporary research files are not durable repository
evidence: preserve reviewed synthetic-content fixtures and provenance before
implementation acceptance.

### Observations and implications

| Observed fact | Implementation consequence |
| --- | --- |
| Root logs occupy `$XDG_DATA_HOME/muse/sessions/YYYY/MM/DD/<session-id>/session.jsonl`, defaulting to `~/.local/share/muse`. | Add native roots and bounded header inspection. Date directories are locator hints, not original-creation authority. |
| Every observed hook supplied `transcript_path: null`, although retained files existed. | Resolve logs independently; a Claude-style hook-path adapter would miss capture. |
| User hooks load from `$XDG_CONFIG_HOME/muse/settings.json`; project hooks use `.muse/hooks.json`. | Use owned user settings for ordinary setup and preserve unrelated configuration. |
| Hooks received `MUSE_SESSION_ID` for the root and `MUSE_THREAD_ID` for the current thread; custom XDG variables and `AGENT_ARCHIVE_REPLAY` were absent. | Inject root/thread host observations and explicit approved source/archive roots. Do not assume environment inheritance. |
| Child hook payload `session_id` identified the child, not the parent. | Decode Muse child events separately from Claude events. |
| Workflow `subagent_id` differed from `child_session_id`. | Keep logical agent and conversation identities separate. |
| Parent records included `subagent.control.spawn_accepted` and `subagent.control.child_session_bound`, with `storage_layout: nested_session_v1`. | Validate explicit native bindings before establishing ancestry or locator authority. |
| Children wrote `<root-session-dir>/subagent/<child-session-id>/session.jsonl`. | Collect child files separately and verify their stream identity. |
| Observer links used `memory_reminder_child_session_linked`, sometimes with a relative log path. Goal/verification files were observed; a skill link did not establish a retained file. | Preserve linked-but-unavailable children and distinguish absence from a late writer. |
| `SubagentStop` repeated for the same child. | Hooks are idempotent activity hints, not proof of one final completion. |
| Ordinary envelopes and `session_permission_transaction` frames containing `children[].record_json` shared one JSONL file. | Decode framing before filtering; one line need not equal one logical event. |
| Native `recorded_at` values were integer microseconds. | Convert with checked arithmetic; preserve native time separately from observation time. |
| Retained `runtime.session` events differed from `exec --json` stdout events. | Archive native files; stdout is not the retained-file schema. |
| Model usage appeared in `model_completed`, goal attribution and parent workflow summaries. | Select one per-call authority and avoid adding rollups again. |
| Root export included root records and accepted-spawn summaries, without observed child contents. | Root export alone cannot provide child transcript parity. |
| Resumed `exec --session-id` appended to the same file and emitted prompt/stop/end without a new observed `SessionStart`. | Continue known owners without another start; do not admit unknown resumes as fresh. |

### Plan validation and remaining acceptance

[Plan validation](muse-plan-validation.md) records the additional native campaign
and [machine-readable evidence](muse-plan-validation.json). Synthetic-provider
TUI/exec/serve runs, successful workflow result submission, direct children,
explicit grandchild lineage, isolated-worktree create/cleanup records, native
rename/fork/read, tool success/failure pairing, managed replay propagation,
normal outside-workspace artifact reads and memory-only sessions were exercised.
These establish native feasibility, not Muse-specific archive capture acceptance.

The additional [qualification campaign](muse-qualification-readiness.md) verified
child follow-up settlement, hard-kill recovery, native 1.4.7-to-1.4.8 continuation,
local stdio MCP success/error, project skill trust/read evidence, and direct managed
replay forwarding on 1.4.8. A schema-3 history-preserving checkpoint was produced,
resumed and continued with its raw log prefix preserved in a controlled case.

The original 1.4.7 checkpoint probes remain inconclusive historical observations.
Manual MSP compaction was unavailable on both builds; TUI compact/quit attempts
remain unqualified. Closed-child reopen was refused on 1.4.7. Real Meta accounting,
all compaction/prune/rewrite forms, branch original-creation authority, broader
skills/MCP, Linux/arm64 and Muse-specific filtering/publication/read-back remain
gates. None of these observations establishes production capture acceptance.

## Implementation design

### Identity, ports and composition

Declare canonical `agentmeta.Muse` as `muse`, alias `muse-code`, display name
`Muse Code`, and one binding in `builtin.NewBuiltins`. Follow
[adding an integration](../contributing/adding-an-adapter.md) and
[integration contracts](../proposals/implemented/agent-integration-contracts.md).
Do not add switches for Muse throughout shared consumers.

Root and child conversation keys use exact Muse session IDs qualified by the
agent. Agent IDs, workflow IDs, parent/root IDs and attempt IDs are related facts,
not interchangeable session keys. Preserve shared opaque-ID semantics; validate
separately any ID used as a path component. Do not manufacture a
`parent:subagent:agent-id` key for an independently identified Muse child.

Implement source/filter/parser and evidence first, then hooks/setup,
native inspection/discovery/import/children, skills/runtime/version and launcher
ports as their capabilities become supported. Register no placeholder operations.

### Source roots and the minimal shared changes

Use ordinary file registrations and the existing file source/filter/parser path.
Do not introduce a second publication pipeline, a new Muse source-kind database,
a per-agent daemon, or a general catalog framework. The existing hook path permits
an empty transcript path; the collector's file reader does not. Resolve that
specific gap before normal source collection.

Initially support one explicitly reviewed Muse data home per installation.
Setup persists its absolute configured spelling, canonical root and binding
revision alongside native source settings. Roots, physical locator hints and
authority bindings are local state; do not publish that configuration as new
source metadata. Existing installations default to no Muse capture. The integration declares native XDG path interpretation through
existing hook/discovery location ports; collector launches must not rediscover a
different home from their environment. Additional homes are a follow-up after a
measured need. Source-home changes invalidate lookup state and require explicit
setup reconciliation; they never rebind an existing owner silently.

The hook command needs the absolute archive executable and selected
`AGENT_ARCHIVE_HOME`. It does not need a second source-root environment variable:
the collector reads committed native source settings. Inject only bounded
root/thread environment observations into `HookInput`, using
the integration's declared key allowlist rather than copying `os.Environ`. Convert
those values into content-free typed lifecycle facts before queueing; never persist
the raw environment or reevaluate it during replay. The decoder remains pure
and does not walk directories, read transcripts or run the native CLI.

Required shared changes are limited to these contracts:

| Boundary | Contract |
| --- | --- |
| Settings/root binding | Persist the reviewed native root and revision; derive native locations through integration ports and apply changes through setup journals. Missing/unknown root settings never infer capture permission. |
| Collector locator resolution | An optional integration lookup accepts the qualified native ID, immutable registration facts and approved root binding. It yields a file candidate with evidence or explicit pending/ambiguous/refused status. It cannot admit a session or change destination/project policy. |
| Opened-source admission | Reuse `SourceAdmissionSignature` and snapshot `SourceAdmissionValidator` with generic expected ID/root/start facts where applicable. Native identity is checked on the same confined handle used for filtering, before retaining bytes. Extend the collector's currently Codex-only admission construction and confinement wiring; do not copy Codex history machinery into Muse. |
| Child evidence | An optional native inspector yields validated independent child/parent/root/role/start facts from bounded retained evidence. Shared code owns consent, registration and links; native decoder vocabulary remains in Muse. |

The source provider performs file I/O; the host supplies approved locations and
existing filesystem/budget dependencies. Do not build two independent inventories
in the host lookup and provider. Use one pass-owned lookup whose enumeration is
integration-defined and whose results the collector and source provider share.

### Locator lifecycle, confinement and recovery

A fresh hook may create an ordinary registration with no locator under existing
fresh-start/project/destination policy. That registration is the durable pending
work item; avoid a second queue containing the same session. Locator discovery
is resolution of an already admitted owner, not automatic historical admission.

The collector proceeds as follows:

1. Load the registration and committed source-root revision under normal collector
   ownership. An already resolved path is tried first through confined native
   validation; failure does not authorize adopting another session's bytes.
2. Resolve an absent/stale path through the bounded shared inventory. New unknown
   candidates remain unadmitted. Select a uniquely supported candidate, or a
   qualified native current pointer if such a read-only pointer is later proven.
   Incomplete enumeration or a pruned observation cache cannot prove uniqueness.
3. Open the candidate using the approved root and reject native descendant
   symlinks, nonregular files and root escapes. Validate stream ID, original owner
   facts, applicable workspace/start facts and child bindings on that descriptor.
   A filename, matching UUID, mtime or hook ID alone is insufficient.
4. Before persisting a resolved locator, take the existing short registration
   update lock and compare the registration, root revision, pause/removal window,
   project activation, destination and original binding. Recheck current policy.
   If anything changed, defer; preserve a concurrent hook's later observations.
   Enumeration and transcript reads occur outside `hooks.lock`.
5. Persist the path and content-free binding atomically, keeping source kind,
   native/archive IDs, origin, original creation/admission and destination stable.
   A crash before commit repeats resolution; a crash after commit revalidates the
   binding. Continue through the existing source/filter/publication journal.

Persist enough generic native binding facts to enforce confinement for Muse hook,
backfill and child reads, regardless of registration origin. Current collector
confinement specializes in Codex/discovery and cannot be assumed to protect a new
Muse hook path. Implementations may extend existing binding fields/contracts;
do not add competing locator and admission stores. Parser identity checks after
filtering are not a substitute for opened-source validation.

A known path may be reused after revalidation while unrelated inventory coverage
is pending. Relocation requires continuity of supported identity/start/source
facts; conflicting copies remain explicit. Never choose by newest mtime, largest
size or lexical order. Changing a native root or workspace does not silently move
capture to a new project, destination or consent window. Exercise the shared
continuation policy for supported workspace changes and defer unqualified changes.

Keep missing, empty, inaccessible, changed, unsafe, ambiguous, unsupported and
limit-exhausted outcomes distinct. Retain the prior verified archive on deferral;
never publish an empty replacement. Frozen publication retries use staged bytes
and do not need current native resolution. Retained-source privacy maintenance
also remains independent of native file availability.

### Bounded inventory and source work

Enumerate once per pass/approved root and share compact observations across all
unresolved registrations. Persist a bounded content-free traversal cursor and
root/revision scope so work resumes across collector ticks and process restarts.
A budget-limited pass must not restart at the first date folder indefinitely.
Reuse the bounded directory/cursor worker in
[`internal/discovery/discovery.go`](../../internal/discovery/discovery.go), with
existing native-read accounting through a narrow filesystem port where needed.
Its bounded `ReadDirent` batches and directory cookies already provide the
traversal primitive. Reuse those mechanics without registering Muse in Codex's
automatic-admission registry or importing its consent/catalog behavior. Qualify
cookie invalidation and platform behavior; an I/O primitive does not itself prove
coverage or source authority.

Bound directory acquisition itself. `discoveryio.Walk` calls `ReadDir` for a
whole directory before applying the candidate cap; using it unchanged does not
bound a large directory's allocation. Use bounded directory batches for the
scheduled locator path and expose incomplete coverage explicitly. Charge unrelated
entries and directories as well as matching files. Unreadable folders do not pin
all later work, and retries cannot consume the whole forward-progress allowance.

Cache compact identity/locator observations, never raw headers or full parent
transcripts per child. Cached positives remain untrusted hints. Eviction and
restored cursors lose coverage authority until revalidated; a negative lookup is
valid only for a completed scoped observation. Validate membership changes without
turning each unresolved session into a separate full-store scan.

Root facts follow the first permission frame, so identity inspection needs a
bounded multi-record prefix. Keep the first-record-only `DiscoveryBoundedProjects`
contract unchanged and return unavailable for that fast path; use existing
budgeted project/import/handoff inspection before proposing another header API.
Do not introduce a separate prefix-reader implementation for each consumer.

Use existing `transcriptio` snapshots, `sourceio` helpers and serial `SourcePass`
ownership. Initial qualified ordinary files are append-only. A rewrite/truncation
or unqualified checkpoint/history format is refused/deferred while preserving the
last verified publication; do not disguise it as a replaceable snapshot. Unknown
compaction is a release qualification gap, not an optimization to postpone silently.
Do not change Muse compaction, reminders or model behavior to make capture pass.
The qualified checkpoint case preserves the raw prefix; it does not establish
that every summary, goal, descendant or future checkpoint format does so.

The native `session-index.db` is a later optimization only if filesystem lookup
measurements justify it and a read-only snapshot/current-pointer contract is
qualified. Never mutate it, create native WAL/SHM files or infer admission from
its rows. No bespoke SQL index is needed in the initial implementation.

### Native framing and privacy filtering

Qualify ordinary envelope, transaction-frame and retained-marker versions
explicitly. Native forks also contain `retained_marker: omitted_live_only` with
positions instead of ordinary envelopes and timestamp-less `session.fork.*`
records. Preserve their omission/ownership semantics and require timestamps for
own conversation activity rather than every bookkeeping line. Preserve
logical child order, stream/event identity, sequence, timestamps, discriminators,
payload versions and supported causal references. Validate `record_json`, frame
version, child indices and integrity against vendor-produced fixtures. Charge
embedded JSON and expansion to aggregate budgets; nesting cannot bypass limits.
Preserve exact native frame bytes before validation. The 1.4.8 native exporter
rejected whitespace/key-order mutations and returned exit 0 with diagnostics;
independent canonical/checksum encoding is still unqualified. Do not assume that
JSON reserialization preserves native validity or classify every rejection as a
checksum-specific failure. Use the reviewed exact/derived frame excerpts as
scoped oracle evidence, not a complete decoder acceptance corpus.

Explicitly qualify `context_projection_checkpoint` schema 3 and copied
`session.fork.compaction`. The observed history-preserving checkpoint includes
`model_messages`, `base_instructions`, `run_context_messages`, `transcript_tail`,
resume state and repeated usage. Drop private snapshot bodies, never replay them
as new messages/calls, and preserve only qualified bounded ownership/coverage facts.
Its retained/raw identity and frontier semantics must be tested before enabling
that format; a producer resume pass does not qualify the archive parser.

An incomplete final record/frame remains pending for its writer. Malformed
committed records and unsupported framing yield refusal or explicit coverage
gaps under shared policy. Do not publish truncated output or acknowledge beyond
unprocessed evidence. Unsupported ownership evidence cannot grant authority.

After native validation, flatten supported frames into one deterministic retained
record representation with outer ordinal/child-position provenance and supported
native stream/event facts. Do not retain nested `record_json` strings or claim a
native checksum still verifies privacy-redacted bytes. Keep native frame integrity
proof local; the existing source-bundle SHA verifies the safe archived artifact.
`Refilter` consumes this retained representation, not the original frame decoder.

Define an initially versioned `muse-jsonl` retained format and native field
allowlist. Filter before source-bundle construction. Admit only supported
conversation, tool, usage, naming and lineage facts. Apply shared redaction to
all retained strings, including nested arguments/results, titles, objectives,
paths and skill instructions. Drop hidden reasoning, encrypted reasoning blobs,
raw provider request/context dumps, credentials, arbitrary hook output and
unknown payload contents. `muse export --redacted` is insufficient: observed CLI
help states that encrypted reasoning remains verbatim in both export modes.

Make failure behavior explicit:

| Input/result | Required behavior |
| --- | --- |
| Valid complete supported conversation | Filter and publish normally with evidence-backed availability. |
| Unknown optional non-authority record/field | Drop its contents, retain bounded content-free coverage facts, and mark affected metrics unavailable/partial through existing completeness and observability fields. |
| Unsupported framing/checkpoint, invalid checksum, contradictory identity/lineage | Refuse that candidate; preserve prior publication and show a fixed diagnostic. No source acknowledgement beyond unsupported evidence. |
| Incomplete final line/frame, missing late child, changed file or temporary I/O failure | Defer with bounded retry and no empty publication. |
| Raw/record/output/graph limit | Explicit limit refusal/defer under current policy; do not publish truncated output or raise global caps to force a pass. |
| Privacy-redacted visible content | Retain only allowlisted safe fields with current privacy/filter provenance; never promote omissions into complete metrics. |

Keep content-free identity, timestamp provenance and coverage when body fields
are omitted. Implement `Refilter` and `EvidenceExtends` on safe retained evidence.
Parser-only refresh must not reopen native files; retained refiltering must not
feed archive serialization back through the native-input parser.

### Normalized analysis and ownership

Derive one pure `archive.Analysis` for metadata, transcript, handoff and eval
export. Qualify these mappings rather than assuming similar event names imply
the same semantics:

| Evidence | Intended interpretation |
| --- | --- |
| Session stream, workspace/build/provider metadata, opened-session facts | Native identity, project, version and original-creation observations |
| Accepted user intent and run `started.prompt` | One logical prompt, deduplicated by supported command/run identity |
| `assistant_message_committed` | Visible committed response; avoid duplicating deltas and terminal text |
| Model/run configuration and model completion | Model switches and per-call usage attribution |
| Committed tool call/result families | Tool pairing, failure/cancellation, MCP activity, shell/git evidence |
| Spawn acceptance plus child-session binding | Parent, logical agent, child conversation and role |
| Observer child linkage | Observer relation and actual transcript availability |
| Naming, fork, rewind, copied-context and checkpoint records | Labels and ownership boundaries after native qualification |

Use checked microsecond conversion and complete timestamp provenance. Native
creation/activity and collection observation time remain separate. Folder dates,
file creation time and hook arrival cannot substitute for original creation.
If a required start fact is absent, qualify another native fact or keep automatic
admission unavailable.

Treat observed `session.fork.model_prefix` and `session.fork.turn` as copied
context, even when they contain full human prompts and source timestamps. Do not
use inherited permission-frame timestamps as a new fork's creation time.
Exclude copied parent/fork prefixes from a child's own prompts, tools, tokens,
titles and activity bounds. Fork/rewind/side provenance is a branch/context
relation, not evidence of delegated-agent ancestry. Do not populate
`ParentSessionID`/`NativeChild` or roll a side conversation into an agent tree
merely because its native record names a source session. Preserve existing
presentation/ownership policy and require independent original-creation authority.
 Explicitly identified harness reminders/notifications
are not human prompts; never infer origin from prose. Preserve failed/cancelled
and interrupted states without inventing successful tool results. Observed committed call/result batches pair by `call_id` and
`tool_call_id`, but result text alone has no typed failure flag. Link the native
task's `side_effect_intent` (`idempotency_key: tool:<call-id>`) to its task terminal
within the owning run. Never infer failure from the result's English wording;
use supplemental failure hooks only when native task coverage is unavailable.

Select one per-call usage representation keyed by owner and supported call
identity. Goal attribution, workflow summaries and terminal/export totals must
not add another copy. Provider response IDs alone are insufficient when reused.
Ambiguous cumulative scope yields partial/unavailable metrics, not guessed deltas.
Controlled usage of input 101, output 23, cached 17 and reasoning 5 yielded
`cached_tokens: 17`, `cache_read_tokens: 17` and `reasoning_tokens: 5` in one
`model_completed`; cache aliases are not additive. Verify actual-provider
cache-read/cache-write and reasoning-token overlap before assigning shared
totals. Cost stays unavailable until a dated price entry exists; unknown prices
are not zero cost.

Distinguish root, delegated child, workflow child and observer roles. Reuse
existing fields where accurate; add an optional versioned role field only if
public filtering/presentation needs it. Do not encode observers as replays or
human prompts. Human history defaults to roots; complete JSON and analytics can
include explicit descendants. Use the existing stats root/descendant rollup:
root prompt/session counts remain root-only, while independently owned child
usage/tools roll into the root once.
Do not create persisted tree-total token fields or a second aggregation engine.
Child metadata stores its own activity, and observer role is descriptive metadata,
not a new consent shortcut. Keep existing date-window, orphan and replay behavior;
qualify role-specific presentation separately before adding new filters. Root own
usage can be measured while descendant capture is pending/unavailable; aggregate
output describes recorded totals and exposes missing descendant coverage through
links/gaps. Claim exact tree totals only for a complete controlled snapshot. Add a
coverage annotation to the existing engine only if existing output cannot show
that distinction.

### Lifecycle, freshness and hook setup

Configure `SessionStart`, `UserPromptSubmit`, `Stop`, `StopFailure`, `SessionEnd`,
`SubagentStart` and `SubagentStop`. Qualify `PostToolBatch` for long-running
activity if needed; transcript/usage capture must not depend on intercepting all
tool/model requests.

Startup/clear can establish freshness only with supported new-conversation
facts. Resume/compact are continuation. Fork needs its own ownership/admission
interpretation. Null `transcript_path` never proves Muse freshness. Prompt/stop
continue existing owners but cannot admit an unknown old conversation.

Add injected hook-environment observations for root/thread identity; native
variable names remain inside the integration. Cross-check payload/environment
rather than replacing contradictory identity. Root `MUSE_SESSION_ID` does not
identify a grandchild's immediate parent; native lineage must establish it.

Generate user settings hooks with absolute executable and explicit archive/source
configuration from committed setup. Keep source roots in archive settings rather
than duplicating them in a hook environment prefix. Preserve unknown settings and
foreign handlers. Use expected-byte
`filechange.Change` plans and setup journals for install, refresh, rollback,
inspection and removal. Do not take over an existing managed-hook file or require
project trust for ordinary machine-wide capture. Hooks remain fast, silent and
nonblocking, with no filtering, upload or networking.

Replay classification is dynamic. Ordinary hooks must not stamp every session
with a fixed replay ID. Managed-hook forwarding of `AGENT_ARCHIVE_REPLAY` through
`managed_hooks_env_vars` passed for root, workflow child and direct descendants;
user hooks still dropped it. Direct recorder commands also passed on 1.4.8;
compound shell instrumentation lost the extra variables on both builds. Qualify
the exact generated archive command, including quoting/environment prefixes,
through each launch/child/observer mode. A settings allowlist alone is insufficient.
Use an optional runner-owned managed file for this mode, preserving any existing
managed configuration. Continue to qualify observer,
TUI/serve/recovery propagation separately. Any alternative launch-scoped mechanism
must preserve the actual marker through environment clearing. If unavailable,
report replay capability unavailable and leave that parity row gated.

### Child contract and consent

Existing non-Codex stop handling derives a child key from its parent, and
`internal/collector/subagent_capture.go` checks Claude-style parent IDs inside
child records. Muse's independent child stream and binding records do not fit
that convention. Do not falsify filtered `SessionIDs` or add Muse branches to
Claude-specific provenance rules.

Introduce a narrow native-child evidence/inspection contract carrying exact child
conversation ID, optional agent ID, immediate-parent/root relations, role, native
creation/own-task evidence, verified locator authority and coverage/availability.
Native integrations interpret relationships; shared orchestration owns consent,
registration, collisions, destination, replayed effects and publication. Preserve
Claude/Codex behavior with adapters and regression fixtures.

Bind Muse spawn acceptance to child-session binding and verify the opened child's
session stream. Paths and stop hooks alone grant no authority. Observer event
families need equivalent native validation; linked-but-missing files leave gaps.
The descendant probe placed the grandchild beside its parent under the root's
`subagent/` directory and reported depth 1 for both. Reconstruct ancestry from
`parent_session_id`, agent-lineage admission and child-session bindings; neither
physical nesting nor `depth`/display path is global authority. Root logs may
forward a child's control event, so reconcile repeated records by validated
identity/provenance rather than creating another child.
Repeated stops/reopen attempts update one owner idempotently. A new conversation
ID creates another owner even when a logical agent ID is reused.

Discover child candidates from validated spawn/binding/observer facts during an
ordinary root scan as well as lifecycle hooks. A stop-only path cannot archive a
long-running child or one whose stop hook was lost. Once admitted, children are
ordinary collector registrations and are polled independently. A start hook alone
is not freshness proof for a reopen; final response is a hint rather than a terminal
watermark. Do not reuse Claude's quiet-time/end-bound assumptions without Muse
qualification. Task attempts can finish while the stable child later receives work.

Children require original-creation, source/project and destination consent; an
archived parent does not authorize arbitrary descendants. Isolated worktrees
map to included repositories only through proven native/project evidence, not
path-prefix guesses. The isolated-worktree probe's child hook still named the
source checkout as `cwd`; use `subagent.worktree.*` prepared/lease/cleanup records
to establish the actual isolated checkout and source repository mapping.
A child can keep running after root termination; collection
must not freeze or delete it prematurely. Unresolved parents remain explicit
and may resolve later without changing stable keys or original admission. Parent
absence alone must not reclassify a declared child as a root conversation. The shared
`NativeChild` validation currently permits only Codex: generalize own-child
validation deliberately for qualified Muse bindings instead of setting a flag the
current archive rejects or creating a competing child marker. Preserve Codex's
existing legacy ownership/migration checks.

When original child authority cannot be proven, keep a bounded pending candidate
or an explicit unavailable link; never fabricate its start from permission-seed
time or treat parent consent as sufficient. If the root is absent and no supported
child evidence can establish its own scope, leave capture pending/unavailable.
Reconcile linked-session status idempotently after child publication, independent
of whether the parent currently has another changed transcript to upload. Extend
[`internal/collector/native_links.go`](../../internal/collector/native_links.go)
through the qualified native-child contract rather than building another linker.
It currently inventories only Codex parents, omits agent from its native lookup
key, and hard-codes the Codex removal namespace. Qualify keys by agent, native ID,
project/destination and native source-root binding; use the owning agent for
removal checks. Keep legacy composite reservation/migration handling Codex-only.
Queue link requests before marking reconciliation complete and preserve retries
through parent absence, retention/undo and subsequent publication. Reject cycles,
self-parenting and conflicting roots; graph limits cannot erase own-child status.

### Import, discovery and local access

Implement purpose-specific discovery, native headers, import inspection, project
paths and child enumeration. Backfill previews exact root/child scope, dates,
duplicates, refusals and gaps before registration. Reuse batch ownership and
exact undo: remove only the batch's ownership and link effects, preserving
independent live capture and all native Muse files.

Pre-setup discovery supports bounded read-only finding/show/handoff without
storage setup; it must not enable archiving. Automatic admission of sessions
whose start hook was never observed is an optional follow-up, not a prerequisite for parity with the existing Claude/Cursor
hook-plus-backfill baseline. Keep its capability absent initially. Before enabling
it, define a separate Muse-specific consent/creation contract and implementation
slice; do not treat the current Codex discovery configuration as generic.
Codex-only all-projects consent must not expand to Muse. Initial Muse scope is included projects; a later all-projects mode needs
explicit separate consent and design changes. Existing discovery authorizations
have an agent field, but root configuration and reconciliation remain Codex-specific;
that is a concrete extension prerequisite, not already implemented shared behavior.

Qualify original starts versus copies/imported/forked/inherited/resumed history.
If original-start evidence is insufficient, ship explicit import and hook capture
while automatic discovery stays unavailable. Missed-hook recovery requires
enabled discovery, proven native creation and valid consent; recent file arrival
is not authorization.

### Readers, skills, handoff and status

Use shared Analysis for list/show/title search, project/repository filters, stats
text/HTML, JSON, eval export and linked-child views. Preserve root/child and replay
visibility defaults. Partial evidence stays visible in availability and gaps.

Muse runtime detection uses injected thread/root observations, preferring the
exact current thread over project-latest fallback. Version inspection uses fixed
host `muse --version` probes. Compare version schemes explicitly: native metadata
had semver/SHA whereas CLI display included an R build identifier. Child metadata
without a build stamp cannot be called directly observed child-version evidence.

Declare skill installation and evidence roots separately. Muse scans shared
`.agents/skills`, config-local and some foreign/project roots. Deduplicate shared
installation with Codex/Cursor. Qualify trust, activation, foreign-root gates,
plugin skills and native invocation/read evidence before claiming eligibility or
observed-no-use. Filesystem inventory proves installed-only, not model eligibility.
On 1.4.8, successful `read_skill` produced `skill_read_observed` with
`evidence_kind: read_skill_tool` and a hash; qualify its identity/owner mapping
instead of inferring invocation from prompt prose. Archive install/setup must
use owned byte-preserving file plans, not `muse skills enable/disable/user-only`
or other native settings writers: those commands were observed dropping unrelated
MCP type/cwd/required/timeout fields. Do not repair foreign state automatically.

Archive-to-Muse handoff creates a new conversation using the existing safe context
artifact. Validate positional prompt/argv, artifact access under ordinary
permissions, quoting and terminal launching. Do not add bypass flags to make
handoff work. Muse-to-other-agent handoff uses normalized safe evidence. Native
resume is separate: exported logs do not establish portable native resumption.

Status separates presence, installed version, configured hooks, observed events,
local capture, publication, read-back and gaps. Fixtures or CLI installation do
not establish `verified_by_capture`. Sessions with logging disabled cannot be
recovered from files; report that limitation when positively observed.

## Capability parity and qualification matrix

Every row is proposed; none establishes production Muse support today.

| Capability | Required implementation | Required parity evidence |
| --- | --- | --- |
| Fresh root capture | Start authority, hook admission and independent locator lookup | New TUI/exec/serve sessions publish and read back; unknown old resumes remain excluded |
| Continuation/failures | Stable owner, append refresh, lifecycle signals | Multiple turns, resume, failure, interruption and hard kill preserve all committed own content exactly once |
| Children/observers | Independent keys, bindings, roles and child sources | Successful/failed direct/workflow children, observers with/without logs, grandchildren, reopen and child after parent exit |
| Consent/projects | Original-start/destination policy and verified worktree mapping | Excluded roots, pre-activation creation, removal/reactivation, nested projects and isolated worktrees respect scope |
| Privacy | Field allowlist and nested-frame filtering | Sentinel secrets scrubbed in every retained string location; hidden/encrypted reasoning and dumps absent remotely |
| Conversation fidelity | Intent deduplication and committed responses | Streams/retries/harness messages give exact expected text and prompt/turn counts |
| Tools/MCP | Committed call/result pairing | Success/error/cancel, parallel calls, structured/oversized results and native MCP attribution |
| Usage/cost | Authoritative own per-call usage and dated pricing | Exact controlled root/child/tree totals; real cache/reasoning semantics; unknown models unpriced |
| Names/models | Native label/model facts and refresh | Rename, automatic titles, Unicode, model switches and metadata-only refresh |
| Skills/instructions | Declared roots plus eligibility/use evidence | Shared-root deduplication, trust/activation/foreign/plugin gates, invocation/read distinction; unsupported facts unavailable |
| Git/repository | Existing hook lookup and native workspace facts | Dirty/clean starts, branch changes, detached/deleted worktrees, children and Git unavailable |
| Backfill/undo | Bounded import, batch ownership and exact undo | Date/project filters, roots/children, duplicates, restart/conflicts and import/live overlap |
| Automatic discovery (optional follow-up) | Separate design/consent and native creation proof; absent initially | Missed hooks, downtime, copies, old resumes and incomplete inventory; no consent inheritance |
| Local pre-setup access | Purpose-specific bounded headers/previews | Find/show/handoff without storage; no archive mutation |
| Readers/eval export | Shared Analysis and explicit roles | Text/JSON/HTML/export agree on identities, counts, gaps and own activity |
| Handoff | Safe Muse launcher/context access | Each existing agent to Muse and Muse to each; root/child selections, Unicode and shell metacharacters |
| Replay | Dynamic marker propagation | Root/observer/workflow/grandchild classification; ordinary sessions ordinary; default history hides replays |
| Publication/recovery | Existing frozen journal and verification | Failure at each write boundary, native source loss after staging, restart, byte-identical retry and exact read-back |
| Retention/refilter | Shared ownership/reference maintenance | Root/child expiration, unresolved links, privacy refresh, protected pending stages and no dangling acknowledged sources |
| Setup/remove/status | Owned transactional settings and evidence | Foreign handlers survive, rollback/recovery, remove only own changes, verified health only after publication/read-back |
| Platforms/upgrades | Qualified formats and fixed host probes | Actual macOS/Linux campaign, version upgrade/downgrade/future records; no Windows support claim |

## Required tests

### Fixtures, filtering and pure analysis

Preserve reviewed small synthetic-content native fixtures for observed root,
observer, workflow child and continuation formats. Add direct child, successful
completion, grandchild, tool/MCP, naming, fork/side-thread and compaction fixtures
after qualification. Include provenance: exact build, provider mode, procedure,
native framing and expected normalized facts. Temporary research paths are not
fixtures; scrub private machine data and review each fixture diff.

Test mixed frames, child ordering, nested JSON expansion limits and integrity,
unsupported versions, duplicate keys/IDs, invalid UTF-8, blank lines, missing
newline, partial frames, sequence conflicts, stream-local ordering, timestamp
overflow and contradictory identity. Unknown optional content can reduce coverage;
unknown ownership/framing cannot authorize capture.

Assert exact retained transcript and prompt/message/turn/tool counts, per-model
usage, role/ancestry, own activity, names and availability. Inject sentinel secrets
in arguments/results, nested strings, titles, objectives, instructions and paths.
Assert absence of reasoning, encrypted blobs and raw context dumps. Verify retained
refiltering and parser-only refresh with native files removed.

### Conformance, shared consumers and failure injection

Run applicable `internal/testutil/agenttest` suites for file/record sources,
discovery, historical import, skills, runtime and launch. Register the shared
`-update` flag in new packages. Exercise Muse through normal registry injection
across setup, hooks, source/filter/parser, publication, readers, backfill, child
links and handoff, not only standalone decoder tests.

Test colliding IDs across all agents, conflicting source roots, reused agent IDs
with distinct child sessions, multiple archive installations, foreign handlers,
payload/environment disagreement, lock contention, replayed effects and deferred
locator recovery. Exercise cancellation, snapshot ownership, pending writers and
all aggregate budget ceilings without unbounded working memory.

Inject failures before/after local state, stage creation, source PUT, metadata PUT,
verification, acknowledgement and cleanup. Restart must preserve frozen bytes,
original destination and ownership. New native bytes cannot replace a staged retry.
Privacy/exclusion maintenance covers every referenced source. Backfill undo must
preserve later independent ownership and continuations.

### Reproducible corpus and authority tests

Program hashes without their sources and temporary native logs are historical
observations, not repeatable acceptance. Step 0 checks in a small runnable
loopback/PTY/MSP probe with pinned CLI inputs, timeout/process cleanup and assertions,
plus reviewed native fixtures and expected normalized facts. The probe must use
fresh isolated roots, avoid foreign user-context dependence, make no external
provider request, and fail if a scenario claimed as tested did not execute.

Fixture provenance records raw/sanitized hashes, build, provider mode, framing,
redaction/omissions and expected ID/graph/usage facts. Scrubbing a frame invalidates
its native checksum: retain a reviewed exact synthetic frame or clearly label it
as derived evidence and do not use it to attest native checksum encoding. Keep
historical failed probe attempts, corrected outcomes and unexercised cases
separate. Assertions, not result labels, decide whether a native path passed.
Native command admission is not settlement. Wait for the owning child's new
`result_ready`/terminal and verify exact stream continuity. Keep rejected states,
timeouts, endpoint changes and corrected attempts explicit. The local MCP probe
required the canonical session tool ID; an unknown bare alias failure must not
be mislabeled an executed MCP error. The actual missing-file task settled `failed`
when correlated through its `tool:call_1` intent within the owning run.

Add consumer tests that the existing fourth-agent fixture cannot establish:

- Muse startup registration with null locator, restart before/after binding commit,
  concurrent stop/setup/pause/removal/destination changes and a replaced file at
  the same path. Verify no filtering/upload precedes strict opened-source admission.
- Hook/import/child paths all reject outside-root/symlink/mismatched-owner evidence;
  retained privacy refresh and frozen retry still work after native files vanish.
- Inventories larger than one per-pass budget eventually reach later entries;
  cache eviction, root change, unreadable folders, negative-cache invalidation and
  conflicting copies never prove unsupported uniqueness.
- Active child capture without a stop, root shutdown, lost stops, reopen, reused
  agent IDs, forwarded controls, contradictory parent edges/cycles and unresolved
  parents. Give two agents the same native IDs and source-root spelling to prove
  linkage/removal never crosses agents. No synthetic root fallback, duplicate owner
  or inherited permission appears; parent arrival triggers linkage without new
  parent transcript bytes.
- Mixed frames/markers with omitted/private records, copied fork turns and unknown
  checkpoints. Unsupported authority retains the prior verified archive and exposes
  gaps; omitted metrics never become available zero or complete coverage.
- Muse root/child/observer stats use the current single rollup with root-only prompts,
  child own tokens/tools, orphan/window/replay rules, missing usage and cache aliases.

### Performance and scalability gates

Use operation counts and allocation bounds first, plus timings/RSS on recorded
reference machines. Do not claim a speedup from unrelated CLI versions or warm
runs, and do not invent an absolute latency promise before measurement.

| Workload | Required behavior |
| --- | --- |
| Cold resolution of many pending roots | One shared bounded traversal epoch, progress across ticks/restarts, no session-times-store enumeration; record total directory/header operations. |
| Warm unchanged known sessions | Cheap confined identity/signature validation; no full native-body filtering, metadata PUT or repeated whole-store enumeration merely because a tick ran. |
| One growing root with many descendants | One parent analysis per changed scan; share compact child bindings, not repeated whole-parent reads per child. Child source cost is charged independently. |
| Large date/child directory, unrelated files | Paged acquisition under byte/entry/deadline limits; a candidate cap after whole-directory allocation is insufficient. |
| Large/nested record and retained refilter | Reserve raw/decode/output/graph memory before allocation; no unbounded map/string expansion and no full raw context cache. |
| Missing/ambiguous sources beside valid ones | Bounded fair retry; valid independent sessions continue while incomplete work stays observable. |

Reuse existing 1/128/512-session performance cases and full raw/record/source limit
fixtures, adding a store larger than one scan budget and long root/child graphs.
Restore full-size bodies under the repository's existing tier policy. Counters
must prove warm/cold behavior and forward progress; small short-mode success is
not a large-store acceptance result.

`NativeReadBudget` bounds charged memory, not total bytes read or process RSS.
Charge Muse lookup/graph caches with the current pass reservation, and verify
combined Codex/Cursor/Claude/Muse pressure. Do not allocate a second full cache
reserve or raise `PassReadBudget`/`PassCacheReserve` silently. Bound disk bytes read,
records visited and active time separately; release reservations when snapshots
and pass caches close. Leave working room for an ordinary session under existing
session ceilings.

Use existing changed-source signatures and frozen journals. Initial changed-file
filtering may reread the bounded snapshot as current integrations do; do not add
a raw-offset tailer or incremental derived-state database before profiling and
proving refilter/rewrite/checkpoint correctness. Any later optimization must
preserve the same authority and publication tests.

### Live native acceptance campaign

Use isolated archive/Muse homes, reviewed configuration, disposable workspaces
and a disposable bucket. Start with local scripted responses for deterministic
content, tools, usage and failures; then use real Muse/Meta calls for actual usage,
successful completion contracts and native behavior. Synthetic usage proves
mapping, not billing accuracy.

Record a dated pass/fail/pending matrix for TUI, exec and serve: exact versions,
argv, roots, hook events, native files, expected transcript/metrics, archive IDs,
remote checksums, read-back and `status --json`. Verify each child independently;
parent publication does not prove child capture. Required scenarios:

1. Fresh start, second turn, registered resume, unknown old resume, model failure,
   orderly exit, SIGINT/SIGTERM and hard kill with late log writes.
2. Successful direct/workflow child, failed/cancelled child, observer with and
   without retained file, repeated stop, follow-up/reopen, grandchild, and a child
   finishing after root shutdown.
3. Shared and isolated worktrees, missing/deleted worktree, included/excluded
   repositories, nested projects, reactivation and destination changes.
4. Fork/rewind/side conversation, manual/automatic compaction and resume across
   each; inherited history must not inflate own usage or human activity.
5. Rename/automatic title, model switches, skills, MCP, failed/parallel tools,
   large/structured results, Unicode names and multiline messages.
6. Nondefault XDG/archive roots, cleared hook environment, owned removal,
   upgrade/refresh, no-session-log mode and dynamic replay propagation.
7. Backfill with live continuation, exact undo, copied-source conflicts and
   local/bidirectional handoff. Missed-start automatic admission has its own
   optional follow-up campaign and consent gate.
8. Publication failure/restart/read-back, retained privacy refresh, expiration
   of roots/children and reader/eval-export round trips.

Repeat supported scenarios on real macOS and Linux for each advertised native
architecture. The current campaign covers macOS x86_64 only; cross-builds do not
qualify native arm64/Linux behavior. Current archive support does not include
Windows; native Muse Windows availability does not change that.

### Regression, resource and release checks

Follow [testing](../contributing/testing.md) against the final implementation
ancestry: race suites, vet/lint, architecture boundaries, golden policy, fuzz,
platform builds and required release checks. Fuzz framing, filtering, lineage and
retained analysis with bounded allocation assertions. Benchmark large inventories
and long root/child logs to prove bounded enumeration per pass and no per-session
full-store walk. Measure real hook latency and verify collection cannot block
native tools.

Shared child/hook changes need Claude/Codex/Cursor regression tests and existing
synthetic fourth-agent flows. Preserve identity, admission, reader JSON,
visibility, publication and ownership contracts. Review changed goldens rather
than regenerating unrelated output blindly.

## Implementation sequence and review gates

| Step | Deliverables | Gate |
| --- | --- | --- |
| 0. Native qualification and reproducibility | Checked-in reviewed fixture corpus, runnable loopback probe, expected assertions and source hashes; original-start/frame/checkpoint authority | Required authority proven before advertising capture; unresolved producer behavior isolated explicitly |
| 1. Source and analysis | Identity/binding, ordinary file source, strict opened-source admission, deterministic frames/filter/parser/preview, versions/evidence | Conformance/fuzz, exact transcript/usage, privacy and append refusal |
| 2. Root capture/setup | Single reviewed root, resumable locator inventory, short locked binding commit, minimal hook environment, transactional setup/status | TUI/exec/serve publication/read-back, custom-root/consent failures and current-agent regressions |
| 3. Children/observers | Child inspection contract, independent owners, lineage/roles and child metrics | Success/failure/reopen/grandchild/observer/worktree matrix; no consent inheritance or double counting |
| 4. Explicit import/local access | Backfill/undo and pre-setup bounded inventory reusing the same native readers | Scope/date/duplicates/budgets/recovery; no unobserved-start automatic admission |
| 5. User surfaces | Skills/runtime/launch, names, readers/stats/export, handoff and replay | Bidirectional handoff, output goldens, real skill evidence where claimed, dynamic markers |
| 6. Acceptance/release | Dated macOS/Linux campaign, measured performance, writer compatibility, storage/retention/privacy and exact-source gates | Every claimed baseline row passed; full parity unavailable until all baseline gates pass |
| Optional automatic discovery | Separate Muse start/consent design plus missed-hook native qualification | No extension of Codex consent/configuration by inference |

A limited root-capture release can ship after step 2 only with qualified source
framing/integrity, strict authority/confinement, crash-safe lookup, fixture coverage
and actual publication/read-back for the advertised launch surfaces/platforms.
Ordinary-mode probes alone do not qualify default long-context compaction behavior.
If that remains unsupported, describe the limitation explicitly and stop capture
safely on that format. Full baseline parity remains unavailable while child/observer,
replay, import, handoff or platform gates remain open. Update README/help,
setup/status docs, JSON contracts and schemas, capture capabilities and spec
indexes with actual implemented scope.

### Compatibility, rollout and operational state

Ship integration slices behind explicit Muse selection in setup. Do not enable
Muse because it is installed, because a shared skill already exists, or when an
old installation refreshes. Record actual supported native format/features rather
than accepting a CLI semver as proof. Failed qualification disables only the
unsupported capture lane and preserves its prior verified artifacts.

Source-root and child-binding additions affect persisted local authority. Define
schema changes and writer compatibility protection together, so an older binary
cannot silently drop those facts on setup/refresh or rewrite. Record the actual
old-reader matrix: metadata-only access where supported, otherwise a documented
upgrade requirement without corrupting data. Do not promise that an old binary
can enumerate or parse Muse. Test old-writer refusal/preservation, and upgrade
with existing registrations, pending publication, imports, setup journals and
privacy refresh. No automatic destructive migration.

Use existing status/capture-gap/retry reporting for pending locator, incomplete
coverage, unsupported framing, unresolved child, missing metric and verified
publication. Diagnostics contain fixed reasons, versions and counts rather than
native prompts or arbitrary error strings. Report all files contributing to hook
configuration, including optional runner-managed replay hooks; install/remove
must preserve foreign managed settings and tolerate interrupted journals.

Removal disables new capture and removes only owned hooks/skills/jobs under
existing policy. It does not erase native Muse state or acknowledged archives.
Rollback retains source bindings, consent and frozen publications rather than
clearing them to make an older binary run. Use one tested collector/scheduler;
there is no separate Muse retry loop or self-updater.

Follow [version rules](../maintainers/versions.md): allocate Muse parser/adapter
versions, bump filter versions for retained/privacy changes as required, and
update source/metadata schemas alongside any role or lineage additions. Choose
the next available global version during implementation rather than reserving
one here. Existing archived sessions must remain readable.

## Settled design choices and remaining questions

The plan uses native retained logs plus lifecycle hints, owned user hooks,
included-project scope, independent child conversation identity and shared
publication policy. It does not require a long-lived MSP observer, execution of
`muse export` every pass, native index mutation or upstream Muse changes for
ordinary retained capture.

Resolve these with native probes and the step-0 fixture corpus before enabling
the affected capture/metric capability:

- Which fact proves original root/child creation across copies, resumes, forks
  and checkpoints, and can bounded inspection acquire it?
- Which transaction integrity encodings/formats exist beyond the observed
  permission frame? Can compaction prune, replace or split the log?
- How are direct children, nested parents, reopened attempts and isolated
  worktree mappings bound durably when a parent is missing?
- Which observer sessions intentionally lack retained files, and how does native
  evidence distinguish that from late writes or disabled logging?
- Which tool/skill events prove committed pairing, invocation, eligibility and
  own usage without exposing hidden reasoning or raw context?
- How do retry/cache/reasoning/background/compaction token counters overlap?
- Can launch-scoped replay markers reach every native surface and descendant
  through cleared environments without marking ordinary sessions?
- Can handoff artifacts be read under normal permissions, and which naming,
  side-thread and fork facts establish ownership parity?

If an answer is unavailable, leave the operation or metric unavailable and name
the evidence needed to enable it. Optional upstream improvements include populated
transcript paths and immediate-parent/root IDs in every child hook. Their absence
must not be hidden by an unsupported completeness claim.
