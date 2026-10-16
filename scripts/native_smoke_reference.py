#!/usr/bin/env python3
"""Bounded manual native smoke cohorts and actual cache-state observations."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import time

import ci_cache
import go_cache_context as keys

ROOT = Path(__file__).resolve().parent.parent
SUITE = 'native-smoke-reference'
STATES = {'trusted', 'miss', 'corrupt-rebuilt'}


class Failure(RuntimeError):
    pass


def git_source():
    return subprocess.check_output(['git', '-C', str(ROOT), 'rev-parse', 'HEAD'], timeout=20).decode().strip()


def dependency():
    return keys.digest({name: hashlib.sha256((ROOT/name).read_bytes()).hexdigest()
                        for name in ('go.mod', 'go.sum')})


def guard():
    env = os.environ
    source = env.get('EXPECTED_NATIVE_SOURCE', '')
    if (env.get('GITHUB_ACTIONS') != 'true' or env.get('GITHUB_EVENT_NAME') != 'workflow_dispatch'
            or env.get('CI_SUITE') != SUITE or not re.fullmatch('[0-9a-f]{40}', source)
            or source != env.get('GITHUB_SHA') or source != git_source()
            or env.get('NATIVE_SMOKE_MODE') not in ('normal', 'sensitive')
            or env.get('NATIVE_SMOKE_CACHE') not in ('cold', 'trusted-seed')):
        raise Failure('manual native smoke requires exact explicit suite/source/cohort inputs')
    return source


def controlled_paths():
    paths = keys.owned_paths(Path(os.environ['RUNNER_TEMP']))
    if [os.environ.get('GOCACHE'), os.environ.get('GOMODCACHE')] != list(paths):
        raise Failure('native cache paths differ from controlled caller paths')
    return paths


def write(path, value):
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(value, indent=2, sort_keys=True)+'\n')


def assert_empty(output):
    source = guard()
    if os.environ['NATIVE_SMOKE_CACHE'] != 'cold':
        raise Failure('empty-cache proof only belongs to the explicit cold cohort')
    paths = controlled_paths()
    for raw in paths:
        path = Path(raw)
        if path.is_symlink() or not path.is_dir() or any(path.iterdir()):
            raise Failure('cold native cohort requires both owned caches empty before module prelude')
    write(output, {'schema': 1, 'source': source, 'paths': list(paths), 'entries': [0, 0],
                   'boundary': 'before-module-download-and-verify', 'actions_restore': False})


def observe(state, output):
    if state not in STATES:
        raise Failure('missing or unknown verified compilation restore state')
    paths = controlled_paths()
    saved = json.loads((Path(os.environ['RUNNER_TEMP'])/'compilation-cache-plan.json').read_text())
    plan, context = saved['plan'], keys.validate_context(saved['context'], native=True)
    source = plan['source']
    if (context['go']['GOOS'] != 'darwin' or plan['paths'] != list(paths)
            or source['tested'] != os.environ.get('GITHUB_SHA') or source['tested'] != git_source()
            or source['dependency'] != dependency() or not re.fullmatch('[0-9a-f]{40}', source['tree'])):
        raise Failure('native cache observation source/context/path mismatch')
    expected = ci_cache.general_plan(context, source, Path(os.environ['RUNNER_TEMP']))
    if plan['seed'] != expected.seed or plan['primary'] != expected.primary:
        raise Failure('native cache observation seed identity mismatch')
    manifest = ci_cache.manifest_path(expected)
    if manifest.is_symlink() or state == 'trusted' and not manifest.is_file():
        raise Failure('trusted restore lacks its actual regular checksum manifest')
    value = {'schema': 1, 'source': source['tested'], 'tree': source['tree'],
             'dependency': source['dependency'], 'state': state, 'seed': expected.seed,
             'context_sha256': keys.digest(context),
             'manifest_sha256': hashlib.sha256(manifest.read_bytes()).hexdigest() if manifest.is_file() else None,
             'event': os.environ.get('GITHUB_EVENT_NAME'), 'run_id': os.environ.get('GITHUB_RUN_ID'),
             'run_attempt': os.environ.get('GITHUB_RUN_ATTEMPT')}
    write(output, value)
    return value


def verify_observation(value, proof, context):
    if not isinstance(value, dict):
        raise Failure('native cache observation must be an object')
    expected = ci_cache.general_plan(context, {'tested': proof['source'],
        'tree': proof['tree'], 'dependency': dependency()}, Path(os.environ['RUNNER_TEMP'])).seed
    if (value.get('schema') != 1 or value.get('source') != proof['source'] or value.get('tree') != proof['tree']
            or value.get('dependency') != dependency() or value.get('state') not in STATES
            or value.get('context_sha256') != keys.digest(context) or value.get('seed') != expected
            or value.get('state') == 'trusted' and not re.fullmatch('[0-9a-f]{64}', value.get('manifest_sha256') or '')):
        raise Failure('native cache observation does not bind current source/compiler/state')


def run(output):
    import run_ci_tier as tier
    source = guard()
    output.mkdir(parents=True, exist_ok=True)
    report = {'schema': 1, 'success': False, 'kind': 'manual-native-smoke-reference',
              'required_pr_gate': False, 'p95_traffic': False, 'source': source,
              'mode': os.environ['NATIVE_SMOKE_MODE'], 'cache_request': os.environ['NATIVE_SMOKE_CACHE'],
              'workflow_source': os.environ['GITHUB_SHA'], 'event': os.environ['GITHUB_EVENT_NAME']}
    try:
        observation = observe(os.environ.get('NATIVE_CACHE_STATE'), output/'cache-observation.json')
        report['cache_observation'] = observation
        if report['cache_request'] == 'trusted-seed' and observation['state'] != 'trusted':
            raise Failure('trusted-seed qualification unavailable: actual final restore state is '+observation['state'])
        if report['cache_request'] == 'cold':
            empty = json.loads((output/'cold-start.json').read_text())
            if (empty != {'schema': 1, 'source': source, 'paths': list(controlled_paths()), 'entries': [0, 0],
                          'boundary': 'before-module-download-and-verify', 'actions_restore': False}
                    or observation['state'] != 'miss'):
                raise Failure('cold cohort lacks empty build+module cache proof or contains a restore')
            report['cold_start'] = empty
        started = time.monotonic()
        try:
            tier.run_suite('pr', 'macos', output, extras={'native': report['mode'] == 'sensitive'})
        finally:
            report['producer_seconds'] = time.monotonic()-started
        native = json.loads((output/'tier.json').read_text())
        if native['success'] is not True or native['native_smoke']['qualification'] != 'hosted':
            raise Failure('manual qualification lacks complete hosted native bodies')
        verify_observation(observation, native['source_proof'], native['native_smoke']['context'])
        report.update(source_proof=native['source_proof'], context=native['native_smoke']['context'],
                      context_sha256=native['native_smoke']['context_sha256'],
                      phase_seconds=native['native_smoke']['phase_seconds'], success=True)
    except BaseException as error:
        report['error'] = {'type': type(error).__name__, 'message': str(error)}
        raise
    finally:
        write(output/'reference.json', report)



def compare(paths, output):
    """Metadata comparability only; never a body verdict or PR/p95 gate."""
    records = [json.loads(Path(path).read_text()) for path in paths]
    wanted = {(m, c) for m in ('normal', 'sensitive') for c in ('cold', 'trusted-seed')}
    if len(records) != 4 or {(r.get('mode'), r.get('cache_request')) for r in records} != wanted:
        raise Failure('comparison requires exactly the four distinct manual cohorts')
    identity = None
    for record in records:
        context = keys.validate_context(record.get('context'), native=True)
        proof = record.get('source_proof', {})
        observation = record.get('cache_observation', {})
        if (record.get('success') is not True or record.get('kind') != 'manual-native-smoke-reference'
                or record.get('required_pr_gate') is not False or record.get('p95_traffic') is not False
                or set(proof) != {'source', 'tree'}
                or any(not re.fullmatch('[0-9a-f]{40}', proof.get(k) or '') for k in ('source', 'tree'))
                or record.get('error') is not None or record.get('event') != 'workflow_dispatch'
                or record.get('source') != proof.get('source') or record.get('workflow_source') != proof.get('source')
                or context['go']['GOOS'] != 'darwin' or record.get('context_sha256') != keys.digest(context)
                or observation.get('schema') != 1 or observation.get('source') != proof.get('source')
                or observation.get('tree') != proof.get('tree')
                or observation.get('context_sha256') != keys.digest(context)
                or observation.get('state') != ('miss' if record['cache_request'] == 'cold' else 'trusted')
                or observation.get('state') == 'trusted' and not re.fullmatch('[0-9a-f]{64}', observation.get('manifest_sha256') or '')):
            raise Failure('comparison lacks successful actual source/context/cache cohort metadata')
        current = (proof, context)
        if identity is not None and identity != current:
            raise Failure('different source/image/compiler contexts: keep separate unqualified comparisons')
        identity = current
    write(output, {'schema': 1, 'kind': 'manual-native-metadata-comparison',
        'required_pr_gate': False, 'p95_traffic': False, 'same_source_context': True,
        'source_proof': identity[0], 'context_sha256': keys.digest(identity[1]),
        'cohorts': [{'mode': r['mode'], 'cache_request': r['cache_request'],
                     'phase_seconds': r['phase_seconds'],
                     'producer_seconds': r.get('producer_seconds')} for r in records]})

def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('phase', choices=('guard', 'assert-empty', 'observe', 'run', 'compare'))
    parser.add_argument('--output', type=Path, default=Path('native-tier-results'))
    parser.add_argument('--reference', type=Path, action='append', default=[])
    args = parser.parse_args()
    try:
        if args.phase == 'guard':
            guard()
        elif args.phase == 'assert-empty':
            assert_empty(args.output)
        elif args.phase == 'observe':
            observe(os.environ.get('NATIVE_CACHE_STATE'), args.output)
        elif args.phase == 'run':
            run(args.output)
        else:
            compare(args.reference, args.output)
    except (RuntimeError, keys.InvalidContext, ValueError, KeyError, OSError, subprocess.SubprocessError) as error:
        print('native smoke observation/qualification failed: '+str(error), file=sys.stderr)
        return 1
    return 0


if __name__ == '__main__':
    sys.exit(main())
