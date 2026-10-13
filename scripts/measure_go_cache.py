#!/usr/bin/env python3
"""Dedicated dispatch adapter. Install alongside current production helpers.
Render immutable approval literals only after root review; never run PR-only main.
"""
import argparse
from dataclasses import asdict
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import time

import go_cache_context as context
import go_cache_steps as steps

# Root replaces these literals in the dedicated adapter before freezing M.
APPROVED = {
    'ref': 'refs/heads/codex/ci-followup-cache-measure-20261013',
    'workflow': 'Representative cache measurement',
    'workflow_ref': 'wangjohn/agent-archive/.github/workflows/extended.yml@refs/heads/codex/ci-followup-cache-measure-20261013',
    'repository': 'wangjohn/agent-archive', 'batch': 'followup-20261013-0349',
    'runtime_digest': '2f0514d298a628b468fb8947eb28fd0bee5b01560e4d5b80743117f356bc5ff7', 'runtime_source': '370b43bd2ba9d2df5910c0307adec852a1a3c469',
}
# M can differ from reviewed S only at these two tracked files.
ADDITIONS = {'.github/workflows/extended.yml', 'scripts/measure_go_cache.py'}


def approval_guard(env, approved=APPROVED):
    if (env.get('GITHUB_EVENT_NAME') != 'workflow_dispatch'
            or env.get('GITHUB_SHA') != env.get('EXPECTED_SHA')
            or not context.SHA.fullmatch(env.get('EXPECTED_SHA', ''))
            or env.get('GITHUB_REF') != approved['ref']
            or env.get('GITHUB_WORKFLOW') != approved['workflow']
            or env.get('GITHUB_WORKFLOW_REF') != approved['workflow_ref']
            or env.get('GITHUB_WORKFLOW_SHA') != env.get('EXPECTED_SHA')
            or env.get('GITHUB_REPOSITORY') != approved['repository']
            or env.get('BATCH') != approved['batch']
            or not re.fullmatch(r'[a-z0-9]+(?:-[a-z0-9]+)*', approved['batch'])
            or env.get('MODE') not in ('off', 'seed', 'warm')):
        raise context.InvalidContext('dispatch differs from frozen approval')


def runtime_digest(source, sha, git=context.scope.git):
    """Frame path/mode/blob identity of every runtime input; excludes only adapter/workflow."""
    h = hashlib.sha256()
    for record in context.scope.records(git(source, 'ls-tree', '-r', '-z', sha)):
        metadata, path = record.split(b'\t', 1)
        if context.scope.repository_path(path) in ADDITIONS:
            continue
        for field in (metadata, path):
            h.update(len(field).to_bytes(8, 'big')); h.update(field)
    return h.hexdigest()


def measurement_plan(worker, actual_context, source, runner_temp, batch):
    if not re.fullmatch(r'[a-z0-9]+(?:-[a-z0-9]+)*', batch):
        raise context.InvalidContext('unsafe measurement batch')
    # Shared production key primitive returns the production CachePlan dataclass.
    return context.owned_plan('aa-ci-measure-v1-'+batch, worker, actual_context, source, runner_temp)


def classification(mode, state):
    required = {'off': 'miss', 'seed': 'miss', 'warm': 'exact'}
    if mode not in required or state != required[mode]:
        raise context.InvalidContext('spent allocation is not an approved mode observation')
    return 'service-disabled' if mode == 'off' else state


def save_allowed(mode, state, successful):
    return successful is True and mode != 'off' and state != 'exact'


def status_gate(needs):
    if set(needs) != {'worker0', 'worker1'} or any(
            not isinstance(n, dict) or n.get('result') != 'success' for n in needs.values()):
        raise context.InvalidContext('both actual workers must succeed')


def stamp(record, label):
    record.setdefault('timings', []).append({'phase': label, 'unix_ns': time.time_ns(),
                                             'monotonic_ns': time.monotonic_ns()})


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('phase', choices=('paths', 'prepare', 'verify', 'start-worker', 'complete', 'report', 'verdict'))
    args = parser.parse_args()
    env = os.environ
    record = {}
    stamp(record, args.phase+'-start')
    state_file = Path(env['RUNNER_TEMP'])/'measurement-state.json'
    if args.phase == 'report':
        # Evidence must survive source/module/worker/measurement failures.
        started = record['timings']
        record = json.loads(state_file.read_text()) if state_file.exists() else {}
        record.setdefault('timings', []).extend(started)
        try:
            approval_guard(env)
            current = context.source_identity(Path(env['GITHUB_WORKSPACE']).resolve(), env['GITHUB_SHA'])
            record['report_source'] = current
        except (context.InvalidContext, KeyError, TypeError, ValueError, OSError, subprocess.SubprocessError):
            record['report_guard'] = 'failed'
        record['service'] = {k: env.get(k, '') for k in
                             ('RESTORE_OUTCOME', 'MATCHED_KEY', 'EXACT_HIT', 'SAVE_OUTCOME', 'WORKER_OUTCOME')}
        record['logical_cache_bytes'] = {p: sum(f.stat().st_size for f in Path(p).rglob('*')
                                               if f.is_file() and not f.is_symlink())
                                         for p in (env.get('GOCACHE', '/nonexistent'), env.get('GOMODCACHE', '/nonexistent'))}
        stamp(record, 'report-end')
        (Path(env['RUNNER_TEMP'])/'measurement-evidence.json').write_text(json.dumps(record, sort_keys=True))
        return 0
    try:
        approval_guard(env)
        if args.phase == 'paths':
            # Pure owned-path validation/cleanup before setup-go or ANY Go tool.
            if env.get('SHARD') not in ('0', '1'):
                raise context.InvalidContext('invalid worker')
            paths = context.owned_paths(env['RUNNER_TEMP'])
            if (env.get('GOCACHE'), env.get('GOMODCACHE')) != paths:
                raise context.InvalidContext('worker paths differ')
            context.prepare_owned_paths(paths, clear=True)
            record['pre_setup_paths'] = {'paths': list(paths), 'sha': env['GITHUB_SHA'], 'worker': env['SHARD']}
            stamp(record, 'paths-end')
            state_file.write_text(json.dumps(record, sort_keys=True))
            return 0
        source = Path(env['GITHUB_WORKSPACE']).resolve()
        actual = context.source_identity(source, env['GITHUB_SHA'])
        stamp(record, 'source-identity-end')
        if runtime_digest(source, env['GITHUB_SHA']) != APPROVED['runtime_digest']:
            raise context.InvalidContext('runtime inputs differ from reviewed S')
        stamp(record, 'runtime-proof-end')
        if args.phase == 'verdict':
            status_gate(json.loads(env['NEEDS']))
            # Actual source/body/tier reconciliation remains authoritative.
            subprocess.run(['python3', '-B', 'scripts/run_go_shard.py', '--reconcile',
                            env['EVIDENCE_DIRECTORY']], check=True)
            return 0
        if env.get('SHARD') not in ('0', '1'):
            raise context.InvalidContext('invalid worker')
        worker = int(env['SHARD'])
        prior = json.loads(state_file.read_text())
        if prior.get('pre_setup_paths') != {'paths': list(context.owned_paths(env['RUNNER_TEMP'])),
                                           'sha': env['GITHUB_SHA'], 'worker': env['SHARD']}:
            raise context.InvalidContext('pre-setup owned paths phase missing')
        runtime = context.runtime_context()
        stamp(record, 'runtime-context-end')
        plan = measurement_plan(worker, runtime, actual, env['RUNNER_TEMP'], env['BATCH'])
        if (env.get('GOCACHE'), env.get('GOMODCACHE')) != plan.paths:
            raise context.InvalidContext('cache paths differ')
        identity = {'plan': json.loads(json.dumps(asdict(plan))), 'context': context.digest(runtime),
                    'mode': env['MODE'], 'approval': APPROVED}
        def guard(expected):
            approval_guard(env)
            if context.source_identity(source, env['GITHUB_SHA']) != expected:
                raise context.InvalidContext('source changed')
        if args.phase == 'prepare':
            initial = record['timings']
            record = json.loads(state_file.read_text())
            if record.get('pre_setup_paths') != {'paths': list(plan.paths), 'sha': env['GITHUB_SHA'], 'worker': env['SHARD']}:
                raise context.InvalidContext('pre-setup owned paths phase missing')
            record.setdefault('timings', []).extend(initial)
            record['runtime_binary_hashes'] = {key: runtime.get(key) for key in ('toolchain', 'cc', 'cxx')}
            context.prepare_paths(plan, clear=True)
            record['identity'] = identity
            record['event_payload_sha256'] = hashlib.sha256(Path(env['GITHUB_EVENT_PATH']).read_bytes()).hexdigest()
            record['actual'] = {k: env.get(k, '') for k in ('GITHUB_EVENT_NAME', 'GITHUB_SHA', 'GITHUB_REF',
                                'GITHUB_WORKFLOW', 'GITHUB_WORKFLOW_SHA', 'GITHUB_WORKFLOW_REF',
                                'GITHUB_RUN_ID', 'GITHUB_RUN_ATTEMPT', 'RUNNER_OS', 'RUNNER_ARCH',
                                'ImageOS', 'ImageVersion')}
            record['cpu'] = {'affinity': sorted(os.sched_getaffinity(0)),
                             'model': Path('/proc/cpuinfo').read_text(),
                             'quota': Path('/sys/fs/cgroup/cpu.max').read_text() if Path('/sys/fs/cgroup/cpu.max').exists() else 'unavailable'}
            context.outputs(plan)  # same safe output encoder as production
            steps.emit(context.outputs(plan), env['GITHUB_OUTPUT'])
        else:
            initial = record['timings']
            record = json.loads(state_file.read_text())
            record.setdefault('timings', []).extend(initial)
            if record.get('identity') != identity:
                raise context.InvalidContext('source/context differs from prepare')
            if args.phase == 'verify':
                if env['MODE'] == 'off':
                    if env.get('RESTORE_OUTCOME') != 'skipped' or env.get('MATCHED_KEY') or env.get('EXACT_HIT'):
                        raise context.InvalidContext('off service was not disabled')
                    outcome, matched, exact = 'success', '', ''
                else:
                    outcome, matched, exact = env['RESTORE_OUTCOME'], env.get('MATCHED_KEY', ''), env.get('EXACT_HIT', '')
                # Retain production cleanup/download/verify/fatal-source behavior for all service states.
                def measured_command(argv):
                    label = 'module-'+argv[-1]
                    stamp(record, label+'-start')
                    try:
                        return steps.execute_modules(argv)
                    finally:
                        stamp(record, label+'-end')
                state = steps.verify_restored(plan, outcome, matched, exact, guard, command=measured_command)
                record['restore_state'] = state
                record['observation'] = classification(env['MODE'], state)
                steps.emit({'restore_state': state}, env['GITHUB_OUTPUT'])
            elif args.phase == 'complete':
                classification(env['MODE'], record['restore_state'])
                if record.get('failed_phase'):
                    raise context.InvalidContext('prior phase failed')
                manifest = json.loads((source/'shard-results/manifest.json').read_text())
                steps.worker_success(plan, manifest, guard, worker)
                allowed = save_allowed(env['MODE'], record['restore_state'], True)
                steps.emit({'save_ready': 'true' if allowed else 'false'}, env['GITHUB_OUTPUT'])
        stamp(record, args.phase+'-end')
        state_file.write_text(json.dumps(record, sort_keys=True))
        return 0
    except (context.InvalidContext, KeyError, TypeError, ValueError, OSError, subprocess.SubprocessError):
        # Preserve failed allocation phase even if mode classification/module verification fails.
        if record:
            # Source/runtime guards can fail before the normal state reload.
            # Preserve already-spent preparation evidence on that early path.
            try:
                retained = json.loads(state_file.read_text())
            except (OSError, TypeError, ValueError):
                retained = {}
            if retained and record.get('timings', [])[:1] != retained.get('timings', [])[:1]:
                record = {**retained, **record, 'timings': retained.get('timings', []) + record.get('timings', [])}
            record['failed_phase'] = args.phase
            stamp(record, args.phase+'-failed')
            state_file.write_text(json.dumps(record, sort_keys=True))
        print('Measurement source/context/mode/worker contract failed; retain spent allocation', file=sys.stderr)
        return 1


if __name__ == '__main__':
    sys.exit(main())
