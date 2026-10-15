"""Diagnostic timing must not turn incomplete runs into qualified samples."""
import copy
import contextlib
import io
import json
from pathlib import Path
import tempfile
from unittest import mock
import unittest

import report_ci_timing as timing


def sample():
    value = {'id': 'one', 'source': 'abc', 'cohort': 'source-only', 'cache_state': 'warm',
            'comparable_group': 'linux4-v1', 'event_at': '2026-10-14T00:00:00Z',
            'required_jobs': {'Test': ['linux-race']}, 'workflows': [
                {'run': {'name': 'Test', 'id': 1, 'head_sha': 'abc', 'status': 'completed',
                         'conclusion': 'success', 'created_at': '2026-10-14T00:00:01Z'},
                 'jobs': {'total_count': 1, 'jobs': [{'id': 2, 'name': 'linux-race', 'status': 'completed',
                           'conclusion': 'success', 'started_at': '2026-10-14T00:00:10Z',
                           'completed_at': '2026-10-14T00:02:00Z', 'steps': []}]}}]}
    other = copy.deepcopy(value['workflows'][0])
    other['run'].update(name='Levenshtein', id=3)
    other['jobs']['jobs'][0].update(name='verify', id=4)
    value['required_jobs']['Levenshtein'] = ['verify']
    value['workflows'].append(other)
    return value


class ReportTests(unittest.TestCase):
    def test_elapsed_and_minutes(self):
        result = timing.sample_report(sample())
        self.assertTrue(result['qualifying'])
        self.assertEqual(result['event_to_required_completion_seconds'], 120)
        self.assertAlmostEqual(result['actual_runner_minutes'], 220/60)
        self.assertEqual(result['jobs'][0]['run_created_to_job_start_seconds'], 9)

    def test_cancelled_queue_placeholders_are_not_runner_usage(self):
        value = sample()
        # Actual API shape: run 38672959471, jobs 116221661785/116221661846.
        # Both have a 93-second cancellation interval without runner allocation.
        for name, job_id in (('script-purge (0)', 116221661785), ('linux-full', 116221661846)):
            value['required_jobs']['Test'].append(name)
            value['workflows'][0]['jobs']['jobs'].append({
                'id': job_id, 'name': name, 'status': 'completed', 'conclusion': 'cancelled',
                'created_at': '2026-10-15T02:54:34Z', 'started_at': '2026-10-15T02:54:34Z',
                'completed_at': '2026-10-15T02:56:07Z',
                'runner_id': 0, 'runner_name': '', 'steps': []})
        value['workflows'][0]['jobs']['total_count'] = 3
        result = timing.sample_report(value)
        self.assertFalse(result['qualifying'])
        self.assertTrue(result['runner_minutes_complete'])
        self.assertAlmostEqual(result['actual_runner_minutes'], 220/60)
        placeholders = result['jobs'][1:3]
        self.assertEqual([j['seconds'] for j in placeholders], [93, 93])
        self.assertEqual([j['runner_seconds'] for j in placeholders], [0, 0])
        self.assertEqual([j['runner_allocation'] for j in placeholders], ['unallocated'] * 2)

    def test_allocated_cancelled_usage_and_unknown_allocation_are_distinct(self):
        for conclusion in ('cancelled', 'failure', 'skipped'):
            for metadata, state, seconds in (
                    ({'runner_id': 42, 'runner_name': 'GitHub Actions 42'}, 'allocated', 110),
                    ({'steps': [{'name': 'Set up job', 'started_at': '2026-10-14T00:00:11Z',
                                 'conclusion': 'cancelled'}]}, 'allocated', 110),
                    ({}, 'unknown', None)):
                value = sample()
                value['workflows'][0]['jobs']['jobs'][0].update(conclusion=conclusion, **metadata)
                result = timing.sample_report(value)
                with self.subTest(conclusion=conclusion, metadata=metadata):
                    self.assertFalse(result['qualifying'])
                    self.assertEqual(result['jobs'][0]['runner_allocation'], state)
                    self.assertEqual(result['jobs'][0]['runner_seconds'], seconds)
                    self.assertEqual(result['runner_minutes_complete'], seconds is not None)
                    if seconds is None:
                        self.assertIsNone(result['actual_runner_minutes'])
                        self.assertAlmostEqual(result['known_runner_minutes'], 110/60)
                        self.assertTrue(any('allocation unknown' in issue for issue in result['issues']))
                    else:
                        self.assertAlmostEqual(result['actual_runner_minutes'], 220/60)
        value = sample()
        value['workflows'][0]['jobs']['jobs'][0].update(
            conclusion='skipped', runner_id=None, runner_name=None)
        skipped = timing.sample_report(value)
        self.assertFalse(skipped['qualifying'])
        self.assertEqual(skipped['jobs'][0]['runner_seconds'], 0)
        self.assertTrue(skipped['runner_minutes_complete'])
        value = sample()
        job = value['workflows'][0]['jobs']['jobs'][0]
        job.update(runner_id=42, runner_name='GitHub Actions 42', completed_at=None)
        missing_time = timing.sample_report(value)
        self.assertFalse(missing_time['qualifying'])
        self.assertFalse(missing_time['runner_minutes_complete'])
        self.assertIsNone(missing_time['actual_runner_minutes'])
        self.assertIsNone(missing_time['jobs'][0]['runner_seconds'])
        legacy = timing.sample_report(sample())
        self.assertEqual(legacy['jobs'][0]['runner_allocation'], 'legacy-success-timestamps')
        value = sample()
        value['workflows'][0]['jobs']['jobs'][0].update(runner_id=0, runner_name='')
        self.assertFalse(timing.sample_report(value)['qualifying'])

    def test_unallocated_skipped_backwards_api_timestamps_are_diagnostic(self):
        # Exact untouched API row from run38674259669; completion precedes start.
        job = {'id': 116224141593, 'name': 'checker-benchmark', 'status': 'completed',
               'conclusion': 'skipped', 'runner_id': None, 'runner_name': None, 'steps': [],
               'created_at': '2026-10-15T03:03:32Z', 'started_at': '2026-10-15T03:03:32Z',
               'completed_at': '2026-10-15T03:03:31Z'}
        value = sample()
        value['workflows'][1]['jobs']['jobs'].append(job)
        value['workflows'][1]['jobs']['total_count'] = 2
        result = timing.sample_report(value)
        self.assertTrue(result['qualifying'])
        self.assertTrue(result['runner_minutes_complete'])
        self.assertAlmostEqual(result['actual_runner_minutes'], 220/60)
        row = result['jobs'][-1]
        self.assertEqual(row['runner_seconds'], 0)
        self.assertIsNone(row['seconds'])
        self.assertEqual(row['started_at'], job['started_at'])
        self.assertEqual(row['completed_at'], job['completed_at'])
        self.assertEqual(row['timestamp_issues'], ['job interval: timestamps run backwards'])
        for field, invalid in (('started_at', None), ('completed_at', None),
                               ('started_at', 'invalid'), ('completed_at', 'invalid')):
            changed = copy.deepcopy(value)
            changed['workflows'][1]['jobs']['jobs'][-1][field] = invalid
            result = timing.sample_report(changed)
            with self.subTest(field=field, invalid=invalid):
                self.assertTrue(result['qualifying'])
                self.assertEqual(result['jobs'][-1]['runner_seconds'], 0)
                self.assertIsNone(result['jobs'][-1]['seconds'])
                self.assertTrue(result['jobs'][-1]['timestamp_issues'])
        value['required_jobs']['Levenshtein'].append('checker-benchmark')
        self.assertFalse(timing.sample_report(value)['qualifying'])

    def test_allocated_and_unknown_backwards_timestamps_still_fail_closed(self):
        for metadata in ({'runner_id': 42, 'runner_name': 'runner'}, {}):
            for start in ('2026-10-14T00:03:00Z', 'invalid'):
                value = sample()
                value['workflows'][0]['jobs']['jobs'][0].update(
                    metadata, conclusion='cancelled', started_at=start)
                with self.subTest(metadata=metadata, start=start), self.assertRaises(ValueError):
                    timing.sample_report(value)

    def test_missing_cancelled_failed_skipped_and_cold(self):
        for state in ('failure', 'cancelled', 'skipped', None):
            value = sample()
            value['workflows'][0]['jobs']['jobs'][0]['conclusion'] = state
            self.assertFalse(timing.sample_report(value)['qualifying'])
        value = sample()
        value['workflows'][0]['jobs']['jobs'] = []
        self.assertFalse(timing.sample_report(value)['qualifying'])
        value = sample()
        value['cache_state'] = 'cold'
        self.assertTrue(timing.sample_report(value)['qualifying'])
        self.assertEqual(timing.sample_report(value)['cache_state'], 'cold')

    def test_unknown_cache_event_source_and_workflow(self):
        for mutate in (lambda s: s.pop('event_at'), lambda s: s.update(cache_state='unknown'),
                       lambda s: s.update(source='other'), lambda s: s['workflows'].pop()):
            value = sample()
            mutate(value)
            self.assertFalse(timing.sample_report(value)['qualifying'])

    def test_no_percentile_until_twenty_unique_runs(self):
        values = []
        for i in range(20):
            value = sample()
            value['id'] = str(i)
            value['workflows'][0]['run']['id'] = 2*i
            value['workflows'][1]['run']['id'] = 2*i+1
            values.append(value)
        self.assertIsNone(timing.report({'schema': 1, 'samples': values[:19]})['summaries'][0]['provisional_p95_seconds'])
        self.assertEqual(timing.report({'schema': 1, 'samples': values})['summaries'][0]['provisional_p95_seconds'], 120)
        for value in values:
            value['workflows'][0]['run']['id'] = 1
        summary = timing.report({'schema': 1, 'samples': values})['summaries'][0]
        self.assertEqual(summary['qualifying_unique_runs'], 1)
        self.assertIsNone(summary['provisional_p95_seconds'])

    def test_partial_campaign_cannot_qualify(self):
        value = sample()
        value['workflows'].pop()
        value['required_jobs'].pop('Levenshtein')
        result = timing.sample_report(value)
        self.assertFalse(result['qualifying'])
        self.assertIn('qualification requires Test and Levenshtein inventories', result['issues'])
        self.assertEqual(result['event_to_required_completion_seconds'], 120)

    def test_event_cannot_follow_either_workflow_creation(self):
        value = sample()
        value['event_at'] = '2026-10-14T00:00:02Z'
        result = timing.sample_report(value)
        self.assertFalse(result['qualifying'])
        self.assertIn('event timestamp follows workflow creation', result['issues'])

    def test_reusing_one_workflow_with_new_counterparts_is_not_independent(self):
        values = []
        for i in range(20):
            value = sample()
            value['id'] = str(i)
            value['workflows'][1]['run']['id'] = 100+i
            values.append(value)
        summary = timing.report({'schema': 1, 'samples': values})['summaries'][0]
        self.assertEqual(summary['qualifying_unique_runs'], 1)
        self.assertIsNone(summary['provisional_p95_seconds'])

    def test_cohorts_and_cache_separated(self):
        other = copy.deepcopy(sample())
        other.update(id='two', cache_state='cold', cohort='native-platform')
        self.assertEqual(len(timing.report({'schema': 1, 'samples': [sample(), other]})['summaries']), 2)

    def test_pagination_attempt_and_run_identity(self):
        for mutate in (lambda w: w['jobs'].update(total_count=2),
                       lambda w: w['jobs']['jobs'][0].update(run_id=99),
                       lambda w: w['jobs']['jobs'][0].update(run_attempt=2)):
            value = sample()
            mutate(value['workflows'][0])
            self.assertFalse(timing.sample_report(value)['qualifying'])

    def test_existing_worker_parent_times_are_not_summed(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            (directory/'manifest.json').write_text(json.dumps({'catalog': {'source': 'merge'}, 'shard': 0, 'success': True}))
            events = [
                {'Action': 'run', 'Package': 'p', 'Test': 'TestA', 'Time': '2026-10-14T00:00:00Z'},
                {'Action': 'pass', 'Package': 'p', 'Test': 'TestA/child', 'Elapsed': 3},
                {'Action': 'pass', 'Package': 'p', 'Test': 'TestA', 'Elapsed': .03, 'Time': '2026-10-14T00:00:04Z'},
                {'Action': 'pass', 'Package': 'p', 'Elapsed': 5}]
            (directory/'ordinary.json').write_text('\n'.join(json.dumps(row) for row in events))
            value = timing.worker_evidence(directory)
            self.assertEqual(value['complete_parent_trees'], [{'package': 'p', 'seconds': 4,
                'own_elapsed_seconds': .03, 'status': 'pass', 'log': 'ordinary.json',
                'parent': 'TestA', 'timing_issues': []}])
            self.assertEqual(value['packages'][0]['seconds'], 5)

    def parallel_events(self):
        # Real-shaped Go t.Parallel stream: parent own elapsed is tiny while
        # descendants keep its complete tree alive for nearly 150 seconds.
        return [dict(Package='statshtml', Test=name, Action=action, Time=when, **extra)
                for name, action, when, extra in [
                    ('TestBuild', 'run', '2026-10-14T00:00:00.000000001Z', {}),
                    ('TestBuild', 'pause', '2026-10-14T00:00:00.030000001Z', {}),
                    ('TestBuild', 'cont', '2026-10-14T00:00:10.000000001Z', {}),
                    ('TestBuild/large', 'run', '2026-10-14T00:00:10.010000001Z', {}),
                    ('TestBuild/large', 'pass', '2026-10-14T00:02:38.070000001Z', {'Elapsed': 148.06}),
                    ('TestBuild', 'pass', '2026-10-14T00:02:38.080000001Z', {'Elapsed': .03})]]

    def test_parallel_parent_uses_complete_tree_wall_not_own_elapsed(self):
        row, = timing.parent_tree_timings(self.parallel_events())
        self.assertAlmostEqual(row['seconds'], 148.080, places=5)
        self.assertEqual(row['own_elapsed_seconds'], .03)
        self.assertEqual(row['timing_issues'], [])

    def test_parent_missing_or_malformed_lifecycle_is_unknown(self):
        events = self.parallel_events()
        for broken in (events[1:], events[:-1], [e for e in events if e['Action'] != 'cont'],
                       [dict(e, Time=None) if e['Action'] == 'run' else e for e in events],
                       [dict(e, Time='bad') if e['Action'] == 'cont' else e for e in events],
                       [dict(e, Time='2026-10-14T00:00:10+01:99') if e['Action'] == 'cont' else e for e in events],
                       [dict(e, Time='2026-10-13T00:00:00Z') if e['Action'] == 'pass' else e for e in events],
                       [e for e in events if e['Action'] != 'pause']):
            with self.subTest(events=broken):
                row, = timing.parent_tree_timings(broken)
                self.assertIsNone(row['seconds'])
                self.assertTrue(row['timing_issues'])

    def test_parent_repeated_invocations_and_truncated_invocation_stay_separate(self):
        events = self.parallel_events()
        rows = timing.parent_tree_timings(events + events)
        self.assertEqual(len(rows), 2)
        self.assertTrue(all(not r['timing_issues'] for r in rows))
        # New package start flushes an incomplete earlier invocation, even
        # when the same parent name appears again.
        rows = timing.parent_tree_timings(events[:-1] + [{'Package': 'statshtml', 'Action': 'start'}] + events)
        self.assertEqual(len(rows), 2)
        self.assertIsNone(rows[0]['seconds'])
        self.assertAlmostEqual(rows[1]['seconds'], 148.08, places=5)
        rows = timing.parent_tree_timings(events[:1] + events)
        self.assertTrue(rows[0]['timing_issues'])
        self.assertIsNone(rows[0]['seconds'])

    def test_tier_batch_reports_overlap_without_summing(self):
        import run_ci_tier
        output = io.StringIO()
        commands = [(['go', 'test'], Path('one'), {}), (['go', 'test'], Path('two'), {})]
        with mock.patch.object(run_ci_tier.full_race.shards, 'supervise', return_value=[2.0, 3.0]), contextlib.redirect_stdout(output):
            run_ci_tier.checked_many(commands, 10)
        record = json.loads(output.getvalue().split('tier invocation timing: ')[1])
        self.assertTrue(record['overlap'])
        self.assertEqual([r['seconds'] for r in record['invocations']], [2.0, 3.0])


    def test_reject_duplicate_and_backwards_evidence(self):
        with self.assertRaises(ValueError):
            timing.report({'schema': 1, 'samples': [sample(), sample()]})
        value = sample()
        value['workflows'][0]['jobs']['jobs'][0]['completed_at'] = '2026-10-13T00:00:00Z'
        with self.assertRaises(ValueError):
            timing.sample_report(value)


if __name__ == '__main__':
    unittest.main()
