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

PROOF = {'source': 'a'*40, 'tree': 'b'*40}
CONTEXT = {'GOOS': 'darwin', 'CGO_ENABLED': '1', 'GOVERSION': 'go1.27.2'}


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
            listing += [dict(Action='output', Package=package, Output=name+'\n') for name in names]
            listing.append(dict(Action='pass', Package=package))
        (directory/'native-list.jsonl').write_text(lines(listing))
        (directory/'native-build.log').write_text('')
        native = dict(schema=1, source_proof=copy.deepcopy(PROOF), post_source_proof=copy.deepcopy(PROOF),
                      context=copy.deepcopy(CONTEXT), sensitive=sensitive, build_command=['go', 'build', './...'],
                      build_sha256=hashlib.sha256(b'').hexdigest(), discovery=inventory,
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
        with mock.patch.object(tier.full_race, 'source_proof', return_value=PROOF):
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
                   lambda r: r['native_smoke']['context'].update(CGO_ENABLED='0'),
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
        for corruption in ('child', 'failed', 'malformed', 'name', 'parent', 'no-run', 'skipped'):
            with self.subTest(corruption=corruption), tempfile.TemporaryDirectory() as d:
                directory = Path(d)
                report = self.fixture(directory, True)
                target = directory/'native-cli.jsonl'
                events = [json.loads(line) for line in target.read_text().splitlines()]
                if corruption == 'child':
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
                if corruption in ('failed', 'parent', 'no-run', 'skipped'):
                    try:
                        report['invocations'][1]['evidence'] = tier.body_evidence(target.read_text(), True)
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
                with mock.patch.object(tier.subprocess, 'check_output', return_value=json.dumps(CONTEXT).encode()), mock.patch.object(tier.full_race, 'source_proof', return_value=PROOF), mock.patch.object(tier, 'checked', side_effect=checked), mock.patch.object(tier, 'discover', return_value=template['native_smoke']['discovery']):
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
            with mock.patch.object(tier.subprocess, 'check_output', return_value=json.dumps(CONTEXT).encode()), mock.patch.object(tier.full_race, 'source_proof', return_value=PROOF), mock.patch.object(tier, 'checked', side_effect=checked), mock.patch.object(tier, 'discover', return_value=template['native_smoke']['discovery']):
                with self.assertRaisesRegex(tier.Failure, 'native command failed'):
                    tier.run_native_smoke(directory, 100, report, lambda: None, execute, False)
                self.assertIn('native-cli', report['native_smoke']['phase_seconds'])
                self.assertNotIn('post_source_proof', report['native_smoke'])
                with self.assertRaises(tier.Failure):
                    self.verify(directory, report)


if __name__ == '__main__':
    unittest.main()
