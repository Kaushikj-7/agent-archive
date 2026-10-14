# Senior review of the Muse integration plan

Reviewed 2026-10-14 against repository HEAD
`7d8aee13c7146ceb38d15df7188d7daa31f464eb` and the uncommitted Muse
[implementation plan](muse-integration.md), [native validation report](muse-plan-validation.md)
and [probe evidence](muse-plan-validation.json). No production Muse code exists;
this is a design/readiness review against current interfaces, not implementation
acceptance. Changes below have been incorporated into the plan.

The later [qualification packet](muse-qualification-readiness.md) records native
1.4.8 observations and work sequencing. It does not establish implementation
acceptance or resolve the independent frame integrity contract.

## Assessment

The integration fits the existing architecture. Ordinary file providers, injected
native codecs, one Analysis, shared registration, frozen publication and existing
readers are the right foundation. The native probes establish useful feasibility.
They do not yet establish production readiness or complete native-format coverage.

The previous plan was insufficiently concrete at several authority and resource
boundaries. Without correction, an implementation could be functionally successful
on small probes while choosing an unsafe source, starving older sessions, missing
active children or overstating completeness. The revised plan addresses those
contracts and limits the initial scope. Stage 0 still needs reproducible fixtures
and supported native framing/start authority before the corresponding capture
lane can be enabled.

## Findings and changes

### R1 — P1: Locator resolution lacked an enforceable admission/confinement contract

The plan proposed a bounded lookup but did not specify its durable owner, update
conditions, native binding or opened-source validation path. The collector's
[source admission construction](../../internal/collector/source.go) currently
returns no native admission facts for non-Codex agents, and its confinement
wiring specializes in Codex/discovery. Merely adding a Muse provider or setting a
path does not make those protections automatic. The hook
[batch contract](../../internal/capture/batch.go) permits only file sources and
rejects locator keys, while file collection refuses an empty path.

Consequence: a resolver could race setup/removal or adopt a conflicting file at
the same path, then filter/publish before discovering identity inconsistency.
A second pending queue would also duplicate existing registration ownership.

Correction: keep the empty-locator registration as the durable pending work item.
Use a single approved data root and pass-owned lookup. Persist generic source
binding facts and resolve/update under a short compare-and-policy-checked
registration lock. Reuse the existing signature/snapshot admission interfaces with
Muse expected owner/root/start facts; validate them on the confined descriptor
used for filtering. Extend current collector wiring explicitly. No new source-kind
service or separate publication system is required.

Gate: real registry flow with null locator, restart on both sides of commit,
replaced file, symlink/root escape, mismatched stream and concurrent
pause/setup/project/destination changes. No unsafe candidate may reach filtering
or PUT. Frozen retry and retained refilter must still work without native files.

### R2 — P1: Bounded scans could still starve sessions or allocate entire directories

“One bounded enumeration per pass” did not require forward progress across ticks
or process restarts. Restarting at the first date folder can leave later sessions
unresolved forever. [discoveryio.Walk](../../internal/discoveryio/walk.go) calls
whole-directory `ReadDir` before applying its candidate limit, so that limit alone
does not bound allocation. Repeating scans for each unresolved registration would
scale with both session count and store size.

Correction: one shared scoped traversal with bounded directory acquisition,
compact charged observations and a resumable content-free cursor. Charge unrelated
entries too; reserve forward progress beside retries. Reuse the existing
[bounded directory/cursor worker](../../internal/discovery/discovery.go) through a
narrow filesystem boundary, without coupling Muse to Codex's automatic admission.
Eviction/partial enumeration cannot establish uniqueness or absence. Known bound files can proceed through
validation while unrelated inventory work remains pending. Native SQLite indexing
is deferred until filesystem measurements justify it.

Gate: a store larger than one pass budget, late entries, restart, unreadable
folders, cache eviction and root changes. Assert eventual coverage and directory/
header operation counts, not only elapsed time. Restore existing 1/128/512 and
full-size fixtures; measure combined-agent memory under the existing pass reserve.

### R3 — P1: Long-running and independently identified children needed a real lifecycle

The plan described independent IDs but left candidate discovery and admission too
close to the existing stop-based path. Muse repeated stop hooks, and children can
outlive a root. Parent absence also needs a persisted own-child indication:
[NativeChild validation](../../internal/archive/types.go) currently accepts only
Codex, while the older [child hook path](../../internal/capture/subagent.go)
constructs a parent-derived ID and the [child collector](../../internal/collector/subagent_capture.go)
expects Claude provenance/end bounds. The existing
[native parent linker](../../internal/collector/native_links.go) inventories only
Codex parents, omits agent from its lookup key, and hard-codes Codex removal checks.
Simply broadening its parent inventory could create cross-agent links.

Consequence: an active or interrupted child could never enter collection; an
unresolved parent could turn a child into a displayed root, or inherited parent
permission could admit content outside the child's actual scope.

Correction: inspect bounded spawn/binding/observer facts during ordinary root scans
as well as hooks. Admit proven children as independent normal registrations and
poll them through the existing collector. Generalize own-child validation and
native evidence deliberately, preserving Codex legacy checks. Use actual session
and agent bindings rather than display depth/path; preserve unavailable links and
avoid synthetic root fallbacks. Reuse the native linker with agent-qualified keys
and removal checks, keeping Codex legacy migration agent-specific. Queue durable
link work before marking it complete. Terminal attempts and final-response hints
do not permanently close a stable child conversation.

Gate: no-stop active child, lost stop, root exit, reopen, reuse of an agent ID with
a new session, forwarded records, contradictory ancestry, missing parent and
isolated worktree. Reject cycles/self-parenting, and collide native IDs/root
spellings across agents. Verify exact own scope, one owner and idempotent link
status through publication/read-back, even without new parent transcript bytes.

### R4 — P1: Supported framing, retained representation and completeness were underspecified

Native forks include timestamp-less copied records and omitted-live-only markers.
The corrupt-frame probe's export succeeded with an unparseable-line diagnostic.
Actual checkpoint production and checksum encoding remain unqualified. The earlier
plan also left room to confuse native checksum validation with integrity of
privacy-redacted retained bytes.

Correction: qualify frame/marker versions and integrity before enabling their
capture lane. Flatten validated input into a deterministic safe retained format
with position provenance; refilter that representation without replaying native
checksums over changed bytes. Separate optional-content omissions from unsupported
framing/authority, pending final records, limits and privacy redaction in an explicit
failure table. Unknown authority preserves prior verified publication rather than
acknowledging a partial new source. Never disable Muse compaction or observers just
to make archive acceptance pass.

Gate: reviewed native/derived fixture provenance, known valid/corrupt frames,
markers/copied prefixes, unqualified checkpoint/rewrite and exact availability/gap
assertions. A “root-only production” label still requires these guarantees for
its advertised native mode; ordinary small-session probes are insufficient.

### R5 — P2: The evidence campaign was not yet reproducible acceptance

The evidence manifest stores result labels and hashes of scripts that remain in
temporary storage. Raw logs are not a reviewed fixture corpus. Several original
probe assertions were corrected, and periodic checkpoints were not exercised.
These are legitimate research observations but cannot serve as repeatable CI
acceptance after temporary files disappear. The report also incorrectly labeled
the campaign arm64; the manifest, current runtime and executable agree on macOS
x86_64. The report is corrected and other architectures remain unqualified.

Correction: make a checked-in runnable isolated probe, exact expected assertions,
reviewed native fixtures and provenance the first implementation deliverable.
Distinguish historical failed attempts, corrected assertions and unexercised paths.
Sanitizing native frames invalidates their checksum; derived fixtures must not be
used to certify original integrity encoding. The existing fourth-agent tests remain
extension evidence, not Muse-specific authority/privacy acceptance.

Gate: another run from fresh roots reproduces every claimed scenario without
personal context dependency or paid/external provider calls; failing or skipped
assertions cannot be recorded as passed. Native provider/platform acceptance stays
separate from the loopback corpus.

### R6 — P2: Optional capabilities and generic abstractions risked unnecessary scope

The current [discovery configuration](../../internal/config/discovery.go) has an
agent field in authorizations but uses `CodexHomes` and Codex-specific
reconciliation. Reusing it for Muse is additional work, not an implemented generic
service. Multiple native homes, a native SQL catalog and a new prefix inspection
API were also suggested before performance need was established.

Correction: the initial baseline is included-project hooks, proven children,
explicit backfill, local access and existing reader/handoff surfaces. Resolving
an admitted session is not admitting an unknown historical session. Automatic
missed-start admission is a separately designed optional follow-up; keep its port
absent initially and never inherit Codex consent. Start with one reviewed root,
file sources and existing budgeted header purposes. Preserve the first-record-only
fast path rather than silently broadening it.

This is a deliberate scope clarification relative to the original aspirational
matrix. It retains the existing Claude/Cursor tracking baseline and the future
Muse discovery goal, without making a Codex-specific feature a prerequisite for
a sound fourth-agent integration. Full baseline parity still includes all claimed
child, reader, privacy, storage and platform guarantees.

### R7 — P2: Rollout, writer compatibility and observability lacked release gates

Source roots and child bindings become persisted local authority. Old writers
could otherwise discard those facts during refresh or rewrite. The plan also
needed to distinguish a configured integration from a stalled/unsupported one.

Correction: explicit setup opt-in, no refresh-driven activation, schema/writer
protection with authority changes, an actual tested old-reader access/upgrade
matrix and old-writer refusal/preservation. Use existing transactional setup and frozen journals, fixed
status reasons, bounded retries and diagnostics. Removal preserves native state,
archives and pending ownership; rollback cannot erase authority to make old code
run. Optional replay-managed hooks must report all owned files and preserve foreign
managed configuration.

Gate: upgrade/downgrade with registrations, imports, setup and publication journals;
privacy refresh without native files; explicit removal and pending recovery. A
failure must leave recognizable local state and the last verified publication.

## Kept simple on purpose

- One scheduled collector, one storage pipeline, one registration owner and one
  shared statistics rollup. Child metadata contains own metrics; no persisted
  tree-total counters or duplicate aggregation engine.
- Ordinary bounded file snapshots and existing changed-source signatures. No
  raw-offset tailer or incremental metrics database before profiling and proving
  checkpoint/refilter correctness.
- Compact native facts and graph observations under current memory reservations.
  The [native read budget](../../internal/agentapi/read_budget.go) does not bound
  total disk bytes or process RSS; those need separate measurements/limits.
- Metadata-only skills remain installed evidence until eligibility/invocation is
  qualified. Unknown usage/pricing/errors do not become zero, success or full
  completeness.

## Coverage and readiness

Reviewed all substantive planned surfaces: setup/removal, hook admission, locator
and source reads, framing/privacy/refilter, normalized analysis, child ownership,
metrics/stats, import/local discovery, optional automatic discovery, handoff/skills,
replay, storage/recovery/retention, platforms and upgrade/resource gates. Existing
native and fourth-agent checks were reused as scoped evidence; no production Muse
behavior was invented from their success.

These focused checks passed for the review, with Go build cache redirected to
writable temporary storage:

```sh
go test -short -count=1 ./internal/sourceio ./internal/transcriptio ./internal/discoveryio
go test -short -count=1 \
  -run '^Test(Native(Link|LateParent|ParentRepair|FrozenParent)|LegacyNativeChild)' \
  ./internal/collector
go test -short -count=1 \
  -run '^Test(OnlyCodexIsRegisteredAndEnumerationIsBoundedAndCancelled|ConflictingSourceDoesNotStarveLaterDirectoryBatches|UnavailableDirectoryRetriesNextPassWithoutStarvingBacklog|BoundedScanContinuesFairlyAcrossActiveAndArchivedSources)$' \
  ./internal/discovery
go test -count=1 ./internal/doclinks
git diff --check
```

They validate current reusable source/snapshot behavior, bounded traversal/retry
fairness, link/late-parent recovery and Codex legacy child ownership, plus
documentation. They do not implement or qualify the new Muse contracts. Read-only runtime/executable inspection
confirmed macOS x86_64. Inspection of 23 existing permission transaction frames
did not establish their checksum encoding; no native integrity pass is claimed.
The relevant native executions were not repeated without a changed hypothesis.
Native creation across copies/forks/checkpoints, checksum encoding, real usage,
descendant recovery, skill/MCP and native arm64/Linux qualification remain open.

The revised plan is ready to implement its qualification/foundation stages after
approval. It is not evidence that full production Muse parity has already been
achieved. Capture lanes must pass their stated authority and acceptance gates
before enablement; no performance claim is made without the specified measurements.
