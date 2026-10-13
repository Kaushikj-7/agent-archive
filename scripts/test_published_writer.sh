#!/bin/bash
# Native acceptance of the actual published writer, with disposable state only.
set -euo pipefail
[[ "$(uname -s)" == Darwin ]] || { echo "requires native macOS" >&2; exit 1; }
case "$(uname -m)" in
  arm64) archive_arch=arm64; archive_sha=09f08430627cc0c867afd41ce7eb2c3e61859b640dfb2994323577847eee06e6 ;;
  x86_64) archive_arch=amd64; archive_sha=c8fff68b623a7e0143503adce2d83c6494d7efa55fa0724eb4f76ae54c66255e ;;
  *) echo "unsupported published architecture" >&2; exit 1 ;;
esac
archive_probe=$(mktemp -d)
trap 'rm -rf "$archive_probe"' EXIT
archive_binary="$archive_probe/agent-archive-darwin-$archive_arch"
curl --fail --location --silent --show-error --max-time 120 \
  "https://github.com/wangjohn/agent-archive/releases/download/v0.1.1/agent-archive-darwin-$archive_arch" \
  --output "$archive_binary"
printf '%s  %s\n' "$archive_sha" "$archive_binary" | shasum -a 256 --check
chmod 700 "$archive_binary"
AGENT_ARCHIVE_OLD_BINARY="$archive_binary" go test -count=1 -json -timeout 2m \
  -run '^TestPublishedWriterRefusesProtectedConfiguration$' ./internal/config
