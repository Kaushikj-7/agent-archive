"""Synthetic API and command fixtures: no credentials, Apple calls or publication."""
import copy
import hashlib
import json
import os
import re
import sys
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

import release_candidate as rc
from test_release_assets import job_text, job_permissions, run_texts, steps

TAG = 'v0.2.0'
COMMIT = 'a' * 40
REPO = 'wangjohn/agent-archive'


class FakeGitHub:
    repo = REPO

    def __init__(self, files):
        self.files = files
        self.current = None
        self.latest = {'id': 1, 'tag_name': 'v0.1.1'}
        self.calls = []
        self.unknown_create = False
        self.unknown_patch = False
        self.partial = False
        self.ci_failure = None
        self.change_before_patch = False
        self.reads = 0
        self.count_downloads = False

    def candidate(self):
        return {'id': 2, 'tag_name': TAG, 'draft': False, 'prerelease': True,
                'assets': [{'id': i + 10, 'name': name, 'size': len(self.files[name]),
                            'state': 'uploaded', 'digest': 'sha256:' + hashlib.sha256(self.files[name]).hexdigest()}
                           for i, name in enumerate(rc.ASSETS)]}

    def release(self, tag):
        self.calls.append(('read', tag))
        self.reads += 1
        result = copy.deepcopy(self.current)
        if self.change_before_patch and self.reads == 2:
            result['assets'][0]['id'] += 100
        return result

    def tag_commit(self, tag):
        return COMMIT

    def download(self, asset, path):
        self.calls.append(('download', asset['name']))
        path.write_bytes(self.files[asset['name']])
        if self.count_downloads:
            for item in self.current['assets']:
                if item['id'] == asset['id']:
                    item['download_count'] = item.get('download_count', 0) + 1

    def create(self, tag, directory):
        self.calls.append(('create', tag))
        self.current = self.candidate()
        if self.partial:
            self.current['assets'].pop()
        if self.unknown_create:
            raise RuntimeError('connection lost after create')

    def api(self, endpoint, payload=None, missing=False):
        self.calls.append(('api', endpoint, payload))
        if endpoint == 'releases/latest':
            return copy.deepcopy(self.latest)
        if payload is not None:
            self.current['prerelease'] = False
            self.latest = self.current
            if self.unknown_patch:
                raise RuntimeError('connection lost after PATCH')
            return copy.deepcopy(self.current)
        run_id = int(endpoint.split('/')[2])
        workflow = list(rc.CI_JOBS)[run_id - 1]
        if '/jobs?' in endpoint:
            return {'jobs': [{'name': name, 'head_sha': COMMIT,
                             'conclusion': 'failure' if name == self.ci_failure else 'success'}
                            for name in rc.CI_JOBS[workflow]]}
        return {'head_sha': COMMIT, 'status': 'completed', 'conclusion': 'success',
                'name': workflow, 'repository': {'full_name': REPO}, 'event': 'push',
                'head_branch': 'release-candidate/test'}


class CandidateTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.dist = Path(self.temp.name) / 'dist'
        self.dist.mkdir()
        for name in rc.BINARIES:
            (self.dist / name).write_bytes(f'signed fixture {name}'.encode())
        (self.dist / 'SHA256SUMS').write_text(''.join(f'{rc.digest(self.dist/name)}  {name}\n' for name in rc.BINARIES))
        self.notary = Path(self.temp.name)
        for arch in ('amd64', 'arm64'):
            (self.notary / f'notary-{arch}.json').write_text(json.dumps({'id': arch, 'status': 'Accepted'}))
        rc.make_manifest(self.dist, TAG, COMMIT, REPO, 123, self.notary)
        self.files = {name: (self.dist / name).read_bytes() for name in rc.ASSETS}
        self.gh = FakeGitHub(self.files)
        self.security_calls = []
        self.security = lambda *args: self.security_calls.append(args)
        row = {'result': 'passed', 'tester': 'Fixture Tester', 'utc': '2026-10-04T00:00:00Z', 'evidence': 'https://example.test/evidence'}
        self.acceptance = {'schema': 1, 'tag': TAG, 'commit': COMMIT,
                           'sha256': {name: rc.digest(self.dist/name) for name in rc.ASSETS},
                           'checks': {name: copy.deepcopy(row) for name in rc.ROWS},
                           'ci': {name: dict(row, run_id=i+1) for i, name in enumerate(rc.CI_JOBS)}}

    def stage(self):
        rc.stage(self.gh, TAG, COMMIT, self.dist, self.security)

    def promote(self):
        rc.promote(self.gh, TAG, COMMIT, self.acceptance, self.security, lambda *args: None)

    def mutations(self):
        return [c for c in self.gh.calls if c[0] == 'create' or (c[0] == 'api' and c[2] is not None)]

    def ready(self):
        self.gh.current = self.gh.candidate()

    def test_stage_creates_prerelease_once_and_same_byte_retry_is_read_only(self):
        self.stage()
        self.assertTrue(self.gh.current['prerelease'])
        self.assertEqual(self.gh.latest['id'], 1)
        self.stage()
        self.assertEqual(self.mutations(), [('create', TAG)])
        self.assertEqual(len(self.security_calls), 2)

    def test_unknown_create_reconciles_exact_assets_without_retrying_mutation(self):
        self.gh.unknown_create = True
        self.stage()
        self.assertEqual(self.mutations(), [('create', TAG)])

    def test_partial_unknown_create_fails_closed_and_retry_does_not_create_or_upload(self):
        self.gh.unknown_create = self.gh.partial = True
        with self.assertRaisesRegex(RuntimeError, 'partial'):
            self.stage()
        with self.assertRaisesRegex(RuntimeError, 'partial'):
            self.stage()
        self.assertEqual(self.mutations(), [('create', TAG)])

    def test_existing_candidate_never_overwrites_new_signing_timestamp_bytes(self):
        self.ready()
        (self.dist / rc.BINARIES[0]).write_bytes(b'new signing timestamp')
        with self.assertRaisesRegex(RuntimeError, 'refusing replacement'):
            self.stage()
        self.assertEqual(self.mutations(), [])

    def test_tampered_download_refused_before_signature_or_promotion(self):
        self.ready()
        self.files[rc.BINARIES[0]] = b'tampered'
        with self.assertRaisesRegex(RuntimeError, 'size mismatch|digest mismatch'):
            self.promote()
        self.assertEqual(self.mutations(), [])
        self.assertEqual(self.security_calls, [])

    def test_draft_missing_duplicate_extra_or_incomplete_assets_refused(self):
        for kind in ('draft', 'missing', 'duplicate', 'extra', 'incomplete', 'digest'):
            with self.subTest(kind=kind):
                self.ready()
                if kind == 'draft':
                    self.gh.current['draft'] = True
                elif kind == 'missing':
                    self.gh.current['assets'].pop()
                elif kind == 'duplicate':
                    self.gh.current['assets'][0] = self.gh.current['assets'][1]
                elif kind == 'extra':
                    self.gh.current['assets'].append({'name': 'extra'})
                elif kind == 'incomplete':
                    self.gh.current['assets'][0]['state'] = 'starter'
                else:
                    self.gh.current['assets'][0]['digest'] = None
                with self.assertRaises(RuntimeError):
                    self.promote()
        self.assertEqual(self.mutations(), [])

    def test_manifest_wrong_commit_toolchain_or_notary_refused_even_with_api_digest(self):
        for key, value in [('commit', 'b'*40), ('toolchain', 'go1.0'), ('toolchain', 'go1.27.1'), ('notarization', {})]:
            with self.subTest(key=key):
                m = json.loads(self.files[rc.MANIFEST])
                m[key] = value
                self.files[rc.MANIFEST] = json.dumps(m).encode()
                self.ready()
                with self.assertRaises(RuntimeError):
                    self.promote()
                self.files[rc.MANIFEST] = (self.dist / rc.MANIFEST).read_bytes()
        self.assertEqual(self.mutations(), [])

    def test_checksums_cannot_disagree_with_manifest(self):
        self.files['SHA256SUMS'] = b'invalid checksum format\n'
        m = json.loads(self.files[rc.MANIFEST])
        m['sha256']['SHA256SUMS'] = hashlib.sha256(self.files['SHA256SUMS']).hexdigest()
        self.files[rc.MANIFEST] = json.dumps(m).encode()
        self.ready()
        with self.assertRaisesRegex(RuntimeError, 'SHA256SUMS'):
            self.promote()

    def test_security_failure_never_promotes(self):
        self.ready()
        def reject(*args):
            raise subprocess.CalledProcessError(1, ['codesign'])
        self.security = reject
        with self.assertRaises(subprocess.CalledProcessError):
            self.promote()
        self.assertEqual(self.mutations(), [])

    def test_complete_acceptance_promotes_same_release_only_and_retry_is_noop(self):
        self.ready()
        self.promote()
        assets = copy.deepcopy(self.gh.current['assets'])
        self.promote()
        self.assertEqual(self.mutations(), [('api', 'releases/2', {'prerelease': False, 'make_latest': 'true'})])
        self.assertEqual(self.gh.current['assets'], assets)
        self.assertFalse(self.gh.current['prerelease'])
        self.assertEqual(self.gh.latest['id'], 2)

    def test_unknown_patch_success_reconciles_without_second_patch(self):
        self.ready()
        self.gh.unknown_patch = True
        self.promote()
        self.assertEqual(len(self.mutations()), 1)

    def test_unknown_patch_failure_remains_unconfirmed(self):
        self.ready()
        original = self.gh.api
        def api(endpoint, payload=None, missing=False):
            if payload is not None:
                raise RuntimeError('disconnected before update')
            return original(endpoint, payload, missing)
        self.gh.api = api
        with self.assertRaisesRegex(RuntimeError, 'not confirmed'):
            self.promote()

    def test_old_candidate_cannot_roll_latest_back_and_old_stable_retry_is_noop(self):
        self.ready()
        self.gh.latest = {'id': 3, 'tag_name': 'v0.3.0'}
        with self.assertRaisesRegex(RuntimeError, 'roll latest back'):
            self.promote()
        self.gh.current['prerelease'] = False
        self.promote()
        self.assertEqual(self.mutations(), [])

    def test_replacement_during_verification_refused(self):
        self.ready()
        self.gh.change_before_patch = True
        with self.assertRaisesRegex(RuntimeError, 'changed during verification'):
            self.promote()
        self.assertEqual(self.mutations(), [])

    def test_pending_missing_or_unattributed_acceptance_refused(self):
        for key, value in [('result', 'pending'), ('tester', ''), ('evidence', ''), ('utc', '2026-10-04')]:
            with self.subTest(key=key):
                self.ready()
                self.acceptance['checks'][rc.ROWS[0]][key] = value
                with self.assertRaises((RuntimeError, ValueError)):
                    self.promote()
        self.assertEqual(self.mutations(), [])

    def test_acceptance_requires_every_supported_app_and_no_unsupported_app(self):
        # The product catalog and published acceptance sheet support three
        # apps. An unsupported fourth row made honest acceptance impossible.
        root = Path(__file__).resolve().parent.parent
        catalog = (root / 'internal/agentmeta/catalog.go').read_text()
        identities = set(re.findall(r'(?m)^\s*\w+\s+ID\s*=\s*"([a-z-]+)"$', catalog))
        self.assertEqual(identities, {'claude', 'codex', 'cursor'})
        app_rows = {'claude-code' if name == 'claude' else name for name in identities}
        template = json.loads((root / 'dev/maintainers/release-evidence/example.json').read_text())
        self.assertEqual(set(template['checks']), set(rc.ROWS))
        self.assertTrue(app_rows <= set(rc.ROWS))
        self.ready()
        self.acceptance['checks'] = {name: row for name, row in self.acceptance['checks'].items()
                                     if name != 'gemini'}
        self.promote()
        self.assertEqual(len(self.mutations()), 1)
        for name in sorted(app_rows):
            with self.subTest(missing_app=name):
                row = self.acceptance['checks'].pop(name)
                self.ready()
                before = len(self.mutations())
                with self.assertRaisesRegex(RuntimeError, 'missing or pending'):
                    self.promote()
                self.assertEqual(len(self.mutations()), before)
                self.acceptance['checks'][name] = row

    def test_acceptance_must_bind_all_six_digests(self):
        self.ready()
        self.acceptance['sha256'].pop(rc.MANIFEST)
        with self.assertRaisesRegex(RuntimeError, 'bind exact'):
            self.promote()

    def test_failing_required_exact_commit_ci_job_blocks(self):
        self.ready()
        self.gh.ci_failure = 'fuzz'
        with self.assertRaisesRegex(RuntimeError, 'fuzz'):
            self.promote()
        self.assertEqual(self.mutations(), [])

    def test_wrong_commit_workflow_event_branch_or_repo_ci_blocks(self):
        original = self.gh.api
        for key, value in [('head_sha', 'b'*40), ('name', 'Other'), ('event', 'pull_request'),
                           ('head_branch', 'feature'), ('repository', {'full_name': 'other/repo'})]:
            with self.subTest(key=key):
                self.ready()
                def api(endpoint, payload=None, missing=False):
                    result = original(endpoint, payload, missing)
                    if endpoint.startswith('actions/runs/') and '/jobs?' not in endpoint:
                        result[key] = value
                    return result
                self.gh.api = api
                with self.assertRaisesRegex(RuntimeError, 'exact-commit'):
                    self.promote()
        self.assertEqual(self.mutations(), [])

    def test_unaccepted_notary_result_cannot_create_manifest(self):
        (self.notary/'notary-arm64.json').write_text('{"status":"Invalid","id":"rejected"}')
        with self.assertRaisesRegex(RuntimeError, 'not accepted'):
            rc.make_manifest(self.dist, TAG, COMMIT, REPO, 123, self.notary)

    def test_tag_guard_resolves_annotated_tag_commit_and_requires_main(self):
        calls = []
        def git(args):
            calls.append(args)
            return (COMMIT+'\n').encode()
        with patch.object(rc, 'run', git):
            rc.guard_tag(TAG, COMMIT)
        self.assertIn(['git', 'rev-parse', f'refs/tags/{TAG}^{{commit}}'], calls)
        self.assertIn(['git', 'merge-base', '--is-ancestor', COMMIT, 'origin/main'], calls)
        with patch.object(rc, 'run', return_value=('b'*40+'\n').encode()):
            with self.assertRaisesRegex(RuntimeError, 'expected commit'):
                rc.guard_tag(TAG, COMMIT)


    def test_preflight_missing_candidate_does_not_sign_or_mutate(self):
        self.assertFalse(rc.preflight(self.gh, TAG, COMMIT, self.security))
        self.assertEqual(self.security_calls, [])
        self.assertEqual(self.mutations(), [])

    def test_preflight_verifies_existing_candidate_and_blocks_partial_before_rebuilding(self):
        self.ready()
        self.assertTrue(rc.preflight(self.gh, TAG, COMMIT, self.security))
        self.gh.current['assets'].pop()
        with self.assertRaisesRegex(RuntimeError, 'partial'):
            rc.preflight(self.gh, TAG, COMMIT, self.security)
        self.assertEqual(self.mutations(), [])

    def test_download_count_changes_do_not_invalidate_exact_asset_identity(self):
        self.ready()
        self.gh.count_downloads = True
        self.assertTrue(rc.preflight(self.gh, TAG, COMMIT, self.security))
        self.promote()
        self.assertEqual(len(self.mutations()), 1)

    def test_remote_tag_move_during_acceptance_blocks_patch(self):
        self.ready()
        values = iter([COMMIT, 'b'*40])
        self.gh.tag_commit = lambda tag: next(values)
        with self.assertRaisesRegex(RuntimeError, 'remote tag moved'):
            self.promote()
        self.assertEqual(self.mutations(), [])

    def test_ci_jobs_paginate_and_never_accept_skipped_required_jobs(self):
        self.ready()
        original = self.gh.api
        def api(endpoint, payload=None, missing=False):
            if '/jobs?' in endpoint and endpoint.endswith('page=1'):
                return {'jobs': [{'name': 'other', 'conclusion': 'success', 'head_sha': COMMIT}]*100}
            return original(endpoint, payload, missing)
        self.gh.api = api
        self.promote()
        self.assertEqual(len([c for c in self.gh.calls if c[0] == 'api' and '/jobs?' in c[1] and 'page=2' in c[1]]), 3)

    def test_ci_job_pagination_is_bounded_and_incomplete_inventory_never_promotes(self):
        self.ready()
        original = self.gh.api
        pages = []
        def api(endpoint, payload=None, missing=False):
            if '/jobs?' in endpoint:
                page = int(endpoint.rsplit('=', 1)[1])
                pages.append(page)
                if page > 10:
                    raise AssertionError('unbounded request after ten full pages')
                # Finding all required names on early pages does not establish
                # that the remaining inventory is complete.
                required = original(endpoint, payload, missing)['jobs']
                return {'jobs': required + [{'name': 'other', 'conclusion': 'success',
                                            'head_sha': COMMIT}] * (100 - len(required))}
            return original(endpoint, payload, missing)
        self.gh.api = api
        with self.assertRaisesRegex(RuntimeError, 'safe bound'):
            self.promote()
        self.assertEqual(pages, list(range(1, 11)))
        self.assertEqual(self.mutations(), [])

    def test_real_gh_subprocess_fixture_reconciles_unknown_create_and_never_overwrites(self):
        # Exercise the real command adapter with a fake gh executable and API
        # persisted between calls; no network or authenticated client exists.
        fixture = Path(self.temp.name)/'fixture.json'
        fixture.write_text(json.dumps({'release': None, 'assets': self.gh.candidate()['assets'], 'calls': []}))
        shim = Path(self.temp.name)/'gh'
        shim.write_text('#!' + sys.executable + '\n' + r"""
import json, os, sys
from pathlib import Path
p = Path(os.environ['FAKE_GH_STATE'])
s = json.loads(p.read_text()); args = sys.argv[1:]; s['calls'].append(args)
if args[:2] == ['release', 'create']:
    assert '--prerelease' in args and '--latest=false' in args and '--verify-tag' in args
    s['release'] = {'id': 2, 'tag_name': 'v0.2.0', 'draft': False, 'prerelease': True, 'assets': s['assets']}
    p.write_text(json.dumps(s))
    sys.exit(1) # unknown outcome after all exact bytes uploaded
p.write_text(json.dumps(s))
endpoint = args[1]
if '/git/ref/tags/' in endpoint:
    print(json.dumps({'object': {'type':'commit', 'sha':'a'*40}}))
elif '/releases/tags/' in endpoint:
    if s['release'] is None:
        print('gh: Not Found (HTTP 404)', file=sys.stderr); sys.exit(1)
    print(json.dumps(s['release']))
elif '/releases/assets/' in endpoint:
    asset = next(a for a in s['assets'] if a['id'] == int(endpoint.rsplit('/',1)[1]))
    sys.stdout.buffer.write((Path(os.environ['FAKE_GH_DIST'])/asset['name']).read_bytes())
else:
    raise RuntimeError('unexpected command ' + repr(args))
""")
        shim.chmod(0o755)
        env = {'PATH': str(shim.parent)+os.pathsep+os.environ['PATH'],
               'FAKE_GH_STATE': str(fixture), 'FAKE_GH_DIST': str(self.dist)}
        with patch.dict(os.environ, env):
            gh = rc.GitHub(REPO)
            rc.stage(gh, TAG, COMMIT, self.dist, self.security)
            self.assertTrue(rc.preflight(gh, TAG, COMMIT, self.security))
        calls = json.loads(fixture.read_text())['calls']
        self.assertEqual(len([c for c in calls if c[:2] == ['release','create']]), 1)
        self.assertFalse(any('upload' in c or '--clobber' in c for c in calls))


class CommandsTest(unittest.TestCase):
    def test_create_is_prerelease_latest_false_existing_tag_and_never_clobbers(self):
        with patch.object(rc, 'run', return_value=b'') as command:
            rc.GitHub(REPO).create(TAG, Path('dist'))
        args = command.call_args.args[0]
        for arg in ('--prerelease', '--latest=false', '--verify-tag'):
            self.assertIn(arg, args)
        self.assertNotIn('--clobber', args)
        self.assertEqual(args[-6:], [f'dist/{name}' for name in rc.ASSETS])

    def test_binary_download_uses_asset_id_and_octet_stream(self):
        with tempfile.TemporaryDirectory() as temp, patch.object(rc, 'run', return_value=b'bytes') as command:
            dest = Path(temp)/'asset'
            rc.GitHub(REPO).download({'id': 22}, dest)
            self.assertEqual(dest.read_bytes(), b'bytes')
            self.assertEqual(command.call_args.args[0], ['gh', 'api', f'repos/{REPO}/releases/assets/22', '-H', 'Accept: application/octet-stream'])

    def test_only_explicit_404_is_missing_not_auth_or_network_error(self):
        for code in (403, 500, 404):
            result = subprocess.CompletedProcess([], 1, b'', f'gh: error (HTTP {code})'.encode())
            with patch.object(rc.subprocess, 'run', return_value=result):
                if code == 404:
                    self.assertIsNone(rc.GitHub(REPO).release(TAG))
                else:
                    with self.assertRaises(RuntimeError):
                        rc.GitHub(REPO).release(TAG)

    def test_security_verifies_manifest_and_binaries_constrained_provenance_and_darwin_only(self):
        with tempfile.TemporaryDirectory() as temp, patch.object(rc, 'run', return_value=b'') as command:
            directory = Path(temp)
            for name in rc.BINARIES:
                (directory/name).write_bytes(b'fixture')
            rc.verify_security(directory, TAG, COMMIT, REPO)
        calls = [c.args[0] for c in command.call_args_list]
        provenance = [c for c in calls if c[0] == 'gh']
        self.assertEqual(len(provenance), 5)
        for args in provenance:
            self.assertIn('--deny-self-hosted-runners', args)
            self.assertEqual(args[args.index('--source-digest')+1], COMMIT)
            self.assertEqual(args[args.index('--source-ref')+1], f'refs/tags/{TAG}')
            self.assertEqual(args[args.index('--signer-workflow')+1], f'{REPO}/.github/workflows/release.yml')
        for args in calls:
            if args[0] in ('codesign', 'spctl'):
                self.assertIn('darwin', args[-1])
                self.assertNotIn('linux', args[-1])
        self.assertEqual(len([c for c in calls if c[0]=='codesign']), 4)
        self.assertFalse(any(c[0] == 'spctl' for c in calls))
        signature = [c for c in calls if c[0]=='codesign' and '--check-notarization' not in c]
        tickets = [c for c in calls if '--check-notarization' in c]
        self.assertEqual(len(signature), 2)
        self.assertEqual(len(tickets), 2)
        for args in signature:
            self.assertIn('subject.OU', ' '.join(args))
            self.assertIn('identifier', ' '.join(args))
        for args in tickets:
            self.assertIn('-R=notarized', args)
            self.assertIn('--strict', args)


    def test_signature_and_online_notarization_each_fail_closed(self):
        for gate in ('signature', 'notarization'):
            with self.subTest(gate=gate), tempfile.TemporaryDirectory() as temp:
                directory = Path(temp)
                for name in rc.BINARIES:
                    (directory/name).write_bytes(b'fixture')
                def command(args):
                    if args[0] == 'codesign' and (('--check-notarization' in args) == (gate == 'notarization')):
                        raise subprocess.CalledProcessError(1, args)
                    return b''
                with patch.object(rc, 'run', side_effect=command):
                    with self.assertRaises(subprocess.CalledProcessError):
                        rc.verify_security(directory, TAG, COMMIT, REPO)

    def test_network_commands_have_explicit_timeouts(self):
        result = subprocess.CompletedProcess([], 0, b'{}', b'')
        with patch.object(rc.subprocess, 'run', return_value=result) as command:
            rc.GitHub(REPO).api('releases/latest')
            self.assertEqual(command.call_args.kwargs['timeout'], 120)
            rc.run(['gh', 'attestation', 'verify', 'fixture'])
            self.assertEqual(command.call_args.kwargs['timeout'], 300)


class AcceptancePathTest(unittest.TestCase):
    def test_only_bounded_tracked_repository_relative_regular_json_is_accepted(self):
        with tempfile.TemporaryDirectory() as temp:
            old = Path.cwd()
            try:
                os.chdir(temp)
                directory = Path('dev/maintainers/release-evidence')
                directory.mkdir(parents=True)
                path = directory/'candidate.json'
                path.write_text('{"schema":1}')
                with patch.object(rc, 'run', return_value=b'') as git:
                    self.assertEqual(rc.acceptance_path(path), {'schema':1})
                    self.assertEqual(git.call_args.args[0], ['git', 'ls-files', '--error-unmatch', str(path)])
                    for other in (None, Path('/tmp/file.json'), directory/'../file.json', directory/'candidate.yaml'):
                        with self.subTest(path=other), self.assertRaises(RuntimeError):
                            rc.acceptance_path(other)
                    path.write_bytes(b'x'*(256*1024+1))
                    with self.assertRaisesRegex(RuntimeError, 'exceeds'):
                        rc.acceptance_path(path)
                    path.unlink()
                    path.symlink_to('../../../outside.json')
                    with self.assertRaisesRegex(RuntimeError, 'symlink'):
                        rc.acceptance_path(path)
                    path.unlink()
                    directory.rmdir()
                    directory.symlink_to('/tmp')
                    with self.assertRaisesRegex(RuntimeError, 'escapes'):
                        rc.acceptance_path(directory/'file.json')
            finally:
                os.chdir(old)

    def test_untracked_record_is_rejected_and_pending_template_is_not_a_pass(self):
        template = Path(__file__).resolve().parent.parent/'dev/maintainers/release-evidence/example.json'
        value = json.loads(template.read_text())
        for row in list(value['checks'].values()) + list(value['ci'].values()):
            with self.assertRaisesRegex(RuntimeError, 'pending'):
                rc.evidence_row(row)
        with patch.object(rc, 'run', side_effect=subprocess.CalledProcessError(1,['git'])):
            with self.assertRaises(subprocess.CalledProcessError):
                rc.acceptance_path(Path('dev/maintainers/release-evidence/example.json'))


class WorkflowTest(unittest.TestCase):
    def setUp(self):
        self.root = Path(__file__).resolve().parent.parent
        self.release = (self.root/'.github/workflows/release.yml').read_text()
        self.promotion = (self.root/'.github/workflows/promote-release.yml').read_text()

    def test_retry_preflight_is_read_only_and_all_rebuilding_waits_for_missing_candidate(self):
        self.assertEqual(job_permissions(job_text(self.release, 'preflight')), {'contents':'read', 'attestations':'read'})
        self.assertIn('python3 scripts/release_candidate.py preflight', job_text(self.release, 'preflight'))
        for job in ('build', 'build-linux', 'publish'):
            self.assertIn("if: needs.preflight.outputs.exists == 'false'", job_text(self.release, job))
            self.assertIn('preflight', job_text(self.release, job))

    def test_promotion_is_main_only_protected_manual_no_signing_build_or_upload(self):
        self.assertIn('  workflow_dispatch:', self.promotion)
        self.assertNotIn('  push:', self.promotion)
        promote = job_text(self.promotion, 'promote')
        self.assertIn("if: github.ref == 'refs/heads/main'", promote)
        self.assertIn('environment: release-promotion', promote)
        self.assertEqual(job_permissions(promote), {'contents':'write','attestations':'read','actions':'read'})
        for marker in ('secrets.', 'APPLE_', 'id-token:', 'build-release', 'setup-go@', 'upload-artifact@', 'download-artifact@'):
            self.assertNotIn(marker, self.promotion)
        self.assertIn('PROMOTION_ENABLED: ${{ vars.PROMOTION_ENABLED }}', promote)
        self.assertIn('test "$PROMOTION_ENABLED" = true', promote)
        for body in run_texts(self.promotion):
            self.assertNotIn('${{', body)

    def test_publication_workflows_share_non_cancelling_serialization_and_pin_actions(self):
        for workflow in (self.release, self.promotion):
            self.assertIn('group: release-publication', workflow)
            self.assertIn('cancel-in-progress: false', workflow)
            import re
            for action in re.findall(r'(?m)^\s*(?:- )?uses: (\S+)', workflow):
                self.assertRegex(action, r'@[0-9a-f]{40}$')

    def test_manifest_generated_after_notary_and_attested_before_staging_with_retained_signed_artifact(self):
        publish = job_text(self.release, 'publish')
        self.assertLess(publish.index('name: Notarize'), publish.index('release_candidate.py manifest'))
        self.assertLess(publish.index('release_candidate.py manifest'), publish.index('actions/attest-build-provenance@'))
        self.assertLess(publish.index('retention-days: 30'), publish.index('release_candidate.py stage'))


if __name__ == '__main__':
    unittest.main()
