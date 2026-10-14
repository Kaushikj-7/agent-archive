#!/usr/bin/env bash
# Whole-production Darwin/cgo build and complete native smoke bodies.
set -euo pipefail
# Preserve the standalone entry point; the tier runner owns discovery,
# source-bound evidence, per-phase timings and the shared compiled inventory.
exec python3 -B scripts/run_macos_smoke.py "$@"
