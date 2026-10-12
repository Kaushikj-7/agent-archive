# Testing

What CI checks, run locally from the repository root (the
[Levenshtein checks](#levenshtein-checks) below run too):

```sh
go test -race -timeout 20m ./...                         # internal/cli alone takes 4.5 to 7 minutes in CI
go vet ./...
golangci-lint run --disable=revive                       # v2.14.0; the blocking lint run
golangci-lint run --enable-only=revive --new-from-merge-base=origin/main   # doc comments, new code only
go run golang.org/x/tools/cmd/deadcode@v0.50.0 ./...     # only the exceptions listed in test.yml
GOBIN=/tmp/deadcode go install golang.org/x/tools/cmd/deadcode@v0.50.0   # the Linux pass needs a built binary:
GOOS=linux CGO_ENABLED=0 /tmp/deadcode/deadcode ./...    # also excepts credentials.errorForOSStatus
python3 scripts/test_release_signing.py
python3 scripts/test_release_assets.py
python3 scripts/test_release_candidate.py                 # synthetic release API, retry, and promotion fixtures
python3 scripts/test_install.py
python3 scripts/test_install_from_source.py
python3 scripts/test_purge_recipe.py                     # runs the bucket purge recipes in the docs
python3 scripts/test_ci_workflow.py                      # Extended keeps the validated systemd image
python3 scripts/test_measure_hook.py                     # synthetic hook benchmark effect/cleanup checks
./scripts/test_macos_smoke.sh                             # native macOS with cgo; the PR gate
VERSION=dev ./scripts/build-release.sh                   # the release build (CI runs it on a release tag)
```

CI's [Test workflow](../../.github/workflows/test.yml) runs the full race
suite, performance assertions, vet, and script tests on Ubuntu for each pull
request. On macOS it builds all production packages with cgo and runs the
focused Keychain, launchd, terminal, and CLI smoke suite above. macOS
golangci-lint also runs on each pull request: the first pass blocks, and
revive's doc-comment rule checks only code the pull request adds or changes.
All jobs use Go 1.27.2 exactly (go.mod's `toolchain` line). A new push to a
pull request cancels that pull request's older Test and Levenshtein runs.

The Git identity compatibility job separately runs isolated native Git tests on
pinned upstream Git 2.30.2 and 2.39.2, and the runner's current Git. It logs exact
executable versions and verifies the pinned cases. Upstream 2.30.2 tests older
command semantics; it does not establish acceptance of a Debian-patched Git
package. Older distribution package acceptance is recorded separately.

A run-owned `release-candidate/**` branch push runs the same Test jobs and
Levenshtein `verify` against the pushed commit, plus Extended `macos-full`,
`fuzz`, and `real-systemd`. This provides an automatic final integration
campaign when manual dispatch is unavailable. Extended skips its duplicate
Linux suite on these branches because Test runs full Linux race coverage.
Test, Levenshtein and Extended disable setup-go's shared module/build cache
for every trigger, so candidate validation cannot restore executable build
state from another run. Go still verifies downloaded modules against go.sum;
fresh runners may take longer while downloading and compiling dependencies.
Main and nightly behavior remains as described below, and concurrency groups
keep each candidate branch separate from main. Candidate pushes cannot start
the tag-only signing workflow or the manual promotion workflow. Record the
exact pushed SHA and all three run URLs; missing, skipped, queued, timed-out,
or failed required jobs leave readiness pending. Push a fresh candidate ref
at the final main commit after merges if merge commits or squash changed the
previously tested SHA. These campaigns do not establish live provider, app,
architecture, or signed-installer acceptance; see [releasing](../maintainers/releasing.md).

The [Extended workflow](../../.github/workflows/extended.yml) runs the full
Linux and macOS race suites after a merge to `main`; it keeps only the newest
main run during a burst of merges. Nightly it reruns the full macOS suite and
runs the fuzz and real-systemd jobs. Dispatch it manually on a selected ref
to run all jobs or one suite before merging a change to those paths. The
Keychain code needs cgo and Xcode's command line tools on macOS; elsewhere a
stub is built. The `real-systemd` job runs the Linux scheduler against a real
systemd user manager on Ubuntu (see [below](#never-test-against-your-real-machine)).
It is not a required pull-request check. `go test ./...` also checks the docs:
`internal/doclinks` fails on a broken relative link or `#anchor` in any
Markdown file, `TestDocsQuoteOnlyRealCommandsAndFlags` on an
`agent-archive COMMAND --flag` quoted in the README, the docs, or an issue
template that the CLI does not accept, and `TestCLIReferenceIsCurrent` on a
stale [CLI reference](../../docs/reference/cli.md).

Performance tests check what a pass costs on every run (published-state
decodes and local writes, counted, not timed). Their wall-clock targets run
only at full size in a plain build, which CI does in a step of its own:

```sh
AGENT_ARCHIVE_PERF=1 go test -v -p 1 -count=1 -run 'StayFast|FiveMegabyte|TestCursorOverlappingHooksRegisterOnce' ./internal/collector ./internal/archive ./internal/capture
```

A test of a timeout or budget doesn't time the whole call: `-race` on a
loaded runner stretches the file writes around the wait by seconds. It
checks the deadline a stub sees against clock readings taken in the stub
(`TestOriginURLGivesGitAtMostTheTimeout`), or runs the code in a
`testing/synctest` bubble, whose clock moves only while every goroutine in
it is blocked (`TestHookRegistersOnTimeWhenTheRepoKeyLookupHangs`).

## Levenshtein checks

The `verify` job (`levenshtein.yml`) runs the shared checks from
[wangjohn/levenshtein](https://github.com/wangjohn/levenshtein) at the
commit that workflow pins, in a Linux container: Go lint (staticcheck and
more) and vet, HTTP and SQL rules, `go.mod` hygiene, `govulncheck`,
`actionlint`, and `zizmor` for the workflows. `levenshtein.json` picks the
checks. Run the same thing from a sibling checkout of Levenshtein at the
pinned commit (it needs Docker):

```sh
../levenshtein/verify pre-merge --source .
```

The Go lint is the check most changes trip, and it runs natively in
seconds. Build its binary once from the pinned commit, then lint as the
container does, with Linux build tags and the checks list from
Levenshtein's `runner/toolchain.json`:

```sh
pin=$(sed -n 's/^ *ref: \([0-9a-f]\{40\}\)$/\1/p' .github/workflows/levenshtein.yml)
git -C ../levenshtein worktree add /tmp/levenshtein-pin "$pin"
(cd /tmp/levenshtein-pin/runner/lint && go build -o /tmp/levenshtein-lint ./cmd/levenshtein-lint)
GOOS=linux CGO_ENABLED=0 /tmp/levenshtein-lint \
  -checks 'all,-ST1000,-ST1003,-ST1016,-ST1020,-ST1021,-ST1022,-gocognit' ./...
```

It prints nothing when clean. Because it lints Linux build tags, the
darwin-only Keychain and launchd files are covered by the macOS
golangci-lint job instead. Two things it enforces that surprise people:
a `//lint:ignore nilerr` directive never works (restructure the code
instead), and terminal output goes through `internal/terminal` rather than
`_, _ = fmt.Fprintf`.

## Never test against your real machine

Tests and hand-run experiments must not touch your real home directory, your
apps' real hook files, the real LaunchAgent or systemd user units, your
Keychain, Cursor's real database, or a real bucket. The live collector on your
machine is `com.agent-archive.collector` (launchd) or
`agent-archive-collector.timer` (systemd).

In Go tests, everything goes through injection:

- `internal/cli` tests build an `Env` (see `testEnv` in `cli_test.go`) with a
  temporary data directory, temporary user and account homes, a fixed clock,
  no environment variables, and an in-memory bucket. Its scheduler
  (`Env.Scheduler`, a `fakeScheduler` that answers every job "missing" and
  fails the test on a load or stop), Keychain, and executable fields fail the
  test unless the test sets them; `setupTestEnv` gives it a scheduler that
  loads and stops jobs like launchd. To drive the real launchd code, set
  `Env.Scheduler` to nil and replace `runLaunchctl` with `stubLaunchctl`.
- Isolation in `internal/cli` fails closed. Its `TestMain` points `$HOME` at a
  temporary folder, unsets `AGENT_ARCHIVE_HOME`, `CLAUDE_CONFIG_DIR`,
  `CODEX_HOME` and the AWS configuration variables, and replaces the real
  `launchctl` and Keychain with stand-ins that stop the test (see
  `isolation_test.go`). A test that leaves an `Env` field unset can therefore
  never reach your real apps, launchd, or Keychain.
- `internal/capture` (the hook runtime) takes its data directory as an
  argument and does not import anything that runs launchctl, opens the
  Keychain, or uses the network (`TestCaptureImportBoundary`), so it has no
  stand-ins for them. Its tests may not either:
  `TestCaptureTestsCannotReachTheMac` fails a test file that imports
  `os/exec` or the network, or uses `internal/credentials` for anything but
  a config's storage settings. Its `TestMain` gives its tests a
  temporary `HOME`, unsets the same variables, and keeps `$TMPDIR` in a
  folder of the run's own (`internal/testutil/isolation`). Its tests call
  `capture.HandleEvent` directly; tests that go through a command (`_hook`,
  `status`, `sync`, `setup`) stay in `internal/cli`.
- `internal/setupjournal` (setup's journal, rollback and recovery) reaches a
  scheduler only through the `Backends` it is passed, which resolve the backend
  name each journal records, so its tests pass a `launchdSim` (a launchd's
  ownership rules: a label loaded from another plist is never stopped, and
  bootstrap and bootout can fail) or a `schedulertest.Model` (a second
  backend, to see each job driven through the backend that made it) and cannot
  reach launchctl; its `TestMain` isolates the process as `internal/capture`'s
  does. Tests that run `setup` itself stay in `internal/cli`.
- `internal/scheduler/launchd` (the macOS adapter) runs launchctl only through
  the `scheduler.Runner` it is given, so its tests pass a recording Runner and
  cannot reach launchd. It passes `schedulertest.RunConformance`, the suite
  every scheduler adapter must (the state matrix, refusing to stop what another
  installation owns with typed errors, an idempotent unload, loads and unloads
  that a cancelled context does not stop, `Plan`'s purity and
  recorded output, the `Plan` to `Inspect` and refresh round trips, no
  credential in a definition, `Installed` listing the installation's own job
  first and its earlier jobs after), over a fake `launchctl` that prints the
  recordings in `internal/cli/testdata/scheduler/launchctl-print`; the
  `schedulertest.Model`, a scheduler with a vocabulary of its own, passes it
  too, and is what code written against the port can be tested over. `internal/scheduler/host` owns the real Runner; its
  tests run a stand-in `launchctl` script found on a temporary `PATH`.
  `TestOnlyListedPackagesRunPrograms` fails when a package outside a listed
  set imports `os/exec`, and `TestOnlyHostImportsAdapters` when one but `host`
  imports an adapter; depguard says the same in
  `.golangci.yml`.
- `internal/scheduler/systemd` (the Linux adapter) is tested the same way: a
  recording or fake `Runner` and no `systemctl`. It passes `schedulertest.RunConformance` for systemd 239, 245, 252 and 255
  over a fake `systemctl` that answers `show` from the fixtures in
  `internal/scheduler/systemd/testdata/systemctl` (captured from real user
  managers in disposable containers; the README there says how), and the
  state map is pinned over the same fixtures. `internal/cli`'s Linux tests
  drive the real commands over it with `fakeUserManager`, a user manager that
  keeps each job's state, answers `show` and enables a timer only when its unit
  files are where the manager searches.
- The same adapter also runs against a **real** systemd user manager, which no
  fake can vouch for: `TestRealUserManagerConformance` (the conformance suite,
  with states put in and read back by `systemctl` itself) in
  `internal/scheduler/systemd` and `TestRealSystemdSetupRunsTheTimerAndUninstallStopsIt`
  (the real `setup`, the manager's timer starting the job's program, the real
  `uninstall`) in `internal/cli`, with
  `TestRealSystemdUninstallSkippingTheSchedulerPrintsACommandThatStopsTheJob`
  (`uninstall --skip-scheduler` with the manager out of reach, then the command
  it prints, which must stop the timer the manager still runs after the unit
  files are gone). They skip unless
  `AGENT_ARCHIVE_REAL_SYSTEMD=1`, because they change the running user's
  manager (units named `agent-archive-collector*` in `~/.config/systemd/user`,
  and the user's hook and skill files for the smoke); they refuse a machine that
  already has such units. Extended CI runs them nightly or on demand in the
  `real-systemd` job on `ubuntu-24.04` (a virtual machine with systemd: it
  enables lingering for the runner user, sets `XDG_RUNTIME_DIR` and
  `DBUS_SESSION_BUS_ADDRESS`, and
  waits up to two and a half minutes for the timer's first run). That job is
  a nightly or manually requested check, pinned to the image it was
  validated on rather than `ubuntu-latest`, whose move to a new image would
  change the systemd version and defaults. Bump the pin
  deliberately, re-validating on the new image with
  `scripts/acceptance/linux` first. Keep the job's name `real-systemd` for
  consistent backstop results (`scripts/test_ci_workflow.py` fails on a
  rename or an unpinned image). A
  failure in it is a real finding about the adapter. To run it yourself, never
  on your own machine or login, use a disposable Linux container with
  systemd as PID 1 (Docker on macOS runs it in a Linux VM) and a non-root
  user (or run
  `scripts/acceptance/linux/host.sh`, which does all of this and more; see
  [the Linux live acceptance run](#the-linux-live-acceptance-run)):

  ```sh
  docker run -d --name aa-systemd --privileged --cgroupns=host \
    -v /sys/fs/cgroup:/sys/fs/cgroup:rw --tmpfs /run --tmpfs /run/lock \
    IMAGE /sbin/init   # Ubuntu 24.04 with systemd, dbus-user-session, sudo and Go
  # as a user with sudo, in a checkout of the repository inside it:
  sudo loginctl enable-linger "$USER"     # starts the user's manager
  export XDG_RUNTIME_DIR=/run/user/$(id -u) DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/$(id -u)/bus
  AGENT_ARCHIVE_REAL_SYSTEMD=1 go test -count=1 -v -run 'TestReal' ./internal/scheduler/systemd ./internal/cli
  docker rm -f aa-systemd                 # when done
  ```
- `internal/stats` (the statistics engine) is a pure function of the metadata,
  time, time zone and price table it is passed, so its tests build synthetic
  `archive.Metadata` and need no isolation. `TestStatsImportBoundary` and
  `TestStatsReadsNoClockOrEnvironment` keep it that way: it imports only
  `archive`'s types and reads no clock, file or environment. Its default
  prices are `internal/stats/prices.json`, dated and versioned; update the
  file (and its `as_of` and `version`) from the pages in its `sources` when
  list prices change. Read each model's own page as well as the pricing
  table (the two can differ), and re-check any price the notes call
  promotional on its end date: the table does not expire by itself.
- `internal/statshtml` (the `stats --html` page) is a pure function of the
  `stats.Stats` it is passed, so its tests compute stats from synthetic
  metadata and need no isolation. Every page a test renders goes through
  `checkPage`: strict XML parsing, an allowlist of elements, no script, event
  handler, link or other request, unique ids, and a stylesheet that fetches
  nothing. `TestHostileTextIsInertInEveryField` sets every text field of the
  stats, in turn, to names built to break out of markup;
  `TestSpoiledNumbersNeverBreakTheGeometry` does the same with numbers (NaN,
  infinities, extremes); `TestOnlyTheseStatsTextsReachThePage` pins which
  stats texts may reach the page at all (a new one is a privacy decision);
  and `TestPaletteContrast` checks the colors' WCAG contrast in both themes
  from the stylesheet itself. `TestStatsPageAgreesWithTheTerminalAndJSON`
  (in `internal/cli`, which may import both) runs the screen, `--json` and
  `--html` over the same archives and compares every table, card and
  sentence, because the two lay their numbers out separately (they share
  `internal/statsfmt`, whose one table pins every formatter, and whose
  `TestFormattersImportBoundary` keeps it pure). The page goldens are in
  `internal/statshtml/testdata/` (`go test ./internal/statshtml -update`);
  look at a changed page in a browser, light and dark and at phone width,
  before accepting a diff.
- `internal/credentials` fails closed too: its `TestMain` replaces every
  Keychain call `KeychainStore` makes with one that stops the test, so a
  test can reach the real login Keychain only through the opt-in
  `TestKeychainRoundTrip` (`AGENT_ARCHIVE_KEYCHAIN_ROUND_TRIP=1`).
  The file store and the environment store (what `OpenDefault` picks
  off macOS) have no build tag, so their tests run on macOS and Ubuntu alike;
  they write only under `t.TempDir()`, take the platform, the folder, the
  environment and the Keychain constructor as arguments, and never open a
  real Keychain. In `internal/cli`, `credentialOS` is `platform.Darwin` in
  every test (the fake store stands for the Keychain, so its wording is pinned
  on every runner); a test of the other platform's wording calls
  `useCredentialOS` and must not be parallel.
- The operating system is a value (`platform.OS`), read once by
  `platform.Current` and passed everywhere else, so a test answers for macOS,
  Linux or an unknown system on any host: `Env.OS` in `internal/cli`,
  `Environment.OS` in `internal/backfill`, `OpenOptions.OS` in
  `internal/credentials`, `platform.NewLocations` for where each system keeps
  Cursor's data. Tests pin a platform explicitly (`testEnv` models a Mac) and
  never branch on `runtime.GOOS` except to say what the real system must
  answer; `TestOnlyPlatformReadsRuntimeGOOS` fails a production file that reads
  `runtime.GOOS` itself. An unknown system fails closed (no Cursor location, no
  credential store) and each caller's choice is pinned by a test.
- The mount table the Linux network-home check reads (`/proc/self/mountinfo`)
  is `Env.MountTable` in `internal/cli`, and the package's isolation replaces
  its default with one that reads nothing, so no test sees the machine's own
  mounts: a test of a network home passes a table (`mountTableWith`), and an
  unreadable one stops nothing. `local.FilesystemProbe` takes the same table
  and its own symlink resolver.
- `internal/backfill` and `internal/cli` point Cursor database copies at a
  per-run temporary folder (`cursorstore.SnapshotTempDirForTesting`, set in
  their `TestMain`). The real root is a choice of `platform.Locations` over
  values (macOS: the per-user temporary directory; Linux: under
  `$XDG_CACHE_HOME` or the account's `~/.cache`), and `cursorstore`'s tests
  make it in a temporary directory through `preparedSnapshotRoot(root,
  cacheDir)`, so no test touches a real cache directory. The isolation
  helpers unset `XDG_CACHE_HOME` with `XDG_CONFIG_HOME`.
- Storage tests use `storagetest.NewMemoryStore()` (`internal/storage/storagetest`, test code only: depguard keeps it out of production code, as it does `state/statetest`).

## Running the binary by hand in a sandbox

```sh
scratch=$(mktemp -d)
mkdir -p "$scratch/home" "$scratch/stub"
cat > "$scratch/stub/launchctl" <<'EOF'
#!/bin/sh
# Stand-in for launchctl: no job is ever loaded, and nothing reaches launchd.
echo "stub launchctl $*" >&2
if [ "$1" = print ]; then
  echo "Could not find service \"$2\" in domain for port" >&2
  exit 113
fi
exit 0
EOF
chmod +x "$scratch/stub/launchctl"

export AGENT_ARCHIVE_HOME="$scratch/data"   # a data directory of its own
export HOME="$scratch/home"                 # app configs and LaunchAgents live here
export PATH="$scratch/stub:$PATH"           # agent-archive runs `launchctl` from PATH
# Variables that would point setup back at your real configuration:
unset CLAUDE_CONFIG_DIR CODEX_HOME AWS_CONFIG_FILE AWS_SHARED_CREDENTIALS_FILE XDG_CONFIG_HOME
```

- `AGENT_ARCHIVE_HOME` gives the sandbox its own data directory and its own
  launchd label; a sandboxed `HOME` keeps setup away from your real app
  configs. Neither stops a completed setup from loading its job into your
  real launchd, which is what the `launchctl` stub is for. The stub answers
  `print` as launchd does for a job that isn't loaded ("Could not find
  service", exit 113); a stub that just exits 0 there leaves the job's state
  unknown, and setup refuses to continue. Without the stub, cancel setup at
  "Start archiving?".
- `CLAUDE_CONFIG_DIR` and `CODEX_HOME` would send setup's hooks to your real
  app configuration even with a sandboxed `HOME`; the `AWS_*` file variables
  would use your real AWS profiles. Unset them.
- Choose S3 (a local MinIO, below) in a sandbox, not R2: R2 secrets are
  saved in your real login Keychain, which a sandboxed `HOME` doesn't change.
- Setup needs a terminal; `script -q /dev/null agent-archive setup` supplies
  one when you drive it from a script.
- For storage, run a local S3-compatible server (MinIO works) and point an
  AWS profile in the sandbox's `~/.aws/config` at it with
  `endpoint_url = http://127.0.0.1:9000`.
- To exercise the pipeline without setup, write `config.json` by hand (see
  [configuration](../../docs/reference/configuration.md)) and feed `agent-archive
  _hook --harness <app>` a JSON payload on stdin with a copied transcript path, then
  run `agent-archive sync`. Never register a real transcript path.
- `scripts/measure-hook.py BINARY` measures hook latency in its own temporary
  directory; it installs nothing.
- **On Linux a stub `systemctl` is not enough.** `setup` asks `systemctl
  --version` and `systemctl --user show ...` and refuses to go on when it
  cannot get a real answer, and a sandboxed `HOME` does not stop it from
  enabling a timer in your real user manager. Run the binary by hand only in a
  disposable Linux container with systemd as PID 1 (the recipe under the
  real-systemd bullet above, or [the Linux live acceptance
  run](#the-linux-live-acceptance-run), which builds one and drives the whole
  product in it), never in your own login.

The hidden commands `_hook` (what app hooks run) and `_collect` (what the
LaunchAgent runs) are not part of the user interface and may change.

## The Linux live acceptance run

The Linux adapter's unit tests run against fakes and the `real-systemd` CI job
runs two real-manager tests; neither runs the product the way a person does.
`scripts/acceptance/linux/host.sh` does: the real `setup`, hooks, timer, collector,
`status`, `setup --refresh` and `uninstall`, over a real systemd 255 user manager,
with a MinIO standing in for the bucket. Run it before a change to the Linux
scheduler, `setup`'s rollback, `uninstall`, the collector's environment or the
Cursor snapshot root merges, and before any claim that Linux works is published.

```sh
scripts/acceptance/linux/host.sh            # the whole run
scripts/acceptance/linux/host.sh --dry-run  # what it would create; no Docker needed
```

It needs only Docker (colima or Docker Desktop on a Mac; with a Linux Docker the
privileged machine shares your own kernel, so use a VM there) and Go to
cross-build (`BUILD_IN_DOCKER=1` builds in a `golang` container instead). It builds `linux`
binaries of your working tree, starts a privileged Ubuntu 24.04 container with
systemd as PID 1 and a MinIO container on a network of their own, runs
`guest.sh` in the machine as root, prints each check as `PASS` or `FAIL`, and
removes everything it made (named `aa-accept-<random>-*`; it removes nothing
else, such as a MinIO of your own) whether it passes, fails or is interrupted.
Nothing runs on your Mac but Docker's client and the Go build, nothing of your
home, Keychain or buckets is involved, the only credentials are a MinIO user and
password made up for the run, and every transcript is synthetic (the repository's
fixture, and a Cursor database the script writes). It takes about
three minutes (150 to 170 seconds measured) with Docker's caches warm, most of it waiting for systemd timers (the
collector's first run is a minute after boot). It is not in CI: a privileged
container is not something CI should be asked for.
`python3 scripts/test_linux_acceptance.py` is in CI and checks the scripts
without Docker (shellcheck, `--dry-run`, and with a fake `docker` that cleanup
removes only the run's own names, on success, failure and interruption).

**What it covers**, by section of `guest.sh` (the README beside the scripts lists
each check):

- Setup and the timer: `setup --yes` to S3 from a shell with XDG variables;
  the unit files' content and mode, the recorded `PATH` and XDG environment,
  `append:` logging, the enable link, the timer running the collector in the
  manager, `config.json`'s backend and `host_id`.
- Capture: a synthetic Claude Code session through the hook command setup
  installed, published by the **timer's** collector (not `sync`) and read back
  with `list` and `show`; a hand-made Cursor database written the way a running
  Cursor leaves it, imported by `backfill` through a copy under
  `$XDG_CACHE_HOME/agent-archive/cursor-snapshots` (mode, `CACHEDIR.TAG`, the
  copy removed, nothing under `/tmp` or the default cache).
- `status`: the XDG-drift warnings, and the machine-ID clone warning (quiet on
  the original; after `/etc/machine-id` changes it warns in `status`, `--json`
  and `setup`, and `setup` keeps the recorded `host_id`; a machine ID
  bind-mounted over `/etc/machine-id` is not read, so it stays quiet).
- `setup --refresh` after the binary moved, and `uninstall`.
- The enable link on every path that removes a job: `uninstall`;
  `uninstall --skip-scheduler` with a reachable manager and with no user bus
  (the unit files go, no dangling link is left, the manager keeps the timer until
  the printed stop command is run, and that command stops it); and a `setup`
  whose start fails after `systemctl enable` made the link (a `systemctl` wrapper
  in the user's `PATH` enables without starting, then fails): the rollback leaves
  no files, no link and no running timer.
- The real-manager Go tests (`AGENT_ARCHIVE_REAL_SYSTEMD=1`) as a second lingering
  user, and `cursorstore`'s snapshot tests on a real Linux account.
- On the container's own disk, `status` reports no network filesystem and
  `config.json` records no `allow_network_home` opt-in. A real network home
  remains untested live.

**What it does not cover:** the real Cursor application, `cursor-agent` and the
Cursor database layout on Linux (the database is hand-made after macOS's and VS
Code's layout, so a real Cursor's Linux paths and its hook approval are
unverified); the real Claude Code and Codex apps (only hand-written Claude Code
hook payloads are exercised); any distribution other than Ubuntu 24.04 or systemd other than 255 (the fixtures cover 239, 245,
252 and 255 for the adapter's parsing, but only 255 has run live); a manager
without lingering over a real logout (the no-user-bus session is simulated by unsetting the bus variables); real R2 or
AWS (MinIO stands in); WSL; Linux running as the machine's only user session
with a desktop; and anything about upgrades from an earlier release.

**Recorded runs** (record each run that follows a change to what it covers: date,
commit, systemd, architecture, result):

- 2026-09-30, the tree of the pull request that added the run (on top of 5c's
  enable-link and clone-warning change), Ubuntu 24.04.5 LTS, systemd 255
  (255.4-1ubuntu8.17), linux/arm64 (Docker in a colima VM on an Apple silicon
  Mac): **88 passed, 0 failed**, 150 seconds. The run found nothing wrong with
  the product. At that point amd64 had not been run.
- 2026-09-30, the same pull request after review (the Go test binaries must
  report each named test passed, a root-only `cursorstore` test added), same
  machine: **89 passed, 0 failed**, 160 seconds. A deliberately broken build
  (the enable link left out of uninstall's paths, and the clone warning turned
  off) failed 8 checks, in sections 6, 9, 11 and 12.
- 2026-10-01, the network-home guard's pull request after review (section 2
  also checks that `status` sees no network filesystem and `config.json`
  records no `allow_network_home` on the container's own disk), same machine:
  **91 passed, 0 failed**, 103 seconds. A home on a real network filesystem is
  not run live (the container cannot fake an NFS mount's type); the unit
  tests cover it with injected mount tables.
- 2026-10-01, clean commit `61d068b785a6d554bad8400ae8ab69023e355dad`,
  Ubuntu 24.04.5 LTS, systemd 255 (255.4-1ubuntu8.17), linux/amd64 (Docker in
  the x86-64 Colima `levenshtein` VM on macOS): **91 passed, 0 failed**,
  exit 0, 341 seconds including guest image preparation. This establishes
  live harness coverage on amd64 as well as arm64. The first attempt stopped
  before any checks because Quay returned 401 for the default MinIO image;
  Docker Hub images were also unavailable and official binary downloads
  returned 410. The successful retry used `MINIO_IMAGE` and `MC_IMAGE` with
  temporary Alpine 3.24 images built from official source pins:
  `github.com/minio/minio@v0.0.0-20260212201848-7aac2a2c5b7c` and
  `github.com/minio/mc@v0.0.0-20251106162529-77f82e18b540`, cross-built with
  Go 1.27.1, `GOOS=linux GOARCH=amd64 CGO_ENABLED=0`. Product code and checks
  were unchanged; the run's resources and temporary MinIO images were removed.
  Local evidence (not committed):
  `/private/tmp/agent-archive-linux-acceptance-20261001-61d068b-retry.log` and
  `/private/tmp/agent-archive-linux-acceptance-20261001-61d068b-summary.txt`.

When a check fails, read it from the top (later sections build on earlier ones),
and diagnose before changing a check: a failure is a finding about the product
until shown to be the harness's. `KEEP=1` leaves the machine for a look; the
README says how to get a shell as the user whose manager is running.

## Fixtures and goldens

- Adapter fixtures are in `internal/archive/testdata/` as `<app>-<shape>.jsonl`
  with synthetic content only. `filter-golden.json` pins the SHA-256 of what
  each fixture filters to; Cursor database chats
  (`internal/archive/testdata/cursor-composer/`), handoff output
  (`testdata/handoff/`), eval export records (`internal/archive/testdata/eval-export/`, each line validated against its schema), the `stats` screens end to end at 60, 80 and 120
  columns and without a terminal (`internal/cli/testdata/stats/`), every `stats`
  page (overview, detail, projects, models, agents) at 60, 80 and 120 columns
  with and without color from hand-built numbers, plus a previous period,
  Cursor alone, saturated sums, hostile names and ASCII
  (`internal/cli/testdata/stats/pages/`; a colored golden writes each escape
  character as `\e`), backfill plans (`internal/cli/testdata/backfill/`,
  `internal/backfill/testdata/`) and the [CLI reference](../../docs/reference/cli.md)
  have goldens of their own.
- One flag rewrites every golden file:

  ```sh
  go test ./... -update                  # or one package: go test ./internal/archive -update
  git diff                               # review every changed line
  ```

  Review the diff line by line: a change to the archive's goldens is a
  privacy change (see [versions](../maintainers/versions.md)). The flag is
  defined once, in `internal/testutil/golden` (`golden.Check`,
  `golden.Update`), which every package with tests imports, so no package
  rejects it (`TestEveryTestedPackageKnowsUpdate`). A new golden test uses
  `golden.Check`; a package with tests but no goldens imports the package
  blank.
- Screen goldens (`internal/cli/testdata/screens/`, `TestScreens` in
  `internal/cli/screens_test.go`) record what `setup` and `status` print,
  whole, for the screens users meet most: a first run with and without apps,
  the recent-projects list, the resume and reconfigure menus, a failed
  storage check, the review, the next steps, and status when ready and when
  it needs attention. Each is recorded twice: `NAME.txt` as a pipe or
  `NO_COLOR` shows it, and `NAME.color.txt` as a color terminal does, with
  each escape character written `\e`. Setup's answers are echoed after their
  prompts, and the paths (home `/Users/alex`) and the clock (2026-09-25
  12:00 UTC) are fixed. A change to what setup or status prints rewrites
  them, so review the diff as the change's screenshots:

  ```sh
  go test ./internal/cli -run Screens -update
  git diff internal/cli/testdata/screens
  ```

  A new screen is one more entry in `screens`: its answers, its exit code,
  and an `arrange` function that prepares the machine through the fixture.
- A bug fix comes with a test that fails without the fix. Check by reverting
  the fix.

## Where tests live

- A test sits next to the production file it covers: tests of `status.go` go
  in `status_test.go`, or in `status_<aspect>_test.go` when one aspect has
  enough tests to be worth its own file (`status_gaps_test.go`,
  `setup_transaction_recovery_test.go`). Package-wide test helpers
  (`testEnv`, fixtures used by many files) live in `cli_test.go` or the
  package's `testonly_test.go`; `TestMain` lives in `main_test.go`.
- Name files and tests by the behavior they pin
  (`TestUndoKeepsAProjectAnotherImportStillNeeds`), never by the review,
  round, or pull request that found the bug: no `review_fixes_test.go`,
  `second_review_test.go`, or `cli_correctness_test.go`.
- Record where a regression came from in the test's doc comment instead:

  ```go
  // A resumed chat keeps the project it started in.
  //
  // Regression: 2026-09 review H-21.
  func TestResumeKeepsItsProject(t *testing.T) {
  ```

- Moving tests between files is a pure move: keep each test's body
  unchanged, and check that `go test -list '.*' ./...` prints the same names
  before and after.

## Parallel tests

Tests call `t.Parallel()` unless they cannot share the process: a test that
assigns a package variable (`stubLaunchctl`, `collectSoftDeadline`,
`hookDiagnosticsWait`), calls `t.Setenv` or `os.Chdir`, reads a process-wide
counter (`state.PublishedStateLoads`), removes this process's Cursor
snapshots or checks what a sweep of the shared snapshot folder did, orders goroutines with real sleeps, or needs work to finish
within a production time bound that a busy parallel run can exceed (a
hook's lock wait, a version command's output deadline) stays
sequential, with a comment saying why when it is not obvious. Go runs every sequential test
before it releases the parallel ones, so a package variable a sequential
test changes and restores is never seen by a parallel test. Test seams
that vary per test belong in `Env` (`observeFlags`, `backfillCheckpoint`,
`exitOnSignal`), not in package variables. Subtests that each build their
own fixture call `t.Parallel()` too.

Check a change for order dependence and races with
`go test -race -count=3 -shuffle=on ./internal/cli`, and for timing
under load with `go test -race -cpu 1,4,18 -shuffle=on ./internal/cli`.
`internal/cli`'s `TestMain` puts `$TMPDIR` (so every `t.TempDir`) in a
folder of the run's own under `/tmp`: `local.CanonicalPath` lists each parent
of a path, and macOS's per-user temporary folder can hold thousands of
entries. (The product calls it only from setup, status and uninstall, on
paths under your home folder, never from a hook or a collector pass.)

## Live acceptance: guided R2 creation

Guided R2 creation (`internal/cloudflare`, `internal/cli/setup_r2_create.go`)
is tested against a fake Cloudflare (`internal/cloudflare/cloudflaretest`). Its
request and response shapes come from Cloudflare's API reference, read on
2026-09-29, and the fake cannot confirm the points the documentation leaves
open. A partial real-account provider run on 2026-10-05 is recorded below.
The maintainer approved general availability on 2026-10-05 after additional
acceptance testing. Guided creation and machine pairing/key management now need
no experimental environment switches. The checklist below tracks remaining
provider coverage; approval does not turn unrecorded cases into passes.
Use a scratch
Cloudflare account (never one with real archives), and the sandbox recipe
below, so nothing touches your real Mac; create the bootstrap token with
exactly the two permissions setup prints, then run `agent-archive setup` and
choose "Cloudflare R2", then "Continue". Record the result of each item in the
open-source acceptance record.

The [2026-10-05 provider evidence](../maintainers/acceptance/cloudflare/2026-10-05-provider.json)
records a run on macOS arm64 using two new private scratch buckets, three
bucket-scoped keys and synthetic objects. All keys and buckets were deleted.
Lowercase-hex SHA-256 derivation, storage probe/write/read/list/delete, access
outside the archive prefix, denial on the second bucket, canonical immutable
names/policies with no expiry, bounded inventory and independent revocation
passed. Revocation denial was observed after about 4.24 seconds in this one
run; this does not establish an immediate-cutoff guarantee. Activation was
measured after three sequential creations, so exact mint-to-activation latency
is still unknown. An already-deleted token returned HTTP 404, code 1003; this
observation does not justify treating every 404 as confirmed absence.

The sole failed check exposed a classifier mismatch: management REST returned
HTTP 409 with code 10004 for the duplicate bucket. The classifier and fake now
recognize that response, while retaining code 10073 from the Workers/S3 error
reference. The original failed evidence is preserved. The
[provider rerun](../maintainers/acceptance/cloudflare/2026-10-05-provider-rerun.json)
passed every required runner check, including collision classification, and
confirmed cleanup of all three keys and both buckets. Actual guided-setup
collision/retry acceptance is still required.

Build and run the isolated provider runner from a human terminal:

```sh
go build -o /private/tmp/agent-archive-cloudflare-acceptance ./scripts/acceptance/cloudflare
/private/tmp/agent-archive-cloudflare-acceptance --output /private/tmp/aa-cloudflare-acceptance-next.json
```

It prompts for the account ID and a hidden management token, uses no real
archive configuration, Keychain or scheduler, journals only sanitized evidence
with mode 0600, and cleans up only resources created by that run. Interrupted
runs exit unsuccessfully. Evidence-write failures stop acceptance work but do
not stop the remaining cleanup attempts, and are reported as failures. It does
not exercise the CLI pairing transaction. Exact management-token permissions have
not yet been confirmed by the operator, so account/domain reads with precisely
the advertised two permissions remain open. The unchecked items below remain
coverage gaps; a partial provider observation does not close a whole item.

- [ ] **Secret encoding (blocker).** The derived key
      (`cloudflare.DeriveS3Credentials`: Access Key ID = the token's `id`,
      Secret Access Key = lowercase hex SHA-256 of its `value`) passes the
      storage check (probe, write, read, list, delete). Cloudflare's docs say
      only "SHA-256 hash". If the check fails there, try the other encodings
      (base64, raw bytes) in that one function; it is the only place to
      change.
- [ ] **Key activation delay.** How long after the token is created the key is
      accepted. Setup makes up to `r2VerifyAttempts` checks, `r2VerifyPause`
      apart; tune them to what you see.
- [ ] **`GET /accounts` with only the two permissions.** Whether it lists the
      account (else setup asks for the account ID, which is the fallback).
- [ ] **Public-access reads.** Whether `GET .../domains/managed` and
      `GET .../domains/custom` work with only Workers R2 Storage Write
      (else setup says "couldn't check"; adjust its wording if a Read
      permission is needed), and that `result.enabled` and
      `result.domains[]` are the fields read, and that a bucket with no
      custom domains answers with an empty `domains` list and not without
      the field (setup treats a missing `enabled` or `domains`, like any
      success without a result, as unreadable, never as "off" or "none").
      Turn r2.dev on for a scratch
      bucket and confirm the warning and the "What now?" menu (Enter
      revokes the key and returns to the storage question; "Check again"
      sees the dashboard change).
- [ ] **Token expiry.** A token created without `expires_on` has no expiry in
      the dashboard.
- [ ] **Token revoke.** `DELETE /accounts/{account}/tokens/{id}` with the
      bootstrap token revokes the key. Exercise the failure path (for
      example a scratch build with a wrong derivation): the key is revoked,
      and nothing is stored.
- [ ] **Delete-token 404.** What `DELETE /accounts/{account}/tokens/{id}`
      returns for a token that is already gone or was never created (setup
      says "Cloudflare says that token doesn't exist" and tells the person to
      check the dashboard, since the meaning is unconfirmed), and that a
      second delete of the same ID behaves the same.
- [ ] **Prefix scoping unavailable.** The runtime token reaches the whole
      bucket, and only it: it cannot read or list another bucket, create a
      bucket, or set a lifecycle rule. The docs describe bucket-level scope
      only; setup treats prefix scoping as unavailable.
- [ ] **Bucket name collision.** The 2026-10-05 management REST run returned
      HTTP 409, code 10004. `cloudflare.Error.AlreadyExists` now accepts that
      response and the documented Workers/S3 code 10073, each only with 409.
      The provider rerun passed; confirm guided setup retry with a new name.
- [ ] **Jurisdictions.** For each of `eu`, `us`, and `fedramp` (the ones setup
      offers): a bucket created with the jurisdiction, and its token resource
      string `..._<jurisdiction>_<bucket>`, pass the storage check at
      `<account>.<jurisdiction>.r2.cloudflarestorage.com`, and the saved
      endpoint works after setup finishes. `fedramp-high` is documented only
      for the create header, so setup does not offer it; add it only once its
      resource string and endpoint are confirmed.
- [ ] **Non-administrator member.** Token creation by a member who lacks a
      permission is refused with a 403 that the message covers.
- [ ] **Permission group listing.** The lookup by name returns the
      bucket-item-write group with `is_selectable`, the paging parameter is
      accepted (or ignored harmlessly), and the `name` filter matches the
      exact name (setup also compares names itself, so a fuzzy filter is
      harmless, an over-strict one is not).
- [ ] **R2 not enabled.** On an account where R2 is not enabled (or needs a
      payment method), what bucket creation returns. Setup maps a 403 to "needs
      Workers R2 Storage Write" and adds a hint to enable R2; confirm the
      status and message, and give that case its own text if it is not a 403.
- [ ] **Public-access response shape.** `GET .../domains/managed` returns
      `result.enabled` (Cloudflare's reference page for it was unavailable when
      this was written; the shape comes from its summary).
- [ ] **Rate limits.** A 429 carries `Retry-After` in whole seconds, as the
      client reads it.
- [ ] **Orphan token name.** The name printed before creation is the name the
      dashboard shows for the token.
- [ ] **No leftovers.** After a run, search the sandbox (data directory, hook
      files, LaunchAgent plist, `setup-draft.json`) for the bootstrap token;
      it appears nowhere. The automated search
      (`TestGuidedR2NeverPersistsTheBootstrapToken`) covers the fake only.

## Terminal tests

What a real terminal does (echo, key mode, Ctrl-C, Ctrl-Z, a resize, a
hangup) is tested under a pseudo-terminal: a Python script (`python3`, or
the test skips) opens one, starts this test binary as a child in it, types,
and checks the terminal's modes and the output. Run such a script with
`runPTYScript` (`internal/cli/pty_harness_test.go`):

- The script owns its deadlines (60 seconds overall, 30 for each step) and
  says what it was waiting for when one passes; match what it waits for to
  offsets in the output, never to sleeps.
- `runPTYScript` is the backstop, 90 seconds and never later than 30 seconds
  before the test binary's own `-timeout`, so a stuck harness fails its test
  with a message instead of ending the package in a timeout panic. It stops
  the script with SIGTERM, which raises an exception in the script so its
  `finally` stops the child, and kills it only after ten seconds more. A
  script killed outright leaves its child running with nobody to stop it.
- Budget for a slow start. On macOS, `/usr/bin/python3` is Xcode's, and with
  the fresh `HOME` that `TestMain` gives each run it compiles its library
  into `~/Library/Caches` first: a few seconds before the script's first
  line, more on a loaded machine.

## Fuzzing

Redaction, parsing, hook-file editing and the hook itself have fuzz targets
(`go test -list Fuzz ./...`). In `internal/archive`:

| Target | Input | Properties |
| --- | --- | --- |
| `FuzzRedactSensitive` | any string | redacting twice changes nothing; valid UTF-8 stays valid |
| `FuzzRedactCredentialTemplates` | a secret in each credential shape | the secret never survives |
| `FuzzSanitizeValueIdempotent` | any string | the whole string sanitizer (JSON inside strings, instruction blocks, redaction, the cap) is idempotent |
| `FuzzFilterJSONL` | any JSONL, with each adapter (Claude Code, Codex, Cursor) | no panic; only `FilterError`s; retained records are JSON objects that refilter unchanged; the handoff renders with no control character; every token count in the metadata is from 0 to 2^53, and the per-model counts add up to the session's unless one saturated |
| `FuzzFilterJSONLDropsSecrets` | a secret in typed input, credential-named arguments, and JSON strings, per adapter | the secret never survives |
| `FuzzSubagentMeta` | any `.meta.json` bytes beside a subagent transcript | no failure; at most one `subagent-meta` record, first, holding only a bounded string of valid UTF-8; the transcript's own records unchanged; the output refilters unchanged |
| `FuzzSubagentMetaDropsSecrets` | a secret in a description, among filler so the cap can fall in or beside it | the secret never survives |
| `FuzzCursorText` | any Cursor text transcript | refiltering is a no-op; no hidden section is retained; the handoff finds no more prompts than the filter kept |
| `FuzzCursorComposer` | a Cursor database chat and one message row | no panic; retained records are JSON objects; the handoff renders cleanly |
| `FuzzDecodeSource` | any byte stream, gzip or not | no panic; the streaming and whole-bundle readers agree |
| `FuzzLineMatchesCoverWholeString` | any text | running each credential pattern line by line, only on lines with its needles, finds everything matching the whole string finds |
| `FuzzGatedRedactionMatchesWhole` | any text | the redaction as it runs (line by line, gated by needles and separators) equals the same redaction run ungated over the whole string |
| `FuzzNormalizeRemoteURL` | any remote URL or other string | no panic; repository keys have the documented shape, and normalized URLs have no edge slash, control character, or `.git` suffix |
| `FuzzURLUserinfoMatchesReference` | any text up to 4096 bytes | the linear URL userinfo scanner agrees with the reference implementation |

In `internal/hooks`:

| Target | Input | Properties |
| --- | --- | --- |
| `FuzzMergeRemove` | any hook file Merge accepts, per harness | merging twice changes nothing; Remove takes out exactly what Merge added |
| `FuzzCommandDataHome` | any data directory | the hook command reads back the directory it was built with |

In `internal/scheduler/launchd`:

| Target | Input | Properties |
| --- | --- | --- |
| `FuzzLaunchAgentRoundTrip` | an executable, a data directory, a label, and one environment variable | the three plist readers give back what `LaunchAgent` wrote (the program, the environment with `AGENT_ARCHIVE_HOME`, the data directory), up to XML's own rewriting of characters it cannot spell |
| `FuzzLaunchAgentReaders` | any bytes | the readers never panic, and the data directory is what the environment says |

In `internal/scheduler/systemd`:

| Target | Input | Properties |
| --- | --- | --- |
| `FuzzRenderServiceRoundTrip` | an executable, a data directory, and one environment variable | the unit reader gives back exactly what `renderService` wrote (the program, the environment with `AGENT_ARCHIVE_HOME`), whatever `%`, `$`, quotes, backslashes and spaces the values hold, and every line of the unit is a setting the renderer writes, so a value cannot inject one |
| `FuzzReadService` | any bytes | the reader never panics, and a unit it accepts has a program and an environment |

In `internal/cli`:

| Target | Input | Properties |
| --- | --- | --- |
| `FuzzHookPayload` | an app name and up to eight hook payloads, run in turn against a fresh data directory set up for all three apps | every run exits 0 without a panic, and nothing outside the data directory (the project folder, `HOME`) changes |

In `internal/termlaunch`:

| Target | Input | Properties |
| --- | --- | --- |
| `FuzzShellQuote` | any shell argument without NUL | `/bin/sh` reads the quoted argument back unchanged |

Their seeds come from the fixtures in `testdata/` and from
`testdata/fuzz/<target>/`, and plain `go test` runs them on every pull request.
The `fuzz` job in [Extended CI](../../.github/workflows/extended.yml) mutates
all 24 targets for 30 seconds each nightly or on demand. For a change to
redaction or parsing, run the affected targets for a couple of minutes each,
with fast minimization so a
failure is reported promptly:

```sh
go test ./internal/archive -run '^$' -fuzz '^FuzzFilterJSONL$' -fuzztime 2m -fuzzminimizetime 2s
```

A failing input is written to `testdata/fuzz/<target>/`; keep it there as a
seed once it is fixed.


### Published writer refusal

`bash scripts/test_published_writer.sh` runs an opt-in native macOS acceptance
check against the checksum-pinned public v0.1.1 Darwin binary. It uses disposable
HOME and data roots, stripped environment, and scheduler/Keychain stubs. A
legacy scalar config is the successful control; protected config must fail the
old integer decoder and remain byte-identical. Extended macOS runs this check.

## Non-live machine pairing regression coverage

Run combined source, recipient and third-operator fake acceptance together with
scope, delivery, token-source and progress regressions:

```sh
go test -race ./internal/cli -run 'TestMachine(TwoHome|Remote)FakeAcceptance|TestPairingStandalone|TestPairingClipboardUnavailable|TestManagementTokenFailure|TestCommittedPairingHousekeeping|TestRetired|TestDirectDiscovery|TestRevocationProgressUses|TestPairingMissingApp' -count=1
```

These use temporary homes, private synthetic operator bindings, memory object and
credential stores, and a loopback fake Cloudflare provider. Source issuer labels,
rename-safe third-machine IDs, re-pairing history, unused versus delivered spares,
issuer compromise and per-key partial deletion/retry are combined command flows.
The existing issuance/revocation suites additionally exercise lock contention,
interrupted reservations, uncertain delivery and cancelled execution budgets;
bounded discovery/listing suites cover partial reads, shared deadlines, caps and
worker limits. Fake results do not establish live token permissions, account
inventory visibility, absence semantics, propagation or macOS/Linux timings.
First-run pairing and guided creation are generally available by maintainer
approval. Record separate live and platform acceptance results for the final
release candidate; fake tests do not establish that evidence.
