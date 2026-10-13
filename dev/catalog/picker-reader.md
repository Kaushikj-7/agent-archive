# Bounded picker reader

`catalog.Snapshot.PickerPage` accepts a typed `PickerQuery`: browse or handoff
membership, an optional exact canonical harness, and a normalized repository
scope. `All` selects global roots. A RepoKey selects its repository plus the
missing-RepoKey ProjectID ranges; without a RepoKey, ProjectIDs select all
repository copies. Duplicate ProjectIDs are removed. At most 32 actual disjoint
ranges and a page size of 1–100 are supported. Named projects, model, skill and
arbitrary predicates retain the complete compatibility reader above this API.
`ErrPickerUnsupported` is distinct from a complete empty page and an I/O error.

Each result has bounded `PickerRow` presentation facts and an opaque immutable
`PickerReference`. Rows are not metadata or source authority. `Total` counts the
disjoint validated subtree ranges, and `Empty` proves their total is zero. All
errors discard the page, including `ErrPickerBudget`; an error does not prove
emptiness or a complete prefix.

Each logical page uses one head and coordinator observation (the opening
snapshot supplies the initial observations), then pinned internal counts and
walks. Every range retrieves at most P+1 candidates. A continuation stores each
range's last **consumed** key, so unconsumed lookahead is fetched again rather
than skipped. It binds the normalized query, membership, exact roots and format,
page size, snapshot nonce and original ten-minute snapshot lifetime. HMAC-sealed,
compressed tokens are at most 64 KiB, with a 256 KiB decompression limit. Wrong
scope, tampering, foreign snapshots, expiry and changed heads require explicit
refresh (`ErrStaleCursor`). Freshness checks on existing generic Snapshot APIs
remain enabled.

Node visits and decoded bytes, including cache hits, have explicit operation
budgets. Defaults are 8192 node visits and 32 MiB; limits also apply to counts
and lookahead. Byte-limited node GETs refuse before downloading an oversized
node. The conservative structural bound is O(S × H × (P+1)), including valid
underfull and unary trees, rather than assuming a minimum node occupancy. A
budget too small for that tree returns `ErrPickerBudget`; callers do not silently
scan the archive or fall back to expensive search. Canonical identity batches accept at most 100 identities and use the exact
canonical-harness/session lookup ranges. Zero proves absence, one returns its
physical raw-harness reference, and more than one returns
`ErrPickerIdentityAmbiguous` for the batch, with no arbitrary result. Lookup
includes children, replays, and known zero-prompt publications regardless of page
scope. These finite batches are measured separately from repository-page work
and local inventory.

`HydratePicker` accepts the original opaque reference, checks current head and
coordinator authority, reads only its immutable metadata body, verifies hash,
physical identity and projection, and returns `PickerHydration` with existing
consumable `ResolvedMetadata` provenance. Callers then apply existing current
source, replay and destination policy. Replacing the reference with an ID and
loading a newer same-ID rewrite is not equivalent.

Reader tests compare every page against an independent complete repository
oracle and exercise overflow labels, zero/unknown prompts, children, replays,
counts, expired/foreign/malformed cursors, changed heads, cancellation, missing
and corrupt nodes, and unary-tree budgets. Bulk immutable-tree fixtures avoid
retaining dead COW nodes, and construction is outside measured operations. The
10k/100k page measurement covers this reader, not whole CLI startup or provider
qualification; those remain separate integration evidence.
