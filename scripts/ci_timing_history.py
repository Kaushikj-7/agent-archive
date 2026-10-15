#!/usr/bin/env python3
"""Build a compact review candidate from normalized observations; never auto-promote."""
import argparse
import json
from pathlib import Path
from statistics import median

from normalize_ci_timing import digest, seconds
import collect_ci_timing
import report_ci_timing

MAX_OBSERVATIONS = 256


def candidate(observations, pipelines=()):
    eligible = set()
    for pipeline in pipelines:
        if pipeline.get('snapshot_status') != 'terminal':
            continue
        document = pipeline['input']
        summary = report_ci_timing.report(document)
        for sample, row in zip(document['samples'], summary['samples']):
            topology = sample.get('tested_checkout') or {}
            if (sample.get('scope', 'production-pipeline') != 'production-pipeline'
                    or sample.get('observation_kind') != 'normal' or not row['required_completed_at']
                    or sample.get('required_jobs') != collect_ci_timing.inventory(topology.get('workers'))
                    or any(snap['run'].get('conclusion') != 'success' for snap in sample['workflows'])):
                continue
            for snap in sample['workflows']:
                if snap['run']['name'] == 'Test':
                    eligible.add((str(snap['run']['id']), str(snap['run']['run_attempt']), topology['source']))
    identities, groups = {}, {}
    for observation in observations:
        if observation.get('schema') != 1 or observation.get('kind') != 'worker-timing':
            raise ValueError('unsupported timing observation')
        identity = tuple(observation[key] for key in ('run_id', 'run_attempt', 'shard'))
        signature = digest(observation)
        if identity in identities:
            if identities[identity] != signature:
                raise ValueError('conflicting observation for physical worker')
            continue
        identities[identity] = signature
        if (not observation.get('success') or observation.get('observation_kind') != 'normal'
                or (str(identity[0]), str(identity[1]), observation['source']) not in eligible):
            continue
        cohort = observation['cohort']
        if cohort.get('cache_state') == 'unknown' or cohort.get('runner_image') == 'unknown':
            continue
        group = {'configuration_sha256': observation['configuration_sha256'], 'cohort': cohort}
        key = digest(group)
        bucket = groups.setdefault(key, {'identity': group, 'sources': set(), 'tests': {}})
        bucket['sources'].add(observation['source'])
        local = set()
        for row in observation['tests']:
            ident = row['test_id']
            if ident in local:
                raise ValueError('duplicate parent timing within worker')
            local.add(ident)
            own = seconds(row['own_elapsed_seconds'])
            if own is None or row['status'] != 'pass' or row['timing_issues']:
                continue
            bucket['tests'].setdefault(ident, []).append(own)
    return {'schema': 1, 'kind': 'reviewed-baseline-candidate', 'automatic_promotion': False,
            'physical_worker_observations': len(identities),
            'groups': [{'key': key, **bucket['identity'], 'sources': sorted(bucket['sources']),
                        'tests': {ident: {'own_elapsed_median_seconds': median(values),
                                          'observations': len(values), 'setup_attribution': 'unattributed'}
                                  for ident, values in sorted(bucket['tests'].items())}}
                       for key, bucket in sorted(groups.items())],
            'note': 'Review candidate only. No permissions, baseline or performance allowances are changed. Own Elapsed is not CPU time and may omit parallel descendant waits.'}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--observation', type=Path, action='append', required=True)
    parser.add_argument('--pipeline-observation', type=Path, action='append', required=True,
                        help='Terminal collector receipt proving full production pipeline success')
    parser.add_argument('--output', type=Path, required=True)
    args = parser.parse_args()
    try:
        if len(args.observation) > MAX_OBSERVATIONS or len(args.pipeline_observation) > MAX_OBSERVATIONS:
            raise ValueError('history candidate exceeds bounded observation count')
        observations = []
        for path in args.observation:
            if path.is_symlink() or path.stat().st_size > 8 * 1024 * 1024:
                raise ValueError('observation exceeds bound or is a symlink')
            observations.append(json.loads(path.read_text()))
        pipelines = []
        for path in args.pipeline_observation:
            if path.is_symlink() or path.stat().st_size > 8 * 1024 * 1024:
                raise ValueError('pipeline observation exceeds bound or is a symlink')
            pipelines.append(json.loads(path.read_text()))
        args.output.write_text(json.dumps(candidate(observations, pipelines), indent=2) + '\n')
    except (ValueError, KeyError, TypeError, OSError) as error:
        parser.exit(1, 'Cannot build reviewed history candidate: ' + str(error) + '\n')


if __name__ == '__main__':
    main()
