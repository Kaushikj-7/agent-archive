#!/usr/bin/env python3
"""Build a compact review candidate from normalized observations; never auto-promote."""
import argparse
import json
import re
from pathlib import Path
from statistics import median

from normalize_ci_timing import digest, seconds
import collect_ci_timing
import report_ci_timing

MAX_OBSERVATIONS = 256


def production_jobs(pipeline, sample, row):
    """Authorize strict production allocations, beyond the advisory reporter."""
    topology = sample['tested_checkout']
    if (type(topology.get('workers')) is not int
            or topology['workers'] != collect_ci_timing.ci_worker_topology.DEFAULT_WORKERS
            or topology.get('observation_kind') != 'normal'
            or topology.get('repository') != collect_ci_timing.REPOSITORY
            or type(topology.get('pr_number')) is not int or topology['pr_number'] <= 0
            or any(not isinstance(topology.get(key), str)
                   or not collect_ci_timing.ci_worker_topology.SHA.fullmatch(topology[key])
                   for key in ('source', 'tree', 'head', 'event_base', 'tested_base'))):
        return []
    for snapshot in sample['workflows']:
        run = snapshot['run']
        name = run['name']
        # Empty postmerge associations need the collector's authenticated
        # artifact receipt. Never replace a contradictory nonempty association.
        checked = run
        if run.get('pull_requests') == []:
            proofs = pipeline.get('authenticated_retained_identities', [])
            if not any(type(proof.get('artifact_id')) is int and proof['artifact_id'] > 0
                       and proof.get('run_id') == run.get('id')
                       and proof.get('run_attempt') == run.get('run_attempt')
                       and isinstance(proof.get('digest'), str)
                       and re.fullmatch(r'sha256:[0-9a-f]{64}', proof['digest'])
                       for proof in proofs):
                return []
            checked = dict(run, pull_requests=[{'number': topology['pr_number'],
                           'head': {'sha': topology['head']}, 'base': {'sha': topology['event_base']}}])
        try:
            collect_ci_timing.validate_run(checked, name, topology)
        except (ValueError, KeyError, TypeError, AttributeError):
            return []
        raw = snapshot['jobs']
        raw = raw['jobs'] if isinstance(raw, dict) else raw
        for job in raw:
            if job['name'] not in sample['required_jobs'][name]:
                continue
            if (type(job.get('id')) is not int or job['id'] <= 0
                    or type(job.get('run_id')) is not int or job['run_id'] != run['id']
                    or type(job.get('run_attempt')) is not int
                    or not 1 <= job['run_attempt'] <= run['run_attempt']
                    or job.get('head_sha') != topology['head']
                    or type(job.get('runner_id')) is not int or job['runner_id'] <= 0
                    or not isinstance(job.get('runner_name'), str) or not job['runner_name'].strip()):
                return []
    required = [job for job in row['jobs'] if job['required']]
    if any(job['runner_allocation'] != 'allocated' or job['timestamp_issues']
           or not any(step.get('status') == 'completed' and step.get('conclusion') == 'success'
                      and step.get('started_at') and step.get('completed_at')
                      and report_ci_timing.stamp(job['started_at']) <= report_ci_timing.stamp(step['started_at'])
                      <= report_ci_timing.stamp(step['completed_at']) <= report_ci_timing.stamp(job['completed_at'])
                      for step in job['allocation_steps'])
           for job in required):
        return []
    return required


def candidate(observations, pipelines=(), *, bind_inventory=True):
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
            jobs = production_jobs(pipeline, sample, row)
            # Targeted reruns carry successful physical jobs from older
            # attempts. Bind each shard to its actual allocation attempt,
            # rather than relabeling it with the enclosing workflow attempt.
            for job in jobs:
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
        if (observation.get('success') is not True or observation.get('observation_kind') != 'normal'
                or (*identity, observation['source']) not in eligible):
            continue
        cohort = observation['cohort']
        if (cohort.get('cache_state') not in ('exact', 'prefix', 'trusted', 'miss', 'error', 'corrupt-rebuilt')
                or not isinstance(cohort.get('runner_image'), str)
                or not cohort['runner_image'].strip() or cohort['runner_image'] == 'unknown'):
            continue
        group = {'configuration_sha256': observation['configuration_sha256'], 'cohort': cohort}
        if bind_inventory:
            group['compiled_inventory_sha256'] = observation['compiled_inventory_sha256']
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
