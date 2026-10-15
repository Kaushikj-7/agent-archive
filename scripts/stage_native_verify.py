#!/usr/bin/env python3
"""Stage tracked declared inputs for native checks; this is not a sandbox."""
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


CHECKS = {'lint': 'go-lint', 'vet': 'go-vet', 'modules': 'go-mod',
          'vulnerabilities': 'go-vuln', 'workflow-lint': 'workflow-lint',
          'workflow-security': 'workflow-security'}


def ci_run(event):
    if event == 'schedule':
        return 'main'
    if event in ('pull_request', 'push', 'workflow_dispatch'):
        return 'pre-merge'
    raise ValueError('unsupported native verification event: '+event)


def guard_ci_policy(config, run):
    """Refuse future policy that the fixed native adapter cannot preserve.

    Version-one native lint skips community rule modules with a warning. A
    broader CI route must fail before execution rather than silently omit them.
    Environment overrides are likewise deliberate only for this known policy.
    """
    if (set(config) != {'version', 'targets', 'environments', 'checks', 'runs'}
            or config.get('version') != 1
            or config.get('environments') != {'go': {'executor': 'dagger'}}):
        raise ValueError('unsupported native CI policy or environment; review adapter equivalence')
    expected_targets = {
        'archive': {'dir': '.', 'workspace': '.', 'inputs': ['go.mod', 'go.sum', 'cmd', 'internal', 'schemas']},
        'workflows': {'dir': '.', 'workspace': '.', 'inputs': ['.github']}}
    expected_checks = {name: {'kind': kind, 'target': 'workflows' if name.startswith('workflow-') else 'archive',
                              'environment': 'go'} for name, kind in CHECKS.items()}
    if config.get('targets') != expected_targets or config.get('checks') != expected_checks:
        raise ValueError('unsupported native CI check/target policy; all six checks must remain equivalent')
    selected = config.get('runs', {}).get(run, {})
    expected_options = {'checks'} if run == 'pre-merge' else {'checks', 'rerun_checks'}
    if (run not in ('pre-merge', 'main') or set(selected) != expected_options
            or not isinstance(selected.get('checks'), list)
            or len(selected['checks']) != len(CHECKS) or set(selected['checks']) != set(CHECKS)
            or (run == 'main' and selected.get('rerun_checks') is not True)):
        raise ValueError('unsupported native CI run policy; preserve six checks and scheduled freshness')


def guard_ci_plan(plan, run):
    checks = plan.get('checks', [])
    if (len(checks) != len(CHECKS) or {item.get('id') for item in checks} != set(CHECKS)
            or any(item.get('rule_modules') for item in checks)
            or (run == 'main' and plan.get('rerun_checks') is not True)):
        raise ValueError('native CI planner omitted checks, added community policy, or lost scheduled freshness')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--source', type=Path, required=True)
    parser.add_argument('--destination', type=Path, required=True)
    parser.add_argument('--shared', type=Path, required=True)
    parser.add_argument('--run', default='pre-merge')
    parser.add_argument('--ci-event', help='guarded complete CI route; only PR events may select PR scope')
    parser.add_argument('--pr-scope', action='store_true', help='prove unchanged PR workflow inputs before selecting a generated Go-only run')
    args = parser.parse_args()
    if args.ci_event:
        if args.pr_scope and args.ci_event != 'pull_request':
            parser.error('PR scoping is forbidden outside pull_request')
        args.run = ci_run(args.ci_event)
        args.pr_scope = args.ci_event == 'pull_request'
    config = json.loads((args.source / 'levenshtein.json').read_text())
    if args.ci_event:
        guard_ci_policy(config, args.run)
    # The pinned planner validates configuration before the consumer copies it.
    plan = json.loads(subprocess.check_output([str(args.shared.resolve() / 'verify'), args.run,
                                              '--source', str(args.source.resolve()), '--dry-run']))
    if args.ci_event:
        guard_ci_plan(plan, args.run)
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
    if args.pr_scope or args.ci_event:
        message = (scope.summarize(run, reason) if args.pr_scope else
                   f'Selected original complete {run} run. {reason}\n')
        print(message, end='')
        if os.environ.get('GITHUB_STEP_SUMMARY'):
            with open(os.environ['GITHUB_STEP_SUMMARY'], 'a') as summary:
                summary.write(message)
        if os.environ.get('GITHUB_OUTPUT'):
            with open(os.environ['GITHUB_OUTPUT'], 'a') as output:
                output.write(f'run={run}\n')


if __name__ == '__main__':
    main()
