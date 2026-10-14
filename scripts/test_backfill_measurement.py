#!/usr/bin/env python3
"""Synthetic validation of matched backfill measurement evidence."""
import contextlib
import importlib.util
import io
import json
from pathlib import Path
import sys
import tempfile
import types
import unittest
from unittest.mock import patch


def module(name):
    spec = importlib.util.spec_from_file_location(name, Path(__file__).with_name(name + '.py'))
    result = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(result)
    return result


class MeasurementEvidence(unittest.TestCase):
    def compare(self, edit=None, samples=5):
        fields = ['host', 'logical_cpus', 'gomaxprocs', 'fixture_version', 'toolchain',
                  'planning_workers', 'default_adapter_workers', 'byte_budget',
                  'sessions', 'selected', 'imported', 'skipped', 'input_bytes',
                  'artifact_sha256', 'elapsed_seconds', 'command_cpu_seconds',
                  'command_peak_rss_at_return_bytes']
        rows = [dict.fromkeys(fields, 1) | {'scenario': 'recent', 'mode': 'import',
                'sample': i, 'state': 'cold', 'trace': False} for i in range(samples)]
        candidate = [row.copy() for row in rows]
        if edit:
            edit(candidate)
        with tempfile.TemporaryDirectory() as directory:
            paths = [Path(directory) / name for name in ['before', 'after']]
            for path, data in zip(paths, [rows, candidate]):
                path.write_text(''.join(json.dumps(row) + '\n' for row in data))
            with patch.object(sys, 'argv', ['compare', *map(str, paths)]), contextlib.redirect_stdout(io.StringIO()):
                module('compare_backfill').main()

    def test_equivalent_unique_samples(self):
        self.compare()

    def test_empty_inputs_rejected(self):
        with self.assertRaisesRegex(SystemExit, 'At least five matched samples'):
            self.compare(samples=0)

    def test_too_few_matched_samples_rejected(self):
        with self.assertRaisesRegex(SystemExit, 'At least five samples'):
            self.compare(samples=4)

    def test_duplicate_samples_rejected(self):
        with self.assertRaisesRegex(SystemExit, 'Duplicate sample'):
            self.compare(lambda rows: rows[-1].update(sample=0))

    def test_unmatched_samples_rejected(self):
        with self.assertRaisesRegex(SystemExit, 'Unmatched sample'):
            self.compare(lambda rows: rows[-1].update(sample=8))

    def test_different_host_rejected(self):
        with self.assertRaisesRegex(SystemExit, 'Non-equivalent host'):
            self.compare(lambda rows: rows[-1].update(host='other-host'))

    def test_runner_does_not_inherit_fixture_flags(self):
        runner = module('measure_backfill')
        environments = []

        def child(_args, env, stdout, **_kwargs):
            environments.append(env)
            stdout.write(b'AA_BACKFILL_RESULT {"elapsed_seconds": 1}\n')
            return types.SimpleNamespace(pid=123, returncode=None)

        usage = types.SimpleNamespace(ru_utime=1, ru_stime=0, ru_maxrss=1)
        with tempfile.TemporaryDirectory() as directory:
            args = ['measure', '/synthetic.test', directory, '--samples', '1', '--scenario', 'recent']
            with patch.object(sys, 'argv', args), patch.dict(runner.os.environ, {'AA_BACKFILL_TRACE': '1', 'AA_BACKFILL_SMALL': '1'}), patch.object(runner.subprocess, 'Popen', child), patch.object(runner.os, 'wait4', return_value=(123, 0, usage)), patch.object(runner.platform, 'platform', return_value='synthetic-host'), contextlib.redirect_stdout(io.StringIO()):
                runner.main()
        self.assertEqual(len(environments), 2)
        for env in environments:
            self.assertNotIn('AA_BACKFILL_TRACE', env)
            self.assertNotIn('AA_BACKFILL_SMALL', env)


if __name__ == '__main__':
    unittest.main()
