#!/usr/bin/env python3
"""Normalize existing worker measurements; no execution, baseline promotion or network."""
import argparse
import hashlib
import json
import math
from pathlib import Path
import re

import report_ci_timing
import run_go_shard

MARKER = 'CI_FIXTURE_METRIC_V1 '
MAX_LOG_BYTES = 64 * 1024 * 1024
SHA = re.compile('[0-9a-f]{40}')


def digest(value):
    return hashlib.sha256(json.dumps(value, sort_keys=True, separators=(',', ':')).encode()).hexdigest()


def seconds(value):
    if value is None:
        return None
    if type(value) not in (int, float) or not math.isfinite(value) or value < 0:
        raise ValueError('invalid measured seconds')
    return value


def fixtures(events):
    rows = []
    for event in events:
        output = event.get('Output', '')
        if event.get('Action') != 'output' or MARKER not in output:
            continue
        if not event.get('Package') or not event.get('Test'):
            raise ValueError('fixture metric lacks enclosing test')
        value = json.loads(output.split(MARKER, 1)[1].strip())
        if (value.get('version') != 1 or not isinstance(value.get('fixture'), str)
                or not isinstance(value.get('phase'), str)
                or type(value.get('elapsed_ns')) is not int or value['elapsed_ns'] < 0
                or not isinstance(value.get('operations'), dict)
                or any(not isinstance(key, str) or type(count) is not int or count < 0
                       for key, count in value['operations'].items())):
            raise ValueError('invalid fixture metric')
        rows.append({'test_id': event['Package'] + '::' + event['Test'].split('/')[0],
                     'emitted_test': event['Test'], 'fixture': value['fixture'], 'phase': value['phase'],
                     'seconds': value['elapsed_ns'] / 1e9, 'operations': value['operations'],
                     'additive': False})
    return rows


def worker(directory, topology=None):
    manifest = json.loads((directory / 'manifest.json').read_text())
    catalog = manifest['catalog']
    source = catalog['source']
    if not isinstance(source, str) or not SHA.fullmatch(source):
        raise ValueError('worker source missing')
    if topology is not None and topology.get('source') != source:
        raise ValueError('worker differs from retained topology')
    if not isinstance(manifest.get('phases'), dict) or not manifest['phases']:
        raise ValueError('worker phase measurements missing')
    observation = manifest.get('timing_observation', {})
    identity = {'run_id': observation.get('run_id'), 'run_attempt': observation.get('run_attempt'),
                'shard': manifest.get('shard')}
    if (not str(identity['run_id']).isdecimal() or int(identity['run_id']) <= 0
            or not str(identity['run_attempt']).isdecimal() or not 1 <= int(identity['run_attempt']) <= 10
            or type(identity['shard']) is not int or identity['shard'] < 0):
        raise ValueError('worker lacks exact run/attempt/shard identity')
    inventory = {key: catalog[key] for key in ('units', 'cli', 'parents')}
    settings = {key: value for key, value in manifest['settings'].items()
                if key not in ('source_proof', 'plain_vet_source_proof', 'plain_vet_context_sha256', 'plain_vet_context')}
    tests, metrics = [], []
    filenames = ['ordinary.json', 'cli.json', 'git-identity.json'] + sorted(p.name for p in directory.glob('parents-*.json'))
    for filename in filenames:
        path = directory / filename
        if not path.exists():
            continue
        if path.is_symlink() or path.stat().st_size > MAX_LOG_BYTES:
            raise ValueError('worker log exceeds bound or is a symlink')
        events = list(run_go_shard.json_stream(path.read_text()))
        metrics.extend(dict(row, log=filename) for row in fixtures(events))
        for row in report_ci_timing.parent_tree_timings(events):
            tests.append({'test_id': row['package'] + '::' + row['parent'],
                          'family': row['package'], 'parent_wall_seconds': seconds(row['seconds']),
                          'own_elapsed_seconds': seconds(row['own_elapsed_seconds']),
                          'status': row['status'], 'timing_issues': row['timing_issues'],
                          'setup_attribution': 'unattributed', 'log': filename})
    return {'schema': 1, 'kind': 'worker-timing', **identity, 'source': source,
            'observation_kind': topology.get('observation_kind', 'unknown') if topology else 'unknown',
            'success': manifest.get('success') is True,
            'compiled_inventory_sha256': digest(inventory),
            'configuration_sha256': digest({'compiler': catalog['configuration'], 'settings': settings,
                                             'allocation': catalog.get('allocation')}),
            'cohort': {'cache_state': observation.get('cache_restore_state', 'unknown'),
                       'runner_image': observation.get('runner_image_version', 'unknown'),
                       'compiler': catalog['configuration']},
            'phases': [{'phase': key, 'seconds': seconds(value), 'additive': False,
                        'measurement': 'wall-including-compilation-or-startup'}
                       for key, value in manifest.get('phases', {}).items()],
            'tests': tests, 'fixture_metrics': metrics,
            'note': 'Parent wall includes descendant waits; own Elapsed can omit them. Fixture metrics can nest. Do not sum or subtract these as disjoint intervals.'}


def native(directory):
    tier = json.loads((directory / 'tier.json').read_text())
    if tier.get('schema') != 1 or not isinstance(tier.get('source'), str) or not SHA.fullmatch(tier['source']):
        raise ValueError('native source missing')
    smoke = tier.get('native_smoke', {})
    if not isinstance(smoke.get('phase_seconds'), dict) or not smoke['phase_seconds']:
        raise ValueError('native phase measurements missing')
    return {'schema': 1, 'kind': 'native-timing', 'source': tier['source'],
            'success': tier.get('success') is True,
            'phases': [{'phase': key, 'seconds': seconds(value), 'additive': False,
                        'measurement': 'serial-wall-including-compilation-or-startup'}
                       for key, value in smoke.get('phase_seconds', {}).items()],
            'note': 'These are measured invocation phases, not isolated compiler or test-body timings.'}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    inputs = parser.add_mutually_exclusive_group(required=True)
    inputs.add_argument('--worker', type=Path)
    inputs.add_argument('--native', type=Path)
    parser.add_argument('--topology', type=Path)
    parser.add_argument('--output', type=Path, required=True)
    args = parser.parse_args()
    try:
        topology = json.loads(args.topology.read_text()) if args.topology else None
        args.output.write_text(json.dumps(worker(args.worker, topology) if args.worker else native(args.native), indent=2) + '\n')
    except (ValueError, KeyError, TypeError, OSError) as error:
        parser.exit(1, 'Cannot normalize timing: ' + str(error) + '\n')


if __name__ == '__main__':
    main()
