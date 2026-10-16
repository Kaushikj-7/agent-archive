#!/usr/bin/env python3
"""Retain a small exact-event receipt; offline consumers authenticate its ZIP digest."""
import argparse
import json
import os
from pathlib import Path
import subprocess

import ci_worker_topology

PATHS = {'Test': '.github/workflows/test.yml', 'Levenshtein': '.github/workflows/levenshtein.yml'}


def capture(env, event, git):
    name = env['GITHUB_WORKFLOW']
    path = PATHS[name]
    source = git('rev-parse', 'HEAD')
    if git('rev-parse', '--is-shallow-repository') != 'false' or git('status', '--porcelain', '--untracked-files=no'):
        raise ValueError('identity requires clean full-history checkout')
    parents = [line.split()[1] for line in git('cat-file', '-p', source).split('\n\n', 1)[0].splitlines()
               if line.startswith('parent ')]
    def ancestor(base, head):
        return git('merge-base', base, head) == base
    topology = ci_worker_topology.validate(env, event, source, git('rev-parse', 'HEAD^{tree}'),
                                          parents, ancestor, workflow_path=path)
    run_id, attempt = int(env['GITHUB_RUN_ID']), int(env['GITHUB_RUN_ATTEMPT'])
    if run_id <= 0 or not 1 <= attempt <= 10:
        raise ValueError('invalid run identity')
    return {'schema': 1, 'run_id': run_id, 'run_attempt': attempt,
            'workflow': name, 'workflow_path': path, 'event': 'pull_request', 'topology': topology}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--output', type=Path, required=True)
    args = parser.parse_args()
    try:
        def git(*argv):
            return subprocess.check_output(['git', *argv], text=True, timeout=10).strip()
        receipt = capture(os.environ, json.loads(Path(os.environ['GITHUB_EVENT_PATH']).read_text()), git)
        args.output.parent.mkdir(parents=True, exist_ok=True)
        args.output.write_text(json.dumps(receipt, indent=2) + '\n')
    except (KeyError, ValueError, TypeError, OSError, subprocess.SubprocessError) as error:
        parser.exit(1, 'Cannot retain run identity: ' + str(error) + '\n')


if __name__ == '__main__':
    main()
