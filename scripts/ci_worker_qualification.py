#!/usr/bin/env python3
"""Guard manual shard-topology diagnostics; never a production PR verdict."""
import argparse
import json
import os
from pathlib import Path
import re
import signal
import subprocess


def validate(env, source):
    expected = env.get('EXPECTED_SOURCE', '')
    if not re.fullmatch('[0-9a-f]{40}', expected):
        raise ValueError('expected source must be a full immutable SHA')
    if (env.get('GITHUB_EVENT_NAME') != 'workflow_dispatch'
            or env.get('GITHUB_REPOSITORY') != 'wangjohn/agent-archive'
            or env.get('GITHUB_SHA') != expected
            or env.get('GITHUB_WORKFLOW_SHA') != expected
            or source != expected):
        raise ValueError('manual workflow, definition and checkout must share the expected source')
    if not env.get('GITHUB_WORKFLOW_REF', '').startswith(
            'wangjohn/agent-archive/.github/workflows/linux-worker-qualification.yml@refs/heads/'):
        raise ValueError('qualification requires the selected repository branch workflow')
    workers = env.get('QUALIFICATION_WORKERS')
    if workers not in ('4', '8'):
        raise ValueError('qualification workers must be four or eight')
    return {'schema': 1, 'scope': 'linux-shard-topology-only', 'source': expected,
            'workflow_source': env['GITHUB_WORKFLOW_SHA'], 'workers': int(workers),
            'matrix': {'shard': list(range(int(workers)))},
            'production_verdict': False, 'cache_writes': False}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    action = parser.add_mutually_exclusive_group()
    action.add_argument('--plan', action='store_true')
    action.add_argument('--audit', action='store_true')
    args = parser.parse_args()
    try:
        source = subprocess.check_output(['git', 'rev-parse', 'HEAD'], text=True).strip()
        if subprocess.check_output(['git', 'status', '--porcelain', '--untracked-files=no'], text=True).strip():
            raise ValueError('tracked checkout must be clean')
        result = validate(os.environ, source)
        if args.plan:
            with Path(os.environ['GITHUB_OUTPUT']).open('a') as output:
                output.write('matrix=' + json.dumps(result['matrix'], separators=(',', ':')) + '\n')
                output.write('workers=' + str(result['workers']) + '\n')
            Path('qualification.json').write_text(json.dumps(result, indent=2) + '\n')
        if args.audit:
            # The general tier CLI deliberately promotes manual suites to full.
            # This guarded diagnostic calls the identical owned PR audit body
            # directly, retaining the actual workflow_dispatch event in receipts.
            import run_ci_tier
            def interrupted(signum, frame):
                raise run_ci_tier.Failure('qualification audit cancelled')
            previous = {sig: signal.signal(sig, interrupted) for sig in (signal.SIGTERM, signal.SIGINT)}
            try:
                run_ci_tier.run_suite('pr', 'audit', Path('source-audit-results'),
                                      seconds=780, own_plain_vet=True)
            finally:
                for sig, handler in previous.items():
                    signal.signal(sig, handler)
        print(json.dumps(result, sort_keys=True))
    except (ValueError, OSError, KeyError, subprocess.SubprocessError) as error:
        parser.exit(1, 'Worker qualification guard failed: ' + str(error) + '\n')


if __name__ == '__main__':
    main()
