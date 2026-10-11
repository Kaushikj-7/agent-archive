# Install agent-archive

`agent-archive` is a single, self-contained binary with no runtime
dependencies. It runs on macOS (Intel and Apple Silicon) and on Linux (x86-64
and arm64).

The latest published release, v0.1.1, provides macOS binaries for Intel and
Apple Silicon. Linux support and newer CLI features described in these docs
are available by [building the current checkout](#build-from-source); they
have not yet been published in a release. With a current source build, try
`agent-archive handoff` inside a project to continue an existing Claude Code
or Codex conversation before creating a bucket or running setup. See [native local selection and limits](../guides/handoff.md#before-setup-native-local-sessions).

For persistent capture and cross-machine history, continue with [setup](setup.md).
If you want to pull in older sessions, you can import them with
`agent-archive backfill` after setup ([backfill guide](../guides/backfill.md)).
Add `--dry-run` to preview without importing.

## Platforms

**macOS** is the platform the project was built on. Releases are signed with
a Developer ID and notarized, the background collector is a LaunchAgent, and
an R2 key is kept in the login Keychain.

**Linux** is supported for persistent capture: hooks, the scheduled
collector, credentials, and Cursor paths. What it needs:

- **systemd 240 or newer, with a user manager you can reach.** The background
  collector is a systemd user timer (`agent-archive-collector.timer`, every
  60 seconds), because that is the only background scheduler agent-archive
  has on Linux. The logs use `StandardOutput=append:`, which came in with
  systemd 240, so Ubuntu 18.04 (237) and CentOS 7 (219) are out. RHEL 8 and
  its rebuilds (Rocky, Alma, Oracle Linux, CentOS Stream 8) ship 239 and
  work from 8.3 (package `239-32`), where Red Hat backported the option;
  Debian 10 and later and Ubuntu 20.04 and later are fine. Older systemd is
  refused with a message that says to upgrade.
- **A systemd user session bus**, which a desktop or console login has and
  `ssh` has when `pam_systemd` is in use. On a headless machine, a container
  or an SSH session without one, run `loginctl enable-linger` once so the
  user manager runs without a login. Without a user manager `setup` stops
  before it changes anything and says so. See [setup on
  Linux](setup.md#setup-on-linux) and
  [troubleshooting](../guides/troubleshooting.md#linux-and-systemd).
- **No Keychain, so an R2 key is kept in a file** (mode 0600 in a folder of
  mode 0700 under the data directory), which is not encrypted. An S3 profile
  stores no secret of agent-archive's own and is the better choice on Linux
  ([where credentials are kept](../security/privacy.md#where-credentials-are-kept)).

What was tested on Linux, so you can judge how far to trust it:

- A live acceptance run of the real `setup`, hook, timer-driven collector,
  `status`, `setup --refresh` and `uninstall` on Ubuntu 24.04 with systemd
  255 on arm64 and amd64 ([`scripts/acceptance/linux`](../../scripts/acceptance/linux/README.md)),
  over synthetic sessions and a throwaway S3-compatible bucket (MinIO): 91
  checks, all passed. What it covers and what it does not is in
  [testing](../../dev/contributing/testing.md#the-linux-live-acceptance-run).
- The systemd adapter against a real user manager in CI on an x86-64 Ubuntu
  runner, and against recorded output of systemd 239 (Rocky 8), 245 (Ubuntu
  20.04), 252 (Debian 12) and 255 (Ubuntu 24.04).

What is **not** verified on Linux:

- **Cursor.** The real Cursor app and `cursor-agent` have not been run
  against agent-archive on Linux. Where Cursor keeps its chat database
  there (`~/.config/Cursor`, or `$XDG_CONFIG_HOME/Cursor`) is the VS Code
  convention, not something seen on a real install, and a report on Cursor's
  forum says `cursor-agent` hooks on Linux may fail silently. Treat Cursor
  capture on Linux as best effort: `status` shows whether a session was
  captured, and setup cannot tell which Cursor version is installed
  ("version not detected").
- **Claude Code and Codex on Linux.** Neither real app has been run against
  agent-archive on Linux. The live run fed the installed hook command
  hand-written Claude Code payloads; both apps use the same hook files, paths
  and transcript formats as on macOS, so they are expected to work, but that
  is an expectation, not a result.
- **Live product runs on distributions other than Ubuntu 24.04 or systemd
  versions other than 255.** Other versions above have fixture coverage only.
- **Real R2 and Amazon S3 from Linux.** The live run uploaded to a MinIO
  bucket; the storage code is the same as on macOS.
- **A real logout with lingering off** (the no-user-bus case is simulated),
  **a desktop login** (the live run was a headless container), **WSL**, and **a home directory shared across several machines**, which on Linux setup
  [refuses on a network filesystem](../guides/multiple-machines.md#a-home-directory-shared-across-machines)
  unless you allow it for a home only one machine mounts.

Windows is not supported.

## Install with the script

The pinned release below is for macOS. On Linux, [build from source](#build-from-source)
until a release with Linux assets is published.

```sh
curl -fsSL https://raw.githubusercontent.com/wangjohn/agent-archive/v0.1.1/install.sh | AGENT_ARCHIVE_VERSION=v0.1.1 sh
```

The pinned v0.1.1 script installs macOS binaries, verifies the checksum and
Developer ID signature, and never runs setup or refresh. Read its
[published script](https://github.com/wangjohn/agent-archive/blob/v0.1.1/install.sh)
before running it.

The current-checkout [`install.sh`](../../install.sh) is short; read it first
if you prefer. Its behavior below applies when installing a release containing
the corresponding features and assets. It downloads the release binary for
your operating system and architecture
(`agent-archive-darwin-arm64`, `-darwin-amd64`, `-linux-arm64` or
`-linux-amd64`), checks it against the release's `SHA256SUMS` (which catches
a damaged download; both files come from the same release), and installs it
as `agent-archive`, without `sudo`. On macOS it also checks that the binary
carries a valid Developer ID signature from the team the script names (which
catches a binary someone else built). The checksum check is mandatory on
every system: the install stops before it installs anything when there is no
SHA-256 tool (`sha256sum` or `shasum`), no single well-formed entry for the
binary, or a mismatch.
If `agent-archive` is already on your `PATH`, it replaces that copy,
so the hooks and background collector keep pointing at it. Otherwise it uses
`/usr/local/bin` when that is writable, and `~/.local/bin` if not, printing
the line to add to your shell profile when the directory isn't on your
`PATH` (`~/.bash_profile` for Bash on macOS, `~/.bashrc` for Bash on Linux, or
`~/.zshrc` for zsh). Open a new terminal after adding it. It never runs
setup. On Linux, setup refuses a program path that systemd cannot take in a
unit file (one with a quote, a backslash, `$`, `*`, `?`, `[` or a control
character, such as a home directory named `/home/o'brien`); install the
binary elsewhere with `AGENT_ARCHIVE_INSTALL_DIR`.

**On Linux** the binary is not signed, so the checksum only guards against a
damaged download. The installer says so and prints the command that confirms
the binary came from this repository's release workflow, which needs the
[GitHub CLI](https://cli.github.com/):

```sh
gh attestation verify "$(command -v agent-archive)" --repo wangjohn/agent-archive
```

The current installer supports Linux asset selection, but no published release
has Linux binaries yet. v0.1.1 and earlier are macOS only. An architecture
other than x86-64 and arm64 is refused.

Run the same pinned command again to reinstall that version; choose a newer
published tag when upgrading. The current-checkout installer adds the following
refresh behavior, which the v0.1.1 installer does not have: on a machine already
set up (a
`config.json` in `~/.local/share/agent-archive`, or in `AGENT_ARCHIVE_HOME`
when that is set), the installer then runs the new binary's
`agent-archive setup --refresh` and prints its one-line result. That brings
the app hooks, the background job's definition (the LaunchAgent's plist, or
the systemd unit files), and the
[skill files](setup.md#what-setup-changes-on-your-machine) up to date for the
new binary, and changes nothing else: it asks nothing, needs no terminal, and
leaves your storage, credentials, projects, and retention alone. If it
cannot (for example, an interrupted setup needs recovery), the installer
says why and how to run it yourself, and the install still succeeds. Run
under `sudo` (which the installer never needs), it skips the refresh, since it
would leave root-owned files in your home directory, and says to run it as
yourself. A fresh install runs nothing.

Upgrades keep your capture settings. If you want to pull in older sessions,
you can import them with `agent-archive backfill`.

To pin both the installer script and the published release, or to choose a
directory on macOS, use this `v0.1.1` example:

```sh
curl -fsSL https://raw.githubusercontent.com/wangjohn/agent-archive/v0.1.1/install.sh | AGENT_ARCHIVE_VERSION=v0.1.1 AGENT_ARCHIVE_INSTALL_DIR="$HOME/bin" sh
```

## Build from source

You need Go 1.27.2 (go.mod pins `toolchain go1.27.2`; any `go` command
from Go 1.21 on downloads it automatically). On macOS you also need Xcode's
command line tools (`xcode-select --install`), which the Keychain code needs
through cgo. On Linux no C toolchain is needed: the binary is built with
`CGO_ENABLED=0`.

```sh
git clone https://github.com/wangjohn/agent-archive.git
cd agent-archive
VERSION=dev ./scripts/build-release.sh
./dist/agent-archive-darwin-$(uname -m | sed 's/x86_64/amd64/') --version   # macOS
```

On Linux, `VERSION=dev ./scripts/build-release.sh linux` builds
`dist/agent-archive-linux-amd64` and `dist/agent-archive-linux-arm64`, and
`go build ./cmd/agent-archive` builds for the machine you are on.

`scripts/build-release.sh` is the script the release workflow uses, so a
local build has the release's build flags. Plain `go build
./cmd/agent-archive` also works for quick tests. Either way, a build from a
Git checkout reports `dev-<commit>` from `--version` (with `-dirty` when you
had uncommitted changes); put that in bug reports.

To build and install the current checkout in one command, including any
uncommitted changes, run:

```sh
./scripts/install-from-source.sh
```

This installs a persistent development binary at
`~/.local/share/agent-archive-dev/bin/agent-archive` and prints its absolute
path. Use that path to run it; a release binary on `PATH` is left alone. To
replace the `agent-archive` currently on `PATH` at the same path, run
`./scripts/install-from-source.sh --replace-current`. You can also pass
`--destination /absolute/path/to/agent-archive`. The command does not run
setup or change hooks, data, credentials, or the background collector. Run
setup from the development binary only when you intend to point hooks and the
collector at it. Source builds do not test the release download, signature,
or notarization.

Replacing the binary keeps your capture settings. To pull in older sessions,
run `agent-archive backfill` after setup using the installed binary.

On macOS, a source build is signed ad hoc, which macOS treats as a new program after
every rebuild, so it asks again for Keychain access to the R2 key each time,
even after "Always Allow". To keep that approval across rebuilds, set
`AGENT_ARCHIVE_SIGN_IDENTITY` (for example in your shell profile) and
`install-from-source.sh` and `build-release.sh` sign each build with it:
`auto` picks a Developer ID Application certificate from your Keychain,
otherwise the first valid code signing identity; you can also give an
identity's name or SHA-1 hash, or `-` for the ad hoc signature. A build
signed with the project's own Developer ID matches the release's signature,
so a Keychain that already trusts the release asks nothing; any other
identity is asked once. That trust covers every build you sign this way,
including one from a branch you have not reviewed. If signing fails (for
example, a locked Keychain over SSH), the build stays signed ad hoc with a
warning.

Put the binary on your `PATH` as plain `agent-archive`, for example:

```sh
mv ./dist/agent-archive-darwin-arm64 /opt/homebrew/bin/agent-archive   # Apple Silicon with Homebrew
# or: sudo mv ./dist/agent-archive-darwin-amd64 /usr/local/bin/agent-archive
```

A build runs on the machine that built it. If you copy a macOS build to
another Mac (AirDrop, a browser download), macOS may quarantine the unsigned
binary; right-click it in Finder and choose **Open** once, or run
`xattr -d com.apple.quarantine` on it.

To upgrade, move or remove the old binary before putting the new one in
place. Overwriting it in place with `cp` can leave macOS killing it at launch
until the file is recreated. Setup records the binary's path in the hooks and
the background job; if you move it, run `agent-archive setup --refresh` from the
new location (`status` reports hooks and background as `broken` until you
do). It points them at the binary you run it from, and refuses a temporary
build such as `go run`'s.

## Install a release build by hand

1. Download the binary for your machine from the
   [latest release](https://github.com/wangjohn/agent-archive/releases/latest)
   (`agent-archive-darwin-arm64` for Apple Silicon, `agent-archive-darwin-amd64`
   for Intel), and `SHA256SUMS` from the same release. The current published
   release has no Linux assets; use a source build on Linux. When a release
   includes Linux assets, their names are `agent-archive-linux-arm64` and
   `agent-archive-linux-amd64`.
2. Select the binary for this machine and verify its checksum. Run these commands
   in the directory containing the downloads, in the same shell as the steps
   below:

   ```sh
   case "$(uname -s)-$(uname -m)" in
     Darwin-arm64) asset=agent-archive-darwin-arm64 ;;
     Darwin-x86_64) asset=agent-archive-darwin-amd64 ;;
     Linux-aarch64|Linux-arm64) asset=agent-archive-linux-arm64 ;;
     Linux-x86_64) asset=agent-archive-linux-amd64 ;;
     *) echo "Unsupported system or architecture" >&2; exit 1 ;;
   esac
   test -f "$asset" || { echo "Missing $asset" >&2; exit 1; }
   expected=$(awk -v f="$asset" '$2 == f || $2 == "*" f { print $1 }' SHA256SUMS)
   test -n "$expected" || { echo "No checksum for $asset" >&2; exit 1; }
   actual=$(sha256sum "$asset" 2>/dev/null || shasum -a 256 "$asset")
   actual=${actual%% *}
   test "$actual" = "$expected" || { echo "Checksum mismatch for $asset" >&2; exit 1; }
   ```

3. On macOS, check the selected binary's signature (Linux binaries are not
   signed; use the attestation check below instead):

   ```sh
   codesign --verify --strict --verbose=2 "$asset"
   codesign -dv "$asset" 2>&1 | grep TeamIdentifier
   ```

   Compare the reported `TeamIdentifier` with `team_id` in
   [`install.sh`](../../install.sh). On either system, if you have the GitHub
   CLI, also run
   `gh attestation verify "$asset" --repo wangjohn/agent-archive` to check
   that this release asset was built by the repository's release workflow.

4. Make the selected binary executable and install it in a directory you own:

   ```sh
   chmod +x "$asset"
   mkdir -p "$HOME/.local/bin"
   mv "$asset" "$HOME/.local/bin/agent-archive"
   ```

   Add `export PATH="$HOME/.local/bin:$PATH"` to `~/.zshrc` (zsh),
   `~/.bash_profile` (Bash on macOS) or `~/.bashrc` (Bash on Linux) if that directory is not already on your `PATH`,
   then open a new terminal. If you already installed `agent-archive`
   elsewhere, move or remove that older copy so your shell uses this one. If
   its path changed, run `agent-archive setup --refresh` to update the hooks
   and background job.

5. Confirm it runs: `agent-archive --version`.

macOS release binaries are signed with a Developer ID and notarized by
Apple. Gatekeeper may need network access to check the notarization ticket on
first launch. Linux release binaries are static and unsigned.

## Next

- [Set up capture](setup.md): choose apps and projects, connect a bucket,
  start the background collector.
- [Import sessions already on this machine](../guides/backfill.md).
- [Uninstall](uninstall.md).
