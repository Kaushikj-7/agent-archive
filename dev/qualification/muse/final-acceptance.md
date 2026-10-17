# Final Muse acceptance evidence

The final campaign has **not run**. The reusable protocol in
[`muse_final_acceptance.py`](../../../scripts/muse_final_acceptance.py) is an offline
harness, not evidence that Muse capture, a child mode, billing, or a platform is
qualified. Capture remains dormant. This document records the next campaign's
requirements; it grants no native, provider, setup, release, or deployment authority.

Run the bounded offline regression tests without a native binary or credentials:

```sh
python3 -B scripts/test_muse_final_acceptance.py
python3 -B scripts/muse_final_acceptance.py catalog
```

The catalog is a canonical inventory rather than a list supplied by the campaign.
It inventories specified boundaries; its count is not a parity score.
Root scope marks child and surface requirements UNCLAIMED. Every other absent case
is UNVERIFIED. Full scope requires the entire union. Failed, skipped, timed-out,
cross-built, reduced-scale and unavailable cases cannot supply a PASS. Each
assertion checker must itself receive independent source review and must exercise
the existing implementation's discovery, source/filter/parser, Analysis, storage,
reader, and launcher boundaries. This harness introduces no native decoder.

## Reviewed packet and execution

Before execution, freeze a packet with these version-1 fields:

| Field | Required content |
| --- | --- |
| `source` | Exact combined Git `source` commit and `tree`; the shared source guard checks tracked bytes, kinds and modes, including hidden edits, and untracked code. |
| `scope` | `root` or `full`; scope is not a release authorization. |
| `catalog_sha256` | Hash emitted by `catalog`; missing requirements remain visible. |
| `budget_limits` | Existing pass ledger 268435456, cache reserve 100663296, session ceiling 167772160 bytes; raising them refuses the packet. |
| `budget_files`, `schema_files` | Lists of absolute regular `path` and `sha256` pins inside the exact combined repository. Include the authoritative ledger/collector/archive limit files and every current and historical schema being tested. |
| `binaries` | `archive`, `producer`, `old_reader`, `old_writer`, each with absolute regular `path`, `sha256`, exact `version`, and `build_source: {source, tree}`. Archive source matches the combined candidate; old builds differ. Producer provenance may identify its upstream source/build. Independently verify build provenance; a JSON claim cannot establish it. |
| `helpers` | Named absolute regular `path`/`sha256` pins for the interpreter, checker and full imported helper closure. The exact command's executable must be pinned. |
| `configuration` | `isolated: true`, `roots` mapping each of `home`, `archive`, `config`, `data`, `cache`, `state`, `tmp`, `storage` to its same case-relative directory, and `files` containing pinned reviewed configuration bytes. Provider cases additionally need separately reviewed `provider_authorized: true`. |
| `cases` | Unique canonical `id`, matching `kind`, canonical `assertions`, literal `argv` array, finite positive `seconds` up to 120 per invocation. The aggregate case deadlines are at most 1800 seconds. Divide a larger campaign into separately reviewed packets; reconcile all evidence on one exact candidate before claiming completeness. |

`{case_root}` in argv expands to the new owned case directory. The runner creates
private isolated HOME/XDG/archive/temp roots and empty credential files, constructs
an environment rather than inheriting one, and removes the old probe's dummy
provider credential. It never resolves personal credentials or copies configuration
into place. A checker must explicitly consume pinned configuration paths from its
reviewed argv; pinning a file does not prove that the product used it. Its isolated
runtime/storage setup belongs to that checker's reviewed procedure. No provider
credentials are inherited. Design a separate isolated bounded provider transport
before scheduling real provider cases; do not put credentials in a packet or argv.

After independent review and explicit execution authorization for the packet:

```sh
python3 -B scripts/muse_final_acceptance.py run \
  --packet /absolute/reviewed-packet.json --repository /absolute/combined-checkout \
  --output /absolute/new-owned-evidence-directory
python3 -B scripts/muse_final_acceptance.py verify \
  --packet /absolute/reviewed-packet.json --repository /absolute/combined-checkout \
  --output /absolute/new-owned-evidence-directory
```

The runner reuses the existing bounded `muse_qualification` supervisor and source
proof from `full_race`; both are part of the helper closure. Each child has an
owned process group, captured-stream caps, deadline, and TERM/KILL/reap/group-absence
checks. Failure and cancellation retain packet, raw stdout/stderr, process receipt,
source checks and partial report. Keep failed evidence; use a new output directory
for a retry. The complete campaign's wall clock also includes source verification
and bounded cleanup; a deadline does not mean a descendant may survive it. Checkers
must not daemonize or escape the owned process group.

## Checker output contract

A checker writes exactly one JSON object to stdout, retaining raw artifacts beneath
its case directory. It emits `version: 1`, canonical `case`, matching `kind`, the
exact `source` object, `assertions` mapping every canonical assertion to `PASS`, the
catalog's `facts` object, and a nonempty `artifacts` array. Each artifact has a confined
relative regular-file `path` and lowercase `sha256`. A PASS word, exit zero, or
caller-edited summary alone is insufficient. Duplicate JSON keys, nonfinite values,
unsafe paths, symlinks, changed hashes, missing obligations, skipped terminals and
incomplete process cleanup refuse acceptance.

Additional obligations:

- Publication: `entities` has a unique nonempty `archive_id` and exact case `role`
  for each independently checked root/direct/workflow/observer/grandchild entity.
  Its `stages` contains `source_put` and `metadata_put` with distinct object `key`
  and artifact `body`; `checksum` contains `source_sha256` and `metadata_sha256`;
  `readback` contains source/metadata artifact references with matching hashes;
  `ack` and `status` reference hashed evidence. All referenced bodies are in
  `artifacts`. Checkers verify actual publication order, ownership, identities,
  source sets and status semantics through the existing product pipeline.
- Lifecycle: canonical facts require frozen bytes/destination/owner after native
  source loss, all before/after publication failure boundaries, exclusions,
  protected stages, expiry/link cleanup and undo preservation. Artifact review
  must establish these facts; declaring booleans is not an independent oracle.
- Actual old reader/writer: include `binary_sha256` for the pinned prior binary
  and `schemas` equal to the exact packet schema pins. Retain actual argv,
  refusal/read results and before/after state artifacts. A fixture schema or
  simulation does not replace executing the prior binary.
- Resources: include `measurements` for operations, allocation bytes, disk reads,
  record visits, elapsed seconds, peak RSS, ledger peak and cache peak, with named
  `machine`, exact `budget_files`, `full_scale: true`, and exact `sessions` for
  1/128/512. Full raw/record/source cases need `limits_reached: true`. Warm unchanged
  requires zero full filters, metadata PUTs and store walks; child graph requires
  one parent analysis/read. Cold inventory and four-agent pressure require all
  catalog facts. The checker retains underlying profiles/counters, exact limits,
  fixture dimensions and tools; RSS is separate from charged ledger memory.
- Native: include `execution: native`, matching `target` and actual `platform`
  (`darwin|linux` plus `amd64|arm64`), and the current named `machine`. TUI/exec/serve
  each retain actual fresh/second-turn/resume/failure/exit/signal/hard-kill,
  fork/rewind/side/recovery, compaction and disabled-logging evidence. A cross-build
  or synthetic output cannot stand in for the actual host lane.

Verification recomputes the matrix and stream/artifact hashes, source/tree,
argv/roots, canonical assertion union, process terminal and cleanup. Its
`protocol_complete` means protocol completeness only. Per-case success is
`PROTOCOL_VERIFIED`; the capability matrix stays UNVERIFIED and
`qualification_complete` stays false until separate independent final acceptance. The trust boundary remains
explicit: hashes bind bytes; they cannot authenticate a manually fabricated
receipt or prove a reviewed checker measured what it claims. Independent checker,
binary-build and raw-artifact review is mandatory. The harness always returns
`release_authorized: false` and `capture_unlock_authorized: false`.

## Inputs still missing for actual final acceptance

1. Reviewed immutable combined PR6–11 heads, actual combined tree, final source-built
   archive binary and build receipt, reconciled docs/capability/schema inventory.
2. Source-pinned case checkers and expected native facts on the final APIs; every
   root and child publication must be independently represented. Prior private
   PR6 pipeline acceptance is design context until rebound and reviewed on this tree.
3. Reviewed exact producer binary/version/source provenance and reproducible
   disposable native roots/configuration. Current local qualification is not a
   substitute for final full-pipeline TUI/exec/serve acceptance.
4. Actual previous reader/writer builds with source provenance and the exact
   current/prior config, registration, binding, import and pending-journal schemas.
5. Native macOS/Linux amd64/arm64 machines for each advertised lane, including
   actual signal/kill/late-write and compaction behavior. No Linux availability
   or provider authentication was rechecked by this offline harness work.
6. Usable isolated Meta accounting and disposable remote-storage configuration,
   separately approved bounded spend/requests/bytes/lifetime, credential transport,
   cleanup and storage prefix. Personal settings, Keychain and buckets are excluded.
7. Full-size 1/128/512 and boundary fixtures, named machine/counter/profile tooling,
   cold inventory, warm unchanged, child graphs and combined four-agent pressure
   measurements under current reserves. No budget is increased by this harness.
8. Exact-candidate Test, Levenshtein and Extended checks; independent combined review;
   postmerge validation. Root owns leases, final campaign reconciliation, enablement
   decisions and any later release request.

No native execution, paid call, remote write, setup mutation, release tag or
publication occurs by generating a catalog or running the offline tests.

## Requirement mapping and scope

The inventory maps to the approved proposal's section 4 tables and resource
paragraphs, plus the PR12 acceptance in the orchestration plan. The protocol field
names below are evidence slots for those existing requirements, not additional
product guarantees. Source-pinned independent checkers still own semantic oracles.

| Catalog cases | Existing requirement |
| --- | --- |
| `admission.*` | Admission and roots: no filtering/PUT before same-handle validation; concurrent stop/setup/pause/removal. |
| `privacy.nested_sentinels`, `privacy.exclusion_dependencies` | Framing and privacy nested secret sentinels/no reasoning dumps; storage privacy/exclusion across every referenced source. |
| `storage.frozen_retry_source_gone`, `protected_stages`, `failure_boundaries` | Storage and maintenance: frozen retry after native source loss; protected stages; failures before **and** after local state, staging, source PUT, metadata PUT, verification, acknowledgement and cleanup. |
| `storage.root_expiry`, `storage.child_expiry_links` | Storage and maintenance: root/child expiry, unresolved links and no dangling acknowledgement. |
| `children.*` | Children and totals: independent ancestry/authority, graph conflicts, late parent links without parent bytes, own totals and incomplete coverage. |
| `publication.root/direct/workflow/observer/grandchild` | PR12 explicitly requires actual root and each claimed child's source/metadata PUT, checksum, read-back and acknowledgement; proposal names these four child roles. The role row is not permission to omit additional actual entities. |
| `compat.current_reader_writer`, `old_reader`, `old_writer` | Setup and compatibility: actual current/old reader/writer upgrade/downgrade against config, binding, registration, imports and pending journals. |
| `compat.producer_upgrade`, `future_format` | Native modes and versions: supported producer upgrades/future records and safe refusal of unsupported shapes. |
| `import.*` | Backfill and undo: exact undo preserving later live/acknowledged/native data, restart and mutation-free pre-setup access. |
| `reader.*`, `skills.*`, `handoff.*`, `replay.*` | Readers/handoff/replay agreement, safe bidirectional argv/artifacts, exact forwarding; tools/MCP/skills installed/eligible/invoked/read distinction. Detailed owning-PR tests remain necessary. |
| `docs.schemas_capabilities_versions` | PR12 final docs/schema/capability inventory reconciliation. |
| `resource.1/128/512`, `full_raw_limit/full_record_limit/full_source_limit` | Resource paragraph explicitly restores these cohorts and full current boundaries; reduced tiers are not full evidence. |
| `resource.cold_inventory/warm_unchanged/child_graph/four_agent_pressure` | Resource table explicitly names larger-than-budget census/currentness and valid progress beside retries, zero unchanged work, one parent analysis per changed scan, and combined Codex/Cursor/Claude/Muse pressure/headroom. |
| `native.<target>.<mode>.<action>` | Native modes and versions row explicitly names TUI/exec/serve; fresh, second-turn, known/unknown resume, failure, orderly exit, SIGINT/SIGTERM/hard kill with late writes; fork/rewind/side/recovery; manual/default/periodic compaction and resume; logging-disabled/ephemeral. Four target labels match the existing release targets in `scripts/build-release.sh`. |
| `provider.real_usage_accounting`, `disposable_storage_readback` | Proposal requires bounded real Meta accounting and disposable remote publication/read-back; isolated configuration and separate authorization remain prerequisites. |
| `candidate.test/levenshtein/extended/combined_review/postmerge` | PR12 exact-candidate required checks, independent combined review and postmerge validation. |

The 216 native cells expand only those explicitly listed actions/modes over the
four existing advertised release targets. They are an inventory of observations to
resolve, not 216 assumed supported features, synthetic fixtures, or independent
proofs. A single reviewed actual scenario can supply multiple boundary observations,
but every mapping and raw artifact needs review. An unsupported capability remains
UNVERIFIED/unavailable in qualification; a safe refusal is evidence of refusal,
not evidence of feature support. Root-scoped qualification must review which rows
are applicable to its actual claim; this reusable inventory does not decide a
release scope or waive a missing platform. Full parity must retain all original
acceptance, including detailed scenarios within each broad catalog case.

## Local resource acceptance controls

`internal/cli/muse_final_acceptance_test.go` reuses the root-capture fixture,
synthetic native fixture encoder, real hook queue, builtin Filter and shared
inventory. It does not manufacture admission or replace the concrete leased
source objects. The warm test selects one owner in ordinary CI; its 128- and
512-owner children require `AGENT_ARCHIVE_PERF=1` in a separately bounded
campaign. Full acceptance must require all three exact children (`owners1`,
`owners128`, `owners512`) in the raw test evidence; absent children remain
UNVERIFIED. Ordinary CI selects only `owners1` and does not invent skips.

Cohort setup stages at most 128 unique hook intents at once, matching the
existing queue bound, and drains each chunk through the real collector before
staging the next. Each hook chunk must leave the preexisting owner set unchanged
until admission. One publication union and one 128-tick bound span all chunks;
every final native ID must match the expected cohort exactly and have a durable
admission, source binding and checkpoint. This keeps the 512-owner setup within
the real queue rather than enlarging it.

The warm assertions cover three settled ticks with zero native Filter input
bytes, zero storage PUTs, zero calls through all three listing entry points,
unchanged published files and released shared charges. Initial publication and
forced signature-removal reread are positive controls. The four-agent control
uses real Muse admission alongside ordinary Claude/Codex/Cursor file sessions,
occupies the existing shared pass ledger, checks ordinary-agent growth publication
beside a named Muse shared-pressure deferral while preserving the Muse baseline,
then releases pressure and publishes Muse growth without reuploading or consuming
Filter input from the settled ordinary sources. Existing provider environments
must carry the same observed ledger object.
It adds no reserve or production test port.

These controls report the existing ledger peak separately from process memory.
They cannot observe exact Filter invocation counts, full-record visits, physical
disk reads or child-process RSS. Zero Filter input bytes does not assign zero to
those unavailable counters. Direct process RSS remains the external pinned
`measure_picker_process.py` measurement. Full boundary fixtures, native mode and
platform runs, provider evidence, child entity coverage and exact-candidate final
integration remain separate required gates. These local controls alone cannot
qualify or unlock Muse capture.
