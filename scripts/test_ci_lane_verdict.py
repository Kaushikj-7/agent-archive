"""Stable check names require every independent lane, on PRs and candidates."""
import unittest

from ci_lane_verdict import LANES, verdict
from test_ci_workflow import ROOT, jobs, job_key, steps, step_key, condition_matches


class LaneVerdictTest(unittest.TestCase):
    def test_missing_extra_and_unsuccessful_lanes_fail_closed(self):
        for group, names in LANES.items():
            good = {name: {'result': 'success'} for name in names}
            verdict(group, good)
            for name in names:
                for state in ('failure', 'cancelled', 'skipped', None, 'unknown'):
                    bad = {**good, name: {'result': state}}
                    with self.subTest(group=group, name=name, state=state), self.assertRaises(ValueError):
                        verdict(group, bad)
                bad = dict(good); del bad[name]
                with self.assertRaises(ValueError):
                    verdict(group, bad)
            with self.assertRaises(ValueError):
                verdict(group, {**good, 'unexpected': {'result': 'success'}})

    def test_stable_aggregates_and_each_lane_are_unconditional(self):
        found = jobs((ROOT/'.github/workflows/test.yml').read_text())
        for group, names in LANES.items():
            aggregate = found[group]
            self.assertEqual(job_key(aggregate, 'if'), '${{ !cancelled() }}')
            self.assertIsNone(job_key(aggregate, 'name'))
            self.assertEqual(set(job_key(aggregate, 'needs').strip('[]').split(', ')), names)
            self.assertIn(f'scripts/ci_lane_verdict.py {group}', aggregate)
            self.assertNotIn('continue-on-error:', aggregate)
            for name in names:
                lane = found[name]
                self.assertIsNone(job_key(lane, 'if'))
                self.assertNotIn('continue-on-error:', lane)
                for step in steps(lane):
                    if 'run:' in step and 'Remove isolated' not in step:
                        self.assertIsNone(step_key(step, 'if'))

    def test_cancelable_gates_run_on_failures_and_reject_unsuccessful_needs(self):
        import run_go_shard
        found = jobs((ROOT/'.github/workflows/test.yml').read_text())
        expected = dict(LANES)
        expected['linux-race'] = {'linux-short', 'linux-full', 'script-static', 'script-install', 'script-purge', 'macos-smoke', 'source-audit'}
        for group, names in expected.items():
            predicate = job_key(found[group], 'if')
            for state in ('success', 'failure', 'skipped', 'cancelled'):
                # A canceled prerequisite is not equivalent to the workflow
                # itself being superseded. Failures still reach the verdict.
                self.assertTrue(condition_matches(predicate, 'pull_request', cancelled=False))
                self.assertFalse(condition_matches(predicate, 'pull_request', cancelled=True))
                needs = {name: {'result': 'success'} for name in names}
                for name in names:
                    bad = dict(needs, **{name: {'result': state}})
                    if state == 'success':
                        if group == 'linux-race':
                            run_go_shard.verdict('pull_request', 'refs/pull/1/merge', bad)
                        else:
                            verdict(group, bad)
                    else:
                        with self.subTest(group=group, state=state, lane=name), self.assertRaises((ValueError, run_go_shard.Failure)):
                            if group == 'linux-race':
                                run_go_shard.verdict('pull_request', 'refs/pull/1/merge', bad)
                            else:
                                verdict(group, bad)

    def test_consumers_plan_without_an_extra_eligibility_dependency(self):
        found = jobs((ROOT/'.github/workflows/test.yml').read_text())
        self.assertNotIn('tier-plan', found)
        self.assertNotIn('script-tests', found)
        for name in ('script-purge', 'macos-smoke', 'linux-full', 'script-install', 'script-static', *LANES['cross-build']):
            self.assertIsNone(job_key(found[name], 'needs'))

    def test_script_invocation_inventory_has_one_owner(self):
        expected = {
            'script-install': {'test_install.py', 'test_install_from_source.py'},
            'script-purge': {'test_purge_recipe.py'},
            'script-static': {
                'test_release_signing.py', 'test_release_assets.py', 'test_release_candidate.py',
                'test_ci_workflow.py', 'test_stage_native_verify.py', 'test_git_identity_versions.py', 'test_scope_native_verify.py',
                'test_stage_release_vulnerability_scan.py', 'test_measure_hook.py', 'test_muse_qualification.py',
                'test_linux_acceptance.py', 'test_go_shards.py', 'test_generate_go_shard_hints.py',
                'test_ci_tiers.py', 'test_local_ci_plan.py', 'test_historical_vet_contract.py', 'test_macos_smoke.py', 'test_native_smoke_reference.py', 'test_native_full.py', 'test_go_cache_context.py',
                'test_go_cache_steps.py', 'test_ci_cache.py', 'test_warm_shared_module_graph.py', 'test_ci_lane_verdict.py', 'test_report_ci_timing.py', 'test_purge_ci.py',
                'test_ci_worker_topology.py', 'test_collect_ci_timing.py', 'test_ci_timing_history.py', 'test_ci_performance_gate.py',
                'test_go_parent_shards.py', 'test_pr_reference.py',
                'test_benchmark_shared_verify.py'},
        }
        import re
        found = jobs((ROOT/'.github/workflows/test.yml').read_text())
        all_scripts = []
        for lane, scripts in expected.items():
            actual = re.findall(r'python3 (?:-B )?scripts/(test_\w+\.py)', found[lane])
            self.assertEqual(set(actual), scripts, lane)
            self.assertEqual(len(actual), len(set(actual)), lane)
            all_scripts.extend(actual)
            self.assertIn('HOME=%s/'+lane+'-home', found[lane])
            self.assertIn('XDG_CONFIG_HOME=%s/'+lane+'-xdg', found[lane])
            self.assertIn('if: always()', found[lane])
            # runner is only available after assignment, not in job-level env.
            prefix = found[lane].split('    steps:', 1)[0]
            self.assertNotIn('${{ runner.', prefix)
            cleanup = next(step for step in steps(found[lane]) if 'Remove isolated' in step)
            self.assertIn('"$RUNNER_TEMP/'+lane+'-home"', cleanup)
            self.assertNotIn('"$HOME"', cleanup)
        self.assertEqual(len(all_scripts), len(set(all_scripts)))

    def test_all_cross_target_commands_preserved(self):
        found = jobs((ROOT/'.github/workflows/test.yml').read_text())
        arm = found['cross-linux-arm64']; darwin = found['cross-darwin-nocgo']; release = found['cross-linux-release']
        for key in ('GOOS: linux', 'GOARCH: arm64', "CGO_ENABLED: '0'", 'go build ./...', 'go vet ./...'):
            self.assertIn(key, arm)
        for key in ('GOOS: darwin', 'GOARCH: arm64', "CGO_ENABLED: '0'", 'go vet ./...'):
            self.assertIn(key, darwin)
        for key in ('VERSION: v0.0.0-ci', './scripts/build-release.sh linux', './scripts/verify-linux-release.sh'):
            self.assertIn(key, release)


if __name__ == '__main__':
    unittest.main()
