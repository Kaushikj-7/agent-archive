"""Adversarial performance admission, attribution, approval and budget contracts."""
import copy
from datetime import date
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest import mock

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
    result['work_budgets'] = []
    ids = [PREFIX + 'Test' + str(i) for i in range(8)]
    result['baseline'] = {'inventory': ids, 'groups': [],
        'metric': 'maximum-parent-or-descendant-terminal-elapsed', 'provenance': {
        'source': 'f'*40, 'tree': 'b'*40, 'run_id': 11, 'run_attempt': 1, 'workers': 8,
        'inventory_sha256': norm.digest(ids), 'compiled_inventory_sha256': 'd'*64,
        'pipeline_sha256': 'c'*64}}
    result['baseline']['samples'] = [dict(result['baseline']['provenance'], run_id=i) for i in (11, 10, 9)]
    identity = {'configuration_sha256': 'e'*64, 'cohort': worker(7)['cohort']}
    result['baseline']['groups'] = [{'key': norm.digest(identity), 'identity': identity,
        'tests': {ids[7]: {'observations': 1, 'own_elapsed_median_seconds': 1}}}]
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
    def test_offline_legacy_admission_is_explicit_and_retains_body_validation(self):
        import test_go_parent_shards as parent_fixture
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            parent_fixture.evidence(root, parent_fixture.catalog())
            paths = sorted(root.glob('*/manifest.json'))
            topology = dict(receipt(), source=parent_fixture.SOURCE['source'], tree=parent_fixture.SOURCE['tree'])
            for path in paths:
                manifest = gate.read(path)
                manifest['catalog']['schema'] = 3
                manifest['catalog_sha256'] = parent_fixture.r.digest(manifest['catalog'])
                path.write_text(json.dumps(manifest))
                (path.parent/'listing-combined.json').unlink()
            with self.assertRaisesRegex(ValueError, 'raw compiled catalog invalid'):
                gate.verify_raw_coverage(paths, topology)
            gate.verify_raw_coverage(paths, topology, allow_legacy=True)
            # Opt-in accepts the old discovery format, never missing bodies.
            assignment = gate.read(paths[0])['coverage']
            package = next(iter(assignment['parents']))
            log = paths[0].parent/parent_fixture.p.log_name(package)
            events = parent_fixture.r.json_stream(log.read_text())
            missing = assignment['parents'][package][0]
            log.write_text(parent_fixture.rows([event for event in events
                           if event.get('Test', '').split('/')[0] != missing]))
            with self.assertRaisesRegex(ValueError, 'raw compiled coverage invalid'):
                gate.verify_raw_coverage(paths, topology, allow_legacy=True)

    def test_actual_fixture_receipts_activate_every_checked_in_budget(self):
        checked = gate.read(Path(gate.__file__).with_name('ci_performance_policy.json'))
        self.assertEqual(len(checked['work_budgets']), 5)
        self.policy['work_budgets'] = checked['work_budgets']
        proof = gate.read(Path(__file__).with_name('testdata')/'fixture_work_receipts.json')
        events = proof['events']
        metrics = norm.fixtures(events)
        identities = sorted({budget['test_id'] for budget in checked['work_budgets']})
        self.assertEqual(set(identities), {row['test_id'] for row in metrics})
        for index, ident in enumerate(identities):
            self.rows[index]['tests'][0]['test_id'] = ident
            self.rows[index]['fixture_metrics'] = [row for row in metrics if row['test_id'] == ident]
        self.assertTrue(self.run_gate()['passed'])
        original = copy.deepcopy(self.rows)
        for budget in checked['work_budgets']:
            with self.subTest(budget=budget):
                self.rows = copy.deepcopy(original)
                owner = next(row for row in self.rows if row['tests'][0]['test_id'] == budget['test_id'])
                selected = [row for row in owner['fixture_metrics']
                            if row['fixture'] == budget['fixture'] and row['phase'] == budget['phase']]
                self.assertEqual(sum(row['operations'][budget['operation']] for row in selected), budget['maximum'])
                selected[0]['operations'][budget['operation']] += 1
                self.assertFalse(self.run_gate()['passed'])
                self.rows = copy.deepcopy(original)
                owner = next(row for row in self.rows if row['tests'][0]['test_id'] == budget['test_id'])
                selected = next(row for row in owner['fixture_metrics']
                                if row['fixture'] == budget['fixture'] and row['phase'] == budget['phase'])
                del selected['operations'][budget['operation']]
                with self.assertRaisesRegex(ValueError, 'fixture metric missing'):
                    self.run_gate()
                self.rows = copy.deepcopy(original)
                owner = next(row for row in self.rows if row['tests'][0]['test_id'] == budget['test_id'])
                owner['fixture_metrics'] = [row for row in owner['fixture_metrics']
                                           if row['fixture'] != budget['fixture'] or row['phase'] != budget['phase']]
                with self.assertRaisesRegex(ValueError, 'fixture metric missing'):
                    self.run_gate()
                self.rows = copy.deepcopy(original)
                owner = next(row for row in self.rows if row['tests'][0]['test_id'] == budget['test_id'])
                owner['tests'] = []
                owner['fixture_metrics'] = []
                with self.assertRaisesRegex(ValueError, 'fixture parent missing'):
                    self.run_gate()
        self.rows = copy.deepcopy(original)
        # Both real 1401-row build receipts belong to the same parent. Repeating
        # either must exceed the aggregate constructor and encoding ceilings.
        owner = next(row for row in self.rows if 'LargeFixtures' in row['tests'][0]['test_id'])
        owner['fixture_metrics'].append(copy.deepcopy(owner['fixture_metrics'][0]))
        self.assertFalse(self.run_gate()['passed'])
        self.rows = copy.deepcopy(original)
        # Moving genuine work to another valid enclosing test cannot satisfy its
        # source owner's required phase, even when the operation count matches.
        owner = next(row for row in self.rows if 'LargeFixtures' in row['tests'][0]['test_id'])
        other = self.rows[7]
        for metric in owner['fixture_metrics']:
            metric['test_id'] = other['tests'][0]['test_id']
        other['fixture_metrics'], owner['fixture_metrics'] = owner['fixture_metrics'], []
        with self.assertRaisesRegex(ValueError, 'fixture metric missing'):
            self.run_gate()

    def test_checked_in_policy_has_admitted_baseline(self):
        checked = gate.policy_check(gate.read(Path(gate.__file__).with_name('ci_performance_policy.json')))
        baseline = checked['baseline']
        self.assertTrue(baseline['groups'], 'checked-in policy must not retain the installation placeholder')
        self.assertIn(baseline['provenance'], baseline['samples'])
        identities = [(row['run_id'], row['run_attempt']) for row in baseline['samples']]
        self.assertEqual(len(identities), len(set(identities)))
        observed = set()
        for group in baseline['groups']:
            self.assertTrue(group['tests'])
            observed.update(group['tests'])
            for measurement in group['tests'].values():
                self.assertLessEqual(measurement['observations'], len(identities))
        self.assertTrue(observed)
        self.assertLessEqual(observed, set(baseline['inventory']))

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

    def test_slow_large_parallel_family_needs_both_exception_budgets(self):
        self.new_test(own_elapsed_seconds=45, parallel_descendants=101,
                      parallel_elapsed_work_seconds=60)
        self.exception()
        self.assertFalse(self.run_gate(API([approved()]))['passed'])
        self.exception(maximum_parallel_descendants=120)
        self.assertFalse(self.run_gate(API([approved()]))['passed'])
        self.exception(maximum_parallel_descendants=120, maximum_parallel_elapsed_work_seconds=90)
        self.assertTrue(self.run_gate(API([approved()]))['passed'])

    def test_exception_must_have_an_expiry_even_if_unused(self):
        self.exception()
        del self.exceptions['exceptions'][0]['expires']
        with self.assertRaisesRegex(ValueError, 'expiry required'):
            self.run_gate()

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

    def test_latest_submitted_review_overrides_creation_id_order(self):
        self.new_test(own_elapsed_seconds=84)
        self.exception()
        older_created_later_submitted = approved(id=1, state='CHANGES_REQUESTED',
                                                 submitted_at='2026-10-15T01:00:00Z')
        newer_created_first_submitted = approved(id=2)
        for reviews in ([older_created_later_submitted, newer_created_first_submitted],
                        [newer_created_first_submitted, older_created_later_submitted]):
            with self.subTest(reviews=reviews), self.assertRaises(ValueError):
                self.run_gate(API(reviews))
        older_created_later_submitted['state'] = 'APPROVED'
        newer_created_first_submitted['state'] = 'CHANGES_REQUESTED'
        self.assertTrue(self.run_gate(API([newer_created_first_submitted,
                                          older_created_later_submitted]))['passed'])

    def test_pending_reviews_do_not_replace_latest_submitted_decision(self):
        self.new_test(own_elapsed_seconds=84)
        self.exception()
        pending = approved(id=3, state='PENDING', submitted_at=None)
        for submitted_at in (None, 'absent'):
            pending['submitted_at'] = submitted_at
            if submitted_at == 'absent':
                del pending['submitted_at']
            for reviews in ([pending, approved()], [approved(), pending]):
                with self.subTest(reviews=reviews):
                    self.assertTrue(self.run_gate(API(reviews))['passed'])
            with self.assertRaises(ValueError):
                self.run_gate(API([pending]))
            with self.assertRaises(ValueError):
                self.run_gate(API([approved(), pending, approved(id=2, state='CHANGES_REQUESTED')]))
        # Only explicit drafts are ignored; malformed submitted reviews fail.
        with self.assertRaisesRegex(ValueError, 'submitted review identity'):
            self.run_gate(API([approved(), approved(id=4, submitted_at=None)]))

    def test_bare_compiled_parent_names_receive_budgets_and_can_be_admitted(self):
        for name in ('Test', 'Fuzz', 'Example'):
            self.setUp()
            test = self.rows[0]['tests'][0]
            test.update(test_id=PREFIX + name, own_elapsed_seconds=31)
            with self.subTest(name=name):
                result = self.run_gate()
                self.assertEqual(result['failures'][0]['reason'], 'new-test-budget')
                self.exception(test_id=test['test_id'])
                self.assertTrue(self.run_gate(API([approved()]))['passed'])
                self.policy['parallel_budgets'][test['test_id']] = {
                    'maximum_descendants': 2, 'maximum_elapsed_work_seconds': 3}
                self.policy['baseline']['inventory'] = sorted(
                    self.policy['baseline']['inventory'] + [test['test_id']])
                self.policy['baseline']['provenance']['inventory_sha256'] = norm.digest(
                    self.policy['baseline']['inventory'])
                self.policy['baseline']['samples'][0] = copy.deepcopy(self.policy['baseline']['provenance'])
                gate.policy_check(self.policy)

    def test_malformed_baseline_cannot_enable_fabricated_history(self):
        original = copy.deepcopy(self.policy)
        changes = [lambda b: b.update(groups=[]), lambda b: b.pop('samples'),
                   lambda b: b['samples'].append(copy.deepcopy(b['samples'][0])),
                   lambda b: b['samples'][0].pop('compiled_inventory_sha256'),
                   lambda b: next(iter(b['groups'][0]['tests'].values())).update(observations=4)]
        for change in changes:
            self.policy = copy.deepcopy(original)
            change(self.policy['baseline'])
            with self.subTest(change=change), self.assertRaises((ValueError, KeyError)):
                self.run_gate()
        self.policy = copy.deepcopy(original)
        extra = copy.deepcopy(self.policy['baseline']['groups'][0])
        extra['identity']['cohort']['runner_image'] = 'other'
        extra['key'] = norm.digest(extra['identity'])
        for group in (extra, self.policy['baseline']['groups'][0]):
            next(iter(group['tests'].values()))['observations'] = 2
        self.policy['baseline']['groups'].append(extra)
        with self.assertRaisesRegex(ValueError, 'physical sample inventory'):
            self.run_gate()
        for change in ({'cache_state': 'unknown'}, {'runner_image': 'unknown'}, {'compiler': {}}):
            self.policy = copy.deepcopy(original)
            group = self.policy['baseline']['groups'][0]
            group['identity']['cohort'].update(change)
            group['key'] = norm.digest(group['identity'])
            with self.subTest(change=change), self.assertRaisesRegex(ValueError, 'cohort invalid'):
                self.run_gate()

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
        original = copy.deepcopy(self.rows[0])
        for change in ({'runner_image': 'different'}, {'cache_state': 'miss'},
                       {'compiler': {'GOVERSION': 'different'}}):
            self.rows[0] = copy.deepcopy(original)
            self.rows[0]['cohort'].update(change)
            with self.subTest(change=change):
                self.assertTrue(self.run_gate()['passed'])
        self.rows[0] = original
        self.rows[0]['configuration_sha256'] = 'f'*64
        self.assertTrue(self.run_gate()['passed'])

    def test_explicit_family_budget_survives_baseline_admission(self):
        test = self.rows[0]['tests'][0]
        self.policy['parallel_budgets'][test['test_id']] = {
            'maximum_descendants': 120, 'maximum_elapsed_work_seconds': 90}
        test.update(parallel_descendants=121)
        self.assertFalse(self.run_gate()['passed'])
        test.update(parallel_descendants=120, parallel_elapsed_work_seconds=91)
        self.assertFalse(self.run_gate()['passed'])
        test.update(parallel_elapsed_work_seconds=90)
        self.assertTrue(self.run_gate()['passed'])

    def test_inventory_addition_does_not_erase_existing_parent_regression(self):
        pipeline = collect_ci_timing.collect(FakeAPI(), receipt(), 1, 2)
        for row in self.rows:
            row['run_id'] = '1'
        samples = []
        for run_id in (20, 30):
            api = FakeAPI()
            api.runs[run_id] = api.runs.pop(1)
            api.runs[run_id]['id'] = run_id
            api.jobs[run_id] = api.jobs.pop(1)
            for job in api.jobs[run_id]:
                job['run_id'] = run_id
            rows = copy.deepcopy(self.rows)
            for row in rows:
                row.update(run_id=str(run_id), compiled_inventory_sha256=str(run_id)[0]*64)
            samples.append((rows, receipt(), collect_ci_timing.collect(api, receipt(), run_id, 2)))
        self.policy = admission.admit(self.policy, self.rows, receipt(), pipeline, samples)
        baseline = next(iter(self.policy['baseline']['groups'][0]['tests'].values()))
        self.assertEqual(baseline['observations'], 3)
        for row in self.rows:
            row.update(run_id='12', compiled_inventory_sha256='f'*64)
        old = self.rows[0]['tests'][0]
        old['own_elapsed_seconds'] = 20
        new = copy.deepcopy(old)
        new.update(test_id=PREFIX + 'TestAdded', own_elapsed_seconds=35)
        self.rows[0]['tests'].append(new)
        result = self.run_gate()
        self.assertEqual({row['reason'] for row in result['failures']},
                         {'existing-test-regression', 'new-test-budget'})
        self.assertEqual(len(result['failures']), 2)

    def test_first_actual_execution_of_known_skip_has_no_unbounded_allowance(self):
        test = self.rows[0]['tests'][0]
        test['own_elapsed_seconds'] = 90
        self.assertEqual(self.run_gate()['failures'][0]['reason'], 'new-test-budget')
        test.update(own_elapsed_seconds=1, parallel_descendants=101)
        self.assertFalse(self.run_gate()['passed'])
        test.update(status='skip', parallel_descendants=0, own_elapsed_seconds=0)
        self.assertTrue(self.run_gate()['passed'])
        test.update(status='pass', own_elapsed_seconds=90)
        identity = {'configuration_sha256': 'e'*64, 'cohort': self.rows[0]['cohort']}
        group = {'key': norm.digest(identity), 'identity': identity, 'tests': {
            test['test_id']: {'observations': 1, 'own_elapsed_median_seconds': 1}}}
        self.policy['baseline']['groups'] = [group]
        for count in (1, 2):
            group['tests'][test['test_id']]['observations'] = count
            result = self.run_gate()
            self.assertTrue(result['passed'])
            self.assertIn('insufficient-comparable-baseline', [row['reason'] for row in result['warnings']])

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
        valid = copy.deepcopy(self.rows[0]['fixture_metrics'][0])
        for changes in ({'fixture': 'other'}, {'phase': 'other'}, {'operations': {}}):
            self.rows[0]['fixture_metrics'] = [dict(valid, **changes)]
            with self.subTest(changes=changes), self.assertRaisesRegex(ValueError, 'missing'):
                self.run_gate()
        self.rows[0]['fixture_metrics'] = [valid, copy.deepcopy(valid)]
        self.assertEqual(self.run_gate()['failures'][0]['reason'], 'deterministic-work-budget')
        self.rows[0]['fixture_metrics'] = []
        self.rows[0]['tests'] = []
        with self.assertRaisesRegex(ValueError, 'required deterministic fixture parent missing'):
            self.run_gate()

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
        self.assertEqual(len(result['baseline']['samples']), 1)
        for mutate in (lambda p: p.update(snapshot_status='partial'),
                       lambda p: p['input']['samples'][0]['workflows'][1]['run'].update(conclusion='failure'),
                       lambda p: p['input']['samples'][0]['tested_checkout'].update(source='e'*40)):
            invalid = copy.deepcopy(pipeline)
            mutate(invalid)
            with self.assertRaises(ValueError):
                admission.admit(policy(), self.rows, receipt(), invalid)

    def test_targeted_rerun_accepts_only_api_proven_physical_worker_attempts(self):
        api = FakeAPI()
        jobs = api.jobs[1]
        actual = gate.worker_attempts(jobs, receipt(), 1, 2)
        for row in self.rows:
            row['run_id'] = '1'
        result = gate.evaluate(self.policy, self.rows, receipt(), '1', '2', self.exceptions,
                               actual_attempts=actual)
        self.assertTrue(result['passed'])
        with self.assertRaisesRegex(ValueError, 'physical worker receipt'):
            gate.evaluate(self.policy, self.rows, receipt(), '1', '2', self.exceptions)
        for change in ({'run_attempt': 3}, {'run_attempt': None}, {'head_sha': 'f'*40},
                       {'run_id': 4}, {'conclusion': 'failure'}):
            invalid = copy.deepcopy(jobs)
            next(job for job in invalid if job['name'] == 'linux-short (0)').update(change)
            with self.subTest(change=change), self.assertRaises(ValueError):
                gate.worker_attempts(invalid, receipt(), 1, 2)

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

    def test_admission_rechecks_raw_compiled_assignment_and_bodies(self):
        import test_go_parent_shards as parent_fixture
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            catalog = parent_fixture.catalog()
            parent_fixture.evidence(root, catalog)
            paths = sorted(root.glob('*/manifest.json'))
            topology = dict(receipt(), source=parent_fixture.SOURCE['source'],
                            tree=parent_fixture.SOURCE['tree'])
            gate.verify_raw_coverage(paths, topology)
            original = paths[0].read_text()
            for mutate in (lambda m: m.update(catalog_sha256='0'*64),
                           lambda m: m.update(coverage={}),
                           lambda m: m['catalog']['source_proof'].update(tree='f'*40)):
                manifest = json.loads(original)
                mutate(manifest)
                paths[0].write_text(json.dumps(manifest))
                with self.assertRaises(ValueError):
                    gate.verify_raw_coverage(paths, topology)
            paths[0].write_text(original)
            manifest = json.loads(original)
            assignment = manifest['coverage']
            package = next(iter(assignment['parents']))
            log = paths[0].parent / parent_fixture.p.log_name(package)
            events = parent_fixture.r.json_stream(log.read_text())
            missing = assignment['parents'][package][0]
            events = [e for e in events if e.get('Test', '').split('/')[0] != missing]
            log.write_text(parent_fixture.rows(events))
            # A successful manifest and enclosing pipeline cannot replace a
            # missing actual selected parent body in the raw evidence.
            self.assertTrue(manifest['success'])
            with self.assertRaisesRegex(ValueError, 'raw compiled coverage invalid'):
                gate.verify_raw_coverage(paths, topology)

    def test_live_cli_rechecks_raw_assignments_before_using_normalized_timings(self):
        import test_go_parent_shards as parent_fixture
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            artifacts = root/'workers'
            artifacts.mkdir()
            catalog = json.loads(json.dumps(parent_fixture.catalog()).replace('"light"',
                '"github.com/wangjohn/agent-archive/internal/light"').replace('"fileless"',
                '"github.com/wangjohn/agent-archive/internal/fileless"'))
            catalog['units'] = sorted(catalog['units'])
            parent_fixture.evidence(artifacts, catalog)
            topology = dict(receipt(), source=parent_fixture.SOURCE['source'],
                            tree=parent_fixture.SOURCE['tree'])
            inputs = {'policy': self.policy, 'exceptions': self.exceptions, 'topology': topology}
            argv = ['ci_performance_gate.py', '--workers', str(artifacts), '--output', str(root/'result.json')]
            for name, document in inputs.items():
                path = root/(name + '.json')
                path.write_text(json.dumps(document))
                argv.extend(['--' + name, str(path)])
            manifests = sorted(artifacts.glob('*/manifest.json'))
            for path in manifests:
                manifest = json.loads(path.read_text())
                manifest.update(phases={'worker_total_seconds': 1},
                                timing_observation={'run_id': '12', 'run_attempt': '1'})
                path.write_text(json.dumps(manifest))
            original = manifests[0].read_text()
            assignment = json.loads(original)['coverage']
            package = next(iter(assignment['parents']))
            log = manifests[0].parent/parent_fixture.p.log_name(package)
            original_log = log.read_text()
            missing = assignment['parents'][package][0]
            for damage in (None, 'catalog', 'assignment', 'parent-body'):
                manifests[0].write_text(original)
                log.write_text(original_log)
                manifest = json.loads(original)
                if damage == 'catalog':
                    manifest['catalog_sha256'] = '0'*64
                elif damage == 'assignment':
                    manifest['coverage'] = {}
                elif damage == 'parent-body':
                    events = parent_fixture.r.json_stream(original_log)
                    log.write_text(parent_fixture.rows(
                        [event for event in events if event.get('Test', '').split('/')[0] != missing]))
                manifests[0].write_text(json.dumps(manifest))
                with self.subTest(damage=damage), mock.patch('sys.argv', argv), \
                        mock.patch.dict(os.environ, {'GITHUB_RUN_ID': '12', 'GITHUB_RUN_ATTEMPT': '1'}), \
                        mock.patch.object(gate.collect_ci_timing, 'API'), \
                        mock.patch.object(gate, 'current_worker_attempts', return_value={i: 1 for i in range(8)}), \
                        mock.patch.object(gate.normalize_ci_timing, 'worker', wraps=gate.normalize_ci_timing.worker) as normalize:
                    if damage is None:
                        gate.main()
                        self.assertTrue(json.loads((root/'result.json').read_text())['passed'])
                        self.assertEqual(normalize.call_count, 8)
                    else:
                        with self.assertRaises(SystemExit) as failure:
                            gate.main()
                        self.assertEqual(failure.exception.code, 1)
                        self.assertFalse(json.loads((root/'result.json').read_text())['passed'])
                        normalize.assert_not_called()

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
