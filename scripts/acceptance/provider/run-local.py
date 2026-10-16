#!/usr/bin/env python3
"""Run synthetic fixtures against a private loopback MinIO, with explicit opt-in."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import secrets
import signal
import socket
import subprocess
import sys
import tempfile
import time
import urllib.request

MODULES = {'minio': 'v0.0.0-20260212201848-7aac2a2c5b7c',
           'mc': 'v0.0.0-20251106162529-77f82e18b540'}
TESTS = '^(TestDisposableProviderPortableListingKeepsCanonicalCoverage|TestDisposableProviderReadySelectionRecoversExactCommit|TestDisposableProviderStagedGroupSurvivesNativeLoss)$'


def policy(prefix):
    bucket = 'arn:aws:s3:::aa-disposable-acceptance'
    return {'Version': '2012-10-17', 'Statement': [
        {'Effect': 'Allow', 'Action': ['s3:GetBucketLocation'], 'Resource': [bucket]},
        {'Effect': 'Allow', 'Action': ['s3:ListBucket'], 'Resource': [bucket],
         'Condition': {'StringLike': {'s3:prefix': [prefix + '/*']}}},
        {'Effect': 'Allow', 'Action': ['s3:GetObject', 's3:PutObject', 's3:DeleteObject'],
         'Resource': [bucket + '/' + prefix + '/*']}]}


def interrupted(_signum, _frame):
    raise KeyboardInterrupt


def main():
    signal.signal(signal.SIGTERM, interrupted)
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--run-disposable-provider', action='store_true', required=True)
    parser.add_argument('--tools', type=Path, required=True)
    parser.add_argument('--output', type=Path, required=True)
    args = parser.parse_args()
    tools = args.tools.resolve()
    args.output.mkdir(parents=True, exist_ok=False)
    output = args.output.resolve()
    root = Path(__file__).resolve().parents[3]
    identities = {}
    for tool, version in MODULES.items():
        binary = tools / tool
        identity = subprocess.check_output(['go', 'version', '-m', str(binary)], text=True)
        if f'\tmod\tgithub.com/minio/{tool}\t{version}\t' not in identity:
            raise ValueError('provider tool has an unexpected module identity')
        identities[tool] = {'build': identity, 'sha256': hashlib.sha256(binary.read_bytes()).hexdigest()}
    candidate = subprocess.check_output(['git', 'rev-parse', 'HEAD'], cwd=root, text=True).strip()
    dirty = subprocess.check_output(['git', 'status', '--porcelain'], cwd=root, text=True)
    if dirty:
        raise ValueError('commit the exact fixture candidate before acceptance')
    (output / 'provenance.json').write_text(json.dumps({'candidate': candidate, 'tools': identities}, indent=2))
    with tempfile.TemporaryDirectory(prefix='aa-provider-') as temporary:
        work = Path(temporary)
        (work / 'data').mkdir()
        with socket.socket() as sock:
            sock.bind(('127.0.0.1', 0))
            port = sock.getsockname()[1]
        endpoint = f'http://127.0.0.1:{port}'
        admin, admin_secret = 'admin' + secrets.token_hex(8), secrets.token_hex(24)
        access, secret = 'owner' + secrets.token_hex(8), secrets.token_hex(24)
        peer, peer_secret = 'peer' + secrets.token_hex(8), secrets.token_hex(24)
        prefix = 'aa-provider-' + secrets.token_hex(8)
        base_env = {key: value for key, value in os.environ.items()
                    if not key.startswith(('MINIO_', 'AWS_', 'AA_PROVIDER_'))
                    and key.lower() not in ('http_proxy', 'https_proxy', 'all_proxy')}
        env = dict(base_env, MINIO_ROOT_USER=admin, MINIO_ROOT_PASSWORD=admin_secret,
                   HOME=str(work), MINIO_BROWSER='off')
        # Service diagnostics stay private and ephemeral. Only test events and
        # credential-free provenance leave this owned temporary directory.
        with (work / 'service.log').open('wb') as service_log:
            process = subprocess.Popen([str(tools / 'minio'), 'server', '--address', f'127.0.0.1:{port}',
                                        '--console-address', '127.0.0.1:0', str(work / 'data')],
                                       env=env, stdout=service_log, stderr=subprocess.STDOUT)
            try:
                for _ in range(120):
                    if process.poll() is not None:
                        raise RuntimeError('disposable provider exited before readiness')
                    try:
                        with urllib.request.urlopen(endpoint + '/minio/health/ready', timeout=1):
                            break
                    except OSError:
                        time.sleep(0.25)
                else:
                    raise RuntimeError('disposable provider readiness timed out')

                def mc(*arguments):
                    subprocess.run([str(tools / 'mc'), '--config-dir', str(work / 'mc'), *arguments],
                                   check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                                   timeout=30, env=env)

                mc('alias', 'set', 'acceptance', endpoint, admin, admin_secret)
                mc('mb', 'acceptance/aa-disposable-acceptance')
                policy_path = work / 'policy.json'
                policy_path.write_text(json.dumps(policy(prefix)))
                mc('admin', 'policy', 'create', 'acceptance', 'fixtures', str(policy_path))
                for user, password in ((access, secret), (peer, peer_secret)):
                    mc('admin', 'user', 'add', 'acceptance', user, password)
                    mc('admin', 'policy', 'attach', 'acceptance', 'fixtures', '--user', user)
                test_env = dict(base_env, HOME=str(work / 'home'), TMPDIR=str(work),
                                AGENT_ARCHIVE_PROVIDER_ACCEPTANCE='1', AA_PROVIDER_ENDPOINT=endpoint,
                                AA_PROVIDER_BUCKET='aa-disposable-acceptance', AA_PROVIDER_RUN_PREFIX=prefix,
                                AA_PROVIDER_ACCESS=access, AA_PROVIDER_SECRET=secret,
                                AA_PROVIDER_PEER_ACCESS=peer, AA_PROVIDER_PEER_SECRET=peer_secret)
                (work / 'home').mkdir()
                command = ['go', 'test', '-json', '-race', '-p', '2', '-short', '-count=1',
                           '-timeout', '20m', '-run', TESTS, './internal/listingindex', './internal/collector']
                with (output / 'tests.jsonl').open('wb') as events:
                    tests = subprocess.Popen(command, cwd=root, env=test_env, stdout=events,
                                             stderr=subprocess.STDOUT, start_new_session=True)
                    try:
                        returncode = tests.wait(timeout=1500)
                    except BaseException:
                        os.killpg(tests.pid, signal.SIGTERM)
                        try:
                            tests.wait(timeout=15)
                        except subprocess.TimeoutExpired:
                            os.killpg(tests.pid, signal.SIGKILL)
                            tests.wait(timeout=15)
                        raise
                (output / 'command.json').write_text(json.dumps({'command': command, 'exit_code': returncode}))
                events_path = output / 'tests.jsonl'
                contents = events_path.read_bytes()
                leaked = False
                for credential in (admin, admin_secret, access, secret, peer, peer_secret):
                    if credential.encode() in contents:
                        leaked = True
                        contents = contents.replace(credential.encode(), b'[REDACTED]')
                events_path.write_bytes(contents)
                if leaked:
                    raise RuntimeError('credential found in evidence; redacted and acceptance refused')
                if returncode:
                    raise RuntimeError('provider fixtures failed; inspect test events')
            finally:
                # A timeout or signal must also sanitize partial evidence.
                partial = output / 'tests.jsonl'
                if partial.exists():
                    contents = partial.read_bytes()
                    for credential in (admin, admin_secret, access, secret, peer, peer_secret):
                        contents = contents.replace(credential.encode(), b'[REDACTED]')
                    partial.write_bytes(contents)
                process.terminate()
                try:
                    process.wait(timeout=15)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait(timeout=15)
                (output / 'cleanup.json').write_text(json.dumps({'owned_process_reaped': True}))


if __name__ == '__main__':
    try:
        main()
    except (Exception, KeyboardInterrupt):
        # Subprocess exceptions may embed ephemeral credentials in their argv.
        print('disposable provider acceptance failed; inspect retained evidence', file=sys.stderr)
        raise SystemExit(1) from None
