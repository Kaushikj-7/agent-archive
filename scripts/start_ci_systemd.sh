#!/usr/bin/env bash
# Only disposable GitHub runners may start the real user manager.
set -euo pipefail
[[ "${GITHUB_ACTIONS:-}" == true && "${RUNNER_ENVIRONMENT:-}" == github-hosted ]] || { echo 'requires a disposable GitHub-hosted runner' >&2; exit 1; }
uid="$(id -u)"
sudo loginctl enable-linger "$USER"
for _ in $(seq 1 60); do
  [ -S "/run/user/$uid/bus" ] && break
  sleep 0.5
done
test -S "/run/user/$uid/bus"
echo "XDG_RUNTIME_DIR=/run/user/$uid" >> "$GITHUB_ENV"
echo "DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/$uid/bus" >> "$GITHUB_ENV"
export XDG_RUNTIME_DIR="/run/user/$uid" DBUS_SESSION_BUS_ADDRESS="unix:path=/run/user/$uid/bus"
systemctl --version | head -n 1
systemctl --user show-environment > /dev/null
loginctl show-user "$uid" --property=Linger
