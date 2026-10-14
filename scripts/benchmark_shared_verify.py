#!/usr/bin/env python3
"""Manual diagnostic of pinned shared checker concurrency; never a CI verdict."""
import argparse
from collections import Counter
import hashlib
import json
import os
from pathlib import Path
import shutil
import signal
import statistics
import subprocess
import tempfile
import time

import go_cache_context as context
import stage_native_verify as staging

ORDER = (4, 2, 2, 4)
CHECKS = {'lint', 'vet', 'modules', 'vulnerabilities', 'workflow-lint', 'workflow-security'}
MAX_CACHE_BYTES = 3 * 1024 ** 3


def write(path, value):
    path.write_text(json.dumps(value, indent=2, sort_keys=True) + '\n')


def tree_digest(root, limit=None):
    """Bound and fingerprint regular cache/staged bytes; reject aliases."""
    digest = hashlib.sha256()
    total = 0
    for path in sorted(root.rglob('*')):
        if path.is_symlink():
            raise ValueError('benchmark inputs contain a symlink')
        if path.is_dir():
            continue
        if not path.is_file():
            raise ValueError('benchmark inputs are not regular files')
        size = path.stat().st_size
        total += size
        if limit is not None and total > limit:
            raise ValueError('benchmark compilation cache exceeds 3 GiB diagnostic bound')
        digest.update(json.dumps([str(path.relative_to(root)), size]).encode())
        with path.open('rb') as stream:
            for block in iter(lambda: stream.read(1024 * 1024), b''):
                digest.update(block)
    return {'sha256': digest.hexdigest(), 'bytes': total}


def validate_report(report, plan):
    """A faster missing/cached/failed check is not a useful observation."""
    expected = Counter(check['id'] for check in plan['checks'])
    if (report.get('version') != 1 or report.get('status') != 'passed'
            or report.get('plan') != plan
            or Counter(item['id'] for item in report.get('results', [])) != expected):
        raise ValueError('shared report does not prove the complete successful plan')
    for result in report['results']:
        if (result.get('status') != 'passed'
                or result.get('cache', {}).get('status') not in ('miss', 'disabled')):
            raise ValueError('every benchmark check must execute freshly and pass')


def summarize(observations):
    if [item['jobs'] for item in observations] != list(ORDER):
        raise ValueError('the complete balanced 4/2/2/4 sequence is required')
    medians = {str(jobs): statistics.median(item['seconds'] for item in observations
                                          if item['jobs'] == jobs) for jobs in (4, 2)}
    return {'median_seconds': medians,
            'two_vs_four_ratio': medians['2'] / medians['4'],
            'production_jobs': 4,
            'qualification': 'manual diagnostic only; two observations per variant are not a p95',
            'decision': 'retain 4 until independent hosted evidence and review support a change'}


def git(source, *args):
    return subprocess.check_output(['git', '-C', str(source), *args]).decode().strip()


def execute(shared, source, directory, cache_directory, jobs, env, plan):
    directory.mkdir()
    command = [str(shared / 'verify'), 'pre-merge', '--source', str(source),
               '--jobs', str(jobs), '--cache-dir', str(cache_directory), '--format', 'json']
    started = time.monotonic()
    with (directory / 'report.json').open('w') as stdout, (directory / 'stderr.log').open('w') as stderr:
        process = subprocess.Popen(command, env=env, stdout=stdout, stderr=stderr, start_new_session=True)
        try:
            code = process.wait(timeout=900)
        except subprocess.TimeoutExpired:
            # A stopped diagnostic must not leave checker/compiler children
            # writing into scratch while its owner removes that directory.
            os.killpg(process.pid, signal.SIGTERM)
            try:
                process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                pass
            try:
                os.killpg(process.pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
            process.wait()
            code = 124
    seconds = time.monotonic() - started
    write(directory / 'invocation.json', {'command': command, 'seconds': seconds, 'exit_code': code})
    if code:

        raise ValueError(f'fresh shared verify failed: {directory.name}; inspect raw report')
    report = json.loads((directory / 'report.json').read_text())
    validate_report(report, plan)
    return {'jobs': jobs, 'seconds': seconds,
            'checks': [{'id': item['id'], 'duration_ms': item['duration_ms'],
                        'execution_ms': item['execution_ms'], 'cache': item['cache']}
                       for item in report['results']]}


def benchmark(source, shared, output, scratch):
    if output.exists():
        raise ValueError('output must be a new directory')
    for root in (source, shared):
        if output == root or root in output.parents or scratch == root or root in scratch.parents:
            raise ValueError('benchmark output and scratch must be outside checkouts')
    pin = context.TOOL_PINS['shared']
    tested = git(source, 'rev-parse', 'HEAD')
    if git(shared, 'rev-parse', 'HEAD') != pin:
        raise ValueError('shared checkout is not the reviewed immutable pin')
    proofs = {'product': context.source_identity(source, tested),
              'shared': context.source_identity(shared, pin)}
    compiler = context.runtime_context()
    context.validate_context(compiler)
    output.mkdir(parents=True)
    metadata = {'schema': 1, 'source': proofs, 'compiler': compiler, 'shared_pin': pin,
                'order': list(ORDER), 'observations': [], 'status': 'incomplete'}
    write(output / 'benchmark.json', metadata)
    # Only the helper's task-owned work is removed; no external cache is mutated.
    with tempfile.TemporaryDirectory(prefix='shared-benchmark-', dir=scratch) as temporary:
        work = Path(temporary)
        build = work / 'build'
        initial_cache = os.environ.get('GOCACHE')
        if initial_cache and Path(initial_cache).is_dir():
            tree_digest(Path(initial_cache), MAX_CACHE_BYTES)
            shutil.copytree(initial_cache, build)
        else:
            build.mkdir()
        env = dict(os.environ, GOCACHE=str(build), XDG_CACHE_HOME=str(work / 'planner-xdg'))
        original_plan = json.loads(subprocess.check_output(
            [str(shared / 'verify'), 'pre-merge', '--source', str(source), '--dry-run'], env=env))
        if Counter(item['id'] for item in original_plan['checks']) != Counter(CHECKS):
            raise ValueError('pinned full pre-merge check inventory changed; review benchmark policy')
        config = json.loads((source / 'levenshtein.json').read_text())
        staged = work / 'source'
        staging.stage(source, staged, config, original_plan)
        # Fresh results on each invocation already prevent reuse; explicit rerun
        # also requests current scanner data under the pinned shared interface.
        config['runs']['pre-merge']['rerun_checks'] = True
        write(staged / 'levenshtein.json', config)
        plan = json.loads(subprocess.check_output(
            [str(shared / 'verify'), 'pre-merge', '--source', str(staged), '--dry-run'], env=env))
        write(output / 'plan.json', plan)
        staged_proof = tree_digest(staged)
        metadata['staged_source'] = staged_proof
        warm_env = dict(env, XDG_CACHE_HOME=str(work / 'warmup-xdg'))
        metadata['warmup'] = execute(shared, staged, output / 'warmup', work / 'warmup-verification', 4, warm_env, plan)
        # Save only diagnostic reports, never verification/tool/compiler caches.
        shutil.rmtree(work / 'warmup-verification')
        snapshot = work / 'baseline'
        build.rename(snapshot)
        baseline = tree_digest(snapshot, MAX_CACHE_BYTES)
        metadata['warm_compilation_baseline'] = baseline
        for index, jobs in enumerate(ORDER):
            if tree_digest(staged) != staged_proof:
                raise ValueError('staged inputs changed between observations')
            copy_started = time.monotonic()
            shutil.copytree(snapshot, build)
            if tree_digest(build, MAX_CACHE_BYTES) != baseline:
                raise ValueError('variant compilation baseline differs')
            preparation_seconds = time.monotonic() - copy_started
            label = f'{index + 1}-jobs-{jobs}'
            variant_env = dict(env, XDG_CACHE_HOME=str(work / (label + '-xdg')))
            observation = execute(shared, staged, output / label, work / label, jobs, variant_env, plan)
            observation['baseline_copy_verify_seconds'] = preparation_seconds
            observation['baseline'] = baseline
            metadata['observations'].append(observation)
            shutil.rmtree(work / label)
            write(output / 'benchmark.json', metadata)
            shutil.rmtree(build)
        if tree_digest(staged) != staged_proof or tree_digest(snapshot, MAX_CACHE_BYTES) != baseline:
            raise ValueError('staged source or immutable compilation baseline changed')
        if (context.source_identity(source, tested) != proofs['product']
                or context.source_identity(shared, pin) != proofs['shared']
                or context.runtime_context() != compiler):
            raise ValueError('source or full compiler context changed during benchmark')
        metadata.update(status='complete', summary=summarize(metadata['observations']))
        write(output / 'benchmark.json', metadata)
    return metadata


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--source', required=True, type=Path)
    parser.add_argument('--shared', required=True, type=Path)
    parser.add_argument('--output', required=True, type=Path)
    parser.add_argument('--scratch', required=True, type=Path)
    args = parser.parse_args()
    result = benchmark(args.source.resolve(), args.shared.resolve(), args.output.resolve(), args.scratch.resolve())
    print(json.dumps(result['summary'], indent=2))


if __name__ == '__main__':
    main()
