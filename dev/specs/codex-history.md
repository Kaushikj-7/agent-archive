# Codex retained history reader

Status: implemented retained-history reader and lifecycle contract (#336, #341,
#348, #355), reconciled 2026-10-11 at `db2ccd3e`. Native lookup, ownership,
child admission and maintenance are covered by code and synthetic tests. The
broader [historical recovery plan](../proposals/historical-backfill-recovery.md)
now includes historical-only ownership and bounded bulk mappings merged in
[#433](https://github.com/wangjohn/agent-archive/pull/433) (`18ca0c48`,
2026-10-12). The [fragment foundation](codex-fragments.md) is a proposed schema
and contract only; no fragment reader or import route is shipped. Fragment
artifacts (step 6b) and remaining combined acceptance work stay open; the
[backfill contract](backfill.md#historical-only-ownership-and-bounded-mapping-files)
defines the implemented historical identity and destination policy.

A stable Codex thread is distinct from its physical rollout revisions, root
conversation, parent and fork relation. The source provider consumes injected
`CodexRolloutLookup` catalog evidence; it never enumerates a home directory per
thread. An ordinary-looking original rollout also consults the lookup when
available, because a later physical revision may be current.

A current locator is an untrusted hint. The opened header must match the stable
thread, the file must remain inside its admitted source root, and the lookup
revision must remain current. Without a current locator (no row in Codex's
thread index, or an older Codex), the tip is the one rollout among the seed and
the thread's observed rollouts that no other continues from; several such
rollouts are a named, retryable failure (`rollout_tip_ambiguous`). An observed
rollout counts as continued from when the chain walked back by rollout ID from
another apparent tip reaches it, so an unobserved middle segment does not make
the thread's root look like a second tip. Rollouts off
the chain walked back from the tip take no part in selection. Modification time, file size and
lexical order never choose a current revision.

Physical `history_base` edges form an ordered graph. Each included prefix ends
on a newline and agrees with the raw exclusive ordinal boundary. Cycles,
missing or duplicate physical IDs, split records, ordinal gaps/regressions,
unknown history modes and contradictory bounds fail closed. Blank lines do not
consume ordinals; safely omitted unknown native records do. A valid JSON leaf
record without its final newline remains pending for the next capture.

Logical ownership is separate from the physical graph. An explicit child or
fork boundary excludes copied prefix activity. An absent child boundary excludes
nothing, matching the producer's optional-boundary behavior. A fork boundary
survives later same-thread reverts; same-thread history before a revert otherwise
remains own activity. The active header supplies identity and native start.
Inherited model context can inform own messages, but inherited prompts, tools,
usage, titles and timestamps do not become own activity. Cumulative-only token
counters with uncertain scope produce an explicit gap, without guessing whether
a child reset its counters. Independent own per-call usage remains useful.

## Shared native catalog

### Foreground backfill work target

**Approved target, 2026-10-17; not yet implemented.** The
[large-backfill completion contract](backfill.md#large-backfill-completion-contract)
requires an explicit foreground lookup policy, separate from background pass
scheduling. The current five-second allowance and 256 header-probe window must
not be a whole-import deadline or an unrecoverable source classification.
Foreground preparation, inspection and admission need renewed bounded work
scopes and continuations; a supported individual history needing over five
seconds of cumulative lookup work must also be able to complete. Merely giving
each whole source another five seconds is insufficient if it restarts forever.

Preserve the charged shared ledger, per-operation cancellation, current-row
revalidation, chain-local ownership, source confinement and semantic per-history
limits. A scope boundary must not retain a stale selection token as authority,
lose active native leases, reset outstanding charges, or substitute an incomplete
inventory for a uniqueness proof. Facts may be reused only under their actual
observation/selection checks. Cache eviction or a scheduler allowance says
nothing about whether a source is complete or supported.

Expose allowance exhaustion distinctly from history-depth, raw-byte, record,
actual capacity, missing-dependency and own-task refusals. Foreground consumers
continue pending work fairly and detect no progress; background collection keeps
its independent responsiveness policy. The chain below remains the native
history proof: this target does not reintroduce a global absence/coverage epoch
before capture. Project witness completeness remains the separate backfill
attribution obligation.

Delivery, compatibility review and actual qualification are owned by the
[completion plan](../proposals/large-backfill-completion.md). No native schema,
format fence or per-source limit is changed by this specification edit.

### Current lookup and lineage

`discovery.CodexRolloutLookup` owns the pass's native current projection,
observed rollout facts, rollout directory listings and optional full metadata
epoch. Ordinary collection and generation preview use the lookup directly.
`MetadataInventory` explicitly requests a lazy view with separate bounds: 16
homes, 16384 physical entries, 2048 directories and 16MiB aggregate metadata
facts/header work, charged to the shared native read budget. That view serves
only a content-free diagnostic for a related-history refusal (see below); no
capture or backfill waits for it. Neither observation cache membership nor any
listing proves complete inventory.
Physical candidates count before header validity; filesystem fingerprints include
subdirectories and unrelated entries and retain a separate18432-entry traversal
ceiling. Aliases of one canonical home share acquisition while every approved
home spelling remains fenced.

A thread's lineage is a chain, and the chain is its own proof. The tip is the
rollout Codex's thread index names (`threads.rollout_path` in
`state_5.sqlite`). Each rollout's header names the rollout it continues from
(`history_base`); the source walks tip, base, base's base, until a rollout with
no base. The lookup finds a base by its rollout ID: first at a locator observed
this pass that still exists, else by file name
(`rollout-<local time>-<rollout ID>.jsonl`) in the day directories
`sessions/YYYY/MM/DD` within one day of the creation time its UUIDv7 carries,
and in `archived_sessions/`. Each directory is listed at most once per pass
(at most 65536 entries, 4MiB of listings per pass, charged to the shared
ledger); a directory whose listing no longer fits is searched for the one
rollout ID without keeping its listing. The provider reopens each locator and
checks its header identity. A
base found nowhere, or at two locators, is a named, retryable failure
(`history_base_unavailable`), never a wait.

No proof that the native home holds no other rollout of the thread is needed,
and none is attempted. Earlier versions required one: a global coverage epoch
that observed every directory under `sessions/` and `archived_sessions/` and
then validated that none had changed. While Codex ran it kept creating rollout
files, so epochs never validated, and once 256 threads had requested coverage
no new thread could enter; every Codex capture deferred indefinitely. A catalog
those versions wrote still carries a `native_coverage` field; a read ignores it
and the next checkpoint drops it.

What the chain cannot see is a segment created after Codex's index was read.
The next pass reads the index again, finds the new tip, its lookup revision
differs, the source looks changed and is captured again.

The lookup also reports the thread's observed same-thread rollouts as
candidates, and its revision covers them and their identity evidence, so a newly
observed segment changes the source's signature. Without a current locator the
source chooses the tip from them. Beside a current locator, candidates off the
chain (an abandoned branch after a rollback, say) never block capture or choose
the tip; ones whose files still exist are offered after the chain as
best-effort historical revisions, up to the 64-candidate limit with the
chain's own segments first; off-chain ones past it are left out rather than
refusing the capture. The source also names the chain's tip, the rollout its
read took as the current revision. Reconciliation reads each segment before the
tip as a historical revision but not the tip itself: the scan's own read already
holds it (with the whole chain it inherits), so reading it again would only
double what a long thread needs in read memory. The read's encoded records are
released once the current candidate is built from them, and the pending
publication's fences read only the proofs and catalog commit it carries, not
the whole file. A pending publication is written one record at a time,
and preparing a publication a scan froze from its own candidate takes its
current source from that candidate rather than decoding the staged copy. The
source marks the off-chain candidates best effort: a
failure reading one
(its own base gone, an unreadable header, the reconciliation's byte budget
spent, or the session's own read ceiling, which the session meets again on
every pass) or staging one (the revision planner's stage count or bytes, an
import's stage budget, or the session's read ceiling, none of which a refusal
changes) only leaves that branch out and releases what it held, while a
failure reading a segment of the current chain still fails the
reconciliation. A best-effort failure that can clear on its own (the pass's
shared read budget held by others, a rollout changed while reading,
cancellation)
still fails it too, so the session defers and a later pass reads the branch:
leaving it out would record a signature that an abandoned branch never
changes again. A branch kept while reconciling holds what it retained for
the rest of the scan; if guarding, settling or publishing then meets the
session's read ceiling before any publication is saved, the scan runs once
more from the start with every off-chain branch left out, so the branches
never make the chain's own capture too large to read; a branch it does not
read again but an earlier publication preserves by reference, unchanged
since, is still published, so it is not left out (one that changed since is
published only as it was, even when it is staged again from the last
publication that carried it as current, so it is). A branch left out is
recorded as the local capture diagnostic `revision_left_out` (the app, the
included project and the time only) once the scan ends without the session
recorded as a capture gap; an import leaves branches out without one.
Without an admitted source home
they are not read at all. Beside a current locator, a thread with more than 64
observed same-thread rollouts is still captured: the lookup offers the 64
newest that still exist, the chain's own segments come first and the newest
off-chain rollouts fill the remaining slots. A current chain itself longer than
64 segments is still refused with the rollout history depth limit. Without a
current locator such a thread is refused with a limit failure, since its tip
is chosen from all of its observed rollouts and a subset could choose the
wrong one. Only rollouts the lookup has observed (a
discovery listing or the restored cache) are offered: no proof says every
abandoned branch was found, and a branch captured while it was current stays
preserved through the published revision history either way. A discovery
checkpoint that misses its pass deadline hands its catalog to the lookup, whose
close writes it without that deadline.

The full view binds lazy acquisition to the owner's constructor-approved
canonical root set and fences every supplied alias spelling. A changed or
collapsed root set leaves incomplete evidence before native acquisition.
It enumerates approved active and archived stores with the same
bounded directory adapter. Each header is acquired on one confined descriptor
using one-byte ReadAt requests through the first newline, at most64KiB including
that delimiter. A resumable step performs at most1024 reads and100ms of work,
retaining one in-memory partial header. Cancellation or changed root/source
clears the cursor. It reads no body, task event or duplicate prefix. Missing
stores have absence evidence; unreadable stores, unknown/malformed headers,
identity conflicts and exhausted bounds leave incomplete evidence. One physical
rollout UUID has one stable logical identity and immutable pagination links;
UUID spelling variants denote that same physical rollout. Different rollout
UUIDs may carry different revision-local pagination. Every
physical copy remains represented; complete enumeration does not establish
content agreement or ancestor capture permission.

Caller-owned validation slices share one filesystem sweep for up to512 lookup
and check calls and30seconds after the sweep, with a60second maximum. A sweep
rechecks captured directory membership stamps and every physical file
observation; selected providers independently check opened headers/content.
Thread and Check retain live targeted current-row queries and WAL refresh/replay
despite filesystem proof reuse. Ordinary requested work retains its five-second allowance. An explicitly
requested full epoch has one fixed thirty-second cumulative active-work
allowance for acquisition, sweeps and current checks. Caller cancellation and
earlier deadlines win; slice renewal and lifetime never replenish that allowance. Failed sweeps are retained only in their bounded slice; callers
close snapshots before renewal. CloseReadOnly releases resources without a
capture-catalog checkpoint. Filesystem observations use the practical
size/mtime/identity contract; restored timestamps are not a filesystem lock.

Only actual path-specific related-history refusal requests the same owner's
view for a content-free diagnostic under a100ms caller deadline. It preserves
the original error and says this operation remains pending. Upstream admitted
and retained history paths retain their existing behavior. No second production
catalog, new content authority or new history admission is introduced here.
The PR-only legacy prefix-coalescing catalog and settled-VFS engine are retired;
metadata completeness does not establish duplicate content agreement. Authorized
both-copy prefix checks and cross-home ancestor content remain separately owned.

## Captured-prefix consistency and resources

Each snapshot fixes a newline-complete prefix, validates its raw ordinals and
hashes its bytes. Before returning filtered output, the provider hashes that
same native prefix again and verifies both descriptor and locator identity.
Headers used for selection, graph edges, ownership and preliminary admission
are proven against their originally observed bytes, including dependencies
that contribute zero captured bytes. Those header proofs retain descriptor
leases and are checked before returning a snapshot, at admission and before
returning filtered output, for ordinary and related captures.
Appends beyond the captured prefix are allowed; replacements and prefix rewrites
are retryable changes. This permits continuously active chats to make progress.

A pass owns at most 64 open dependencies, depth 64 and one million records per
materialized graph. The shared pass charge (agentapi.PassReadBudget, 256 MiB)
includes cached headers, immutable ancestor-prefix byte caches, bounded prefix
summaries and line buffers grown only as far as the longest record read. A
rollout read through its descriptor is not charged for its length. Cached
ancestor prefixes total at most 16 MiB per pass, within the remaining budget.
A thread's rollouts may total archive.MaxCodexRawTranscriptBytes (512 MiB);
past that the source reports a raw-limit failure with the sizes and an
observation, which the collector records once as a named size-limit gap.
Descendants reuse
its bytes and validated ordinal summary. The provider still reads and hashes
the native prefix once per completed descendant capture; it does not use file
stamps as proof that cached bytes remained unchanged. Changed dependencies evict
unused entries, while live snapshots prevent unsafe replacement. Close releases
leases, cached bytes and descriptors; cancellation/error closes the snapshot.

The byte charge is not a Go heap or RSS cap. Privacy filtering, encoded retained
records and assembled native maps consume additional memory. Shared prefixes
are filtered again for each self-contained output: the privacy codec has
stateful interpretation, so no unproved filtered-result reuse is introduced.
There is one bounded immutable raw copy, rather than one raw copy per child.
`BenchmarkRelatedHistoryRecords` measures actual native reads, opens, cache hits,
filtered/output records, allocation and sampled peak heap for 1k/10k/100k records
and four children sharing a 100k-record prefix.

## Reader-before-writer compatibility

Ordinary sources retain source schema 2, metadata schema 1 and their byte/record
behavior. History sources use schema 3: a bounded manifest plus exact unsigned
ordinals in native-record envelopes. The filtered native objects never carry
unvalidated metadata, absolute native locators or copied producer instructions.
Spans are recomputed after privacy drops, and retained ordinal precision exceeds
the floating-point native map range. Source schema 3 is Codex-only.

Metadata schema 2 adds an active physical revision and bounded preserved source
references. Every source key must be checksum-addressed under the same archive
session prefix. Prior source-schema-2 references are permitted. Reader validation
precedes derivation and selection; a malformed reference cannot widen reads.

The collector's already-enabled history publication uses exact retained source
sets. Private source-set version 17 (`source-set-publication-v17`) permanently
protects outer protocol-5 publication and derives the durable-storage guard
inside the same held private root before a new journal is emitted. Its distinct
`source_set_commit5` field retains inner selecting commit version 2. Existing
ordinary ReadySelection2, staged3, Cursor4 and catalog authority remain separate;
all prior configuration protections are preserved. It preserves existing native consent and
history ownership checks; it does not enable historical import admission or
future native ancestor authority. Unchanged settled admission uses exact-byte validated summary reuse and existing
scan signatures. Summary reuse still reads the complete file; cold processes
validate and decode in full. Generation recovery validates the exact frozen successor.

Private pending state retains legacy active-source fields and adds a versioned
commit identity plus up to 65 exact source payload references: one current and
64 preserved revisions. The revision-reference limit is independent of the
64 physical graph-span limit. Inline compressed replay payload descriptors are capped at
128 MiB in aggregate; the active payload is stored once. The collector preserves
its existing 32 MiB compressed-object producer, resolver and retained-reader
ceiling, independently of the 128 MiB filtered-data ceiling. Generic ref-only
state/storage descriptors can exceed that aggregate and retain a 128 MiB
descriptor bound. Descriptor validity does not expand the collector read allowance;
over-ceiling references retain pending evidence rather than authorizing larger
reads. Owned history-stage handles resolve serially under the same shared memory and
1 GiB physical disk budget. Protocol-5 admission-stage handles remain unsupported; existing typed import
staging retains its own selecting protocol and cleanup authority. Malformed journals remain visible pending
evidence and block replacement; older quarantined copies remain visible to status
and retention. An obsolete adapter/filter/skill policy retains replay evidence
for explicit refiltering rather than uploading or discarding it.

The exact SHA-256 of metadata bytes binds retries. The semantic source-set digest
uses deterministic JSON of all known metadata fields (including schema, ownership,
selection, capture/filter/producer/relationship/generation facts), with preserved
references sorted by revision ID, plus private destination, admission, policy and
mutation purpose. Referenced revision IDs, timestamps, keys, checksums and sizes
must be valid and unique. The raw digest also binds unknown fields and encoding.
The predecessor is explicitly known absent, known present with its exact body
digest, or unknown. A remote exact-next body permits local completion after every
source verifies; an exact predecessor permits replacement. Any other body,
unreadable remote evidence or unknown predecessor stays pending, without adopting
the remote winner as replacement authority. Legacy state with neither exact
metadata nor a recorded predecessor stays pending until its evidence is restored
or reconciled. Existing checksum-addressed objects with different bytes are never
overwritten. These operations rely on unsynced, single-owner local state; a
GET/compare/PUT sequence is not a cross-machine atomic conditional write.

A physical selection change preserves the exact prior current reference and all
prior preserved references. Same-revision updates cannot accumulate append
snapshots and require a provider-approved continuation bound to both source
digests, plus consistent retained filtered ordinals, spans and record prefixes.
Filtered equality alone cannot prove raw native prefix continuity or authorize
using a filtered source as a raw dependency. Missing proof stays pending. Privacy
full-set replacement requires typed transformation receipts, exact predecessor
and next sets, and durable old-key retirement. Successful full local settlement
prunes obsolete private raw proof bodies; maintenance-owed baseline acknowledgement
retains the exact original proof and obligations. Selection-only publication
cannot evict or rewrite preserved evidence. At capacity, preserve the committed archive and stop.

Listing repair indexes the exact validated phase-C response and its same-response
validator. A later phase-D conflict can leave a harmless stale hint, with repair
still owed; that hint never authorizes adopting an unverified winner.
The verified metadata and full local source set become durable before covered
requests complete; pending removal is last. A later admission-stage port must
resolve one immutable size/checksum-bound object at a time under aggregate disk
quota (including temporary/pending copies), protect referenced handles and release
staging only after that local commit and request completion. No native reread or
Git reattribution may replace admitted evidence. Remote wire schemas and filter
versions are unchanged. Leading `publication_version: 5`, pending `journal_version: 5` and the closed
`source_set_commit5` phase codecs agree with the permanent source-set writer
floor; legacy and other selecting-protocol dispatch remain explicit. Future historical admission and native ancestor authority are separate
gates.
The collector publishes related histories through its existing durable pending
journal. Every final reference is verified before exact predecessor preflight and
metadata replacement, and every reference plus exact metadata bytes is read back
before acknowledgement. An exact committed retry performs no source or metadata
PUT. Stronger privacy resolves predecessor/final state before replacing private
work; unknown authority remains pending. Older config writers remain protected by
the permanent source-set-publication-v17 marker, preserving prior history,
storage, import, search and selection protections.

Retained maintenance carries the complete source set and original captures.
Frozen generations use retained bytes and recorded Git facts. Retention uses the
latest meaningful archive capture across the set, with the existing clock clamp;
parser, link, privacy and representation work do not manufacture activity. Cleanup
protects all live references, rechecks metadata before source deletion and removes
metadata before whole-prefix deletion. Unknown schemas and identities refuse.

Capture admission independently validates immutable native creation, producer,
relationship and project facts. A previously unknown native home can migrate only
through a configured confined root containing the admitted locator, rechecked
before binding persistence. Lookup hints never grant that permission. Read-only
source interpretation remains separate from capture authority.

Producer references: [ordinal writer](https://raw.githubusercontent.com/openai/codex/rust-v0.160.0/codex-rs/rollout/src/ordinal.rs)
and [history materialization](https://raw.githubusercontent.com/openai/codex/rust-v0.160.0/codex-rs/thread-store/src/local/thread_history_materialization.rs).
Synthetic tests model the pinned producer contracts; no private transcript is a
fixture or an acceptance claim.

## Native child admission and reconciliation

The global native candidate stream projects the stable thread, root and immediate
parent from shared Codex metadata. It never creates Claude composite subagent IDs
or enumerates a native home for each parent. Child root absence remains unknown.
A validated source snapshot supplies original same-thread creation and the first
OWN task. Its timestamp must be no earlier than original native creation, with
the same one-second tolerance as bounded header admission. The task's optional
`root_turn_id` is attribution to the originating root-thread turn, so a
descendant's own turn UUID may differ. Both IDs must be valid when
present, but equality is not ownership proof. Native own-history boundaries and
creation-time checks establish ownership independently of root attribution.
See the [producer event definition](https://github.com/openai/codex/blob/rust-v0.160.0/codex-rs/protocol/src/protocol.rs).
A rejected first task cannot be replaced by a later native-looking event. Discovery, historical
import and ongoing capture apply that constraint. Earlier private related-owner (fork or child)
scan proofs are revalidated once; parser, adapter and privacy versions remain
unchanged by this admission correction. The inherited prefix may
exceed the bounded catalog header window;
metadata-only identity/creation/format facts do not authorize capture. Seen-invalid
first tasks remain rejected until their source observation changes; missing or
partial tasks and unavailable, canceled or budget-limited reads remain retryable.
All qualified history imports establish the permanent writer protection before
registration, including forks whose public native-child flag is absent.

Each native child carries its own origin, admission, destination and import batch.
Missing parent links do not prevent independently admissible capture. Relationship
repair uses the existing registration inventory, scoped to the same native source
home, project and destination; ambiguous owners and removal tombstones remain
unresolved. Parent hooks and admission are never substituted for child consent.
Native discovery and import do not fabricate SubagentStop observations.
Relationship repair queues its durable publication obligation before marking the
link reconciled. Resolving a missing parent preserves the child's capture age and
own usage. Privacy maintenance accepts an unresolved historical parent only for
positively identified native child evidence; conflicting known parents are refused.

Earlier writers could retain native children without the optional public ownership
marker. A supported persisted child binding for the exact already admitted owner
can migrate that marker before retained maintenance. All current and preserved
headers are upgraded under their original source authority; unknown or conflicting
owners stay refused. This migration reads no current native inventory and does
not change admission, capture age or raw ownership. Absent legacy public markers
remain compatible with readers.

`native_child` in retained source and metadata records ownership independently of
`parent_session_id`. A captured child with an unresolved archive parent includes
`native_parent_link_pending`. Source-history dependencies remain distinct from
these conversational relationships. Resolving a parent for an existing revision
set uses sequential retained maintenance to update every referenced source header
before the sidecar changes; exact reader identity checks remain in force. Each
frozen input retains its original parent header independently of the prepared
output target, so a policy successor can verify both old and prepared stages.
Older journals recover that input fact from checksum-matching predecessor
references, or retain their unchanged header authority. Parent deletion does not remove a child's
self-contained retained history. Undo uses exact batch membership and orders only
selected descendants before selected ancestors.

Initial discovery shares one serial native source pass for the active authorized
home. Switching homes closes the previous pass, preserving the aggregate 128 MiB
provider data charge. Actual source read calls/bytes/opens are counted separately
from bounded metadata probes; this is not an RSS or heap cap.

Legacy Codex composite links are reconsidered with one exact local index lookup
for `parent:subagent:child` when the independently admitted native child and parent
are known. An unadmitted matching reservation receives an existing linked-evidence
provenance marker, `native:legacy-unverified-composite`, and a content-free
`native_child_link_unverified` gap. Raw historical supplemental evidence stays
retained; current derived links exclude only that positively identified ghost.
Unknown local evidence and legitimate unavailable links stay explicit. A private
`native_link_version` registration marker makes this migration idempotent across
restart, without deleting identities or reading parent source bundles.
