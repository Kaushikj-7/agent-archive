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

- A pass reads, filters, holds and uploads only what is new since the last
  sealed checkpoint: normally the open tail of the tip, but after a pause or
  an offline machine possibly several new rollouts. Memory follows the
  segment size, not the thread size or the amount of new input.
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

- Every rollout's span is cut by one **chunking rule**: the collector seals a
  chunk at a newline-complete record boundary **before** adding a record that
  would take it past the chunk target (D2). A chunk therefore never exceeds
  the target, except a chunk holding one record that is larger than the
  target by itself, which is sealed alone. Such a lone record is bounded by
  `archive.MaxRecordBytes` (64 MiB) as today; at the ~3–4× working estimate a
  lone record above roughly 40 MiB filtered still meets the 160 MiB session
  ceiling and records the existing read-ceiling gap, exactly as one does in an
  unsegmented session today. PR 3 measures that bound. The rule applies to
  earlier rollouts as well as the tip, so a first (migration) read or a
  checkpoint-invalidating re-read of a large ancestor produces several chunks,
  never one oversized segment.
- An earlier rollout contributes the span its successor's `history_base`
  boundary includes, as one or more chunks. They are **sealed** when that
  boundary is proved: they never change again for this chain.
- The tip contributes zero or more **sealed chunks** and one **open tail**.
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
binding version 2: positive raw byte boundary plus SHA-256).

Progress must also be durable where nothing is retained: the filter can drop a
long run of raw records (large tool outputs) without producing a chunk. So the
collector also writes a **progress checkpoint** at a newline-aligned raw
boundary once 64 MiB of raw input has passed since the last checkpoint of either
kind. It has the same fields with no `segment_sha256`, plus the filter's
**complete cross-record accumulator** at that boundary, serialized as a
versioned, content-free private record: the gap set, omitted and denied
key-name sets, `NativeStartComplete`, the identity facts gathered by
`noteOutputNativeIdentity`, and any other state the filter carries between
records. PR 3 enumerates every such field in the filter and its test asserts
that a read resumed at each checkpoint equals an uninterrupted read in every
field, not only in records; state that cannot be serialized forces the
checkpoint back to the last boundary before it was set.
When the chunk in progress already holds retained records, the progress
checkpoint also stages them as a private **partial stage** (checksum-named,
durable, never uploaded) and records its checksum; the next pass reloads it and
keeps appending, and the partial stage is replaced when the chunk seals. A
checkpoint is valid only if its partial stage is present and matches, so a
resumed pass never skips raw records whose retained output was not saved; if the
partial stage is missing, the pass resumes from the previous checkpoint instead.
A pass that stops at the per-pass limit therefore always stops at a checkpoint,
even when the last 512 MiB produced no output.

A pass:

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
  `rollout_id`, `start_ordinal`, `end_ordinal_exclusive`, the record count
  and whether the segment is sealed. It carries no ownership boundary and no
  rollout header: both depend on which revision lists the segment, so they
  live on the head. Records carry exact ordinals as in schema 3. A segment
  never carries a `session_meta` record. Supplemental evidence
  (`session_labels`, skill evidence, child links) also lives only on the
  **head** object (below), so renames and label updates never rewrite a
  segment.
- **Segmented metadata** (metadata schema M). The sidecar's `source_bundle`
  reference names the **head**: a small source object holding the envelope
  (identity, capture, `native_child`, parent, linked sessions, supplemental
  evidence), the revision's `own_start_ordinal` when it has an explicit child or
  fork boundary, the filtered `session_meta` header of **every** rollout its
  list covers (each bound to its rollout ID and validated as today's active
  header is), and the ordered range list. Each list entry is `{rollout_id,
  start_ordinal, end_ordinal_exclusive, captured_at}` plus either a segment
  reference or, for a range whose records the filter dropped entirely, `empty:
  true` with no object. Empty ranges prove ordinal coverage the same way
  segments do, so a rollout that retains nothing still appears in the list and
  the list can end on the active rollout. Because each head carries the headers
  of its own rollouts, a preserved revision whose tip was an earlier rollout of
  the current chain still has that rollout's header to validate identity and
  native start from, while sharing its segments. Preserved revisions become
  `{revision_id, captured_at, kind, reference}`, where `kind` is required:
  `head` for a segmented revision, or `source` with its legacy source schema (2
  or 3) for a revision carried over from before migration. Heads list their own
  segments, so revisions that share a prefix share its segment objects. In
  schema M the active `source_bundle` always names a head; legacy sources appear
  only as preserved `source` entries. Segmentation is therefore **sticky**: once
  a session has any segmented revision it is always published in schema M, even
  when a rollback leaves its active chain under the D1 threshold.

All keys stay checksum-addressed under the session's prefix; a reader refuses
any reference outside it before reading.

The segment list lives only in the head (decision D3). Listing reads bounded
metadata bodies ([listing at scale](listing-at-scale.md)), so this keeps the
sidecar's size independent of the segment count. The cost is that the
sidecar alone no longer names every live object:

- `Metadata.SourceReferences()` in `internal/archive` keeps its signature (no
  context, no store) and returns only what the sidecar names: the active and
  preserved heads. It is renamed or documented as such, so no caller mistakes
  it for the complete set.
- A new bounded **head resolver** in `internal/reader` (reads belong there,
  construction in `internal/archive`) takes the sidecar and an object store,
  reads each `head` reference under a small fixed limit, validates it, and
  returns the complete live set: heads, every distinct segment they list, and
  every `source` reference (a legacy schema-2 or schema-3 revision) as a
  direct object, which it never decodes as a head. A reference whose kind is
  missing or contradicts its decoded header is invalid.
- Every lifecycle caller that protects or deletes objects moves to the
  resolver: catalog GC and migration (`internal/catalog`), retention and
  delete (`internal/retention`), purge (`internal/purge`), search
  (`internal/search`), the collector's fences, readback and authority
  recovery, and the state journal's reference checks. Callers that only hold
  a metadata summary (catalog GC today) must load the heads or fail closed.
- **Fail closed:** if any head of a live sidecar cannot be read or validated,
  GC and retention delete nothing under that session's prefix in that run.
  Whole-session delete and purge are unaffected, since they remove the prefix.

This refactor is part of PR 7 and must merge before PR 8 enables a writer.

### Collector flow

`sessionScan` keeps its shape; what changes is what each step holds.

- **read**: the Codex provider gains a segmented read that takes the private
  checkpoints and returns new sealed chunks plus the open tail as separate
  `FilteredTranscript`s. Each chunk is staged (compressed, checksum-named,
  durable) as soon as it is sealed, and its records are released before the
  next chunk is filtered. Peak work is one chunk plus the tail. Any number of
  previously unseen rollouts (Codex continued the thread several times while
  collection was paused or the machine was offline, up to the existing
  64-rollout chain depth) are read in chain order by the same loop. Each
  sealed chunk's raw checkpoint and stage are made durable before the next
  chunk is read, so a pass that reaches the per-pass unsealed-input limit
  (512 MiB) or its time budget stops at a sealed checkpoint, publishes
  nothing yet, and the next pass resumes from that checkpoint; the session is
  deferred, not gapped. Publication happens once the chain is covered up to
  its open tail. Checkpoints for staged but unpublished chunks are private
  and are discarded with their stages if the chain they assumed changes.
  Stages not yet published are never dropped (they cannot be fetched from
  the bucket); see local state below for how they are bounded.
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

- Segment boundaries are a refinement of today's spans: every span boundary is a
  segment boundary; chunk boundaries add more. Validation is the existing
  `SourceHistory.Validate` rule applied to the concatenated range list (segments
  and empty ranges alike): contiguous ordinals across entries, `start` of each
  segment equals `end` of the previous one within a rollout, and each rollout's
  first segment starts where its base's included prefix ended. Gaps,
  regressions, duplicate segment identities and a list that does not end on the
  active rollout refuse.
- `own_start_ordinal` keeps its meaning and is recorded once per revision, on
  its head, never on a segment (a segment can be shared by revisions with
  different boundaries). `OwnRecord` compares each record's physical ordinal
  with the head's boundary, so every record of a segment wholly before it is
  inherited context, and a segment straddling it is split by ordinal. An
  absent child boundary still excludes nothing; validation refuses a head
  whose boundary lies outside its list's ordinal range.
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
- Record-level filter gaps are the union across segments, deduplicated by
  code and detail as `addGap` does now. The key-name gaps
  (`unknown_field_omitted`, `sensitive_or_hidden_field_omitted`) are not
  per-segment strings: `addOutputKeyGaps` joins every omitted or denied key
  name into one detail for the whole transcript. Segments therefore record
  their omitted and denied key-name sets as structured, content-free
  evidence, and the head's derivation unions the sets across its segments
  and emits one canonical gap of each kind, matching an assembled read. The
  PR 3 equivalence test compares the assembled gap list, including a thread
  whose segments omit different keys.

Each publication streams every segment again (decision D4): memory bounded
by one segment, CPU proportional to F, rate limited by the upload interval.
The alternative, resuming from a private serialized accumulator checkpoint at
the last sealed segment, was set aside: streaming needs no new persisted
state, and an equivalence test (iterator over the segment list == `ParseCodex`
over the assembled bundle) proves it. A checkpoint is a follow-up
only if profiles show derivation CPU matters.

Reading one segment at a time bounds *input*, not *output*. Today
`nativecodec.ParseCodex` appends every visible turn, tool-call candidate,
tool result and skill use to `Analysis.View`, and `BuildMetadataWithAnalysis`
consumes that whole view, so an iterator that returned the same `Analysis`
would still hold O(thread) data. Segmented derivation therefore uses an
**aggregate-only** path: each record updates bounded metadata accumulators
(counts, per-model tokens, first and last timestamps, title candidate, gaps)
and its normalized per-record data is discarded before the next record.

Some metadata values are exact over the whole thread: the top tools need a
count for every tool name (one may only become frequent later), and
`Counts.FilesTouched` needs the exact distinct-path set. Those accumulators
stay **exact and uncapped in count**, but their memory follows the number of
distinct names and paths, not the number of records, and it is charged to the
session's read ledger. A thread whose distinct keys exceed a fixed bound
(proposed 8 MiB of keys, checked in PR 4) records a size-limit gap rather than
publishing changed values; no derived value is silently truncated, so the
parser version only changes if PR 4's equivalence tests find a real
difference. The equivalence test compares the resulting metadata, not
`Analysis.View`, and the benchmark asserts heap grows with distinct keys, not
records.

Assistant message counting deduplicates streamed responses by
`message.id` (`summarizeTurns` in `internal/archive/metadata.go` keeps every
ID it has seen), and that set grows with the number of responses, not with
distinct names. PR 4 first establishes from the pinned producer and tests
whether Codex's records sharing an ID are always contiguous. If they are, the
accumulator compares only with the previous ID (constant memory), proved
equal by the equivalence test. If not, the ID set is kept exactly in the
private spool (an on-disk sorted, de-duplicated run merged at the end), so
memory stays bounded without changing the count. PR 4's benchmark includes a
thread with one million distinct response IDs.

Tool-call correlation has the same shape. `dedupeToolCalls` keeps every
invocation ID to suppress later completion echoes, and `linkToolResults`
maps call IDs to results for tool errors and git activity. PR 4 establishes
from the pinned producer and tests whether a call's echoes and results
always arrive before its turn ends. If they do, correlation state holds only
the calls open in the current turn and is cleared at each turn boundary,
proved equal by the equivalence test. If not, calls and results are written to
the spool keyed by ID and joined by an on-disk sort-merge at the end, exactly.
Either way derived values do not change, so no parser bump is needed; if PR 4
finds that neither holds, the change in derived values is treated as a
parser version change under versions.md, not shipped silently. The
benchmark adds a thread with one million unique tool-call IDs.

Consumers that need a whole per-record view use a **spooled view** instead of
memory: segments are streamed through the existing normalization into a
private file (mode 0600, record per line, never uploaded) in a managed
`spool/` directory under the agent-archive home, named by session and owning
process. The command removes it on exit; because a killed process or power loss
skips that, every command sweeps `spool/` on entry and removes files whose
owning process no longer exists or that are older than a day, and session delete
and purge also remove any spool for that session. The consumer renders from the
spool with its own existing size rules. Memory is one segment plus one rendered
record; disk is the normalized view's size.

### Reading: show, handoff, search, verification, eval export

- `internal/reader` gains a segment iterator: open the head, then each segment
  in order, each decoded under today's per-object `Limits` and checked against
  its list entry (thread, rollout, ordinals, count, checksum) before any record
  is yielded. A malformed reference cannot widen reads. Because a later
  segment can still fail after earlier records were yielded, no display path
  writes to the user from the iterator directly: every display path (show
  text, `show --json`/`--normalized`, the browser's `t` action in
  `cli/list_browse.go`, eval export) first builds the spooled view, and
  shows nothing unless the iterator reached a successful end. A failure
  reports the error and shows no partial transcript, as `LoadSource` does
  today.
- `LoadSource` keeps returning one assembled `SourceBundle` for callers that
  need it, but only when the assembled size fits the caller's limits; past
  that it returns a named error telling the caller to use the iterator.
- `cli/show_transcript.go` renders text from the validated spool. With the
  default 120,000-byte limit or any nonzero `--max-bytes`, `FitTranscript`
  (`internal/archive/transcript_fit.go`) needs the whole session to protect
  the newest exchanges, choose the fewest oldest-first elisions and save the
  untrimmed copy when it trims. PR 2 gives it a spool-backed form: an index
  of per-exchange rendered sizes built in one pass, the same prefix search
  over that index, then a second pass that writes the fitted output (and the
  untrimmed copy) from the spool. Memory is the index plus one exchange.
  Only the no-limit mode writes the spool straight through. `show
  --transcript --json` and `--normalized` render from the same spool, keeping
  their current trimming and their no-limit mode.
- The browser's `t` action builds the normalized transcript into a
  `bytes.Buffer` today; it renders from the spool into a file the pager
  reads instead, so it no longer holds the whole thread. The summary-only
  `show SESSION_ID --revision REVISION_ID` (`loadShowSummarySelection` →
  `loadArchiveSelection` in `cli/revisions.go`) re-derives a revision's
  summary; for a segmented revision it uses the aggregate-only derivation over
  the iterator instead of assembling the source.
- `cli/picker_handoff.go` keeps the `archive.FitHandoff` contract: no prompt
  is ever dropped, and the first prompt keeps its body up to its cap. The
  output bound is not a source-read cutoff. Handoff streams every segment in
  order into a bounded summary state that keeps every prompt (each capped as
  `FitHandoff` caps it), one condensed line per older exchange, and full
  detail only for a trailing window of recent exchanges; detail leaving the
  window is condensed as it leaves. Memory is one segment plus that state.
  When even the prompts and one-line exchanges exceed the handoff bound,
  handoff reports it does not fit, as `FitHandoff` does today.
- `internal/search/projection.go` caches an **ownership-neutral** extraction
  of each sealed segment once, keyed by its checksum, the segment source
  schema, the parser version and the search `ExtractorVersion` (the same
  interpretation versions today's search source identity carries, so an
  upgrade re-extracts unchanged bytes), and applies the listing
  head's `own_start_ordinal` (and rollout headers) when assembling the
  session's projection. A segment shared by heads with different ownership
  boundaries is therefore indexed correctly for each; only the tail and the
  assembly are redone per publication.
- Publication verification (`verifyPublication` in `cli/verification.go`)
  validates every new head and the source-set digest each time, but reads
  each immutable segment only once: a local verification receipt keyed by
  segment checksum and compressed length records that the remote object
  decoded and validated. Later verifications read only objects without a
  receipt, so an append costs one segment, not the whole set (re-reading a
  set near 1 GiB within its five-minute timeout would fail on slow links and
  block privacy cleanup that waits on verification). A segment listed by
  several heads is read at most once per verification. A receipt's key is
  the segment checksum, compressed length, segment source schema and a
  verification-algorithm version that any change to segment decoding or
  validation bumps. A receipt from an older key is ignored, so a later
  release re-reads and re-validates each object once under its own checks
  before verification can authorize privacy cleanup. Receipts are also
  invalidated when the session's privacy policy or the destination changes.
- Eval export (`exportArchivedSession` in `cli/eval_export.go`, through
  `loadArchiveSelection`, including `--revision`) renders
  `EvalExportDetailFull` from the spooled view instead of calling
  `LoadSource`/`LoadRevision`. Nothing is capped beyond what export already
  does: `--max-bytes` trims as today, and `--max-bytes 0` keeps its documented
  no-limit meaning, streamed from the spool.

### Size limits

| Limit | Today | Proposed |
| --- | --- | --- |
| Filtered size | 64 MiB per thread (`DefaultMaxTranscriptBytes`) | Chunk target 16 MiB (D2), sealed before a record would pass it; only a lone record larger than the target exceeds it (bounded by the 64 MiB record limit). A new per-thread total filtered ceiling 1 GiB (D7). |
| Segment count | 64 spans per thread | 256 per revision (D7) and 1024 distinct objects per session. |
| Retained records | 1,000,000 per thread (`archive.MaxHistoryRecords`) | Replaced in schema M: 1,000,000 per segment, 16,000,000 per revision, with a `record_count` gap. |
| Raw size | 512 MiB per thread read (`MaxCodexRawTranscriptBytes`) | 512 MiB of *unsealed* bytes filtered in one pass; a separate total raw ceiling for the checkpoint hash 4 GiB (D7). |
| Read ceiling | 160 MiB per session, ~3–4×F | Unchanged constant; work is ~3–4× one chunk, about 64 MiB at the 16 MiB target. A lone record over ~40 MiB filtered still meets it, as today. |
| Record | 64 MiB | Unchanged. |
| Chain depth | 64 rollouts | Unchanged. |

A thread over a byte ceiling (total filtered, total raw, per-segment work)
records the existing size-limit gap kinds with the new `LimitBytes`, so
`readCeilingChanged`-style signature checks read it again when a build
changes the limit. The count ceilings (segments per revision, distinct
objects per session, distinct derivation keys, retained records) are not
bytes: PR 5 adds count-aware gap kinds to `state.SizeLimitGap`
(`segment_count`, `object_count`, `derivation_keys`, `record_count`) carrying
`{limit, count}`, words for
`SizeLimitLabel` and the status problem, and includes each count ceiling in
the signature check, so changing one reads the session again. The gap's
capture is the last fully published segment set.

### Local state

- **Raw checkpoints** live with the session's private native binding (D6
  covers when they are trusted); a version bump of that record refuses older
  writers, as Muse's binding v2 does.
- **Sealed segment stages** are kept as owned, checksum-named stages so that
  privacy maintenance, parser refresh and readback recovery never need native
  input. They count against the shared 1 GiB staging/publication quota
  (`state.durableStorageQuota`). Past 256 MiB per session (decision D5) the
  oldest **published** stages are dropped and fetched from the bucket with
  checksum verification when needed. Stages not yet published (a first
  migration read or a checkpoint-invalidating re-read in progress) are never
  dropped. One session's unpublished stages may hold at most 512 MiB of the
  1 GiB quota, compressed; the 1 GiB filtered thread ceiling normally
  compresses well below that. A session that would need more records a
  size-limit gap of a new `unpublished_stage_bytes` kind (bytes, so a changed
  limit reads it again) rather than dropping work or starving other
  sessions' staging. Recording that gap (or any other terminal gap during an
  unpublished read) releases the session's unpublished stages, partial
  stages and their checkpoints, so a gapped session holds no quota; if a
  later build changes the limit, the session is read again from its last
  published state. No segment is uploaded before the chain is covered:
  the existing rule that preparation makes no remote write until every input
  is staged is kept.
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
- GC protects every object the head resolver returns for the live sidecar
  (failing closed when a head cannot be read) and every pending journal's
  stages, and rechecks metadata before deleting a
  source, as today. A sealed segment shared by several revisions is deleted
  only when no live head lists it.
- Undo and import batch ownership stay whole-session.

### Reader-before-writer rollout and versioning

1. **Reader release.** PR 2 ships readers for the segment source and segmented
   metadata in a release before any writer exists. An upgrade message cannot
   be added to builds already released. Today's builds fail metadata
   validation with `unsupported metadata schema version N`, and exhaustive
   listing skips invalid or newer sidecars. So on a build from before PR 2 a
   segmented session is **hidden**: absent from `list`, and `show` or
   `handoff` by ID reports no matching session or the generic schema error.
   The user docs and CHANGELOG for PR 8 say so plainly; nothing in this
   design relies on old builds explaining it.
2. **Destination reader gate.** Waiting a release does not prove every
   machine reading the bucket has upgraded. So the writer is not enabled by
   release alone: PR 8 adds an explicit per-destination opt-in. Before turning
   it on, the CLI reads the destination's machine records
   (`machines/<machine_id>.json`). PR 2 adds an optional `reader_capabilities`
   list to the record, which builds from PR 2 on write with
   `codex-segments-v1`. The CLI names every paired machine whose record lacks
   that capability, including source builds (`dev-<commit>`) and any version
   it cannot order, so an unknown reader is always treated as unable to read
   segmented sessions, never assumed able. The owner confirms knowing those
   machines will hide segmented sessions until upgraded, or upgrades them first.
   A machine with no record (an unpaired reader using shared keys) cannot be
   detected, which the prompt says. The opt-in is recorded in configuration
   under the writer capability below, never inferred. The gate is not
   one-time: once per pass, before any segmented publication, the collector
   re-reads the destination's machine records (one listing of `machines/`,
   skipped when unchanged since the last check). A newly seen machine
   without `codex-segments-v1` defers segmented publications (not gaps; the
   last published snapshots stay), and `status` names that machine and says
   to upgrade it or confirm the opt-in again. Pairing a machine from a
   current build warns the same way.
3. **Writer capability.** The writer is enabled behind a new permanent
   configuration writer capability (the next unreserved configuration version
   in `internal/config/wire.go`, coordinated with open reservations), so an
   older collector on the same configuration refuses to write rather than
   republishing a whole-chain schema-3 source over segmented metadata. Older
   collectors that read remote segmented metadata already refuse unknown
   schemas.
4. **Versions.** The writer PR bumps the Codex adapter and filter versions (the
   uploaded representation changes), the source and metadata schema versions,
   and the Codex parser version only if derived values change (equivalence
   says they should not). `schemas/` and versions.md change in the same PRs,
   along with a privacy changelog entry stating that what is retained per
   record is unchanged.

### Migration of existing history sources

A schema-3 session, and a single-rollout schema-2 session that D1 selects, is
migrated by its next capture, not by rewriting retained bytes: the first
segmented pass has no checkpoints, so it reads the chain once from ordinal 0,
sealing and staging chunks as it goes (memory one chunk), and publishes the
segment set as the same current revision.

Migration is a representation change, so it must not make an old session look
newly active and postpone its deletion. Every migrated segment whose records the
prior publication already covered (same rollout, ordinals below the prior active
span's end) takes the prior publication's `captured_at`, and the head takes it
too, unless the pass also found genuinely new records. Only segments holding
records past the prior coverage get the pass time. A schema-2 prior has no raw
ordinals, so its coverage is established with the existing legacy check
(`ownedRevisionProjection`: its ordered retained records must be a prefix of the
same physical rollout's new records) and covers the segments holding those
records; a schema-2 prior that is not such a prefix stays pending as a rewrite,
as today. If the old object is unreadable, so that coverage cannot be
established, the session stays pending rather than guessing a time. Carried-over
preserved references keep their own `captured_at` and are written with `kind:
source`. The old schema-2 or schema-3 active object becomes a retired reference
after the new set's readback. Preserved schema-3 revisions stay as preserved
references; readers keep reading them (prior schema-2 and schema-3 references
are already permitted in metadata schema 2, and remain so in M).

Splitting retained schema-3 objects into segments without a native read (D9)
would keep capture times but produce no raw checkpoints, so the next pass would
read the whole chain anyway; recapture is simpler and gives the same result.

A session in a `SizeLimitReadBudget` or `SizeLimitFiltered` gap (like
`87b47b56`) must be read again once the writer is enabled, but nothing in its
recorded gap changes then: the read ceiling stays 160 MiB, and a build
installed with the opt-in still off may already have refreshed its signature
to the new versions while recording the same gap. So the scan signature
records whether segmentation was enabled for the session's destination when
it was taken, and unchanged-source skipping (`unchangedSinceLastScan`) treats
a signature taken with segmentation off as stale once it is on. Enabling the
opt-in additionally requeues every Codex session currently in a size-limit
gap, so the change takes effect on the next pass rather than on the next
native change.

## PR breakdown

Each PR has one concern and is merged in order unless marked parallel.

1. **Spec and schemas.** `dev/specs/codex-segments.md`, `schemas/` for the
   segment source, head and segmented metadata; allocate the schema numbers in
   versions.md; schema tests. No code paths.
2. **Archive types and reader.** `internal/archive` segment types and
   validators; `internal/reader` segment iterator, bounded `LoadSource`
   assembly; the head resolver's legacy `source` branch; search
   (ownership-neutral segment cache keyed with interpretation versions)
   consumes the iterator; show text (spool-backed `FitTranscript`) and the
   browser's `t` action render from the validated spool;
   verification with checksum-keyed segment receipts; bounded handoff summary
   state that keeps every prompt; the spooled view for `show --json` /
   `--normalized` and eval export (including `--revision` and
   `--max-bytes 0`).
   Synthetic fixtures only. Ships in a release before PR 8 enables a writer.
3. **Filter equivalence and segmented provider read** (parallel with 2).
   Codex provider read from raw checkpoints; chunk sealing; tests and a fuzz
   target proving that concatenated segment filtering equals whole-chain
   filtering (records, ordinals, gaps union) for the same input, including
   child/fork boundaries, rollbacks and records straddling a chunk target.
4. **Streaming Codex parse** (parallel with 2–3). Aggregate-only,
   record-iterator derivation that keeps no per-record view, with exact
   distinct-key accumulators for top tools and files touched; metadata
   equivalence tests against the assembled-bundle parse (including threads
   with many distinct tools and paths), and a benchmark on 10k/100k/1M-record
   synthetic threads asserting heap grows with distinct keys, not records.
5. **Local state.** Raw checkpoints in the native binding, sealed segment
   stages and quota accounting, the new `PendingHistory` journal version and
   `Published` cache shape, count-aware size-limit gap kinds and their
   signature invalidation, the segmentation-enabled bit in scan signatures,
   resumable pre-publication checkpoints across passes, with crash-recovery
   tests for every checkpoint boundary.
6. **Collector writer.** Segmented capture path in `sessionScan`, reconciliation
   as segment lists, per-segment size limits and gaps, pending/readback; behind
   the new configuration capability, off until PR 8.
7. **Lifecycle.** The `internal/reader` head resolver and the move of every
   protecting or deleting caller to it (catalog GC and migration, retention,
   purge, search, collector fences/readback, state reference checks), with
   fail-closed tests for an unreadable head. Privacy maintenance, retention,
   delete, purge and GC over segment sets, with tests that a shared segment
   survives the deletion of one revision. Must merge before PR 8.
8. **Enable and migrate.** The per-destination opt-in, its machine-version check
   and the requeue of Codex sessions in size-limit gaps; turn the writer on for
   threads selected by D1; migration of schema-3 and selected schema-2 sessions
   by recapture; adapter/filter/parser bumps, CHANGELOG, privacy changelog, user
   docs (`docs/` size-limit and status wording), versions.md.

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
  an off-chain tail preserved as a revision sharing sealed segments, validated
  from its own head's rollout header; a child boundary after several whole
  segments, proving none of their prompts, tools, usage, titles or timestamps
  count as the child's own.
- A first read of a chain whose earlier rollout filters to several times the
  chunk target produces only segments within the per-segment ceiling.
- Crash at each journal step; stricter privacy re-filter of a 32-segment
  thread with one segment in memory; delete/purge/GC of shared segments.
- Migration of an unchanged schema-3 session, and of a single-rollout
  schema-2 session over 16 MiB, keeps its prior `captured_at` on every
  segment and the head, so retention age is unchanged; a session with
  preserved schema-3 revisions resolves them as `source` references and GC
  stays able to run.
- A segment shared by heads with different ownership boundaries is searchable
  as each head's own records only.
- Five unseen rollouts totalling more than 512 MiB of new input are captured
  over several passes, each resuming from the last sealed checkpoint, with no
  gap recorded.
- A session gapped by a build with the opt-in off is read on the first pass
  after the opt-in is turned on, with no native change.
- More than 512 MiB of raw input that the filter drops entirely advances by
  progress checkpoints across passes and reaches publication, with the same
  gap set as one uninterrupted read.
- A segment that fails validation after earlier segments decoded shows no
  partial transcript in show, browse or eval export.
- `show` with the default limit on a 1 GiB synthetic thread produces the same
  fitted output and untrimmed copy as `FitTranscript` over the assembled
  bundle, with bounded memory.
- A rollout whose records are all dropped publishes as an empty range.
- A pass stopped at the per-pass limit with 8 MiB of retained output in an
  unsealed chunk resumes from its partial stage and publishes every record.
- A `show --json` process killed mid-spool leaves a file the next command
  removes; purge removes a session's spool.
- A session gapped for unpublished stage bytes holds no staging quota.
- A newly paired machine without the reader capability defers segmented
  publication and is named by `status`.
- A rollback that leaves a segmented thread's active chain under 16 MiB
  still publishes schema M with the old revision preserved as a head.
- A paired machine whose record lacks `codex-segments-v1` (including a
  `dev-<commit>` build) is named by the opt-in check.
- A receipt written under an older verification-algorithm version does not
  skip re-validation.

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
| D1 | Which sessions use segments: every Codex history thread, only threads whose filtered size passes a threshold, or every Codex session including single-rollout ones. | Threads (history or single-rollout) whose filtered size passes 16 MiB. Smaller sessions keep schema 2/3, so the migration and its bucket churn touch only the few long threads. The threshold decides only entry: once a session has any segmented revision, active or preserved, it stays segmented, so a rollback to a short chain is still published as a head (there is no sidecar shape with a legacy active source and a preserved head). |
| D2 | Whether to chunk the open tip, and the chunk target. | Yes, it is required for `87b47b56`, whose tip is 240 MB. Target 16 MiB filtered. |
| D3 | Segment list in the metadata sidecar, or in a small head object it references. | Head object, so sidecar size (read by listing) stays independent of segment count. |
| D4 | Derive metadata by streaming every segment per publication, or keep a private accumulator checkpoint. | Stream first; checkpoint only if profiles demand it. |
| D5 | Keep sealed segments locally (counted in the 1 GiB staging quota), or drop past a bound and fetch from the bucket when needed. | Keep locally up to 256 MiB per session, fetch with checksum verification beyond it. |
| D6 | Verify sealed raw prefixes by full streaming re-hash on each changed pass, or trust file identity and size for sealed rollouts with periodic full checks. Same question for remote readback of kept segments. | Full re-hash locally (it is O(1) memory and only on changed passes); HEAD-size plus checksum metadata for kept remote segments, full readback for new ones. |
| D7 | New ceilings: per-thread total filtered, segments per revision, total raw. | 1 GiB filtered, 256 segments, 4 GiB raw. |
| D8 | Release spacing: how long the reader ships before the writer is enabled, given older builds cannot read segmented sessions. | One release between PR 2 and PR 8. Review revised this: since released builds cannot gain an upgrade message, PR 8 also needs the per-destination opt-in that names paired machines on pre-reader builds. |
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
- **Old readers.** Builds from before PR 2 hide segmented sessions (they skip
  newer sidecars rather than explain them). The destination opt-in names the
  paired machines still on such builds, but cannot see unpaired readers.
- **Scope.** This touches capture, state, reader and lifecycle. The PR order
  keeps the writer off until the readers and lifecycle that understand
  segments have merged.
