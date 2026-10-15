#!/bin/sh
# Installs the latest agent-archive release for this machine (macOS or Linux).
#
#   curl -fsSL https://raw.githubusercontent.com/wangjohn/agent-archive/main/install.sh | sh
#
# Downloads the binary for this machine's operating system and architecture
# from GitHub Releases, checks it against the release's SHA256SUMS (which
# catches a damaged download, not a tampered release, since both come from
# the same place), and installs it as `agent-archive`. On macOS the binary is
# signed and notarized, and the script also checks that it is signed with
# this project's Developer ID (which does catch a binary someone else built).
# It never runs setup and never needs sudo. On a machine that is already set
# up (a config.json in the data directory) it then runs
# `agent-archive setup --refresh`, which asks nothing and changes only the
# hooks, the background job's definition (the LaunchAgent's plist, or the
# systemd unit files), and the skill files, so they name the new binary; if
# that fails, or this runs as root, the install still succeeds.
#
# Linux release assets (linux-amd64, linux-arm64) are static and not signed,
# so the Developer ID check does not apply to them; the script prints the
# `gh attestation verify` command that confirms where one came from. On every
# OS the SHA256SUMS check is mandatory: the install stops before anything is
# installed if there is no SHA-256 tool, no single well-formed entry, or a
# mismatch. Releases up to v0.1.1 have no Linux asset.
#
# Environment:
#   AGENT_ARCHIVE_VERSION      release tag to install, e.g. v0.1.0 (default: latest)
#   AGENT_ARCHIVE_INSTALL_DIR  directory to install into (default: where
#                              agent-archive already is, else /usr/local/bin
#                              if writable, else ~/.local/bin)
#   AGENT_ARCHIVE_HOME         data directory of the installation to refresh
#                              (default: ~/.local/share/agent-archive)
#   AGENT_ARCHIVE_DOWNLOAD_URL release download base, for testing only
#
# The whole script is one function called on the last line, so a download
# cut off partway through runs nothing.
set -eu

# The Apple Developer Team ID that signs macOS release binaries. The release
# workflow refuses to publish unless it equals the signing team (the
# APPLE_TEAM_ID secret), so the two cannot drift.
team_id="568CGRV32C"

main() {
  repo_url="https://github.com/wangjohn/agent-archive"

  os="$(detect_os)" || exit 1

  # Before downloading anything: without a pinned team there is nothing
  # this script could accept on macOS. (Linux binaries have no signature.)
  if [ "$os" = darwin ]; then
    check_team_id
  fi

  arch="$(detect_arch "$os")" || exit 1
  asset="agent-archive-${os}-${arch}"

  # Both are needed before there is anything to download or verify.
  command -v curl >/dev/null 2>&1 || fail "curl is required to download the release."
  hasher="$(find_sha256_tool)" || exit 1

  version="${AGENT_ARCHIVE_VERSION:-}"
  if [ -n "$version" ]; then
    if [ "$version" = latest ]; then
      fail "AGENT_ARCHIVE_VERSION=latest is not a release tag; leave it unset to install the latest release."
    fi
    case "$version" in v*) ;; *) version="v${version}" ;; esac
    # A release tag is one path segment of the download URL: no slashes, no
    # "..", nothing but tag characters.
    case "$version" in
      v | *[!0-9A-Za-z._+-]* | *..*)
        fail "AGENT_ARCHIVE_VERSION is not a release tag: ${AGENT_ARCHIVE_VERSION}"
        ;;
    esac
    default_base="${repo_url}/releases/download/${version}"
  else
    default_base="${repo_url}/releases/latest/download"
  fi
  base="${AGENT_ARCHIVE_DOWNLOAD_URL:-$default_base}"

  install_dir="$(choose_install_dir)"
  # Absolute, so the commands printed below work from any directory, and so
  # a directory whose name starts with "-" is never taken for an option.
  case "$install_dir" in
    /*) ;;
    *) install_dir="$(pwd)/${install_dir#./}" ;;
  esac
  target="${install_dir}/agent-archive"

  tmp="$(mktemp -d)" || fail "cannot create a temporary directory"
  staged=
  trap 'cleanup' EXIT
  trap 'exit 130' INT TERM
  trap 'exit 129' HUP

  say "Downloading ${asset} (${version:-latest})"
  if [ -n "${AGENT_ARCHIVE_DOWNLOAD_URL:-}" ]; then
    say "Downloading from ${base}"
  fi
  download "${base}/${asset}" "${tmp}/${asset}"
  download "${base}/SHA256SUMS" "${tmp}/SHA256SUMS"

  # The checksum step is mandatory on every OS. Each command's status is
  # checked on its own (a pipeline would report only the last command's),
  # and both digests must be well-formed before they are compared, so an
  # empty or garbled digest can never compare equal to another one.
  expected="$(awk -v f="$asset" '$2 == f || $2 == "*" f { print $1 }' "${tmp}/SHA256SUMS")" ||
    fail "could not read SHA256SUMS."
  if [ -z "$expected" ]; then
    fail "SHA256SUMS has no entry for ${asset}."
  fi
  if ! is_sha256 "$expected"; then
    fail "SHA256SUMS has a malformed or duplicate entry for ${asset}."
  fi
  [ -s "${tmp}/${asset}" ] || fail "downloaded ${asset} is empty."
  actual="$(sha256_of "${tmp}/${asset}")" || exit 1
  if [ "$expected" != "$actual" ]; then
    fail "checksum mismatch for ${asset}: expected ${expected}, got ${actual}."
  fi
  say "Checksum verified"
  if [ "$os" = darwin ]; then
    verify_signature "${tmp}/${asset}"
    say "Signature verified (Developer ID, team ${team_id})"
  else
    say "Skipping the Developer ID signature check, which is macOS only."
  fi

  [ ! -d "$target" ] || fail "${target} is a directory; not installing over it."
  mkdir -p "$install_dir"
  # Stage beside the target, then rename over it: replacing the file (a new
  # inode) rather than overwriting it in place keeps macOS from killing the
  # upgraded binary at launch.
  staged="$(mktemp "${install_dir}/.agent-archive.install.XXXXXXXX")" ||
    fail "cannot create a temporary file in ${install_dir}"
  cp "${tmp}/${asset}" "$staged"
  chmod 755 "$staged"
  mv -f "$staged" "$target"
  staged=

  installed_version="$("$target" --version 2>&1)" ||
    fail "installed ${target}, but it failed to run: ${installed_version}"
  say "✓ installed agent-archive ${installed_version} to ${target}"
  if [ "$os" != darwin ]; then
    # This advice holds only for releases whose workflow attests the Linux
    # binaries (Linux PR 1, #174); it says nothing about older releases.
    say ""
    say "The checksum only guards against a damaged download, and this binary is not signed."
    say "To confirm it came from the project's release workflow, run (needs the gh CLI):"
    say "  gh attestation verify $(shell_quote "$target") --repo wangjohn/agent-archive"
  fi

  # An upgrade of a set-up machine: the app hooks, the background collector, and
  # the skills name this binary, so bring them up to date. A fresh install
  # runs nothing.
  configured=0
  if existing_installation; then
    configured=1
    refresh_installation
  fi

  case ":${PATH}:" in
    *":${install_dir}:"*) ;;
    *)
      say ""
      # shellcheck disable=SC2088 # the tilde is printed for the user, not expanded
      case "${SHELL:-}" in
        */bash)
          # A macOS terminal starts login shells, which read
          # ~/.bash_profile; on Linux new terminals read ~/.bashrc.
          if [ "$os" = darwin ]; then
            profile="~/.bash_profile"
          else
            profile="~/.bashrc"
          fi
          ;;
        */zsh) profile="~/.zshrc" ;;
        *) profile="your shell profile" ;;
      esac
      say "${install_dir} is not on your PATH. Add this line to ${profile}:"
      say "  export PATH=\"${install_dir}:\$PATH\""
      say "Open a new terminal after updating your profile."
      ;;
  esac
  if [ "$configured" = 0 ]; then
    say ""
    say "To see it work right away, with no bucket needed, run this inside a"
    say "project that already has Claude Code or Codex sessions:"
    say ""
    say "  $(shell_quote "$target") handoff"
    say ""
    say "To get started, run:"
    say ""
    say "agent-archive setup"
  fi
  say ""
  say "If you want to pull in older sessions, you can import them with:"
  say "  $(shell_quote "$target") backfill"
}

# existing_installation succeeds when this machine already has a completed
# setup: a settings file in the data directory, AGENT_ARCHIVE_HOME or the
# default one.
existing_installation() {
  [ -f "${AGENT_ARCHIVE_HOME:-${HOME}/.local/share/agent-archive}/config.json" ]
}

# refresh_installation runs the new binary's `setup --refresh`, which asks
# nothing, needs no terminal, and changes only the hook files, the collector's
# plist, and the skill files, and prints what it did. It never runs setup
# itself, and a failed refresh never fails the install: the binary is
# installed either way, so it says why, and both ways on: refresh again when
# the reason was temporary, setup when there is nothing installed to refresh.
refresh_installation() {
  say ""
  # Under sudo, HOME can still be the person's, and the refresh would leave
  # root-owned files in their app settings and LaunchAgents, which their apps
  # then cannot read or change. The installer never needs sudo.
  if [ "$(id -u)" = 0 ]; then
    say "Not bringing your existing setup up to date: this is running as root, and the refresh would leave"
    say "root-owned files in your home directory. As yourself, without sudo, run: ${target} setup --refresh"
    return 0
  fi
  if output="$("$target" setup --refresh </dev/null 2>&1)"; then
    say "Bringing your existing setup up to date (agent-archive setup --refresh):"
    printf '%s\n' "$output" | sed 's/^/  /'
  else
    printf 'agent-archive install: could not refresh the hooks and skills of your existing setup: %s\n' "$output" >&2
    printf 'The new agent-archive is installed, but your hooks and skills were not brought up to date.\n' >&2
    printf 'If the reason above is temporary, try again with: %s setup --refresh\n' "$target" >&2
    printf 'If it says to run setup (you uninstalled, or setup never finished), run: %s setup\n' "$target" >&2
  fi
}

cleanup() {
  rm -rf "$tmp"
  if [ -n "${staged:-}" ]; then
    rm -f "$staged"
  fi
}

# shell_quote prints $1 as one single-quoted shell word.
shell_quote() {
  quote_rest="$1"
  quote_out=
  while :; do
    case "$quote_rest" in
      *"'"*)
        quote_out="${quote_out}${quote_rest%%"'"*}'\\''"
        quote_rest="${quote_rest#*"'"}"
        ;;
      *) break ;;
    esac
  done
  printf "'%s%s'" "$quote_out" "$quote_rest"
}

detect_os() {
  case "$(uname -s)" in
    Darwin) echo darwin ;;
    Linux) echo linux ;;
    *) fail "this script recognises macOS and Linux release assets only (this system reports $(uname -s))." ;;
  esac
}

detect_arch() {
  if [ "$1" = linux ]; then
    case "$(uname -m)" in
      x86_64 | amd64) echo amd64 ;;
      aarch64 | arm64) echo arm64 ;;
      *) fail "unsupported architecture: $(uname -m) (Linux releases are built for x86_64 and aarch64)" ;;
    esac
    return
  fi
  case "$(uname -m)" in
    arm64 | aarch64) echo arm64 ;;
    x86_64)
      # A shell running under Rosetta reports x86_64 on Apple Silicon; the
      # native build is the right one there.
      if [ "$(sysctl -n hw.optional.arm64 2>/dev/null || true)" = "1" ]; then
        echo arm64
      else
        echo amd64
      fi
      ;;
    *) fail "unsupported architecture: $(uname -m)" ;;
  esac
}

# find_sha256_tool names the SHA-256 tool to use: sha256sum (Linux, and
# anywhere with coreutils) when present, else shasum (macOS). With neither,
# the install stops: it never continues unverified.
find_sha256_tool() {
  if command -v sha256sum >/dev/null 2>&1; then
    echo sha256sum
  elif command -v shasum >/dev/null 2>&1; then
    echo shasum
  else
    fail "no SHA-256 tool found (need sha256sum or shasum), so the download cannot be verified; not installing it."
  fi
}

# sha256_of prints the SHA-256 of the file $1 as 64 lowercase hex digits, or
# fails. The tool's own exit status decides success, so a tool that fails
# cannot leave an empty digest behind.
sha256_of() {
  case "$hasher" in
    sha256sum) sum_out="$(sha256sum "$1")" || fail "sha256sum failed on ${asset}; not installing it." ;;
    shasum) sum_out="$(shasum -a 256 "$1")" || fail "shasum failed on ${asset}; not installing it." ;;
    *) fail "no SHA-256 tool selected; not installing it." ;;
  esac
  digest="${sum_out%% *}"
  if ! is_sha256 "$digest"; then
    fail "${hasher} printed no valid SHA-256 for ${asset}; not installing it."
  fi
  echo "$digest"
}

# is_sha256 succeeds only for exactly 64 lowercase hex digits. (The digit
# set is spelled out: a range like a-f can match capitals in some locales.)
is_sha256() {
  case "$1" in
    *[!0123456789abcdef]*) return 1 ;;
  esac
  [ "${#1}" -eq 64 ]
}

# verify_signature requires a valid, strict code signature from a Developer
# ID Application certificate that Apple issued to this project's team: the
# standard Developer ID designated requirement, with the team pinned.
verify_signature() {
  check_team_id
  requirement="anchor apple generic and certificate 1[field.1.2.840.113635.100.6.2.6] exists and certificate leaf[field.1.2.840.113635.100.6.1.13] exists and certificate leaf[subject.OU] = \"${team_id}\""
  if ! codesign --verify --strict -R="$requirement" "$1" 2>/dev/null; then
    fail "${asset} is not signed by the agent-archive Developer ID (team ${team_id}); not installing it."
  fi
}

check_team_id() {
  if [ -z "$team_id" ]; then
    fail "this install script names no signing team yet, so there is no signed release to install. Build from source instead: https://github.com/wangjohn/agent-archive/blob/main/docs/getting-started/install.md"
  fi
  case "$team_id" in
    *[!A-Z0-9]*) fail "invalid signing team ID in this script: ${team_id}" ;;
  esac
}

choose_install_dir() {
  if [ -n "${AGENT_ARCHIVE_INSTALL_DIR:-}" ]; then
    echo "$AGENT_ARCHIVE_INSTALL_DIR"
    return
  fi
  # Upgrade in place, so the hooks and background collector setup recorded
  # keep pointing at the new binary.
  existing="$(command -v agent-archive 2>/dev/null || true)"
  if [ -n "$existing" ] && [ -w "$(dirname "$existing")" ]; then
    dirname "$existing"
    return
  fi
  if [ -d /usr/local/bin ] && [ -w /usr/local/bin ]; then
    echo /usr/local/bin
    return
  fi
  [ -n "${HOME:-}" ] || fail "HOME is not set; set AGENT_ARCHIVE_INSTALL_DIR to choose where to install."
  echo "${HOME}/.local/bin"
}

download() {
  curl --fail --silent --show-error --location --proto '=https,file' --retry 3 \
    --output "$2" "$1" || fail "download failed: $1"
}

say() {
  printf '%s\n' "$1"
}

fail() {
  printf 'agent-archive install: %s\n' "$1" >&2
  exit 1
}

main "$@"
