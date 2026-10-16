# Codex session names

Collection sets and refreshes `name` for already admitted Codex sessions.
`title` remains the first human prompt fallback. Listing reads archived metadata
and does not probe native storage or launch Codex.

Names come from Codex's own name index, `$CODEX_HOME/session_index.jsonl`
(`~/.codex/session_index.jsonl` by default). Codex appends a record
`{id, thread_name, updated_at}` whenever a thread is named or renamed, and the
last nonempty name for a thread is current; this is how Codex resolves names
itself. The index is a plain append-only file, so it is read while Codex runs,
and its format is the same across Codex releases, so no release is pinned.
Codex's `state_5.sqlite` is not opened: Codex keeps it in WAL mode while it
runs, which made the earlier 0.159.2-only database resolver name no current
thread.

Only an admitted top-level thread (discovered or imported) whose rollout lies
in exactly one approved Codex home is looked up; subagent threads keep the
name their parent gave the task, and frozen captures are not looked up. A
complete read of the index without the thread is confirmed absence, and the
title falls back to the first prompt. An index that cannot be read whole (an
unfinished last record, more than 16 MiB or 262,144 lines) proves nothing, and
the last verified name stays; a single record over 16 KiB is skipped.

A thread that continues into a later rollout (a history source set) keeps its
name: the history source retains the same name evidence an ordinary source
does. A rename of a history source set is published with its next capture,
which reads the thread again and counts as activity: its capture time, and so
its retention, moves forward. While its transcript is missing the rename waits.
An ordinary source's rename is published from its retained source without
reading the transcript, keeping its activity, capture time and retention.

Unavailable observations preserve the last verified name. Confirmed absence
clears it; an index-only miss cannot clear a previous canonical database name.
Names whose normalized text cannot remain stable through privacy filtering
are also unavailable and preserve the last verified name.
Rename publication uses retained source evidence when the native transcript is
unchanged or missing, preserving original activity, capture time and retention.
Attempted publication retries keep their frozen bytes; a newer rename is owed
after the retry succeeds.

Each pass budgets 64 admitted targets, 16 configured homes and two seconds.
Index scans are limited to 16 MiB and 262,144 lines, one read per home. Homes are visited in persistent target order. A separate durable priority
cursor skips the previous first work group across ID-order wraparound, so
interleaved IDs in a slow home cannot monopolize later homes’ priority. A bounded
per-group archive-ID cursor also rotates the first target within each prioritized
group, preserving deferred-ID opportunity after lookup backoff expires. Group
identities are hashed by the collector before persistence; removed targets
retire their cursor, while temporary backoff preserves progress. The target cursor advances
over deferred targets, with bounded backoff for unavailable observations.
Lookup context is content-free: the thread ID from the registration, keyed to
the retained source checksum and codec contract, so no retained publication is
decoded to ask. Unchanged lookup reads only a bounded leading checksum and stat. These limits defer work without delaying normal capture.
Serialized and decoded context also share the pass's data budget; a borrowed
publication remains charged until its pass scope ends.
Rename integrity verification and candidate encoding reserve compression scratch
and output against the same ledger; verification output is released before the
renamed candidate is rendered. Shared pressure preserves the last acknowledged
source and deferred name evidence for a later pass.
Aggregate exhaustion is deferred to another pass. A retained publication over
the 8 MiB rename-only decode is published by the session's own scan instead,
within its read ceiling. Its prior name and pending transaction
remain intact. Real native content changes still use normal capture limits
and may publish the latest resolved label alongside the new conversation.

A name is published only by a capture or a rename refresh, each of which
checks that retained source and metadata agree on the admitted session, native
identity and current machine; a name-only refresh from retained source applies
to ordinary sources only.
The private cache is machine-scoped and rechecks a content-free state token even
during lookup backoff; stale proof cannot trigger a name publication.

Context cache entries store only IDs, filtered evidence, producer/contract,
hashes, timestamps and scheduling counters. A name-only change publishes a
new filtered source and metadata through normal durable pending publication,
including the listing revision. Capture permission, frozen generations and
retention policy continue to govern whether retained evidence can be used.
The lookup cache holds at most 2,048 entries and 4 MiB of compact JSON. Eviction
does not remove a name already retained in published source evidence.

The shared collector, archive and private cache treat provider identity as an
opaque bounded value. Codex interprets its native UUID, producer and file
contract within its integration. A hash of provider interpretation, filter and
per-agent parser versions invalidates private lookup context. Earlier retained
file-contract evidence remains readable offline; it does not authorize a new
native lookup under the current contract.

The [deferred native comparison protocol](../../dev/maintainers/native-session-name-comparisons.md) tracks sidebar/list/show parity, restart, coexistence and history coverage with an explicitly pending evidence template. Lookup probes alone do not complete that acceptance.

## Explicit native opt-in

To enable native lookup, pause collection, back up the archive data directory's
`config.json`, and add `"codex_name_lookup": "native"` to that JSON object,
preserving all other fields and authorization history. Resume collection after
checking the JSON. Set `"files"` or remove the optional field to return to files.
Unknown values are rejected. Configuration schema remains unchanged; older
writers may drop this optional preference and safely return to files.

Native mode starts `codex app-server` through PATH only during eligible collector
work. Listing, showing, setup and status do not launch it. One lazy stdio process
per approved home is reused for a pass and closed afterwards. The only messages
are initialize, initialized and thread/read with includeTurns false; no thread
start/resume, turns, rename, tools or inventory requests are sent. The returned
home, native ID and pinned 0.159.2 protocol/retained producer are checked. Missing
optional names and unknown versions are unavailable; compatible null is explicit
absence. Known current environment or local configuration overrides and related
histories are refused. Managed/system configuration and historical SQLite
placement remain unverified: the child's `CODEX_SQLITE_HOME` selects the home
only as a fallback, and Codex's merged `sqlite_home` setting can override it.
Startup may therefore touch configured native storage beyond that fallback.

Opt-in accepts native startup side effects: disposable 0.159.2 probes observed
database/state and system-skill writes even with metadata-only requests. One
initialization exceeded eight seconds. Two synthetic servers coexisted in an
earlier probe; coexistence with the desktop app, real credential-store behavior,
live sidebar parity and realistic archive-backed native homes remain unverified.
This mode does not promise a read-only native host. It respects user native auth
settings through HOME/CODEX_HOME, but does not forward archive credentials, AWS
variables or shell API keys, and never executes login or approval requests.

Native work is serial, capped at 64 IDs and two seconds across the pass; startup
and initialization share that total budget, each read has at most 250 ms.
Responses are limited to 256 KiB per line and 1 MiB across hosts; stderr is
discarded with a 16 KiB cap. Cancellation closes pipes, kills and reaps with a
250 ms grace. Unavailable API reads may use the guarded file provider; file
absence cannot clear previously retained API evidence. Cache backoff and cursor
fairness remain the same as files mode. One measured warm lookup using installed
0.159.2, the production host and an invented owning thread in a disposable home returned the expected name in
598 ms; actual process reap and both reader exits passed. Cold instrumentation
failed before a lookup measurement. This single warm observation does not
establish a startup latency distribution or acceptance of the unresolved live
cases above; the runtime budgets remain enforced independently of it.
