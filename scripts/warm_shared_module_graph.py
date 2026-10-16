#!/usr/bin/env python3
"""Prefetch only checksummed product modules for fresh shared tidy checks.

A no-argument go mod download downloads the main requirements, but tidy also
loads dependency tests. Seed their already-reviewed go.sum entries once on a
cold trusted-main producer, without executing or caching any check verdict.
"""
import argparse
import base64
import os
from pathlib import Path
import re
import subprocess

from go_cache_context import InvalidContext


MAX_MODULES = 512


def module_graph(data):
    modules = set()
    manifests = set()
    checksums = {}
    for line in data.decode('utf-8').splitlines():
        fields = line.split()
        if len(fields) != 3:
            raise InvalidContext('invalid product go.sum entry')
        module, version, checksum = fields
        try:
            valid_hash = (checksum.startswith('h1:') and
                          len(base64.b64decode(checksum[3:], validate=True)) == 32)
        except ValueError:
            valid_hash = False
        if (not valid_hash or not re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9._~/-]*', module)
                or any(part in ('', '.', '..') for part in module.split('/'))
                or not re.fullmatch(r'v[0-9][A-Za-z0-9.+-]*(?:/go.mod)?', version)):
            raise InvalidContext('invalid product module identity')
        identity = (module, version)
        if identity in checksums and checksums[identity] != checksum:
            raise InvalidContext('conflicting product module checksums')
        checksums[identity] = checksum
        if len(checksums) > MAX_MODULES * 2:
            raise InvalidContext('product module graph exceeds prefetch bound')
        if version.endswith('/go.mod'):
            manifests.add((module, version[:-7]))
        else:
            modules.add((module, version))
    if not modules or len(modules) > MAX_MODULES or not modules <= manifests:
        raise InvalidContext('product modules require bounded checksummed manifests')
    return [module + '@' + version for module, version in sorted(modules)]


def warm(source, command=subprocess.check_call):
    source = Path(source).resolve(strict=True)
    paths = [source / name for name in ('go.mod', 'go.sum')]
    if any(path.is_symlink() or not path.is_file() for path in paths):
        raise InvalidContext('product module manifests must be regular files')
    before = [path.read_bytes() for path in paths]
    modules = module_graph(before[1])
    env = dict(os.environ, GOTOOLCHAIN='local', GOWORK='off', GOENV='off',
               GOFLAGS='-mod=readonly')
    try:
        command(['go', 'mod', 'download', *modules], cwd=source, env=env)
    finally:
        if any(path.is_symlink() or not path.is_file() for path in paths) or [path.read_bytes() for path in paths] != before:
            raise InvalidContext('product manifests changed during module prefetch')
    return len(modules)


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--source', type=Path, default=Path('.'))
    args = parser.parse_args()
    print(f'Prefetched {warm(args.source)} checksummed product module versions; checks still execute freshly')
