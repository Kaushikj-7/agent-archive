# Muse runtime, version and skills boundaries

The runtime [presence qualification](runtime-presence.md) remains authoritative.
A root shell observation does not establish child, resume or monitor identity.
`MUSE_THREAD_ID` identifies the running chat in the documented contract;
`MUSE_SESSION_ID` refers to its root. Equal root IDs must not become a child
contract, and inventory must not substitute for independently qualified identity.
Production selection remains unknown until that qualification is available.

The pure version parser accepts the observed native output shape
`Muse Code 1.4.9 (1.4.9-R6297.1)`. It separates release `1.4.9` and display build
`1.4.9-R6297.1`; neither value is an executable SHA. Contradictory release/build
versions, additional output, control characters and unknown forms are refused.
An installed executable observation cannot stamp a child conversation's version.
The registered installed version probe uses the injected PATH lookup and fixed
`--version` operation, with `MUSE_NO_AUTO_UPDATE=1`, the existing two-second
deadline and a strict 4096-byte output cap. A fake executable verifies argv,
environment preservation and refusal of overlong/multiline output; no native
Muse process was run for these tests. Unrecognized output stays unknown.

The skill provider uses the existing shared `.agents/skills` destination and
ownership/expected-byte planner. It declares user/project shared and foreign roots separately, with config-local
and Codex roots resolved only from the fixed two-variable environment snapshot.
The caller additionally qualifies the effective data home once through the
existing native-location policy, shared by canonical Muse aliases. The exact
builtin root is `muse/skills/bundled/muse-core/skills` under that base; an absent
or invalid base supplies no builtin root. It never runs `muse skills` or changes native settings,
activation, hooks or MCP entries. Foreign and nonregular files are preserved.
Shared installation must be deduplicated with Codex and Cursor by the existing
installation transaction.

The [official skills guide](https://meta-models.github.io/muse-code-sdk/next/guides/extend/skills/)
documents additional config-local, foreign and plugin sources. Project trust and
activation affect availability; foreign personal roots have configuration and
rollout gates. Config-local and foreign filesystem inventory now uses exact injected locations;
project trust and native activation remain unknown. The builtin filesystem layout was observed in the isolated pinned native
1.4.9-R6297.1 case, and is now declared explicitly. General plugin installed roots
remain uninspected until their installed catalog locator/schema/provenance are
qualified; no scratch package is treated as an installed plugin. The existing
observer supports finite tagged manifest-file declarations, preserving package
confinement, installed-only coverage, physical-read deduplication and one shared
byte/declaration allowance. This is infrastructure for qualified provenance, not
a test-only claim of production plugin coverage. Production observation never
executes native CLI commands.
Inventory proves installation; it does not prove eligibility, invocation or a
read. Unknown coverage must remain unavailable, never observed-no-use. Optional metadata installed declaration identities preserve logical scope/id
provenance when multiple declarations share a name and filtered content hash.
They remain attached to the existing content group and do not create duplicate
legacy rows, eligible coverage or use. Muse parser0.7.0 stamps this derivation;
other agent/default parser versions are unchanged. Invocation
and read attribution additionally require exact owning conversation and skill
identity/hash qualification.

## Validation scope

Deterministic parser and shared planner conformance tests use synthetic inputs.
They cannot qualify native trust/activation, skill use, child identity, supported
platforms or safe native version execution. These gates remain pending; no capture
or default setup enablement follows from skill presence.
