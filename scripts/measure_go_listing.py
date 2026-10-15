#!/usr/bin/env python3
"""Bounded local listing/binary diagnostic. Never used by CI or for admission."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import shutil
import signal
import re
import subprocess
import tarfile
import tempfile
import time

import full_race
import go_parent_shards
import run_go_shard

ROOT = Path(__file__).resolve().parents[1]
PATTERN = '^(Test|Fuzz|Example)'
PACKAGES = [run_go_shard.CLI, *sorted(go_parent_shards.HEAVY)]


def measure(go, output):
    """Measure two local listings and five independently compiled test binaries."""
    source = full_race.source_proof(ROOT)
    output.mkdir(parents=True, exist_ok=False)
    go = str(Path(shutil.which(go) or go).resolve(strict=True))
    base_env = os.environ.copy()
    config = json.loads(subprocess.check_output(
        [go, 'env', '-json', *go_parent_shards.CONFIG_FIELDS, 'GOMODCACHE'],
        cwd=ROOT, env=base_env, text=True))
    if config['GOVERSION'] != 'go1.27.2' or config['GOFLAGS']:
        raise ValueError('use exact Go 1.27.2 with empty GOFLAGS')
    rows = []
    # One overall budget and the production supervisor own every process group.
    deadline = time.monotonic() + 600
    with tempfile.TemporaryDirectory(prefix='listing-diagnostic-') as scratch:
        env = dict(base_env, HOME=scratch, XDG_CONFIG_HOME=scratch,
                   AGENT_ARCHIVE_HOME=str(Path(scratch) / 'archive'),
                   GOCACHE=str(Path(scratch) / 'go-cache'),
                   GOMODCACHE=config.pop('GOMODCACHE'), GOPROXY='off', GOSUMDB='off')
        def run(label, command):
            seconds = run_go_shard.supervise([(command, output / (label + '.log'), env)], deadline)[0]
            rows.append({'phase': label, 'command': command, 'seconds': seconds})
        combined = [go, 'test', '-race', '-short', '-count=1', '-json', '-p', '1', '-list', PATTERN, *PACKAGES]
        run('combined-empty-cache', combined)
        run('combined-same-cache', combined)
        expected = go_parent_shards.listing((output / 'combined-same-cache.log').read_text(), PACKAGES)
        if go_parent_shards.listing((output / 'combined-empty-cache.log').read_text(), PACKAGES) != expected:
            raise ValueError('compiled listings differ between cache states')
        binaries = []
        for index, package in enumerate(PACKAGES):
            binary = Path(scratch) / ('package-' + str(index) + '.test')
            run('compile-' + str(index), [go, 'test', '-race', '-short', '-count=1', '-p', '1', '-c', '-o', str(binary), package])
            binaries.append(binary)
            run('binary-list-' + str(index), [str(binary), '-test.short', '-test.count=1', '-test.list', PATTERN])
            names = [line for line in (output / ('binary-list-' + str(index) + '.log')).read_text().splitlines()
                     if re.fullmatch(r'(Test|Fuzz|Example)\w*', line)]
            if sorted(names) != expected[package]:
                raise ValueError('binary listing differs from compiled discovery')
        # This is local compression, not an upload/download or trusted artifact.
        archive = Path(scratch) / 'binaries.tar.gz'
        start = time.monotonic()
        with tarfile.open(archive, 'w:gz') as bundle:
            for binary in binaries:
                bundle.add(binary, arcname=binary.name)
        compression_seconds = time.monotonic() - start
        sizes = [{'package': package, 'bytes': binary.stat().st_size,
                  'sha256': hashlib.sha256(binary.read_bytes()).hexdigest()}
                 for package, binary in zip(PACKAGES, binaries)]
        packed_bytes = archive.stat().st_size
    if full_race.source_proof(ROOT) != source:
        raise ValueError('source changed during diagnostic')
    value = {'schema': 1, 'kind': 'local-listing-diagnostic', **source,
             'configuration': config, 'packages': PACKAGES, 'measurements': rows,
             'binaries': sizes, 'gzip_bytes': packed_bytes,
             'compression_seconds': compression_seconds,
             'hosted_transfer_seconds': None, 'producer_dependency_wait_seconds': None,
             'production_sharing': 'not-established',
             'limitations': 'Local serial diagnostic only. Empty private build cache; existing module cache, network disabled. Combined phase includes vet/compile/link/race startup/TestMain; binary listing includes race startup/TestMain. No bodies or hosted producer, transfer, scheduling, allocation or p95 comparison. Binary listing is not production coverage evidence.'}
    (output / 'measurement.json').write_text(json.dumps(value, indent=2) + '\n')
    return value


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--go', default='go')
    parser.add_argument('--output', required=True, type=Path)
    args = parser.parse_args()
    output = args.output.resolve()

    def interrupted(signum, frame):
        raise run_go_shard.Failure('listing diagnostic cancelled by signal ' + str(signum))
    handlers = {sig: signal.signal(sig, interrupted) for sig in (signal.SIGTERM, signal.SIGINT)}
    os.chdir(ROOT)
    try:
        measure(args.go, output)
    except (ValueError, KeyError, TypeError, OSError, subprocess.SubprocessError, run_go_shard.Failure) as error:
        parser.exit(1, 'Cannot measure listing: ' + str(error) + '\n')
    finally:
        for sig, handler in handlers.items():
            signal.signal(sig, handler)


if __name__ == '__main__':
    main()
