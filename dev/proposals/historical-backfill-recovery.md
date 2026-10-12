# Historical backfill recovery: implementation plan

Status: partially implemented; remaining milestones remain gated. Prepared
2026-10-06 against `f0dca28b1191`; historical-only ownership and bounded bulk
mappings merged 2026-10-12 in
[#433](https://github.com/wangjohn/agent-archive/pull/433) (`18ca0c48`).

Step 4a now stages the selected filtered source set, including preserved Codex
revisions and qualified children, before admission. A bounded SHA-256 journal
links identity, selection, privacy context, destination and batch ownership to
registration. Recovery scans at most 50 session directories per cursor slice;
explicit foreground recovery drains slices. Staging and publication share the
1 GiB quota with reserved cancellation controls. Native deletion, admission
crashes, remote readback failures, quota retry, privacy replacement, pause,
cancellation and undo have synthetic lifecycle coverage. The writer capability
is fenced as `import-staging-v9` only with staging-aware mutation paths.

This completes the pre-admission staging gap on top of existing source-set
publication and revision maintenance; it does not establish live provider,
platform or signed release acceptance. Step 6a now implements historical-only
destinations and bulk mappings. Fragment import and remaining scale/release
gates below remain separate work.

## Current implementation and remaining scope

Diagnostics (#345), portable Git recovery (#347), shared rollout lookup (#348),
history lifecycle (#341), first-run recovery (#349), and native child/history
capture (#355) have landed. Later recovery fixes (#387, #393–#395, #397–#398,
#403, #408, #410, #417) harden inventory, per-app isolation, admission and paths.
See `internal/backfill`, `internal/discovery`, `internal/collector/history_*.go`
and [the retained-history contract](../specs/codex-history.md).

Steps 1–5 now have substantial implementation; this does not establish every
original completion gate. Historical-only ownership without a live checkout
and bounded bulk mappings (step 6a) are implemented under the contract below.
A separately versioned fragment artifact and lifecycle (step 6b), and the full
portability/provider/older-writer acceptance matrix (step 7), remain open.

The gap descriptions and delivery gates below preserve the reviewed starting
point; claims that all related histories are pending, all history writes are
fenced, or historical ownership is still missing describe that baseline, not
current behavior. The current step 6a section records its implemented behavior;
the remaining delivery gates do not imply completed platform acceptance.


Staff review: revised 2026-10-06. The sequence below incorporates the findings
from an architecture and lifecycle review. Implementation can begin with
diagnostics and portable Git recovery; history and fragment writers require
the explicit contract gates below first.

## Review findings and decisions

| Finding | Adjustment required before the affected implementation |
| --- | --- |
| Import registration currently queues a later native read; transcripts may vanish before upload. | Confirmed imports durably stage filtered evidence before admitting the session. Journal staging, registration and publication recovery together. |
| Semantic Git revalidation is underspecified and could become a subprocess per session or an ongoing live-repository requirement. | Define observation epochs and shared per-slice validation, with explicit bounds. Freeze ownership at admission; later replay does not require the old checkout or a new unique remote match. |
| One lifecycle change contains publication, privacy mutation, retention, undo and compatibility. | Split lifecycle work into dependent changes with writer enablement last. Make preservation limits and downgrade protection concrete contracts. |
| Dependency availability does not establish permission to publish inherited content. | Specify source-root confinement and ancestor authorization before enabling related-history imports. Excluded ancestors must not leak through retained bundles, handoff or export. |
| Physical identity, catalogue completeness and already archived sessions need explicit reconciliation. | Separate stable thread identity from revisions, bound inventory epochs, and define upgrades of existing registrations plus undo of those upgrades. |
| Historical destinations and fragments are two separate product/schema changes. | Give each a contract and reader/lifecycle gate. Deliver them as separate later milestones without blocking complete-history recovery. |

This review also corrects two overly broad requirements: ordinary backfill keeps
its existing project-addition/capture behavior, and detailed history accounting
cannot be completed before the rollout lookup exists.

## Outcome

Backfill should archive recoverable local history even when its original checkout
has disappeared or a Codex conversation spans child, fork, or revised rollout
files. The result must remain readable after the native files disappear, support
continued collection, privacy updates, retention and undo, and explain every
entry it cannot reconstruct. This is a portable product change, not a repair
specific to one user's paths, installed Git, or project configuration.

The implementation targets the project's supported macOS and Linux platforms,
on Intel/x86-64 and arm64. It does not add Windows support. Git and native app
capabilities must be detected from available evidence, rather than inferred from
an OS, installation path, app version string, or the developer's machine.

## Current gaps and reusable pieces

- [Project recovery](../../internal/sourcefacts/recovery.go) already matches a
  recorded repository identity against the complete configured inventory and
  supports reviewed exact cwd mappings. Unknown inventory entries prevent a
  proof of uniqueness; exclusions and duplicate clones remain significant.
- [Git identity checks](../../internal/gitremote/project_identity.go) currently
  depend on Git variables some installed versions do not support. Configuration
  enumeration shares the 4 KiB limit used for short Git answers. Either failure
  becomes unknown inventory evidence, then a misleading deleted-worktree skip.
- [Native headers](../../internal/agents/codex/native_headers.go) still mark
  child, fork and revised histories pending. [Planning](../../internal/backfill/plan.go)
  excludes them before filtering, and `collector.FilterSource` supplies no
  rollout lookup.
- The [history source provider](../../internal/agents/codex/related_source.go)
  and [retained history contract](../specs/codex-history.md) already validate
  identity, physical history edges, ownership boundaries, ordinals, prefixes
  and resource limits. Reuse these contracts rather than adding another reader.
- History source schema 3 and metadata schema 2 have readers, but
  [mutation fences](../../internal/archive/history.go) still block publication,
  refiltering, recovery and deletion pending lifecycle support. These fences
  cannot simply be removed to make backfill appear successful.

## Required behavior

1. Enumerate native sources once per pass across supported default, explicit and
   previously recorded agent homes, including active and archived stores. Do not
   recursively search the user's home or depend on a terminal's environment.
2. Distinguish physical files, logical threads, dependencies and duplicate copies.
   Report these counts separately; an excluded file is not necessarily an
   additional conversation.
3. Preserve original creation time, import admission time, destination, logical
   identity, parent/fork relationships and original cwd evidence. Observation,
   modification time and import time must not become native creation time.
4. Honor existing explicit exclusions, removal records, archive identity and
   destination boundaries. Keep ordinary backfill's documented behavior of
   adding projects and capturing subsequent sessions. Exact historical mappings,
   historical-only destinations and fragment admission confer no additional
   future capture authority.
5. Select revised history using validated native current evidence or a complete,
   connected lineage with a unique tip. Never select by size, mtime or filename.
6. Retain self-contained, privacy-filtered source evidence before publishing
   metadata that references it. Never retain raw private transcripts as an
   alternative to the filter.
7. Explain unavailable evidence precisely and offer an applicable recovery
   action. Unknown evidence remains unknown; a compatibility failure must not be
   described as a missing checkout.
8. Dry-run writes no registrations, durable staged transcript evidence or remote
   artifacts. Confirmed import, interruption, resume and undo keep their existing
   guarantees. Once import admission is durable, its selected filtered evidence
   survives deletion of every native dependency before upload.

## Contract gates

Resolve these in code-facing design notes and synthetic contract tests before
the associated writer change. Do not let implementers independently choose
incompatible answers in different packages.

- **Repository proof (step 2):** define the legacy semantic observation epoch,
  comparison fields, canonical paths, policy generation, retry state and cost
  bounds. Two agreeing bounded sweeps around planning/admission detect observed
  changes but do not establish an atomic filesystem snapshot. State this limit,
  preserve current source observation checks, and do not represent semantic
  validation as equivalent to complete dependency stamps. Persistent churn or
  an unknown inventory entry stays pending. Persist the admitted ownership proof
  separately from temporary validation caches.
- **Catalog snapshot (step 3):** define native-home boundaries, physical copy
  comparison, membership revision, completion scope and concurrent-change
  behavior. A complete walk with unparsed/unreadable relevant headers is not a
  complete lineage. Completion tokens apply only to their enumerated source
  roots and observation epoch. Never reuse them after restart without renewal.
- **Ancestor authorization (steps 3 and 5):** a readable dependency is not
  automatically publishable. Validate identity and known project exclusions
  before retaining content. Explicitly excluded ancestor content must not enter
  the child's retained source, rendered conversation or export. If the current
  reader cannot reconstruct a permitted child while enforcing this boundary,
  leave it pending or retain only independently validated own fragments in step
  6b. Test content, not just counts and derived usage. Metadata-only inspection
  does not authorize a full excluded-source read.
- **Publication (step 4a):** specify the durable staging/publication state
  machine, canonical source-set digest, conditional remote write/reconciliation
  rules, reference bounds and listing repair. Existing per-session writer locks
  do not serialize two machines writing the same remote key; either prove
  single-writer ownership or use supported storage preconditions and reject
  conflicting ownership. A retry cannot restore an older pending metadata body
  over a newer published revision.
- **Retained revisions (step 4):** use the current bound of 64 preserved revision
  references as a first-release limit, separate from the 64 physical graph-span
  bound. Preserve meaningful discarded revisions, not every
  append snapshot. Define meaning and same-revision replacement precisely. At
  the bound, preserve the last committed archive and stop further revision
  mutation with an actionable limit reason; do not silently evict history.
  Removing this bound or compacting references requires a separate contract.
- **Import identity/undo (step 5):** define transitions from an ordinary archived
  thread to validated related history and from partial to complete evidence.
  Preserve archive ID, original admission, origin, destination and the original
  import's ownership. A new batch records an enrichment separately; undo must
  restore the prior selection or remove only that batch's new contribution,
  never delete a pre-existing session. If bounded predecessor state cannot
  support that guarantee, refuse enrichment until its journal can.
- **Historical ownership (step 6a):** define a stable opaque project identity,
  display label, original private cwd evidence, destination/batch ownership and
  policy for disabling the import. Do not use a synthetic path or overloaded
  live-directory inclusion to manufacture acceptance. Repository remotes are
  match hints, never globally unique conversation identity.
- **Fragments (step 6b):** define supported fragment schemas, reader capability,
  filter initialization without ancestors, identity and ownership confidence,
  relation to complete sessions, allowed queries and repair/undo transitions.
  Unknown ownership and unavailable native creation are representable without
  inventing them or weakening ordinary registration validation. This is a new
  artifact contract, not a special flag around the complete-history writer.

## Delivery sequence

Each row is one implementation area; the substeps identify required separate
reviewable changes. Steps 2 and 3 can be developed independently after step 1.
Step 5 must not enable history writes before step 4c is complete. Ship milestone
A (steps 1–2), then B (steps 3–5), then C (steps 6a–6b); run the relevant step 7
acceptance for each milestone. All three remain part of the requested outcome.

| Step | Change | Dependencies | Completion gate |
| --- | --- | --- | --- |
| 1 | Typed project diagnostics and accounting foundations | None | Project failures are specific; accounting supports later logical-history classification. |
| 2 | Portable repository recovery | 1 | Older and current Git recover eligible deleted checkouts without relaxing ownership rules. |
| 3 | Shared Codex rollout lookup | 1 | Current selection and complete-lineage fallback work across supported native stores. |
| 4a | Durable import staging and source-set publication journal | 3, publication contract | Crash recovery preserves both staged import evidence and previously published history; writes remain fenced. |
| 4b | History maintenance, deletion and compatibility | 4a | Privacy changes, retention, undo, purge, and actual older writers have tested behavior; writes remain fenced. |
| 4c | Writer enablement | 4a, 4b | Every mutation path is protected and end-to-end lifecycle acceptance passes. |
| 5 | Backfill history integration | 2, 3, 4c, import identity/undo contract | Complete child, fork and revised histories import through the full pipeline. |
| 6a | Historical-only ownership and bulk mappings | 2, 4c, 5, ownership contract | Valid complete history can import without a live repository or available Git. |
| 6b | Fragment readers, lifecycle and import | 4c, 5, 6a, fragment contract | Supported surviving evidence archives honestly, without claiming complete history. |
| 7 | Portability acceptance and documentation | All | Platform matrix, lifecycle acceptance and user docs agree with shipped behavior. |

### Step 1: expose evidence and reconcile the inventory

Work in `internal/backfill`, `internal/sourcefacts`, `internal/agentapi` and the
CLI renderers. Preserve detailed recovery outcomes instead of collapsing all
failures into an unexplained `worktree_unresolved`. Keep existing stable primary
skip codes and precedence; add a typed detail and retry/action category, and
render a specific explanation. Preserve the pending-history classification and
attach its actual dependency/selection failure when inspected.

Add bounded text summaries and structured dry-run diagnostics for incompatible
Git capabilities, unavailable configured roots, ambiguous repositories, missing
recorded identity, exclusions, missing bases, conflicting physical IDs,
incomplete inventories, unknown formats, changed sources and resource limits.
Local diagnostic details may show source paths needed for recovery; default
summaries, telemetry and published metadata must not expose native locators,
remote credentials or transcript content.

Define the accounting model now; enrich history details in steps 3 and 5, where
selection/dependencies can actually be inspected. A physical file may contribute
to multiple logical histories, so physical roles are not mutually exclusive.
Give each file exactly one inventory disposition and a separate set of bounded
roles/references; report unique files, logical sessions and retained revisions
without adding overlapping role counts together. Dependency files and identical
duplicate copies are accounted for without being reported as lost conversations.
Keep ordinary CLI and JSON compatibility; use optional fields where compatible
and bump the relevant schema when necessary.

Tests: synthetic fixtures for every classification, mixed failure precedence,
text/JSON output, exact accounting and bounded diagnostics.

### Step 2: repair Git recovery across installations

Work in `internal/gitremote` and the shared recovery resolver. Detect required
Git command capabilities once per pass and scope caches to the executable and
relevant environment. Do not require an upgrade just because optional newer
Git introspection is unavailable.

Give configuration-origin enumeration a separate bounded output budget; keep
short-answer limits for URLs and object names. Enumerate names/origins without
logging values. Bound subprocess time, output, path count, traversal and total
work, and surface budget exhaustion separately from missing evidence.

Modern Git can keep dependency-stamp validation. Where older Git cannot expose
all candidate configuration paths, use a bounded semantic revalidation mode:
re-query checkout root and repository identity for the full relevant inventory
before admission/publication, and compare the result to the planned proof. Do
not invent compiled-in system config paths or claim absent configuration is
covered by stamps. Store the proof's validation method, invalidate it on a
change, and perform Git work outside admission locks. Review this fallback
against the repository-proof contract gate before enabling it. Coalesce full
inventory revalidation once per bounded import admission slice, not per session;
carry the validated epoch into the short lock hold, where configuration and
destination are checked again. Budget exhaustion must preserve a fair retry
cursor and make progress across many distinct cwds, rather than always retrying
the first expensive root. Freeze an admitted import's attribution: remote URL
changes, new clones or deletion of the mapped repository afterward must not
prevent publishing its durable staged evidence, refiltering or reading it.
Current project/source exclusions, pause and destination policy still apply.

Keep complete-inventory uniqueness, clone ambiguity and nested exclusions.
An unreadable root cannot be ignored merely because its basename differs. A
genuinely missing Git executable or unsupported essential command leaves
automatic attribution unavailable with an actionable diagnostic; explicit
historical ownership in step 6a provides an independent route.

First-time backfill must not depend on already having imported the live projects
in a previous run. Resolve verified live candidates first, then let the import
plan propose eligible live destinations for unresolved historical sessions.
Include configured roots and reviewed proposed destinations in that plan's
bounded recovery inventory, preserving all configured exclusions and clone
ambiguities. Confirming the plan may commit those proposed projects through
ordinary backfill's existing configuration transaction before admitting their
imports. Automatic discovery continues to use committed configuration only.
Uniqueness is within this explicitly defined inventory, never a claim that no
other clone exists anywhere on disk. Selection filters must not hide configured
roots to manufacture a unique match. If no eligible destination can be proposed,
the historical-only route remains available in step 6a.

Tests: unsupported optional commands, real Git 2.39.2 regression, older Linux
Git, current Git, large configurations, no origin, nonportable origins, missing
Git, linked worktrees, symlink aliases, global/XDG/system configs, includes and
conditional includes, absent files that later appear, changes during validation,
unreadable roots, duplicate clones, exclusions, cancellation and budget retries.
Include empty initial configuration, live and deleted sessions in the same run,
and proposed-project confirmation rollback.
Use generated fixtures and disposable homes; no maintainer paths or private
configuration may be embedded in tests.

### Step 3: supply one shared rollout lookup

Implement the existing `agentapi.CodexRolloutLookup` behind a lower-level source
catalog shared by backfill and collector orchestration. Do not make collector
depend on backfill. Index stable thread IDs and physical rollout IDs from
bounded validated headers, retaining all possible copies and conflicts.

Use capability-probed, read-only native current-rollout evidence where available.
Native database locators are hints, not trusted identity. Handle missing, locked,
changing and unfamiliar databases through a separately tested read-only Codex
catalog port. Do not assume Cursor's database-copy mechanism handles Codex's
database schema or WAL behavior. The initial implementation uses bounded
read-only transactions; if a consistent read cannot be obtained, fall back to
complete file-lineage evidence or report unavailable current evidence. Any
snapshot-copy fallback is a separate change with sidecar consistency, ownership,
cleanup and cancellation tests. Database absence alone must not prevent
reconstruction from a complete native file inventory.

Enumerate active and archived stores consistently, including nested archived
layouts where supported. Completion means the required stores were fully
enumerated without permission failures or exhausted budgets, not that a
prunable observation cache happens to contain candidates. The lookup revision
must bind membership and current selection and be revalidated by `Check`.

Keep the logical identity key agent-qualified stable thread ID, compatible with
the existing session index. Home/root is source authority, not another session
namespace: a duplicate UUID across homes is not silently assigned a second
archive ID. Agreeing identity may supply additional physical evidence; conflicts
in creation, relationship, producer or ownership stay unresolved. Case and
symlink canonicalization follow shared filesystem contracts.

Constrain every opened dependency to its individually approved native source
root. Cross-home dependencies require an explicit allowlist of rooted openers
and per-file checks; expanding one root to a common parent is forbidden. Source
authorization and project/content authorization are separate checks. A catalog
entry pointing outside all approved roots remains unavailable, even if it exists.

Identical copies may be coalesced only after verified identity and captured
prefix agreement; conflicting copies remain ambiguous. Carry the same lookup
contract into filtering and collection; renew it after interruption rather than
trusting a stale persisted inventory. Use bounded shared caches and deterministic
continuation without rescanning every source home for every thread.

Tests: multiple/custom homes, paths with spaces/non-ASCII, active and archived
copies, archived-only histories, database absence/lock/schema changes, stale
current hints, complete and incomplete lineage, disconnected tips, duplicate
physical IDs, prefix rewrites, appends and cancellation. Assert enumeration and
native read costs on many children sharing ancestors.

### Step 4: complete history writing and lifecycle protection

Work in `internal/collector`, `internal/state`, `internal/archive`,
`internal/retention`, reader/export paths and CLI recovery. Reconcile the current
reader contract with publication design before changing any mutation fence.

Implement this area as 4a (staging and publication journal), 4b (maintenance and
compatibility), then 4c (writer enablement), with no newly enabled writers in 4a
or 4b. All import entry points, including setup's historical import and background
backfill, must use the same staged-evidence contract.

After confirmation, materialize the selected privacy-filtered source bundle
outside the admission lock and persist it in private local staging with its
identity, selection digest, policy context and destination. Admission atomically
links a validated staging record to the session and import journal. Staging
failure leaves the session unadmitted. Source changes before staging is complete
require retry/reconfirmation if the selected revision or ownership changes;
ordinary appends may fix a validated prefix as the reader contract permits.
Dry-run must not create staging artifacts. Cancellation before admission cleans
or journals cleanup for unreferenced staging; cancellation after admission keeps
the evidence and resumes without native sources. Give disk usage a bounded
quota and a visible capacity error, and protect referenced staging from cleanup.

Do not require all batch sources to fit simultaneously in memory or local
staging. Stream bounded groups through durable admission and upload; when space
is exhausted preserve the completed group and a resumable cursor. Report
admitted/staged and remotely verified counts separately. Free staged bytes only
after their remote sources are verified and the local publication state is
durable, or after a correctly journaled cancellation/removal.

Extend the existing pending-publication journal for source sets and revisions:
write checksum-addressed sources, verify them, then publish metadata selecting
the current revision and preserving required prior references. Commit local
published state only after the remote result is verified. Interrupted writes
must recover idempotently; uncertain writes must not discard the prior readable
revision. Preserve identity, creation, producer and relationship facts in the
same-handle admission check, not merely native ID and cwd.

Define bounded retained revision/reference behavior without silently losing
historical evidence. Refilter all retained source evidence that remains
referenced when privacy rules change. Metadata refresh, generation recovery,
retention, whole-session deletion, undo and purge must understand the full source
set. Protect readers and older mutating binaries with the repository's writer
compatibility mechanism before permitting history writes. Release readers before
writers where required; update schemas and version documentation together.

Privacy replacement must rewrite both current and preserved revisions and then
retire every old-filter object according to the existing privacy-sensitive
cleanup contract. Keeping readable prior history must not keep obsolete private
data reachable through a preserved reference, listing entry or pending journal.
Purge treats the validated union of all active/preserved references as live and
must not trust a corrupted or incomplete metadata body as permission to delete.

Inventory actual mutating entry points and prove downgrade refusal for local
state plus shared remote archives. Configuration fencing on one machine does
not by itself protect history against an older binary using a different data
directory on another paired machine. Use actual prior release binaries where
available, not just mocks of their expected decoder. If the required remote
compatibility protection cannot be enforced with the existing storage protocol,
keep writes disabled until a compatible protocol or an explicit coordinated
upgrade boundary is implemented. Do not promise a transparent downgrade.

Tests: crash at each source/journal/metadata/local-state boundary, retry,
concurrent change, rollback/revert, unchanged scans, parser refresh, privacy
refilter, old writer refusal, retention, undo, purge and destination changes.
After successful upload, remove all disposable native sources and verify list,
show, handoff, export and metadata reconstruction from retained filtered evidence.
Also delete native files after admission but before first upload and restart;
the staged import must still complete without manufacturing missing records.

### Step 5: import complete histories end to end

Work in backfill discovery, planning, registration and CLI orchestration.
Replace the blanket history skip only for candidates validated by the shared
lookup and reader. Plan logical threads rather than treating physical revision
files as independent sessions. An ordinary-looking original rollout must also
consult current selection when later revisions exist.

Carry validated identity, original creation, relationship, ownership and source
selection facts into registration. Resolve parent/fork archive links without
requiring enumeration order or fabricating a parent that cannot be imported.
Dependency availability does not itself authorize a separate parent import;
preserve exclusions and removal records. Inherited activity must not become the
child's own prompts, titles, usage or timestamps. Do not count physical revision
files as duplicate logical sessions or count shared history repeatedly as usage.

Run the complete inventory before applying session date/project import filters:
a needed base may fall outside the selected date range, and a clone or exclusion
outside the output filter can still invalidate automatic attribution. Then apply
filters to the selected logical thread's own creation and project proof, not to
each physical file independently. Retaining permitted inherited context and
counting own activity are separate operations; enforce the ancestor-authorization
contract on native retained records as well as rendered/derived output.

Do not treat `already_archived` as proof that historical reconstruction is done.
Inspect compatible ordinary registrations for a needed history transition and
journal that enrichment with its own batch contribution. A relation link added
to an existing parent is likewise an enrichment, not permission to claim that
parent's original import or delete it on undo. Keep unresolved parent IDs as
non-authorizing relationship evidence without reserving a fictitious session.

Initially the native reader can reconstruct only from available validated raw
dependencies; a filtered archived source is not interchangeable with raw JSONL.
Add a separate retained-dependency contract for missing native ancestors using
validated history manifests and exact retained ordinals, plus digest/identity
bindings sufficient to establish the requested prefix. Ordinals alone cannot
prove omitted raw bytes. If the bindings are insufficient, do not synthesize a
complete history from flattened metadata or an older schema-2 source. Preserve
the prior archive, explain the missing proof, and use the fragment route when
appropriate. Test source schema 2, schema 3, privacy-dropped records and foreign
archive references independently.

Revalidate source and project proofs between plan, registration and upload.
Make importer and collector use the same history lookup and publication path;
a successful plan must not queue work that is still refused by a lifecycle fence.
Keep import batch attribution, retention admission time, tombstones and resumable
batch matching. Existing compatible sessions keep their archive identity and
ownership; new history must not create a second archive session on rerun.

Tests: ordinary compatibility, independent children, forks, forks after reverts,
multi-file revisions, shared ancestors, parent excluded/missing/already archived,
source disappearing between steps, changed current selection, interruption,
resume, rerun, continued capture and exact undo ownership.

### Step 6a: historical-only ownership and bulk mappings

Implemented in [#433](https://github.com/wangjohn/agent-archive/pull/433)
(`18ca0c48`, 2026-10-12). The implementation was reconciled against main
`b7435197` (including #430 streaming read accounting, per-session ceilings
and named size gaps) and adds
strict bounded mapping JSON, separate opaque historical identities and
destination permissions, frozen private dependency cwd evidence, retained-only
publication/local reads and permanent v10 compatibility protection. Existing
staging/history foundations are reused. Original-vs-selected-revision admission
and remote history-presence verification receive narrow prerequisite repairs.
Undo retains historical identities and import permissions as explicit settings;
it removes only batch-owned sessions/evidence. The operational contract is in
the [backfill spec](../specs/backfill.md#historical-only-ownership-and-bounded-mapping-files).
Fragment work in 6b and platform/provider release acceptance remain open.

Exact `--map-project` remains available for existing configured destinations.
`--mapping-file FILE` adds bounded, reviewable assignments for many original
cwds, with conflict checks and a canonical digest included in batch resume
identity. The CLI help, reference and backfill guide document these commands.

An explicit historical project destination supports sessions whose original
repository is gone or cannot be automatically attributed. It stores an
import-only ownership proof and display identity independent of a live path.
The import preview shows the assignments before confirmation. It grants no
watcher, hook, discovery or future-project inclusion; an explicit source or
project exclusion still wins. Registration acceptance and retention support
this proof independently of an existing configured root.

Paired machines reconcile this project identity and its destination-scoped
import permission, preserving local labels and enabled/disabled decisions.
A display-label change cannot change ownership; moving to another destination
requires the existing destination
policy rather than silently republishing there. Home/temp/above-home source
restrictions remain applicable; historical attribution cannot bypass them.

Synthetic tests cover complete history with no live repository/no Git, first-time
configuration, bulk mapping conflicts, exclusions, pairing, label changes,
policy disable/re-enable, destination changes, interruption, retention and
exact batch undo.

### Step 6b: fragment readers, lifecycle and import

The [fragment foundation](../specs/codex-fragments.md) reconciles this step against
`e1436e35` (2026-10-12). Its independent version-1 schema and executable line
checks are proposed only: there is no fragment decoder, filter route, publisher,
registration or import option. Initial activity ownership is always unknown;
historical project attribution remains a separate authorization decision. Schema
validation and capture provenance never certify privacy. Complete source and
metadata schemas and their validators remain unchanged.

Deliver the decoder and context-proven filter with the first real explicit
incomplete consumer, then staging/publication and every maintenance/compatibility
path, then reviewed opt-in import with a separate logical fragment registry.
Coordinate future writer capabilities with existing historical-projects-v10;
the foundation allocates no new fence. Inspection of current batch/undo code found
no generic enrichment/contribution journal. That step-5 prerequisite must land
before complete repair or exact undo can be enabled; ordinary whole-session batch
undo and the publication journal do not provide that contract.

For incomplete but identifiable history, design a separately versioned retained
fragment representation. Do not weaken the complete-history validator or turn
fragments into ordinary complete sessions. Each fragment retains only approved,
privacy-filtered records, validated available identity, physical ordinals where
known, completeness status and missing dependency reasons. Unknown ownership
must not contribute to aggregate usage, creation-time claims or complete
handoff/replay. An unknown format is not safe merely because it parses as JSON;
retain only supported records, otherwise leave it unresolved.

Import fragments only through an explicit option included in the reviewed plan.
Readers label them incomplete, and every maintenance path understands them.
Later complete evidence can supersede fragments while preserving archive
identity, batch provenance, removal semantics and verified prior references.
Do not invent missing history, relations or ownership boundaries.

Land the proposed format/schema foundation first, then readers with an explicit
incomplete consumer and proven filter context, then staging/publication and
maintenance, then the explicit import option. Make failure to initialize the
privacy filter without missing ancestor context an unresolved outcome, not a
fallback to generic JSON/text retention. Repair to a complete history and undo
of that repair must use the prerequisite enrichment journal specified in step 5.
That journal is not implemented in the reconciled current batch/undo code.
Fragment archival must not reserve the ordinary thread identity in a way that prevents its later
complete import, or create two logical sessions counted as independent usage.

Tests: no live repository/no Git, mappings over one batch's interactive limit,
mapping conflicts, excluded sources, missing base, ambiguous tips, unknown
ownership, fragment filtering, reader labels, usage exclusion, repair to complete
history, interruption, retention, undo and old-reader/writer compatibility.

## Portability and acceptance matrix

| Dimension | Required coverage |
| --- | --- |
| Operating systems | Native macOS and Linux tests; macOS privacy/access failures and Linux XDG/headless locations. |
| Architectures | amd64 and arm64 release builds; runtime acceptance recorded separately from cross-compilation. |
| Git | Older distribution Git, 2.39.2 regression, current Git, optional capability absent, Git absent, slow/erroring commands. Record exact tested versions. |
| Storage/writers | Disposable S3-compatible acceptance plus required provider coverage; actual older release writers, paired machines, lost local state, conflicting pending publications and listing repair. |
| Agent layouts | Desktop and CLI producer fixtures, default/custom/recorded homes, multiple homes, active/archived/nested archives, DB available/unavailable/newer schema. |
| Projects | Live/deleted main checkout, deleted linked worktree, no remote, custom remote, clone ambiguity, exclusions, non-Git and historical-only destination. |
| History | Ordinary, child, fork, revision/revert, copied prefix, missing base, duplicate/conflicting files, cycles, huge graph and changed sources. |
| Lifecycle | Plan/import/upload/read/source removal/continued capture/refilter/retention/undo/purge; interruption at each durable boundary. |
| Scale | Thousands of files and distinct deleted cwds, shared ancestor fanout, large Git configs, bounded staging disk/memory/descriptors/operations, fair retry and cancellation. |

Keep fixtures synthetic and isolated from real credentials, native apps and
storage. Add real Git compatibility jobs or disposable acceptance images; fake
runners alone cannot establish support. Capability tests must remain effective
when CI upgrades Git. Exercise publication using the repository's disposable
storage acceptance harness. Run the required checks from the
[testing guide](../contributing/testing.md) for each affected change, plus parser
and privacy fuzz targets when those contracts change.

Pin executable compatibility cases in CI, including Git 2.39.2 and an older
distribution build, instead of relying on whichever Git the runner currently
ships. Exercise current Git as a separate job. Publish command/format capability
results and exact tested versions; do not infer support for an untested producer
merely from a successful version-string comparison. Add operation-count gates
for Git queries per admission slice and source-home enumeration per epoch,
unchanged collector scans, shared dependency reads and staging cleanup. Existing
ordinary capture must not acquire history-sized reads or per-session Git sweeps.

Update the backfill specification, guide, CLI reference, session eligibility,
identity and history contracts, schemas, version record and changelog as their
respective steps ship. Keep unshipped options and behavior confined to proposal
documentation. Record actual platform/app/provider acceptance without treating
synthetic producer fixtures as proof that every native app version works.

## Completion criteria

- Supported machines can recover attributable deleted-worktree history without
  requiring a newer Git solely for optional introspection.
- Complete child, fork and revised histories import, publish and remain readable
  without the original native files; reruns and continuation preserve identity.
- Confirmed admitted imports survive source deletion before first upload. Source
  loss during dry-run/planning is reported and never presented as durable import.
- Existing archived threads can gain valid retained history without a second
  archive identity; undo of enrichment leaves their prior archive intact.
- Surviving supported fragments and sessions from vanished repositories have an
  explicit historical archival route, with truthful completeness and ownership.
- Every enumerated entry is reconciled to an imported logical session, retained
  fragment, dependency, identical duplicate, exclusion or specific unresolved
  reason. Permission failures and unenumerated stores are reported separately.
- Existing ordinary sessions and Claude/Cursor imports retain their behavior.
- Privacy maintenance, retention, undo and writer compatibility work for every
  newly writable artifact type. No blanket fence is removed ahead of its tests.
- The supported-platform matrix and user documentation describe what was
  actually verified. No private transcript, local path or machine-specific
  workaround is required for the implementation to pass.
