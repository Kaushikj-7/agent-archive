# Bounded Cursor live-WAL admission

Status: preparatory backend only, reviewed against `d338beef`. This does not
enable live database admission. The current [backfill contract](../specs/backfill.md)
continues to require settled evidence; live WAL frames remain pending.

## Remaining capture gap

Current durable `ImportStage` and `StagedSelection` already preserve selected
filtered input before admission. The unresolved part of the older #359 proposal
is reading a coherent live SQLite snapshot without unbounded temporary writes or
mutating Cursor's database, WAL or shared memory. An ordinary copied path is not
a sealed native capture proof. The old mutable admission source provider must
not be restored.

## Preparatory implementation

`internal/cursorstore/admission*.go` contains a private backend with no production
caller. It reuses the pinned online-backup/VFS design from #359, adapted to a
private rooted scratch-reservation contract. It refuses ordinary settled paths;
those retain their existing read-only capture path. The copy holds one actual
source transaction before observing page geometry, caps the image at 128 MiB,
steps at most 16 pages at a time and bounds busy retries. The scratch owner must
reserve the complete image before the destination is created. Destination VFS
callbacks check write/truncate bounds before filesystem writes and prevent
SQLite from opening journals or other scratch paths. The destination binds a
held rooted descriptor even if an ancestor path changes.

Native reads refuse unknown Unix VFS ABIs and root execution, enforce read-only
native opens, retain checked database/WAL/SHM identities and reject a writable
process-wide shared-memory node before mapping. Failed copies leave partial
scratch with their owner, rather than claiming successful cleanup or releasing
its quota. These are synthetic implementation checks, not actual Cursor or
macOS acceptance evidence.

## Integration gates

1. Implement a durable scratch owner using current storage protections. Reserve
   capacity before allocating scratch, persist its receipt, retain quota across
   failed deletion/crashes, and recover stale receipts without deleting unrelated
   paths. Bind a captured shared memory reservation to the entire SQLite/image
   read lifetime, including SQLite working memory and simultaneous decoded views.
   The preparatory backend's tests use a synthetic owner; they do not implement
   or prove these durable guarantees.
2. Add a distinct sealed producer operation that binds the original request,
   context, source home, exact database/source identity and captured reservation
   to the held image. Check routing and metadata from the same pinned image.
   A generic filesystem path must never satisfy `CaptureSettledCursor` or mint a
   `nativecapture.Proof`. Preserve exact selected source bytes before admission.
3. Compose the sealed result with existing staged3/Cursor4 ownership and recovery
   controls. Do not require or weaken the separate source-set publication protocol.
   Preserve catalog compatibility, stage quotas, finalizer ordering and the
   behavior of ordinary settled reads. Persisted-format changes need their own
   compatibility review before assigning versions.
4. Independently review the complete composed path. Run actual supported-platform
   Cursor acceptance on disposable data, with concurrent external writers,
   cancellation, contention, process crashes and cleanup failures. Required CI
   and exact-head review remain merge gates.

The preparatory slice alone is insufficient to close #359 as completed. Keep its
remaining admission scope visible until the durable and sealed integration gates
are implemented and independently verified.
