#!/usr/bin/env python3
"""Fail-closed report checks and a complete isolated benchmark rehearsal."""
import copy
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest import mock

import benchmark_shared_verify as benchmark
from test_ci_workflow import ROOT, jobs, job_key


class BenchmarkTest(unittest.TestCase):
    def report(self):
        plan = {'rerun_checks': True,
                'checks': [{'id': name} for name in sorted(benchmark.CHECKS)]}
        report = {'version': 1, 'status': 'passed', 'plan': copy.deepcopy(plan),
                  'results': [{'id': name, 'status': 'passed', 'cache': {
                      'status': 'disabled' if name in ('modules', 'vulnerabilities') else 'fresh'}}
                              for name in sorted(benchmark.CHECKS)]}
        return report, plan

    def test_missing_duplicate_failed_cached_and_changed_plan_are_rejected(self):
        report, plan = self.report()
        benchmark.validate_report(report, plan)
        mutations = [lambda r: r['results'].pop(),
                     lambda r: r['results'].append(copy.deepcopy(r['results'][0])),
                     lambda r: r['results'][0].update(status='failed'),
                     lambda r: r['results'][0]['cache'].update(status='hit'),
                     lambda r: r['results'][0]['cache'].update(status='unavailable'),
                     lambda r: r['results'][0].pop('cache'),
                     lambda r: r['plan'].update(rerun_checks=False),
                     lambda r: r.update(status='cancelled')]
        for mutate in mutations:
            value = copy.deepcopy(report)
            mutate(value)
            with self.subTest(value=value), self.assertRaises(ValueError):
                benchmark.validate_report(value, plan)

    def test_pinned_forced_fresh_outcomes_require_an_explicit_rerun_plan(self):
        # Observed in API-digest-verified artifact 11943760660, run 38669468388:
        # four CacheFresh executions, and uncached modules/vulnerabilities.
        report, plan = self.report()
        self.assertEqual([item['cache']['status'] for item in report['results']].count('fresh'), 4)
        benchmark.validate_report(report, plan)
        for value in (False, None, 1):
            changed = copy.deepcopy(plan)
            if value is None:
                changed.pop('rerun_checks')
            else:
                changed['rerun_checks'] = value
            report['plan'] = copy.deepcopy(changed)
            with self.subTest(rerun=value), self.assertRaises(ValueError):
                benchmark.validate_report(report, changed)
        report, plan = self.report()
        for item in report['results']:
            item['cache']['status'] = 'miss'
        benchmark.validate_report(report, plan)

    def test_cache_fingerprint_rejects_aliases_and_oversize(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / 'object').write_bytes(b'abc')
            first = benchmark.tree_digest(root)
            (root / 'object').write_bytes(b'abd')
            self.assertNotEqual(benchmark.tree_digest(root), first)
            with self.assertRaisesRegex(ValueError, 'bound'):
                benchmark.tree_digest(root, limit=2)
            (root / 'alias').symlink_to(root / 'object')
            with self.assertRaisesRegex(ValueError, 'symlink'):
                benchmark.tree_digest(root)

    def test_small_balanced_sample_remains_diagnostic(self):
        values = [{'jobs': jobs, 'seconds': seconds}
                  for jobs, seconds in zip(benchmark.ORDER, [100, 80, 90, 110])]
        result = benchmark.summarize(values)
        self.assertEqual(result['median_seconds'], {'4': 105, '2': 85})
        self.assertEqual(result['production_jobs'], 4)
        self.assertIn('not a p95', result['qualification'])
        with self.assertRaises(ValueError):
            benchmark.summarize(values[:2])

    def test_manual_job_has_no_normal_pr_cost_or_policy_replacement(self):
        workflow = (ROOT / '.github/workflows/levenshtein.yml').read_text()
        found = jobs(workflow)
        self.assertEqual(job_key(found['checker-benchmark'], 'if'),
                         "github.event_name == 'workflow_dispatch' && inputs.benchmark-checker-jobs")
        self.assertEqual(job_key(found['checker-benchmark'], 'runs-on'), 'ubuntu-24.04')
        self.assertIn('default: false', workflow)
        self.assertIn('--jobs 4', found['verify'])
        self.assertIn('native-release-vulnerabilities', found)
        self.assertIn('retention-days: 7', found['checker-benchmark'])
        self.assertIn('profile: shared', found['checker-benchmark'])

    def test_full_rehearsal_restores_identical_warm_cache_and_fresh_results(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            source, shared, scratch = (root / name for name in ('product', 'shared', 'scratch'))
            for path in (source, shared, scratch):
                path.mkdir()
            (source / 'cmd').mkdir()
            (source / 'cmd/main.go').write_text('package main\n')
            config = {'environments': {'go': {'executor': 'dagger'}},
                      'runs': {'pre-merge': {'checks': sorted(benchmark.CHECKS)}}}
            (source / 'levenshtein.json').write_text(json.dumps(config))
            verify = shared / 'verify'
            verify.write_text('''#!/usr/bin/env python3
import json, os, pathlib, sys
args=sys.argv
source=pathlib.Path(args[args.index('--source')+1])
config=json.loads((source/'levenshtein.json').read_text())
checks=[{'id': name, 'check': {'environment': 'go'},
         'target': {'inputs':['cmd'], 'discovery':'git'}}
        for name in config['runs']['pre-merge']['checks']]
plan={'checks': checks, 'source': str(source), 'run':'pre-merge',
      'rerun_checks':config['runs']['pre-merge'].get('rerun_checks',False)}
if '--dry-run' in args:
    print(json.dumps(plan)); sys.exit(0)
cache=pathlib.Path(args[args.index('--cache-dir')+1])
assert not cache.exists(), 'verification result cache reused'
cache.mkdir()
xdg=pathlib.Path(os.environ['XDG_CACHE_HOME'])
assert not xdg.exists(), 'XDG reused'
xdg.mkdir()
build=pathlib.Path(os.environ['GOCACHE'])
assert not any(p.name.startswith('variant-') for p in build.iterdir()), 'prior variant leaked'
if 'warmup' in xdg.name:
    (build/'warm-marker').write_text('warm compilation')
else:
    assert (build/'warm-marker').read_text()=='warm compilation'
    (build/('variant-'+xdg.name)).write_text('local action')
assert plan['rerun_checks'] is True
results=[{'id':c['id'],'status':'passed','cache':{'status':
          'disabled' if c['id'] in ('modules','vulnerabilities') else 'fresh'},
          'duration_ms':1,'execution_ms':1} for c in checks]
print(json.dumps({'version':1,'status':'passed','plan':plan,'results':results}))
''')
            verify.chmod(0o755)
            proof = {'tested': 'a' * 40}
            def fake_git(path, *args):
                return benchmark.context.TOOL_PINS['shared'] if path == shared else 'a' * 40
            with (mock.patch.object(benchmark, 'git', side_effect=fake_git),
                  mock.patch.object(benchmark.context, 'source_identity', return_value=proof),
                  mock.patch.object(benchmark.context, 'runtime_context', return_value={'test': True}),
                  mock.patch.object(benchmark.context, 'validate_context'),
                  mock.patch.object(benchmark.staging.subprocess, 'check_output', wraps=benchmark.subprocess.check_output) as run,
                  mock.patch.dict(os.environ, {'GOCACHE': str(root / 'absent-cache')})):
                actual_run = run._mock_wraps
                def listing(command, **kwargs):
                    if command[0] == 'git':
                        return b'cmd/main.go\0'
                    return actual_run(command, **kwargs)
                run.side_effect = listing
                result = benchmark.benchmark(source, shared, root / 'evidence', scratch)
            self.assertEqual(result['status'], 'complete')
            self.assertEqual([item['jobs'] for item in result['observations']], list(benchmark.ORDER))
            self.assertEqual(len({item['baseline']['sha256'] for item in result['observations']}), 1)
            self.assertEqual(list(scratch.iterdir()), [])
            self.assertEqual(len(list((root / 'evidence').rglob('report.json'))), 5)
            self.assertFalse(list((root / 'evidence').rglob('verification')))


if __name__ == '__main__':
    unittest.main()
