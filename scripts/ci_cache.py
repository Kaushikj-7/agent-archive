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
MAX_BYTES = 1024 * 1024 * 1024
MANIFEST = '.aa-compilation-manifest.json'


def file_inventory(paths):
    """Reject links/devices and checksum bytes before letting Go consume them."""
    result = {}
    size = 0
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
                if path.is_symlink() or not path.is_file():
                    raise InvalidContext('cache member is not regular')
                size += path.stat().st_size
                if size > MAX_BYTES:
                    raise InvalidContext('cache exceeds one GiB publication budget')
                sha = hashlib.sha256()
                with path.open('rb') as handle:
                    for chunk in iter(lambda: handle.read(1024 * 1024), b''):
                        sha.update(chunk)
                result[f'{index}/{path.relative_to(root).as_posix()}'] = sha.hexdigest()
    return result, size


def bound_compilation(plan):
    """Drop only Go's recoverable action/object entries, oldest first.

    Module contents, pinned binaries, manifests and receipts are never edited.
    A missing Go cache object is an ordinary cold compilation, not a skipped
    check. Preserve 90% of the cap as the publication target for manifest room.
    """
    size = sum(path.stat().st_size for raw in plan.paths for path in Path(raw).rglob('*')
               if path.is_file() and not path.is_symlink())
    target = int(MAX_BYTES * .9)
    if size <= target:
        return 0
    candidates = []
    build = Path(plan.paths[0])
    for directory in build.iterdir():
        if not re.fullmatch(r'[0-9a-f]{2}', directory.name) or directory.is_symlink() or not directory.is_dir():
            continue
        for path in directory.iterdir():
            if (not path.is_symlink() and path.is_file()
                    and re.fullmatch(r'[0-9a-f]{64}-[ad]', path.name)):
                candidates.append(path)
    removed = 0
    for path in sorted(candidates, key=lambda item: item.stat().st_mtime_ns):
        if size <= target:
            break
        length = path.stat().st_size
        path.unlink()
        size -= length
        removed += length
    return removed


def publish_manifest(plan, context):
    bound_compilation(plan)
    files, size = file_inventory(plan.paths)
    (Path(plan.paths[0])/MANIFEST).write_text(json.dumps({
        'schema': SCHEMA, 'context': digest(context),
        'dependency': plan.source['dependency'], 'source': plan.source,
        'files': files, 'bytes': size}, sort_keys=True))
    return size


def validate_manifest(plan, context):
    manifest = json.loads((Path(plan.paths[0])/MANIFEST).read_text())
    files, size = file_inventory(plan.paths)
    if (not isinstance(manifest, dict) or set(manifest) != {
            'schema', 'context', 'dependency', 'source', 'files', 'bytes'}
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
    args = parser.parse_args()
    env = os.environ
    try:
        source = Path(args.source).resolve(strict=True)
        event = json.loads(Path(env['GITHUB_EVENT_PATH']).read_text())
        tested = env['GITHUB_SHA']
        current = current_source(source, tested)
        context = validate_context(runtime_context(native=True), native=True)
        plan = general_plan(context, current, Path(env['RUNNER_TEMP']))
        def guard(expected):
            if current_source(source, tested) != expected or runtime_context(native=True) != context:
                raise InvalidContext('source/compiler changed during compilation cache use')
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
                emit({'restore_state': state}, env['GITHUB_OUTPUT'])
            else:
                guard(current)
                # General consumers are read-only. Only the main seed job
                # calls complete, after its build commands succeeded.
                if not trusted_main(env, event):
                    raise InvalidContext('trusted cache writes require exact push-main event')
                execute_modules(['go', 'mod', 'verify'])
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
