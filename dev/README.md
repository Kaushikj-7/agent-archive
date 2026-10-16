# agent-archive development

Specifications, contributor guides, and maintainer runbooks. For using
agent-archive, see the [user documentation](../docs/README.md); to contribute,
start with [CONTRIBUTING.md](../CONTRIBUTING.md).

## Contributing

| Doc | For |
| --- | --- |
| [Architecture](contributing/architecture.md) | How the pieces fit, and the package map. |
| [Testing](contributing/testing.md) | Tests, lint, fuzzing, and a sandbox that never touches your real machine. |
| [Adding an adapter](contributing/adding-an-adapter.md) | Supporting another coding agent. |
| [Session admission](contributing/session-admission.md) | How a registration's start and admission times drive each boundary check, and the guard tests. |

## Specifications

The [engineering spec index](specs/README.md) lists every spec, its implementation
status and remaining work. Specs include current contracts and historical designs;
code and user documentation describe current behavior where they differ.

## Proposals

The [proposal index](proposals/README.md) separates remaining implementation,
acceptance/release work and implemented designs. Completed designs and companion
evidence live in `proposals/implemented/`. Historical recovery and listing still
have unfinished scope; cloud capture remains proposed. The index was reconciled
against the default-branch checkout on 2026-10-11.

## Maintainers

| Doc | For |
| --- | --- |
| [Releasing](maintainers/releasing.md) | Tagging, signing, and notarization. |
| [Published-release acceptance](maintainers/open-source-acceptance.md) | Disposable Mac and bucket smoke test with recorded pass, fail, or pending evidence. |
| [Versions](maintainers/versions.md) | Filter, adapter, parser, and schema versions, and when each is bumped. |
| [Next release implementation plan](maintainers/next-release-plan.md) | Historical reviewed plan and current implementation reconciliation; release acceptance remains a gate. |
