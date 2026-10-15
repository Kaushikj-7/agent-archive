"""Real Git plan determinism and independent cross-platform selection proofs."""
import copy
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest
from unittest import mock

import run_ci_tier as tier


class LocalPlanTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name).resolve()
        self.source = self.root/'source'
        self.source.mkdir()
        self.git('init', '-q')
        self.git('config', 'user.name', 'CI plan fixture')
        self.git('config', 'user.email', 'ci@example.invalid')
        self.git('config', 'commit.gpgsign', 'false')
        (self.source/'scripts').mkdir()
        shutil.copyfile(tier.ROOT/'scripts/ci_tiers.json', self.source/'scripts/ci_tiers.json')
        (self.source/'internal/scheduler').mkdir(parents=True)
        (self.source/'internal/scheduler/fixture.go').write_text('package scheduler\n')
        self.git('add', '.')
        self.git('commit', '-qm', 'base')
        self.base = self.git('rev-parse', 'HEAD')
        branch = self.git('branch', '--show-current')
        self.git('checkout', '-qb', 'fixture-pr')
        (self.source/'internal/scheduler/fixture.go').write_text('package scheduler\n// changed\n')
        self.git('commit', '-qam', 'change')
        self.head = self.git('rev-parse', 'HEAD')
        self.git('checkout', branch)
        self.git('merge', '--no-ff', '-m', 'tested merge', self.head)
        self.tested = self.git('rev-parse', 'HEAD')
        self.event = self.root/'event.json'
        self.event.write_text(json.dumps({'pull_request': {'base': {'sha': self.base}, 'head': {'sha': self.head}}}))
        self.bin = self.root/'bin'
        self.bin.mkdir()
        forbidden = self.bin/'go'
        forbidden.write_text('#!/bin/sh\necho forbidden > '+str(self.root/'go-invoked')+'\nexit 99\n')
        forbidden.chmod(0o755)
        self.env = {'PATH': str(self.bin)+os.pathsep+os.environ['PATH'], 'GITHUB_SHA': self.tested, 'GITHUB_EVENT_NAME': 'pull_request',
                    'GITHUB_REF': 'refs/pull/1/merge', 'GITHUB_EVENT_PATH': str(self.event),
                    'CI_SCHEDULE': '', 'CI_SUITE': 'all'}

    def git(self, *args):
        return subprocess.check_output(['git', '-C', str(self.source), *args], text=True, stderr=subprocess.PIPE).strip()

    def plan(self, suffix, env=None, source=None, strict=True):
        output = self.root/(suffix+'.json')
        argv = ['run_ci_tier.py', '--plan', '--output', str(output)]
        if strict:
            argv.append('--require-pr-proof')
        with mock.patch.object(tier, 'ROOT', source or self.source), mock.patch.object(sys, 'argv', argv), mock.patch.dict(os.environ, dict(self.env, **(env or {}))):
            status = tier.main()
        return status, json.loads(output.read_text()) if output.exists() else None

    def test_actual_source_plan_is_identical_across_job_environment_and_cwd(self):
        plans = []
        previous = Path.cwd()
        try:
            for index, image in enumerate(('ubuntu24', 'macos14', 'purge0', 'purge1', 'script-gate', 'linux-gate')):
                directory = self.root/image
                directory.mkdir()
                os.chdir(directory)
                env = {'HOME': str(directory), 'TMPDIR': str(directory), 'RUNNER_TEMP': str(directory),
                       'ImageOS': image, 'GITHUB_JOB': image, 'GITHUB_OUTPUT': str(directory/'outputs')}
                status, plan = self.plan('plan'+str(index), env)
                self.assertEqual(status, 0)
                self.assertTrue(plan['systemd'])
                self.assertEqual(plan['reason'], 'verified base/head/tested merge')
                plans.append(plan)
        finally:
            os.chdir(previous)
        self.assertTrue(all(plan == plans[0] for plan in plans))
        self.assertEqual(self.git('status', '--porcelain'), '')
        self.assertFalse((self.root/'go-invoked').exists())

    def advanced_candidate(self):
        self.git('checkout', '-qb', 'newer-base', self.base)
        advance = self.source/'internal/config/base_advance.go'
        advance.parent.mkdir(parents=True)
        advance.write_text('package config\n')
        self.git('add', '.')
        self.git('commit', '-qm', 'base advanced after event')
        actual_base = self.git('rev-parse', 'HEAD')
        self.git('merge', '--no-ff', '-m', 'tested newer base', self.head)
        self.tested = self.git('rev-parse', 'HEAD')
        self.env['GITHUB_SHA'] = self.tested
        return actual_base

    def test_newer_descendant_base_is_accepted_with_distinct_recorded_identity(self):
        actual_base = self.advanced_candidate()
        self.assertEqual(self.git('show', '-s', '--format=%P', self.tested).split(), [actual_base, self.head])
        status, plan = self.plan('advanced-base')
        self.assertEqual(status, 0)
        self.assertEqual(plan['base'], self.base)
        self.assertEqual(plan['tested_base'], actual_base)
        self.assertNotEqual(plan['base'], plan['tested_base'])
        self.assertIn('internal/config/base_advance.go', plan['paths'])
        self.assertTrue(plan['systemd'])
        self.assertTrue(plan['native'])
        canonical = self.root/'forged-actual-base.json'
        canonical.write_text(json.dumps(dict(plan, tested_base=self.base)))
        with mock.patch.object(tier, 'ROOT', self.source), mock.patch.dict(os.environ, self.env):
            with self.assertRaisesRegex(tier.Failure, 'plans differ'):
                tier.verify_plan(canonical, plan)

    def test_reversed_actual_parents_and_unrelated_first_parent_are_rejected(self):
        actual_base = self.advanced_candidate()
        self.git('checkout', '-qb', 'reversed-merge', self.head)
        self.git('merge', '--no-ff', '-m', 'reversed parents', actual_base)
        self.env['GITHUB_SHA'] = self.git('rev-parse', 'HEAD')
        self.assertEqual(self.git('show', '-s', '--format=%P', 'HEAD').split(), [self.head, actual_base])
        status, plan = self.plan('reversed')
        self.assertEqual(status, 1)
        self.assertIsNone(plan)
        self.git('checkout', '--orphan', 'unrelated-base')
        self.git('rm', '-rf', '.')
        (self.source/'foreign.txt').write_text('independent history\n')
        self.git('add', '.')
        self.git('commit', '-qm', 'unrelated first parent')
        foreign = self.git('rev-parse', 'HEAD')
        self.git('merge', '--allow-unrelated-histories', '--no-ff', '-m', 'foreign tested merge', self.head)
        self.env['GITHUB_SHA'] = self.git('rev-parse', 'HEAD')
        self.assertEqual(self.git('show', '-s', '--format=%P', 'HEAD').split(), [foreign, self.head])
        status, plan = self.plan('foreign-parent')
        self.assertEqual(status, 1)
        self.assertIsNone(plan)

    def test_changed_event_base_and_real_shallow_history_fail_before_coverage(self):
        event = json.loads(self.event.read_text())
        event['pull_request']['base']['sha'] = self.head
        bad = self.root/'bad-event.json'
        bad.write_text(json.dumps(event))
        status, plan = self.plan('changed-base', {'GITHUB_EVENT_PATH': str(bad)})
        self.assertEqual(status, 1)
        self.assertIsNone(plan)
        shallow = self.root/'shallow'
        subprocess.run(['git', 'clone', '-q', '--depth=1', self.source.as_uri(), str(shallow)], check=True)
        self.assertEqual(subprocess.check_output(['git', '-C', str(shallow), 'rev-parse', '--is-shallow-repository'], text=True).strip(), 'true')
        status, plan = self.plan('shallow', source=shallow)
        self.assertEqual(status, 1)
        self.assertIsNone(plan)
        # Other legacy planner users keep the existing explicit fallback policy.
        status, plan = self.plan('legacy-fallback', source=shallow, strict=False)
        self.assertEqual(status, 0)
        self.assertEqual(plan['reason'], 'representative fallback: source proof unavailable')

    def test_independent_plan_rejects_changed_extras_and_duplicate_fields(self):
        status, expected = self.plan('expected')
        self.assertEqual(status, 0)
        canonical = self.root/'canonical.json'
        canonical.write_text(json.dumps(expected))
        with mock.patch.object(tier, 'ROOT', self.source), mock.patch.dict(os.environ, self.env):
            tier.verify_plan(canonical, expected)
            changed = dict(expected, systemd=False)
            canonical.write_text(json.dumps(changed))
            with self.assertRaisesRegex(tier.Failure, 'plans differ'):
                tier.verify_plan(canonical, expected)
            canonical.write_text('{"source":"old","source":"'+self.tested+'"}')
            with self.assertRaisesRegex(tier.Failure, 'duplicate'):
                tier.verify_plan(canonical, expected)

    def test_native_selection_is_bound_without_using_aggregate_compiler(self):
        status, plan = self.plan('native-expected')
        self.assertEqual(status, 0)
        proof = tier.full_race.source_proof(self.source)
        report = {'schema': 1, 'success': True, 'source': self.tested, 'source_proof': proof,
                  'tier': 'pr', 'suite': 'macos', 'event': 'pull_request', 'ref': self.env['GITHUB_REF'],
                  'extras': plan, 'native_smoke': {'sensitive': plan['native'], 'source_proof': proof, 'post_source_proof': proof}}
        directory = self.root/'native'
        directory.mkdir()
        path = directory/'tier.json'
        with mock.patch.object(tier, 'ROOT', self.source), mock.patch.dict(os.environ, self.env):
            path.write_text(json.dumps(report))
            tier.verify_native_plan(directory, plan)
            for mutate in (lambda r: r['extras'].update(native=False),
                           lambda r: r['native_smoke'].update(sensitive=False),
                           lambda r: r.update(source='a'*40),
                           lambda r: r.update(success=False)):
                changed = copy.deepcopy(report)
                mutate(changed)
                path.write_text(json.dumps(changed))
                with self.assertRaisesRegex(tier.Failure, 'native coverage selection'):
                    tier.verify_native_plan(directory, plan)


if __name__ == '__main__':
    unittest.main()
