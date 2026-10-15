"""install.sh must install only a checksum-verified binary (and, on macOS, a signature-verified one)."""
import hashlib
import os
from pathlib import Path
import re
import shutil
import signal
import stat
import subprocess
import sys
import tempfile
import unittest

INSTALL_SH = Path(__file__).resolve().parent.parent / 'install.sh'
INSTALL_GUIDE = INSTALL_SH.parent / 'docs' / 'getting-started' / 'install.md'
TEAM = 'SYNTH12345'


def with_team(script, team):
    """script with its team_id line naming team, whatever it named before."""
    return re.sub(r'^team_id="[A-Z0-9]*"$', f'team_id="{team}"', script, count=1, flags=re.M)
DEVELOPER_ID_REQUIREMENT = (
    'anchor apple generic and certificate 1[field.1.2.840.113635.100.6.2.6] exists '
    'and certificate leaf[field.1.2.840.113635.100.6.1.13] exists '
    f'and certificate leaf[subject.OU] = "{TEAM}"'
)

# A codesign stand-in. The fake binaries are "signed" by $FAKE_SIGNING_TEAM
# (empty: unsigned). It records its arguments and passes only a strict
# verification against exactly the Developer ID requirement for that team.
CODESIGN_SHIM = """#!/bin/sh
printf '%s\\n' "$@" >> "$CODESIGN_LOG"
[ "$1" = --verify ] && [ "$2" = --strict ] || exit 1
[ -n "$FAKE_SIGNING_TEAM" ] || exit 1
want="-R=anchor apple generic and certificate 1[field.1.2.840.113635.100.6.2.6] exists and certificate leaf[field.1.2.840.113635.100.6.1.13] exists and certificate leaf[subject.OU] = \\"$FAKE_SIGNING_TEAM\\""
[ "$3" = "$want" ] || exit 3
[ -f "$4" ] || exit 1
"""


def write_executable(path, body):
    path.write_text(body)
    path.chmod(path.stat().st_mode | stat.S_IXUSR | stat.S_IXGRP | stat.S_IXOTH)


# Prints "<sha256>  <file>", the format of sha256sum and shasum, so the
# checksum shims below do not depend on which tools this host has.
HASH_HELPER = """import hashlib, sys
for path in sys.argv[1:]:
    print(hashlib.sha256(open(path, 'rb').read()).hexdigest() + '  ' + path)
"""

# How a stand-in checksum tool behaves. Every one logs its own name.
HASH_BEHAVIOR = {
    'ok': 'exec "$PYTHON" "$HASH_HELPER" "$@"',
    'fail': 'exit 1',  # no output, failing status
    'silent': 'exit 0',  # no output, success status
    'garbage': 'printf "deadbeef  %s\\n" "$1"',  # short digest, success status
    'empty_digest': 'printf "  %s\\n" "$1"',  # empty digest, success status
}

# The tools install.sh itself needs besides uname, sysctl, codesign and the
# checksum tools. Hermetic runs put only these (as symlinks) on PATH.
BASE_TOOLS = ('curl', 'awk', 'mktemp', 'rm', 'mkdir', 'cp', 'chmod', 'mv', 'dirname')


class InstallScriptBase(unittest.TestCase):
    """A fake release, home directory, and shims to run a copy of install.sh in."""

    def setUp(self):
        self.root = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, self.root, ignore_errors=True)
        self.release = self.root / 'release'
        self.release.mkdir()
        for arch in ('arm64', 'amd64'):
            write_executable(self.release / f'agent-archive-darwin-{arch}',
                             f'#!/bin/sh\necho v9.9.9-{arch}\n')
            write_executable(self.release / f'agent-archive-linux-{arch}',
                             f'#!/bin/sh\necho v9.9.9-linux-{arch}\n')
        self.write_sums()
        self.home = self.root / 'home'
        self.home.mkdir()
        # The shipped script names the real team; the tests run a copy that
        # names the synthetic one.
        self.install_sh = self.root / 'install.sh'
        self.install_sh.write_text(with_team(INSTALL_SH.read_text(), TEAM))
        self.codesign_log = self.root / 'codesign.log'
        self.checksum_log = self.root / 'checksum.log'
        self.sysctl_log = self.root / 'sysctl.log'
        (self.root / 'hash.py').write_text(HASH_HELPER)
        self.runs = 0
        self.default_target = self.home / '.local' / 'bin' / 'agent-archive'

    def write_sums(self, override=None):
        """SHA256SUMS for the four release binaries. override maps a name to
        a digest to write instead, or to '' to leave the name out."""
        lines = []
        for os_name in ('darwin', 'linux'):
            for arch in ('amd64', 'arm64'):
                name = f'agent-archive-{os_name}-{arch}'
                digest = hashlib.sha256((self.release / name).read_bytes()).hexdigest()
                if override and override.get(name) is not None:
                    digest = override[name]
                if override and override.get(name) == '':
                    continue
                lines.append(f'{digest}  {name}\n')
        (self.release / 'SHA256SUMS').write_text(''.join(lines))

    def run_install(self, system='Darwin', machine='arm64', rosetta='0', extra_env=None, path_dirs=(),
                    default_dir=False, script=None, signing_team=TEAM,
                    hermetic=False, codesign=True, hash_tools=None, overrides=None, start=False,
                    cwd=None, umask=None):
        """Run install.sh under shims for uname, sysctl and codesign.

        By default PATH ends in the host's /usr/bin:/bin. hermetic=True puts
        only the shims and symlinks to the few tools install.sh needs on PATH,
        so the presence of codesign and of each checksum tool is under the
        test's control: codesign=False leaves it out, and hash_tools maps
        'sha256sum' and/or 'shasum' to a HASH_BEHAVIOR key (default: both,
        'ok'). Passing hash_tools implies hermetic. overrides maps a tool
        name to the body of a shell shim that replaces it. start=True returns
        the running Popen instead of waiting for the result. cwd and umask
        set the script's working directory and umask."""
        self.runs += 1
        hermetic = hermetic or hash_tools is not None or not codesign
        shims = self.root / f'shims-{self.runs}'
        shims.mkdir()
        write_executable(shims / 'uname',
                         f'#!/bin/sh\ncase "$1" in -s) echo {system} ;; -m) echo {machine} ;; esac\n')
        write_executable(shims / 'sysctl', f'#!/bin/sh\necho sysctl >> "$SYSCTL_LOG"\necho {rosetta}\n')
        if codesign:
            write_executable(shims / 'codesign', CODESIGN_SHIM)
        if hermetic:
            for tool in BASE_TOOLS:
                (shims / tool).symlink_to(shutil.which(tool))
            for tool, behavior in (hash_tools if hash_tools is not None else
                                   {'sha256sum': 'ok', 'shasum': 'ok'}).items():
                body = HASH_BEHAVIOR[behavior]
                if tool == 'shasum':
                    body = '[ "$1" = -a ] && [ "$2" = 256 ] || exit 2\nshift 2\n' + body
                write_executable(shims / tool, f'#!/bin/sh\necho {tool} >> "$CHECKSUM_LOG"\n{body}\n')
            path = [str(shims), *map(str, path_dirs)]
        else:
            path = [str(shims), *map(str, path_dirs), '/usr/bin', '/bin']
        env = {
            'HOME': str(self.home),
            'PATH': os.pathsep.join(path),
            'AGENT_ARCHIVE_DOWNLOAD_URL': self.release.as_uri(),
            'CODESIGN_LOG': str(self.codesign_log),
            'CHECKSUM_LOG': str(self.checksum_log),
            'SYSCTL_LOG': str(self.sysctl_log),
            'PYTHON': sys.executable,
            'HASH_HELPER': str(self.root / 'hash.py'),
            'FAKE_SIGNING_TEAM': signing_team,
        }
        for tool, body in (overrides or {}).items():
            (shims / tool).unlink(missing_ok=True)
            write_executable(shims / tool, f'#!/bin/sh\n{body}\n')
        if not default_dir:
            # The default choice may be the real /usr/local/bin.
            env['AGENT_ARCHIVE_INSTALL_DIR'] = str(self.home / '.local' / 'bin')
        env.update(extra_env or {})
        argv = [shutil.which('sh'), str(script or self.install_sh)]

        def prepare_child():
            # A test runner started in the background may have SIGINT ignored,
            # and an ignored signal cannot be trapped.
            signal.signal(signal.SIGINT, signal.SIG_DFL)
            if umask is not None:
                os.umask(umask)

        if start:
            return subprocess.Popen(argv, env=env, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True,
                                    cwd=cwd, preexec_fn=prepare_child)
        return subprocess.run(argv, env=env, capture_output=True, text=True, cwd=cwd, preexec_fn=prepare_child)

    def assert_nothing_installed(self):
        self.assertFalse((self.home / '.local').exists(), 'something was installed')

    def checksum_tools_used(self):
        return self.checksum_log.read_text().split() if self.checksum_log.exists() else []


class InstallScriptTest(InstallScriptBase):
    def test_installs_verified_binary_for_apple_silicon(self):
        result = self.run_install(extra_env={'SHELL': '/bin/bash'})
        self.assertEqual(result.returncode, 0, result.stderr)
        target = self.home / '.local' / 'bin' / 'agent-archive'
        self.assertEqual(subprocess.run([str(target)], capture_output=True, text=True).stdout, 'v9.9.9-arm64\n')
        self.assertIn('not on your PATH', result.stdout)
        self.assertIn('Add this line to ~/.bash_profile:', result.stdout)
        self.assertIn('export PATH="' + str(target.parent) + ':$PATH"', result.stdout)
        self.assertIn(f'✓ installed agent-archive v9.9.9-arm64 to {target}\n', result.stdout)
        self.assertIn('\nTo get started, run:\n\nagent-archive setup\n', result.stdout)
        self.assertEqual([p.name for p in target.parent.iterdir()], ['agent-archive'])

    def test_zsh_path_instructions(self):
        result = self.run_install(extra_env={'SHELL': '/bin/zsh'})
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('Add this line to ~/.zshrc:', result.stdout)

    def test_unknown_shell_path_instructions(self):
        result = self.run_install(extra_env={'SHELL': '/bin/fish'})
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('Add this line to your shell profile:', result.stdout)

    @unittest.skipIf(os.access('/usr/local/bin', os.W_OK), '/usr/local/bin is writable here')
    def test_defaults_to_home_local_bin(self):
        result = self.run_install(default_dir=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue((self.home / '.local' / 'bin' / 'agent-archive').exists())

    def test_picks_intel_build_and_native_build_under_rosetta(self):
        target = self.home / '.local' / 'bin' / 'agent-archive'
        result = self.run_install(machine='x86_64')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('v9.9.9-amd64', result.stdout)
        result = self.run_install(machine='x86_64', rosetta='1')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(subprocess.run([str(target)], capture_output=True, text=True).stdout, 'v9.9.9-arm64\n')

    def test_upgrades_existing_install_in_place(self):
        existing = self.root / 'existing-bin'
        existing.mkdir()
        write_executable(existing / 'agent-archive', '#!/bin/sh\necho v0.0.1\n')
        result = self.run_install(path_dirs=[existing], default_dir=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn(f'to {existing}/agent-archive', result.stdout)
        self.assertIn('\nTo get started, run:\n\nagent-archive setup\n', result.stdout)
        self.assertFalse((self.home / '.local').exists())

    def test_honors_install_dir_override(self):
        chosen = self.root / 'chosen'
        result = self.run_install(extra_env={'AGENT_ARCHIVE_INSTALL_DIR': str(chosen)})
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue((chosen / 'agent-archive').exists())

    def test_rejects_checksum_mismatch(self):
        self.write_sums({'agent-archive-darwin-arm64': '0' * 64})
        result = self.run_install()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('checksum mismatch', result.stderr)
        self.assertFalse((self.home / '.local' / 'bin' / 'agent-archive').exists())

    def test_rejects_missing_checksum_entry(self):
        self.write_sums({'agent-archive-darwin-arm64': ''})
        result = self.run_install()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('no entry for agent-archive-darwin-arm64', result.stderr)

    def test_verifies_the_developer_id_signature_with_the_pinned_team(self):
        result = self.run_install()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn(f'Signature verified (Developer ID, team {TEAM})', result.stdout)
        args = self.codesign_log.read_text().splitlines()
        self.assertEqual(args[:3], ['--verify', '--strict', f'-R={DEVELOPER_ID_REQUIREMENT}'])
        self.assertTrue(args[3].endswith('agent-archive-darwin-arm64'), args)

    def test_rejects_a_binary_signed_by_another_team(self):
        result = self.run_install(signing_team='OTHER12345')
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('not signed by the agent-archive Developer ID', result.stderr)
        self.assertFalse((self.home / '.local' / 'bin' / 'agent-archive').exists())

    def test_rejects_an_unsigned_binary(self):
        result = self.run_install(signing_team='')
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('not signed by the agent-archive Developer ID', result.stderr)
        self.assertFalse((self.home / '.local').exists())

    def test_shipped_script_without_a_team_installs_nothing(self):
        if 'team_id=""' not in INSTALL_SH.read_text():
            self.skipTest('install.sh names its signing team')
        # It says so before downloading: pointed at a release that doesn't
        # exist, it still fails on the missing team, not on the download.
        result = self.run_install(script=INSTALL_SH,
                                  extra_env={'AGENT_ARCHIVE_DOWNLOAD_URL': (self.root / 'no-release').as_uri()})
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('names no signing team yet', result.stderr)
        self.assertFalse((self.home / '.local').exists())
        self.assertFalse(self.codesign_log.exists())

    # --- Linux ---------------------------------------------------------

    def linux_output(self, arch):
        return f'v9.9.9-linux-{arch}\n'

    def installed_output(self):
        return subprocess.run([str(self.default_target)], capture_output=True, text=True).stdout

    def test_installs_linux_amd64(self):
        for machine in ('x86_64', 'amd64'):
            with self.subTest(machine=machine):
                result = self.run_install(system='Linux', machine=machine, hermetic=True)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertIn('Downloading agent-archive-linux-amd64', result.stdout)
                self.assertEqual(self.installed_output(), self.linux_output('amd64'))
                self.assertIn(f'✓ installed agent-archive v9.9.9-linux-amd64 to {self.default_target}\n', result.stdout)
                self.assertEqual([p.name for p in self.default_target.parent.iterdir()], ['agent-archive'])

    def test_installs_linux_arm64(self):
        for machine in ('aarch64', 'arm64'):
            with self.subTest(machine=machine):
                result = self.run_install(system='Linux', machine=machine, hermetic=True)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertIn('Downloading agent-archive-linux-arm64', result.stdout)
                self.assertEqual(self.installed_output(), self.linux_output('arm64'))

    def test_linux_ignores_the_rosetta_probe(self):
        # x86_64 on Linux is amd64 even if a sysctl answering "1" is around.
        result = self.run_install(system='Linux', machine='x86_64', rosetta='1', hermetic=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.installed_output(), self.linux_output('amd64'))
        self.assertFalse(self.sysctl_log.exists())

    def test_linux_rejects_unsupported_architectures(self):
        for machine in ('armv7l', 'riscv64', 'i686', 'ppc64le', 's390x'):
            with self.subTest(machine=machine):
                # Point at no release at all: it must fail before downloading.
                result = self.run_install(system='Linux', machine=machine, hermetic=True, extra_env={
                    'AGENT_ARCHIVE_DOWNLOAD_URL': (self.root / 'no-release').as_uri()})
                self.assertNotEqual(result.returncode, 0)
                self.assertIn(f'unsupported architecture: {machine}', result.stderr)
                self.assertNotIn('download failed', result.stderr)
                self.assert_nothing_installed()

    def test_darwin_rejects_unsupported_architectures(self):
        result = self.run_install(machine='ppc64', extra_env={
            'AGENT_ARCHIVE_DOWNLOAD_URL': (self.root / 'no-release').as_uri()})
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('unsupported architecture: ppc64', result.stderr)
        self.assertNotIn('download failed', result.stderr)
        self.assert_nothing_installed()

    def test_rejects_unsupported_operating_systems(self):
        for system in ('FreeBSD', 'MINGW64_NT-10.0-22631', 'CYGWIN_NT-10.0', 'SunOS', 'Windows_NT'):
            with self.subTest(system=system):
                result = self.run_install(system=system, machine='x86_64', extra_env={
                    'AGENT_ARCHIVE_DOWNLOAD_URL': (self.root / 'no-release').as_uri()})
                self.assertNotEqual(result.returncode, 0)
                self.assertIn('recognises macOS and Linux release assets only', result.stderr)
                self.assertIn(system, result.stderr)
                self.assertNotIn('download failed', result.stderr)
                self.assert_nothing_installed()

    def test_linux_install_needs_no_codesign_and_says_how_to_verify(self):
        # The harness downloads from AGENT_ARCHIVE_DOWNLOAD_URL, a custom
        # source with no GitHub attestation: the skip is said out loud,
        # before the install, and no post-install advice follows execution.
        result = self.run_install(system='Linux', machine='x86_64', codesign=False)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertNotIn('Signature verified', result.stdout)
        self.assertIn('Checksum verified', result.stdout)
        self.assertIn('Skipping the Developer ID signature check, which is macOS only.', result.stdout)
        skip = 'Skipping attestation verification: AGENT_ARCHIVE_DOWNLOAD_URL is set'
        self.assertIn(skip, result.stdout)
        self.assertLess(result.stdout.index(skip), result.stdout.index('✓ installed'))
        self.assertNotIn('gh attestation verify', result.stdout)
        self.assertNotIn('only guards against a damaged download', result.stdout)
        self.assertNotIn('trust rests', result.stdout)

    def test_the_attestation_advice_quotes_awkward_paths(self):
        # No gh on the hermetic PATH and a real (faked-curl) release base:
        # the not-signed advice names the final path before installing.
        awkward = self.root / "it's here"
        curl, _ = self.fake_release_curl()
        result = self.run_install(system='Linux', machine='x86_64', hermetic=True,
                                  overrides={'curl': curl},
                                  extra_env={'AGENT_ARCHIVE_DOWNLOAD_URL': '',
                                             'AGENT_ARCHIVE_INSTALL_DIR': str(awkward)})
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertLess(result.stdout.index('gh attestation verify'), result.stdout.index('✓ installed'))
        # Pasted into a shell, the printed word is exactly the installed path.
        word = re.search(r'gh attestation verify (.*) --repo', result.stdout).group(1)
        pasted = subprocess.run(['sh', '-c', f'printf %s {word}'], capture_output=True, text=True).stdout
        self.assertEqual(pasted, str(awkward / 'agent-archive'))

    def test_darwin_prints_no_attestation_advice(self):
        result = self.run_install()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertNotIn('attestation', result.stdout)

    def test_linux_never_invokes_codesign(self):
        result = self.run_install(system='Linux', machine='aarch64', hermetic=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse(self.codesign_log.exists())

    # --- Linux attestation, before anything is installed or run --------
    # These runs clear AGENT_ARCHIVE_DOWNLOAD_URL (which skips attestation)
    # and serve the fake release through a curl stand-in instead, so the
    # script takes its real GitHub download base.

    def fake_release_curl(self):
        """(curl shim body, log path): copies the file named by the URL's
        last segment out of the fake release and logs each URL."""
        log = self.root / 'curl.log'
        body = (
            'out=\nprev=\nlast=\n'
            'for a in "$@"; do\n'
            '  if [ "$prev" = --output ]; then out="$a"; fi\n'
            '  prev="$a"\n  last="$a"\n'
            'done\n'
            f'echo "$last" >> {log}\n'
            f'cp "{self.release}/${{last##*/}}" "$out"'
        )
        return body, log

    def fake_gh(self, order_log, exit_code=0, output=''):
        """A gh stand-in: logs its arguments, accepts only the exact
        attestation-verify form against an existing file, then behaves as
        told."""
        say = f'echo "{output}" >&2\n' if output else ''
        return (
            f'echo "gh $@" >> {order_log}\n'
            '[ "$1" = attestation ] && [ "$2" = verify ] || exit 9\n'
            '[ -f "$3" ] || exit 8\n'
            '[ "$4" = --repo ] && [ "$5" = wangjohn/agent-archive ] || exit 9\n'
            f'{say}exit {exit_code}'
        )

    def order_logging_linux_binary(self, order_log, arch='amd64'):
        """Replace a fake Linux release binary with one that records each
        run in order_log, so a test can prove what happened before it."""
        write_executable(self.release / f'agent-archive-linux-{arch}',
                         f'#!/bin/sh\necho ran >> {order_log}\necho v9.9.9-linux-{arch}\n')
        self.write_sums()

    def run_linux_from_github_base(self, overrides, extra_env=None):
        env = {'AGENT_ARCHIVE_DOWNLOAD_URL': '', **(extra_env or {})}
        return self.run_install(system='Linux', machine='x86_64', hermetic=True,
                                overrides=overrides, extra_env=env)

    def test_linux_gh_attestation_is_verified_before_the_binary_is_installed_or_run(self):
        order = self.root / 'order.log'
        self.order_logging_linux_binary(order)
        curl, curl_log = self.fake_release_curl()
        result = self.run_linux_from_github_base({'curl': curl, 'gh': self.fake_gh(order)})
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('releases/latest/download/agent-archive-linux-amd64', curl_log.read_text())
        lines = order.read_text().splitlines()
        gh_lines = [line for line in lines if line.startswith('gh ')]
        self.assertEqual(len(gh_lines), 1, lines)
        # gh verified the downloaded temp file, not the install target, and
        # did so before the binary ever ran.
        verified_file = gh_lines[0].split()[3]
        self.assertTrue(verified_file.endswith('/agent-archive-linux-amd64'), gh_lines)
        self.assertNotEqual(verified_file, str(self.default_target))
        self.assertLess(lines.index(gh_lines[0]), lines.index('ran'))
        verified = 'Attestation verified (GitHub build provenance, wangjohn/agent-archive)'
        self.assertIn(verified, result.stdout)
        self.assertLess(result.stdout.index(verified), result.stdout.index('✓ installed'))
        self.assertEqual(self.installed_output(), self.linux_output('amd64'))

    def test_linux_gh_attestation_failure_installs_and_runs_nothing(self):
        order = self.root / 'order.log'
        self.order_logging_linux_binary(order)
        curl, _ = self.fake_release_curl()
        for output, expected in (
                ('verification failed: no attestation found', 'attestation verification failed'),
                ('dial tcp: network unreachable', 'could not verify the attestation')):
            with self.subTest(output=output):
                order.unlink(missing_ok=True)
                result = self.run_linux_from_github_base(
                    {'curl': curl, 'gh': self.fake_gh(order, exit_code=1, output=output)})
                self.assertNotEqual(result.returncode, 0)
                self.assertIn(expected, result.stderr)
                self.assertIn(output, result.stderr)
                self.assertIn('Checksum verified', result.stdout)
                self.assertNotIn('✓ installed', result.stdout)
                self.assertNotIn('ran', order.read_text().splitlines())
                self.assert_nothing_installed()

    def test_linux_without_gh_warns_before_installing_and_proceeds(self):
        curl, _ = self.fake_release_curl()
        result = self.run_linux_from_github_base({'curl': curl})
        self.assertEqual(result.returncode, 0, result.stderr)
        warning = 'only guards against a damaged download'
        self.assertIn(warning, result.stdout)
        self.assertLess(result.stdout.index(warning), result.stdout.index('✓ installed'))
        self.assertIn('gh attestation verify', result.stdout)
        # The warning is printed once, before execution, not again after it.
        self.assertEqual(result.stdout.count(warning), 1)
        self.assertEqual(result.stdout.count('gh attestation verify'), 1)
        self.assertIn('docs/getting-started/install.md', result.stdout)
        self.assertIn('Continuing without attestation verification because gh is not on PATH', result.stdout)
        self.assertEqual(self.installed_output(), self.linux_output('amd64'))

    def test_linux_require_attestation_without_gh_fails_closed_before_downloading(self):
        curl, curl_log = self.fake_release_curl()
        result = self.run_linux_from_github_base(
            {'curl': curl}, {'AGENT_ARCHIVE_REQUIRE_ATTESTATION': '1'})
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('AGENT_ARCHIVE_REQUIRE_ATTESTATION=1 requires the gh CLI', result.stderr)
        self.assertNotIn('Downloading', result.stdout)
        self.assertFalse(curl_log.exists())
        self.assert_nothing_installed()

    def test_linux_require_attestation_with_a_verifying_gh_proceeds(self):
        order = self.root / 'order.log'
        curl, _ = self.fake_release_curl()
        result = self.run_linux_from_github_base(
            {'curl': curl, 'gh': self.fake_gh(order)},
            {'AGENT_ARCHIVE_REQUIRE_ATTESTATION': '1'})
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('Attestation verified', result.stdout)
        self.assertEqual(self.installed_output(), self.linux_output('amd64'))

    def test_linux_require_attestation_refuses_a_custom_download_url_before_downloading(self):
        curl, curl_log = self.fake_release_curl()
        # The harness default AGENT_ARCHIVE_DOWNLOAD_URL stays set here.
        result = self.run_install(system='Linux', machine='x86_64', hermetic=True,
                                  overrides={'curl': curl},
                                  extra_env={'AGENT_ARCHIVE_REQUIRE_ATTESTATION': '1'})
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('cannot be combined with AGENT_ARCHIVE_DOWNLOAD_URL', result.stderr)
        self.assertNotIn('Downloading', result.stdout)
        self.assertFalse(curl_log.exists(), 'a download was attempted')
        self.assert_nothing_installed()

    def test_linux_custom_download_url_skips_attestation_with_a_say_line(self):
        # Even a gh that would fail is never consulted for a custom source.
        order = self.root / 'order.log'
        result = self.run_install(system='Linux', machine='x86_64', hermetic=True,
                                  overrides={'gh': self.fake_gh(order, exit_code=1, output='no')})
        self.assertEqual(result.returncode, 0, result.stderr)
        skip = 'Skipping attestation verification: AGENT_ARCHIVE_DOWNLOAD_URL is set'
        self.assertIn(skip, result.stdout)
        self.assertLess(result.stdout.index(skip), result.stdout.index('✓ installed'))
        self.assertFalse(order.exists(), 'gh was consulted for a custom download source')
        self.assertEqual(self.installed_output(), self.linux_output('amd64'))

    def test_linux_install_does_not_depend_on_the_signing_team(self):
        # The signing team is a macOS matter: neither a real nor an empty
        # one changes a Linux install.
        for team in (None, ''):
            with self.subTest(team=team):
                script = INSTALL_SH
                if team is not None:
                    script = self.root / 'no-team.sh'
                    script.write_text(with_team(INSTALL_SH.read_text(), team))
                result = self.run_install(system='Linux', machine='x86_64', hermetic=True, script=script)
                self.assertEqual(result.returncode, 0, result.stderr)

    def test_linux_bash_path_instructions_use_bashrc(self):
        result = self.run_install(system='Linux', machine='x86_64', hermetic=True, extra_env={'SHELL': '/bin/bash'})
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('Add this line to ~/.bashrc:', result.stdout)
        self.assertNotIn('bash_profile', result.stdout)
        result = self.run_install(system='Linux', machine='x86_64', hermetic=True, extra_env={'SHELL': '/bin/zsh'})
        self.assertIn('Add this line to ~/.zshrc:', result.stdout)

    def test_linux_defaults_to_home_local_bin(self):
        if os.access('/usr/local/bin', os.W_OK):
            self.skipTest('/usr/local/bin is writable here')
        result = self.run_install(system='Linux', machine='x86_64', hermetic=True, default_dir=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(self.default_target.exists())

    def test_linux_upgrades_existing_install_in_place(self):
        existing = self.root / 'existing-bin'
        existing.mkdir()
        write_executable(existing / 'agent-archive', '#!/bin/sh\necho v0.0.1\n')
        result = self.run_install(system='Linux', machine='x86_64', hermetic=True, path_dirs=[existing],
                                  default_dir=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(subprocess.run([str(existing / 'agent-archive')], capture_output=True, text=True).stdout,
                         self.linux_output('amd64'))
        self.assertFalse((self.home / '.local').exists())

    def test_linux_without_a_home_asks_for_an_install_dir(self):
        if os.access('/usr/local/bin', os.W_OK):
            self.skipTest('/usr/local/bin is writable here')
        result = self.run_install(system='Linux', machine='x86_64', hermetic=True, default_dir=True,
                                  extra_env={'HOME': ''})
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('AGENT_ARCHIVE_INSTALL_DIR', result.stderr)

    def test_fails_without_curl(self):
        result = self.run_install(system='Linux', machine='x86_64', hermetic=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        shims = self.root / 'shims-1'
        (shims / 'curl').unlink()
        env = {'PATH': str(shims), 'HOME': str(self.home), 'AGENT_ARCHIVE_INSTALL_DIR': str(self.root / 'x')}
        result = subprocess.run([shutil.which('sh'), str(self.install_sh)], env=env, capture_output=True, text=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('curl is required', result.stderr)

    # --- Checksums, on both operating systems --------------------------

    SYSTEMS = (('Linux', 'x86_64', 'linux', 'amd64'), ('Linux', 'aarch64', 'linux', 'arm64'),
               ('Darwin', 'arm64', 'darwin', 'arm64'), ('Darwin', 'x86_64', 'darwin', 'amd64'))

    def each_system(self, light=False):
        """Yield (system, machine, asset name) for every supported system,
        each inside its own subTest. light=True yields one Linux and one
        macOS system, for tests that run many installs each."""
        for system, machine, os_name, arch in (self.SYSTEMS[::2] if light else self.SYSTEMS):
            name = f'agent-archive-{os_name}-{arch}'
            with self.subTest(system=system, machine=machine):
                yield system, machine, name

    def test_checksum_mismatch_installs_nothing(self):
        for system, machine, name in self.each_system():
            self.write_sums({name: '0' * 64})
            result = self.run_install(system=system, machine=machine, hermetic=True)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn(f'checksum mismatch for {name}', result.stderr)
            self.assertNotIn('Checksum verified', result.stdout)
            self.assert_nothing_installed()

    def test_a_tampered_binary_installs_nothing(self):
        for system, machine, name in self.each_system():
            (self.release / name).write_text('#!/bin/sh\necho evil\n')
            result = self.run_install(system=system, machine=machine, hermetic=True)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn('checksum mismatch', result.stderr)
            self.assert_nothing_installed()
            self.assertNotIn('evil', result.stdout)

    def test_a_truncated_or_empty_download_installs_nothing(self):
        for system, machine, name in self.each_system():
            good = (self.release / name).read_bytes()
            self.write_sums()
            (self.release / name).write_bytes(good[:10])
            result = self.run_install(system=system, machine=machine, hermetic=True)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn('checksum mismatch', result.stderr)
            # Even an empty binary whose digest a SHA256SUMS lists is refused.
            (self.release / name).write_bytes(b'')
            self.write_sums()
            result = self.run_install(system=system, machine=machine, hermetic=True)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn('is empty', result.stderr)
            self.assert_nothing_installed()
            (self.release / name).write_bytes(good)

    def test_missing_checksum_entry_installs_nothing(self):
        for system, machine, name in self.each_system():
            self.write_sums({name: ''})
            result = self.run_install(system=system, machine=machine, hermetic=True)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn(f'no entry for {name}', result.stderr)
            self.assert_nothing_installed()

    def test_checksum_entry_is_matched_by_exact_name(self):
        for system, machine, name in self.each_system(light=True):
            digest = hashlib.sha256((self.release / name).read_bytes()).hexdigest()
            for listed in (f'{name}.sig', f'x{name}', f'{name}x', f'other/{name}', name.upper()):
                (self.release / 'SHA256SUMS').write_text(f'{digest}  {listed}\n')
                result = self.run_install(system=system, machine=machine, hermetic=True)
                self.assertNotEqual(result.returncode, 0, listed)
                self.assertIn('no entry', result.stderr)
                self.assert_nothing_installed()

    def test_binary_mode_checksum_entry_is_accepted(self):
        for system, machine, name in self.each_system():
            digest = hashlib.sha256((self.release / name).read_bytes()).hexdigest()
            (self.release / 'SHA256SUMS').write_text(f'{digest} *{name}\n')
            result = self.run_install(system=system, machine=machine, hermetic=True)
            self.assertEqual(result.returncode, 0, result.stderr)

    def test_malformed_or_duplicate_checksum_entries_install_nothing(self):
        for system, machine, name in self.each_system(light=True):
            digest = hashlib.sha256((self.release / name).read_bytes()).hexdigest()
            for sums in (f'{digest[:-1]}  {name}\n',  # short
                         f'{digest}0  {name}\n',  # long
                         f'{digest.upper()}  {name}\n',  # not lowercase
                         f'{"g" * 64}  {name}\n',  # not hex
                         f'{digest}  {name}\n{"0" * 64}  {name}\n',  # duplicate, one right
                         f'{digest}  {name}\n{digest}  {name}\n'):  # duplicate, identical
                (self.release / 'SHA256SUMS').write_text(sums)
                result = self.run_install(system=system, machine=machine, hermetic=True)
                self.assertNotEqual(result.returncode, 0, sums)
                self.assertIn('malformed or duplicate entry', result.stderr)
                self.assert_nothing_installed()

    def test_empty_checksum_file_installs_nothing(self):
        for system, machine, name in self.each_system():
            (self.release / 'SHA256SUMS').write_text('')
            result = self.run_install(system=system, machine=machine, hermetic=True)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn(f'no entry for {name}', result.stderr)
            self.assert_nothing_installed()

    def test_download_failures_install_nothing(self):
        # curl retries a failed download for several seconds, so one system
        # stands for all: this step does not depend on the OS.
        name = 'agent-archive-linux-amd64'
        for missing in (name, 'SHA256SUMS'):
            with self.subTest(missing=missing):
                hidden = self.release / f'{missing}.hidden'
                (self.release / missing).rename(hidden)
                try:
                    result = self.run_install(system='Linux', machine='x86_64', hermetic=True)
                finally:
                    hidden.rename(self.release / missing)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn('download failed', result.stderr)
                self.assert_nothing_installed()

    def test_an_error_page_in_place_of_the_binary_installs_nothing(self):
        # A server that answers 200 with an HTML error page for the binary.
        for system, machine, name in self.each_system():
            (self.release / name).write_text('<html><body>Not Found</body></html>\n')
            result = self.run_install(system=system, machine=machine, hermetic=True)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn('checksum mismatch', result.stderr)
            self.assert_nothing_installed()

    def test_an_error_page_in_place_of_the_checksums_installs_nothing(self):
        for system, machine, name in self.each_system():
            (self.release / 'SHA256SUMS').write_text('<html><body>Not Found</body></html>\n')
            result = self.run_install(system=system, machine=machine, hermetic=True)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn('no entry', result.stderr)
            self.assert_nothing_installed()

    def test_uses_sha256sum_when_it_is_the_only_tool(self):
        for system, machine, name in self.each_system():
            self.checksum_log.unlink(missing_ok=True)
            result = self.run_install(system=system, machine=machine, hash_tools={'sha256sum': 'ok'})
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(set(self.checksum_tools_used()), {'sha256sum'})

    def test_uses_shasum_when_it_is_the_only_tool(self):
        for system, machine, name in self.each_system():
            self.checksum_log.unlink(missing_ok=True)
            result = self.run_install(system=system, machine=machine, hash_tools={'shasum': 'ok'})
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(set(self.checksum_tools_used()), {'shasum'})

    def test_prefers_sha256sum_when_both_exist(self):
        result = self.run_install(system='Linux', machine='x86_64', hash_tools={'sha256sum': 'ok', 'shasum': 'ok'})
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(set(self.checksum_tools_used()), {'sha256sum'})

    def test_with_no_checksum_tool_nothing_is_downloaded_or_installed(self):
        for system, machine, name in self.each_system():
            # Even pointed at a release that does not exist, it fails on the
            # missing tool first.
            result = self.run_install(system=system, machine=machine, hash_tools={}, extra_env={
                'AGENT_ARCHIVE_DOWNLOAD_URL': (self.root / 'no-release').as_uri()})
            self.assertNotEqual(result.returncode, 0)
            self.assertIn('no SHA-256 tool found (need sha256sum or shasum)', result.stderr)
            self.assertNotIn('download failed', result.stderr)
            self.assertNotIn('Checksum verified', result.stdout)
            self.assert_nothing_installed()

    def test_a_failing_checksum_tool_installs_nothing_and_does_not_fall_back(self):
        # The chosen tool fails: the install stops, rather than trusting an
        # empty digest or quietly using the other tool instead.
        for system, machine, name in self.each_system():
            for tools, chosen in (({'sha256sum': 'fail'}, 'sha256sum'),
                                  ({'shasum': 'fail'}, 'shasum'),
                                  ({'sha256sum': 'fail', 'shasum': 'ok'}, 'sha256sum')):
                self.checksum_log.unlink(missing_ok=True)
                result = self.run_install(system=system, machine=machine, hash_tools=tools)
                self.assertNotEqual(result.returncode, 0, tools)
                self.assertIn(f'{chosen} failed on {name}', result.stderr)
                self.assertNotIn('Checksum verified', result.stdout)
                self.assertEqual(set(self.checksum_tools_used()), {chosen})
                self.assert_nothing_installed()

    def test_a_checksum_tool_printing_nothing_installs_nothing(self):
        # Exit status 0 and no digest: what a `tool | awk` pipeline would
        # turn a failing tool into.
        for behavior in ('silent', 'empty_digest'):
            for tool in ('sha256sum', 'shasum'):
                for system, machine, name in self.each_system(light=True):
                    result = self.run_install(system=system, machine=machine, hash_tools={tool: behavior})
                    self.assertNotEqual(result.returncode, 0, (behavior, tool))
                    self.assertIn('printed no valid SHA-256', result.stderr)
                    self.assertNotIn('Checksum verified', result.stdout)
                    self.assert_nothing_installed()

    def test_a_garbage_digest_never_matches_a_garbage_sums_entry(self):
        # If both sides were the same malformed string, a plain comparison
        # would pass. It must not.
        for system, machine, name in self.each_system():
            (self.release / 'SHA256SUMS').write_text(f'deadbeef  {name}\n')
            result = self.run_install(system=system, machine=machine, hash_tools={'sha256sum': 'garbage'})
            self.assertNotEqual(result.returncode, 0)
            self.assertNotIn('Checksum verified', result.stdout)
            self.assert_nothing_installed()

    def test_nothing_is_executed_before_the_checksum_is_verified(self):
        for system, machine, name in self.each_system():
            marker = self.root / 'ran'
            marker.unlink(missing_ok=True)
            # A shell builtin: the hermetic PATH has no touch.
            write_executable(self.release / name, f'#!/bin/sh\n: > {marker}\necho v1\n')
            self.write_sums({name: '1' * 64})
            result = self.run_install(system=system, machine=machine, hermetic=True)
            self.assertNotEqual(result.returncode, 0)
            self.assertFalse(marker.exists())
            self.assert_nothing_installed()

    def test_a_failed_upgrade_leaves_the_existing_binary_alone(self):
        for system, machine, name in self.each_system():
            existing = self.home / '.local' / 'bin'
            existing.mkdir(parents=True, exist_ok=True)
            write_executable(existing / 'agent-archive', '#!/bin/sh\necho old\n')
            self.write_sums({name: '0' * 64})
            result = self.run_install(system=system, machine=machine, hermetic=True)
            self.assertNotEqual(result.returncode, 0)
            self.assertEqual(self.installed_output(), 'old\n')
            self.assertEqual([p.name for p in existing.iterdir()], ['agent-archive'])

    # --- macOS still requires the signature ----------------------------

    def test_darwin_install_fails_when_the_codesign_check_fails(self):
        for machine in ('arm64', 'x86_64'):
            for team in ('', 'OTHER12345'):
                with self.subTest(machine=machine, team=team):
                    self.codesign_log.unlink(missing_ok=True)
                    result = self.run_install(machine=machine, signing_team=team, hermetic=True)
                    self.assertNotEqual(result.returncode, 0)
                    self.assertIn('not signed by the agent-archive Developer ID', result.stderr)
                    self.assertNotIn('Skipping', result.stdout)
                    self.assertTrue(self.codesign_log.exists())
                    self.assert_nothing_installed()

    def test_darwin_install_fails_when_codesign_is_missing(self):
        for machine in ('arm64', 'x86_64'):
            with self.subTest(machine=machine):
                result = self.run_install(machine=machine, codesign=False)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn('not signed by the agent-archive Developer ID', result.stderr)
                self.assertIn('Checksum verified', result.stdout)
                self.assertNotIn('Skipping', result.stdout)
                self.assert_nothing_installed()

    def test_darwin_checks_the_signature_whichever_checksum_tool_it_uses(self):
        for tool in ('sha256sum', 'shasum'):
            with self.subTest(tool=tool):
                shutil.rmtree(self.home / '.local', ignore_errors=True)
                result = self.run_install(hash_tools={tool: 'ok'}, signing_team='')
                self.assertNotEqual(result.returncode, 0)
                self.assertTrue(self.codesign_log.exists())
                self.assert_nothing_installed()
                result = self.run_install(hash_tools={tool: 'ok'})
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertIn(f'Signature verified (Developer ID, team {TEAM})', result.stdout)

    def test_darwin_still_needs_a_pinned_team_before_anything_else(self):
        no_team = self.root / 'no-team.sh'
        no_team.write_text(with_team(INSTALL_SH.read_text(), ''))
        result = self.run_install(script=no_team, hermetic=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('names no signing team yet', result.stderr)
        self.assert_nothing_installed()
        self.assertFalse(self.checksum_log.exists())

    def test_darwin_messages_are_unchanged(self):
        result = self.run_install(extra_env={'SHELL': '/bin/bash'})
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout.splitlines()[:4], [
            'Downloading agent-archive-darwin-arm64 (latest)',
            f'Downloading from {self.release.as_uri()}',
            'Checksum verified',
            f'Signature verified (Developer ID, team {TEAM})',
        ])

    # --- Inputs, downloads, and cleanup --------------------------------

    def test_release_version_must_look_like_a_tag(self):
        for version in ('v1/../x', '../evil', 'v1.0.0/', 'v1..2', 'v', 'v1 2', 'v1;id', 'v1$x', '/x', 'v1?x=y'):
            with self.subTest(version=version):
                result = self.run_install(extra_env={'AGENT_ARCHIVE_VERSION': version})
                self.assertNotEqual(result.returncode, 0)
                self.assertIn('AGENT_ARCHIVE_VERSION is not a release tag', result.stderr)
                self.assertNotIn('Downloading', result.stdout)
                self.assert_nothing_installed()

    def test_release_version_accepts_tags_with_or_without_the_v(self):
        for given, shown in (('v1.2.3', 'v1.2.3'), ('1.2.3', 'v1.2.3'), ('v1.0.0-rc.1', 'v1.0.0-rc.1'),
                             ('v1.0.0+build5', 'v1.0.0+build5')):
            with self.subTest(version=given):
                result = self.run_install(extra_env={'AGENT_ARCHIVE_VERSION': given})
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertIn(f'Downloading agent-archive-darwin-arm64 ({shown})', result.stdout)

    def test_the_version_becomes_the_release_url_when_no_override_is_given(self):
        # A curl stand-in that only reports what it was asked for.
        log = self.root / 'curl.log'
        result = self.run_install(hermetic=True, overrides={'curl': f'echo "$@" >> {log}\nexit 1'}, extra_env={
            'AGENT_ARCHIVE_DOWNLOAD_URL': '', 'AGENT_ARCHIVE_VERSION': '1.2.3'})
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('https://github.com/wangjohn/agent-archive/releases/download/v1.2.3/agent-archive-darwin-arm64',
                      log.read_text())
        self.assertNotIn('Downloading from', result.stdout)

    def test_plain_http_download_bases_are_refused_without_a_request(self):
        import http.server
        import threading
        seen = []

        class Handler(http.server.BaseHTTPRequestHandler):
            def do_GET(self):
                seen.append(self.path)
                self.send_response(200)
                self.end_headers()

            def log_message(self, *args):
                pass

        server = http.server.HTTPServer(('127.0.0.1', 0), Handler)
        self.addCleanup(server.server_close)
        threading.Thread(target=server.serve_forever, daemon=True).start()
        self.addCleanup(server.shutdown)
        for system, machine in (('Linux', 'x86_64'), ('Darwin', 'arm64')):
            with self.subTest(system=system):
                result = self.run_install(system=system, machine=machine, hermetic=True, extra_env={
                    'AGENT_ARCHIVE_DOWNLOAD_URL': f'http://127.0.0.1:{server.server_port}/rel'})
                self.assertNotEqual(result.returncode, 0)
                self.assertIn('download failed', result.stderr)
                self.assertEqual(seen, [])
                self.assert_nothing_installed()

    def temp_dir_entries(self):
        tmpdir = self.root / 'tmpdir'
        return sorted(p.name for p in tmpdir.iterdir())

    def private_tmpdir(self):
        """Overrides that make `mktemp -d` create its directory under
        self.root / 'tmpdir' (macOS's mktemp ignores TMPDIR)."""
        (self.root / 'tmpdir').mkdir(exist_ok=True)
        real = shutil.which('mktemp')
        return {'mktemp': f'[ "$1" = -d ] && [ $# -eq 1 ] && exec {real} -d "{self.root}/tmpdir/install.XXXXXXXX"\n'
                          f'exec {real} "$@"'}

    def test_failed_installs_leave_no_temporary_files(self):
        overrides = self.private_tmpdir()
        self.write_sums({'agent-archive-linux-amd64': '0' * 64})
        result = self.run_install(system='Linux', machine='x86_64', hermetic=True, overrides=overrides)
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(self.temp_dir_entries(), [])
        self.write_sums()
        result = self.run_install(system='Linux', machine='x86_64', hermetic=True, overrides=overrides)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.temp_dir_entries(), [])

    def test_a_signal_during_the_download_removes_the_temporary_directory(self):
        import time
        started = self.root / 'curl.started'
        # A download that is still running when the terminal closes or the
        # user interrupts.
        slow_curl = f': > {started}\n/bin/sleep 2\nexit 1'
        for sig, code in ((signal.SIGHUP, 129), (signal.SIGTERM, 130), (signal.SIGINT, 130)):
            with self.subTest(signal=sig.name):
                started.unlink(missing_ok=True)
                proc = self.run_install(system='Linux', machine='x86_64', hermetic=True, start=True,
                                        overrides={'curl': slow_curl, **self.private_tmpdir()})
                try:
                    deadline = time.time() + 10
                    while not started.exists() and time.time() < deadline:
                        time.sleep(0.05)
                    self.assertTrue(started.exists(), 'the download never started')
                    self.assertEqual(len(self.temp_dir_entries()), 1)
                    proc.send_signal(sig)
                    proc.communicate(timeout=20)
                finally:
                    if proc.poll() is None:
                        proc.kill()
                        proc.communicate()
                self.assertEqual(proc.returncode, code)
                self.assertEqual(self.temp_dir_entries(), [])
                self.assert_nothing_installed()

    def test_fails_clearly_when_no_temporary_directory_can_be_made(self):
        result = self.run_install(system='Linux', machine='x86_64', hermetic=True, overrides={'mktemp': 'exit 1'})
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('cannot create a temporary directory', result.stderr)
        self.assert_nothing_installed()

    def test_the_staged_file_is_not_predictable_and_is_removed_on_failure(self):
        for tool in ('cp', 'chmod'):
            with self.subTest(failing=tool):
                result = self.run_install(system='Linux', machine='x86_64', hermetic=True, overrides={tool: 'exit 1'})
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(list((self.home / '.local' / 'bin').iterdir()), [])
        # It is made with mktemp beside the target, not named after $$.
        log = self.root / 'mktemp.log'
        result = self.run_install(system='Linux', machine='x86_64', hermetic=True, overrides={
            'mktemp': f'echo "$@" >> {log}\nexec {shutil.which("mktemp")} "$@"'})
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn(f'{self.home}/.local/bin/.agent-archive.install.XXXXXXXX', log.read_text())

    def test_fails_clearly_when_no_staging_file_can_be_made(self):
        # mktemp works for the download directory but not beside the target.
        real = shutil.which('mktemp')
        result = self.run_install(system='Linux', machine='x86_64', hermetic=True, overrides={
            'mktemp': f'[ "$1" = -d ] && exec {real} -d\nexit 1'})
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('cannot create a temporary file in', result.stderr)
        self.assertEqual(list((self.home / '.local' / 'bin').iterdir()), [])

    def test_refuses_to_install_over_a_directory(self):
        (self.default_target).mkdir(parents=True)
        result = self.run_install(system='Linux', machine='x86_64', hermetic=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('is a directory', result.stderr)
        self.assertEqual(list(self.default_target.iterdir()), [])


    # --- What lands on disk --------------------------------------------

    def test_an_upgrade_replaces_the_file_rather_than_overwriting_it(self):
        # macOS can kill an upgraded binary that was overwritten in place, so
        # the new file must be a new inode: an old hard link keeps old content.
        for system, machine in (('Darwin', 'arm64'), ('Linux', 'x86_64')):
            with self.subTest(system=system):
                shutil.rmtree(self.home / '.local', ignore_errors=True)
                self.default_target.parent.mkdir(parents=True)
                write_executable(self.default_target, '#!/bin/sh\necho old\n')
                old_link = self.root / f'old-link-{system}'
                os.link(self.default_target, old_link)
                old_inode = self.default_target.stat().st_ino
                result = self.run_install(system=system, machine=machine, hermetic=True)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertNotEqual(self.default_target.stat().st_ino, old_inode)
                self.assertEqual(old_link.read_text(), '#!/bin/sh\necho old\n')
                self.assertNotEqual(self.installed_output(), 'old\n')

    def test_the_installed_binary_is_mode_755_whatever_the_umask_or_staging_mode(self):
        real = shutil.which('mktemp')
        # A staging file that mktemp made world-writable, and one made 0600.
        loose = {'mktemp': f'f="$({real} "$@")" || exit 1\n[ "$1" = -d ] || chmod 666 "$f"\necho "$f"'}
        for label, umask, overrides in (('umask 000', 0o000, None), ('umask 077', 0o077, None),
                                        ('umask 022', 0o022, None), ('loose staging file', 0o000, loose)):
            for system, machine in (('Darwin', 'arm64'), ('Linux', 'x86_64')):
                with self.subTest(case=label, system=system):
                    shutil.rmtree(self.home / '.local', ignore_errors=True)
                    result = self.run_install(system=system, machine=machine, hermetic=True, umask=umask,
                                              overrides=overrides)
                    self.assertEqual(result.returncode, 0, result.stderr)
                    self.assertEqual(stat.S_IMODE(self.default_target.stat().st_mode), 0o755)

    def test_a_relative_install_dir_is_made_absolute(self):
        work = self.root / 'work'
        work.mkdir()
        for given in ('rel/bin', './rel/bin', 'rel/bin/'):
            with self.subTest(given=given):
                shutil.rmtree(work / 'rel', ignore_errors=True)
                result = self.run_install(system='Linux', machine='x86_64', hermetic=True, cwd=work,
                                          extra_env={'AGENT_ARCHIVE_INSTALL_DIR': given})
                self.assertEqual(result.returncode, 0, result.stderr)
                target = (work / 'rel' / 'bin' / 'agent-archive').resolve()
                self.assertTrue(target.exists())
                self.assertIn(f'installed agent-archive v9.9.9-linux-amd64 to {work.resolve()}/rel/bin', result.stdout)
                # Every printed path can be pasted from another directory.
                # (The attestation line here is the custom-download-URL
                # skip, which names no path.)
                self.assertIn('Skipping attestation verification: AGENT_ARCHIVE_DOWNLOAD_URL is set',
                              result.stdout)
                self.assertIn(f'export PATH="{work.resolve()}/rel/bin', result.stdout)
                self.assertNotIn('/./', result.stdout)

    def test_an_install_dir_starting_with_a_dash_is_not_taken_for_an_option(self):
        work = self.root / 'work'
        work.mkdir()
        for system, machine in (('Linux', 'x86_64'), ('Darwin', 'arm64')):
            with self.subTest(system=system):
                shutil.rmtree(work / '-bin', ignore_errors=True)
                result = self.run_install(system=system, machine=machine, hermetic=True, cwd=work,
                                          extra_env={'AGENT_ARCHIVE_INSTALL_DIR': '-bin'})
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertTrue((work / '-bin' / 'agent-archive').exists())
                self.assertEqual([p.name for p in (work / '-bin').iterdir()], ['agent-archive'])

    def test_the_word_latest_is_not_a_release_tag(self):
        result = self.run_install(extra_env={'AGENT_ARCHIVE_VERSION': 'latest'})
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('leave it unset', result.stderr)
        self.assertNotIn('Downloading', result.stdout)
        self.assert_nothing_installed()


# A release binary that logs how `setup` is run. FAKE_REFRESH_EXIT and
# FAKE_REFRESH_OUTPUT decide what `setup --refresh` does; any other setup
# command fails, so an installer that ran setup in its own way would show.
REFRESHING_BINARY = """#!/bin/sh
case "$1" in
--version) echo v9.9.9 ;;
setup)
  echo "args: $*" >> "$SETUP_LOG"
  echo "home: ${AGENT_ARCHIVE_HOME:-}" >> "$SETUP_LOG"
  if [ -t 0 ]; then echo "stdin: terminal" >> "$SETUP_LOG"; fi
  [ "$*" = "setup --refresh" ] || { echo "unexpected setup command" >&2; exit 2; }
  echo "$FAKE_REFRESH_OUTPUT"
  echo "$FAKE_REFRESH_ERROR" >&2
  exit "${FAKE_REFRESH_EXIT:-0}"
  ;;
esac
"""


class InstallRefreshTest(InstallScriptBase):
    """An upgrade of a set-up Mac runs `setup --refresh`; a fresh install runs nothing."""

    def setUp(self):
        super().setUp()
        for arch in ('arm64', 'amd64'):
            write_executable(self.release / f'agent-archive-darwin-{arch}', REFRESHING_BINARY)
        self.write_sums()
        self.setup_log = self.root / 'setup.log'
        self.default_data = self.home / '.local' / 'share' / 'agent-archive'

    def configure(self, data_dir):
        data_dir.mkdir(parents=True)
        (data_dir / 'config.json').write_text('{}\n')

    def install(self, **env):
        env = {'SETUP_LOG': str(self.setup_log), 'FAKE_REFRESH_OUTPUT': 'nothing to refresh', **env}
        return self.run_install(extra_env=env)

    def test_as_root_it_does_not_refresh(self):
        # `sudo sh install.sh` can keep HOME: a refresh would leave root-owned
        # files in the person's app settings, so it says how to run it instead.
        self.configure(self.default_data)
        shim = self.root / 'root-shim'
        shim.mkdir()
        write_executable(shim / 'id', '#!/bin/sh\necho 0\n')
        result = self.run_install(extra_env={'SETUP_LOG': str(self.setup_log), 'FAKE_REFRESH_OUTPUT': 'x'},
                                  path_dirs=[shim])
        target = self.home / '.local' / 'bin' / 'agent-archive'
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse(self.setup_log.exists())
        self.assertIn('running as root', result.stdout)
        self.assertIn(f'As yourself, without sudo, run: {target} setup --refresh', result.stdout)

    def test_a_fresh_install_runs_nothing(self):
        result = self.install()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse(self.setup_log.exists())
        self.assertIn('\nTo get started, run:\n\nagent-archive setup\n', result.stdout)

    def test_a_data_directory_without_settings_is_a_fresh_install(self):
        self.default_data.mkdir(parents=True)
        (self.default_data / 'registrations.json').write_text('[]')
        result = self.install()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse(self.setup_log.exists())

    def test_refreshes_an_existing_installation_and_prints_the_result(self):
        self.configure(self.default_data)
        result = self.install(FAKE_REFRESH_OUTPUT='refreshed Claude Code hooks and 2 skill files')
        self.assertEqual(result.returncode, 0, result.stderr)
        # Exactly `setup --refresh`, with no terminal to ask anything on.
        self.assertEqual(self.setup_log.read_text().splitlines(), ['args: setup --refresh', 'home: '])
        self.assertIn('  refreshed Claude Code hooks and 2 skill files\n', result.stdout)
        self.assertIn('agent-archive setup --refresh', result.stdout)
        self.assertNotIn('To get started', result.stdout)
        self.assertEqual(result.stderr, '')

    def test_honors_agent_archive_home(self):
        elsewhere = self.root / 'elsewhere'
        self.configure(elsewhere)
        result = self.install(AGENT_ARCHIVE_HOME=str(elsewhere))
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.setup_log.read_text().splitlines(), ['args: setup --refresh', f'home: {elsewhere}'])
        self.assertIn('  nothing to refresh\n', result.stdout)

    def test_another_installations_settings_do_not_count(self):
        # The default installation is set up, but AGENT_ARCHIVE_HOME names a
        # directory that is not: the install is fresh for that one.
        self.configure(self.default_data)
        result = self.install(AGENT_ARCHIVE_HOME=str(self.root / 'empty'))
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse(self.setup_log.exists())
        self.assertIn('\nTo get started, run:\n\nagent-archive setup\n', result.stdout)

    def test_a_failed_refresh_says_why_and_does_not_fail_the_install(self):
        self.configure(self.default_data)
        result = self.install(FAKE_REFRESH_EXIT='1', FAKE_REFRESH_OUTPUT='',
                              FAKE_REFRESH_ERROR='agent-archive: setup --refresh: setup was interrupted and needs recovery')
        target = self.home / '.local' / 'bin' / 'agent-archive'
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(target.exists())
        self.assertIn(f'✓ installed agent-archive v9.9.9 to {target}', result.stdout)
        self.assertIn('setup was interrupted and needs recovery', result.stderr)
        self.assertIn('your hooks and skills were not brought up to date', result.stderr)
        self.assertIn(f'try again with: {target} setup --refresh', result.stderr)
        # An uninstalled or never-finished setup cannot be refreshed: name setup too.
        self.assertIn(f'run: {target} setup\n', result.stderr)

    def test_a_binary_that_cannot_refresh_still_installs(self):
        # An older release has no --refresh: usage error, exit 2.
        self.configure(self.default_data)
        result = self.install(FAKE_REFRESH_EXIT='2', FAKE_REFRESH_ERROR='agent-archive: setup: unknown flag --refresh')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('unknown flag --refresh', result.stderr)

    def test_never_runs_setup_beyond_refresh(self):
        self.configure(self.default_data)
        self.install()
        self.assertEqual([line for line in self.setup_log.read_text().splitlines() if line.startswith('args:')],
                         ['args: setup --refresh'])
        self.assertNotIn('stdin: terminal', self.setup_log.read_text())


class ManualInstallGuideTest(unittest.TestCase):
    def test_checks_and_installs_only_the_selected_asset(self):
        guide = INSTALL_GUIDE.read_text()
        select = re.search(r'2\. Select the binary.*?```sh\n(.*?)\n   ```', guide, re.S)
        verify = re.search(r'3\. On macOS, check the selected binary.*?```sh\n(.*?)\n   ```', guide, re.S)
        install = re.search(r'4\. Make the selected binary.*?```sh\n(.*?)\n   ```', guide, re.S)
        self.assertIsNotNone(select)
        self.assertIsNotNone(verify)
        self.assertIsNotNone(install)
        # The signature step is macOS only: Linux binaries are unsigned.
        commands = {
            'darwin': select.group(1) + '\n' + verify.group(1) + '\n' + install.group(1) + '\n',
            'linux': select.group(1) + '\n' + install.group(1) + '\n',
        }
        systems = (('Darwin', 'arm64', 'darwin', 'arm64'), ('Darwin', 'x86_64', 'darwin', 'amd64'),
                   ('Linux', 'aarch64', 'linux', 'arm64'), ('Linux', 'x86_64', 'linux', 'amd64'))
        for system, machine, goos, expected in systems:
            for checksums in ('both', 'selected_missing', 'selected_bad'):
                with self.subTest(system=system, machine=machine, checksums=checksums), \
                        tempfile.TemporaryDirectory() as root:
                    root = Path(root)
                    home = root / 'home'
                    home.mkdir()
                    shims = root / 'shims'
                    shims.mkdir()
                    write_executable(shims / 'uname',
                                     f'#!/bin/sh\ncase "$1" in -s) echo {system} ;; *) echo {machine} ;; esac\n')
                    write_executable(shims / 'codesign',
                                     '#!/bin/sh\ncase "$1" in -dv) echo TeamIdentifier=568CGRV32C ;; esac\n')
                    for os_name in ('darwin', 'linux'):
                        for arch in ('arm64', 'amd64'):
                            (root / f'agent-archive-{os_name}-{arch}').write_text(arch)
                    other = 'amd64' if expected == 'arm64' else 'arm64'
                    lines = []
                    for os_name in ('darwin', 'linux'):
                        for arch in ('arm64', 'amd64'):
                            selected = (os_name, arch) == (goos, expected)
                            if selected and checksums == 'selected_missing':
                                continue
                            digest = hashlib.sha256(arch.encode()).hexdigest()
                            if selected and checksums == 'selected_bad':
                                digest = '0' * 64
                            lines.append(f'{digest}  agent-archive-{os_name}-{arch}\n')
                    (root / 'SHA256SUMS').write_text(''.join(lines))

                    result = subprocess.run(
                        ['sh', '-e', '-c', commands[goos]], cwd=root,
                        env={'HOME': str(home), 'PATH': f'{shims}:/usr/bin:/bin'},
                        capture_output=True, text=True,
                    )
                    if checksums == 'both':
                        self.assertEqual(result.returncode, 0, result.stderr)
                        self.assertEqual((home / '.local/bin/agent-archive').read_text(), expected)
                    else:
                        self.assertNotEqual(result.returncode, 0)
                        self.assertFalse((home / '.local/bin/agent-archive').exists())
                    self.assertTrue((root / f'agent-archive-{goos}-{other}').exists())


if __name__ == '__main__':
    unittest.main()
