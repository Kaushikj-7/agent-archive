# Initial ordinary-root creation contract

A narrow native logical-start contract is qualified for the observed **Muse
1.4.9-R6297.1 macOS x86_64 exec and no-turn MSP initial-root lanes**. Native code
reserves the start clock and serializes that value into `recorded_at` of both
initial permission children. A strict complete initial-root discriminator can
therefore select that reserved logical start, coherent identity and initial
workspace. **The production prefix validator, same-handle source checks,
uniqueness resolution and consent admission are not implemented; capture stays
unavailable.**

This is a logical conversation start, not an inode birth or identification of a
unique original physical copy. It does not establish creation from arbitrary
permission timestamps, the earliest event, an opening hook or a cached API field.
The [metadata-only evidence](root-creation-results.json) pins the binary, local
probe scripts, native inputs, reports and selected field facts. Raw permission
bodies, private profiles, provider context and full disassembly stay outside Git.
Historical [Q0 results](results.json) and [1.4.9 results](results-1.4.9.json) keep
their original provenance; this later adjudication supplies the field linkage.

## Reserved start reaches the retained field

The exact binary is 389577808 bytes, SHA-256
`4c6ed04ccd5a453bd7b38799aeb9eb290b8635b32c95b5a35e6950a2f20446cb`.
Bounded instruction inspection establishes this dataflow:

1. `reserve_session_start_recorded_at`, VM `0x10cbda420`, reserves native
   wall-clock microseconds or a value from the installed record clock. Its
   no-record-clock branch reads `SystemTime::now`; the other branch reserves and
   advances the explicit clock. This is intentional start reservation.
2. The caller forwards the reserved value through `prepare_session_start`
   (`0x110186470`) into permission initialization (`0x11018ecf0`), where the
   timestamp arrives at input offset `0x260`.
3. Initialization places that same value at offset `0xb8` of each of two
   `0xe0`-byte records. Literal payload type pointers identify the declaration
   and initial profile commit. Both assembled records reach the transaction's
   actual child serializer.
4. Record serializer `0x1101c59d0` passes record offset `0xb8` to helper
   `0x1101c93c0`, which selects the literal field `recorded_at`. Field serializer
   `0x110260c40` reads that numeric value and writes its decimal bytes into child
   JSON. This closes the clock-to-retained-field link.

Function names alone are not the proof. Instruction-level value propagation,
record layout and literal serialized field establish it. Cause selection is
qualified with actual fresh, child and fork records; this work does not claim a
complete reverse-engineered cause enum or all writer entry paths.

## Required complete initial-root discriminator

The qualified contract is conditional on the whole supported startup prefix:

- Physical first line is the supported schema-1 permission transaction with
  outer ordinal 1 and independently matching [checksum](permission-integrity.md).
  Its children have indices 0 and 1, stream sequences 1 and 2, equal exact
  integral timestamps, and one coherent session stream. Payloads are the
  schema-1 declaration then initial profile commit, with initial epoch 1 and
  `cause=new_session_default`.
- Envelope and payload schemas, record IDs, stream IDs, payload session IDs,
  sequence progression and timestamps must be coherent. A unique original
  `session.opened.observed` with `resume=false` corroborates original startup;
  it does not supply the selected start clock.
- The root's own initial metadata supplies its workspace. In these roots,
  permission `workspace_roots` is empty and cannot supply cwd. MSP intake
  metadata, subsequent own metadata and the approved workspace must agree;
  later metadata cannot silently replace the original binding.
- Require the applicable exec or MSP prefix below rather than searching past
  unknown records for convenient timestamps. Unsupported fork, `child_spawn`,
  checkpoint, prune, rewrite or missing original-prefix authority must refuse
  that lane; an incomplete prefix remains pending. Missing, contradictory or
  duplicate original openings, identity or workspace must not qualify.

Observed startup ordering is mode-specific:

| Lane | Initial physical lines / stream sequence |
| --- | --- |
| Exec | Permission frame line 1 / sequences 1–2; own metadata line 2 / sequence 3; original opening line 3 / sequence 4. |
| No-turn MSP | Permission frame line 1 / sequences 1–2; capture selection line 2 / sequence 3; command intake line 3 / sequence 4; `intake_v3` line 4 / sequence 5 with activation `startup`; `session.started` line 5 / sequence 6; own metadata line 6 / sequence 7; original opening line 7 / sequence 8. |

The MSP command intake, version-3 startup intake and started record must be
related to the same startup command and root identity. Exec has its own root
facts and short prefix; do not impose the MSP prefix on it. These are frozen
observed lane facts for the future strict validator, not a validator delivered
by this qualification PR or a claim about all TUI/mode/version/platform paths.

## Opening time is later than logical start

The reviewed exec root reserves `1792083855882704` microseconds; its original
opening is `1792083858677136`, **2.794432 seconds later**. An activation at
`1792083857000000` falls between them. This conversation began before activation;
using the opening as birth would wrongly move it into the newer consent window.
The reserved initial-root time avoids that concrete counterexample.

Later prompt/stop/resume or metadata observations cannot create a fresh owner or
reset its original start. A complete original prefix and the locked project,
destination, activation and removal policy checks remain necessary. A hook may
create a pending owner; it cannot substitute for qualified retained authority.

## Resume, copies and counterexamples

Zero-turn MSP experiments used constructed HOME/XDG/workspace/temp roots, empty
AWS files, file credential backend and a synthetic loopback endpoint. They
issued **zero model turns and zero provider requests**. Normal process deadlines,
stream limits and owned process-group cleanup applied. The first listener bind
was denied before session creation; a normal authorized retry is recorded
separately. This is separate research from the new checksum runner campaign,
whose 24 assertions do not qualify a creation validator.

| Observation | Consequence |
| --- | --- |
| Exec resume preserves prefix and appends opened(true). | Original logical start and workspace continue. |
| Successful custom-ID MSP resume preserves the original nine lines, appends resume intake/started/metadata/`session.resumed`, and adds no opened(true). | Opened(true) is not required for every native resume lane. |
| Sibling server-minted empty root returns `sessionNotFound` in the second lifecycle. | This resume attempt failed; no broader resume/index coverage is claimed. |
| Exact new-home copy and relocation under a 1999 dated folder retain original bytes, identity and read projection. | Same logical start; competing physical sources require separate bounded uniqueness resolution. |
| Normal fork starts with timestamp-less `session.fork.created`, then permission ordinal 2 with inherited source time and cause `fork`, and has its own opened(false). | False opening and permission time alone cannot qualify an initial ordinary root. |
| Two historical 1.4.8 native children begin with permission ordinal 1, sequences 1–2 and cause `child_spawn`. | First-frame shape alone cannot qualify a root; child creation remains a separate lane. |
| Eight full-session exports (exact baseline plus seven named mutation cases, six distinct mutated byte inputs) all produce zero diagnostics. | Export is not an independent identity or creation validator. |

MSP `createdAt` changed between start, read and list responses: startup intake
time and permission time appeared on different projection/cache paths. Custom
valid UUIDv7 roots encoding 1970-01-02 and 2036-10-14 both started; their tested
root reads, including cold reads, used current permission time. A UUIDv4 control
was rejected. Cold list returned empty pages while direct reads succeeded, so
cold-list coverage is not established. A static helper named
`resolve_branch_created_at` can select embedded UUID time in its own branch;
that helper is **not proven to be the tested root read path**. Do not generalize
it into UUID-derived root creation authority.

## Reproduction boundary

The pinned local no-turn probe scripts and their inputs/results are identified
by SHA-256 in the metadata. Their normal MSP sequence initializes the connection,
starts a root without a turn, reads/lists it, closes and reopens the server, then
resumes the same root. Separate cases copy/relocate the exact snapshot into a new
private home and perform direct reads; another starts a root and forks it without
a turn. Old/future UUID inputs and the UUIDv4 negative control are recorded in
the research reports. Use the same [isolated environment and pinned direct
binary](README.md#run-safely); never point these experiments at personal state.
The existing runner's model-turn MSP case is not this zero-turn campaign.

## Next implementation gate

The local semantic contract is concrete; upstream clarification is not required
for this narrow lane. Next implement the strict prefix validator and negative
coverage for child/fork/checkpoint prefixes, malformed or conflicting startup,
resume/copy continuity and activation between reservation and opening. Validate
from the same confined descriptor, resolve duplicate physical candidates, then
commit protected owner/binding and consent facts transactionally. Unsupported
lanes must remain unavailable. This qualification does not implement those
operations or grant publication, retention, child or full-platform authority.
