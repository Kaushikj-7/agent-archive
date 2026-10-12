"""Git fixtures exercise the actual event/head/merge scoping proof."""
import copy
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

import scope_native_verify as scope


class ScopeTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.source = Path(self.temp.name) / 'source'
        self.source.mkdir()
        self.git('init', '-q', '-b', 'main')
        # Waiting for commit/merge does not wait for detached housekeeping.
        # Keep all writers inside these synchronous fixture subprocesses.
        self.git('config', 'maintenance.auto', 'false')
        self.git('config', 'gc.auto', '0')
        self.git('config', 'user.name', 'Scope fixture')
        self.git('config', 'user.email', 'scope@example.invalid')
        self.config = json.loads((Path(__file__).resolve().parent.parent / 'levenshtein.json').read_text())
        self.write('levenshtein.json', json.dumps(self.config))
        self.write('.github/workflows/a.yml', 'name: fixture\n')
        self.write('scripts/stage.py', 'fixture\n')
        self.write('internal/main.go', 'base\n')
        self.commit()
        self.base = self.sha()
        self.git('checkout', '-q', '-b', 'feature')

    def git(self, *args):
        return subprocess.check_output(['git', '-C', str(self.source), *args], stderr=subprocess.PIPE).decode().strip()

    def write(self, path, text):
        item = self.source / path
        item.parent.mkdir(parents=True, exist_ok=True)
        item.write_text(text)
        return item

    def sha(self):
        return self.git('rev-parse', 'HEAD')

    def commit(self):
        self.git('add', '-A')
        self.git('commit', '-q', '--allow-empty', '-m', 'fixture')

    def merge(self):
        self.commit()
        self.head = self.sha()
        self.git('checkout', '-q', 'main')
        self.git('merge', '-q', '--no-ff', 'feature', '-m', 'tested merge')
        self.tested = self.sha()
        self.event = {'pull_request': {'base': {'sha': self.base}, 'head': {'sha': self.head}}}

    def selected(self, **kwargs):
        return scope.decision(kwargs.get('source', self.source), kwargs.get('config', self.config),
                              kwargs.get('event', self.event), kwargs.get('tested', self.tested))[0]

    def test_go_only_and_docs_keep_four_checks_and_truthful_summary(self):
        self.write('internal/main.go', 'changed\n')
        self.write('docs/example.md', 'docs\n')
        self.merge()
        self.assertEqual(self.selected(), scope.SCOPED_RUN)
        run, reason = scope.decision(self.source, self.config, self.event, self.tested)
        summary = scope.summarize(run, reason)
        self.assertIn('NOT RUN', summary)
        for sha in (self.base, self.head, self.tested):
            self.assertIn(sha, summary)

    def test_docs_only_can_scope(self):
        self.write('docs/example.md', 'docs\n')
        self.merge()
        self.assertEqual(self.selected(), scope.SCOPED_RUN)

    def test_fixture_commits_do_not_launch_background_maintenance(self):
        # A commit can return while detached maintenance still writes packs,
        # racing TemporaryDirectory cleanup. Force eligible tasks and inspect
        # real Git children instead of relying on cleanup timing.
        self.git('config', 'maintenance.loose-objects.enabled', 'true')
        self.git('config', 'maintenance.loose-objects.auto', '-1')
        trace = Path(self.temp.name) / 'git-trace.jsonl'
        with patch.dict(os.environ, GIT_TRACE2_EVENT=str(trace)):
            self.write('internal/main.go', 'changed\n')
            self.merge()
        self.assertEqual(self.selected(), scope.SCOPED_RUN)
        children = [json.loads(line).get('argv', []) for line in trace.read_text().splitlines()
                    if json.loads(line).get('event') == 'child_start']
        self.assertFalse([argv for argv in children
                          if 'maintenance' in argv or 'gc' in argv], children)

    def test_each_relevant_category_forces_full_run(self):
        for path in ('.github/workflows/a.yml', '.github/actions/a/action.yml', 'levenshtein.json',
                     'scripts/new-helper.py', '.gitmodules', 'action.yml', 'actionlint.yaml',
                     'zizmor.yml', '.golangci.yml', '.go-version'):
            with self.subTest(path=path):
                self.git('reset', '--hard', self.base)
                self.git('checkout', '-q', 'feature')
                self.git('reset', '--hard', self.base)
                self.write(path, 'changed\n')
                self.merge()
                self.assertEqual(self.selected(), 'pre-merge')

    def test_deletion_rename_and_mode_changes_force_full_run(self):
        for operation in ('delete', 'rename', 'mode'):
            with self.subTest(operation=operation):
                self.git('reset', '--hard', self.base)
                self.git('checkout', '-q', 'feature')
                self.git('reset', '--hard', self.base)
                path = self.source / '.github/workflows/a.yml'
                if operation == 'delete':
                    path.unlink()
                elif operation == 'rename':
                    self.git('mv', '.github/workflows/a.yml', 'outside.yml')
                else:
                    path.chmod(0o755)
                self.merge()
                self.assertEqual(self.selected(), 'pre-merge')

    def test_base_side_change_is_not_hidden_by_triple_dot(self):
        self.write('internal/main.go', 'feature\n')
        self.commit()
        self.git('checkout', '-q', 'main')
        self.write('.github/workflows/a.yml', 'base update\n')
        self.commit()
        self.base = self.sha()
        self.git('checkout', '-q', 'feature')
        self.merge()
        self.assertEqual(self.git('diff', '--name-only', self.base + '...' + self.head), 'internal/main.go')
        self.assertEqual(self.selected(), 'pre-merge')

    def test_merge_only_relevant_change_forces_full_run(self):
        self.write('internal/main.go', 'feature\n')
        self.merge()
        self.write('.github/workflows/a.yml', 'merge resolution\n')
        self.git('add', '-A')
        self.git('commit', '-q', '--amend', '--no-edit')
        self.tested = self.sha()
        self.assertEqual(self.selected(), 'pre-merge')

    def test_dirty_and_untracked_content_cannot_scope(self):
        self.merge()
        for path in ('.github/workflows/a.yml', 'scripts/new.py', 'internal/main.go'):
            with self.subTest(path=path):
                self.write(path, 'dirty\n')
                self.assertEqual(self.selected(), 'pre-merge')
                self.git('reset', '--hard', self.tested)
                self.git('clean', '-fd')

    def test_unchanged_relevant_symlink_cannot_scope(self):
        self.git('checkout', '-q', 'main')
        (self.source / '.github/link').symlink_to('/etc/passwd')
        self.commit()
        self.base = self.sha()
        self.git('checkout', '-q', 'feature')
        self.git('reset', '--hard', self.base)
        self.merge()
        self.assertEqual(self.selected(), 'pre-merge')

    def test_index_flags_cannot_hide_changed_workflow_bytes(self):
        self.merge()
        for flag in ('--assume-unchanged', '--skip-worktree'):
            with self.subTest(flag=flag):
                self.git('update-index', flag, '.github/workflows/a.yml')
                self.write('.github/workflows/a.yml', 'hidden working change\n')
                self.assertEqual(self.git('status', '--porcelain'), '')
                self.assertEqual(self.selected(), 'pre-merge')
                self.write('.github/workflows/a.yml', 'name: fixture\n')
                self.git('update-index', '--no-assume-unchanged', '--no-skip-worktree', '.github/workflows/a.yml')

    def test_filemode_setting_cannot_hide_changed_workflow_mode(self):
        self.merge()
        self.git('config', 'core.filemode', 'false')
        (self.source / '.github/workflows/a.yml').chmod(0o755)
        self.assertEqual(self.git('status', '--porcelain'), '')
        self.assertEqual(self.selected(), 'pre-merge')

    def test_index_flag_cannot_hide_working_symlink(self):
        self.merge()
        self.git('update-index', '--assume-unchanged', '.github/workflows/a.yml')
        (self.source / '.github/workflows/a.yml').unlink()
        (self.source / '.github/workflows/a.yml').symlink_to('../actionlint.yaml')
        self.assertEqual(self.git('status', '--porcelain'), '')
        self.assertEqual(self.selected(), 'pre-merge')

    def test_bad_missing_checkout_and_parent_identities_fall_back(self):
        self.merge()
        for sha in ('', 'not-a-sha', 'f' * 40, self.base, self.head):
            with self.subTest(sha=sha):
                self.assertEqual(self.selected(tested=sha), 'pre-merge')
        self.assertEqual(self.selected(event={}), 'pre-merge')
        changed = copy.deepcopy(self.event)
        changed['pull_request']['base']['sha'] = self.head
        self.assertEqual(self.selected(event=changed), 'pre-merge')

    def test_disconnected_ancestry_falls_back(self):
        self.merge()
        self.git('checkout', '-q', '--orphan', 'unrelated')
        self.commit()
        unrelated = self.sha()
        self.git('checkout', '-q', 'main')
        changed = copy.deepcopy(self.event)
        changed['pull_request']['head']['sha'] = unrelated
        self.assertEqual(self.selected(event=changed), 'pre-merge')

    def test_shallow_history_falls_back(self):
        self.merge()
        clone = Path(self.temp.name) / 'shallow'
        subprocess.run(['git', '-c', 'maintenance.auto=false', '-c', 'gc.auto=0',
                        'clone', '-q', '--depth=1', self.source.as_uri(), str(clone)], check=True)
        self.assertEqual(self.selected(source=clone), 'pre-merge')

    def test_git_errors_and_malformed_records_fall_back(self):
        self.merge()
        actual = scope.git
        for bad in (subprocess.CalledProcessError(1, 'git'), b'bad', b'\0', b'/absolute\0'):
            def altered(source, *args):
                if args[0] == 'diff':
                    if isinstance(bad, Exception):
                        raise bad
                    return bad
                return actual(source, *args)
            with self.subTest(bad=bad), patch.object(scope, 'git', side_effect=altered):
                self.assertEqual(self.selected(), 'pre-merge')
        with patch.object(scope, 'git', side_effect=lambda source, *args:
                          b'bad\0' if args[0] == 'ls-tree' else actual(source, *args)):
            self.assertEqual(self.selected(), 'pre-merge')

    def test_nul_paths_and_deceptive_prefixes(self):
        self.write('.github-old/example\twith\nspaces', 'unrelated\n')
        self.merge()
        self.assertEqual(self.selected(), scope.SCOPED_RUN)
        self.git('checkout', '-q', 'feature')
        self.write('.github/example\twith\nspaces', 'relevant\n')
        self.merge()
        self.assertEqual(self.selected(), 'pre-merge')

    def test_generated_run_preserves_original_definitions_and_options(self):
        config = copy.deepcopy(self.config)
        config['runs']['pre-merge']['rerun_checks'] = True
        original = copy.deepcopy(config)
        self.assertTrue(scope.compatible(config))
        scope.add_run(config)
        self.assertEqual(config['runs'][scope.SCOPED_RUN], {'checks': scope.GO_CHECKS, 'rerun_checks': True})
        del config['runs'][scope.SCOPED_RUN]
        self.assertEqual(config, original)

    def test_unknown_configuration_keeps_complete_run(self):
        self.merge()
        for mutate in (lambda c: c['runs']['pre-merge']['checks'].append('extra'),
                       lambda c: c['checks']['lint'].update(kind='command'),
                       lambda c: c['targets']['workflows']['inputs'].append('action.yml'),
                       lambda c: c.update(baseline='baseline.json')):
            config = copy.deepcopy(self.config)
            mutate(config)
            self.assertEqual(self.selected(config=config), 'pre-merge')

    def test_staging_cli_emits_selected_run_after_validating_and_copying(self):
        self.write('internal/main.go', 'changed\n')
        self.merge()
        shared = Path(self.temp.name) / 'shared'
        shared.mkdir()
        # A lightweight planner stub exercises the consumer CLI sequence; the
        # actual pinned planner is validated separately before publication.
        verify = shared / 'verify'
        verify.write_text('''#!/usr/bin/env python3
import json, pathlib, sys
source = pathlib.Path(sys.argv[sys.argv.index('--source') + 1])
config = json.loads((source / 'levenshtein.json').read_text())
checks = config['runs'][sys.argv[1]]['checks']
print(json.dumps({'checks': [{'check': config['checks'][name],
                             'target': config['targets'][config['checks'][name]['target']]}
                            for name in checks]}))
''')
        verify.chmod(0o755)
        event = Path(self.temp.name) / 'event.json'
        event.write_text(json.dumps(self.event))
        output = Path(self.temp.name) / 'output'
        summary = Path(self.temp.name) / 'summary'
        staged = Path(self.temp.name) / 'staged'
        environment = dict(os.environ, GITHUB_SHA=self.tested, GITHUB_EVENT_PATH=str(event),
                           GITHUB_OUTPUT=str(output), GITHUB_STEP_SUMMARY=str(summary))
        subprocess.run([sys.executable, '-B', str(Path(__file__).with_name('stage_native_verify.py')),
                        '--source', str(self.source), '--destination', str(staged),
                        '--shared', str(shared), '--pr-scope'], env=environment,
                       check=True, stdout=subprocess.PIPE)
        self.assertEqual(output.read_text(), 'run=pr-go\n')
        self.assertIn('NOT RUN', summary.read_text())
        generated = json.loads((staged / 'levenshtein.json').read_text())
        self.assertEqual(generated['runs']['pre-merge'], self.config['runs']['pre-merge'])
        self.assertEqual(generated['runs']['main'], self.config['runs']['main'])
        self.assertEqual(generated['checks'], self.config['checks'])
        self.assertEqual(generated['runs']['pr-go']['checks'], scope.GO_CHECKS)
        self.assertFalse((staged / 'scripts').exists())
        self.assertFalse(self.git('status', '--porcelain'))


if __name__ == '__main__':
    unittest.main()
