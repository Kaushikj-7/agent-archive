from pathlib import Path
import os
import tempfile
import unittest
from unittest import mock
import contextlib
import io

from test_ci_workflow import jobs
from test_release_assets import steps

from go_cache_context import CachePlan, InvalidContext
from go_cache_steps import restore_result, verify_restored, worker_success, emit, main, execute_modules
PIN = "55cc8345863c7cc4c66a329aec7e433d2d1c52a9"


class PhaseTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = root = Path(self.temp.name).resolve()
        prefix = 'aa-pr-go-v1-pr42-w0-'+'c'*64+'-'+'d'*64+'-'
        self.plan = CachePlan(prefix+'a'*40, prefix,
                              (str(root/'pr-go-build'), str(root/'pr-go-modules')),
                              {'tested': 'b'*40, 'tree': 'a'*40, 'dependency': 'd'*64})

    def test_module_commands_use_job_budget_instead_of_metadata_probe_deadline(self):
        def slow_download(argv, **kwargs):
            if kwargs['timeout'] <= 31:
                raise TimeoutError('ordinary cold download exceeds metadata deadline')
            return b'download completed'
        with mock.patch('go_cache_steps.subprocess.check_output', side_effect=slow_download):
            self.assertEqual(execute_modules(['go', 'mod', 'download']), b'download completed')

    def test_paths_phase_cleans_before_any_source_or_toolchain_probe(self):
        build, modules = map(Path, self.plan.paths)
        build.mkdir()
        (build/'old').write_text('partial')
        env = {'GITHUB_EVENT_NAME': 'pull_request', 'PR_NUMBER': '42', 'SHARD': '0',
               'GITHUB_WORKSPACE': str(self.root), 'RUNNER_TEMP': str(self.root),
               'GOCACHE': str(build), 'GOMODCACHE': str(modules)}
        with mock.patch.dict(os.environ, env, clear=True), \
                mock.patch('sys.argv', ['go_cache_steps.py', 'paths']), \
                mock.patch('go_cache_steps.source_identity') as source, \
                mock.patch('go_cache_steps.runtime_context') as context:
            self.assertEqual(main(), 0)
        self.assertFalse((build/'old').exists())
        self.assertTrue(modules.is_dir())
        source.assert_not_called()
        context.assert_not_called()

    def test_paths_phase_rejects_symlink_owned_directory_before_tool_use(self):
        build, modules = map(Path, self.plan.paths)
        outside = self.root/'outside'
        outside.mkdir()
        (outside/'keep').write_text('unrelated')
        build.symlink_to(outside, target_is_directory=True)
        env = {'GITHUB_EVENT_NAME': 'pull_request', 'PR_NUMBER': '42', 'SHARD': '0',
               'GITHUB_WORKSPACE': str(self.root), 'RUNNER_TEMP': str(self.root),
               'GOCACHE': str(build), 'GOMODCACHE': str(modules)}
        with contextlib.redirect_stderr(io.StringIO()), mock.patch.dict(os.environ, env, clear=True), \
                mock.patch('sys.argv', ['go_cache_steps.py', 'paths']), \
                mock.patch('go_cache_steps.runtime_context') as context:
            self.assertEqual(main(), 1)
        self.assertEqual((outside/'keep').read_text(), 'unrelated')
        context.assert_not_called()

    def test_silent_service_miss_discards_partial_state_then_verifies(self):
        build = Path(self.plan.paths[0])
        build.mkdir()
        (build/'partial').write_text('incomplete cache download')
        events = []
        result = verify_restored(self.plan, 'success', '', '',
                                 lambda source: events.append('source'),
                                 lambda argv: events.append(argv))
        self.assertEqual(result, 'miss')
        self.assertFalse((build/'partial').exists())
        self.assertEqual(events, ['source', ['go', 'mod', 'download'], ['go', 'mod', 'verify'], 'source'])

    def test_readonly_module_directory_is_cleaned_without_touching_symlink_target(self):
        modules = Path(self.plan.paths[1])
        nested = modules/'module@v1'
        nested.mkdir(parents=True)
        (nested/'go.mod').write_text('module fixture')
        nested.chmod(0o555)
        outside = self.root/'outside'
        outside.mkdir()
        (outside/'keep').write_text('unrelated')
        (modules/'linked').symlink_to(outside, target_is_directory=True)
        verify_restored(self.plan, 'failure', '', '', lambda s: None, lambda c: None)
        self.assertEqual((outside/'keep').read_text(), 'unrelated')
        self.assertFalse(nested.exists())

    def test_exact_prefix_miss_normalization_and_bad_match_rejection(self):
        self.assertEqual(restore_result(self.plan, 'success', self.plan.primary, 'true').state, 'exact')
        self.assertEqual(restore_result(self.plan, 'success', self.plan.prefix+'e'*40, 'false').state, 'prefix')
        for matched, exact in [(self.plan.primary, 'false'), ('foreign', 'false'), ('', 'true'),
                               (self.plan.prefix+'injected\nkey=bad', 'false')]:
            with self.assertRaises(InvalidContext):
                restore_result(self.plan, 'success', matched, exact)

    def test_checksum_failure_cannot_become_save_ready_and_stale_manifest_is_rejected(self):
        def command(argv):
            if argv[-1] == 'verify':
                raise RuntimeError('checksum failure')
        with self.assertRaises(RuntimeError):
            verify_restored(self.plan, 'success', self.plan.primary, 'true', lambda source: None, command)
        good = {'success': True, 'tier': 'pr', 'shard': 0, 'catalog': {'source': 'b'*40}}
        self.assertTrue(worker_success(self.plan, good, lambda source: None, 0))
        for changed in ({'success': False}, {'tier': 'full'}, {'shard': 1}, {'catalog': {'source': 'e'*40}}):
            with self.assertRaises(InvalidContext):
                worker_success(self.plan, dict(good, **changed), lambda source: None, 0)

    def test_action_output_injection_cannot_write_partial_file(self):
        output = self.root/'outputs'
        with self.assertRaises(InvalidContext):
            emit({'primary': 'safe', 'gocache': 'bad\ninjected=true'}, output)
        self.assertFalse(output.exists())

    def test_source_failure_stops_before_any_module_command(self):
        def fail(source):
            raise InvalidContext('source changed')
        command = mock.Mock()
        with self.assertRaises(InvalidContext):
            verify_restored(self.plan, 'success', self.plan.primary, 'true', fail, command)
        command.assert_not_called()

    def test_all_hit_outcomes_run_download_verify_and_guard(self):
        for outcome, matched, exact, expected in (
                ('success', self.plan.primary, 'true', 'exact'),
                ('success', self.plan.prefix+'e'*40, 'false', 'prefix'),
                ('success', '', '', 'miss'), ('failure', '', '', 'error')):
            events = []
            result = verify_restored(self.plan, outcome, matched, exact,
                                     lambda source: events.append('guard'), lambda argv: events.append(argv))
            self.assertEqual(result, expected)
            self.assertEqual(events, ['guard', ['go', 'mod', 'download'], ['go', 'mod', 'verify'], 'guard'])

    def test_cli_fails_closed_before_source_or_go_on_non_pr(self):
        with contextlib.redirect_stderr(io.StringIO()), \
                mock.patch.dict(os.environ, {'GITHUB_EVENT_NAME': 'workflow_dispatch'}, clear=True), \
                mock.patch('sys.argv', ['go_cache_steps.py', 'prepare']), \
                mock.patch('go_cache_steps.source_identity') as source, \
                mock.patch('go_cache_steps.runtime_context') as context:
            self.assertEqual(main(), 1)
        source.assert_not_called()
        context.assert_not_called()


class WorkflowTests(unittest.TestCase):
    def setUp(self):
        root = Path(__file__).resolve().parent.parent
        self.workflows = {name: jobs((root/'.github/workflows'/f'{name}.yml').read_text())
                          for name in ('test', 'extended', 'levenshtein', 'release', 'promote-release')}

    def test_only_pr_short_workers_restore_save_and_setup_go_remains_fresh(self):
        for name, found in self.workflows.items():
            for job, text in found.items():
                for step in steps(text):
                    if 'actions/cache/' in step:
                        self.assertEqual((name, job), ('test', 'linux-short'))
                        self.assertIn('@'+PIN, step)
                    if 'actions/setup-go@' in step:
                        self.assertIn('cache: false', step)
        short = self.workflows['test']['linux-short']
        self.assertIn("if: github.event_name == 'pull_request'", short)
        self.assertIn('shard: [0, 1, 2, 3]', short)
        self.assertIn('fetch-depth: 0', short)
        self.assertLess(short.index('scripts/go_cache_steps.py paths'), short.index('actions/setup-go@'))

    def test_cache_outcomes_never_gate_runner_and_saves_require_current_success(self):
        found = steps(self.workflows['test']['linux-short'])
        runner = next(s for s in found if 'name: Discovered race-short shard' in s)
        self.assertNotIn('        if:', runner)
        restore = next(s for s in found if 'id: go-cache-restore' in s)
        save = next(s for s in found if 'id: go-cache-save' in s)
        for step in (restore, save):
            self.assertIn('${{ runner.temp }}/pr-go-build', step)
            self.assertIn('${{ runner.temp }}/pr-go-modules', step)
            self.assertNotIn('shard-results', step)
        self.assertIn('success() &&', save)
        self.assertIn("steps.go-cache-complete.outputs.save_ready == 'true'", save)
        self.assertIn("steps.go-cache-verify.outputs.restore_state != 'exact'", save)
        self.assertIn('github.event.pull_request.head.repo.full_name == github.repository', save)
        self.assertNotIn('always()', save)
        self.assertTrue(any('scripts/test_go_cache_context.py' in s for s in steps(self.workflows['test']['script-static'])))
        self.assertTrue(any('scripts/test_go_cache_steps.py' in s for s in steps(self.workflows['test']['script-static'])))


if __name__ == '__main__':
    unittest.main()
