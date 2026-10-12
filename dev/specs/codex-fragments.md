# Codex fragment foundation

Status: proposed format and executable schema checks only (step 6B1), reconciled
against `e1436e356d7c74e4f8cf2c5b26713766564e59f1` on 2026-10-12. No fragment
decoder, native filter route, registration, publisher or user option is shipped.
The [recovery proposal](../proposals/historical-backfill-recovery.md#step-6b-fragment-readers-lifecycle-and-import)
tracks the dependent work. The [complete-history contract](codex-history.md)
and its validators remain unchanged.

## Representation and identity

The proposed artifact is gzip-compressed JSONL. Its independent discriminator is
`kind=fragment_header`, `fragment_version=1`; it is not source schema 2 or 3,
metadata schema 1 or 2, or a complete session. The
[fragment line schema](../../schemas/codex-fragment.schema.json) describes each
line. Exactly one header appears first, followed by exactly `record_count`
`fragment_record` lines. Every line ends in a newline; blank, duplicate-header,
unknown-kind, unterminated and surplus lines are refused. A decoder must reject
duplicate JSON object keys rather than accepting last-key-wins interpretation.

The header binds a validated stable Codex `thread_id` and the available
`physical_rollout_id`. The latter identifies this surviving physical file; it
does not select a current lineage tip. UUIDs are canonical lowercase, and no
path, native-home locator, archive-session reservation, destination permission,
parent link or creation-time claim appears in the envelope. Capture time is the
archive observation time, never native creation. A future registry must bind the
agent-qualified logical thread to the eventual complete archive identity without
reserving the ordinary session index or counting fragments as independent sessions.
Conflicting known identity remains unresolved, not a new identity namespace.

Version 1 carries one physical rollout per artifact. Records retain physical
order. An optional `ordinal` is the exact unsigned 64-bit native physical ordinal;
absence means unknown, not zero. Known ordinals must increase strictly across the
artifact, including across records with unknown ordinals. Gaps are allowed and
do not establish raw-prefix agreement, ancestry or contiguous coverage. Never
invent ordinals from retained-array positions. Header `record_count` is between
1 and 1,000,000. Missing evidence has 1–64 unique entries; an available dependency
UUID may accompany a content-free reason. A reason is not permission to read or
retain the dependency. Do not disclose a forbidden dependency locator or content.

Decoder limits must include a 64 KiB header, at most 65 MiB per record line
(the existing 64 MiB native record limit plus envelope), and a 64 MiB total
uncompressed artifact. Streaming reads charge the existing shared read budget;
cancellation closes resources. A stricter caller limit wins, and no caller can
raise these ceilings. The schema checks line structure and counts' bounds; these
cross-line, byte, resource and duplicate-key rules require the future decoder.

## Incompleteness and activity ownership

Every header says `completeness=incomplete` and
`activity_ownership=unknown`. Missing-base, ambiguous-tip, incomplete-inventory,
unavailable-current-selection, excluded-dependency and unproven-ownership reasons
remain distinct. Even an attributable historical project does not establish who
owns surviving activity. Conversely unknown activity ownership does not authorize
unknown project attribution. The [historical-only policy](backfill.md#historical-only-ownership-and-bounded-mapping-files)
continues to govern destination permission, app allowlists, exclusions, removal
records and future-capture boundaries.

The first consumer must explicitly label fragments **incomplete; activity ownership
unknown**. It must not derive aggregate tokens, counts, titles, native creation,
complete handoff, replay or ordinary session metadata from these records. Retained
event timestamps and message roles are evidence, not claims of own activity. The
initial type must stay separate from `SourceBundle` and `Metadata`; accepting a
fragment as input to their parser or complete-history validator is forbidden.
No implicit conversion, inherited-message ownership guess or complete-history
fallback is permitted. Independently proven own activity requires a later reviewed
contract, not a boolean inferred from this header.

## Supported records and privacy gate

The initial schema deliberately supports a narrow subset of filtered Codex shapes:
identity-only `session_meta`, model-only `turn_context`, text-message
`response_item`, and user/agent-message or task-marker `event_msg`. It closes their
record and payload keys. Tool calls, token accounting, other record variants and
other agents are outside this initial format. Parseable JSON does not approve
their retention. Text is bounded to 65,536 Unicode characters per field and
4,096 text blocks per message, in addition to the byte ceilings.
The first filter route must either safely omit an
unsupported record with an honest gap or leave the candidate unresolved; it must
not retain it under a generic JSON/text variant. Expanding these shapes requires
reviewed schema, filter and privacy tests.

`capture` records Codex adapter, source format, filter version and archive
observation provenance. A header or successful schema validation cannot certify
privacy or authorize publication. Before retaining even a supported shape, the
future filter must validate native identity and format, approved confined source
roots and per-record content authority, then positively initialize the existing
privacy filter with all required context. Missing ancestors are not assumed safe.
Failure to initialize the privacy context leaves the candidate unresolved; no
generic JSON/text fallback, raw transcript staging or relaxed filter is allowed.
Records must pass the existing allowlist, secret redaction and injected/hidden
instruction removal before fragment materialization. Text from an excluded
ancestor must not enter retained or displayed records.

The future decoder verifies structure and identity bindings, not privacy consent.
Its caller must verify exact compressed checksum/length and artifact authority
before decoding. A `session_meta.payload.id` must equal the header thread UUID;
a native root `session_id`, if present, is evidence only. Date-time fields must be
valid timestamps. Source identity, authority and privacy proof must remain valid
through filtering/staging; changed or unavailable proof requires retry or refusal.

## Delivery and lifecycle gates

1. This foundation adds only the proposed schema, contract and schema tests. It
   does not allocate a configuration writer version or change filter/parser versions.
2. Add a decoder and context-proven filter together with the first real explicit
   incomplete consumer. Keep it outside ordinary metadata, usage and complete
   handoff/replay. Test unsupported shapes, missing context, identity conflicts,
   unknown ordinals, bounded reads and truthful display.
3. Add durable staging and publication only after privacy replacement, interrupted
   recovery, retention, whole-artifact removal, purge and local/remote older-writer
   protection understand every referenced fragment. Coordinate the new permanent
   capability name/version with other work; historical-projects-v10 already owns
   version 10. Do not infer a remote downgrade boundary from a local config fence.
4. Add an explicit reviewed import opt-in and separate logical fragment registry.
   Dry-run stages nothing. An incomplete artifact must not block later complete
   import, bypass historical permissions, resurrect tombstones, or double-count a
   logical thread. Batch resume identity includes the option and selected evidence.
5. Complete repair needs an enrichment/contribution journal before enablement.
   Current `backfill.Batch` and undo own whole sessions; no such generic journal
   was found in the reconciled implementation. A source publication journal alone
   does not establish batch-owned repair undo. Repair must preserve the same logical
   identity and verified predecessor references; undo restores precisely the prior
   fragment/reference selection and admission/retention/removal state without
   deleting an older complete import or manufacturing new activity. Later privacy
   policy still governs restored references. An interrupted or conflicting repair
   preserves the prior readable state and remains pending.

Synthetic schema tests establish only the proposed line contract. They do not
establish fragment archival, native app support, provider acceptance, downgrade
safety, step 7 coverage or a published release.
