# Contributing

Thanks for helping. agent-archive is a small tool with a privacy promise, so
contributions are held to a few firm rules; everything else is ordinary Go.

## Before you start

- For anything bigger than a small fix, open an issue first so we can agree
  on the approach.
- Larger changes are planned in a [proposal](dev/proposals/README.md) (the
  implementation path: problem, decisions, PR breakdown, live checks). Lasting
  formats, rules and behavior are described in a [spec](dev/specs/README.md)
  (long-term understanding of the system), which stays current after the
  proposal lands.
- A session or field that should have been captured and wasn't: use the
  **capture gap** issue template.
- Vulnerabilities go through [SECURITY.md](SECURITY.md), never a public issue.
- A bug report should say which build you ran: `agent-archive --version`
  prints the release, or `dev-<commit>` for a source build.
- By contributing you agree to follow the [code of conduct](CODE_OF_CONDUCT.md)
  and to license your work under the [MIT License](LICENSE).

## Set up

- Go **1.27.2** exactly: go.mod's `toolchain` line, which any `go`
  command from Go 1.21 on downloads for you. CI refuses any other version.
- macOS with Xcode's command line tools (`xcode-select --install`): the
  Keychain code uses cgo and Security.framework. Or Linux, which needs only
  Go (the build is `CGO_ENABLED=0` and uses a stub in place of the Keychain).
  Both are supported platforms and CI runs the tests on both; from a Mac,
  `GOOS=linux CGO_ENABLED=0 go vet ./...` and `GOOS=darwin CGO_ENABLED=0 go vet
  ./...` catch most cross-platform breakage. Linux-only behavior (the systemd
  scheduler, the credentials file, Cursor's Linux paths) is tested through
  injected values, so it runs on any host; see [testing](dev/contributing/testing.md).
- [golangci-lint v2.14.0](https://golangci-lint.run/) for lint.

What CI blocks on (tests, vet, golangci-lint, the release and installer
scripts, and the shared [Levenshtein](https://github.com/wangjohn/levenshtein)
checks), and the commands to run the same locally, are on the
[testing](dev/contributing/testing.md) page.

## Never test against your real machine

Don't run setup, uninstall, sync, or backfill against your real home
directory, your apps' real settings, the real LaunchAgent or systemd user
units, your Keychain, Cursor's real database, or a real bucket while
developing. In Go tests, use
the injected `Env` and in-memory store the existing tests use. By hand, use
`AGENT_ARCHIVE_HOME` plus a temporary `HOME` and a stub `launchctl` on
`PATH`: [testing](dev/contributing/testing.md) has the full recipe, and
explains why the stub matters even with a sandboxed `HOME`. On Linux a stub
`systemctl` is not enough: use a disposable container with systemd as PID 1,
never your own login's user manager (the real-systemd tests run in CI and in
such a container, and `scripts/acceptance/linux/host.sh` makes one and runs
the product live in it).

## Privacy-sensitive changes

Anything that changes what leaves the machine is a privacy change:

- Bump `archive.FilterVersion` and the adapter version, add a section to the
  [filter changelog](dev/specs/privacy-filter-changelog.md), and update
  [privacy](docs/security/privacy.md) if what is uploaded or dropped
  changes.
- Regenerate the goldens ([how](dev/contributing/testing.md#fixtures-and-goldens))
  and review every changed line in the PR.
- Add fixtures for new shapes, with synthetic content only. Never commit a
  real transcript, even a "harmless" one.
- For redaction or parsing changes, run the fuzz targets for a couple of
  minutes each with `-fuzzminimizetime 2s`, and keep any failing input as a
  seed.

Changes to derived metadata bump `DefaultParserVersion`; schema changes
update `schemas/`. The rules are in [versions](dev/maintainers/versions.md).

## Pull requests

- Keep each PR to one concern, with a description of what changed, why, and
  the test that proves it. A bug fix includes a test that fails without the
  fix.
- Name tests by the behavior they pin, not the review or ticket that found
  the bug.
- Match the surrounding style: full-sentence comments on exported
  identifiers, errors that say what to do next, no output from hooks.
- Update user-facing docs (README, `docs/`, help text) and
  [CHANGELOG.md](CHANGELOG.md) when behavior changes.
- CI must be green: tests on macOS and Ubuntu, lint, and the Levenshtein
  `verify` job.

## Where things are

The [architecture](dev/contributing/architecture.md) page maps the
packages. Supporting a new coding agent is described in
[adding an adapter](dev/contributing/adding-an-adapter.md). The hidden
`_hook` and `_collect` commands are what app hooks and the background job
(a LaunchAgent or a systemd timer) run;
`scripts/measure-hook.py` measures hook latency, and
`scripts/acceptance/linux/host.sh` runs the product live on Linux in a
disposable container (see [testing](dev/contributing/testing.md#the-linux-live-acceptance-run)). Maintainers: see
[releasing](dev/maintainers/releasing.md).
