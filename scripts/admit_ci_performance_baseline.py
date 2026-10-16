#!/usr/bin/env python3
"""Prepare a review-only performance policy from a verified ordinary full pipeline."""
import argparse
import copy
import json
from pathlib import Path

import ci_performance_gate as gate
import ci_timing_history
import normalize_ci_timing


def verified(rows, topology, pipeline):
    if topology['observation_kind'] != 'normal' or pipeline.get('snapshot_status') != 'terminal':
        raise ValueError('baseline admission requires terminal ordinary production evidence')
    matched = [sample for sample in pipeline['input']['samples']
               if sample.get('tested_checkout') == topology and sample.get('observation_kind') == 'normal']
    if len(matched) != 1:
        raise ValueError('baseline pipeline differs from exact tested topology')
    snapshots = [snap for snap in matched[0]['workflows'] if snap['run']['name'] == 'Test']
    runs = [snap['run'] for snap in snapshots]
    if len(runs) != 1:
        raise ValueError('baseline Test run unavailable')
    run = runs[0]
    actual = gate.worker_attempts(snapshots[0]['jobs']['jobs'], topology, run['id'], run['run_attempt'])
    tests, _ = gate.observations(rows, topology, run['id'], run['run_attempt'], actual)
    measured = copy.deepcopy(rows)
    for row in measured:
        for test in row['tests']:
            test['own_elapsed_seconds'] = max(test['own_elapsed_seconds'], test['slowest_descendant_seconds'])
    candidate = ci_timing_history.candidate(measured, [pipeline])
    if candidate['physical_worker_observations'] != topology['workers'] or not candidate['groups']:
        raise ValueError('baseline lacks eligible successful full pipeline observations')
    return tests, measured, {'source': topology['source'], 'tree': topology['tree'],
        'run_id': run['id'], 'run_attempt': run['run_attempt'], 'workers': topology['workers'],
        'inventory_sha256': normalize_ci_timing.digest(sorted(tests)),
        'compiled_inventory_sha256': measured[0]['compiled_inventory_sha256'],
        'pipeline_sha256': normalize_ci_timing.digest(pipeline)}


def admit(policy, rows, topology, pipeline, history_samples=()):
    tests, measured, proof = verified(rows, topology, pipeline)
    observations, pipelines, proofs = list(measured), [pipeline], [proof]
    for old_rows, old_topology, old_pipeline in history_samples:
        _, old_measured, old_proof = verified(old_rows, old_topology, old_pipeline)
        observations.extend(old_measured)
        pipelines.append(old_pipeline)
        proofs.append(old_proof)
    # Each sample already passed exact source, compiled inventory and physical
    # allocation validation. Inventory additions must not erase comparison of
    # surviving individual parents in the same execution configuration.
    candidate = ci_timing_history.candidate(observations, pipelines, bind_inventory=False)
    # Current compiled/executed inventory defines admission. Removed historical
    # parents cannot become an allowance for a future reintroduced test.
    ids = sorted(tests)
    unique_proofs = {}
    for sample in proofs:
        identity = (sample['run_id'], sample['run_attempt'])
        if identity in unique_proofs and unique_proofs[identity] != sample:
            raise ValueError('conflicting baseline sample provenance')
        unique_proofs[identity] = sample
    policy['baseline'] = {'inventory': ids, 'provenance': proof, 'samples': list(unique_proofs.values()),
        'metric': 'maximum-parent-or-descendant-terminal-elapsed',
        'groups': [{'key': group['key'],
                    'identity': {key: group[key] for key in ('configuration_sha256', 'cohort')},
                    'tests': {ident: value for ident, value in group['tests'].items() if ident in tests}}
                   for group in candidate['groups'] if any(ident in tests for ident in group['tests'])]}
    return gate.policy_check(policy)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--policy', type=Path, required=True)
    parser.add_argument('--topology', type=Path, required=True)
    parser.add_argument('--pipeline', type=Path, required=True)
    parser.add_argument('--workers', type=Path, required=True)
    parser.add_argument('--output', type=Path, required=True)
    parser.add_argument('--history-sample', nargs=3, action='append', default=[],
                        metavar=('TOPOLOGY', 'PIPELINE', 'WORKERS'),
                        help='Additional authenticated ordinary sample; repeats do not increase sample size')
    parser.add_argument('--allow-legacy-compiled-catalog', action='store_true',
                        help='Offline review only: validate original schema-3 compiled receipts; live CI still requires schema 4')
    args = parser.parse_args()
    try:
        topology = gate.read(args.topology)
        paths = sorted(args.workers.glob('*/manifest.json'))
        rows = gate.normalize_workers(paths, topology, verify_coverage=True, allow_legacy=args.allow_legacy_compiled_catalog)
        if len(args.history_sample) > 30:
            raise ValueError('history exceeds bounded ordinary sample count')
        history = []
        for topology_path, pipeline_path, workers_path in args.history_sample:
            old_topology = gate.read(Path(topology_path))
            old_paths = sorted(Path(workers_path).glob('*/manifest.json'))
            history.append((gate.normalize_workers(old_paths, old_topology, verify_coverage=True, allow_legacy=args.allow_legacy_compiled_catalog), old_topology, gate.read(Path(pipeline_path))))
        result = admit(gate.read(args.policy), rows, topology, gate.read(args.pipeline), history)
        args.output.write_text(json.dumps(result, indent=2) + '\n')
        print('Review candidate only: commit after validating source evidence and maintainer review; never automatically promote.')
    except (ValueError, KeyError, TypeError, OSError) as error:
        parser.exit(1, 'Baseline admission failed: ' + str(error) + '\n')


if __name__ == '__main__':
    main()
