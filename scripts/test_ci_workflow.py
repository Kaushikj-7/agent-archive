"""Extended CI preserves the real-systemd job and its validated runner image.

The job runs nightly or on demand. Its stable name identifies the systemd
backstop, and its pinned image prevents an unreviewed systemd upgrade.
Read without PyYAML, like test_release_assets.py.
"""
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
    return re.sub(r'\s+#.*$', '', match.group(1)).strip().strip('\'"')


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


class PublishedWriterJobTest(unittest.TestCase):
    def test_published_writer_runs_only_on_native_macos(self):
        found = jobs(EXTENDED_YML.read_text())
        self.assertIn('bash scripts/test_published_writer.sh', found['macos-full'])
        self.assertRegex(job_key(found['macos-full'], 'runs-on'), r'^macos-')
        for name, text in found.items():
            if name != 'macos-full':
                self.assertNotIn('test_published_writer.sh', text)


class CandidateCampaignTest(unittest.TestCase):
    def test_candidate_validation_never_restores_shared_go_build_state(self):
        # Candidate evidence must come from fresh source/module verification,
        # not executable build state restored from another workflow's cache.
        expected = {'test': 5, 'levenshtein': 1, 'extended': 4}
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
        self.assertEqual(set(jobs(workflow)), {'linux-race', 'git-identity-compatibility', 'macos-smoke', 'cross-build', 'lint'})
        for name, text in jobs(workflow).items():
            self.assertIsNone(job_key(text, 'if'), name)
        self.assertIn('go test -race -timeout 20m ./...', workflow)
        self.assertIn('github.event.pull_request.number || github.ref', workflow)

    def test_extended_conditions_preserve_main_nightly_dispatch_and_add_candidate_campaign(self):
        workflow = EXTENDED_YML.read_text()
        conditions = {name: job_key(text, 'if') for name, text in jobs(workflow).items()}
        expected = {
            'linux-race': "(github.event_name == 'push' && github.ref == 'refs/heads/main') || (github.event_name == 'workflow_dispatch' && (inputs.suite == 'all' || inputs.suite == 'linux'))",
            'macos-full': "github.event_name == 'push' || github.event_name == 'schedule' || (github.event_name == 'workflow_dispatch' && (inputs.suite == 'all' || inputs.suite == 'macos'))",
        }
        for name, suite in (('fuzz', 'fuzz'), ('real-systemd', 'systemd')):
            expected[name] = "(github.event_name == 'push' && startsWith(github.ref, 'refs/heads/release-candidate/')) || github.event_name == 'schedule' || (github.event_name == 'workflow_dispatch' && (inputs.suite == 'all' || inputs.suite == '" + suite + "'))"
        self.assertEqual(conditions, expected)
        self.assertIn("github.event_name == 'push' && github.ref || github.run_id", workflow)
        self.assertIn('go test -race -timeout 20m ./...', jobs(workflow)['macos-full'])
        self.assertIn('-fuzztime 30s -fuzzminimizetime 2s', jobs(workflow)['fuzz'])
        self.assertIn('test "$found" -ge 24', jobs(workflow)['fuzz'])
        self.assertIn('AGENT_ARCHIVE_REAL_SYSTEMD:', jobs(workflow)['real-systemd'])


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
    def test_all_required_checks_use_the_pinned_shared_container(self):
        config = json.loads((ROOT / 'levenshtein.json').read_text())
        expected = {'lint', 'vet', 'http', 'sql', 'modules', 'vulnerabilities',
                    'workflow-lint', 'workflow-security'}
        self.assertEqual(set(config['checks']), expected)
        for name in ('pre-merge', 'main'):
            self.assertEqual(set(config['runs'][name]['checks']), expected)
        self.assertEqual(config['checks']['vulnerabilities']['kind'], 'go-vuln')
        for name, check in config['checks'].items():
            self.assertEqual(config['environments'][check['environment']],
                             {'executor': 'dagger'}, name)
        workflow = (ROOT / '.github/workflows/levenshtein.yml').read_text()
        shared_checkout = [step for step in steps(jobs(workflow)['verify'])
                           if 'repository: wangjohn/levenshtein' in step]
        self.assertEqual(len(shared_checkout), 1)
        self.assertRegex(shared_checkout[0], r'(?m)^          ref: [0-9a-f]{40}$')
        self.assertIn('./levenshtein/verify "$run" --source ./app', workflow)


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
