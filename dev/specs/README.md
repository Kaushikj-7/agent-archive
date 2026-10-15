# Engineering specifications

Specs are for long-term understanding of the system: what a format, a rule
set or a component's behavior is, written so someone changing that code years
from now can check their change against it. A spec describes the system as it
is (or a contract it must meet), stays here after it is built, and is kept up
to date when the behavior changes.

How a change gets built is a [proposal](../proposals/README.md), not a spec:
the problem, options, PR breakdown, sequencing, decisions and live checks. A
change can have both: the proposal is the implementation path, and the spec
(new or updated) is what remains true once it lands. If a document would stop
being useful once its PRs merge, it is a proposal.

Reviewed 2026-10-11 against `db2ccd3e`.
Code and [user documentation](../../docs/README.md) describe current behavior
where a historical design differs. Fixture coverage does not establish live
platform, native UI, provider or published-release acceptance.

Historical-only ownership status reconciled 2026-10-12 after #433 merged as
`18ca0c48`; the broader recovery plan remains partial.

| Spec | Implementation status and remaining work |
| --- | --- |
| [Archive](archive.md) | Original design reference; mostly implemented. Remaining gaps in [capture capabilities](../../docs/reference/capture-capabilities.md). |
| [Privacy filter](privacy-filter.md) | Implemented current rules; filter 19, including the explicit local Muse conversation subset. |
| [Privacy filter changelog](privacy-filter-changelog.md) | Version history; maintained alongside the filter contract. |
| [Backfill](backfill.md) | Implemented original file/database import and historical-only ownership/bounded bulk mappings (#433). Cursor subagent import remains a follow-up; broader [historical recovery](../proposals/historical-backfill-recovery.md) remains partial. |
| [Codex history](codex-history.md) | Implemented retained-history reader, lookup, lifecycle, native child admission and historical-only ownership (#433). Fragment artifacts (6b) and combined acceptance remain gated. |
| [Codex fragments](codex-fragments.md) | Proposed step 6B1 format and schema checks only. Decoder, privacy-context-proven filtering, explicit incomplete consumer, lifecycle, opt-in import and exact repair undo are not shipped. |
| [Eval export](eval-export.md) | Implemented archived/local/bulk export; schema and golden fixtures define the contract. |

For the implementation queue and outstanding acceptance records, use the
[proposal index](../proposals/README.md).
