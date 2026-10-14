#!/usr/bin/env python3
"""Summarize saved GitHub API run/job snapshots; diagnostic, never a CI verdict.

Input schema and collection recipe: docs/reference/ci-timing.md.
"""
import argparse
from collections import Counter
from datetime import datetime
import json
import math
from pathlib import Path
import re


COHORTS = ('source-only', 'cli-catalog', 'native-platform', 'dependency-toolchain', 'fork-cold-cache')


def stamp(value):
    parsed = datetime.fromisoformat(value.replace('Z', '+00:00'))
    if parsed.tzinfo is None:
        raise ValueError('timestamp requires timezone')
    return parsed.timestamp()


def elapsed(start, end):
    value = stamp(end) - stamp(start)
    if value < 0:
        raise ValueError('timestamps run backwards')
    return value


def category(name):
    name = name.lower()
    for label, pattern in (
        ('evidence-upload', r'upload|preserve.*evidence|preserve catalog'),
        ('module-restore-verify', r'restore|verify restored|cache'),
        ('tool-setup', r'set.up|install.*(go|compiler|scanner|tool)|require.*go'),
        ('discovery-build-body', r'shard|audit body|smoke|test|build|verify'),
        ('final-verdict', r'verdict|reconcile|required.*result'),
    ):
        if re.search(pattern, name):
            return label
    return 'other'


def sample_report(sample):
    issues, jobs, workflows, identities = [], [], [], []
    required = sample['required_jobs']
    if not required or any(not names or len(set(names)) != len(names) for names in required.values()):
        raise ValueError('nonempty unique required job inventories are mandatory')
    seen = set()
    for snapshot in sample['workflows']:
        run = snapshot['run']
        name = run['name']
        if name in seen or name not in required:
            raise ValueError('duplicate or undeclared workflow')
        seen.add(name)
        identities.append(run['head_sha'])
        if run.get('status') != 'completed' or run.get('conclusion') != 'success':
            issues.append(name + ': workflow not successful/completed')
        raw = snapshot['jobs']
        if isinstance(raw, dict) and raw.get('total_count', len(raw['jobs'])) != len(raw['jobs']):
            issues.append(name + ': incomplete paginated job snapshot')
        raw = raw['jobs'] if isinstance(raw, dict) else raw
        if any(j.get('run_id', run['id']) != run['id'] for j in raw):
            issues.append(name + ': job belongs to another run')
        if any(j.get('run_attempt', run.get('run_attempt', 1)) != run.get('run_attempt', 1) for j in raw):
            issues.append(name + ': mixed run attempts')
        names = Counter(job['name'] for job in raw)
        for expected in required[name]:
            if names[expected] != 1:
                issues.append(name + ': missing/duplicate required job ' + expected)
        for job in raw:
            selected = job['name'] in required[name]
            if selected and (job.get('status') != 'completed' or job.get('conclusion') != 'success'):
                issues.append(name + ': required job not successful ' + job['name'])
            seconds = None
            if job.get('started_at') and job.get('completed_at'):
                seconds = elapsed(job['started_at'], job['completed_at'])
            elif selected:
                issues.append(name + ': missing job timestamps ' + job['name'])
            if seconds is None and job.get('conclusion') != 'skipped':
                issues.append(name + ': incomplete runner usage timestamps ' + job['name'])
            steps = []
            for step in job.get('steps', []):
                duration = (elapsed(step['started_at'], step['completed_at'])
                            if step.get('started_at') and step.get('completed_at') else None)
                steps.append({'name': step['name'], 'category': category(step['name']),
                              'seconds': duration, 'conclusion': step.get('conclusion')})
            jobs.append({'workflow': name, 'name': job['name'], 'required': selected,
                         'conclusion': job.get('conclusion'), 'seconds': seconds,
                         'run_created_to_job_start_seconds': elapsed(run['created_at'], job['started_at']) if job.get('started_at') else None,
                         'runner_name': job.get('runner_name'), 'labels': job.get('labels', []),
                         'steps': steps})
        workflows.append({'name': name, 'id': run['id'], 'attempt': run.get('run_attempt', 1),
                          'source': run['head_sha'], 'created_at': run['created_at']})
    if seen != set(required):
        issues.append('missing required workflows')
    if set(identities) != {sample['source']}:
        issues.append('workflow source mismatch')
    event = sample.get('event_at')
    if not event:
        issues.append('event timestamp unavailable: run-created latency is a lower bound')
    cohort, cache = sample['cohort'], sample['cache_state']
    if cohort not in COHORTS or cache not in ('warm', 'cold', 'mixed', 'unknown'):
        raise ValueError('unsupported cohort/cache state')
    if cache == 'unknown':
        issues.append('cache state unknown')
    if not sample.get('comparable_group'):
        issues.append('comparable_group missing')
    ends = [job['completed_at'] for s in sample['workflows']
            for job in (s['jobs']['jobs'] if isinstance(s['jobs'], dict) else s['jobs'])
            if job['name'] in required[s['run']['name']] and job.get('completed_at')]
    start = event or min((w['created_at'] for w in workflows), default=None)
    wall = elapsed(start, max(ends, key=stamp)) if start and ends else None
    return {'id': sample['id'], 'cohort': cohort, 'cache_state': cache,
            'comparable_group': sample.get('comparable_group'), 'source': sample['source'],
            'qualifying': not issues, 'issues': issues, 'event_to_required_completion_seconds': wall if event else None,
            'run_created_lower_bound_seconds': wall if not event else None,
            'actual_runner_minutes': sum(j['seconds'] or 0 for j in jobs) / 60,
            'runner_minutes_complete': all(j['seconds'] is not None or j['conclusion'] == 'skipped' for j in jobs),
            'workflows': workflows, 'jobs': jobs,
            'cache_inventory': sample.get('cache_inventory'),
            'artifact_inventory': sample.get('artifact_inventory'),
            'worker_evidence': sample.get('worker_evidence', []),
            'notes': ['Job start delay includes queueing, dependencies and orchestration; not pure queue time.',
                      'Step categories are labels; inspect raw steps. Parallel command durations must not be summed.',
                      'Runner minutes are actual elapsed usage, not rounded billable minutes.']}


def report(document):
    if document.get('schema') != 1:
        raise ValueError('unsupported timing input schema')
    samples = [sample_report(s) for s in document['samples']]
    if len({s['id'] for s in samples}) != len(samples):
        raise ValueError('duplicate sample ids')
    groups = {}
    for s in samples:
        key = (s['cohort'], s['cache_state'], s['comparable_group'])
        groups.setdefault(key, []).append(s)
    summaries = []
    for (cohort, cache, group), items in groups.items():
        valid = [s for s in items if s['qualifying']]
        # Repeated attempts of the same workflow run are one observation. Keep
        # all attempts visible, but do not manufacture percentile sample size.
        unique = {}
        for s in valid:
            identity = tuple(sorted((w['name'], w['id']) for w in s['workflows']))
            unique.setdefault(identity, s)
        values = sorted(s['event_to_required_completion_seconds'] for s in unique.values())
        n = len(values)
        p95 = values[math.ceil(.95*n)-1] if n >= 20 else None
        summaries.append({'cohort': cohort, 'cache_state': cache, 'comparable_group': group,
                          'observations': len(items), 'qualifying_unique_runs': n,
                          'excluded_or_repeated': len(items)-n, 'directional_sample_ready': n >= 10,
                          'provisional_p95_seconds': p95, 'target_150s_met': p95 <= 150 if p95 is not None else None})
    return {'schema': 1, 'diagnostic_only': True, 'samples': samples, 'summaries': summaries}


def worker_evidence(directory):
    """Read existing coverage artifacts; this is not a second coverage registry."""
    import run_go_shard
    manifest = json.loads((directory / 'manifest.json').read_text())
    packages, parents = [], []
    for filename in ('ordinary.json', 'cli.json', 'git-identity.json'):
        path = directory / filename
        if not path.exists():
            continue
        for event in run_go_shard.json_stream(path.read_text()):
            if event.get('Action') not in ('pass', 'fail', 'skip'):
                continue
            name = event.get('Test')
            row = {'package': event.get('Package'), 'seconds': event.get('Elapsed'),
                   'status': event['Action'], 'log': filename}
            if name and '/' not in name:
                parents.append(dict(row, parent=name))
            elif not name:
                packages.append(row)
    order = lambda row: row.get('seconds') or 0
    return {'directory': str(directory), 'source': manifest.get('catalog', {}).get('source'),
            'shard': manifest.get('shard'), 'success': manifest.get('success'),
            'phases': manifest.get('phases'), 'settings': manifest.get('settings'),
            'timing_observation': manifest.get('timing_observation'),
            'packages': sorted(packages, key=order, reverse=True),
            'complete_parent_trees': sorted(parents, key=order, reverse=True),
            'note': 'Elapsed includes parent descendants/parallel waits; never sum parents. Coverage reconciliation remains authoritative.'}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('input', type=Path)
    parser.add_argument('--output', type=Path)
    parser.add_argument('--evidence', type=Path, action='append', default=[],
                        help='existing worker evidence directory, summarized separately without changing qualification')
    args = parser.parse_args()
    try:
        value = report(json.loads(args.input.read_text()))
        value['local_worker_evidence'] = [worker_evidence(path) for path in args.evidence]
        result = json.dumps(value, indent=2, sort_keys=True) + '\n'
    except (KeyError, TypeError, ValueError, OSError) as error:
        parser.error(str(error))
    if args.output:
        args.output.write_text(result)
    else:
        print(result, end='')


if __name__ == '__main__':
    main()
