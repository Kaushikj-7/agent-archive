"""The four release binaries must be checksummed and published consistently.

Two things the release workflow cannot show without a tag: that SHA256SUMS is
written the same way with sha256sum (Linux) or shasum (macOS), and that every
place the workflow names the released files names the same ones.
"""
import hashlib
import os
from pathlib import Path
import platform
import re
import shutil
import stat
import struct
import subprocess
import tempfile
import unittest

SCRIPTS = Path(__file__).resolve().parent
ROOT = SCRIPTS.parent
WRITE_CHECKSUMS = SCRIPTS / 'write-checksums.sh'
RELEASE_YML = ROOT / '.github' / 'workflows' / 'release.yml'
BASH = shutil.which('bash')

BINARIES = [
    'agent-archive-darwin-amd64',
    'agent-archive-darwin-arm64',
    'agent-archive-linux-amd64',
    'agent-archive-linux-arm64',
]


def make_dist(directory, names=BINARIES):
    directory.mkdir(parents=True, exist_ok=True)
    for name in names:
        (directory / name).write_bytes(f'synthetic {name}\n'.encode())


def make_shim(directory, name, digest, skip_args=0):
    """A stand-in checksum tool printing digest for each file it is given."""
    directory.mkdir(parents=True, exist_ok=True)
    path = directory / name
    path.write_text(f'#!/bin/sh\nshift {skip_args}\nfor f in "$@"; do echo "{digest}  $f"; done\n')
    path.chmod(path.stat().st_mode | stat.S_IEXEC)


def run_write_checksums(dist, path=None):
    """Runs write-checksums.sh; a given path replaces PATH entirely."""
    env = {'PATH': path} if path is not None else None
    return subprocess.run([BASH, str(WRITE_CHECKSUMS), str(dist)], env=env, capture_output=True, text=True)


class WriteChecksumsTest(unittest.TestCase):
    def setUp(self):
        self.root = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, self.root, True)
        self.dist = self.root / 'dist'
        make_dist(self.dist)

    def sums(self):
        return (self.dist / 'SHA256SUMS').read_text()

    def test_lists_all_four_in_order_in_the_format_install_sh_reads(self):
        result = run_write_checksums(self.dist)
        self.assertEqual(result.returncode, 0, result.stderr)
        expected = ''.join(
            f'{hashlib.sha256((self.dist / name).read_bytes()).hexdigest()}  {name}\n' for name in BINARIES
        )
        self.assertEqual(self.sums(), expected)

    def test_prefers_sha256sum_and_falls_back_to_shasum(self):
        shims = self.root / 'shims'
        make_shim(shims, 'shasum', 'b' * 64, skip_args=2)  # shasum -a 256 FILE...
        result = run_write_checksums(self.dist, path=str(shims))
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.sums(), ''.join(f'{"b" * 64}  {name}\n' for name in BINARIES))

        make_shim(shims, 'sha256sum', 'a' * 64)
        result = run_write_checksums(self.dist, path=str(shims))
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.sums(), ''.join(f'{"a" * 64}  {name}\n' for name in BINARIES))

    def test_fails_without_a_checksum_tool(self):
        empty = self.root / 'empty'
        empty.mkdir()
        result = run_write_checksums(self.dist, path=str(empty))
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('sha256sum or shasum', result.stderr)
        self.assertFalse((self.dist / 'SHA256SUMS').exists())

    def test_fails_when_a_binary_is_missing(self):
        for name in BINARIES:
            with self.subTest(missing=name):
                dist = self.root / f'without-{name}'
                make_dist(dist, [other for other in BINARIES if other != name])
                result = run_write_checksums(dist)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn(name, result.stderr)
                self.assertFalse((dist / 'SHA256SUMS').exists())

    def test_names_the_same_binaries_this_test_expects(self):
        script = WRITE_CHECKSUMS.read_text()
        listed = re.search(r'assets=\(\n(.*?)\n\)', script, re.S).group(1).split()
        self.assertEqual(listed, BINARIES)


VERIFY = SCRIPTS / 'verify-linux-release.sh'
VERSION = 'v0.0.0-test'
AMD64 = 'agent-archive-linux-amd64'
ARM64 = 'agent-archive-linux-arm64'
# Where the script runs the amd64 binary instead of only reading it.
NATIVE_AMD64 = platform.system() == 'Linux' and platform.machine() == 'x86_64'
MACHINES = {'amd64': 0x3E, 'arm64': 0xB7}


def elf_header(arch):
    """A 64-byte ELF64 header with no program headers: a static file to readelf and file."""
    ident = b'\x7fELF\x02\x01\x01' + bytes(9)
    return ident + struct.pack('<HHIQQQIHHHHHH', 2, MACHINES[arch], 1, 0, 0, 0, 0, 64, 56, 0, 64, 0, 0)


def embedding(version, arch=None):
    """File contents that carry version on a line of its own (as strings sees it)."""
    return (elf_header(arch) if arch else b'') + b'\n' + version.encode() + b'\n'


def native_script(version):
    """A stand-in amd64 binary for the native run: prints version, and embeds it too."""
    return f'#!/bin/sh\necho {version}\nexit 0\n{version}\n'.encode()


class VerifyLinuxReleaseTest(unittest.TestCase):
    """verify-linux-release.sh must fail on anything but the right static binaries."""

    def setUp(self):
        self.root = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, self.root, True)
        self.dist = self.root / 'dist'
        self.dist.mkdir()
        self.shims = self.root / 'shims'
        self.shims.mkdir()

    # -- fixtures -------------------------------------------------------

    def fake_readelf(self, header_ok=True, dynamic=False):
        """A readelf that says ELF, machine by the file name, and INTERP if dynamic."""
        path = self.shims / 'readelf'
        path.write_text(
            '#!/bin/sh\n'
            + ('' if header_ok else 'echo "readelf: Error: Not an ELF file" >&2; exit 1\n')
            + 'if [ "$1" = -h ]; then\n'
            '  case "$2" in\n'
            '    *amd64) echo "  Machine:    Advanced Micro Devices X86-64" ;;\n'
            '    *) echo "  Machine:    AArch64" ;;\n'
            '  esac\n'
            '  exit 0\n'
            'fi\n'
            + ('echo "  INTERP  0x0 0x0 0x0 0x1c 0x1c R 0x1"\n' if dynamic else 'echo "  LOAD"\n')
        )
        path.chmod(path.stat().st_mode | stat.S_IEXEC)

    def write_binaries(self, amd64, arm64):
        """Writes both files; each is bytes, or None to leave it out."""
        for name, contents in ((AMD64, amd64), (ARM64, arm64)):
            if contents is not None:
                (self.dist / name).write_bytes(contents)
                (self.dist / name).chmod(0o755)

    def good(self):
        """(amd64, arm64) contents that pass on this machine, given a readelf shim."""
        amd64 = native_script(VERSION) if NATIVE_AMD64 else embedding(VERSION)
        return amd64, embedding(VERSION)

    def run_verify(self, path=None, **env):
        base = {'PATH': path or f'{self.shims}:/usr/bin:/bin', 'VERSION': VERSION}
        base.update(env)
        return subprocess.run([BASH, str(VERIFY), str(self.dist)], env=base, capture_output=True, text=True)

    def tools_only(self, *names):
        """A PATH holding only these tools, so readelf and file are both absent."""
        bindir = self.root / 'tools'
        bindir.mkdir(exist_ok=True)
        for name in names:
            found = shutil.which(name, path='/usr/bin:/bin')
            if found and not (bindir / name).exists():
                (bindir / name).symlink_to(found)
        return str(bindir)

    # -- the good case, so the failures below mean something -------------

    def test_accepts_matching_static_binaries(self):
        self.fake_readelf()
        self.write_binaries(*self.good())
        result = self.run_verify()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn(f'version {VERSION}', result.stdout)

    # -- files and versions ----------------------------------------------

    def test_fails_when_a_binary_is_missing(self):
        self.fake_readelf()
        for missing in (AMD64, ARM64):
            with self.subTest(missing=missing):
                amd64, arm64 = self.good()
                self.write_binaries(amd64, arm64)
                (self.dist / missing).unlink()
                result = self.run_verify()
                self.assertNotEqual(result.returncode, 0, result.stdout)
                self.assertIn('is missing', result.stderr)

    def test_fails_when_the_arm64_binary_embeds_another_version(self):
        self.fake_readelf()
        for kind, contents in {
            'different version': embedding('v9.9.9'),
            'version only inside a longer line': b'\nprefix-' + VERSION.encode() + b'-suffix\n',
            'no version': b'\nnothing to see\n',
        }.items():
            with self.subTest(kind=kind):
                amd64, _ = self.good()
                self.write_binaries(amd64, contents)
                result = self.run_verify()
                self.assertNotEqual(result.returncode, 0, result.stdout)
                self.assertIn('does not embed version', result.stderr)

    def test_fails_when_the_amd64_binary_has_another_version(self):
        self.fake_readelf()
        _, arm64 = self.good()
        for kind, contents in {
            'different version': native_script('v9.9.9') if NATIVE_AMD64 else embedding('v9.9.9'),
            'version only inside a longer line': (
                native_script(VERSION + '-extra') if NATIVE_AMD64 else b'\nprefix-' + VERSION.encode() + b'\n'
            ),
        }.items():
            with self.subTest(kind=kind):
                self.write_binaries(contents, arm64)
                result = self.run_verify()
                self.assertNotEqual(result.returncode, 0, result.stdout)
                self.assertRegex(result.stderr, 'reported|does not embed')

    @unittest.skipUnless(NATIVE_AMD64, 'the amd64 binary runs only on x86-64 Linux')
    def test_fails_when_the_native_run_disagrees_with_the_embedded_version(self):
        # The file embeds the right version but prints another one: only the
        # native run can tell.
        self.fake_readelf()
        _, arm64 = self.good()
        lying = f'#!/bin/sh\necho v9.9.9\nexit 0\n{VERSION}\n'.encode()
        self.write_binaries(lying, arm64)
        result = self.run_verify()
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assertIn('reported', result.stderr)

    # -- what kind of file --------------------------------------------------

    def test_rejects_a_file_that_is_not_elf_when_readelf_exists(self):
        # readelf prints nothing on stdout for these, which looks like "no
        # INTERP header" and so like a static binary unless checked first.
        self.fake_readelf(header_ok=False)
        for kind, contents in {
            'Mach-O': b'\xcf\xfa\xed\xfe' + bytes(64),
            'script': b'#!/bin/sh\necho hi\n',
        }.items():
            with self.subTest(kind=kind):
                self.write_binaries(contents + b'\n' + VERSION.encode() + b'\n', contents + b'\n' + VERSION.encode() + b'\n')
                result = self.run_verify()
                self.assertNotEqual(result.returncode, 0, result.stdout)
                self.assertIn('not a statically linked ELF', result.stderr)

    def test_rejects_a_dynamically_linked_elf(self):
        self.fake_readelf(dynamic=True)
        self.write_binaries(*self.good())
        result = self.run_verify()
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assertIn('not a statically linked ELF', result.stderr)

    def test_rejects_binaries_built_for_the_other_architecture(self):
        self.fake_readelf()
        # The amd64 name on a file the shim reports as AArch64, and back.
        for swapped in (AMD64, ARM64):
            with self.subTest(swapped=swapped):
                self.write_binaries(*self.good())
                path = self.shims / 'readelf'
                other = 'AArch64' if swapped == AMD64 else 'Advanced Micro Devices X86-64'
                path.write_text(
                    '#!/bin/sh\nif [ "$1" = -h ]; then\n'
                    f'  case "$2" in *{swapped}) echo "  Machine:    {other}" ;;\n'
                    '    *amd64) echo "  Machine:    Advanced Micro Devices X86-64" ;;\n'
                    '    *) echo "  Machine:    AArch64" ;;\n  esac\n  exit 0\nfi\necho "  LOAD"\n'
                )
                result = self.run_verify()
                self.assertNotEqual(result.returncode, 0, result.stdout)
                self.assertRegex(result.stderr, r'is not a linux/(amd64|arm64) binary')

    def test_real_tools_reject_swapped_and_non_elf_files(self):
        # No shims: whichever of readelf or file this machine has.
        if not (shutil.which('readelf') or shutil.which('file')):
            self.skipTest('needs readelf or file')
        real_path = os.environ['PATH']
        for kind, amd64, arm64 in (
            ('swapped', embedding(VERSION, 'arm64'), embedding(VERSION, 'amd64')),
            ('text', b'not an ELF file\n' + VERSION.encode() + b'\n', b'not an ELF file\n' + VERSION.encode() + b'\n'),
        ):
            with self.subTest(kind=kind):
                self.write_binaries(amd64, arm64)
                result = self.run_verify(path=real_path)
                self.assertNotEqual(result.returncode, 0, result.stdout)
                self.assertRegex(result.stderr, 'not a linux/|not a statically linked ELF')

    # -- no tool to inspect files ---------------------------------------------

    def test_without_readelf_or_file_a_local_run_passes_with_a_notice_and_ci_fails(self):
        tools = self.tools_only('uname', 'grep', 'strings')
        self.write_binaries(*self.good())
        local = subprocess.run(
            [BASH, str(VERIFY), str(self.dist)], env={'PATH': tools, 'VERSION': VERSION}, capture_output=True, text=True
        )
        self.assertEqual(local.returncode, 0, local.stderr)
        self.assertIn('notice: neither readelf nor file', local.stderr)

        ci = subprocess.run(
            [BASH, str(VERIFY), str(self.dist)],
            env={'PATH': tools, 'VERSION': VERSION, 'CI': 'true'},
            capture_output=True,
            text=True,
        )
        self.assertNotEqual(ci.returncode, 0, ci.stdout)
        self.assertIn('cannot be checked', ci.stderr)

    def test_an_empty_ci_variable_counts_as_a_local_run(self):
        tools = self.tools_only('uname', 'grep', 'strings')
        self.write_binaries(*self.good())
        result = subprocess.run(
            [BASH, str(VERIFY), str(self.dist)],
            env={'PATH': tools, 'VERSION': VERSION, 'CI': ''},
            capture_output=True,
            text=True,
        )
        self.assertEqual(result.returncode, 0, result.stderr)


def job_text(workflow, job):
    """The text of one top-level job in the workflow."""
    match = re.search(rf'^  {re.escape(job)}:\n(.*?)(?=^  [\w-]+:\n|\Z)', workflow, re.S | re.M)
    if not match:
        raise AssertionError(f'no job {job!r} in the workflow')
    return match.group(1)


def block_lines(text, header):
    """The indented lines of the YAML block scalar that follows header."""
    lines = text.splitlines()
    for index, line in enumerate(lines):
        if line.strip() == header:
            indent = len(line) - len(line.lstrip())
            body = []
            for following in lines[index + 1:]:
                if following.strip() and len(following) - len(following.lstrip()) <= indent:
                    break
                if following.strip():
                    body.append(following.strip())
            return body
    raise AssertionError(f'no {header!r} in the text')


def strip_comment(line):
    return re.sub(r'\s+#.*$', '', line).rstrip()


def scalar(text):
    text = text.strip()
    if len(text) >= 2 and text[0] == text[-1] and text[0] in '\'"':
        return text[1:-1]
    return {} if text == '{}' else text


def parse_block(lines):
    """A nested dict from the mappings and lists of a small YAML block.

    Enough for the workflow's `on:` and `permissions:` blocks (PyYAML is not
    guaranteed to be installed): `key: value`, `key:` with nested keys or
    `- item` lines, and comments.
    """
    root = {}
    # Each entry: the indent of a key, the mapping holding it, and the key.
    stack = [(-1, {None: root}, None)]
    for raw in lines:
        line = strip_comment(raw)
        if not line.strip() or line.strip().startswith('#'):
            continue
        indent = len(line) - len(line.lstrip())
        text = line.strip()
        while stack[-1][0] >= indent:
            stack.pop()
        _, holder, holder_key = stack[-1]
        container = holder[holder_key]
        if text.startswith('- '):
            if container == {}:
                container = holder[holder_key] = []
            container.append(scalar(text[2:]))
            continue
        key, _, value = text.partition(':')
        container[key] = scalar(value) if value.strip() else {}
        stack.append((indent, container, key))
    return root


def top_level_block(workflow, key):
    """A top-level key's inline value and the lines nested under it."""
    lines = workflow.splitlines()
    for index, line in enumerate(lines):
        if line.startswith(f'{key}:'):
            body = []
            for following in lines[index + 1:]:
                if following.strip() and not following.startswith((' ', '#')):
                    break
                body.append(following)
            return strip_comment(line)[len(key) + 1:].strip(), body
    raise AssertionError(f'no top-level {key!r} in the workflow')


def job_permissions(job):
    """The job's own `permissions:` mapping."""
    lines = job.splitlines()
    for index, line in enumerate(lines):
        if line == '    permissions:':
            body = []
            for following in lines[index + 1:]:
                if following.strip() and len(following) - len(following.lstrip()) <= 4:
                    break
                body.append(following)
            return parse_block(body)
    raise AssertionError('job has no permissions block')


def steps(job):
    """The text of each step of a job, in order."""
    parts = re.split(r'^      - ', job, flags=re.M)
    return ['      - ' + part for part in parts[1:]]


BLOCK_SCALAR = re.compile(r'^[|>][-+0-9]*\s*(#.*)?$')


def run_bodies(workflow):
    """(line index, body line count, text) of each `run:`: the value and every deeper line.

    Covers a block scalar (`run: |`, `|-`, `>+`, with a trailing comment) and
    a plain or quoted scalar continued over several lines.
    """
    lines = workflow.splitlines()
    found = []
    for index, line in enumerate(lines):
        match = re.match(r'^(\s*)(?:- )?run:(.*)$', line)
        if not match:
            continue
        indent = len(match.group(1))
        rest = match.group(2).strip()
        body = []
        for following in lines[index + 1:]:
            if following.strip() and len(following) - len(following.lstrip()) <= indent:
                break
            body.append(following)
        text = '\n'.join(body) if BLOCK_SCALAR.match(rest) else '\n'.join([rest] + body)
        found.append((index, len(body), text))
    return found


def run_texts(workflow):
    """The text of every `run:` in the workflow."""
    return [text for _, _, text in run_bodies(workflow)]


def script_free_lines(workflow):
    """The workflow's lines outside `run:` bodies and comments, without comments."""
    lines = workflow.splitlines()
    skip = set()
    for index, count, _ in run_bodies(workflow):
        skip.update(range(index + 1, index + 1 + count))
    return [
        strip_comment(line)
        for number, line in enumerate(lines)
        if number not in skip and line.strip() and not line.strip().startswith('#')
    ]


def code_lines(text):
    """text without comment-only lines and trailing comments."""
    return '\n'.join(
        strip_comment(line) for line in text.splitlines() if line.strip() and not line.strip().startswith('#')
    )


class ReleaseWorkflowTest(unittest.TestCase):
    def setUp(self):
        self.workflow = RELEASE_YML.read_text()
        self.publish = job_text(self.workflow, 'publish')
        self.dist = {f'dist/{name}' for name in BINARIES}

    def step(self, name):
        match = re.search(rf'^      - name: {re.escape(name)}\n(.*?)(?=^      - |\Z)', self.publish, re.S | re.M)
        self.assertIsNotNone(match, f'no step {name!r} in publish')
        return match.group(1)

    def test_attestation_subjects_are_exactly_the_binaries_and_candidate_manifest(self):
        attest = self.publish[self.publish.index('actions/attest-build-provenance@'):]
        subjects = block_lines(attest, 'subject-path: |')
        self.assertEqual(len(subjects), len(set(subjects)), subjects)
        self.assertEqual(set(subjects), self.dist | {'dist/release-candidate.json'})

    def test_uploaded_and_released_assets_are_the_binaries_checksums_and_manifest(self):
        upload = self.publish[self.publish.index('name: agent-archive-${{ github.ref_name }}'):]
        uploaded = block_lines(upload, 'path: |')
        self.assertEqual(len(uploaded), len(set(uploaded)), uploaded)
        self.assertEqual(set(uploaded), self.dist | {'dist/SHA256SUMS', 'dist/release-candidate.json'})

        self.assertIn('python3 scripts/release_candidate.py stage', self.publish)
        from release_candidate import ASSETS
        self.assertEqual(set(ASSETS), set(BINARIES) | {'SHA256SUMS', 'release-candidate.json'})

    def test_build_jobs_together_produce_the_unsigned_binaries(self):
        produced = []
        for job in ('build', 'build-linux'):
            produced += block_lines(job_text(self.workflow, job), 'path: |')
        self.assertEqual(sorted(produced), sorted(self.dist))

    def test_builds_require_verified_full_tag_coverage_before_build_and_retain_raw_evidence(self):
        for job, suite in [('build', 'macos'), ('build-linux', 'linux')]:
            with self.subTest(job=job):
                text = job_text(self.workflow, job)
                bodies = steps(text)
                test = next(body for body in bodies if 'name: Test\n' in body)
                plan = 'python3 -B scripts/run_ci_tier.py --plan --tier full --output release-tier-results/plan.json'
                run = f'python3 -B scripts/run_ci_tier.py --tier full --suite {suite} --seconds 2400 --output release-tier-results'
                verify = f'python3 -B scripts/run_ci_tier.py --verify --tier full --suite {suite} --extra-plan release-tier-results/plan.json --output release-tier-results'
                self.assertLess(test.index(plan), test.index(run))
                self.assertLess(test.index(run), test.index(verify))
                self.assertIn('go vet ./...', test)
                self.assertNotIn('go test ', test)
                for script in ('release_signing', 'release_assets', 'release_candidate', 'install', 'purge_recipe'):
                    self.assertIn(f'python3 scripts/test_{script}.py', test)
                evidence = next(body for body in bodies if 'name: Preserve source-bound release validation evidence' in body)
                self.assertIn('if: always()', evidence)
                self.assertIn(f'name: release-validation-{suite}', evidence)
                self.assertIn('path: release-tier-results', evidence)
                self.assertIn('if-no-files-found: error', evidence)
                build = next(body for body in bodies if 'name: Build\n' in body)
                self.assertLess(bodies.index(test), bodies.index(evidence))
                self.assertLess(bodies.index(evidence), bodies.index(build))
                # Retain the existing default job bound rather than increasing it.
                self.assertNotIn('timeout-minutes:', text)

    def linux_prerequisites(self):
        bodies = steps(job_text(self.workflow, 'build-linux'))
        matching = [body for body in bodies if 'name: Install purge prerequisites\n' in body]
        self.assertEqual(len(matching), 1)
        return bodies, matching[0]

    def test_linux_provisions_purge_tools_after_tag_guard_before_go_validation(self):
        bodies, prerequisites = self.linux_prerequisites()
        guard = next(body for body in bodies if 'name: Require a vX.Y.Z tag' in body)
        setup = next(body for body in bodies if 'actions/setup-go@' in body)
        test = next(body for body in bodies if 'name: Test\n' in body)
        self.assertLess(bodies.index(guard), bodies.index(prerequisites))
        self.assertLess(bodies.index(prerequisites), bodies.index(setup))
        self.assertLess(bodies.index(setup), bodies.index(test))
        self.assertNotRegex(prerequisites, r'(?m)^\s+(if|continue-on-error):')

    def test_linux_prerequisite_bootstrap_installs_tools_and_fails_if_any_is_missing(self):
        _, prerequisites = self.linux_prerequisites()
        script = '\n'.join(block_lines(prerequisites, 'run: |'))
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            log = root / 'sudo.log'
            sudo = root / 'sudo'
            sudo.write_text('#!/bin/sh\nprintf "%s\\n" "$*" >> "$BOOTSTRAP_LOG"\n')
            sudo.chmod(0o755)
            for tool in ('jq', 'bash', 'zsh'):
                (root / tool).symlink_to('/bin/sh')

            def run():
                log.unlink(missing_ok=True)
                return subprocess.run(
                    [BASH, '-e', '-o', 'pipefail', '-c', script],
                    env={'PATH': str(root), 'BOOTSTRAP_LOG': str(log)},
                    capture_output=True, text=True,
                )

            result = run()
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(log.read_text().splitlines(), [
                'apt-get update', 'apt-get install --yes jq zsh',
            ])
            # A successful package-manager exit is insufficient: the tools
            # must actually be available to the subsequent required matrix.
            for tool in ('jq', 'bash', 'zsh'):
                with self.subTest(missing=tool):
                    (root / tool).unlink()
                    result = run()
                    self.assertNotEqual(result.returncode, 0, result.stdout)
                    self.assertIn(f'required purge prerequisite {tool} is missing', result.stderr)
                    (root / tool).symlink_to('/bin/sh')

    def test_only_the_macos_binaries_are_signed_and_notarized(self):
        for name in ('Codesign', 'Notarize'):
            with self.subTest(step=name):
                text = self.step(name)
                self.assertIn('darwin', text)
                self.assertNotIn('linux', text)

    def test_publish_waits_for_both_builds_and_downloads_both(self):
        self.assertRegex(self.publish, r'needs: \[preflight, build, build-linux\]')
        self.assertRegex(self.publish, r'name: unsigned\n\s+path: dist')
        self.assertRegex(self.publish, r'name: linux\n\s+path: dist')

    def test_every_build_job_keeps_the_tag_and_main_guard(self):
        for job in ('build', 'build-linux'):
            with self.subTest(job=job):
                text = job_text(self.workflow, job)
                self.assertIn(r'^v[0-9]+\.[0-9]+\.[0-9]+$', text)
                self.assertIn('git merge-base --is-ancestor "$GITHUB_SHA" origin/main', text)
                self.assertIn('VERSION: ${{ github.ref_name }}', text)

    def test_the_workflow_still_runs_only_on_release_tags(self):
        _, body = top_level_block(self.workflow, 'on')
        self.assertEqual(parse_block(body), {'push': {'tags': ['v*.*.*']}})

    def test_workflow_and_job_permissions_are_least_privilege(self):
        inline, _ = top_level_block(self.workflow, 'permissions')
        self.assertEqual(inline, '{}')
        read = {'contents': 'read'}
        self.assertEqual(job_permissions(job_text(self.workflow, 'build')), read)
        self.assertEqual(job_permissions(job_text(self.workflow, 'build-linux')), read)
        self.assertEqual(
            job_permissions(self.publish),
            {'contents': 'write', 'id-token': 'write', 'attestations': 'write'},
        )

    def test_publish_is_a_release_environment_job_only_for_a_missing_candidate(self):
        self.assertRegex(self.publish, r'(?m)^    environment: release$')
        self.assertIn("if: needs.preflight.outputs.exists == 'false'", self.publish)
        # The one step that runs after a failure only removes the keychain.
        for text in steps(self.publish):
            if 'always()' in text:
                self.assertIn('name: Clean up signing keychain', text)

    def test_checksums_are_written_after_signing_and_before_attestation(self):
        texts = steps(self.publish)

        def index(marker):
            found = [i for i, text in enumerate(texts) if marker in text]
            self.assertEqual(len(found), 1, f'{marker!r} should appear in exactly one publish step')
            return found[0]

        checksums = index('name: Recompute checksums after signing')
        self.assertIn('./scripts/write-checksums.sh dist', texts[checksums])
        self.assertNotIn('shasum', texts[checksums])
        self.assertGreater(checksums, index('name: Codesign'))
        self.assertGreater(checksums, index('name: Notarize'))
        self.assertLess(checksums, index('actions/attest-build-provenance@'))

    def test_every_action_is_pinned_to_a_full_commit(self):
        used = re.findall(r'(?m)^\s*(?:- )?uses: (\S+)', self.workflow)
        self.assertGreaterEqual(len(used), 10)
        for action in used:
            with self.subTest(action=action):
                self.assertRegex(action, r'@[0-9a-f]{40}$')

    def test_checkouts_keep_no_credentials_and_go_has_no_module_cache(self):
        for job in ('build', 'build-linux', 'publish'):
            with self.subTest(job=job):
                text = job_text(self.workflow, job)
                self.assertEqual(text.count('actions/checkout@'), 1)
                self.assertEqual(text.count('persist-credentials: false'), 1)
                self.assertNotIn('persist-credentials: true', text)
        for job in ('build', 'build-linux'):
            with self.subTest(job=job):
                text = job_text(self.workflow, job)
                self.assertIn('cache: false', text)
                self.assertNotIn('cache: true', text)

    def test_build_jobs_see_no_secrets_or_variables(self):
        for job in ('build', 'build-linux'):
            with self.subTest(job=job):
                text = code_lines(job_text(self.workflow, job))
                self.assertNotRegex(text, r'\bsecrets\b')
                self.assertNotRegex(text, r'\bvars\b')

    def test_artifacts_hand_off_under_matching_names_into_dist(self):
        def uploads(job):
            return re.findall(r'upload-artifact@\S+[^\n]*\n\s+with:\n\s+name: (\S+)', job_text(self.workflow, job))

        self.assertEqual(uploads('build'), ['release-validation-macos', 'unsigned'])
        self.assertEqual(uploads('build-linux'), ['release-validation-linux', 'linux'])
        downloads = re.findall(
            r'download-artifact@\S+[^\n]*\n\s+with:\n\s+name: (\S+)\n\s+path: (\S+)\n', self.publish
        )
        self.assertEqual(sorted(downloads), [('linux', 'dist'), ('unsigned', 'dist')])

    def test_each_platform_builds_on_its_own_runner_with_its_own_checks(self):
        linux = job_text(self.workflow, 'build-linux')
        self.assertRegex(linux, r'(?m)^    runs-on: ubuntu-24\.04$')
        self.assertRegex(linux, r'(?m)^        run: \./scripts/build-release\.sh linux$')
        self.assertNotIn('build-release.sh darwin', linux)
        self.assertRegex(linux, r'(?m)^        run: \./scripts/verify-linux-release\.sh$')
        darwin = job_text(self.workflow, 'build')
        self.assertRegex(darwin, r'(?m)^    runs-on: macos-14$')
        self.assertRegex(darwin, r'(?m)^        run: \./scripts/build-release\.sh darwin$')
        self.assertNotIn('build-release.sh linux', darwin)

    def test_release_is_created_for_an_existing_tag(self):
        from release_candidate import GitHub
        from unittest.mock import patch
        with patch('release_candidate.run') as command:
            GitHub('wangjohn/agent-archive').create('v1.2.3', Path('dist'))
        self.assertIn('--verify-tag', command.call_args.args[0])

    def test_no_expression_is_expanded_into_script_text(self):
        texts = run_texts(self.workflow)
        self.assertGreaterEqual(len(texts), 8)
        for text in texts:
            self.assertNotIn('${{', text)

    def test_no_flow_style_mappings_hide_a_step_from_these_checks(self):
        # `- {run: ..., env: ...}` would put a script or an expression where
        # the line-based checks above do not look. Outside run bodies the
        # only braces allowed are expressions and the literal `{}`.
        for line in script_free_lines(self.workflow):
            with self.subTest(line=line):
                bare = re.sub(r'\$\{\{.*?\}\}', '', line).replace('{}', '')
                self.assertNotIn('{', bare)
                self.assertNotIn('}', bare)

    def test_run_scalars_are_read_in_every_yaml_form(self):
        # The parser behind the script-text check, on the forms it must see.
        cases = {
            'block': 'a:\n  - run: |\n      echo ${{ x }}\n',
            'block with chomping and comment': 'a:\n  - run: |- # why\n      echo ${{ x }}\n',
            'folded': 'a:\n  - run: >\n      echo ${{ x }}\n',
            'plain over lines': 'a:\n  - run: echo\n      ${{ x }}\n',
            'quoted over lines': 'a:\n  - run: "echo\n      ${{ x }}"\n',
            'single line': 'a:\n  - run: echo ${{ x }}\n',
        }
        for name, text in cases.items():
            with self.subTest(form=name):
                self.assertTrue(any('${{' in body for body in run_texts(text)), run_texts(text))

    def test_the_workflow_has_preflight_and_the_three_build_publish_jobs(self):
        jobs = re.findall(r'(?m)^  ([\w-]+):$', self.workflow[self.workflow.index('\njobs:\n'):])
        self.assertEqual(jobs, ['preflight', 'build', 'build-linux', 'publish'])

    def test_release_critical_steps_are_not_skippable_or_allowed_to_fail(self):
        self.assertNotIn('continue-on-error', self.workflow)
        # Conditional steps only clean up or retain diagnostic evidence.
        for job in ('build', 'build-linux', 'publish'):
            for text in steps(job_text(self.workflow, job)):
                if re.search(r'(?m)^        if:', text):
                    with self.subTest(job=job):
                        if job == 'publish':
                            self.assertIn('name: Clean up signing keychain', text)
                        else:
                            self.assertIn('name: Preserve source-bound release validation evidence', text)
                            self.assertIn('if: always()', text)
                            self.assertNotIn('run:', text)
        for job, names in {
            'publish': ('Codesign', 'Notarize', 'Recompute checksums after signing'),
            'build-linux': ('Verify build reports the release version', 'Build', 'Test'),
            'build': ('Verify build reports the release version', 'Build', 'Test'),
        }.items():
            texts = steps(job_text(self.workflow, job))
            for name in names:
                with self.subTest(job=job, step=name):
                    matching = [text for text in texts if f'- name: {name}\n' in text]
                    self.assertEqual(len(matching), 1)
                    self.assertNotRegex(matching[0], r'(?m)^\s+(if|continue-on-error):')

    def test_version_comes_only_from_the_tag_at_job_level(self):
        for job in ('build', 'build-linux', 'publish'):
            with self.subTest(job=job):
                head, _, body = job_text(self.workflow, job).partition('    steps:\n')
                self.assertRegex(head, r'(?m)^      VERSION: \$\{\{ github\.ref_name \}\}$')
                self.assertNotRegex(body, r'(?m)^\s*VERSION:')
                self.assertNotIn('GITHUB_ENV', body)

    def test_checkouts_use_the_tagged_commit(self):
        for job in ('build', 'build-linux', 'publish'):
            with self.subTest(job=job):
                checkout = [t for t in steps(job_text(self.workflow, job)) if 'actions/checkout@' in t]
                self.assertEqual(len(checkout), 1)
                self.assertNotRegex(checkout[0], r'(?m)^\s+(ref|repository|token):')

    def test_only_the_expected_actions_are_used(self):
        used = set(re.findall(r'(?m)^\s*(?:- )?uses: ([^@\s]+)@', self.workflow))
        self.assertEqual(
            used,
            {
                'actions/checkout',
                'actions/setup-go',
                'actions/upload-artifact',
                'actions/download-artifact',
                'actions/attest-build-provenance',
            },
        )
        for job in ('build', 'build-linux'):
            with self.subTest(job=job):
                self.assertNotIn('actions/cache', job_text(self.workflow, job))


if __name__ == '__main__':
    unittest.main()
