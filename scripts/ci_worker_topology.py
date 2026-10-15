#!/usr/bin/env python3
"""Resolve the captured PR's exhaustive topology; no API calls or new runner."""
import argparse
import json
import os
from pathlib import Path
import re
import subprocess


DEFAULT_WORKERS = 8
PREFIX = 'ci-qualification/'
SHA = re.compile(r'[0-9a-f]{40}')
QUALIFIER = re.compile(r'ci-qualification/workers-([48])/[a-z0-9][a-z0-9-]{0,39}/[a-z0-9][a-z0-9-]{0,39}')


def selection(event, repository):
    pr = event['pull_request']
    head = pr['head']
    # GitHub equality/startsWith are case insensitive. Python must agree.
    same_repo = head['repo']['full_name'].casefold() == repository.casefold()
    branch = head['ref']
    if not isinstance(branch, str):
        raise ValueError('head branch unavailable')
    match = QUALIFIER.fullmatch(branch.casefold()) if same_repo else None
    if same_repo and branch.casefold().startswith(PREFIX) and not match:
        raise ValueError('malformed reserved qualification branch')
    return (int(match[1]), 'full-pipeline-experiment') if match else (DEFAULT_WORKERS, 'normal')


def validate(env, event, source, tree, parents):
    repository = env.get('GITHUB_REPOSITORY', '')
    pr = event['pull_request']
    number = pr['number']
    if (env.get('GITHUB_EVENT_NAME') != 'pull_request'
            or repository != 'wangjohn/agent-archive'
            or event['repository']['full_name'].casefold() != repository.casefold()
            or type(number) is not int or number <= 0
            or event.get('number') != number
            or pr['base']['repo']['full_name'].casefold() != repository.casefold()):
        raise ValueError('requires the actual repository PR event')
    ref = f'refs/pull/{number}/merge'
    if (env.get('GITHUB_REF') != ref
            or env.get('GITHUB_HEAD_REF') != pr['head']['ref']
            or env.get('GITHUB_BASE_REF') != pr['base']['ref']
            or env.get('GITHUB_SHA') != source
            or env.get('GITHUB_WORKFLOW_SHA') != source
            or env.get('GITHUB_WORKFLOW_REF') != repository + '/.github/workflows/test.yml@' + ref
            or any(not isinstance(s, str) or not SHA.fullmatch(s)
                   for s in (source, tree, pr['base']['sha'], pr['head']['sha']))
            or parents != [pr['base']['sha'], pr['head']['sha']]):
        raise ValueError('workflow, checkout, event and merge parents differ')
    workers, observation = selection(event, repository)
    if env.get('WORKERS') is not None and env['WORKERS'] != str(workers):
        raise ValueError('matrix count differs from captured PR topology')
    shard = env.get('SHARD')
    if shard is not None and shard not in tuple(map(str, range(workers))):
        raise ValueError('shard outside captured PR topology')
    return {'schema': 1, 'repository': repository, 'pr_number': number,
            'workers': workers, 'observation_kind': observation,
            'source': source, 'tree': tree, 'parents': parents,
            'head': pr['head']['sha'], 'head_ref': pr['head']['ref'],
            'head_repository': pr['head']['repo']['full_name'], 'base_ref': pr['base']['ref'],
            'workflow_source': env['GITHUB_WORKFLOW_SHA'],
            'workflow_ref': env['GITHUB_WORKFLOW_REF']}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--output', type=Path, required=True)
    parser.add_argument('--lint-base-ref', help='Record actual revive diff endpoints without changing them')
    args = parser.parse_args()
    try:
        def git(*argv):
            return subprocess.check_output(['git', *argv], text=True, timeout=10).strip()
        source = git('rev-parse', 'HEAD')
        # Read raw commit headers: this works on shallow audit checkouts too.
        parents = [line.split()[1] for line in git('cat-file', '-p', source).split('\n\n', 1)[0].splitlines()
                   if line.startswith('parent ')]
        if git('status', '--porcelain', '--untracked-files=no'):
            raise ValueError('tracked source must be clean')
        result = validate(os.environ, json.loads(Path(os.environ['GITHUB_EVENT_PATH']).read_text()),
                          source, git('rev-parse', 'HEAD^{tree}'), parents)
        if args.lint_base_ref is not None:
            if args.lint_base_ref != os.environ['GITHUB_BASE_REF']:
                raise ValueError('lint ref differs from captured PR base ref')
            ref = 'origin/' + args.lint_base_ref
            resolved = git('rev-parse', '--verify', ref+'^{commit}')
            merge_bases = git('merge-base', '--all', source, resolved).splitlines()
            if not SHA.fullmatch(resolved) or not merge_bases or any(not SHA.fullmatch(v) for v in merge_bases):
                raise ValueError('lint diff endpoints unavailable')
            result['lint_diff'] = {'base_ref': ref, 'resolved_base': resolved,
                                   'tested': source, 'merge_bases': sorted(merge_bases)}
        args.output.write_text(json.dumps(result, indent=2) + '\n')
        with Path(os.environ['GITHUB_ENV']).open('a') as handle:
            handle.write('WORKERS=' + str(result['workers']) + '\n')
        print(json.dumps(result, sort_keys=True))
    except (ValueError, KeyError, TypeError, AttributeError, OSError, subprocess.SubprocessError) as error:
        parser.exit(1, 'Worker topology guard failed: ' + str(error) + '\n')


if __name__ == '__main__':
    main()
