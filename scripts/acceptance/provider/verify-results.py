#!/usr/bin/env python3
"""Verify complete Go JSON execution for explicit package::test requirements."""
import argparse
import json
from pathlib import Path


def verify(path, required):
    required = list(required)
    identities = [tuple(name.split('::')) for name in required]
    if (not identities or len(set(required)) != len(required)
            or any(len(parts) != 2 or not all(parts) for parts in identities)):
        raise ValueError('require unique package::test identities')
    expected_packages = {package for package, _ in identities}
    packages = {}
    tests = {}
    with path.open() as stream:
        for line in stream:
            event = json.loads(line)
            if not isinstance(event, dict):
                raise ValueError('event must be an object')
            package, action, test = event.get('Package'), event.get('Action'), event.get('Test')
            if package not in expected_packages:
                raise ValueError('unexpected or missing package')
            if action in ('fail', 'skip'):
                raise ValueError('failed or skipped execution')
            if action not in ('start', 'run', 'pause', 'cont', 'output', 'pass'):
                raise ValueError('unsupported event action')
            if test is not None:
                if not isinstance(test, str) or not test or packages.get(package) != 'running':
                    raise ValueError('test outside an active package')
                key = (package, test)
                if action == 'run':
                    if key in tests:
                        raise ValueError('duplicate test execution')
                    tests[key] = 'running'
                elif action in ('pause', 'cont', 'pass', 'output'):
                    if tests.get(key) != 'running':
                        raise ValueError('test event without active execution')
                    if action == 'pass':
                        tests[key] = 'pass'
                else:
                    raise ValueError('invalid test event')
            elif action == 'start':
                if package in packages:
                    raise ValueError('duplicate package execution')
                packages[package] = 'running'
            elif action == 'pass':
                if packages.get(package) != 'running' or any(
                        owner == package and result != 'pass'
                        for (owner, _), result in tests.items()):
                    raise ValueError('package ended without complete execution')
                packages[package] = 'pass'
            elif action != 'output':
                raise ValueError('invalid package event')
    if (any(packages.get(package) != 'pass' for package in expected_packages)
            or any(tests.get(identity) != 'pass' for identity in identities)):
        raise ValueError('required execution is incomplete')
    return {'executed': required, 'packages': packages, 'result': 'pass'}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('path', type=Path)
    parser.add_argument('required', nargs='+', help='exact package::test identities')
    args = parser.parse_args()
    print(json.dumps(verify(args.path, args.required), sort_keys=True))


if __name__ == '__main__':
    main()
