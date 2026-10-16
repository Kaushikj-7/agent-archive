# Repository instructions

Follow [CONTRIBUTING.md](CONTRIBUTING.md) for the contribution workflow.

## Start here

- This is a Go CLI that archives coding-agent sessions with privacy filtering.
- Use the exact Go toolchain in `go.mod` and lint version in CONTRIBUTING.md.
- Read [architecture](dev/contributing/architecture.md) before changing package
  boundaries, and [testing](dev/contributing/testing.md) before running checks.
- `cmd/agent-archive/` is the entry point; `internal/cli/` owns commands.
- `internal/agents/` owns native integrations; `internal/archive/` owns shared
  privacy policy and archive construction.
- `internal/capture/`, `collector/`, and `reader/` own capture, publication,
  and reading respectively (all under `internal/`).
- `docs/` holds user guides; `dev/` holds contributor docs and design specs.

## Make focused changes

- Keep each PR to one concern. Preserve unrelated and concurrent work; stage
  only intended files. Do not reset, clean, or overwrite someone else's work.
- Check for an existing implementation and overlapping PRs before adding one.
- Follow nearby patterns and package boundaries. Reuse existing helpers;
  introduce abstractions or dependencies only for a concrete need.
- Format changed Go files with `gofmt`. Document exported identifiers and
  write errors that tell the user what to do next.
- Keep hooks fast, silent on stdout, and free of network access.
- Update user docs, help text, and CHANGELOG.md when user-visible behavior changes.

## Protect the user's machine and data

- Unless explicitly instructed by the user, do not develop or test setup,
  uninstall, sync, or backfill against real home directories, app settings,
  schedulers, Keychains, Cursor databases, or buckets.
- Prefer injected `Env`, temporary directories, in-memory stores, and existing
  fakes. Manual macOS checks need temporary `HOME`, `AGENT_ARCHIVE_HOME`, and
  stubbed `launchctl`; follow the testing guide's complete recipe.
- Real systemd checks require a disposable container with systemd as PID 1;
  a `systemctl` stub alone does not isolate the user's service manager.
- Use synthetic fixtures only. Never commit real transcripts or credentials,
  or expose them in logs, PR descriptions, or diagnostic output.

## Privacy and compatibility

- Changes to uploaded or dropped content require the filter and adapter version
  updates, privacy changelog, and documentation described in CONTRIBUTING.md.
- Derived metadata changes require the applicable parser version bump; schema
  changes update `schemas/`. Follow [versions](dev/maintainers/versions.md).
- Preserve existing compatibility and writer protections; check the version
  rules before changing persisted formats or allocating a new version.
- Regenerate affected goldens using the documented commands and inspect every
  changed line. Do not update snapshots just to silence an unexplained failure.

## Validate and report

- Start with affected tests; bug fixes need a regression test that fails
  without the fix. Name tests for behavior, not review or ticket history.
- Useful commands from the repository root:

  ```sh
  go test -race -short -count=1 ./internal/archive  # example: affected package
  go test -race -short -count=1 -timeout 20m ./...
  go vet ./...
  golangci-lint run --disable=revive
  golangci-lint run --enable-only=revive --new-from-merge-base=origin/main
  git diff --check
  ```

- The testing guide defines the complete checks, platform prerequisites,
  Python/script gates, Levenshtein checks, and when Extended CI is required.
  The commands above are a starting point, not a replacement for those gates.
- For parsing or redaction changes, fuzz affected targets for a couple of
  minutes with `-fuzzminimizetime 2s`; retain failing inputs as regression seeds.
- Distinguish synthetic tests, cross-compilation, and live/platform acceptance.
  State what actually ran, what passed, and what remains unverified.
- PR descriptions explain the problem, resulting behavior, and validation.
  Do not claim checks passed when they were skipped, pending, or not run.

## Merge policy

This repository runs many PRs in parallel. Branches do not need to be up to
date with `main` before merging. Do not require rebasing, merging the latest
`main`, or rerunning CI solely because `main` advanced.

Before merging, required CI checks must pass for the PR's current head.
Obtain appropriate review and resolve actual merge conflicts or identified
integration defects.

Do not enable GitHub's "Require branches to be up to date before merging",
change bypass settings, or change any repository protection settings.

## Worktree resources and disk usage

- Ordinary local Go builds and reviews use the user's configured shared build
  and module caches. Do not create a separate `GOCACHE` or `GOMODCACHE` for every
  worktree, PR, reviewer, or agent.
- Preserve isolation explicitly required by CI, qualification, security, or
  cold-cache measurements. For exceptional local caches, reuse an existing
  self-cleaning runner or a temporary directory with cleanup on success,
  failure, and cancellation. Record why isolation is required.
- Keep small logs, reports, and patches separately from compiler output. Do not
  retain a whole cache or build tree merely to preserve evidence.
- Stop owned background processes before removing their scratch. Track the owner
  and cleanup action for disposable databases, simulators, and containers.
  Remove only task-owned resources after checking that they are no longer in use;
  preserve shared caches, other tasks' resources, and unmerged/untracked work.
- At completion, report any retained large directories, why they remain, and how
  to clean them. Follow [worktree storage and cleanup](dev/contributing/worktree-storage.md) when running many worktrees.
