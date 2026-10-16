"""Synthetic host fixtures for bounded manual cohort and cache state contracts."""
from dataclasses import asdict
import copy
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest import mock

import native_smoke_reference as reference
import run_ci_tier as tier
import test_macos_smoke as smoke_fixtures

CANONICAL, PROOF = smoke_fixtures.CANONICAL, smoke_fixtures.PROOF
from test_ci_workflow import jobs, ROOT, steps, step_key


class ReferenceTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name).resolve()
        self.out = self.root/'native-tier-results'
        self.context = copy.deepcopy(CANONICAL)
        self.source = {'tested': PROOF['source'], 'tree': PROOF['tree'], 'dependency': reference.dependency()}
        self.plan = reference.ci_cache.general_plan(self.context, self.source, self.root)
        reference.keys.prepare_paths(self.plan)
        reference.write(self.root/'compilation-cache-plan.json', {'plan': asdict(self.plan), 'context': self.context})
        self.env = {'GITHUB_ACTIONS': 'true', 'GITHUB_EVENT_NAME': 'workflow_dispatch',
                    'GITHUB_SHA': PROOF['source'], 'EXPECTED_NATIVE_SOURCE': PROOF['source'],
                    'CI_SUITE': reference.SUITE, 'NATIVE_SMOKE_MODE': 'normal',
                    'NATIVE_SMOKE_CACHE': 'cold', 'NATIVE_CACHE_STATE': 'miss',
                    'RUNNER_TEMP': str(self.root), 'GOCACHE': self.plan.paths[0], 'GOMODCACHE': self.plan.paths[1]}
        patcher = mock.patch.dict(os.environ, self.env, clear=True)
        patcher.start(); self.addCleanup(patcher.stop)
        patcher = mock.patch.object(reference, 'git_source', return_value=PROOF['source'])
        patcher.start(); self.addCleanup(patcher.stop)

    def empty(self):
        reference.assert_empty(self.out/'cold-start.json')

    def seed(self):
        reference.ci_cache.publish_manifest(self.plan, self.context)

    def test_guard_requires_exact_event_explicit_suite_actual_source_and_enums(self):
        self.assertEqual(reference.guard(), PROOF['source'])
        for key, value in [('GITHUB_EVENT_NAME', 'pull_request'), ('CI_SUITE', 'all'),
                           ('EXPECTED_NATIVE_SOURCE', 'short'), ('GITHUB_SHA', 'c'*40),
                           ('NATIVE_SMOKE_MODE', 'unknown'), ('NATIVE_SMOKE_CACHE', 'downloaded'),
                           ('GITHUB_ACTIONS', 'false')]:
            with self.subTest(key=key), mock.patch.dict(os.environ, {key: value}):
                with self.assertRaises(reference.Failure):
                    reference.guard()
        with mock.patch.object(reference, 'git_source', return_value='d'*40):
            with self.assertRaises(reference.Failure):
                reference.guard()

    def test_cold_requires_both_owned_caches_empty_before_modules(self):
        self.empty()
        for raw in self.plan.paths:
            with self.subTest(raw=raw):
                path = Path(raw)/'prior-object-or-module'
                path.write_text('old')
                with self.assertRaises(reference.Failure):
                    self.empty()
                path.unlink()
        with mock.patch.dict(os.environ, {'GOCACHE': str(self.root/'uncontrolled')}):
            with self.assertRaises(reference.Failure):
                self.empty()
        with mock.patch.dict(os.environ, {'NATIVE_SMOKE_CACHE': 'trusted-seed'}):
            with self.assertRaises(reference.Failure):
                self.empty()

    def test_observation_keeps_final_verified_state_and_binds_source_context_modules(self):
        for state in ('miss', 'corrupt-rebuilt'):
            value = reference.observe(state, self.out/'cache-observation.json')
            self.assertEqual(value['state'], state)
            reference.verify_observation(value, PROOF, self.context)
        with self.assertRaises(reference.Failure):
            reference.observe('trusted', self.out/'cache-observation.json')
        self.seed()
        value = reference.observe('trusted', self.out/'cache-observation.json')
        self.assertRegex(value['manifest_sha256'], '^[a-f0-9]{64}$')
        for mutation in (lambda v: v.update(source='d'*40), lambda v: v.update(tree='d'*40),
                         lambda v: v.update(context_sha256='f'*64), lambda v: v.update(state='downloaded'),
                         lambda v: v.update(seed='other'), lambda v: v.update(manifest_sha256=None)):
            changed = copy.deepcopy(value); mutation(changed)
            with self.assertRaises(reference.Failure):
                reference.verify_observation(changed, PROOF, self.context)
        with mock.patch.object(reference, 'dependency', return_value='d'*64):
            with self.assertRaises(reference.Failure):
                reference.observe('trusted', self.out/'cache-observation.json')

    def fake_native(self, tier_name, suite, output, extras):
        self.assertEqual((tier_name, suite), ('pr', 'macos'))
        self.assertEqual(extras['native'], os.environ['NATIVE_SMOKE_MODE'] == 'sensitive')
        value = {'success': True, 'source_proof': PROOF, 'native_smoke': {
            'qualification': 'hosted', 'context': self.context,
            'context_sha256': reference.keys.digest(self.context), 'phase_seconds': {'build': 1}}}
        reference.write(output/'tier.json', value)

    def test_four_explicit_cohorts_use_same_producer_and_keep_manual_labels(self):
        for mode in ('normal', 'sensitive'):
            for cache in ('cold', 'trusted-seed'):
                with self.subTest(mode=mode, cache=cache), mock.patch.dict(os.environ, {
                        'NATIVE_SMOKE_MODE': mode, 'NATIVE_SMOKE_CACHE': cache,
                        'NATIVE_CACHE_STATE': 'miss' if cache == 'cold' else 'trusted'}):
                    reference.keys.prepare_paths(self.plan, clear=True)
                    if cache == 'cold': self.empty()
                    else: self.seed()
                    with mock.patch.object(tier, 'run_suite', side_effect=self.fake_native) as execute:
                        reference.run(self.out)
                    execute.assert_called_once()
                    report = json.loads((self.out/'reference.json').read_text())
                    self.assertTrue(report['success'])
                    self.assertFalse(report['required_pr_gate']); self.assertFalse(report['p95_traffic'])
                    self.assertEqual(report['mode'], mode); self.assertEqual(report['cache_request'], cache)
                    self.assertEqual(report['source_proof'], PROOF)

    def test_warm_fallback_is_not_executed_or_relabelled_and_native_failure_is_preserved(self):
        for state in ('miss', 'corrupt-rebuilt'):
            with self.subTest(state=state), mock.patch.dict(os.environ, {'NATIVE_SMOKE_CACHE': 'trusted-seed', 'NATIVE_CACHE_STATE': state}), mock.patch.object(tier, 'run_suite') as execute:
                with self.assertRaisesRegex(reference.Failure, 'qualification unavailable'):
                    reference.run(self.out)
                execute.assert_not_called()
                report = json.loads((self.out/'reference.json').read_text())
                self.assertFalse(report['success']); self.assertEqual(report['cache_observation']['state'], state)
        self.empty()
        with mock.patch.object(tier, 'run_suite', side_effect=tier.Failure('actual native lane failed')):
            with self.assertRaises(tier.Failure):
                reference.run(self.out)
        self.assertFalse(json.loads((self.out/'reference.json').read_text())['success'])


    def test_producer_context_drift_does_not_mix_observation_and_completed_receipt(self):
        self.empty()
        def drift(*args, **kwargs):
            self.fake_native(*args, **kwargs)
            value = json.loads((self.out/'tier.json').read_text())
            value['native_smoke']['context']['sdk']['sdk_build'] = 'f'*64
            reference.write(self.out/'tier.json', value)
        with mock.patch.object(tier, 'run_suite', side_effect=drift):
            with self.assertRaises(reference.Failure):
                reference.run(self.out)
        self.assertFalse(json.loads((self.out/'reference.json').read_text())['success'])

    def test_comparison_requires_all_four_and_exact_source_and_image_identity(self):
        paths = []
        for mode in ('normal', 'sensitive'):
            for cache in ('cold', 'trusted-seed'):
                path = self.root/(mode+'-'+cache+'.json')
                reference.write(path, {'success': True, 'kind': 'manual-native-smoke-reference',
                    'required_pr_gate': False, 'p95_traffic': False, 'source': PROOF['source'],
                    'workflow_source': PROOF['source'], 'event': 'workflow_dispatch',
                    'source_proof': PROOF, 'context': self.context,
                    'context_sha256': reference.keys.digest(self.context), 'mode': mode,
                    'cache_request': cache, 'cache_observation': {'schema': 1, 'source': PROOF['source'],
                        'tree': PROOF['tree'], 'context_sha256': reference.keys.digest(self.context),
                        'manifest_sha256': 'e'*64 if cache == 'trusted-seed' else None,
                        'state': 'miss' if cache == 'cold' else 'trusted'},
                    'phase_seconds': {'build': 1}})
                paths.append(path)
        reference.compare(paths, self.root/'comparison.json')
        for mutation in (lambda r: r.update(source='c'*40),
                         lambda r: r['context']['image'].update(ImageVersion='different-image'),
                         lambda r: r['cache_observation'].update(state='corrupt-rebuilt'),
                         lambda r: r['cache_observation'].update(source='c'*40),
                         lambda r: r.update(event='pull_request')):
            value = json.loads(paths[-1].read_text()); changed = copy.deepcopy(value)
            mutation(changed); changed['context_sha256'] = reference.keys.digest(changed['context'])
            reference.write(paths[-1], changed)
            with self.assertRaises(reference.Failure):
                reference.compare(paths, self.root/'invalid.json')
            reference.write(paths[-1], value)
        with self.assertRaises(reference.Failure):
            reference.compare(paths[:3], self.root/'incomplete.json')


    def test_actual_native_verifier_binds_observation_and_rejects_tampered_state(self):
        self.out.mkdir(parents=True)
        report = smoke_fixtures.NativeSmokeTest().fixture(self.out)
        observation = reference.observe('miss', self.out/'cache-observation.json')
        report['native_smoke']['cache_observation'] = observation
        with mock.patch.object(tier.full_race, 'source_proof', return_value=PROOF), mock.patch.object(tier, 'native_host_context', return_value=copy.deepcopy(CANONICAL)):
            tier.verify_native_smoke(self.out, report, False)
            for change in (lambda r: r.update(state='trusted'), lambda r: r.update(source='c'*40),
                           lambda r: r.update(context_sha256='f'*64)):
                changed = copy.deepcopy(observation); change(changed)
                reference.write(self.out/'cache-observation.json', changed)
                report['native_smoke']['cache_observation'] = changed
                with self.assertRaises(tier.Failure):
                    tier.verify_native_smoke(self.out, report, False)

    def test_manual_workflow_never_joins_defaults_and_records_actual_pr_state(self):
        extended = jobs((ROOT/'.github/workflows/extended.yml').read_text())
        job = extended[reference.SUITE]
        self.assertIn("if: github.event_name == 'workflow_dispatch' && inputs.suite == 'native-smoke-reference'", job)
        self.assertIn('runs-on: macos-14', job); self.assertNotIn('matrix:', job)
        self.assertIn('retention-days: 7', job)
        self.assertIn('python3 -B scripts/ci_cache.py prepare', job)
        self.assertIn('python3 -B scripts/ci_cache.py verify', job)
        self.assertNotIn('actions/cache/save', job); self.assertNotIn('native-local', job)
        self.assertIn('ref: ${{ github.sha }}', job)
        found = jobs((ROOT/'.github/workflows/test.yml').read_text())
        observe = next(step for step in steps(found['macos-smoke']) if 'Record final verified native cache state' in step)
        self.assertIn('steps.native_cache.outputs.restore-state', observe)
        self.assertIn('native-tier-results/cache-observation.json', observe)
        self.assertIn('scripts/test_native_smoke_reference.py', found['script-static'])


if __name__ == '__main__':
    unittest.main()
