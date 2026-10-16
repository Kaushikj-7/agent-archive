# macOS compilation seed retention

The product seed serves both native smoke tests and macOS lint. On a cold
trusted-main producer, production/race compilation runs before the pinned lint
and deadcode binaries are built. The seed has a two-GiB payload cap. Keeping
only the newest Go build objects can therefore evict earlier product objects
in favor of checker-build intermediates, even though the finished pinned
checker binaries are retained separately.

The producer now captures the actual product Go action/object keys immediately
after product compilation. The priority record lives in runner temporary space,
outside every restored/saved cache path, and is bound to the exact source and
compiler/SDK context. Final publication evicts nonpriority recoverable Go objects
first, then priority objects if still necessary. Modules and pinned executables
are never eviction candidates. All retained bytes still receive the existing
checksum manifest and module verification. A final inventory counts actual saved
bytes, including manifest metadata; an oversized Mac seed continues uncached.
A restored Mac seed over that same total cap is discarded and rebuilt cold.
The logged raw payload size continues to exclude manifest metadata.
Priority is not test/check evidence.

This changes only cold macOS product seed publication. Linux and shared-tool
seed retention, all PR checks, runner counts, cache profiles, deadlines, and the
repository cache budget are unchanged. An existing trusted immutable seed stays
valid; this strategy takes effect when a new context/dependency produces a cold
seed. No result/verdict cache or additional producer job is introduced.

Measure the existing producer's total minutes as well as smoke/lint elapsed
and runner minutes, payload size, transfer size, and restore state. The new
producer-only key capture adds an inventory pass, a bounded metadata record,
and source/compiler checkpoints. Do not infer PR latency savings from the
retention model alone. Compare ordinary runs using the same source, image,
compiler/SDK, and verified cache state. Native body/discovery and fresh lint
remain independent obligations even when the seed is trusted.
