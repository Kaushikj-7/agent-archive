# Headless local/archive picker

`internal/cli/picker_model.go` owns the offered union independently of terminal
rendering. `First`, `Older` and `Select` retain an immutable captured
archive view. Each scope has its own local position, remote buffer, cursor,
suppression set, emitted physical keys and visible prefix. Every operation checks
the captured lifetime before publishing a prefix; an operation served entirely
from buffered or local candidates obtains its own fresh head observation.
The caller prefetches the initial page, so initial CLI setup makes two head
observations around page/model construction, plus one per exact local identity
lookup batch. This fixed CLI overhead is separate from the reader page API's
one-observation cost and the registration-dependent native inventory work.
Refresh reconstructs the model with a new captured archive view and observed
native activity; cached scopes do not renew the ten-minute snapshot lifetime.
The browser layer owns that reconstruction rather than an unused model method.

Supported bare handoff setup consumes this model through `picker_handoff.go`.
The format probe and typed scope guard run before local activity/label reads.
Unsupported formats and named project scopes retain the complete compatibility
loader. The shared list/show browser layer connects bounded pages, remote Older and
cancellable asynchronous search; its state and selected references use the same
captured view. Actual command-scale costs are tracked by
[picker qualification](../maintainers/picker-qualification/README.md).

Exact canonical lookup classifies all relevant top-level non-replay local
registrations in batches of at most 100. It shares the page snapshot and finds
uploaded copies outside the page, scope, or handoff membership. A canonical
alias collision is an actionable lookup error. Native activity preserves
provider batching while isolating canonical harnesses, so another harness with
the same session ID cannot overwrite an observation.

Local membership delegates to `sessionScope.contains`, preserving registration
RepoKey precedence, configured roots, symlinks and path fallback. A readable
local registration overrides activity unconditionally, including movement
backward. The entire overridden identity is suppressed remotely before merging.
Unreadable native activity retains archive-only eligibility except under
`--source local`. Known zero-prompt archive copies can revive through readable
native labels only in auto/local mode.

The union order is effective activity descending, then archived provenance:
original archive activity, capture, raw harness and session ID; archive-associated
rows precede local-only rows at equal activity. Local-only ties use registration
ordinal. Local presentation capture times never replace archive tie provenance.
Remote pages retain P+1 bounded candidates where supported and keep their
unconsumed lookahead buffer and cursor. More is proven by an accepted lookahead;
unknown eligibility is explicit incomplete continuation. Per-operation refill is
limited to `2 + ceil(R/P)` pages, where R is the relevant suppression inventory;
this local-dependent additive work is separate from bounded archive page cost.

Each operation allows 50 uncached native label attempts, counting failed and
no-prompt reads. Native read/parse failures are unknown eligibility rather than
known no-prompt; they are not cached and leave the ordered local candidate
pending for Older to retry. Cancellation returns an error without consuming
the visible prefix. Known no-prompt results remain safely cached. Labels are cached by canonical harness/session, activity,
registration facts and source policy. When an unresolved local candidate could
be next, the model stops with an honest prefix and `Incomplete=true`;
`More=true`, `Total=-1`, and `Empty=false`. Older continues from that position.
Leading empty locals therefore cannot produce a false empty scope or silently
trigger all-project fallback. Activity inventory work and native label attempts
are separate costs; local inventory is not an archive-size-independent cost.

Ordinary archive failures, including a store that cannot open, preserve supported
auto/local offline selection using the same native attempt budget. Archive
association remains unknown and never receives “not yet uploaded.” Offline auto
cannot prove the combined scope empty or widen it, even when the native subset
is complete; refreshing the archive resolves that uncertainty. Source-local can
prove its native-only scope empty. Native activity batch errors are explicit
failures, since missing ordering observations cannot establish the top page.
Archive-source failures, cancellation, stale views, lookup ambiguity and catalog
work-budget errors remain explicit failures. Hidden-child
totals are unknown at startup; each archived parent uses its card's child count.

Opaque references travel with browser rows into handoff options and the selected
resolver. Before native or archived source reads, archived associations pass
`HydratePicker` and `ResolvedMetadata`: immutable hash, identity, projection,
captured authority and current activation are checked. Safe native labels never
become selected metadata authority. Auto/local reread current native registration
and filtered sources; archive fallback uses the verified immutable source pointer
without a same-ID metadata refresh. A rewrite/deletion requires refresh.

Complete compatibility search stays lazy. Its bounded opt-in uses one strict
activity inventory; a batch failure fails search explicitly. Ordinary local label
failures preserve known archive matches with a partial local-eligibility notice
and never poison the shared known-negative label cache. Legacy callers retain
their two-result/native best-effort API defaults. Context/error-bearing browser
search reports loading, cancellation and read failure instead of substituting
the loaded browse prefix. Clearing search restores the valid paged view.
Search-only selections also retain
exact captured references, including child rows.
