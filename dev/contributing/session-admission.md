# Session admission

How a registration's times are used, for contributors. The rules users see
are in [session eligibility](../../docs/reference/session-eligibility.md).

`agent-archive backfill` registers sessions the hooks never saw, after the
person confirms a plan ([backfill design](../specs/backfill.md)). An
imported registration has `origin: import` and two times:

- `session_started_at` is the true start, from the transcript
  (`started_at_source: transcript`) or, for a Cursor file with no timestamps,
  the file's birth time (`file_created`).
- `admitted_at` is when backfill took ownership. A hook registration sets it
  to its own start.

Every boundary check uses `archive.SessionRegistration.Admitted()`
(`internal/archive/types.go`), which is `admitted_at`, or
`session_started_at` for a registration older than that field.
`AcceptSession` and `InCurrentDestination` are methods of `config.Config`
(`internal/config/config.go`):

| Check | Uses |
|---|---|
| Project activation in `AcceptSession` | `Admitted()` |
| Storage destination in `AcceptSession`, and retention's current-bucket check (`InCurrentDestination`) | The registration's `destination_id`, set at registration by hooks and backfill; `Admitted()` against `DestinationSince` for a registration without one |
| Retention age before a first capture | `Admitted()` |
| App selection | `Harnesses`, plus `ImportedHarnesses` for imports |
| Fresh-start eligibility for hooks, metadata `started_at`, subagent ordering, handoff | `session_started_at` |
| App hook observation | Durable `HookObservedAt` from an actual supported hook; independent of original registration origin |
| Discovery fresh-start permission | Native original creation within the current agent/project/destination authorization generation and its unpaused intervals |

Two guard tests in `internal/archive/admission_guard_test.go` read the
source. `TestNoBoundaryComparesSessionStartedAtOutsideAdmitted` fails if code
compares `session_started_at` with `ActivatedAt` or `DestinationSince`
outside `Admitted()`, and
`TestNoDestinationTimeComparisonOutsideInCurrentDestination` fails if code
compares an admission with `DestinationSince` outside
`InCurrentDestination`.

A hook that later resumes an imported session continues it: the
registration keeps its start, admission, destination ID, and origin.
Retention and undo leave a removal record when they forget a session, so
backfill does not import it again (unless `--include-removed`).

## Shared discovery foundations

Automatic discovery is not activated by these foundations. `sourcefacts` supplies shared project and worktree facts; those facts never
authorize capture. Strict source readers belong with the scanner integration.
`config.ReconcileDiscovery` records forward-only permission generations and
`DiscoveryGeneration` checks native creation against half-open intervals.

`state.RegisterOrMerge` uses the existing qualified SessionKey reservation and
request-lock protocol, preserves compatible original provenance, and rejects
removed automatic candidates. Explicit imports retain their removal override.
The older low-level `RegisterNewSession` remains a replacement primitive for
existing controlled callers; new admission paths must use register-or-merge.
A real hook records `HookObservedAt` without relabeling imports or discovery.
Generic hook locators cannot replace a discovery-owned confined source.

Protected authorization uses a schema-version object that published integer
decoders refuse. Legacy hook-only config remains numeric. Setup integration must
fence protected Before snapshots before journaling a reconfiguration; never
recover a numeric snapshot over protected authorization.

## Codex native identity and related history

The pure `codexmeta` decoder is shared by bounded source facts, native headers
and live capture filtering. `payload.id` is the stable thread ID;
`session_id` is its root conversation ID, and `parent_thread_id` is its
immediate parent. A grandchild's root and parent differ. A fork has independent
thread ownership: `forked_from_id` is logical ancestry, not child ownership.
A rollout filename identifies a physical history segment and may differ from
the stable thread after revert. `history_base.thread_id` names that physical
prefix rollout, with exclusive ordinal and byte bounds. See the pinned
[upstream protocol](https://github.com/openai/codex/blob/01fc69f4026735edfdf6789820549727a4867b11/codex-rs/protocol/src/protocol.rs).

`CodexMeta.Identity(path)` returns bounded `CodexIdentity` facts, including
optional ordinal pointers that distinguish absent and zero. It validates known
shapes and direct contradictions; dependency resolution, cycles across files,
active-rollout selection and capture authorization remain separate. Missing
older optional fields and unknown additive fields remain readable.

Children, forks and revised histories currently report explicit pending
capture outcomes. Backfill skips these files and all observed sibling segments
of the same thread rather than selecting an incomplete view. Live source
filtering also refuses related metadata before privacy filtering can omit it.
Existing retained ordinary sources still use the privacy-only refilter. Source
confinement, original creation consent and first native task checks remain
required; none of these metadata facts grant admission.

A snapshot that changes while backfill reads it reports `source_changed`, with
an instruction to retry. It does not claim the JSON format is unsafe or stop
other candidates, and a later settled scan can admit the session.

## Scheduled identity recovery

Recovery retains the qualified identity census as its authority. Each scheduled
slice validates the complete registration inventory and all duplicate owners,
then resumes derived-index application from a content-free cursor tied to that
inventory, the request generation, and the membership revision. A partial,
interrupted or damaged cursor never certifies absence. Candidate reservations
and requested misses must also finish before the complete marker is written.
Repeated requests for an already retained exact key preserve progress, including
an applied absence that is not yet certified; readers still require the complete
marker before accepting that absence. A new request changes the recovery
generation. After a fresh complete census proves equivalent membership and
inventory, owner/candidate application can retain its earlier progress across
that change; candidate facts must also match their complete phase fingerprint.
Requested-miss coverage restarts so the new generation cannot skip a new key.
Requested keys that name previously applied owners or candidates are repaired
from the complete registration/candidate authority during that final phase;
retained application offsets cannot turn an owned key into certified absence
or strand a request for a damaged index. The candidate snapshot is loaded once
per slice reaching the final phase, rather than reread for each requested key.

Discovery's request for a census-certified absence is not damage. Under a
complete version-1 marker, `RequestSessionIndexAbsence` writes a fresh opaque
token to `session-index-absence.json` and then the key's retained request
entry, under hooks.lock, without changing the marker: other keys' misses stay
allowed. The next slice reads the token under hooks.lock, so every entry
carrying it already exists, then enumerates the complete registration
and candidate inventory, and applies requested misses with the ordinary
owner/candidate repair. Each absence write is fenced under hooks.lock on the
same complete marker generation and an unchanged token, so every applied
request preceded the census and its key's earlier owners are in it; the
retained entry refuses later registrations of that key. The slice then records
the token certified. A changed token or marker leaves work for the next slice.
A repeated request for a retained key keeps it scheduled; when its token was
lost or already certified (the entry was stranded), the repeated request,
from a hook or discovery's failed lookup, writes a fresh token instead of
withdrawing the census. Damaged, owned or
uncertified evidence, packed markers, and unusable tokens take the full
recovery request.

A hook refused only because a census is pending must not change its
generation either: a new generation fails that census's completion, so a busy
machine's stream of refused starts and subagents could keep recovery pending
indefinitely. While the marker is incomplete no miss is allowed, so no
registration of an unowned key is admitted after the census enumerates. A
request for a key whose entry names a valid owner (a SubagentStop's parent)
writes nothing. A request for a key with no entry, or a certified absence,
under a version-1 marker is retained with a fresh absence token instead of a
new generation: the census certifies the entry when its listing includes it,
and otherwise the slice after completion does. Other requests keep the full
path.

Registration membership changes stage a small durable revision outside locks.
The request lock precedes the short membership lock, which commits the revision
before the registration rename. Completion takes hooks then membership and
checks the revision before committing the certificate. The membership lock
never acquires hooks/request locks or covers staging, syncing or a census.
Valid continuation updates retain identity and do not change the revision.
Recorded membership evidence that disappears stays uncertain rather than
becoming a legacy store again.

The scheduled application allowance is at most four seconds and starts after
complete registration validation and fingerprinting. One deadline charges all
application phases, including later phase inventories and checkpoints. The
caller context bounds the entire stage separately; if complete validation
cannot finish within it, recovery stays uncertified. The CLI gives complete
validation up to half of its remaining soft budget,
reserves checkpoint time within the separate application allowance, and leaves
time for publication within its existing soft/hard budgets. Complete
inventory reads and one in-flight atomic operation can exceed an allowance on
slow storage, so this is a measured scheduling target rather than an arbitrary
host IO deadline. Local recovery/discovery still precedes credentials/storage,
and publication of already admitted sessions can proceed while recovery is
pending. The CLI avoids a second recovery slice inside the collector. Synthetic
fixtures exhaust the same scheduled engine within their contexts;
production consumers retain bounded scheduling.

The content-free marker, cursor and membership fence distinguish pending
application from a completed census. They are internal recovery evidence; the
activation phase must add bounded health/status presentation of pending
identity-recovery phases. Missing or corrupt evidence must remain unknown, and
that health reader must not read native sources, Git or storage.


A validated committed qualified index for the same key/archive owner is left
unchanged after the request-lock registration check. Recovery repairs corrupt,
conflicting, requested, absent or unfinished reservation entries instead of
using that optimization. Existing-index and missing-index migration benchmarks
are separate cases; their allocation and elapsed distributions are not pooled.

## Pass-owned Codex current lookup

The CLI owns one `discovery.CodexRolloutLookup` per collector pass and shares it
with the discovery directory worker and the native source factory. Declared
hook/import homes may supply read facts; only the enabled configured discovery
homes can create discovered registrations. Current locators, cached headers and
SQL metadata never supply admission permission. Original creation, project,
current consent, exclusions and destination are checked independently.

Native SQLite main and WAL files are confined read-only regular-file handles.
SQLite opens only the private projected main. Same-generation committed WAL
appends can be checksum/prefix revalidated and replayed into that existing
private view, followed by a real targeted row query. A checkpoint or generation
change discards the old projection and attempts a fully verified new snapshot
within the same remaining lookup time and native memory ledger. The selected
locator and revision are still compared; an uncertain snapshot or an actual
selection change stays retryable. This uses filesystem stamp observations and
does not acquire a native SQLite reader lock or claim an atomic filesystem
snapshot.

The lookup keeps at most 8 MiB of observed rollout facts (hints) and 4 MiB of
rollout directory listings per pass, with conservative bounds before
accumulation, charged to the shared native read ledger. A base rollout is found
by its ID in at most the three day directories its UUIDv7 time names and
archived_sessions/, each listed once per pass and at most 65,536 entries; a
directory whose listing no longer fits the 4 MiB is searched for that one ID
without keeping its listing.
Observation leaves one second of the same pass allowance for its durable
checkpoint. The checkpoint inherits the original caller's cancellation, so
expiry of the observation slice preserves the queue without extending the pass
deadline. The prunable observation cache remains
at most 2048 entries. Completed attempts rotate only after their consumer receives
the result; unfinished and undelivered work stays enrolled. Physical dependencies
use the same worker and requested facts, including those outside the hint cache.
A validation epoch digests every observed directory entry fingerprint before
completeness; it does not protect against same-stamp rewrites.

Already admitted, unbound ordinary legacy registrations can retain their prior
anchored source representation when current evidence is unavailable. The native
provider verifies absent history mode, matching physical/thread identity, local
ordinary execution and absence of relationships. It checks actual identity/cwd,
source prefix and the real lookup token, without manufacturing modern bindings
from optional legacy evidence. Generic non-native hook files use the existing
registered lane with actual ownership and current-row revalidation. Bound,
discovered, paginated, forked and revised sources remain strict. This compatibility
lane creates no registrations. History publication and maintenance require a supported complete source set,
durable frozen journal and all-reference verification. Unknown home migration uses
configured confined evidence, rechecked before saving the binding; lookup hints
never provide admission permission.
