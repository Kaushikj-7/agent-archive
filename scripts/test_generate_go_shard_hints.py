import copy
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest import mock

import generate_go_shard_hints as hints
import run_ci_tier

CLI = hints.runner.CLI
PKGS = ['example/a', 'example/b']
NAMES = ['TestAlpha', 'TestBeta']
SETTINGS = {str(i): {'cpus': 4, 'package_p': 1, 'isolated_packages': [],
                     'ordinary_cpus': 2, 'cli_cpus': 2} for i in range(2)}


def stream(path, rows):
    path.write_text('\n'.join(json.dumps(r) for r in rows))


def fixture(root, duration):
    catalog = {'source': 'accepted-source', 'packages': {p: True for p in PKGS + [CLI]},
               'cli': NAMES, 'assignments': [{'packages': [PKGS[i]], 'cli': [NAMES[i]]} for i in range(2)]}
    for i in range(2):
        p = root / str(i)
        p.mkdir(parents=True)
        manifest = {'shard': i, 'catalog': catalog, 'catalog_sha256': hints.runner.digest(catalog),
                    'success': True, 'coverage': catalog['assignments'][i],
                    'settings': SETTINGS[str(i)], 'phases': {'vet_seconds': 55 if i == 0 else 0}}
        (p / 'manifest.json').write_text(json.dumps(manifest))
        stream(p / 'packages.json', [{'ImportPath': n, 'Dir': '/tmp/example', 'TestGoFiles': ['test.go']} for n in PKGS + [CLI]])
        stream(p / 'listing.json', [{'Package': CLI, 'Action': 'output', 'Output': n + '\n'} for n in NAMES] + [{'Package': CLI, 'Action': 'pass'}])
        stream(p / 'ordinary.json', [{'Package': PKGS[i], 'Action': 'pass', 'Elapsed': duration + i}])
        stream(p / 'cli.json', [{'Package': CLI, 'Test': NAMES[i], 'Action': 'run', 'Time': '2026-01-01T00:00:00Z'},
                               {'Package': CLI, 'Test': NAMES[i], 'Action': 'pass', 'Elapsed': duration,
                                'Time': f'2026-01-01T00:00:0{duration}Z'},
                               {'Package': CLI, 'Action': 'pass', 'Elapsed': duration + 1}])
        manifest['tier'] = 'pr'
        manifest['body_evidence'] = run_ci_tier.body_evidence(
            (p / 'ordinary.json').read_text() + '\n' + (p / 'cli.json').read_text(), short=True)
        (p / 'manifest.json').write_text(json.dumps(manifest))


class GeneratorTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        for i, duration in enumerate([1, 3]):
            fixture(self.root / str(i), duration)
        self.samples = [(f'run{i}', self.root / str(i)) for i in range(2)]

    def generate(self, samples=None):
        return hints.generate(samples or self.samples, 'accepted-source', SETTINGS,
                              PKGS + ['example/new'], NAMES + ['TestUnknown'])

    def mutate(self, update, file='manifest.json', shard='0'):
        path = self.root / '0' / shard / file
        value = json.loads(path.read_text())
        update(value)
        path.write_text(json.dumps(value))

    def test_medians_positive_unknowns_deterministic_and_provenance(self):
        result = self.generate()
        self.assertEqual(result['packages'], {'example/a': 2000, 'example/b': 3000, 'example/new': 1000})
        self.assertEqual(result['cli'], {'TestAlpha': 2000, 'TestBeta': 2000, 'TestUnknown': 1000})
        self.assertEqual(result, self.generate())
        self.assertEqual(result['provenance']['source'], 'accepted-source')
        self.assertEqual(result['provenance']['settings'], SETTINGS)
        self.assertEqual(len(result['provenance']['samples']), 2)
        self.assertTrue(result['provenance']['samples'][0]['files'])
        self.assertEqual(hints.positive_ms(0), 1)
        self.assertEqual(hints.positive_ms('0.0015'), 2)
        for invalid in [-1, float('inf'), float('nan'), None, True, 'bad']:
            with self.assertRaises(hints.runner.Failure):
                hints.positive_ms(invalid)

    def timed(self, name, action, seconds, elapsed=None):
        record = {'Test': name, 'Action': action, 'Time': f'2026-01-01T00:{seconds // 60:02d}:{seconds % 60:02d}Z'}
        if elapsed is not None:
            record['Elapsed'] = elapsed
        return record

    def test_parallel_cohort_queue_and_prelude_count_once(self):
        e = self.timed
        records = [e('TestAlpha', 'run', 0), e('TestAlpha', 'pause', 1),
                   e('TestAlpha', 'cont', 40), e('TestAlpha/left', 'run', 41),
                   e('TestAlpha/left', 'pause', 42), e('TestAlpha/right', 'run', 42),
                   e('TestAlpha/right', 'pause', 43), e('TestAlpha/left', 'cont', 80),
                   e('TestAlpha/left', 'pass', 86, 7), e('TestAlpha/right', 'cont', 90),
                   e('TestAlpha/right', 'pass', 97, 8), e('TestAlpha', 'pass', 97, 4)]
        self.assertEqual(hints.cli_durations(records, ['TestAlpha']), {'TestAlpha': 17})

    def test_sequential_parent_with_nested_parallel_children_excludes_wait(self):
        e = self.timed
        records = [e('TestAlpha', 'run', 0), e('TestAlpha/seq', 'run', 1),
                   e('TestAlpha/seq/par', 'run', 2), e('TestAlpha/seq/par', 'pause', 3),
                   e('TestAlpha/seq/par', 'cont', 30), e('TestAlpha/seq/par', 'pass', 34, 5),
                   e('TestAlpha/seq', 'pass', 34, 3), e('TestAlpha', 'pass', 35, 35)]
        self.assertEqual(hints.cli_durations(records, ['TestAlpha']), {'TestAlpha': 9})

    def test_overlapping_trees_and_skipped_bodies(self):
        e = self.timed
        records = [e('TestAlpha', 'run', 0), e('TestBeta', 'run', 0),
                   e('TestAlpha/skip', 'run', 1), e('TestAlpha/skip', 'skip', 2, 1),
                   e('TestAlpha', 'pass', 3, 3), e('TestBeta', 'skip', 4, 4)]
        self.assertEqual(hints.cli_durations(records, NAMES), {'TestAlpha': 3, 'TestBeta': 4})
        for replacement in ('fail', 'cont', 'pause'):
            broken = copy.deepcopy(records)
            broken[-1]['Action'] = replacement
            with self.assertRaises(hints.runner.Failure): hints.cli_durations(broken, NAMES)
        with self.assertRaises(hints.runner.Failure): hints.cli_durations(records + [records[-1]], NAMES)

    def test_slash_named_parallel_siblings_do_not_invent_a_parent(self):
        e = self.timed
        records = [e('TestAlpha', 'run', 0), e('TestAlpha/private', 'run', 1),
                   e('TestAlpha/private', 'pause', 2), e('TestAlpha/private/child', 'run', 3),
                   e('TestAlpha/private/child', 'pause', 4), e('TestAlpha/private', 'cont', 20),
                   e('TestAlpha/private', 'pass', 21, 2), e('TestAlpha/private/child', 'cont', 22),
                   e('TestAlpha/private/child', 'pass', 24, 3), e('TestAlpha', 'pass', 24, 5)]
        self.assertEqual(hints.cli_durations(records, ['TestAlpha']), {'TestAlpha': 8})

    def test_rfc3339_nanoseconds_parse_without_float_rounding(self):
        self.assertEqual(hints.stamp({'Time': '2026-01-01T00:00:00.123456789Z'}) -
                         hints.stamp({'Time': '2026-01-01T00:00:00Z'}), hints.Decimal('.123456789'))
        for fractional in ('1', '1234', '1234567'):
            hints.stamp({'Time': f'2026-01-01T00:00:00.{fractional}Z'})
        for bad in ('2026-01-01T00:00:00.1234567890Z', '2026-02-30T00:00:00Z', '2026-01-01T25:00:00Z'):
            with self.assertRaises(hints.runner.Failure): hints.stamp({'Time': bad})

    def test_failed_manifest_rejected(self):
        self.mutate(lambda m: m.update(success=False))
        with self.assertRaises(hints.runner.Failure): self.generate()

    def test_mixed_source_rejected(self):
        self.mutate(lambda m: m['catalog'].update(source='other'))
        with self.assertRaises(hints.runner.Failure): self.generate()

    def test_settings_mismatch_rejected(self):
        self.mutate(lambda m: m['settings'].update(cpus=8))
        with self.assertRaises(hints.runner.Failure): self.generate()

    def test_manifest_missing_assignment_rejected(self):
        self.mutate(lambda m: m['coverage'].update(cli=[]))
        with self.assertRaises(hints.runner.Failure): self.generate()

    def test_actual_missing_cli_terminal_rejected(self):
        path = self.root / '0' / '0' / 'cli.json'
        rows = hints.json_stream(path.read_text())
        stream(path, [r for r in rows if r.get('Test') != 'TestAlpha' or r['Action'] != 'pass'])
        with self.assertRaises(hints.runner.Failure): self.generate()

    def test_actual_failure_rejected_despite_success_manifest(self):
        stream(self.root / '0' / '0' / 'ordinary.json', [{'Package': PKGS[0], 'Action': 'fail'}])
        with self.assertRaises(hints.runner.Failure): self.generate()

    def test_missing_shard_rejected(self):
        (self.root / '0' / '1' / 'manifest.json').unlink()
        with self.assertRaises(hints.runner.Failure): self.generate()

    def test_discovery_mismatch_rejected(self):
        stream(self.root / '0' / '0' / 'listing.json', [{'Package': CLI, 'Action': 'output', 'Output': 'TestOther\n'}, {'Package': CLI, 'Action': 'pass'}])
        with self.assertRaises(hints.runner.Failure): self.generate()

    def test_duplicate_samples_rejected(self):
        with self.assertRaises(hints.runner.Failure):
            self.generate([self.samples[0], self.samples[0]])

    def test_object_stream_parser_matches_pretty_and_line_objects(self):
        self.assertEqual(hints.json_stream(' \n{ "one": 1 }\n{\n "two": 2\n}\n'), [{'one': 1}, {'two': 2}])
        with self.assertRaises(hints.runner.Failure): hints.json_stream('[1, 2]')

    def test_actual_body_proof_and_tier_required(self):
        for change in (lambda m: m['body_evidence'].update(actual=[]),
                       lambda m: m.update(tier='daily')):
            original = (self.root / '0' / '0' / 'manifest.json').read_text()
            self.mutate(change)
            with self.assertRaises(hints.runner.Failure): self.generate()
            (self.root / '0' / '0' / 'manifest.json').write_text(original)

    def test_provenance_hashes_actual_bytes_not_only_duration(self):
        before = self.generate()
        p = self.root / '0' / '0' / 'ordinary.json'
        p.write_text(p.read_text() + '\n')
        after = self.generate()
        self.assertEqual(before['packages'], after['packages'])
        self.assertNotEqual(before['provenance']['samples'], after['provenance']['samples'])

    def test_parser_contract_restored_after_failure_and_success(self):
        original = hints.runner.json_stream
        self.generate()
        self.assertIs(hints.runner.json_stream, original)
        self.mutate(lambda m: m.update(success=False))
        with self.assertRaises(hints.runner.Failure): self.generate()
        self.assertIs(hints.runner.json_stream, original)

    def test_imported_source_uses_request_and_restores_ci_event_source(self):
        for event_source in (None, 'current-ci-merge-source'):
            with mock.patch.dict(os.environ):
                if event_source is None:
                    os.environ.pop('GITHUB_SHA', None)
                else:
                    os.environ['GITHUB_SHA'] = event_source
                self.assertEqual(self.generate()['provenance']['source'], 'accepted-source')
                self.assertEqual(os.environ.get('GITHUB_SHA'), event_source)
                with self.assertRaises(hints.runner.Failure):
                    hints.generate(self.samples, 'wrong-request-source', SETTINGS, PKGS, NAMES)
                self.assertEqual(os.environ.get('GITHUB_SHA'), event_source)

    def test_parent_elapsed_larger_than_envelope_and_missing_start(self):
        rows = [{'Test': 'TestAlpha', 'Action': 'run', 'Time': '2026-01-01T00:00:00Z'},
                {'Test': 'TestAlpha', 'Action': 'pass', 'Elapsed': 4, 'Time': '2026-01-01T00:00:01Z'}]
        self.assertEqual(hints.cli_durations(rows, ['TestAlpha']), {'TestAlpha': 4})
        with self.assertRaises(hints.runner.Failure): hints.cli_durations(rows[1:], ['TestAlpha'])
        rows[1]['Time'] = '2025-12-31T23:59:59Z'
        with self.assertRaises(hints.runner.Failure): hints.cli_durations(rows, ['TestAlpha'])

    def test_cli_timestamps_require_timezone_and_valid_text(self):
        for value in ('2026-01-01T00:00:00', True, None, 'bad'):
            with self.assertRaises(hints.runner.Failure): hints.stamp({'Time': value})
        self.assertEqual(hints.stamp({'Time': '2026-01-01T00:00:00Z'}),
                         hints.stamp({'Time': '2026-01-01T01:00:00+01:00'}))


if __name__ == '__main__':
    unittest.main()
