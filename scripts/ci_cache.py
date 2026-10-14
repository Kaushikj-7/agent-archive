#!/usr/bin/env python3
"""Adapters for verified compilation caches; no check results or XDG state.

One shared immutable trusted-main seed per complete host/compiler/module context.
PR workers share a single PR-scoped compilation snapshot, published by worker 0.
Checks always run against current source; neither a cache nor its manifest is a
verification receipt. Artifact/receipt locations are never cache paths.
"""
import argparse
from dataclasses import asdict
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys

from go_cache_context import (InvalidContext, CachePlan, digest, execute,
                              owned_paths, prepare_paths, runtime_context,
                              source_identity, validate_context)
from go_cache_steps import emit, execute_modules

SCHEMA = 'aa-compilation-v2'
MAX_BYTES = 2 * 1024 * 1024 * 1024
MANIFEST = '.aa-compilation-manifest.json'


def cache_files(paths):
    """Walk once: retain stat metadata for size budgeting and checksum capture."""
    import stat
    result = []
    for index, raw in enumerate(paths):
        root = Path(raw)
        if root.is_symlink() or not root.is_dir():
            raise InvalidContext('cache root is not a real directory')
        for current, dirs, files in os.walk(root, followlinks=False):
            for name in dirs:
                if (Path(current)/name).is_symlink():
                    raise InvalidContext('cache directory symlink')
            for name in files:
                path = Path(current)/name
                if index == 0 and path == root/MANIFEST:
                    continue
                info = path.lstat()
                if not stat.S_ISREG(info.st_mode):
                    raise InvalidContext('cache member is not regular')
                result.append((path, f'{index}/{path.relative_to(root).as_posix()}',
                               info.st_size, info.st_mtime_ns))
    return result


def file_inventory(paths, files=None):
    """Checksum bytes before Go consumes them; no redundant per-file stats."""
    files = cache_files(paths) if files is None else files
    size = sum(item[2] for item in files)
    if size > MAX_BYTES:
        raise InvalidContext('cache exceeds two GiB publication budget')
    result = {}
    for path, key, length, _ in files:
        sha = hashlib.sha256()
        observed = 0
        with path.open('rb') as handle:
            for chunk in iter(lambda: handle.read(1024 * 1024), b''):
                observed += len(chunk)
                sha.update(chunk)
        if observed != length:
            raise InvalidContext('cache member changed during checksum capture')
        result[key] = sha.hexdigest()
    return result, size


def bound_compilation(plan, files=None):
    """Drop only Go's recoverable action/object entries, oldest first.

    Module contents, pinned binaries, manifests and receipts are never edited.
    Preserve 98% of the cap for manifest room. Missing Go objects trigger normal
    fresh compilation. Enumerated metadata avoids an extra tree walk/stat pass.
    """
    files = cache_files(plan.paths) if files is None else files
    size = sum(item[2] for item in files)
    target = int(MAX_BYTES * .98)
    removed = 0
    candidates = []
    for item in files:
        path, key, length, modified = item
        parts = key.split('/')
        if (len(parts) == 3 and parts[0] == '0' and re.fullmatch(r'[0-9a-f]{2}', parts[1])
                and re.fullmatch(r'[0-9a-f]{64}-[ad]', parts[2])):
            candidates.append(item)
    discarded = set()
    for path, key, length, modified in sorted(candidates, key=lambda item: item[3]):
        if size <= target:
            break
        path.unlink()
        discarded.add(key)
        size -= length
        removed += length
    files[:] = [item for item in files if item[1] not in discarded]
    return removed


def snapshot_paths(plan, kind):
    if kind == 'compilation':
        return (plan.paths[0],)
    if kind == 'seed':
        return (plan.paths[0], str(Path(plan.paths[1])/'cache'))
    raise InvalidContext('unknown compilation snapshot kind')


def publish_manifest(plan, context, kind='seed'):
    paths = snapshot_paths(plan, kind)
    for path in paths:
        Path(path).mkdir(parents=True, exist_ok=True)
    candidates = cache_files(paths)
    bound_compilation(plan, candidates)
    files, size = file_inventory(paths, candidates)
    (Path(plan.paths[0])/MANIFEST).write_text(json.dumps({
        'schema': SCHEMA, 'kind': kind, 'context': digest(context),
        'dependency': plan.source['dependency'], 'source': plan.source,
        'files': files, 'bytes': size}, sort_keys=True))
    return size


def validate_manifest(plan, context, kind='seed'):
    manifest = json.loads((Path(plan.paths[0])/MANIFEST).read_text())
    if not isinstance(manifest, dict) or manifest.get('kind') != kind:
        raise InvalidContext('compilation manifest kind differs')
    paths = snapshot_paths(plan, manifest.get('kind'))
    files, size = file_inventory(paths)
    if (not isinstance(manifest, dict) or set(manifest) != {
            'schema', 'kind', 'context', 'dependency', 'source', 'files', 'bytes'}
            or manifest['schema'] != SCHEMA or manifest['context'] != digest(context)
            or manifest['dependency'] != plan.source['dependency']
            or manifest['files'] != files or manifest['bytes'] != size
            or not isinstance(manifest['source'], dict)
            or set(manifest['source']) != {'tested', 'tree', 'dependency'}
            or manifest['source']['dependency'] != plan.source['dependency']
            or any(not re.fullmatch(r'[0-9a-f]{40}', manifest['source'].get(k, ''))
                   for k in ('tested', 'tree'))):
        raise InvalidContext('cache checksum/context manifest differs')
    return size


def trusted_main(env, event):
    """Event/ref plus exact checkout proof; never trust a PR's claimed branch."""
    return (env.get('GITHUB_EVENT_NAME') == 'push'
            and env.get('GITHUB_REF') == 'refs/heads/main'
            and event.get('ref') == 'refs/heads/main'
            and event.get('after') == env.get('GITHUB_SHA')
            and event.get('repository', {}).get('full_name') == env.get('GITHUB_REPOSITORY')
            and event.get('deleted') is False)


def general_plan(context, source, runner_temp):
    key = f'aa-trusted-go-v2-{digest(context)}-{source["dependency"]}'
    return CachePlan(key, key, owned_paths(runner_temp), source, key)


def restore_compilation(plan, context, outcome, matched, guard, command=execute_modules):
    """Corrupt/missing accelerators are discarded, then redownloaded and verified."""
    state = 'miss'
    try:
        if outcome != 'success' or matched != plan.seed:
            raise InvalidContext('trusted seed unavailable')
        validate_manifest(plan, context)
        state = 'trusted'
    except (InvalidContext, OSError, ValueError, TypeError):
        prepare_paths(plan, clear=True)
    guard(plan.source)
    try:
        command(['go', 'mod', 'download'])
        command(['go', 'mod', 'verify'])
    except subprocess.SubprocessError:
        # A restored module can fail verification or contain broken download
        # metadata. Retry once from empty owned paths; a real upstream/module
        # failure still fails the job on this second attempt.
        if state != 'trusted':
            raise
        prepare_paths(plan, clear=True)
        state = 'corrupt-rebuilt'
        command(['go', 'mod', 'download'])
        command(['go', 'mod', 'verify'])
    guard(plan.source)
    return state


def current_source(source, tested):
    # Cache consumers may have written only reviewed evidence/release outputs.
    # Pass the same tracked-byte/mode proof through the existing authority.
    return source_identity(source, tested, accelerator_outputs=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('phase', choices=('prepare', 'verify', 'complete'))
    parser.add_argument('--source', default='.')
    parser.add_argument('--profile', choices=('product', 'shared'), default='product')
    parser.add_argument('--shared-source', default='')
    args = parser.parse_args()
    env = os.environ
    try:
        source = Path(args.source).resolve(strict=True)
        event = json.loads(Path(env['GITHUB_EVENT_PATH']).read_text())
        tested = env['GITHUB_SHA']
        current = current_source(source, tested)
        runtime = validate_context(runtime_context(native=True), native=True)
        shared = None
        if args.profile == 'shared':
            from warm_shared_tools import shared_identity, verify_modules
            if not args.shared_source:
                raise InvalidContext('shared profile requires pinned source')
            shared = Path(args.shared_source).resolve(strict=True)
            shared_proof = shared_identity(shared)
        context = runtime if shared is None else {'runtime': runtime, 'profile': 'shared', 'shared': shared_proof}
        plan = general_plan(context, current, Path(env['RUNNER_TEMP']))
        def guard(expected):
            if current_source(source, tested) != expected or runtime_context(native=True) != runtime:
                raise InvalidContext('source/compiler changed during compilation cache use')
            if shared is not None and shared_identity(shared) != shared_proof:
                raise InvalidContext('shared compiler source changed during cache use')
        state_path = Path(env['RUNNER_TEMP'])/'compilation-cache-plan.json'
        saved = {'plan': asdict(plan), 'context': context}
        if args.phase == 'prepare':
            prepare_paths(plan, clear=True)
            state_path.write_text(json.dumps(saved, sort_keys=True))
            emit({'seed': plan.seed, 'gocache': plan.paths[0], 'gomodcache': plan.paths[1]}, env['GITHUB_OUTPUT'])
        else:
            if json.loads(state_path.read_text()) != json.loads(json.dumps(saved)):
                raise InvalidContext('source/context differs from prepared cache')
            if args.phase == 'verify':
                state = restore_compilation(plan, context, env.get('RESTORE_OUTCOME'),
                                            env.get('MATCHED_KEY'), guard)
                if shared is not None:
                    try:
                        verify_modules(shared)
                    except subprocess.SubprocessError:
                        if state != 'trusted':
                            raise
                        state = restore_compilation(plan, context, 'failure', '', guard)
                        verify_modules(shared)
                    guard(current)
                emit({'restore_state': state}, env['GITHUB_OUTPUT'])
            else:
                guard(current)
                # General consumers are read-only. Only the main seed job
                # calls complete, after its build commands succeeded.
                if not trusted_main(env, event):
                    raise InvalidContext('trusted cache writes require exact push-main event')
                execute_modules(['go', 'mod', 'verify'])
                if shared is not None:
                    verify_modules(shared)
                try:
                    size = publish_manifest(plan, context)
                except InvalidContext:
                    emit({'save_ready': 'false'}, env['GITHUB_OUTPUT'])
                    print('::notice::Compilation exceeds publication budget; continuing uncached')
                    return 0
                guard(current)
                emit({'save_ready': 'true', 'cache_bytes': str(size), 'seed': plan.seed}, env['GITHUB_OUTPUT'])
        return 0
    except (InvalidContext, OSError, ValueError, TypeError, KeyError, subprocess.SubprocessError):
        print('Compilation cache source/context contract failed', file=sys.stderr)
        return 1


if __name__ == '__main__':
    sys.exit(main())
