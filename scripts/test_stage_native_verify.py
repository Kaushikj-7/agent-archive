import copy
import importlib.util
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


if __name__ == '__main__':
    unittest.main()
