# Proposals

Proposals are the implementation path for a change: the problem, the options
considered, the decisions, the PR breakdown and sequencing, and the live checks
that show it worked. They are written to get something built, not to describe
the system for the long term; that is a [spec](../specs/README.md)'s job. When a
proposal introduces or changes a format, rule set or lasting behavior, write or
update the spec too, and link the two.

Keep open and partially implemented proposals here. Move a proposal into
`implemented/` when its main scope has landed on the default branch, preserving
it as a design record. Record differences and remaining follow-ups; acceptance
and release gates remain visible even after a design moves. Update incoming and
relative links, and move companion evidence with its design.

Reviewed 2026-10-11 against `b4122ea6`. Statuses describe this checkout, not a
published release or current remote PR activity. Recent merged work establishes
implementation progress; an open plan alone does not establish someone is
actively working on it. Code and user documentation remain the current contract.

Historical-only ownership status reconciled 2026-10-12 after #433 merged as
`18ca0c48`; fragment and combined acceptance work remain open.

## Remaining implementation

| Proposal | Implemented | Remaining |
| --- | --- | --- |
| [Conversation search](content-search.md) | Delivery branches contain engine, worker state, reconciliation, CLI and terminal implementations; final integration/review pending. | Six dependency-ordered PRs cover those components and opt-in background hints/scheduling. Combined acceptance and supported-platform/provider evidence remain required; this is not a merged-release claim. |
| [Historical backfill recovery](historical-backfill-recovery.md) | Diagnostics, portable recovery, rollout lookup, history lifecycle and native child capture; recovery hardening; historical-only ownership and bounded bulk mappings (#433). | [Fragment format foundation](../specs/codex-fragments.md) is proposed with schema checks; decoder/filter/consumer, lifecycle, opt-in import, repair journal and the combined acceptance matrix remain gated. |
| [CLI performance](cli-performance.md) | Synthetic command and historical listing benchmark harnesses with output validation. | Matched repaired-harness measurements and proposed follow-ups. |
| [Backfill transcript performance](backfill-performance.md) | Source-path investigation; no optimization implemented. | Matched backfill measurements, early date exclusion, streaming inspection summaries and bounded verified reuse; read-pass consolidation conditional on profiles. |
| [Archive listing at scale](listing-at-scale.md) | Parallel listing, tracing, bounded metadata-body reads; 10k benchmark and 10k/20k operation-count coverage. | Full 1k/10k/100k measurement matrix and an index that bounds LIST header cost. |
| [Cursor live-WAL admission](cursor-live-wal-admission.md) | Bounded SQLite backend, durable scratch owner, distinct sealed producer and guarded runtime caller under review. | Independent integration review and supported-platform acceptance; current live-WAL admission remains pending. |
| [Cloud capture](cloud-capture.md) | No cloud collector/push or vendor pull implementation. | Proposed, unscheduled; requires producer probes and runtime/authentication design. |
| [Muse Code integration](muse-integration.md) | Native qualification probes (against `7d8aee13`); no production capture. | Single proposed plan covering design, implementation order and acceptance gates; the first release is root capture only. Q0 reconciles the probes with the current base; implementation needs separate authorization. |

[Setup recent import](implemented/setup-recent-import.md) now includes both automatic
seven-day import and the status prompt for existing installations. See the
[complete spec index](../specs/README.md).

## Acceptance and release work

| Record | Status |
| --- | --- |
| [CLI reliability and keep forever](cli-reliability-and-retention.md) | A–G implementation landed; [exact implementation-SHA evidence](../maintainers/acceptance/cli-reliability-and-retention-2026-10-11.md) recorded. Final-head checks/review and signed-release/provider/app/platform acceptance remain open; retain the plan here while those gates are pending. |
| [v0.2 release review fixes](v0.2-release-review-fixes.md) | A/B/C merged in PR519/518/521 with independent source verification and passing exact-source Test/Levenshtein/Extended at `42288a85`; see linked completion evidence. D documentation review/checks/merge, E release-build alignment, final post-D/E candidate campaign and artifact/provider/app/platform acceptance remain pending. |
| [Release remediation](release-remediation.md) | Implementation record; disposable provider/per-app acceptance and release sign-off remain open. |
| [Native name comparisons](../maintainers/native-session-name-comparisons.md) | Partial sandbox evidence; native UI acceptance blocked. |
| [Published-release acceptance](../maintainers/open-source-acceptance.md) | Maintainer runbook for candidate/provider/app checks; implementation does not imply a passing release gate. |
| [Archive catalog performance](cli-performance.md) | Catalog implementation source composed in ROOT407; final source/runtime/combined stack and native/provider/operator acceptance remain pending. |

## Implemented designs

| Proposal | Scope and follow-ups |
| --- | --- |
| [Setup and CLI experience](implemented/setup-and-cli-ux.md) | Shared prompts, setup and guided commands (#354, #357, #358); platform acceptance remains separate. |
| [Native session names](implemented/native-session-names.md) | Claude titles, Codex file providers and optional native API, retained evidence and refresh (#346, #350, #353); producer restrictions, history-source name refresh and UI acceptance remain. |
| [Local session discovery](implemented/local-session-discovery.md) | Codex consent, admission, collector scans and explicit scope (#305, #306, #309–#311, #316, #327); other agents and cloud deferred. |
| [First local handoff before bucket setup](implemented/local-handoff-before-setup.md) | Read-only native selection, filtering and launch (#277, #280, #288); actual-platform acceptance limits remain. |
| [Coding-agent integration abstraction](implemented/agent-integration-abstraction.md) | Phases 0–6 merged; [contracts](implemented/agent-integration-contracts.md), [historical performance evidence](implemented/agent-integration-baseline.md) and raw data stay together. Historical relative timing gate remains unresolved. |
| [Adding a machine](implemented/machine-pairing.md) | Pairing, dedicated keys and revocation; generally available by maintainer approval (#338), with further live coverage tracked. |
| [Storage setup with default bucket creation](implemented/storage-setup-default-creation.md) | Implemented; R2 creation generally available; further live coverage tracked. |
| [Git activity in metadata](implemented/git-activity.md) | Implemented in parser 0.15.0. |
| [List and browse UX](implemented/list-browse-ux.md) | Implemented; see [list and show](../../docs/guides/list-and-show.md). |
| [Session list UI](implemented/session-list-ui.md) | One-line rows, day groups, cursor and detail strip; PRs 1–6 merged 2026-10-15 (#537, #536, #550, #558, #562, #560). Owner live check not recorded. |
| [Handoff](implemented/handoff.md) | Implemented (H1–H3 and direct handoff v2); [local pre-setup handoff](implemented/local-handoff-before-setup.md) extends it; current behavior is in the [handoff guide](../../docs/guides/handoff.md). |
| [Agent skills](implemented/agent-skill.md) | Implemented; explicit follow-ups and per-app live verification limits remain. |
| [Session finding](implemented/session-finding.md) | PRs 1–8 merged; owner live-check evidence remains unrecorded. |
| [Git HEAD](implemented/git-head.md) | Implemented session-start commit metadata; the schema and docs are the contract. |
| [Replay sessions](implemented/replay-sessions.md) | Implemented replay classification and default-history exclusion. |
| [Setup recent import](implemented/setup-recent-import.md) | §1–§3 merged in #386; §4 status prompt implemented in `status_pre_setup.go` and `refused_starts.go`. |
| [Platform abstraction](implemented/platform-abstraction.md) | Implemented; follow-ups and verification gaps recorded in the design. |
| [Portable handoff, guided setup, and Linux](implemented/portable-handoff-and-onboarding.md) | Implemented; guided R2 creation generally available; further coverage tracked. |

[Local discovery source evidence](implemented/local-session-discovery-evidence.md)
is a companion investigation, not another open proposal. Pinned baseline reports
preserve their original measurements; they are not measurements of today's tree.
