#!/usr/bin/env python3
"""Stage or promote exact signed release bytes; never replace an asset.

Uses gh's authenticated REST client. A failed mutation is reconciled once by
reading state; partial uploads fail closed and need operator recovery.
"""
import argparse
from datetime import datetime
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import tempfile

BINARIES = [f'agent-archive-{os_name}-{arch}' for os_name in ('darwin', 'linux') for arch in ('amd64', 'arm64')]
MANIFEST = 'release-candidate.json'
ASSETS = BINARIES + ['SHA256SUMS', MANIFEST]
# These are acceptance categories, not claims that any live check has passed.
ROWS = ['signed-install-darwin-amd64', 'signed-install-darwin-arm64',
        'install-linux-amd64', 'install-linux-arm64', 'clean-user', 'upgrade',
        's3', 'r2', 'claude-code', 'codex', 'cursor', 'recovery',
        'cleanup', 'listing-budget', 'documentation', 'repository-settings']
CI_JOBS = {'Test': ['linux-race', 'macos-smoke', 'cross-build', 'lint'],
           'Levenshtein': ['verify'], 'Extended': ['macos-full', 'fuzz', 'real-systemd']}
CI_JOB_PAGE_LIMIT = 10


def require(ok, message):
    if not ok:
        raise RuntimeError(message)


def digest(path):
    with path.open('rb') as source:
        hashed = hashlib.sha256()
        for chunk in iter(lambda: source.read(1024 * 1024), b''):
            hashed.update(chunk)
        return hashed.hexdigest()


def run(args):
    return subprocess.run(args, check=True, capture_output=True, timeout=300).stdout


class GitHub:
    def __init__(self, repo):
        require(re.fullmatch(r'[\w.-]+/[\w.-]+', repo), 'invalid repository')
        self.repo = repo

    def api(self, endpoint, payload=None, missing=False):
        args = ['gh', 'api', f'repos/{self.repo}/{endpoint}']
        if payload is not None:
            args += ['--method', 'PATCH', '--input', '-']
        result = subprocess.run(args, input=json.dumps(payload).encode() if payload is not None else None,
                                capture_output=True, timeout=120)
        if result.returncode and missing and b'(HTTP 404)' in result.stderr:
            return None
        require(result.returncode == 0, f'GitHub API failed: {result.stderr.decode(errors="replace")}')
        return json.loads(result.stdout)

    def release(self, tag):
        return self.api(f'releases/tags/{tag}', missing=True)

    def tag_commit(self, tag):
        obj = self.api(f'git/ref/tags/{tag}')['object']
        for _ in range(8):
            if obj.get('type') == 'commit':
                return obj['sha']
            require(obj.get('type') == 'tag', 'tag does not resolve to a commit')
            obj = self.api(f'git/tags/{obj["sha"]}')['object']
        raise RuntimeError('tag indirection exceeds safe bound')

    def download(self, asset, path):
        path.write_bytes(run(['gh', 'api', f'repos/{self.repo}/releases/assets/{asset["id"]}',
                              '-H', 'Accept: application/octet-stream']))

    def create(self, tag, directory):
        run(['gh', 'release', 'create', tag, '--repo', self.repo, '--verify-tag',
             '--prerelease', '--latest=false', '--title', f'agent-archive {tag}',
             '--notes', 'See CHANGELOG.md for changes and docs/getting-started/install.md for installation. '
             'Checksums are in SHA256SUMS. Candidate acceptance and provenance verification are documented '
             'in dev/maintainers/releasing.md. These exact assets are promoted without rebuilding.',
             *[str(directory / name) for name in ASSETS]])


def version(tag):
    require(re.fullmatch(r'v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)', tag), 'expected canonical vX.Y.Z tag')
    return tuple(map(int, tag[1:].split('.')))


def guard_tag(tag, commit):
    version(tag)
    require(re.fullmatch(r'[0-9a-f]{40}', commit), 'expected full commit SHA')
    require(run(['git', 'rev-parse', f'refs/tags/{tag}^{{commit}}']).decode().strip() == commit,
            'tag does not point to expected commit')
    run(['git', 'merge-base', '--is-ancestor', commit, 'origin/main'])


def make_manifest(directory, tag, commit, repo, run_id, notary_dir):
    version(tag)
    notary = {}
    for arch in ('amd64', 'arm64'):
        result = json.loads((notary_dir / f'notary-{arch}.json').read_text())
        require(result.get('status') == 'Accepted' and result.get('id'), 'notarization not accepted')
        notary[arch] = {'id': result['id'], 'status': 'Accepted'}
    manifest = {'schema': 1, 'tag': tag, 'commit': commit, 'repository': repo,
                'run_id': int(run_id), 'toolchain': 'go1.27.2', 'notarization': notary,
                'sha256': {name: digest(directory / name) for name in BINARIES + ['SHA256SUMS']}}
    (directory / MANIFEST).write_text(json.dumps(manifest, indent=2, sort_keys=True) + '\n')


def verify_security(directory, tag, commit, repo):
    # Every release asset is an attestation subject: the binaries,
    # SHA256SUMS, and the manifest (release.yml attests exactly these).
    for name in ASSETS:
        run(['gh', 'attestation', 'verify', str(directory / name), '--repo', repo,
             '--signer-workflow', f'{repo}/.github/workflows/release.yml',
             '--source-digest', commit, '--source-ref', f'refs/tags/{tag}', '--deny-self-hosted-runners'])
    # Same identity and team as install.sh. Apple recommends codesign's
    # notarized requirement plus online check for other code (raw CLI here),
    # rather than spctl's app assessment. Both gates fail closed independently.
    team = re.search(r'^team_id="([A-Z0-9]+)"$', Path('install.sh').read_text(), re.M)
    require(team is not None, 'installer signing team is missing')
    requirement = ('anchor apple generic and certificate 1[field.1.2.840.113635.100.6.2.6] exists '
                   'and certificate leaf[field.1.2.840.113635.100.6.1.13] exists '
                   f'and certificate leaf[subject.OU] = "{team[1]}"')
    for name in BINARIES[:2]:
        (directory / name).chmod(0o755)
        run(['codesign', '--verify', '--strict', f'-R=identifier "{name}" and {requirement}', str(directory / name)])
        run(['codesign', '--verify', '--strict', '-R=notarized', '--check-notarization', str(directory / name)])


def asset_inventory(release):
    # GET/download increments download_count; that mutable statistic must not
    # make a same-byte retry look like asset replacement. Pin immutable asset
    # identity plus bytes/state and update time, never a count or API URL.
    return sorted((asset['id'], asset['name'], asset['size'], asset.get('digest'),
                   asset.get('state'), asset.get('updated_at')) for asset in release['assets'])


def snapshot(release):
    return {**{key: release[key] for key in ('id', 'tag_name', 'draft', 'prerelease')},
            'assets': asset_inventory(release)}


def verify_release(gh, release, tag, commit, directory, security=verify_security):
    require(gh.tag_commit(tag) == commit, 'remote tag moved or differs from expected commit')
    require(release and release.get('tag_name') == tag and release.get('draft') is False,
            'release missing, draft, or wrong tag; manual recovery required')
    assets = release.get('assets', [])
    require(len(assets) == len(ASSETS) and {a['name'] for a in assets} == set(ASSETS),
            'partial or unexpected asset set; recover original signed artifacts, never overwrite assets')
    directory.mkdir(parents=True, exist_ok=True)
    for asset in assets:
        name = asset['name']
        require(asset.get('state') == 'uploaded', 'asset upload incomplete')
        gh.download(asset, directory / name)
        require((directory / name).stat().st_size == asset['size'], f'asset size mismatch: {name}')
        require(asset.get('digest') == 'sha256:' + digest(directory / name), f'asset digest mismatch: {name}')
    manifest = json.loads((directory / MANIFEST).read_text())
    require(manifest.get('schema') == 1 and manifest.get('tag') == tag and manifest.get('commit') == commit
            and manifest.get('repository') == gh.repo and manifest.get('toolchain') == 'go1.27.2',
            'candidate identity/toolchain mismatch')
    require(type(manifest.get('run_id')) is int and manifest['run_id'] > 0, 'missing signing workflow run ID')
    require(manifest.get('sha256') == {name: digest(directory / name) for name in BINARIES + ['SHA256SUMS']},
            'candidate manifest digests mismatch')
    for arch in ('amd64', 'arm64'):
        receipt = manifest.get('notarization', {}).get(arch, {})
        require(receipt.get('status') == 'Accepted' and receipt.get('id'), 'missing accepted notarization receipt')
    expected = ''.join(f'{digest(directory / name)}  {name}\n' for name in BINARIES)
    require((directory / 'SHA256SUMS').read_text() == expected, 'invalid SHA256SUMS')
    security(directory, tag, commit, gh.repo)
    return {name: digest(directory / name) for name in ASSETS}


def stage(gh, tag, commit, directory, security=verify_security):
    require(gh.tag_commit(tag) == commit, 'remote tag differs from expected commit')
    release = gh.release(tag)
    if release:
        with tempfile.TemporaryDirectory() as scratch:
            hashes = verify_release(gh, release, tag, commit, Path(scratch), security)
            require(hashes == {name: digest(directory / name) for name in ASSETS},
                    'existing candidate differs from local signed bytes; refusing replacement')
        return
    try:
        gh.create(tag, directory)
    except (RuntimeError, subprocess.CalledProcessError, subprocess.TimeoutExpired):
        # Never retry create or upload blindly: an unknown result may have
        # already published some assets, signed with nondeterministic timestamps.
        pass
    release = gh.release(tag)
    require(release and release.get('prerelease') is True, 'candidate creation failed or release state changed')
    with tempfile.TemporaryDirectory() as scratch:
        hashes = verify_release(gh, release, tag, commit, Path(scratch), security)
        require(hashes == {name: digest(directory / name) for name in ASSETS}, 'published bytes differ from signed artifact')


def evidence_row(row):
    require(isinstance(row, dict) and row.get('result') == 'passed', 'acceptance row is missing or pending')
    require(isinstance(row.get('tester'), str) and row['tester'].strip(), 'acceptance tester required')
    require(isinstance(row.get('evidence'), str) and row['evidence'].startswith('https://'), 'acceptance evidence URL required')
    parsed = datetime.fromisoformat(row.get('utc', '').replace('Z', '+00:00'))
    require(parsed.utcoffset() is not None and parsed.utcoffset().total_seconds() == 0, 'acceptance UTC timestamp required')


def verify_acceptance(gh, acceptance, tag, commit, hashes):
    require(acceptance.get('schema') == 1 and acceptance.get('tag') == tag
            and acceptance.get('commit') == commit and acceptance.get('sha256') == hashes,
            'acceptance does not bind exact candidate identity and all six asset digests')
    for name in ROWS:
        evidence_row(acceptance.get('checks', {}).get(name))
    for workflow, names in CI_JOBS.items():
        item = acceptance.get('ci', {}).get(workflow, {})
        evidence_row(item)
        run_id = item.get('run_id')
        require(isinstance(run_id, int) and run_id > 0, 'CI run ID required')
        result = gh.api(f'actions/runs/{run_id}')
        require(result.get('head_sha') == commit and result.get('status') == 'completed'
                and result.get('conclusion') == 'success' and result.get('name') == workflow
                and result.get('repository', {}).get('full_name') == gh.repo
                and result.get('event') == 'push'
                and (result.get('head_branch') == 'main' or result.get('head_branch', '').startswith('release-candidate/')), 'CI run is not a successful exact-commit run')
        # Pagination matters: fuzz campaigns and future matrices can exceed 100 jobs.
        found = []
        # Bound total requests as well as each subprocess. A missing final
        # page leaves CI evidence incomplete, even if required names appeared.
        for page in range(1, CI_JOB_PAGE_LIMIT + 1):
            batch = gh.api(f'actions/runs/{run_id}/jobs?per_page=100&page={page}').get('jobs', [])
            found += batch
            if len(batch) < 100:
                break
        else:
            raise RuntimeError('CI job pagination exceeds safe bound; incomplete inventory')
        for name in names:
            require(any(job.get('name') == name and job.get('conclusion') == 'success'
                        and job.get('head_sha') == commit for job in found), f'required successful CI job missing: {name}')


def guard_latest(gh, tag):
    latest = gh.api('releases/latest', missing=True)
    if latest:
        require(version(latest['tag_name']) <= version(tag), 'refusing to roll latest back to an older version')
    return latest


def promote(gh, tag, commit, acceptance, security=verify_security, tag_guard=guard_tag):
    tag_guard(tag, commit)
    release = gh.release(tag)
    with tempfile.TemporaryDirectory() as scratch:
        hashes = verify_release(gh, release, tag, commit, Path(scratch), security)
    verify_acceptance(gh, acceptance, tag, commit, hashes)
    if release['prerelease'] is False:
        latest = gh.api('releases/latest', missing=True)
        require(latest and (latest['id'] == release['id'] or version(latest['tag_name']) > version(tag)),
                'stable release has unexpected latest state; manual reconciliation required')
        return  # a superseded stable tag is a verified no-op, never a rollback
    guard_latest(gh, tag)
    # Check again after downloads, Apple service and CI calls. Concurrency uses
    # one shared group across both workflows; external owner edits still need
    # the release freeze described in the runbook (REST has no compare-and-swap).
    tag_guard(tag, commit)
    require(gh.tag_commit(tag) == commit, 'remote tag moved during verification')
    require(snapshot(gh.release(tag)) == snapshot(release), 'release changed during verification')
    guard_latest(gh, tag)
    try:
        gh.api(f'releases/{release["id"]}', {'prerelease': False, 'make_latest': 'true'})
    except (RuntimeError, subprocess.TimeoutExpired):
        pass  # reconcile unknown PATCH result, never blindly retry
    after = gh.release(tag)
    require(after and after['prerelease'] is False and after['draft'] is False
            and after['id'] == release['id'] and asset_inventory(after) == asset_inventory(release), 'promotion not confirmed or assets changed')
    require(gh.api('releases/latest')['id'] == release['id'], 'latest promotion not confirmed')
    tag_guard(tag, commit)
    require(gh.tag_commit(tag) == commit, 'remote tag changed after promotion; manual reconciliation required')


def acceptance_path(path):
    require(path is not None and re.fullmatch(r'dev/maintainers/release-evidence/[\w.-]+\.json', str(path)),
            'acceptance must be a committed release-evidence JSON file on main')
    require(not path.is_symlink() and path.resolve().is_relative_to(Path.cwd().resolve()),
            'acceptance path escapes repository or is a symlink')
    require(path.stat().st_size <= 256 * 1024, 'acceptance record exceeds 256 KiB')
    run(['git', 'ls-files', '--error-unmatch', str(path)])
    return json.loads(path.read_text())


def preflight(gh, tag, commit, security=verify_security):
    release = gh.release(tag)
    require(gh.tag_commit(tag) == commit, 'remote tag differs from expected commit')
    if release:
        with tempfile.TemporaryDirectory() as scratch:
            verify_release(gh, release, tag, commit, Path(scratch), security)
        require(snapshot(gh.release(tag)) == snapshot(release), 'release changed during preflight')
        require(gh.tag_commit(tag) == commit, 'remote tag moved during preflight')
    return release is not None


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('mode', choices=['preflight', 'manifest', 'stage', 'promote'])
    parser.add_argument('--tag', default=os.environ.get('VERSION'))
    parser.add_argument('--commit', default=os.environ.get('RELEASE_COMMIT', os.environ.get('GITHUB_SHA')))
    parser.add_argument('--repo', default=os.environ.get('GITHUB_REPOSITORY'))
    parser.add_argument('--dist', type=Path, default=Path('dist'))
    parser.add_argument('--acceptance', type=Path)
    args = parser.parse_args()
    guard_tag(args.tag, args.commit)
    gh = GitHub(args.repo)
    if args.mode == 'manifest':
        make_manifest(args.dist, args.tag, args.commit, args.repo, os.environ['GITHUB_RUN_ID'], Path(os.environ['RUNNER_TEMP']))
    elif args.mode == 'preflight':
        exists = preflight(gh, args.tag, args.commit)
        with open(os.environ['GITHUB_OUTPUT'], 'a') as output:
            output.write(f'exists={str(exists).lower()}\n')
    elif args.mode == 'stage':
        stage(gh, args.tag, args.commit, args.dist)
    else:
        require(os.environ.get('GITHUB_REF') == 'refs/heads/main', 'promotion must run from main')
        promote(gh, args.tag, args.commit, acceptance_path(args.acceptance))


if __name__ == '__main__':
    try:
        main()
    except (RuntimeError, subprocess.CalledProcessError, subprocess.TimeoutExpired, ValueError, KeyError, TypeError, OSError) as error:
        raise SystemExit(f'release refused: {error}')
