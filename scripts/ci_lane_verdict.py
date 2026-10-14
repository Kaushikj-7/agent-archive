"""Fail closed on the complete dependency set behind stable required checks."""
import json
import os
import sys

LANES = {
    'script-tests': frozenset(('script-static', 'script-install', 'script-purge')),
    'cross-build': frozenset(('cross-linux-arm64', 'cross-darwin-nocgo', 'cross-linux-release')),
}


def verdict(group, needs):
    expected = LANES[group]
    if not isinstance(needs, dict) or set(needs) != expected:
        raise ValueError(f'{group}: expected exactly {sorted(expected)}')
    for name in sorted(expected):
        if not isinstance(needs[name], dict) or needs[name].get('result') != 'success':
            raise ValueError(f'{group}: {name} did not succeed')


if __name__ == '__main__':
    try:
        verdict(sys.argv[1], json.loads(os.environ['NEEDS']))
    except (IndexError, KeyError, ValueError, TypeError) as error:
        raise SystemExit(str(error))
