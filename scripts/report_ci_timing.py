#!/usr/bin/env python3
"""Summarize saved GitHub API run/job snapshots; diagnostic, never a CI verdict.

Input schema and collection recipe: docs/reference/ci-timing.md.
"""
import argparse
from collections import Counter
from datetime import datetime
from decimal import Decimal
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
    if not {'Test', 'Levenshtein'} <= set(required):
        issues.append('qualification requires Test and Levenshtein inventories')
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
    elif any(stamp(event) > stamp(w['created_at']) for w in workflows):
        issues.append('event timestamp follows workflow creation')
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
        # Reusing either workflow with a new counterpart is still repetition.
        unique, used_runs = [], set()
        for s in valid:
            identities = {(w['name'], w['id']) for w in s['workflows']}
            if identities & used_runs:
                continue
            used_runs.update(identities)
            unique.append(s)
        values = sorted(s['event_to_required_completion_seconds'] for s in unique)
        n = len(values)
        p95 = values[math.ceil(.95*n)-1] if n >= 20 else None
        summaries.append({'cohort': cohort, 'cache_state': cache, 'comparable_group': group,
                          'observations': len(items), 'qualifying_unique_runs': n,
                          'selected_sample_ids': [s['id'] for s in unique],
                          'excluded_or_repeated': len(items)-n, 'directional_sample_ready': n >= 10,
                          'provisional_p95_seconds': p95, 'target_150s_met': p95 <= 150 if p95 is not None else None})
    return {'schema': 1, 'diagnostic_only': True, 'samples': samples, 'summaries': summaries}


def go_event_stamp(value):
    """Parse Go RFC3339Nano without losing fractional precision on Python 3.9."""
    if not isinstance(value, str):
        raise ValueError('missing Go event timestamp')
    match = re.fullmatch(r'(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2})(?:\.(\d{1,9}))?(Z|[+-]\d{2}:\d{2})', value)
    if not match:
        raise ValueError('invalid Go event timestamp')
    whole, fraction, zone = match.groups()
    if zone != 'Z' and (int(zone[1:3]) > 23 or int(zone[4:6]) > 59):
        raise ValueError('invalid Go event timezone offset')
    return Decimal(str(stamp(whole + zone))) + Decimal('0.' + (fraction or '0'))


def parent_tree_timings(events):
    """Diagnostic wall intervals for top-level Go lifecycles, per invocation.

    Go's terminal Elapsed omits waits for parallel descendants. A paused parent
    starts its measured wall interval at cont; an unpaused parent starts at run.
    Incomplete or inconsistent lifecycle evidence never supplies a wall estimate.
    """
    active, rows = {}, []

    def finish(key, state, terminal=None):
        issues = list(state['issues'])
        seconds = None
        if terminal is None:
            issues.append('missing parent terminal')
        elif state['phase'] == 'paused':
            issues.append('paused parent missing cont')
        if terminal is not None and not issues:
            try:
                seconds = float(go_event_stamp(terminal.get('Time')) - go_event_stamp(state['start']))
                if seconds < 0:
                    raise ValueError('parent lifecycle timestamps run backwards')
            except (AttributeError, TypeError, ValueError):
                seconds = None
                issues.append('missing/invalid parent lifecycle timestamp')
        rows.append({'package': key[0], 'parent': key[1], 'seconds': seconds,
                     'own_elapsed_seconds': terminal.get('Elapsed') if terminal else None,
                     'status': terminal.get('Action') if terminal else None,
                     'timing_issues': issues})

    for event in events:
        package, name, action = event.get('Package'), event.get('Test'), event.get('Action')
        # A fresh package invocation cannot complete a previous invocation's tree.
        if not name and action == 'start':
            for key in list(active):
                if key[0] == package:
                    finish(key, active.pop(key))
        if not name or '/' in name or action not in ('run', 'pause', 'cont', 'pass', 'fail', 'skip'):
            continue
        key = (package, name)
        if action == 'run':
            if key in active:
                old = active.pop(key)
                old['issues'].append('duplicate run before parent terminal')
                finish(key, old)
            active[key] = {'phase': 'run', 'start': event.get('Time'), 'last': None, 'issues': []}
        if key not in active:
            active[key] = {'phase': 'missing-run', 'start': None, 'last': None,
                           'issues': ['missing parent run']}
        state = active[key]
        try:
            current = go_event_stamp(event.get('Time'))
            if state['last'] is not None and current < state['last']:
                state['issues'].append('parent lifecycle timestamps run backwards')
            state['last'] = current
        except (AttributeError, TypeError, ValueError):
            state['issues'].append('missing/invalid parent lifecycle timestamp')
        if action == 'pause':
            if state['phase'] != 'run':
                state['issues'].append('unexpected parent pause')
            state['phase'] = 'paused'
        elif action == 'cont':
            if state['phase'] != 'paused':
                state['issues'].append('unexpected parent cont')
            state['phase'], state['start'] = 'continued', event.get('Time')
        elif action in ('pass', 'fail', 'skip'):
            finish(key, active.pop(key), event)
    for key, state in active.items():
        finish(key, state)
    return rows


def worker_evidence(directory):
    """Read existing coverage artifacts; this is not a second coverage registry."""
    import run_go_shard
    manifest = json.loads((directory / 'manifest.json').read_text())
    packages, parents = [], []
    for filename in ('ordinary.json', 'cli.json', 'git-identity.json'):
        path = directory / filename
        if not path.exists():
            continue
        events = list(run_go_shard.json_stream(path.read_text()))
        parents.extend(dict(row, log=filename) for row in parent_tree_timings(events))
        for event in events:
            if event.get('Action') in ('pass', 'fail', 'skip') and not event.get('Test'):
                packages.append({'package': event.get('Package'), 'seconds': event.get('Elapsed'),
                                 'status': event['Action'], 'log': filename})
    order = lambda row: row.get('seconds') or 0
    return {'directory': str(directory), 'source': manifest.get('catalog', {}).get('source'),
            'shard': manifest.get('shard'), 'success': manifest.get('success'),
            'phases': manifest.get('phases'), 'settings': manifest.get('settings'),
            'timing_observation': manifest.get('timing_observation'),
            'packages': sorted(packages, key=order, reverse=True),
            'complete_parent_trees': sorted(parents, key=order, reverse=True),
            'note': 'Parent seconds measure cont-to-terminal (run-to-terminal when unpaused), including descendant waits; unknown lifecycle timing is null. own_elapsed_seconds is Go terminal Elapsed and can omit parallel descendant waits. Never sum parents. Coverage reconciliation remains authoritative.'}


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
