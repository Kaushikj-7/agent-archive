# Large backfill completion

Status: approved product direction, implementation pending. Recorded 2026-10-17
against `a78571963534`, Go 1.27.2. This change updates specifications and records
isolated design-probe findings; it does not repair an installed binary or run an
operator import. Existing tracking:
[historical recovery #344](https://github.com/wangjohn/agent-archive/issues/344)
and [inspection performance #494](https://github.com/wangjohn/agent-archive/issues/494).

## Outcome

A person can import every supported, authorized, recoverable session in a large
local inventory without repeatedly adjusting an internal timeout or losing the
tail of the import to a scheduler allowance. Foreground backfill continues until
its selected work is verified, the person cancels, or specific evidence or
capacity failures require action. Every remaining candidate has an accurate
disposition and a recovery route where evidence supports one.

The lasting target is the [backfill completion contract](../specs/backfill.md#large-backfill-completion-contract),
with the matching [native-history work contract](../specs/codex-history.md#foreground-backfill-work-target).
Current shipped behavior remains separately described; no proposed switch or
format is advertised as available.

## Findings and limits of the investigation

Source inspection and content-free local diagnostics found two reproducible
mechanisms. A local clone can be imported and become a configured project,
then block later automatic recovery because its observed local-only origin is
represented as unknown. The existing proposed-root exception and configured-root
strict rule intentionally differ. That exception is not a safe fix to extend:
a filesystem origin may lead to another clone of the recorded repository, so
its stable local spelling does not establish a non-match. Also, native
preparation/inspection/admission reuse lookup allowances intended to protect
background work; allowance exhaustion can become a generic source-limit skip.

These findings do not establish that every refusal in a real inventory has the
same cause. One primary skip can hide another. Metadata counts do not prove
complete source availability, own-task attribution, admissibility or publication.
No private paths, native IDs, transcripts or credentials belong in this plan or
its evidence record.

The [initial design probes](#initial-design-probes) below record the synthetic
results and experimental limitations. An overlay is an experiment, not reviewed
production code. In particular, fresh
allowances per source do not solve a single source needing multiple scopes;
the initial experiment does not implement restart, staging or remote publication.

## Decisions

1. **Completion is the product invariant.** A short allowance is a scheduling
   boundary, not a reason to discard an otherwise importable session. Keep
   memory/disk limits, privacy filtering and evidence checks independent.
2. **Foreground and background have explicit policies.** Preserve the current
   background five-second responsiveness budget and short hook holds. Foreground
   uses renewable scopes/continuations with cancellation and progress, without a
   cumulative five-second command or session ceiling. Retain bounded individual
   I/O. Do not solve this by globally increasing constants or adding a required
   timeout-tuning flag.
3. **Observed local origins need ownership evidence.** Classify them consistently
   before and after project configuration, but never treat a stable local-only
   origin as a non-match by itself. Automatic recovery requires sufficient bounded
   local-origin-chain resolution; unresolved or ambiguous ownership requires a
   reviewed assignment. Read and renew all origin values, traversed dependencies,
   checkout identity and observation scope, without network access or config
   mutation. Cycles, unreadability, conflicts and incomplete traversal stay
   uncertain. Same-repository physical clones remain distinct competitors and
   existing source/ancestor exclusions remain binding.
4. **Review is a supported recovery mechanism.** Reuse exact bulk mappings and
   historical-only projects. Produce grouped suggestions with evidence/conflicts;
   no filename guess, silent global fallback or configuration deletion supplies
   authority. Keep ordinary reviewed live-project additions separate from mapping
   an old cwd or selecting a historical project.
5. **Scale concerns the whole command.** Planning, proof preparation, inspection,
   staging, admission, publication and restart must all advance. Fixing only the
   preview is insufficient. Publish/verify completed groups to relieve staging
   backpressure; do not expand the 1 GiB durable quota as the completion mechanism.
6. **Inventories may exceed caches.** Page/privately spool content-free facts and
   decisions; preserve full relevant project-witness evidence and native chain
   validation. Cache membership is never proof of completeness or uniqueness.
7. **Preserve supported partial evidence explicitly.** Independently valid child
   work may already import without a parent. Otherwise distinguish no independent
   activity, unsupported own-task shapes and incomplete dependencies. Reuse the
   existing separately versioned fragment/repair work; do not flatten inherited
   content into a fictitious complete session.

## Delivery in focused PRs

PR numbers below are delivery labels, not already-open pull requests. Each needs
its own current-head review, required checks and accurate user documentation.

| PR | Scope and concrete result | Depends on | Exit evidence |
| --- | --- | --- | --- |
| 1 | Preserve `lookup_allowance_spent`; distinguish scheduling exhaustion, actual source limits and missing evidence. Add focused regressions and baseline counters for phases/scopes/unfinished candidates. | This contract | No generic limit for a known allowance cause; unchanged privacy/admission decisions; source-size refusals remain specific. |
| 2 | Classify configured/proposed local-only origins consistently; add bounded, renewed local-origin-chain identity evidence for automatic recovery. Keep unresolved/conflicting roots uncertain and existing reviewed assignments usable independently. | This contract; reuse recovery foundations | Real synthetic Git covers an unrelated resolved chain, a local-origin clone of the portable-key repository remaining a distinct competing clone, first/second import, cycles, missing/unreadable targets, chain changes, multiple origins and exclusions. Unresolved/ambiguous cases recover only through supported reviewed assignments. |
| 3 | Give foreground native preparation and inspection renewable work policies/continuations. Deduplicate requested identities, reuse validated facts, release idle caches, advance fairly and report progress/no-progress. | PR 1 | Real CLI cold/warm dry runs above 256 probes finish; deterministic allowance rollover, cancellation, one source requiring over five seconds and a moving index retain selection validation. |
| 4 | Carry continuation through confirmed staging/admission and publication; renew proofs in the correct ownership scope; resume exact batch/mapping identity after restart; drain settled groups under staging pressure. | PRs 1 and 3 | Preview-successful supported sessions also stage, register, publish and read back. Interrupt before/after each durable step; delete native evidence after staging; saturate quota and preserve outstanding owners. |
| 5 | Remove inventory-window completion ceilings with paged/spooled content-free source/project/revision work and bounded decision/report windows. Retain full relevant uniqueness/exclusion evidence across windows. | PRs 2–4 | 5k/50k candidate inventories, over 1,024 witness roots and inventories over disposable catalog/listing windows complete without changing source safety limits or asserting partial uniqueness. |
| 6 | Add private grouped recovery suggestions and a review/export path using the existing mapping schema and historical identities. Expose ambiguity and suggestions rejected by exclusions. Partition oversized reviews with exact continuation/undo attribution. | PRs 1–2; integrate PR 5 windows | Mapping review changes no capture authority; accepted exact assignments unblock supported sessions; rejection/exclusion/conflict remains effective; restart uses the same semantic assignments. |
| 7 | Reconcile physical/logical coverage and all lifecycle stages; classify true empty children, unsupported own-task formats and incomplete history. Add compatibility fixtures only where real format evidence warrants them; show fragment availability honestly. | PRs 1, 3–6 | Each enumerated candidate has one physical disposition, logical owners are not double-counted, independently admissible work is retained, and every residual has a specific action. Publication pending is not presented as archive completion. |
| 8 | Qualify the combined implementation, release behavior and operator recovery. Update guides/help/changelog to the actually shipped contract and record remaining gaps. | PRs 1–7 | Supported-platform, disposable-provider, scale and final-head CI/review gates below; then a separately authorized local recovery import with verified read-back and residual accounting. |

PRs 2 and 3 can be developed independently after their shared contract is fixed.
PR 4 must not wait for fragment support, mapping assistance or broad performance
optimizations: existing exact mapping files can provide the early recovery route.
Those mappings and historical destinations must not wait for local-origin-chain
resolution or PR 5's larger-window work. PR 2 may enable an automatic case only
after its ownership evidence is sufficient; unresolved roots remain uncertain.
PR 5 provides the larger-than-one-window guarantee. PR 6 improves the operator
experience; its first useful grouped suggestions can precede full paging.

Fragment import remains the existing [historical recovery step 6b](historical-backfill-recovery.md)
and [fragment contract](../specs/codex-fragments.md). Separate reader/filter,
confirmed writer/lifecycle, and repair/acceptance PRs may follow those gates.
They are required to claim recovery of supported incomplete histories, but must
not delay the complete-history work above. The separate
[segmented-history proposal](codex-segmented-history.md) owns genuinely long
individual histories; do not duplicate it by raising graph/record limits here.

## Initial design probes

The focused feasibility run used Go 1.27.2, Darwin/amd64, race detection,
synthetic source records, temporary homes and private SQLite indexes. The CLI
called its actual `backfill --dry-run --json` path. Temporary code overlays
renewed preparation work between bounded groups, gave each native source
inspection a fresh lookup allowance, and applied the existing proposed-keyless
rule to the configured-keyless case. No production source was changed or
installed, and archive/config state remained unchanged in each CLI dry run.

| Probe | Baseline | Experimental result |
| --- | --- | --- |
| Three valid child sessions with a fresh allowance | 3 planned | 3 planned |
| 512 tiny child sessions with indexed current rows | Recorded run: 374 planned, 138 generic limit skips; an earlier run planned all 512 | All 512 planned |
| Three valid children with preparation's time allowance deterministically exhausted before inspection | 0 planned, 3 generic limit skips | All 3 planned |
| Missing-worktree recovery with a synthetic configured local-only clone | Refused | Accepted the configured target by ignoring the local-only clone; uniqueness was not established |
| Exact mapping, unreadability, excluded original cwd and identity change | Mapping succeeds; unreadable/excluded evidence refuses recovery | Same controls hold; a recovered proof is rejected if the clone gains the matching key before admission |

The exhaustion injection models an already-spent allowance without sleeping;
it is not a measurement of five seconds of work. Baseline variation with local
load shows there is no universal 256-session ceiling. These were correctness
probes, not matched performance benchmarks. Repository classification was
injected through the existing observation seam, so actual Git classification,
multi-origin/exclusion combinations and repeat-import behavior still require
the implementation regressions in PR 2.

[Ownership review](https://github.com/wangjohn/agent-archive/pull/678#discussion_r4306094199)
identified a flaw in the blanket proposed/configured-keyless
overlay: the supposedly unrelated local clone could instead lead to the same
portable repository, yet the overlay would still ignore it. Reobserving an
unchanged local origin does not repair that proof. The probe did not exercise
this case; its successful recovery count is feasibility evidence only, not a
safe production policy. PR 2 must replace the blanket exception with sufficient
chain evidence or reviewed assignment for proposed and configured roots alike.

The experimental resets are insufficient as a production solution: they do not
continue a single source needing multiple scopes, implement durable restart or
qualify confirmed staging/publication. Synthetic probes and their small raw
evidence are retained as task artifacts for the implementers; PR 1 should turn
the relevant probes into maintained regressions against the actual implementation.

Three existing isolated child/fork/historical-publication regressions also
passed with race detection, as did documentation/version links and existing
synthetic Git recovery controls. No 5k/50k run, saturated staging, real provider,
installed-binary repair or operator import is established by these findings.

## Engineering ownership and compatibility

- `sourcefacts` owns repository classification and recovery proofs; `gitremote`
  owns bounded native Git observation. Do not add Git subprocesses in hooks,
  admission locks or per-session sweeps over unchanged inventories.
- `discovery`/`agentapi` own native lookup work policy and evidence contracts;
  native source providers own selection, snapshots and history continuations.
  `backfill` owns orchestration and bounded inventory/decision state; `cli`
  selects foreground/background policy and presents review/progress.
- `state` and the existing staging/publication owners retain crash safety,
  identity, quota and exact undo behavior. A work cursor cannot grant publication
  authority. Release active native leases before replacing a lookup epoch;
  share the existing charged ledger rather than creating an uncharged cache.
- Prefer private disposable continuation for dry-run. A confirmed durable cursor
  or new proof format needs schema/compatibility review, bounded strict decoding,
  old-writer fencing where applicable and migration/recovery tests before writes.
- Follow the [version rules](../maintainers/versions.md): scheduling-only changes
  do not imply a source-format change; any change to retained/dropped content,
  own-task interpretation, derived metadata or persisted authority gets its
  applicable filter/adapter/parser/schema/protection updates, privacy review,
  synthetic goldens and fuzzing. Decide from the actual patch, not the PR label.

## Work-scope protocol

Each scope has a finite work/I/O allowance and explicit ownership of its native
snapshot/lookup facts, memory charges and selection checks. At a boundary the
consumer reports completed work, continuation, source change, a specific
unrecoverable evidence/format failure, or no progress. A continuation includes
only bounded private source/selection and policy bindings sufficient to resume
and revalidate; no raw transcript or raw remote URL becomes a diagnostic record.

Do not renew the same failed operation indefinitely. A renewable allowance
exhaustion is distinct from record/raw-size/graph limits, genuine disk pressure,
an unavailable provider and an invalid own-task proof. Fair scheduling moves
other candidates forward while affected candidates retain accurate pending state.
Counter-based tests pin progress and maximum repeated work; elapsed time alone
must not be the correctness oracle.

Preparation and source inspection share reusable validated facts, not a
single consumable lifetime for the job. Confirmed admission obtains renewed
authority before durable staging/registration; publication uses exact retained
evidence after admission, including after native deletion. Renewals do not
reinterpret an already admitted session's provenance or extend its capture age.

## Qualification and measurements

| Tier | Inventory and scenario | Required observation |
| --- | --- | --- |
| Focused short | 3 sessions; exact mappings; resolved unrelated and same-repository local-origin chains; cyclic/unreadable/conflicting/excluded/identity-changing controls; deterministic spent allowance | Actual CLI decisions, distinct physical-clone ambiguity, reviewed recovery for unresolved ownership, unchanged dry-run archive/config state, precise causes and revalidation. |
| Window boundary | 512 native children/forks and mixed ordinary sessions, cold and warm | Every eligible logical session reaches a plan and confirmed publication across multiple scopes; >256 probes is not a source refusal. |
| Representative | 5,000 sessions, shared ancestors, duplicates/revisions, local origins and multiple projects | Bounded descriptors/charged memory; no starvation; cancellation/restart and quota-draining publication preserve exact identity. |
| Extended | 50,000 candidates; >1,024 witness roots; large current indexes, flat/day stores and inventories over cache/listing windows | Complete reconciliation without cache-sized result ceilings, repeated whole-inventory source reads or partial uniqueness claims. |
| Slow work | Deterministic simulated >5s job and >5s supported single-history lookup; representative actual I/O timings separately | Work continues/revalidates across scopes; cancellation works; no required global timeout increase. Simulated time is labeled as such. |
| Lifecycle/provider | Confirmed foreground and background, stage saturation, pause/provider outage, restart, source deletion, undo and retained reads | Actual production entry points, byte/digest source-set verification, no loss of admitted evidence or unrelated pending work. |

Record command, actual source/overlay digest, toolchain/OS/architecture, fixture
sizes, terminal results and which cases were not run. Measure planning versus
filtering versus staging/admission versus upload/read-back, cumulative lookup
time, slices/renewals, headers/SQL/Git operations, retry/no-progress counts,
peak RSS and logical charges, disk/stage high water, worker/descriptor cleanup.
RSS and charged bytes are different measures. Retain small evidence separately
from compiler caches and remove only task-owned idle scratch.

Use Go 1.27.2, the [testing guide](../contributing/testing.md), current lint,
synthetic fixtures and fake homes/services. Each implementation PR starts with
affected race regressions and appropriate full checks. Parsing/redaction changes
need the documented two-minute affected fuzz runs and reviewed golden/version
updates. Extended CI must run the full scale/lifecycle cases and platform lanes
selected by the actual change; short probes cannot substitute for those gates.

The operator acceptance run is separate from development tests. First save a
private metadata-only inventory and proposed exact assignments, review original
scope restrictions/retention and historical destinations, then confirm the
supported import. Keep the mappings for exact resume. Count remotely verified
logical sessions and reconcile all residuals; do not use successful registration
or disappearance of one skip reason as proof that recovery is complete. This plan
does not authorize changing native app settings, deleting sources or bulk running
development builds against a live archive.

## Coordination and sequencing limits

Do not duplicate #494's streaming inspection/cache work or #344's fragment and
retained-history lifecycle. At planning time, open #644 and #645 cover exact
generation and privacy prerequisites; open #674 adds Muse historical imports.
Recheck their actual merged state at implementation time. Keep foreground
orchestration generic, with Codex-specific evidence in its current integration.
Do not wait for unrelated PRs or rebase solely because main advanced.

No PR is complete until its required current-head CI and independent review
pass. This plan is not merge authorization for unrelated work. The focused
prototype and local documentation checks recorded with it are not claims that
PRs 1–8, full-size acceptance, installed-binary repair or an operator backfill have
already happened.
