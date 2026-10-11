# agent-archive development

Specifications, contributor guides, and maintainer runbooks. For using
agent-archive, see the [user documentation](../docs/README.md); to contribute,
start with [CONTRIBUTING.md](../CONTRIBUTING.md).

## Contributing

| Doc | For |
| --- | --- |
| [Architecture](contributing/architecture.md) | How the pieces fit, and the package map. |
| [Testing](contributing/testing.md) | Tests, lint, fuzzing, and a sandbox that never touches your real machine. |
| [Adding an adapter](contributing/adding-an-adapter.md) | Supporting another coding agent. |
| [Session admission](contributing/session-admission.md) | How a registration's start and admission times drive each boundary check, and the guard tests. |

## Specifications

Each says at the top how far it is implemented; where a spec and the code
differ, the code and the user documentation describe current behavior.

| Spec | Status |
| --- | --- |
| [Archive](specs/archive.md) | The original product and engineering specification; mostly implemented. |
| [Privacy filter](specs/privacy-filter.md) | Implemented: every rule the filter applies. |
| [Privacy filter changelog](specs/privacy-filter-changelog.md) | What each filter version changed, and why. |
| [Backfill](specs/backfill.md) | Implemented. |
| [Handoff](specs/handoff.md) | Implemented. |
| [Agent skills](specs/agent-skill.md) | Implemented; follow-ups listed in the spec. |

## Proposals

Open and in-progress proposals stay at the top level of [proposals](proposals/README.md);
implemented design records live in `proposals/implemented/`.

| Proposal | Status |
| --- | --- |
| [Setup and CLI experience](proposals/setup-and-cli-ux.md) | Accepted design and implementation plan; shared rendering implemented; selector and flow migrations pending. |
| [Release remediation](proposals/release-remediation.md) | Implementation and release-gate record; disposable provider and per-app acceptance remain open. |
| [Cloud capture](proposals/cloud-capture.md) | Proposed; not implemented. |
| [Local session discovery](proposals/local-session-discovery.md) | Proposed; Codex first, with shared discovery and admission rules. |
| [Historical backfill recovery](proposals/historical-backfill-recovery.md) | Planned; portable project recovery, history import lifecycle, and incomplete historical evidence. |
| [Portable handoff, guided setup, and Linux](proposals/implemented/portable-handoff-and-onboarding.md) | Implemented (repo-key handoff, guided S3 and R2 creation, Linux); guided R2 and machine pairing are generally available. Further provider coverage is tracked in testing. |
| [Platform abstraction](proposals/implemented/platform-abstraction.md) | Implemented (scheduler port, OS value, systemd backend, Linux support); open items in the document. |
| [Git activity in metadata](proposals/implemented/git-activity.md) | Implemented in parser 0.15.0. |
| [List and browse UX](proposals/implemented/list-browse-ux.md) | Implemented design record; see the current list and show documentation. |
| [Archive listing at scale](proposals/listing-at-scale.md) | In progress: phases 1–2 implemented; phase 3 planned; phase 4 needs a specification. |
| [Adding a machine: pairing, per-machine keys, and revocation](proposals/machine-pairing.md) | Implemented; generally available. Further provider coverage is tracked in testing. |
| [First local handoff before bucket setup](proposals/implemented/local-handoff-before-setup.md) | Implemented; on-demand utility, no persistent local archive; live-platform acceptance remains pending. |
| [Coding-agent integration abstraction](proposals/agent-integration-abstraction.md) | Proposed; interfaces and migration plan for adding fully archived agents. |

## Maintainers

| Doc | For |
| --- | --- |
| [Releasing](maintainers/releasing.md) | Tagging, signing, and notarization. |
| [Published-release acceptance](maintainers/open-source-acceptance.md) | Disposable Mac and bucket smoke test with recorded pass, fail, or pending evidence. |
| [Versions](maintainers/versions.md) | Filter, adapter, parser, and schema versions, and when each is bumped. |
| [Next release implementation plan](maintainers/next-release-plan.md) | Historical reviewed plan and current implementation reconciliation; release acceptance remains a gate. |
