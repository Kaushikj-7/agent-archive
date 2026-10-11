# Adding an agent integration

Add a concrete package under `internal/agents/<name>` and bind its implemented
ports once in `agents/builtin`. Shared archive, capture, collector, reader,
backfill and CLI policies consume narrow `agentapi` lookups. Native vocabulary
belongs to the integration. Read [architecture](architecture.md) and the
[integration contracts](../proposals/implemented/agent-integration-contracts.md) first.

## Establish evidence and declare identity

Collect synthetic-content native transcripts and hooks from a dated observed
version. Record the evidence in [capture capabilities](../../docs/reference/capture-capabilities.md).
Fixture success establishes fixture coverage; installed-version capture remains
unverified until a real session is published and read back on that machine.

Declare a canonical `agentmeta.Descriptor` and aliases in `internal/agentmeta/catalog.go`, then bind actual ports in `builtin.NewBuiltins`. Registry operation projections derive from those ports. Native IDs remain opaque
UTF-8 values qualified by the canonical agent. Do not apply UUID assumptions,
case folding, trimming, or filename rules to shared session keys. Consumer
restrictions, such as native handoff's bounded ASCII IDs, remain separate.

## Bind source, filter and retained parser

Implement `SourceProvider.Describe/OpenPass` and the serial `SourcePass` contract.
File providers reuse `sourceio` and verified `transcriptio` handles. Record
providers return bounded borrowed records and close snapshots before their
pass ends. A snapshot's independent record cursors must fail after ownership
ends. Keep missing, empty, unavailable, changed, limit and unsafe outcomes
separate. Declare append-only or replaceable behavior; shared orchestration
owns rewrite gaps, retry, retained baselines and publication.

Implement `TranscriptFilter.Filter/Refilter/EvidenceExtends`, declaring the
retained format and allowlisted native fields. Reuse shared privacy helpers;
never pass unknown native shapes or hidden reasoning through. Keep filter-time
identity, timestamp bounds and completeness even when records are omitted.
Filtering receives native input, not an archive JSON serialization.

Implement a pure `TranscriptParser`. It derives `archive.Analysis` once from
retained evidence, including normalized turns, native identity facts, and
availability for this actual format/filter version. Unavailable and unknown
metrics remain distinct from measured zero. Shared builders render metadata,
transcript and handoff from that same Analysis. A parser-only version change
must not reread or refilter native sources. Implement `RecordPreviewer` only
when bounded records can safely expose labels without a full source parse.

## Bind hook, setup and inspection operations

Implement the pure `HookDecoder` with your observed native event names and
freshness evidence. Shared capture owns admission, locks, qualified identity,
config ownership and replayed effects. A follow-up or continuation cannot admit
a never-seen session. Current lifecycle locators permit files; a record provider
may interpret that file as its manifest. Provider-qualified nonfile locators
are available to source and historical registration consumers.

Implement `HookConfigurator.Location/Plan/Inspect` with observed files and
installation ownership. Return expected-byte `filechange.Change` values;
shared `fileapply` and setup journals perform mutations. Foreign handlers and
other installations must survive removal.

Declare skill installation and evidence locations separately through
`SkillProvider`. Use shared rendered templates, marker ownership and transaction
application. `Plan` and `Inspect` receive existing bytes. Evidence roots include
their user/project boundaries. Preserve path/content deduplication when agents
share a destination. Current Cursor installation uses `.agents/skills`, whereas
its inventory uses `.cursor/skills`; extension must explicitly declare its own
behavior rather than silently expand coverage.

## Bind discovery and host observations only where supported

`Discoverer` emits candidates into the caller's existing inventory. Reference
enumeration must not open transcripts. Declare purpose-specific roots and
bounded header interpretation through `NativeHeaderInspector`: compatibility
import and no-setup handoff can have different formats and availability.
`ImportInspector` interprets filtered admission observations without reparsing
a full transcript. Native project/workspace/worktree and child discovery ports
own their conventions. A database catalog inspector receives borrowed rows
through the narrow callback host, retaining compact metadata only.

A launcher returns argv; a runtime detector reads only the injected environment;
a version inspector requests fixed supported host probes. Integrations never
run programs or contact storage/network themselves. Capability declarations,
implemented ports, host presence and capture verification are separate facts.
Unsupported operations remain absent from the registry.

## Verify the real consumers

Use capability-specific suites in `internal/testutil/agenttest`: `LaunchConformance`, `RuntimeConformance`, `FileSource`, `RecordSource`, `Discovery`, `HistoricalImport`, and `Skills`. Exercise only implemented ports with synthetic fixtures. Register the shared golden `-update` flag
in every new tested package, including packages without local golden cases.
Check ordinary registry injection across hook admission, filtering, publication,
readback, metadata and handoff, plus discovery/backfill and setup/removal where
implemented. Include colliding agent-qualified IDs, multiple records/files,
unavailable metrics, repeated effects, transient reads/publication failures,
permitted replacements and append-only refusal.

`internal/testutil/orbifold` and `TestFourthNormalRegistryFlow` demonstrate an
independent test-only vocabulary, record framing and retained format through
normal ports. They are synthetic extension evidence, not certification of a
fourth production agent or installed native version. The fixture declares archive handoff and historical discovery, while native-file handoff, SQL catalog and child discovery remain unavailable. Its two native shard files feed borrowed records through normal capture; `internal/cli/fourth_flow_test.go` also exercises ordinary setup inventory, setup journals, removal, frozen publication retry and parser-only refresh. Run the repository's
architecture, normal lint, shipped Levenshtein policy, golden-flag smoke, fuzz
and platform checks against the final ancestry. See [testing](testing.md) for
the required commands and review every changed golden before regeneration.
