#!/usr/bin/env python3
"""Compile immutable shared helpers into Go's cache, never checker results."""
import argparse
import os
from pathlib import Path
import subprocess

from go_cache_context import InvalidContext, TOOL_PINS, source_identity

MODULES = (
    ('', './cmd/levenshtein'),
    ('runner/lint', './cmd/levenshtein-lint'),
    ('runner/tools/actionlint', 'github.com/rhysd/actionlint/cmd/actionlint'),
    ('runner/tools/govulncheck', 'golang.org/x/vuln/cmd/govulncheck'),
)


def shared_identity(source):
    source = Path(source).resolve(strict=True)
    proof = source_identity(source, TOOL_PINS['shared'])
    if (source/'.go-version').read_text().strip() != '1.27.2':
        raise InvalidContext('shared compiler pin differs')
    return proof


def checkout(root, command=subprocess.check_call):
    root = Path(root)
    if root.is_symlink() or not root.is_absolute():
        raise InvalidContext('shared seed source root is not controlled')
    if not root.exists():
        command(['git', 'clone', '--quiet', '--no-checkout', '--filter=blob:none',
                 'https://github.com/wangjohn/levenshtein.git', str(root)])
        command(['git', '-C', str(root), 'checkout', '--quiet', TOOL_PINS['shared']])
    shared_identity(root)
    return root


def verify_modules(source, command=subprocess.check_call):
    before = shared_identity(source)
    for directory, _ in MODULES:
        module = Path(source)/directory
        command(['go', 'mod', 'download'], cwd=module)
        command(['go', 'mod', 'verify'], cwd=module)
    if shared_identity(source) != before:
        raise InvalidContext('shared source changed during module verification')


def warm(source, output, command=subprocess.check_call):
    before = shared_identity(source)
    output = Path(output)
    # Match the pinned launcher's flags. No verifier command is invoked.
    env = dict(os.environ, GOTOOLCHAIN='local', GOWORK='off', CGO_ENABLED='0',
               GOFLAGS='-mod=readonly -buildvcs=false')
    command(['go', 'build', '-o', str(output), MODULES[0][1]], cwd=source, env=env)
    for directory, package in MODULES[1:]:
        command(['go', 'build', '-trimpath', '-o', str(output), package], cwd=Path(source)/directory)
    verify_modules(source, command)
    if shared_identity(source) != before:
        raise InvalidContext('shared source changed during tool compilation')


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--checkout-only', action='store_true')
    args = parser.parse_args()
    source = checkout(Path(os.environ['RUNNER_TEMP'])/'shared-seed-source')
    if not args.checkout_only:
        warm(source, Path(os.environ['RUNNER_TEMP'])/'shared-seed-tool')
