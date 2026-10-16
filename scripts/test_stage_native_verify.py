import copy
import importlib.util
import json
import os
import sys
from unittest import mock
from pathlib import Path
import subprocess
import tempfile
import unittest

spec = importlib.util.spec_from_file_location('stage_native_verify', Path(__file__).with_name('stage_native_verify.py'))
staging = importlib.util.module_from_spec(spec)
spec.loader.exec_module(staging)


class NativeStageTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.source = self.root / 'source'
        self.source.mkdir()
        subprocess.run(['git', 'init', '-q', str(self.source)], check=True)
        self.config = {'environments': {'go': {'executor': 'dagger'}}, 'checks': {}, 'runs': {}}
        self.plan = {'checks': [{'check': {'environment': 'go'}, 'target': {
            'inputs': ['go.mod', 'cmd', 'schemas', '.github', 'optional'],
            'exclude': ['cmd/excluded'], 'discovery': 'git'}}]}

    def tracked(self, path, content='original'):
        item = self.source / path
        item.parent.mkdir(parents=True, exist_ok=True)
        item.write_text(content)
        subprocess.run(['git', '-C', str(self.source), 'add', '--', path], check=True)
        return item

    def stage(self):
        return staging.stage(self.source, self.root / 'staged', copy.deepcopy(self.config), self.plan)

    def test_declared_inputs_preserve_current_content_deletions_and_private_filter(self):
        modified = self.tracked('cmd/main.go')
        modified.write_text('modified')
        self.tracked('cmd/deleted.go').unlink()
        for path in ['go.mod', '.github/workflows/test.yml', 'schemas/a.json', 'cmd/.env.example',
                     'cmd/.env', 'cmd/.env.secret', 'cmd/excluded/skip.go', 'scripts/outside.go']:
            self.tracked(path)
        files = self.stage()
        self.assertEqual(files, ['.github/workflows/test.yml', 'cmd/.env.example', 'cmd/main.go', 'go.mod', 'schemas/a.json'])
        self.assertEqual((self.root / 'staged/cmd/main.go').read_text(), 'modified')
        self.assertFalse((self.root / 'staged/.git').exists())
        self.assertFalse((self.root / 'staged/scripts').exists())
        self.assertEqual(self.config['environments']['go']['executor'], 'dagger')

    def test_symlink_and_ancestor_alias_fail_before_copy(self):
        item = self.tracked('cmd/main.go')
        item.unlink()
        item.symlink_to('/etc/passwd')
        with self.assertRaisesRegex(ValueError, 'symlink'):
            self.stage()
        self.assertFalse((self.root / 'staged').exists())
        item.unlink()
        item.parent.rmdir()
        item.parent.symlink_to(self.root)
        with self.assertRaisesRegex(ValueError, 'symlink'):
            self.stage()

    def test_unsupported_discovery_and_pattern_fail_closed(self):
        target = self.plan['checks'][0]['target']
        target['discovery'] = 'filesystem'
        with self.assertRaisesRegex(ValueError, 'tracked git'):
            self.stage()
        target['discovery'] = 'git'
        target['inputs'] = ['cmd/**']
        with self.assertRaisesRegex(ValueError, 'literal'):
            self.stage()

    def test_existing_or_nested_destination_is_rejected(self):
        with self.assertRaisesRegex(ValueError, 'outside source'):
            staging.stage(self.source, self.source / 'stage', self.config, self.plan)
        destination = self.root / 'staged'
        destination.mkdir()
        with self.assertRaisesRegex(ValueError, 'new directory'):
            self.stage()

    def test_git_failure_does_not_fall_back_to_copying_workspace(self):
        import shutil
        self.tracked('cmd/main.go')
        shutil.rmtree(self.source / '.git')
        with self.assertRaises(subprocess.CalledProcessError):
            self.stage()
        self.assertFalse((self.root / 'staged').exists())


class NativeCIPolicyTest(unittest.TestCase):
    def setUp(self):
        self.config = json.loads((Path(__file__).resolve().parent.parent/'levenshtein.json').read_text())

    def test_current_event_routes_preserve_full_non_pr_runs_and_scheduled_freshness(self):
        for event in ('pull_request', 'push', 'workflow_dispatch', 'schedule'):
            run = staging.ci_run(event)
            self.assertEqual(run, 'main' if event == 'schedule' else 'pre-merge')
            staging.guard_ci_policy(self.config, run)
            self.assertEqual(set(self.config['runs'][run]['checks']), set(staging.CHECKS))
        self.assertTrue(self.config['runs']['main']['rerun_checks'])
        with self.assertRaisesRegex(ValueError, 'unsupported'):
            staging.ci_run('pull_request_target')

    def test_future_community_environment_check_target_and_run_policy_fail_closed(self):
        changes = [
            lambda c: c.update(rule_modules={'future': {'ref': 'x'}}),
            lambda c: c['environments']['go'].update(env={'GOFLAGS': '-tags=future'}),
            lambda c: c['environments']['go'].update(pass_env=['GH_TOKEN']),
            lambda c: c['environments']['go'].update(tools={'go': 'future'}),
            lambda c: c['checks']['lint'].update(lint={'rule_modules': False}),
            lambda c: c['checks']['vet'].update(kind='command'),
            lambda c: c['targets']['archive']['inputs'].append('future'),
            lambda c: c['runs']['main'].update(rerun_checks=False),
            lambda c: c['runs']['main']['checks'].remove('workflow-security'),
            lambda c: c['runs']['main'].update(future_option=True),
        ]
        for mutate in changes:
            config = copy.deepcopy(self.config)
            mutate(config)
            with self.subTest(config=config), self.assertRaisesRegex(ValueError, 'unsupported'):
                staging.guard_ci_policy(config, 'main')

    def test_plan_cannot_omit_duplicate_or_skip_community_checks(self):
        plan = {'checks': [{'id': name} for name in staging.CHECKS], 'rerun_checks': True}
        staging.guard_ci_plan(plan, 'main')
        for mutate in (lambda p: p['checks'].pop(),
                       lambda p: p['checks'][-1].update(id='lint'),
                       lambda p: p['checks'][0].update(rule_modules=[{'ref': 'future'}]),
                       lambda p: p.update(rerun_checks=False)):
            broken = copy.deepcopy(plan)
            mutate(broken)
            with self.assertRaisesRegex(ValueError, 'planner'):
                staging.guard_ci_plan(broken, 'main')

    def test_actual_cli_staging_selects_full_non_pr_and_scopes_only_pr(self):
        for event in ('push', 'workflow_dispatch', 'schedule', 'pull_request'):
            with self.subTest(event=event), tempfile.TemporaryDirectory() as raw:
                root = Path(raw)
                source = root/'source'
                source.mkdir()
                subprocess.run(['git', 'init', '-q', str(source)], check=True)
                (source/'levenshtein.json').write_text(json.dumps(self.config))
                (source/'go.mod').write_text('module example.com/test\n')
                subprocess.run(['git', '-C', str(source), 'add', '.'], check=True)
                output = root/'output'
                run = staging.ci_run(event)
                plan = {'checks': [{'id': name, 'check': self.config['checks'][name],
                                   'target': self.config['targets'][self.config['checks'][name]['target']]}
                                  for name in staging.CHECKS], 'rerun_checks': event == 'schedule'}
                argv = ['stage_native_verify.py', '--source', str(source), '--shared', str(root/'shared'),
                        '--destination', str(root/'staged'), '--ci-event', event]
                original = staging.subprocess.check_output
                def probe(command, **kwargs):
                    if command[0].endswith('/verify'):
                        self.assertEqual(command[1], run)
                        return json.dumps(plan).encode()
                    return original(command, **kwargs)
                with mock.patch.object(sys, 'argv', argv), mock.patch.dict(os.environ, {'GITHUB_OUTPUT': str(output)}), \
                        mock.patch.object(staging.subprocess, 'check_output', side_effect=probe), \
                        mock.patch.object(staging.subprocess, 'run') as validate, \
                        mock.patch.object(staging.scope, 'decision', return_value=('pre-merge', 'full proof fallback')) as scope:
                    staging.main()
                    self.assertEqual(scope.call_count, 1 if event == 'pull_request' else 0)
                    self.assertEqual(validate.call_args.args[0][1], run)
                self.assertEqual(output.read_text(), 'run='+run+'\n')
                staged = json.loads((root/'staged/levenshtein.json').read_text())
                self.assertEqual(staged['runs'][run], self.config['runs'][run])
                self.assertEqual(staged['checks'], self.config['checks'])
                self.assertEqual(staged['environments']['go']['executor'], 'native')
                self.assertEqual(staged['environments']['go']['env']['GOOS'], 'linux')
                self.assertEqual(staged['environments']['go']['env']['GOARCH'], 'amd64')
                self.assertEqual(staged['environments']['go']['env']['CGO_ENABLED'], '1')
                self.assertEqual(staged['environments']['go']['env']['GOMAXPROCS'], '4')
                self.assertEqual(staged['environments']['go']['pass_env'], ['GOCACHE', 'GOMODCACHE'])


if __name__ == '__main__':
    unittest.main()
