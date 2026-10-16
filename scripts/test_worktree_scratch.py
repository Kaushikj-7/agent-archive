#!/usr/bin/env python3
"""Synthetic scratch lifecycle tests; never inspect the user's cache root."""
import fcntl
import importlib.util
import json
import os
from pathlib import Path
import shutil
import signal
import subprocess
import sys
import tempfile
import time
import unittest
from unittest import mock


SCRIPT = Path(__file__).with_name('worktree_scratch.py')
spec = importlib.util.spec_from_file_location('scratch', SCRIPT)
scratch = importlib.util.module_from_spec(spec)
spec.loader.exec_module(scratch)


class ScratchTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix='scratch-tests-')
        self.addCleanup(self.temporary.cleanup)
        self.base = Path(self.temporary.name).resolve()
        self.root = self.base / 'owned'

    def invoke(self, *args, env=None):
        return subprocess.run([sys.executable, '-B', str(SCRIPT), '--root', str(self.root),
                               *args], capture_output=True, text=True, timeout=25, env=env)

    def retained(self, code='pass'):
        result = self.invoke('run', '--keep', '--', sys.executable, '-c', code)
        self.assertEqual(result.returncode, 0, result.stderr)
        return next(self.root.iterdir())

    def age(self, path, days=10):
        p = path / 'metadata.json'
        value = json.loads(p.read_text())
        value['finished'] = time.time() - days * 86400
        p.write_text(json.dumps(value))

    @unittest.skipUnless(shutil.which('lsof'), 'automatic cleanup needs lsof')
    def test_success_and_failure_remove_only_owned_scratch(self):
        sentinel = self.base / 'keep.txt'
        sentinel.write_text('keep')
        for code, expected in [('pass', 0), ('raise SystemExit(7)', 7)]:
            result = self.invoke('run', '--', sys.executable, '-c', code)
            self.assertEqual(result.returncode, expected, result.stderr)
            self.assertEqual(list(self.root.iterdir()), [])
        self.assertEqual(sentinel.read_text(), 'keep')

    def test_normal_runs_share_default_and_isolation_requires_reason(self):
        env = dict(os.environ, GOCACHE='/unused/per-task-cache', GOMODCACHE='/preserved/modules')
        result = self.invoke('run', '--keep', '--', sys.executable, '-c',
                             'import os,json; print(json.dumps(dict(os.environ)))', env=env)
        self.assertEqual(result.returncode, 0, result.stderr)
        value = json.loads(result.stdout)
        self.assertNotIn('GOCACHE', value)
        self.assertEqual(value['GOMODCACHE'], '/preserved/modules')
        self.assertTrue(value['TMPDIR'].startswith(str(self.root)))
        self.assertEqual(value['TMPDIR'], value['GOTMPDIR'])
        result = self.invoke('run', '--isolated-go-cache', '--', sys.executable, '-c', 'pass')
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('requires --reason', result.stderr)
        result = self.invoke('run', '--keep', '--isolated-go-cache', '--reason', 'cold benchmark',
                             '--', sys.executable, '-c', 'import os; print(os.environ["GOCACHE"])')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(result.stdout.strip().startswith(str(self.root)))
        metadata = json.loads((Path(result.stdout.strip()).parent / 'metadata.json').read_text())
        self.assertEqual(metadata['reason'], 'cold benchmark')

    @unittest.skipUnless(shutil.which('lsof'), 'pruning needs lsof')
    def test_prune_defaults_to_preview_and_observes_age(self):
        path = self.retained()
        result = self.invoke('prune', '--apply')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(path.exists())
        self.age(path)
        result = self.invoke('prune')
        self.assertIn('WOULD REMOVE', result.stdout)
        self.assertTrue(path.exists())
        result = self.invoke('prune', '--apply')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse(path.exists())

    def test_locked_run_survives_prune(self):
        path = self.retained()
        self.age(path)
        with (path / 'lease').open('r+') as lock:
            fcntl.flock(lock, fcntl.LOCK_EX)
            result = self.invoke('prune', '--apply', '--older-than-days', '0')
            self.assertIn('ACTIVE', result.stdout)
            self.assertTrue(path.exists())

    def test_unrecognized_and_symlink_entries_are_preserved(self):
        self.root.mkdir(mode=0o700)
        outside = self.base / 'outside'
        outside.mkdir()
        (outside / 'important').write_text('keep')
        (self.root / 'run-abcdefgh').symlink_to(outside, target_is_directory=True)
        unknown = self.root / 'legacy-build-cache'
        unknown.mkdir()
        result = self.invoke('prune', '--apply', '--older-than-days', '0')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual((outside / 'important').read_text(), 'keep')
        self.assertTrue(unknown.is_dir())
        self.assertTrue((self.root / 'run-abcdefgh').is_symlink())

    def test_metadata_cannot_redirect_cleanup_or_hide_live_group(self):
        path = self.retained()
        self.age(path)
        metadata = json.loads((path / 'metadata.json').read_text())
        metadata['pgid'] = os.getpgrp()
        (path / 'metadata.json').write_text(json.dumps(metadata))
        result = self.invoke('prune', '--apply', '--older-than-days', '0')
        self.assertIn('ACTIVE', result.stdout)
        self.assertTrue(path.exists())
        for malformed in ([], {'format': 'other'}, dict(metadata, finished=float('nan'))):
            (path / 'metadata.json').write_text(json.dumps(malformed))
            result = self.invoke('prune', '--apply', '--older-than-days', '0')
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertIn('SKIP', result.stdout)
            self.assertTrue(path.exists())

    def test_missing_lsof_blocks_pruning(self):
        path = self.retained()
        self.age(path)
        with mock.patch.object(scratch.shutil, 'which', return_value=None):
            busy, reason = scratch.open_elsewhere(path)
        self.assertTrue(busy)
        self.assertIn('unavailable', reason)

    @unittest.skipUnless(shutil.which('lsof'), 'open-file check needs lsof')
    def test_open_files_block_pruning(self):
        path = self.retained()
        self.age(path)
        with (path / 'data').open('w'):
            result = self.invoke('prune', '--apply')
            self.assertEqual(result.returncode, 1, result.stderr)
            self.assertIn('open files', result.stdout)
            self.assertTrue(path.exists())

    def test_root_permissions_and_symlinks_are_rejected(self):
        self.root.mkdir(mode=0o755)
        self.root.chmod(0o755)
        result = self.invoke('status')
        self.assertNotEqual(result.returncode, 0)
        self.root.rmdir()
        outside = self.base / 'outside'
        outside.mkdir(mode=0o700)
        self.root.symlink_to(outside, target_is_directory=True)
        result = self.invoke('status')
        self.assertNotEqual(result.returncode, 0)

    def test_size_budget_and_invalid_thresholds(self):
        path = self.retained()
        (path / 'bytes').write_bytes(b'x' * 8192)
        result = self.invoke('status', '--budget-gib', '0')
        self.assertEqual(result.returncode, 2)
        self.assertIn('OVER BUDGET', result.stderr)
        for argument in ('-1', 'nan', 'inf'):
            result = self.invoke('prune', '--older-than-days', argument)
            self.assertEqual(result.returncode, 2)

    def test_live_background_descendant_retains_scratch(self):
        code = ('import subprocess,sys; '
                'subprocess.Popen([sys.executable,"-c","import time; time.sleep(60)"],'
                'stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)')
        result = self.invoke('run', '--', sys.executable, '-c', code)
        self.assertEqual(result.returncode, 0, result.stderr)
        path = next(self.root.iterdir())
        metadata = json.loads((path / 'metadata.json').read_text())
        try:
            self.assertIn('live descendants', result.stderr)
            self.age(path)
            result = self.invoke('prune', '--apply')
            self.assertIn('ACTIVE', result.stdout)
            self.assertTrue(path.exists())
        finally:
            try:
                os.killpg(metadata['pgid'], signal.SIGKILL)
            except ProcessLookupError:
                pass

    @unittest.skipUnless(shutil.which('lsof'), 'automatic cleanup needs lsof')
    def test_cancellation_forwards_signal_and_cleans_scratch(self):
        marker = self.base / 'ready'
        process = subprocess.Popen([sys.executable, '-B', str(SCRIPT), '--root', str(self.root),
                                    'run', '--', sys.executable, '-c',
                                    f'from pathlib import Path; import time; Path({str(marker)!r}).touch(); time.sleep(60)'],
                                   stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        try:
            deadline = time.monotonic() + 10
            while not marker.exists() and process.poll() is None and time.monotonic() < deadline:
                time.sleep(0.02)
            self.assertTrue(marker.exists())
            process.send_signal(signal.SIGTERM)
            _out, error = process.communicate(timeout=15)
            self.assertEqual(process.returncode, 143, error)
            self.assertEqual(list(self.root.iterdir()), [])
        finally:
            if process.poll() is None:
                process.kill()
                process.communicate()


if __name__ == '__main__':
    unittest.main()
