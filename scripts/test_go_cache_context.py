import hashlib
import json
from pathlib import Path
import tempfile
import unittest

import go_cache_context as keys


def metadata():
    go = dict.fromkeys(keys.GO_FIELDS, '')
    go.update(GOVERSION='go1.27.2', GOOS='linux', GOHOSTOS='linux',
              GOARCH='amd64', GOHOSTARCH='amd64', GOAMD64='v1', CGO_ENABLED='1',
              GOTOOLCHAIN='local', GOWORK='off', GOFLAGS='-mod=readonly', CC='gcc', CXX='g++')
    return go


class ContextTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.release = self.root/'os-release'
        self.release.write_text('ID=ubuntu\nVERSION_ID="24.04"\n')
        self.go = metadata()
        self.env = {'RUNNER_OS': 'Linux', 'RUNNER_ARCH': 'X64',
                    'ImageOS': 'ubuntu24', 'ImageVersion': '20261001.1.0'}

    def context(self):
        def fake_go(argv):
            if argv == ['go', 'version']:
                return f'go version {self.go["GOVERSION"]} {self.go["GOOS"]}/{self.go["GOARCH"]}\n'.encode()
            if argv == ['go', 'env', '-json', *keys.GO_FIELDS]:
                return json.dumps(self.go).encode()
            self.fail(f'unexpected command {argv}')
        return keys.runtime_context(fake_go, self.env, self.release,
                                    compiler=lambda name: {'binary': keys.digest(name), 'version': keys.digest('1')})

    def plan(self, context=None, pr=12, worker=0, source=None):
        source = source or {'tested': 'a'*40, 'tree': 'b'*40, 'dependency': 'c'*64}
        return keys.cache_plan(pr, worker, context or self.context(), source, self.root)

    def test_semantic_invalidation_and_constrained_source_reuse(self):
        context = self.context()
        first = self.plan(context)
        self.assertEqual(first, self.plan(dict(reversed(list(context.items())))))
        new = self.plan(context, source={'tested': 'd'*40, 'tree': 'e'*40, 'dependency': 'c'*64})
        self.assertNotEqual(first.primary, new.primary)
        self.assertEqual(first.prefix, new.prefix)
        for label, changed in [('experiment', {'go': dict(context['go'], GOEXPERIMENT='loopvar')}),
                               ('compiler', {'cc': {'binary': 'new', 'version': '1'}}),
                               ('runner image', {'image': {'ImageOS': 'ubuntu24', 'ImageVersion': '20261002.1.0'}}),
                               ('OS image', {'os_release': 'new'}), ('schema', {'schema': 'new'}),
                               ('arch', {'go': dict(context['go'], GOARCH='arm64')}),
                               ('CGO', {'go': dict(context['go'], CGO_CFLAGS='-O0')})]:
            with self.subTest(label=label):
                self.assertNotEqual(first.prefix, self.plan(dict(context, **changed)).prefix)
        self.assertNotEqual(first.prefix, self.plan(pr=13).prefix)
        self.assertNotEqual(first.prefix, self.plan(worker=1).prefix)
        changed = dict(first.source, dependency='d'*64)
        self.assertNotEqual(first.prefix, self.plan(source=changed).prefix)

    def test_actual_go_metadata_fails_closed(self):
        for key, value in [('GOVERSION', 'go1.27.3'), ('CGO_ENABLED', '0'),
                           ('GOHOSTARCH', 'arm64'), ('GOFLAGS', '-mod=mod'),
                           ('GOTOOLCHAIN', 'auto'), ('GOWORK', '/tmp/other')]:
            with self.subTest(key=key):
                original = self.go[key]
                self.go[key] = value
                with self.assertRaises(keys.InvalidContext):
                    self.context()
                self.go[key] = original
        self.go.pop('GOARM64')
        with self.assertRaises(keys.InvalidContext):
            self.context()

    def test_runner_image_missing_or_unsafe_is_explicit_failure(self):
        for version in (None, '', 'version\nkey=unsafe'):
            self.env['ImageVersion'] = version
            with self.assertRaises(keys.InvalidContext):
                self.context()

    def test_unsafe_pr_worker_path_cannot_select_key(self):
        for pr, worker in [('12\nother=1', 0), (12, '0'), (True, 0), (12, 2)]:
            with self.assertRaises(keys.InvalidContext):
                self.plan(pr=pr, worker=worker)
        alias = self.root/'alias'
        alias.symlink_to(self.root, target_is_directory=True)
        with self.assertRaises(keys.InvalidContext):
            keys.cache_plan(12, 0, self.context(), self.plan().source, alias)

    def test_measurement_namespace_uses_same_encoding_without_pr_event_impersonation(self):
        actual = self.plan()
        measured = keys.owned_plan('aa-ci-measure-v1-approvedBatch', 0, self.context(), actual.source, self.root)
        self.assertNotEqual(actual.prefix, measured.prefix)
        self.assertEqual(actual.paths, measured.paths)
        self.assertTrue(measured.primary.endswith(actual.source['tree']))
        for namespace in ('bad\nkey=injected', 'aa-pr-go-v1/pr12'):
            with self.assertRaises(keys.InvalidContext):
                keys.owned_plan(namespace, 0, self.context(), actual.source, self.root)

    def test_compiler_actual_binary_and_version_both_invalidate(self):
        binary = self.root/'cc'
        binary.write_bytes(b'compiler-one')
        version = [b'cc version 1']
        def run(argv):
            self.assertEqual(argv, [str(binary), '--version'])
            return version[0]
        first = keys.compiler_identity('cc', run, lambda name: str(binary))
        binary.write_bytes(b'compiler-two')
        self.assertNotEqual(first, keys.compiler_identity('cc', run, lambda name: str(binary)))
        binary.write_bytes(b'compiler-one')
        version[0] = b'cc version 2'
        self.assertNotEqual(first, keys.compiler_identity('cc', run, lambda name: str(binary)))
        with self.assertRaises(keys.InvalidContext):
            keys.compiler_identity('ccache gcc', run, lambda name: str(binary))

    def test_tracked_actual_bytes_modes_symlinks_and_both_module_inputs(self):
        # Fake Git supplies immutable metadata, real proof helper inspects fixture files.
        files = {'go.mod': b'module example\n', 'go.sum': b'example sum\n', 'main.go': b'package main\n'}
        for name, content in files.items():
            (self.root/name).write_bytes(content)
        def tree_records():
            result = b''
            for name, content in files.items():
                blob = b'blob '+str(len(content)).encode()+b'\0'+content
                oid = hashlib.sha1(blob, usedforsecurity=False).hexdigest().encode()
                result += b'100644 blob '+oid+b'\t'+name.encode()+b'\0'
            return result
        untracked = [b'']
        def git(source, *args):
            return {('rev-parse', 'HEAD'): b'a'*40+b'\n',
                    ('rev-parse', 'HEAD^{tree}'): b'b'*40+b'\n',
                    ('ls-tree', '-r', '-z', 'a'*40): tree_records(),
                    ('ls-files', '--others', '-z'): untracked[0]}[args]
        first = keys.source_identity(self.root, 'a'*40, git)
        untracked[0] = b'shard-results/manifest.json\0'
        self.assertEqual(first, keys.source_identity(self.root, 'a'*40, git))
        untracked[0] = b'internal/new.go\0'
        with self.assertRaises(keys.InvalidContext):
            keys.source_identity(self.root, 'a'*40, git)
        untracked[0] = b''
        for name in ('go.mod', 'go.sum'):
            with self.subTest(name=name):
                old = files[name]
                files[name] += b'new\n'
                (self.root/name).write_bytes(files[name])
                self.assertNotEqual(first['dependency'], keys.source_identity(self.root, 'a'*40, git)['dependency'])
                files[name] = old
                (self.root/name).write_bytes(old)
        (self.root/'main.go').write_bytes(b'changed hidden by assume-unchanged')
        with self.assertRaises(keys.InvalidContext):
            keys.source_identity(self.root, 'a'*40, git)
        (self.root/'main.go').write_bytes(files['main.go'])
        (self.root/'main.go').chmod(0o755)
        with self.assertRaises(keys.InvalidContext):
            keys.source_identity(self.root, 'a'*40, git)
        (self.root/'main.go').unlink()
        (self.root/'main.go').symlink_to(self.root/'go.mod')
        with self.assertRaises(keys.InvalidContext):
            keys.source_identity(self.root, 'a'*40, git)


if __name__ == "__main__":
    unittest.main()
