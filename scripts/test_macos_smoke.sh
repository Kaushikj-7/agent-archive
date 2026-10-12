#!/usr/bin/env bash
# Fast pre-merge coverage of code that Linux cannot build or exercise natively.
set -euo pipefail

if [[ "$(go env GOOS)" != darwin || "$(go env CGO_ENABLED)" != 1 ]]; then
  echo 'macOS smoke tests require a native Darwin build with cgo enabled' >&2
  exit 1
fi

# Compile every production package with the real Darwin/cgo build tags.
go build ./...

# The Keychain round-trip remains opt-in; the package's other tests fail closed
# instead of reaching a real login Keychain.
go test -race -short -count=1 -timeout 10m \
  ./internal/credentials \
  ./internal/scheduler/launchd \
  ./internal/scheduler/host \
  ./internal/terminal \
  ./internal/termlaunch \
  ./internal/sourcefacts \
  ./internal/discovery

cli_tests=(
  TestScreens
  TestGuidedPromptTerminalCells
  TestGuidedPromptEmissionPreservesTerminalOwnership
  TestGuidedCommandsTerminalOwnership
  TestPairingCodeDisplayRecognizesWrappedTerminal
  TestGuidedPromptCollapseRequiresOwnedVisibleRows
  TestGuidedEOFNeverResolvesDefault
  TestFirstSetupLaunchctlSequence
  TestSetupAndUninstallNeedATerminal
  TestBrowserKeysRestoreTheTerminal
  TestStatsScreenOnARealTerminal
  TestSupportedDiscoveryPublishesWithoutHooksAndAcceptsRecentCopy
)

# -run silently passes when a name disappears. Require every selected test to
# exist before starting the focused CLI run.
listed="$(go test -race -short -list '^Test' ./internal/cli)"
for test_name in "${cli_tests[@]}"; do
  if ! grep -Fxq "$test_name" <<< "$listed"; then
    echo "macOS smoke test missing: $test_name" >&2
    exit 1
  fi
done

pattern="^($(IFS='|'; echo "${cli_tests[*]}"))$"
go test -race -short -count=1 -timeout 10m -run "$pattern" ./internal/cli
