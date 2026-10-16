"""Pure-data v1 vet decoding for explicitly Git-bound timing imports only.

Git objects identify a reviewed contract; none of their Python is executed.
This module supplies no cache identity or live-verification compatibility.
"""
from dataclasses import dataclass
import re
import subprocess

from go_cache_context import InvalidContext


PATHS = ('scripts/go_cache_context.py', 'scripts/run_ci_tier.py',
         'scripts/run_go_shard.py', 'scripts/ci_tiers.json')
# Independently read from immutable measured07c8 and main73a9 Git trees.
CONTRACTS = frozenset({
    ('8f93af51a3dc07041e7ce97d04dd31dad5e5c95e',
     'cb213aa8f38155b1ef0799b79003a0fdf38e9712',
     'be7755fc11c3751ba7cbf6477edb44c00bc9eeb8',
     '09b1948d5eb8a180a61f16213c4b8658f3fcc4f5'),
    ('8f93af51a3dc07041e7ce97d04dd31dad5e5c95e',
     'cb213aa8f38155b1ef0799b79003a0fdf38e9712',
     'b3834ccb023097fdcd9151c9c7b1e7ddc43ecda1',
     '09b1948d5eb8a180a61f16213c4b8658f3fcc4f5'),
})
GO_FIELDS = frozenset(('GOVERSION GOOS GOARCH GOHOSTOS GOHOSTARCH GOAMD64 GOARM64 '
    'CGO_ENABLED GOEXPERIMENT GOFLAGS GOTOOLCHAIN CC CXX CGO_CFLAGS CGO_CPPFLAGS '
    'CGO_CXXFLAGS CGO_LDFLAGS GOWORK GOROOT GOTOOLDIR').split())
FIELDS = frozenset(('go version runner_os runner_arch image os_release cc cxx '
                    'toolchain race tags schema').split())


@dataclass(frozen=True)
class HistoricalContract:
    source: str
    tree: str
    blobs: tuple


def resolve(root, expected):
    """Return authority only for a regular, recognized immutable Git contract."""
    if (not isinstance(expected, dict) or set(expected) != {'source', 'tree'}
            or any(type(v) is not str or not re.fullmatch('[0-9a-f]{40}', v)
                   for v in expected.values())):
        raise InvalidContext('historical vet requires an immutable source/tree proof')
    def git(*args):
        return subprocess.check_output(['git', '-C', str(root), *args],
            stderr=subprocess.PIPE, timeout=20).decode().strip()
    try:
        source, tree = expected['source'], expected['tree']
        if (git('rev-parse', '--verify', source + '^{commit}') != source
                or git('rev-parse', source + '^{tree}') != tree):
            raise InvalidContext('historical vet Git source/tree differs')
        blobs = []
        for path in PATHS:
            entry = git('ls-tree', source, '--', path)
            if not entry:
                return None
            metadata, actual_path = entry.split('\t')
            mode, kind, blob = metadata.split()
            if (mode != '100644' or kind != 'blob' or actual_path != path
                    or not re.fullmatch('[0-9a-f]{40}', blob)):
                return None
            blobs.append(blob)
        blobs = tuple(blobs)
        return HistoricalContract(source, tree, blobs) if blobs in CONTRACTS else None
    except (subprocess.SubprocessError, OSError, UnicodeError, ValueError) as error:
        raise InvalidContext('historical vet immutable Git contract unavailable') from error


def validate_context(context, expected, authority, root):
    """Validate original v1 data; never rewrite its raw bytes or digest inputs."""
    if (type(authority) is not HistoricalContract
            or authority.blobs not in CONTRACTS
            or {'source': authority.source, 'tree': authority.tree} != expected
            or resolve(root, expected) != authority):
        raise InvalidContext('historical vet authority differs from measured Git source')
    go = context.get('go') if isinstance(context, dict) else None
    if (not isinstance(context, dict) or set(context) != FIELDS
            or context.get('schema') != 'aa-pr-go-v1'
            or not isinstance(go, dict) or set(go) != GO_FIELDS
            or any(type(v) is not str for v in go.values())
            or context.get('race') is not False or context.get('tags') != []
            or context.get('version') != 'go version go1.27.2 linux/amd64'
            or context.get('runner_os') != 'Linux' or context.get('runner_arch') != 'X64'
            or any(go[k] != v for k, v in {'GOVERSION': 'go1.27.2', 'GOOS': 'linux',
                'GOHOSTOS': 'linux', 'GOARCH': 'amd64', 'GOHOSTARCH': 'amd64',
                'CGO_ENABLED': '1', 'GOTOOLCHAIN': 'local', 'GOWORK': 'off',
                'GOFLAGS': '-mod=readonly'}.items())):
        raise InvalidContext('historical vet runtime schema/build identity differs')
    image = context['image']
    if (not isinstance(image, dict) or set(image) != {'ImageOS', 'ImageVersion'}
            or any(type(v) is not str or not re.fullmatch('[A-Za-z0-9_.-]+', v)
                   for v in image.values())):
        raise InvalidContext('historical vet image identity malformed')
    hashes = [context['os_release']]
    for name in ('cc', 'cxx', 'toolchain'):
        values = context[name]
        keys = {'go', 'compile', 'asm', 'link', 'cgo', 'vet'} if name == 'toolchain' else {'binary', 'version'}
        if not isinstance(values, dict) or set(values) != keys:
            raise InvalidContext('historical vet compiler identity malformed')
        hashes.extend(values.values())
    if any(type(v) is not str or not re.fullmatch('[0-9a-f]{64}', v) for v in hashes):
        raise InvalidContext('historical vet compiler checksum malformed')
    return context
