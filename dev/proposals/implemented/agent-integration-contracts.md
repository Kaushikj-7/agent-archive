# Integration phase-entry contracts

Companion to [the integration proposal](agent-integration-abstraction.md).
This is an implementation checklist, characterized at `c85d239`, with the
performance reference at `98f8bd0`. It settles representation choices; it does
not change production behavior. The [measurement report](agent-integration-baseline.md)
records executable gates and their limits.

Target-context update: `main` at `d938fd99eb1d091388511a77484342423623e3e3`
has extracted file reads into `transcriptio` and Claude/Codex discovery into
`nativesessions` (#277). The historical characterization below remains pinned
to `c85d239`; phase 4 must preserve the newer collector's context-aware reads
and post-read `Snapshot.Check`, which rejects observable size/mtime changes
instead of publishing that read. The fixed boundary still excludes later
appends, and same-size/same-mtime rewrites remain undetectable. Phase 4 should
reuse the real `transcriptio` boundary/snapshot helpers; phase 6 should consume
the real `nativesessions` discovery/header helpers. These target changes do not
alter the recorded historical measurement snapshots.

Live target `d05defabb8ee1d7f268ee854882ca98dcfc0bde5` additionally includes
native handoff before setup (#280). Its read-only Claude/Codex selection uses
`nativesessions.Discover`/`InspectNative`, bounded identity and preview windows,
and complete selected-source filtering on a verified snapshot. Phase 6 must
preserve both bounded native-handoff coverage and import's separate compatibility
header rules; incomplete discovery cannot establish `--latest`, and loading
labels cannot recover uninspected identities. The native selector's current
ASCII/128-byte ID acceptance is a consumer restriction, not a new SessionKey
or registration limit. Its bounded worker pools do not authorize collector
concurrency or eager full-source filtering during discovery.

Live target `4add976435af4b91d1dfbd31b3c2496316a0d3f4` also includes archived
handoff selection (#257). Keep its shared picker/word-search policy distinct
from native extraction: a known zero human-turn count is omitted; an unavailable
count (text format or parse failure) is not zero. Exact/full or short archive
IDs still name promptless sessions. A local registration may supply a prompt
added since a promptless archive snapshot; archive-only selection retains that
snapshot's result. Preserve this behavior when Analysis availability and the
native/configured-archive handoff consumers move.

Live target `e3b82989d56903966a0fb1c6a774c18c15f5e838` additionally preserves
cancellation during checkout canonicalization and before preview loading.
After header workers finish, discovery releases unused reservations and carries
actual read bytes into the preview budget. Exhausting that budget leaves verified
identity-only rows selectable without further preview reads. Incomplete identity
discovery refuses automatic latest selection even with a destination/current
session: interactive use requires an explicit picker choice; noninteractive use
returns qualified ID/agent/checkout recipes. Preserve these separate discovery,
preview and shared selection policies through phases 2, 4, 5 and 6.

Live target `0ffa628e84a2b4e6265ffa2ff481788269027567` includes machine-registry
publication, bounded portable project matching, default storage creation and
pairing-bundle redaction. Shared filter/adapter versions are now 15/0.15.0:
preserve `aa-pair1:` redaction at every retained depth, including truncated and
mixed-case payloads, and the target's golden fixtures. Historical measurements
remain pinned to their earlier filter versions. Machine credential provenance,
independent registration retry/status, setup ownership and storage confirmation
remain shared policy. Project matching retains bounded read-only native history,
repository/checkout verification, canonical exclusions and explicit-path choices;
later discovery extraction must not widen capture scope or turn incomplete
matching into verified admission. These target features are inherited context,
not new integration capabilities or permission to modify their behavior.

## Qualified identity and local migration (phase 3a)

Use `SessionKey{Agent, NativeID}`. Canonicalize the agent once (trim, lowercase,
`claude-code` → `claude`); do not trim, lowercase, split, or path-clean the native
ID. Preserve the existing rejection of all-whitespace IDs. Native IDs remain
opaque UTF-8 string values. JSON identity fields already decode invalid UTF-8
to replacement characters; filenames and SQLite key suffixes can instead
contain invalid bytes, which current registration JSON cannot round-trip.
At the new boundary classify those as malformed discovery identity (and reject
injected invalid-UTF-8 SessionKey callers), rather than silently normalizing
them or adding a second persisted byte identity. Test this explicit malformed
input refusal alongside exact Unicode persist/reload adoption. Preserve exact
valid string bytes; no UUID-shaped identity restriction is introduced.

Define `E(s) = uint64-big-endian(byte-length(s)) || bytes(s)` and
`K = ASCII("agent-archive/session-key/v1") || E(agent) || E(nativeID)`.
Store at `sessions-v1/<lowercase-hex-SHA256(K)>.json`; entry contains version,
canonical agent, native ID and archive session ID. Persist the native ID as its
JSON string, matching SessionRegistration; validate UTF-8 before persistence
so index and registration reload identically. Validate entry contents against
the requested key and the safe archive-ID component. Do not concatenate with a delimiter.

Examples: (`claude-code`, `ABC`) and (`claude`, `ABC`) are identical keys;
(`codex`, `ABC`), (`claude`, `abc`), and (`claude`, ` ABC `) are different.
(`a`, `b:c`) and (`a:b`, `c`) have different encodings. Native `/`, NUL and
Unicode bytes never become path components. Archive IDs, parent archive IDs,
remote keys and existing source/metadata schemas do not change.

Migration and interruption rules:

1. Under the existing capture/hooks lock and archive request-lock order, check
   the qualified index. Validate the referenced registration before using it;
   distinguish a disappeared registration from a conflicting owner.
2. On a qualified miss, read only the single legacy `sessions/SHA256(nativeID)`
   entry and its registration. Adopt only if registration archive ID matches
   the entry and both canonical agent and exact native ID match. A legacy
   entry naming another agent is a miss, never a reusable ID. This bounded
   lookup validates only the referenced owner; it does not prove global
   uniqueness. Directly referenced conflicting owners and durable conflict
   evidence block adoption. A complete off-hot-path recovery census detects
   same-agent duplicate registrations and records an explicit conflict, never
   a first-match choice. Ordinary absence does not require a census before
   validated fresh admission; arbitrary manually introduced duplicates are
   undetectable by bounded lookup alone.
3. Write the qualified entry atomically using `local.Write`. Leave the legacy
   entry as historical recovery evidence, with **no new unqualified writes**.
   Repeating after interruption before/after this write gives the same ID.
   Retention/undo remove a legacy entry only when it still points to the
   registration being removed; they never delete another agent's evidence.
4. Re-read under the request lock as `RegisterNewSession` does today. Expiry
   between lookup and registration cannot recreate a forgotten archive ID.
   A fresh start may create a new ID; continuation alone cannot do so.
5. Missing/corrupt qualified entries or corrupt legacy entries needing a scan
   are repaired by an explicit maintenance/collector recovery pass, outside
   hook admission. Scan registrations once, canonicalize keys, reject conflicts,
   and atomically repair each entry. Interrupted recovery is resumable; do not
   retain a partial inventory as proof that no registration exists. Hooks return
   a bounded recovery-needed diagnostic and preserve eligible deferred intent.
6. Carry the same key through registration, request/retention deletion,
   backfill deduplication/undo, subagent parent/child lookup and deferred intents.
   Preserve existing generation checks and lock ordering (`state/store.go`,
   `state/lineage.go`, `capture/admission_intent.go`); do not add a second lock
   acquisition order for migration.

Downgrade: a qualified namespace is not writable-compatible with old binaries.
An old binary ignores new markers and can create a second ID or reuse a legacy
collision. No marker can make already released binaries refuse this. Before
running an old writer, stop collectors/hooks, back up the data directory, and
restore the complete pre-migration local snapshot (accepting loss of later
local pending work), or use a separate empty data home. Remote archives remain
readable. Do not run old and new writers against the migrated home; do not
reverse-convert into an unqualified namespace. Record local index format in a
new-version-readable migration marker for diagnostics/recovery, not as claimed
old-binary enforcement. Document this operational restriction when phase 3a
ships.

Required new migration tests: cross-agent same-ID collisions; alias adoption;
Unicode persist/reload adoption and invalid-UTF-8 caller rejection; wrong/missing referenced registration; corrupt entry;
interruption before/after every durable conversion step; duplicate same-agent
registrations; retention and undo during adoption; delayed deferred intent;
subagent parent collision. Existing corruption tests intentionally scan on the
hook path; changing that needs explicit recovery tests, not deletion of coverage.

## Freshness and event decoding (phase 3b)

Exact present predicate: `capture/hook.go`'s `provesFreshSessionStart`,
`cursorProvesFreshStart`, `emptyTranscriptProvesFreshStart`, `startsCapture`.
Freshness below means evidence only; inclusion, activation, destination,
transaction state and generation checks can still decline admission.

| Agent/event | Native source/path | Evidence for a never-seen session |
| --- | --- | --- |
| Claude/Codex `SessionStart` | `source`, trimmed/lowercased, `startup` or `clear` | Explicit fresh, regardless of transcript presence/content |
| Claude/Codex `SessionStart` | `resume` or `compact` | Explicit continuation; cannot admit |
| Claude/Codex `SessionStart` | Any other nonempty trimmed/lowercased source | Unknown; cannot admit even with an empty file |
| Claude/Codex `SessionStart` | Source absent, empty, all-whitespace, or non-string | Bounded path stat request; use rules below |
| Claude/Codex `UserPromptSubmit` | Any source/path | Turn start only; cannot admit |
| Cursor `sessionStart` or `beforeSubmitPrompt` | Path absent, null, or string `""` | Fresh (tested desktop contract) |
| Cursor `sessionStart` or `beforeSubmitPrompt` | Path present, non-string/non-null | Unknown; cannot admit |
| Cursor `sessionStart` or `beforeSubmitPrompt` | Nonempty string path | Bounded path stat request; use rules below |
| Any agent stop/response/subagent event | Any source/path | Cannot admit a new parent session |
| Unknown agent or event (including wrong event case) | Any | Ignored |

Path inspection: absolute missing path (`errors.Is(os.ErrNotExist)`) or absolute
empty regular file proves fresh. Existing nonempty file gives no fresh proof;
relative path, directory, or stat error other than not-exist gives unknown.
No transcript is opened, and file read permissions are not checked: an empty
regular file with mode 000 still proves fresh if stat succeeds. Stat follows
symlinks as today. Cursor's `source` and
`cursor_version` are irrelevant to fresh proof. Unknown evidence can continue
an already admitted session after shared ownership checks; it cannot create a
never-seen registration. Existing file continuation may relocate its transcript.
Cursor late path adoption checks the conversation filename and only supported
response/stop paths; a database source cannot switch to file. Keep those checks
separate from freshness. First Cursor prompt emits start-candidate then turn
start; batch validation precedes both effects, and retries preserve partial
admission/evidence effects and intent generation.

Event names remain exact: Claude stop events `Stop`, `StopFailure`, `SessionEnd`;
Codex `Stop`, `Interrupt`, `SessionEnd`; Cursor `afterAgentResponse`, `stop`,
`sessionEnd`; subagent `SubagentStop` versus Cursor `subagentStop`.

## Input and observation encoding (phase 4)

Use an explicit file/record union. File input supplies observed length and a
bounded `ReaderAt` valid until snapshot close. Framing is JSONL or whole text;
hints select attempts, not safety. JSONL trims only its torn tail: a newline
ends a record; valid final JSON without newline is complete; invalid final
bytes after a newline are deferred; no-newline input is attempted whole.
Preserve `completeJSONLBoundary`'s one-byte ordinary check, 64 KiB backward
chunks and record-limit cap. Text uses the whole observed length.

The live target's `transcriptio.Snapshot.Records` is a separate bounded preview
operation: only newline-terminated complete records are visited; oversized
records, tail-leading fragments and unfinished final records are skipped with
explicit incomplete coverage. Do not substitute this excerpt framing for full
filtering's valid final JSON acceptance or torn-tail rules. Preserve context
checks during boundary probes and record iteration, post-read verification,
actual read-byte accounting, cumulative discovery/preview reservations and
explicit older batches. Browser filtering/redraw uses retained safe display
facts; selecting a candidate filters its complete source once and revalidates
identity/checkout, including identity conflicts beyond the bounded header.

Concrete inputs:

- Claude/Codex JSONL: `File{ReaderAt: section, Length: observedSize}` with framing
  selected by codec. Claude child additionally gets typed optional metadata
  bytes from the adjacent `.meta.json`, at most 16 KiB. Provider performs the
  only path lookup; codec sees no arbitrary file opener. Missing/malformed
  metadata remains optional, as `subagent_meta.go` specifies.
- Cursor file: try JSONL first. Current collector tries text only when the
  Cursor JSONL filter returns `archive.ErrUnsafeSourceFormat`, and never after
  a record/size failure. Preserve the entire accepted-input set when renaming
  this result to typed format mismatch; malformed input tightening is a
  separate versioned privacy change. Text is one bounded record, not JSONL.
- Cursor database: ordered composer raw row followed by header-ordered bubble
  records (`kind`, bubble identity bytes, `json.RawMessage`, `missing` flag).
  A missing row is explicit even when its bytes are nil. Inline conversation
  remains inside the composer row, not an invented second JSON envelope. Use
  existing raw bytes directly. Borrowed record bytes live until next iteration;
  `Input()` does not marshal, copy or decode them again. A materialized legacy
  composer can initially adapt its slice without copying transcript content.

Limits from code (not inferred native UUID lengths): archive/collector maximum
record is 64 MiB (`archive.MaxRecordBytes`); default retained transcript 64 MiB;
raw aggregate 4 × retained limit (default 256 MiB), capped at `1 << 62`;
Cursor text whole document ≤ record limit; Claude metadata ≤16 KiB. Cursor
`CursorChatSize` counts composer/bubble value bytes, not synthetic framing.
Retained-size validation stays after filtering. Provider enforcement before
allocation must preserve outcomes; today's Cursor store materializes composer
and rows **before** collector size checks and has no header-count/ID-byte cap.
Do not claim current database reads are bounded before allocation. Adding a
new rejection limit for native IDs/headers is a separate compatibility decision.

Signature: use a versioned provider-qualified equality token. The observed
result separately has `present` (including empty) or `missing`; errors carry
transient/deterministic/cleanup categories, never fake equality tokens. Empty
files/chats are present with a real token; missing is not zero-length content.
File token v1 is base64url-without-padding of exactly 16 bytes:
`uint64BE(size) || uint64BE(mtimeUnixNano)` (signed mtime represented as int64
bits). It preserves today's size/nanosecond equality, including its residual
same-size/same-mtime weakness; no content hashing is added.

Cursor token v1 is lowercase hex SHA-256 of
`ASCII("agent-archive/cursor-signature/v1") || int64BE(lastUpdatedAt) ||
uint64BE(headerCount) || E(lastBubbleID) || uint64BE(messageRows) ||
E(lastMessageHash)`; counts must be nonnegative, hash empty or 32 lowercase
hex bytes (the existing store hashes SHA-256 then retains its first 16 bytes;
the phase-0 64-byte characterization was incorrect). The outer token remains
64 lowercase hex characters. `LastBubbleID` is **unbounded in `cursorstore.decodeHeaders`**;
fixed 36/64-byte UUID assumptions or truncation would alter equality. Hashing
length-prefixed existing fields bounds persisted tokens without truncating
native values. The digest has the usual cryptographic collision assumption;
retain exact tuple comparison when reading legacy signatures. Legacy file
fields and `ScanSignature.CursorSignature()` remain accepted until rewritten
through a normal completed scan, not a mass conversion.

Persist only `{version:1, provider:<declared source kind>, token:<token>}`.
Built-in providers are `file` and `cursor/sqlite`; provider names are validated
ASCII components ≤64 bytes. Maximum serialized compact JSON is 166 bytes
(64-byte provider and 64-byte digest), so set the representation ceiling to
256 bytes including future fixed fields; this is an envelope ceiling, **not**
a new native-ID limit. File token is 22 ASCII bytes, Cursor token 64. Reject
unknown token versions for scanning as needing reobservation, not unchanged.
Keep parser/filter/adapter versions, limit policy, failure/block reason and
source format as separate scan fields. Signature is not freshness or ownership.

Passes open lazily and serially. Cursor unchanged checks read in place, do not
copy; one changed pass has at most one snapshot **attempt**, including failure.
Running Cursor uses online-backup committed state; closed Cursor uses existing
immutable-read/change-check rules. File stat is taken before bounded read;
append beyond the observed boundary waits for the next pass, but in-place
rewriting during read can alter bytes. No new isolation guarantee. Context
checks cannot interrupt all blocked filesystem calls. Join cleanup errors with
primary failure, retain raw-snapshot cleanup visibility, make closes idempotent.
Append-only rewrite blocks; replaceable database snapshot records existing
bounded cumulative Cursor rewrite gap and retains publication provenance.

## Analysis ownership (phase 5)

Every row below must move together with a real caller. Parser facts come only
from retained bundle evidence; source ownership remains filter-time evidence.

| Current native-shape consumer | Integration-owned extraction | Shared owner after extraction |
| --- | --- | --- |
| `archive.ParseNormalized` and native turn/tool helpers | Claude/Codex/Cursor turn, tools, usage, model, errors, compaction, native skill uses; Cursor text turns | Common normalized values, tool linking/aggregation, rendering |
| `archive.metadata.tokenModel`, `accumulateTokens` | Native usage meanings, message IDs, model attribution | Saturation, aggregate/model totals and public counts |
| `archive.metadata.nativeTurnEnd` | Cursor `turn_ended` status and last-record outcome | Hook/native precedence, idle state and outcome presentation |
| `archive.session_labels.deriveSessionName` | Claude custom title/subagent description; Cursor composer name; last nonempty fact wins | Title collapse and name/title display precedence |
| `archive.session_labels.derivePullRequests` | Claude PR-link records with validated number/repository/URL | Stable deduplication and `MaxPullRequests` cap |
| `archive.handoff.recordedBranch`, `recordedWorkspace`, `workspaceRoot` | Native working directory, Git branch and workspace evidence | Branch validity/detached HEAD policy and current-checkout comparison |
| `archive.metadata.structuredCounts`, `toolErrorsObservable` | Availability from actual source format and recorded filter version | Unknown versus zero; common counts and metadata schema |
| `archive.BuildMetadata`, `SessionLabels`, `BuildTranscript`, `BuildHandoff` | Resolve parser and compute Analysis once at operation entry | Accept that same Analysis; never independently parse bundle |
| `archive.PreviewRecord`, `PreviewAccumulator`, `collector.PreviewTranscript` | Filtered record label/branch/activity and prompt classification, including pending slash commands | Bounded safe display facts, partial/unavailable coverage and loaded-row search; no full Analysis or raw excerpt retention for each preview |
| `nativesessions.InspectNative`, `cli.nativeBundleIdentityMatches` | Native header and complete-selected-record identity/sidechain interpretation | Verified candidate/checkout selection and conflict refusal; keep bounded discovery distinct from import compatibility |
| `archive.filter_session_labels`, `filter_subagent_meta`, `subagent_type.SanitizeSubagentType`, composer filter | Safe retained facts, sanitized harness observations | Existing filtering/privacy/gap rules and source header assembly |
| `collector.subagent_capture.checkSubagentProvenance`, `CheckImportedSubagent` | Local filter observations: native/agent IDs, complete identity flags, native bounds | Parent ownership, admission/time bounds, child publication links |
| `capture` hook evidence/model/status helpers | Decode native enums/fields; sanitize via shared evidence filter | Hook/native outcome precedence, requests, debounce and replay policy |

Analysis has common view, retained NativeFacts, and bundle-specific typed
availability with fixed reason codes. It owns no raw handles or live database.
Safe source retention survives parse failure; metadata minimal failure behavior
and subsequent parser upgrade remain intact. Current `BuildMetadata` already
passes its view to label derivation, but separate labels/transcript/handoff
entry points parse again: do not describe today's multi-builder path as one
analysis. Historical filter-version fixtures must still parse through current
agent parser. Extraction alone does not bump shared versions or regenerate
goldens.

## Narrow host and configuration requests

Introduce each shape only with its first caller. Constructors perform no host
probes. Use injected filesystem reads (`ReadFile` with limit, `Stat`, ordered
`ReadDir`), typed platform locations, and explicit observation time; no whole
CLI Env or unrestricted callback/function registry. Discovery streams bounded
candidates/cancellation into the shared existing materialized plan, avoiding a
second full candidate inventory. Empty, unreadable and incomplete store results
are distinct.

Hook/skill plan request: action enum, native locations, bounded existing file
bytes plus presence/read-error, archive executable/data-home identity and
installation ownership. Plan output: path, expected prior digest/presence,
replacement bytes/remove flag, permissions. Inspect output: absent, owned,
foreign or unreadable with fixed safe reason. Shared journal/atomic editor
applies plans and checks concurrent edits; integrations never apply writes.
Reuse byte-preserving JSON editing. Deduplicate shared skill destinations;
remove only when no selected integration still needs them.

Launch request: resolved executable, project directory, prompt, handoff path,
explicit extra argv. Native launcher builds argv and checks collisions; shared
code controls environment, terminal, process and handoff cleanup. No shell or
unused stdin contract. Runtime request: injected environment getter and known
native locations. Observation distinguishes exact ID and project-presence
fallback; shared code handles ambiguity and session-variable cleanup.

Native handoff before setup is an existing read-only consumer of these boundaries.
Preserve its consumer-owned dependencies, config load once per invocation,
exact current-session selection and ambiguity checks, and lack of archive-store,
capture or setup side effects. Native launches use private temporary files in
the dedicated local-handoff namespace, retained for the launched session with
best-effort cleanup of owned old files; they do not create archive state. Shared
launch orchestration continues to own argv, environment, working-directory and
file lifecycle. Preserve the separate configured-archive and explicit-file paths.

Executable/version inspection gets only a resolved executable and a typed
probe operation for that agent, with bounded output and timeout; restrict argv
to the implementation's declared version invocation. Capture never receives
this probe dependency, process execution, network, Keychain or terminal access
(including transitively). Native discovery is read-only. Diagnostics never
serialize raw hook payload or arbitrary host error text into common events.

## Composition sequencing (phases 1–2)

Catalog identities can land with configuration and reader consumers first. A
registry must have a real operational consumer: bring the first launcher
contract/caller into phase 1, or combine phases 1 and 2. Do not declare
LiveCapture/Backfill capabilities before their interfaces arrive, temporary
boolean promises, nil stubs, or an unused empty registry. Operation projection
is derived from implemented interfaces. Preserve presentation policies: setup
orders Codex/Claude/Cursor; reader/handoff order Claude/Codex/Cursor. Derive each
order from one catalog plus explicit presentation policy, not repeated built-in
identity literals. Unknown archive agents stay representable and visible.

The catalog audit also includes #280's native store roots, preview codec dispatch,
native display names and current-session selection. Bind only implemented
capabilities: automatic native discovery currently supports Claude/Codex, while
Cursor remains explicit-file input. Phase 2 launch/runtime migration must retain
native, configured-archive and explicit-file consumers; phase 5 must migrate
safe preview facts together with full parsing, without turning previews into
whole-transcript analyses or forcing a parser dependency into identity discovery.


## Phase 1 implementation boundary

Phase 1 introduces `agentmeta` identities/catalog and immutable operational
composition. Its only operation is launch argv construction, exercised by both
handoff launch and installed destination discovery. Native launch argument rules
moved into Claude/Codex/Cursor integration packages with this first real caller;
phase 2 completes runtime detection and child environment cleanup. The launcher
accepts project, prompt, handoff path and extra argv; executable resolution stays
with shared CLI code because current native argv rules do not consume it.

Identity-only legacy configuration/reader entry points delegate to `agentmeta`;
CLI composes the operation registry once. Injected catalogs reach configuration,
reader probes and CLI flags/launch. Known-agent probe lists are cached outside
session scans. Setup presentation derives Codex-first order from the single
identity catalog; reader and handoff preserve catalog order Claude/Codex/Cursor.
Unknown archived agents remain visible through general listings and direct-read
fallback. Alias ingress is canonicalized, including handoff config map keys;
conflicting alias/canonical keys are rejected rather than chosen by map order.

No lifecycle, source, filter, parser, discovery or skills interfaces or capability
promises are introduced here. Existing native store roots, preview dispatch and
current-session runtime facts remain implementation-specific until their assigned
phases. Launch implementation support does not claim native discovery, complete
capture/backfill support, installed-version verification or observed host health.
