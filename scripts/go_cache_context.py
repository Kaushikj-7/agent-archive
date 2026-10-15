#!/usr/bin/env python3
"""PR worker cache identity and controlled paths; never cache verification evidence.

Integration uses scope_native_verify's tracked-byte/mode/symlink proof helpers.
Outputs contain hashes and safe identifiers, not arbitrary environment values.
"""
from dataclasses import dataclass
import hashlib
import json
import os
from pathlib import Path
import re
import shlex
import subprocess
import shutil
import stat
import tempfile

import scope_native_verify as scope

SCHEMA = 'aa-pr-go-v2'
GO_FIELDS = ('GOVERSION GOOS GOARCH GOHOSTOS GOHOSTARCH GOAMD64 GOARM64 '
             'CGO_ENABLED GOEXPERIMENT GOFLAGS GOTOOLCHAIN CC CXX '
             'CGO_CFLAGS CGO_CPPFLAGS CGO_CXXFLAGS CGO_LDFLAGS GOWORK GOROOT GOTOOLDIR '
             'GOENV GOFIPS140 GODEBUG GOCACHEPROG GOARM GO386 GOMIPS GOMIPS64 GOPPC64 GORISCV64 GOWASM').split()
SHA = re.compile(r'[0-9a-f]{40}')
# One policy identity covers every accelerator consumer's immutable tools and
# supported compilation modes; Go still keys each actual action by its inputs.
TOOL_PINS = {
    'golangci-lint': ['github.com/golangci/golangci-lint/v2', 'v2.14.0'],
    'deadcode': ['golang.org/x/tools', 'v0.50.0'],
    'git-2.30.2': '94f6e3e283f2adfc518b39cfc39291f1c2832ad0',
    'git-2.39.2': 'cbf04937d5b9fcf0a76c28f69e6294e9e3ecd7e6',
    'shared': '6f799f72befb925d10f2ef01a7c9a46725fa714d',
}
COMPILATION_MODES = {
    'race': [False, True], 'tags': [],
    'cross': ['linux/arm64/cgo0', 'linux/amd64/cgo0', 'darwin/arm64/cgo0'],
    'release': ['-trimpath', '-s -w -X github.com/wangjohn/agent-archive/internal/cli.Version=<current>'],
    'shared-tools': ['-trimpath', '-mod=readonly -buildvcs=false'],
}



class InvalidContext(ValueError):
    """An uncertain identity cannot safely select or save a cache."""


def digest(value):
    return hashlib.sha256(json.dumps(value, sort_keys=True, separators=(',', ':')).encode()).hexdigest()


def execute(argv):
    if argv[:2] == ['go', 'env']:
        # CGO metadata probes use Go's compiler-stat/flag memoization. A new
        # runner can rewrite those entries even with identical compiler bytes.
        # Never let identity discovery mutate a restored or captured inventory.
        with tempfile.TemporaryDirectory(prefix='aa-go-identity-') as temporary:
            return subprocess.check_output(argv, stderr=subprocess.PIPE, timeout=30,
                                           env=dict(os.environ, GOCACHE=temporary))
    return subprocess.check_output(argv, stderr=subprocess.PIPE, timeout=30)


def pr_ancestry(source, tested, event, pr_number, git=scope.git):
    """Require the actual event base/head and tested merge before cache use."""
    pr = event['pull_request']
    if type(pr.get('number')) is not int or pr['number'] != pr_number:
        raise InvalidContext('PR event ownership differs')
    base, head = pr['base']['sha'], pr['head']['sha']
    for sha in (base, head, tested):
        if not isinstance(sha, str) or not SHA.fullmatch(sha):
            raise InvalidContext('invalid event commit')
        if git(source, 'rev-parse', '--verify', sha+'^{commit}').decode().strip() != sha:
            raise InvalidContext('event commit unavailable')
    if git(source, 'rev-parse', '--is-shallow-repository') != b'false\n':
        raise InvalidContext('PR history unavailable')
    if git(source, 'show', '-s', '--format=%P', tested).decode().strip().split() != [base, head]:
        raise InvalidContext('tested source is not event merge')
    if not SHA.fullmatch(git(source, 'merge-base', base, head).decode().strip()):
        raise InvalidContext('PR histories disconnected')
    for sha in (base, head):
        git(source, 'merge-base', '--is-ancestor', sha, tested)


def source_identity(source, tested, git=scope.git, matches=scope.working_input_matches,
                    evidence_directory='shard-results', accelerator_outputs=False):
    """Reuse current proof primitives; inspect tracked bytes even if status hides edits."""
    if not isinstance(tested, str) or not SHA.fullmatch(tested):
        raise InvalidContext('invalid tested source')
    if git(source, 'rev-parse', 'HEAD').decode().strip() != tested:
        raise InvalidContext('checkout differs from tested source')
    tree = git(source, 'rev-parse', 'HEAD^{tree}').decode().strip()
    if not SHA.fullmatch(tree):
        raise InvalidContext('invalid source tree')
    dependencies = {}
    for record in scope.records(git(source, 'ls-tree', '-r', '-z', tested)):
        metadata, raw_path = record.split(b'\t', 1)
        mode, kind, oid = metadata.split()
        path = scope.repository_path(raw_path)
        if (mode not in (b'100644', b'100755') or kind != b'blob'
                or not re.fullmatch(b'[0-9a-f]{40}', oid)
                or not matches(source, path, mode, oid)):
            raise InvalidContext('tracked input differs or is not regular')
        if path in ('go.mod', 'go.sum'):
            dependencies[path] = hashlib.sha256((Path(source)/path).read_bytes()).hexdigest()
    if set(dependencies) != {'go.mod', 'go.sum'}:
        raise InvalidContext('tracked module inputs missing')
    # Existing runner writes evidence after execution. Exempt only its dedicated
    # top-level output directory, never an arbitrary production/package subtree.
    if evidence_directory != 'shard-results':
        raise InvalidContext('unreviewed evidence directory')
    for raw in scope.records(git(source, 'ls-files', '--others', '-z')):
        path = scope.repository_path(raw)
        if path.startswith(evidence_directory+'/'):
            continue
        if accelerator_outputs:
            # Dedicated evidence only: never allow new Go/module/production
            # inputs under a blanket ignored directory.
            evidence = ('source-audit-results/', 'native-tier-results/')
            if path.startswith(evidence) and Path(path).suffix in ('.json', '.jsonl', '.log', '.txt', '.stderr'):
                continue
            if re.fullmatch(r'dist/(agent-archive-linux-(amd64|arm64)|SHA256SUMS)', path):
                continue
        raise InvalidContext('untracked source inputs present')
    return {'tested': tested, 'tree': tree, 'dependency': digest(dependencies)}


def compiler_identity(command, run=execute, which=None):
    # No shell parsing/execution; multi-command wrappers need a reviewed key policy.
    words = shlex.split(command)
    if len(words) != 1:
        raise InvalidContext('compiler wrapper requires explicit policy')
    if which is None:
        which = shutil.which
    selected = which(words[0])
    if selected is None:
        raise InvalidContext('compiler unavailable')
    binary = Path(selected).resolve(strict=True)
    if not binary.is_file():
        raise InvalidContext('compiler is not regular')
    return {'binary': hashlib.sha256(binary.read_bytes()).hexdigest(),
            'version': hashlib.sha256(run([str(binary), '--version'])).hexdigest()}


def toolchain_identity(go, which=shutil.which):
    """Hash the actual Go driver and native build/vet tools, not only their version."""
    selected = which('go')
    if selected is None:
        raise InvalidContext('Go executable unavailable')
    paths = {'go': Path(selected)}
    tool_root = Path(go['GOTOOLDIR'])
    if not tool_root.is_absolute() or not Path(go['GOROOT']).is_absolute():
        raise InvalidContext('Go toolchain paths unavailable')
    for name in ('compile', 'asm', 'link', 'cgo', 'vet'):
        paths[name] = tool_root/name
    result = {}
    for name, path in paths.items():
        binary = path.resolve(strict=True)
        if not binary.is_file():
            raise InvalidContext('Go toolchain binary is not regular')
        result[name] = hashlib.sha256(binary.read_bytes()).hexdigest()
    return result


def runtime_context(run=execute, env=None, os_release=Path('/etc/os-release'),
                    compiler=compiler_identity, toolchain=toolchain_identity, native=False):
    env = os.environ if env is None else env
    version = run(['go', 'version']).decode().strip()
    go = json.loads(run(['go', 'env', '-json', *GO_FIELDS]))
    if not isinstance(go, dict) or set(go) != set(GO_FIELDS) or any(type(v) is not str for v in go.values()):
        raise InvalidContext('incomplete Go metadata')
    if (not re.fullmatch(r'go version go1\.27\.2 (linux|darwin)/(amd64|arm64)', version)
            or go['GOVERSION'] != 'go1.27.2'
            or go['GOOS'] not in (('linux', 'darwin') if native else ('linux',))
            or go['GOHOSTOS'] != go['GOOS']
            or go['GOARCH'] != go['GOHOSTARCH'] or go['GOARCH'] not in ('amd64', 'arm64')
            or go['CGO_ENABLED'] != '1'
            or go['GOTOOLCHAIN'] != 'local' or go['GOWORK'] != 'off'
            or go['GOFLAGS'] != '-mod=readonly' or go['GOCACHEPROG'] != ''):
        raise InvalidContext('unsupported or inconsistent Go build context')
    if version != f'go version {go["GOVERSION"]} {go["GOHOSTOS"]}/{go["GOHOSTARCH"]}':
        raise InvalidContext('Go executable and metadata disagree')
    expected_runner_arch = {'amd64': 'X64', 'arm64': 'ARM64'}[go['GOARCH']]
    expected_os = {'linux': 'Linux', 'darwin': 'macOS'}[go['GOOS']]
    if env.get('RUNNER_OS') != expected_os or env.get('RUNNER_ARCH') != expected_runner_arch:
        raise InvalidContext('runner and Go architecture disagree')
    image = {key: env.get(key) for key in ('ImageOS', 'ImageVersion')}
    if any(not isinstance(v, str) or not re.fullmatch(r'[A-Za-z0-9_.-]+', v) for v in image.values()):
        raise InvalidContext('runner image identity unavailable')
    sdk = {}
    if go['GOOS'] == 'darwin':
        release = run(['sw_vers'])
        for name, argv in {
                'xcode': ['xcodebuild', '-version'],
                'developer': ['xcode-select', '-p'],
                'sdk_path': ['xcrun', '--show-sdk-path'],
                'sdk_version': ['xcrun', '--show-sdk-version'],
                'sdk_build': ['xcrun', '--show-sdk-build-version']}.items():
            value = run(argv)
            if not value.strip():
                raise InvalidContext('native SDK identity incomplete')
            sdk[name] = hashlib.sha256(value).hexdigest()
        # xcrun's selected SDK and explicit deployment overrides are distinct
        # compiler inputs. Hash them without leaking arbitrary environment text.
        sdk['overrides'] = digest({name: env.get(name) for name in
                                   ('SDKROOT', 'DEVELOPER_DIR', 'MACOSX_DEPLOYMENT_TARGET')})
    else:
        release = os_release.read_bytes()
        if b'ID=' not in release or b'VERSION_ID=' not in release:
            raise InvalidContext('OS release metadata incomplete')
    return {'go': go, 'version': version, 'runner_os': env['RUNNER_OS'],
            'runner_arch': env['RUNNER_ARCH'], 'image': image,
            'os_release': hashlib.sha256(release).hexdigest(),
            'cc': compiler(go['CC']), 'cxx': compiler(go['CXX']),
            'toolchain': toolchain(go),
            'race': True, 'tags': [], 'schema': SCHEMA,
            'pinned_tools': TOOL_PINS, 'compilation_modes': COMPILATION_MODES,
            'native_environment': digest({name: env.get(name) for name in
                ('CFLAGS', 'CPPFLAGS', 'CXXFLAGS', 'LDFLAGS', 'MAKEFLAGS', 'CPATH',
                 'C_INCLUDE_PATH', 'CPLUS_INCLUDE_PATH', 'LIBRARY_PATH', 'PKG_CONFIG_PATH')}),
            **({'sdk': sdk} if sdk else {})}


def validate_context(context, *, native=False):
    """One exact schema validator for cache, audit and per-worker vet proofs."""
    fields = {'go', 'version', 'runner_os', 'runner_arch', 'image', 'os_release',
              'cc', 'cxx', 'toolchain', 'race', 'tags', 'schema',
              'pinned_tools', 'compilation_modes', 'native_environment'}
    if not isinstance(context, dict):
        raise InvalidContext('runtime context is not an object')
    go = context.get('go')
    darwin = isinstance(go, dict) and go.get('GOOS') == 'darwin'
    if darwin:
        fields.add('sdk')
    if (set(context) != fields or context.get('schema') != SCHEMA
            or not isinstance(go, dict) or set(go) != set(GO_FIELDS)
            or any(type(v) is not str for v in go.values())
            or context.get('pinned_tools') != TOOL_PINS
            or context.get('compilation_modes') != COMPILATION_MODES
            or type(context.get('race')) is not bool or context.get('tags') != []):
        raise InvalidContext('runtime context schema differs')
    system, arch = go['GOOS'], go['GOARCH']
    if (system not in (('linux', 'darwin') if native else ('linux',))
            or go['GOHOSTOS'] != system or go['GOHOSTARCH'] != arch
            or arch not in ('amd64', 'arm64')
            or context['version'] != f'go version go1.27.2 {system}/{arch}'
            or context['runner_os'] != {'linux': 'Linux', 'darwin': 'macOS'}[system]
            or context['runner_arch'] != {'amd64': 'X64', 'arm64': 'ARM64'}[arch]
            or any(go[k] != v for k, v in {'GOVERSION': 'go1.27.2', 'CGO_ENABLED': '1',
                    'GOTOOLCHAIN': 'local', 'GOWORK': 'off', 'GOFLAGS': '-mod=readonly', 'GOCACHEPROG': ''}.items())):
        raise InvalidContext('runtime build/runner identity differs')
    image = context['image']
    if (not isinstance(image, dict) or set(image) != {'ImageOS', 'ImageVersion'}
            or any(type(v) is not str or not re.fullmatch(r'[A-Za-z0-9_.-]+', v)
                   for v in image.values())):
        raise InvalidContext('runtime image provenance malformed')
    hashes = [context['os_release'], context['native_environment']]
    for name in ('cc', 'cxx', 'toolchain'):
        values = context[name]
        expected = {'go', 'compile', 'asm', 'link', 'cgo', 'vet'} if name == 'toolchain' else {'binary', 'version'}
        if not isinstance(values, dict) or set(values) != expected:
            raise InvalidContext('runtime compiler identity malformed')
        hashes.extend(values.values())
    if darwin:
        sdk = context['sdk']
        if not isinstance(sdk, dict) or set(sdk) != {'xcode', 'developer', 'sdk_path', 'sdk_version', 'sdk_build', 'overrides'}:
            raise InvalidContext('runtime SDK identity malformed')
        hashes.extend(sdk.values())
    if any(type(value) is not str or not re.fullmatch(r'[0-9a-f]{64}', value) for value in hashes):
        raise InvalidContext('runtime compiler checksum malformed')
    return context


@dataclass(frozen=True)
class CachePlan:
    primary: str
    prefix: str
    paths: tuple
    source: dict
    seed: str = ''


def cache_plan(pr, worker, context, source, runner_temp, workers=2):
    if type(pr) is not int or pr <= 0 or type(worker) is not int or type(workers) is not int or workers not in (2, 4, 8) or worker not in range(workers):
        raise InvalidContext('invalid PR or worker ownership')
    # Compilation is content addressed by Go, independent of test assignment.
    # All PR workers can read one snapshot; only worker zero publishes it.
    return owned_plan(f'{SCHEMA}-pr{pr}-n{workers}', 0, context, source, runner_temp, workers)


def owned_plan(namespace, worker, context, source, runner_temp, workers=2):
    """Shared key primitive; callers own event/namespace policy, never spoof events."""
    if not re.fullmatch(r'[A-Za-z0-9-]+', namespace) or type(worker) is not int or type(workers) is not int or workers not in (2, 4, 8) or worker not in range(workers):
        raise InvalidContext('unsafe cache namespace or worker')
    if (set(source) != {'tested', 'tree', 'dependency'}
            or not SHA.fullmatch(source['tested']) or not SHA.fullmatch(source['tree'])
            or not re.fullmatch(r'[0-9a-f]{64}', source['dependency'])):
        raise InvalidContext('invalid source proof outputs')
    paths = owned_paths(runner_temp)
    prefix = f'{namespace}-w{worker}-{digest(context)}-{source["dependency"]}-'
    primary = prefix + source['tree']
    if len(primary) > 512 or not re.fullmatch(r'[a-zA-Z0-9-]+', primary):
        raise InvalidContext('unsafe or oversized cache key')
    seed = f'aa-trusted-go-v2-{digest(context)}-{source["dependency"]}'
    return CachePlan(primary, prefix, paths, source, seed)


def owned_paths(runner_temp):
    root = Path(runner_temp)
    if (not root.is_absolute() or root.is_symlink() or root.resolve() != root
            or any(char in str(root) for char in '\r\n')):
        raise InvalidContext('runner temporary root is not canonical')
    return (str(root/'pr-go-build'), str(root/'pr-go-modules'))


def outputs(plan):
    # Intentionally exclude raw Go/CGO flags/compiler versions from log evidence.
    return {'primary': plan.primary, 'restore_prefix': plan.prefix,
            'gocache': plan.paths[0], 'gomodcache': plan.paths[1],
            'tested': plan.source['tested'], 'tree': plan.source['tree'],
            'seed': plan.seed}


def prepare_paths(plan, clear=False):
    prepare_owned_paths(plan.paths, clear)


def prepare_owned_paths(paths, clear=False):
    roots = {Path(p).parent for p in paths}
    if len(roots) != 1 or [Path(p).name for p in paths] != ['pr-go-build', 'pr-go-modules']:
        raise InvalidContext('cache paths outside controlled directories')
    root = roots.pop()
    if not root.is_absolute() or root.resolve() != root:
        raise InvalidContext('cache root is not canonical')
    for raw in paths:
        path = Path(raw)
        if path.is_symlink() or (path.exists() and not path.is_dir()):
            raise InvalidContext('unsafe cache path')
        if clear and path.exists():
            # Only the two direct controlled directories, never a checkout or parent.
            # Go's extracted module directories are read-only. Make only real
            # directories writable; do not follow or chmod cached symlink targets.
            for current, directories, _ in os.walk(path, followlinks=False):
                item = Path(current)
                if not item.is_symlink():
                    item.chmod(item.stat().st_mode | stat.S_IRUSR | stat.S_IWUSR | stat.S_IXUSR)
            shutil.rmtree(path)
        path.mkdir(parents=True, exist_ok=True)
