# Bounded repository windows and picker startup

Status: PR 1 reserves the format and integrates refusal controls. Bounded indexed
picker construction, reads and production activation remain unimplemented.
Prepared 2026-10-13 against main
`4b9b612ff9fbac454c93a292435bb151f7cde75f`.

This is the next source implementation slice of
[listing at scale](listing-at-scale.md) and
[CLI performance](cli-performance.md). It preserves the product behavior in
[session finding](../specs/session-finding.md). Production activation remains
subject to [provider qualification](../catalog/provider-qualification.md) and
[migration authority](../catalog/migration-and-query.md).

## Outcome and architecture

Opening bare handoff, list or show should read one repository's first page of
recent root sessions without enumerating the archive. Older fetches another
remote page. Selection reads and verifies the chosen full metadata and sources.
Search may initialize the complete searchable universe on demand; first-screen
startup must not initialize it preemptively.

The authoritative index consists of immutable B+tree node objects in the archive
bucket, committed through `catalog-v4/head.json`. Publishers update it themselves;
there are no S3/R2 event listeners. Per-command memory caches validated tree
nodes. The private SQLite catalog remains a disposable local search cache, not
the shared authority or a prerequisite for first-page display.

The existing catalog writer, CAS head commit, durable admission/coordinator,
child counters, immutable metadata, snapshot lifetime and GC machinery are reused.
This work adds scoped ordering and paged consumers. It does not replace these
foundations, enable production providers, implement fragments or reconcile the
entire older historical-publication PR stack.

## Current source to understand first

| Concern | Existing source | Present limitation |
| --- | --- | --- |
| Scope identity and fallback | `internal/cli/scope.go`, `scope_browse.go` | Correct matcher operates on an already loaded universe. |
| Handoff setup and local merge | `internal/cli/handoff_select.go` | Loads all archive metadata; Older increases an in-memory limit. |
| Browser archive loading | `internal/cli/list_browse.go`, `session_catalog.go` | Summary/SQLite paths can request every row. |
| Remote query eligibility | `internal/reader/remote_catalog.go` | `ScopeMatch` and harness predicates reject bounded selection. |
| Shared ordering and writes | `internal/catalog/catalog.go` | Project keys order by identity, not repository/activity. |
| Snapshots and cursors | `internal/catalog/snapshot.go`, `read_view.go` | Existing ten-minute and changed-head rules must remain. |
| Compact/overflow entries | `internal/catalog/entry_codec.go`, `resolved_row.go` | Current overflow projection omits RepoKey and display/eligibility facts. |
| Child counts and child ranges | `internal/catalog/child_counts.go` | Reuse counts; do not enumerate children for the first screen. |
| Migration and maintenance | `internal/catalog/migration*.go`, `gc*.go`, `coordinator*.go` | New format completeness must survive upgrade, rollback and GC. |

## Query contract

The initial supported bounded queries are ordinary root windows ordered by
activity, either all projects, an inferred/directory repository scope, or a
directory scope with ProjectIDs but no RepoKey. Support an optional exact
canonical harness filter from the first implementation. Exclude replays using
the existing handoff/default browser rules. Preserve current activity ordering
and every tie breaker by using the current exhaustive result as the oracle.

Matching must reproduce `sessionScope.contains`:

- When scope and record both have RepoKeys, only equality matches. A conflicting
  known RepoKey never matches through ProjectID.
- With a scoped RepoKey, merge its repository range with ProjectID ranges for
  records that have no RepoKey. These are disjoint membership classes.
- With no scoped RepoKey, query the scope's ProjectID ranges regardless of the
  records' RepoKeys, as the current directory matcher does.
- Local registrations retain their existing ProjectRoot and registration RepoKey
  precedence. Do not derive cross-machine equivalence from equal folder names.
- Named `--project NAME` uses the existing case-insensitive name/ID union. Keep a
  documented complete-summary path initially; do not silently narrow it to IDs.

Advanced predicates and complete search remain explicit compatibility paths.
Do not claim bounded behavior for a query by scanning and post-filtering an
unbounded number of candidates. Unsupported queries retain correct results.

A page returns small typed rows, pinned metadata references/revisions, child
counts, `more` and an opaque continuation. Counts are exact only when proven by
the relevant index ranges. Mixed local eligibility may leave totals unknown,
as current handoff does. Empty first-page results prove archive emptiness only
for a complete supported query; errors or an exhausted work budget do not.

A row projection is presentation data, never full metadata/source authority.
Include only already authorized derived fields needed for current row rendering:
identity, canonical harness, scope identity, ordering timestamps, bounded name/
title/project labels, root/replay classification, child counts and prompt state.
Prompt state must preserve the difference between known zero and unknown turns.
Unknown counts must not be interpreted as zero. No transcript, source body,
native path or newly retained native field is added to the index.

The index needs separate browse-root and handoff-eligible membership. Archive-only
handoff excludes known no-prompt sessions without scanning them after retrieval;
local sessions whose archive copy had no prompt can still be considered through
the local merge. Unknown prompt counts preserve current eligibility behavior.

## Index layout and format boundary

Use new versioned namespaces in the existing Project tree rather than adding a
new top-level root. The existing head digest, retirement witnesses and tree GC
then retain all indexed objects through the same root. An illustrative key is:

```text
!picker/v1/<membership>/<scope-kind>/<encoded-scope>/<harness-tier>/<order-key>
```

Membership distinguishes browser roots and handoff candidates. Scope kinds cover
global, RepoKey, any-RepoKey ProjectID and missing-RepoKey ProjectID. Harness tiers
provide both all-harness and exact-harness ranges. Order keys reuse the current
activity/capture/identity tie order. PR 1 freezes the actual escaping, boundary,
maximum-length and membership specification; raw names/paths are not keys.

Generate keys from verified full mutation metadata before overflow compaction.
Update all old/new memberships in the same head CAS for create, rewrite, scope
change, child classification change, prompt change and deletion. Propagate child
counter changes into the new row projections. Keep fanout a documented constant
per session; do not index every possible combined predicate.

Freeze a small bounded row codec so unusually large metadata does not force full
body downloads on first-page display. Truncation is presentation-only, UTF-8 safe
and cannot affect scope membership, ordering, eligibility or exhaustive search.
Avoid wrapping the full `archive.Metadata` type as a row and accidentally treating
it as selected-body authority. Validate row/reference correspondence on writes,
upgrade and reads.

The new namespaces require a stronger catalog protocol/head contract and a
permanent writer capability. A marker alone is insufficient: current protocol-10
writers would otherwise preserve the marker while failing to update these keys.
Older writers must refuse before any mutation. New readers retain old catalogs
through the complete-summary path. Allocate the exact numeric versions in PR 1
after inspecting the then-current version registry and concurrent changes;
`publication-composition-v12` and the proposed staged-publication version 13 must
not be reused by assumption. Capability names and all decoder combinations must
be recorded in the versions document and protocol fixtures.

## Cursor, request and cost contracts

Repository windows merge a finite set of ordered ranges. Fetch at most a page plus
bounded lookahead per range, then merge using the exhaustive comparator. Continue
from each range's unconsumed position; do not skip buffered rows or repeat rows.
No OFFSET over the full archive and no caller predicate closures in this API.

The continuation binds the exact scope/ProjectIDs, membership, harness, order,
format, captured roots and original request lifetime. Keep existing snapshot
rules: changed head or expiry produces an explicit refresh outcome. A new command
cannot adopt a token from an older snapshot. Refresh discards old continuation
state; toggling scope may reuse its page only while that view is still valid.

Use one fresh head observation for each logical page or bounded identity-lookup
batch; internal range walks share that captured request view. Current per-query
freshness checks must be adapted through an explicit caller-connected batch
contract, not disabled to avoid network calls. Preserve validation at page return,
continuation and selected-body use, and never restart the original expiry clock.

For page size P, tree height H and scope range count S, the conservative first-page
tree-work bound is O(S * H * (P + 1)), with bounded lookahead. O(S * H + S * P)
is an amortized target requiring a traversal/occupancy proof for the current
underfull/unary tree. Publish concrete
request and byte ceilings derived from node/page limits, with bounded lookahead.
Set a finite supported scope-range ceiling in PR 1 (start with 32 unique ranges);
larger configurations use an explicit compatibility path. Do not infer constant
cost for arbitrarily many ProjectIDs. First-page archive metadata-body and
transcript reads are zero; tree nodes and small row projections are counted.

Local state is a separate cost: R relevant registrations can require identity
lookups, activity observations and bounded label materialization. Report it
separately; the claim is bounded archive work, not constant cost for an unlimited
local registration inventory. All paths honor cancellation and close resources.

## Local/archive merge and selected-body authority

Never treat absence from a page as absence from the archive. For relevant local
registrations, use exact identity lookups in the same captured view before
asserting `not uploaded`, including registrations outside the first archive page.
Preserve source=local/source=archive/default selection, local activity overrides,
root/replay exclusion, prompt eligibility and existing label-read bounds.

Merge local candidates and ordered remote candidates using the existing total
order, deduplicating by canonical harness/session identity. A locally overridden
row can sort ahead of its archived position; remember that identity so later
remote pages do not repeat it. Page lookahead must still establish the top P of
the union. Unknown local eligibility remains unknown, not a false empty scope.

When a row is chosen, read its immutable metadata reference and verify its hash,
identity and relevant projection. Run the existing source/replay/destination
checks on verified full metadata. Use a current authorization/read view where
existing handoff requires it; a pinned presentation snapshot is not permission
to revive deleted or excluded content. Changed authority requires refresh or an
explicit failure, never substitution of a different session. Selected metadata
may be reused only with existing full-body provenance, not a forged projection.

## PR sequence and engineer handoffs

Each implementation PR must be independently reviewable and preserve working
fallbacks. Merge dependencies first and reconcile children against actual main.
Use the current source base and exact-head checks, not the preparation SHA above,
when implementing. Proposed API names below may change; their contracts may not.

| PR | Title | Depends on | Deliverable |
| --- | --- | --- | --- |
| 1 | Define scoped picker protocol and compatibility | Main | Reviewed format/query/cost contract, consumed protocol decoding and refusal controls. |
| 2 | Publish transactional scoped picker indexes | 1 | Keys, bounded rows, eligibility and atomic mutation integration. |
| 3 | Build and upgrade complete scoped catalog indexes | 2 | Fresh construction, guarded resumable upgrade and completeness activation. |
| 4 | Read bounded repository pages and continuations | 2, 3 | Typed paged reader, range merge, counts, cursor and hydration APIs. |
| 5 | Merge local sessions with bounded archive pages | 4 | Correct headless picker model and selected-body policy integration. |
| 6 | Connect browsers to remote Older and lazy search | 5 | Shared handoff/list/show setup, scope states, UI actions and cancellation. |
| 7 | Qualify bounded startup and reconcile tracking | 6 | Paired measurements, operation budgets, compatibility/runtime evidence and docs. |

### PR 1: freeze the protocol and integrate refusal

Read the files in the source map, `schemas/`, configuration/version codecs,
pending catalog journals, migration, coordinator and GC protocol validation.
Inventory every path that reads or writes the current head, including direct
writer APIs and recovery. Reserve a unique permanent writer capability and head
protocol, layering existing configuration protection rather than replacing it.

Land actual caller-connected decoding/refusal controls with format fixtures;
do not ship unused exported types as a claimed implementation. This PR must not
produce or activate a new complete catalog yet. Freeze row limits, key layout,
range ceiling, ordering, eligibility, cursor bindings and error/fallback rules.

Tests: current head/config/journal replay remains unchanged; an old writer
refuses the new protocol before source/index writes; missing/mixed/unknown
capabilities refuse; unknown schemas and malformed bounded rows refuse.
Document what frozen protocol-10 pending work must drain before upgrade.

### PR 2: maintain scoped indexes transactionally

Primary touchpoints: `internal/catalog/catalog.go`, `entry_codec.go`,
`child_counts.go`, `tree.go`, `write_authority.go`, and new private scoped-index
helpers. Implement bounded rows and key generation from full verified metadata.
Integrate membership insertion/removal and child-count propagation in commit;
keep existing receipt digest and mutation reuse protections intact.

Tests: create/rewrite/delete, RepoKey/ProjectID change, harness distinctions,
replay/root/child transition, unknown/zero prompt state, long row fields, identical
timestamps and unrelated-session CAS races. Lost-response replay must resolve
one exact mutation without duplicate membership. Different same-session writers
must retain current conflict semantics. New indexes remain unactivated until
their complete construction path exists.

### PR 3: construct and upgrade complete catalogs

Primary touchpoints: `internal/catalog/migration*.go`, `coordinator*.go`, `gc*.go`,
`internal/cli/migrate.go` and the operator routes. Fresh catalog construction and
legacy migration must build every required range before activation.

For an existing old catalog, obtain the real destination admission seal/barrier,
drain supported pending owners and refuse unresolved ones. Retain the old readable
head while building new Project-tree ranges in bounded pages. Durable checkpoints
bind the source roots, destination, exact owner, target format and build progress.
Interrupted work resumes only that intent. Full traversal is allowed here, not
in picker setup. Validate the complete new membership/row universe against the
identity/metadata oracle and atomically activate the new head protocol. Maintain
the configuration/remote writer fence before releasing writes.

Preserve immutable metadata, capture dates, tombstones, receipts, source/history
references and predecessor witnesses. Keep candidate roots protected from GC
through the existing pending/coordinator inventory. Unknown ownership, stale
source roots, corrupted objects or changed credentials refuse activation. Define
rollback before enabling this route: retain the stronger writer fence; refuse a
rollback that would forget post-upgrade writes; never reactivate old writers.

Tests: interrupted pages/activation/local config save, unresolved pending work,
competing upgrader, malformed/overflow source metadata, live reader snapshot,
GC retention and exact rollback. No user flag bypasses provider or cutover proof.

### PR 4: typed bounded reader and range merge

Primary touchpoints: `internal/reader/remote_catalog.go`, `catalog_selected.go`,
new scoped-page helpers, and `internal/catalog/snapshot.go` as needed. Add a
caller-used API for supported scoped root windows rather than widening the
opaque `ScopeMatch` callback to mean an indexed query.

Implement finite range selection, ordered merge, per-range continuation state,
proven counts/emptiness and pinned row/reference loading. Reuse immutable node
cache and request view. No SQLite `RefreshRemote`, `CatalogSummaries`, identity
tree scan or canonical LIST occurs in the supported first-page route.

Tests: every page concatenated equals the exhaustive scope/order oracle, including
ties, empty ranges, missing/conflicting RepoKeys, multi-clone scopes and harness
filters. Test cursor tampering/foreign scope, head change, expiry, cancellation,
missing nodes and selected-body mismatch. Assert node/byte/body budgets with
fixed page size while unrelated archive entries grow from 10k to 100k.

### PR 5: local merge and selection model

Primary touchpoints: `internal/cli/handoff_select.go`, `handoff_match.go`,
`handoff.go`, `scope.go` and new private page/model helpers. Integrate paged
archive rows into a headless model first, exercising it through real callers
without depending on terminal golden changes. Preserve all existing flags.

Use pinned exact identity lookups for relevant local registrations, then merge
their observed activity and bounded labels with remote pages. Carry unknown
counts/eligibility honestly. Provide selected-body hydration through existing
handoff verification; never pass a row projection as an authoritative metadata
body. Retain local-only behavior on archive failure with the current diagnostic.

Tests: local-only, archive-only, both, archived identity beyond page one, local
activity newer than archived activity, zero-prompt archive gaining a local prompt,
missing native input, source flags, known repository conflicts, duplicate identities
on later pages and bounded native label reads. Verify selected deletion/rewrite,
policy refusal and cancellation before source materialization.

### PR 6: shared browser page state and lazy search

Primary touchpoints: `internal/cli/scope_browse.go`, `list_browse.go`,
`session_catalog.go`, `browser.go`, `browser_filter.go`, `handoff_select.go` and
existing Older actions. Share a page provider across handoff/list/bare show,
with per-scope rows, continuation, counts and captured-view state.

Older fetches and appends a remote page; a toggles scopes without silently
mixing views. Default fallback requires proven archive and local emptiness.
Refresh restarts affected page state after explicit stale-view handling.
Selection numbering stays stable for appended rows; a refreshed order is shown
explicitly. Footer counts distinguish returned, more and exact/unknown totals.

Search initializes the complete scoped/search universe only on demand, preserving
all matcher fields, subagent search, parent grouping and ambiguity behavior.
The current synchronous `search func() []listRow` is insufficient for slow or
failing remote work: introduce caller-connected context/error-aware loading with
progress, cancellation and no silent fallback to searching only loaded pages.
Clearing search restores the valid paged browse view. Archive read contexts must
follow command/browser cancellation, rather than uncancellable Background calls.

Tests: first screen, Older, repeated scope toggles, empty fallback, search past
page one, search initialization failure/retry/cancel, child results, selected
hydration, terminal interruption and resource closure. Update affected goldens,
CLI reference and user docs only where behavior actually changes.

### PR 7: acceptance evidence and tracking

Add real CLI.Run first-screen and Older operation-count controls using synthetic
10k/100k archives, with shared fixtures outside measured time. Use the same repaired
harness on baseline and final candidate. Measure cold/warm/changed runs, latency,
requests, wire bytes, allocations, peak memory and local registration work;
separate archive and native costs. Do not count setup or a skipped/timed-out case
as passing acceptance.

Run focused package race tests, the current CI tiers and policy checks. Catalog
and configuration changes may select full platform backstops under the new tier
planner; retain exact source/body evidence. Use Go 1.27.2 and the synthetic Env/
store/home fixtures described in [testing](../contributing/testing.md). No real
homes, native settings or production buckets are used for implementation tests.

Update listing, CLI performance, session-finding deviations, migration/provider
docs and CHANGELOG with final query eligibility and compatibility costs. Runtime
provider qualification runs only in explicitly isolated targets and remains a
separate release gate, with endpoint-scoped evidence and capability code. Record
failure/uncertainty honestly rather than deleting gates to make rollout green.

## Parallel work and merge order

Keep one owner of the plan, version allocation and integration branch. After
PR 1 freezes contracts, separate engineers can prepare query/UI fixtures and
benchmark oracles while the writer owner builds PR 2. Upgrade and reader work
can proceed separately after PR 2, but PR 4 must prove behavior against PR 3's
actual constructed/updated catalog. Local model and browser code have overlapping
files: land PR 5 before PR 6 rather than having two writers edit those paths.
Qualification preparation can run in parallel; acceptance uses the integrated
final tree. Each PR description states scope, prerequisites, exact tested source,
operation budgets and still-open provider gates.

Before implementation, open the engineering issue required by CONTRIBUTING and
link this plan; the maintainer's request authorizes preparation without another
design approval round. Do not merge obsolete original PRs merely to obtain code
already present in main. Reconcile publication work such as #479 when it lands,
especially permanent capability composition and pending-owner draining.

## Release and definition of done

Source implementation is done when supported first-page queries use complete
versioned ranges; current exhaustive behavior matches across scope/local/source
cases; Older remotely pages; search remains complete; selection retains existing
authority checks; upgrade/crash/GC/old-writer tests and exact-source CI pass.

Production availability additionally requires qualified S3/R2 CAS, same-version
timestamps/clock, actual credential-cutover authority, multi-machine lifecycle
acceptance and full-load/matched performance evidence. Legacy destinations keep
their header-proportional fallback. There is no claim that merging these PRs
alone makes every existing archive fast without a qualified format migration.

Step 6B fragments, broader typed historical/staged/private publication factories,
bounded live Cursor admission and exact historical deletion/Restore remain their
own streams. They are not prerequisites for supported ordinary scoped windows;
shared schema, pending-owner and writer protections must still compose correctly.

## PR 1 frozen reservations and consumer inventory

Engineering issue: [#484](https://github.com/wangjohn/agent-archive/issues/484).
This slice reserves, but cannot construct or activate, head protocol 11/schema 5,
`Picker: {Version: 1, RowSchema: 1, Capabilities: ["scoped-picker-v1"]}` and
configuration `{version: 14, writer: scoped-picker-v1}` with permanent
`scoped_picker_protection`. Schema 13 belongs to staged legacy publication (#479);
configuration 11/12 retain their existing catalog/publication meaning. The coordinator reservation is protocol 11 with the same exact Picker descriptor;
current coordinator decoders refuse that protocol and unknown descriptor fields.
Activation must change both head and coordinator under the seal, because older
direct source/immutable writers check coordinator admission before writing and
a head marker alone cannot fence them. These are independent version domains. No configuration save emits 14 in this slice.

The only operationally accepted head remains protocol 10/schema 4. The shared
head JSON decoder checks live and retained heads, refuses mixed/unknown versions,
capabilities and descriptors, and explicitly refuses even a valid reserved head.
Configuration decoding refuses the reserved fence and any scoped protection
marker on an older fence. All existing protection fields and authorization floors
retain their codecs. This prevents a current writer from flattening a future
capability or using old GC traversal against a new head. Ordinary admission and
journal transfer observe the head before authorizing source/index writes, adding
one bounded head observation per admission. This is safety overhead, not a
startup performance improvement. A valid protocol-10 pending owner admitted
before an upgrade remains protected by the existing durable coordinator seal;
PR 3 must drain it before activation. No decoder-only change makes published
older binaries support protocol 11: their exact protocol/schema checks and the
configuration writer fence must both remain installed during cutover.

| Consumer | Existing authority and reservation impact |
| --- | --- |
| `catalog.go`: Head/readHead, Find, Commit, PutImmutable | Shared strict head decoder; admission observes head before immutable/index writes. |
| `store.go`, `publication_claim.go`, `journal_publication.go` | Ordinary source admission and exact frozen journal transfer check the head before writes; existing admitted owners retain coordinator protection. |
| `snapshot.go`, `delta.go`, `internal/reader/remote_catalog.go`, `catalog_selected.go` | Snapshot opens/freshness observations go through Head; old exhaustive summary and selected-body authority remain unchanged. |
| `coordinator.go`, `coordinator_validate.go`, `migration_intent.go` | Admissions, completion, durable barriers and GC released-head validation remain protocol 10; released-head JSON uses the shared decoder. |
| `gc.go`, `gc_recovery.go` | Live, predecessor and recovered released-head decoding all refuse the reservation; no new roots can be silently omitted from reachability. |
| `migration.go`, `migration_intent.go`, `internal/cli/migrate.go` | Migration state/proofs and constructed heads remain protocol 10; no upgrade/activation route is supplied. |
| `internal/state/catalog_journal.go`, `publication_unknown.go`, `internal/local` catalog journal ownership | Frozen protocol-10 input and exact replay only; unknown pending protocols still refuse. |
| `internal/config/wire.go`, `discovery.go`, `durable_storage.go` | Existing schema 11/12 composition, lower floors and rollback protections remain unchanged; 14 and its marker refuse. |

Before upgrading, seal real destination admission, finish or explicitly resolve
all direct immutable/source/commit, pending journal, migration and GC owners,
including unknown-outcome acknowledgements and completed journal receipts that
still await exact local replay. An unrecognized or unresolved frozen owner is a
refusal, never permission to reinterpret its bytes under protocol 11. Retain the
seal through complete construction, validation, activation and durable config
save; never roll back the permanent fence. PR 3 owns executable upgrade proof.

### Frozen key and row contract

Project-tree keys are exactly
`!picker/v1/<membership>/<scope-kind>/<scope-hex>/<harness-tier>/<order-key>`.
Membership is `browse` or `handoff`, both ordinary non-replay roots; children and
replay queries retain complete-summary compatibility until explicitly indexed.
Scope kinds are `global`, `repo`, `project-any`, `project-missing`; global scope
uses `-`, others use lowercase hex of exact UTF-8 scope identity bytes. Harness
tier is `all` or lowercase hex of the canonical harness bytes. No raw path or
label appears in a key. Scope identities are nonempty and at most 512 UTF-8
bytes; canonical session identity keys are at most 1024 bytes; keys are at most
4096 bytes. Exceeding a structural bound refuses activation or selects explicit
compatibility before indexing; it must never truncate membership identity.
Ranges use the full prefix including the final slash and its exclusive lexical
successor; no prefix without that boundary is a scope match.

Canonical harness filtering must preserve the original harness/session tie
discriminator used by the exhaustive caller oracle; aliases cannot silently
change identity ordering. Order keys reuse `orderKeys` activity/capture timestamp formatting and
`descendingIdentity(rawMetadataObjectKey)` exactly; range traversal uses
the existing descending activity/capture and identity tie comparator. `MetadataObjectKey` preserves the exact raw harness
spelling; canonical harness normalization is only filter/group authority, never
permission to substitute an exact source identity. RepoKey
membership includes only validated nonempty RepoKeys. Project-any includes every
matching ProjectID; project-missing includes only empty RepoKey. Global + project-any + either repo or project-missing, times two harness tiers
and two memberships, is at most 12 memberships per session. Handoff excludes
known-zero prompt roots but retains unknown counts. Generate new and removed
memberships from full verified mutation metadata. Existing overflow projections
can omit RepoKey and prompt facts: hydrate verified old metadata before removal,
or preserve exact membership facts in the PR 2 codec. Never guess from projection.

Row schema 1 is a strict small JSON projection bounded to 8192 encoded bytes.
Fields are schema, canonical harness/session identity, metadata ObjectRef and
revision, ProjectID, RepoKey, activity/capture timestamps, name/title/project
labels, original harness identity plus canonical harness filter identity, root/replay classification, ordinary/replay child counts and prompt
count state (`unknown`, `zero`, `positive`). Labels are UTF-8 safe prefixes of at
most 256 bytes each; truncation never changes other fields. Counts are nonnegative
signed-64-bit integers; unknown prompt state carries no count, zero carries zero,
positive carries a positive count. Metadata reference/hash, identity, revision,
ordering, scope and eligibility must agree with verified metadata. Live indexed
child aggregates are validated against index authority separately; they can
change without rewriting the immutable parent metadata or its revision. Unknown fields,
versions, invalid UTF-8, excess bytes and mismatches refuse. Rows contain no source
bodies, transcripts or native paths and are never full-metadata authority.
Operational row decoding/refusal belongs to PR 2's consumed codec and PR 4's
reader; PR 1 does not add unused exported row types or a partial catalog producer.

### Frozen caller, cursor and cost contract

Typed supported requests specify scope union (global, repository RepoKey plus
missing-RepoKey configured ProjectIDs, or compatibility ProjectID set), membership,
canonical harness filter, activity order and page size 1..100. Deduplicate exact
ranges and support at most 32; excess ranges, named-project union and arbitrary
predicates use the complete-summary path explicitly. The typed page contains
rows, proven exact/unknown total, more, continuation and captured view. Identity
lookups share that view and never infer archive absence from a page. Refusal,
unknown local eligibility and cancellation cannot prove emptiness.

An opaque integrity-checked continuation binds normalized exact scope IDs,
membership, harness, ordering, head protocol/row format, all captured roots,
per-range unconsumed positions, original command nonce and original deadline.
The token has a maximum 64 KiB encoded size; oversize state refuses continuation.
Changed head, foreign command, changed query or expiry requires explicit refresh;
refresh discards continuation and never resets an older token's lifetime. Page
return and selected-body loading retain current freshness/provenance checks.

One fresh bounded head observation per logical page/identity batch replaces
per-range observations only through PR 4's caller-connected batch contract.
With S ranges (<=32), P rows (<=100), H actual tree height (<=64), one lookahead
per range, and 128 KiB nodes, the provisional conservative traversal bound is
O(S*H*(P+1)), plus bounded head/coordinator observations. The existing tree allows
underfull/unary branches, so O(S*H+S*P) amortization is a target requiring an
actual traversal/occupancy proof, not an established hard ceiling. OpenSnapshot
also observes Coordinator.Active (coordinator object limit 4 MiB), in addition
to the head (16 KiB). PR 4 must derive and enforce concrete request and wire-byte
ceilings against its actual walker, counting all observations and lookahead,
and return explicit budget refusal without claiming emptiness or a complete page.
Inline rows are included in node bytes; first-page archive metadata-body and
transcript reads are zero. Local registration work R, identity lookups, local
activity overrides and attempted native label reads are reported separately;
PR 5 may require bounded R exclusions/lookups to establish the correct union
page. Do not claim a strict P-only bound with unlimited R.

Local merge identity is canonical harness plus session. Preserve existing tie
provenance: archived activity/capture/order rank survives local activity override;
local-only ties retain registration ordinal. A local override can move either
forward or backward, so remember its identity across remote pages in both cases.
Source selection and selected-body authorization retain existing semantics.

Local identity lookups are not filtered by page scope or handoff eligibility: an
uploaded identity may lie outside page one, be a child/replay, or have zero prompts.
Unknown lookup results never mean not uploaded. Preserve source=archive activity
overrides while excluding local-only and locally revived zero-prompt rows. Selected
pinned reference/provenance must survive picker-to-resolver handoff; returning
only harness/ID and substituting a newer same-ID publication is not permitted.
Local label limits currently bound accepted rank, not attempted reads; PR 5 must
count attempts including failures and expose partial/unknown state on its explicit
finite work budget. Complete archive search retains all matcher fields and child
parent grouping through context/error-aware loading, with no loaded-page substitute
on failure.

### PR 2 consumed mutation and row codec

The candidate protocol-11/schema-5 mutation path now derives `CatalogEntry.Picker`
from checksum-verified full metadata, maintains its scoped Project memberships in
that mutation's existing head CAS, and updates parent child aggregates in every
copy without changing the metadata revision. `BuildPickerRow` and
`PickerMembershipKeys` are shared construction helpers for the upgrade owner.
A scoped candidate refuses a preexisting live identity missing its facts;
construction must build the complete universe before activation. Protocol-10
mutations refuse supplied Picker fields and continue emitting the old envelope.
Head and coordinator operational decoding still refuse protocol 11; this slice
cannot activate a new destination or bypass provider qualification.

Picker Project values use the existing strict `CatalogEntry` envelope with its
`metadata-v10` compact Summary and a bounded, strict Picker member. Identity and
other order entries preserve the same facts during automatic overflow compaction.
`DecodePickerRow` consumes this envelope in Project snapshot decoding and GC,
validates exact scope/harness/membership/order correspondence, and reads no body.
The Picker member is at most 8192 encoded bytes; each UTF-8 label prefix is at
most 256 bytes; the complete compact picker envelope is at most 16384 bytes.
The row schema is recorded in `schemas/catalog-picker-row.schema.json`; runtime
also enforces byte limits and exact immutable checksum keys. Selected body
verification recomputes metadata-derived facts while retaining separately
validated live index child counts. A projection never authorizes source loading.
Required row facts must be explicit and non-null, including empty labels and
zero child counts; unknown prompts omit the count field. Selected picker rows
match the exact compact projection of their pinned identity even when that
identity fits inline, then gain full-body provenance only after verified hydration.


### Canonical association lookup copy

Every live scoped-format identity, including child/replay/known-zero roots and
entries without ProjectID or RepoKey, additionally receives one Project leaf:
`!picker-identity/v1/<canonical-harness-hex>/<session-id-hex>/<raw-session-key-hex>`.
Hex is lowercase encoding of exact UTF-8 bytes. Harness group identity uses the
existing `CanonicalHarness`; the session ID and raw metadata-object-key suffix
retain exact identity. The raw metadata object key is at most 1024 bytes; the
complete lookup key is at most 4096 bytes. Structural identities are never
truncated. `PickerIdentityRange` validates nonempty safe identity components and
returns the full prefix ending in `/`; its exclusive lexical upper bound replaces
that trailing slash with `0`. This is one additional copy beyond the maximum
twelve ordinary browse/handoff memberships (thirteen total picker copies).

`PickerIdentityKey` and the same strict compact CatalogEntry/PickerRow decoder
serve construction, mutation, snapshot and GC. Insertion, rewrite, deletion and
parent aggregate updates maintain the lookup copy with all other roots in the
same candidate CAS. Default Project ranges remain `project/..project0` and do not
include these copies. Operational protocol-10 writes still emit no Picker fields,
and protocol-11 activation remains refused in this slice.

A canonical group may contain multiple physical raw-harness aliases. They remain
separate entries with their exact pinned revision/reference/source identity;
lookup must never silently choose or deduplicate them. The bounded reader counts
this exact prefix: zero proves absence, one yields a unique pinned entry, and more
than one returns explicit ambiguity while fetching at most two rows as evidence.
PR 4 owns that API and its captured-view/request budgets; PR 3 must construct and
validate every additional lookup copy before activation.
