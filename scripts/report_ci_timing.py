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
from statistics import median


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


def runner_usage(job, seconds):
    """Separate API timestamp intervals from evidence of runner allocation."""
    runner_id = job.get('runner_id')
    executed = any(step.get('started_at') and step.get('conclusion') != 'skipped'
                   for step in job.get('steps', []))
    if (type(runner_id) is int and runner_id > 0) or job.get('runner_name') or executed:
        return 'allocated', seconds
    explicit_no_runner = (type(runner_id) is int and runner_id == 0
                          and job.get('runner_name') == '')
    # GitHub emits null allocation fields for a job skipped by its condition.
    skipped_no_runner = (job.get('conclusion') == 'skipped' and 'runner_id' in job
                         and runner_id is None and 'runner_name' in job
                         and job['runner_name'] is None)
    if (job.get('status') == 'completed' and job.get('conclusion') in ('cancelled', 'skipped')
            and (explicit_no_runner or skipped_no_runner) and job.get('steps') == []):
        return 'unallocated', 0
    # Older saved successful snapshots omitted allocation fields. Preserve that
    # format explicitly; an unsuccessful unknown job cannot use this fallback.
    if ('runner_id' not in job and 'runner_name' not in job
            and job.get('conclusion') == 'success'):
        return 'legacy-success-timestamps', seconds
    return 'unknown', None


def diagnostic_interval(start, end, label, diagnostics):
    """Keep malformed raw API intervals visible without inventing runtime."""
    if not start or not end:
        diagnostics.append(label + ': missing timestamps')
        return None
    try:
        return elapsed(start, end)
    except (ValueError, TypeError, AttributeError) as error:
        diagnostics.append(label + ': ' + str(error))
        return None


def platform(job):
    """Only explicit API labels identify an OS; runner names are not evidence."""
    labels = [str(value).lower() for value in job.get('labels', [])]
    systems = set()
    for label in labels:
        if label == 'linux' or label.startswith(('ubuntu-', 'ubuntu_')):
            systems.add('linux')
        elif label in ('macos', 'darwin') or label.startswith('macos-'):
            systems.add('macos')
        elif label == 'windows' or label.startswith('windows-'):
            systems.add('windows')
    return next(iter(systems)) if len(systems) == 1 else 'unknown'


def physical_jobs(jobs):
    """Keep API IDs; recognize only fully evidenced carried-forward aliases."""
    found, allocations, result = {}, {}, []
    for job in sorted(jobs, key=lambda row: row['job_attempt']
                      if type(row.get('job_attempt')) is int else 0):
        identity = (job.get('repository'), job['run_id'], job.get('id'))
        if identity[-1] is not None and identity in found:
            previous = found[identity]
            fields = ('name', 'workflow', 'started_at', 'completed_at', 'runner_id',
                      'runner_name', 'runner_allocation', 'runner_seconds', 'platform',
                      'conclusion', 'job_attempt', 'allocation_steps', 'source')
            if any(previous.get(key) != job.get(key) for key in fields):
                raise ValueError('conflicting snapshots for physical job identity')
            continue
        # Targeted reruns can issue NEW job IDs for old successful allocations.
        # Require the same run/name/source, positive runner identity, exact
        # interval and step evidence, plus a later attempt created AFTER that
        # allocation completed. A runner ID alone never establishes an alias.
        fingerprint = (job.get('repository'), job['run_id'], job['workflow'], job['name'],
                       job.get('source'), job.get('runner_id'), job.get('runner_name'),
                       job.get('started_at'), job.get('completed_at'),
                       tuple(job.get('labels', [])), json.dumps(job.get('allocation_steps'), sort_keys=True))
        evidenced = (job['runner_allocation'] == 'allocated' and job['conclusion'] == 'success'
                     and not job.get('timestamp_issues')
                     and type(job.get('job_attempt')) is int and job['job_attempt'] > 0
                     and type(job.get('runner_id')) is int and job['runner_id'] > 0
                     and job.get('runner_name') and job.get('runner_seconds') is not None
                     and job.get('id') is not None and job.get('source')
                     and any(step.get('status') == 'completed'
                             and step.get('conclusion') == 'success'
                             and step.get('started_at') and step.get('completed_at')
                             and stamp(job['started_at']) <= stamp(step['started_at'])
                             <= stamp(step['completed_at']) <= stamp(job['completed_at'])
                             for step in job.get('allocation_steps', [])))
        previous = allocations.get(fingerprint) if evidenced else None
        if (previous is not None and job.get('created_at') and job.get('completed_at')
                and job['job_attempt'] > previous['job_attempt']
                and stamp(job['created_at']) >= stamp(job['completed_at'])):
            previous['allocation_job_ids'].append(job.get('id'))
            if identity[-1] is not None:
                found[identity] = dict(job)
            continue
        row = dict(job, allocation_job_ids=[job.get('id')])
        result.append(row)
        if identity[-1] is not None:
            found[identity] = row
        if evidenced:
            allocations[fingerprint] = row
    return result


def resource_report(jobs, inventory_complete=True):
    jobs = physical_jobs(jobs)
    known = sum(j['runner_seconds'] or 0 for j in jobs)
    complete = inventory_complete and all(j['runner_seconds'] is not None for j in jobs)
    linux = [j for j in jobs if j['platform'] == 'linux']
    unknown_os = any(j['platform'] == 'unknown' and j['runner_allocation'] != 'unallocated' for j in jobs)
    linux_complete = inventory_complete and not unknown_os and all(j['runner_seconds'] is not None for j in linux)
    points = []
    peak_complete = linux_complete
    for job in linux:
        if job['runner_allocation'] == 'unallocated':
            continue
        if (job['runner_allocation'] != 'allocated' or job['runner_seconds'] is None
                or job.get('id') is None):
            peak_complete = False
            continue
        if job['runner_seconds'] > 0:
            points.extend(((stamp(job['started_at']), 1), (stamp(job['completed_at']), -1)))
    active = peak = 0
    for _, change in sorted(points):  # End before start at the same instant.
        active += change
        peak = max(active, peak)
    linux_minutes = sum(j['runner_seconds'] or 0 for j in linux) / 60
    return {'physical_job_rows': len(jobs), 'inventory_complete': inventory_complete,
            'carried_forward_allocation_aliases': [j['allocation_job_ids'] for j in jobs if len(j['allocation_job_ids']) > 1], 'actual_runner_minutes': known / 60 if complete else None,
            'known_runner_minutes': known / 60, 'runner_minutes_complete': complete,
            'linux_runner_minutes': linux_minutes if linux_complete else None,
            'known_linux_runner_minutes': linux_minutes, 'linux_usage_complete': linux_complete,
            'peak_simultaneous_linux_jobs': peak if peak_complete else None,
            'known_peak_simultaneous_linux_jobs': peak, 'linux_peak_complete': peak_complete}


def readiness(job, run, telemetry):
    """The jobs REST API alone cannot split dependency and admission waits."""
    result = {'dependency_wait_seconds': None, 'eligible_to_start_seconds': None,
              'readiness_evidence': None}
    if not telemetry:
        return result
    if not isinstance(telemetry.get('source'), str) or not telemetry['source'].strip():
        raise ValueError('readiness telemetry requires an evidence source')
    dependency = telemetry.get('dependencies_ready_at')
    eligible = telemetry.get('eligible_at')
    start = job.get('started_at')
    if start:
        try:
            stamp(start)
        except (ValueError, TypeError, AttributeError):
            start = None  # The raw job timestamp is retained with row diagnostics.
    created = run['created_at']
    if dependency:
        result['dependency_wait_seconds'] = elapsed(created, dependency)
        if start:
            elapsed(dependency, start)
    if eligible:
        elapsed(created, eligible)
        if dependency:
            elapsed(dependency, eligible)
        if start:
            result['eligible_to_start_seconds'] = elapsed(eligible, start)
    result['readiness_evidence'] = telemetry
    return result


def sample_report(sample):
    issues, jobs, workflows, identities = [], [], [], []
    inventory_complete = True
    required = sample['required_jobs']
    if not required or any(not names or len(set(names)) != len(names) for names in required.values()):
        raise ValueError('nonempty unique required job inventories are mandatory')
    scope = sample.get('scope', 'production-pipeline')
    if scope not in ('production-pipeline', 'linux-shard-topology-only', 'auxiliary-campaign'):
        raise ValueError('unsupported observation scope')
    expected_workflows = {'Test', 'Levenshtein'}
    if scope == 'linux-shard-topology-only':
        expected_workflows = {'Linux worker qualification'}
        if sample.get('worker_count') not in (4, 8):
            raise ValueError('shard diagnostic requires four/eight worker count')
        issues.append('shard-topology diagnostic cannot qualify production PR latency')
    if scope == 'auxiliary-campaign':
        expected_workflows = set(required)
        issues.append('auxiliary campaign cannot qualify production PR latency')
    if not expected_workflows <= set(required):
        issues.append('qualification requires Test and Levenshtein inventories')
        inventory_complete = False
    seen = set()
    for snapshot in sample['workflows']:
        run = snapshot['run']
        name = run['name']
        if type(run.get('run_attempt', 1)) is not int or run.get('run_attempt', 1) < 1:
            raise ValueError('workflow attempt must be a positive integer')
        if name in seen or name not in required:
            raise ValueError('duplicate or undeclared workflow')
        seen.add(name)
        identities.append(run['head_sha'])
        repository = run.get('repository', {}).get('full_name', sample.get('repository'))
        if sample.get('repository') and repository != sample['repository']:
            issues.append(name + ': workflow repository mismatch')
            inventory_complete = False
        if run.get('status') != 'completed' or run.get('conclusion') != 'success':
            issues.append(name + ': workflow not successful/completed')
        if run.get('status') != 'completed':
            inventory_complete = False  # More jobs can still appear in a live run.
        raw = snapshot['jobs']
        if isinstance(raw, dict) and raw.get('total_count', len(raw['jobs'])) != len(raw['jobs']):
            issues.append(name + ': incomplete paginated job snapshot')
            inventory_complete = False
        raw = raw['jobs'] if isinstance(raw, dict) else raw
        if any(j.get('status') != 'completed' for j in raw):
            issues.append(name + ': unfinished job snapshot')
            inventory_complete = False
        if any(j.get('run_id', run['id']) != run['id'] for j in raw):
            issues.append(name + ': job belongs to another run')
            inventory_complete = False
        if any(type(j.get('run_attempt', 1)) is not int or not 1 <= j.get('run_attempt', 1) <= run.get('run_attempt', 1) for j in raw):
            issues.append(name + ': invalid/future job attempt')
            inventory_complete = False
        if any(j.get('head_sha', run['head_sha']) != run['head_sha'] for j in raw):
            issues.append(name + ': job source mismatch')
            inventory_complete = False
        if any(j.get('workflow_name', name) != name for j in raw):
            issues.append(name + ': job workflow mismatch')
            inventory_complete = False
        if len({j.get('id') for j in raw if j.get('id') is not None}) != sum(j.get('id') is not None for j in raw):
            raise ValueError('duplicate job id within workflow snapshot')
        names = Counter(job['name'] for job in raw)
        for expected in required[name]:
            if names[expected] != 1:
                issues.append(name + ': missing/duplicate required job ' + expected)
                inventory_complete = False
        for job in raw:
            selected = job['name'] in required[name]
            if selected and (job.get('status') != 'completed' or job.get('conclusion') != 'success'):
                issues.append(name + ': required job not successful ' + job['name'])
            allocation, _ = runner_usage(job, None)
            timestamp_issues = []
            seconds = diagnostic_interval(job.get('started_at'), job.get('completed_at'),
                                          'job interval', timestamp_issues)
            start_delay = diagnostic_interval(run['created_at'], job.get('started_at'),
                                              'job start delay', timestamp_issues)
            if job.get('created_at'):
                diagnostic_interval(job['created_at'], job['created_at'],
                                    'job creation', timestamp_issues)
            allocation, runner_seconds = runner_usage(job, seconds)
            if allocation == 'unknown':
                issues.append(name + ': runner allocation unknown ' + job['name'])
            elif runner_seconds is None:
                issues.append(name + ': incomplete runner usage timestamps ' + job['name'])
            steps = []
            for step in job.get('steps', []):
                duration = (diagnostic_interval(step.get('started_at'), step.get('completed_at'),
                                               'step ' + step['name'], timestamp_issues)
                            if step.get('started_at') or step.get('completed_at') else None)
                steps.append({'name': step['name'], 'category': category(step['name']),
                              'seconds': duration, 'conclusion': step.get('conclusion')})
            if timestamp_issues and allocation != 'unallocated':
                issues.append(name + ': invalid/incomplete timing evidence ' + job['name'])
            jobs.append({'workflow': job.get('workflow_name', name), 'snapshot_workflow': name,
                         'name': job['name'], 'required': selected,
                         'id': job.get('id'), 'run_id': job.get('run_id', run['id']),
                         'snapshot_run_id': run['id'],
                         'repository': repository,
                         'job_attempt': job.get('run_attempt', run.get('run_attempt', 1)),
                         'snapshot_attempt': run.get('run_attempt', 1),
                         'runner_id': job.get('runner_id'), 'platform': platform(job),
                         'source': job.get('head_sha', run['head_sha']),
                         'snapshot_source': run['head_sha'], 'created_at': job.get('created_at'),
                         'allocation_steps': [{k: step.get(k) for k in ('name', 'number', 'status', 'conclusion', 'started_at', 'completed_at')} for step in job.get('steps', [])],
                         **readiness(job, run, sample.get('job_readiness', {}).get(str(job.get('id')))),
                         'status': job.get('status'), 'conclusion': job.get('conclusion'), 'seconds': seconds,
                         'runner_allocation': allocation, 'runner_seconds': runner_seconds,
                         'run_created_to_job_start_seconds': start_delay,
                         'started_at': job.get('started_at'), 'completed_at': job.get('completed_at'),
                         'timestamp_issues': timestamp_issues,
                         'runner_name': job.get('runner_name'), 'labels': job.get('labels', []),
                         'steps': steps})
        workflows.append({'name': name, 'id': run['id'], 'attempt': run.get('run_attempt', 1),
                          'source': run['head_sha'], 'repository': repository,
                          'event': run.get('event'), 'created_at': run['created_at']})
    if seen != set(required):
        issues.append('missing required workflows')
        inventory_complete = False
    if set(identities) != {sample['source']}:
        issues.append('workflow source mismatch')
        inventory_complete = False
    if len({w['repository'] for w in workflows if w['repository']}) > 1:
        issues.append('workflow repositories differ')
        inventory_complete = False
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
    ends = [job['completed_at'] for job in jobs
            if job['required'] and job['seconds'] is not None]
    start = event or min((w['created_at'] for w in workflows), default=None)
    wall = elapsed(start, max(ends, key=stamp)) if start and ends else None
    resources = resource_report(jobs, inventory_complete and expected_workflows <= seen)
    return {'id': sample['id'], 'cohort': cohort, 'cache_state': cache,
            'comparable_group': sample.get('comparable_group'), 'source': sample['source'],
            'qualifying': not issues, 'issues': issues, 'event_to_required_completion_seconds': wall if event else None,
            'run_created_lower_bound_seconds': wall if not event else None,
            **resources,
            'required_completed_at': (max(ends, key=stamp) if ends and inventory_complete
                                      and all(j['status'] == 'completed' and j['conclusion'] == 'success' and j['seconds'] is not None
                                              for j in jobs if j['required']) else None),
            'event_at': event, 'worker_count': sample.get('worker_count'),
            'scope': sample.get('scope', 'production-pipeline'),
            'workflows': workflows, 'jobs': jobs,
            'cache_inventory': sample.get('cache_inventory'),
            'artifact_inventory': sample.get('artifact_inventory'),
            'worker_evidence': sample.get('worker_evidence', []),
            'notes': ['Job start delay includes queueing, dependencies and orchestration; not pure queue time.',
                      'Step categories are labels; inspect raw steps. Parallel command durations must not be summed.',
                      'Job seconds retain API timestamp intervals, including unallocated cancellation placeholders.',
                      'Runner minutes use allocated elapsed usage, not rounded billable minutes; unknown usage makes the total null.',
                      'Legacy successful snapshots without allocation metadata use explicitly labeled timestamp estimates.']}


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
                          'median_event_to_required_completion_seconds': median(values) if values else None,
                          'provisional_p95_seconds': p95, 'target_150s_met': p95 <= 150 if p95 is not None else None})
    return {'schema': 1, 'diagnostic_only': True, 'samples': samples, 'summaries': summaries,
            'resources_across_snapshots': resource_report([j for s in samples for j in s['jobs']], all(s['inventory_complete'] for s in samples)),
            'batches': batch_reports(document.get('batches', []), samples)}


def batch_reports(batches, samples):
    indexed = {s['id']: s for s in samples}
    output, seen = [], set()
    for batch in batches:
        if batch['id'] in seen:
            raise ValueError('duplicate batch id')
        seen.add(batch['id'])
        ids = batch['sample_ids']
        if not ids or len(set(ids)) != len(ids):
            raise ValueError('batch requires unique nonempty sample ids')
        selected = [indexed[value] for value in ids]
        runs = [(w['name'], w['id']) for s in selected for w in s['workflows']]
        if len(runs) != len(set(runs)):
            raise ValueError('batch PR observations must not share workflow runs')
        all_events = all(s['event_at'] for s in selected)
        starts = [s['event_at'] if all_events else min(w['created_at'] for w in s['workflows']) for s in selected]
        ends = [s['required_completed_at'] for s in selected]
        complete = all(ends)
        span = elapsed(min(starts, key=stamp), max(ends, key=stamp)) if complete else None
        output.append({'id': batch['id'], 'sample_ids': ids,
                       'scope': sorted(set(s['scope'] for s in selected)),
                       'all_samples_qualifying': all(s['qualifying'] for s in selected),
                       'first_event_to_last_required_completion_seconds': span if all_events else None,
                       'run_created_batch_lower_bound_seconds': span if not all_events else None,
                       **resource_report([j for s in selected for j in s['jobs']], all(s['inventory_complete'] for s in selected)),
                       'note': 'Observed batch span includes arrival spacing. It is not a causal queue estimate or a production verdict.'})
    return output


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
    filenames = ['ordinary.json','cli.json','git-identity.json'] + sorted(p.name for p in directory.glob('parents-*.json'))
    for filename in filenames:
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
