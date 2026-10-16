# Current publication foundation composition

## Current integration work (2026-10-16)

The historical checkpoints below describe earlier source candidates. The current
unpublished integration uses outer journal/publication version 5 with distinct
`source_set_commit5` and the sticky configuration-17 source-set capability. It
preserves ordinary ReadySelection2, staged3, Cursor4 and catalog authority. The
inner source-set commit remains version 2; it does not reuse the existing outer
protocols or their wire fields. The bounded encoder lives in `internal/jsonwire`;
`agentmeta` retains its pure sizing API.

The integrated replay fixes have focused positive and refusal coverage for legacy
original-parent provenance, native-child preparation, exact ordinary and history
privacy successors, request ownership, source verification and bounded listing
repair. These results do not establish final runtime qualification. A measured
idle-pass regression remains: settled SourceSet5 peers currently require complete
published-state validation on each pass. Raw leading summaries cannot replace that
validation because retention, deletion and link maintenance consume their facts as
authority. Any optimization must preserve closed-wire validation and exact bytes,
owner and configuration binding, charge retained memory to the existing budgets,
and report cold-process work and file bytes read as well as decode allocations.
Final immutable-source review, complete required checks and the unfinished
live-WAL/provider/deletion integrations remain separate gates.

## Historical composition checkpoints

Source preparation pinned actual main65917b84d396bce94dfe1b1c40e80fab920634e2 plus ROOT460 f8d6d773b6c226ddda860b89cd6bb302565b316c. Original352 f20f609f3a6458be25c0bdd7811d4ab5d6ae6c72; original35630d7de1fc2bce9671f22984c91bce124c6991e87. The initial design checkpoint is retained below. The current source candidate implements the bounded serializer plus ordinary-ready producer slice described at the end. It does not activate any configuration or qualify a runtime/native/provider gate. Original refs remain untouched.

Two exact wire conflicts prevent a safe additive port:

1.352 config writer publication-composition-v8 requires PublicationCompositionProtection plus DurableStorageProtection. Current config8 already means retention-forever-v8; current9 import staging,10 permanent historical projects and11 catalog have their own layered protection. Reusing numeric8 breaks existing decoding/floors or wrongly grants publication capability. Reserve a distinct exact publication-composition-v12 fence above the FULL current validation chain. Config schema12 is unrelated to remote catalog protocol10 or publication journal protocol2.
2.352 PendingPublication.Commit (*PublicationCommit) uses `json:"commit,omitempty"`; current460 PendingPublication.Catalog (*CatalogPublication) uses EXACTLY the same JSON field. Adding both declarations would make encoding/json ambiguous and omit both at the same nesting depth, erasing selecting AND catalog transaction authority. The original352 writer version/serializer cannot be accepted unchanged, and moving Catalog's field would break current origin-bound journal digest/recovery proofs.

Concrete integration alternative:

- Retain current Catalog's existing `commit` wire and its complete current journal digest. Give new PublicationCommit a distinct `selection_commit` wire, explicitly serialized/decoded under publication journal2 and the new config12 capability. The field name is proposed, not silently activated. Any compatibility reader for experimental old352 journal2 `commit` must discriminate exact object shape/fields and protocol, reject ambiguous/mixed forms and unknown authority, and never manufacture catalog ownership. Published experiment/old artifact adoption is UNKNOWN, so backward replay acceptance needs explicit source-format evidence; current catalog format must remain readable without reinterpretation.
- Private sequential preparation writes exact original selection, policy, parent/NativeChild/historical owner inputs and bounded stage handles while no catalog mutation is admitted. Once the complete selection_commit is ready and immutable, freeze the catalog mutation and bind the ENTIRE ready selection to the current CatalogJournalDigest, including source selection, exact raw metadata, predecessor, destination, admission and policy. After catalog origin admission, mutation reuse/source-policy changes remain refused; progress can no longer alter the frozen selecting journal. Attempt/recovery normalization must stay exactly bounded to execution state, not weaken the digest to metadata only.
- Unknown predecessor remains unknown until exact remote status proves the next raw body selected; absent authority cannot be inferred from missing local state. Preserve current staged ImportExpectedMetadataSHA256/ImportRetired, historical-project proof, original NativeChild/parent authority, current request token and local readback-before-release semantics. Source-first stages and original evidence remain until guarded publication, complete local save and covered request acknowledgment.
- Map closed immutable payload handles to CURRENT sessions/id/import-stage/stage.json only through a validation factory accepting the exact selected registration/digest and source checksum/size; do not transplant old admission-stages layout or broaden current32MiB structured stage bound into original64MiB allocation. Rooted stage read, quota coexistence, parent/child permission and original capture time remain independently verified. Existing current stage can supply a subset of original356 behaviors but is not its complete proof engine.
- Reuse main453 Reservation handles for every new bounded scratch/payload owner, including delayed releases; never restore old NativeReadBudget.Reserve/Release. Port JSON encode/raw traversal into current agentmeta counter/codec architecture with explicit token handling and retain current JSONWireBound/Refused, rather than restoring removed internal/jsonwire or moving package blindly. Native bounded codec leases and cancellation/checked-close must be fully wired before any selecting publication can be enabled.

Reviewable implementation sequence, ROOT-owned:

1. Joint state wire/decoder/digest and layered writer guard, with unchanged current-format replay and explicit rejection tests for mixed catalog/selection Commit, numeric12/missing/wrong writer, removed underlying flags and changed raw/body/source/policy. This slice must include actual pending Read/Save validation callers and producer guard; detached unused type declarations are not a feature implementation. Keep the branch draft until current ordinary ready publication has a meaningful supported caller.
2. Ordinary ready-source factory and collector publish/readback integration, before history/native or broader privacy. Propagate received/admitted context explicitly; catalog ownership starts only after closed selecting proof is immutable. Test ordinary exact retry, source mismatch, unknown remote winner, failed local save and unchanged newer hook/request handling. Preserve current all-reference authority and caps.
3. Historical/private preparation/migration/native header and typed privacy receipt factories with original oldest replay; owner/destination/source correspondence and stage credit tests, preserving all existing original352 assertions. Frozen stricter catalog successors remain an intentional refusal until a durable new-slot/owner-checked handoff exists; draining older policy bytes or removing stage first is forbidden.
4. Original356 admission/group/resume integration against current stage ownership; only then359 live Cursor snapshot/VFS/workspace quota,360 full privacy maintenance,362 exact SessionDeletion/Restore engine. Those are concrete pending implementations, not gates already satisfied by460.

Raw source-only merge preview on f8d+352 is conflicted; it was not applied to this worktree. The conflict set includes current retained-budget/header, native codec, config floors, state journal/quota and collector caller files plus removed jsonwire package moves. A clean merge or file presence would not resolve the two wire collisions or frozen authority lifecycle.

## Bounded source candidate

The candidate adds exact publication-composition-v12, mandatory pending journal_version2 with distinct selection_commit, and published publication_version2 proof persistence. Current catalog commit protocol10 and existing catalog11 serialization/digest normalization remain unchanged. Both distinct authorities can coexist. Numeric12, missing/wrong capability, malformed or duplicate authority and experimental old352 commit shapes refuse.

Ordinary collector rendering freezes a ready selection before its first pending save and before catalog mutation admission. The current registration/destination, policy, parent/native-child, exact metadata/source reference/hash, request/ready-time/body binding and predecessor are frozen. Present predecessors are bound to retained metadata; missing local metadata never grants absence without a remote missing observation. Unknown predecessors only complete on exact next-body readback. A source-first retry rechecks remote metadata before metadata publication, then verifies exact metadata and source before local proof save/covered acknowledgment/removal. The public wire descriptor is always revalidated; an internally valid recomputed proof cannot replace an existing durable pending owner. Catalog ownership may attach afterward and covers the entire selection in its digest.

An opted-in selecting source is refused for metadata-only, revision/history, staged/historical import and unsupported policy successors. New request evidence and clock capping cannot replace frozen pending input. Legacy exact work can still replay; the default producer without the new protection stays unchanged. Current structured stage32MiB, source/history128MiB bounds and owner reservations are unchanged. Closed-stage/group/native/privacy factories from352/356 remain subsequent implementation, not features completed here.

At the initial source-only candidate d8dd6bcc246e4edc084b15cf0a78b8b1cd43cdc5, the new tests had not been run. They cover actual Store Save/Load/published snapshots, distinct authorities, stable attempt digest, mutation/replacement evidence retention, malformed wire, collector failure/reopen/exact retry and unknown winner readback. Later results below belong only to their exact tested heads. New API tests require the candidate; the separate producer probe uses existing public APIs and raw writer12 JSON, so the same source compiles on the pre-feature baseline.

## Pinned qualification checkpoint

Actual main is f92ec1ae054d71d1f8c2894a22f236122b8eb69f. ROOT ran the checks below serially. Source composition3fc9cd1f3841c14f63cc2a137b578fe307693097 adds only main's three changelog lines to tested cfd403d0420a32be6b49dd1b12be0a5f6e15a9db. Source-reviewed6fbaaf5da7078745f9633ce14358f2818942b0e9 commits the unchanged, independently reviewed95-line probe. Those source identities do not turn earlier runtime results into new-head results.

Raw log names below are under the recovery workspace. SHA256 values identify the preserved evidence, including failures.

| Exact head / check | Observed result | Raw log / SHA256 |
| --- | --- | --- |
| cfd403d0420a32be6b49dd1b12be0a5f6e15a9db, all-package compile | PASS, exit0; no tests run | work54-foundation-cfd-all-compile.log / 7c1df284728f5b9626be023619fb5ac344076b48d903972f18178ee2fe148ed7 |
| f51270492b58b4981135ab7bbf1cd3b51e33a667, unchanged reader controls | FAIL, race0.750s | work54-foundation-readers-beforef512-race.log / 9bf92a798891a88223782412ad5ccd44b0d6fda9ae6cd44c16cc498020e06e7b |
| ea9747e6f1ceee50162f4b10171de5413b81429c, unchanged first-owner controls | FAIL, race1.180s | work54-foundation-owner-beforeea974-race.log / f99a5af063043a6a645c26d6a58eebe7e07c19d7040b9c74cb069f0af04c72a5 |
| 54b307ba778052e884e53beff4595456e3d3a816, unchanged slot controls | FAIL, race0.970s | work54-foundation-slot-before54b-race.log / aca21b1147ab01c71a1a0db18d6194e6f93cecdb4686e6fd7cbf775fab185362 |
| cfd403d0420a32be6b49dd1b12be0a5f6e15a9db, ready state controls including those three same-source groups | PASS, race3.566s | work54-foundation-cfd-state-race.log / 5718ad0e7a820bef7abec33eae3ba160cd4cabf88a20b7af6617b11a2cc7ef3c |
| cf6cd947165201f3c31b983a5d0cd8764ec75514, public-API/raw12 producer probe | FAIL: unsupported selecting writer fence; missing feature, not an existing-main bug | work54-foundation-feature-beforecf6-race.log / 48ae8e8f6794e92a1298309ae69a5c27bd9d40c5a28e8a02e19bb2b37ecf6314 |
| cfd403d0420a32be6b49dd1b12be0a5f6e15a9db, same producer probe | PASS, race1.161s | work54-foundation-feature-cfd-race.log / 8f68b94f9b9b5a5e13c5ae696f012cca4fb52a4d42d171cc505e8e593a17daee |
| 3fc9cd1f3841c14f63cc2a137b578fe307693097, complete config/state race (ROOT session48468) | Config PASS1.982s; state FAIL109.189s; exit1 | work54-foundation-3fc-config-state-race.log / 0be5f35d5f1fa3959ee9e1509216ee380a9eb574ac8a4bf885327222407ee915 |
| cf6cd947165201f3c31b983a5d0cd8764ec75514, unchanged TestOpaqueEvidenceProtectsHistoryAndCorruptPending | PASS, race1.165s | work54-foundation-opaque-baselinecf6-race.log / cb4e8a2243dbb159dfeee24a32ca136476c298c030b25cb999e9674e03469cdd |

Identical paired test SHA256: ready_selection_readers_test.go c6dae5ed9d8e2586f8f8a1b064388ab40993919f27a0cb2e8281fd540bf069ac; ready_selection_owner_test.go 9595347b177348c67ba1c43cd3929907009a5ca0c795be2f22c678e1c5352bb5; ready_selection_slot_test.go f9fba64ed7749a34a01682500b16f6479819eced2a52ed4816500c2e73cb0a70. The unchanged public-API probe SHA256 is551ad8c2feb6247a7a16e21cd3967abc906dbdf8e7a14ecd9a6c99ca77234396. Compilation failure is not a causal before-test failure.

The complete state failure is material: the new selecting cleanup guard returned a raw corrupt-JSON error before the existing guard could attach ErrDurableStorageRecovery. The existing opaque-evidence assertion was preserved. Source correction22e8385706d5b36aa184b3669caad3d8f5cfb5e6 joins the recovery marker with the original cause and preserves missing-file behavior, all retained evidence and caps. No runtime result yet qualifies that correction or this documentation update; ROOT must rerun the same assertion and complete state suite after different final source review.

Publication/complete runtime qualification remains HOLD. Broader352/356 historical/private/stage/group factories,359 live Cursor snapshot/VFS/quota,360 privacy receipts and362 deletion/Restore remain unimplemented dependencies. Native/operator/provider/CAS/atomic absence/durability/full-size/RSS/performance gates remain HOLD. Remote observations and the scoped synthetic passes do not qualify them. Different exact-final source review and required protected checks remain mandatory before publication.
