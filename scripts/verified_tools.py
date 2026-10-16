#!/usr/bin/env python3
"""Pinned Go tool binaries inside the verified compilation seed, never verdicts."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import subprocess
import shutil

from go_cache_context import InvalidContext, TOOL_PINS

TOOLS = {
    'golangci-lint': ('github.com/golangci/golangci-lint/v2/cmd/golangci-lint',
                      *TOOL_PINS['golangci-lint']),
    'deadcode': ('golang.org/x/tools/cmd/deadcode', *TOOL_PINS['deadcode']),
}


def inspect(binary, name, command=subprocess.check_output):
    package, module, version = TOOLS[name]
    if binary.is_symlink() or not binary.is_file() or not os.access(binary, os.X_OK):
        raise InvalidContext('tool is not a regular executable')
    output = command(['go', 'version', '-m', str(binary)], timeout=30).decode()
    fields = [line.strip().split('\t') for line in output.splitlines()[1:]]
    if (not output.splitlines()[0].endswith(': go1.27.2')
            or ['path', package] not in fields
            or not any(row[:3] == ['mod', module, version] for row in fields)
            or any(row[0] == '=>' for row in fields)):
        raise InvalidContext('wrong tool module/version/compiler')
    values = json.loads(command(['go', 'env', '-json', 'GOOS', 'GOARCH', 'CGO_ENABLED'], timeout=30))
    for key, value in values.items():
        if ['build', key+'='+value] not in fields:
            raise InvalidContext('tool build architecture/cgo differs')
    return {'package': package, 'module': module, 'version': version,
            'compiler': 'go1.27.2', 'build': values,
            'sha256': hashlib.sha256(binary.read_bytes()).hexdigest()}


def ensure(name, root, command=subprocess.check_output):
    root = Path(root)
    if not root.is_absolute() or root.is_symlink():
        raise InvalidContext('tool directory must be absolute and real')
    root.mkdir(parents=True, exist_ok=True)
    binary, receipt = root/name, root/(name+'.json')
    try:
        observed = inspect(binary, name, command)
        if json.loads(receipt.read_text()) != observed:
            raise InvalidContext('tool checksum/provenance differs')
        return binary
    except (InvalidContext, OSError, ValueError, subprocess.SubprocessError):
        for path in (binary, receipt):
            if path.is_dir() and not path.is_symlink():
                shutil.rmtree(path)
            else:
                path.unlink(missing_ok=True)
    env = dict(os.environ, GOBIN=str(root), GOTOOLCHAIN='local', GOWORK='off')
    package, _, version = TOOLS[name]
    command(['go', 'install', package+'@'+version], env=env, timeout=1800)
    receipt.write_text(json.dumps(inspect(binary, name, command), sort_keys=True))
    return binary


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('name', choices=TOOLS)
    args = parser.parse_args()
    binary = ensure(args.name, Path(os.environ['GOCACHE'])/'aa-pinned-tools')
    print(binary)


if __name__ == '__main__':
    main()
