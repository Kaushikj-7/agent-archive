"""Native smoke proof and complete-selection regression tests, no host mutation."""
import copy
import hashlib
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest import mock

import run_ci_tier as tier
from test_go_cache_context import metadata

PROOF = {'source': 'a'*40, 'tree': 'b'*40}
CONTEXT = {'GOOS': 'darwin', 'CGO_ENABLED': '1', 'GOVERSION': 'go1.27.2'}



def canonical_context():
    """Synthetic unit fixture built through the actual shared schema validator."""
    go = metadata()
    go.update(GOOS='darwin', GOHOSTOS='darwin', GOARCH='arm64', GOHOSTARCH='arm64', GOENV='off')
    def run(argv):
        if argv == ['go', 'version']:
            return b'go version go1.27.2 darwin/arm64\n'
        if argv == ['go', 'env', '-json', *tier.cache_keys.GO_FIELDS]:
            return json.dumps(go).encode()
        return ('synthetic unit metadata: '+' '.join(argv)).encode()
    context = tier.cache_keys.runtime_context(run=run, env={
        'RUNNER_OS': 'macOS', 'RUNNER_ARCH': 'ARM64', 'ImageOS': 'unit-macos', 'ImageVersion': 'unit-1'},
        compiler=lambda name: {'binary': 'c'*64, 'version': 'd'*64},
        toolchain=lambda go: dict.fromkeys(('go', 'compile', 'asm', 'link', 'cgo', 'vet'), 'e'*64), native=True)
    return tier.cache_keys.validate_context(context, native=True)


CANONICAL = canonical_context()


def lines(events):
    return '\n'.join(json.dumps(event) for event in events)+'\n'


class NativeSmokeTest(unittest.TestCase):
    def fixture(self, directory, sensitive=False):
        selected = tier.native_selections(sensitive)
        inventory = {tier.MODULE+p[2:]: ['TestNative', 'FuzzNative'] for p in tier.NATIVE_PACKAGES}
        inventory.update({p: names+['TestUnselected'] for p, names in selected.items()})
        inventory = {p: sorted(names) for p, names in inventory.items()}
        listing = []
        for package, names in inventory.items():
            if package not in selected:
                continue
            listing += [dict(Action='output', Package=package, Output=name+'\n') for name in names]
            listing.append(dict(Action='pass', Package=package))
        (directory/'native-list.jsonl').write_text(lines(listing))
        (directory/'native-build.log').write_text('')
        native = dict(schema=1, source_proof=copy.deepcopy(PROOF), post_source_proof=copy.deepcopy(PROOF),
                      context=copy.deepcopy(CANONICAL), post_context=copy.deepcopy(CANONICAL),
                      context_sha256=tier.cache_keys.digest(CANONICAL), qualification='hosted', sensitive=sensitive, build_command=['go', 'build', './...'],
                      build_sha256=hashlib.sha256(b'').hexdigest(), discovery={p: inventory[p] for p in selected},
                      phase_seconds={'build': 1.0, 'discovery': 1.0})
        report = dict(invocations=[], native_smoke=native)
        for name, command in tier.native_commands(sensitive):
            wanted = ({p: inventory[p] for p in inventory if p not in selected} if name == 'native-packages'
                      else tier.native_selected_phases(sensitive)[name])
            raw = []
            for package, names in wanted.items():
                for parent in names:
                    raw.extend([dict(Action='run', Package=package, Test=parent),
                                dict(Action='run', Package=package, Test=parent+'/child'),
                                dict(Action='pass', Package=package, Test=parent+'/child'),
                                dict(Action='pass', Package=package, Test=parent)])
                raw.append(dict(Action='pass', Package=package))
            text = lines(raw)
            (directory/(name+'.jsonl')).write_text(text)
            report['invocations'].append(dict(command=command, log=name+'.jsonl',
                performance_enabled=False, real_systemd_enabled=False,
                evidence=tier.body_evidence(text, True)))
            native['phase_seconds'][name] = 1.0
        return report

    def verify(self, directory, report, sensitive=False):
        with mock.patch.object(tier.full_race, 'source_proof', return_value=PROOF), mock.patch.object(tier, 'native_host_context', return_value=copy.deepcopy(CANONICAL)):
            tier.verify_native_smoke(directory, report, sensitive)

    def test_base_and_conditional_complete_with_original_separate_deadlines(self):
        for sensitive in (False, True):
            with self.subTest(sensitive=sensitive), tempfile.TemporaryDirectory() as d:
                report = self.fixture(Path(d), sensitive)
                self.verify(Path(d), report, sensitive)
                cli = [i for i in report['invocations'] if i['log'] == 'native-cli.jsonl']
                self.assertEqual(len(cli), 1)
                self.assertEqual(cli[0]['command'][cli[0]['command'].index('-timeout')+1], '10m')
                if sensitive:
                    extra = next(i for i in report['invocations'] if i['log'] == 'native-sensitive-cli.jsonl')
                    self.assertEqual(extra['command'][extra['command'].index('-timeout')+1], '20m')
                self.assertEqual(len(tier.native_selections(sensitive)[tier.MODULE+'internal/cli']), 15 if sensitive else 12)
                self.assertFalse(any('writer' in str(i) for i in report['invocations']))

    def test_missing_selected_parent_missing_phase_source_and_weakened_commands(self):
        changes = [lambda r: r['invocations'].pop(),
                   lambda r: r['native_smoke'].update(post_source_proof={'source': 'c'*40, 'tree': 'b'*40}),
                   lambda r: r['native_smoke'].update(sensitive=False),
                   lambda r: r['native_smoke']['context']['go'].update(CGO_ENABLED='0'),
                   lambda r: r['native_smoke']['context']['sdk'].pop('sdk_build'),
                   lambda r: r['native_smoke']['post_context']['sdk'].update(sdk_version='f'*64),
                   lambda r: r['native_smoke']['post_context']['toolchain'].update(compile='f'*64),
                   lambda r: r['native_smoke']['post_context']['image'].update(ImageVersion='unit-new'),
                   lambda r: r['invocations'][0]['command'].remove('-count=1'),
                   lambda r: r['native_smoke']['phase_seconds'].pop('build'),
                   lambda r: r['native_smoke'].update(build_command=['go','build','./internal/cli'])]
        for change in changes:
            with self.subTest(change=change), tempfile.TemporaryDirectory() as d:
                report = self.fixture(Path(d), True)
                change(report)
                with self.assertRaises(tier.Failure):
                    self.verify(Path(d), report, True)

    def test_missing_child_failed_lane_malformed_stream_and_missing_compiled_name(self):
        for corruption in ('child', 'failed', 'malformed', 'name', 'parent', 'no-run', 'skipped', 'ordinary-package', 'ordinary-body', 'ordinary-failed'):
            with self.subTest(corruption=corruption), tempfile.TemporaryDirectory() as d:
                directory = Path(d)
                report = self.fixture(directory, True)
                ordinary = corruption.startswith('ordinary-')
                target = directory/('native-packages.jsonl' if ordinary else 'native-cli.jsonl')
                events = [json.loads(line) for line in target.read_text().splitlines()]
                if corruption == 'ordinary-package':
                    events = [e for e in events if e['Package'] != tier.MODULE+'internal/credentials']
                elif corruption == 'ordinary-body':
                    events = [e for e in events if e['Package'] != tier.MODULE+'internal/credentials' or not e.get('Test')]
                elif corruption == 'ordinary-failed':
                    events[-1]['Action'] = 'fail'
                elif corruption == 'child':
                    events = [e for e in events if not (e.get('Test', '').endswith('/child') and e['Action'] == 'pass')]
                elif corruption == 'failed':
                    events[-1]['Action'] = 'fail'
                elif corruption == 'no-run':
                    events = [e for e in events if e['Action'] != 'run']
                elif corruption == 'skipped':
                    events = [dict(e, Action='skip') if e.get('Test', '').endswith('/child') and e['Action'] == 'pass' else e for e in events]
                elif corruption == 'parent':
                    missing = tier.NATIVE_CLI[0]
                    events = [e for e in events if e.get('Test', '').split('/')[0] != missing]
                elif corruption == 'name':
                    name = tier.NATIVE_CLI[0]
                    listing = directory/'native-list.jsonl'
                    listing.write_text(lines([e for e in map(json.loads, listing.read_text().splitlines()) if e.get('Output') != name+'\n']))
                    report['native_smoke']['discovery'] = tier.discover_from_log(listing.read_text())
                target.write_text('not-json\n' if corruption == 'malformed' else lines(events))
                # Recompute summarized evidence when possible: the verifier must
                # reject incomplete raw evidence, not only stale summary bytes.
                if ordinary or corruption in ('failed', 'parent', 'no-run', 'skipped'):
                    try:
                        report['invocations'][0 if ordinary else 1]['evidence'] = tier.body_evidence(target.read_text(), True)
                    except tier.Failure:
                        pass
                with self.assertRaises((tier.Failure, ValueError)):
                    self.verify(directory, report, True)

    def test_runner_records_each_phase_preserves_keychain_opt_in_and_builds_whole_tree(self):
        for sensitive in (False, True):
            with self.subTest(sensitive=sensitive), tempfile.TemporaryDirectory() as d:
                directory = Path(d)
                template = self.fixture(directory, sensitive)
                report = {'invocations': []}
                commands = []
                def checked(command, output, deadline, env=None):
                    commands.append(command)
                    self.assertEqual(command, ['go', 'build', './...'])
                    output.write_text('')
                    return ''
                def execute(command, name, **kwargs):
                    item = next(i for i in template['invocations'] if i['log'] == name+'.jsonl')
                    self.assertEqual(command, item['command'])
                    report['invocations'].append(item)
                with mock.patch.object(tier, 'native_host_context', return_value=copy.deepcopy(CANONICAL)), mock.patch.object(tier.full_race, 'source_proof', return_value=PROOF), mock.patch.object(tier, 'checked', side_effect=checked), mock.patch.object(tier, 'discover', return_value=template['native_smoke']['discovery']):
                    tier.run_native_smoke(directory, 100, report, lambda: None, execute, sensitive)
                self.assertEqual(commands, [['go', 'build', './...']])
                self.assertNotIn('AGENT_ARCHIVE_KEYCHAIN_ROUND_TRIP', os.environ)

    def test_failed_phase_retains_timing_and_cannot_produce_success_proof(self):
        with tempfile.TemporaryDirectory() as d:
            directory = Path(d)
            template = self.fixture(directory)
            report = {'invocations': []}
            def checked(command, output, deadline, env=None):
                output.write_text('')
                return ''
            def execute(command, name, **kwargs):
                if name == 'native-cli':
                    raise tier.Failure('native command failed')
                report['invocations'].append(next(i for i in template['invocations'] if i['log'] == name+'.jsonl'))
            with mock.patch.object(tier, 'native_host_context', return_value=copy.deepcopy(CANONICAL)), mock.patch.object(tier.full_race, 'source_proof', return_value=PROOF), mock.patch.object(tier, 'checked', side_effect=checked), mock.patch.object(tier, 'discover', return_value=template['native_smoke']['discovery']):
                with self.assertRaisesRegex(tier.Failure, 'native command failed'):
                    tier.run_native_smoke(directory, 100, report, lambda: None, execute, False)
                self.assertIn('native-cli', report['native_smoke']['phase_seconds'])
                self.assertNotIn('post_source_proof', report['native_smoke'])
                with self.assertRaises(tier.Failure):
                    self.verify(directory, report)

    def test_local_diagnostics_allow_dirty_inputs_and_never_qualify_hosted_receipts(self):
        with tempfile.TemporaryDirectory() as d, mock.patch.dict(os.environ, {}, clear=True):
            directory = Path(d)
            template = self.fixture(directory)
            commands = {tuple(command): name+'.jsonl' for name, command in tier.native_commands(False)}
            def checked(command, output, deadline, env=None):
                text = '' if command == ['go', 'build', './...'] else (directory/commands[tuple(command)]).read_text()
                output.write_text(text)
                return text
            with mock.patch.object(tier.subprocess, 'check_output', return_value=json.dumps(dict(CONTEXT, GOVERSION='go1.28.0')).encode()), mock.patch.object(tier, 'git', return_value='a'*40), mock.patch.object(tier.full_race, 'source_proof', side_effect=AssertionError('dirty local inputs must not claim a source proof')), mock.patch.object(tier, 'checked', side_effect=checked), mock.patch.object(tier, 'discover', return_value=template['native_smoke']['discovery']):
                tier.run_suite('pr', 'macos', directory, native_local=True)
            report = json.loads((directory/'tier.json').read_text())
            self.assertTrue(report['success'])
            self.assertEqual(report['native_smoke']['qualification'], 'local-diagnostic-only')
            self.assertIsNone(report['native_smoke']['context'])
            self.assertIsNone(report['source_proof'])
            self.assertIsNone(report['native_smoke']['post_source_proof'])
            with mock.patch.object(tier.full_race, 'source_proof', return_value=PROOF), mock.patch.object(tier, 'native_host_context', return_value=copy.deepcopy(CANONICAL)):
                with self.assertRaises(tier.Failure):
                    tier.verify_native_smoke(directory, report, False)
            with mock.patch.dict(os.environ, {'GITHUB_ACTIONS': 'true'}):
                with self.assertRaises(tier.Failure):
                    tier.run_suite('pr', 'macos', directory, native_local=True)
                with self.assertRaises(tier.Failure):
                    tier.verify_native_smoke(directory, report, False, local=True)

    def test_hosted_context_delegates_to_shared_native_policy_and_rejects_sdk_or_platform(self):
        with mock.patch.object(tier.cache_keys, 'runtime_context', return_value=copy.deepcopy(CANONICAL)) as collect:
            self.assertEqual(tier.native_host_context(), CANONICAL)
            collect.assert_called_once_with(native=True)
        bad = copy.deepcopy(CANONICAL)
        bad['sdk'].pop('sdk_build')
        with mock.patch.object(tier.cache_keys, 'runtime_context', return_value=bad):
            with self.assertRaises(tier.Failure):
                tier.native_host_context()
        linux = copy.deepcopy(CANONICAL)
        linux.pop('sdk')
        linux['go'].update(GOOS='linux', GOHOSTOS='linux')
        linux.update(version='go version go1.27.2 linux/arm64', runner_os='Linux')
        with mock.patch.object(tier.cache_keys, 'runtime_context', return_value=linux):
            with self.assertRaisesRegex(tier.Failure, 'requires Darwin'):
                tier.native_host_context()
        with mock.patch.object(tier.cache_keys, 'runtime_context', side_effect=tier.cache_keys.InvalidContext('image unavailable')):
            with self.assertRaisesRegex(tier.Failure, 'image unavailable'):
                tier.native_host_context()

    def test_canonical_post_execution_sdk_drift_cannot_qualify_even_with_passing_bodies(self):
        with tempfile.TemporaryDirectory() as d:
            directory = Path(d)
            template = self.fixture(directory)
            report = {'invocations': []}
            changed = copy.deepcopy(CANONICAL)
            changed['sdk']['sdk_build'] = 'f'*64
            def checked(command, output, deadline, env=None):
                output.write_text('')
                return ''
            def execute(command, name, **kwargs):
                report['invocations'].append(next(i for i in template['invocations'] if i['log'] == name+'.jsonl'))
            with mock.patch.object(tier, 'native_host_context', side_effect=[copy.deepcopy(CANONICAL), changed, copy.deepcopy(CANONICAL)]), mock.patch.object(tier.full_race, 'source_proof', return_value=PROOF), mock.patch.object(tier, 'checked', side_effect=checked), mock.patch.object(tier, 'discover', return_value=template['native_smoke']['discovery']):
                with self.assertRaises(tier.Failure):
                    tier.run_native_smoke(directory, 100, report, lambda: None, execute, False)

    def test_consistent_stale_sdk_receipt_still_must_match_current_canonical_host(self):
        with tempfile.TemporaryDirectory() as d:
            directory = Path(d)
            report = self.fixture(directory)
            changed = copy.deepcopy(CANONICAL)
            changed['sdk']['sdk_build'] = 'f'*64
            report['native_smoke'].update(context=changed, post_context=copy.deepcopy(changed),
                                          context_sha256=tier.cache_keys.digest(changed))
            with self.assertRaises(tier.Failure):
                self.verify(directory, report)


if __name__ == '__main__':
    unittest.main()
