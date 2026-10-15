#!/usr/bin/env python3
"""Adapters for verified Go caches; no external verdicts, receipts or XDG state.

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
PRODUCT_PRIORITY = 'compilation-product-priority.json'


def go_object_key(key):
    return isinstance(key, str) and re.fullmatch(r'0/[0-9a-f]{2}/[0-9a-f]{64}-[ad]', key) is not None


def product_priority_path(runner_temp):
    path = Path(runner_temp)/PRODUCT_PRIORITY
    if path.is_symlink() or (path.exists() and not path.is_file()):
        raise InvalidContext('product priority is not a regular file')
    return path


def capture_product_priority(plan, context, runner_temp):
    # Capture identities, never copy object bytes. This is uncached producer
    # metadata, taken before checker compilation adds recoverable build objects.
    keys = sorted(key for _, key, _, _ in cache_files((plan.paths[0],)) if go_object_key(key))
    payload = json.dumps({'schema': 1, 'source': plan.source,
                          'context': digest(context), 'objects': keys}, sort_keys=True)
    if len(payload.encode()) > 8 * 1024 * 1024:
        raise InvalidContext('product priority exceeds metadata budget')
    product_priority_path(runner_temp).write_text(payload)


def read_product_priority(plan, context, runner_temp):
    path = product_priority_path(runner_temp)
    if path.stat().st_size > 8 * 1024 * 1024:
        raise InvalidContext('product priority exceeds metadata budget')
    value = json.loads(path.read_text())
    if (not isinstance(value, dict) or set(value) != {'schema', 'source', 'context', 'objects'}
            or type(value['schema']) is not int or value['schema'] != 1
            or value['source'] != plan.source or value['context'] != digest(context)
            or not isinstance(value['objects'], list)
            or any(not go_object_key(key) for key in value['objects'])
            or value['objects'] != sorted(set(value['objects']))):
        raise InvalidContext('product priority source/context differs')
    return frozenset(value['objects'])


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


def bound_compilation(plan, files=None, preferred=()):
    """Drop recoverable Go objects, oldest first within each priority tier.

    Module contents, pinned binaries, manifests and receipts are never edited.
    Preserve 98% of the cap for manifest room. Missing Go objects trigger normal
    fresh compilation. Enumerated metadata avoids an extra tree walk/stat pass.
    """
    files = cache_files(plan.paths) if files is None else files
    size = sum(item[2] for item in files)
    target = int(MAX_BYTES * .98)
    removed = 0
    preferred = frozenset(preferred)
    if any(not go_object_key(key) for key in preferred):
        raise InvalidContext('product priority contains invalid Go object identity')
    candidates = []
    for item in files:
        path, key, length, modified = item
        if go_object_key(key):
            candidates.append(item)
    discarded = set()
    # A checker executable is already retained separately. Its newer build
    # intermediates must not displace product objects solely by mtime. Priority
    # is soft: even product objects remain recoverable when they exceed the cap.
    for path, key, length, modified in sorted(candidates, key=lambda item: (item[1] in preferred, item[3], item[1])):
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


def manifest_path(plan):
    path = Path(plan.paths[0])/MANIFEST
    if path.is_symlink() or (path.exists() and not path.is_file()):
        raise InvalidContext('cache manifest is not a regular file')
    return path


def publish_manifest(plan, context, kind='seed', *, preferred=()):
    target = manifest_path(plan)
    paths = snapshot_paths(plan, kind)
    for path in paths:
        Path(path).mkdir(parents=True, exist_ok=True)
    candidates = cache_files(paths)
    bound_compilation(plan, candidates, preferred)
    files, size = file_inventory(paths, candidates)
    payload = json.dumps({
        'schema': SCHEMA, 'kind': kind, 'context': digest(context),
        'dependency': plan.source['dependency'], 'source': plan.source,
        'files': files, 'bytes': size}, sort_keys=True)
    if len(payload.encode()) > 32 * 1024 * 1024:
        raise InvalidContext('cache manifest exceeds metadata budget')
    target.write_text(payload)
    return size


def validate_manifest(plan, context, kind='seed'):
    path = manifest_path(plan)
    if path.stat().st_size > 32 * 1024 * 1024:
        raise InvalidContext('cache manifest exceeds metadata budget')
    manifest = json.loads(path.read_text())
    if not isinstance(manifest, dict) or manifest.get('kind') != kind:
        raise InvalidContext('compilation manifest kind differs')
    paths = snapshot_paths(plan, manifest.get('kind'))
    files, size = file_inventory(paths)
    if (kind == 'seed' and context.get('go', {}).get('GOOS') == 'darwin'
            and size + path.stat().st_size > MAX_BYTES):
        raise InvalidContext('retained macOS seed exceeds publication budget')
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


def restore_compilation(plan, context, outcome, matched, guard, command=execute_modules, *, observation=None):
    """Corrupt/missing accelerators are discarded, then redownloaded and verified."""
    state, accepted_bytes = 'miss', None
    if observation is not None:
        observation.clear()
    try:
        if outcome != 'success' or matched != plan.seed:
            raise InvalidContext('trusted seed unavailable')
        accepted_bytes = validate_manifest(plan, context)
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
        state, accepted_bytes = 'corrupt-rebuilt', None
        command(['go', 'mod', 'download'])
        command(['go', 'mod', 'verify'])
    guard(plan.source)
    if observation is not None:
        observation.update(state=state, raw_payload_bytes=accepted_bytes)
    return state


def observe_cache(phase, profile, state, raw_payload_bytes):
    # Bytes are the already-validated/captured uncompressed payload inventory,
    # excluding manifest metadata. Never infer archive transfer/storage bytes.
    print('compilation-cache-observation: ' + json.dumps({
        'schema': 1, 'phase': phase, 'profile': profile, 'state': state,
        'raw_payload_bytes': raw_payload_bytes}, sort_keys=True))


def current_source(source, tested):
    # Cache consumers may have written only reviewed evidence/release outputs.
    # Pass the same tracked-byte/mode proof through the existing authority.
    return source_identity(source, tested, accelerator_outputs=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('phase', choices=('prepare', 'verify', 'prioritize-product', 'complete'))
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
        if args.phase != 'prepare' and (env.get('GOCACHE') != plan.paths[0]
                                            or env.get('GOMODCACHE') != plan.paths[1]):
            raise InvalidContext('Go caches differ from controlled paths')
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
                observation = {}
                state = restore_compilation(plan, context, env.get('RESTORE_OUTCOME'),
                                            env.get('MATCHED_KEY'), guard, observation=observation)
                if shared is not None:
                    try:
                        verify_modules(shared)
                    except subprocess.SubprocessError:
                        if state != 'trusted':
                            raise
                        state = restore_compilation(plan, context, 'failure', '', guard, observation=observation)
                        verify_modules(shared)
                    guard(current)
                # Emit only the final outcome after shared-module retry and
                # all source/compiler guards. A discarded hit retains no bytes.
                emit({'restore_state': state}, env['GITHUB_OUTPUT'])
                observe_cache('restore', args.profile, state, observation['raw_payload_bytes'])
            else:
                guard(current)
                # General consumers are read-only. Only the main seed job
                # calls complete, after its build commands succeeded.
                if not trusted_main(env, event):
                    raise InvalidContext('trusted cache writes require exact push-main event')
                if args.phase == 'prioritize-product':
                    if args.profile != 'product' or runtime['go']['GOOS'] != 'darwin':
                        raise InvalidContext('product priority requires the native macOS product producer')
                    capture_product_priority(plan, context, Path(env['RUNNER_TEMP']))
                    guard(current)
                    return 0
                execute_modules(['go', 'mod', 'verify'])
                if shared is not None:
                    verify_modules(shared)
                try:
                    preferred = (read_product_priority(plan, context, Path(env['RUNNER_TEMP']))
                                 if args.profile == 'product' and runtime['go']['GOOS'] == 'darwin' else ())
                    size = publish_manifest(plan, context, preferred=preferred)
                    if args.profile == 'product' and runtime['go']['GOOS'] == 'darwin':
                        # Recount the actual saved paths after eviction and manifest
                        # creation. Non-evictable bytes can consume the reserved
                        # metadata room; never publish beyond the existing cap.
                        retained = sum(item[2] for item in cache_files(snapshot_paths(plan, 'seed')))
                        retained += manifest_path(plan).stat().st_size
                        if retained > MAX_BYTES:
                            raise InvalidContext('retained macOS seed exceeds publication budget')
                except InvalidContext:
                    emit({'save_ready': 'false'}, env['GITHUB_OUTPUT'])
                    observe_cache('capture', args.profile, 'not-captured', None)
                    print('::notice::Compilation exceeds publication budget; continuing uncached')
                    return 0
                guard(current)
                emit({'save_ready': 'true', 'cache_bytes': str(size), 'seed': plan.seed}, env['GITHUB_OUTPUT'])
                observe_cache('capture', args.profile, 'ready-for-save', size)
        return 0
    except (InvalidContext, OSError, ValueError, TypeError, KeyError, subprocess.SubprocessError):
        print('Compilation cache source/context contract failed', file=sys.stderr)
        return 1


if __name__ == '__main__':
    sys.exit(main())
