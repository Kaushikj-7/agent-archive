"""Exercise version isolation and failure propagation without building Git/Go."""
import hashlib
import io
import json
import os
from pathlib import Path
import subprocess
import sys
import tarfile
import tempfile
import unittest
from unittest import mock

import run_git_identity_versions as runner


class GitVersionsTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name).resolve()
        self.bin = self.root/'bin'
        self.bin.mkdir()
        self.git = self.bin/'git'
        self.git.write_text('#!/bin/sh\ncase "$1" in\n--version) echo "git version runner-fixture";;\nrev-parse) echo '+('a'*40)+';;\n*) exit 2;;\nesac\n')
        self.git.chmod(0o755)
        go = self.bin/'go'
        go.write_text('#!'+sys.executable+'\n'+'''
import json,os,subprocess,sys
from pathlib import Path
names=('PATH','HOME','XDG_CONFIG_HOME','TMPDIR','TMP','TEMP','GOMAXPROCS')
record={name:os.environ[name] for name in names}
record['argv']=sys.argv[1:]
record['git']=subprocess.check_output(['git','--version'],text=True).strip()
with open(os.environ['TRACE'],'a') as handle:handle.write(json.dumps(record)+'\\n')
if os.environ.get('FAIL_VERSION') and os.environ['FAIL_VERSION'] in os.environ['HOME']:
    sys.exit(1)
''')
        go.chmod(0o755)
        self.cache = self.root/'cache'
        cache = self.cache/'aa-pinned-git'
        cache.mkdir(parents=True)
        for name, (tag, commit, version) in runner.pinned_git.PINS.items():
            archive = cache/(name+'.tar.gz')
            data = ('#!/bin/sh\necho "'+version+'"\n').encode()
            with tarfile.open(archive, 'w:gz') as handle:
                item = tarfile.TarInfo('bin/git')
                item.mode = 0o755
                item.size = len(data)
                handle.addfile(item, io.BytesIO(data))
            receipt = {'tag': tag, 'commit': commit, 'expected': version,
                       'flags': runner.pinned_git.FLAGS,
                       'sha256': hashlib.sha256(archive.read_bytes()).hexdigest()}
            (cache/(name+'.json')).write_text(json.dumps(receipt))
        self.trace = self.root/'trace.jsonl'
        self.env = dict(os.environ, PATH=str(self.bin)+':/usr/bin:/bin', GOCACHE=str(self.cache),
                        GITHUB_SHA='a'*40, TRACE=str(self.trace), PYTHONDONTWRITEBYTECODE='1')
        self.env.pop('FAIL_VERSION', None)
        self.output = self.root/'output'

    def execute(self):
        result = subprocess.run([sys.executable, '-B', str(Path(runner.__file__).resolve()),
                                 '--output', str(self.output)], env=self.env,
                                capture_output=True, text=True, timeout=30)
        summary = json.loads((self.output/'summary.json').read_text())
        return result, summary

    def test_real_supervisor_runs_all_exact_versions_in_separate_environments(self):
        result, summary = self.execute()
        self.assertEqual(result.returncode, 0, result.stderr+result.stdout)
        self.assertTrue(summary['success'])
        self.assertEqual([v['name'] for v in summary['versions']], list(runner.VERSIONS))
        self.assertEqual(summary['baseline_git'], str(self.git))
        records = [json.loads(line) for line in self.trace.read_text().splitlines()]
        self.assertEqual(len(records), 6)
        homes = set()
        for index, name in enumerate(runner.VERSIONS):
            lane = records[index*2:index*2+2]
            self.assertEqual([r['argv'] for r in lane], [list(c[1:]) for c in runner.COMMANDS])
            expected = runner.pinned_git.PINS[name][2] if name in runner.pinned_git.PINS else 'git version runner-fixture'
            self.assertEqual([r['git'] for r in lane], [expected, expected])
            for record in lane:
                self.assertEqual(record['GOMAXPROCS'], '2')
                self.assertEqual(record['HOME'], str(self.output/name/'home'))
                self.assertEqual(record['XDG_CONFIG_HOME'], str(self.output/name/'xdg'))
                self.assertEqual(record['TMPDIR'], str(self.output/name/'tmp'))
                self.assertEqual(record['TMP'], record['TMPDIR'])
                self.assertEqual(record['TEMP'], record['TMPDIR'])
            homes.add(lane[0]['HOME'])
            receipt = json.loads((self.output/name/'receipt.json').read_text())
            self.assertTrue(receipt['success'])
            self.assertEqual(receipt['pin'], list(runner.pinned_git.PINS[name]) if name in runner.pinned_git.PINS else None)
        self.assertEqual(len(homes), 3)
        self.assertEqual(records[-1]['PATH'], self.env['PATH'])
        self.assertNotIn('upstream-', records[-1]['PATH'])

    def test_one_failing_version_still_executes_both_later_versions(self):
        self.env['FAIL_VERSION'] = runner.VERSIONS[0]
        result, summary = self.execute()
        self.assertEqual(result.returncode, 1)
        self.assertFalse(summary['success'])
        self.assertEqual([v['success'] for v in summary['versions']], [False, True, True])
        records = [json.loads(line) for line in self.trace.read_text().splitlines()]
        self.assertEqual(len(records), 5)  # Preserve first-command failure within one version.
        self.assertEqual(records[-1]['git'], 'git version runner-fixture')

    def test_wrong_selected_version_fails_before_any_test_body(self):
        directory = self.root/'lane'
        directory.mkdir()
        wrong = self.root/'wrong'
        wrong.mkdir()
        (wrong/'git').write_text('#!/bin/sh\necho wrong-version\n')
        (wrong/'git').chmod(0o755)
        with mock.patch.dict(os.environ, self.env, clear=True), mock.patch.object(runner.pinned_git, 'ensure', return_value=wrong):
            status = runner.version_lane(runner.VERSIONS[0], directory, str(self.git), self.env['PATH'], 'git version runner-fixture', 'a'*40)
        self.assertEqual(status, 1)
        report = json.loads((directory/'receipt.json').read_text())
        self.assertFalse(report['success'])
        self.assertEqual(report['commands'], [])
        self.assertIn('version differs', report['error'])
        self.assertFalse(self.trace.exists())

    def test_pinned_preparation_error_is_a_failed_version_receipt(self):
        directory = self.root/'lane'
        directory.mkdir()
        with mock.patch.dict(os.environ, self.env, clear=True), mock.patch.object(runner.pinned_git, 'ensure', side_effect=ValueError('invalid pinned receipt')):
            status = runner.version_lane(runner.VERSIONS[1], directory, str(self.git), self.env['PATH'], 'git version runner-fixture', 'a'*40)
        self.assertEqual(status, 1)
        self.assertEqual(json.loads((directory/'receipt.json').read_text())['error'], 'invalid pinned receipt')

    def test_existing_output_is_not_reused_as_a_verdict(self):
        self.output.mkdir()
        with self.assertRaisesRegex(ValueError, 'new directory'):
            runner.run_all(self.output)

    def test_timeout_stops_process_group_and_continues_remaining_versions(self):
        calls = []
        class Process:
            def __init__(self, argv, **kwargs):
                self.argv = argv
                calls.append(argv[argv.index('--version')+1])
            def wait(self, timeout):
                raise subprocess.TimeoutExpired(self.argv, timeout)
        with mock.patch.dict(os.environ, self.env, clear=True), mock.patch.object(runner.subprocess, 'Popen', side_effect=Process), mock.patch.object(runner, 'stop') as stop, mock.patch('builtins.print'):
            # Capture baseline probes before mocking Popen (check_output uses it).
            with mock.patch.object(runner.subprocess, 'check_output', side_effect=['git version runner-fixture\n', 'a'*40+'\n']):
                status = runner.run_all(self.output)
        self.assertEqual(status, 1)
        self.assertEqual(calls, list(runner.VERSIONS))
        self.assertEqual(stop.call_count, 3)
        summary = json.loads((self.output/'summary.json').read_text())
        self.assertTrue(all(not item['success'] for item in summary['versions']))

    @unittest.skipUnless(sys.platform == 'linux', 'production Linux process-group qualification')
    def test_cancel_stops_actual_owned_process_group(self):
        program = ('import subprocess,sys,time; '
                   'p=subprocess.Popen([sys.executable,"-c","import time;time.sleep(60)"]); '
                   'print(p.pid,flush=True); time.sleep(60)')
        process = subprocess.Popen([sys.executable, '-c', program], stdout=subprocess.PIPE,
                                   text=True, start_new_session=True)
        try:
            child = int(process.stdout.readline())
            runner.stop(process)
            self.assertIsNotNone(process.poll())
            state = subprocess.run(['ps', '-o', 'stat=', '-p', str(child)], capture_output=True,
                                   text=True, timeout=5).stdout.strip()
            self.assertTrue(not state or state.startswith('Z'), state)
        finally:
            try:
                runner.stop(process)
            finally:
                if process.poll() is None:
                    process.kill()
                process.wait()
                process.stdout.close()


if __name__ == '__main__':
    unittest.main()
