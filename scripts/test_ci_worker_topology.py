"""Captured topology must agree with the fixed matrix and exhaustive argv."""
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile
import unittest

import ci_worker_topology as topology
from test_ci_workflow import jobs, steps, step_key


def event(branch='ordinary', fork=False):
    return {'number': 12, 'repository': {'full_name': 'wangjohn/agent-archive'},
            'pull_request': {'number': 12,
                'base': {'sha': 'b'*40, 'ref': 'main', 'repo': {'full_name': 'wangjohn/agent-archive'}},
                'head': {'sha': 'c'*40, 'ref': branch,
                         'repo': {'full_name': 'someone/fork' if fork else 'wangjohn/agent-archive'}}}}


def environment(branch='ordinary'):
    return {'GITHUB_EVENT_NAME': 'pull_request', 'GITHUB_REPOSITORY': 'wangjohn/agent-archive',
            'GITHUB_REF': 'refs/pull/12/merge', 'GITHUB_HEAD_REF': branch, 'GITHUB_BASE_REF': 'main',
            'GITHUB_SHA': 'a'*40, 'GITHUB_WORKFLOW_SHA': 'a'*40,
            'GITHUB_WORKFLOW_REF': 'wangjohn/agent-archive/.github/workflows/test.yml@refs/pull/12/merge'}


def receipt(branch='ordinary', fork=False):
    return topology.validate(environment(branch), event(branch, fork), 'a'*40, 'd'*40, ['b'*40, 'c'*40])


class TopologyTests(unittest.TestCase):
    def test_cli_reads_actual_ordered_commit_headers_even_when_shallow(self):
        script = Path(topology.__file__).resolve()
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            env = dict(os.environ, GIT_AUTHOR_NAME='CI test', GIT_AUTHOR_EMAIL='ci@example.invalid',
                       GIT_COMMITTER_NAME='CI test', GIT_COMMITTER_EMAIL='ci@example.invalid')
            def git(*args, data=None):
                return subprocess.check_output(['git', *args], cwd=root, env=env, input=data, text=True).strip()
            git('init', '-q')
            tree = git('write-tree')
            base = git('commit-tree', tree, data='base\n')
            head = git('commit-tree', tree, '-p', base, data='head\n')
            merge = git('commit-tree', tree, '-p', base, '-p', head, data='merge\n')
            git('reset', '--hard', merge)
            (root/'.git/shallow').write_text(merge + '\n')
            self.assertEqual(git('rev-parse', '--is-shallow-repository'), 'true')
            payload = event('ci-qualification/workers-4/campaign/one')
            payload['pull_request']['base']['sha'] = base
            payload['pull_request']['head']['sha'] = head
            (root/'event.json').write_text(json.dumps(payload))
            (root/'environment').touch()
            env.update(environment(payload['pull_request']['head']['ref']), GITHUB_SHA=merge,
                       GITHUB_WORKFLOW_SHA=merge, WORKERS='4', SHARD='3',
                       GITHUB_EVENT_PATH=str(root/'event.json'), GITHUB_ENV=str(root/'environment'))
            subprocess.run([sys.executable, '-B', str(script), '--output', str(root/'receipt.json')],
                           cwd=root, env=env, check=True, capture_output=True, text=True)
            result = json.loads((root/'receipt.json').read_text())
            self.assertEqual(result['parents'], [base, head])
            self.assertEqual(result['source'], merge)
            self.assertEqual((root/'environment').read_text(), 'WORKERS=4\n')
            (root/'.git/shallow').unlink()
            git('update-ref', 'refs/remotes/origin/main', base)
            argv = [sys.executable, '-B', str(script), '--lint-base-ref', 'main',
                    '--output', str(root/'receipt.json')]
            subprocess.run(argv, cwd=root, env=env, check=True, capture_output=True, text=True)
            original = json.loads((root/'receipt.json').read_text())['lint_diff']
            self.assertEqual(original['resolved_base'], base)
            self.assertEqual(original['merge_bases'], [base])
            # Moving the fetched main ref changes real lint endpoints, never the
            # event/candidate proof; retain that difference for cohort comparison.
            git('update-ref', 'refs/remotes/origin/main', head)
            subprocess.run(argv, cwd=root, env=env, check=True, capture_output=True, text=True)
            moved = json.loads((root/'receipt.json').read_text())['lint_diff']
            self.assertEqual(moved['resolved_base'], head)
            self.assertEqual(moved['merge_bases'], [head])

    def test_normal_and_both_experiments_casefold_and_fork(self):
        self.assertEqual(receipt()['workers'], 8)
        self.assertEqual(receipt()['observation_kind'], 'normal')
        for count in (4, 8):
            for branch in (f'ci-qualification/workers-{count}/campaign/one',
                           f'CI-QUALIFICATION/WORKERS-{count}/Campaign/ONE'):
                result = receipt(branch)
                self.assertEqual(result['workers'], count)
                self.assertEqual(result['observation_kind'], 'full-pipeline-experiment')
                result = receipt(branch, fork=True)
                self.assertEqual(result['workers'], 8)
                self.assertEqual(result['observation_kind'], 'normal')

    def test_reserved_malformed_branches_fail_closed(self):
        for branch in ('ci-qualification/workers-2/a/b', 'ci-qualification/workers-4/a',
                       'ci-qualification/workers-4/a/b/more', 'ci-qualification/workers-4/a/b;bad',
                       'ci-qualification/WORKERS-4//b'):
            with self.subTest(branch=branch), self.assertRaises(ValueError):
                receipt(branch)

    def test_actual_event_workflow_and_ordered_merge_identity(self):
        env = environment()
        for key in env:
            other = dict(env, **{key: 'wrong'})
            with self.subTest(key=key), self.assertRaises(ValueError):
                topology.validate(other, event(), 'a'*40, 'd'*40, ['b'*40, 'c'*40])
        for parents in (['c'*40, 'b'*40], ['b'*40], ['b'*40, 'e'*40]):
            with self.assertRaises(ValueError):
                topology.validate(env, event(), 'a'*40, 'd'*40, parents)
        for path in (('number',), ('pull_request', 'number'), ('pull_request', 'base', 'sha')):
            other = event()
            target = other
            for key in path[:-1]:
                target = target[key]
            target[path[-1]] = 999
            with self.assertRaises(ValueError):
                topology.validate(env, other, 'a'*40, 'd'*40, ['b'*40, 'c'*40])
        fixed = event()
        fixed['pull_request']['base']['ref'] = 'ci-qualification-base/campaign'
        changed = dict(env, GITHUB_BASE_REF='ci-qualification-base/campaign')
        self.assertEqual(topology.validate(changed, fixed, 'a'*40, 'd'*40, ['b'*40, 'c'*40])['base_ref'],
                         'ci-qualification-base/campaign')

    def test_worker_count_and_index_must_match_captured_policy(self):
        branch = 'ci-qualification/workers-4/campaign/one'
        for count, shard in (('8', '0'), ('4', '4'), ('4', '-1'), ('4', '01'), ('4', 'x')):
            env = dict(environment(branch), WORKERS=count, SHARD=shard)
            with self.assertRaises(ValueError):
                topology.validate(env, event(branch), 'a'*40, 'd'*40, ['b'*40, 'c'*40])
        env = dict(environment(branch), WORKERS='4', SHARD='3')
        self.assertEqual(topology.validate(env, event(branch), 'a'*40, 'd'*40, ['b'*40, 'c'*40])['workers'], 4)

    def test_fixed_yaml_predicate_matrix_and_coverage_contract(self):
        workflow = (Path(__file__).resolve().parent.parent / '.github/workflows/test.yml').read_text()
        found = jobs(workflow)
        worker = found['linux-short']
        expr = re.search(r'(?m)^      matrix: \$\{\{ fromJSON\((.*)\) }}$', worker)[1]
        # Freeze the only policy expression: no eval of untrusted workflow text.
        predicate = ("github.event.pull_request.head.repo.full_name == github.repository && "
                     "startsWith(github.head_ref, 'ci-qualification/workers-4/')")
        self.assertTrue(expr.startswith(predicate + ' && '))
        matrices = [json.loads(v) for v in re.findall(r"'(\{.*?\})'", expr)]
        self.assertEqual(matrices, [{'workers': [4], 'shard': list(range(4))},
                                    {'workers': [8], 'shard': list(range(8))}])
        for branch, fork in [('ordinary', False), ('CI-QUALIFICATION/WORKERS-4/a/b', False),
                             ('ci-qualification/workers-8/a/b', False), ('ci-qualification/workers-4/a/b', True)]:
            selected = matrices[0 if not fork and branch.casefold().startswith('ci-qualification/workers-4/') else 1]
            self.assertEqual(selected['workers'][0], receipt(branch, fork)['workers'])
        self.assertNotIn('needs:', worker)
        self.assertIn('WORKERS: ${{ matrix.workers }}', worker)
        self.assertIn('name: linux-short (${{ matrix.shard }})', worker)
        self.assertIn('--workers "$WORKERS" --plain-audit-owned --plain-vet-owned --shard "$SHARD"', worker)
        self.assertIn('--workers "$WORKERS" --audit-evidence', found['linux-race'])
        for name in ('tier-plan', 'source-audit', 'linux-short', 'linux-race'):
            guard = next(v for v in steps(found[name]) if 'ci_worker_topology.py' in v)
            self.assertEqual(step_key(guard, 'if'), "github.event_name == 'pull_request'")
            self.assertNotIn('continue-on-error:', guard)
        self.assertNotIn('workflow_run:', workflow)
        self.assertNotIn('labeled', workflow)
        diagnostic = next(v for v in steps(found['linux-race']) if 'collect_ci_timing.py' in v)
        self.assertEqual(step_key(diagnostic, 'continue-on-error'), 'true')
        self.assertEqual(step_key(diagnostic, 'timeout-minutes'), '1')
        self.assertEqual(step_key(diagnostic, 'if'), "success() && github.event_name == 'pull_request'")


if __name__ == '__main__':
    unittest.main()
