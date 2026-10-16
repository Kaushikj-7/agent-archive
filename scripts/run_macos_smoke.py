#!/usr/bin/env python3
"""Standalone native smoke entry point used by main push jobs and engineers."""
import argparse
from pathlib import Path
import sys
import tempfile

import run_ci_tier as tier


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--output', type=Path)
    parser.add_argument('--local', action='store_true', help='nonqualifying local diagnostics, including dirty checkouts')
    args = parser.parse_args()
    try:
        if args.output:
            tier.run_suite('pr', 'macos', args.output, native_local=args.local)
        else:
            with tempfile.TemporaryDirectory(prefix='agent-archive-native-smoke-') as directory:
                tier.run_suite('pr', 'macos', Path(directory), native_local=args.local)
    except (tier.Failure, ValueError, KeyError, TypeError, OSError) as error:
        print('native smoke failed: '+str(error), file=sys.stderr)
        return 1
    return 0


if __name__ == '__main__':
    sys.exit(main())
