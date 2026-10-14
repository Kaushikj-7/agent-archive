# Muse implementation plan validation

Date: 2026-10-14. Companion to [the implementation plan](muse-integration.md).
The subsequent [senior design review](muse-plan-review.md) tightened implementation
contracts; this report remains the evidence record for the native campaign.
Evidence: [probe results and program hashes](muse-plan-validation.json).

Later [qualification/readiness](muse-qualification-readiness.md) covers Muse 1.4.8
and scoped checkpoint/recovery/MCP/skill results. This report describes the earlier
1.4.7 campaign; its unexercised cases are not retrospectively marked passed.

## Verdict

The integration architecture is feasible for ordinary retained roots, task and
workflow children, and explicit descendant relationships. The current archive's
registry/interface and synthetic fourth-agent flows passed. Native Muse tests
established the needed storage, conversation, tool, usage and lineage facts.

The plan needed corrections before implementation: support retained markers and
timestamp-less fork records; resolve grandchildren from explicit graph edges;
use native worktree records instead of child-hook cwd; count cache aliases once;
and correlate failed tool tasks rather than interpreting result prose.

Full parity is not established. Successful compaction/checkpoint production,
real-provider accounting, complete skill/MCP evidence, descendant recovery,
Linux, and the future Muse adapter's privacy/admission/publication behavior remain
explicit gates. A passing native probe does not validate code that does not exist.

## Environment and scope

Tests ran on macOS x86_64 against the installed, pinned
`Muse Code 1.4.7 (1.4.7-R6101.1)`, native build SHA `d8c53d25bd`.
The first research campaign's temporary installation was reused directly, with
no upgrade. Config/data/cache roots and workspaces were isolated under
`/private/tmp/agent-archive-muse-validation`. One disposable Git repository tested
worktree isolation. A loopback HTTP endpoint supplied deterministic model/catalog
responses using the official SDK quickstart's protocol pattern.

No real Meta calls, account login commands, production implementation, archive
uploads or personal hook/skill configuration edits were performed. Native tools
had filesystem writes and shell execution disabled; ordinary sandbox/approval
policy remained enabled. The isolated worktree was created and cleaned by Muse's
native runtime. Handoff testing read a harmless synthetic context file outside
the workspace under normal read permissions.

Raw native logs and request/context dumps remain in temporary research storage;
they are not privacy-filtered archive fixtures. Only reviewed synthetic facts and
bounded excerpts enter the durable evidence manifest. Program hashes identify the
actual diagnostic sources used; this report is an observed campaign, not a new
production test suite. Step 0 must still build reviewed reusable native fixtures.

## Native results

| Probe | Result | Evidence and limitation |
| --- | --- | --- |
| Root creation | Passed | Original root `session.opened.observed` had `resume: false` and microsecond recorded time. This is a candidate start fact, not proof for copied/checkpointed/forked history. |
| Exec continuation | Passed | Second run retained the same ID, appended byte-for-byte to the old prefix, and recorded opened-session flags false then true. |
| Exec without logging | Passed | A subsequent `--no-session-log` run created no new retained session file. |
| Controlled usage | Passed | Input 101, output 23, cached 17 and reasoning 5 appeared in `model_completed`; cached and cache-read both equaled 17. Actual provider overlap/billing remains untested. |
| Managed replay environment | Passed | Managed hooks received dynamic `AGENT_ARCHIVE_REPLAY` and allowed XDG data root; user hooks dropped them. |
| Workflow result contract | Passed | `submit_result` with text and nullable notes produced completed workflow-child lifecycle, replacing the first campaign's `child_result_missing`. |
| Direct child | Passed for spawn/log/turn evidence | Independent agent/session IDs and child log observed. Follow-up/cancel/reopen/result recovery remain separate tests. |
| Isolated workflow | Passed for native create/cleanup | Worktree prepared/lease/cleanup records identified source repository, isolated checkout and clean removal. Child hook cwd still named the source checkout. |
| Child replay propagation | Passed | Managed workflow child hooks and direct descendant hooks carried the same launch marker. All observer/TUI/serve/restart combinations are not covered. |
| Grandchild graph | Passed after correcting an assumption | Three logs and two explicit edges formed root to middle to leaf. Native depth was 1 for both children and files were flat under the root. |
| Successful tool | Passed | `assistant_tool_calls_committed.call_id` matched `tool_result_batch_committed.tool_call_id`; successful read result and hooks agreed. |
| Failed tool | Failure evidence observed | Invalid zero offset generated `PostToolUseFailure` and a native task terminal `failed`; task intent used `tool:call_1`. Success rerun used offset 1. Result text itself had no typed error flag. |
| MSP initialization/start/turns | Passed after handshake correction | `initialized` notification is required after `initialize`; two native serve turns completed. |
| MSP rename/read | Passed | Rename settled `validation-title`; read returned inline history. Native archive title refresh is not yet implemented. |
| MSP fork | Passed for native format/own continuation | Fork returned explicit provenance, two copied turns, and accepted its own subsequent turn. Native copied-record classification still needs adapter tests. |
| Manual MSP compaction | Unavailable in this probe | Returned `commandRejected`, reason `compaction_unavailable`. Unchanged log prefix does not prove append-only compaction. |
| Opt-in periodic checkpoint | Inconclusive | Setting 4096 bytes was accepted, two turns resumed successfully, but no checkpoint frame/publication was observed. This does not qualify checkpoint formats. |
| Ephemeral serve | Passed | Initialize reported ephemeral; session/read returned methodNotFound and no retained log was created. |
| Interactive TUI | Passed for fresh root lifecycle | Actual CLI ran in a PTY, fired startup/prompt/stop, produced root terminal and observer hooks. Session was then terminated; orderly quit/resume/navigation are not certified. |
| Outside-workspace handoff artifact | Passed for normal read | Read tool returned the synthetic sentinel from an absolute context path without approval/sandbox bypass flags. Cross-agent launcher/UI handoff remains an implementation acceptance test. |
| Corrupt transaction checksum | Native rejection behavior observed | Export exited 0 but diagnostics reported one unparseable line. Exit code alone cannot establish complete evidence. Archive decoder checksum encoding remains unqualified. |

## Corrections to the plan

### Source acquisition

Current shared hook admission already permits an empty transcript path. The
missing capability is bounded resolution before file-source collection, which
currently refuses an empty path. Add that narrow lookup/update boundary rather
than rewriting working registration policy. Ordinary hooks must supply approved
archive/source-root configuration explicitly.

Multi-record bounded inspection is required: a root's first line can be a
permission transaction, with workspace/build/start facts later in the prefix.
Do not silently reinterpret `DiscoveryBoundedProjects`, whose current contract
is first-record-only. Return unavailable for that fast path or introduce a
separately budgeted prefix-inspection contract. Keep native creation distinct
from hook observation and inherited permission timestamps.

### Framing and copied activity

The fork began with `session.fork.created`, `session.fork.model_prefix` and
`session.fork.turn`; these records had no top-level recorded_at. Their copied
turns held source-run timestamps. Four `retained_marker: omitted_live_only`
records supplied positions/omission descriptions instead of normal event payloads.
A permission transaction followed them with source-session timestamps.

Consequently, a uniform envelope/timestamp assumption would reject valid forks
or misattribute inherited activity. The decoder must understand marker framing
and copied prefixes. Own conversation records still require valid timestamp
provenance. Full raw model prefixes are outside the retained privacy allowlist.

### Descendant graph and worktrees

The verified graph used spawn records' parent_session_id and each logical agent's
child_session_bound. Detailed agent_lineage_admission included root and immediate
parent agent IDs. Root and child logs could repeat/forward a control record.
Derive ancestry from these facts, reconcile repeated events, and verify the
opened child stream. Neither display path/depth nor physical directory depth
establishes ancestry.

Use subagent.worktree prepared/lease/cleanup facts for isolated checkout mapping.
The isolated child hook reported the original repository cwd, so hook cwd alone
would misidentify its execution checkout.

### Metrics and errors

The same cache usage appeared under cached_tokens and cache_read_tokens: count
once. Goal/workflow attribution is a rollup rather than another provider call.
Typed tool error provenance requires joining task ID, owning run, intent and
terminal to the committed call/result; matching English failure text is invalid.

Managed replay forwarding works, but it is optional runner configuration. Preserve
foreign managed hooks and do not install a fixed run ID into ordinary user hooks.

## Repository checks

The following commands passed with `GOCACHE` redirected to temporary storage:

```sh
go test -short -count=1 ./internal/agents/builtin ./internal/agentapi ./internal/doclinks
go test -short -count=1 -run '^TestFourth' ./internal/cli
go test -race -short -count=1 -run '^TestFourth' ./internal/cli
```

The first validates the current registry, integration ports and documentation
contracts. The fourth-agent flows cover ordinary setup/status/removal, skills,
record ownership, launch/runtime/evidence, qualified discovery/import, readers,
publication retry and parser refresh using the existing Orbifold fixture. They
prove extension points work; they do not certify Muse parsing or storage capture.

The fourth-agent race variant, documentation-link tests and `git diff --check`
also passed. No unrelated production suite is claimed as run.

## Remaining gates before a full parity claim

- A successful manual/automatic compaction and periodic checkpoint campaign,
  including prune/rewrite/split behavior and checksum encoding qualification.
- Real Meta call/usage/cache/reasoning behavior, successful recovery and actual
  model-version attribution, using explicitly configured provider access.
- Descendant follow-up/reopen/cancel/crash recovery, unresolved parent handling,
  observers without files and all launch-surface replay propagation.
- Actual skill eligibility/invocation/activation/trust and MCP success/error
  records; native rewind and side-thread ownership.
- Real Linux native acceptance, plus supported-version changes on both systems.
- Implemented Muse filter/decoder admission tests, secret sentinel tests, bounded
  resources, cloud publication/read-back, retention and exact undo/retry.

The core architecture can proceed after the plan's normal implementation approval.
Full capability claims remain gated by this evidence; the synthetic campaign does
not establish all future acceptance requirements in advance.
