#!/usr/bin/env python3
"""PR-only lifecycle for explicit pinned GitHub restore/save actions.

No action/service call lives here. Tests inject Go commands and source guards.
"""
import argparse
from dataclasses import asdict, dataclass
import json
import os
from pathlib import Path
import re
import subprocess
import sys

from go_cache_context import (InvalidContext, cache_plan, digest, outputs, owned_paths,
                              prepare_paths, prepare_owned_paths, pr_ancestry,
                              runtime_context, source_identity, validate_context)


@dataclass(frozen=True)
class Restore:
    state: str
    key: str = ""


def execute_modules(argv):
    # Downloads can exceed a metadata probe's 30 seconds on an ordinary cold
    # runner. The hosted job's existing 30-minute deadline remains authoritative.
    return subprocess.check_output(argv, stderr=subprocess.PIPE, timeout=1800)


def restore_result(plan, outcome, matched, exact):
    """An empty match is cold: discard any partial extraction even on success."""
    if outcome not in ('success', 'failure') or exact not in ('', 'true', 'false'):
        raise InvalidContext('invalid action outcome')
    if outcome == 'failure':
        return Restore('error')
    if not matched:
        if exact == 'true':
            raise InvalidContext('exact hit without matched key')
        return Restore('miss')
    if plan.seed and matched == plan.seed and exact != 'true':
        return Restore('trusted', matched)
    if matched == plan.primary and exact == 'true':
        return Restore('exact', matched)
    if (exact != 'true' and matched != plan.primary
            and re.fullmatch(re.escape(plan.prefix)+r'[0-9a-f]{40}', matched)):
        return Restore('prefix', matched)
    raise InvalidContext('cache match violates exact or worker ownership contract')


def verify_restored(plan, outcome, matched, exact, guard, command=execute_modules, context=None):
    result = restore_result(plan, outcome, matched, exact)
    prepare_paths(plan, clear=result.state in ('miss', 'error'))
    state = result.state
    if context is not None and state not in ('miss', 'error'):
        from ci_cache import validate_manifest
        try:
            validate_manifest(plan, context)
        except (InvalidContext, OSError, ValueError, TypeError):
            prepare_paths(plan, clear=True)
            state = 'corrupt-rebuilt'
    guard(plan.source)
    try:
        command(['go', 'mod', 'download'])
        command(['go', 'mod', 'verify'])
    except subprocess.SubprocessError:
        if context is None or state in ('miss', 'error', 'corrupt-rebuilt'):
            raise
        prepare_paths(plan, clear=True)
        state = 'corrupt-rebuilt'
        command(['go', 'mod', 'download'])
        command(['go', 'mod', 'verify'])
    guard(plan.source)
    return state


def worker_success(plan, manifest, guard, worker, workers=2):
    guard(plan.source)
    if (not isinstance(manifest, dict) or manifest.get('success') is not True
            or manifest.get('catalog', {}).get('source') != plan.source['tested']
            or manifest.get('tier') != 'pr' or manifest.get('shard') != worker
            or manifest.get('catalog', {}).get('workers', 2) != workers):
        raise InvalidContext('current PR worker evidence is incomplete')
    # Production's required verdict still verifies exact coverage/body logs.
    return True


def emit(values, output):
    for key, value in values.items():
        if not re.fullmatch(r'[a-z_]+', key) or not isinstance(value, str) or '\n' in value or '\r' in value:
            raise InvalidContext('unsafe action output')
    with open(output, 'a', encoding='utf-8') as handle:
        for key, value in values.items():
            handle.write(f'{key}={value}\n')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('phase', choices=('paths', 'prepare', 'verify', 'complete'))
    args = parser.parse_args()
    env = os.environ
    try:
        if env.get('GITHUB_EVENT_NAME') != 'pull_request':
            raise InvalidContext('cache steps require PR event')
        workers = int(env.get('WORKERS', '2'))
        if workers not in (2, 4, 8) or not re.fullmatch(r'[1-9][0-9]*', env.get('PR_NUMBER', '')) or env.get('SHARD') not in tuple(str(i) for i in range(workers)):
            raise InvalidContext('unsafe PR worker identifiers')
        source = Path(env['GITHUB_WORKSPACE']).resolve()
        runner_temp = Path(env['RUNNER_TEMP'])
        paths = owned_paths(runner_temp)
        if env.get('GOCACHE') != paths[0] or env.get('GOMODCACHE') != paths[1]:
            raise InvalidContext('worker caches differ from controlled paths')
        if args.phase == 'paths':
            prepare_owned_paths(paths, clear=True)
            return 0
        state_path = runner_temp/'pr-go-cache-plan.json'
        event = json.loads(Path(env['GITHUB_EVENT_PATH']).read_text())
        pr_ancestry(source, env['GITHUB_SHA'], event, int(env['PR_NUMBER']))
        current = source_identity(source, env['GITHUB_SHA'])
        context = validate_context(runtime_context())
        plan = cache_plan(int(env['PR_NUMBER']), int(env['SHARD']), context, current, runner_temp, workers)
        if env.get('GOCACHE') != plan.paths[0] or env.get('GOMODCACHE') != plan.paths[1]:
            raise InvalidContext('worker caches differ from saved paths')
        def guard(expected):
            pr_ancestry(source, env['GITHUB_SHA'], event, int(env['PR_NUMBER']))
            if source_identity(source, env['GITHUB_SHA']) != expected:
                raise InvalidContext('tracked source changed during cache execution')
        if args.phase == 'prepare':
            prepare_paths(plan, clear=True)
            # Uncached per-job metadata, never included in action paths.
            state_path.write_text(json.dumps({'plan': asdict(plan), 'context': digest(context)}))
            emit(outputs(plan), env['GITHUB_OUTPUT'])
        else:
            recorded = json.loads(state_path.read_text())
            # Serialization changes tuple paths into a list; compare canonical JSON.
            if recorded != json.loads(json.dumps({'plan': asdict(plan), 'context': digest(context)})):
                raise InvalidContext('source/environment differs from prepared key')
            if args.phase == 'verify':
                state = verify_restored(plan, env['RESTORE_OUTCOME'], env.get('MATCHED_KEY', ''),
                                        env.get('EXACT_HIT', ''), guard, context=context)
                emit({'restore_state': state}, env['GITHUB_OUTPUT'])
            else:
                manifest = json.loads((source/'shard-results/manifest.json').read_text())
                worker_success(plan, manifest, guard, int(env['SHARD']), workers)
                # Every worker retains its source/evidence guard; only the
                # one authorized publisher pays the checksum/upload cost.
                if (int(env['SHARD']) != 0
                        or event['pull_request'].get('head', {}).get('repo', {}).get('full_name')
                        != env.get('GITHUB_REPOSITORY')):
                    emit({'save_ready': 'false'}, env['GITHUB_OUTPUT'])
                    return 0
                from ci_cache import publish_manifest
                try:
                    size = publish_manifest(plan, context)
                except InvalidContext:
                    emit({'save_ready': 'false'}, env['GITHUB_OUTPUT'])
                    return 0
                emit({'save_ready': 'true', 'cache_bytes': str(size)}, env['GITHUB_OUTPUT'])
        return 0
    except InvalidContext as error:
        print('PR Go cache contract failed: ' + str(error), file=sys.stderr)
        return 1
    except (KeyError, TypeError, ValueError, OSError, subprocess.SubprocessError):
        # Do not expose PR-controlled metadata/subprocess stderr in summaries.
        print('PR Go cache source/context/module contract failed', file=sys.stderr)
        return 1


if __name__ == '__main__':
    sys.exit(main())
