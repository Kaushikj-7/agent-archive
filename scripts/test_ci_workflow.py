"""Extended CI preserves the real-systemd job and its validated runner image.

The job runs nightly or on demand. Its stable name identifies the systemd
backstop, and its pinned image prevents an unreviewed systemd upgrade.
Read without PyYAML, like test_release_assets.py.
"""
import ast
from pathlib import Path
import json
import re
import unittest

from test_release_assets import steps

ROOT = Path(__file__).resolve().parent.parent
EXTENDED_YML = ROOT / '.github' / 'workflows' / 'extended.yml'
TESTING_MD = ROOT / 'dev' / 'contributing' / 'testing.md'

JOB = 'real-systemd'
PINNED_UBUNTU = re.compile(r'ubuntu-\d\d\.\d\d')


def jobs(workflow):
    """Each top-level job's id mapped to its text, in the order they appear."""
    start = re.search(r'(?m)^jobs:\n', workflow)
    if not start:
        raise AssertionError('no jobs: in the workflow')
    body = workflow[start.end():]
    end = re.search(r'(?m)^[^\s#]', body)  # the next top-level key; comments are not one
    if end:
        body = body[:end.start()]
    found = {}
    for match in re.finditer(r'(?ms)^  ([\w-]+):[ \t]*(?:#[^\n]*)?\n(.*?)(?=^  [\w-]+:|\Z)', body):
        found[match.group(1)] = match.group(2)
    return found


def job_key(text, key):
    """The value of one of the job's own keys (four spaces in), or None."""
    match = re.search(rf'(?m)^    {re.escape(key)}:(.*)$', text)
    if not match:
        return None
    value = re.sub(r'\s+#.*$', '', match.group(1)).strip()
    if len(value) >= 2 and value[0] == value[-1] and value[0] in ("'", '"'):
        value = value[1:-1]
    return value


def condition_matches(expression, event, ref='', suite=''):
    """Evaluate only the small boolean condition grammar used by these jobs."""
    values = {'github.event_name': event, 'github.ref': ref, 'inputs.suite': suite}
    for key, value in values.items():
        expression = expression.replace(key, repr(value))
    tree = ast.parse(expression.replace('&&', ' and ').replace('||', ' or '), mode='eval')

    def value(node):
        if isinstance(node, ast.Constant) and isinstance(node.value, str):
            return node.value
        if isinstance(node, ast.BoolOp):
            items = [value(item) for item in node.values]
            if isinstance(node.op, ast.And):
                return all(items)
            if isinstance(node.op, ast.Or):
                return any(items)
        if isinstance(node, ast.Compare) and len(node.ops) == 1 and isinstance(node.ops[0], ast.Eq):
            return value(node.left) == value(node.comparators[0])
        if (isinstance(node, ast.Call) and isinstance(node.func, ast.Name)
                and node.func.id == 'startsWith' and len(node.args) == 2 and not node.keywords):
            return value(node.args[0]).startswith(value(node.args[1]))
        raise AssertionError(f'unsupported CI condition: {expression}')

    return bool(value(tree.body))


def step_key(text, key):
    match = re.search(rf'(?m)^        {re.escape(key)}:(.*)$', text)
    return match.group(1).strip() if match else None


class RealSystemdJobTest(unittest.TestCase):
    def setUp(self):
        self.jobs = jobs(EXTENDED_YML.read_text())

    def job(self):
        self.assertIn(JOB, self.jobs, f'extended.yml has no job named {JOB}; nightly systemd coverage must remain')
        return self.jobs[JOB]

    def test_the_job_is_named_exactly_real_systemd(self):
        text = self.job()
        # Keep a stable check name for the systemd backstop.
        self.assertIsNone(job_key(text, 'name'), f'{JOB} must not set name:, which renames the check')
        self.assertIsNone(job_key(text, 'strategy'), f'{JOB} must not have a matrix, which renames the check')

    def test_runs_on_a_pinned_ubuntu_image(self):
        runs_on = job_key(self.job(), 'runs-on')
        self.assertIsNotNone(runs_on, f'{JOB} has no runs-on')
        self.assertRegex(
            runs_on,
            rf'^{PINNED_UBUNTU.pattern}$',
            f'{JOB} must run on a pinned image such as ubuntu-24.04, not {runs_on!r}; bump it deliberately',
        )

    def test_the_testing_docs_name_the_same_image(self):
        runs_on = job_key(self.job(), 'runs-on')
        self.assertIn(f'`{runs_on}`', TESTING_MD.read_text(), f'dev/contributing/testing.md should name {runs_on}')

    def test_pr_real_manager_uses_the_validated_backstop_image(self):
        pr = jobs((ROOT / '.github/workflows/test.yml').read_text())['linux-full']
        self.assertIn('bash scripts/start_ci_systemd.sh', pr)
        self.assertEqual(job_key(pr, 'runs-on'), job_key(self.job(), 'runs-on'))


class PurgePrerequisiteTest(unittest.TestCase):
    def test_every_purge_job_provisions_tools_before_running_bodies(self):
        workflows = [(ROOT / '.github/workflows/test.yml', 'script-tests'),
                     (EXTENDED_YML, 'linux-race'), (EXTENDED_YML, 'macos-full')]
        for path, name in workflows:
            with self.subTest(workflow=path.name, job=name):
                job = jobs(path.read_text())[name]
                job_steps = steps(job)
                purge_index = next(i for i, step in enumerate(job_steps)
                                   if 'python3 scripts/test_purge_recipe.py ' in step)
                install_index = next(i for i, step in enumerate(job_steps)
                                     if 'name: Install purge prerequisites' in step)
                self.assertLess(install_index, purge_index)
                install = job_steps[install_index]
                self.assertIsNone(step_key(install, 'if'))
                if job_key(job, 'runs-on').startswith('ubuntu-'):
                    self.assertIn('sudo apt-get update', install)
                    self.assertIn('sudo apt-get install --yes jq zsh', install)
                else:
                    self.assertIn('for tool in jq zsh;', install)
                    self.assertIn('brew install "$tool"', install)


class PublishedWriterJobTest(unittest.TestCase):
    def test_published_writer_runs_only_on_native_macos(self):
        found = jobs(EXTENDED_YML.read_text())
        self.assertIn('--suite macos', found['macos-full'])
        self.assertIn("execute(['bash','scripts/test_published_writer.sh']", (ROOT / 'scripts/run_ci_tier.py').read_text())
        self.assertRegex(job_key(found['macos-full'], 'runs-on'), r'^macos-')
        for name, text in found.items():
            if name != 'macos-full':
                self.assertNotIn('test_published_writer.sh', text)


class CandidateCampaignTest(unittest.TestCase):
    def test_candidate_validation_never_restores_shared_go_build_state(self):
        # Candidate evidence must come from fresh source/module verification,
        # not executable build state restored from another workflow's cache.
        expected = {'test': 6, 'levenshtein': 2, 'extended': 4}
        for name, count in expected.items():
            workflow = (ROOT / '.github/workflows' / f'{name}.yml').read_text()
            setup = [step for job in jobs(workflow).values() for step in steps(job)
                     if re.search(r'uses: actions/setup-go@', step)]
            self.assertEqual(len(setup), count, name)
            for step in setup:
                with self.subTest(workflow=name, step=step):
                    self.assertRegex(step, r'(?m)^          cache: false(?:\s+#.*)?$')
                    self.assertNotIn('cache-dependency-path:', step)

    def test_candidate_push_triggers_test_verify_and_extended_without_signing(self):
        for name in ('test', 'levenshtein', 'extended'):
            workflow = (ROOT / '.github/workflows' / f'{name}.yml').read_text()
            self.assertRegex(workflow, r"(?m)^  push:\n    branches: \[.*'release-candidate/\*\*'.*\]$")
            self.assertNotIn('environment: release', workflow)
            self.assertNotIn('secrets.APPLE_', workflow)
        release = (ROOT / '.github/workflows/release.yml').read_text()
        self.assertNotIn('branches:', release)
        self.assertNotIn('workflow_dispatch:', release)
        self.assertIn("      - 'v*.*.*'", release)

    def test_candidate_test_keeps_all_pr_gates(self):
        workflow = (ROOT / '.github/workflows/test.yml').read_text()
        self.assertIn('  pull_request:', workflow)
        self.assertEqual(set(jobs(workflow)), {'linux-race', 'linux-short', 'linux-full', 'script-tests', 'git-identity-compatibility', 'macos-smoke', 'cross-build', 'lint', 'tier-plan'})
        for name, text in jobs(workflow).items():
            if name not in ('linux-race', 'linux-short', 'linux-full'):
                self.assertIsNone(job_key(text, 'if'), name)
        self.assertIn('--tier full --suite linux', workflow)
        self.assertIn('github.event.pull_request.number || github.ref', workflow)

    def test_extended_conditions_preserve_nightly_dispatch_and_candidate_campaign(self):
        workflow = EXTENDED_YML.read_text()
        conditions = {name: job_key(text, 'if') for name, text in jobs(workflow).items()}
        expected = {
            'linux-race': "github.event_name == 'schedule' || (github.event_name == 'workflow_dispatch' && (inputs.suite == 'all' || inputs.suite == 'linux'))",
            'macos-full': "(github.event_name == 'push' && startsWith(github.ref, 'refs/heads/release-candidate/')) || github.event_name == 'schedule' || (github.event_name == 'workflow_dispatch' && (inputs.suite == 'all' || inputs.suite == 'macos'))",
        }
        for name, suite in (('fuzz', 'fuzz'), ('real-systemd', 'systemd')):
            expected[name] = "(github.event_name == 'push' && startsWith(github.ref, 'refs/heads/release-candidate/')) || github.event_name == 'schedule' || (github.event_name == 'workflow_dispatch' && (inputs.suite == 'all' || inputs.suite == '" + suite + "'))"
        self.assertEqual(conditions, expected)
        self.assertIn("github.event_name == 'push' && github.ref || github.run_id", workflow)
        self.assertIn('--suite macos', jobs(workflow)['macos-full'])
        self.assertIn('--suite fuzz', jobs(workflow)['fuzz'])
        runner = (ROOT / 'scripts/run_ci_tier.py').read_text()
        self.assertIn("duration = '5s' if tier == 'daily' else '30s'", runner)
        self.assertIn('len(targets) < 24', runner)
        self.assertIn('AGENT_ARCHIVE_REAL_SYSTEMD:', jobs(workflow)['real-systemd'])


class CadenceTest(unittest.TestCase):
    def test_pr_scope_keeps_required_verify_and_full_non_pr_path(self):
        workflow = (ROOT / '.github/workflows/levenshtein.yml').read_text()
        verify = jobs(workflow)['verify']
        self.assertIsNone(job_key(verify, 'if'))
        self.assertNotIn('paths:', workflow)
        self.assertNotIn('paths-ignore:', workflow)
        checkout = next(step for step in steps(verify) if 'path: app' in step)
        self.assertIn("fetch-depth: ${{ github.event_name != 'pull_request' && 1 || 0 }}", checkout)
        staging = next(step for step in steps(verify) if 'id: stage' in step)
        self.assertEqual(step_key(staging, 'if'), "github.event_name == 'pull_request'")
        self.assertIn("PYTHONDONTWRITEBYTECODE: '1'", staging)
        self.assertIn('python3 -B ', staging)
        self.assertIn('--run pre-merge --pr-scope', staging)
        execute = next(step for step in steps(verify) if 'name: Verify\n' in step)
        self.assertIsNone(step_key(execute, 'if'))
        self.assertIn('run=pre-merge', execute)
        self.assertRegex(execute, r'if \[\[ "\$EVENT" == pull_request \]\]; then[\s\S]*?run="\$PR_RUN"\n          fi')
        self.assertIn('./levenshtein/verify "$run" --source "$source" --jobs 4', execute)
        self.assertNotIn('|| true', execute)

    def test_scoping_helper_suite_is_a_mandatory_pr_gate(self):
        linux = jobs((ROOT / '.github/workflows/test.yml').read_text())['script-tests']
        step = next(step for step in steps(linux) if 'python3 -B scripts/test_scope_native_verify.py' in step)
        self.assertIsNone(step_key(step, 'if'))

    def test_pr_and_candidate_linux_steps_select_the_correct_fixture_sizes(self):
        found = jobs((ROOT / '.github/workflows/test.yml').read_text())
        self.assertEqual(condition_matches(job_key(found['linux-short'],'if'),'pull_request'),True)
        self.assertIsNone(job_key(found['linux-full'],'if'))
        self.assertIn('--suite extra --extra-plan', found['linux-full'])
        self.assertIn('--tier full --suite linux', found['linux-full'])
        self.assertIn('shard: [0, 1]', found['linux-short'])
        self.assertIn('fail-fast: false', found['linux-short'])
        self.assertIn('python3 scripts/run_go_shard.py --shard',found['linux-short'])
        self.assertEqual(job_key(found['linux-race'],'if'),'always()')
        self.assertEqual(job_key(found['linux-race'],'needs'),'[linux-short, linux-full, script-tests, macos-smoke, tier-plan]')
        self.assertIn('--verdict', found['linux-race'])
        self.assertIn('--reconcile', found['linux-race'])
        self.assertIn('--tier "$tier" --evidence', found['script-tests'])
        self.assertIn('fetch-depth: 0', found['tier-plan'])
        for script in ('test_release_candidate.py','test_ci_workflow.py','test_stage_native_verify.py','test_scope_native_verify.py','test_go_shards.py','test_ci_tiers.py'):
            step=next(step for step in steps(found['script-tests']) if script in step)
            self.assertIsNone(step_key(step,'if'))

    def test_extended_event_matrix_and_full_size_backstops(self):
        workflow = EXTENDED_YML.read_text()
        self.assertIn("    branches: ['release-candidate/**']", workflow)
        self.assertIn('  schedule:', workflow)
        self.assertIn('options: [all, macos, linux, fuzz, systemd]', workflow)
        found = jobs(workflow)
        all_jobs = set(found)
        for event, ref, suite, expected in (
                ('pull_request', 'refs/pull/1/merge', '', set()),
                ('push', 'refs/heads/main', '', set()),
                ('push', 'refs/heads/feature', '', set()),
                ('push', 'refs/heads/release-candidate/final', '', all_jobs - {'linux-race'}),
                ('schedule', 'refs/heads/main', '', all_jobs),
                ('workflow_dispatch', 'refs/heads/main', 'all', all_jobs),
                ('workflow_dispatch', 'refs/heads/main', 'linux', {'linux-race'}),
                ('workflow_dispatch', 'refs/heads/main', 'macos', {'macos-full'}),
                ('workflow_dispatch', 'refs/heads/main', 'fuzz', {'fuzz'}),
                ('workflow_dispatch', 'refs/heads/main', 'systemd', {'real-systemd'}),
                ('workflow_dispatch', 'refs/heads/main', 'unknown', set())):
            with self.subTest(event=event, ref=ref, suite=suite):
                actual = {name for name, text in found.items()
                          if condition_matches(job_key(text, 'if'), event, ref, suite)}
                self.assertEqual(actual, expected)
        for name,suite in (('linux-race','linux'),('macos-full','macos')):
            self.assertIn('--suite '+suite+' --output tier-results',found[name])
            self.assertIn('if-no-files-found: error',found[name])
        runner=(ROOT/'scripts/run_ci_tier.py').read_text()
        self.assertIn("['-short'] if tier == 'daily' else []",runner)
        self.assertIn("AGENT_ARCHIVE_PERF='1'",runner)
        self.assertIn("'-count=1'",runner)

    def test_native_smoke_retains_build_and_runs_short_race_tests(self):
        smoke = (ROOT / 'scripts/test_macos_smoke.sh').read_text()
        self.assertIn('go build ./...', smoke)
        self.assertEqual(len(re.findall(r'go test -race -short ', smoke)), 3)
        self.assertNotRegex(smoke, r'go test -race (?!-short)')
        self.assertIn('macOS smoke test missing:', smoke)
        self.assertIn('CGO_ENABLED', smoke)

    def test_daily_vulnerability_audit_remains_fresh(self):
        workflow = (ROOT / '.github/workflows/levenshtein.yml').read_text()
        self.assertIn('  schedule:', workflow)
        self.assertRegex(workflow, r'if \[\[ "\$EVENT" == schedule \]\]; then\n            run=main')
        config = (ROOT / 'levenshtein.json').read_text()
        self.assertIn('"rerun_checks": true', config)
        self.assertIn('"vulnerabilities"', config)


class GitIdentityCompatibilityTest(unittest.TestCase):
    def test_compatibility_cases_keep_pinned_upstream_releases(self):
        workflow = (ROOT / '.github/workflows/test.yml').read_text()
        text = jobs(workflow)['git-identity-compatibility']
        self.assertEqual(job_key(text, 'runs-on'), 'ubuntu-24.04')
        for version, commit in {
            '2.30.2': '94f6e3e283f2adfc518b39cfc39291f1c2832ad0',
            '2.39.2': 'cbf04937d5b9fcf0a76c28f69e6294e9e3ecd7e6',
        }.items():
            self.assertRegex(text, rf'git: upstream-{version}\n            tag: v{version}\n            ref: {commit}\n            expected: git version {version}')
        self.assertIn('git: runner-current', text)
        self.assertIn('repository: git/git', text)
        self.assertIn('test "$(git --version)" = "$EXPECTED_GIT_VERSION"', text)
        self.assertIn('HOME: ${{ runner.temp }}/git-identity-home', text)
        self.assertIn('XDG_CONFIG_HOME: ${{ runner.temp }}/git-identity-xdg', text)
        self.assertIn('./internal/gitremote ./internal/sourcefacts', text)
        self.assertIn("-run 'RecoveredImport|DeletedWorktree' ./internal/backfill", text)
        self.assertIn('Older distribution package acceptance is recorded separately.', TESTING_MD.read_text())


class LevenshteinSharedChecksTest(unittest.TestCase):
    def test_required_checks_keep_full_selection_and_pinned_shared_verifier(self):
        config = json.loads((ROOT / 'levenshtein.json').read_text())
        expected = {'lint', 'vet', 'modules', 'vulnerabilities',
                    'workflow-lint', 'workflow-security'}
        self.assertEqual(set(config['checks']), expected)
        for name in ('pre-merge', 'main'):
            self.assertEqual(set(config['runs'][name]['checks']), expected)
        # The pinned full go-lint selection includes bodyclose and sqlclosecheck.
        # A per-check selection override could silently drop those analyses now
        # that they no longer run as separate go-http and go-sql checks.
        self.assertEqual(config['checks']['lint'],
                         {'kind': 'go-lint', 'target': 'archive', 'environment': 'go'})
        self.assertEqual(config['checks']['vulnerabilities']['kind'], 'go-vuln')
        for name, check in config['checks'].items():
            self.assertEqual(config['environments'][check['environment']],
                             {'executor': 'dagger'}, name)
        workflow = (ROOT / '.github/workflows/levenshtein.yml').read_text()
        shared_checkout = [step for step in steps(jobs(workflow)['verify'])
                           if 'repository: wangjohn/levenshtein' in step]
        self.assertEqual(len(shared_checkout), 1)
        self.assertRegex(shared_checkout[0], r'(?m)^          ref: [0-9a-f]{40}$')
        self.assertIn('./levenshtein/verify "$run" --source "$source" --jobs 4', workflow)
        # PR verification uses the declared-input staging path; non-PR runs
        # retain the complete source and configured container execution.
        self.assertIn('source=./app', workflow)
        self.assertIn('source="$RUNNER_TEMP/native-source"', workflow)
        self.assertIn('--run pre-merge --pr-scope', workflow)


class JobsParserTest(unittest.TestCase):
    def test_finds_each_job_and_its_own_keys(self):
        workflow = (
            'name: Test\n'
            'jobs:\n'
            '  a:\n'
            '    runs-on: ubuntu-latest\n'
            '  real-systemd:  # comment\n'
            '    # runs-on: ubuntu-latest\n'
            '    runs-on: "ubuntu-24.04"  # pinned\n'
            '    steps:\n'
            '      - name: step\n'
            'other: x\n'
        )
        found = jobs(workflow)
        self.assertEqual(list(found), ['a', 'real-systemd'])
        self.assertEqual(job_key(found['real-systemd'], 'runs-on'), 'ubuntu-24.04')
        self.assertIsNone(job_key(found['real-systemd'], 'name'))


if __name__ == '__main__':
    unittest.main()
