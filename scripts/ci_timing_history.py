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
            # The collector's outer provenance and each embedded sample must
            # describe the same tested merge. Job completeness alone validates
            # PR head allocations, not an independently relabeled merge SHA.
            if (topology != pipeline.get('topology')
                    or sample.get('source') != topology.get('head')
                    or topology.get('source') != topology.get('workflow_source')
                    or topology.get('parents') != [topology.get('tested_base'), topology.get('head')]):
                raise ValueError('pipeline tested source provenance differs')
            if (sample.get('scope', 'production-pipeline') != 'production-pipeline'
                    or sample.get('observation_kind') != 'normal' or not row['required_completed_at']
                    or sample.get('required_jobs') != collect_ci_timing.inventory(topology.get('workers'))
                    or any(snap['run'].get('conclusion') != 'success' for snap in sample['workflows'])):
                continue
            # Targeted reruns carry successful physical jobs from older
            # attempts. Bind each shard to its actual allocation attempt,
            # rather than relabeling it with the enclosing workflow attempt.
            for job in row['jobs']:
                if job['snapshot_workflow'] != 'Test':
                    continue
                for shard in range(topology['workers']):
                    if job['name'] == f'linux-short ({shard})':
                        eligible.add((str(job['run_id']), str(job['job_attempt']),
                                      shard, topology['source']))
    identities, groups = {}, {}
    for observation in observations:
        if observation.get('schema') != 1 or observation.get('kind') != 'worker-timing':
            raise ValueError('unsupported timing observation')
        run_id, attempt, shard = (observation[key] for key in ('run_id', 'run_attempt', 'shard'))
        if (not str(run_id).isdecimal() or int(run_id) <= 0
                or not str(attempt).isdecimal() or not 1 <= int(attempt) <= 10
                or type(shard) is not int or shard < 0):
            raise ValueError('invalid physical worker identity')
        identity = (str(int(run_id)), str(int(attempt)), shard)
        signature = digest(observation)
        if identity in identities:
            if identities[identity] != signature:
                raise ValueError('conflicting observation for physical worker')
            continue
        identities[identity] = signature
        if (not observation.get('success') or observation.get('observation_kind') != 'normal'
                or (*identity, observation['source']) not in eligible):
            continue
        cohort = observation['cohort']
        if (cohort.get('cache_state') not in ('exact', 'prefix', 'trusted', 'miss', 'error', 'corrupt-rebuilt')
                or not isinstance(cohort.get('runner_image'), str)
                or not cohort['runner_image'].strip() or cohort['runner_image'] == 'unknown'):
            continue
        group = {'configuration_sha256': observation['configuration_sha256'],
                 'compiled_inventory_sha256': observation['compiled_inventory_sha256'], 'cohort': cohort}
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
