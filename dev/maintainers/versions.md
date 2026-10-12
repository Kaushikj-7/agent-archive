# Versions

Several independent version numbers describe what produced an archived
session. Each is recorded with the session, so a reader can tell which rules
applied, and a change to any of them makes the collector re-derive what it
affects.

| Version | Where | Recorded in | Current | Changes when |
| --- | --- | --- | --- | --- |
| Release | `cli.Version` (set at build time) | `--version` | Build-dependent (`dev-<commit>` from source) | A tag is cut. |
| Filter | `archive.FilterVersion` | source header `capture.filter_version`, metadata `filter_version` | 18 | What the privacy filter keeps, drops, or redacts changes: any change to filtered output. |
| Adapter | `adapterVersion` in `internal/archive/adapters.go` | `capture.adapter_version` | 0.18.0 | An adapter's output changes (bumped with the filter in practice). |
| Parser | per-agent `Parser.Version()` / `archive.DefaultParserVersion` | metadata `parser.version` | 0.21.0 | How metadata is derived from a source changes: counts, turns, models, skills, gaps, titles, session names, branch, linked pull requests, tools used, end time, tokens (per model too), tool errors, MCP calls, git activity, the repository key (derived from the project's git origin, not the source). |
| Source schema | `archive.SourceSchemaVersion` / `HistorySourceSchemaVersion` | source header `schema_version` | 2 | The source bundle's line format changes. Readers refuse other versions. |
| Metadata schema | `archive.MetadataSchemaVersion` / `HistoryMetadataSchemaVersion` | metadata `schema_version` | 1 | The metadata sidecar changes incompatibly. Optional fields don't bump it. |
| Codex parser | `codex.Parser.Version()` | metadata `parser.version` | 0.27.0 | Codex identity, ownership or metadata interpretation changes. |
| History source schema | `archive.HistorySourceSchemaVersion` | source header `schema_version` | 3 | The self-contained Codex history manifest or record envelopes change. |
| History metadata schema | `archive.HistoryMetadataSchemaVersion` | metadata `schema_version` | 2 | Preserved native revision references change incompatibly. |
| Machine record | `machines.SchemaVersion` | `machines/<machine_id>.json` and `machines --json` | 1 | Informational registry format changes incompatibly; independent of session schemas and filtering. |
| Configuration | `config.SchemaVersion` | `config.json` `schema_version` | 1 | `config.json` changes incompatibly. |
| List JSON | `cli.listSchemaVersion` | `list --json` `schema_version` | 4 | The script-facing list document changes incompatibly. Version 3 removed `unavailable`; version 4 makes exact-count knowledge explicit. |
| Status JSON | `statusView.Version` in `internal/cli/status.go` | `status --json` `schema_version` | 4 | Status output changes incompatibly; schema 4 separates discovery from actual hook evidence. |

Claude uses the common parser version; Cursor remains at parser 0.20.0.
Codex has its own parser version as recorded above.

The per-version filter changes are in the
[filter changelog](../specs/privacy-filter-changelog.md).

## What a bump does

The collector compares each session's last scan (its scan signature) with the
running build's parser, filter, and adapter versions. When the filter or
adapter differs, it re-reads the transcript, filters it again, and
republishes the session. When only the parser changed, it republishes
metadata derived from the retained source, and reads the transcript only
if it changed since the last scan (an unchanged one would filter to
exactly what was retained); a changed one is published as usual. Nothing
is re-uploaded unless it changed.

## Rules for contributors

- **Any change to filtered output** — a key kept or dropped, a redaction
  pattern, a new block type — bumps `FilterVersion` and adds a section to the
  [filter changelog](../specs/privacy-filter-changelog.md), updates the
  [filter specification](../specs/privacy-filter.md), and, if what users
  should know changes, [privacy](../../docs/security/privacy.md). Regenerate the goldens
  ([how](../contributing/testing.md#fixtures-and-goldens)) and review the
  diff: every changed line is a privacy decision.
  Bump `adapterVersion` in the same change.
- **Any change to derived metadata** (counts, turn detection, model or skill
  attribution) bumps `DefaultParserVersion`.
- **Schema changes** need the JSON schemas in `schemas/` updated in the same
  change, and a reader that still reads what earlier versions wrote, or a
  clear refusal.
- Update the version line in this file.

Discovery authorization uses an incompatible schema-version object
`{version: 2, writer: discovery-floor-v2}`. It retains an immutable
`native_start_floor` for each permission generation, including generations
created while paused. The earlier `discovery-v2` writer cannot preserve this
boundary and must refuse the new fence. Hook-only configuration retains numeric
version 1; the current decoder reads both protected writer identities and
refuses unknown fences. Every save and nested setup snapshot writes the current
fence; identity writing upgrades an earlier protected fence first.

An older nonempty history migrates conservatively from its earliest retained
interval. An older empty history has no recoverable consent boundary: it may
be preserved and fenced, but cannot resume or authorize starts. Setup
reconciliation renews it as a fresh generation at the latest of reconciliation,
project activation, and destination activation. It does not reconstruct old
consent from activation alone. Future scope-policy schemas must preserve these
floors and the stronger writer fence through all saves and rollback snapshots.

Codex capture scope and local admission proof use the stronger incompatible
`{version: 3, writer: codex-scope-floor-v3}` configuration fence. It remains installed
when scope is reduced or discovery disabled, and on rollback over protected
history. New readers retain numeric/v2 included-project consent without
opening blanket windows. This changes session consent and physical project
attribution, not adapter filtering, native parsing or derived-field algorithms;
filter, adapter, parser and published source/metadata schema versions remain
unchanged. The private policy/proof contract is documented in configuration.

Strict JSON Schema validators using the v0.1.1 schemas reject new discovery
provenance: source schema 2 disallows additional properties, and metadata
schema 1 constrains origin to import. Use the updated schemas shipped with
the new release. Ordinary typed readers accept older records and optional
new provenance; the source line format remains 2 and metadata remains 1.
Optional metadata fields do not bump those schema versions; parser version
20 records the changed derivation.

Codex discovery's format-based compatibility changes admission and local
diagnostics, not privacy filtering, adapter output or derived metadata.
Filter, adapter, parser and published source/metadata schema versions remain
unchanged. The private discovery catalog advances to version 3 to reprobe
older cached observations for format profiles. Health/status gain an optional
bounded `observed_formats` array; older health summaries remain readable and
status schema 4 remains compatible. Existing consent and writer fences do not
change.

Explicit transcript recovery installs the incompatible
`{version: 4, writer: archive-generations-v4}` configuration writer fence.
Its underlying discovery and Codex policies retain their independent v2/v3
floors and consent semantics. Every save and setup rollback keeps generation
protection after it is enabled, including after content expires. Earlier
writers refuse this fence rather than ignore frozen historical generations.

Recovery adds optional `previous_generation_id` provenance to source headers
and metadata and a persistent `recovered_generation` coverage gap to successor
bundles. This relation is copied from registration by
`ApplyRegistrationProvenance`; it is not computed by native parsing or metadata
analysis. Filtering, adapter output and parser algorithms are unchanged, so
filter, adapter and parser versions remain unchanged. Source schema 2 and
metadata schema 1 remain compatible: earlier readers ignore these optional
fields. The explicit relation is independent of subagent `parent_session_id`.

Codex identity interpretation advances `codex.Parser.Version()` to 0.21.0
through the existing per-agent parser port; Claude and Cursor remain at 0.20.0.
For Codex, retained
`session_meta.payload.id` identifies the thread, while `session_id` identifies
its root conversation and is no longer an identity conflict. The private
discovery catalog advances to version 5 to reconsider previous identity
refusals and checkpoint bounded configured-project recovery. Related live
histories remain capture-pending. No additional native metadata is retained, so filter 15, adapter 0.15.0, source schema 2 and metadata
schema 1 remain unchanged. Privacy-only refiltering of existing retained
sources remains compatible with absent older metadata.

Codex history interpretation advances only its parser to 0.22.0. It excludes
inherited activity from own counts and preserves inherited model context.
Cumulative-only accounting with unproven ownership stays unknown with a
`history_cumulative_tokens_unavailable` gap; independently observed own usage
remains counted. Source schema 3 and metadata schema 2 are additional readable
formats for self-contained history and preserved revision references. Ordinary
schema-2 encoding remains compatible, with filter 15 and adapter 0.15.0 unchanged.
The Codex source signature includes `codex-history-v1` interpretation evidence;
this does not change Claude or Cursor parser/adapter behavior. Revision-aware
history publication now uses verified retained source sets and exact authority
through the pending journal; config-v5 protects it from older writers. This
lifecycle work does not change the reader schemas or parser/filter versions.

Opt-in Codex native naming adds a fixed API evidence source/contract with filter
18, adapter 0.18.0 and Codex parser 0.25.0. Existing file observations remain
readable. The optional codex_name_lookup setting needs no writer fence: loss by
an older writer safely returns to files. Source/metadata schemas stay compatible.

Listing v3 evolves only its auxiliary object-key encoding: canonical base64url
summaries are split at 255-byte path-component boundaries. Readers retain old
single-component v2/v3 hints, and older readers use compatibility scanning when
they reject segmented hints. Canonical source and metadata schemas, derived
metadata, privacy filtering, adapter and parser versions are unchanged.

Codex native child ownership advances its parser from the naming policy 0.25.0
to 0.26.0. Native children retain their stable thread identity and own admission,
and `native_child` remains observable while their parent archive link is pending.
The new parser version also rechecks older naming-era child caches before
migrating their retained ownership headers. Claude and the common parser remain
0.21.0, Cursor remains 0.20.0, adapter 0.18.0 and privacy filter 18.

Codex parser 0.27.0 applies filter 16's injected-context rule to user text a
filter 15 or earlier source retained, so a leading open-page wrapper is neither
a prompt nor the title, and the title falls through to the next real prompt.
The bump makes existing sessions refresh their metadata from the retained
source without a new source upload. Filter, adapter and other parsers are unchanged.

Confirmed import staging uses the incompatible configuration fence
`{version: 9, writer: import-staging-v9}` and sticky
`import_staging_protection`. It dominates retention's version 8 fence while
preserving retention protection and every underlying authorization floor.
Only staging-aware writers accept version 9: the earlier retention-only writer
refuses it before mutation because it cannot preserve the registration's staged
source digest. This adds private admission/publication journal fields; published
source, metadata, filter and adapter versions remain unchanged.

Historical-only project permission installs the incompatible permanent
`{version: 10, writer: historical-projects-v10}` configuration capability.
Private historical ownership proof is version 1 and staged source-set proof
includes every private dependency cwd. The historical scan-source contract is
version 4. Existing source line formats, metadata schemas, filter/adapter output
and parser algorithms remain unchanged: historical display names are explicit
configuration applied at publication, not native derivation. Pairing schema 1
adds an optional bounded historical-project whitelist; strict older receivers
refuse the unknown section. No native path or mapping is portable in that section.
All lower policy floors, protections and published older-writer refusal controls
remain mandatory. Native macOS/old-release and provider acceptance are recorded
separately from synthetic Linux tests and cross-compilation.

The [Codex fragment foundation](../specs/codex-fragments.md) proposes an independent
`fragment_version: 1` line schema. This is unpublished and has only executable
schema checks; it adds no production decoder, consumer, filter route or writer.
Published source/metadata schemas, filter/adapter/parser versions and permanent
configuration capabilities remain unchanged. Schema-valid headers do not certify
privacy. A future fragment writer capability must be coordinated separately;
historical-projects-v10 already owns configuration version 10.
