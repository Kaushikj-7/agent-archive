#!/usr/bin/env python3
"""Bounded read-only Actions snapshots for the existing offline timing reporter."""
import argparse
from datetime import datetime, timezone
import json
import os
from pathlib import Path
import re
import time
import urllib.error
import urllib.parse
import urllib.request

import ci_worker_topology
import report_ci_timing


REPOSITORY = 'wangjohn/agent-archive'
ORIGIN = 'https://api.github.com'
PATHS = {'Test': '.github/workflows/test.yml', 'Levenshtein': '.github/workflows/levenshtein.yml'}


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        raise ValueError('API redirects are not permitted')


class API:
    def __init__(self, token, seconds=20, request_limit=30, opener=None):
        self.token = token
        self.deadline = time.monotonic() + seconds
        self.remaining = request_limit
        self.opener = opener or urllib.request.build_opener(NoRedirect()).open

    def get(self, path):
        parsed = urllib.parse.urlsplit(path)
        if (parsed.scheme or parsed.netloc or parsed.fragment or not path.startswith('/repos/' + REPOSITORY + '/')
                or '..' in parsed.path.split('/') or '\\' in path or any(ord(c) < 32 for c in path)):
            raise ValueError('API path outside allowed repository')
        remaining = self.deadline - time.monotonic()
        if self.remaining <= 0 or remaining <= 0:
            raise ValueError('diagnostic request budget exhausted')
        self.remaining -= 1
        request = urllib.request.Request(ORIGIN + path, headers={
            'Authorization': 'Bearer ' + self.token,
            'Accept': 'application/vnd.github+json', 'X-GitHub-Api-Version': '2022-11-28',
            'User-Agent': 'agent-archive-ci-timing'}, method='GET')
        try:
            with self.opener(request, timeout=min(4, remaining)) as response:
                if response.geturl().split('?', 1)[0] != (ORIGIN + path).split('?', 1)[0]:
                    raise ValueError('unexpected API response URL')
                data = response.read(4 * 1024 * 1024 + 1)
                if len(data) > 4 * 1024 * 1024:
                    raise ValueError('API response exceeds diagnostic limit')
                return json.loads(data)
        except urllib.error.HTTPError as error:
            # Never include response bodies, request headers, or token in diagnostics.
            raise ValueError('Actions API HTTP status ' + str(error.code)) from None
        except urllib.error.URLError:
            raise ValueError('Actions API unavailable') from None

    def pages(self, path, key):
        found, expected = [], None
        for page in range(1, 11):
            result = self.get(path + ('&' if '?' in path else '?') + f'per_page=100&page={page}')
            count = result.get('total_count')
            if type(count) is not int or count < 0 or not isinstance(result.get(key), list):
                raise ValueError('invalid paginated API inventory')
            if expected is None:
                expected = count
            if count != expected:
                raise ValueError('API inventory changed during pagination')
            rows = result[key]
            if len(rows) > 100:
                raise ValueError('oversized API page')
            found.extend(rows)
            if len(found) == expected:
                ids = [row['id'] for row in found]
                if len(set(ids)) != len(ids):
                    raise ValueError('duplicate paginated API identity')
                return {'total_count': expected, key: found}
            if not rows or len(found) > expected:
                raise ValueError('incomplete paginated API inventory')
        raise ValueError('API pagination budget exhausted')


def inventory(workers):
    if workers not in (4, 8):
        raise ValueError('unsupported worker count')
    return {'Test': ['source-audit', *[f'linux-short ({i})' for i in range(workers)],
                     'linux-full', 'script-static', 'script-install', 'script-purge (0)',
                     'script-purge (1)', 'lint', 'cross-linux-arm64',
                     'cross-darwin-nocgo', 'cross-linux-release', 'cross-build', 'macos-smoke',
                     'linux-race', 'Git identity (all versions)'],
            'Levenshtein': ['verify', 'native-release-vulnerabilities']}


def validate_run(run, name, topology):
    associations = run.get('pull_requests')
    if (run.get('name') != name or run.get('path') != PATHS[name]
            or run.get('event') != 'pull_request'
            or run.get('repository', {}).get('full_name', '').casefold() != REPOSITORY.casefold()
            or run.get('head_repository', {}).get('full_name', '').casefold() != topology['head_repository'].casefold()
            or run.get('head_sha') != topology['head'] or run.get('head_branch') != topology['head_ref']
            or not isinstance(associations, list) or len(associations) != 1
            or associations[0].get('number') != topology['pr_number']
            or associations[0].get('head', {}).get('sha') != topology['head']
            or associations[0].get('base', {}).get('sha') != topology['event_base']
            or type(run.get('id')) is not int or run['id'] <= 0
            or type(run.get('run_attempt')) is not int or not 1 <= run['run_attempt'] <= 10):
        raise ValueError('run does not belong to exact PR/workflow/revision context')


def snapshot(api, run, attempt=None):
    prefix = f'/repos/{REPOSITORY}/actions/runs/{run["id"]}'
    number = attempt or run['run_attempt']
    if number != run['run_attempt']:
        run = api.get(prefix + f'/attempts/{number}')
    jobs = api.pages(prefix + f'/attempts/{number}/jobs', 'jobs')
    return {'run': run, 'jobs': jobs}


def collect(api, topology, test_run_id, leven_run_id=None, *, live=False,
            cohort='unknown', cache_state='unknown', event_at=None, experiment=False):
    required = inventory(topology['workers'])
    runs = [api.get(f'/repos/{REPOSITORY}/actions/runs/{test_run_id}')]
    validate_run(runs[0], 'Test', topology)
    if leven_run_id:
        peer = api.get(f'/repos/{REPOSITORY}/actions/runs/{leven_run_id}')
        validate_run(peer, 'Levenshtein', topology)
        runs.append(peer)
    elif live:
        peers = api.pages(f'/repos/{REPOSITORY}/actions/workflows/levenshtein.yml/runs?event=pull_request&head_sha={topology["head"]}', 'workflow_runs')['workflow_runs']
        matches = []
        for peer in peers:
            try:
                validate_run(peer, 'Levenshtein', topology)
            except ValueError:
                continue
            matches.append(peer)
        if len(matches) > 1:
            raise ValueError('ambiguous paired Levenshtein runs; finalize with explicit run IDs')
        runs.extend(matches)
    else:
        raise ValueError('offline finalization requires explicit Test and Levenshtein run IDs')
    snapshots = [snapshot(api, run) for run in runs]
    partial = live or len(runs) != 2 or any(run['status'] != 'completed' for run in runs)
    sample = {'id': f'pr-{topology["pr_number"]}-test-{test_run_id}',
              'source': topology['head'], 'worker_count': topology['workers'],
              'cohort': cohort, 'cache_state': cache_state,
              'comparable_group': f'throughput-v1-{topology["workers"]}-workers',
              'event_at': event_at, 'required_jobs': required, 'workflows': snapshots,
              'observation_kind': 'full-pipeline-experiment' if experiment else topology['observation_kind'],
              'tested_checkout': topology}
    samples = [sample]
    if not live:
        for run in runs:
            for attempt in range(1, run['run_attempt']):
                old = snapshot(api, run, attempt)
                validate_run(old['run'], run['name'], topology)
                samples.append(dict(sample, id=f'usage-{run["id"]}-attempt-{attempt}',
                                    scope='auxiliary-campaign', required_jobs={run['name']: required[run['name']]},
                                    workflows=[old], event_at=None))
    document = {'schema': 1, 'samples': samples}
    result = report_ci_timing.report(document)
    if partial:
        for row in result['samples']:
            row['required_completed_at'] = None
            row['event_to_required_completion_seconds'] = None
            row['run_created_lower_bound_seconds'] = None
            row['qualifying'] = False
            row['issues'].append('live snapshot includes unfinished current collector job; final totals unavailable'
                                 if live else 'paired workflows not terminal; final totals unavailable')
        for summary in result['summaries']:
            summary.update(qualifying_unique_runs=0, selected_sample_ids=[],
                           median_event_to_required_completion_seconds=None, provisional_p95_seconds=None,
                           target_150s_met=None, directional_sample_ready=False)
    return {'schema': 1, 'collected_at': datetime.now(timezone.utc).isoformat(), 'snapshot_status': 'partial' if partial else 'terminal',
            'collection_mode': 'live' if live else 'offline', 'topology': topology,
            'input': document, 'report': result,
            'notes': [('Current collector allocation cannot be finalized while it is running.' if live
                       else 'Offline collector is outside measured workflows; record its cost separately if hosted.'),
                      'API start delay is not pure capacity queue time; missing event timestamps remain unknown.']}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--topology', type=Path, required=True)
    parser.add_argument('--test-run', type=int)
    parser.add_argument('--levenshtein-run', type=int)
    parser.add_argument('--live', action='store_true')
    parser.add_argument('--experiment', action='store_true', help='Exclude intentional qualification traffic from normal p95')
    parser.add_argument('--cohort', choices=report_ci_timing.COHORTS, default='unknown')
    parser.add_argument('--cache-state', choices=('unknown', 'cold', 'mixed', 'warm'), default='unknown')
    parser.add_argument('--event-at', help='Retained initiating event telemetry, never commit/workflow-created time')
    parser.add_argument('--output', type=Path, required=True)
    args = parser.parse_args()
    try:
        topology = json.loads(args.topology.read_text())
        if (topology.get('repository') != REPOSITORY or topology.get('schema') != 1
                or topology.get('observation_kind') not in ('normal', 'full-pipeline-experiment')
                or any(not isinstance(topology.get(key), str) or not ci_worker_topology.SHA.fullmatch(topology[key])
                       for key in ('head', 'source', 'tree', 'event_base', 'tested_base'))
                or not isinstance(topology.get('parents'), list) or len(topology['parents']) != 2
                or topology['parents'][1] != topology['head']
                or topology['parents'][0] != topology['tested_base']
                or any(not isinstance(v, str) or not ci_worker_topology.SHA.fullmatch(v) for v in topology['parents'])
                or type(topology.get('pr_number')) is not int or topology['pr_number'] <= 0
                or not isinstance(topology.get('head_ref'), str)):
            raise ValueError('invalid retained topology receipt')
        expected_workers, expected_kind = ci_worker_topology.selection(
            {'pull_request': {'head': {'ref': topology['head_ref'],
                                       'repo': {'full_name': topology['head_repository']}}}}, REPOSITORY)
        if (topology['workers'] != expected_workers or topology['observation_kind'] != expected_kind
                or topology.get('workflow_source') != topology['source']
                or topology.get('workflow_ref') != REPOSITORY + '/.github/workflows/test.yml@refs/pull/' + str(topology['pr_number']) + '/merge'):
            raise ValueError('retained topology policy or workflow identity differs')
        test_id = args.test_run or (int(os.environ['GITHUB_RUN_ID']) if args.live else None)
        if not test_id or test_id <= 0 or (args.levenshtein_run is not None and args.levenshtein_run <= 0):
            raise ValueError('positive run IDs required')
        if args.live and (os.environ.get('GITHUB_EVENT_NAME') != 'pull_request'
                          or os.environ.get('GITHUB_REPOSITORY') != REPOSITORY
                          or os.environ.get('GITHUB_SHA') != topology['source']):
            raise ValueError('live diagnostics require current PR checkout identity')
        token = os.environ.get('GH_TOKEN', '')
        if not token:
            raise ValueError('read-only Actions token unavailable')
        result = collect(API(token), topology, test_id, args.levenshtein_run, live=args.live,
                         cohort=args.cohort, cache_state=args.cache_state, event_at=args.event_at, experiment=args.experiment)
        args.output.write_text(json.dumps(result, indent=2) + '\n')
        if args.live and os.environ.get('GITHUB_STEP_SUMMARY'):
            with Path(os.environ['GITHUB_STEP_SUMMARY']).open('a') as handle:
                row = result['report']['samples'][0]
                handle.write('### CI resource snapshot (partial)\n\n'
                             + 'Known Linux allocation subtotal: ' + str(round(row['known_linux_runner_minutes'], 3))
                             + ' runner-minutes; observed Linux overlap peak: '
                             + str(row['known_peak_simultaneous_linux_jobs']) + '.\n\n'
                             + 'Final collector allocation and possibly the paired workflow remain unknown. '
                             'This diagnostic does not certify latency, p95, cache warmth or queue time. '
                             'Finalize completed run IDs with collect_ci_timing.py.\n')
    except (ValueError, KeyError, TypeError, AttributeError, OSError) as error:
        # Exception messages from parser/network paths above never contain tokens.
        parser.exit(1, 'Timing collection unavailable: ' + str(error) + '\n')


if __name__ == '__main__':
    main()
