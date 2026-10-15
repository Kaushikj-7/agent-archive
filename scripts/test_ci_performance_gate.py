"""Adversarial performance admission, attribution, approval and budget contracts."""
import copy
from datetime import date
import json
from pathlib import Path
import tempfile
import unittest

import ci_performance_gate as gate
import admit_ci_performance_baseline as admission
import collect_ci_timing
from test_collect_ci_timing import FakeAPI
import normalize_ci_timing as norm
from test_ci_worker_topology import receipt

PREFIX = 'github.com/wangjohn/agent-archive/internal/example::'


def worker(index):
    return {'schema': 1, 'kind': 'worker-timing', 'shard': index, 'success': True,
            'source': 'a'*40, 'run_id': '12', 'run_attempt': '1', 'observation_kind': 'normal',
            'compiled_inventory_sha256': 'd'*64, 'configuration_sha256': 'e'*64,
            'cohort': {'cache_state': 'prefix', 'runner_image': 'image1', 'compiler': {'GOVERSION': 'go1.27.2'}},
            'phases': [{'phase': 'worker_total_seconds', 'seconds': 2}],
            'fixture_metrics': [], 'tests': [{'test_id': PREFIX + 'Test' + str(index),
                'own_elapsed_seconds': 1, 'parent_wall_seconds': 1,
                'status': 'pass', 'timing_issues': [], 'slowest_descendant_seconds': 0,
                'parallel_descendants': 0, 'parallel_elapsed_work_seconds': 0}]}


def policy():
    result = json.loads(Path(gate.__file__).with_name('ci_performance_policy.json').read_text())
    ids = [PREFIX + 'Test' + str(i) for i in range(8)]
    result['baseline'] = {'inventory': ids, 'groups': [],
        'metric': 'maximum-parent-or-descendant-terminal-elapsed', 'provenance': {
        'source': 'f'*40, 'tree': 'b'*40, 'run_id': 11, 'run_attempt': 1, 'workers': 8,
        'inventory_sha256': norm.digest(ids), 'pipeline_sha256': 'c'*64}}
    return result


class API:
    def __init__(self, reviews=None, author='author', head='c'*40):
        self.reviews = reviews or []
        self.author, self.head = author, head

    def get(self, path):
        if '/reviews?' in path:
            return self.reviews
        return {'state': 'open', 'head': {'sha': self.head}, 'user': {'login': self.author}}


def approved(**changes):
    result = {'id': 1, 'state': 'APPROVED', 'commit_id': 'c'*40,
              'submitted_at': '2026-10-15T00:00:00Z', 'user': {'login': 'wangjohn', 'type': 'User'}}
    result.update(changes)
    return result


class GateTests(unittest.TestCase):
    def setUp(self):
        self.policy, self.rows = policy(), [worker(i) for i in range(8)]
        self.exceptions = {'schema': 1, 'exceptions': []}

    def run_gate(self, api=None):
        return gate.evaluate(self.policy, self.rows, receipt(), '12', '1', self.exceptions, api,
                             today=date(2026, 10, 15))

    def new_test(self, **changes):
        test = self.rows[0]['tests'][0]
        test.update(test_id=PREFIX + 'TestNew', **changes)
        return test

    def exception(self, **changes):
        row = {'test_id': PREFIX + 'TestNew', 'maximum_seconds': 90,
               'reason': 'Required integration coverage', 'owner': 'wangjohn', 'expires': '2026-11-01'}
        row.update(changes)
        self.exceptions['exceptions'] = [row]

    def test_new_ordinary_package_test_cannot_hide_behind_package_unit(self):
        self.new_test(own_elapsed_seconds=84, parent_wall_seconds=84)
        result = self.run_gate()
        self.assertFalse(result['passed'])
        self.assertEqual(result['failures'][0]['reason'], 'new-test-budget')
        self.assertEqual(result['parents_measured'], 8)

    def test_warning_threshold_and_parallel_wait_attribution(self):
        self.new_test(own_elapsed_seconds=11, parent_wall_seconds=94)
        result = self.run_gate()
        self.assertTrue(result['passed'])
        self.assertIn('new-test-warning', [row['reason'] for row in result['warnings']])
        self.rows[0]['tests'][0]['own_elapsed_seconds'] = 0
        self.assertTrue(self.run_gate()['passed'])

    def test_slow_descendant_and_large_parallel_family_cannot_evade(self):
        test = self.new_test(slowest_descendant_seconds=40)
        self.assertFalse(self.run_gate()['passed'])
        test.update(slowest_descendant_seconds=.001, parallel_descendants=2, parallel_elapsed_work_seconds=.002)
        self.assertTrue(self.run_gate()['passed'])
        test['parallel_descendants'] = 101
        self.assertEqual(self.run_gate()['failures'][0]['reason'], 'new-parallel-family-requires-explicit-budget')
        test.update(parallel_descendants=31, parallel_elapsed_work_seconds=31)
        self.assertFalse(self.run_gate()['passed'])
        self.exception(maximum_parallel_descendants=40, maximum_parallel_elapsed_work_seconds=40)
        self.assertTrue(self.run_gate(API([approved()]))['passed'])

    def test_exception_requires_live_current_head_non_author_human_maintainer(self):
        self.new_test(own_elapsed_seconds=84)
        self.exception()
        self.assertTrue(self.run_gate(API([approved()]))['passed'])
        cases = [API([]), API([approved(commit_id='b'*40)]), API([approved(state='DISMISSED')]),
                 API([approved(user={'login': 'wangjohn', 'type': 'Bot'})]),
                 API([approved(user={'login': 'outsider', 'type': 'User'})]),
                 API([approved()], author='wangjohn'), API([approved()], head='f'*40),
                 API([approved(), approved(id=2, state='CHANGES_REQUESTED')])]
        for api in cases:
            with self.subTest(api=api.reviews), self.assertRaises(ValueError):
                self.run_gate(api)
        self.exceptions['exceptions'][0]['expires'] = '2026-10-14'
        with self.assertRaisesRegex(ValueError, 'expired'):
            self.run_gate(API([approved()]))

    def test_comparable_regression_requires_three_verified_baseline_observations(self):
        test = self.rows[0]['tests'][0]
        identity = {'configuration_sha256': 'e'*64, 'cohort': self.rows[0]['cohort']}
        group = {'key': norm.digest(identity), 'identity': identity, 'tests': {
            test['test_id']: {'observations': 2, 'own_elapsed_median_seconds': 20}}}
        self.policy['baseline']['groups'] = [group]
        test['own_elapsed_seconds'] = 31
        self.assertTrue(self.run_gate()['passed'])
        group['tests'][test['test_id']]['observations'] = 3
        self.assertFalse(self.run_gate()['passed'])
        test['own_elapsed_seconds'] = 30
        self.assertTrue(self.run_gate()['passed'])
        test['own_elapsed_seconds'] = 99
        self.rows[0]['cohort'] = dict(self.rows[0]['cohort'], runner_image='different')
        self.assertTrue(self.run_gate()['passed'])

    def test_fixture_budget_hard_failure_has_no_timing_override(self):
        ident = self.rows[0]['tests'][0]['test_id']
        self.policy['work_budgets'] = [{'test_id': ident, 'fixture': 'seed', 'phase': 'build',
                                       'operation': 'commits', 'maximum': 2}]
        with self.assertRaisesRegex(ValueError, 'missing'):
            self.run_gate()
        self.rows[0]['fixture_metrics'] = [{'test_id': ident, 'fixture': 'seed', 'phase': 'build',
            'operations': {'commits': 3}, 'seconds': 1, 'additive': False}]
        self.assertFalse(self.run_gate()['passed'])
        self.rows[0]['fixture_metrics'][0]['operations']['commits'] = 2
        self.assertTrue(self.run_gate()['passed'])

    def test_invalid_or_missing_evidence_fails_closed(self):
        modifications = [lambda: self.rows.pop(), lambda: self.rows.append(worker(0)),
            lambda: self.rows[0].update(source='f'*40), lambda: self.rows[0].update(run_attempt='2'),
            lambda: self.rows[0].update(shard=1), lambda: self.rows[0].update(success=False),
            lambda: self.rows[0]['tests'].append(copy.deepcopy(self.rows[0]['tests'][0])),
            lambda: self.rows[0]['tests'][0].update(own_elapsed_seconds=float('nan')),
            lambda: self.rows[0]['tests'][0].update(timing_issues=['missing terminal']),
            lambda: self.rows[0].update(phases=[]),
            lambda: self.rows[0]['phases'][0].update(seconds=None),
            lambda: self.policy['baseline'].update(inventory=[])]
        for modify in modifications:
            self.setUp()
            modify()
            with self.subTest(modify=modify), self.assertRaises(ValueError):
                self.run_gate()

    def test_baseline_admission_full_pipeline_deduplicates_without_inventing_samples(self):
        pipeline = collect_ci_timing.collect(FakeAPI(), receipt(), 1, 2)
        for row in self.rows:
            row['run_id'] = '1'
        result = admission.admit(self.policy, self.rows, receipt(), pipeline,
                                 [(self.rows, receipt(), pipeline)])
        self.assertEqual(result['baseline']['provenance']['run_id'], 1)
        self.assertEqual(len(result['baseline']['inventory']), 8)
        test = next(iter(result['baseline']['groups'][0]['tests'].values()))
        self.assertEqual(test['observations'], 1)
        self.assertEqual(test['own_elapsed_median_seconds'], 1)
        for mutate in (lambda p: p.update(snapshot_status='partial'),
                       lambda p: p['input']['samples'][0]['workflows'][1]['run'].update(conclusion='failure'),
                       lambda p: p['input']['samples'][0]['tested_checkout'].update(source='e'*40)):
            invalid = copy.deepcopy(pipeline)
            mutate(invalid)
            with self.assertRaises(ValueError):
                admission.admit(policy(), self.rows, receipt(), invalid)

    def test_raw_normalization_counts_descendants_without_parent_wait_false_cost(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            package = PREFIX.split('::')[0]
            manifest = {'catalog': {'source': 'a'*40, 'configuration': {'GOVERSION': 'go1.27.2'},
                                   'units': [package], 'cli': [], 'parents': {}},
                        'settings': {}, 'shard': 0, 'success': True,
                        'phases': {'worker_total_seconds': 95},
                        'timing_observation': {'run_id': '12', 'run_attempt': '1'}}
            path = root/'manifest.json'
            path.write_text(json.dumps(manifest))
            events = [{'Action': 'run', 'Package': package, 'Test': 'TestNew', 'Time': '2026-10-15T00:00:00Z'},
                      {'Action': 'pause', 'Package': package, 'Test': 'TestNew/child'},
                      {'Action': 'pass', 'Package': package, 'Test': 'TestNew/child', 'Elapsed': 4},
                      {'Action': 'pass', 'Package': package, 'Test': 'TestNew', 'Time': '2026-10-15T00:01:34Z', 'Elapsed': .01}]
            log = root/'ordinary.json'
            log.write_text('\n'.join(json.dumps(row) for row in events))
            row = gate.normalize_workers([path], receipt())[0]['tests'][0]
            self.assertEqual(row['own_elapsed_seconds'], .01)
            self.assertEqual(row['parent_wall_seconds'], 94)
            self.assertEqual(row['slowest_descendant_seconds'], 4)
            self.assertEqual(row['parallel_descendants'], 1)
            self.assertEqual(row['parallel_elapsed_work_seconds'], 4)
            events.pop(2)
            log.write_text('\n'.join(json.dumps(row) for row in events))
            with self.assertRaisesRegex(ValueError, 'missing terminal'):
                gate.normalize_workers([path], receipt())

    def test_json_duplicate_keys_nonfinite_and_symlink_rejected(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory)/'input.json'
            for text in ('{"a":1,"a":2}', '{"a":NaN}'):
                path.write_text(text)
                with self.assertRaises(ValueError):
                    gate.read(path)
            link = Path(directory)/'link.json'
            link.symlink_to(path)
            with self.assertRaises(ValueError):
                gate.read(link)


if __name__ == '__main__':
    unittest.main()
