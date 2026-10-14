import copy
import json
import os
from pathlib import Path
import tempfile
import subprocess
import unittest
from unittest import mock

import generate_go_shard_hints as hints
import run_ci_tier

CLI = hints.runner.CLI
PKGS = ['example/a', 'example/b']
NAMES = ['TestAlpha', 'TestBeta']
SETTINGS = {str(i): {'cpus': 4, 'package_p': 1, 'isolated_packages': [],
                     'ordinary_cpus': 2, 'cli_cpus': 2} for i in range(2)}


def stream(path, rows):
    path.write_text('\n'.join(json.dumps(r) for r in rows))


def fixture(root, duration):
    catalog = {'source': 'accepted-source', 'packages': {p: True for p in PKGS + [CLI]},
               'cli': NAMES, 'assignments': [{'packages': [PKGS[i]], 'cli': [NAMES[i]]} for i in range(2)]}
    for i in range(2):
        p = root / str(i)
        p.mkdir(parents=True)
        manifest = {'shard': i, 'catalog': catalog, 'catalog_sha256': hints.runner.digest(catalog),
                    'success': True, 'coverage': catalog['assignments'][i],
                    'settings': SETTINGS[str(i)], 'phases': {'vet_seconds': 55 if i == 0 else 0}}
        (p / 'manifest.json').write_text(json.dumps(manifest))
        stream(p / 'packages.json', [{'ImportPath': n, 'Dir': '/tmp/example', 'TestGoFiles': ['test.go']} for n in PKGS + [CLI]])
        stream(p / 'listing.json', [{'Package': CLI, 'Action': 'output', 'Output': n + '\n'} for n in NAMES] + [{'Package': CLI, 'Action': 'pass'}])
        stream(p / 'ordinary.json', [{'Package': PKGS[i], 'Action': 'pass', 'Elapsed': duration + i}])
        stream(p / 'cli.json', [{'Package': CLI, 'Test': NAMES[i], 'Action': 'run', 'Time': '2026-01-01T00:00:00Z'},
                               {'Package': CLI, 'Test': NAMES[i], 'Action': 'pass', 'Elapsed': duration,
                                'Time': f'2026-01-01T00:00:0{duration}Z'},
                               {'Package': CLI, 'Action': 'pass', 'Elapsed': duration + 1}])
        manifest['tier'] = 'pr'
        manifest['body_evidence'] = run_ci_tier.body_evidence(
            (p / 'ordinary.json').read_text() + '\n' + (p / 'cli.json').read_text(), short=True)
        (p / 'manifest.json').write_text(json.dumps(manifest))


class GeneratorTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        for i, duration in enumerate([1, 3]):
            fixture(self.root / str(i), duration)
        self.samples = [(f'run{i}', self.root / str(i)) for i in range(2)]

    def generate(self, samples=None):
        return hints.generate(samples or self.samples, 'accepted-source', SETTINGS,
                              PKGS + ['example/new'], NAMES + ['TestUnknown'])

    def mutate(self, update, file='manifest.json', shard='0'):
        path = self.root / '0' / shard / file
        value = json.loads(path.read_text())
        update(value)
        path.write_text(json.dumps(value))

    def test_medians_positive_unknowns_deterministic_and_provenance(self):
        result = self.generate()
        self.assertEqual(result['packages'], {'example/a': 2000, 'example/b': 3000, 'example/new': 1000})
        self.assertEqual(result['cli'], {'TestAlpha': 2000, 'TestBeta': 2000, 'TestUnknown': 1000})
        self.assertEqual(result, self.generate())
        self.assertEqual(result['provenance']['source'], 'accepted-source')
        self.assertEqual(result['provenance']['settings'], SETTINGS)
        self.assertEqual(len(result['provenance']['samples']), 2)
        self.assertTrue(result['provenance']['samples'][0]['files'])
        self.assertEqual(hints.positive_ms(0), 1)
        self.assertEqual(hints.positive_ms('0.0015'), 2)
        for invalid in [-1, float('inf'), float('nan'), None, True, 'bad']:
            with self.assertRaises(hints.runner.Failure):
                hints.positive_ms(invalid)

    def timed(self, name, action, seconds, elapsed=None):
        record = {'Test': name, 'Action': action, 'Time': f'2026-01-01T00:{seconds // 60:02d}:{seconds % 60:02d}Z'}
        if elapsed is not None:
            record['Elapsed'] = elapsed
        return record

    def test_parallel_cohort_queue_and_prelude_count_once(self):
        e = self.timed
        records = [e('TestAlpha', 'run', 0), e('TestAlpha', 'pause', 1),
                   e('TestAlpha', 'cont', 40), e('TestAlpha/left', 'run', 41),
                   e('TestAlpha/left', 'pause', 42), e('TestAlpha/right', 'run', 42),
                   e('TestAlpha/right', 'pause', 43), e('TestAlpha/left', 'cont', 80),
                   e('TestAlpha/left', 'pass', 86, 7), e('TestAlpha/right', 'cont', 90),
                   e('TestAlpha/right', 'pass', 97, 8), e('TestAlpha', 'pass', 97, 4)]
        self.assertEqual(hints.cli_durations(records, ['TestAlpha']), {'TestAlpha': 17})

    def test_sequential_parent_with_nested_parallel_children_excludes_wait(self):
        e = self.timed
        records = [e('TestAlpha', 'run', 0), e('TestAlpha/seq', 'run', 1),
                   e('TestAlpha/seq/par', 'run', 2), e('TestAlpha/seq/par', 'pause', 3),
                   e('TestAlpha/seq/par', 'cont', 30), e('TestAlpha/seq/par', 'pass', 34, 5),
                   e('TestAlpha/seq', 'pass', 34, 3), e('TestAlpha', 'pass', 35, 35)]
        self.assertEqual(hints.cli_durations(records, ['TestAlpha']), {'TestAlpha': 9})

    def test_overlapping_trees_and_skipped_bodies(self):
        e = self.timed
        records = [e('TestAlpha', 'run', 0), e('TestBeta', 'run', 0),
                   e('TestAlpha/skip', 'run', 1), e('TestAlpha/skip', 'skip', 2, 1),
                   e('TestAlpha', 'pass', 3, 3), e('TestBeta', 'skip', 4, 4)]
        self.assertEqual(hints.cli_durations(records, NAMES), {'TestAlpha': 3, 'TestBeta': 4})
        for replacement in ('fail', 'cont', 'pause'):
            broken = copy.deepcopy(records)
            broken[-1]['Action'] = replacement
            with self.assertRaises(hints.runner.Failure): hints.cli_durations(broken, NAMES)
        with self.assertRaises(hints.runner.Failure): hints.cli_durations(records + [records[-1]], NAMES)

    def test_slash_named_parallel_siblings_do_not_invent_a_parent(self):
        e = self.timed
        records = [e('TestAlpha', 'run', 0), e('TestAlpha/private', 'run', 1),
                   e('TestAlpha/private', 'pause', 2), e('TestAlpha/private/child', 'run', 3),
                   e('TestAlpha/private/child', 'pause', 4), e('TestAlpha/private', 'cont', 20),
                   e('TestAlpha/private', 'pass', 21, 2), e('TestAlpha/private/child', 'cont', 22),
                   e('TestAlpha/private/child', 'pass', 24, 3), e('TestAlpha', 'pass', 24, 5)]
        self.assertEqual(hints.cli_durations(records, ['TestAlpha']), {'TestAlpha': 8})

    def test_rfc3339_nanoseconds_parse_without_float_rounding(self):
        self.assertEqual(hints.stamp({'Time': '2026-01-01T00:00:00.123456789Z'}) -
                         hints.stamp({'Time': '2026-01-01T00:00:00Z'}), hints.Decimal('.123456789'))
        for fractional in ('1', '1234', '1234567'):
            hints.stamp({'Time': f'2026-01-01T00:00:00.{fractional}Z'})
        for bad in ('2026-01-01T00:00:00.1234567890Z', '2026-02-30T00:00:00Z', '2026-01-01T25:00:00Z'):
            with self.assertRaises(hints.runner.Failure): hints.stamp({'Time': bad})

    def test_timezone_offsets_reject_normalized_invalid_minutes(self):
        utc = hints.stamp({'Time': '2026-01-01T00:00:00.123456789Z'})
        for valid in ('2026-01-01T01:30:00.123456789+01:30',
                      '2025-12-31T22:30:00.123456789-01:30'):
            self.assertEqual(hints.stamp({'Time': valid}), utc)
        for offset in ('+00:60', '-00:60', '+01:99', '-01:99', '+24:00', '-24:00'):
            with self.subTest(offset=offset), self.assertRaises(hints.runner.Failure):
                hints.stamp({'Time': '2026-01-01T00:00:00' + offset})

    def test_package_cost_exclusion_is_observed_sequential_body_residual(self):
        exclusions = {PKGS[0]: 'TestAudit'}
        for _, sample in self.samples:
            path = sample/'0'/'ordinary.json'
            rows = hints.json_stream(path.read_text())
            rows[:0] = [{'Action':'run','Package':PKGS[0],'Test':'TestAudit'},
                        {'Action':'pass','Package':PKGS[0],'Test':'TestAudit','Elapsed':.5}]
            stream(path, rows)
            manifest_path=sample/'0'/'manifest.json'; manifest=json.loads(manifest_path.read_text())
            manifest['body_evidence']=run_ci_tier.body_evidence(path.read_text()+'\n'+(sample/'0'/'cli.json').read_text(),True)
            manifest_path.write_text(json.dumps(manifest))
        result=hints.generate(self.samples,'accepted-source',SETTINGS,PKGS,NAMES,package_cost_exclusions=exclusions)
        self.assertEqual(result['packages'][PKGS[0]],1500)
        self.assertEqual(result['provenance']['samples'][0]['package_cost_exclusions'][0]['derived_remaining_seconds'],'0.5')
        with self.assertRaises(hints.runner.Failure):
            hints.generate(self.samples,'accepted-source',SETTINGS,PKGS,NAMES,package_cost_exclusions={PKGS[0]:'TestMissing'})
        path=self.samples[0][1]/'0'/'ordinary.json'; rows=hints.json_stream(path.read_text()); rows[1]['Action']='skip'; stream(path,rows)
        with self.assertRaises(hints.runner.Failure):
            hints.generate(self.samples,'accepted-source',SETTINGS,PKGS,NAMES,package_cost_exclusions=exclusions)

    def test_failed_manifest_rejected(self):
        self.mutate(lambda m: m.update(success=False))
        with self.assertRaises(hints.runner.Failure): self.generate()

    def test_mixed_source_rejected(self):
        self.mutate(lambda m: m['catalog'].update(source='other'))
        with self.assertRaises(hints.runner.Failure): self.generate()

    def test_settings_mismatch_rejected(self):
        self.mutate(lambda m: m['settings'].update(cpus=8))
        with self.assertRaises(hints.runner.Failure): self.generate()

    def test_manifest_missing_assignment_rejected(self):
        self.mutate(lambda m: m['coverage'].update(cli=[]))
        with self.assertRaises(hints.runner.Failure): self.generate()

    def test_actual_missing_cli_terminal_rejected(self):
        path = self.root / '0' / '0' / 'cli.json'
        rows = hints.json_stream(path.read_text())
        stream(path, [r for r in rows if r.get('Test') != 'TestAlpha' or r['Action'] != 'pass'])
        with self.assertRaises(hints.runner.Failure): self.generate()

    def test_actual_failure_rejected_despite_success_manifest(self):
        stream(self.root / '0' / '0' / 'ordinary.json', [{'Package': PKGS[0], 'Action': 'fail'}])
        with self.assertRaises(hints.runner.Failure): self.generate()

    def test_missing_shard_rejected(self):
        (self.root / '0' / '1' / 'manifest.json').unlink()
        with self.assertRaises(hints.runner.Failure): self.generate()

    def test_discovery_mismatch_rejected(self):
        stream(self.root / '0' / '0' / 'listing.json', [{'Package': CLI, 'Action': 'output', 'Output': 'TestOther\n'}, {'Package': CLI, 'Action': 'pass'}])
        with self.assertRaises(hints.runner.Failure): self.generate()

    def test_duplicate_samples_rejected(self):
        with self.assertRaises(hints.runner.Failure):
            self.generate([self.samples[0], self.samples[0]])

    def test_object_stream_parser_matches_pretty_and_line_objects(self):
        self.assertEqual(hints.json_stream(' \n{ "one": 1 }\n{\n "two": 2\n}\n'), [{'one': 1}, {'two': 2}])
        with self.assertRaises(hints.runner.Failure): hints.json_stream('[1, 2]')

    def test_actual_body_proof_and_tier_required(self):
        for change in (lambda m: m['body_evidence'].update(actual=[]),
                       lambda m: m.update(tier='daily')):
            original = (self.root / '0' / '0' / 'manifest.json').read_text()
            self.mutate(change)
            with self.assertRaises(hints.runner.Failure): self.generate()
            (self.root / '0' / '0' / 'manifest.json').write_text(original)

    def test_provenance_hashes_actual_bytes_not_only_duration(self):
        before = self.generate()
        p = self.root / '0' / '0' / 'ordinary.json'
        p.write_text(p.read_text() + '\n')
        after = self.generate()
        self.assertEqual(before['packages'], after['packages'])
        self.assertNotEqual(before['provenance']['samples'], after['provenance']['samples'])

    def test_parser_contract_restored_after_failure_and_success(self):
        original = hints.runner.json_stream
        self.generate()
        self.assertIs(hints.runner.json_stream, original)
        self.mutate(lambda m: m.update(success=False))
        with self.assertRaises(hints.runner.Failure): self.generate()
        self.assertIs(hints.runner.json_stream, original)

    def test_pinned_source_catalog_validates_historical_projection_and_restores_current(self):
        source_catalog = run_ci_tier.catalog()
        changed = copy.deepcopy(source_catalog)
        changed['restorations'].append({'package': 'internal/cli', 'test': 'TestAlpha',
                                        'restore': 'weekly', 'pr': 'new projection'})
        with mock.patch.object(run_ci_tier, 'catalog', return_value=changed) as current:
            with self.assertRaises(hints.runner.Failure): self.generate()
            result = hints.generate(self.samples, 'accepted-source', SETTINGS, PKGS, NAMES, source_catalog)
            self.assertEqual(result['cli']['TestAlpha'], 2000)
            self.assertIs(run_ci_tier.catalog, current)
        self.assertEqual(result['provenance']['body_catalog_sha256'], hints.runner.digest(source_catalog))

    def test_imported_source_uses_request_and_restores_ci_event_source(self):
        for event_source in (None, 'current-ci-merge-source'):
            with mock.patch.dict(os.environ):
                if event_source is None:
                    os.environ.pop('GITHUB_SHA', None)
                else:
                    os.environ['GITHUB_SHA'] = event_source
                self.assertEqual(self.generate()['provenance']['source'], 'accepted-source')
                self.assertEqual(os.environ.get('GITHUB_SHA'), event_source)
                with self.assertRaises(hints.runner.Failure):
                    hints.generate(self.samples, 'wrong-request-source', SETTINGS, PKGS, NAMES)
                self.assertEqual(os.environ.get('GITHUB_SHA'), event_source)

    def test_measured_source_requires_real_commit_regular_git_catalog_bytes(self):
        repo = self.root / 'git-proof'; repo.mkdir()
        def git(*args):
            return subprocess.check_output(['git', '-C', str(repo), *args], stderr=subprocess.PIPE, timeout=20).decode().strip()
        git('init', '-q'); git('config', 'user.name', 'Test'); git('config', 'user.email', 'test@example.invalid')
        (repo/'scripts').mkdir(); catalog_path=repo/'scripts/ci_tiers.json'
        raw=json.dumps({'schema':1,'restorations':[]})+'\n'; catalog_path.write_text(raw)
        git('add', '.'); git('commit', '-qm', 'catalog'); source=git('rev-parse','HEAD')
        with mock.patch.object(hints.runner,'ROOT',repo):
            proof, actual=hints.measured_source(source,raw)
            self.assertEqual(proof,{'source':source,'tree':git('rev-parse','HEAD^{tree}')})
            self.assertEqual(actual,raw)
            for bad in ('f'*40, 'HEAD', git('rev-parse','HEAD^{tree}')):
                with self.assertRaises(hints.runner.Failure):hints.measured_source(bad,raw)
            with self.assertRaises(hints.runner.Failure):hints.measured_source(source,raw+' ')
            # Construct the symlink Git mode without following a host link.
            blob=git('rev-parse','HEAD:scripts/ci_tiers.json')
            git('update-index','--cacheinfo','120000,'+blob+',scripts/ci_tiers.json')
            git('commit','-qm','symlink')
            with self.assertRaises(hints.runner.Failure):hints.measured_source(git('rev-parse','HEAD'))

    def test_owned_historical_audit_uses_git_proof_and_preserves_live_guard(self):
        catalog=run_ci_tier.catalog(); proof={'source':'accepted-source','tree':'b'*40}
        settings=copy.deepcopy(SETTINGS)
        for value in settings.values():value['plain_audit_owned']=True
        for _,sample in self.samples:
            for i in range(2):
                path=sample/str(i)/'manifest.json';m=json.loads(path.read_text());m['settings']=settings[str(i)];path.write_text(json.dumps(m))
        audit=self.root/'audit';audit.mkdir()
        rows=[{'Package':run_ci_tier.SOURCE_AUDIT_PACKAGE,'Test':run_ci_tier.SOURCE_AUDIT_TEST,'Action':'run'},
              {'Package':run_ci_tier.SOURCE_AUDIT_PACKAGE,'Test':run_ci_tier.SOURCE_AUDIT_TEST,'Action':'pass','Elapsed':5},
              {'Package':run_ci_tier.SOURCE_AUDIT_PACKAGE,'Action':'pass','Elapsed':6}]
        stream(audit/'source-audit.jsonl',rows)
        report={'schema':1,'source':proof['source'],'source_proof':proof,'success':True,'tier':'pr','suite':'audit',
                'invocations':[{'command':run_ci_tier.SOURCE_AUDIT_COMMAND,'log':'source-audit.jsonl',
                                'performance_enabled':False,'real_systemd_enabled':False,
                                'evidence':run_ci_tier.source_audit_body((audit/'source-audit.jsonl').read_text())}]}
        (audit/'tier.json').write_text(json.dumps(report))
        def generate():
            return hints.generate(self.samples,proof['source'],settings,PKGS,NAMES,catalog,
                                  audit_directories={sid:audit for sid,_ in self.samples})
        with mock.patch.object(hints,'measured_source',return_value=(proof,json.dumps(catalog))), mock.patch.object(run_ci_tier.full_race,'source_proof',return_value={'source':'current','tree':'c'*40}):
            result=generate()
            self.assertEqual(result['owned_plain_audit_exclusions_ms'][run_ci_tier.SOURCE_AUDIT_PACKAGE],5000)
            self.assertEqual(len(result['provenance']['samples'][0]['plain_audit']['files']),2)
            # Future owned-vet samples bind their requested context/source and
            # fingerprint the separate actual vet log without breaking old samples.
            import test_ci_tiers
            context=test_ci_tiers.PlainVetTest.context
            for value in settings.values():
                value.update(plain_vet_owned=True,plain_vet_context_sha256=hints.runner.digest(context),plain_vet_source_proof=proof)
            for _,sample in self.samples:
                for i in range(2):
                    path=sample/str(i)/'manifest.json';manifest=json.loads(path.read_text());manifest['settings']=settings[str(i)];path.write_text(json.dumps(manifest))
            (audit/'plain-vet.txt').write_text('')
            report['plain_vet']={'command':run_ci_tier.PLAIN_VET_COMMAND,'log':'plain-vet.txt','exit_code':0,
                'pre_source_proof':proof,'post_source_proof':proof,'context':context,
                'log_sha256':hints.hashlib.sha256(b'').hexdigest(),'performance_enabled':False,'real_systemd_enabled':False}
            (audit/'tier.json').write_text(json.dumps(report))
            owned=generate()
            self.assertIn('plain-vet.txt',[f['file'] for f in owned['provenance']['samples'][0]['plain_audit']['files']])
            (audit/'plain-vet.txt').unlink()
            with self.assertRaises(hints.runner.Failure):generate()
            (audit/'plain-vet.txt').write_text('')
            with self.assertRaises(run_ci_tier.Failure):run_ci_tier.verify_source_audit(audit,proof['source'])
            with self.assertRaises(hints.runner.Failure):hints.generate(self.samples,proof['source'],settings,PKGS,NAMES,catalog)
            for update in (lambda r:r['source_proof'].update(tree='c'*40),lambda r:r.update(source='wrong'),
                           lambda r:r['invocations'][0]['command'].append('-race')):
                bad=copy.deepcopy(report);update(bad);(audit/'tier.json').write_text(json.dumps(bad))
                with self.assertRaises(hints.runner.Failure):generate()
            (audit/'tier.json').write_text(json.dumps(report))
            for bad in (rows[1:], rows[:-1], rows+[rows[-1]], [dict(r,Action='skip') if r.get('Test') and r['Action']=='pass' else r for r in rows], [rows[-1]]):
                stream(audit/'source-audit.jsonl',bad)
                with self.assertRaises(hints.runner.Failure):generate()


    def test_parent_elapsed_larger_than_envelope_and_missing_start(self):
        rows = [{'Test': 'TestAlpha', 'Action': 'run', 'Time': '2026-01-01T00:00:00Z'},
                {'Test': 'TestAlpha', 'Action': 'pass', 'Elapsed': 4, 'Time': '2026-01-01T00:00:01Z'}]
        self.assertEqual(hints.cli_durations(rows, ['TestAlpha']), {'TestAlpha': 4})
        with self.assertRaises(hints.runner.Failure): hints.cli_durations(rows[1:], ['TestAlpha'])
        rows[1]['Time'] = '2025-12-31T23:59:59Z'
        with self.assertRaises(hints.runner.Failure): hints.cli_durations(rows, ['TestAlpha'])

    def test_cli_timestamps_require_timezone_and_valid_text(self):
        for value in ('2026-01-01T00:00:00', True, None, 'bad'):
            with self.assertRaises(hints.runner.Failure): hints.stamp({'Time': value})
        self.assertEqual(hints.stamp({'Time': '2026-01-01T00:00:00Z'}),
                         hints.stamp({'Time': '2026-01-01T01:00:00+01:00'}))


if __name__ == '__main__':
    unittest.main()
