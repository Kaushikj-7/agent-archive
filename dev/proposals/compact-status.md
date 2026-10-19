# Compact status

The approved compact overview reports collection activity, last completed scan,
last upload, observed pending work, and detected agents with separate archiving
enablement. The default command must not enumerate sessions, decode publication
bodies, discover applications, access credentials, or contact storage.

Collection writes a bounded, versioned advisory summary tied to its configuration,
plus throttled phase/progress observations while holding the collector lock.
Status reads saved application detections and makes scheduler queries within one shared 150 ms budget.
Missing, incompatible, changed-configuration or stale evidence is unknown rather
than a reason to recompute history. Activity requires current lock/process evidence;
an idle scheduled collector is normal. These files never authorize capture,
publication, deletion or recovery.

`status --issues` explicitly loads detailed problems and capture gaps. Existing
app, verbose and version-4 JSON views retain their detailed evidence. Default
status prints directly, with commands to open the deeper views.

Implementation and validation proceed as one focused change:

1. Add bounded summary/progress producers and readers, including uninstall ownership.
2. Route default status through the compact view; add detected agents and issues.
3. Cover stale/missing evidence, failures, process liveness, bounded deadlines,
   absence of session reads, producer integration and JSON compatibility.
4. Update help, user docs and screen goldens; run affected and repository checks.
5. Benchmark complete command startup/output against synthetic large archives and
   read-only local state. Target p95 below 500 ms; report actual measurements
   separately from deterministic I/O/deadline tests and platform acceptance.

No uploaded content or source/metadata derivation changes. The new private report
uses its own schema version 1; incompatible reports are ignored. No filter,
adapter, parser, configuration writer or status-JSON version changes are needed.
