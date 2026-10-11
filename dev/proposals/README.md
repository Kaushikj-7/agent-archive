# Proposals

Keep open and in-progress proposals in this directory. Move a proposal into
`implemented/` when its main scope has shipped, preserving it as a design record.
Record implementation differences and remaining follow-ups in the document;
small follow-ups do not keep an implemented proposal in the active queue.
Partially implemented proposals stay here until their remaining phases are
complete or explicitly deferred. Update incoming links and relative links when
moving a proposal.

## Open and in progress

| Proposal | Status |
| --- | --- |
| [CLI reliability and keep forever](cli-reliability-and-retention.md) | Implementation in progress; cleanup safety, CLI defects, explicit permanent retention, revision access and durable imports. |
| [Setup and CLI experience](setup-and-cli-ux.md) | Accepted design and implementation plan; shared rendering implemented; selector and flow migrations pending. |
| [Native session names](native-session-names.md) | Ordinary-session names and refresh implemented; native lookup is opt-in; history-source refresh and live UI comparisons remain incomplete. |
| [Release remediation](release-remediation.md) | Implementation and release-gate record; disposable provider and per-app acceptance remain open. |
| [Cloud capture](cloud-capture.md) | Proposed; not implemented or scheduled. |
| [Local session discovery](local-session-discovery.md) | Proposed; Codex first, with shared discovery and admission rules. |
| [Historical backfill recovery](historical-backfill-recovery.md) | Implementation plan; portable deleted-checkout recovery, complete history imports, and explicit archival of incomplete evidence. |
| [Archive listing at scale](listing-at-scale.md) | Phases 1–2 implemented; phase 3 planned; phase 4 needs a specification. |
| [Adding a machine: pairing, per-machine keys, and revocation](machine-pairing.md) | Implemented; generally available by maintainer approval; further live coverage tracked. |
| [Coding-agent integration abstraction](agent-integration-abstraction.md) | Proposed; interfaces and migration plan for adding fully archived agents. |

## Implemented

These documents preserve the original designs; current code and user documentation
remain the contract.

| Proposal | Status |
| --- | --- |
| [First local handoff before bucket setup](implemented/local-handoff-before-setup.md) | Implemented; on-demand utility, no persistent local archive; live-platform acceptance remains pending. |
| [Storage setup with default bucket creation](implemented/storage-setup-default-creation.md) | Implemented; R2 creation is generally available; remaining live coverage is tracked. |
| [Git activity in metadata](implemented/git-activity.md) | Implemented in parser 0.15.0. |
| [List and browse UX](implemented/list-browse-ux.md) | Implemented design record; see [list and show](../../docs/guides/list-and-show.md) for current behavior. |
| [Platform abstraction](implemented/platform-abstraction.md) | Implemented; remaining follow-ups and verification gaps are recorded in the document. |
| [Portable handoff, guided setup, and Linux](implemented/portable-handoff-and-onboarding.md) | Implemented; guided R2 creation is generally available, with further coverage recorded in testing. |
