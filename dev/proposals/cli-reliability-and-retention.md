# CLI reliability and keep forever implementation plan

Status: A–G implementation landed on `main`; live release acceptance remains
pending. Reconciled 2026-10-11 against
`b4122ea6694dbe679783b724b6adbfdc11047c0d`. The original plan was prepared
against `d3e525605855`, with implementation branches starting at
`20f86e5f33fde4c720f3ca3554f4a234828bd7a3`. The requirements and baseline
findings below preserve that design context; “confirmed defect” and
“implementation gap” describe that baseline, not an outstanding finding in
current code. No release promotion or live provider/platform acceptance is
established by this record.

Fix the CLI defects identified in the open-source readiness review, complete
revision access and durable import admission, and add an explicit **Keep forever**
retention choice while retaining the **90 days** default. Ship the cleanup safety
fixes first. Keep forever should ship before broader marketing: it is a basic
archive preference, and the existing retention engine already supports disabling
whole-session expiry.

This plan covers the findings from that review, plus the retention changes needed
to make the new choice reliable. It does not establish that no other CLI defects
exist. Confirm each finding against the implementation branch before changing it.
Use the existing [CLI experience plan](implemented/setup-and-cli-ux.md) for prompt conventions
and the [historical recovery plan](historical-backfill-recovery.md) for import
architecture; reconcile their remaining work with current code rather than
reimplementing shipped functionality.

## Delivery sequence

| Change | Scope | Release placement |
| --- | --- | --- |
| A | Protect every live source reference in purge and cleanup recipes | Release blocker; land first |
| B | Checked JSON output, bounded feedback input, safe handoff file replacement | Before broader marketing |
| C | Validation before setup and credentials; cancellation and read deadlines | Before broader marketing |
| D | Retention contract, comparison helpers, migration and downgrade protection | Before enabling keep forever |
| E | Keep forever across setup, pairing, imports, undo and reporting | Before broader marketing; depends on D |
| F | Revision selection across archive readers and CLI consumers | Pull forward from v2; depends on A |
| G | Durable import staging and crash recovery | Pull forward from v2; phased delivery |
| H | Documentation, generated reference and release acceptance | Required for each affected change |

A, B and C are separate reviewable changes. D and E may be split further by
config, setup, pairing and import boundaries, but expose the user option only
when the full retention path is safe. F and G need separate design and acceptance
reviews; avoid holding the confirmed bug fixes for those larger projects.

## Implementation delivery record

Updated 2026-10-11. The integrated code and feature-specific help, generated
reference and user contracts are present on `main`. The table records actual
merge/commit evidence, rather than the state of historical proposal branches.
The [implementation evidence](../maintainers/acceptance/cli-reliability-and-retention-2026-10-11.md)
links owning code, regression suites and exact-SHA CI, and distinguishes those
checks from the remaining live gates.

| Plan scope | Delivered change | Current state |
| --- | --- | --- |
| A | [History-safe cleanup #423](https://github.com/wangjohn/agent-archive/pull/423) | Merged; complete live-reference protection and fail-closed plan/apply checks. |
| B/C | [CLI reliability #424](https://github.com/wangjohn/agent-archive/pull/424) | Merged; checked output, safe file handling, early validation and cancellable bounded remote reads. |
| D/E | [Retention flows #427](https://github.com/wangjohn/agent-archive/pull/427) | Merged; explicit forever, sticky compatibility fence, setup/pairing/import/undo and lifecycle semantics. |
| F | [Revision selection #425](https://github.com/wangjohn/agent-archive/pull/425), implementation `3f4f1aed` and follow-ups | Merged; exact referenced native UUID inventory/selection across supported consumers. Historical open PR heads are not merge evidence. |
| G | [Durable import staging #428](https://github.com/wangjohn/agent-archive/pull/428), implementation `7e417c9b` and follow-ups | Merged; filtered admission staging, native-loss publication, recovery, quota and cancellation. |
| H | [Documentation corrections #426](https://github.com/wangjohn/agent-archive/pull/426), [index reconciliation #431](https://github.com/wangjohn/agent-archive/pull/431), and this acceptance reconciliation | Earlier docs merged; current-head validation and independent review of this reconciliation remain required. Signed-artifact/provider/app/platform acceptance remains pending. |

The exact implementation candidate `db2ccd3ef1490970d9c8476041d8056608ed8c2f`
has successful Test, Levenshtein and Extended evidence, including macOS full,
fuzz and real-systemd. That evidence applies to that SHA; the final documentation
head and any later merged commit need their own checks. The live release
follow-up has an [executable acceptance protocol](../maintainers/acceptance/cli-retention-live-protocol.md).

Resolved implementation contracts:

- Remote HEAD, reads and each listing page receive a two-minute operation
  budget covering the response body and retries; a shorter caller deadline
  wins. Archive-wide scans do not share a single two-minute budget.
- Feedback accepts symlinks to regular files and validates UTF-8. Handoff
  replacement rejects symlink and nonregular output targets and is atomic.
- Forever keeps the 90-day installation default and uses explicit
  `--keep-forever` with internal zero. Configuration protection composes with
  the import-staging writer fence instead of replacing earlier protections.
- Revision selectors are exact native rollout UUIDs referenced by validated
  metadata. Inventory for an archive without selectable history is empty.
  Selection applies to the parent; arbitrary source keys are never accepted.

The latest promoted release checked on 2026-10-11 remains
[v0.1.1](https://github.com/wangjohn/agent-archive/releases/tag/v0.1.1), with
`agent-archive-darwin-amd64`, `agent-archive-darwin-arm64` and `SHA256SUMS`.
No v0.2.0 or Linux release assets were published at that check. Current-checkout
features and Linux source-build support must not be presented as bytes already
available in the pinned v0.1.1 installer. Release promotion, signed artifacts,
live S3/R2, clean-user upgrades, actual app capture and platform acceptance
remain pending in the [release acceptance record](../maintainers/open-source-acceptance.md).

## A Protect preserved history during cleanup

**Confirmed defect:** `internal/purge/purge.go` inventory and apply-time checks
protect the active `source_bundle.key` but omit preserved revision references.
A valid history metadata object can therefore remain readable in principle while
purge deletes one of its referenced sources. The selective cleanup recipe in
`docs/security/archive-cleanup.md` has the same omission.

- Derive the live source set through `Metadata.SourceReferences()`, including
  active and preserved references, in both planning and apply-time revalidation.
- Fail closed when metadata or its reference set cannot be validated. An
  unreadable manifest is not evidence that its sources are unreferenced.
- Recheck the complete live set before each candidate deletion. A source added
  to preserved history after planning must survive apply.
- Keep old-filter cleanup history-aware: currently referenced old-filter evidence
  must first be replaced through the verified privacy publication path.
- Update shell recipes and `scripts/test_purge_recipe.py` together. Preserve the
  documented requirement to stop writers during destructive maintenance; a
  read-before-delete check does not make concurrent writes safe.

Acceptance: active and multiple preserved sources survive unreferenced purge;
an actually unreferenced source is removed; corrupted references block unsafe
deletion; plan/apply metadata changes preserve newly live objects; old-filter
cleanup never strands a live reference. Cover both CLI and recipe behavior, and
retain purge retry/recovery guarantees.

## B Correct CLI input and output behavior

### JSON output failures

**Confirmed defect:** `status --json` in `internal/cli/status.go` uses a terminal
print helper that ignores the output writer's error, and returns success.

Use a checked JSON output path that preserves the existing display sanitization
and JSON format. Audit other machine-readable output paths for the same pattern
and fix equivalent findings in the same change. Return an operational failure
when output cannot be delivered; preserve the CLI's established broken-pipe
conventions where applicable.

Acceptance: a failing writer and a short write cannot report success; successful
output still decodes to the current JSON contract, without added prose on stdout.

### Feedback file handling

**Confirmed defect:** `readFeedbackFile` in `internal/cli/feedback.go` opens the
path before checking whether the descriptor is a regular file. A FIFO with no
writer blocks the command indefinitely.

Reject unsupported file types before a blocking open, and validate the opened
descriptor as well. Use a platform-appropriate nonblocking or race-resistant
open so a regular-file-to-FIFO substitution cannot recreate the hang. Reuse
existing safe file helpers where they satisfy this contract. Retain the 64 KiB
bound and privacy filtering; verify the advertised UTF-8 contract during the
input audit and address a mismatch if found.

Acceptance: FIFO, directory and device inputs fail promptly; regular files still
work; oversized input fails without creating a feedback request; a substitution
between inspection and open cannot block or admit a nonregular descriptor. Test
the promised symlink behavior explicitly rather than changing it incidentally.

### Handoff file replacement

**Confirmed behavior and safety gap:** `writeHandoffOutput` in
`internal/cli/handoff.go` follows a symlink and truncates its target under
`--force`. Failure after truncation can also damage an existing output file.

Align with the safe output contract in `internal/cli/stats_html.go`: reject
symlink and nonregular targets; write private temporary bytes in the destination
directory; close successfully before atomic replacement; clean temporary files
on failure. Keep the default no-overwrite guarantee atomic under concurrent
creation. Use platform abstractions for replacement where required. Follow the
repository's durability conventions when finalizing the file.

Acceptance: default output cannot overwrite an existing or concurrently created
file; force replaces an ordinary file completely; symlink targets are untouched;
injected write/close/replace errors preserve the prior file and remove temporary
artifacts. Verify private permissions on macOS and Linux.

## C Validate early and bound remote reads

**Confirmed defect:** `purge plan` resolves config/storage before rejecting an
invalid mode. An unconfigured installation returns an operational error instead
of the documented usage error, and a configured installation can resolve
credentials for an invalid request.

Move syntactic validation of mode, numeric ranges and required/conflicting flag
combinations ahead of config, state and credential access in
`internal/cli/purge.go`. Keep archive-dependent validation after loading the
archive. Audit command dispatchers for equivalent ordering problems; use shared
helpers only where their contracts match.

Acceptance: malformed requests return usage exit code 2 on an unconfigured home,
without reading credentials, opening storage or mutating state. Valid requests
retain their existing operational error behavior.

**Reliability gap to investigate:** several read commands use
`context.Background()`. Transport connection/header deadlines alone do not bound
a stalled response body. This is a source-review concern, not a reproduced hang.

Inventory remote operations for list, show, handoff, eval, stats and status.
Thread command cancellation through readers/storage, and define documented
operation deadlines suitable for paginated listings and large sources. Preserve
interactive prompt time and existing background collector behavior; avoid one
short deadline for an entire large archive scan. Ensure retry budgets cannot
extend a command indefinitely.

Acceptance: a fake service that stalls headers or bodies terminates within the
chosen budget; interruption cancels reads; normal large reads still succeed;
failed reads do not leave final output files or trigger archive mutations.

## D Define keep forever and protect the setting

### User contract

- New installations continue to default to **90 days**. Existing finite settings
  remain unchanged. **Keep forever** is an explicit choice.
- Forever disables automatic whole-session expiry for sessions owned by this
  installation, including existing owned sessions. It also prevents age-based
  expiry of their local session state through that retention path.
- It preserves the active source and the meaningful revisions referenced by
  metadata. It does not promise an unlimited record of every publication or
  redundant source snapshot; existing revision bounds still apply.
- Superseded unreferenced source cleanup, verified privacy replacement and
  orphan maintenance continue. Explicit purge, import undo and other requested
  deletion remain available. Forever is not immutable object storage.
- It does not restore already deleted sessions or change capture eligibility,
  privacy filters, skill evidence, or project selection.
- Retention remains per machine. Pairing copies the preference at pairing time;
  changing it later does not update other machines. Retired machines do not gain
  a remote cleanup worker through this feature.
- Provider lifecycle rules, manual bucket deletion and lost access can still
  remove or make evidence unavailable. Explain this next to the preference,
  especially for users sharing a bucket with lifecycle rules.

### Representation and compatibility

Recommended on-disk representation: keep `retention_days` as an integer, with
**0 meaning forever** and **1–36,500 meaning finite days**. Reject negative and
out-of-range values. This matches `retention.Options.SessionMaxAge == 0` and
avoids a second policy field that can conflict with the numeric value.

Do not simply expose zero today. Setup normalizes nonpositive values to 90 in
multiple paths; another-machine commands replace zero with the default; pairing
rejects zero; import updates ignore a planned zero. Older setup can silently
convert a forever choice back to finite retention.

Implement shared validation, display, duration and comparison helpers with
forever ordered above every finite duration. Track whether the JSON field is
present: absent legacy configuration gets the historical 90-day default;
explicit zero means forever. Do not apply defaults to a loaded explicit zero.
Separate incomplete draft defaults from committed config semantics. Audit direct
`<= 0`, integer comparisons, `cmp.Or`, and duration conversions across all
retention consumers.

Add a sticky retention compatibility protection using the existing writer-fence
mechanism in `internal/config/wire.go`. Allocate a new supported writer/version
after checking the versions reserved by other work. Older integer decoders and
current protected-writer decoders must reject it before performing setup,
collection or other mutations. The new fence must dominate existing discovery,
generation, history and durable-storage protections while preserving all of
their validation rules. Once enabled, later finite retention must not accidentally
drop the fence. Avoid claiming that a numeric schema bump alone protects against
older code: legacy numeric version handling is permissive.

Check config drafts, backups, setup rollback, registration/publication recovery,
generation recovery and refresh for preservation of the setting and protection.
Automatic recovery must not silently restore a finite policy over forever.
Explicit restoration of older configuration needs a clear retention-change
review. Prove downgrade refusal with representative published decoder/command
behavior, rather than only checking the new decoder. Publish the upgrade and
downgrade limitation.

Acceptance: finite legacy config is unchanged; absent days defaults to 90;
explicit zero survives save/load, refresh, drafts, rollback and recovery; invalid
values fail; every policy comparison handles infinity; fenced configurations
retain all existing authorization protections; incompatible binaries refuse
before writing or deleting. No new remote object schema is needed just to
disable the local sweeper's whole-session expiry.

## E Carry forever through every CLI flow

### Setup and reporting

Add `agent-archive setup --keep-forever`, mutually exclusive with an explicitly
supplied `--retention-days N`. Keep that numeric flag's current 1–36,500 contract;
reject `--retention-days 0` with guidance to use `--keep-forever`. This makes
automation explicit and preserves existing numeric-flag expectations.

Offer **90 days**, **Custom duration**, and **Keep forever** in guided setup;
editing an existing installation starts from its saved preference. Use the
shared renderer and allow a named `forever` response where duration text is
accepted. Update setup reviews, details, saved drafts, refresh and the generated
another-machine command. That command must emit `--keep-forever` for zero.

Display **Keep forever** rather than **0 days** in human output. Add retention
to status in a compatible, additive JSON representation if absent; retain a
documented numeric `retention_days: 0` where that field already exists. Update
schemas and automation examples together.

Finite-to-forever and longer finite changes need no deletion warning.
Forever-to-finite is shortening: preview eligible owned sessions using the same
history-aware meaningful capture age, destination rules and pending-work rules
as the sweeper. Audit the existing setup preview and backfill undo preview,
which use numeric comparisons and cached active capture times. Share the age
decision rather than maintaining a subtly different preview algorithm. Do not
delete during preview; existing transaction locking and confirmation govern save.
Under explicit noninteractive finite flags, preserve the documented approval
model and report the effect rather than inventing an additional permission flow.

### Pairing

Allow explicit zero in `internal/pairing/payload.go` and
`schemas/pairing-payload.schema.json`; preserve it in creation, review and receive.
Install the local retention writer protection before any collector or hook can
run on the receiving machine. Validate rollback and interrupted receive.

The existing v1 receiver rejects zero, which provides a fail-closed compatibility
path. Retaining payload v1 with the expanded numeric contract is reasonable if
compatibility fixtures prove all supported older receivers reject it without
side effects. Give the sender/receiver a useful upgrade message. If that proof
fails, version the payload explicitly; never encode forever as a positive
fallback that an old receiver could treat as permission to expire evidence.

### Backfill and undo

Use infinity-aware ordering in `internal/backfill/commit.go`: finite-to-forever
is an increase, forever-to-finite is a prohibited import-side shortening, and
unchanged forever is a no-op. Let guided retention editing choose forever.
Update dry-run notices and JSON so imported sessions have **no automatic expiry**
under this policy, rather than an unknown expiry.

Persist finite-to-forever changes in the import journal's retention change record.
Undo keeps the current retention by default; `--yes` must continue to avoid
shortening it. Only the existing explicit `--restore-retention` path may restore
a prior finite value, with a correct count of other eligible sessions and the
existing confirmation. Preserve the check that skips restoration when retention
was independently changed after import. Update undo/recovery formatting to avoid
phrases such as “from 0 days”.

### Lifecycle acceptance

Test 90-day expiry and forever side by side with an injected clock: old sessions
expire under finite retention and survive forever. Include active/preserved
references, local state, stopped capture, other destinations and paused/pending
work. Verify superseded unreferenced and verified privacy-sensitive source
cleanup still run in forever mode. Explicit purge and undo must still work.

Cover fresh setup, repeated setup, retention-only editing, refresh, saved drafts,
another-machine commands, S3/R2 pairing, finite-to-forever imports, import retry,
undo and rollback. Test transitions in both directions, especially forever-to-
finite previews at cutoff boundaries and with preserved history. No wizard,
pairing or automatic recovery path may silently replace forever with 90 days.

## F Expose preserved revisions

**Implementation gap:** `internal/reader/reader.go` already supports
`LoadRevision`, but CLI consumers do not expose revision selection.

Design one revision identity/selector contract before adding command-specific
flags. A proposed surface is `show SESSION_ID --revisions` for inventory and
`--revision REVISION_ID` for show, handoff and supported export/eval consumers.
Keep the default active revision and existing session identifier behavior.

Inventory must distinguish active/preserved revisions and provide stable IDs,
capture times and provenance in human and JSON forms. Loading a revision must
use the validated parent metadata/reference set; arbitrary object keys cannot
bypass identity, privacy or integrity checks. State whether revision selection
applies only to the parent or also to included children, and reject ambiguous
combinations until a safe contract exists. Respect existing size/record limits.

Acceptance: each preserved revision can be listed and selected; provenance
matches the selected revision; unknown or unreferenced revisions fail; malformed
history fails safely; default output is unchanged; selection stays consistent
across supported consumers. Update reader/export contract docs and examples.

## G Make imported evidence durable before admission

**Implementation gap:** `internal/backfill/register.go` records native file
locators and queues later collection. Native history can disappear before its
first successful publication. A forever policy cannot retain evidence that was
never durably captured.

Execute the remaining staging scope in step 4a of the
[historical recovery plan](historical-backfill-recovery.md), followed by its
maintenance/compatibility and writer-enablement phases. Existing history readers,
resolvers and recovery features remain the starting point.

- Stage bounded, selected, privacy-filtered evidence before durable admission,
  with identity, destination, selection digest and policy context.
- Atomically link admitted ownership to staging and the publication journal.
  Failed staging must leave the candidate unadmitted.
- Resume publication without native files after admission; verify source sets
  before metadata publication and durable local completion.
- Bound disk, memory and descriptors; process groups with resumable progress.
  Show staged/admitted counts separately from remotely verified counts.
- Keep referenced staging through crashes, retries and pause. Journal cleanup
  for cancellation/removal and release bytes only after verified durable success.
- Apply current privacy policy to retained staged evidence before publishing it
  after policy changes; preserve the historical evidence/provenance contract.

Acceptance: removing native files immediately after admission still permits
successful publication; crash injection at every staging/journal/upload boundary
preserves recoverable ownership and readable prior revisions; quota exhaustion
and cancellation produce resumable progress; dry-run writes no staging. Verify
setup imports, guided/flagged backfill, background work and undo use one contract.

## H Documentation and release gates

Update configuration, setup, CLI help/generated reference, pairing, backfill
contracts, FAQ, glossary, multiple-machine and uninstall/lifecycle guidance for
the new retention behavior. Explain that retained history is bounded and that
provider lifecycle rules can override the preference. Preserve the distinction
between new-install metadata-only skill evidence and legacy body policy.

Fix the review's other launch documentation gaps: FAQ skill-body and project
scope claims, the capability matrix's stale Codex child-capture claim, and
proposal index statuses after reconciling implementation. Synchronize README
and installation release/platform claims with the actual promoted release.
These are documentation corrections, not new feature dependencies.

For each code change, run focused regression tests and relevant existing tests;
then run the repository's required Go/Python/reference/schema checks on a stable
combined head. Use the toolchain required by `go.mod`. The prior review's focused
reproductions, vet and selected race tests passed, but its sandboxed full Go suite
was not a clean release gate because filesystem/umask constraints affected tests.
Obtain a clean CI or supported isolated-environment run before release sign-off.

Complete the signed-artifact, provider, clean-install, upgrade, retention and
uninstall acceptance in [open-source acceptance](../maintainers/open-source-acceptance.md).
Exercise forever and finite retention on disposable S3/R2 archives with the
background collector, including pairing and a finite-policy change. Verify
supported macOS/Linux platforms and each advertised capture integration.

## Scope after this release

Keep per-session pinning, centralized bucket-wide policy, object-lock guarantees,
remote cleanup for retired machines and a durable local-only archive as separate
designs. The first forever release is an installation-wide automatic-expiry
preference. Listing indexes, full-text search and storage/cost reporting remain
useful next steps as long-lived archives grow, but are not prerequisites for
making the preference available safely.
