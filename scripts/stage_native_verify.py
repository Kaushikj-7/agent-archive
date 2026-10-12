#!/usr/bin/env python3
"""Stage tracked declared inputs for PR native checks; this is not a sandbox."""
import argparse
import json
import os
from pathlib import Path, PurePosixPath
import shutil
import subprocess

import scope_native_verify as scope


def literal(value):
    path = PurePosixPath(value)
    if not value or path.is_absolute() or '..' in path.parts or str(path) != value or any(c in value for c in '*?[]{}!\n\r\\'):
        raise ValueError(f'input must be a clean literal relative path: {value!r}')
    return value


def below(path, root):
    return root == '.' or path == root or path.startswith(root + '/')


def private(path):
    return any(part == '.git' or part == '.env' or (part.startswith('.env.') and part != '.env.example')
               for part in PurePosixPath(path).parts)


def stage(source, destination, config, plan):
    source, destination = source.resolve(), destination.resolve()
    if destination == source or source in destination.parents or destination.exists():
        raise ValueError('destination must be a new directory outside source')
    targets = [check['target'] for check in plan['checks']]
    for target in targets:
        if target.get('discovery', 'git') != 'git':
            raise ValueError('native PR staging requires tracked git inputs')
        for path in target['inputs'] + target.get('exclude', []):
            literal(path)
    tracked = subprocess.check_output(['git', '-C', str(source), 'ls-files', '-z']).decode().split('\0')
    selected = sorted({path for path in tracked if path and not private(path)
                       and any(any(below(path, item) for item in target['inputs'])
                               and not any(below(path, item) for item in target.get('exclude', []))
                               for target in targets)})
    # Validate all selected paths before copying. Ancestor aliases are rejected
    # too, and tracked deletions remain absent rather than restored from HEAD.
    files = []
    for path in selected:
        literal(path)
        item = source / path
        for component in [item, *item.parents]:
            if component == source:
                break
            if component.is_symlink():
                raise ValueError(f'symlink in declared source: {path}')
        if not item.exists():
            continue
        if not item.is_file():
            raise ValueError(f'declared source is not a regular file: {path}')
        files.append(path)
    destination.mkdir(parents=True)
    for path in files:
        output = destination / path
        output.parent.mkdir(parents=True, exist_ok=True)
        shutil.copy2(source / path, output)
    for check in plan['checks']:
        environment = config['environments'][check['check']['environment']]
        environment['executor'] = 'native'
        environment['env'] = {'GOOS': 'linux', 'GOARCH': 'amd64', 'CGO_ENABLED': '1', 'GOFLAGS': '', 'GOENV': 'off', 'GOMAXPROCS': '4'}
        environment['pass_env'] = ['GOCACHE', 'GOMODCACHE']
    (destination / 'levenshtein.json').write_text(json.dumps(config, indent=2) + '\n')
    return files


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--source', type=Path, required=True)
    parser.add_argument('--destination', type=Path, required=True)
    parser.add_argument('--shared', type=Path, required=True)
    parser.add_argument('--run', default='pre-merge')
    parser.add_argument('--pr-scope', action='store_true', help='prove unchanged PR workflow inputs before selecting a generated Go-only run')
    args = parser.parse_args()
    # The pinned planner validates configuration before the consumer copies it.
    plan = json.loads(subprocess.check_output([str(args.shared.resolve() / 'verify'), args.run,
                                              '--source', str(args.source.resolve()), '--dry-run']))
    config = json.loads((args.source / 'levenshtein.json').read_text())
    run, reason = args.run, 'full run requested'
    if args.pr_scope and args.run == 'pre-merge':
        try:
            event = json.loads(Path(os.environ['GITHUB_EVENT_PATH']).read_text())
        except (KeyError, ValueError, OSError):
            event = {}
        run, reason = scope.decision(args.source, config, event, os.environ.get('GITHUB_SHA', ''))
    stage(args.source, args.destination, config, plan)
    # Preserve the original config/plan validation and staging before adding an
    # isolated generated run. Original definitions and run options stay intact.
    if run == scope.SCOPED_RUN:
        scope.add_run(config)
        (args.destination / 'levenshtein.json').write_text(json.dumps(config, indent=2) + '\n')
    subprocess.run([str(args.shared.resolve() / 'verify'), run,
                    '--source', str(args.destination.resolve()), '--dry-run'], check=True,
                   stdout=subprocess.DEVNULL)
    if args.pr_scope:
        print(scope.summarize(run, reason), end='')
        if os.environ.get('GITHUB_STEP_SUMMARY'):
            with open(os.environ['GITHUB_STEP_SUMMARY'], 'a') as summary:
                summary.write(scope.summarize(run, reason))
        if os.environ.get('GITHUB_OUTPUT'):
            with open(os.environ['GITHUB_OUTPUT'], 'a') as output:
                output.write(f'run={run}\n')


if __name__ == '__main__':
    main()
