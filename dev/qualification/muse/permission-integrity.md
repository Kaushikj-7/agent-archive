# Permission transaction checksum qualification

The pinned schema-1 permission transaction SHA-256 recipe is independently
recovered. Nine distinct held-out native frames and a new final-source 1.4.9
fresh/resume run match exactly. Two later fork-campaign frames also match,
including native ordinal 2. **Complete production frame/admission validation
remains unimplemented; Muse capture stays unavailable.** The separate
[initial-root contract](root-creation.md) qualifies narrow logical-start semantics.

This supplement updates the interpretation of the historical
[1.4.8](results.json) and [1.4.9](results-1.4.9.json) observations without rewriting
their results, runner hashes, exact-vector manifests or derived corpus. The
[metadata-only results](results-checksum.json) record the new evidence and pins.
No native permission body, provider context, binary or full disassembly is added.

## Exact byte recipe

`content_sha256` is `sha256:` followed by lowercase hexadecimal SHA-256 of the
following concatenation:

| Position | Bytes |
| --- | --- |
| Domain | Exactly 39 bytes: ASCII `muse.session_permission_transaction.v1` followed by one NUL byte. |
| Outer ordinal | `outer_log_ordinal` as an unsigned 64-bit big-endian integer. |
| Transaction ID | `transaction_id` decoded from UUID text into 16 raw bytes in UUID/network order. |
| Child count | Array length as an unsigned 32-bit big-endian integer. |
| Each child, in array order | `child_index` as unsigned 32-bit big endian; decoded `record_json` UTF-8 **byte length** as unsigned 64-bit big endian; then those exact UTF-8 bytes. |

No delimiter, trailing newline, outer JSON escaping or reserialized child object
is added. Child JSON whitespace and key order contribute as literal decoded
string bytes. The outer discriminator, `frame_schema_version` and checksum field
itself are not additional preimage fields. They still require separate grammar
and supported-version validation. The domain selects this particular recipe;
changing an outer schema number cannot qualify another schema.

The checksum is unkeyed. Anyone able to rewrite the frame can recompute it; a
match does not authenticate a producer, grant capture consent or prove original
creation. Stream identity and record semantics bind through child bytes but
still need their own interpretation and validation.

## Independent evidence

Local bounded static inspection of the pinned macOS x86_64 1.4.9 binary located
the preimage builder at VM `0x110264870` and SHA-256 helper at `0x11025b550`.
Instruction evidence supplied the domain, byte order, UUID representation and
length prefixes. Separate Python implementations then recomputed stored native
digests; export acceptance was not used to guess a successful hash.

Binary pins:

| Producer | SHA-256 |
| --- | --- |
| 1.4.8-R6256.1 | `94ea8fd1c73664c22e5c6157d8706945b667b11bd1c7c51691811e79f17dfcf1` |
| 1.4.9-R6297.1 | `4c6ed04ccd5a453bd7b38799aeb9eb290b8635b32c95b5a35e6950a2f20446cb` |

The nine held-out frames comprise five 1.4.8 root/native-child frames, two earlier
1.4.9 qualification roots, and two new 1.4.9 MSP roots with server-minted and
custom UUID identities. Root and native-child permission payloads differ in
length and content. All nine match when recomputed using the final qualification
runner. The reviewed 1.4.9 vector with file SHA-256
`a8f9520d4e5d53ab40714627c52a4645dfee1f9fc0d2f968c4cad8c0e7e840c1`
has a 2494-byte preimage and computed digest
`b3c663f30b51bf1221f39c5acbe64efb8d218420a976431e5d21af92a77d0c29`,
exactly matching its stored checksum. Raw lines remain outside the repository.

All nine held-out frames are schema 1 with two children and ordinal 1. The new
final-source campaign produces one additional distinct frame with the same
shape, independently checked before and after resume. Two later distinct
1.4.9 fork-campaign frames also match the same final-source helper: source
ordinal 1 and fork ordinal 2 physically on line 2. This extends the hash encoding
evidence; fork authority is excluded from the initial-root lane. The new
final-source exec campaign passes all **24**
`exec-utc` assertions with exactly two synthetic loopback Responses requests.
The first attempt was denied permission to bind the loopback listener before
creating a session; that failed report is recorded separately. The successful
attempt used the same source and isolated environment. Processes were reaped,
the endpoint shut down and its thread joined. See the [safe runner procedure](README.md#run-safely)
and result hashes rather than relabeling earlier native runs as current-source
evidence.

## Byte binding and separate grammar

An earlier isolated offline 1.4.9 export campaign tested 27 copies of one
synthetic frame. All processes exited zero with empty stderr; acceptance below
uses `diagnostics.unparseable_lines`, separately from exit status and digest
comparison. This research campaign is distinct from the final-source runner.

| Change | Stale checksum | Recomputed checksum |
| --- | --- | --- |
| Exact baseline | Accepted | Same baseline |
| Transaction UUID changed | Rejected | Accepted |
| Compact child JSON with reversed envelope keys | Rejected | Accepted |
| Ordinal 2 physically on line 1 | Rejected | Rejected |
| Leading or internal child JSON whitespace | Rejected | Rejected |
| Remove a child or reverse the child array | Rejected | Rejected |
| Outer schema 2 | Rejected | Rejected |
| Reverse outer frame keys or child-wrapper keys | Rejected | Rejected |
| Change source display name to ASCII, raw non-ASCII or escaped non-ASCII | Rejected | Rejected |

The accepted recomputed UUID and child-key-order cases demonstrate byte binding
independently of canonical-frame rejection. Other rejection cases leave separate
native grammar or semantic checks in play. Both ASCII and non-ASCII display-name
controls reject, so these cases do not qualify native multibyte-child acceptance.

## Runner scope and remaining gates

The existing [qualification runner](../../../scripts/muse_qualification.py)
computes the digest incrementally without building a transaction-sized preimage.
It validates hash-input types, integer widths, UUID text and strict UTF-8, caps
children at 4096, and applies the harness's existing 32 MiB aggregate child-byte
budget. This is a harness resource bound, not a claim about native format limits.
Reports include only positions, child counts, expected/computed hashes and
matches. Fresh and resumed exec assertions require a nonempty set of matching
permission hashes. Native export diagnostics remain separate assertions.

The [offline controls](../../../scripts/test_muse_qualification.py) include fixed
independently calculated ASCII and multibyte encoding-only known answers,
nontrivial big-endian ordinal/UUID bytes, child/order/count/identity mutations,
stale hashes, strict input/range/UTF-8 rejection and aggregate budgets. These
artificial objects are **not admissible native transactions**. A mocked clean
native export cannot hide a stale hash in the exec qualification assertions.

The helper is not a full native decoder. Closed and canonical outer grammar,
duplicate keys, physical ordinal, consecutive indices, permission-child
semantics, profile/snapshot constraints, unsupported schemas and checkpoints
need separate qualification before production use. Native counts other than two,
native multibyte child acceptance and noninitial transaction shapes beyond the
observed ordinal-2 fork remain unqualified. Copies preserve valid checksums and
identity. [Initial-root logical start](root-creation.md) has separate qualified
semantics; complete creation/admission validation is not implemented. No provider
accounting, publication, retention, Linux/arm64 or production capture capability
follows from this checksum work.
