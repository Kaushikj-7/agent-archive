# Catalog-v4 migration and reads

Catalog-v4 is an opt-in destination format. Production S3 and R2 remain refused
until reviewed live conditional-write, same-version timestamp and provider-clock
qualification exists. Migration also requires a provider authority proving old
write credentials revoked, the source read-only, and every writer on protocol 10.
An operator flag cannot substitute for this evidence.

`agent-archive migrate --format catalog-v4 --prefix NEW_PREFIX` copies into an
empty, isolated destination. Use `--bucket NEW_BUCKET` when the original prefix
is empty or the prefixes overlap. Equivalent R2 endpoint/account spellings do
not create isolation. The command checkpoints a bounded source page only after
all its metadata, active source and preserved history bytes are hash verified.
Rerunning the same command resumes its durable checkpoint. Candidate archives
remain unavailable to ordinary readers until the exhaustive source/catalog
oracle and credential proof pass, followed by the local configuration cutover.
Original capture and preserved-history dates are unchanged.

`--rollback` restores the retained read-only source configuration and pauses
collection. It refuses if the activated catalog has subsequently changed.
Unresolved publication owners, mixed legacy metadata and missing/corrupt bytes
keep migration, collection and activation closed. Crashed owners never age into
permission.

Snapshots start their ten-minute monotonic lifetime when the request context is
created. Selected full metadata uses the pinned immutable reference and hash;
linked children and source resolution share that view. A changed or expired
continuation returns an explicit refresh error. A refreshed browser selection
uses a fresh request view.

| Query | Tree work | Discovery |
| --- | --- | --- |
| Exact canonical session identity | O(log N) | No canonical LIST |
| Global capture/activity root list, ordinary or replay, no dates | O(log N + returned rows) | No canonical LIST |
| All-session capture range including replays | O(log N + returned rows) | No canonical LIST |
| Direct project-tree range | O(log N + returned rows) | No canonical LIST |
| Scope tiers, title/text, ID prefix, complex predicates, date-filtered root child counts | Complete summary fallback | No canonical LIST |

Ordinary/replay immediate child counters use the existing harness+parent key.
Writers derive them in the same CAS as child changes; the full metadata body and
its revision stay authoritative. Counter-only leaf changes invalidate local
summary generations. Unknown prior roots rebuild complete summaries. A partial
range never becomes complete canonical listing evidence.

The private summary cache checks the entire persisted row tuple before reuse.
Immutable node/body cache reads verify the content hash and exact revision.
Missing, corrupt, canceled or root-inconsistent reads fail closed.

Maintenance uses the durable global admission coordinator plus any supplied
complete external history barrier. `_catalog recover --owner OWNER` requires the
recorded lease/inventory/head witness and only releases proven ownership.
`_catalog recover-seal --owner OWNER --generation GENERATION` is the separate
explicit recovery for a drained, unheld seal that never acquired a GC link.
Neither path accepts timeout, force or incomplete inventory as proof.

`catalog.WithReadView` preserves an existing request. Interactive browsers call
`catalog.NewReadView` for each newly selected session; this preserves cancellation
and deadlines while capturing a new head. It cannot extend an older view or its
cursors. Within one selection, metadata, linked children and transcript source
resolution share the captured root.

Show candidate projections carry explicit body provenance. A selected projection
loads authoritative immutable metadata. When this request already verified that
body during local summary hydration, `ReadCachedMetadata` may reuse only the
selected cached body after checking the bound complete generation, persisted row
tuple, exact revision, content hash and decoded identity. Missing or damaged body
cache entries fall back to the pinned immutable read; stale generation or damaged
row authority fails closed. Candidate bookkeeping retains verified keys rather
than every full body.

## Large metadata and writer version

The catalog uses writer protocol 10 and the exact `catalog-v4-v10` configuration
fence. Protocol numbers describe the wire format, independently of PR numbering.
Heads, admission records, pending journals and migration checkpoints reject older
protocol 9 state before admission. This format is opt-in and is not deployed here.

Tree nodes remain bounded to 128 KiB. Each serialized leaf, including its key and
record/count/revision wrappers, is measured at every update. Entries larger than
64 KiB use a strict `metadata-v10` envelope containing only index discriminators,
counters, revision and the existing immutable metadata reference. Full metadata
is never truncated or stored in a second payload namespace. Bounded queries stay
lazy and trim their selected rows before loading overflow bodies. Selected body
reads compare the exact hashed body against the envelope; complete fallback,
summary reconciliation, migration verification and GC explicitly resolve the body
and its full source/history references. Verified raw bytes may enter the existing
private metadata cache and are then discarded. Opaque resolved-row provenance is
bound to the exact snapshot and request lifetime and refuses caller mutation.

The 32 MiB metadata-body bound does not remove index-key constraints: canonical
session keys remain at most1024 bytes and encoded index keys at most4096 bytes.
Oversized project/parent/identity fields fail before head publication; no index
field is silently truncated or hashed into a different layout. Large linked-session
arrays are supported through overflow. Existing `list --no-cache` title/PR
searches use the complete summary fallback. Verified request-bound overflow
resolution can supply the selected full body without another GET; ordinary
projections still require selected full-body reads. This O(N) fallback is complete
and is not advertised as bounded or as universally requiring one body read.
`show` retains its existing flags and verifies selected full metadata and sources.

## Interrupted collector publication

Configured destinations bind recovery to the existing credential-free destination
identity and the actual held collector flock. A frozen journal's verified recovery
descriptor is durable before admission. The destination independently verifies
the originating pending journal and exact body/source/history/predecessor evidence.
Only an identical owner may transfer across a restart or drain after a seal.
Parallel and stale invocations refuse; lock release joins guarded work.

Completion keeps bounded durable receipts protecting the body and all source and
history refs. Local acknowledgment and an explicit durable removal record precede
pending unlink. Receipt acknowledgment requires that exact record and completed
unlink; missing files alone prove nothing. Completion replay has no write authority.
Unresolved receipts are never evicted to make room. Direct unfinished owners with
no originating journal remain conservatively fenced for operator recovery.

Migration initialization and rollback durably bind their chosen owner and exact
source/destination/proof/head descriptor before sealing. Rerunning reconciles
only that recorded intent, including an interrupted first checkpoint; unrelated
coordinators and mixed objects refuse. Rollback retains its writer seal and
read-only mode. A completed unchanged-root rollback can finish local configuration
restoration again after a local save failure.

An admitted collector journal remains immutable. Missing/corrupt recorded source
bytes retain the journal and owner; restoring verified bytes permits exact guarded
retry. A later privacy/skill/filter policy, replacement request or clock cap that
would change the frozen digest refuses with the original journal intact. Those
policy conflicts intentionally remain operator-fenced until a supported exact
owner disposition exists; collection never republishes broader evidence just to
clear an owner. Only positively completed work with its originating held guard
and durable exact completion tombstone can be unlinked. Legacy replacement and
source-unavailable skip behavior remain available to legacy journals.

Remote-backed SQLite queries retain the captured request lifetime. Initial and
completed queries validate that lifetime; cursor continuations observe exactly
one fresh head version and reject a changed root. `Snapshot.ValidateContinuation`
does not renew the request or expose cursor authority. Local SQL cursors also bind
the captured root and a private refresh nonce, so a refreshed handle cannot reuse
an earlier cursor even when the root is unchanged.

Remote summary reconciliation runs the existing bounded64-directory cache
maintenance once per cache. Only a committed, complete verified delta or rebuild
can remove body cache directories: absence from the complete current identity universe. Each complete-summary
refresh inventories cache directory names in O(N), then retries best-effort
removal only for known keys absent from its verified complete universe. This
retries interrupted cleanup without adding a journal. Bounded list selection is
unchanged; selected ranges never prove deletion.

Stats date windows select recent roots and include their older descendants using
complete authenticated identity headers and the shared root-child policy. Cold
or uncached catalog stats walk the identity tree in O(N), then load only selected
full metadata; an empty future window loads no overflow metadata bodies. Complex
non-date predicates and ambiguous headers can require a complete pinned body
fallback. These paths never use canonical LIST.

Stats can reuse an already complete SQLite summary universe only when its saved
root exactly matches the captured catalog root. The transaction checks complete
state, whole-row integrity, key count and the existing root binding before reuse.
This saves remote identity-tree walks but still scans O(N) local summaries and
cache directory names. Stats does not bootstrap or rebuild SQL to enable this
optimization: missing, changed, incomplete or damaged state uses lazy catalog
selection. Thus a stats-only workload may receive no SQL benefit. Selected bodies
retain exact revision/hash verification, eight-worker loading and the original
request lifetime. Safe title text longer than the supported catalog identity key
is searched directly instead of being probed as an impossible exact ID.

Native-child ownership is independent of whether a conversation parent has been
resolved. Catalog root ranges exclude both positive native children and linked
children; global child ranges include either form. Parent-specific counters and
indexes require a nonempty resolved parent and retain the harness-plus-parent
identity rule. Strict overflow envelopes preserve the native-child discriminator
and validate it against the immutable full metadata. An unresolved child is an
independent date seed for stats, never an inferred descendant of an unknown
parent. Earlier source bundles may lack the optional native-child marker even
when metadata now records native ownership; existing exact source identity,
parent and reference verification still governs those supported legacy sources.
