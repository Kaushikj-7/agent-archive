# Engineering specifications

Reviewed 2026-10-11 against `db2ccd3e`. Specs stay together here, including
implemented contracts; `proposals/implemented/` holds completed proposal designs.
Code and [user documentation](../../docs/README.md) describe current behavior
where a historical design differs. Fixture coverage does not establish live
platform, native UI, provider or published-release acceptance.

Historical-only ownership status reconciled 2026-10-12 after #433 merged as
`18ca0c48`; the broader recovery plan remains partial.

| Spec | Implementation status and remaining work |
| --- | --- |
| [Archive](archive.md) | Original design reference; mostly implemented. Remaining gaps in [capture capabilities](../../docs/reference/capture-capabilities.md). |
| [Privacy filter](privacy-filter.md) | Implemented current rules; filter 18. |
| [Privacy filter changelog](privacy-filter-changelog.md) | Version history; maintained alongside the filter contract. |
| [Backfill](backfill.md) | Implemented original file/database import and historical-only ownership/bounded bulk mappings (#433). Cursor subagent import remains a follow-up; broader [historical recovery](../proposals/historical-backfill-recovery.md) remains partial. |
| [Codex history](codex-history.md) | Implemented retained-history reader, lookup, lifecycle, native child admission and historical-only ownership (#433). Fragment artifacts (6b) and combined acceptance remain in the recovery proposal. |
| [Handoff](handoff.md) | Implemented; [local pre-setup handoff](../proposals/implemented/local-handoff-before-setup.md) extends the original design. |
| [Agent skills](agent-skill.md) | Implemented; explicit follow-ups and per-app live verification limits remain. |
| [Session finding](session-finding.md) | PRs 1–8 merged; owner live-check evidence remains unrecorded in the spec. |
| [Git HEAD](git-head.md) | Implemented session-start commit metadata. |
| [Eval export](eval-export.md) | Implemented archived/local/bulk export; schema and golden fixtures define the contract. |
| [Replay sessions](replay-sessions.md) | Implemented replay classification and default-history exclusion. |
| [Setup recent import](setup-recent-import.md) | Implemented: §1–§3 merged in #386; §4 status prompt now implemented in `status_pre_setup.go` and `refused_starts.go`. |

For the implementation queue and outstanding acceptance records, use the
[proposal index](../proposals/README.md).
