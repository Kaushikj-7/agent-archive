# Proposals

Keep open and partially implemented proposals here. Move a proposal into
`implemented/` when its main scope has landed on the default branch, preserving
it as a design record. Record differences and remaining follow-ups; acceptance
and release gates remain visible even after a design moves. Update incoming and
relative links, and move companion evidence with its design.

Reviewed 2026-10-11 against `b4122ea6`. Statuses describe this checkout, not a
published release or current remote PR activity. Recent merged work establishes
implementation progress; an open plan alone does not establish someone is
actively working on it. Code and user documentation remain the current contract.

## Remaining implementation

| Proposal | Implemented | Remaining |
| --- | --- | --- |
| [Historical backfill recovery](historical-backfill-recovery.md) | Diagnostics, portable recovery, rollout lookup, history lifecycle and native child capture; recent recovery hardening. | Historical-only ownership/bulk mappings, fragment artifact/lifecycle, full acceptance matrix. |
| [Archive listing at scale](listing-at-scale.md) | Parallel listing, tracing, bounded metadata-body reads; 10k benchmark and 10k/20k operation-count coverage. | Full 1k/10k/100k measurement matrix and an index that bounds LIST header cost. |
| [Cloud capture](cloud-capture.md) | No cloud collector/push or vendor pull implementation. | Proposed, unscheduled; requires producer probes and runtime/authentication design. |

[Setup recent import](../specs/setup-recent-import.md) now includes both automatic
seven-day import and the status prompt for existing installations. See the
[complete spec index](../specs/README.md).

## Acceptance and release work

| Record | Status |
| --- | --- |
| [CLI reliability and keep forever](cli-reliability-and-retention.md) | A–G implementation landed; [exact implementation-SHA evidence](../maintainers/acceptance/cli-reliability-and-retention-2026-10-11.md) recorded. Final-head checks/review and signed-release/provider/app/platform acceptance remain open; retain the plan here while those gates are pending. |
| [Release remediation](release-remediation.md) | Implementation record; disposable provider/per-app acceptance and release sign-off remain open. |
| [Native name comparisons](../maintainers/native-session-name-comparisons.md) | Partial sandbox evidence; native UI acceptance blocked. |
| [Published-release acceptance](../maintainers/open-source-acceptance.md) | Maintainer runbook for candidate/provider/app checks; implementation does not imply a passing release gate. |

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
| [Platform abstraction](implemented/platform-abstraction.md) | Implemented; follow-ups and verification gaps recorded in the design. |
| [Portable handoff, guided setup, and Linux](implemented/portable-handoff-and-onboarding.md) | Implemented; guided R2 creation generally available; further coverage tracked. |

[Local discovery source evidence](implemented/local-session-discovery-evidence.md)
is a companion investigation, not another open proposal. Pinned baseline reports
preserve their original measurements; they are not measurements of today's tree.
