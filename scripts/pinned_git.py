#!/usr/bin/env python3
"""Reuse exact upstream local-only Git builds from a verified compilation seed."""
import argparse
import hashlib
import json
import inspect
import os
from pathlib import Path, PurePosixPath
import shutil
import subprocess
import tarfile
import tempfile

from go_cache_context import InvalidContext, TOOL_PINS

PINS = {
    'upstream-2.30.2': ('v2.30.2', TOOL_PINS['git-2.30.2'], 'git version 2.30.2'),
    'upstream-2.39.2': ('v2.39.2', TOOL_PINS['git-2.39.2'], 'git version 2.39.2'),
}
FLAGS = ['NO_TCLTK=YesPlease', 'NO_GETTEXT=YesPlease', 'NO_CURL=YesPlease', 'NO_EXPAT=YesPlease']


def extract(archive, destination):
    """Installed Git has relative symlinks; validate before any extraction."""
    with tarfile.open(archive, 'r:gz') as handle:
        members = handle.getmembers()
        for member in members:
            path = PurePosixPath(member.name)
            if (path.is_absolute() or '..' in path.parts
                    or not (member.isfile() or member.isdir() or member.issym() or member.islnk())):
                raise InvalidContext('unsafe Git archive member')
            if member.issym() or member.islnk():
                target = PurePosixPath(member.linkname)
                # Git's installation links sibling executables only. Reject
                # parent links rather than trying to normalize an escaping chain.
                if target.is_absolute() or '..' in target.parts:
                    raise InvalidContext('unsafe Git executable symlink')
        # Reject children beneath any archived symlink, irrespective of order.
        links = {m.name.rstrip('/') for m in members if m.issym()}
        if any(any(parent.as_posix() in links for parent in PurePosixPath(m.name).parents)
               for m in members):
            raise InvalidContext('archive writes beneath symlink')
        destination = Path(destination)
        if destination.is_symlink() or (destination.exists() and any(destination.iterdir())):
            raise InvalidContext('Git extraction requires empty real destination')
        destination.mkdir(parents=True, exist_ok=True)
        # Python 3.9 on developer macOS lacks tar filters. The complete member
        # validation above plus an empty destination also protects that path.
        options = {'filter': 'data'} if 'filter' in inspect.signature(handle.extractall).parameters else {}
        handle.extractall(destination, members=members, **options)


def ensure(name, cache, destination, command=subprocess.check_output):
    tag, commit, expected = PINS[name]
    cache, destination = Path(cache), Path(destination)
    if cache.is_symlink() or destination.is_symlink():
        raise InvalidContext('unsafe Git cache/install root')
    cache.mkdir(parents=True, exist_ok=True)
    archive, receipt = cache/(name+'.tar.gz'), cache/(name+'.json')
    identity = {'commit': commit, 'tag': tag, 'expected': expected, 'flags': FLAGS}
    if destination.exists():
        shutil.rmtree(destination)
    destination.mkdir(parents=True)
    try:
        recorded = json.loads(receipt.read_text())
        if recorded != dict(identity, sha256=hashlib.sha256(archive.read_bytes()).hexdigest()):
            raise InvalidContext('Git cache provenance/checksum differs')
        extract(archive, destination)
        if command([str(destination/'bin/git'), '--version']).decode().strip() != expected:
            raise InvalidContext('wrong cached Git executable')
        return destination/'bin'
    except (InvalidContext, OSError, ValueError, tarfile.TarError, subprocess.SubprocessError):
        shutil.rmtree(destination)
        destination.mkdir()
    # Build outside the product checkout; no injected source, tags or credentials.
    with tempfile.TemporaryDirectory(prefix='aa-pinned-git-') as temporary:
        source = Path(temporary)/'git'
        command(['git', 'clone', '--quiet', '--no-checkout', '--filter=blob:none',
                 'https://github.com/git/git.git', str(source)], timeout=600)
        command(['git', '-C', str(source), 'checkout', '--quiet', commit], timeout=600)
        if command(['git', '-C', str(source), 'rev-parse', 'HEAD']).decode().strip() != commit:
            raise InvalidContext('Git upstream commit differs')
        command(['git', '-C', str(source), 'fetch', '--depth=1', 'origin',
                 f'refs/tags/{tag}:refs/tags/{tag}'], timeout=600)
        if command(['git', '-C', str(source), 'rev-parse', tag+'^{commit}']).decode().strip() != commit:
            raise InvalidContext('Git tag does not identify pinned commit')
        command(['make', '-j2', 'prefix='+str(destination), *FLAGS, 'all'], cwd=source, timeout=1200)
        command(['make', 'prefix='+str(destination), *FLAGS, 'install'], cwd=source, timeout=600)
    if command([str(destination/'bin/git'), '--version']).decode().strip() != expected:
        raise InvalidContext('wrong built Git version')
    with tarfile.open(archive, 'w:gz', dereference=False) as handle:
        for child in sorted(destination.iterdir()):
            handle.add(child, arcname=child.name)
    receipt.write_text(json.dumps(dict(identity, sha256=hashlib.sha256(archive.read_bytes()).hexdigest()), sort_keys=True))
    return destination/'bin'


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('name', choices=PINS)
    args = parser.parse_args()
    binary_dir = ensure(args.name, Path(os.environ['GOCACHE'])/'aa-pinned-git',
                        Path(os.environ['RUNNER_TEMP'])/'git-fixture')
    with open(os.environ['GITHUB_PATH'], 'a') as handle:
        handle.write(str(binary_dir)+'\n')


if __name__ == '__main__':
    main()
