# Muse Code integration plan

Status: proposed. Add Meta's Muse Code as a fourth agent in agent-archive,
alongside Claude Code, Codex and Cursor. Native investigation established
feasibility; production capture, privacy, publication and full parity are not
qualified. Implementation still requires separate authorization.

This is the single implementation plan. Qualification findings and design
corrections are incorporated below; [probe data](muse-qualification-evidence.json)
and [frame fixtures](muse-qualification-fixtures/manifest.json) are supporting evidence.

## 1. Scope and success criteria

A root is a human conversation. A child is an independently identified delegated,
workflow or observer conversation. A fork or side conversation may copy context
without being a delegated child.

The baseline is included-project capture through hooks, qualified children,
explicit backfill, bounded local access, readers/statistics/export, skills/runtime,
handoff and replay. It must preserve existing consent, privacy, crash recovery,
publication, read-back and retention guarantees. Use the existing
[capture capabilities](../../docs/reference/capture-capabilities.md) as the
comparison: Cursor child capture is unavailable, Claude child evidence includes
synthetic fixtures, and Codex has its own acceptance limits. Muse must not enlarge
another agent's evidence claim.

Initially support one explicitly reviewed Muse data home, ordinary file sources
and the existing collector, registration owner, storage pipeline and stats rollup.
Automatic admission of missed-start sessions is a separate follow-up, absent
initially; Codex discovery or all-projects consent must never enable Muse.
Multiple data homes, a native SQL index, a daemon, per-pass CLI exports, raw-offset
tailing and an incremental metrics database require measured need and separate design.

A capability ships only when its tests pass for the advertised formats, launch
modes and platforms. Missing metrics remain unavailable/partial rather than zero;
installed/configured status does not mean verified capture. A limited root release
still needs qualified integrity, original creation, confinement, recovery and
actual publication/read-back. Unsupported long-context formats must stop capture
safely and be documented; changing Muse's compaction or observers to pass tests
does not qualify default behavior. Full parity requires every baseline gate.

## 2. Implementation sequence

| Phase | Deliverables and dependency | Exit gate |
| --- | --- | --- |
| Q0 — qualify authority | Reconcile current base/interfaces; pin producer inputs; check in a runnable isolated loopback/PTY/MSP probe and reviewed corpus with exact assertions/provenance. Resolve integrity, original root/child/branch creation and supported checkpoint frontiers. | Reproduce claimed cases from fresh roots without real provider calls or personal context. Unknown authority blocks the affected capture lane. Freeze shared contracts before dependent work. |
| P1 — source and analysis | Q0; identity/builtin binding, minimal shared admission/inspection ports, confined file source, frame decoder, privacy-safe retained format and one Analysis. | Conformance/fuzz, exact content/usage/ownership, bounded expansion and retained refilter without native files. |
| P2 — root capture/setup | P1; reviewed root/revision, durable null-locator owners, shared resumable inventory, short locked binding commit, minimal hooks and transactional setup/status. | TUI/exec/serve publication/read-back, root/consent/race/restart tests and existing-agent regressions. |
| P3 — native children | P1/P2; independent owners, explicit bindings/roles, active polling, agent-qualified durable linking and existing stats rollup. | No-stop/failure/follow-up/reopen/recovery/grandchild/observer/worktree cases; no inherited consent, cross-agent links or duplicate totals. |
| P4 — access and user surfaces | P2, plus P3 where descendants are promised; backfill/undo, local access, skills/runtime/version, names/readers/export, launch/handoff/replay. | Exact undo/owned-file changes, output agreement, bidirectional launch and evidence-backed skills/replay. Split independent areas only after contracts stabilize. |
| P5 — acceptance/release | All claimed phases; combined independent acceptance, native/provider/platform matrix, privacy/storage/retention, compatibility and measured resources. | Every advertised capability passes the tests below on the final source. Unavailable capabilities remain explicit. |

Use one implementer per coherent implementation PR and one writer for shared
state/interfaces. Independent workers use isolated checkouts containing this plan;
pin dependency/review heads and invalidate affected evidence after source-format,
interface, dependency or base changes. With four slots, use at most two independent
implementers plus one reviewer alongside the orchestrator. Combine inseparable
changes rather than forcing an artificial stack; no model override is prescribed.

“OpenMet with sub-agents” remains provisionally interpreted as the
`implement-with-subagents` workflow. A separate OpenMet runtime was not identified
or installed, and no implementation worker was launched. Clarify a different
runtime before relying on it. Publishing this plan does not authorize
implementation or its merges; verify future permissions/CI at launch.

## 3. Implementation contracts

### 3.1 Agent identity and shared boundaries

Add canonical `agentmeta.Muse` (`muse`), alias `muse-code`, display `Muse Code`,
and one binding in `builtin.NewBuiltins`. Keep native vocabulary in
`internal/agents/muse`; shared consumers use narrow `agentapi` ports. Follow
[adding an adapter](../contributing/adding-an-adapter.md) and
[integration contracts](../proposals/implemented/agent-integration-contracts.md).
Register supported operations only.

Use exact session IDs qualified by agent for root and child keys. Agent, workflow,
attempt, parent and root IDs are related facts, not interchangeable conversation
keys. Preserve opaque IDs; validate separately anything used as a path component.

| Shared boundary | Required extension |
| --- | --- |
| Root settings | Setup commits absolute configured spelling, canonical data root and binding revision through existing journals. Locations come from integration ports; collector launches cannot infer a different home from their environment. Root/locator bindings stay local. Changing roots requires explicit reconciliation and invalidates lookup coverage. |
| Locator lookup | Optional integration lookup accepts qualified ID, immutable registration facts and approved root binding. Return a supported file candidate or pending/ambiguous/refused status. It resolves admitted owners; it cannot create consent or change project/destination. Share one pass-owned inventory with the source provider. |
| Opened-source admission | Reuse `SourceAdmissionSignature` and snapshot `SourceAdmissionValidator` with generic expected owner/root/start facts. Extend currently Codex-specific admission/confinement in [collector source](../../internal/collector/source.go); validate on the same confined descriptor used for filtering. |
| Child inspection | Optional bounded native inspection supplies exact child/agent/immediate-parent/root/role, creation/own-task, locator and coverage facts. Shared code owns consent, collisions, registration and durable links. |

Do not copy Codex history machinery into Muse or create competing locator,
admission, child-marker or publication stores.

### 3.2 Source resolution and bounded inventory

An admitted hook registration with no locator is the durable pending work item;
the current hook path allows this, but the file reader needs resolution. Use
ordinary file registrations for hooks, backfill and children.

1. Load registration and committed root revision. Try a known path through strict
   native validation first; unrelated inventory may remain incomplete.
2. Resolve absent/stale paths through one bounded shared traversal. Require
   supported uniqueness or a separately qualified native current pointer.
   Incomplete enumeration, eviction, filename/UUID, mtime and size cannot prove it.
3. Confine the open to the approved root; reject descendant symlinks, nonregular
   files and escapes. Validate stream identity, original creation, workspace and
   child bindings on that descriptor before retaining/filtering bytes.
4. Take the existing short update lock; compare registration/root revision,
   pause/removal window, project activation, destination and original binding.
   Recheck policy and preserve concurrent later hook observations. Enumeration
   and transcript reads stay outside `hooks.lock`.
5. Atomically persist locator and content-free binding, preserving stable IDs,
   origin, original admission and destination. A crash before commit repeats
   lookup; after commit revalidate. Continue through existing frozen publication.

Relocation requires proven continuity; conflicting copies remain explicit.
Workspace/root changes cannot silently move capture to another project,
destination or consent window. Parser checks after filtering are insufficient.
Frozen retry and retained privacy refresh work after native files disappear.

Reuse the bounded `ReadDirent` batches/cookies in
[discovery](../../internal/discovery/discovery.go) through a narrow filesystem
boundary, without inheriting Codex automatic admission/catalog/consent. Persist a
bounded, content-free cursor scoped to root/revision across ticks and restarts.
Charge unrelated entries; reserve forward progress beside fair retries so
unreadable early folders cannot starve later sessions.

Bound directory acquisition, entry/byte counts and active time. `discoveryio.Walk`
allocates a whole directory with `ReadDir` before its cap and is unsuitable
unchanged. Cache compact observations, not raw headers or parent transcripts.
Positive hints need revalidation; negative results require completed scoped
coverage. Cookie invalidation, eviction and root/membership changes invalidate
coverage. Avoid a separate full-store scan for each pending registration.

Identity facts can follow the first permission frame: use budgeted multi-record
inspection. Keep first-record-only `DiscoveryBoundedProjects` unchanged and return
unavailable there; reuse existing project/import/handoff inspection before adding
a new prefix API. Use `sourceio`/`transcriptio` and serial `SourcePass` ownership.
Initially qualified ordinary files are append-only; unsupported rewrite/truncation
or checkpoint semantics preserve prior publication and refuse/defer new capture.
A later native-index optimization needs a read-only contract that creates no
WAL/SHM and never treats rows as admission evidence.

### 3.3 Framing, privacy and retained data

Qualify envelope, transaction, marker and checkpoint versions explicitly. Preserve
exact bytes before integrity validation, logical child order, outer ordinal/
child position, stream/event identity, sequence, discriminators, versions,
timestamps and supported causal references. Validate `record_json`, child indices
and integrity; nested strings/JSON expansion share aggregate limits.

Own activity requires valid timestamp provenance and checked microsecond
conversion. Timestamp-less `session.fork.*` and `retained_marker:
omitted_live_only` records have copied/omission semantics; inherited permission
time, folder date, file time and hook observation cannot establish native birth.

After validation, flatten to a deterministic, versioned `muse-jsonl` safe retained
representation with positional provenance. Do not keep nested `record_json`
strings or certify redacted bytes with native checksums. Native integrity stays
local; the existing source-bundle SHA verifies the archived artifact. `Refilter`
and `EvidenceExtends` use safe retained evidence; parser-only refresh does not
reopen native files or feed archive serialization through the native decoder.

Allowlist supported conversation, tool, usage, naming and lineage fields; apply
shared redaction to every retained string, including arguments/results, objectives,
paths, titles and instructions. Drop hidden/encrypted reasoning, credentials,
raw provider/context dumps, arbitrary hook output and unknown payload bodies.
`muse export --redacted` is insufficient because encrypted reasoning remains.

Schema-3 checkpoints and copied `session.fork.compaction` contain `model_messages`,
`base_instructions`, `run_context_messages`, `transcript_tail`, resume state and
repeated usage. Drop private snapshot bodies, count no copied activity, and keep
only qualified bounded ownership/coverage facts. Producer restart success does
not qualify archive decoding or checkpoint frontier semantics.

| Input/result | Capture behavior |
| --- | --- |
| Complete supported evidence | Filter and publish with evidence-backed availability. |
| Unknown optional non-authority content | Drop body; keep bounded content-free coverage and mark affected metrics partial/unavailable. |
| Unsupported framing/checkpoint, invalid integrity, contradictory identity/lineage | Refuse candidate, preserve prior publication, emit fixed diagnostic; no acknowledgement beyond unprocessed evidence. |
| Incomplete final frame, late/missing child, changed file, temporary I/O | Bounded pending retry; no empty replacement. |
| Raw/record/output/graph limit | Explicit refusal/defer under existing policy; no truncation or silent cap increase. |
| Privacy-redacted content | Safe allowlisted fields with privacy/filter provenance; omissions cannot imply complete metrics. |

### 3.4 Analysis, children and ownership

Produce one pure `archive.Analysis` for metadata, transcript, handoff and eval
export. Deduplicate accepted user intent/run `started.prompt` by supported
command/run identity; use `assistant_message_committed` for visible text rather
than adding deltas/terminal copies. Capture names, model switches, tool/MCP,
shell/git and version facts with their actual availability.

`session.fork.model_prefix`/`turn` are copied context, even with full prompts and
source timestamps. Exclude inherited activity from own prompts, tools, tokens,
titles and time bounds. Fork/rewind/side provenance alone never sets
`ParentSessionID`/`NativeChild`. Typed harness notifications are not human prompts;
English prose cannot determine origin or tool failure. Pair committed calls/
results and correlate side-effect intent/task terminal within the owning run;
use failure hooks only where task coverage is unavailable.

Choose one per-call usage authority keyed by owner and supported call identity;
provider response IDs alone may repeat. Do not add cache aliases, goal/workflow/
export rollups or copied checkpoint usage. Ambiguous cumulative scope stays partial.
Verify real cache-write/cache-read/reasoning overlaps; price only models with a
dated entry, leaving unknown costs unavailable.

Children get independent normal registrations from validated spawn/binding/
observer facts in root scans and hooks. Verify the opened child stream, immediate
parent and root from explicit bindings, not depth/path or root environment.
Reconcile forwarded records; repeated stops/follow-ups update one owner. Reused
agent IDs with new session IDs create new owners. Poll admitted children
independently, including active children without stops and children outliving roots.
Task completion/final response is not permanent closure or a new-start proof.

Each child needs its own original-creation, project/source and destination consent.
Use `subagent.worktree.*` facts for included-repository mapping, not hook cwd or
path-prefix guesses. Missing parents preserve child identity and unavailable links;
never fabricate a root, birth time, ancestry or inherited permission.

Generalize currently Codex-only `NativeChild` validation deliberately and extend
the existing [native linker](../../internal/collector/native_links.go).
Qualify link keys by agent/native ID, project/destination and source-root binding;
use the owning agent's removal namespace. Keep Codex legacy composite migration
Codex-only. Queue durable link work before marking completion; retry across
parent absence, retention/undo and child publication without new parent bytes.
Reject cycles, self-parenting, conflicting roots and ambiguous owners. Preserve
Claude's different child provenance and regressions.

Store child own metrics and reuse the existing stats rollup: root session/prompt
counts remain root-only; child usage/tools roll into the root once. Keep current
date-window, orphan and replay behavior. Root own metrics can be measured while
descendants are unavailable; expose missing coverage in links/gaps and claim
exact tree totals only for a complete controlled snapshot. Roles distinguish
delegated/workflow/observer sessions without consent shortcuts; add versioned
public role/coverage fields only if existing outputs cannot represent them.

### 3.5 Hooks, setup and operational behavior

Configure `SessionStart`, `UserPromptSubmit`, `Stop`, `StopFailure`, `SessionEnd`,
`SubagentStart` and `SubagentStop`; qualify `PostToolBatch` only if needed for
long-running activity. Supported startup/clear facts can establish freshness;
resume/compact continue owners. Prompt/stop and null locators cannot admit an
unknown old session. Cross-check payload and root/thread environment identities.

Inject only declared bounded environment observations into `HookInput`, convert
them to content-free lifecycle facts and discard raw environment. Keep the decoder
pure: no directory walks, transcript reads, native CLI, filtering, upload or
networking in hooks. Hooks stay fast, silent and nonblocking.

Generate absolute executable/`AGENT_ARCHIVE_HOME` commands; native roots come
from committed archive settings rather than a duplicate hook prefix. Use
expected-byte `filechange.Change` plans and setup journals for installation,
inspection, refresh, rollback and removal. Preserve foreign handlers/settings/
managed files; ordinary machine-wide capture must not require project trust.
Do not invoke Muse settings/skill writers that dropped unrelated MCP fields,
or repair foreign configuration automatically.

Replay markers are launch-scoped and dynamic, never a fixed ID in ordinary hooks.
An optional runner-owned managed file can forward `AGENT_ARCHIVE_REPLAY`.
Qualify the exact generated command, quoting and environment prefixes across
root/child/observer/TUI/serve/recovery modes; an allowlist alone is insufficient.
If forwarding fails, keep replay capability unavailable. Report every owned
configuration file and preserve existing managed configuration.

### 3.6 Import, readers, skills and handoff

Reuse purpose-specific headers, inspection, project paths and child enumeration
for explicit backfill and pre-setup find/show/handoff. Local reads need no storage
setup and must not enable archiving. Backfill previews exact scope/date/duplicates/
refusals/gaps; batch undo removes only its ownership/link effects, preserving
independent live capture, later continuation and native files.

All reader, title-search, text/JSON/HTML statistics and eval-export views consume
shared Analysis and agree on identity, own activity and gaps. Preserve existing
root/child and replay defaults. Missing retained files cannot be recovered in
logging-disabled modes; report this only when native evidence establishes it.

Runtime detection prefers injected exact thread identity over project-latest
fallback. Fixed `muse --version` probes distinguish semver/SHA from R-build display;
an unstamped child version cannot be called directly observed evidence.

Declare installation and evidence roots separately. Deduplicate shared
`.agents/skills` installation with Codex/Cursor. Config-local, project, foreign and
plugin roots need trust/activation/eligibility/use qualification. Inventory proves
installed only; invocation/read hashes require owner/identity mapping. Unsupported
eligibility cannot become observed-no-use.

Archive-to-Muse handoff uses a new conversation and the existing safe context
artifact; test argv/positional prompt, Unicode/metacharacters, quoting, terminal
launch and artifact reads under ordinary permissions. Muse-to-each-existing-agent
handoff uses safe normalized evidence. Do not add bypass flags. Native resume is
separate; an exported log is not proof of portable resumption.

## 4. Tests required before claiming parity

All tests must assert outcomes, counts and availability through normal registry/
consumer paths, not just isolated decoding or result labels. Reuse applicable
`internal/testutil/agenttest` source/discovery/import/skills/runtime/launch suites,
the shared `-update` flag and existing fourth-agent regressions.

| Area | Required cases and assertions |
| --- | --- |
| Admission and roots | Fresh versus unknown resume; copies/forks/inherited time; excluded/nested/reactivated projects and destination/removal windows. Null locator and restart before/after commit; concurrent stop/setup/pause/removal; replaced files, outside-root/symlink/nonregular/mismatched streams. No filtering or PUT before same-handle validation. Multiple installations and cross-agent ID/root collisions. |
| Inventory and fairness | Store larger than a pass budget; late/unrelated entries, unreadable folders, restart/cookie invalidation, root change, eviction and conflicting copies. Prove eventual coverage and bounded directory/header operations, not guessed uniqueness or repeated session-times-store work. Valid sessions progress beside retries. |
| Framing and privacy | Mixed frames, child order, retained markers/copied prefixes/checkpoints, nested expansion, bad integrity/versions, duplicate keys/IDs, sequence conflicts, invalid UTF-8, blank/missing-newline/partial lines, timestamp overflow and contradictory authority. Fuzz allocation bounds. Secret sentinels in every nested argument/result/title/objective/instruction/path; no reasoning/encrypted/context dumps remotely. Exact refusal/gap/partial-metric behavior. |
| Conversation and usage | Exact own prompts/messages/turns/text, Unicode/multiline, streams/retries/deltas/harness notifications, names/automatic titles/model switches and metadata-only refresh. Own per-model calls/cache/reasoning/cost availability; no copied or rollup duplicates. Retained refilter and parser refresh with native files removed. |
| Children and totals | Direct/workflow/observer/grandchild success, failure/cancel, missing/late logs, no-stop/lost-stop active child, parent exit/absence, follow-up/reopen/reused agent IDs and forwarded records. Explicit ancestry, cycles/self-parenting/conflicting roots; one stable owner and no root fallback. Parent arrival links without new parent bytes. Root-only prompts plus child own tokens/tools, window/orphan/replay rules and incomplete tree coverage. |
| Tools, MCP and skills | Committed parallel/success/error/cancel/interrupted calls, structured/oversized results and owning-run task pairing. MCP canonical IDs, stdio/HTTP/OAuth/plugin/retry attribution. Shared-root deduplication, trust/activation/foreign/plugin gates, invocation versus read and installed versus eligible evidence; foreign config survives. |
| Projects and Git | Shared/isolated/missing/deleted worktrees, clean/dirty starts, branch changes, detached checkout and Git unavailable. Native worktree evidence controls included-repository mapping; hook cwd cannot bypass scope. |
| Setup and compatibility | Foreign hooks/managed files/settings preserved byte-for-byte; quoting/custom XDG/archive roots, payload/environment disagreement, lock contention and interrupted setup/rollback/remove journals. Explicit opt-in only. Current/old-reader and old-writer upgrade/downgrade matrices with registrations, imports and pending journals; no lost authority. |
| Backfill and undo | Date/project/root/child selection, duplicates/conflicting copies, restart/budgets and live continuation/import overlap. Exact batch undo preserves later independent ownership and acknowledged/native data. Pre-setup access causes no archive mutation. |
| Readers, handoff and replay | Text/JSON/HTML/eval-export agreement on IDs/counts/gaps/roles; root/child selections and visibility defaults. Each existing agent to Muse and back, safe artifacts/argv/terminal behavior, Unicode/metacharacters. Dynamic markers through root/observer/workflow/grandchild/TUI/serve/restart; ordinary sessions stay ordinary. |
| Storage and maintenance | Inject before/after local state, staging, source PUT, metadata PUT, verification, acknowledgement and cleanup. Restart/native source loss preserve staged bytes, destination and ownership; new bytes cannot replace frozen retry. Read-back checksums; privacy/exclusion across every referenced source; root/child expiration, unresolved links and protected pending stages without dangling acknowledgement. |
| Native modes and versions | Fresh/second-turn/known-resume/unknown-resume, failure, orderly exit, SIGINT/SIGTERM/hard kill with late writes across TUI/exec/serve. Fork/rewind/side, manual/default/periodic compaction and resume, including prune/rewrite/split. Logging-disabled/ephemeral behavior. Supported producer upgrades, future records and real macOS/Linux architectures; no Windows claim. |

The corpus records exact build/provider/procedure, raw and sanitized hashes,
redactions/omissions, framing and expected identity/graph/text/usage. Sanitization
changes checksums: exact reviewed synthetic frames can certify native observations;
derived mutations cannot certify valid encoding. Probe deadlines/process cleanup
are explicit; another fresh-root run must reproduce all claimed assertions.
Wait for actual owning child settlement and preserve refused/timed-out attempts.

Start deterministic integration acceptance locally, then run a bounded real Meta
campaign and disposable storage publication/read-back. Record exact source/CLI,
argv, roots, hooks/native files, expected facts, archive IDs, remote checksums,
read-back and `status --json`. Verify each child separately. Real provider access
and disposable destination must be configured before those tests; synthetic usage
does not establish billing accuracy.

### Resource and performance gates

Measure operation counts/allocation bounds first, then wall time and RSS on named
reference machines. `NativeReadBudget` limits charged memory, not total disk reads
or RSS. Reserve before allocation, charge shared lookup/graph caches under current
`PassReadBudget`/`PassCacheReserve` and release with snapshots/pass lifetime.
Verify combined Codex/Cursor/Claude/Muse pressure and ordinary-session headroom;
do not add another cache reserve or silently raise caps.

| Workload | Required invariant |
| --- | --- |
| Many cold pending roots | One bounded traversal epoch shared by owners, progress across ticks/restarts, bounded total directory/header work. |
| Warm unchanged owners | Cheap confined identity/signature checks; no full filtering, metadata PUT or whole-store enumeration merely because a tick ran. |
| Growing root with many children | One parent analysis per changed scan, shared compact bindings and independently charged child reads; no full parent read per child. |
| Large directories/records/checkpoints/refilter | Bounded acquisition and raw/decode/output/graph expansion, no full context cache; disk bytes, visited records and active time bounded independently. |
| Missing/ambiguous beside valid owners | Fair bounded retries with visible pending state; independent valid capture continues. |

Restore existing 1/128/512-session and full raw/record/source limit cases under
current tier policy; add larger-than-budget inventories and long live graphs.
Short-mode success does not prove full-scale behavior. Measure hook latency and
collection impact on native tools. An offset tailer or incremental cache is a
later optimization only after profiling and proving identical authority/
refilter/rewrite/checkpoint semantics.

Existing evidence: one warm scan of 1000 settled synthetic Codex registrations
took 3.130s, allocated 32,359,360 bytes/297,756 objects and performed zero filters,
bundle decodes or remote reads on Intel i7-8559U/macOS amd64. Whole-command time
was 589.089s including untimed fixture setup/initial publication. This establishes
an existing reuse property, not Muse performance or a universal latency target.

## 5. Enablement, release gates and unresolved work

| Gate | Owner and evidence needed |
| --- | --- |
| Integrity and original creation | Q0, then P1/P2/P3: producer contract or independently verified encoding against exact valid/mutated frames; root/child/branch creation through copies/resumes/checkpoints/missing parents. Blocks affected capture enablement. Producer clarification may be necessary. |
| Compaction and recovery edges | Q0/P5: qualified default/manual/prune/rewrite/split/fork/rewind/TUI gestures and closed-child states. Current history-preserving checkpoint covers only its recorded case. |
| Real accounting | P5 with locally configured provider access: dated actual model/cache/reasoning/retry/background/compaction attribution and pricing. |
| Platform/version support | Release owner/P5: actual supported native macOS/Linux architectures with pinned producer/runtime; cross-builds alone are insufficient. |
| Adapter and user surfaces | Each phase owner/P5: implemented privacy/admission/storage/undo/retention/resource campaign; exact skill/MCP/replay/handoff coverage before advertising it. |

Enable Muse only through explicit setup selection, never installation, shared
skill presence or refresh. Persisted source-root/child authority changes need
schema and writer protections together: old writers must refuse or preserve them.
Test actual old-reader metadata access where supported; otherwise require upgrade.
Rollback/removal disables new capture and removes only owned resources, preserving
native state, bindings/consent, archives and frozen/pending ownership. No destructive
migration, separate retry loop or self-updater.

Use existing fixed status/capture-gap reasons for pending locator, incomplete
coverage, unsupported format, unresolved child, missing metric and verified
publication. Diagnostics expose versions/counts, not prompts or arbitrary native
error strings. Presence, version, hooks, events, local capture, publication and
read-back remain distinct.

Follow [testing](../contributing/testing.md) and
[version rules](../maintainers/versions.md) on the final ancestry: required CI,
race/vet/lint, architecture boundaries, fuzz, reviewed goldens, platform/release
checks. Allocate parser/adapter/filter versions when implementing; update schemas
for role/lineage changes and keep archived sessions readable. Update README/help,
setup/status docs, JSON contracts, capture capabilities and spec index with actual
scope. Shared changes must preserve Claude/Codex/Cursor admission, ownership,
readers, visibility and publication.

No upstream Muse change is assumed. Populated hook transcript paths and explicit
immediate-parent/root IDs would simplify resolution; missing facts cannot be
replaced by fabricated completeness. If a gate cannot be resolved, leave the
capability unavailable and identify its required evidence.

## 6. What native qualification established

### Environment and evidence limits

Tests ran on 2026-10-14 on macOS x86_64 with Muse `1.4.7-R6101.1`
(native SHA `d8c53d25bd`) and `1.4.8-R6256.1`. Official binaries, config/data/cache
and disposable workspaces were isolated in temporary storage. The 1.4.8 download
matched Meta's advertised size/SHA-256. A loopback endpoint supplied deterministic
model/catalog responses; filesystem writes and shell tools were disabled under
ordinary permissions. Native worktree creation/cleanup was exercised separately.
No personal settings edits, real Meta calls, paid requests or archive uploads ran.

The host had no configured Meta authentication or usable Linux VM/container
runtime. Native Linux/arm64 remains untested; cross-builds do not establish it.
An official local stdio filesystem MCP server, version `1.0.0`, ran with Node
24.19.0 against synthetic files. Broader transports and plugins remain unqualified.

Repository design/contract checks used HEAD
`7d8aee13c7146ceb38d15df7188d7daa31f464eb`. Documentation was rebased onto
`4f94edb97f992f408521928cd1227d2780c6401c`. Between those heads, Codex
history/refusal, source encoding/budgets, discovery/read bounds, picker/handoff and
CI contracts changed; reconcile them at Q0. Historical native evidence does not certify a newer
implementation base.

### Observations that determine the design

| Area | Established result and limit |
| --- | --- |
| Retained storage | Roots use `$XDG_DATA_HOME/muse/sessions/YYYY/MM/DD/<session-id>/session.jsonl`, default data home `~/.local/share/muse`. Every observed hook had `transcript_path: null`. Root export lacked actual child content; retained files require independent resolution. |
| Identity and ancestry | Logical agent IDs differ from child session IDs. `spawn_accepted`, `child_session_bound` (`nested_session_v1`), `parent_session_id` and agent-lineage admission supplied explicit edges. Children and grandchildren were flat under `<root>/subagent/<child-id>/session.jsonl`; both depths could be 1. Forwarded control records repeat across logs. |
| Hooks and environment | User hooks use `$XDG_CONFIG_HOME/muse/settings.json`; project hooks use `.muse/hooks.json`. `MUSE_SESSION_ID` identifies the root, `MUSE_THREAD_ID` the current thread, and child payload `session_id` the child. User hooks dropped custom XDG/replay variables. Direct managed commands forwarded the allowed marker to root/children on 1.4.8; compound shell instrumentation lost extras on both builds. |
| Root lifecycle | Fresh TUI/exec/serve turns and exec resume were exercised. Resume kept the same ID and raw prefix, sometimes without another `SessionStart`. Root `session.opened.observed` with `resume: false` is candidate creation evidence, not proof for copies/forks/checkpoints. `--no-session-log` and ephemeral serve created no retained file; ephemeral `session/read` was unavailable. |
| Child lifecycle | Successful workflow completion required `submit_result` with text and nullable notes. Direct child follow-up settled a new `result_ready` on 1.4.7; close passed after settlement. Closed-child reopen was refused with `subagent_state_ineligible`. Hard-kill parent recovery and 1.4.7-to-1.4.8 upgrade preserved root/child prefixes and settled another follow-up. These are producer tests, not archive recovery tests. |
| Grandchildren and observers | Explicit grandchild graph passed on 1.4.7; the newer compound-instrumented orderly run timed out. Observer links included `memory_reminder_child_session_linked` and sometimes relative paths. Goal/verification files appeared; a skill link did not prove a retained file. Absence versus late/disabled logging needs qualification. |
| Isolated worktrees | Native prepared/lease/cleanup records passed, including 1.4.8. Child hook cwd still named the source checkout. Archive project/consent mapping is unimplemented. |
| Framing and integrity | Ordinary envelopes, `session_permission_transaction` with `children[].record_json`, omitted-live-only markers and timestamp-less fork records coexist. Exact schema-1 permission bytes exported cleanly; checksum, whitespace/key-order and other mutations produced an unparseable-line diagnostic while exit stayed 0. Canonical/checksum encoding remains unknown. |
| Checkpoints and branches | A 1.4.8 `context_projection_checkpoint` schema 3 with history `preserved` was produced using the opt-in 4096-byte setting. It includes private context/resume state and repeated usage. A corrected same-endpoint restart returned in 4.857s, preserved the prefix and completed another turn; the earlier timeout remains unexplained. A side conversation copied `session.fork.compaction` and continued independently. An older fork cut was refused as `forkBoundaryInvalid`/`runtime_checkpoint_tail`. |
| Compaction and TUI limits | Manual MSP compaction returned `compaction_unavailable` on both builds, including strategy/context-limit probes. Earlier 1.4.7 periodic probes produced no checkpoint. TUI startup passed; two later compact/quit attempts missed their readiness trigger. Default/manual compaction, prune/rewrite/split behavior and orderly TUI gestures are not qualified. |
| Tools and MCP | Committed `call_id`/`tool_call_id` pair. Actual local stdio success returned the sentinel; missing-file execution failed. Join `tool:<call-id>` side-effect intent to the owning run's task terminal for typed status; result text has no typed error flag. The first bare MCP alias was unknown, not an executed MCP failure. |
| Usage | Controlled input 101, output 23, cached 17 and reasoning 5 appeared in `model_completed`. `cached_tokens` and `cache_read_tokens` both equaled 17; aliases and goal/workflow summaries must not be added twice. Real cache/reasoning overlap, billing and retry attribution remain untested. |
| Skills | Project catalog evidence depended on workspace trust. Successful `read_skill` produced `skill_read_observed` with `evidence_kind: read_skill_tool` and a hash. Native `user-only`/`disable`/`enable` exited 0 but dropped unrelated MCP type/cwd/required/timeout fields while preserving foreign hooks. Full activation, foreign-root and plugin eligibility remain gates. |
| Names and handoff | Corrected MSP initialization required `initialized` after `initialize`; rename/read and 1.4.7 fork continuation passed. A synthetic context artifact outside the workspace was readable under normal permissions. Actual cross-agent launching, title refresh and portable native resume are not established. |

Failed attempts, corrected assertions and untouched cases remain distinct in the
probe data. Command acknowledgement or model-call counts do not establish settled
child results. SDK/web-preview wire schemas differed from the tested binaries:
use each binary's offline export for probe parameters, including required resume
`commandId`. Stable and experimental fingerprints are separate; schemas/exporters
do not specify the retained-file integrity contract.

## 7. Evidence and reproduction details

The [single evidence file](muse-qualification-evidence.json) preserves the initial
1.4.7 campaign, later 1.4.7/1.4.8 attempts, corrected outcomes, hashes and scoped
resource/fixture results. Raw model requests, private context/full logs and probe
helpers remain temporary; hashes alone are not a reproducible CI runner.
Authoring that runner and the complete reviewed corpus is Q0's first deliverable.

The [fixture manifest](muse-qualification-fixtures/manifest.json) pins binary and
file hashes. Its exact schema-1 permission excerpt contains no model context or
private home path; the invalid-checksum excerpt is derived. Neither is a complete
session fixture. With the same checksum-verified temporary CLI, reproduce:

```sh
/private/tmp/agent-archive-muse-qualification-20261014/bin/muse-1.4.8-R6256.1 --version
/private/tmp/agent-archive-muse-qualification-20261014/bin/muse-1.4.8-R6256.1 export --session dev/specs/muse-qualification-fixtures/permission-frame-1.4.8.jsonl --out /private/tmp/muse-permission-valid-export.json
/private/tmp/agent-archive-muse-qualification-20261014/bin/muse-1.4.8-R6256.1 export --session dev/specs/muse-qualification-fixtures/permission-frame-invalid-checksum.jsonl --out /private/tmp/muse-permission-invalid-export.json
```

Assert `diagnostics.unparseable_lines` is 0 for the original and 1 for the mutation;
both may exit 0. A temporary CLI may disappear. Native acceptance of the original
and rejection of mutations do not establish independent checksum encoding.

Reusable registry/fourth-agent (including race), source/snapshot, bounded
traversal/retry and native-link/late-parent/Codex-legacy checks passed at the
recorded review HEAD; commands are preserved in the evidence file. They validate
extension points, not unimplemented Muse behavior. Document links, fixture hashes
and whitespace were checked during documentation publication.

Primary references: [extending/hooks](https://dev.meta.ai/docs/muse-code/extending),
[audit/resume](https://dev.meta.ai/docs/cookbook/audit-agent-sessions),
[interactive behavior](https://dev.meta.ai/docs/muse-code/interactive),
[SDK/protocol fixtures](https://github.com/meta-models/muse-code-sdk),
[hook events](https://meta-models.github.io/muse-code-sdk/next/guides/plugins/reference/hook-events/),
[settings hooks](https://meta-models.github.io/muse-code-sdk/next/guides/extend/hooks/),
[skills](https://meta-models.github.io/muse-code-sdk/next/guides/extend/skills/),
[durability](https://meta-models.github.io/muse-code-sdk/next/guides/msp-concepts/durability-profiles/),
[SDK preview](https://meta-models.github.io/muse-code-sdk/), and
[changelog](https://dev.meta.ai/docs/muse-code/changelog). Preview documentation
had older-build assumptions; pin evidence to actual native builds/formats.
