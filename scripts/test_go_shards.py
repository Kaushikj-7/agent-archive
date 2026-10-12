"""Behavior checks for discovery, complete coverage and bounded process cleanup."""
import copy
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import tempfile
import time
import unittest

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


class ProcessTest(unittest.TestCase):
    def sleeper(self, directory):
        pidfile = Path(directory)/'pid'
        code = "import subprocess,time; from pathlib import Path; p=subprocess.Popen(['"+sys.executable+"','-c','import time;time.sleep(60)']); Path("+repr(str(pidfile))+").write_text(str(p.pid));time.sleep(60)"
        return [sys.executable, '-c', code], pidfile

    def assert_stopped(self, pidfile):
        if pidfile.exists():
            pid = int(pidfile.read_text())
            # An orphan zombie awaits init reaping but cannot execute or mutate files.
            stat = Path(f'/proc/{pid}/stat')
            self.assertTrue(not stat.exists() or stat.read_text().split()[2] == 'Z')

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

    def test_cancellation_stops_process_group(self):
        with tempfile.TemporaryDirectory() as d:
            sleeper, pidfile = self.sleeper(d)
            code = "import sys,signal,time;sys.path.insert(0,"+repr(str(runner.ROOT/'scripts'))+");import run_go_shard as r;signal.signal(signal.SIGTERM,lambda *args: (_ for _ in ()).throw(r.Failure('cancelled')));r.supervise([("+repr(sleeper)+","+repr(str(Path(d)/'out'))+",None)],time.monotonic()+60)"
            process = subprocess.Popen([sys.executable, '-c', code], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
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
