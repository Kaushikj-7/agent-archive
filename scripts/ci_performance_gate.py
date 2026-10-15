#!/usr/bin/env python3
"""Required test-cost gate. Execute this script and its imports from trusted base."""
import argparse
from datetime import datetime, timezone
import json
import math
import os
from pathlib import Path
import re

import collect_ci_timing
import normalize_ci_timing

SHA = re.compile(r'[0-9a-f]{40}')
IDENT = re.compile(r'github\.com/wangjohn/agent-archive/[^:]+::(?:Test|Fuzz|Example)\w*')
MAX_BYTES = 16 * 1024 * 1024
POLICY = 'scripts/ci_performance_policy.json'
EXCEPTIONS = 'scripts/ci_performance_exceptions.json'


def read(path):
    if path.is_symlink() or path.stat().st_size > MAX_BYTES:
        raise ValueError('oversized or symlink performance input')
    def pairs(rows):
        out = {}
        for key, value in rows:
            if key in out:
                raise ValueError('duplicate JSON key: ' + key)
            out[key] = value
        return out
    return json.loads(path.read_text(), object_pairs_hook=pairs,
                      parse_constant=lambda _: (_ for _ in ()).throw(ValueError('nonfinite JSON number')))


def number(value):
    if type(value) not in (float, int) or not math.isfinite(value) or value < 0:
        raise ValueError('invalid performance budget or measurement')
    return value


def policy_check(policy):
    if (policy.get('schema') != 1 or policy.get('new_warning_seconds') != 10
            or policy.get('new_failure_seconds') != 30 or policy.get('regression_factor') != 1.5
            or policy.get('regression_seconds') != 10 or policy.get('minimum_observations') != 3
            or not isinstance(policy.get('maintainers'), list) or not policy['maintainers']
            or any(not re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9-]{0,38}', v) for v in policy['maintainers'])):
        raise ValueError('unsupported performance policy')
    if type(policy['new_parallel_descendants']) is not int or policy['new_parallel_descendants'] <= 0:
        raise ValueError('invalid default parallel family cap')
    number(policy['new_parallel_elapsed_work_seconds'])
    baseline = policy['baseline']
    if baseline.get('metric') != 'maximum-parent-or-descendant-terminal-elapsed':
        raise ValueError('baseline timing metric mismatch')
    ids = baseline['inventory']
    if (not isinstance(ids, list) or not ids or ids != sorted(set(ids))
            or any(not IDENT.fullmatch(v) for v in ids)):
        raise ValueError('reviewed individual-parent baseline inventory missing or invalid')
    proof = baseline['provenance']
    samples = baseline.get('samples')
    if not isinstance(samples, list) or not samples or proof not in samples:
        raise ValueError('baseline sample provenance missing')
    physical = set()
    for sample in samples:
        if (not SHA.fullmatch(sample['source']) or not SHA.fullmatch(sample['tree'])
                or type(sample['run_id']) is not int or sample['run_id'] <= 0
                or type(sample['run_attempt']) is not int or not 1 <= sample['run_attempt'] <= 10
                or sample['workers'] not in (4, 8)
                or any(not re.fullmatch('[0-9a-f]{64}', sample[key]) for key in
                       ('inventory_sha256', 'compiled_inventory_sha256', 'pipeline_sha256'))):
            raise ValueError('baseline provenance invalid')
        identity = (sample['run_id'], sample['run_attempt'])
        if identity in physical:
            raise ValueError('duplicate baseline physical sample')
        physical.add(identity)
    if proof['inventory_sha256'] != normalize_ci_timing.digest(ids):
        raise ValueError('baseline inventory provenance invalid')
    if not isinstance(baseline['groups'], list) or not baseline['groups']:
        raise ValueError('baseline measured cohorts missing')
    group_keys, parent_counts = set(), {}
    for group in baseline['groups']:
        if group['key'] in group_keys:
            raise ValueError('duplicate baseline cohort')
        group_keys.add(group['key'])
        if (set(group['identity']) != {'configuration_sha256', 'cohort'}
                or group['key'] != normalize_ci_timing.digest(group['identity'])):
            raise ValueError('baseline cohort identity invalid')
        identity = group['identity']
        cohort = identity['cohort']
        if (not re.fullmatch('[0-9a-f]{64}', identity['configuration_sha256'])
                or set(cohort) != {'cache_state', 'runner_image', 'compiler'}
                or cohort['cache_state'] not in ('exact', 'prefix', 'trusted', 'miss', 'error', 'corrupt-rebuilt')
                or not isinstance(cohort['runner_image'], str) or not cohort['runner_image'].strip()
                or cohort['runner_image'] == 'unknown'
                or not isinstance(cohort['compiler'], dict) or not cohort['compiler']
                or any(not isinstance(k, str) or not isinstance(v, str) for k, v in cohort['compiler'].items())
                or not isinstance(group['tests'], dict) or not group['tests']):
            raise ValueError('baseline measured cohort invalid')
        for ident, row in group['tests'].items():
            if (ident not in ids or type(row['observations']) is not int
                    or not 1 <= row['observations'] <= len(physical)):
                raise ValueError('baseline timing outside inventory')
            number(row['own_elapsed_median_seconds'])
            parent_counts[ident] = parent_counts.get(ident, 0) + row['observations']
            if parent_counts[ident] > len(physical):
                raise ValueError('baseline parent exceeds physical sample inventory')
    for ident, budget in policy['parallel_budgets'].items():
        if (not IDENT.fullmatch(ident) or type(budget['maximum_descendants']) is not int
                or budget['maximum_descendants'] < 0):
            raise ValueError('invalid parallel family budget')
        number(budget['maximum_elapsed_work_seconds'])
    seen = set()
    for budget in policy['work_budgets']:
        key = (budget['test_id'], budget['fixture'], budget['phase'], budget['operation'])
        if key in seen or not IDENT.fullmatch(key[0]) or any(not isinstance(v, str) or not v for v in key):
            raise ValueError('duplicate or malformed work budget')
        seen.add(key)
        if type(budget['maximum']) is not int or budget['maximum'] < 0:
            raise ValueError('invalid deterministic work budget')
    return policy


def observations(rows, topology, run_id, attempt, actual_attempts=None):
    workers = topology['workers']
    if workers not in (4, 8) or len(rows) != workers:
        raise ValueError('missing or excess worker timing receipts')
    if actual_attempts is None:
        actual_attempts = {shard: int(attempt) for shard in range(workers)}
    if set(actual_attempts) != set(range(workers)):
        raise ValueError('physical worker attempt inventory incomplete')
    shards, tests, metrics = set(), {}, []
    inventory = None
    for row in rows:
        shard = row.get('shard')
        if (row.get('schema') != 1 or row.get('kind') != 'worker-timing'
                or row.get('success') is not True or row.get('source') != topology['source']
                or str(row.get('run_id')) != str(run_id)
                or str(row.get('run_attempt')) != str(actual_attempts.get(shard))
                or row.get('observation_kind') != topology['observation_kind']
                or type(shard) is not int or shard not in range(workers) or shard in shards):
            raise ValueError('duplicate or mismatched physical worker receipt')
        shards.add(shard)
        if not isinstance(row.get('phases'), list) or not row['phases']:
            raise ValueError('required worker phase evidence missing')
        phase_names = set()
        for phase in row['phases']:
            if not isinstance(phase.get('phase'), str) or phase['phase'] in phase_names:
                raise ValueError('duplicate or invalid measured phase')
            phase_names.add(phase['phase'])
            number(phase['seconds'])
        if inventory is None:
            inventory = row['compiled_inventory_sha256']
        if row['compiled_inventory_sha256'] != inventory or not re.fullmatch('[0-9a-f]{64}', inventory):
            raise ValueError('worker compiled inventories differ')
        cohort = row['cohort']
        if (not isinstance(cohort.get('cache_state'), str) or not cohort['cache_state']
                or not isinstance(cohort.get('runner_image'), str) or not cohort['runner_image']
                or not isinstance(cohort.get('compiler'), dict) or not cohort['compiler']
                or not re.fullmatch('[0-9a-f]{64}', row['configuration_sha256'])):
            raise ValueError('worker cohort evidence invalid')
        key = normalize_ci_timing.digest({'configuration_sha256': row['configuration_sha256'], 'cohort': cohort})
        local_tests = set()
        for test in row['tests']:
            ident = test['test_id']
            if (not IDENT.fullmatch(ident) or ident in tests or test['status'] not in ('pass', 'skip')
                    or test['timing_issues'] or test['own_elapsed_seconds'] is None):
                raise ValueError('invalid, duplicate or incomplete parent timing')
            number(test['parent_wall_seconds'])
            number(test['own_elapsed_seconds'])
            tests[ident] = (test, key, cohort)
            local_tests.add(ident)
        for metric in row['fixture_metrics']:
            if (metric['test_id'] not in local_tests or metric['additive'] is not False
                    or not isinstance(metric['operations'], dict)):
                raise ValueError('invalid fixture metric attribution')
            number(metric['seconds'])
            for count in metric['operations'].values():
                if type(count) is not int or count < 0:
                    raise ValueError('invalid fixture operation count')
            metrics.append(metric)
    if not tests:
        raise ValueError('missing individual parent timings')
    return tests, metrics


def worker_attempts(jobs, topology, run_id, attempt):
    result = {}
    for shard in range(topology['workers']):
        selected = [job for job in jobs if job.get('name') == f'linux-short ({shard})']
        if len(selected) != 1:
            raise ValueError('missing or duplicate API worker job')
        job = selected[0]
        actual = job.get('run_attempt')
        if (job.get('run_id') != int(run_id) or job.get('head_sha') != topology['head']
                or job.get('status') != 'completed' or job.get('conclusion') != 'success'
                or type(actual) is not int or not 1 <= actual <= int(attempt)):
            raise ValueError('API worker allocation identity or attempt invalid')
        result[shard] = actual
    return result


def current_worker_attempts(api, topology, run_id, attempt):
    path = f'/repos/{collect_ci_timing.REPOSITORY}/actions/runs/{run_id}/attempts/{attempt}'
    run = api.get(path)
    if run.get('id') != int(run_id) or run.get('run_attempt') != int(attempt):
        raise ValueError('API enclosing workflow attempt mismatch')
    collect_ci_timing.validate_run(run, 'Test', topology)
    jobs = api.pages(path + '/jobs', 'jobs')['jobs']
    return worker_attempts(jobs, topology, run_id, attempt)


def verify_raw_coverage(paths, topology):
    """Recheck complete compiled assignments before preparing an admission."""
    import go_parent_shards
    import run_go_shard
    first = None
    shards = set()
    for path in paths:
        manifest = read(path)
        for raw in path.parent.glob('*.json'):
            if raw.is_symlink() or raw.stat().st_size > normalize_ci_timing.MAX_LOG_BYTES:
                raise ValueError('raw compiled evidence exceeds bound or is a symlink')
        catalog = manifest['catalog']
        shard = manifest['shard']
        try:
            go_parent_shards.validate(catalog, topology['workers'])
        except run_go_shard.Failure as error:
            raise ValueError('raw compiled catalog invalid: ' + str(error)) from error
        if (type(shard) is not int or shard not in range(topology['workers']) or shard in shards
                or catalog['source_proof'] != {'source': topology['source'], 'tree': topology['tree']}
                or manifest.get('success') is not True
                or manifest.get('catalog_sha256') != run_go_shard.digest(catalog)
                or manifest.get('coverage') != catalog['assignments'][shard]):
            raise ValueError('raw compiled assignment/source evidence invalid')
        if first is not None and catalog != first:
            raise ValueError('raw compiled catalogs differ')
        first = catalog
        shards.add(shard)
        try:
            go_parent_shards.verify_logs(path.parent, catalog, catalog['assignments'][shard])
        except run_go_shard.Failure as error:
            raise ValueError('raw compiled coverage invalid: ' + str(error)) from error
    if shards != set(range(topology['workers'])):
        raise ValueError('raw compiled worker coverage incomplete')


def normalize_workers(paths, topology, *, verify_coverage=False):
    if verify_coverage:
        verify_raw_coverage(paths, topology)
    rows = []
    for path in paths:
        if path.is_symlink() or path.stat().st_size > MAX_BYTES:
            raise ValueError('invalid raw worker manifest')
        read(path)  # Reject duplicate JSON keys before the reporting parser reads it.
        row = normalize_ci_timing.worker(path.parent, topology)
        by_log = {}
        for test in row['tests']:
            if test['log'] not in by_log:
                groups = {}
                events = normalize_ci_timing.run_go_shard.json_stream((path.parent / test['log']).read_text())
                for event in events:
                    name = event.get('Test', '')
                    if '/' not in name:
                        continue
                    key = event['Package'] + '::' + name.split('/')[0]
                    group = groups.setdefault(key, {'maximum': 0, 'paused': set(), 'terminal': {}})
                    if event.get('Action') == 'pause':
                        group['paused'].add(name)
                    if event.get('Action') in ('pass', 'skip'):
                        value = number(event['Elapsed'])
                        if name in group['terminal']:
                            raise ValueError('duplicate descendant terminal timing')
                        group['terminal'][name] = value
                        group['maximum'] = max(group['maximum'], value)
                by_log[test['log']] = groups
            group = by_log[test['log']].get(test['test_id'], {'maximum': 0, 'paused': set(), 'terminal': {}})
            test['slowest_descendant_seconds'] = group['maximum']
            test['parallel_descendants'] = len(group['paused'])
            if not group['paused'] <= group['terminal'].keys():
                raise ValueError('parallel descendant missing terminal')
            # Nonadditive review indicator: nested parallel terminal Elapsed may overlap.
            test['parallel_elapsed_work_seconds'] = sum(group['terminal'][name] for name in group['paused'])
        rows.append(row)
    return rows


def approval(api, topology, maintainers):
    path = f'/repos/{collect_ci_timing.REPOSITORY}/pulls/{topology["pr_number"]}'
    live = api.get(path)
    if live.get('head', {}).get('sha') != topology['head'] or live.get('state') != 'open':
        raise ValueError('live PR differs from current approval revision')
    author = live['user']['login'].casefold()
    allowed = {v.casefold() for v in maintainers} - {author}
    latest = {}
    for page in range(1, 6):
        rows = api.get(path + f'/reviews?per_page=100&page={page}')
        if not isinstance(rows, list) or len(rows) > 100:
            raise ValueError('invalid review API response')
        for row in rows:
            login = row.get('user', {}).get('login', '').casefold()
            if login in allowed and row.get('state') != 'PENDING':
                if type(row.get('id')) is not int or not isinstance(row.get('submitted_at'), str):
                    raise ValueError('invalid submitted review identity')
                submitted = datetime.fromisoformat(row['submitted_at'].replace('Z', '+00:00'))
                if submitted.tzinfo is None:
                    raise ValueError('submitted review timestamp lacks timezone')
                order = (submitted, row['id'])
                if login not in latest or order > latest[login][0]:
                    latest[login] = (order, row)
        if len(rows) < 100:
            break
    else:
        raise ValueError('review inventory exceeds bounded page limit')
    for login, (_, row) in sorted(latest.items()):
        if (row.get('state') == 'APPROVED' and row.get('commit_id') == topology['head']
                and row.get('user', {}).get('type') == 'User'):
            return {'review_id': row['id'], 'reviewer': login, 'head': topology['head']}
    raise ValueError('performance exception requires designated non-author maintainer approval at current HEAD')


def exceptions_check(document, today):
    if document.get('schema') != 1 or not isinstance(document.get('exceptions'), list):
        raise ValueError('invalid performance exceptions')
    rows, selectors = [], set()
    for row in document['exceptions']:
        if set(row) - {'test_id', 'family', 'maximum_seconds', 'maximum_parallel_descendants', 'maximum_parallel_elapsed_work_seconds', 'reason', 'owner', 'expires'}:
            raise ValueError('unknown exception field')
        selector = tuple((key, row[key]) for key in ('test_id', 'family') if key in row)
        if len(selector) != 1 or selector in selectors:
            raise ValueError('duplicate or ambiguous performance exception')
        kind, value = selector[0]
        if (kind == 'test_id' and not IDENT.fullmatch(value)) or (kind == 'family' and not re.fullmatch(r'github\.com/wangjohn/agent-archive/[^:]+', value)):
            raise ValueError('invalid exception selector')
        selectors.add(selector)
        number(row['maximum_seconds'])
        if 'maximum_parallel_elapsed_work_seconds' in row:
            number(row['maximum_parallel_elapsed_work_seconds'])
        if 'maximum_parallel_descendants' in row and (type(row['maximum_parallel_descendants']) is not int or row['maximum_parallel_descendants'] < 0):
            raise ValueError('invalid parallel family exception budget')
        if any(not isinstance(row.get(key), str) or not row[key].strip() for key in ('reason', 'owner')):
            raise ValueError('exception reason and owner required')
        if not isinstance(row.get('expires'), str):
            raise ValueError('performance exception expiry required')
        expiry = datetime.strptime(row['expires'], '%Y-%m-%d').date()
        if expiry < today:
            raise ValueError('expired performance exception')
        rows.append(row)
    return rows


def evaluate(policy, rows, topology, run_id, attempt, exceptions, api=None, today=None, actual_attempts=None):
    policy_check(policy)
    tests, metrics = observations(rows, topology, run_id, attempt, actual_attempts)
    overrides = exceptions_check(exceptions, today or datetime.now(timezone.utc).date())
    known = set(policy['baseline']['inventory'])
    groups = {group['key']: group for group in policy['baseline']['groups']}
    measured = {ident for group in groups.values() for ident in group['tests']}
    warnings, failures, decisions = [], [], []
    authorization = None
    for ident, (test, key, cohort) in sorted(tests.items()):
        elapsed = max(test['own_elapsed_seconds'], number(test['slowest_descendant_seconds']))
        parallel = test['parallel_descendants']
        elapsed_work = number(test['parallel_elapsed_work_seconds'])
        if type(parallel) is not int or parallel < 0:
            raise ValueError('invalid parallel-descendant inventory')
        budget = None
        reason = None
        new_cost = ident not in known or (ident not in measured and test['status'] == 'pass')
        family_budget = policy['parallel_budgets'].get(ident)
        if family_budget is None and new_cost:
            family_budget = {'maximum_descendants': policy['new_parallel_descendants'],
                             'maximum_elapsed_work_seconds': policy['new_parallel_elapsed_work_seconds']}
        parallel_exceeded = family_budget is not None and (
            parallel > family_budget['maximum_descendants']
            or elapsed_work > family_budget['maximum_elapsed_work_seconds'])
        if parallel_exceeded:
            budget, reason = policy['new_failure_seconds'], 'parallel-family-budget'
        if new_cost:
            if test['parent_wall_seconds'] > policy['new_failure_seconds']:
                warnings.append({'test_id': ident, 'reason': 'parent-lifecycle-wall-not-isolated-cost', 'seconds': test['parent_wall_seconds']})
            if parallel_exceeded:
                reason = 'new-parallel-family-requires-explicit-budget'
            if elapsed > policy['new_warning_seconds']:
                warnings.append({'test_id': ident, 'seconds': elapsed, 'reason': 'new-test-warning'})
            if elapsed > policy['new_failure_seconds']:
                budget, reason = policy['new_failure_seconds'], 'new-test-budget'
        else:
            baseline = groups.get(key, {}).get('tests', {}).get(ident)
            if (baseline is None or baseline['observations'] < policy['minimum_observations']
                    or 'unknown' in (cohort['cache_state'], cohort['runner_image'])):
                warnings.append({'test_id': ident, 'reason': 'insufficient-comparable-baseline'})
            else:
                prior = baseline['own_elapsed_median_seconds']
                if elapsed > prior * policy['regression_factor'] and elapsed > prior + policy['regression_seconds']:
                    budget, reason = max(prior * policy['regression_factor'], prior + policy['regression_seconds']), 'existing-test-regression'
        if budget is None:
            continue
        matches = [entry for entry in overrides if entry.get('test_id') == ident
                   or entry.get('family') == ident.split('::')[0]]
        if len(matches) > 1:
            raise ValueError('overlapping test and family exceptions')
        if (matches and elapsed <= matches[0]['maximum_seconds']
                and (not parallel_exceeded
                     or (parallel <= matches[0].get('maximum_parallel_descendants', 0)
                         and elapsed_work <= matches[0].get('maximum_parallel_elapsed_work_seconds', 0)))):
            if authorization is None:
                if api is None:
                    raise ValueError('exception approval API unavailable')
                authorization = approval(api, topology, policy['maintainers'])
            decisions.append({'test_id': ident, 'exception': matches[0], 'approval': authorization})
        else:
            failures.append({'test_id': ident, 'seconds': elapsed, 'budget_seconds': budget, 'reason': reason,
                             'parallel_descendants': parallel, 'parallel_elapsed_work_seconds': elapsed_work})
    for budget in policy['work_budgets']:
        if budget['test_id'] not in tests:
            raise ValueError('required deterministic fixture parent missing')
        matched = [row for row in metrics if all(row[key] == budget[key] for key in ('test_id', 'fixture', 'phase'))]
        # Metrics may nest: only sum repeated observations of this exact phase,
        # never add a constructor's child phase to its parent phase.
        if not matched or any(budget['operation'] not in row['operations'] for row in matched):
            raise ValueError('required deterministic fixture metric missing')
        count = sum(row['operations'][budget['operation']] for row in matched)
        if count > budget['maximum']:
            failures.append({'test_id': budget['test_id'], 'reason': 'deterministic-work-budget',
                             'operation': budget['operation'], 'count': count, 'maximum': budget['maximum']})
    return {'schema': 1, 'source': topology['source'], 'head': topology['head'],
            'policy_sha256': normalize_ci_timing.digest(policy),
            'trusted_base': os.environ.get('CI_PERFORMANCE_TRUSTED_BASE'),
            'run_id': str(run_id), 'run_attempt': str(attempt), 'parents_measured': len(tests),
            'passed': not failures, 'failures': failures, 'warnings': warnings, 'exceptions': decisions}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--policy', type=Path, required=True)
    parser.add_argument('--exceptions', type=Path, required=True)
    parser.add_argument('--topology', type=Path, required=True)
    parser.add_argument('--workers', type=Path, required=True)
    parser.add_argument('--output', type=Path, required=True)
    args = parser.parse_args()
    try:
        topology = read(args.topology)
        paths = sorted(args.workers.glob('*/manifest.json'))
        if len(paths) != topology['workers']:
            raise ValueError('missing or excess raw worker manifests')
        rows = normalize_workers(paths, topology, verify_coverage=True)
        api = collect_ci_timing.API(os.environ.get('GH_TOKEN', ''), seconds=20, request_limit=12)
        run_id, attempt = os.environ['GITHUB_RUN_ID'], os.environ['GITHUB_RUN_ATTEMPT']
        actual = current_worker_attempts(api, topology, run_id, attempt)
        result = evaluate(read(args.policy), rows, topology, run_id, attempt, read(args.exceptions),
                          api, actual_attempts=actual)
        args.output.write_text(json.dumps(result, indent=2) + '\n')
        print(json.dumps({'passed': result['passed'], 'parents_measured': result['parents_measured'],
                          'failures': result['failures'], 'warning_count': len(result['warnings']),
                          'exceptions': result['exceptions']}, sort_keys=True))
        if not result['passed']:
            parser.exit(1, 'Performance budget exceeded; optimize or obtain a current-HEAD approved exception.\n')
    except (ValueError, KeyError, TypeError, OSError) as error:
        args.output.write_text(json.dumps({'schema': 1, 'passed': False, 'validation_error': str(error)}) + '\n')
        parser.exit(1, 'Performance gate failed closed: ' + str(error) + '\n')


if __name__ == '__main__':
    main()
