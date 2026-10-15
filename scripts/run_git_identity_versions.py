#!/usr/bin/env python3
"""Run every Git compatibility version serially with isolated process state."""
import argparse
import json
import os
from pathlib import Path
import shutil
import signal
import subprocess
import sys
import time

import pinned_git

ROOT = Path(__file__).resolve().parent.parent
VERSIONS = ('upstream-2.30.2', 'upstream-2.39.2', 'runner-current')
COMMANDS = (
    ('go', 'test', '-p', '2', '-count=1', '-v', './internal/gitremote', './internal/sourcefacts'),
    ('go', 'test', '-p', '2', '-count=1', '-v', '-run', 'RecoveredImport|DeletedWorktree', './internal/backfill'),
)
VERSION_SECONDS = 20 * 60


def write(path, value):
    path.write_text(json.dumps(value, indent=2, sort_keys=True) + '\n')


def version_lane(name, directory, baseline_git, baseline_path, baseline_version, source):
    """Child process: never publish Git through the runner's GITHUB_PATH."""
    report = {'schema': 1, 'name': name, 'source': source, 'success': False,
              'commands': [], 'pin': list(pinned_git.PINS[name]) if name in pinned_git.PINS else None}
    try:
        env = dict(os.environ, PATH=baseline_path, GOMAXPROCS='2')
        for key, part in (('HOME', 'home'), ('XDG_CONFIG_HOME', 'xdg'),
                          ('TMPDIR', 'tmp'), ('TMP', 'tmp'), ('TEMP', 'tmp')):
            folder = directory/part
            folder.mkdir(exist_ok=True)
            env[key] = str(folder)
        # Preparation and child tests use this version's isolated configuration.
        # This child cannot mutate the supervisor's environment or later lanes.
        os.environ.update(env)
        binary = Path(baseline_git)
        expected = baseline_version
        if name in pinned_git.PINS:
            binary = pinned_git.ensure(name, Path(env['GOCACHE'])/'aa-pinned-git',
                                       directory/'install')/'git'
            expected = pinned_git.PINS[name][2]
        # Keep the original PATH byte-for-byte for runner-current: prepending
        # /usr/bin could otherwise shadow setup-go with a different compiler.
        env['PATH'] = (str(binary.parent) + os.pathsep + baseline_path
                       if name in pinned_git.PINS else baseline_path)
        selected = shutil.which('git', path=env['PATH'])
        if selected is None or Path(selected).resolve() != binary.resolve():
            raise ValueError('version lane selected the wrong Git executable')
        actual = subprocess.check_output([str(binary), '--version'], env=env, text=True, timeout=30).strip()
        report.update(binary=str(binary), expected_version=expected, actual_version=actual,
                      environment={key: env[key] for key in ('PATH', 'HOME', 'XDG_CONFIG_HOME', 'TMPDIR', 'TMP', 'TEMP', 'GOMAXPROCS')})
        if actual != expected:
            raise ValueError('version lane Git version differs from its exact expected version')
        print(f'{name}: {binary}: {actual}', flush=True)
        for command in COMMANDS:
            start = time.monotonic()
            result = subprocess.run(command, cwd=ROOT, env=env, check=False)
            report['commands'].append({'argv': list(command), 'exit_code': result.returncode,
                                       'seconds': time.monotonic()-start})
            if result.returncode:
                raise ValueError('Git identity test command failed')
        report['success'] = True
    except (OSError, ValueError, KeyError, subprocess.SubprocessError, pinned_git.InvalidContext) as error:
        report['error'] = str(error)
    finally:
        write(directory/'receipt.json', report)
    return 0 if report['success'] else 1


def stop(process):
    """Stop the entire version process group, including Go and Git children."""
    try:
        os.killpg(process.pid, signal.SIGTERM)
    except ProcessLookupError:
        return
    deadline = time.monotonic()+5
    while time.monotonic() < deadline:
        try:
            os.killpg(process.pid, 0)
        except ProcessLookupError:
            process.wait()
            return
        # Reap the immediate child as soon as possible; wait() alone does not
        # prove that descendants in its process group have exited.
        process.poll()
        time.sleep(.05)
    try:
        os.killpg(process.pid, signal.SIGKILL)
    except ProcessLookupError:
        pass
    process.wait()


def run_all(output):
    if output.exists():
        raise ValueError('Git compatibility output must be a new directory')
    output.mkdir(parents=True)
    baseline_path = os.environ['PATH']
    selected = shutil.which('git', path=baseline_path)
    if selected is None:
        raise ValueError('runner Git is unavailable')
    baseline_git = str(Path(selected).absolute())
    baseline_version = subprocess.check_output([baseline_git, '--version'], text=True, timeout=30).strip()
    source = subprocess.check_output([baseline_git, 'rev-parse', 'HEAD'], cwd=ROOT, text=True, timeout=30).strip()
    if source != os.environ.get('GITHUB_SHA', source):
        raise ValueError('Git compatibility checkout differs from event source')
    report = {'schema': 1, 'source': source, 'versions': [], 'success': False,
              'baseline_git': baseline_git, 'baseline_version': baseline_version,
              'version_timeout_seconds': VERSION_SECONDS}
    write(output/'summary.json', report)
    for name in VERSIONS:
        directory = output/name
        directory.mkdir()
        command = [sys.executable, '-B', str(Path(__file__).resolve()), '--version', name,
                   '--output', str(directory), '--baseline-git', baseline_git,
                   '--baseline-path', baseline_path, '--baseline-version', baseline_version,
                   '--source', source]
        record = {'name': name, 'success': False}
        start = time.monotonic()
        process = None
        try:
            with (directory/'execution.log').open('w') as log:
                process = subprocess.Popen(command, cwd=ROOT, stdout=log, stderr=subprocess.STDOUT,
                                           start_new_session=True)
                try:
                    record['exit_code'] = process.wait(timeout=VERSION_SECONDS)
                finally:
                    # A completed parent can still have descendants. Clean the
                    # session before a later version reuses compiler resources.
                    stop(process)
            receipt = json.loads((directory/'receipt.json').read_text())
            expected = pinned_git.PINS[name][2] if name in pinned_git.PINS else baseline_version
            record.update(actual_version=receipt.get('actual_version'), binary=receipt.get('binary'))
            if (record['exit_code'] == 0 and receipt.get('schema') == 1 and receipt.get('success') is True
                    and receipt.get('actual_version') == expected and receipt.get('expected_version') == expected
                    and receipt.get('pin') == (list(pinned_git.PINS[name]) if name in pinned_git.PINS else None)
                    and receipt.get('source') == source and receipt.get('name') == name
                    and [item['argv'] for item in receipt.get('commands', [])] == [list(c) for c in COMMANDS]
                    and all(item.get('exit_code') == 0 for item in receipt['commands'])):
                record['success'] = True
            else:
                record['error'] = receipt.get('error', 'incomplete Git identity version receipt')
        except (OSError, ValueError, KeyError, subprocess.SubprocessError) as error:
            record['error'] = str(error)
        finally:
            record['seconds'] = time.monotonic()-start
            report['versions'].append(record)
            write(output/'summary.json', report)
            print(json.dumps(record, sort_keys=True), flush=True)
    report['success'] = len(report['versions']) == len(VERSIONS) and all(item['success'] for item in report['versions'])
    write(output/'summary.json', report)
    if os.environ.get('GITHUB_STEP_SUMMARY'):
        with open(os.environ['GITHUB_STEP_SUMMARY'], 'a') as summary:
            summary.write('### Git identity compatibility\n\n')
            for item in report['versions']:
                summary.write(f"- {item['name']}: {'PASS' if item['success'] else 'FAIL'}; "
                              f"{item.get('actual_version', 'version unavailable')}\n")
    return 0 if report['success'] else 1


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--output', type=Path, required=True)
    parser.add_argument('--version', choices=VERSIONS)
    parser.add_argument('--baseline-git')
    parser.add_argument('--baseline-path')
    parser.add_argument('--baseline-version')
    parser.add_argument('--source')
    args = parser.parse_args()
    def interrupted(signum, frame):
        raise KeyboardInterrupt('Git compatibility canceled')
    for sig in (signal.SIGINT, signal.SIGTERM):
        signal.signal(sig, interrupted)
    try:
        if args.version:
            if any(value is None for value in (args.baseline_git, args.baseline_path, args.baseline_version, args.source)):
                parser.error('version subprocess requires captured baseline and source')
            return version_lane(args.version, args.output, args.baseline_git,
                                args.baseline_path, args.baseline_version, args.source)
        return run_all(args.output.resolve())
    except (KeyboardInterrupt, OSError, ValueError, subprocess.SubprocessError) as error:
        print('Git compatibility failed: '+str(error), file=sys.stderr)
        return 1


if __name__ == '__main__':
    sys.exit(main())
