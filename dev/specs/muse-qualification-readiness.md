# Muse qualification and subagent implementation readiness

Date: 2026-10-14. Reviewed repository HEAD:
`7d8aee13c7146ceb38d15df7188d7daa31f464eb`.
Companions: [implementation plan](muse-integration.md),
[senior review](muse-plan-review.md), [earlier native campaign](muse-plan-validation.md),
[current evidence](muse-qualification-evidence.json), and
[reviewed frame excerpts](muse-qualification-fixtures/manifest.json).

## Decision

The plan has concrete native evidence and work packets for a future implementation
with subagents. Production enablement is not qualified. Frame integrity and
original-creation authority must be resolved before enabling capture, and the
implemented adapter still needs its own acceptance campaign.

The qualification campaign performed native investigation, temporary
command-driven probes, installation of qualification tools in temporary storage,
and documentation/data updates. During that campaign, no repository implementation
code, branch, commit or PR was created. No implementation worker was launched.
No archive publication or real provider call occurred. The campaign ended with
the checkout detached at the reviewed HEAD and the planning documents uncommitted.
Subsequent publication of these documents does not qualify the production adapter.

Publication base check: these documents were rebased onto
`4f94edb97f992f408521928cd1227d2780c6401c`, after the reviewed HEAD above.
Since the review, `main` changed Codex history selection/refusal, shared source
encoding/read budgets, discovery/read bounds, picker/handoff and CI contracts.
The dated native evidence is retained; it does not certify those later shared
contracts. Q0 must reconcile them and rerun the affected regressions against the
actual implementation base before freezing interfaces for dependent workers.

“OpenMet with sub-agents” is provisionally interpreted as preparation for the
`implement-with-subagents` skill (read at
`$CODEX_HOME/skills/implement-with-subagents/SKILL.md` on the qualification host).
A clarification was requested; a separate OpenMet runtime has not been identified
or configured. The packets below are independent of that interpretation and do
not authorize starting an implementation run.

## Qualification environment

The earlier CLI was `1.4.7-R6101.1`, native SHA `d8c53d25bd`.
The official stable-channel metadata now offered `1.4.8-R6256.1`; its macOS x86_64
artifact was downloaded into temporary storage, checked against Meta's advertised
size/SHA-256, and executed directly without changing PATH or personal settings.
Its binary SHA-256 is:

`94ea8fd1c73664c22e5c6157d8706945b667b11bd1c7c51691811e79f17dfcf1`.

Temporary state is under `/private/tmp/agent-archive-muse-qualification-20261014`.
HTTP model/catalog responses came from a local deterministic endpoint. Native
filesystem writes and shell tools were disabled; normal approval/sandbox policy
remained in place. A disposable Git repository qualified native isolated worktrees.
A pinned official filesystem MCP server (`@modelcontextprotocol/server-filesystem@1.0.0`)
ran with Node 24.19.0 against synthetic temporary files only.

The machine has no Muse auth file, `META_API_KEY` or alternate `MUSE_AUTH_PATH`.
It is macOS x86_64. Docker's CLI is present, but no daemon or installed local Linux
VM runtime was available. Native Linux/arm64 behavior was not executed.

The SDK checkout and web preview do not match the tested binary's wire schema.
Use each binary's offline schema export for probe parameters. Stable fingerprints:

| Build | Fingerprint |
| --- | --- |
| 1.4.7 | `sha256:6de32db08bbbe0deb478d2bfcaba001e9fb490986bc80db3aceb4d996abf7df9` |
| 1.4.8 | `sha256:4e3200c3c9dc0cd7919d11022630d43f9bc1c5cdda375381e90b0183b5665069` |

The experimental export has its own fingerprint; do not compare it as though it
were the stable handshake. The tested resume request requires `commandId`.
Schemas and exporters are probe tools, not the retained-file codec specification.
See the [SDK preview](https://meta-models.github.io/muse-code-sdk/) and
[official changelog](https://dev.meta.ai/docs/muse-code/changelog).

## Native results and implications

| Area | Result | Consequence and limit |
| --- | --- | --- |
| Stable child follow-up | Passed on 1.4.7 | A new native `result_ready` settled after the follow-up, with the same child and preserved byte prefix. Admission ack/model-call count alone is insufficient. |
| Child close | Passed after result settlement | Native owner state became `closed`. An earlier in-flight close refusal was a timing error in the probe. |
| Closed-child reopen | Unqualified | 1.4.7 rejected the settled closed child's reopen with `subagent_state_ineligible`. Keep the refusal and prior child; do not fabricate a new attempt or assume the advertised method succeeds in every state. |
| Hard-kill recovery | Passed on 1.4.7 | Killed the native host while a controlled child provider request was in flight. Parent resumed, both prefixes remained, and another child follow-up reached a new `result_ready`. |
| Native upgrade | Passed, 1.4.7 to 1.4.8 | The same parent/child resumed and another child result settled; root and child byte prefixes remained. This is a native producer upgrade, not archive writer compatibility. |
| 1.4.8 workflow | Passed with the direct recorder | `submit_result` produced completed/terminal workflow lifecycle. A compound-shell probe exceeded its deadline despite retained completion; that attempt is not a clean shutdown pass. |
| 1.4.8 isolated worktree | Passed for native create/cleanup | Prepared, lease and cleanup records plus completed workflow were retained. This does not test the future archive's included-project mapping. |
| Explicit grandchildren | Earlier 1.4.7 graph passed; 1.4.8 orderly run unqualified | The newer compound-instrumented run timed out. Do not promote file count or observer files into a descendant lifecycle pass. |
| Managed replay forwarding | Passed with direct recorder on 1.4.8 | Root and two child hooks carried the launch marker; ordinary user hook did not. Compound shell instrumentation did not preserve the extra variables, including on 1.4.7. Qualify the exact generated command style, not just the settings allowlist. |
| Project skill trust | Passed on 1.4.8 | The synthetic project skill was absent without workspace trust and present with trust. This does not establish plugin/foreign-root eligibility. |
| Skill invocation evidence | Passed on 1.4.8 | Successful `read_skill` hook and native `skill_read_observed`, with `evidence_kind: read_skill_tool` and evidence hash, were observed. This is stronger than installed catalog evidence. |
| Skill activation commands | Executed; preservation failed | `user-only`, `disable`, `enable` exited 0 and retained foreign hooks, but dropped MCP `type`, `cwd`, `required` and timeout fields. Command success does not establish configuration preservation or every activation semantic. |
| MCP read and error | Passed for local stdio calls | Success returned the sentinel; missing file emitted failure hook. Joining `tool:call_1` intent/task ID to native `completed`/`failed` proves status; committed result text has no typed error field. HTTP/OAuth, parallel/large results and plugin servers remain gates. |
| Permission frame export | Exact bytes accepted | Reviewed native frame schema 1 exported with zero unparseable lines; corrupt checksum and other mutations produced one unparseable line while exit remained 0. Checksum encoding/canonical rules are still unknown. |
| Manual MSP compaction | Unavailable on both tested builds | `session/compact` reported `compaction_unavailable`, including explicit strategy/context-limit probes. Admission/failure cannot be relabeled successful compaction. |
| Periodic checkpoint | Produced on 1.4.8 | `context_projection_checkpoint`, schema 3, history `preserved`, appeared with the opt-in 4096-byte setting. It contains private model/instruction/context snapshots and repeated usage, not new conversation or another call. |
| Checkpoint restart/continuation | Passed in corrected isolated case | Kept one local endpoint alive, requested no inline items, allowed a bounded longer deadline. Resume returned in 4.857s and another turn completed; the raw prefix remained. The earlier changed-fixture timeout remains unexplained. |
| Side conversation | Passed for creation and own continuation | Copied `session.fork.compaction`, then `session.side.started`/`boundary` and own activity. These are branch/context relations, not delegated-agent ancestry. |
| Older fork cut after checkpoint | Refused on 1.4.8 | A completed earlier turn returned `forkBoundaryInvalid`, `seedPath: runtime_checkpoint_tail`. Native boundary availability needs qualification; do not invent an equivalent branch. |
| TUI compaction/orderly quit | Unqualified | Two PTY attempts did not satisfy their intended readiness trigger. They left native completed-response/checkpoint evidence, but did not qualify the requested compact gesture or shutdown. |

### Integrity and durable evidence

The [exact permission frame](muse-qualification-fixtures/permission-frame-1.4.8.jsonl)
contains only native permission declarations; no model dump or private home path.
The [checksum mutation](muse-qualification-fixtures/permission-frame-invalid-checksum.jsonl)
is explicitly derived. Their hashes/provenance and expected exporter diagnostics
are in the fixture manifest. They are excerpts, not full valid session fixtures.

Changing outer whitespace/key order also produced an unparseable-line diagnostic.
This observation does not prove which canonicalization or checksum rule rejected
it. Several plausible hash encodings did not reproduce the declared digest.
Preserve exact input bytes before frame validation. Do not assume generic JSON
reserialization preserves native validity, or use an export exit code as proof.
Resolve the actual format contract before advertising frame support.

Model requests, full logs and checkpoint context remain temporary. The checked-in
manifest contains reviewed facts/results and corrected/failed attempts. In-memory
probe commands and reused research helpers are not a checked-in CI runner. Per the
no-code constraint, that runner has not been authored here; it remains an explicit
first deliverable when implementation is authorized.

### Resource evidence

The existing collector benchmark for 1000 settled synthetic Codex registrations
passed with one measured iteration on Intel i7-8559U, macOS amd64:

- Warm pass: 3.130 seconds, 32,359,360 allocated bytes, 297,756 allocations.
- Zero transcript filters, published-bundle decodes and remote reads.
- Whole command: 589.089 seconds, including expensive untimed fixture preparation
  and initial publication. This is not the warm-pass duration.

This validates an existing reuse property, not Muse performance or a universal
latency promise. Do not rerun it routinely or raise budgets based on it. Muse
cold inventory, combined-agent pressure, large record/context/checkpoint bodies
and many live descendants still require the operation-count/allocation/RSS gates
in the implementation plan.

## Work packets for a future subagent run

These are planned implementation scopes, not created implementation PRs. After authorization, use one implementer
per coherent PR and a fresh reviewer. Combine inseparable changes rather than
forcing these boundaries into an artificial stack.

| Packet | Scope and cross-packet contract | Dependencies and acceptance |
| --- | --- | --- |
| Q0 — evidence/authority | Runnable isolated corpus, exact fixture provenance, supported frame/canonical integrity, original root/child/branch creation facts, reproducible checkpoint/producer limits | First. Resolve or explicitly gate unsupported native modes; no default capture while required authority is unknown. |
| P1 — source/analysis foundation | Muse identity/builtin composition, minimal generic source binding/admission facts, confined same-handle validation, frame decoding and safe retained representation, one Analysis | Q0 for advertised formats. Exact text/usage/lineage/privacy; conformance/fuzz; retained refilter independent of native files. Freeze shared contracts before dependent workers start. |
| P2 — root capture/setup | One reviewed root/revision, durable pending registrations, bounded resumable inventory, short locked binding commit, narrow hook environment, byte-preserving install/remove/status | P1. Null locator/restart, changed policy/file/root, copies/symlinks, native launch surfaces and real publication/read-back. No automatic missed-start admission. |
| P3 — native children | Independent child owners, typed bindings/roles, agent-qualified parent/removal keys, active child polling and durable link recovery; existing stats rollup | P1/P2. No-stop/reuse/recovery/observer/worktree matrix; no inherited consent or duplicate totals; separate branch provenance from the child tree. |
| P4 — access and user surfaces | Explicit backfill/undo, local access, runtime/skills/evidence, launcher/handoff/replay, readers/export | P2; P3 where descendants are promised. Exact owned-file changes, purpose budgets, output agreement, bidirectional handoff; no Muse settings writers in setup. Split independent areas only after shared contracts stabilize. |
| P5 — release acceptance | Current-head compatibility, failures/recovery, privacy/retention, measured resources, macOS/Linux/native architectures and real provider campaign | All claimed packets. Independent combined review and actual release matrix; unavailable capabilities remain absent/explicitly unavailable. |

Shared interface/state changes have one writer. During a future four-slot run,
reserve capacity for review: at most two independent implementers plus one reviewer
alongside the orchestrator. No model overrides are prescribed. Independent work
must use isolated checkouts; a final review covers the combined dependency heads.

Use a checkout containing the implementation plan and qualification evidence.
On launch, reconcile the live base/repository instructions and carry the relevant
planning inputs into every isolated checkout. Pin review heads/dependency context; invalidate evidence after
interface, dependency, source-format or base changes.

Host subagent APIs and a GitHub PR connector were available during qualification;
worker launch and PR creation were not exercised by that campaign. `gh` was
absent. Verify future implementation permissions and current CI at launch. Keep
the future implementation run in handoff mode unless the user explicitly authorizes
its merges; authorization to publish these planning documents does not authorize
production implementation or its merges.

## Gates and owners

| Gate | What unlocks it | Owner/when |
| --- | --- | --- |
| Frame integrity/canonical encoding | Producer contract or independently verified implementation against exact valid/mutated frames, bounds and supported versions | Q0 qualification engineer; producer clarification if necessary. Blocks capture enablement. |
| Original creation/scope | Native evidence across copies, resumes, forks/side/checkpoints and missing parents, compared with original consent floors; hook time is not native birth | Q0 and P1/P2/P3. Blocks the affected admission lane. |
| Default/manual compaction and recovery edges | Completed native compaction, prune/rewrite/split/goal/child scenarios, qualified TUI gestures and closed-child behavior | Q0/P5. Current checkpoint result covers only its recorded case. |
| Real Meta behavior/accounting | User-configured provider access, bounded dated campaign with real cache/reasoning/model/retry semantics | User provides access locally; qualification engineer runs tests. No credentials requested in the report. |
| Native Linux/arm64 | Supported actual host/CI/VM with the pinned producer and runtime prerequisites | Release/qualification owner before claiming that platform. Cross-builds alone are insufficient. |
| Adapter/privacy/cloud/undo/performance | Actual Muse implementation through normal registry/consumer paths, approved disposable storage and required repository checks | Each packet owner; final independent reviewer/P5. Cannot pass before that code exists. |
| Broader skills/MCP/parallel operations | Exact activation, plugin/foreign/trust and MCP transport/error/retry matrices | P4/P5 before advertising those evidence capabilities. |
| Separate OpenMet tool | Identify the intended runtime and its requirements if it differs from the provisional workflow interpretation | User clarification; no unknown tool installed. |

The next authorized work can follow this packet structure. The first task is Q0,
not bypassing missing format authority or claiming all production gates passed.
