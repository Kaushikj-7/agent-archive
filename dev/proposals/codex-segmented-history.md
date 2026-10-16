# Segmented Codex history: capture a long thread a segment at a time

Status: proposed, not implemented; owner decisions D1–D9 recorded
2026-10-16. Prepared 2026-10-16 against `0966523b`
(main after [#615](https://github.com/wangjohn/agent-archive/pull/615)). No
version, schema or configuration capability is allocated by this document;
the PRs below allocate them under [versions](../maintainers/versions.md). The
format this introduces gets its own spec (`dev/specs/codex-segments.md`) in
PR 1; the [retained-history contract](../specs/codex-history.md) stays the
contract for source schema 3 until then.

## Problem

A Codex thread in paginated history mode continues into later rollout files:
each later rollout's `session_meta.history_base` names the rollout it continues
from, with an exclusive ordinal boundary. The collector materializes the whole
chain into one self-contained history source (source schema 3, metadata schema
2). Every capture pass therefore reads, filters, holds and uploads the entire
thread, although every rollout before the tip is immutable once continued and
the tip itself only grows by appending.

The owner's thread `01a10e11` (archive `87b47b56`) shows the cost. Its chain is
a 19 MB rollout and a 240 MB tip; the filtered size F is about 56 MB and grows
about 11 MB a day.

- **Read memory.** Even after #615 (reconciliation skips the tip, pending
  publications are written a record at a time, the pending fence reads only
  proofs), a pass needs about 3–4×F of `collector.SessionReadCeiling`
  (160 MiB). The thread already fails that ceiling and sits in a
  `SizeLimitReadBudget` gap. The owner chose not to raise the ceiling.
- **Filtered ceiling.** `collector.DefaultMaxTranscriptBytes` (64 MiB) bounds
  the whole filtered thread. Any thread reaches it near 290 MB of rollout,
  whatever the read ceiling is. For this thread the read ceiling bound first
  (near 45 MB filtered), so the filtered ceiling changes nothing for it today.
- **CPU and upload.** Redacting kept text is the dominant cost of a read (32 s
  for a 190 MB rollout, per the `archive.MaxCodexRawTranscriptBytes` comment).
  Each pass that sees an appended record repeats all of it and uploads a new
  copy of the whole filtered thread. Every preserved revision is another
  self-contained copy of a mostly shared prefix.

Note that per-*rollout* capture alone does not fix this thread: the 240 MB
rollout is the tip, still appended to. The durable fix has to stop re-reading
the already-captured prefix of the **tip** too. This proposal therefore cuts a
thread into **segments**: ordinal ranges of one rollout, where an earlier
rollout's span is one sealed segment and the tip is cut into sealed chunks
plus one open tail.

## Until this ships

Nothing is changed in the meantime. `87b47b56` stays in its read-ceiling gap at
its last published snapshot. Raising the filtered ceiling would not help,
because the read ceiling binds first. Raising the read ceiling was declined,
and any full-chain stopgap meets a ceiling again within weeks. Waiting loses
no history as long as the native rollouts stay on disk: the chain lookup also
searches `archived_sessions/`, and the first segmented pass reads the chain
from ordinal 0 (see [migration](#migration-of-existing-history-sources)). The
only risk is the rollout files being deleted from the Codex home before then.

## Goals and non-goals

Goals:

- A pass reads, filters, holds and uploads only the open tail of the tip
  (plus at most one new rollout), so memory and CPU follow the segment size,
  not the thread size.
- Sealed segments are uploaded once and referenced by content address from
  every later publication and every preserved revision that shares them.
- Reading (show, handoff, search, verification, parser refresh) walks the
  segments one at a time with memory bounded by one segment.
- Privacy guarantees, ordinal proofs, logical ownership, the reader-before-
  writer rule and the existing lifecycle (retention, delete, purge, GC,
  privacy maintenance) all hold per segment and across the set.

Non-goals:

- Raising `SessionReadCeiling`, `PassReadBudget` or the per-record limit.
- Segmenting Claude or Cursor sources, or ordinary (schema 2) Codex sessions
  under the threshold (see decision D1).
- Incomplete or partial archives. The [fragment foundation](../specs/codex-fragments.md)
  is a different artifact: a single surviving rollout whose chain is missing,
  labelled incomplete with unknown activity ownership. A segment set is a
  complete chain cut into pieces, every piece proved against the chain. This
  proposal does not reuse the fragment envelope or its narrow record subset,
  and fragment readers must never accept a segment (or the reverse).

## Where the cost lives today

| Place | What it holds per pass for a history source |
| --- | --- |
| `internal/agents/codex/related_source.go` `readHistorySelection` / `historySnapshot` | Streams every span of the chain from rollout ID and ordinal 0; hashes each native prefix twice (snapshot and before returning output). Descriptor reads are not charged; line buffers and cached ancestor prefixes are (`prefixCacheLimit` 16 MiB). |
| `internal/agents/codex/history_filter.go` `filterHistoryEncoded` → `nativecodec.FilterCodexHistoryEncoded` | Filters every record of every span. Output is one `FilteredTranscript` for the whole chain; span `FirstRecord/EndRecord` are recomputed after drops. |
| `internal/collector/session.go` `read` / `build` / `compare` / `guard` | The filtered transcript, the candidate `SourceBundle` built from it, and the `state.Published` cached bundle it is compared and guarded against (`bundleEvidenceEqual`, `nativeEvidenceExtends`). |
| `internal/collector/history_reconcile*.go` | Reads each earlier chain rollout as a historical revision (`revisions`, `readRevision`), plus off-chain candidates best effort. |
| `internal/collector/history_freeze.go`, `history_prepare.go`, `history_transaction.go` | Freezes a `state.PendingPublication` whose `Bundle` and `SourceBytes` are the whole thread; stages each input; reads back every reference before acknowledging. |
| `internal/collector/size_limit.go` | Records `SizeLimitRaw` / `SizeLimitFiltered` / `SizeLimitReadBudget` gaps for the whole thread. |
| `internal/archive/history.go` | `SourceHistory` (≤ `MaxHistorySpans` 64 spans, ≤ 1M records, one active rollout), `RevisionHistory` (≤ 64 preserved whole-source references). |
| `internal/reader/reader.go` `LoadSource` / `LoadRevision` | Decodes the whole bundle (defaults 32 MiB compressed, 128 MiB uncompressed) for show (`cli/show_transcript.go`), handoff (`cli/picker_handoff.go`), browse, search projection and verification. |
| `internal/agents/nativecodec/parse.go` `ParseCodex` | Derives metadata from the whole in-memory `SourceBundle`. |

The Codex record filter is close to stateless per record: the only state that
crosses records is the deduplicated gap set, the omitted/denied key-name sets,
`NativeStartComplete` and the identity notes (`noteOutputNativeIdentity`). Per-
segment filtering is therefore feasible, but the codex-history spec warns that
"the privacy codec has stateful interpretation"; PR 3 turns that warning into
an equivalence test instead of an assumption.

## Design

### Segments

A **segment** is a newline-complete, ordinal-contiguous range
`[start_ordinal, end_ordinal)` of exactly one physical rollout of the thread,
filtered on its own. A thread's **segment set** for one revision is the ordered
list of segments covering that revision's chain, and its ordinals are the
existing physical ordinals (the spans of today's manifest, cut finer):

- An earlier rollout contributes the span its successor's `history_base`
  boundary includes. It is **sealed** when that boundary is proved: it never
  changes again for this chain.
- The tip contributes zero or more **sealed chunks** and one **open tail**.
  The collector seals a chunk when the filtered bytes captured since the last
  seal pass the chunk target (D2), cutting at the next newline-complete record
  boundary. A chunk can exceed the target only by its last record, which is
  already bounded by `archive.MaxRecordBytes`.
- A rollout that becomes an earlier rollout (Codex continued into a new tip)
  has its open tail sealed at the new successor's boundary.

Sealing is a collector decision about filtered output; it is never inferred
from file size, modification time or lexical order.

### Raw checkpoints (private)

To skip a sealed prefix, the next pass needs proof that the native bytes it
covers are unchanged. For each sealed segment the collector keeps a private
**raw checkpoint** in local state, never uploaded:

`{rollout_id, start_ordinal, end_ordinal, raw_start, raw_end, raw_sha256, filter_version, adapter_version, segment_sha256}`

`raw_end` is the byte offset just past the segment's last newline. This follows
the precedent of the Muse successful-validation checkpoint (private native
binding version 2: positive raw byte boundary plus SHA-256). A pass:

1. Reopens the chain as today (tip from Codex's index, walk `history_base`,
   header proofs, admission checks). Lineage selection is unchanged.
2. For each rollout, streams and hashes its prefix up to the last checkpoint's
   `raw_end` (memory O(1); descriptor reads are not charged) and compares it to
   the recorded hashes (D6). Any mismatch, a moved boundary, a changed header
   proof or a filter/adapter version change invalidates that checkpoint and
   every later one, and those segments are read again.
3. Filters only from the first invalid or open position onward, starting the
   ordinal counter at the checkpoint's `end_ordinal`, and emits new chunks.

Re-hashing still reads the whole rollout from disk each changed pass, but it is
a streaming checksum: no decode, no filter, no retained records, and it only
happens when the source signature changed. It is the same native-prefix hash
the provider already performs twice per read today.

### Uploaded format

Two new incompatible versions, allocated in PR 1 (numbers here are
placeholders):

- **Segment source** (source schema S, Codex-only). One gzip JSONL object per
  segment, same line kinds as schemas 2/3. Its header carries the thread ID,
  `rollout_id`, `start_ordinal`, `end_ordinal_exclusive`, the record count,
  whether the segment is sealed, and `own_start_ordinal` when an explicit
  child/fork boundary falls inside or before it. Records carry exact ordinals
  as in schema 3. Only the segment that holds the active rollout's
  `session_meta` carries it; supplemental evidence (`session_labels`, skill
  evidence, child links) lives only on the **head** object (below), never on
  a sealed segment, so renames and label updates never rewrite a segment.
- **Segmented metadata** (metadata schema M). The sidecar's `source_bundle`
  reference names the **head**: a small source object holding the envelope
  (identity, capture, `native_child`, parent, linked sessions, supplemental
  evidence) and the active revision's ordered segment list. Each list entry is
  `{segment reference, rollout_id, start_ordinal, end_ordinal_exclusive,
  captured_at}`. Preserved revisions become `{revision_id, captured_at, head
  reference}` and their heads list their own segments, so revisions that share
  a prefix share its segment objects.

`Metadata.SourceReferences()` returns the head, every distinct segment the
active and preserved heads list, and every preserved head, so the 20-odd
callers that already walk it (`internal/retention`, `internal/purge`,
`internal/catalog` GC and migration, search, the collector's fences and
readback) see every object without each learning the format. All keys stay
checksum-addressed under the session's prefix; a reader refuses any reference
outside it before reading.

Whether the segment list lives in the metadata sidecar itself or only in the
head is decision D3. Listing reads bounded metadata bodies
([listing at scale](listing-at-scale.md)), so the recommended shape keeps the
sidecar's size independent of the segment count by putting the list in the
head, and has `SourceReferences()` read heads (bounded, small) when it needs
the full set.

### Collector flow

`sessionScan` keeps its shape; what changes is what each step holds.

- **read**: the Codex provider gains a segmented read that takes the private
  checkpoints and returns new sealed chunks plus the open tail as separate
  `FilteredTranscript`s. Each chunk is staged (compressed, checksum-named,
  durable) as soon as it is sealed, and its records are released before the
  next chunk is filtered. Peak work is one chunk plus the tail.
- **build / compare / guard**: the candidate is the head plus the open tail.
  `state.Published` caches the head, the segment list and the open tail
  instead of the whole bundle. `nativeEvidenceExtends` guards the open tail
  against the cached tail; sealed segments are guarded by their checkpoints
  (unchanged raw bytes under the same filter version yield the same segment by
  construction). Capture time is assigned per segment: a pass that only
  extends the tail gives the tail (and the head) the new `captured_at`; sealed
  segments keep theirs.
- **publish**: the pending journal (`state.PendingPublication`) records the
  head, the new segment stages and the references it keeps by checksum, not a
  whole `Bundle`. Publication uploads only new segments and the new head, then
  the sidecar; readback verifies every *new* object fully and every kept
  reference by HEAD/size and checksum metadata (D6 applies to remote readback
  too). An exact committed retry still performs no PUT.
- **reconcile**: an earlier rollout that was once the tip is a historical
  revision whose head lists the sealed segments it shares with the current
  chain plus, when its file holds records past the successor's boundary (a
  rollback), one extra off-chain tail segment. Reconciliation no longer
  re-reads shared prefixes; it reads only off-chain tails, still best effort
  under the existing rules (`revision_left_out`, the 64-candidate cap, the
  rerun without off-chain branches when the ceiling is met).

### Ordinals, `history_base` boundaries and ownership

- Segment boundaries are a refinement of today's spans: every span boundary is
  a segment boundary; chunk boundaries add more. Validation is the existing
  `SourceHistory.Validate` rule applied to the concatenated list: contiguous
  ordinals across segments, `start` of each segment equals `end` of the
  previous one within a rollout, and each rollout's first segment starts where
  its base's included prefix ended. Gaps, regressions, duplicate segment
  identities and a list that does not end on the active rollout refuse.
- `own_start_ordinal` keeps its meaning. A segment entirely before the own
  boundary is inherited context, a segment that straddles it carries the
  boundary, and `OwnRecord` applies per segment. An absent child boundary still
  excludes nothing.
- A segment's identity is `(thread_id, rollout_id, start_ordinal,
  end_ordinal_exclusive, filter_version)`; its content address is the SHA-256
  of its compressed bytes. Two chains that include different prefixes of one
  base rollout produce different segments: correct, and rare enough not to
  optimise.

### Metadata derivation across segments

Today `codex.Parser.Parse` calls `nativecodec.ParseCodex` on the whole bundle.
Segmented derivation converts the Codex parse into a record iterator over the
segment list:

- Counts, tokens per model, tools, MCP calls, git activity, end time and gaps
  accumulate in one pass in segment order. Cumulative-only token counters keep
  their explicit gap rule, and the last counter value comes from the tail.
- The title (first own prompt, with filter 16's injected-context rule) comes
  from the first segment with own activity; `session_labels` (on history
  sources since filter 26) and native names come from the head's supplemental
  evidence, exactly as `archive.CurrentSessionLabel` reads them today.
- Filter gaps are the union across segments, deduplicated by code and detail
  as `addGap` does now.

Decision D4 is whether each publication streams every segment again (memory
bounded by one segment, CPU proportional to F, rate limited by the upload
interval) or resumes from a private, serialized accumulator checkpoint at the
last sealed segment. The proposal recommends streaming first: it needs no new
persisted state, and an equivalence test (iterator over the segment list ==
`ParseCodex` over the assembled bundle) proves it. A checkpoint is a follow-up
only if profiles show derivation CPU matters.

### Reading: show, handoff, search, verification

- `internal/reader` gains a segment iterator: open the head, then each segment
  in order, each decoded under today's per-object `Limits` and checked against
  its list entry (thread, rollout, ordinals, count, checksum) before any record
  is yielded. A malformed reference cannot widen reads.
- `LoadSource` keeps returning one assembled `SourceBundle` for callers that
  need it, but only when the assembled size fits the caller's limits; past
  that it returns a named error telling the caller to use the iterator.
- `cli/show_transcript.go` streams segments into the pager; memory is one
  segment plus the rendered window.
- `cli/picker_handoff.go` builds the handoff from the tail backwards: it reads
  segments newest first until the handoff's own size bound is met, taking the
  title and counts from metadata, so a handoff of a 1 GB thread reads only its
  recent segments.
- `internal/search/projection.go` projects each sealed segment once, keyed by
  its checksum, and re-projects only the tail.
- Publication verification (`verifyPublication` in `cli/verification.go`)
  loads each segment of the active and preserved heads one at a time.

### Size limits

| Limit | Today | Proposed |
| --- | --- | --- |
| Filtered size | 64 MiB per thread (`DefaultMaxTranscriptBytes`) | Chunk target (D2, recommended 16 MiB) and a hard per-segment ceiling of target + one record. A new per-thread total filtered ceiling (D7, recommended 1 GiB). |
| Segment count | 64 spans per thread | Per revision (D7, recommended 256) and distinct objects per session (recommended 1024); sealed chunk size could grow geometrically past the count if D7 prefers. |
| Raw size | 512 MiB per thread read (`MaxCodexRawTranscriptBytes`) | 512 MiB of *unsealed* bytes filtered in one pass; a separate total raw ceiling for the checkpoint hash (D7, recommended 4 GiB). |
| Read ceiling | 160 MiB per session, ~3–4×F | Unchanged constant; work is ~3–4× (one chunk + tail), about 64 MiB at the recommended target. |
| Record | 64 MiB | Unchanged. |
| Chain depth | 64 rollouts | Unchanged. |

A thread over a new ceiling records the existing size-limit gap kinds with the
new `LimitBytes`, so `readCeilingChanged`-style signature checks read it again
when a build changes the limit. The gap's capture is the last fully published
segment set.

### Local state

- **Raw checkpoints** live with the session's private native binding (D6
  covers when they are trusted); a version bump of that record refuses older
  writers, as Muse's binding v2 does.
- **Sealed segment stages** are kept as owned, checksum-named stages so that
  privacy maintenance, parser refresh and readback recovery never need native
  input. They count against the shared 1 GiB staging/publication quota
  (`state.durableStorageQuota`); decision D5 is whether past a bound they are
  dropped and fetched from the bucket with checksum verification instead.
- **Pending journal**: a new `PendingHistory` journal version carrying the head,
  segment stages and kept references. Older journal versions keep their
  current recovery paths.

### Privacy maintenance

A stricter privacy policy re-filters every segment from its retained stage,
sequentially, one segment in memory at a time, producing new segment objects
and a new head; `stricterHistorySuccessor` already works input by input and
never reads native data or assigns maintenance time as capture time. Retired
segments become `RetiredSource` cleanup obligations after successor readback.
A filter or adapter version bump that needs native input invalidates every
checkpoint and re-reads the chain segment by segment, across passes if the
pass's time budget ends, keeping the previous segment set published until the
new one is complete.

### Retention, deletion and GC

- `MeaningfulCapturedAt` is the latest `captured_at` across the active and
  preserved segment entries and heads, with the existing clock clamp. Parser,
  link, privacy and representation work still do not manufacture activity.
- Session delete and purge keep their order: metadata first, then the whole
  prefix. Because segments live under the session prefix, whole-prefix
  deletion removes them.
- GC protects every object `SourceReferences()` returns for the live sidecar
  and every pending journal's stages, and rechecks metadata before deleting a
  source, as today. A sealed segment shared by several revisions is deleted
  only when no live head lists it.
- Undo and import batch ownership stay whole-session.

### Reader-before-writer rollout and versioning

1. **Reader release.** PR 2 ships readers for the segment source and segmented
   metadata in a release before any writer exists. Readers before it refuse the
   new schemas (they already refuse unknown versions), so a segmented session
   is unreadable to an old `show`/`handoff` and the error must say to upgrade.
2. **Writer capability.** The writer is enabled behind a new permanent
   configuration writer capability (the next unreserved configuration version
   in `internal/config/wire.go`, coordinated with open reservations), so an
   older collector on the same configuration refuses to write rather than
   republishing a whole-chain schema-3 source over segmented metadata. Older
   collectors that read remote segmented metadata already refuse unknown
   schemas.
3. **Versions.** The writer PR bumps the Codex adapter and filter versions (the
   uploaded representation changes), the source and metadata schema versions,
   and the Codex parser version only if derived values change (equivalence
   says they should not). `schemas/` and versions.md change in the same PRs,
   along with a privacy changelog entry stating that what is retained per
   record is unchanged.

### Migration of existing history sources

A schema-3 session is migrated by its next capture, not by rewriting retained
bytes: the first segmented pass has no checkpoints, so it reads the chain once
from ordinal 0, sealing and staging chunks as it goes (memory one chunk), and
publishes the segment set as the same current revision. The old schema-3
object becomes a retired reference after the new set's readback. Preserved
schema-3 revisions stay as preserved references; readers keep reading them
(prior schema-2 and schema-3 references are already permitted in metadata
schema 2, and remain so in M).

Splitting retained schema-3 objects into segments without a native read (D9)
would keep capture times but produce no raw checkpoints, so the next pass would
read the whole chain anyway; recapture is simpler and gives the same result.

A session in a `SizeLimitReadBudget` or `SizeLimitFiltered` gap (like
`87b47b56`) is read again once the writer is enabled, because its recorded gap
limit no longer matches the build's.

## PR breakdown

Each PR has one concern and is merged in order unless marked parallel.

1. **Spec and schemas.** `dev/specs/codex-segments.md`, `schemas/` for the
   segment source, head and segmented metadata; allocate the schema numbers in
   versions.md; schema tests. No code paths.
2. **Archive types and reader.** `internal/archive` segment types and
   validators; `internal/reader` segment iterator, bounded `LoadSource`
   assembly; show, handoff, browse, search and verification consume the
   iterator. Synthetic fixtures only. Ships in a release before PR 6 enables a
   writer.
3. **Filter equivalence and segmented provider read** (parallel with 2).
   Codex provider read from raw checkpoints; chunk sealing; tests and a fuzz
   target proving that concatenated segment filtering equals whole-chain
   filtering (records, ordinals, gaps union) for the same input, including
   child/fork boundaries, rollbacks and records straddling a chunk target.
4. **Streaming Codex parse** (parallel with 2–3). Record-iterator form of
   `ParseCodex`; equivalence tests and benchmark against the assembled-bundle
   parse on 10k/100k/1M-record synthetic threads.
5. **Local state.** Raw checkpoints in the native binding, sealed segment
   stages and quota accounting, the new `PendingHistory` journal version and
   `Published` cache shape, with crash-recovery tests for every checkpoint
   boundary.
6. **Collector writer.** Segmented capture path in `sessionScan`, reconciliation
   as segment lists, per-segment size limits and gaps, pending/readback; behind
   the new configuration capability, off until PR 8.
7. **Lifecycle.** Privacy maintenance, retention, delete, purge, GC and catalog
   migration over segment sets, with tests that a shared segment survives the
   deletion of one revision. Must merge before PR 8.
8. **Enable and migrate.** Turn the writer on for threads selected by D1;
   migration of schema-3 sessions by recapture; adapter/filter/parser bumps,
   CHANGELOG, privacy changelog, user docs (`docs/` size-limit and status
   wording), versions.md.

## Validation and live checks

Synthetic (every PR, CI):

- Equivalence tests in PRs 3 and 4; fuzz the segmented filter and reader for a
  couple of minutes with `-fuzzminimizetime 2s`, keeping failing inputs.
- A synthetic 300 MB, two-rollout thread (generated, never a real transcript)
  that today hits `SizeLimitReadBudget`: captured with peak ledger use and
  sampled heap under 64 MiB per pass; a subsequent append pass reads only the
  tail (count source read bytes filtered vs hashed) and uploads one segment
  plus head and sidecar.
- Rollback, fork and child cases: a base rollout continued at two boundaries;
  an off-chain tail preserved as a revision sharing sealed segments.
- Crash at each journal step; stricter privacy re-filter of a 32-segment
  thread with one segment in memory; delete/purge/GC of shared segments.

Sandboxed live (maintainer, before PR 8 enables the writer): the
[local end-to-end recipe](../contributing/testing.md) with a temporary `HOME`,
`AGENT_ARCHIVE_HOME` and `CODEX_HOME`, a local MinIO bucket and a real Codex
build producing a paginated thread in that temporary home. Check: capture,
continue into a second rollout, append for several passes, `show`, `handoff`,
publication verification, `stats`, an older build refusing to read and to
write, rollback to an earlier turn, a stricter-privacy pass, delete and purge.

Owner live check (after release, on the owner's own machine and archive, run
by the owner): `agent-archive status codex` no longer lists `87b47b56` in a size
limit gap after one pass; a pass after more Codex activity uploads one segment;
`show` and `handoff` of the thread work; peak RSS of the collector pass is
recorded. This document does not authorize running sync, backfill or purge
against the owner's real archive.

## Owner decisions

The owner accepted every recommendation below on 2026-10-16; the column
records the decision.

| | Question | Decision |
| --- | --- | --- |
| D1 | Which sessions use segments: every Codex history thread, only threads whose filtered size passes a threshold, or every Codex session including single-rollout ones. | Threads (history or single-rollout) whose filtered size passes 16 MiB. Smaller sessions keep schema 2/3, so the migration and its bucket churn touch only the few long threads. |
| D2 | Whether to chunk the open tip, and the chunk target. | Yes, it is required for `87b47b56`, whose tip is 240 MB. Target 16 MiB filtered. |
| D3 | Segment list in the metadata sidecar, or in a small head object it references. | Head object, so sidecar size (read by listing) stays independent of segment count. |
| D4 | Derive metadata by streaming every segment per publication, or keep a private accumulator checkpoint. | Stream first; checkpoint only if profiles demand it. |
| D5 | Keep sealed segments locally (counted in the 1 GiB staging quota), or drop past a bound and fetch from the bucket when needed. | Keep locally up to 256 MiB per session, fetch with checksum verification beyond it. |
| D6 | Verify sealed raw prefixes by full streaming re-hash on each changed pass, or trust file identity and size for sealed rollouts with periodic full checks. Same question for remote readback of kept segments. | Full re-hash locally (it is O(1) memory and only on changed passes); HEAD-size plus checksum metadata for kept remote segments, full readback for new ones. |
| D7 | New ceilings: per-thread total filtered, segments per revision, total raw. | 1 GiB filtered, 256 segments, 4 GiB raw. |
| D8 | Release spacing: how long the reader ships before the writer is enabled, given older builds cannot read segmented sessions. | One release between PR 2 and PR 8, with the upgrade message in the old reader's refusal path already present. |
| D9 | Migrate schema-3 sessions by recapture, or by splitting retained objects. | Recapture (splitting yields no raw checkpoints). |

## Risks

- **Filter state across segments.** If equivalence fails for some record
  sequence (an identity note or start-completeness fact that depends on earlier
  records), the segment must carry the state forward explicitly in its private
  checkpoint; PR 3 decides this from tests, not assumption.
- **Codex producer changes.** The design depends on paginated rollouts being
  append-only with immutable prefixes. A Codex release that rewrites a prefix
  invalidates checkpoints and falls back to a full segmented re-read, which is
  correct but slow; the pinned producer references in the codex-history spec
  are rechecked in PR 3.
- **Old readers.** Between enablement and the owner upgrading every machine
  that reads the archive, segmented sessions are unreadable there.
- **Scope.** This touches capture, state, reader and lifecycle. The PR order
  keeps the writer off until the readers and lifecycle that understand
  segments have merged.
