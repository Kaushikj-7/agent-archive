# Muse Code integration plan

Status: proposed. Add Meta's Muse Code as a fourth agent in agent-archive,
alongside Claude Code, Codex and Cursor. Native investigation established
feasibility; production capture, privacy, publication and full parity are not
qualified. A separate runtime-presence subset detects noninteractive shell
commands and clears inherited session keys when launching supported agents;
[its evidence and limits](../qualification/muse/runtime-presence.md) do not enable
capture, exact current-session selection or any other Muse operation.
Explicit-file local reading now adds the strict source/filter and retained-only
own-conversation parser described below. Capture and full parity remain pending.

This is the single implementation plan. Qualification findings and design
decisions are incorporated below, and §6 and §7 record the probe results the
design depends on.

## Delivered local subset and residual ownership

The first production source slice combines original P1 source work with the
minimum P4 own-conversation interpretation needed by the real existing
`handoff --file PATH --harness muse` path. It supports complete ordinary exec/MSP
roots, `muse-jsonl` v1, safe own main/chat prompts and committed assistant text,
reserved logical creation time, retained refilter and existing destinations.
Qualified own input/output observations are also available as a separate partial,
unpriced summary. Local snapshots supply exact content proofs and provisional
startup facts; protected binding v2 supports prepared validation checkpoints,
while real capture and empty-locator resolution remain unavailable.
Source/Filter, Parser, Runtime and local handoff Discovery/NativeHeaders/Preview
are bound. Collection remains
refused before native reads/filtering/PUT, including injected registrations.
No durable binding or consent is created. The synthetic 1.4.9 exec held-out has
two own turns; 1.4.9 MSP root evidence has zero turns. Conversational MSP
interpretation is synthetic plus the separate 1.4.8 evidence, not new 1.4.9
native conversational acceptance.

Original P1/P4 residual requirements remain: qualified usage authority, cache,
reasoning/cost, task/call and MCP/Git/tool pairing, model switches, complete
export/view agreement, complete names/version/skills, native show/search, launch
into Muse and replay. Complete accounting and these richer facts remain
unavailable in the current local output; partial input/output observations do
not qualify them. A finite own committed-call slice now retains minimal
call IDs and exact prior result joins, with observed invocation counts only for
nonempty enriched sources without erased legacy tool rows. Blank calls remain
unlisted in narrative handoff; normalized result objects and result/error counts
remain omitted to preserve older typed consumers. Native qualification is one
single-item case; multi-item/partial/reordered grammar has synthetic coverage.
P2 capture/admission/storage/read-back and P3 child,
checkpoint/rewrite, platform, privacy/retention and resource/release gates retain
their original owners and acceptance below. [Automatic handoff](../qualification/muse/local-handoff.md)
now uses one qualified effective XDG data home, bounded
startup corroboration and safe name/activity/ID previews; selected sources still
require complete strict validation. A shared resumable inventory now supplies bounded content-free directory
observations for already admitted empty-locator owners. Per-owner fresh metadata
currentness uses the same allowance; bound owners bypass it. Actual no-request
collector resolution through strict Muse source/filter/proof and guarded first
binding is tested with a private injected producer. Public source capture still
refuses before content reads. Capture is a later separate slice. This
partial delivery does not redefine full integration success or qualify capture.

## 1. Scope and success criteria

A root is a human conversation. A child is an independently identified delegated,
workflow or observer conversation. A fork or side conversation may copy context
without being a delegated child.

The full baseline is capture of included projects through hooks, qualified
children, explicit backfill, bounded local access, readers, statistics and export,
skills and runtime detection, handoff and replay. It must preserve existing
consent, privacy, crash recovery, publication, read-back and retention guarantees.
Use the existing [capture capabilities](../../docs/reference/capture-capabilities.md)
as the comparison: Cursor child capture is unavailable, Claude child evidence
includes synthetic fixtures, and Codex has its own acceptance limits. Muse must
not enlarge another agent's evidence claim.

Initially support one explicitly reviewed Muse data home, ordinary file sources
and the existing collector, registration owner, storage pipeline and stats rollup.
Automatic admission of missed-start sessions is a separate follow-up, absent
initially; Codex discovery or all-projects consent must never enable Muse.
Multiple data homes, a native SQL index, a daemon, per-pass CLI exports, raw-offset
tailing and an incremental metrics database require measured need and separate design.

A capability ships only when its tests pass for the advertised formats, launch
modes and platforms. Missing metrics remain unavailable or partial rather than
zero; installed or configured status does not mean verified capture. Unsupported
long-context formats must stop capture safely and be documented; changing Muse's
compaction or observers to pass tests does not qualify default behavior.

### First release: root capture

The planned first release ships qualified root capture alone, as Cursor shipped
with `SubagentLinkage` unavailable. It requires Q0, P1, P2 and the root-scoped part
of P5 acceptance: hook admission, locator resolution, filtering, publication and
read-back for TUI, exec and serve roots on the supported platforms. The
four-event hook qualification alone does not satisfy those mode/platform gates
or qualify TUI or MSP/serve capture. Its planned
[`CaptureCapabilities`](../../internal/agentapi/capabilities.go) are:

| Dimension | First-release state |
| --- | --- |
| `fresh_start`, `transcript`, `lifecycle` | `fixture_validated` for qualified root formats only. |
| `subagent_linkage` | `unavailable`. Roots report child coverage as a gap; child activity is not counted. |
| `skill_evidence` | `unavailable`. |
| `adapter_fixtures` | `fixture_validated` with reviewed synthetic root fixtures. |

The first release installs only the four qualified root events: `SessionStart`,
`UserPromptSubmit`, `Stop` and `SessionEnd`. Startup is an intent-only
`FreshUnknown` observation; source proof and current consent precede ownership
(§3.2 and §3.5). `PostToolUseFailure`, `StopFailure` and subagent hooks require
separate qualification before installation; subagent capture belongs to P3.
Published roots appear in archive readers, statistics and eval export through
the shared Analysis, with their own tool calls and usage. Backfill, pre-setup
local reads, runtime and skill detection, replay and handoff into Muse are not
registered operations yet. Each later phase adds its capability only when its own
gates pass. Full parity requires every baseline gate in §5.

## 2. Implementation sequence

| Phase | Deliverables and dependency | Exit gate |
| --- | --- | --- |
| Q0: qualify authority | Reconcile the shared ports in §3.1 against the current base and freeze their signatures. Pin producer inputs. Check in a runnable isolated loopback, PTY and MSP probe and a reviewed corpus with exact assertions and provenance. Resolve integrity, original root creation and the session folder's date rule; resolve child and branch creation and checkpoint frontiers before the phases that need them. | Reproduce claimed cases from fresh roots without real provider calls or personal context. Unknown authority blocks only the affected format or capture lane. |
| P1: source and analysis | Q0. Builtin identity and binding, the agent-neutral data-root, locator and admission ports, a confined file source, the frame decoder, the `muse-jsonl` retained format and one Analysis. Q0's reviewed frames become decoder fixtures in `internal/agents/muse/testdata`. | Conformance and fuzz tests; exact content, usage and ownership; bounded expansion; retained refilter without native files. |
| P2: root capture and setup | P1. Reviewed data root and revision, guarded startup intents, shared inventory candidate queries, complete source/current-consent proof, prepared atomic first owner plus binding, four qualified hooks and transactional setup and status. | TUI, exec and serve publication and read-back; consent, race and restart tests; existing-agent regressions. With root-scoped P5 acceptance, this is the first release. |
| P3: native children | P2. The extended child-inspection port, independent child owners, explicit bindings and roles, subagent hooks, active polling, agent-qualified durable linking and the existing stats rollup. | No-stop, failure, follow-up, close, recovery, grandchild, observer and worktree cases; no inherited consent, cross-agent links or duplicate totals. |
| P4: access and user surfaces | P2, plus P3 where descendants are promised. Backfill and undo, pre-setup local access, skills and version detection, name refresh, child and role views across readers and export, launch, handoff and replay. | Exact undo and owned-file changes, agreement across outputs, bidirectional launch and evidence-backed skills and replay. |
| P5: acceptance and release | The phases a release claims. Combined independent acceptance, the native, provider and platform matrix, privacy, storage and retention, compatibility and measured resources. | Every advertised capability passes the tests below on the final source. Unavailable capabilities remain explicit. |

Shared ports and persisted fields have one writer at a time. A change to the
source format, a shared port, a dependency or the base invalidates the evidence
that relied on it; rerun the affected Q0 assertions before continuing.

## 3. Implementation contracts

### 3.1 Agent identity and shared boundaries

Add canonical `agentmeta.Muse` (`muse`), alias `muse-code`, display `Muse Code`,
and one binding in `builtin.NewBuiltins`. Keep native vocabulary in
`internal/agents/muse`; shared consumers use narrow `agentapi` ports. Follow
[adding an adapter](../contributing/adding-an-adapter.md) and
[integration contracts](implemented/agent-integration-contracts.md).
Register supported operations only.

Use exact session IDs qualified by agent for root and child keys. Agent, workflow,
attempt, parent and root IDs are related facts, not interchangeable conversation
keys. Preserve opaque IDs; validate separately anything used as a path component.

Muse needs four shared boundaries. Each is modeled on a port that exists today
for Codex or Claude, so there is one shape per concern rather than a Muse copy:

| Shared boundary | Existing port it builds on | Required extension |
| --- | --- | --- |
| Native data root | `discovery.codex_homes` in [config](../../internal/config/discovery.go) | Setup commits the absolute configured spelling, canonical root and a revision through existing journals. Locations come from integration ports; collector launches cannot infer a different home from their environment. Root and locator bindings stay local and are never published. Changing the root requires explicit reconciliation and invalidates lookup results. Codex homes stay under discovery because they carry automatic-admission consent that Muse must never inherit; this is new config state, not a replacement for them. |
| Locator lookup | `CodexRolloutLookup` and `SourceEnvironment.CodexRollouts` in [agentapi](../../internal/agentapi/codex_rollouts.go) | One agent-neutral lookup slot whose result has the same shape as `CodexRolloutSet`. It resolves already admitted owners and, in combined PR6, explicitly authorized startup candidates distinguished by purpose; it cannot create consent or change project or destination. Every result is an untrusted hint revalidated on the opened handle. Muse is its first implementation; moving Codex's lookup and validation slices behind it is a follow-up outside the first release, so working Codex capture is not on that path. |
| Opened-source admission | `SourceAdmission`, `SourceAdmissionValidator` and `SourceAdmissionFacts`, whose binding is `*archive.CodexSourceBinding` | In memory the binding becomes agent-neutral, converted from `codex_binding` for Codex registrations at the `SourceAdmission` boundary. Reuse `SourceAdmissionSignature` and validate on the same confined descriptor used for filtering in [collector source](../../internal/collector/source.go). |
| Child inspection | `ChildDiscoverer` and `ChildCandidate` in [agentapi](../../internal/agentapi/children.go) | Optional parent, root, logical agent ID, role and coverage facts on each candidate. Zero values keep Claude's current behavior. Today only backfill calls `ChildDiscoverer`; P3 adds the first live-capture caller. Shared code owns consent, collisions, registration and durable links. |

The sketches below describe the shared design shape; existing source/binding and
inventory foundations are delivered as described in the local-subset status
above. The candidate purpose is an approved future combined PR6 extension, not
an existing callable API. Freeze exact final interfaces after source qualification
and before PR6 dispatch:

```go
// archive: one reviewed data home per agent that needs one; config stores it.
type NativeRoot struct {
	Agent      string `json:"agent"`
	Configured string `json:"configured"` // absolute spelling the user approved
	Canonical  string `json:"canonical"`
	Revision   string `json:"revision"`
}

// agentapi: the agent-neutral slot in SourceEnvironment.
type NativeLocatorLookup interface {
	Locate(context.Context, LocatorRequest) (LocatorResult, error)
	Check(ctx context.Context, nativeID, revision string) error
}

type LocatorRequest struct {
	Purpose  LocatorPurpose // default admitted_owner; PR6 adds startup_candidate
	NativeID string
	Root     archive.NativeRoot
	Parent   *SourceRef // validated parent locator, for children
	Observed time.Time  // hook observation; a lookup hint, never creation evidence
}

type LocatorResult struct {
	Status     LocatorStatus // found, pending, ambiguous or refused
	Current    *SourceRef    // a qualified native current pointer, when one exists
	Candidates []SourceRef
	Revision   string
}

// archive: content-free and versioned. Persisted as native_binding for
// non-Codex agents; Codex keeps persisting codex_binding.
type NativeSourceBinding struct {
	Agent           string    `json:"agent"`
	Version         int       `json:"version"`
	NativeID        string    `json:"native_id"`
	RootID          string    `json:"root_id,omitempty"`
	ParentID        string    `json:"parent_id,omitempty"`
	NativeCreatedAt time.Time `json:"native_created_at"`
	Cwd             string    `json:"cwd"`
	StreamID        string    `json:"stream_id,omitempty"`
	Path            string    `json:"path"`
	RootRevision    string    `json:"root_revision,omitempty"`
}

// agentapi: the last five fields are new and optional.
type ChildCandidate struct {
	NativeID       string
	Source         SourceRef
	Bytes          int64
	ParentNativeID string
	RootNativeID   string
	SubagentID     string    // logical agent ID, never the session key
	Role           ChildRole // delegated, workflow or observer
	Coverage       ChildCoverage
}
```

Do not copy Codex history machinery into Muse or create competing locator,
admission, child-marker or publication stores.

### 3.2 Source resolution

In combined PR6, the durable pending work item for a new root will be a bounded
content-free
`startup_candidate` in the existing typed admission queue. It is not an admitted
registration. Before original native start T, current consent and complete source
proof, do not call `RegisterOrMerge` or `EnsureArchiveSessionID`, reserve an ID,
set `AdmittedAt`, or save session-specific request/evidence. Already validated
owners retain their separate lifecycle and continuity path.

Muse stores roots at `<data-root>/sessions/YYYY/MM/DD/<session-id>/session.jsonl`
and children at `<root-dir>/subagent/<child-id>/session.jsonl`. Date and UUID
shape are locator hints, never original-start or uniqueness authority. Resolve
new candidates and existing pending owners through the same shared inventory:

1. Freeze recognized currently eligible startup intents alongside admitted-owner
   queries in the existing `NativeLocatorPass`/`NewNativeInventory` composition
   delivered by PR625. Add a finite purpose: default `admitted_owner`, explicit
   `startup_candidate`. Unknown purpose, cross-root and parent-bearing startup
   queries refuse. Keep captured root/window/config generation, policy and queue
   identity in collector-owned context; inventory hints grant no authority. Do
   not create fake persisted owners or a second resolver/census. Deduplicate
   namespace work while retaining each intent's guard/idempotency identity. Charge
   candidate metadata to existing queue/query/pass limits before allocation; keep
   the existing shared budgets and cache reserve.
2. Already-bound owners open their known confined path without whole-store
   enumeration. Cold pending queries require complete scoped namespace coverage
   and fresh per-selected-ID directory currentness under the shared allowance.
   Date probes cannot exclude conflicting copies elsewhere. Muse currently has
   no qualified native current pointer: `Current` stays nil; a found result needs
   exactly one candidate and a nonempty live selection token, checked after reads
   and before commit. Incomplete/ambiguous coverage remains pending/refused.
3. Open the selected same confined descriptor; reject symlinks, nonregular files
   and escapes. Validate strict original T, identity, ordinary-root cause and
   workspace, then check T against the candidate's original captured window,
   which must still match current root/project/destination/pause/config policy.
   Check the same physical included project before full Filter. Hook/opening,
   UUID and folder times cannot replace T. An old unknown root refuses before
   full Filter, owner/index/reservation or session effects.
4. Run complete concrete Filter with existing source, record, graph, privacy and
   resource limits. Retain snapshot/input/filtered leases through staging and
   independent final complete-boundary/selection verification. Missing/partial,
   unsupported or changed proof remains pending/refused without ownership.
   Native reads, hashes, filtering, project inspection, staging and fsync stay
   outside `hooks.lock`; an expired live token closes the attempt without
   renewing allowance under a held snapshot.
5. Use a distinct prepared first-admission operation for an expected nil owner.
   `CommitNativeValidation` continues to require an existing owner. Bind the
   exact guarded intent, immutable `NativeAdmission`, native key, root, complete
   v2 binding/checkpoint, original start and file locator to a nonforgeable
   prepared value and independent verifier. Stage the complete registration and
   existing index inputs outside the lock, proposing an archive ID without an
   indexed reservation. Reverify after staging and on bounded restaging retries.
6. Under existing ordered state locks, compare captured and current root/window/
   config generation, project/destination/pause/setup/removal policy and expected
   no-owner CAS for the agent-qualified key. Check existing qualified, legacy,
   generation, conflicting, corrupt and reservation states through the existing
   bounded index protocol. Commit first owner plus binding in one logical
   transaction using the existing recoverable registration/index/journal
   machinery. No successful indexed owner may appear before complete proof.
   Any protocol reservation may occur only after proof and locked policy/CAS;
   recovery must revalidate or abandon incomplete work, never promote missing
   proof. Do not create an empty owner and attach binding in a visible second step.
7. Only after success acknowledge the candidate, apply compatible retained
   lifecycle effects and use existing frozen publication/read-back/status. If an
   owner appeared during preparation, fail CAS without effects and reprepare
   under that owner's original authority or refuse conflict. Replay/restart must
   recognize the exact complete owner idempotently without resetting original T.
   Generic writes retain immutable admission/binding fences; no arbitrary-byte
   proof constructor or globally weakened authority check is permitted.

Q0's [fresh-root campaign](../qualification/muse/README.md) observed process-local
folder dates: `TZ=Etc/GMT+12` stored the previous UTC calendar day while the UUIDv7
encoded the current UTC day. This qualifies that case, not arbitrary clock skew
or every timezone. Date hints cannot bypass complete uniqueness. Currentness
costs O(selected IDs × completed directories); measure cold-cohort latency rather
than promising a successful lookup from a handful of date-based `lstat` calls.
A future current-pointer optimization needs separate qualification.

Relocation requires proven continuity; conflicting copies remain explicit.
Workspace/root/window changes cannot move pending authority to newer consent.
An unkeyed checksum cannot authenticate execution origin: a recent exact copy
indistinguishable from valid bytes cannot be promised excluded by integrity alone.
Parser checks after filtering are insufficient. Frozen retry and retained privacy
refresh work after native files disappear.

The shared inventory reuses the bounded `ReadDirent` batches and cookies in
[discovery](../../internal/discovery/discovery.go) through a narrow filesystem
boundary, without inheriting Codex automatic admission, catalog or consent.
`discoveryio.Walk` allocates a whole directory with `ReadDir` before its cap and
is unsuitable unchanged. Bound directory acquisition, entry and byte counts and
active time; charge unrelated entries; let valid owners progress beside fair
retries so unreadable early folders cannot starve later sessions. Cache compact
observations, not raw headers or parent transcripts. The traversal resumes content-free progress in the existing local
`discovery-catalog.json` optional `native_inventory` member. Measurements of the
actual once-process collector lifecycle established that an in-memory position
alone restarts the same prefix on larger-than-pass stores. Recognized complete
pages and actual EOF are historical installation-produced observations, distinct
from scheduling hints and from fresh per-owner currentness.
Negative results require completed scoped coverage; cookie invalidation,
eviction and root/revision changes invalidate it. Current eligibility is independent
of an exact historical scheduling cohort: arrivals wait for the next cohort,
removed/bound owners receive no action, and changed cohorts start without inherited
pages. Missing or incompatible observations reset coverage. This historical
local-state trust model does not claim tamper resistance against coherent manual
state forgery; fresh native source proof, policy and atomic commit remain required.

Identity facts can follow the first permission frame: use budgeted multi-record
inspection. Keep first-record-only `DiscoveryBoundedProjects` unchanged and return
unavailable there; reuse existing project, import and handoff inspection before
adding a new prefix API. Use `sourceio`/`transcriptio` and serial `SourcePass`
ownership. Initially qualified ordinary files are append-only; unsupported
rewrite, truncation or checkpoint semantics preserve prior publication and refuse
or defer new capture. A later native-index optimization needs a read-only
contract that creates no WAL or SHM files and never treats rows as admission
evidence.

### 3.3 Framing, privacy and retained data

Qualify envelope, transaction, marker and checkpoint versions explicitly. Preserve
exact bytes before integrity validation, logical child order, outer ordinal and
child position, stream and event identity, sequence, discriminators, versions,
timestamps and supported causal references. Validate `record_json`, child indices
and integrity; nested strings and JSON expansion share aggregate limits.

Own activity requires valid timestamp provenance and checked microsecond
conversion. Timestamp-less `session.fork.*` and `retained_marker:
omitted_live_only` records have copied or omission semantics; inherited
permission time, folder date, file time and hook observation cannot establish
native birth.

After validation, flatten to a deterministic, versioned `muse-jsonl` retained
format, the Muse counterpart of `codex-jsonl` and `claude-jsonl`. Each line is one
logical record with positional provenance:

```json
{"v":1,"pos":{"line":12,"child":0},"stream":"<stream-id>","seq":41,"type":"assistant_message_committed","ts_us":1792002123984531,"ids":{"run":"<run-id>","call":null},"fields":{}}
```

`pos.child` is null for an ordinary envelope and the index within
`children[]` for a transaction child. `fields` holds only allowlisted, redacted
values for that `type`. Q0 records the maximum reviewed
[per-type field set](../qualification/muse/retained-allowlist.json); unobserved
fields need new qualification and privacy review. Do not keep nested
`record_json` strings or certify redacted bytes with native checksums. Native
integrity stays local; the existing source-bundle SHA verifies the archived
artifact. `Refilter` and `EvidenceExtends` use safe retained evidence;
parser-only refresh does not reopen native files or feed archive serialization
through the native decoder.

Allowlist supported conversation, tool, usage, naming and lineage fields; apply
shared redaction to every retained string, including arguments, results,
objectives, paths, titles and instructions. Drop hidden or encrypted reasoning,
credentials, raw provider or context dumps, arbitrary hook output and unknown
payload bodies. `muse export --redacted` is insufficient because encrypted
reasoning remains.

Schema-3 checkpoints and copied `session.fork.compaction` contain `model_messages`,
`base_instructions`, `run_context_messages`, `transcript_tail`, resume state and
repeated usage. Drop private snapshot bodies, count no copied activity, and keep
only qualified bounded ownership and coverage facts. Producer restart success
does not qualify archive decoding or checkpoint frontier semantics.

| Input or result | Capture behavior |
| --- | --- |
| Complete supported evidence | Filter and publish with evidence-backed availability. |
| Unknown optional non-authority content | Drop body; keep bounded content-free coverage and mark affected metrics partial or unavailable. |
| Unsupported framing or checkpoint, invalid integrity, contradictory identity or lineage | Refuse the candidate, preserve prior publication, emit a fixed diagnostic; no acknowledgement beyond unprocessed evidence. |
| Incomplete final frame, late or missing child, changed file, temporary I/O | Bounded pending retry; no empty replacement. |
| Raw, record, output or graph limit | Explicit refusal or deferral under existing policy; no truncation or silent cap increase. |
| Privacy-redacted content | Safe allowlisted fields with privacy and filter provenance; omissions cannot imply complete metrics. |

### 3.4 Analysis, children and ownership

Produce one pure `archive.Analysis` for metadata, transcript, handoff and eval
export. Deduplicate accepted user intent and run `started.prompt` by supported
command or run identity; use `assistant_message_committed` for visible text
rather than adding deltas or terminal copies. Capture names, model switches,
tool and MCP calls, shell and Git facts and versions with their actual
availability.

`session.fork.model_prefix` and `session.fork.turn` are copied context, even with
full prompts and source timestamps. Exclude inherited activity from own prompts,
tools, tokens, titles and time bounds. Fork, rewind or side provenance alone
never sets `ParentSessionID` or `NativeChild`. Typed harness notifications are
not human prompts; English prose cannot determine origin or tool failure. Pair
committed calls and results and correlate side-effect intent with the task
terminal inside the owning run. A `PostToolUseFailure` fallback requires separate
hook qualification and cannot be inferred from the four-event root contract.

Choose one per-call usage authority keyed by owner and supported call identity;
provider response IDs alone may repeat. Do not add cache aliases, goal or
workflow rollups, export rollups or copied checkpoint usage. Ambiguous cumulative
scope stays partial. Verify real cache-write, cache-read and reasoning overlaps;
price only models with a dated entry, leaving unknown costs unavailable.

Muse children follow Codex's ownership model, not Claude's. Muse stores children
under the parent's directory as Claude does, but each child has its own session
ID and hook payloads, can be followed up independently and can outlive its root.
So children are enumerated with `ChildDiscoverer` under `<root-dir>/subagent/`
and owned as independent normal registrations, linked to their parents later.

Muse child facts map to existing `SessionRegistration` fields:

| Muse fact | Registration field |
| --- | --- |
| Child `session_id` from the payload and `child_session_bound` | `NativeSessionID` |
| Independent admission | `NativeChild` |
| Immediate parent from `parent_session_id` and the spawn binding | `ParentNativeSessionID`, then `ParentSessionID` once linked |
| Root, from `MUSE_SESSION_ID` and the binding | `NativeRootSessionID` |
| Logical agent ID such as `subagent-1` | `SubagentID` |
| Approved data root | `NativeSourceHome` |
| Link reconciliation state | `NativeLinkVersion` |
| Stream identity, original creation, root revision and locator | `NativeSourceBinding`, persisted as the new `native_binding` |
| Delegated, workflow or observer role, and child coverage | No field yet; add versioned role and coverage fields only if existing outputs cannot represent them, and never as a consent shortcut |

`native_binding` is the only new registration field; `codex_binding` keeps its
current shape and Codex keeps writing it. Today `validateNativeChild` rejects
`NativeChild` on any agent but Codex; relaxing it belongs with qualified child
admission. Validation alone does not protect every old mutation or maintenance
entrypoint: registration loads do not validate. An older JSON writer can drop
`native_binding` while preserving `TranscriptPath`, so the owner does not return
to a null locator. Current non-Codex source admission also supplies no replacement
authority. Such field loss is unsafe, not supported downgrade behavior.

Before persisting native roots or bindings, ship writer/schema protection that
supported older mutating entrypoints refuse or preserve, including imports,
pending work and setup recovery. A locator-bearing Muse owner with missing
binding must fail closed until its original authority is recovered from protected
durable state; current files cannot recreate lost continuity. Compatibility tests
in §4 exercise actual old readers and writers, not just the newest validator.

Children get independent registrations from validated spawn, binding and
observer facts in root scans and hooks. Verify the opened child stream, immediate
parent and root from explicit bindings, not depth, path or root environment.
Reconcile forwarded records; repeated stops and follow-ups update one owner.
Reused agent IDs with new session IDs create new owners. Poll admitted children
independently, including active children without stops and children outliving
roots. Task completion or a final response is not closure or a new-start proof.
After a native close on 1.4.7, reopen and follow-up were refused, but that is one
build and one run. Until the child gate in §5 qualifies close across supported
builds, keep closed children registered and poll them at a low rate rather than
assuming no further writes.

Each child needs its own original-creation, project, source and destination
consent. Use `subagent.worktree.*` facts for mapping included repositories, not
hook cwd or path-prefix guesses. Missing parents preserve child identity and
unavailable links; never fabricate a root, birth time, ancestry or inherited
permission.

Extend the existing [native linker](../../internal/collector/native_links.go),
which today accepts only Codex registrations. Qualify link keys by agent and
native ID, project and destination, and source-root binding; use the owning
agent's removal namespace. Keep Codex legacy composite migration Codex-only.
Queue durable link work before marking completion; retry across parent absence,
retention or undo and child publication without new parent bytes. Reject cycles,
self-parenting, conflicting roots and ambiguous owners. Preserve Claude's
different child provenance and regressions.

Store child own metrics and reuse the existing stats rollup: root session and
prompt counts remain root-only; child usage and tools roll into the root once.
Keep current date-window, orphan and replay behavior. Root own metrics can be
measured while descendants are unavailable, as in the first release; expose
missing coverage in links and gaps and claim exact tree totals only for a
complete controlled snapshot.

### 3.5 Hooks, setup and operational behavior

Configure only `SessionStart`, `UserPromptSubmit`, `Stop` and `SessionEnd`
for the qualified root lane. `PostToolUseFailure`, `StopFailure`, subagent,
TUI and MSP/serve hook capture remain unqualified by these receipts; qualify
additional events/modes before advertising or installing them. `PostToolBatch`
requires separate qualification if long-running activity needs it.

The pure decoder declares only `MUSE_SESSION_ID` and `MUSE_THREAD_ID`, bounded
at 128 bytes each. Inject one immutable read-once environment context into
`HookInput`, reused by normal decoding and diagnostic/clock-failure/panic paths;
never scan or persist raw environment. Require payload/root/thread IDs present
and exactly equal, with bounded opaque ASCII grammar
`[A-Za-z0-9][A-Za-z0-9_-]{0,127}`. Native filename/body layout validation remains
in source/inventory. Discard unknown fields, payload bodies and reason values;
observed unknown-key counts grant no authority.

`SessionStart` requires exact `source=startup` and null `transcript_path`.
Map it to observation-only `EventStart`, `Start.Kind=FreshUnknown`, no stat
request, empty `Source.Path`, `LocatorNone` and the exact qualified
`NativeHookContract`. Absolute observed cwd is local project input only.
Missing/changed source or nonnull path cannot infer an alternative authority.
Never convert the observation to `FreshExplicit` or `FreshnessRetainedProof` on
queue/replay. A requested ID, matching environment, startup label or lack of an
old registration proves neither fresh birth nor absence of child/fork provenance.
Unknown-ID startup remains gated on complete native original-T qualification;
its wire receipt does not establish accepted production source topology.

The existing typed queue gains separate closed `startup_candidate` and
`known_owner_effects` modes. A candidate stores only bounded content-free
observation/ID/cwd/time, exact captured root/window/config generation and policy
fingerprints, destination/pause generation and hook contract, with no owner,
archive ID or locator. Capture and compare those guards at hook observation,
queue commit, collector scheduling and final admission. Setup/removal/pause or
root/window/project/destination changes invalidate authority; a late config load
cannot refresh it. Unknown/missing mode/version/contract refuses before queue
eligibility, pruning, clearing or mutation. Legacy intent translation, expiry or
minimal replay evidence cannot promote a candidate. Retire only recognized
expired/revoked/refused work under existing queue ownership policy.

Map prompt to `EventTurnStart` and stop/end to supported lifecycle observations
with fixed archive reasons, `FreshUnknown` and no locator. These may update
normal lifecycle/request evidence only
for the exact validated existing owner under its original authority and current
policy. Unknown-owner continuations are no-op/diagnostic only: no owner,
reservation, candidate, session evidence or directory discovery. One explicitly
committed Muse home and included-project permission remain required; no
all-projects, Codex-discovery or automatic missed-start admission is introduced.

Protect the new intent/config generation through sticky config and setup-journal
capability guards, including root removal, queue prune/clear and raw rollback.
Actual old writers must refuse before mutation; generic saves preserve immutable
admission and native authority. Reuse existing protection machinery and allocate
no numeric versions in this design reconciliation.

Hooks read no transcript/header, scan no native store, invoke no native CLI,
filter/upload/network nothing, and perform no Git inspection for an unowned
candidate. They stay fast, silent and bounded; source work belongs in collector.

Before combined PR6 dispatch, finish native wire/source/original-T qualification,
resolve source compatibility blockers (including approval-free read sessions and
the unknown-ID startup topology), and review/freeze candidate-purpose and prepared
first-admission APIs. The new production decoder, transaction, replay, race,
cancellation, crash/restart, old-writer/journal and negative-before-mutation tests
belong **inside PR6** before readiness/merge. Include original T before activation
but later opening/UUID/folder times after it. PR6 acceptance follows real generated
hook → candidate → shared inventory → same-handle complete source proof → atomic
first owner/binding → publication/read-back/status, with existing-agent/resource
regressions. These implementation tests are not pre-dispatch prerequisites for
code that does not exist yet. Public source/registry capture fences remain until
this chain passes; finite local decoding does not establish capture or full parity.

Generate absolute executable and `AGENT_ARCHIVE_HOME` commands; native roots come
from committed archive settings rather than a duplicate hook prefix. Use
expected-byte `filechange.Change` plans and setup journals for installation,
inspection, refresh, rollback and removal. Preserve foreign handlers, settings
and managed files; ordinary machine-wide capture must not require project trust.
Do not invoke Muse settings or skill writers that dropped unrelated MCP fields,
or repair foreign configuration automatically.

Replay markers are launch-scoped and dynamic, never a fixed ID in ordinary hooks.
An optional runner-owned managed file can forward `AGENT_ARCHIVE_REPLAY` through
direct hook commands; compound shell commands lost the extra variables. Qualify
the exact generated command, quoting and environment prefixes across root, child,
observer, TUI, serve and recovery modes; an allowlist alone is insufficient. If
forwarding fails, keep replay unavailable. Report every owned configuration file
and preserve existing managed configuration.

### 3.6 Import, readers, skills and handoff

Reuse purpose-specific headers, inspection, project paths and child enumeration
for explicit backfill and pre-setup find, show and handoff. Local reads need no
storage setup and must not enable archiving. Backfill previews exact scope, dates,
duplicates, refusals and gaps; batch undo removes only its ownership and link
effects, preserving independent live capture, later continuation and native files.

All reader, title-search, text, JSON and HTML statistics and eval-export views
consume shared Analysis and agree on identity, own activity and gaps. Preserve
existing root and child and replay defaults. Sessions run with
`--no-session-log` or ephemeral serve leave no retained file, so they cannot be
captured; report this only when native evidence establishes it for the session.

Runtime detection prefers injected exact thread identity over a project-latest
fallback. Fixed `muse --version` probes distinguish semver and SHA from the
R-build display; an unstamped child version cannot be called directly observed.

Declare installation and evidence roots separately. Deduplicate shared
`.agents/skills` installation with Codex and Cursor. Config-local, project,
foreign and plugin roots need trust, activation, eligibility and use
qualification. Inventory proves installed only; invocation and read hashes require
owner and identity mapping. Unsupported eligibility cannot become observed-no-use.

Archive-to-Muse handoff uses a new conversation and the existing safe context
artifact; test argv and positional prompts, Unicode and metacharacters, quoting,
terminal launch and artifact reads under ordinary permissions. Muse-to-each-
existing-agent handoff uses safe normalized evidence. Do not add bypass flags.
Native resume is separate; an exported log is not proof of portable resumption.

## 4. Tests required before claiming parity

All tests must assert outcomes, counts and availability through normal registry
and consumer paths, not just isolated decoding or result labels. Reuse applicable
`internal/testutil/agenttest` source, discovery, import, skills, runtime and launch
suites, the shared `-update` flag and existing fourth-agent regressions. The
first release needs the rows marked R1 for root sessions.

| Area | Required cases and assertions |
| --- | --- |
| Admission and roots (R1) | Fresh versus unknown resume; copies, forks and inherited time; excluded, nested and reactivated projects and destination and removal windows. Null locator and restart before and after commit; concurrent stop, setup, pause and removal; replaced files, outside-root, symlink, nonregular and mismatched streams. No filtering or PUT before same-handle validation. Multiple installations and cross-agent ID and root collisions. |
| Locator lookup and fallback (R1) | Complete scoped census/currentness across UTC and local date boundaries, clock skew and a folder date that differs from the ID timestamp. Date probes never bypass uniqueness; bound-owner known paths avoid enumeration. Shared traversal on a store larger than a pass budget: late and unrelated entries, unreadable folders, restart, root change, eviction and conflicting copies. Prove eventual coverage and bounded directory and header operations, never guessed uniqueness; measure selected-ID × completed-directory currentness work. Valid sessions progress beside retries. |
| Framing and privacy (R1) | Mixed frames, child order, retained markers, copied prefixes and checkpoints, nested expansion, bad integrity or versions, duplicate keys and IDs, sequence conflicts, invalid UTF-8, blank, missing-newline and partial lines, timestamp overflow and contradictory authority. Fuzz allocation bounds. Secret sentinels in every nested argument, result, title, objective, instruction and path; no reasoning, encrypted or context dumps remotely. Exact refusal, gap and partial-metric behavior. |
| Conversation and usage (R1) | Exact own prompts, messages, turns and text, Unicode and multiline, streams, retries, deltas and harness notifications, names, automatic titles, model switches and metadata-only refresh. Own per-model calls, cache, reasoning and cost availability; no copied or rollup duplicates. Retained refilter and parser refresh with native files removed. |
| Children and totals | Direct, workflow, observer and grandchild success, failure and cancel, missing and late logs, active children with no or lost stops, parent exit and absence, follow-up, close, reopen and follow-up after close, reused agent IDs and forwarded records. Explicit ancestry, cycles, self-parenting and conflicting roots; one stable owner and no root fallback. Parent arrival links without new parent bytes. Root-only prompts plus child own tokens and tools, window, orphan and replay rules and incomplete tree coverage. |
| Tools, MCP and skills (R1 for root call pairing) | Committed parallel, success, error, cancel and interrupted calls, structured and oversized results and owning-run task pairing; any `PostToolUseFailure` fallback needs separate qualification. MCP canonical IDs, stdio, HTTP, OAuth, plugin and retry attribution. Shared-root deduplication, trust, activation, foreign and plugin gates, invocation versus read and installed versus eligible evidence; foreign config survives. |
| Projects and Git (R1) | Shared, isolated, missing and deleted worktrees, clean and dirty starts, branch changes, detached checkout and Git unavailable. Native worktree evidence controls mapping of included repositories; hook cwd cannot bypass scope. |
| Setup and compatibility (R1) | Foreign hooks, managed files and settings preserved byte for byte; quoting, custom XDG and archive roots, payload and environment disagreement, lock contention and interrupted setup, rollback and remove journals. Explicit opt-in only. Current and old-reader and old-writer upgrade and downgrade matrices with `native_binding`, registrations, imports and pending journals; no lost authority. |
| Backfill and undo | Date, project, root and child selection, duplicates and conflicting copies, restart and budgets and overlap with live continuation and import. Exact batch undo preserves later independent ownership and acknowledged and native data. Pre-setup access causes no archive mutation. |
| Readers, handoff and replay | Text, JSON, HTML and eval-export agreement on IDs, counts, gaps and roles; root and child selections and visibility defaults. Each existing agent to Muse and back, safe artifacts, argv and terminal behavior, Unicode and metacharacters. Dynamic markers through root, observer, workflow, grandchild, TUI, serve and restart; ordinary sessions stay ordinary. |
| Storage and maintenance (R1) | Inject failures before and after local state, staging, source PUT, metadata PUT, verification, acknowledgement and cleanup. Restart and native source loss preserve staged bytes, destination and ownership; new bytes cannot replace frozen retry. Read-back checksums; privacy and exclusion across every referenced source; root and child expiration, unresolved links and protected pending stages without dangling acknowledgement. |
| Native modes and versions (R1 for roots; unqualified modes must refuse safely at the record) | Fresh, second-turn, known-resume and unknown-resume, failure, orderly exit, SIGINT, SIGTERM and hard kill with late writes across TUI, exec and serve. Fork, rewind and side conversations, manual, default and periodic compaction and resume, including prune, rewrite and split. Logging-disabled and ephemeral behavior. Supported producer upgrades, future records and real macOS and Linux architectures; no Windows claim. |

The corpus records the exact build, provider and procedure, raw and sanitized
hashes, redactions and omissions, framing and expected identity, graph, text and
usage. Sanitization changes checksums: exact reviewed synthetic frames can certify
native observations; derived mutations cannot certify valid encoding. Probe
deadlines and process cleanup are explicit; another fresh-root run must reproduce
all claimed assertions. Wait for actual owning child settlement and preserve
refused and timed-out attempts.

Start deterministic integration acceptance locally, then run a bounded real Meta
campaign and disposable storage publication and read-back. Record the exact
source and CLI, argv, roots, hooks and native files, expected facts, archive IDs,
remote checksums, read-back and `status --json`. Verify each child separately.
Real provider access and a disposable destination must be configured before those
tests; synthetic usage does not establish billing accuracy.

### Resource and performance gates

Measure operation counts and allocation bounds first, then wall time and RSS on
named reference machines. `NativeReadBudget` limits charged memory, not total disk
reads or RSS. Reserve before allocation, charge shared lookup and graph caches
under current `PassReadBudget` and `PassCacheReserve` and release them with
snapshots and pass lifetime. Verify combined Codex, Cursor, Claude and Muse
pressure and ordinary-session headroom; do not add another cache reserve or
silently raise caps.

| Workload | Required invariant |
| --- | --- |
| Many cold pending roots | Pending queries share one bounded resumable namespace census and existing persisted progress. Successful selection requires complete scoped coverage plus fresh per-ID currentness; date hints alone cannot prove uniqueness. Measure cold-cohort latency, O(selected IDs × completed directories) currentness, and bounded directory/header work. |
| Warm unchanged owners | Cheap confined identity and signature checks; no full filtering, metadata PUT or whole-store enumeration merely because a tick ran. |
| Growing root with many children | One parent analysis per changed scan, shared compact bindings and independently charged child reads; no full parent read per child. |
| Large directories, records, checkpoints and refilter | Bounded acquisition and raw, decode, output and graph expansion, no full context cache; disk bytes, visited records and active time bounded independently. |
| Missing or ambiguous beside valid owners | Fair bounded retries with visible pending state; independent valid capture continues. |

Restore existing 1-, 128- and 512-session and full raw, record and source limit
cases under current tier policy; add larger-than-budget inventories and long live
graphs. Short-mode success does not prove full-scale behavior. Measure hook
latency and collection impact on native tools. An offset tailer or incremental
cache is a later optimization only after profiling and proving identical
authority, refilter, rewrite and checkpoint semantics.

## 5. Enablement, release gates and unresolved work

Each gate blocks only the formats and capabilities it names. A format whose gate
is open is refused safely while other qualified formats keep capturing.

| Gate | Blocks | Owner and evidence needed |
| --- | --- | --- |
| Root integrity and original creation | First release | Q0 has recovered the [checksum encoding](../qualification/muse/permission-integrity.md) and qualified [strict initial-root logical-start semantics](../qualification/muse/root-creation.md). The local subset supplies strict qualified prefix/same-descriptor checks and inventory foundations. P2 still needs startup-candidate source compatibility qualification and the complete current-consent first-admission/publication path. Unsupported lanes remain unavailable. |
| Child and branch creation | Children (P3) | Q0, then P3: creation through copies, resumes, checkpoints and missing parents; whether close is terminal on every supported build, so closed children can stop being polled; observer link records, relative paths and missing-file semantics. |
| Checkpoint and compaction formats | Capture of sessions containing the unqualified format | Q0 and P5: qualified default, manual, prune, rewrite, split, fork, rewind and TUI gestures. The current history-preserving checkpoint covers only its recorded case. Until qualified, such sessions stop capture safely at that record. |
| Real accounting | Cost and cache claims | P5 with locally configured provider access: dated actual model, cache, reasoning, retry, background and compaction attribution and pricing. |
| Platform and version support | Each advertised platform | Release owner in P5: actual supported native macOS and Linux architectures with a pinned producer and runtime; cross-builds alone are insufficient. |
| Adapter and user surfaces | Each surface it advertises | Each phase owner, then P5: implemented privacy, admission, storage, undo, retention and resource campaign; exact skill, MCP, replay and handoff coverage before advertising it. |

Enable Muse only through explicit setup selection, never installation, shared
skill presence or refresh. Persisted source-root and child authority changes need
schema and writer protections together: old writers must refuse or preserve them.
Test actual old-reader metadata access where supported; otherwise require upgrade.
Rollback and removal disable new capture and remove only owned resources,
preserving native state, bindings and consent, archives and frozen and pending
ownership. No destructive migration, separate retry loop or self-updater.

Use existing fixed status and capture-gap reasons for pending locator, incomplete
coverage, unsupported format, unresolved child, missing metric and verified
publication. Diagnostics expose versions and counts, not prompts or arbitrary
native error strings. Presence, version, hooks, events, local capture,
publication and read-back remain distinct.

Follow [testing](../contributing/testing.md) and
[version rules](../maintainers/versions.md) on the final ancestry: required CI,
race, vet and lint, architecture boundaries, fuzz, reviewed goldens, platform and
release checks. Allocate parser, adapter and filter versions when implementing;
update schemas for role and lineage changes and keep archived sessions readable.
Update README and help, setup and status docs, JSON contracts, capture
capabilities and the spec index with actual scope. Shared changes must preserve
Claude, Codex and Cursor admission, ownership, readers, visibility and publication.

No upstream Muse change is assumed. Populated hook transcript paths and explicit
immediate-parent and root IDs would simplify resolution; missing facts cannot be
replaced by fabricated completeness. If a gate cannot be resolved, leave the
capability unavailable and identify its required evidence.

## 6. What native qualification established

### Environment and evidence limits

Tests ran on 2026-10-14 on macOS x86_64 with Muse `1.4.7-R6101.1`
(native SHA `d8c53d25bd`) and `1.4.8-R6256.1`. Official binaries, config, data
and cache and disposable workspaces were isolated in temporary storage. The 1.4.8
download matched Meta's advertised size and SHA-256. A loopback endpoint supplied
deterministic model and catalog responses; filesystem writes and shell tools were
disabled under ordinary permissions. Native worktree creation and cleanup was
exercised separately. No personal settings edits, real Meta calls, paid requests
or archive uploads ran.

The host had no configured Meta authentication or usable Linux VM or container
runtime. Native Linux and arm64 remain untested; cross-builds do not establish
them. An official local stdio filesystem MCP server, version `1.0.0`, ran with
Node 24.19.0 against synthetic files. Broader transports and plugins remain
unqualified.

Repository design and contract checks used HEAD
`7d8aee13c7146ceb38d15df7188d7daa31f464eb`. Documentation was rebased onto
`4f94edb97f992f408521928cd1227d2780c6401c`. Between those heads, Codex history and
refusal, source encoding and budgets, discovery and read bounds, picker and
handoff and CI contracts changed; Q0 reconciles them. Historical native evidence
does not certify a newer implementation base.

The supplemental [1.4.9 Q0 record](../qualification/muse/results-1.4.9.json)
adds only pinned macOS x86_64 synthetic `exec-utc` fresh/resume/copy and mutation
observations (22 final-run assertions passed). The prior 1.4.8 matrix and corpus
retain their original provenance. No framing or opening-shape change was observed
in this lane. The later [checksum supplement](../qualification/muse/permission-integrity.md)
independently recovers the pinned permission v1 hash recipe. Complete native
grammar and production admission validation remain unimplemented. The
[initial-root contract](../qualification/muse/root-creation.md) separately
qualifies reserved logical-start semantics in the observed strict 1.4.9 exec/MSP
lanes. No broader MSP, PTY, child, checkpoint, platform, real-accounting or
production capture qualification follows from this supplement.

### Observations that determine the design

Most rows come from retained probe results. Some details survived only in the
earlier qualification reports: the 4096-byte checkpoint setting and checkpoint
name, `result_ready` settlement, `submit_result`, compound-shell forwarding and the
observer-link notes. Q0 reruns those before any phase relies on them.

| Area | Established result and limit |
| --- | --- |
| Retained storage | Roots use `$XDG_DATA_HOME/muse/sessions/YYYY/MM/DD/<session-id>/session.jsonl`, default data home `~/.local/share/muse`. Root session IDs are UUIDv7-shaped and matched their folder dates in the one recorded path, which was far from midnight; some child IDs are not time-ordered. Every observed hook had `transcript_path: null`. Root export lacked actual child content; retained files require independent resolution. |
| Identity and ancestry | Logical agent IDs differ from child session IDs. `spawn_accepted`, `child_session_bound` (`nested_session_v1`), `parent_session_id` and agent-lineage admission supplied explicit edges. Children and grandchildren were flat under `<root>/subagent/<child-id>/session.jsonl`; both depths could be 1. Forwarded control records repeat across logs. |
| Hooks and environment | User hooks use `$XDG_CONFIG_HOME/muse/settings.json`; project hooks use `.muse/hooks.json`. `MUSE_SESSION_ID` identifies the root, `MUSE_THREAD_ID` the current thread, and child payload `session_id` the child. User hooks dropped custom XDG and replay variables. Direct managed commands forwarded the allowed marker to root and children on 1.4.8; compound shell instrumentation lost extras on both builds. `PostToolUse` and `PostToolUseFailure` fired for tool calls; `StopFailure` was not exercised. |
| Root lifecycle | Fresh TUI, exec and serve turns and exec resume were exercised. Resume kept the same ID and raw prefix, sometimes without another `SessionStart`. Root `session.opened.observed` with `resume: false` is corroborating startup evidence, not the selected birth. The later [initial-root contract](../qualification/muse/root-creation.md) qualifies reserved start in both initial permission children for strict observed 1.4.9 roots; complete admission validation is still unimplemented. A `--no-session-log` exec run added no retained file (the one log counted in the data home came from the earlier logged run), and ephemeral serve created none; ephemeral `session/read` was unavailable. |
| Child lifecycle | Successful workflow completion required `submit_result` with text and nullable notes on both builds. On 1.4.7, a direct child follow-up settled a new `result_ready`. A close sent while the child was still running was refused with `subagent_state_ineligible`, and so was the reopen that followed it; a close sent after settlement succeeded. After that close, reopen and `followupTask` were both refused with `subagent_state_ineligible`. Hard-kill parent recovery and the 1.4.7-to-1.4.8 upgrade preserved root and child prefixes and settled another follow-up. These are producer tests, not archive recovery tests. |
| Grandchildren and observers | Explicit grandchild graph passed on 1.4.7; the 1.4.8 compound-instrumented orderly run timed out. Earlier notes report observer links through `memory_reminder_child_session_linked`, sometimes with relative paths, goal and verification files, and a skill link without a retained file. No retained probe result records these, so they are leads for Q0 to rerun, not established results. Absence versus late or disabled logging needs qualification. |
| Isolated worktrees | Native prepared, lease and cleanup records passed, including on 1.4.8. Child hook cwd still named the source checkout. Archive project and consent mapping is unimplemented. |
| Framing and integrity | Ordinary envelopes, `session_permission_transaction` with `children[].record_json`, omitted-live-only markers and timestamp-less fork records coexist. Exact schema-1 permission bytes exported cleanly; checksum, whitespace, key-order and other mutations produced an unparseable-line diagnostic while exit stayed 0. The [pinned v1 checksum recipe](../qualification/muse/permission-integrity.md) is now independently verified; complete canonical grammar and unsupported shapes remain unqualified. |
| Checkpoints and branches | On 1.4.8, the opt-in 4096-byte periodic setting produced a `context_projection_checkpoint` with schema 3 and history `preserved`. It includes private context and resume state and repeated usage. On 1.4.7 the same setting was accepted but no checkpoint was observed. In a separate run with an unrecorded build, a corrected same-endpoint restart returned in 4.857s, preserved the prefix and completed another turn; the earlier timeout remains unexplained. On 1.4.8, a side conversation copied `session.fork.compaction` and continued independently, and an older fork cut was refused as `forkBoundaryInvalid` with `runtime_checkpoint_tail`. |
| Compaction and TUI limits | Manual MSP compaction returned `compaction_unavailable` on both builds, including strategy and context-limit probes. TUI startup passed; two later compact-and-quit attempts missed their readiness trigger. Default and manual compaction, prune, rewrite and split behavior and orderly TUI gestures are not qualified. |
| Tools and MCP | Committed `call_id` and `tool_call_id` pair. Actual local stdio success returned the sentinel; missing-file execution failed and fired `PostToolUseFailure`. Join `tool:<call-id>` side-effect intent to the owning run's task terminal for typed status; result text has no typed error flag. The first bare MCP alias was unknown, not an executed MCP failure. |
| Usage | Controlled input 101, output 23, cached 17 and reasoning 5 appeared in `model_completed`. `cached_tokens` and `cache_read_tokens` both equaled 17; aliases and goal or workflow summaries must not be added twice. Real cache and reasoning overlap, billing and retry attribution remain untested. |
| Skills | On 1.4.8, project catalog evidence depended on workspace trust. Successful `read_skill` produced `skill_read_observed` with `evidence_kind: read_skill_tool` and a hash. Native `user-only`, `disable` and `enable` exited 0 but dropped unrelated MCP type, cwd, required and timeout fields while preserving foreign hooks. Full activation, foreign-root and plugin eligibility remain gates. |
| Names and handoff | Corrected MSP initialization required `initialized` after `initialize`; rename, read and 1.4.7 fork continuation passed. A synthetic context artifact outside the workspace was readable under normal permissions. Actual cross-agent launching, title refresh and portable native resume are not established. |

Failed attempts, corrected assertions and untouched cases were kept distinct in
the probe record. Command acknowledgement or model-call counts do not establish settled
child results. SDK and web-preview wire schemas differed from the tested
binaries: use each binary's offline export for probe parameters, including the
required resume `commandId`. Stable and experimental fingerprints are separate;
schemas and exporters do not specify the retained-file integrity contract.

## 7. Evidence and reproduction details

Current Q0 reproduction, complete reviewed synthetic corpus, shared contract
corrections and truthful support/refusal gates are in
[Muse qualification](../qualification/muse/README.md). The permission
checksum recipe is independently recovered in the [checksum supplement](../qualification/muse/permission-integrity.md);
the [initial-root contract](../qualification/muse/root-creation.md) separately
qualifies the reserved logical-start field for strict observed 1.4.9 roots.
These historical campaigns do not certify the later
[delivered local subset](#delivered-local-subset-and-residual-ownership). Complete
format coverage and first-owner consent admission/publication remain pending;
this evidence does not enable capture. Native export acceptance
is not an independent verifier,
and even rejected lines can exit 0 with empty stderr: inspect the document's
`diagnostics.unparseable_lines` separately.

The historical detailed probe record is not kept in the repository. Its last version is
`dev/specs/muse-qualification-evidence.json` at commit `7981d3ee`: per-campaign
observations with their builds, frame-mutation results and the source of each
finding that survived only in earlier reports. The per-campaign results files it
hashed, raw model requests, private context, full logs and probe helpers stayed
in temporary storage. Authoring a runnable probe and the complete reviewed corpus
is Q0's first deliverable; that corpus, not the old record, is the evidence later
phases cite.

Pinned producer inputs for Q0:

| Item | Value |
| --- | --- |
| `1.4.7-R6101.1` | Native build SHA `d8c53d25bd`; stable schema fingerprint `sha256:6de32db08bbbe0deb478d2bfcaba001e9fb490986bc80db3aceb4d996abf7df9` |
| `1.4.8-R6256.1` | Binary SHA-256 `94ea8fd1c73664c22e5c6157d8706945b667b11bd1c7c51691811e79f17dfcf1`; stable schema fingerprint `sha256:4e3200c3c9dc0cd7919d11022630d43f9bc1c5cdda375381e90b0183b5665069` |

On `1.4.8-R6256.1`, `muse export` accepted an exact schema-1 permission-frame
excerpt with 0 unparseable lines. Copies with a zeroed `content_sha256`, changed
outer key order or other mutations each reported 1 unparseable line; every case
exited 0. Q0 regenerates exact frames from the pinned binary, and P1's decoder
tests consume them from `internal/agents/muse/testdata`. Native acceptance of the
original and rejection of mutations do not establish independent checksum
encoding.

These existing checks passed at `7d8aee13` and again at `4f94edb9`. They cover
the extension points the plan reuses, not unimplemented Muse behavior. Rerun them
at Q0 on the current base:

```sh
go test -short -count=1 ./internal/agents/builtin ./internal/agentapi
go test -race -short -count=1 -run '^TestFourth' ./internal/cli
go test -short -count=1 ./internal/sourceio ./internal/transcriptio ./internal/discoveryio
go test -short -count=1 -run '^Test(Native(Link|LateParent|ParentRepair|FrozenParent)|LegacyNativeChild)' ./internal/collector
go test -short -count=1 -run '^Test(OnlyCodexIsRegisteredAndEnumerationIsBoundedAndCancelled|ConflictingSourceDoesNotStarveLaterDirectoryBatches|UnavailableDirectoryRetriesNextPassWithoutStarvingBacklog|BoundedScanContinuesFairlyAcrossActiveAndArchivedSources)$' ./internal/discovery
```

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
had older-build assumptions; pin evidence to actual native builds and formats.

The [production local v1 field policy](../qualification/muse/retained-allowlist-local-v1.json) is the exact current retained projection. The original `retained-allowlist.json` and derived corpus remain byte-frozen historical Q0 evidence; their broader metrics ceiling is not a production promise.

### Guarded root implementation and remaining release gate

The dormant root path decodes only qualified SessionStart/UserPromptSubmit/Stop/
SessionEnd observations. An explicit startup produces an observation-only candidate
under one configured home, included project, destination and captured window.
The ordinary collector uses its shared native inventory and one concrete leased
Filter to establish original native birth and complete source proof before it
atomically exposes the first owner plus immutable binding. Publication and CLI
readback verification retain their ordinary separate stages. Pending queue status
reports only count/time/completeness and never calls an observation captured.

Normal injected setup, generated hook settings, candidate handling, shared
inventory, first ownership, publication, readback and status are exercised as one
deterministic pipeline. Public setup still excludes Muse by default. Actual
production-generated command delivery and native root-scoped TUI/exec/serve and
platform acceptance remain separate gates; this implementation does not establish
broader child/import/runtime/skill/launch or dynamic replay parity.

PR6 observation identity accepts the bounded opaque ASCII hook contract. That
only authorizes a candidate query. The existing qualified native census and
ordinary source format still require their supported canonical UUID identities;
unsupported opaque source formats remain pending/refused before Filter or owner.
UUID spelling, ordering and timestamps supply no freshness evidence.

A pending first-owner reservation now carries `native_reservation`, a comparable
`muse-root-reservation-v1:` plus lowercase SHA256 digest of the complete original
candidate and concrete source binding. The domain-separated digest excludes the
minted archive ID and registration timestamps. It is never admission/recovery
proof: exact retry must freshly reopen, validate and concretely Filter the same
candidate/source. Missing, changed or future authority preserves/refuses the
reservation. Append after a crash conservatively refuses; automatic abandonment
or changed-source recovery is outside this contract. Only a complete matching
durable Muse owner with valid birth admission/v2 binding allows supported-marker
cleanup by the existing index finish/recovery machinery.

Reservation authority is a physical per-key overlay. Current pending native
reservations are never projected into packed census rows. Active unsupported
packed markers refuse before repair/fallback; authoritative physical overlays
remain intact through a census. Explicitly obsolete known derived aggregates
retain the existing bounded replacement lifecycle; PR6 adds no whole-obsolete-
aggregate scan or claim about hypothetical future authority hidden there.
