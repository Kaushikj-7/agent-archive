# Q0 shared contracts

Reconciled against `37609182169b658c380e9113888b0cd11d387c05`. These are
implementation contracts for the next foundation PR, not available operations.
All Muse capture remains unavailable while the authority gates in
[the qualification record](README.md) are open.

## Root and locator

Keep the spec's `archive.NativeRoot{Agent, Configured, Canonical, Revision}`
shape. It is local protected configuration, separate from discovery consent.
Validate bounded absolute paths, canonical agent and nonempty revision. Setup
owns reconciliation and revision changes; collector environment cannot select a
new root. The root must also confine the first read of an unbound registration.

Add `SourceEnvironment.NativeLocators NativeLocatorLookup` alongside the working
Codex slot. Freeze the following shape; names may change mechanically to fit
repository conventions, but semantic changes need a new Q0 decision:

```go
type NativeLocatorLookup interface {
    Locate(context.Context, LocatorRequest) (LocatorResult, error)
    Check(context.Context, string, string) error // nativeID, exact lookup token
}
type LocatorRequest struct {
    NativeID string
    Root archive.NativeRoot
    Parent *SourceRef
    Observed time.Time
}
type LocatorResult struct {
    Status LocatorStatus // found, pending, ambiguous, refused
    Current *SourceRef
    Candidates []SourceRef
    Revision string
}
```

`Revision` binds the exact agent, root revision, native ID, candidate identity
observations and coverage. `Check` revalidates that evidence after reads as well
as before signature equality; changed evidence invalidates the result. A `found`
result needs completed scoped uniqueness coverage or a qualified native current
pointer. Incomplete enumeration cannot claim uniqueness. No Muse current-pointer
authority is qualified. `Current` therefore stays nil initially. Every candidate
remains a hint validated on the same confined opened handle used for filtering.
Lookup cannot register an owner or alter project/destination/admission.

Direct root-date and validated-parent child paths precede one pass-owned bounded
fallback. UTC/local/adjacent dates and hook time locate candidates only. Keep
fallback progress in memory; persist a content-free cursor only if measured
restart starvation requires it. Keep Codex history and validation slices intact.

## Admission without losing Codex facts

The spec's generic binding sketch cannot replace Codex's richer binding.
Use an explicit in-memory tagged union:

```go
type SourceBinding struct {
    Codex *archive.CodexSourceBinding
    Native *archive.NativeSourceBinding
}
```

Exactly one member may be populated; the canonical registration agent selects
it. Change `SourceAdmission.Binding`, `SourceAdmissionFacts.AdmissionFacts` and
`AdmissionEvidence.Binding` to this carrier together. Keep existing
`SourceAdmission` creation/producer fields and `AdmissionEvidence.Task` intact.
Reuse `SourceAdmissionValidator`, `SourceAdmissionSignature` and the combined
`SourceAdmissionEvidence` path, including their final checks. Persist Codex's
existing `codex_binding` bytes/shape and all task, own-start, producer, child,
fork, cwd and physical rollout facts. Prove lossless conversion/round trips.

The initial `archive.NativeSourceBinding` fields are the spec's agent/version,
native/root/parent IDs, native creation, cwd, stream, path and root revision.
They describe observed native facts and never grant consent. The binding cannot
be produced from an integrity-unqualified Muse file. Immutable facts cannot
disappear or change; relocation needs separately proved continuity.

Ship persisted root/binding fields with writer fences in the same PR. Test real
older mutations, registration replacement and updates, import/undo, generation
and pending state, setup journals and stronger preexisting writer fences.
Dropping a binding leaves the file locator in place at this base. Missing binding
must fail closed, not acquire replacement authority from the current file.
Old child validation is not a blanket old-writer protection because loads do
not validate. No production Muse operation is registered by this foundation.

The short locked commit compares original registration, root revision, immutable
binding, project activation, destination, admission/origin, pause/removal and
pending setup. Read/enumerate outside the lock; update the freshly loaded owner
to preserve concurrent later hook observations. Use existing storage/journals.

## Optional children

Keep `ChildDiscoverer` and extend `ChildCandidate` with optional
`ParentNativeID`, `RootNativeID`, `SubagentID`, `Role ChildRole` and
`Coverage ChildCoverage`. Roles are delegated/workflow/observer; coverage is
versioned bounded facts, never consent. Zero values retain Claude semantics.
No live child caller or Muse NativeChild enablement belongs in the foundation.
The later live linker must qualify keys/removal by agent and keep Codex legacy
composite migration Codex-only.

## Retained evidence v1

The following is the **maximum proposed** per-type retained allowlist for the
reviewed ordinary root corpus. It is not permission to decode unqualified native
frames. Preserve `v`, physical line/child position, stream, sequence, record ID,
type/schema, timestamp and supported causal IDs before applying these fields.
Keep native checksum facts local, never on redacted bytes. Every retained string
passes shared redaction. Missing or omitted evidence cannot imply zero activity.

| Native payload/event | Retained fields after validation and redaction |
| --- | --- |
| `runtime.session.metadata` | session ID, cwd/project path, producer version/source only when present |
| `session.opened.observed` | session ID, resume, workspace kind, observed native timestamp; never permission timestamp as birth |
| `session.workspace_branch.observed` | cwd, branch, commit and dirty/availability facts actually supplied |
| `runtime.user_intent.accepted` / `materialized` | command/run/message IDs, typed origin, human text and materialization identity only |
| run `started` | run ID, prompt, typed user-message origin, model and task/command ID; drop attachments, context, options and provider request bodies |
| `assistant_message_committed` | run/message ID and visible text; ignore text deltas and terminal copies |
| `model_completed` | owning run/call ID, model, input/output/cache/reasoning counters and availability; cache aliases are alternatives |
| `assistant_tool_calls_committed` / `tool_result_batch_committed` | owning run/call/tool IDs, canonical tool name, redacted arguments/visible result and typed task status; no status inferred from prose |
| `session.name.changed` | session ID, title, typed automatic/manual origin when present |
| `run.model.configured` | run ID, provider/model, producer-version facts when supplied |
| `runtime.session.task` | task/run IDs, typed origin, terminal status/error code only; no arbitrary diagnostics |
| `session.end` | owner/run IDs, typed reason/status and native time |
| permission transaction children | bounded format/version/identity/sequence facts only; drop permission snapshots and arbitrary commands |
| fork/checkpoint/retained markers | bounded type/version/ownership/coverage facts only after their own qualification; no copied text, usage or snapshot bodies |
| all other records | bounded content-free type/version/coverage only; unknown authority/framing refuses before retention |

This allowlist is intentionally expressed as semantic fields until the decoder
PR pins exact native JSON paths from the corpus. No wildcard payload retention,
`record_json` nesting, hidden/encrypted reasoning, model/context dumps, arbitrary
hook output, environment maps or resume-state bodies are allowed. The sanitized
qualification corpus preserves positional rows using explicit omitted bodies;
it is neither the production `muse-jsonl` format nor a native-valid checksum
vector after sanitization. Any additional retained field requires privacy review,
version/golden changes and an update here in the owning decoder PR.
