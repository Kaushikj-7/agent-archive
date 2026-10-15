"""Adversarial retained identity, measurement attribution and history contracts."""
import copy
import hashlib
import io
import json
from pathlib import Path
import tempfile
import subprocess
import sys
import unittest
import zipfile

import ci_run_identity
import ci_timing_history
import collect_ci_timing
import normalize_ci_timing
import run_shared_verify
from test_ci_worker_topology import environment, event, receipt
from test_collect_ci_timing import FakeAPI, run


class IdentityAPI:
    def __init__(self, metadata, commit, compare=None):
        self.metadata, self.commit, self.compare = metadata, commit, compare

    def get(self, path):
        if '/artifacts/' in path:
            return self.metadata
        if '/compare/' in path:
            return self.compare
        return self.commit


def archive(root, topology=None, *, workflow='Test', attempt=1, ident=1, member='identity.json'):
    topology = copy.deepcopy(topology or receipt())
    topology['workflow_ref'] = collect_ci_timing.REPOSITORY + '/' + collect_ci_timing.PATHS[workflow] + '@refs/pull/12/merge'
    proof = {'schema': 1, 'run_id': ident, 'run_attempt': attempt, 'workflow': workflow,
             'workflow_path': collect_ci_timing.PATHS[workflow], 'event': 'pull_request', 'topology': topology}
    path = root / 'proof.zip'
    with zipfile.ZipFile(path, 'w') as stream:
        stream.writestr(member, json.dumps(proof))
    metadata = {'id': 99, 'name': f'ci-run-identity-{workflow}-{attempt}', 'expired': False,
                'workflow_run': {'id': ident, 'head_sha': 'c'*40, 'head_branch': 'ordinary'}, 'digest': 'sha256:' + hashlib.sha256(path.read_bytes()).hexdigest()}
    commit = {'sha': topology['source'], 'tree': {'sha': topology['tree']},
              'parents': [{'sha': sha} for sha in topology['parents']]}
    return path, metadata, commit


class TimingContracts(unittest.TestCase):
    def test_postmerge_empty_association_requires_authenticated_exact_identity(self):
        with tempfile.TemporaryDirectory() as temporary:
            path, metadata, commit = archive(Path(temporary))
            value = run('Test')
            value['pull_requests'] = []
            api = IdentityAPI(metadata, commit)
            result = collect_ci_timing.validate_run(value, 'Test', receipt(), api=api, archives=[(99, path)])
            self.assertEqual(result['artifact_id'], 99)
            with self.assertRaises(ValueError):
                collect_ci_timing.validate_run(value, 'Test', receipt(), api=api)
            for field, invalid in [('id', 98), ('name', 'ci-run-identity-Levenshtein-1'),
                                   ('expired', True), ('workflow_run', {'id': 2}), ('digest', 'sha256:bad')]:
                altered = dict(metadata, **{field: invalid})
                with self.subTest(field=field), self.assertRaises(ValueError):
                    collect_ci_timing.validate_run(value, 'Test', receipt(), api=IdentityAPI(altered, commit), archives=[(99, path)])
            wrong = copy.deepcopy(commit)
            wrong['parents'].reverse()
            with self.assertRaisesRegex(ValueError, 'merge identity'):
                collect_ci_timing.validate_run(value, 'Test', receipt(), api=IdentityAPI(metadata, wrong), archives=[(99, path)])

    def test_complete_postmerge_pair_keeps_authenticated_provenance(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            proofs, lookup, archives = [], {}, []
            for name, ident in [('Test', 1), ('Levenshtein', 2)]:
                folder = root/name
                folder.mkdir()
                path, metadata, commit = archive(folder, workflow=name, ident=ident)
                metadata['id'] = ident + 100
                lookup[ident + 100] = metadata
                archives.append((ident + 100, path))
            class PostmergeAPI(FakeAPI):
                def get(self, path):
                    if '/artifacts/' in path:
                        return lookup[int(path.rsplit('/', 1)[1])]
                    if '/git/commits/' in path:
                        return commit
                    return super().get(path)
            api = PostmergeAPI()
            for value in api.runs.values():
                value['pull_requests'] = []
            result = collect_ci_timing.collect(api, receipt(), 1, 2, identity_archives=archives)
            self.assertEqual(result['snapshot_status'], 'terminal')
            self.assertEqual(len(result['authenticated_retained_identities']), 2)
            self.assertEqual(result['report']['samples'][0]['linux_runner_minutes'], 24)
            # A contradictory nonempty live association cannot be replaced by
            # an otherwise authentic archive.
            api.runs[2]['pull_requests'] = run('Levenshtein', number=13)['pull_requests']
            with self.assertRaises(ValueError):
                collect_ci_timing.collect(api, receipt(), 1, 2, identity_archives=archives)

    def test_authenticated_wrong_pr_base_source_attempt_or_workflow_fails(self):
        with tempfile.TemporaryDirectory() as temporary:
            value = run('Test')
            value['pull_requests'] = []
            for field, invalid in [('pr_number', 13), ('event_base', 'e'*40), ('head_ref', 'sibling'),
                                   ('source', 'e'*40), ('workflow_source', 'e'*40), ('head_repository', 'other/fork')]:
                altered = dict(receipt(), **{field: invalid})
                path, metadata, commit = archive(Path(temporary), altered)
                with self.subTest(field=field), self.assertRaises(ValueError):
                    collect_ci_timing.validate_run(value, 'Test', receipt(), api=IdentityAPI(metadata, commit), archives=[(99, path)])
            path, metadata, commit = archive(Path(temporary), attempt=2)
            metadata['name'] = 'ci-run-identity-Test-1'
            with self.assertRaises(ValueError):
                collect_ci_timing.validate_run(value, 'Test', receipt(), api=IdentityAPI(metadata, commit), archives=[(99, path)])
            path, metadata, commit = archive(Path(temporary), member='../identity.json')
            with self.assertRaisesRegex(ValueError, 'members'):
                collect_ci_timing.validate_run(value, 'Test', receipt(), api=IdentityAPI(metadata, commit), archives=[(99, path)])

    def test_descendant_base_needs_independent_compare_proof(self):
        topology = dict(receipt(), tested_base='e'*40, parents=['e'*40, 'c'*40])
        with tempfile.TemporaryDirectory() as temporary:
            path, metadata, commit = archive(Path(temporary), topology)
            value = run('Test')
            value['pull_requests'] = []
            compare = {'status': 'ahead', 'merge_base_commit': {'sha': 'b'*40}}
            collect_ci_timing.validate_run(value, 'Test', topology, api=IdentityAPI(metadata, commit, compare), archives=[(99, path)])
            compare['merge_base_commit']['sha'] = 'f'*40
            with self.assertRaisesRegex(ValueError, 'ancestry'):
                collect_ci_timing.validate_run(value, 'Test', topology, api=IdentityAPI(metadata, commit, compare), archives=[(99, path)])

    def test_shared_workflow_capture_uses_its_own_source_context(self):
        env = dict(environment(), GITHUB_WORKFLOW='Levenshtein', GITHUB_RUN_ID='2', GITHUB_RUN_ATTEMPT='1')
        env['GITHUB_WORKFLOW_REF'] = env['GITHUB_WORKFLOW_REF'].replace('test.yml', 'levenshtein.yml')
        def git(*args):
            return {('rev-parse', 'HEAD'): 'a'*40, ('rev-parse', 'HEAD^{tree}'): 'd'*40,
                    ('rev-parse', '--is-shallow-repository'): 'false',
                    ('status', '--porcelain', '--untracked-files=no'): '',
                    ('cat-file', '-p', 'a'*40): 'tree ' + 'd'*40 + '\nparent ' + 'b'*40 + '\nparent ' + 'c'*40 + '\n\nmerge'}[args]
        proof = ci_run_identity.capture(env, event(), git)
        self.assertEqual(proof['workflow_path'], '.github/workflows/levenshtein.yml')
        env['GITHUB_WORKFLOW_SHA'] = 'e'*40
        with self.assertRaises(ValueError):
            ci_run_identity.capture(env, event(), git)

    def test_fixture_metrics_preserve_scopes_and_never_subtract_nested_time(self):
        event_row = {'Action': 'output', 'Package': 'p', 'Test': 'TestParent/child',
                     'Output': '    test.go:10: CI_FIXTURE_METRIC_V1 ' + json.dumps({
                         'version': 1, 'fixture': 'catalog', 'phase': 'clone', 'elapsed_ns': 123,
                         'operations': {'puts': 2}}) + '\n'}
        row = normalize_ci_timing.fixtures([event_row])[0]
        self.assertEqual(row['test_id'], 'p::TestParent')
        self.assertFalse(row['additive'])
        self.assertEqual(row['operations'], {'puts': 2})
        broken = dict(event_row, Output=event_row['Output'].replace('123', '-123'))
        with self.assertRaises(ValueError):
            normalize_ci_timing.fixtures([broken])

    def test_shared_receipt_retains_measured_parallel_checks_only(self):
        report = {'version': 1, 'results': [{'id': 'lint', 'duration_ms': 1000,
                                            'execution_ms': 900, 'status': 'passed', 'cache': {'status': 'miss'},
                                            'stderr': 'not retained'}]}
        row = run_shared_verify.measured(report)[0]
        self.assertEqual(row['seconds'], 1)
        self.assertNotIn('stderr', row)
        for duration in (True, -1, float('nan')):
            report['results'][0]['duration_ms'] = duration
            with self.assertRaises(ValueError):
                run_shared_verify.measured(report)

    def test_shared_command_failure_and_invalid_measurements_fail_closed(self):
        root = Path(__file__).resolve().parent.parent
        with tempfile.TemporaryDirectory() as temporary:
            output = Path(temporary)/'timing.json'
            for payload, expected in [("print('invalid JSON')", 1),
                                      ("import json,sys; print(json.dumps({'version':1,'results':[]})); sys.exit(7)", 7)]:
                result = subprocess.run([sys.executable, '-B', str(root/'scripts/run_shared_verify.py'),
                                         '--source', str(root), '--output', str(output), '--',
                                         sys.executable, '-c', payload], capture_output=True, text=True)
                self.assertEqual(result.returncode, expected, result.stderr)
                value = json.loads(output.read_text())
                self.assertEqual(value['exit_code'], 7 if expected == 7 else 0)
                if expected == 1:
                    self.assertIn('measurement_error', value)

    def test_worker_normalization_keeps_wall_and_own_elapsed_distinct(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            manifest = {'catalog': {'source': 'a'*40, 'configuration': {'GOVERSION': 'go1.27.2'},
                                   'units': ['p'], 'cli': [], 'parents': {}},
                        'settings': {}, 'shard': 0, 'success': True,
                        'phases': {'worker_total_seconds': 5},
                        'timing_observation': {'run_id': '1', 'run_attempt': '1'}}
            (root/'manifest.json').write_text(json.dumps(manifest))
            events = [{'Action': 'run', 'Package': 'p', 'Test': 'TestA', 'Time': '2026-10-15T00:00:00Z'},
                      {'Action': 'pass', 'Package': 'p', 'Test': 'TestA', 'Time': '2026-10-15T00:00:05Z', 'Elapsed': 0.01}]
            (root/'ordinary.json').write_text('\n'.join(json.dumps(item) for item in events))
            observation = normalize_ci_timing.worker(root)
            row = observation['tests'][0]
            self.assertEqual(row['parent_wall_seconds'], 5)
            self.assertEqual(row['own_elapsed_seconds'], .01)
            self.assertEqual(row['setup_attribution'], 'unattributed')
            self.assertEqual(observation['observation_kind'], 'unknown')
            with self.assertRaisesRegex(ValueError, 'topology'):
                normalize_ci_timing.worker(root, {'source': 'b'*40})
            manifest['phases'] = {}
            (root/'manifest.json').write_text(json.dumps(manifest))
            with self.assertRaisesRegex(ValueError, 'missing'):
                normalize_ci_timing.worker(root)

    def test_candidate_dedup_conflicts_experiments_and_cohorts(self):
        observation = {'schema': 1, 'kind': 'worker-timing', 'run_id': '1', 'run_attempt': '1', 'shard': 0,
                       'source': 'a'*40, 'observation_kind': 'normal', 'success': True,
                       'configuration_sha256': 'x', 'compiled_inventory_sha256': 'inventory',
                       'cohort': {'cache_state': 'prefix', 'runner_image': 'image'},
                       'tests': [{'test_id': 'p::TestA', 'own_elapsed_seconds': 31, 'status': 'pass', 'timing_issues': []}]}
        pipeline = collect_ci_timing.collect(FakeAPI(), receipt(), 1, 2)
        result = ci_timing_history.candidate([observation, observation], [pipeline])
        self.assertEqual(result['physical_worker_observations'], 1)
        self.assertEqual(result['groups'][0]['tests']['p::TestA']['observations'], 1)
        self.assertFalse(result['automatic_promotion'])
        # The retained allocation, including its actual attempt, authorizes
        # individual shards; sharing a successful workflow ID is insufficient.
        self.assertEqual(ci_timing_history.candidate([dict(observation, shard=999)], [pipeline])['groups'], [])
        for cohort in ({}, {'cache_state': 'prefix', 'runner_image': ''},
                       {'cache_state': '', 'runner_image': 'image'},
                       {'cache_state': 'made-up', 'runner_image': 'image'}):
            with self.subTest(cohort=cohort):
                self.assertEqual(ci_timing_history.candidate([dict(observation, cohort=cohort)], [pipeline])['groups'], [])
        with self.assertRaisesRegex(ValueError, 'conflicting'):
            ci_timing_history.candidate([observation, dict(observation, run_id=1)], [pipeline])
        changed_inventory = dict(observation, shard=1, compiled_inventory_sha256='different-inventory')
        self.assertEqual(len(ci_timing_history.candidate([observation, changed_inventory], [pipeline])['groups']), 2)
        rerun_api = FakeAPI()
        rerun_api.runs[1]['run_attempt'] = 2
        # All original successful jobs were carried into the final attempt.
        rerun = collect_ci_timing.collect(rerun_api, receipt(), 1, 2)
        self.assertEqual(len(ci_timing_history.candidate([observation], [rerun])['groups']), 1)
        self.assertEqual(ci_timing_history.candidate([dict(observation, run_attempt='2')], [rerun])['groups'], [])
        self.assertEqual(ci_timing_history.candidate([observation])['groups'], [])
        failed_api = FakeAPI()
        failed_api.runs[2]['conclusion'] = 'failure'
        failed = collect_ci_timing.collect(failed_api, receipt(), 1, 2)
        self.assertEqual(ci_timing_history.candidate([observation], [failed])['groups'], [])
        with self.assertRaisesRegex(ValueError, 'conflicting'):
            ci_timing_history.candidate([observation, dict(observation, source='b'*40)])
        for field, value in [('success', False), ('observation_kind', 'full-pipeline-experiment')]:
            self.assertEqual(ci_timing_history.candidate([dict(observation, **{field: value})], [pipeline])['groups'], [])
        self.assertEqual(ci_timing_history.candidate([dict(observation, cohort={'cache_state': 'unknown'})])['groups'], [])


if __name__ == '__main__':
    unittest.main()
