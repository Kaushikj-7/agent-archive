# Coding-agent integration abstraction

Status: implemented on the default branch: phases 0–6 landed in #278, #290, #293, #295, #298, #299, #300 and #301. Reconciled 2026-10-11 at `db2ccd3e`. See `internal/agentapi`, `internal/agents/builtin`, `internal/cli/fourth_flow_test.go` and [Adding an integration](../../contributing/adding-an-adapter.md). The stacked/readiness statuses below are historical. The [baseline](agent-integration-baseline.md) remains pinned historical evidence: its unresolved relative timing gate and later installed-version/platform acceptance are not declared passed by this cleanup.

## Original design and execution record

Prepared 2026-10-01 against `main` at `98f8bd0`. Status: stacked implementation in progress; readiness and independent review
remain pending. Reviewed against the current capture, state, collector, parser,
and source-reader implementations on 2026-10-01. The interface sketch is ready
for design iteration; the contracts and performance gates below must be settled
before their corresponding implementation phase.

## Recommended design

Use an immutable built-in registry for composition, inject narrow interfaces
into shared services, and keep native interpretation inside each agent package.
The reviewed defaults are:

| Concern | Recommendation | Implementation gate |
| --- | --- | --- |
| Identity | Agent-qualified `SessionKey`, with a separate local index namespace and validated legacy adoption. | Migration/recovery and downgrade behavior are specified before lifecycle cutover. |
| Parsing | Separate filter and parser interfaces; pure `archive.Analysis` feeds all shared builders. | Metadata, labels, transcript, and handoff consume one analysis; no registry dependency enters archive. |
| Native facts | Distinguish local filter-time ownership observations from retained semantic facts. | Labels, outcomes, model/workspace facts, and subagent validation have explicit owners and fixture coverage. |
| Sources | Bounded file/record input, lazy serial passes, explicit borrowed-data lifetime. | No artificial database export serialization, eager snapshot, or extra whole-transcript copy. |
| Mutation | Provider-declared append-only versus replaceable-snapshot semantics. | Shared collector owns rewrite decisions and preserves existing gaps and provenance. |
| Hook integration | Separate configurator/inspection from decoder; validate batches and apply replay-safe effects. | Duplicate delivery and failure between effects preserve ownership and deferred intents. |
| Failures | Typed errors with shared retry and deterministic-failure caching policy. | Transient failures never become permanently settled; cleanup failures remain visible. |
| Performance | Establish a baseline before moving hot paths; enforce counted-operation gates. | Unchanged sessions stay cheap, changed sources filter once, and Cursor reuses one snapshot attempt per pass. |
| Scope | Introduce each interface with a real caller; defer stdin transport, new concurrency, and global caches. | Each migration PR is independently reviewable and passes existing checks. |

The detailed contracts below implement these recommendations. Phase 0 records
the remaining representation and migration details; those details are required
phase-entry artifacts, not permission to change these boundaries during coding.

## Problem and intended result

Adding a fully archived coding agent currently requires edits across archive
adapters, normalization, hook installation, capture, collection, backfill,
setup, skills, configuration, and reader lookup. There are useful boundaries,
but no integration boundary that contains an agent's native contracts.

After this refactor, adding an agent should ordinarily mean adding an
integration package, registering it, supplying synthetic fixtures and
capability evidence, and updating supported-agent documentation. Shared
orchestration should change only when an agent introduces a capability the
contracts cannot yet represent. OpenCode and Antigravity CLI are potential
consumers of this work; this proposal does not assert that their capture
contracts have been investigated or implement either agent.

The central division is **native interpretation versus archive policy**:
integrations interpret native lifecycle, source, and transcript evidence;
shared services decide admission, import eligibility, publication, retention,
and presentation.

## Current boundaries

| Area | Existing seam | Limitation |
| --- | --- | --- |
| Identity | `archive.CanonicalHarness`, `KnownHarness` | Setup, reader, skills, handoff, and config have additional supported-agent lists. |
| Filtering | `archive.Adapter` | JSONL-only contract; Cursor text/composer paths use concrete adapter assertions. |
| Parsing | `archive.ParseNormalized` | Shared traversal understands native shapes and has agent-specific branches; implementing `Adapter` is insufficient. |
| Source access | Collector's private `sourceReader` | Useful signature/read seam, but selection, signatures, and database filtering contain Cursor-specific types. |
| Lifecycle | `hooks.events`, `capture.classifyHookEvent` | Installation and interpretation separately enumerate native events; admission contains agent-specific freshness and path logic. |
| Launch | CLI's `agentCommands`, `buildLaunchSpec` | Mostly table-driven, but assumes a directory flag and final positional prompt, with a Claude exception. |
| Discovery | Backfill's per-agent discovery functions | Native identity, locations, and project evidence are interleaved with shared import planning. |

The source bundle, object-key layout, publication loop, and normalized display
are reusable. Preserve them rather than introducing another transcript format.

## Scope and invariants

In scope: migrate Claude Code, Codex, and Cursor to explicit integration
contracts; consolidate identity and operation lookup; make archive parsing and
source access usable by another agent; document and test the extension path.

Out of scope: implementing a fourth production agent, dynamic plugins,
user-defined shell-command integrations, native resume conversion, a new
storage backend, and changing archive contents as part of extraction.

The following remain shared requirements:

- Hooks are bounded, silent, offline, and always protected by the CLI's panic
  recovery and exit behavior. No program execution moves into capture.
- Freshness is evidence, not an assumption. Project inclusion, activation,
  destination ownership, and continuation identity remain capture policy.
- Only filtered content may enter a source bundle. Unknown shapes become
  explicit gaps or refusals. Hidden instructions, reasoning, and secrets keep
  the existing treatment.
- Parsing consumes filtered evidence only. A parser failure does not turn a
  safe retained source into a filter failure.
- Publication ordering, frozen retry bytes, source verification, retention,
  and import confirmation remain under existing shared services.
- Source reads preserve limits, torn-tail behavior, rewrite detection, and
  the observed read boundary and consistency model. Cursor retains pass-level
  snapshot reuse.
- All fixtures are synthetic. Tests use injected environments, temporary
  homes, fake stores, and disposable acceptance environments.

## Proposed packages and dependency direction

```text
internal/agentmeta/          identity and descriptor types; standard library only
internal/agentapi/           integration contracts; may use archive value types
internal/agents/claude/      Claude native contracts
internal/agents/codex/       Codex native contracts
internal/agents/cursor/      Cursor native contracts
internal/agents/builtin/     immutable catalog/registry construction
internal/testutil/agenttest/ reusable integration conformance tests
```

CLI constructs the registry and injects contracts into the services that need
them. Config and metadata lookup receive the lightweight catalog, including a
projection of implemented operations. Capture, collector, and backfill receive
only their narrow operational interfaces, not the whole registry. Neither
`archive` nor `agentmeta` imports the registry or concrete integrations.
Concrete integrations never import CLI, capture, collector, or backfill.

`archive` retains source/metadata types, shared privacy helpers, normalized
types, common derivation, and rendering. It exposes narrow pure helpers needed
by native filters/parsers; extraction must not create an unrestricted bypass
around filtering. Metadata, labels, transcript, and handoff entry points accept
an already computed `archive.Analysis`, rather than an injected parser or
concrete codec. The caller resolves the parser and parses once. This makes the
dependency direction explicit and prevents each renderer from reparsing the
same bundle.

The contracts must not introduce network, process, Keychain, or terminal
dependencies into capture, including transitively. Concrete integrations use
injected host operations for discovery and executable probes. Extend existing
depguard and architecture tests to enforce these boundaries.

## Interface sketch

The examples omit supporting fields and full exported comments. They specify
ownership and intended shapes, not copy-ready Go declarations.

### Identity, catalog, and registry

```go
// agentmeta
type ID string

type Descriptor struct {
    ID          ID
    Aliases     []string
    DisplayName string
    Operations  []Operation // derived projection in the composed catalog
}

type Catalog interface {
    Lookup(name string) (Descriptor, bool)
    All() []Descriptor // stable presentation order; return defensive copies
}

// agentapi
type Integration struct {
    Descriptor agentmeta.Descriptor
    Launcher   Launcher
    Lifecycle  Lifecycle
    Sources    SourceProvider
    Filter     TranscriptFilter
    Parser     TranscriptParser
    Discovery  Discoverer
    Skills     SkillProvider
    Runtime    RuntimeDetector
}

type Registry interface {
    Catalog() agentmeta.Catalog
    Lookup(name string) (Integration, bool)
    Supporting(operation Operation) []Integration
}
```

Construction validates unique canonical IDs and aliases, safe key components,
stable ordering, and coherent combinations. Live capture requires a hook
decoder, sources, and filter; managed hook setup additionally requires a hook
configurator. Backfill requires discovery, sources, and filter. Parsing is a
distinct capability: safe sources can survive a parser failure or an
unavailable parser. Production full-support integrations provide both filter
and parser. Runtime detection and skills are optional. Launch-only support is
valid. Reject typed-nil interface implementations during construction; do not
treat them as implemented operations.

Use concrete immutable catalog/registry implementations rather than several
interchangeable backends. Consumer interfaces remain small. Build lookups and
operation projections once; do not allocate an `All`/`Supporting` slice inside
each session scan. No initialization probes the filesystem, discovers versions,
or opens databases.

Operation support is derived from populated interfaces, avoiding duplicated
capability booleans that can disagree with implementations. Capability evidence
is separate: documented, fixture-tested, installed-version verified,
unavailable, and unknown do not collapse into an implementation boolean. Status
combines that evidence with host discovery and observed capture health.

Canonicalization preserves trim/lowercase behavior and `claude-code` aliasing.
Put built-in identity/alias declarations in the standard-library-only
`agentmeta` package; operational composition binds implementations to those
declarations and checks for missing/duplicate bindings. This lets historical
archive decoding normalize legacy aliases without importing `agents/builtin`.
Additional test integrations supply descriptors through their injected catalog.
Normalize external names at ingress and carry canonical IDs internally; do not
thread a registry through every pure archive helper. An unknown normalized name
stays representable in archived metadata. Explicit CLI operations can reject
unsupported names without making storage readers reject unknown harnesses.
Registry lists are used for known-agent probes, not to filter unknown agents
out of a general archive listing.

### Lifecycle and hook plans

```go
type HookConfigurator interface {
    Plan(HookPlanRequest) ([]FileChange, error)
    Inspect(HookInspectionRequest) (HookInspection, error)
}

type HookDecoder interface {
    Decode(context.Context, HookInput) ([]LifecycleEvent, error)
}

type Lifecycle struct {
    Hooks   HookConfigurator
    Decoder HookDecoder
}

type NativeSession struct {
    Agent    agentmeta.ID
    NativeID string
    Version  string
    Mode     string
}

type LifecycleEvent struct {
    Kind     EventKind
    Session  NativeSession
    Project  ProjectEvidence
    Source   *SourceRef
    Start    *StartEvidence
    Parent   *NativeSession
    Evidence []archive.SupplementalEvidence
}
```

Common event kinds express a start candidate, turn start, response, stop, and
subagent observation. Ignored native events yield no events. A hook may produce
multiple events: Cursor's first prompt can report a start candidate and a turn
start, without a Cursor branch in capture. Validate the entire bounded event
batch before changing state. Apply it under the existing lock rules in native
order. Each effect must be idempotent under duplicate delivery and retries. An
I/O failure can still leave durable partial effects: this proposal does not
promise a new multi-file transaction. Test interruption after each effect and
preserve the current deferred admission-intent behavior.

`HookInput` includes the bounded decoded payload and observation time. Native
payloads stay local to the integration; raw payloads are never serialized as
common events or diagnostics. Supplemental content uses existing filtering and
validation paths before persistence; the interface is not a trust exemption.

`StartEvidence` is a typed result: explicit fresh start, explicit continuation,
unknown, or a request for bounded source inspection. It records a fixed reason
code and relevant identity facts. The integration interprets native fields;
capture verifies evidence and applies policy. Missing/empty transcript evidence
is valid only for an integration with an explicit tested contract for it.
Freshness inspection is constrained to bounded local operations and stays
within the existing hook budget, never opening an expensive source pass.

Parent/child native IDs are agent-qualified. Shared code resolves archive IDs,
validates ownership, and assigns inherited admission/destination boundaries.
The current index in `state/sessionindex.go` hashes only the native ID, so this
is a confirmed migration requirement, not an optional future audit. Introduce a
`SessionKey { Agent, NativeID }` with an unambiguous versioned encoding before
supporting another production agent. Keep archive session IDs and remote object
keys unchanged. Native store duplicates within one agent retain explicit
identity-conflict handling; do not invent an unverified store namespace.

On a qualified-index miss, inspect the legacy index and its referenced
registration. Adopt it only when the canonical harness and native ID both
match; an entry for another agent is never reused. Repair missing/corrupt
indexes outside the hook hot path where a registration scan would be needed.
Registration, retention, undo, subagent links, and deferred intents must use
the same key and existing lock ordering. Make conversion resumable and
idempotent, and test collisions, expiry during registration, and interrupted
conversion. Define old-binary/downgrade behavior explicitly: preserving remote
readability does not imply that an older binary understands a new local index.
Avoid unqualified dual writes that would recreate cross-agent collisions.

`HookPlanRequest` carries injected locations, existing bytes, installation
ownership, and an install/remove action. Inspection has a separate typed
result; it must distinguish absent, owned, foreign, and unreadable settings.
The native integration defines paths, event names, nesting, and ownership
matching. It reuses the current byte-preserving JSON editor. A shared
transaction layer applies `FileChange` plans, with atomic writes, journal
recovery, and rollback. Extract a low-level change type if needed; `agentapi`
must not import `setupjournal` or create a cycle with `hooks`. Inspection
supports status without writing settings.

### Source access

```go
type SourceRef struct {
    Kind SourceKind
    Path string
    Key  string
}

type SourceProvider interface {
    Describe(SourceRef) (SourceSemantics, error)
    OpenPass(context.Context, SourceEnvironment) (SourcePass, error)
}

type SourcePass interface {
    Signature(context.Context, SourceRef) (SourceSignature, error)
    Read(context.Context, SourceRef, ReadLimits) (SourceSnapshot, error)
    Close() error
}

type SourceSnapshot interface {
    Signature() SourceSignature
    Input() NativeInput
    Close() error
}
```

A source signature is a bounded, versioned equality token for that source,
suitable for scan state. Define its serialization and maximum size; use
provider-qualified tokens rather than `any` values or reflection-based
equality. File sources retain size/mtime semantics; database signatures
identify the particular chat rather than the database's global mtime. Source
identity and freshness are distinct from change signatures. Keep reading legacy
scan signatures during transition.

Open passes lazily for providers actually used in the operation. Opening a pass
does not eagerly snapshot a database; an unchanged-only pass creates no copy.
Passes are single-owner and serial initially, matching the current Cursor
reader; future parallel collection requires a separate concurrency contract. A
snapshot cannot outlive its pass.

The pass owns shared read resources; each snapshot owns its own handles. Both
have explicit, idempotent cleanup on success, failure, and cancellation.
Cleanup errors are joined with the primary error rather than discarded. Failure
to remove a snapshot containing raw content is a failed pass, as it is today.
`Input` accesses borrowed data without reparsing or copying it on each call.
Signature checks must remain cheap: checking an unchanged Cursor chat must not
force a database copy. `Read` returns the observation of its read boundary
under the provider's consistency model, not a promise of transactional snapshot
isolation.

`NativeInput` is a small explicit union of file input and native-record input,
with typed attachments such as Claude subagent metadata. File input exposes a
bounded `io.ReaderAt` plus its observed length, so format detection can replay
a prefix or section without buffering a whole transcript. Record input exposes
ordered native records with framing kind, identity key, and raw bytes; Cursor
composer/header and bubble rows can use their existing `json.RawMessage` bytes.
Missing native rows remain representable. Avoid exporting database records by
marshaling an artificial JSON envelope and decoding it again in the filter.

There is no `any` payload registry, generic unbounded object map, or arbitrary
path callback. Borrowed bytes are immutable to consumers. File input is valid
only for the snapshot lifetime; a streamed native record is valid until the
next iteration, and must be filtered before advancing. This permits bounded row
iteration without retaining every raw record. Codecs never receive a live
database connection or open arbitrary paths. Providers enforce raw and record
limits; shared post-filter checks enforce retained-size limits. Format hints
are not proof of safety. Fallback is allowed only for a typed format-mismatch
result, not an arbitrary filter error, an exceeded limit, or unsafe content.
Characterize today's Cursor JSONL/text selection before assigning these error
classes; extraction must preserve existing accepted inputs. Any stricter
malformed-input refusal is a separate reviewed filter change, with the required
version bump.

Source framing must explicitly express append-only JSONL, whole-document text,
and database records. Moving today's JSONL-boundary calculation outside the
collector must preserve its behavior rather than applying it to every format. A
section reader is a bounded read, not an immutable snapshot: another process
can rewrite the same inode during filtering. Preserve the existing pre-read
observation and next-pass invalidation semantics during extraction; do not
claim transactional consistency. The provider documents its consistency model.
Append-after-boundary belongs to the next pass. Test same-size edits,
truncation, replacement, and edits during read. Any stronger before/after
verification is a separate correctness change with its own retry and
performance assessment.

Source refs are local operational data: paths and keys do not automatically
enter uploaded metadata. Providers validate native path/session relationships.
Separate stable native session identity from the source locator. A valid
continuation may report a relocated transcript, as current file-source handling
allows; do not accidentally prohibit that with an immutable `Path` rule.
Locator updates require integration validation and shared continuation checks.
Cursor late adoption of a missing transcript and its stricter path ownership
retain their current behavior. A source-kind/backend change is not a locator
update. New source kinds are provider-qualified and validated by their
provider. Today `archive.SourceKind` validation recognizes only file and Cursor
SQLite; remove that closed-domain assumption without weakening registration
structural validation. Keep legacy file/Cursor source fields and decoding while
adding an optional local signature representation only if necessary. This is a
local-state compatibility change, not a remote source-schema change.

`SourceSemantics` explicitly declares `AppendOnly` or `ReplaceableSnapshot`
rewrite behavior and the signature/consistency contract. Shared collector code
compares candidate evidence with the retained baseline. An append-only rewrite
is blocked; an allowed replacement records a bounded cumulative gap and retains
publication provenance. This replaces the current `SourceKindCursorSQLite`
branch in `sessionScan.guard`. A codec cannot silently choose to discard the
baseline. Generic policy reasons map to existing Cursor gap codes during
extraction so output remains unchanged. Refiltering across version changes
keeps its existing stronger rules.

### Filtering, parsing, and observability

```go
type TranscriptFilter interface {
    Versions() FilterVersions // adapter and filter versions
    Filter(context.Context, NativeInput, FilterContext) (
        FilterResult, error,
    )
}

type TranscriptParser interface {
    Version() string
    Parse(context.Context, archive.SourceBundle) (archive.Analysis, error)
}

type FilterResult struct {
    Transcript  archive.FilteredTranscript
    Observation NativeObservation // local identity, bounds, harness observations
}

// archive: shared value types, with no dependency on agentapi
type Analysis struct {
    View          NormalizedView
    Facts         NativeFacts
    Observability Observability
}
```

Keep filtered native shapes in `SourceBundle`; do not upload `NormalizedView`
or `Analysis` as a second transcript. Analysis is computed once per source
within an operation and passed to metadata, labels, transcript, and handoff
builders. It is not a process-global cache. It cannot refer to closed raw input
resources. If caching is later added, key it by source identity and derivation
versions, with an explicit size budget.

`NativeObservation` carries the existing filter-time identity/time completeness
facts and harness observations needed to validate ownership and build a source
header. These cannot depend on an optional parser or be recovered only after
bundle creation. Bound and validate them; local identity facts are not copied
wholesale into the archive. Sanitized harness observations use existing source
header rules. `Analysis.Facts` contains semantic facts derived exclusively from
retained evidence. This separates local provenance checks from archived content
and avoids silently dropping subagent identity validation.

`Filter` owns native format selection and safe filtering; `Parse` owns native
semantic interpretation. `NativeFacts` must cover native session labels,
branch/PR evidence, workspace/model attribution, native turn outcome, and
child-session evidence in addition to visible turns. Today these are also
interpreted in `metadata.nativeTurnEnd`, `session_labels`, and subagent
collection, so moving only `ParseNormalized` would leave another incomplete
extension boundary. Separate native facts from shared policy: shared code
retains hook-versus-native outcome precedence, label precedence, admission,
counting, and presentation. Text transcript parsing also produces common turns
rather than making each renderer decode native text. Shared pure helpers can
still do redaction, tool linking, token aggregation, and common message
decoding. Extract only helpers with real multiple callers; avoid a generic
schema DSL or per-field callback framework.

`Observability` has typed availability for metrics such as compaction and tool
errors, plus fixed reason codes. It describes the actual bundle, including its
format and recorded filter version, not just the agent's current capability.
Unknown remains different from zero. Shared metadata derives counts and display
from this result instead of agent-name checks. Usage retains native meanings;
the refactor must not fabricate comparability across providers.

Filter and parser are separate interfaces, even when one implementation type
implements both. This lets metadata-only operations depend on a pure parser
without source access and distinguishes unsupported parser/format from unsafe
native input. Safe source retention survives partial parsing. Preserve current
filter/parse error classes and gap behavior. An integration's current parser
must support retained historical formats and filter versions for its agent;
dispatch cannot depend only on the current live-input format. Resolve
filters/parsers through injected lookup at read-back, handoff, refilter,
import, and collection entry points.

Initially all three integrations report today's shared version values. This
proposal does not introduce independent per-agent version policy. Extraction
alone does not bump versions. Any output change follows the existing
filter/parser/schema rules in [Versions](../../maintainers/versions.md), with
reviewed golden changes.

### Historical discovery

```go
type Discoverer interface {
    Discover(context.Context, DiscoveryRequest,
        func(SessionCandidate) error) (DiscoveryReport, error)
}

type SessionCandidate struct {
    Session   NativeSession
    Source    SourceRef
    Project   ProjectEvidence
    StartedAt StartTimeEvidence
    Parent    *NativeSession
}
```

Discovery uses injected, read-only filesystem operations and platform
locations. It reports candidates, conflicting identity evidence, and
content-free reasons for unreadable/incomplete stores. The callback supports
bounded enumeration and cancellation. An empty store is distinct from an
unreadable one.

Integrations provide native working-directory, workspace, and worktree-location
evidence. Shared backfill resolves it against configured projects,
deduplicates, applies boundaries, requests confirmation, creates import
batches, and handles undo. No discoverer can admit a session or bypass source
filtering. Stream emission does not make the final import plan constant-space:
document the existing plan materialization cost and avoid accumulating a second
full candidate list. Discovery ordering and duplicate/conflict resolution
remain deterministic; a callback error stops discovery without turning partial
enumeration into a complete plan.

### Launching, runtime detection, and skills

```go
type Launcher interface {
    Executables() []string
    BuildLaunch(LaunchRequest) (LaunchCommand, error)
}

type LaunchCommand struct {
    Executable string
    Args       []string
    Dir        string
}

type RuntimeDetector interface {
    Detect(RuntimeEnvironment) RuntimeObservation
    SessionEnvironmentKeys() []string
}

type SkillProvider interface {
    Plan(SkillPlanRequest) ([]FileChange, error)
    Inspect(SkillInspectionRequest) (SkillInspection, error)
}
```

`LaunchRequest` supplies a resolved executable, project directory, handoff
file, prompt, and explicit extra arguments. Integrations build argv without
shell fragments and validate native argument collisions. Shared launch code
resolves executables, prepares child environment, manages handoff-file
lifecycle, and selects the terminal. Keep the current positional-prompt
transport for this refactor. A future CLI requiring stdin gets an explicit
prompt-transport contract implemented across foreground and terminal launch,
including limits and cleanup, in that integration change. Do not add an unused
`Stdin` field that the existing terminal API cannot carry. Subcommands and
native argument layouts already fit `BuildLaunch`.

Runtime observations distinguish an exact session ID from agent presence with
project-scoped latest-session fallback. Shared selection owns ambiguity and
fallback policy. Environment cleanup removes declared session variables across
all integrations, preserves configuration variables, and strips trace state as
today. Noninteractive detection consumes the same runtime observations.

Skill planning and inspection use separate typed methods, following hook
configuration; an empty change list cannot mean both absent and healthy. Skill
plans include install, refresh, and removal. Shared templates and ownership
behavior remain in `agentskills`; integrations supply native locations and
invocation conventions. Shared destinations such as `~/.agents/skills` are
deduplicated and retained while any selected agent needs them. Skill inventory
discovery also uses declared native locations, so it does not remain a separate
hidden extension point in `evidence`.

## Compatibility strategy

Keep harness strings, aliases, source schema, metadata schema, object keys,
configuration fields, and CLI behavior stable during extraction. Translate
existing registration source fields into `SourceRef` in memory before proposing
a persisted representation change. Do not rewrite every registration merely
because a type moved.

Compatibility wrappers may delegate old entry points during migration, but
`archive` cannot import concrete integrations to implement a wrapper. Move
dispatch to callers and inject interfaces. Each phase migrates real consumers
and removes dead declarations; CI's dead-code checks rule out landing an unused
framework ahead of its first caller.

Existing pending publication bytes are replayed unchanged. Parser-only
rederivation must still avoid refiltering an unchanged native source. Compare
historical filtered bundles as well as current native-input fixtures.

## Implementation sequence

The user explicitly authorized stacked implementation before parent phases land.
Each child integrates committed parent ancestry without rewriting it; readiness
requires the final combined checks and independent review. Update this table
with PRs and implementation differences as work proceeds.

| Phase | Deliverable | Completion gate | Status |
| --- | --- | --- | --- |
| 0 | Characterization, identity migration design, performance baseline | Capture/index races, native-label/outcome paths, read consistency, and existing hot-path costs are recorded before contracts move. | [PR #278](https://github.com/wangjohn/agent-archive/pull/278): merged contracts and measured references; historical timing caveat remains. |
| 1 | Catalog, immutable registry, operation lookup | Config, flags, setup order, known-agent reader probes, and destinations consume one identity source; aliases and unknown archive names retain behavior. | [PR #290](https://github.com/wangjohn/agent-archive/pull/290): merged on the default branch; acceptance limits above apply. |
| 2 | Launch/runtime contracts for all three agents | Existing argument, environment, terminal, current-session, and noninteractive tests pass through injected integrations. | [PR #293](https://github.com/wangjohn/agent-archive/pull/293): merged on the default branch; acceptance limits above apply. |
| 3a | Qualified identity and migration | Opaque agent-qualified keys, compatibility adoption, races and recovery preserve archive identity. | [PR #295](https://github.com/wangjohn/agent-archive/pull/295): merged on the default branch; acceptance limits above apply. |
| 3b | Lifecycle and hook plans | Qualified native-key migration precedes decoder cutover; native names/payload interpretation leave capture; generic admission preserves fresh/resume, deferred intent, lock, parent/child, and transactional setup behavior. | [PR #298](https://github.com/wangjohn/agent-archive/pull/298): merged on the default branch; acceptance limits above apply. |
| 4 | Source passes and snapshots | File/text/database reads use common contracts; bounds, locator transitions, legacy signatures, typed failures, cleanup, declarative rewrite policy, and Cursor reuse are preserved. | [PR #299](https://github.com/wangjohn/agent-archive/pull/299): merged on the default branch; acceptance limits above apply. |
| 5 | Codec dispatch and bundle observability | Filters/parsers expose shared Analysis; native facts leave bundle/metadata/labels/subagent paths; one analysis feeds shared rendering; old and new fixtures remain equivalent. | [PR #300](https://github.com/wangjohn/agent-archive/pull/300): merged on the default branch; acceptance limits above apply. |
| 6 | Discovery, skills, contributor guide, extension proof | Backfill/evidence/skills use integrations; test-only fourth agent completes archive flows; temporary wrappers and duplicated lists are removed. | [PR #301](https://github.com/wangjohn/agent-archive/pull/301): merged on the default branch; acceptance limits above apply. |

Phases are review boundaries, not a promise of exactly seven PRs. Define
interfaces in the phase that first uses them; phase 1 does not land the entire
unused framework. The identity migration is independently reviewable before
lifecycle cutover. Split a phase when necessary, but every PR must have a
working consumer and green checks. Status discovery/version probing should move
with its relevant operation; phase 6 audits that no separate CLI agent
inventory remains.

## Failure and performance contracts

Validate boundary values centrally: events, source refs, filter observations,
analysis facts, and file plans are implemented by trusted code but represent
untrusted native input. Require bounded lengths/counts and consistent
identities. This validation must not repeatedly serialize entire source bundles
or rerun filtering. No integration is a security sandbox; the shared validation
and privacy helper contracts are reviewable correctness boundaries.

Failures need a common taxonomy, not provider-specific string matching. Define
typed results for source missing, temporarily unavailable, changed during read,
unsupported format, unsafe filter input, exceeded limit, parse failure, and
cleanup failure. Shared orchestration decides retries, gaps, and settled scan
state. Diagnostics contain fixed codes and safe bounded detail; underlying
errors with paths remain available for local `errors.Is`/`errors.As` only.

Do not memoize transient lock/read-race failures as settled. Deterministic
filter/size failures may be cached against source signature, filter versions,
and relevant limits, matching today's Cursor behavior. A provider cannot clear
a capture gap or make a source permanently skippable by returning a successful
signature. Signature results must distinguish missing from empty and failed
inspection from unchanged. Parsing and provider methods check context at
record/read boundaries; context alone is not a hard timeout for a blocked
filesystem call, so preserve existing OS-level and hook-budget constraints.

Record a baseline at `98f8bd0` using synthetic data and compare each hot-path
phase on the same host/toolchain. Required scenarios:

| Scenario | Structural performance gate |
| --- | --- |
| Disabled/paused/ignored hook | No version probe, source pass, database access, or registry reconstruction. |
| Fresh/start, stop, and subagent hook | No full registration scan for index migration; existing lock/read budgets hold; record subprocess p50/p95 and allocations. |
| 1k and 10k unchanged registrations | No content filtering, source-bundle decode, database copy, or remote source read; catalog/registry cost does not scale per session. |
| One changed file amid unchanged sessions | Filter the changed source once; no additional whole-transcript serialization/copy introduced by the interfaces. |
| Parser-version-only change | Reuse retained safe source; do not filter unchanged native input. |
| Many changed Cursor chats | At most one database snapshot attempt per pass; no artificial export serialization layer. |
| Large and near-limit records | Preserve bounded peak memory and limit outcomes; compare bytes/op, allocs/op, peak heap, and runtime with the existing large-record tests. |
| Repeated deterministic versus transient failure | Deterministic failure avoids repeated content work; transient failure remains retryable. |
| Labels plus metadata plus handoff | One native analysis per source per operation, without allocating another uploaded representation. |

Use counted-operation tests for these gates and benchmarks for throughput and
memory. The existing large-session collector tests, Cursor snapshot tests,
large-record memory test, and `scripts/measure-hook.py` provide starting
points. A reproducible greater-than-10% latency or peak-memory regression is a
review blocker pending an explicit measured justification or correction; it is
not a flaky wall-clock assertion in ordinary CI. Capture remains under its
existing absolute budgets regardless of relative benchmark results. No new
collector concurrency or retained global caches ship as part of this
extraction.

## Verification and definition of done

Use existing tests as characterization coverage before relocating behavior.
Review golden equivalence rather than blindly regenerating expected output. Add
reusable conformance suites for registration combinations, hook round trips,
fresh versus resumed sessions, bounded reads, cancellation/cleanup, unsupported
formats, privacy filtering, historical parsing, and unavailable metrics. Suites
run only capabilities an integration actually implements.

A test-only fourth integration must exercise the normal registry injection path
through hook capture, source filtering, publication, read-back, metadata,
handoff, backfill, and setup/removal planning. It uses synthetic formats and
fake host dependencies, lives outside the production registry, and proves that
no agent-specific core switch is needed for represented capabilities. It is not
evidence that a real fourth CLI is supported.

Architecture checks enforce that only production composition code imports
concrete agents (agent-specific and conformance tests may import them), core
packages do not perform concrete adapter assertions, and codec contracts do not
introduce side effects into archive parsing. Agent-specific fixtures and native
helper names are legitimate; prohibit dispatch leaks rather than every mention
of an agent string.

Run checks appropriate to each phase, then the repository's required macOS and
Linux checks before completion; see [Testing](../../contributing/testing.md).
Privacy/parsing changes follow the existing fuzzing requirements. Installed
version verification uses disposable synthetic-content sessions and read-back;
fixture success alone must not upgrade a capability to verified. Strengthen the
test-only fourth integration with deliberately non-Claude/Codex field names,
colliding native IDs, multi-file/record input, missing observability, replayed
hooks, transient errors, and permitted source rewrites. Run conformance tests
separately for the existing file and database consistency models. Do not make
the extension proof another fixture that happens to use existing JSONL shapes.

Done means the three integrations preserve behavior and compatibility, the
fourth-agent extension proof passes, source and hook budgets do not regress,
and [Adding an adapter](../../contributing/adding-an-adapter.md) becomes a
concrete integration recipe instead of a trail through shared packages. Update
the architecture map and capability documentation with the implementation.

## Phase-entry artifacts and recommended defaults

Phase 0 artifacts are the [contract checklist](agent-integration-contracts.md) and
[performance reference](agent-integration-baseline.md), including the unresolved
historical growing-file timing difference. Phase 0 should produce a short contract checklist beside this proposal,
updating these details in place before dependent implementation starts. No
production abstraction is required to record the examples and baseline.

The contract checklist also records target #277's `transcriptio` verified reads
and `nativesessions` discovery helpers. Later source/discovery extraction must
consume those real boundaries and preserve observable-write rejection; the
performance report retains its earlier pinned measurement context.

Live main at `d05defabb8ee1d7f268ee854882ca98dcfc0bde5` also includes native
handoff before setup (#280). Later catalog/launch/runtime/source/codec/discovery
phases must cover its existing read-only native selection, bounded filtered
previews with explicit partial coverage, complete selected-source filtering,
identity revalidation, and private temporary launch-file lifecycle. Reuse its
`nativesessions.Discover` and `transcriptio.Snapshot.Records` boundaries while
keeping preview framing distinct from full transcript filtering and import
compatibility. Preview native facts are another phase-5 owner, not permission
to parse a whole source for every display row. Historical companion measurements
are not measurements of this newer target; the companion artifacts record
these additional obligations without changing pinned samples or hashes.

Target `4add976435af4b91d1dfbd31b3c2496316a0d3f4` also includes archived
handoff no-prompt selection (#257): preserve known-zero versus unavailable
human-turn counts, local prompts newer than the archive copy, and exact-ID
accessibility. These remain shared selection policies when the native facts
and availability move into Analysis.

Target `e3b82989d56903966a0fb1c6a774c18c15f5e838` extends native-handoff
budget and cancellation behavior: release unused header reservations after
workers finish, preserve actual cumulative read accounting, and keep verified
identity-only picker rows available without preview reads after budget exhaustion.
Cancellation is checked during checkout scoping and before loading previews.
Incomplete discovery cannot silently choose latest or the current session;
interactive fallback requires an explicit choice and noninteractive fallback
offers qualified native-ID/agent/checkout recipes. These remain shared selection
and resource policies, with no replacement of historical performance samples.

Latest target `0ffa628e84a2b4e6265ffa2ff481788269027567` also introduces
informational machine registration and independent publication retries, bounded
portable project matching, revised storage creation/confirmation and filter15
pairing-bundle redaction. Preserve the target's shared setup, destination,
credential provenance and capture-scope policies. Discovery extraction must
retain conservative incomplete matching and bounded native history, while codec
extraction keeps filter15/adapter0.15.0 redaction and golden output intact.
Historical timing/instrumentation remains unchanged and does not measure these
newer filter, setup or registration workloads.

| Artifact | Recommended default | Required before |
| --- | --- | --- |
| Qualified identity/index migration | Keep native IDs opaque. Hash an unambiguous versioned encoding of canonical agent ID and native ID into a new qualified namespace. Adopt a legacy entry only after validating its registration; preserve archive IDs. Specify interrupted conversion, corruption recovery, retention races, and downgrade handling. | Phase 3 identity migration |
| Freshness truth table | Explicit fresh/continuation/unknown evidence, plus typed bounded inspection requests. Preserve Claude/Codex startup/clear versus resume/compact and Cursor first-prompt rules exactly. Unknown evidence cannot admit a never-seen session. | Phase 3 decoder cutover |
| Source input examples | Bounded `ReaderAt` plus length for files; ordered raw native records for database input; typed bounded attachments. Demonstrate JSONL, Cursor text, composer/bubbles including missing rows, and Claude metadata without a serialization round trip. | Phase 4 source cutover |
| Signature and read-outcome encoding | Provider-qualified, versioned equality tokens with a fixed maximum size. Missing, empty, transient failure, and deterministic failure remain distinct; legacy file/Cursor signatures decode unchanged. Document the maximum after checking existing native fields, not by guessing. | Phase 4 source cutover |
| Analysis ownership matrix | `archive.Analysis` contains the common view, retained native facts, and bundle-specific availability. Enumerate each current native-shape consumer and migrate its fact extraction; keep filter-time ownership facts out of parser-only paths. | Phase 5 parser cutover |
| Host/configuration request types | Inject narrow read/probe dependencies; use separate inspection results and mutation plans. Integrations never apply plans or execute arbitrary programs. | The first phase using each operation |
| Performance report | Same-host/toolchain baseline, work counts, peak heap, allocations, and relevant latency distributions for the scenarios above. Investigate reproducible regressions rather than changing golden output or limits to hide them. | Every hot-path migration |

Use examples from the current three agents to decide supporting fields. Keep
signatures small and avoid adding fields or optional methods solely for an
uninvestigated future CLI. Supporting a new capability later can extend a
contract through a separate reviewed change.


## Review disposition (2026-10-01)

Proceed with the integration direction after the preimplementation decisions
above are resolved. This review tightened the proposal in nine places:

- Replaced parser injection into archive builders with shared `archive.Analysis`
  value types and one parse per operation, avoiding an import cycle and repeated
  traversal.
- Split filtering from parsing and hook configuration from runtime decoding;
  made inspection an explicit result rather than an overloaded change list.
- Expanded native interpretation to include labels, outcomes, model/workspace,
  bundle observations, and subagent paths that the original sketch omitted.
- Made agent-qualified local identity migration a prerequisite after confirming
  the current index is keyed only by native ID.
- Added declarative rewrite semantics and distinguished session identity from
  relocatable source paths.
- Replaced an artificial database export stream with bounded raw-record input;
  clarified lazy passes, serial ownership, cleanup, and file consistency limits.
- Added typed failures and deterministic-failure caching rules that preserve
  retryability without repeatedly rereading unchanged bad sources.
- Added measured performance baselines and structural work-count gates; kept
  unsupported stdin transport, new concurrency, and global caching deferred.
- Replaced implied event-batch atomicity with validation plus replay-safe,
  idempotent effects under existing locks.
