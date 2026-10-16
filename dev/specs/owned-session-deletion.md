# Owned session deletion and exact retention restoration

This protocol depends on the SourceSet5 publication foundation. Its synthetic
checks qualify the owned legacy/source-set deletion and replay ports. They do
not enable native history admission, historical import staging, or live provider
acceptance that their own entrypoint fences still withhold.

An owned deletion uses `session-deletions/<archive ID>/intent.json`, version 1.
The canonical journal binds the immutable local admission owner (session/native/
project/harness/destination/admission/origin/import/generation identities), removal
reason, exact selecting metadata SHA-256, ordered complete source references,
reviewed request token and deletion time. The exact metadata wire is retained
unchanged at `sources/metadata.json`; each source is retained at
`sources/<SHA-256>.gz`. Checksums, sizes, header identity and source schema are
verified before entering the destructive phase. Unknown owner, schema, temporary
control, source, or selector evidence grants no new authority.

The phases are prepared, deleting, absent, cleaned, finalizing, restoring,
restored and cancelled. Prepared has not granted a remote delete; a newer hook
can cancel it without inventing absent predecessor evidence. Deleting is durable
before sending metadata deletion, so a missing response can be reconciled after
restart. Metadata goes before namespace sources. Every destructive boundary
rechecks the exact immutable intent and current selector; a different winner
refuses. New retention work stops namespace deletion and preserves the complete
private selecting union. Explicit undo revokes restoration and keeps deleting.

Catalog metadata authority remains independent. Its existing exact-revision CAS
tombstone and snapshot/GC ownership apply; this protocol never replaces that
route with auxiliary listing or physical namespace deletion.

A newer, non-backfill hook can authorize retention restoration after the owned
metadata became absent. Only the exact previously acknowledged metadata wire and
all its retained sources can be restored. Their age, source2/source3 format,
filter/adapter/skill ceiling, native relationships and original bytes do not
change. Changed current policy refuses this baseline replay. It must not upload
broader bytes or treat a missing intent as known absence. Undo cannot enter the
restoration factory.

Restoration is a ready SourceSet5 capture with a retention receipt inside its
immutable preparation. That receipt binds the exact deletion intent, owner,
metadata and authorizing request. Deletion stage handles use the closed
`session-deletion-sources` namespace. Every replay and acknowledgement rechecks
the actual durable intent. The normal source resolver, complete source-set
verification and single-use attempt-bound closing proof precede the full local
selecting save. This is a baseline restore: it does not include the newer hook's
content, acknowledge its request, or settle a native scan signature. Normal
capture or an existing capture-gap rule handles that still-owed evidence later.
Once the selecting journal is removed through its normal proof, the exact
restoration inputs and intent are removed durably. A copied old seal then has no
remaining restoration authority.

Local deletion keeps normal request locks, generation retirement, registration
membership commits and staged/catalog ownership fences. Owning directories and
regular-file identities are pinned before unlinking ordinary records. Directory,
home, or file replacement refuses without deleting the replacement target.
After ownership records are removed, finalizing is durable before retained
sources and metadata are cleaned; a crash during that cleanup remains resumable
without native input or a surviving registration. Parent directories are synced
before the local-removed receipt. Surviving controls, protected stages, corrupt
records or anonymous scratch obligations remain recovery work.

The disk pool remains 1 GiB. Sources and immutable metadata retain ordinary
atomic-write accounting. One aggregate 64 KiB allowance inside that pool covers
the old/new coexistence of phase controls, each at most 32 KiB. Extra retained or
interrupted controls remain charged. No publication, stage, transcript, native
read, source count or decompression cap is increased. Sources are processed one
at a time with the existing pass read ledger.

A sticky outer configuration fence `{version:18, writer:"owned-deletion-v18",
base:...}` retains the exact existing base fence, including SourceSet17 and any
native-authority wrapper. It requires source-set and durable-storage protection;
rollback cannot drop it. SourceSet pending/publication outer version 5, source
schemas 2/3, metadata schemas 1/2 and privacy/parser versions are unchanged.
Older writers must refuse the outer fence. It cannot prevent an independently
credentialed peer from issuing uncoordinated object-store writes; coordinated
writer upgrades and the existing destination authority remain required.
