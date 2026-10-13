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

from go_cache_context import (InvalidContext, cache_plan, digest, execute, outputs,
                              prepare_paths, runtime_context, source_identity)


@dataclass(frozen=True)
class Restore:
    state: str
    key: str = ""


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
    if matched == plan.primary and exact == 'true':
        return Restore('exact', matched)
    if (exact != 'true' and matched != plan.primary
            and re.fullmatch(re.escape(plan.prefix)+r'[0-9a-f]{40}', matched)):
        return Restore('prefix', matched)
    raise InvalidContext('cache match violates exact or worker ownership contract')


def verify_restored(plan, outcome, matched, exact, guard, command=execute):
    result = restore_result(plan, outcome, matched, exact)
    prepare_paths(plan, clear=result.state in ('miss', 'error'))
    guard(plan.source)
    command(['go', 'mod', 'download'])
    command(['go', 'mod', 'verify'])
    guard(plan.source)
    return result.state


def worker_success(plan, manifest, guard, worker):
    guard(plan.source)
    if (not isinstance(manifest, dict) or manifest.get('success') is not True
            or manifest.get('catalog', {}).get('source') != plan.source['tested']
            or manifest.get('tier') != 'pr' or manifest.get('shard') != worker):
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
    parser.add_argument('phase', choices=('prepare', 'verify', 'complete'))
    args = parser.parse_args()
    env = os.environ
    try:
        if env.get('GITHUB_EVENT_NAME') != 'pull_request':
            raise InvalidContext('cache steps require PR event')
        if not re.fullmatch(r'[1-9][0-9]*', env.get('PR_NUMBER', '')) or env.get('SHARD') not in ('0', '1'):
            raise InvalidContext('unsafe PR worker identifiers')
        source = Path(env['GITHUB_WORKSPACE']).resolve()
        runner_temp = Path(env['RUNNER_TEMP'])
        state_path = runner_temp/'pr-go-cache-plan.json'
        current = source_identity(source, env['GITHUB_SHA'])
        context = runtime_context()
        plan = cache_plan(int(env['PR_NUMBER']), int(env['SHARD']), context, current, runner_temp)
        if env.get('GOCACHE') != plan.paths[0] or env.get('GOMODCACHE') != plan.paths[1]:
            raise InvalidContext('worker caches differ from saved paths')
        def guard(expected):
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
                                        env.get('EXACT_HIT', ''), guard)
                emit({'restore_state': state}, env['GITHUB_OUTPUT'])
            else:
                manifest = json.loads((source/'shard-results/manifest.json').read_text())
                worker_success(plan, manifest, guard, int(env['SHARD']))
                emit({'save_ready': 'true'}, env['GITHUB_OUTPUT'])
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
