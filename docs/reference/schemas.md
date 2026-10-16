# JSON schemas

The two documents agent-archive writes to the bucket, and the records
`eval export` prints, have JSON Schemas (draft 2020-12) in
[`schemas/`](../../schemas):

| Schema | `$id` | Describes |
| --- | --- | --- |
| [`metadata.schema.json`](../../schemas/metadata.schema.json) | `https://raw.githubusercontent.com/wangjohn/agent-archive/main/schemas/metadata.schema.json` | A session's `metadata.json` sidecar, and each item of `list --json`'s `sessions`. `additionalProperties` is false. |
| [`source-bundle.schema.json`](../../schemas/source-bundle.schema.json) | `https://raw.githubusercontent.com/wangjohn/agent-archive/main/schemas/source-bundle.schema.json` | One line of a `source.<sha256>.jsonl.gz` bundle (source schemas 2 and 3): the header, a native record, a text transcript, or a piece of supplemental evidence. Ordering rules a line schema can't express are in its `description`. |
| [`eval-export.schema.json`](../../schemas/eval-export.schema.json) | `https://raw.githubusercontent.com/wangjohn/agent-archive/main/schemas/eval-export.schema.json` | One line of `agent-archive eval export` output: a session record or an error record. It refers to `metadata.schema.json` for the fields the two share, so a validator needs both. Its own `schema_version` and stability rules are in the [eval export design](../../dev/specs/eval-export.md#stability). |
| [`search-output.schema.json`](../../schemas/search-output.schema.json) | `https://raw.githubusercontent.com/wangjohn/agent-archive/main/schemas/search-output.schema.json` | Versioned search results, bounded context and explicit refresh output. |

The `$id`s resolve to the files on `main`. Pin a commit in the URL if you
validate against a fixed version.

## What the schemas promise

- Enumerations (parser status, lifecycle state, turn outcome, skill
  coverage, evidence kinds, and capture gap codes) list exactly the values
  this build writes. Readers should still accept values they don't know:
  a later version may add one, and older sidecars may carry retired ones.
- A change that removes or redefines a field bumps the document's
  `schema_version` (see [versions](../../dev/maintainers/versions.md)); adding an optional field
  does not. `repo_key`, added in parser `0.16.0`, is such a field: `repo-`
  and 16 lowercase hex digits (a hash of the normalized git `origin`; never
  the address). It is absent when the project had no `origin` or was gone
  when the metadata was derived. See [privacy](../security/privacy.md).
  `git_head` is another: the commit the session started on (with a dirty
  flag) and the last one a stop hook saw, as full object names; absent
  when no hook recorded them ([JSON output](json-output.md#show)).
  `replay` is another: present only for a session a replay tool ran
  ([JSON output](json-output.md#replay-sessions)).

## How they are kept honest

`internal/archive/schema_test.go` validates, with
`santhosh-tekuri/jsonschema/v6`:

- every line of every fixture's source bundle, and its metadata sidecar as
  both a hook-captured and an imported session
  (`TestFixtureOutputMatchesPublishedSchemas`), and that `repo_key` accepts
  only a key, never an address;
- that each schema enum matches the Go constants value for value, reading
  the constants from the source (`TestSchemaEnumsMatchGoConstants`);
- that both schemas list exactly `archive.CaptureGapCodes`, and every gap
  code the package writes is in it.

`show --json` output is not an instance of `metadata.schema.json`: it adds
`linked_session_availability` (see [JSON output](json-output.md#show)).

The `machines --verify --json` `verification` object follows
[`machine-verification.schema.json`](../../schemas/machine-verification.schema.json).
It describes bounded observations and explicitly unknown inventory visibility;
it does not authorize provider changes or establish ownership.

Revocation journals and operation objects follow
[`revocation.schema.json`](../../schemas/revocation.schema.json). They contain
per-key provider outcomes and optional explicitly unverified request selectors,
never secret values or deletion authority copied
from bucket claims. Operator proofs are private inputs; they are not uploaded.

Second-machine capture scope follows
[`project-scope.schema.json`](../../schemas/project-scope.schema.json), a local
input to `setup --project-scope` or `setup --project-scope-file`. It preserves inclusion and exclusion decisions
as a single transfer. Repository matching and resolved-path containment are
runtime checks beyond the JSON schema; these rules are not uploaded.
The input array has at most 4,096 rules. Transfer also refuses saved or resulting
destination scopes above that size and unresolved saved symlinks.

## Retained Codex history

Source schema 3 is a self-contained Codex history snapshot. Its bounded `history`
manifest identifies the selected physical rollout and maps retained records to
physical spans and raw unsigned ordinal ranges. Each native-record envelope
carries its exact `ordinal` separately from the filtered native object. Records
removed for privacy leave ordinal gaps; refiltering recomputes retained span
indices. Ancestor content remains safe retained evidence, while own activity
counts exclude inherited records. A same-thread revert prefix remains owned.

Metadata schema 2 points to the active source and up to 64 preserved native
revision references under that same archive session's prefix. A preserved
reference may name a legacy source-schema-2 bundle. Optional
`source_schema_version` and `filter_version` record that revision’s own format
and privacy provenance; readers compare them with the retained bytes. When
absent in older sidecars, readers validate the bounded source header without
inheriting the active revision’s filter version. Readers validate every pointer
before selecting one. The source-set digest includes active and preserved capture
times and revision provenance, invalidating earlier receipts when these change. Ordinary sources and sidecars continue using
schemas 2 and 1. This reader rollout keeps history publication, recovery,
refilter mutation and deletion protected by the matching revision lifecycle
and writer protection are installed.

See the [reader contract](../../dev/specs/codex-history.md) for identity,
selection, resource limits and compatibility details.
