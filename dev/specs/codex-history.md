# Codex retained history reader

Status: implemented retained-history reader and lifecycle contract (#336, #341,
#348, #355), reconciled 2026-10-11 at `db2ccd3e`. Native lookup, ownership,
child admission and maintenance are covered by code and synthetic tests. The
broader [historical recovery plan](../proposals/historical-backfill-recovery.md)
now includes historical-only ownership and bounded bulk mappings merged in
[#433](https://github.com/wangjohn/agent-archive/pull/433) (`18ca0c48`,
2026-10-12). Fragment artifacts (step 6b) and remaining combined acceptance
work stay open; the [backfill contract](backfill.md#historical-only-ownership-and-bounded-mapping-files)
defines the implemented historical identity and destination policy.

A stable Codex thread is distinct from its physical rollout revisions, root
conversation, parent and fork relation. The source provider consumes injected
`CodexRolloutLookup` catalog evidence; it never enumerates a home directory per
thread. An ordinary-looking original rollout also consults the lookup when
available, because a later physical revision may be current.

A current locator is an untrusted hint. The opened header must match the stable
thread, the file must remain inside its admitted source root, and the lookup
revision must remain current. Without a current locator, selection requires a
complete candidate set with one connected, unique lineage tip. Missing or
ambiguous evidence remains pending. Modification time, file size and lexical
order never choose a current revision.

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

`discovery.CodexRolloutLookup` owns the pass's requested coverage, native current
projection and optional full metadata epoch. Ordinary collection and generation
preview retain their requested lane. `MetadataInventory` explicitly requests a
lazy view with separate bounds: 16 homes, 16384 physical entries, 2048 directories
and 16MiB aggregate metadata facts/header work, charged to the shared native read
budget. A late request starts a complete following round; neither observation
cache membership nor requested coverage proves complete inventory.
Physical candidates count before header validity; filesystem fingerprints include
subdirectories and unrelated entries and retain a separate18432-entry traversal
ceiling. Aliases of one canonical home share acquisition while every approved
home spelling remains fenced.

Requested coverage runs in epochs: observe every directory, then validate every
directory's membership digest and each rollout's header identity, then
complete. Only a completed, unfailed epoch proves a requested thread's
candidate set (`Complete`). A completed epoch restarts when the next pass loads
it, so proofs are double-buffered: on restart each request the epoch covered
keeps that epoch's member candidates as its retained proof, which serves the
thread while later epochs run. A newer unfailed epoch replaces it; one that did
not cover the request (it overflowed) drops it; a failed epoch leaves it. A
proof is a past observation: consumers reopen every candidate and recheck its
identity, and a candidate moved or rewritten since fails that read and is
retried. What a proof can miss is a rollout created later, so live evidence
retires it: when a listing or current-row header read finds a locator claiming
the thread that the proof in use (this epoch's or retained) does not hold, or
another identity at one it does, that request has no proof until a newer epoch
completes. Restored observation-cache facts, which can name vanished locators,
never retire a proof. Live evidence arrives only as listings reach a
directory, so a retained proof can still lack a segment created after it. The
set marks such a proof `Retained`: a source signature may use it to recognize
an unchanged lineage, but a read without a current locator does not choose a
lineage tip from it, and waits for a proof completed in its own pass; the
collector reports that wait as deferred (waiting for history coverage), not
as a failure. A proof completed in the current pass can also lack a segment
created after its directory was listed earlier in the epoch; such a read
chooses the older tip, history already captured, and the next listing of that
directory changes the lineage and causes a recapture. A discovery checkpoint that misses its pass deadline
hands its catalog to the lookup, whose close writes it without that deadline,
so an epoch that completed just before is not lost. Checkpoints accept a retained proof only from an earlier
attempt that covered the request, with every candidate marked a member.

Source signatures hash the set's lineage token: the revision the same current
locator, candidates and candidate observations would carry if complete. The
pass-local revision that `Check` compares still includes completeness, but a
coverage change alone never makes an unchanged session look changed to the
scan-skip check. A new segment, a changed candidate set or changed candidate
stamps still change the token.

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

The collector publishes related histories through its existing durable pending
journal. Every final reference is verified before exact predecessor preflight and
metadata replacement, and every reference plus exact metadata bytes is read back
before acknowledgement. An exact committed retry performs no source or metadata
PUT. Stronger privacy resolves predecessor/final state before replacing private
work; unknown authority remains pending. Older config writers remain protected by
the permanent config-v5 writer marker.

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
