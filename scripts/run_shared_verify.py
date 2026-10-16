#!/usr/bin/env python3
"""Run fresh shared verification unchanged; retain only bounded measured metrics."""
import argparse
import json
import math
import os
from pathlib import Path
import subprocess
import sys
import time

MAX_REPORT = 8 * 1024 * 1024
CHECKS = {'lint', 'vet', 'modules', 'vulnerabilities', 'workflow-lint', 'workflow-security'}


def measured(report):
    if report.get('version') != 1 or not isinstance(report.get('results'), list) or not report['results']:
        raise ValueError('invalid shared timing report')
    rows, seen = [], set()
    for item in report['results']:
        ident = item.get('id')
        duration = item.get('duration_ms')
        execution = item.get('execution_ms')
        if (ident not in CHECKS or ident in seen
                or type(duration) not in (int, float) or not math.isfinite(duration) or duration < 0
                or (execution is not None and (type(execution) not in (int, float)
                    or not math.isfinite(execution) or execution < 0))):
            raise ValueError('invalid shared check measurement')
        seen.add(ident)
        rows.append({'check': ident, 'phase': 'fresh-analysis-including-startup',
                     'seconds': duration / 1000, 'execution_seconds': None if execution is None else execution / 1000,
                     'status': item.get('status'), 'cache_state': item.get('cache', {}).get('status', 'unknown')})
    return rows


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--output', type=Path, required=True)
    parser.add_argument('--source', type=Path, required=True)
    parser.add_argument('command', nargs=argparse.REMAINDER)
    args = parser.parse_args()
    command = args.command[1:] if args.command[:1] == ['--'] else args.command
    if not command:
        parser.error('command required')
    source = subprocess.check_output(['git', '-C', str(args.source), 'rev-parse', 'HEAD'], text=True).strip()
    started = time.monotonic()
    chunks, count = [], 0
    process = subprocess.Popen(command + ['--format', 'json'], stdout=subprocess.PIPE)
    for line in iter(lambda: process.stdout.read(65536), b''):
        sys.stdout.buffer.write(line)
        sys.stdout.buffer.flush()
        count += len(line)
        if count <= MAX_REPORT:
            chunks.append(line)
    code = process.wait()
    receipt = {'schema': 1, 'kind': 'shared-verification', 'source': source,
               'run_id': os.environ.get('GITHUB_RUN_ID'), 'run_attempt': os.environ.get('GITHUB_RUN_ATTEMPT'),
               'runner_image': os.environ.get('ImageVersion', 'unknown'),
               'cache_state': os.environ.get('CI_CACHE_RESTORE_STATE', 'unknown'),
               'seconds': time.monotonic() - started, 'exit_code': code, 'checks': [],
               'note': 'Checks may overlap; never add check durations to infer wall time. Tool startup remains included.'}
    try:
        if count > MAX_REPORT:
            raise ValueError('shared report exceeds diagnostic bound')
        receipt['checks'] = measured(json.loads(b''.join(chunks)))
    except (ValueError, TypeError, KeyError, AttributeError) as error:
        receipt['measurement_error'] = str(error)
        code = code or 1
    args.output.parent.mkdir(parents=True, exist_ok=True)
    args.output.write_text(json.dumps(receipt, indent=2) + '\n')
    return code


if __name__ == '__main__':
    sys.exit(main())
