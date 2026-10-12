# Local session identity and recovery

Local registrations use the agent and its exact native session ID together.
Claude Code, Codex and Cursor can use the same native ID without sharing an
archive session. Agent aliases are canonicalized; native IDs retain their exact
valid UTF-8 bytes, including case and whitespace. Empty/all-whitespace IDs and
invalid UTF-8 identities are refused. Archive session IDs, parent archive IDs,
remote object keys, source/metadata schemas and pending publication bytes do
not change.

The qualified local index lives in `sessions-v1/`. Its filenames hash a
versioned length-prefixed agent/native identity; native IDs never become file
paths. A historical `sessions/` index is adopted only after its referenced
registration matches the archive ID, canonical agent and exact native ID.
New writers leave that legacy evidence in place and never write new
unqualified index entries. Retention and undo remove historical entries only
when they still name the removed registration.

## Recovery

Hooks read only directly referenced entries and registrations. A damaged index
never triggers a full registration scan on the user's turn. An eligible proven
start or Cursor transcript follow-up remains queued with its original capture
generation for the collector. Continuation evidence alone cannot admit a new
session.

The collector runs recovery on its first qualified-index pass and when a hook
requests it. Recovery enumerates registrations once per attempt, refuses
same-agent duplicate native identities, and atomically repairs unique entries.
An interrupted or unreadable census remains incomplete in `session-index.json`;
partial enumeration cannot prove an identity absent. Recovery completion is
guarded by the local marker generation, so a hook request arriving during the
census or completion remains pending for another recovery attempt. Individually validated
registrations remain readable. A requested damaged identity is recorded absent
only after a complete census succeeds. Outstanding child candidates retain
their reserved archive IDs; they cannot recreate a missing parent.

For a completed packed identity index, a new registration or another change
in local session membership makes the recorded census stale. The
`identity_recovery` status reports `unknown` until the collector starts a fresh
census, then reports pending work until recovery completes against the current
membership. Existing registrations remain authoritative during this work;
recovery preserves their archive IDs. Run `agent-archive sync` to advance the
rebuild, which may need multiple passes.

Each membership change can require a full registration and child-candidate
census and a rebuild of the packed index. Large inventories or continued
membership changes can delay completion. The collector's four-second index
application allowance starts after census validation; the source scanner's
five-second budget is separate and does not bound the whole recovery stage.
See [Local discovery scanner](local-discovery-scanner.md) for the census and
operating-envelope limits.

Run `agent-archive sync` to retry collector recovery. Resolve unreadable or
conflicting local registration evidence using a complete backup before retrying.
A quarantined registration remains incomplete recovery evidence until it is
restored or deliberately resolved. Recovery's marker describes new-writer
local state; it does not detect arbitrary later filesystem loss. Bounded hooks
cannot discover unrelated manually introduced duplicate registrations.

## Running an older version

The migrated data home is not writable-compatible with older binaries. A new
format marker cannot make an already released binary refuse it: an old writer
may reuse a legacy collision or create another archive ID.

Before running an old writer, stop collectors and hooks and back up the data
directory. Restore the **complete pre-migration local snapshot**, accepting the
loss of later local pending work, or use a separate empty data home. Do not run
old and new writers together against the migrated home, and do not convert the
index back into an unqualified namespace. Existing remote archives remain
readable by older readers because their IDs, keys and schemas are unchanged.

## Protected discovery configuration

The discovery foundations do not enable automatic capture. When discovery
authorization is present, configuration uses a protected `schema_version` object.
Published v0.1.1 integer-version readers refuse it rather than rewriting away
consent intervals. Existing hook-only configurations keep the numeric format.
This protection does not retroactively protect every existing qualified-index
home: the complete-snapshot rollback guidance above still applies.

New automatic admission honors retention and undo removal records. Deliberate
backfill remains the way to include removed history explicitly. Repeated
admission preserves original identity, origin, destination and admission times.

## Historical project identity

A historical project uses an explicitly supplied random 128-bit opaque ID,
independent of any filesystem root, repository key or display label. Its imported
session keeps the ordinary qualified agent/native archive identity and exact batch
provenance. A private version-1 `historical_project` proof binds the project ID,
destination, original and canonical cwd, and bounded required-source cwd evidence.
Its live `project_root` is empty; the proof cannot coexist with live Codex consent
or generation recovery authority. Source-set staging binds every proof field.
These private paths are not source headers or metadata. Published project IDs
remain opaque; historical labels provide the project name.

Permissions are destination-scoped. Label changes preserve ownership and capture
age. Exclusions apply to retained dependency evidence even after native deletion.
Historical imports never recapture growth from the native session. Their local
handoff reads use retained evidence; native transcript recovery is refused.
Paired machines reconcile IDs and exact destination decisions, preserving local
labels and disabled policy and transferring no native source assignments.
