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

import scope_native_verify as scope

SCHEMA = 'aa-pr-go-v1'
GO_FIELDS = ('GOVERSION GOOS GOARCH GOHOSTOS GOHOSTARCH GOAMD64 GOARM64 '
             'CGO_ENABLED GOEXPERIMENT GOFLAGS GOTOOLCHAIN CC CXX '
             'CGO_CFLAGS CGO_CPPFLAGS CGO_CXXFLAGS CGO_LDFLAGS GOWORK GOROOT GOTOOLDIR').split()
SHA = re.compile(r'[0-9a-f]{40}')


class InvalidContext(ValueError):
    """An uncertain identity cannot safely select or save a cache."""


def digest(value):
    return hashlib.sha256(json.dumps(value, sort_keys=True, separators=(',', ':')).encode()).hexdigest()


def execute(argv):
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
                    evidence_directory='shard-results'):
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
        if not path.startswith(evidence_directory+'/'):
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
                    compiler=compiler_identity, toolchain=toolchain_identity):
    env = os.environ if env is None else env
    version = run(['go', 'version']).decode().strip()
    go = json.loads(run(['go', 'env', '-json', *GO_FIELDS]))
    if not isinstance(go, dict) or set(go) != set(GO_FIELDS) or any(type(v) is not str for v in go.values()):
        raise InvalidContext('incomplete Go metadata')
    if (not re.fullmatch(r'go version go1\.27\.2 linux/(amd64|arm64)', version)
            or go['GOVERSION'] != 'go1.27.2'
            or go['GOOS'] != 'linux' or go['GOHOSTOS'] != 'linux'
            or go['GOARCH'] != go['GOHOSTARCH'] or go['GOARCH'] not in ('amd64', 'arm64')
            or go['CGO_ENABLED'] != '1'
            or go['GOTOOLCHAIN'] != 'local' or go['GOWORK'] != 'off'
            or go['GOFLAGS'] != '-mod=readonly'):
        raise InvalidContext('unsupported or inconsistent Go build context')
    if version != f'go version {go["GOVERSION"]} {go["GOHOSTOS"]}/{go["GOHOSTARCH"]}':
        raise InvalidContext('Go executable and metadata disagree')
    expected_runner_arch = {'amd64': 'X64', 'arm64': 'ARM64'}[go['GOARCH']]
    if env.get('RUNNER_OS') != 'Linux' or env.get('RUNNER_ARCH') != expected_runner_arch:
        raise InvalidContext('runner and Go architecture disagree')
    image = {key: env.get(key) for key in ('ImageOS', 'ImageVersion')}
    if any(not isinstance(v, str) or not re.fullmatch(r'[A-Za-z0-9_.-]+', v) for v in image.values()):
        raise InvalidContext('runner image identity unavailable')
    release = os_release.read_bytes()
    if b'ID=' not in release or b'VERSION_ID=' not in release:
        raise InvalidContext('OS release metadata incomplete')
    return {'go': go, 'version': version, 'runner_os': env['RUNNER_OS'],
            'runner_arch': env['RUNNER_ARCH'], 'image': image,
            'os_release': hashlib.sha256(release).hexdigest(),
            'cc': compiler(go['CC']), 'cxx': compiler(go['CXX']),
            'toolchain': toolchain(go),
            'race': True, 'tags': [], 'schema': SCHEMA}


@dataclass(frozen=True)
class CachePlan:
    primary: str
    prefix: str
    paths: tuple
    source: dict


def cache_plan(pr, worker, context, source, runner_temp, workers=2):
    if type(pr) is not int or pr <= 0 or type(worker) is not int or type(workers) is not int or workers not in (2, 4) or worker not in range(workers):
        raise InvalidContext('invalid PR or worker ownership')
    return owned_plan(f'{SCHEMA}-pr{pr}-n{workers}', worker, context, source, runner_temp, workers)


def owned_plan(namespace, worker, context, source, runner_temp, workers=2):
    """Shared key primitive; callers own event/namespace policy, never spoof events."""
    if not re.fullmatch(r'[A-Za-z0-9-]+', namespace) or type(worker) is not int or type(workers) is not int or workers not in (2, 4) or worker not in range(workers):
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
    return CachePlan(primary, prefix, paths, source)


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
            'tested': plan.source['tested'], 'tree': plan.source['tree']}


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
