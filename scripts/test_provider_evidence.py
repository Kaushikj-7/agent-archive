"""Synthetic evidence controls; these do not execute a storage provider."""
import importlib.util
import json
from pathlib import Path
import tempfile
import unittest


SCRIPT = Path(__file__).parent / 'acceptance/provider/verify-results.py'
SPEC = importlib.util.spec_from_file_location('provider_evidence', SCRIPT)
VERIFIER = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(VERIFIER)


def event(action, package='p', test=None):
    row = {'Action': action, 'Package': package}
    if test is not None:
        row['Test'] = test
    return row


class EvidenceTest(unittest.TestCase):
    def verify(self, events, required=('p::TestRequired',)):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'events.jsonl'
            path.write_text('\n'.join(json.dumps(row) for row in events))
            return VERIFIER.verify(path, required)

    def complete(self):
        return [event('start'), event('run', test='TestRequired'),
                event('pass', test='TestRequired'), event('pass')]

    def test_complete_actual_execution(self):
        self.assertEqual(self.verify(self.complete())['result'], 'pass')

    def test_same_name_in_another_package_cannot_supply_required_test(self):
        rows = self.complete()
        rows[1]['Package'] = rows[2]['Package'] = 'other'
        with self.assertRaises(ValueError):
            self.verify(rows)

    def test_missing_package_or_test_terminals_and_run_are_rejected(self):
        for index in range(4):
            with self.subTest(index=index), self.assertRaises(ValueError):
                self.verify([row for i, row in enumerate(self.complete()) if i != index])

    def test_failure_or_skip_cannot_be_erased_by_later_success(self):
        for action in ('fail', 'skip'):
            for name in (None, 'TestRequired', 'TestRequired/child'):
                with self.subTest(action=action, name=name), self.assertRaises(ValueError):
                    self.verify([event(action, test=name)] + self.complete())

    def test_passed_parent_cannot_hide_unfinished_or_missing_descendant(self):
        rows = self.complete()
        rows.insert(2, event('run', test='TestRequired/child'))
        with self.assertRaises(ValueError):
            self.verify(rows)
        with self.assertRaises(ValueError):
            self.verify(self.complete(), ('p::TestRequired', 'p::TestRequired/child'))

    def test_parallel_output_and_complete_descendant_are_accepted(self):
        rows = self.complete()
        rows[2:2] = [event('pause', test='TestRequired'), event('cont', test='TestRequired'),
                     event('run', test='TestRequired/child'),
                     event('output', test='TestRequired/child'),
                     event('pass', test='TestRequired/child')]
        self.assertEqual(self.verify(rows)['result'], 'pass')

    def test_duplicate_terminal_or_execution_is_rejected(self):
        for row in (event('pass'), event('pass', test='TestRequired'),
                    event('run', test='TestRequired')):
            with self.subTest(row=row), self.assertRaises(ValueError):
                self.verify(self.complete() + [row])

    def test_empty_or_ambiguous_requirements_are_rejected(self):
        for required in ((), ('TestRequired',), ('p::',), ('::TestRequired',),
                         ('p::TestRequired', 'p::TestRequired')):
            with self.subTest(required=required), self.assertRaises(ValueError):
                self.verify(self.complete(), required)

    def test_unexpected_package_and_non_object_events_are_rejected(self):
        for row in (event('start', 'other'), [], None):
            with self.subTest(row=row), self.assertRaises(ValueError):
                self.verify(self.complete() + [row])


if __name__ == '__main__':
    unittest.main()
