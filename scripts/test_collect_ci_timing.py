"""Read-only bounded collection cannot mispair same-head qualification PRs."""
import copy
import io
import json
import unittest
from unittest import mock
import urllib.error

import collect_ci_timing as collector
from test_ci_worker_topology import receipt


def run(name, number=12, ident=None):
    return {'id': ident or (1 if name == 'Test' else 2), 'name': name,
            'path': collector.PATHS[name], 'event': 'pull_request', 'head_sha': 'c'*40,
            'head_branch': 'ordinary', 'head_repository': {'full_name': collector.REPOSITORY}, 'repository': {'full_name': collector.REPOSITORY},
            'run_attempt': 1, 'status': 'completed', 'conclusion': 'success',
            'created_at': '2026-10-15T00:00:01Z',
            'pull_requests': [{'number': number, 'head': {'sha': 'c'*40}, 'base': {'sha': 'b'*40}}]}


def jobs(name, ident):
    return [{'id': ident*100+i, 'run_id': ident, 'head_sha': 'c'*40,
             'name': job, 'status': 'completed', 'conclusion': 'success', 'run_attempt': 1,
             'runner_id': ident*100+i, 'runner_name': 'host', 'labels': ['ubuntu-latest'],
             'started_at': '2026-10-15T00:00:05Z', 'completed_at': '2026-10-15T00:01:05Z',
             'steps': [{'name': 'body', 'status': 'completed', 'conclusion': 'success',
                        'started_at': '2026-10-15T00:00:05Z', 'completed_at': '2026-10-15T00:01:05Z'}]}
            for i, job in enumerate(collector.inventory(8)[name])]


class FakeAPI:
    def __init__(self):
        self.runs = {1: run('Test'), 2: run('Levenshtein')}
        self.jobs = {1: jobs('Test', 1), 2: jobs('Levenshtein', 2)}
        self.peers = [self.runs[2]]

    def get(self, path):
        if '/attempts/' in path:
            ident = int(path.split('/runs/')[1].split('/')[0])
            value = copy.deepcopy(self.runs[ident])
            value['run_attempt'] = int(path.rsplit('/', 1)[1])
            return value
        return copy.deepcopy(self.runs[int(path.rsplit('/', 1)[1])])

    def pages(self, path, key):
        if key == 'workflow_runs':
            return {'total_count': len(self.peers), key: copy.deepcopy(self.peers)}
        ident = int(path.split('/runs/')[1].split('/')[0])
        rows = copy.deepcopy(self.jobs[ident])
        return {'total_count': len(rows), key: rows}


class Response(io.BytesIO):
    def __init__(self, value, url):
        super().__init__(json.dumps(value).encode())
        self.url = url

    def geturl(self):
        return self.url


class CollectionTests(unittest.TestCase):
    def test_all_prior_attempts_retained_and_carried_forward_allocations_dedup(self):
        api = FakeAPI()
        api.runs[1]['run_attempt'] = 2
        result = collector.collect(api, receipt(), 1, 2)
        self.assertEqual(len(result['input']['samples']), 2)
        self.assertEqual(result['input']['samples'][1]['scope'], 'auxiliary-campaign')
        resources = result['report']['resources_across_snapshots']
        self.assertEqual(resources['physical_job_rows'], 26)
        self.assertEqual(resources['linux_runner_minutes'], 26)

    def test_full_fixed_inventory_and_complete_report(self):
        self.assertEqual(sum(map(len, collector.inventory(8).values())), 26)
        self.assertEqual(sum(map(len, collector.inventory(4).values())), 22)
        value = collector.collect(FakeAPI(), receipt(), 1, 2, cohort='source-only', cache_state='mixed',
                                  event_at='2026-10-15T00:00:00Z')
        self.assertEqual(value['snapshot_status'], 'terminal')
        sample = value['report']['samples'][0]
        self.assertTrue(sample['qualifying'])
        self.assertEqual(sample['linux_runner_minutes'], 26)
        self.assertEqual(sample['tested_checkout']['source'], 'a'*40)

    def test_wrong_pr_same_head_branch_path_source_or_base_rejected(self):
        for field, value in [('head_sha', 'z'*40), ('head_branch', 'other'),
                             ('path', '.github/workflows/other.yml'), ('event', 'workflow_dispatch')]:
            api = FakeAPI()
            api.runs[2][field] = value
            with self.subTest(field=field), self.assertRaises(ValueError):
                collector.collect(api, receipt(), 1, 2)
        for field, value in [('number', 13), ('head', {'sha': 'e'*40}), ('base', {'sha': 'e'*40})]:
            api = FakeAPI()
            api.runs[2]['pull_requests'][0][field] = value
            with self.assertRaises(ValueError):
                collector.collect(api, receipt(), 1, 2)
        api = FakeAPI()
        api.runs[2]['pull_requests'] = []
        with self.assertRaises(ValueError):
            collector.collect(api, receipt(), 1, 2)

    def test_live_same_head_other_pr_is_not_paired_and_ambiguous_run_fails(self):
        api = FakeAPI()
        api.peers = [run('Levenshtein', number=13, ident=3)]
        result = collector.collect(api, receipt(), 1, live=True)
        self.assertEqual(len(result['input']['samples'][0]['workflows']), 1)
        self.assertIsNone(result['report']['samples'][0]['linux_runner_minutes'])
        api.peers = [run('Levenshtein'), run('Levenshtein', ident=3)]
        with self.assertRaisesRegex(ValueError, 'ambiguous'):
            collector.collect(api, receipt(), 1, live=True)

    def test_live_current_job_end_and_peer_pending_remain_unknown(self):
        api = FakeAPI()
        own = next(job for job in api.jobs[1] if job['name'] == 'linux-race')
        own.update(status='in_progress', conclusion=None, completed_at=None)
        api.runs[1].update(status='in_progress', conclusion=None)
        api.runs[2].update(status='in_progress', conclusion=None)
        result = collector.collect(api, receipt(), 1, 2, live=True, cache_state='mixed',
                                   event_at='2026-10-15T00:00:00Z')
        sample = result['report']['samples'][0]
        self.assertEqual(result['snapshot_status'], 'partial')
        for key in ('actual_runner_minutes', 'linux_runner_minutes', 'required_completed_at',
                    'event_to_required_completion_seconds', 'run_created_lower_bound_seconds'):
            self.assertIsNone(sample[key], key)
        self.assertFalse(sample['qualifying'])
        self.assertIsNone(result['report']['summaries'][0]['provisional_p95_seconds'])

    def test_missing_required_job_not_disappeared_from_inventory(self):
        api = FakeAPI()
        api.jobs[1].pop()
        result = collector.collect(api, receipt(), 1, 2)
        sample = result['report']['samples'][0]
        self.assertIsNone(sample['linux_runner_minutes'])
        self.assertFalse(sample['qualifying'])

    def test_twenty_experiments_are_excluded_from_normal_percentiles(self):
        import report_ci_timing
        result = collector.collect(FakeAPI(), receipt(), 1, 2, cohort='source-only', cache_state='mixed',
                                   event_at='2026-10-15T00:00:00Z')
        base = result['input']['samples'][0]
        observations = []
        for i in range(20):
            value = copy.deepcopy(base)
            value['id'] = str(i)
            value['observation_kind'] = 'full-pipeline-experiment'
            for snap in value['workflows']:
                snap['run']['id'] += 10*i
                for job in snap['jobs']['jobs']:
                    job['id'] += 1000*i
                    job['run_id'] += 10*i
            observations.append(value)
        report = report_ci_timing.report({'schema': 1, 'samples': observations})
        self.assertEqual(report['summaries'][0]['qualifying_unique_runs'], 0)
        self.assertIsNone(report['summaries'][0]['provisional_p95_seconds'])

    def test_network_fixed_origin_get_no_token_diagnostics_or_redirect(self):
        captured = []
        def opener(request, timeout):
            captured.append((request, timeout))
            return Response({'ok': True}, request.full_url)
        api = collector.API('test-secret', opener=opener)
        self.assertEqual(api.get('/repos/wangjohn/agent-archive/actions/runs/1'), {'ok': True})
        request, timeout = captured[0]
        self.assertEqual(request.get_method(), 'GET')
        self.assertEqual(request.full_url, collector.ORIGIN + '/repos/wangjohn/agent-archive/actions/runs/1')
        self.assertGreater(timeout, 0)
        self.assertLessEqual(timeout, 4)
        for path in ('https://evil.example/repos/wangjohn/agent-archive/', '//evil.example/x',
                     '/repos/other/repo/actions/runs', '/repos/wangjohn/agent-archive/../x',
                     '/repos/wangjohn/agent-archive/x\\y', '/repos/wangjohn/agent-archive/x\n'):
            with self.assertRaises(ValueError):
                api.get(path)
        with self.assertRaisesRegex(ValueError, 'redirect'):
            collector.NoRedirect().redirect_request(None, None, 302, '', {}, 'https://evil.example')
        def denied(request, timeout):
            raise urllib.error.HTTPError(request.full_url, 403, 'test-secret', {}, None)
        with self.assertRaises(ValueError) as caught:
            collector.API('test-secret', opener=denied).get('/repos/wangjohn/agent-archive/actions/runs/1')
        self.assertNotIn('test-secret', str(caught.exception))

    def test_request_deadline_and_pagination_completeness(self):
        api = collector.API('test-secret', request_limit=0)
        with self.assertRaisesRegex(ValueError, 'budget'):
            api.get('/repos/wangjohn/agent-archive/actions/runs/1')
        api = collector.API('test-secret', seconds=-1)
        with self.assertRaisesRegex(ValueError, 'budget'):
            api.get('/repos/wangjohn/agent-archive/actions/runs/1')
        api = collector.API('test-secret')
        api.get = mock.Mock(side_effect=[{'total_count': 101, 'jobs': [{'id': i} for i in range(100)]},
                                         {'total_count': 101, 'jobs': [{'id': 100}]}])
        self.assertEqual(len(api.pages('/repos/wangjohn/agent-archive/actions/runs/1/jobs', 'jobs')['jobs']), 101)
        api.get = mock.Mock(return_value={'total_count': 2, 'jobs': []})
        with self.assertRaisesRegex(ValueError, 'incomplete'):
            api.pages('/repos/wangjohn/agent-archive/actions/runs/1/jobs', 'jobs')
        api.get = mock.Mock(return_value={'total_count': 2, 'jobs': [{'id': 1}, {'id': 1}]})
        with self.assertRaisesRegex(ValueError, 'duplicate'):
            api.pages('/repos/wangjohn/agent-archive/actions/runs/1/jobs', 'jobs')
        api.get = mock.Mock(side_effect=[{'total_count': 101, 'jobs': [{'id': i} for i in range(100)]},
                                         {'total_count': 102, 'jobs': [{'id': 100}]}])
        with self.assertRaisesRegex(ValueError, 'changed'):
            api.pages('/repos/wangjohn/agent-archive/actions/runs/1/jobs', 'jobs')


if __name__ == '__main__':
    unittest.main()
