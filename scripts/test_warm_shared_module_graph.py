#!/usr/bin/env python3
"""Contracts for bounded tidy dependency prefetch, independent of check verdicts."""
import base64
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest import mock

import warm_shared_module_graph as graph
from go_cache_context import InvalidContext

ROOT = Path(__file__).resolve().parent.parent
HASH = 'h1:' + base64.b64encode(bytes(32)).decode()


def entry(module='example.com/dependency', version='v1.2.3'):
    return f'{module} {version} {HASH}\n{module} {version}/go.mod {HASH}\n'


class GraphTests(unittest.TestCase):
    def test_repository_graph_includes_tidy_only_dependencies_without_widening_versions(self):
        data = (ROOT / 'go.sum').read_bytes()
        result = graph.module_graph(data)
        self.assertIn('golang.org/x/tools@v0.49.0', result)
        self.assertIn('modernc.org/ccgo/v4@v4.35.0', result)
        self.assertEqual(set(result), {line.split()[0] + '@' + line.split()[1]
                                      for line in data.decode().splitlines()
                                      if not line.split()[1].endswith('/go.mod')})

    def test_rejects_unbounded_unchecksummed_or_malformed_inputs(self):
        invalid = [b'', entry().splitlines()[0].encode(), b'example.com/d v1.0.0 nope\n',
                   entry('../outside').encode(), entry('-flag').encode(),
                   entry(version='latest').encode(), entry(version='v1.2.3@other').encode(),
                   entry().replace(HASH, 'h1:AA==').encode(),
                   (entry() + entry().replace(HASH, 'h1:' + base64.b64encode(bytes([1]) * 32).decode())).encode(),
                   ''.join(entry(f'example.com/d{i}') for i in range(graph.MAX_MODULES + 1)).encode()]
        for data in invalid:
            with self.subTest(data=data[:100]), self.assertRaises(InvalidContext):
                graph.module_graph(data)

    def test_exact_versions_are_downloaded_once_with_controlled_environment(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            (root / 'go.mod').write_text('module example.com/main\n')
            (root / 'go.sum').write_text(entry() + entry())
            command = mock.Mock()
            with mock.patch.dict(os.environ, {'GOFLAGS': '-mod=mod', 'GOWORK': '/bad/work', 'GOENV': '/bad/env'}):
                self.assertEqual(graph.warm(root, command), 1)
            command.assert_called_once()
            args, kwargs = command.call_args
            self.assertEqual(args[0], ['go', 'mod', 'download', 'example.com/dependency@v1.2.3'])
            self.assertEqual(kwargs['cwd'], root.resolve())
            self.assertEqual(kwargs['env']['GOFLAGS'], '-mod=readonly')
            self.assertEqual(kwargs['env']['GOWORK'], 'off')
            self.assertEqual(kwargs['env']['GOENV'], 'off')
            self.assertEqual(kwargs['env']['GOTOOLCHAIN'], 'local')

    def test_manifest_changes_fail_even_if_download_also_fails(self):
        for name in ('go.mod', 'go.sum'):
            for failure in (False, True):
                with self.subTest(name=name, failure=failure), tempfile.TemporaryDirectory() as tmp:
                    root = Path(tmp)
                    (root / 'go.mod').write_text('module example.com/main\n')
                    (root / 'go.sum').write_text(entry())
                    def command(*args, **kwargs):
                        (root / name).write_text('changed')
                        if failure:
                            raise subprocess.CalledProcessError(1, 'go')
                    with self.assertRaisesRegex(InvalidContext, 'changed'):
                        graph.warm(root, command)

    def test_download_failure_propagates_and_symlinks_are_rejected(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            (root / 'go.mod').write_text('module example.com/main\n')
            (root / 'go.sum').write_text(entry())
            with self.assertRaises(subprocess.CalledProcessError):
                graph.warm(root, mock.Mock(side_effect=subprocess.CalledProcessError(1, 'go')))
            (root / 'go.sum').rename(root / 'sum')
            (root / 'go.sum').symlink_to(root / 'sum')
            with self.assertRaises(InvalidContext):
                graph.warm(root, mock.Mock())

    def test_prefetch_is_only_in_existing_cold_shared_seed_and_fresh_checks_remain(self):
        workflow = (ROOT / '.github/workflows/compilation-seeds.yml').read_text()
        step = workflow.split('      - name: Prefetch checksummed product tidy dependencies\n', 1)[1].split('      - name:', 1)[0]
        self.assertIn("matrix.profile == 'shared' && steps.restored.outputs.restore-state != 'trusted'", step)
        self.assertIn('python3 -B scripts/warm_shared_module_graph.py', step)
        self.assertLess(workflow.index('scripts/warm_shared_module_graph.py'), workflow.index('scripts/ci_cache.py complete'))
        for filename in ('test.yml', 'levenshtein.yml', 'extended.yml'):
            self.assertNotIn('scripts/warm_shared_module_graph.py', (ROOT / '.github/workflows' / filename).read_text())


if __name__ == '__main__':
    unittest.main()
