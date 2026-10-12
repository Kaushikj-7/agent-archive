"""Behavior checks for discovery, complete coverage and bounded process cleanup."""
import copy
import contextlib
import io
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import tempfile
import time
import unittest
from unittest import mock

import run_go_shard as runner

CLI = runner.CLI


def records(*items):
    return '\n'.join(json.dumps(item) for item in items)


def event(action, package=CLI, **kw):
    return dict(Action=action, Package=package, **kw)


class CatalogTest(unittest.TestCase):
    def test_metadata_keeps_fileless_packages_and_rejects_errors(self):
        items = [dict(ImportPath=n, Dir='/tmp', TestGoFiles=['x_test.go'] if n == CLI else [])
                 for n in (CLI, 'ordinary/a', 'ordinary/b')]
        self.assertEqual(runner.metadata(records(*items)), {CLI: True, 'ordinary/a': False, 'ordinary/b': False})
        for bad in (items + [items[0]], [], [dict(items[0], Error={'Err': 'bad'})] + items[1:]):
            with self.assertRaises(runner.Failure):
                runner.metadata(records(*bad))
        with self.assertRaises(ValueError):
            runner.metadata('{} bad-json')

    def test_listing_includes_fuzz_examples_but_never_duplicates_or_testmain(self):
        listing = [event('output', Output=n+'\n') for n in ('TestA', 'FuzzB', 'ExampleC')]
        self.assertEqual(runner.listed_names(records(*listing, event('pass'))), ['ExampleC', 'FuzzB', 'TestA'])
        for bad in (listing + [listing[0]], [event('output', Output='TestMain\n')],
                    [event('output', Output='TestA/child\n')], []):
            with self.assertRaises(runner.Failure):
                runner.listed_names(records(*bad, event('pass')))
        with self.assertRaises(runner.Failure):
            runner.listed_names(records(*listing, event('skip')))

    def test_integer_partition_unknown_stale_zero_and_ties(self):
        self.assertEqual(runner.partition(['b', 'a', 'c'], {'a': 10, 'b': 10, 'stale': 200}), [['c'], ['a', 'b']])
        self.assertEqual(runner.partition(['b', 'a'], {'a': 0, 'b': 0}), [['a'], ['b']])
        for units, hints in ((['a', 'a'], {}), (['a'], {}), (['a', 'b'], {'a': 1.5}), (['a', 'b'], {'a': -1})):
            with self.assertRaises(runner.Failure):
                runner.partition(units, hints)

    def test_exact_selector_prefix_escaping_and_limits(self):
        pattern = runner.selector(['TestA', 'TestAA', 'ExampleB'])
        import re
        self.assertTrue(re.fullmatch(pattern, 'TestAA'))
        self.assertFalse(re.fullmatch(pattern, 'TestAAA'))
        self.assertIn(r'TestA\.', runner.selector(['TestA.']))
        for names in ([], ['TestA', 'TestA'], ['TestA/child']):
            with self.assertRaises(runner.Failure):
                runner.selector(names)
        with self.assertRaises(runner.Failure):
            runner.selector(['TestLong'], limit=3)

    def test_coverage_nested_fuzz_examples_and_no_test_terminals(self):
        es = [event('pass', Test=n) for n in ('TestA/child', 'TestA', 'FuzzB/seed', 'FuzzB', 'ExampleC')]
        es += [event('skip', Test='TestSkipped'), event('pass')]
        selection = ['TestA', 'FuzzB', 'ExampleC', 'TestSkipped']
        self.assertEqual(runner.coverage(records(*es), {CLI: True}, selection)['cli'], sorted(selection))
        for bad in (es[:-1], es[:-2] + [event('pass')], es+[event('pass')],
                    es+[event('pass', Test='TestOther')], es+[event('pass', Test='TestA')],
                    [event('output', Output='warning: no tests to run\n')]+es):
            with self.assertRaises(runner.Failure):
                runner.coverage(records(*bad), {CLI: True}, selection)
        no_tests = records(event('output', 'ordinary/a', Output='? ordinary/a [no test files]\n'), event('skip', 'ordinary/a'))
        runner.coverage(no_tests, {'ordinary/a': False})
        for text, has_tests in ((records(event('skip', 'ordinary/a')), False), (no_tests, True)):
            with self.assertRaises(runner.Failure):
                runner.coverage(text, {'ordinary/a': has_tests})


class GateTest(unittest.TestCase):
    def test_event_verdict_fail_closed(self):
        pr = {j: {'result': s} for j, s in [('linux-short', 'success'), ('linux-full', 'skipped'), ('script-tests', 'success')]}
        runner.verdict('pull_request', 'refs/pull/1/merge', pr)
        candidate = copy.deepcopy(pr)
        candidate['linux-short']['result'], candidate['linux-full']['result'] = 'skipped', 'success'
        runner.verdict('push', 'refs/heads/release-candidate/x', candidate)
        for event_name, ref, needs in [('push', 'refs/heads/main', pr), ('schedule', '', pr), ('pull_request', '', {}),
                                       ('push', 'refs/heads/release-candidate/', candidate)]:
            with self.assertRaises(runner.Failure):
                runner.verdict(event_name, ref, needs)
        for base, event_name, ref in ((pr, 'pull_request', ''), (candidate, 'push', 'refs/heads/release-candidate/x')):
            for job in base:
                for result in ('failure', 'cancelled', None, 'unexpected'):
                    bad = copy.deepcopy(base);bad[job]['result'] = result
                    with self.assertRaises(runner.Failure):
                        runner.verdict(event_name, ref, bad)

    def test_manifest_reconciliation_requires_both_matching_complete_shards(self):
        catalog = {'source': os.environ.get('GITHUB_SHA', 'abc'), 'packages': {CLI: True, 'a': False, 'b': True},
                   'cli': ['FuzzA', 'ExampleB'], 'assignments': [{'packages': ['a'], 'cli': ['FuzzA']},
                                                              {'packages': ['b'], 'cli': ['ExampleB']}]}
        with tempfile.TemporaryDirectory() as directory:
            paths = []
            for i in range(2):
                path = Path(directory)/str(i)/'manifest.json';path.parent.mkdir();paths.append(path)
                path.write_text(json.dumps({'shard': i, 'catalog': catalog, 'catalog_sha256': runner.digest(catalog),
                                            'success': True, 'coverage': catalog['assignments'][i]}))
            runner.reconcile(directory)
            good = paths[1].read_text()
            for key, value in [('success', False), ('shard', 0), ('coverage', {'packages': [], 'cli': []}),
                               ('catalog_sha256', 'different')]:
                item = json.loads(good);item[key] = value;paths[1].write_text(json.dumps(item))
                with self.assertRaises(runner.Failure):
                    runner.reconcile(directory)
            paths[1].unlink()
            with self.assertRaises(runner.Failure):
                runner.reconcile(directory)


@unittest.skipUnless(sys.platform == 'linux', 'Linux worker uses procfs and child subreaping')
class RunPlanTest(unittest.TestCase):
    def test_complete_runner_executes_each_package_once_and_reconciles_both_bins(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root/'scripts').mkdir()
            (root/'scripts/go_shard_hints.json').write_text(json.dumps({'packages': {runner.GIT_IDENTITY: 10000}, 'cli': {}}))
            bindir = root/'bin';bindir.mkdir()
            command_log = root/'commands.jsonl'
            fake = bindir/'go'
            fake.write_text('#!'+sys.executable+'\n'+r'''import json,os,re,sys
args=sys.argv[1:]
cli='github.com/wangjohn/agent-archive/internal/cli'
git='github.com/wangjohn/agent-archive/internal/gitremote'
packages={cli:True,git:True,'ordinary/a':False,'ordinary/b':False}
with open(os.environ['TASK_COMMAND_LOG'],'a') as f:f.write(json.dumps({'args':args,'cpu':os.environ.get('GOMAXPROCS')})+'\n')
def emit(action,package,**kw):print(json.dumps(dict(Action=action,Package=package,**kw)),flush=True)
if args[0]=='list':
 for name,has_tests in packages.items():print(json.dumps(dict(ImportPath=name,Dir='/fixture',TestGoFiles=['x_test.go'] if has_tests else [])))
elif '-list' in args:
 for name in ['TestA','FuzzB','Example']:emit('output',cli,Output=name+'\n')
 emit('pass',cli)
elif args[0]=='vet':print('vet complete')
elif '-run' in args:
 pattern=args[args.index('-run')+1]
 for name in ['TestA','FuzzB','Example']:
  if re.fullmatch(pattern,name):
   if name!='Example':emit('pass',cli,Test=name+'/child')
   emit('pass',cli,Test=name)
 emit('pass',cli)
else:
 for package in [arg for arg in args if arg in packages]:
  if not packages[package]:
   emit('output',package,Output='? '+package+' [no test files]\n');emit('skip',package)
  else:emit('pass',package,Test='TestGit');emit('pass',package)
''')
            fake.chmod(0o755)
            with mock.patch.object(runner, 'ROOT', root), mock.patch.object(runner.subprocess, 'check_output', return_value='head\n'), mock.patch.dict(os.environ, {'PATH': str(bindir)+os.pathsep+os.environ['PATH'], 'GITHUB_SHA': 'head', 'TASK_COMMAND_LOG': str(command_log)}), contextlib.redirect_stdout(io.StringIO()):
                for index in (0, 1):runner.run(index, root/str(index), 30)
                runner.reconcile(root)
            calls = [json.loads(line) for line in command_log.read_text().splitlines()]
            executed = [call for call in calls if call['args'][0]=='test' and '-list' not in call['args']]
            for package in (runner.GIT_IDENTITY, 'ordinary/a', 'ordinary/b'):
                self.assertEqual(sum(package in call['args'] for call in executed), 1)
            git_call = next(call for call in executed if runner.GIT_IDENTITY in call['args'])
            self.assertNotIn('-run', git_call['args'])
            self.assertEqual(sum(runner.CLI in call['args'] for call in executed), 2)
            first = json.loads((root/'0/manifest.json').read_text())
            self.assertEqual(first['settings']['isolated_packages'], [runner.GIT_IDENTITY])
            self.assertIn('git_identity_invocation_seconds', first['phases'])
            self.assertIn('vet_seconds', first['phases'])
            self.assertFalse((root/'0/ordinary.json').exists(), 'empty batch must not default to testing the module root')


@unittest.skipUnless(sys.platform == 'linux', 'Linux process groups and child subreaping')
class ProcessTest(unittest.TestCase):
    def sleeper(self, directory, escaped=False):
        pidfile = Path(directory)/'pid'
        code = "import subprocess,time; from pathlib import Path; p=subprocess.Popen(['"+sys.executable+"','-c','import time;time.sleep(60)']); Path("+repr(str(pidfile))+").write_text(str(p.pid));time.sleep(60)"
        if escaped:
            code = code.replace(']); Path(', '], start_new_session=True); Path(')
        return [sys.executable, '-c', code], pidfile

    def assert_stopped(self, pidfile):
        if pidfile.exists():
            pid = int(pidfile.read_text())
            self.assertFalse(Path(f'/proc/{pid}').exists(), 'test grandchild must be reaped, including zombies')

    def test_failed_child_and_shared_timeout_stop_sibling_descendants(self):
        for failure in (False, True):
            with tempfile.TemporaryDirectory() as d:
                sleeper, pidfile = self.sleeper(d)
                commands = [(sleeper, Path(d)/'sleep.out', os.environ.copy())]
                if failure:
                    commands.append(([sys.executable, '-c', 'import time;time.sleep(.2);raise SystemExit(7)'], Path(d)/'fail.out', os.environ.copy()))
                start = time.monotonic()
                with self.assertRaises(runner.Failure):
                    runner.supervise(commands, start + .4)
                self.assertLess(time.monotonic()-start, 3)
                self.assert_stopped(pidfile)

    def test_cleanup_preserves_a_preexisting_unrelated_child(self):
        unrelated = subprocess.Popen([sys.executable, '-c', 'import time;time.sleep(60)'], start_new_session=True)
        try:
            with tempfile.TemporaryDirectory() as d:
                sleeper, pidfile = self.sleeper(d, escaped=True)
                with self.assertRaises(runner.Failure):
                    runner.supervise([(sleeper, Path(d)/'out', os.environ.copy())], time.monotonic()+.3)
                self.assert_stopped(pidfile)
                self.assertIsNone(unrelated.poll(), 'cleanup must not signal preexisting children')
        finally:
            unrelated.terminate();unrelated.wait(timeout=3)

    def test_timeout_reaps_grandchild_that_created_another_session(self):
        with tempfile.TemporaryDirectory() as d:
            sleeper, pidfile = self.sleeper(d, escaped=True)
            with self.assertRaises(runner.Failure):
                runner.supervise([(sleeper, Path(d)/'out', os.environ.copy())], time.monotonic()+.3)
            self.assert_stopped(pidfile)

    def test_live_json_failure_stops_commands_that_have_not_exited(self):
        with tempfile.TemporaryDirectory() as d:
            sleeper, pidfile = self.sleeper(d)
            code = "import json,time;time.sleep(.2);print(json.dumps({'Action':'fail','Package':'p','Test':'TestA'}),flush=True);time.sleep(60)"
            commands = [(sleeper, Path(d)/'sleep.out', os.environ.copy()),
                        ([sys.executable, '-c', code, 'test', '-json'], Path(d)/'json.out', os.environ.copy())]
            start = time.monotonic()
            with self.assertRaisesRegex(runner.Failure, 'live Go failure'):
                runner.supervise(commands, start+10)
            self.assertLess(time.monotonic()-start, 3)
            self.assert_stopped(pidfile)

    def test_cancellation_stops_process_group(self):
        with tempfile.TemporaryDirectory() as d:
            sleeper, pidfile = self.sleeper(d)
            code = "import sys,signal,time;sys.path.insert(0,"+repr(str(Path(runner.__file__).parent))+");import run_go_shard as r;signal.signal(signal.SIGTERM,lambda *args: (_ for _ in ()).throw(r.Failure('cancelled')));r.supervise([("+repr(sleeper)+","+repr(str(Path(d)/'out'))+",None)],time.monotonic()+60)"
            process = subprocess.Popen([sys.executable, '-B', '-c', code], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            try:
                end = time.monotonic()+3
                while not pidfile.exists() and time.monotonic()<end:
                    time.sleep(.02)
                self.assertTrue(pidfile.exists())
                process.send_signal(signal.SIGTERM)
                self.assertNotEqual(process.wait(timeout=3), 0)
                self.assert_stopped(pidfile)
            finally:
                if process.poll() is None:
                    process.kill();process.wait()


if __name__ == '__main__':
    unittest.main()
