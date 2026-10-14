# Verified CI compilation caches

Caches accelerate compilation and immutable tools. A cache never supplies a
previous test/check receipt or coverage proof. Current test bodies still execute
with their existing `-count=1`, and source/worker/verdict checks remain authoritative.
Every owned `go vet ./...` invocation still executes against current source and
emits a new full source/compiler receipt. Go's intrinsic analysis cache may
reuse vet facts/successful output when its package compilation action ID, tool
identity, analyzer flags, mode and dependency fact hashes all match. This is
normal source-keyed Go memoization, not restoration of an external verification
receipt. Seeds use `-vet=off` for test compilation; PR Go snapshots can contain
that normal internal Go state. Levenshtein verification directories and XDG
state are deliberately fresh, including the vulnerability database.

## Trust and identities

`go_cache_context.py` remains the identity/path/source authority. The worker
adapter is `go_cache_steps.py`; `ci_cache.py` adapts the same authority for
read-only consumers and main seed publication. Identity includes actual Go
and compiler binary checksums/version, module lockfile hashes, Go flags and
architecture/cgo controls, exact runner image, OS release and immutable tool
pins. Darwin additionally includes selected Xcode/SDK metadata and deployment
and SDK overrides. ImageVersion remains part of the compilation key; existing
independent vet receipt image compatibility policy is unchanged.

`compilation-seeds.yml` runs only on push to main, in three bounded standard-runner roles: Linux product, macOS product,
and Linux shared-checker compilation. It checks exact source and the push event before publication.
No PR-target workflow or PR code executes with main publication authority.
GitHub's branch cache isolation also prevents PR-scoped entries becoming a
main seed. A PR or fork can restore available default-branch seeds read-only;
forks never save. Missing seeds do not add a seed-build dependency to a PR.

All restored cache members are inventoried and checksummed before Go can consume them.
Broken extraction, absent/incompatible manifests and corrupt files are cleared
from the two controlled directories. Modules download and verify on hits and
misses. A restored module verification failure retries once from empty caches;
an actual module/source/compiler failure still fails the check.

## Storage and publication

The existing repository cache limit is unchanged: 10 GB with GitHub's normal
LRU eviction. No paid cache capacity, paid runners, images or self-hosted
machines are introduced. Only these directories are cached:

- `$RUNNER_TEMP/pr-go-build`
- `$RUNNER_TEMP/pr-go-modules/cache` (trusted seed archives only)

Main seeds have one immutable key per product/shared profile, exact context and module dependency set,
without a new key for each source tree or worker. All Linux workers share one
PR-scoped compilation snapshot; only successful same-repository worker zero
publishes it. Worker count still participates in PR namespace compatibility,
but increasing from four to eight workers does not create eight full copies.
Source-specific PR keys can be evicted normally; their contents are Go
compilation, never worker evidence. New runner/compiler/module contexts create
new seeds, and older variants remain subject to the same 10 GB LRU limit.

Main seed archives contain only Go objects/tools and immutable module download
archives, not extracted module source trees. Restore still runs Go module
download/verification to reconstruct current dependency sources from those
archives. PR snapshots contain Go objects/tools only and never copy the
immutable module bundle. Separate restore actions preserve GitHub cache path
version compatibility for these two layouts.

Each publication has a two-GiB uncompressed budget. Before capture, only Go's
recoverable action/object files can be removed, oldest first, to target 98% of
the budget. Modules, pinned tools and receipts are never edited by pruning.
Missing objects cause ordinary fresh compilation. If the immutable portion
alone exceeds the cap, publication is skipped with a notice rather than
raising storage limits. `cache_bytes` reports actual inventory bytes. This cap
bounds each entry, not an assertion about live account storage: observe cache
inventory and actual restore/upload costs on hosted runs before qualification.

PR snapshots contain compiler objects and immutable tool binaries only. A PR
snapshot hit downloads/verifies current product modules normally. This avoids
copying the roughly 0.57 GB measured extracted module bundle into each recent
PR entry. Trusted seeds retain module archives for general checker and native
consumers. Linux product seeds omit unused macOS golangci/deadcode binaries;
shared-checker compilation has a separate allowlisted profile to avoid evicting
product compilation.

At the maximum, three current main profiles consume up to six GiB, plus one
narrower entry per recent PR source/context rather than eight entries per PR.
The shared repository LRU may evict old entries before every PR remains warm;
cache misses are correct and must remain functional.

## Pinned binaries and fresh checks

`verified_tools.py` installs/reuses golangci-lint v2.14.0 and deadcode v0.50.0.
On every use it checks recorded output bytes, executable type, exact Go build
version, module/version and host architecture/cgo build settings. A wrong or
corrupt binary is rebuilt. Both native and Linux deadcode checks and both lint
passes still execute against current source.

`pinned_git.py` preserves the exact two upstream commit/tag pins and the
existing local-only build flags. It validates the upstream commit/tag on cold
builds, checks archive provenance/checksums and exact executable version on
hits, and extracts only safe relative files/links to an isolated installation.
Every compatibility regression still executes freshly against the selected
Git binary. The runner-current case remains unchanged.

The pinned Levenshtein implementation intentionally rebuilds helpers and
addresses/checksums the resulting bytes on each call. Only its Go compilation
and module state is reused here. The shared profile verifies the exact pinned
shared source and all four module graphs (CLI, lint, actionlint, govulncheck) on
restore; module verification failures retry once cold. Its main role compiles
those exact helpers without invoking checks or scanners. Its `--cache-dir` contains result/analysis
state as well as helpers and is never restored. Changing that shared binary
contract requires a separately reviewed upstream interface, not copying XDG or
verification state into this repository's cache.

## Qualification and rollback

Run `test_go_cache_context.py`, `test_go_cache_steps.py`, `test_ci_cache.py` and
the workflow/tier/shard contracts. Qualify actual hosted Linux and Darwin cold,
hit, incompatible, corrupt and fork cases, with source/flag/compiler mutation
and the existing complete worker receipt reconciliation. Report compilation
saved versus restore/hash/upload time and actual bytes. A local Darwin measurement of full production/catalog-race compilation and
pinned lint/deadcode generated 1,307,981,087 compiler/tool bytes and 569,314,948
module bytes; this informed the two-GiB cap. Local measurements are not Linux
or hosted Darwin evidence.

General consumers can revert the composite restore step to remain cold without
changing checks. Worker restore/save steps and main seed workflow can be disabled
while leaving all source/module verification and current test bodies intact.
Do not bypass failures of the source/coverage verifier to rescue an accelerator.

## Local layout measurements

On a local Darwin/amd64 Go 1.27.2 experiment (not hosted qualification), a
post-experiment compiler working set plus archive-only dependencies occupied
905,763,308 bytes: manifest capture 3.68 seconds and verification 3.45 seconds.
The narrower compiler snapshot occupied 789,130,606 bytes: capture 2.93 seconds
and verification 3.20 seconds. Compression/upload and full hosted native/shared
working sets remain separate measurements. The original fresh product build,
catalog race compilation and pinned-tool setup had produced 1.75 GiB including
extracted module sources. A rejected one-GiB pruning experiment forced costly
recompilation; neither its cap nor its extracted-module deletion is shipped.
Two actual race `-count=1` executions of `TestProviderGate` emitted their own
run/pass records, including the warm repeat; cached outcomes were not used.
