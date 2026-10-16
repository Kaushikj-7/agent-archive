# Bounded Cursor live-WAL admission

Status: backend, durable owner and distinct sealed producer under review against
`d338beef`; production caller integration remains pending. This does not
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

`internal/cursorstore/admission*.go` contains a bounded backend with no production
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

1. The current candidate adds a durable `CursorScratch` owner under existing
   storage protection. It persists image capacity before creation and retains
   receipts across failed cleanup and crashes. Its bounded recovery refuses active
   owners, unknown receipt shapes, symlinks and unexpected children. Current generic
   durable writers conservatively refuse all temporary obligations, so cleanup is
   mandatory before staging; no older-writer acceptance rule is relaxed. Control
   memory uses the original captured request scope. The SQLite operation charges
   a 32 MiB working allowance before opening, caps WAL-index mappings at 4 MiB,
   requests 2 MiB SQLite caches and bounds destination page buffers at 64 KiB.
   This bounds charged logical work, not process RSS. Independent review and
   supported-platform failure/acceptance evidence remain required.
2. The candidate's distinct `LiveCaptureRequest` binds the exact original request
   lease to a real state-issued scratch owner. Whole-source type/object checks
   restrict issuance to the exact state method and actual owner method values.
   A fixed live producer reads composer and metadata on the held copied image,
   requires synchronous scratch cleanup before minting separate `LiveProof`, and
   retains its original request scope through proof use. A generic filesystem path
   cannot satisfy `CaptureSettledCursor` or create settled proof. Synthetic tests
   cover first staging after the native database disappears. This path still needs
   independent whole-source/lifetime review before runtime wiring.
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
