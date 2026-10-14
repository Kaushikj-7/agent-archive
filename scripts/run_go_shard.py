#!/usr/bin/env python3
"""Run deterministic PR race shards and reconcile their coverage evidence.

Timing hints rank discovered units; they never limit discovery or test coverage.
"""
import argparse
import ctypes
import hashlib
import json
import os
from pathlib import Path
import re
import signal
import subprocess
import sys
import time

ROOT = Path(__file__).resolve().parent.parent
CLI = 'github.com/wangjohn/agent-archive/internal/cli'
GIT_IDENTITY = 'github.com/wangjohn/agent-archive/internal/gitremote'


class Failure(RuntimeError):
    """Discovery, execution, or evidence did not establish complete coverage."""


def json_stream(text):
    """Decode concatenated objects without repeatedly copying the remaining log."""
    decoder, items, offset = json.JSONDecoder(), [], 0
    whitespace = re.compile(r'\s*')
    while True:
        offset = whitespace.match(text, offset).end()
        if offset == len(text):
            return items
        item, offset = decoder.raw_decode(text, offset)
        if not isinstance(item, dict):
            raise Failure('JSON record is not an object')
        items.append(item)


def child_pids():
    # Some kernels omit the optional /proc/.../children file. Fall back to
    # PPID metadata only; never read another process's command or environment.
    own_pid = os.getpid()
    if int(Path('/proc/self/stat').read_text().split(' ', 1)[0]) != own_pid:
        raise Failure('procfs PID namespace does not match signal namespace')
    children = Path(f'/proc/self/task/{own_pid}/children')
    if children.exists():
        return {int(pid) for pid in children.read_text().split()}
    result = set()
    for entry in Path('/proc').iterdir():
        if not entry.name.isdigit():
            continue
        try:
            fields = (entry/'stat').read_text().rsplit(')', 1)[1].split()
            if int(fields[1]) == own_pid:
                result.add(int(entry.name))
        except (OSError, ValueError, IndexError):
            continue  # exited or belongs to another inaccessible actor
    return result


def stop_processes(processes, previous_children):
    # As a Linux subreaper, this process owns orphaned test grandchildren too.
    handlers = {sig: signal.signal(sig, signal.SIG_IGN) for sig in (signal.SIGTERM, signal.SIGINT)}
    try:
        for sig in (signal.SIGTERM, signal.SIGKILL):
            for process in processes:
                try:
                    os.killpg(process.pid, sig)
                except ProcessLookupError:
                    pass
            if sig == signal.SIGTERM:
                end = time.monotonic() + 1
                while time.monotonic() < end and any(p.poll() is None for p in processes):
                    time.sleep(.02)
        end = time.monotonic() + 5
        for process in processes:
            try:
                process.wait(timeout=max(.01, end-time.monotonic()))
            except subprocess.TimeoutExpired as error:
                raise Failure('killed child did not exit within cleanup deadline') from error
        if previous_children is None:
            return  # Native runners retain process-group cleanup and direct-child reaping.
        # PTY harnesses create sessions/groups outside the Go command's
        # group. Once their parent dies they are adopted by this subreaper;
        # terminate and reap all newly adopted children, regardless of PGID.
        grace = min(end, time.monotonic()+1)
        signalled = set()
        while True:
            adopted = child_pids() - previous_children
            if not adopted:
                break
            for pid in adopted:
                try:
                    reaped, _ = os.waitpid(pid, os.WNOHANG)
                    if reaped:
                        continue
                    if pid not in signalled or time.monotonic() >= grace:
                        os.kill(pid, signal.SIGKILL if time.monotonic() >= grace else signal.SIGTERM)
                        signalled.add(pid)
                except (ChildProcessError, ProcessLookupError):
                    pass
            if time.monotonic() >= end:
                raise Failure('test descendant did not exit within cleanup deadline')
            time.sleep(.02)
    finally:
        for sig, handler in handlers.items():
            signal.signal(sig, handler)


def supervise(commands, deadline, *, stderr_path=None, terminal=None, after=None):
    """Record each complete invocation; file streams cannot block on pipe output."""
    if time.monotonic() >= deadline:
        raise Failure('shared discovery/test deadline already exceeded')
    # Linux also adopts escaped test grandchildren. Native macOS full batches
    # retain the existing process-group ownership and reap each direct child.
    previous_children = None
    if sys.platform == 'linux':
        libc = ctypes.CDLL(None, use_errno=True)
        libc.prctl.argtypes = [ctypes.c_int] + [ctypes.c_ulong] * 4
        if libc.prctl(36, 1, 0, 0, 0) != 0:
            raise Failure('cannot enable Linux child-subreaper cleanup')
        previous_children = child_pids()
    elif sys.platform != 'darwin':
        raise Failure('unsupported process supervisor platform')
    if stderr_path is not None and len(commands) != 1:
        raise Failure('separate stderr requires one invocation')
    after = {} if after is None else after
    if any(type(i) is not int or type(previous) is not int or not 0 <= previous < i < len(commands)
           for i,previous in after.items()):
        raise Failure('invalid sequential invocation dependency')
    processes, files = [None]*len(commands), []
    started, durations = [None]*len(commands), [None]*len(commands)
    monitors, pending = [None]*len(commands), [b'']*len(commands)
    try:
        def launch(index):
            command, output, env = commands[index]
            handle = open(output, 'w', encoding='utf-8')
            files.append(handle)
            print('command:', json.dumps(command), flush=True)
            started[index] = time.monotonic()
            stderr_handle = open(stderr_path, 'wb') if stderr_path is not None else None
            if stderr_handle is not None:
                files.append(stderr_handle)
            processes[index] = subprocess.Popen(command, stdout=handle, stderr=stderr_handle, env=env,
                                                start_new_session=True)
            monitors[index] = open(output, 'rb') if 'test' in command and '-json' in command else None
        for index in range(len(commands)):
            if index not in after:
                launch(index)
        while True:
            for index, previous in after.items():
                if processes[index] is None and processes[previous] is not None and processes[previous].poll() == 0:
                    launch(index)

            # Go continues other packages after a test fails. Inspect complete
            # JSON lines live so the first failure stops both commands promptly.
            for index, monitor in enumerate(monitors):
                if monitor is None:
                    continue
                pending[index] += monitor.read()
                lines = pending[index].split(b'\n')
                pending[index] = lines.pop()
                for line in lines:
                    if not line:
                        continue
                    try:
                        record = json.loads(line)
                    except ValueError as error:
                        raise Failure('malformed live Go JSON') from error
                    if not isinstance(record, dict):
                        raise Failure('invalid live Go JSON record')
                    if record.get('Action') in ('fail', 'build-fail'):
                        raise Failure('live Go failure: ' + str(record.get('Package')) + '/' + str(record.get('Test', '')))

            statuses = [p.poll() if p is not None else None for p in processes]
            for index, status in enumerate(statuses):
                if status is not None and durations[index] is None:
                    durations[index] = time.monotonic()-started[index]
                    print('command terminal:', index, 'status:', status,
                          'invocation seconds:', round(durations[index], 3), flush=True)
            if any(s is not None and s != 0 for s in statuses):
                raise Failure(f'child command failed: {statuses}')
            if all(s == 0 for s in statuses):
                return durations
            if time.monotonic() >= deadline:
                raise Failure('shared discovery/test deadline exceeded')
            time.sleep(.05)
    finally:
        try:
            stop_processes([p for p in processes if p is not None], previous_children)
        finally:
            if terminal is not None:
                terminal[:] = [process.returncode if process is not None else None for process in processes]
            for handle in files + [m for m in monitors if m is not None]:
                handle.close()


def metadata(text):
    items = json_stream(text)
    packages = {}
    for item in items:
        name = item.get('ImportPath')
        if (not isinstance(name, str) or not re.fullmatch(r'[A-Za-z0-9._/-]+', name)
                or name.startswith('-') or name in packages or item.get('Error')
                or item.get('DepsErrors') or not isinstance(item.get('Dir'), str)):
            raise Failure('invalid/duplicate package metadata')
        for key in ('TestGoFiles', 'XTestGoFiles'):
            if not isinstance(item.get(key, []), list):
                raise Failure('invalid test-file metadata')
        packages[name] = bool(item.get('TestGoFiles') or item.get('XTestGoFiles'))
    if CLI not in packages or len(packages) < 3:
        raise Failure('CLI or ordinary package catalog missing')
    return packages


def listed_names(text):
    names, terminals = [], []
    for event in json_stream(text):
        if event.get('Package') != CLI:
            raise Failure('unexpected CLI listing package')
        if event.get('Test') or event.get('Action') == 'fail':
            raise Failure('listing unexpectedly executed/failed a test')
        if event.get('Action') in ('pass', 'skip'):
            terminals.append(event['Action'])
        if event.get('Action') == 'output':
            line = event.get('Output', '').strip()
            if line.startswith(('Test', 'Fuzz', 'Example')):
                if (not re.fullmatch(r'(Test|Fuzz|Example)\w*', line)
                        or line == 'TestMain' or line in names):
                    raise Failure('invalid/duplicate runnable name')
                names.append(line)
    if not names or terminals != ['pass']:
        raise Failure('empty/failed CLI listing')
    return sorted(names)


def worker_count(workers):
    if type(workers) is not int or workers not in (2, 4, 8):
        raise Failure("worker count must be 2, 4 or 8")
    return workers


def partition(units, hints, workers=2, initial_loads=None):
    # Generic bounded LPT scheduling is shared with native full coverage.
    # Representative PR topology is independently restricted by worker_count.
    if type(workers) is not int or not 1 <= workers <= 16:
        raise Failure('partition count must be an integer from 1 through 16')
    if len(set(units)) != len(units):
        raise Failure('duplicate partition units')
    weights = {}
    for name in units:
        value = hints.get(name, 1000)
        if type(value) is not int or value < 0:
            raise Failure('hint is not nonnegative integer milliseconds')
        weights[name] = max(1, value)
        if name not in hints:
            print(f'unknown unit assigned: {name}, default={value}ms', flush=True)
    for name in sorted(set(hints) - set(units)):
        print(f'stale hint ignored: {name}', flush=True)
    loads = [0] * workers if initial_loads is None else list(initial_loads)
    if len(loads) != workers or any(type(v) is not int or v < 0 for v in loads):
        raise Failure('invalid initial scheduling loads')
    bins = [[] for _ in range(workers)]
    for name in sorted(units, key=lambda n: (-weights[n], n)):
        index = min(range(workers), key=lambda i: (loads[i], i))
        bins[index].append(name)
        loads[index] += weights[name]
    if (not all(bins) or sum(len(b) for b in bins) != len(set(n for b in bins for n in b))
            or set(n for b in bins for n in b) != set(units)):
        raise Failure('partition is not nonempty, disjoint and exhaustive')
    print("predicted logical loads (ms; not worker elapsed):", loads, flush=True)
    return [sorted(b) for b in bins]


def selector(names, limit=120000):
    if not names or len(set(names)) != len(names) or any('/' in n for n in names):
        raise Failure('invalid selector names')
    pattern = '^(' + '|'.join(re.escape(n) for n in names) + ')$'
    if len(pattern.encode()) + 1 > limit:
        raise Failure('selector exceeds Linux single-argument budget')
    return pattern


def coverage(text, packages, selected=None):
    terminals, tests, no_tests = {}, set(), set()
    for event in json_stream(text):
        package, action, test = event.get('Package'), event.get('Action'), event.get('Test')
        if package not in packages or action not in ('start', 'run', 'pause', 'cont', 'output', 'pass', 'skip', 'fail'):
            raise Failure('unexpected package or JSON action')
        if action == 'fail':
            raise Failure('test failure in JSON')
        if action == 'output':
            output = event.get('Output', '')
            if not isinstance(output, str):
                raise Failure('invalid output record')
            if '[no test files]' in output:
                no_tests.add(package)
            if selected is not None and 'no tests to run' in output:
                raise Failure('empty CLI selection')
        if test and '/' not in test:
            if selected is not None and test not in selected:
                raise Failure(f'unassigned CLI test: {test}')
            if action in ('pass', 'skip'):
                if (package, test) in tests:
                    raise Failure('duplicate test terminal')
                tests.add((package, test))
        if not test and action in ('pass', 'skip'):
            if package in terminals:
                raise Failure('duplicate package terminal')
            terminals[package] = action
    if set(terminals) != set(packages):
        raise Failure('missing package terminals')
    for package, action in terminals.items():
        if action == 'skip' and (packages[package] or package not in no_tests):
            raise Failure('package skip is not established as no-test files')
    if selected is not None and {n for _, n in tests} != set(selected):
        raise Failure('missing CLI test terminals')
    return {'packages': sorted(terminals), 'cli': sorted(n for _, n in tests) if selected is not None else []}


def digest(value):
    return hashlib.sha256(json.dumps(value, sort_keys=True, separators=(',', ':')).encode()).hexdigest()


def execute_worker_vet(out, deadline, settings):
    """Always retain raw streams and terminal/failure receipt before rejection."""
    import run_ci_tier
    env = dict(os.environ, AGENT_ARCHIVE_REAL_SYSTEMD='0')
    env.pop('AGENT_ARCHIVE_PERF', None)
    receipt = {'schema': 1, 'command': ['go', 'vet', './...'], 'log': 'plain-vet.txt',
               'stderr_log': 'plain-vet.stderr', 'performance_enabled': False,
               'real_systemd_enabled': False, 'identity_schema': 'full-v1', 'exit_code': None, 'success': False}
    terminal, started = [], time.monotonic()
    try:
        for name in ('plain-vet.txt', 'plain-vet.stderr', 'plain-vet-receipt.json'):
            (out/name).unlink(missing_ok=True)
        receipt['pre_source_proof'] = run_ci_tier.full_race.source_proof(ROOT)
        receipt['context'] = run_ci_tier.plain_vet_context()
        if (receipt['pre_source_proof'] != settings['plain_vet_source_proof']
                or receipt['context'] != settings['plain_vet_context']):
            raise Failure('worker vet pre-execution source/context mismatch')
        supervise([(receipt['command'], out/'plain-vet.txt', env)], deadline,
                  stderr_path=out/'plain-vet.stderr', terminal=terminal)
        receipt['post_source_proof'] = run_ci_tier.full_race.source_proof(ROOT)
        receipt['post_context'] = run_ci_tier.plain_vet_context()
        if (receipt['post_source_proof'] != receipt['pre_source_proof']
                or receipt['post_context'] != receipt['context']):
            raise Failure('worker vet source/context changed')
        for name, field in [('plain-vet.txt', 'log_sha256'), ('plain-vet.stderr', 'stderr_sha256')]:
            receipt[field] = hashlib.sha256((out/name).read_bytes()).hexdigest()
        receipt['exit_code'] = terminal[0] if len(terminal) == 1 else None
        # Live worker validation keeps the existing actual compiler/context guard.
        run_ci_tier.verify_plain_vet(out, {'plain_vet': receipt},
            settings['plain_vet_source_proof'], settings['plain_vet_context_sha256'], strict_full_context=True)
        receipt['success'] = True
        return receipt
    except BaseException as error:
        receipt['error'] = {'type': type(error).__name__, 'message': str(error)}
        raise
    finally:
        receipt['exit_code'] = terminal[0] if len(terminal) == 1 else None
        receipt['invocation_seconds'] = time.monotonic()-started
        for name, field in [('plain-vet.txt', 'log_sha256'), ('plain-vet.stderr', 'stderr_sha256')]:
            path = out/name
            receipt[field] = hashlib.sha256(path.read_bytes()).hexdigest() if path.exists() else None
        (out/'plain-vet-receipt.json').write_text(json.dumps(receipt, indent=2, sort_keys=True)+'\n')


def verify_worker_vet(directory, manifest, expected):
    import run_ci_tier
    settings = manifest.get('settings', {})
    if type(settings.get('plain_vet_receipt_schema')) is not int or settings['plain_vet_receipt_schema'] != 1:
        raise Failure('worker vet receipt schema missing or invalid')
    try:
        receipt = json.loads((directory/'plain-vet-receipt.json').read_text())
    except (OSError, ValueError) as error:
        raise Failure('worker vet receipt missing or malformed') from error
    if not isinstance(receipt, dict):
        raise Failure('worker vet receipt malformed')
    context = settings.get('plain_vet_context')
    if (not isinstance(context, dict) or set(context) != {'go', 'version', 'runner_os',
            'runner_arch', 'image', 'os_release', 'cc', 'cxx', 'toolchain', 'race', 'tags', 'schema'}
            or not isinstance(context.get('image'), dict)
            or set(context['image']) != {'ImageOS', 'ImageVersion'}
            or any(type(v) is not str or not re.fullmatch(r'[A-Za-z0-9_.-]+', v)
                   for v in context['image'].values())):
        raise Failure('worker vet full image/context identity malformed')
    if (receipt.get('identity_schema') != 'full-v1'
            or type(receipt.get('schema')) is not int or receipt['schema'] != 1
            or not isinstance(context, dict) or digest(context) != settings.get('plain_vet_context_sha256')
            or manifest.get('plain_vet') != receipt or receipt.get('success') is not True
            or receipt.get('post_context') != context or receipt.get('context') != context
            or receipt.get('stderr_log') != 'plain-vet.stderr'
            or receipt.get('error') is not None
            or type(receipt.get('invocation_seconds')) not in (int, float)
            or not 0 <= receipt['invocation_seconds'] < float('inf')):
        raise Failure('worker vet receipt/context mismatch')
    # Cross-worker aggregation validates the explicit schema and full worker
    # context above, never the aggregate runner's unrelated native context.
    # The live worker also invokes this canonical verifier with historical=False.
    run_ci_tier.verify_plain_vet(directory, {'plain_vet': receipt}, expected,
                                digest(context), historical=True, strict_full_context=True)
    if receipt.get('stderr_sha256') != hashlib.sha256((directory/'plain-vet.stderr').read_bytes()).hexdigest():
        raise Failure('worker vet raw stderr mismatch')


def reconcile(directory, workers=2, audit_evidence=None, *, expected_source_proof=None, require_plain_vet=False, historical_legacy=False):
    worker_count(workers)
    paths = sorted(Path(directory).glob('**/manifest.json'))
    if len(paths) != workers:
        raise Failure('expected complete worker manifest count')
    manifests = [json.loads(p.read_text()) for p in paths]
    if any(type(m.get('shard')) is not int for m in manifests) or {m.get('shard') for m in manifests} != set(range(workers)):
        raise Failure('missing/duplicate shard evidence')
    first = manifests[0]['catalog']
    import go_parent_shards as parent_shards
    versioned = 'schema' in first
    if versioned:
        parent_shards.validate(first, workers)
        import run_ci_tier
        proof = expected_source_proof if expected_source_proof is not None else run_ci_tier.full_race.source_proof(ROOT)
        if first['source_proof'] != proof:
            raise Failure('parent catalog differs from verified source tree')
    elif workers == 8 or not historical_legacy:
        raise Failure('live workers require complete-parent catalog schema')
    if first.get('workers', 2) != workers or len(first.get('assignments', [])) != workers:
        raise Failure('catalog worker count mismatch')
    if any(m.get('settings', {}).get('plain_audit_owned') for m in manifests):
        if not all(m.get('settings', {}).get('plain_audit_owned') is True for m in manifests) or audit_evidence is None:
            raise Failure('owned plain audit requires complete independent evidence')
        import run_ci_tier
        try:
            if expected_source_proof is None:
                run_ci_tier.verify_source_audit(audit_evidence, first['source'])
            else:
                run_ci_tier.verify_source_audit(audit_evidence, first['source'], expected_source_proof=expected_source_proof)
        except run_ci_tier.Failure as error:
            raise Failure(str(error)) from error
    vet_owned = [m.get('settings', {}).get('plain_vet_owned', False) for m in manifests]
    if any(vet_owned) or require_plain_vet:
        import run_ci_tier
        if not all(v is True for v in vet_owned) or audit_evidence is None:
            raise Failure('owned plain vet requires complete independent evidence')
        # Legacy shared receipts are only a Git-bound historical import policy.
        # Presence (not truthiness) of ANY new-format signal prevents fallback,
        # including a missing/invalid schema marker on an otherwise new worker.
        legacy = expected_source_proof is not None and all(
            'plain_vet_identity_schema' not in m.get('settings', {})
            and 'plain_vet_receipt_schema' not in m.get('settings', {})
            and 'plain_vet_context' not in m.get('settings', {})
            and 'plain_vet' not in m
            and not (p.parent/'plain-vet-receipt.json').exists()
            and not (p.parent/'plain-vet.stderr').exists()
            for p, m in zip(paths, manifests))
        try:
            if legacy:
                values = [m.get('settings', {}).get('plain_vet_context_sha256') for m in manifests]
                if any(type(v) is not str or not re.fullmatch('[0-9a-f]{64}', v) for v in values):
                    raise Failure('legacy plain vet context digest malformed')
                contexts = set(values)
                if len(contexts) != 1:
                    raise Failure('owned plain vet requires complete independent evidence')
                audit = run_ci_tier.verify_source_audit(audit_evidence, first['source'],
                    expected_source_proof=expected_source_proof, require_plain_vet=True,
                    expected_vet_context_sha256=next(iter(contexts)), strict_full_context=True)
            else:
                audit = run_ci_tier.verify_source_audit(audit_evidence, first['source'],
                    expected_source_proof=expected_source_proof)
            for path, manifest in zip(paths, manifests):
                settings = manifest.get('settings', {})
                if settings.get('plain_vet_source_proof') != audit.get('source_proof'):
                    raise Failure('owned plain vet worker tracked source proof mismatch')
                if not legacy:
                    verify_worker_vet(path.parent, manifest, audit['source_proof'])
        except run_ci_tier.Failure as error:
            raise Failure(str(error)) from error
    for manifest in manifests:
        if (manifest.get('success') is not True or manifest.get('catalog') != first
                or manifest.get('catalog_sha256') != digest(first)
                or first.get('source') != os.environ.get('GITHUB_SHA', first.get('source'))
                or manifest.get('settings', {}).get('workers', 2) != workers
                or manifest.get('tier') != 'pr' or not isinstance(manifest.get('body_evidence'), dict)):
            raise Failure('shard catalog/source/result mismatch')
        index = manifest['shard']
        expected = first['assignments'][index]
        if manifest.get('coverage') != expected:
            raise Failure('shard evidence does not cover its complete assignment')
        import run_ci_tier
        directory = paths[manifests.index(manifest)].parent
        if versioned:
            raw = parent_shards.verify_logs(directory, first, expected)
            if manifest.get('settings', {}).get('source_proof') != first['source_proof']:
                raise Failure('parent catalog/worker source proof differs')
            actual_config = manifest.get('settings', {}).get('plain_vet_context', {}).get('go', {})
            if manifest.get('settings', {}).get('plain_vet_owned') and {key: actual_config.get(key) for key in parent_shards.CONFIG_FIELDS} != first['configuration']:
                raise Failure('parent catalog/actual worker compiler differs')
        else:
            logs = [directory / name for name in ('git-identity.json', 'ordinary.json', 'cli.json') if (directory / name).exists()]
            if not (directory / 'cli.json').exists():
                raise Failure('actual shard CLI evidence missing')
            raw = '\n'.join(log.read_text() for log in logs)
            ordinary_raw = '\n'.join(log.read_text() for log in logs if log.name != 'cli.json')
            actual_packages = coverage(ordinary_raw, {n: first['packages'][n] for n in expected['packages']})
            actual_cli = coverage((directory / 'cli.json').read_text(), {CLI: True}, expected['cli'])
            if {'packages': actual_packages['packages'], 'cli': actual_cli['cli']} != expected:
                raise Failure('actual terminal coverage differs from assignment')
        if manifest.get('settings', {}).get('plain_audit_owned') and run_ci_tier.SOURCE_AUDIT_PACKAGE in expected['packages']:
            try:
                run_ci_tier.verify_owned_audit_skip(raw)
            except run_ci_tier.Failure as error:
                raise Failure(str(error)) from error
        if manifest['body_evidence'] != run_ci_tier.body_evidence(raw, short=True):
            raise Failure('actual shard body evidence differs from manifest')
    if not versioned:
        assignments = first['assignments']
        for kind in ('packages', 'cli'):
            bins = [s[kind] for s in assignments]
            flattened = [n for b in bins for n in b]
            universe = set(first[kind]) - ({CLI} if kind == 'packages' else set())
            if len(flattened) != len(set(flattened)) or set(flattened) != universe or not all(bins):
                raise Failure('manifest partition is not exhaustive/disjoint')
    print('reconciled source/catalog:', first['source'], digest(first))


def verdict(event, ref, needs):
    expected = {'linux-short': 'success', 'linux-full': 'success', 'script-tests': 'success',
                'macos-smoke': 'success', 'tier-plan': 'success', 'source-audit': 'success'}
    if event == 'push' and ref.startswith('refs/heads/release-candidate/') and ref != 'refs/heads/release-candidate/':
        expected['linux-short'], expected['linux-full'], expected['source-audit'] = 'skipped', 'success', 'skipped'
    elif event != 'pull_request':
        raise Failure('unsupported gate event/ref')
    if not isinstance(needs, dict) or set(needs) != set(expected):
        raise Failure('missing/unexpected required job results')
    if any(not isinstance(needs[j], dict) or needs[j].get('result') != result for j, result in expected.items()):
        raise Failure('required job result did not succeed or intentionally skip')


def run(index, out, seconds, workers=2, plain_audit_owned=False, plain_vet_owned=False):
    worker_count(workers)
    if type(index) is not int or index not in range(workers):
        raise Failure("invalid worker index")
    started = time.monotonic()
    out.mkdir(parents=True, exist_ok=True)
    (out / 'manifest.json').unlink(missing_ok=True)
    for old in [*out.glob('parents-*.json'), *(out/n for n in ('ordinary.json','cli.json','git-identity.json'))]:
        old.unlink(missing_ok=True)
    deadline = time.monotonic() + seconds
    env = os.environ.copy()
    if plain_audit_owned:
        env['AGENT_ARCHIVE_PLAIN_SOURCE_AUDIT_OWNED'] = '1'
    else:
        env.pop('AGENT_ARCHIVE_PLAIN_SOURCE_AUDIT_OWNED', None)
    cpus = max(1, len(os.sched_getaffinity(0)))
    quota_file = Path('/sys/fs/cgroup/cpu.max')
    if quota_file.exists():
        quota, period = quota_file.read_text().split()
        if quota != 'max':
            cpus = min(cpus, max(1, int(quota) // int(period)))
    ordinary_cpus = max(1, cpus // 2)
    cli_cpus = max(1, cpus - ordinary_cpus)
    source = subprocess.check_output(['git', 'rev-parse', 'HEAD'], cwd=ROOT, text=True).strip()
    if os.environ.get('GITHUB_SHA', source) != source:
        raise Failure('checkout differs from event SHA')
    import run_ci_tier
    source_proof = run_ci_tier.full_race.source_proof(ROOT)
    phases = {}
    phases['package_discovery_seconds'] = supervise([(['go', 'list', '-json', './...'], out / 'packages.json', env)], deadline)[0]
    packages = metadata((out / 'packages.json').read_text())
    phases['cli_listing_race_build_testmain_seconds'] = supervise([(['go', 'test', '-race', '-short', '-count=1', '-json', '-list', '^(Test|Fuzz|Example)', CLI], out / 'listing.json', env)], deadline)[0]
    names = listed_names((out / 'listing.json').read_text())
    import go_parent_shards as parent_shards
    import run_ci_tier
    heavy = sorted(set(parent_shards.HEAVY) & set(packages))
    if heavy:
        phases['parent_listing_race_build_testmain_seconds'] = supervise([
            (['go','test','-race','-short','-count=1','-json','-p','1','-list','^(Test|Fuzz|Example)'] + heavy,
             out/'listing-parents.json', env)], deadline)[0]
    else:
        (out/'listing-parents.json').write_text('')
    parents = parent_shards.listing((out/'listing-parents.json').read_text(), heavy)
    config = json.loads(subprocess.check_output(['go','env','-json', *parent_shards.CONFIG_FIELDS], env=env, text=True))
    (out/'discovery-config.json').write_text(json.dumps(config, sort_keys=True)+'\n')
    if run_ci_tier.full_race.source_proof(ROOT) != source_proof:
        raise Failure('source changed during compiled discovery')
    hints = json.loads((ROOT / 'scripts/go_shard_hints.json').read_text())
    auxiliary = hints.get('auxiliary_costs_ms', {'vet':0, 'git_invocation':0})
    if set(auxiliary) != {'vet','git_invocation'} or any(type(v) is not int or v < 0 for v in auxiliary.values()):
        raise Failure('invalid auxiliary scheduling hints')
    ranking = json.loads(json.dumps(hints))
    if not plain_audit_owned:
        for package,elapsed in hints.get('owned_plain_audit_exclusions_ms',{}).items():
            ranking['packages'][package] = ranking['packages'].get(package,1000) + elapsed
    if GIT_IDENTITY in packages:
        ranking['packages'][GIT_IDENTITY] = max(ranking['packages'].get(GIT_IDENTITY,1000),auxiliary['git_invocation'])
    fixed = [0 if plain_vet_owned else auxiliary['vet']] + [0] * (workers-1)
    catalog = parent_shards.build(source, source_proof, config, packages, names, parents, ranking, workers, fixed)
    parent_shards.validate(catalog, workers)
    assignment = catalog['assignments'][index]
    package_bins = [a['packages'] for a in catalog['assignments']]
    cli_bins = [a['cli'] for a in catalog['assignments']]
    cli_fixed = [0] * workers
    manifest = {'shard': index, 'catalog': catalog, 'catalog_sha256': digest(catalog), 'success': False,
                'phases': phases,
                'timing_observation': {'schema': 1, 'cache_restore_state': os.environ.get('CI_CACHE_RESTORE_STATE', 'unknown'),
                    'runner_image': os.environ.get('ImageOS', ''), 'runner_image_version': os.environ.get('ImageVersion', ''),
                    'run_id': os.environ.get('GITHUB_RUN_ID', ''), 'run_attempt': os.environ.get('GITHUB_RUN_ATTEMPT', ''),
                    'commands_overlap': cpus > 1, 'note': 'invocation durations include compilation; parallel durations are not additive'},
                'scheduling': {'fixed_vet_ms': fixed, 'cli_fixed_auxiliary_ms': cli_fixed,
                               'usage': 'logical ranking plus observed fixed invocation advice; not elapsed forecast'},
                'settings': {'source_proof': source_proof, 'plain_audit_owned': plain_audit_owned, 'workers': workers, 'cpus': cpus, 'cli_cpus': cli_cpus, 'ordinary_cpus': ordinary_cpus, 'package_p': 1,
                             'ordinary_parallel': ordinary_cpus, 'cli_parallel': cli_cpus, 'deadline_seconds': seconds}}
    if plain_vet_owned:
        import run_ci_tier
        worker_source = run_ci_tier.full_race.source_proof(ROOT)
        if worker_source['source'] != source:
            raise Failure('owned vet worker tracked source mismatch')
        manifest['settings'].update(plain_vet_owned=True, plain_vet_receipt_schema=1, plain_vet_source_proof=worker_source,
            plain_vet_context=run_ci_tier.plain_vet_context())
        manifest['settings']['plain_vet_context_sha256'] = digest(manifest['settings']['plain_vet_context'])
    path = out / 'manifest.json'
    path.write_text(json.dumps(manifest, indent=2, sort_keys=True) + '\n')
    print('effective catalog:', json.dumps(catalog, sort_keys=True), flush=True)
    common = ['go', 'test', '-race', '-short', '-count=1', '-json', '-timeout', '20m', '-p', '1']
    # Native Git observers have fixed 500ms process deadlines. The overlapping
    # workload exposed timeout failures; run this assigned package first with
    # the full worker budget, without changing its assertions or deadlines.
    isolated = [n for n in package_bins[index] if n == GIT_IDENTITY]
    overlapping = [n for n in package_bins[index] if n != GIT_IDENTITY]
    isolated_commands = [(common + ['-parallel', str(cpus)] + isolated,
                          out / 'git-identity.json', dict(env, GOMAXPROCS=str(cpus)))] if isolated else []
    other_env = dict(env, GOMAXPROCS=str(ordinary_cpus))
    if not overlapping:
        cli_cpus = cpus
    cli_env = dict(env, GOMAXPROCS=str(cli_cpus))
    commands = []
    if overlapping:
        commands.append((common + ['-parallel', str(ordinary_cpus)] + overlapping,
                         out / 'ordinary.json', other_env))
    if cli_bins[index]:
        commands.append((common + ['-parallel', str(cli_cpus), '-run', selector(cli_bins[index]), CLI],
                         out / 'cli.json', cli_env))
    for package, selected in assignment['parents'].items():
        commands.append((common + ['-parallel', str(cpus), '-run', selector(selected), package],
                         out / parent_shards.log_name(package), dict(env, GOMAXPROCS=str(cpus))))
    # One ordinary lane executes package groups sequentially while the CLI lane
    # overlaps it. TestMain/seed setup occurs once per assigned package, and at
    # most two commands share the CPU budget. Git observers stay isolated first.
    ordinary_indices = [i for i,(_,log,_) in enumerate(commands) if log.name != 'cli.json']
    after = {current:previous for previous,current in zip(ordinary_indices,ordinary_indices[1:])}
    has_cli = bool(cli_bins[index])
    lane_cpus = max(1,cpus//2) if has_cli and ordinary_indices else cpus
    for command, log, child_env in commands:
        budget = cpus-lane_cpus if log.name == 'cli.json' and ordinary_indices else lane_cpus
        budget = max(1,budget)
        command[command.index('-parallel')+1] = str(budget)
        child_env['GOMAXPROCS'] = str(budget)
    manifest['settings']['execution_lanes'] = int(bool(ordinary_indices))+int(has_cli)
    manifest['settings']['lane_cpus'] = lane_cpus
    manifest['settings']['sequential_after'] = {str(i):previous for i,previous in after.items()}
    # Validate every command before executing any selected tests.
    for command, _, child_env in isolated_commands + commands:
        size = sum(len(v.encode()) + 1 for v in command) + sum(len((k+'='+v).encode()) + 1 for k, v in child_env.items())
        if size > os.sysconf('SC_ARG_MAX') // 2:
            raise Failure('command/environment exceeds conservative execve budget')
    manifest['settings'].update(isolated_packages=isolated, cli_cpus=cli_cpus, cli_parallel=cli_cpus)
    path.write_text(json.dumps(manifest, indent=2, sort_keys=True) + '\n')
    if isolated_commands:
        phases['git_identity_invocation_seconds'] = supervise(isolated_commands, deadline)[0]
    durations = []
    if cpus == 1:
        for command in commands:
            durations.extend(supervise([command], deadline))
    else:
        durations = supervise(commands, deadline, after=after)
    for (_, log, _), duration in zip(commands, durations):
        phases[log.stem + '_invocation_seconds'] = duration
    raw = parent_shards.verify_logs(out, catalog, assignment)
    if run_ci_tier.full_race.source_proof(ROOT) != source_proof:
        raise Failure('worker source changed during execution')
    if plain_vet_owned:
        manifest['plain_vet'] = execute_worker_vet(out, deadline, manifest['settings'])
    if index == 0 and not plain_vet_owned:
        phases['vet_seconds'] = supervise([(['go', 'vet', './...'], out / 'vet.txt', env)], deadline)[0]
    import run_ci_tier
    manifest['tier'] = 'pr'
    manifest['body_evidence'] = run_ci_tier.body_evidence(raw, short=True)
    phases['worker_total_seconds'] = time.monotonic() - started
    print('actual worker phases (seconds):', json.dumps(phases, sort_keys=True), flush=True)
    manifest.update(success=True, phases=phases, coverage=assignment)
    path.write_text(json.dumps(manifest, indent=2, sort_keys=True) + '\n')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    group = parser.add_mutually_exclusive_group(required=True)
    group.add_argument('--shard', type=int)
    parser.add_argument('--workers', type=int, choices=(2, 4, 8), default=2)
    parser.add_argument('--plain-audit-owned', action='store_true')
    parser.add_argument('--plain-vet-owned', action='store_true')
    parser.add_argument('--audit-evidence', type=Path)
    group.add_argument('--reconcile', type=Path)
    group.add_argument('--verdict', action='store_true')
    parser.add_argument('--output', type=Path, default=Path('shard-results'))
    parser.add_argument('--seconds', type=int, default=1500)
    args = parser.parse_args()
    if args.seconds <= 0:
        parser.error('--seconds must be positive')
    def interrupted(signum, frame):
        raise Failure(f'cancelled by signal {signum}')
    for sig in (signal.SIGTERM, signal.SIGINT):
        signal.signal(sig, interrupted)
    try:
        if args.verdict:
            verdict(os.environ['GITHUB_EVENT_NAME'], os.environ['GITHUB_REF'], json.loads(os.environ['NEEDS']))
        elif args.reconcile is not None:
            reconcile(args.reconcile, args.workers, args.audit_evidence, require_plain_vet=args.plain_vet_owned)
        else:
            os.chdir(ROOT)
            run(args.shard, args.output, args.seconds, args.workers, args.plain_audit_owned, args.plain_vet_owned)
    except (Failure, ValueError, KeyError, OSError) as error:
        print(f'shard gate failed: {error}', file=sys.stderr)
        return 1
    return 0


if __name__ == '__main__':
    sys.exit(main())
