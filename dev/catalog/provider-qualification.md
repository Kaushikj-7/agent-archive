# Catalog provider release gate

Production catalog-v4 construction currently refuses every S3/R2 destination.
SDK conditional headers and synthetic protocol tests do not establish provider
atomicity. No config flag or provider-name allowlist overrides this gate.

Before release, run `storage.ProbeConditionalSemantics` explicitly against an
isolated prefix on each exact production endpoint/provider contract. The probe
creates a random synthetic `.catalog-qualification/` object, verifies duplicate
create refusal, races two writes against the same ETag, verifies exactly one
winner and exact readback, rejects a stale validator, and removes its object.
Run repeated cross-client/cross-machine races, including concurrent creation,
provider throttling, disconnections before and after request acceptance,
and HEAD/GET validator consistency. Capture content-free evidence: provider,
endpoint contract, date, region, SDK version, retry settings, permission policy,
request identifiers, repetition counts, outcomes and uncertainty. Never include
credentials, transcripts or production object names. A single passing run is
not enough evidence of provider semantics.

The hidden operator route is `agent-archive _catalog probe --bucket <bucket>
--prefix .catalog-qualification/<name>/ --isolated`. Both target values are
required, and the target must be disjoint from the configured archive namespace.
An archive configured at the bucket root requires a different target bucket.
The command uses the configured credentials only for that explicit synthetic
target, never changes configuration, and reports an atomic observation rather
than granting release qualification or activating catalog-v4.

Qualification also requires an exact bounded GET returning the head body,
ETag and provider LastModified from the same object version, a reviewed
timestamp precision bound, and conservative provider clock bounds. The
conditional probe does not test or qualify this clock contract. Unknown
precision, mismatched versions, regressing publication timestamps, clock skew
outside the qualified bounds, or unavailable clock evidence must refuse GC.
S3's SDK LastModified response alone does not establish that contract.

Qualification must be reviewed and checked into this repository before the
matching `AtomicCatalogProvider.CatalogAtomicQualification` implementation may
return success. Record scope exclusions (custom endpoints, bucket types,
unsupported conditional headers) and preserve default legacy behavior. PR9
additionally gates cutover on all writers supporting catalog-v4 and an isolated
migration destination; mixed legacy/catalog namespaces cannot be complete.

# Writer and garbage collection contract

Only `catalog-v4/head.json` commits catalog visibility. Immutable metadata and
bounded B+tree nodes use SHA256 keys with create-only writes. Identity records
retain tombstones; a fifth bounded receipt tree retains mutation digests and
unique committed revisions. Persist the frozen mutation ID and expected session
revision in guarded `PendingPublication.commit` before any upload. Prior durable
writers reject the reserved `commit` field, and catalog destination configs use
the forward writer fence `catalog-v4-v10`. Legacy config/state remains readable.

`catalog.Store` resolves canonical metadata reads to the current identity entry,
including bounded history reads and validators. It never writes canonical
metadata. Source keys already include content hashes; adapter uploads are
create-only and reject different bytes at an existing key. Catalog entry
verification requires every source key to use the metadata's exact session
namespace and a lowercase SHA256 filename before consulting its bytes.
`Publication` binds one frozen mutation to the source-first metadata write.
`DeleteSession` commits
a tombstone and retains source objects until fenced GC. Direct source cleanup
returns `ErrGCRequired`, preserving upstream local cleanup obligations.

`Collect(ctx, Barrier)` requires a global complete writer/preservation/pending
inventory witness. `Barrier.Hold` must drain all writers, across all machines,
and hold them until release; its pending source and immutable object references
must be complete. A local mutex or one home scan is not such a witness. PR9 owns
the real destination coordinator. GC then acquires a durable head lease, retains
current roots, roots valid within ten minutes after their successor committed,
all preserved history source references, and the protected pending inventory.
Corrupt/missing references abort and leave the lease closed. Tree traversal
validates hashes and bounded structure before deleting any object.

Retirement uses the successor publication's exact provider witness, extended
by its timestamp precision, compared with the conservative earliest provider
time. Capture timestamps and client wall clocks cannot age snapshots. The
original publication witness survives every GC lease, release and recovery;
lease LastModified must never reset retirement ages. Immutable predecessor
heads retain their observed ETag, publication time, precision and root digest.
PR9 must start snapshot expiry from monotonic request start, refuse an open
that takes ten minutes, and refresh expired cursors through the current head.

Every lease transition changes generation and epoch to prevent head ABA.
Commit refuses a held lease and revalidates source references on each CAS
attempt. A crashed GC lease never expires into permission. `RecoverGC` requires
the exact observed owner and the complete global barrier, releases only the
lease, and leaves a later collection to rebuild inventory. Production must not
invoke GC without that qualified coordinator.

`agent-archive _catalog collect` and `agent-archive _catalog recover --owner
<exact-observed-lease-owner>` are the narrow maintenance routes. They require a
qualified catalog adapter and its `CatalogBarrier(context.Context)` capability,
supplied by the real destination coordinator. Missing or nil coordinator evidence
fails before any head or source write. The current provider refusal remains in
place before credential access, including the R2 Keychain. Recovery releases
only the exact observed durable owner under a newly held complete barrier.
