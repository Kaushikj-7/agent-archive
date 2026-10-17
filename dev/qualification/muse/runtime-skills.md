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
ownership/expected-byte planner. It declares user and project shared inventory
roots separately. It never runs `muse skills` or changes native settings,
activation, hooks or MCP entries. Foreign and nonregular files are preserved.
Shared installation must be deduplicated with Codex and Cursor by the existing
installation transaction.

The [official skills guide](https://meta-models.github.io/muse-code-sdk/next/guides/extend/skills/)
documents additional config-local, foreign and plugin sources. Project trust and
activation affect availability; foreign personal roots have configuration and
rollout gates. Those roots require exact injected locations and qualified native
evidence before use. Current declarations intentionally cover only shared roots.
Inventory proves installation; it does not prove eligibility, invocation or a
read. Unknown coverage must remain unavailable, never observed-no-use. Invocation
and read attribution additionally require exact owning conversation and skill
identity/hash qualification.

## Validation scope

Deterministic parser and shared planner conformance tests use synthetic inputs.
They cannot qualify native trust/activation, skill use, child identity, supported
platforms or safe native version execution. These gates remain pending; no capture
or default setup enablement follows from skill presence.
