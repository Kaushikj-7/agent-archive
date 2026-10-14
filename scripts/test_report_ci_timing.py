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
    return {'id': 'one', 'source': 'abc', 'cohort': 'source-only', 'cache_state': 'warm',
            'comparable_group': 'linux4-v1', 'event_at': '2026-10-14T00:00:00Z',
            'required_jobs': {'Test': ['linux-race']}, 'workflows': [
                {'run': {'name': 'Test', 'id': 1, 'head_sha': 'abc', 'status': 'completed',
                         'conclusion': 'success', 'created_at': '2026-10-14T00:00:01Z'},
                 'jobs': {'total_count': 1, 'jobs': [{'id': 2, 'name': 'linux-race', 'status': 'completed',
                           'conclusion': 'success', 'started_at': '2026-10-14T00:00:10Z',
                           'completed_at': '2026-10-14T00:02:00Z', 'steps': []}]}}]}


class ReportTests(unittest.TestCase):
    def test_elapsed_and_minutes(self):
        result = timing.sample_report(sample())
        self.assertTrue(result['qualifying'])
        self.assertEqual(result['event_to_required_completion_seconds'], 120)
        self.assertAlmostEqual(result['actual_runner_minutes'], 110/60)
        self.assertEqual(result['jobs'][0]['run_created_to_job_start_seconds'], 9)

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
                       lambda s: s.update(source='other'), lambda s: s['required_jobs'].update(Levenshtein=['verify'])):
            value = sample()
            mutate(value)
            self.assertFalse(timing.sample_report(value)['qualifying'])

    def test_no_percentile_until_twenty_unique_runs(self):
        values = []
        for i in range(20):
            value = sample()
            value['id'] = str(i)
            value['workflows'][0]['run']['id'] = i
            values.append(value)
        self.assertIsNone(timing.report({'schema': 1, 'samples': values[:19]})['summaries'][0]['provisional_p95_seconds'])
        self.assertEqual(timing.report({'schema': 1, 'samples': values})['summaries'][0]['provisional_p95_seconds'], 120)
        for value in values:
            value['workflows'][0]['run']['id'] = 1
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
            (directory/'ordinary.json').write_text('\n'.join(json.dumps(row) for row in [
                {'Action': 'pass', 'Package': 'p', 'Test': 'TestA/child', 'Elapsed': 3},
                {'Action': 'pass', 'Package': 'p', 'Test': 'TestA', 'Elapsed': 4},
                {'Action': 'pass', 'Package': 'p', 'Elapsed': 5}]))
            value = timing.worker_evidence(directory)
            self.assertEqual(value['complete_parent_trees'], [{'package': 'p', 'seconds': 4, 'status': 'pass', 'log': 'ordinary.json', 'parent': 'TestA'}])
            self.assertEqual(value['packages'][0]['seconds'], 5)

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
