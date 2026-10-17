#!/usr/bin/env python3
"""Bounded offline protocol/failure tests; no native Muse, Go or provider calls."""

import copy
import json
import os
import pathlib
import subprocess
import sys
import tempfile
import time
import unittest
from unittest import mock

import muse_final_acceptance as final


HELPER = '''import hashlib,json,os,pathlib,sys,time
root=pathlib.Path.cwd()
mode=sys.argv[1]
if mode=='timeout':
    print('failed-attempt-before-timeout',flush=True)
    time.sleep(60)
if mode=='nonzero':
    print('failed-attempt',flush=True)
    sys.exit(9)
if mode=='descendant':
    import subprocess
    subprocess.Popen([sys.executable,'-c','import time; time.sleep(60)'])
if mode=='overflow':
    print('x'*(9*1024*1024),flush=True)
    sys.exit(0)
p=root/'facts.txt'
p.write_text('synthetic protocol facts only\\n')
case=os.environ['MUSE_FINAL_CASE']
value={'version':1,'case':case,'kind':'deterministic',
       'source':json.loads(os.environ['MUSE_FINAL_SOURCE']),
       'assertions':{case:'SKIP' if mode=='skip' else 'PASS'},
       'facts':{'remote_secret_sentinels':0,'remote_reasoning_dumps':0},
       'artifacts':[{'path':'facts.txt','sha256':hashlib.sha256(p.read_bytes()).hexdigest()}]}
print(json.dumps(value))
'''


class AcceptanceTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix='muse-final-offline-')
        self.addCleanup(self.temp.cleanup)
        self.root = pathlib.Path(self.temp.name).resolve()
        self.repo = self.root / 'repo'
        self.repo.mkdir()
        self.helper = self.repo / 'checker.py'
        self.helper.write_text(HELPER)
        self.policy = self.repo / 'policy.txt'
        self.policy.write_text('synthetic fixed budgets/schema control only\n')
        self.budgets = []
        for name in final.BUDGET_PATHS:
            path = self.repo / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text('synthetic authoritative budget path control only')
            self.budgets.append(path)
        self.git('init', '-q')
        self.git('add', '.')
        self.git('-c', 'user.name=Synthetic', '-c', 'user.email=synthetic@example.invalid',
                 'commit', '-qm', 'synthetic evidence control')
        self.source = final.full_race.source_proof(self.repo)
        self.config = self.root / 'configuration.json'
        self.config.write_text('{"synthetic":true}\n')
        self.binary_paths = {}
        for role in ('archive', 'producer', 'old_reader', 'old_writer'):
            path = self.root / (role + '.synthetic')
            path.write_text('synthetic nonexecutable build pin ' + role)
            self.binary_paths[role] = path
        self.packet = self.make_packet()

    def git(self, *args):
        return subprocess.run(['git', '-C', str(self.repo), *args], check=True,
                              stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=5).stdout

    def pin(self, path):
        return {'path': str(path), 'sha256': final.probe.file_digest(path)}

    def make_packet(self, mode='pass'):
        prior = {'source': '1'*40, 'tree': '2'*40}
        return {'version': 1, 'source': self.source, 'scope': 'root',
                'catalog_sha256': final.sha(final.encode(final.catalog())),
                'budget_limits': final.BUDGETS.copy(),
                'budget_files': [self.pin(path) for path in self.budgets], 'schema_files': [self.pin(self.policy)],
                'binaries': {role: dict(self.pin(path), version='synthetic-only',
                                       build_source=self.source if role == 'archive' else prior)
                             for role, path in self.binary_paths.items()},
                'helpers': {'python': self.pin(pathlib.Path(sys.executable).resolve()),
                            'checker': self.pin(self.helper),
                            **{name: self.pin(pathlib.Path(final.__file__).resolve().parent / name)
                               for name in final.HARNESS_HELPERS}},
                'configuration': {'isolated': True, 'provider_authorized': False,
                                  'roots': {name: name for name in final.ROOTS},
                                  'files': [self.pin(self.config)]},
                'cases': [{'id': 'privacy.nested_sentinels', 'kind': 'deterministic',
                           'assertions': ['privacy.nested_sentinels'], 'seconds': 5,
                           'argv': [str(pathlib.Path(sys.executable).resolve()), str(self.helper), mode]}]}

    def run_packet(self, packet=None):
        packet = packet or self.packet
        output = self.root / 'evidence'
        report = final.execute(packet, self.repo, output)
        return output, report

    def test_catalog_covers_native_lifecycle_full_resources_children_and_candidate(self):
        cases = final.catalog()
        self.assertEqual(len([n for n in cases if n.startswith('native.')]), 4*3*18)
        for name in ('publication.grandchild', 'resource.512', 'resource.full_raw_limit',
                     'resource.four_agent_pressure', 'candidate.extended', 'compat.old_writer'):
            self.assertIn(name, cases)
        self.assertNotIn('cross-build', cases)

    def test_successful_protocol_cannot_promote_missing_cases_or_authorize_release(self):
        output, report = self.run_packet()
        self.assertEqual(report['cases']['privacy.nested_sentinels']['status'], 'PROTOCOL_VERIFIED')
        result = final.verify(self.packet, self.repo, output)
        self.assertFalse(result['protocol_complete'])
        self.assertEqual(result['matrix']['native.linux-arm64.exec.fresh'], 'UNVERIFIED')
        self.assertEqual(result['matrix']['publication.direct'], 'UNCLAIMED')
        self.assertEqual(result['matrix']['privacy.nested_sentinels'], 'UNVERIFIED')
        self.assertEqual(result['protocol_matrix']['privacy.nested_sentinels'], 'PROTOCOL_VERIFIED')
        self.assertFalse(result['qualification_complete'])
        self.assertFalse(result['release_authorized'])
        self.assertFalse(result['capture_unlock_authorized'])

    def test_even_complete_checker_protocol_cannot_self_qualify_any_capability(self):
        packet = dict(self.packet, scope='full')
        protocol = final.matrix(packet, {'cases': {name: {'status': 'PROTOCOL_VERIFIED'}
                                                 for name in final.catalog()}})
        self.assertTrue(all(status == 'PROTOCOL_VERIFIED' for status in protocol.values()))
        self.assertTrue(all(status == 'UNVERIFIED' for status in final.qualification_matrix(protocol).values()))

    def test_tampered_artifact_invalidates_unchanged_pass_summary(self):
        output, _ = self.run_packet()
        (output / 'case-0000/facts.txt').write_text('forged replacement')
        with self.assertRaisesRegex(final.Failure, 'artifact checksum'):
            final.verify(self.packet, self.repo, output)

    def test_forged_success_matrix_does_not_replace_missing_union(self):
        output, report = self.run_packet()
        report['matrix'] = {name: 'PASS' for name in final.catalog()}
        report['protocol_complete'] = True
        (output / 'report.json').write_bytes(final.encode(report))
        self.assertFalse(final.verify(self.packet, self.repo, output)['protocol_complete'])

    def test_skip_and_nonzero_preserve_failed_receipts(self):
        for index, mode in enumerate(('skip', 'nonzero')):
            packet = self.make_packet(mode)
            output = self.root / ('failure-' + str(index))
            report = final.execute(packet, self.repo, output)
            self.assertEqual(report['cases'][packet['cases'][0]['id']]['status'], 'FAIL')
            self.assertTrue((output / 'case-0000/stdout.bin').exists())
            receipt = final.load(output / 'case-0000/process.json')
            self.assertTrue(receipt['cleanup']['group_absent'])
            self.assertFalse(final.verify(packet, self.repo, output)['protocol_complete'])

    def test_timeout_is_failed_retains_stream_and_reaps_group(self):
        packet = self.make_packet('timeout')
        packet['cases'][0]['seconds'] = 2
        start = time.monotonic()
        output, report = self.run_packet(packet)
        self.assertLess(time.monotonic() - start, 18)
        self.assertEqual(report['cases'][packet['cases'][0]['id']]['status'], 'FAIL')
        receipt = final.load(output / 'case-0000/process.json')
        self.assertTrue(receipt['timeout'])
        self.assertTrue(receipt['cleanup']['leader_reaped'])
        self.assertTrue(receipt['cleanup']['group_absent'])
        self.assertIn(b'before-timeout', (output / 'case-0000/stdout.bin').read_bytes())

    def test_live_descendant_prevents_false_success_and_is_joined(self):
        packet = self.make_packet('descendant')
        packet['cases'][0]['seconds'] = 2
        output, report = self.run_packet(packet)
        self.assertEqual(report['cases'][packet['cases'][0]['id']]['status'], 'FAIL')
        self.assertTrue(final.load(output / 'case-0000/process.json')['cleanup']['group_absent'])

    def test_output_overflow_is_failed_with_bounded_retained_stream(self):
        output, report = self.run_packet(self.make_packet('overflow'))
        self.assertEqual(report['cases'][self.packet['cases'][0]['id']]['status'], 'FAIL')
        self.assertLessEqual((output / 'case-0000/stdout.bin').stat().st_size, final.probe.OUTPUT_LIMIT)
        self.assertTrue(final.load(output / 'case-0000/process.json')['cleanup']['group_absent'])

    def test_source_proof_catches_hidden_working_edit(self):
        self.git('update-index', '--assume-unchanged', 'checker.py')
        self.helper.write_text(HELPER + '\n# hidden changed code\n')
        with self.assertRaises(final.full_race.shards.Failure):
            final.packet_check(self.packet, self.repo)

    def test_helper_binary_configuration_and_policy_hashes_are_checked(self):
        for path in (self.helper, self.config, self.policy, self.binary_paths['producer']):
            original = path.read_bytes()
            path.write_bytes(original + b'changed')
            with self.assertRaises((final.Failure, final.full_race.shards.Failure)):
                final.packet_check(self.packet, self.repo)
            path.write_bytes(original)

    def test_source_mutation_during_run_cannot_pass(self):
        original = final.probe.run_process
        def changed(*args, **kwargs):
            result = original(*args, **kwargs)
            self.policy.write_text('changed after checker')
            return result
        with mock.patch.object(final.probe, 'run_process', side_effect=changed):
            _, report = self.run_packet()
        self.assertEqual(report['cases'][self.packet['cases'][0]['id']]['status'], 'FAIL')

    def test_rejects_weakened_catalog_assertions_budget_old_binary_or_duplicate(self):
        changes = []
        p = copy.deepcopy(self.packet); p['catalog_sha256'] = '0'*64; changes.append(p)
        p = copy.deepcopy(self.packet); p['cases'][0]['assertions'] = ['okay']; changes.append(p)
        p = copy.deepcopy(self.packet); p['budget_limits']['pass_read_bytes'] *= 2; changes.append(p)
        p = copy.deepcopy(self.packet); p['binaries']['old_writer'] = p['binaries']['archive']; changes.append(p)
        p = copy.deepcopy(self.packet); p['cases'] *= 2; changes.append(p)
        for packet in changes:
            with self.subTest(packet=packet['catalog_sha256']), self.assertRaises(final.Failure):
                final.packet_check(packet, self.repo)

    def test_synthetic_or_crossbuild_cannot_fill_native_case(self):
        packet = self.make_packet()
        case = packet['cases'][0]
        case.update(id='native.linux-amd64.exec.fresh', assertions=['native.linux-amd64.exec.fresh'])
        with self.assertRaisesRegex(final.Failure, 'cannot replace'):
            final.packet_check(packet, self.repo)
        case['kind'] = 'native'
        output, report = self.run_packet(packet)
        self.assertEqual(report['cases'][case['id']]['status'], 'FAIL')

    def test_provider_requires_separate_authorization(self):
        packet = self.make_packet()
        packet['cases'][0].update(id='provider.real_usage_accounting', kind='provider',
                                  assertions=['provider.real_usage_accounting'])
        with self.assertRaisesRegex(final.Failure, 'separate explicit authorization'):
            final.packet_check(packet, self.repo)

    def test_unsafe_artifact_paths_symlinks_duplicate_keys_and_nonfinite_rejected(self):
        directory = self.root / 'artifacts'; directory.mkdir()
        path = directory / 'data'; path.write_bytes(b'synthetic')
        link = directory / 'link'; link.symlink_to(path)
        for name in ('../configuration.json', str(path), 'link'):
            with self.assertRaises(final.Failure):
                final.artifact(directory, {'path': name, 'sha256': final.probe.file_digest(path)})
        for text in ('{"x":1,"x":2}', '{"x":NaN}'):
            path.write_text(text)
            with self.assertRaises(final.Failure):
                final.load(path)

    def observation(self, case_id):
        case = {'id': case_id, 'kind': final.catalog()[case_id]['kind'], 'assertions': [case_id]}
        directory = self.root / 'observation'; directory.mkdir(exist_ok=True)
        path = directory / 'evidence'; path.write_text('synthetic')
        ref = {'path': path.name, 'sha256': final.probe.file_digest(path)}
        value = {'version': 1, 'case': case_id, 'kind': case['kind'], 'source': self.source,
                 'assertions': {case_id: 'PASS'}, 'facts': final.FACTS.get(case_id, {}), 'artifacts': [ref]}
        return case, value, directory, ref

    def test_publication_requires_each_stage_checksum_readback_and_role(self):
        case, value, directory, ref = self.observation('publication.root')
        stages = {'source_put': {'key': 'source', 'body': ref}, 'metadata_put': {'key': 'metadata', 'body': ref},
                  'checksum': {'source_sha256': ref['sha256'], 'metadata_sha256': ref['sha256']},
                  'readback': {'source': ref, 'metadata': ref}, 'ack': ref, 'status': ref}
        value['entities'] = [{'archive_id': 'synthetic-root', 'role': 'root', 'stages': stages}]
        self.assertTrue(final.observations(case, value, directory, self.packet))
        for stage in final.PUBLICATION:
            mutated = copy.deepcopy(value); del mutated['entities'][0]['stages'][stage]
            with self.assertRaises(final.Failure):
                final.observations(case, mutated, directory, self.packet)
        stages['readback']['source'] = dict(ref, sha256='0'*64)
        with self.assertRaisesRegex(final.Failure, 'disagree'):
            final.observations(case, value, directory, self.packet)

    def test_resource_requires_actual_scale_measurements_caps_and_unchanged_work(self):
        case, value, directory, _ = self.observation('resource.512')
        value.update(measurements={name: 1 for name in final.METRICS}, machine=final.platform.node(),
                     budget_files=self.packet['budget_files'], full_scale=True, sessions=512)
        self.assertTrue(final.observations(case, value, directory, self.packet))
        for key, invalid in (('sessions', 128), ('full_scale', False), ('measurements', {})):
            changed = dict(value, **{key: invalid})
            with self.assertRaises(final.Failure):
                final.observations(case, changed, directory, self.packet)
        value['measurements']['ledger_peak_bytes'] = final.BUDGETS['pass_read_bytes'] + 1
        with self.assertRaises(final.Failure):
            final.observations(case, value, directory, self.packet)

    def test_lifecycle_facts_cannot_be_replaced_by_a_pass_assertion(self):
        for name in ('storage.frozen_retry_source_gone', 'storage.failure_boundaries',
                     'storage.child_expiry_links', 'import.exact_undo_live_overlap',
                     'privacy.exclusion_dependencies', 'resource.four_agent_pressure'):
            case, value, directory, _ = self.observation(name)
            value['facts'] = {}
            with self.subTest(case=name), self.assertRaisesRegex(final.Failure, 'facts differ'):
                final.observations(case, value, directory, self.packet)

    def test_actual_old_binary_and_schema_identity_required(self):
        case, value, directory, _ = self.observation('compat.old_writer')
        value.update(binary_sha256=self.packet['binaries']['old_writer']['sha256'],
                     schemas=self.packet['schema_files'])
        self.assertTrue(final.observations(case, value, directory, self.packet))
        for key, invalid in (('binary_sha256', self.packet['binaries']['archive']['sha256']), ('schemas', [])):
            with self.assertRaises(final.Failure):
                final.observations(case, dict(value, **{key: invalid}), directory, self.packet)

    def test_warm_tick_and_child_graph_repeated_parent_work_fail(self):
        for name in ('resource.warm_unchanged', 'resource.child_graph'):
            case, value, directory, _ = self.observation(name)
            value.update(measurements={name: 1 for name in final.METRICS}, machine=final.platform.node(),
                         budget_files=self.packet['budget_files'], full_scale=True,
                         unchanged={'full_filter': 0, 'metadata_put': 0, 'store_walk': 0},
                         parent_analyses=1, full_parent_reads=1)
            self.assertTrue(final.observations(case, value, directory, self.packet))
            if name.endswith('warm_unchanged'):
                value['unchanged']['metadata_put'] = 1
            else:
                value['full_parent_reads'] = 2
            with self.assertRaises(final.Failure):
                final.observations(case, value, directory, self.packet)

    def test_crossbuild_cannot_certify_native_execution(self):
        name = 'native.linux-amd64.exec.fresh'
        case, value, directory, _ = self.observation(name)
        value['execution'] = 'cross-build'
        with self.assertRaisesRegex(final.Failure, 'not native'):
            final.observations(case, value, directory, self.packet)

    def test_process_receipt_argv_and_cleanup_forgery_rejected(self):
        output, report = self.run_packet()
        receipt_path = output / 'case-0000/process.json'
        original = final.load(receipt_path)
        for key, changed in (('argv', ['wrong']), ('cleanup', {'leader_reaped': True, 'group_absent': False})):
            receipt = dict(original, **{key: changed})
            receipt_path.write_bytes(final.encode(receipt))
            report['cases'][self.packet['cases'][0]['id']]['files']['process.json'] = final.probe.file_digest(receipt_path)
            (output / 'report.json').write_bytes(final.encode(report))
            with self.assertRaises(final.Failure):
                final.verify(self.packet, self.repo, output)


if __name__ == '__main__':
    unittest.main()
