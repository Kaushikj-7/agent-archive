#!/usr/bin/env python3
"""Explicit CI cadence, conservative PR backstops and actual execution evidence."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import platform
import re
import signal
import subprocess
import sys
import tempfile
import time

import full_race

ROOT = Path(__file__).resolve().parent.parent
MODULE = 'github.com/wangjohn/agent-archive/'
PERFORMANCE = ['go', 'test', '-v', '-json', '-p', '1', '-count=1', '-run',
               'StayFast|FiveMegabyte|TestCursorOverlappingHooksRegisterOnce',
               './internal/collector', './internal/archive', './internal/agents/nativecodec', './internal/capture']


class Failure(RuntimeError):
    pass


def catalog():
    value = json.loads((ROOT / 'scripts/ci_tiers.json').read_text())
    if value.get('schema') != 1 or not value.get('restorations'):
        raise Failure('tier catalog is missing or unsupported')
    seen = set()
    for item in value['restorations']:
        key = (item['package'], item['test'])
        if (key in seen or not re.fullmatch(r'internal/[\w/-]+', key[0])
                or not re.fullmatch(r'Test\w+', key[1]) or item['restore'] not in ('daily', 'weekly')):
            raise Failure('invalid or duplicate restoration')
        seen.add(key)
    return value


def resolve(event, ref='', schedule='', suite='all'):
    if event == 'pull_request':
        return 'pr'
    if event == 'push' and ref.startswith('refs/heads/release-candidate/') and ref != 'refs/heads/release-candidate/':
        return 'full'
    if event == 'workflow_dispatch' and suite in ('all', 'linux', 'macos', 'fuzz', 'systemd'):
        return 'full'
    if event == 'schedule':
        config = catalog()
        if schedule == config['daily_cron']:
            return 'daily'
        if schedule == config['weekly_cron']:
            return 'full'
    raise Failure('unsupported event, schedule or manual suite')


def git(*args):
    return subprocess.check_output(['git', '-C', str(ROOT), *args], stderr=subprocess.PIPE, timeout=20).decode().strip()


PR_REGRESSIONS = {
    'catalog': {'internal/catalog': ['TestPublicationBoundsAndCoordinatorMutationRefuseBeforeWrite', 'TestMissingCorruptAndOversizedNodesFailClosed', 'TestPickerUnsupportedAndBudgetAreNotEmptyProofs']},
    'archive': {'internal/state': ['TestImportStageCapacityEncodedBoundary', 'TestImportStageCapacityRefilterBoundary'], 'internal/agents/codex': ['TestRelatedCachedRecordCountLimitIsTypedAndReleasesBudget']},
    'recovery': {'internal/state': ['TestImportStageCapacityEncodedBoundary', 'TestImportStageCapacityRefilterBoundary']},
    'cli': {'internal/cli': ['TestCodexExceptionRequiresNamedOtherAppConsentBeforeAdmission', 'TestRealCLIOverflowShowKeepsFullBodyAndSourceAuthority', 'TestRealCLICatalogDefaultList50Bounded']},
}


# Shared CLI dispatch, installation identity and status observation participate
# in setup/uninstall even though their filenames do not name those commands.
SYSTEMD_SHARED_CLI = {'cli.go', 'install_paths.go', 'status.go'}


def representative_fallback():
    # The required verdict reconciles the exact-source unrestricted Linux short
    # shards and native smoke. Unknown inputs do not duplicate that complete run.
    return {'packages': [], 'regressions': {}, 'ordinary_coverage': 'required-linux-short-shards',
            'native': False, 'systemd': False, 'fuzz': False, 'purge': False}


def extra_for_paths(paths):
    """Explicit targeted PR backstops on top of mandatory complete short coverage."""
    result = representative_fallback()
    families = {
        'catalog': {'reader', 'catalog', 'storage'},
        'recovery': {'state', 'backfill', 'retention', 'revocation', 'setupjournal', 'collector', 'sourceio', 'discovery'},
        'archive': {'archive', 'agents', 'capture', 'nativecodec', 'transcriptio', 'sourcefacts'},
        'platform': {'scheduler', 'config', 'credentials', 'platform', 'setupresources', 'skillownership', 'termlaunch', 'terminal'},
        'presentation': {'stats', 'statshtml', 'statsfmt'},
        'purge': {'purge'}, 'cli': {'cli'},
    }
    groups, uncertain = set(), False
    for path in paths:
        if path.startswith(('docs/', 'dev/')) or path.endswith('.md') or path == 'LICENSE':
            continue
        if path.startswith('internal/'):
            components = path.split('/')
            found = {name for name, members in families.items() if len(components) > 2 and components[1] in members}
            if not found:
                uncertain = True
            groups.update(found)
            if len(components) > 2 and (components[1] in {'scheduler', 'setupjournal', 'setupresources'}
                    or components[1] == 'cli' and (components[-1] in SYSTEMD_SHARED_CLI
                        or any(word in components[-1] for word in ('setup', 'uninstall', 'systemd', 'scheduler')))):
                result['systemd'] = True
        else:
            uncertain = True
    # Keep critical targeted integration backstops even alongside unknown paths.
    if uncertain:
        groups.update(('catalog', 'archive', 'cli'))
    selected = {}
    for group in groups:
        for package, names in PR_REGRESSIONS.get(group, {}).items():
            selected.setdefault(package, set()).update(names)
    result.update(regressions={p: sorted(n) for p,n in sorted(selected.items())},
                  packages=['./'+p for p in sorted(selected)],
                  native=bool(groups & {'platform', 'archive', 'catalog', 'cli'}))
    return result


def pr_extras(event, tested):
    """Require exact base/head/tested-merge proof; uncertainty repeats all short."""
    full = extra_for_paths(['unmapped-source-proof'])
    try:
        base, head = event['pull_request']['base']['sha'], event['pull_request']['head']['sha']
        for sha in (base, head, tested):
            if not isinstance(sha, str) or not re.fullmatch('[0-9a-f]{40}', sha) or git('rev-parse', '--verify', sha+'^{commit}') != sha:
                raise Failure('event commit unavailable')
        if git('rev-parse', 'HEAD') != tested or git('rev-parse', '--is-shallow-repository') != 'false':
            raise Failure('tested source/history unavailable')
        if git('show', '-s', '--format=%P', tested).split() != [base, head]:
            raise Failure('unexpected tested merge')
        git('merge-base', '--is-ancestor', base, tested)
        git('merge-base', '--is-ancestor', head, tested)
        if git('status', '--porcelain', '--untracked-files=all'):
            raise Failure('dirty source')
        # Reuse the verifier's tracked-byte/type/mode proof, including index-hidden edits.
        import scope_native_verify as scope
        for sha in (base, head, tested):
            for record in scope.records(scope.git(ROOT, 'ls-tree', '-r', '-z', sha)):
                metadata, raw_path = record.split(b'\t', 1)
                mode, kind, oid = metadata.split(b' ')
                tree_path = scope.repository_path(raw_path)
                if mode not in (b'100644', b'100755') or kind != b'blob':
                    raise Failure('symlink/submodule/unusual tracked input')
                if sha == tested and not scope.working_input_matches(ROOT, tree_path, mode, oid):
                    raise Failure('tracked bytes differ from tested source')
        paths = set()
        for endpoint in (head, tested):
            raw = subprocess.check_output(['git', '-C', str(ROOT), 'diff', '--no-ext-diff', '--no-textconv', '--no-renames', '--name-only', '-z', base, endpoint, '--'], timeout=20)
            if raw and not raw.endswith(b'\0'):
                raise Failure('invalid changed paths')
            paths.update(p.decode() for p in raw.split(b'\0') if p)
        return dict(extra_for_paths(sorted(paths)), reason='verified base/head/tested merge', base=base, head=head, paths=sorted(paths))
    except (KeyError, TypeError, ValueError, OSError, subprocess.SubprocessError, Failure):
        return dict(full, reason='representative fallback: source proof unavailable')


def terminals(text):
    """An observed skip is retained as skip, including nested children."""
    items, runs, reasons = {}, set(), {}
    for line in text.splitlines():
        if not line.strip():
            continue
        event = json.loads(line)
        if not isinstance(event, dict):
            raise Failure('invalid Go event')
        package, test, action = event.get('Package'), event.get('Test', ''), event.get('Action')
        if action in ('fail', 'build-fail'):
            raise Failure('Go body/build failed')
        if test and action == 'run':
            runs.add((package, test))
        if test and action == 'output':
            reasons.setdefault((package, test), []).append(event.get('Output', ''))
        if test and action in ('pass', 'skip'):
            key = (package, test)
            if key in items:
                raise Failure('duplicate body terminal')
            items[key] = action
    if runs - set(items):
        raise Failure('missing body terminals')
    return [{'package': p, 'test': t, 'status': status,
             **({'reason': ''.join(reasons.get((p, t), []))} if status == 'skip' else {})}
            for (p, t), status in sorted(items.items())]


def body_evidence(text, short):
    actual = terminals(text)
    observed = {(r['package'], r['test'].split('/')[0]) for r in actual}
    scale = []
    for entry in catalog()['restorations']:
        key = (MODULE + entry['package'], entry['test'])
        if key in observed:
            scale.append(dict(entry, full_body='NOT RUN' if short else 'executed; see actual terminals'))
    return {'actual': actual, 'full_restorations': scale}


def checked(command, output, deadline, env=None):
    """Linux retains the existing subreaper; Darwin stops the entire process group."""
    if sys.platform == 'linux':
        import run_go_shard
        try:
            run_go_shard.supervise([(command, output, env or os.environ.copy())], deadline)
        except run_go_shard.Failure as error:
            raise Failure(str(error)) from error
        return output.read_text()
    def cancel(signum, frame):
        raise Failure('tier command cancelled')
    old = {sig: signal.signal(sig, cancel) for sig in (signal.SIGTERM, signal.SIGINT)}
    process = None
    try:
        with output.open('w') as handle:
            process = subprocess.Popen(command, stdout=handle, start_new_session=True, env=env)
            try:
                status = process.wait(timeout=max(.01, deadline-time.monotonic()))
            except subprocess.TimeoutExpired as error:
                raise Failure('tier deadline exceeded') from error
            if status:
                raise Failure('tier command failed')
        return output.read_text()
    finally:
        if process:
            try:
                os.killpg(process.pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
            process.wait()
        for sig, handler in old.items():
            signal.signal(sig, handler)


def discover(packages, out, deadline, race=False):
    """Exact selected-name/fuzz discovery; ordinary unrestricted runs need no listing."""
    env = dict(os.environ, AGENT_ARCHIVE_REAL_SYSTEMD='0')
    env.pop('AGENT_ARCHIVE_PERF', None)
    text = checked(['go', 'test', *(['-race'] if race else []), '-count=1', '-json', '-list', '^(Test|Fuzz|Example)', *packages], out, deadline, env)
    names, ended = {}, set()
    for line in text.splitlines():
        item = json.loads(line)
        pkg = item.get('Package')
        if item.get('Action') in ('fail', 'build-fail'):
            raise Failure('listing failed')
        if item.get('Action') in ('pass', 'skip') and not item.get('Test'):
            ended.add(pkg)
        if item.get('Action') == 'output':
            name = item.get('Output', '').strip()
            if re.fullmatch(r'(Test|Fuzz|Example)\w*', name) and name != 'TestMain':
                if name in names.setdefault(pkg, set()):
                    raise Failure('duplicate discovered name')
                names[pkg].add(name)
    if not ended or set(names) - ended:
        raise Failure('listing lacks terminal packages')
    return {p: sorted(n) for p, n in names.items()}


def prepare_performance(out, deadline, env, expected_source):
    """Compile dependencies with two workers; never run initialization or tests."""
    packages = PERFORMANCE[-4:]
    def require_source():
        try:
            if full_race.source_proof(ROOT) != expected_source:
                raise Failure('performance compilation source changed')
        except full_race.shards.Failure as error:
            raise Failure(str(error)) from error
    require_source()
    with tempfile.TemporaryDirectory(prefix='agent-archive-ci-compile-') as directory:
        command = ['go', 'test', '-c', '-p', '2', '-o', directory, *packages]
        checked(command, out/'performance-compile.log', deadline, env)
        for package in packages:
            binary = Path(directory)/(package.rsplit('/', 1)[1]+'.test')
            if binary.is_symlink() or not binary.is_file():
                raise Failure('performance compilation lacks required binary')
        require_source()
    return {'source_proof': expected_source, 'packages': packages,
            'parallel': 2, 'completed': True}


def expected_real_manager_skip(record):
    """One fake-manager case is unrepresentable on the actual user manager."""
    name = 'TestRealUserManagerConformance/UnreadableAndAbsentDefinitions'
    if (record['package'] != MODULE+'internal/scheduler/systemd'
            or record['test'] != name or record['status'] != 'skip'):
        return False
    # Preserve the raw reason in evidence, but exclude only Go's own frames
    # when checking the exact diagnostic. Extra/changed diagnostics fail closed.
    lines = [line for line in record.get('reason', '').splitlines()
             if line != '=== RUN   '+name
             and not re.fullmatch(r'--- SKIP: '+re.escape(name)+r' \([0-9.]+s\)', line)]
    return len(lines) == 1 and re.fullmatch(
        r'\s*conformance\.go:\d+: a real manager cannot be put in a loaded state over damaged unit files',
        lines[0]) is not None


def verify_selected(actual, selected, children=None, real_manager=False):
    passed = {(r['package'], r['test']) for r in actual if r['status'] == 'pass'}
    for pkg, names in selected.items():
        for name in names:
            if (pkg, name) not in passed:
                raise Failure('selected full body did not pass: '+pkg+'/'+name)
            if any(r['status'] != 'pass' and not (real_manager and expected_real_manager_skip(r))
                   for r in actual if r['package'] == pkg and (r['test'] == name or r['test'].startswith(name+'/'))):
                raise Failure('selected full body skipped a child')
            for child in (children or {}).get((pkg, name), []):
                if (pkg, name+'/'+child) not in passed:
                    raise Failure('required full-scale child missing')


SOURCE_AUDIT_PACKAGE = MODULE + 'internal/agents/builtin'
SOURCE_AUDIT_TEST = 'TestNativeCaptureWholeSourceBoundary'
SOURCE_AUDIT_ENV = 'AGENT_ARCHIVE_PLAIN_SOURCE_AUDIT_OWNED'
SOURCE_AUDIT_SKIP = 'whole-source audit owned by required plain source-audit gate; race body NOT RUN'
SOURCE_AUDIT_COMMAND = ['go', 'test', '-count=1', '-json', '-timeout', '12m', '-p', '1',
                        '-parallel', '1', '-run', '^' + SOURCE_AUDIT_TEST + '$', './internal/agents/builtin']


def verify_owned_audit_skip(raw):
    actual = terminals(raw)
    audit = [r for r in actual if r['package'] == SOURCE_AUDIT_PACKAGE and r['test'] == SOURCE_AUDIT_TEST]
    if len(audit) != 1 or audit[0]['status'] != 'skip' or SOURCE_AUDIT_SKIP not in audit[0].get('reason', ''):
        raise Failure('owned race audit must record explicit NOT RUN skip')


def source_audit_body(raw):
    selected = {SOURCE_AUDIT_PACKAGE: [SOURCE_AUDIT_TEST]}
    verify_pr_stream(raw, selected)
    lifecycle = [r['Action'] for r in full_race.shards.json_stream(raw)
                 if r.get('Test') == SOURCE_AUDIT_TEST and r.get('Action') != 'output']
    if lifecycle != ['run', 'pass']:
        raise Failure('plain source audit lacks actual named passing body')
    actual = body_evidence(raw, short=False)
    verify_selected(actual['actual'], selected)
    return actual


PLAIN_VET_COMMAND = ['go', 'vet', './...']
PLAIN_VET_IDENTITY_SCHEMA = 'plain-vet-compatible-v1'


def plain_vet_context():
    """Use the existing strict Go/compiler identity for native Linux plain vet."""
    import go_cache_context
    try:
        context = go_cache_context.runtime_context()
    except go_cache_context.InvalidContext as error:
        raise Failure('plain vet runtime context invalid') from error
    if context['go']['GOARCH'] != 'amd64':
        raise Failure('plain vet requires native Linux amd64')
    return dict(context, race=False)


def plain_vet_identity(context):
    """Cross-job vet identity excludes only the runner image release label.

    Raw context and cache keys retain that label. Go/compiler/tool/OS identity
    and ImageOS remain part of this projection, including any future fields.
    """
    image = context.get('image') if isinstance(context, dict) else None
    if (not isinstance(image, dict) or set(image) != {'ImageOS','ImageVersion'}
            or any(not isinstance(v,str) or not re.fullmatch('[A-Za-z0-9_.-]+',v)
                   for v in image.values())):
        raise Failure('plain vet raw image provenance missing')
    return dict(context, image={'ImageOS':image['ImageOS']})


def execute_plain_vet(out, deadline):
    env = dict(os.environ, AGENT_ARCHIVE_REAL_SYSTEMD='0')
    env.pop('AGENT_ARCHIVE_PERF', None)
    before = full_race.source_proof(ROOT)
    context = plain_vet_context()
    started = time.monotonic()
    raw = checked(PLAIN_VET_COMMAND, out/'plain-vet.txt', deadline, env)
    after = full_race.source_proof(ROOT)
    if before != after or context != plain_vet_context():
        raise Failure('plain vet source or context changed during execution')
    return {'command': PLAIN_VET_COMMAND, 'log': 'plain-vet.txt', 'exit_code': 0,
            'pre_source_proof': before, 'post_source_proof': after, 'context': context,
            'identity_schema': PLAIN_VET_IDENTITY_SCHEMA,
            'log_sha256': hashlib.sha256(raw.encode()).hexdigest(),
            'invocation_seconds': time.monotonic()-started,
            'performance_enabled': False, 'real_systemd_enabled': False}


def verify_plain_vet(directory, report, expected, expected_context_sha256, historical=False):
    """Require canonical successful execution receipt separately from audit bodies."""
    import run_go_shard
    receipt = report.get('plain_vet')
    if not isinstance(receipt, dict):
        raise Failure('owned plain vet receipt missing')
    context = receipt.get('context', {})
    if not isinstance(context, dict) or not isinstance(context.get('go'), dict):
        raise Failure('plain vet recorded context malformed')
    identity_schema = receipt.get('identity_schema')
    if identity_schema == PLAIN_VET_IDENTITY_SCHEMA:
        compatibility = plain_vet_identity(context)
    elif historical and identity_schema is None:
        plain_vet_identity(context)  # Validate full raw image provenance too.
        compatibility = context  # Old full-digest receipts remain historical facts only.
    else:
        raise Failure('plain vet compatibility identity schema missing or unsupported')
    go = context['go']
    import go_cache_context
    hashes = [context.get('os_release')]
    for compiler in ('cc', 'cxx'):
        identity = context.get(compiler, {})
        if not isinstance(identity, dict) or set(identity) != {'binary', 'version'}:
            raise Failure('plain vet compiler identity missing')
        hashes.extend(identity.values())
    tools = context.get('toolchain', {})
    if not isinstance(tools, dict) or set(tools) != {'go','compile','asm','link','cgo','vet'}:
        raise Failure('plain vet toolchain identity missing')
    hashes.extend(tools.values())
    if (context.get('schema') != go_cache_context.SCHEMA or set(go) != set(go_cache_context.GO_FIELDS)
            or any(not isinstance(v,str) for v in go.values())
            or any(not isinstance(v,str) or not re.fullmatch('[0-9a-f]{64}',v) for v in hashes)):
        raise Failure('plain vet compiler/environment identity malformed')
    if (context.get('race') is not False or context.get('tags') != []
            or context.get('version') != 'go version go1.27.2 linux/amd64'
            or context.get('runner_os') != 'Linux' or context.get('runner_arch') != 'X64'
            or any(go.get(k) != v for k,v in {'GOVERSION':'go1.27.2', 'GOOS':'linux',
                'GOHOSTOS':'linux','GOARCH':'amd64','GOHOSTARCH':'amd64','CGO_ENABLED':'1',
                'GOTOOLCHAIN':'local','GOWORK':'off','GOFLAGS':'-mod=readonly'}.items())
            or not isinstance(expected_context_sha256,str)
            or not re.fullmatch('[0-9a-f]{64}',expected_context_sha256)
            or run_go_shard.digest(compatibility) != expected_context_sha256):
        raise Failure('plain vet recorded context mismatch')
    if not historical and compatibility != plain_vet_identity(plain_vet_context()):
        raise Failure('plain vet current context mismatch')
    if (receipt.get('command') != PLAIN_VET_COMMAND or receipt.get('log') != 'plain-vet.txt'
            or type(receipt.get('exit_code')) is not int or receipt['exit_code'] != 0
            or receipt.get('pre_source_proof') != expected or receipt.get('post_source_proof') != expected
            or receipt.get('performance_enabled') is not False
            or receipt.get('real_systemd_enabled') is not False):
        raise Failure('plain vet execution/source receipt mismatch')
    try:
        raw = (Path(directory)/'plain-vet.txt').read_bytes()
    except OSError as error:
        raise Failure('plain vet raw log missing') from error
    if receipt.get('log_sha256') != hashlib.sha256(raw).hexdigest():
        raise Failure('plain vet raw log mismatch')


def verify_source_audit(directory, source, *, expected_source_proof=None, require_plain_vet=False, expected_vet_context_sha256=None):
    """Require exact-source plain named body and package PASS, never compilation."""
    directory = Path(directory)
    report = json.loads((directory / 'tier.json').read_text())
    proof = report.get('source_proof', {})
    if (report.get('schema') != 1 or report.get('success') is not True
            or report.get('source') != source or proof.get('source') != source
            or not re.fullmatch('[0-9a-f]{40}', proof.get('tree', ''))
            or report.get('tier') != 'pr' or report.get('suite') != 'audit'
            or len(report.get('invocations', [])) != 1):
        raise Failure('plain source audit source/status mismatch')
    # Historical importers resolve this proof from Git, never from the report.
    # Live callers retain the current tracked-byte and event-source guard.
    expected = full_race.source_proof(ROOT) if expected_source_proof is None else expected_source_proof
    if (not isinstance(expected, dict) or set(expected) != {'source', 'tree'}
            or expected.get('source') != source
            or not re.fullmatch('[0-9a-f]{40}', expected.get('tree', ''))
            or proof != expected):
        raise Failure('plain source audit tracked source proof mismatch')
    invocation = report['invocations'][0]
    if (invocation.get('command') != SOURCE_AUDIT_COMMAND or invocation.get('log') != 'source-audit.jsonl'
            or invocation.get('performance_enabled') is not False
            or invocation.get('real_systemd_enabled') is not False):
        raise Failure('plain source audit command/environment mismatch')
    raw = (directory / 'source-audit.jsonl').read_text()
    actual = source_audit_body(raw)
    if invocation.get('evidence') != actual:
        raise Failure('plain source audit raw body evidence mismatch')
    if require_plain_vet:
        verify_plain_vet(directory, report, expected, expected_vet_context_sha256,
                         historical=expected_source_proof is not None)
    return report


def run_suite(tier, suite, out, extras=None, seconds=2400, *, own_plain_vet=False):
    if own_plain_vet and (tier != 'pr' or suite != 'audit'):
        raise Failure('plain vet ownership requires PR source-audit suite')
    out.mkdir(parents=True, exist_ok=True)
    source = git('rev-parse', 'HEAD')
    if source != os.environ.get('GITHUB_SHA', source):
        raise Failure('tier source differs from event SHA')
    report = {'schema': 1, 'source': source, 'tier': tier, 'suite': suite,
              'platform': platform.system(), 'event': os.environ.get('GITHUB_EVENT_NAME','local'),
              'ref': os.environ.get('GITHUB_REF',''), 'schedule': os.environ.get('CI_SCHEDULE',''),
              'success': False, 'invocations': [], 'extras': extras}
    path = out / 'tier.json'
    path.write_text(json.dumps(report, indent=2)+'\n')
    deadline = time.monotonic()+seconds
    if tier == 'pr':
        report['source_proof'] = full_race.source_proof(ROOT)
    def execute(command, name, short=False, selected=None, children=None, env=None, real_manager=False):
        if env is None:
            env = dict(os.environ, AGENT_ARCHIVE_REAL_SYSTEMD='0')
            env.pop('AGENT_ARCHIVE_PERF', None)
        invocation = {'command': command, 'log': name+'.jsonl', 'evidence': None,
                      'performance_enabled': env.get('AGENT_ARCHIVE_PERF') == '1',
                      'real_systemd_enabled': env.get('AGENT_ARCHIVE_REAL_SYSTEMD') == '1'}
        report['invocations'].append(invocation)
        path.write_text(json.dumps(report, indent=2)+'\n')
        text = checked(command, out/(name+'.jsonl'), deadline, env)
        if command[0] == 'bash':
            text = '\n'.join(line for line in text.splitlines() if line.startswith('{'))
        proof = source_audit_body(text) if command == SOURCE_AUDIT_COMMAND else body_evidence(text, short)
        if selected:
            if tier == 'pr' or command == SOURCE_AUDIT_COMMAND:
                verify_pr_stream(text, selected)
            verify_selected(proof['actual'], selected, children, real_manager=real_manager)
        invocation['evidence'] = proof
        path.write_text(json.dumps(report, indent=2)+'\n')
    if suite == 'audit' or tier == 'daily' and suite == 'linux':
        audit_source = full_race.source_proof(ROOT)
        report['audit_source_proof'] = audit_source
        audit_started = time.monotonic()
        execute(SOURCE_AUDIT_COMMAND, 'source-audit', selected={SOURCE_AUDIT_PACKAGE: [SOURCE_AUDIT_TEST]})
        report['audit_plain_seconds'] = time.monotonic() - audit_started
        if full_race.source_proof(ROOT) != audit_source:
            raise Failure('plain source audit source changed during execution')
    common = ['go', 'test', '-race', '-count=1', '-json', '-timeout', '20m']
    if suite == 'macos' and tier in ('pr', 'daily'):
        execute(['bash', 'scripts/test_macos_smoke.sh'], 'native-smoke', short=True)
        if not report['invocations'][-1]['evidence']['actual']:
            raise Failure('native smoke executed no bodies')
    if suite == 'macos' and tier == 'daily':
        execute(common + ['-short', './internal/config', './internal/catalog', './internal/capture', './internal/sourceio'], 'native-integration', True)
    if suite == 'macos' and tier == 'pr' and extras and extras['native']:
        selected = {MODULE+'internal/cli': ['TestCodexExceptionRequiresNamedOtherAppConsentBeforeAdmission', 'TestRealCLIOverflowShowKeepsFullBodyAndSourceAuthority', 'TestRealCLICatalogDefaultList50Bounded'],
                    MODULE+'internal/catalog': ['TestPublicationBoundsAndCoordinatorMutationRefuseBeforeWrite', 'TestMissingCorruptAndOversizedNodesFailClosed', 'TestPickerUnsupportedAndBudgetAreNotEmptyProofs']}
        for pkg, names in selected.items():
            relative = './'+pkg[len(MODULE):]
            listing = discover([relative], out/('native-list-'+pkg.rsplit('/',1)[1]+'.jsonl'), deadline, race=True)
            if not set(names) <= set(listing.get(pkg, [])):
                raise Failure('native sensitive regression absent from discovery')
            pattern = '^('+'|'.join(re.escape(n) for n in names)+')$'
            execute(common + ['-short', '-run', pattern, relative], 'native-sensitive-'+pkg.rsplit('/',1)[1], True, selected={pkg:names})
    if suite in ('linux', 'extra') or suite == 'macos' and tier == 'full':
        packages = extras['packages'] if suite == 'extra' else ['./...']
        if packages:
            if tier == 'full':
                try:
                    full_race.run(packages, out, deadline, report,
                                  lambda: path.write_text(json.dumps(report, indent=2)+'\n'),
                                  checked, execute, ROOT, body_evidence, verify_selected,
                                  catalog()['restorations'])
                except full_race.shards.Failure as error:
                    raise Failure(str(error)) from error
            else:
                if tier == 'pr':
                    report['representative_discovery'] = discover(packages, out/'representative-list.jsonl', deadline, race=True)
                    for relative, names in extras['regressions'].items():
                        pkg = MODULE+relative
                        if not set(names) <= set(report['representative_discovery'].get(pkg, [])):
                            raise Failure('PR targeted regression missing from compiled source')
                        pattern = '^('+'|'.join(re.escape(n) for n in names)+')$'
                        execute(common + ['-short', '-run', pattern, './'+relative], 'regression-'+relative.replace('/', '-'), True, selected={pkg:names})
                else:
                    short_env = dict(os.environ, AGENT_ARCHIVE_REAL_SYSTEMD='0', **{SOURCE_AUDIT_ENV: '1'})
                    short_env.pop('AGENT_ARCHIVE_PERF', None)
                    execute(common + ['-short'] + packages, 'race', True, env=short_env)
                    if tier == 'daily':
                        verify_owned_audit_skip((out/'race.jsonl').read_text())
        if tier == 'daily' and suite == 'linux':
            chosen = [e for e in catalog()['restorations'] if e['restore'] == 'daily']
            by_pkg = {}
            children = {}
            for entry in chosen:
                pkg = MODULE+entry['package']
                by_pkg.setdefault(pkg, []).append(entry['test'])
                children[(pkg, entry['test'])] = entry.get('full_children', [])
            listing = discover(['./'+p[len(MODULE):] for p in by_pkg], out/'medium-list.jsonl', deadline, race=True)
            for pkg, names in by_pkg.items():
                if not set(names) <= set(listing.get(pkg, [])):
                    raise Failure('daily restoration absent from discovery')
                pattern = '^('+'|'.join(re.escape(n) for n in names)+')$'
                execute(common+['-run', pattern, './'+pkg[len(MODULE):]], 'medium-'+pkg.rsplit('/',1)[1], selected={pkg:names}, children=children)
        if tier == 'full' and packages:
            performance_env = dict(os.environ, AGENT_ARCHIVE_PERF='1', AGENT_ARCHIVE_REAL_SYSTEMD='0')
            report['performance_compile'] = prepare_performance(
                out, deadline, performance_env, report['full_schedule']['source_proof'])
            path.write_text(json.dumps(report, indent=2)+'\n')
            execute(PERFORMANCE, 'performance', env=performance_env)
    if suite == 'macos' and tier in ('daily', 'full'):
        execute(['bash','scripts/test_published_writer.sh'],'published-writer', selected={MODULE+'internal/config':['TestPublishedWriterRefusesProtectedConfiguration']})
    if suite == 'fuzz' or suite == 'extra' and extras['fuzz']:
        listing = discover(['./...'], out/'fuzz-list.jsonl', deadline)
        targets = [(pkg,n) for pkg,names in listing.items() for n in names if n.startswith('Fuzz')]
        if len(targets) < 24:
            raise Failure('fuzz discovery lost known targets')
        report['fuzz_discovered'] = targets
        report['fuzz_completed'] = []
        duration = '5s' if tier == 'daily' else '30s'
        for index,(pkg,name) in enumerate(targets):
            text = checked(['go','test','-count=1','-json',pkg,'-run','^$','-fuzz','^'+re.escape(name)+'$','-fuzztime',duration,'-fuzzminimizetime','2s'],out/f'fuzz-{index}.jsonl',deadline,dict(os.environ,AGENT_ARCHIVE_REAL_SYSTEMD='0'))
            # A successful package terminal and successful mutation process are required.
            if not any(json.loads(l).get('Action') == 'pass' and not json.loads(l).get('Test') for l in text.splitlines() if l) or 'fuzz: elapsed:' not in text:
                raise Failure('fuzz campaign lacks successful package terminal')
            report['fuzz_completed'].append([pkg,name])
            path.write_text(json.dumps(report,indent=2)+'\n')
        if report['fuzz_completed'] != [list(t) for t in targets]:
            raise Failure('fuzz campaign coverage mismatch')
        report['fuzz_seconds_per_target'] = duration
    if suite == 'systemd' or suite == 'extra' and extras['systemd']:
        if os.environ.get('AGENT_ARCHIVE_REAL_SYSTEMD') != '1':
            raise Failure('real systemd was not enabled')
        for pkg,pattern in [('systemd','^TestReal'),('cli','^TestRealSystemd')]:
            relative = './internal/scheduler/systemd' if pkg == 'systemd' else './internal/cli'
            listing = discover([relative],out/(pkg+'-list.jsonl'),deadline)
            selected = {p:[n for n in names if re.search(pattern,n)] for p,names in listing.items()}
            if not any(selected.values()):
                raise Failure('real systemd selection empty')
            execute(['go','test','-count=1','-json','-timeout','8m','-run',pattern,relative], 'systemd-'+pkg, selected=selected, env=dict(os.environ,AGENT_ARCHIVE_REAL_SYSTEMD='1'), real_manager=True)
    if tier == 'full' and report.get('full_schedule'):
        try:
            full_race.verify(out, report, packages, ROOT, body_evidence, verify_selected, catalog()['restorations'])
        except full_race.shards.Failure as error:
            raise Failure(str(error)) from error
    if own_plain_vet:
        report['plain_vet'] = execute_plain_vet(out, deadline)
    if tier == 'pr' and full_race.source_proof(ROOT) != report['source_proof']:
        raise Failure('PR regression source changed during execution')
    report['success'] = True
    path.write_text(json.dumps(report,indent=2)+'\n')
    if suite == 'audit':
        import run_go_shard
        context_hash = run_go_shard.digest(plain_vet_identity(report['plain_vet']['context'])) if own_plain_vet else None
        verify_source_audit(out, source, require_plain_vet=own_plain_vet, expected_vet_context_sha256=context_hash)


def verify_full(directory, plan):
    """Required verdict demands exact-source successful full/empty-extra proof."""
    path = directory / 'tier.json'
    report = json.loads(path.read_text())
    if plan.get('source') != os.environ['GITHUB_SHA']:
        raise Failure('plan source mismatch')
    if report.get('tier') == 'pr':
        verify_pr(directory, report, plan)
        return
    if (report.get('schema') != 1 or report.get('success') is not True
            or report.get('source') != os.environ['GITHUB_SHA'] or report.get('tier') != 'full'):
        raise Failure('full tier source/status mismatch')
    if (report.get('event') != os.environ.get('GITHUB_EVENT_NAME', 'local')
            or report.get('ref') != os.environ.get('GITHUB_REF', '')
            or report.get('schedule') != os.environ.get('CI_SCHEDULE', '')):
        raise Failure('full tier event/ref/schedule mismatch')
    extra = report.get('suite') == 'extra'
    if extra and report.get('extras') != plan:
        raise Failure('extra coverage differs from exact-source plan')
    if not extra and report.get('suite') != 'linux':
        raise Failure('unexpected full verdict suite')
    expected_packages = plan['packages'] if extra else ['./...']
    if expected_packages:
        try:
            full_race.verify(directory, report, expected_packages, ROOT, body_evidence, verify_selected, catalog()['restorations'])
        except full_race.shards.Failure as error:
            raise Failure(str(error)) from error
        expected_compile = {'source_proof': report['full_schedule']['source_proof'],
                            'packages': PERFORMANCE[-4:], 'parallel': 2, 'completed': True}
        if report.get('performance_compile') != expected_compile:
            raise Failure('performance compilation proof missing or changed')
    elif report['invocations'] or report.get('full_schedule') or report.get('full_discovery') or any(directory.glob('full-*')) or any(directory.glob('race*')):
        raise Failure('empty extra contains unexpected execution/discovery')
    for invocation in report['invocations']:
        name = invocation['log']
        if not re.fullmatch(r'[\w-]+\.jsonl', name):
            raise Failure('unsafe evidence path')
        text = (directory/name).read_text()
        if invocation['command'][0] == 'bash':
            text = '\n'.join(line for line in text.splitlines() if line.startswith('{'))
        if invocation['evidence'] != body_evidence(text, short=False):
            raise Failure('raw body evidence mismatch')
    allowed = {f'race-{n}.jsonl': cmd for n, cmd in enumerate(report.get('full_schedule', {}).get('commands', []))}
    if expected_packages:
        allowed['performance.jsonl'] = PERFORMANCE
    if extra and plan['systemd']:
        for name, pattern, relative in [('systemd', '^TestReal', './internal/scheduler/systemd'), ('cli', '^TestRealSystemd', './internal/cli')]:
            allowed['systemd-'+name+'.jsonl'] = ['go', 'test', '-count=1', '-json', '-timeout', '8m', '-run', pattern, relative]
    if (len(report['invocations']) != len(allowed)
            or {i['log'] for i in report['invocations']} != set(allowed)
            or any(i['command'] != allowed[i['log']] for i in report['invocations'])):
        raise Failure('unexpected, missing or weakened full invocation')
    if expected_packages and not any(i['log']=='performance.jsonl' and i.get('performance_enabled') is True for i in report['invocations']):
        raise Failure('full plain performance evidence missing')
    if extra and plan['systemd'] and not all(any(i['log']=='systemd-'+name+'.jsonl' for i in report['invocations']) for name in ('systemd','cli')):
        raise Failure('required real manager body evidence missing')
    if extra and plan['fuzz'] and (len(report.get('fuzz_discovered',[])) < 24 or report.get('fuzz_completed') != report.get('fuzz_discovered') or report.get('fuzz_seconds_per_target') != '30s'):
        raise Failure('required full fuzz campaign missing')


def verify_pr_stream(text, selected):
    # Body PASS events alone do not establish a complete successful package.
    try:
        full_race.shards.coverage(text, {p: True for p in selected},
                                  [n for names in selected.values() for n in names])
    except full_race.shards.Failure as error:
        raise Failure(str(error)) from error


def verify_pr(directory, report, plan):
    """Recompute exact-source focused short evidence without calling it full."""
    if (report.get('schema') != 1 or report.get('success') is not True
            or report.get('source') != os.environ['GITHUB_SHA'] or report.get('suite') != 'extra'
            or report.get('extras') != plan or plan.get('tier') != 'pr'
            or report.get('event') != os.environ.get('GITHUB_EVENT_NAME', 'local')
            or report.get('ref') != os.environ.get('GITHUB_REF', '')
            or report.get('schedule') != os.environ.get('CI_SCHEDULE', '')
            or report.get('source_proof') != full_race.source_proof(ROOT)):
        raise Failure('PR regression source/status/plan mismatch')
    if plan.get('ordinary_coverage') != 'required-linux-short-shards':
        raise Failure('PR plan omits mandatory ordinary coverage')
    allowed, selected_by_log = {}, {}
    for relative, names in plan.get('regressions', {}).items():
        name = 'regression-'+relative.replace('/', '-')+'.jsonl'
        pattern = '^('+'|'.join(re.escape(n) for n in names)+')$'
        allowed[name] = ['go', 'test', '-race', '-count=1', '-json', '-timeout', '20m', '-short', '-run', pattern, './'+relative]
        selected_by_log[name] = {MODULE+relative:names}
    if plan['systemd']:
        for name, pattern, relative in [('systemd', '^TestReal', './internal/scheduler/systemd'), ('cli', '^TestRealSystemd', './internal/cli')]:
            allowed['systemd-'+name+'.jsonl'] = ['go', 'test', '-count=1', '-json', '-timeout', '8m', '-run', pattern, relative]
    if report.get('full_schedule') or report.get('performance_compile') or report.get('fuzz_completed'):
        raise Failure('PR evidence contains unexpected full campaign')
    invocations = report.get('invocations', [])
    if len(invocations) != len(allowed) or {i['log'] for i in invocations} != set(allowed):
        raise Failure('missing or unexpected PR regression invocation')
    for invocation in invocations:
        name = invocation['log']
        if invocation['command'] != allowed[name] or invocation.get('performance_enabled'):
            raise Failure('weakened PR regression invocation')
        short = name.startswith('regression-')
        proof = body_evidence((directory/name).read_text(), short)
        if invocation['evidence'] != proof or not proof['actual']:
            raise Failure('PR regression raw evidence mismatch/empty')
        if short:
            listing = discover_from_log((directory/'representative-list.jsonl').read_text())
            if report.get('representative_discovery') != listing:
                raise Failure('PR compiled discovery differs from raw evidence')
            selected = selected_by_log[name]
            if any(not set(names) <= set(listing.get(pkg, [])) for pkg,names in selected.items()):
                raise Failure('PR selected compiled parent absent')
            verify_pr_stream((directory/name).read_text(), selected)
            verify_selected(proof['actual'], selected)
        if not short:
            listing = discover_from_log((directory/('systemd-list.jsonl' if name == 'systemd-systemd.jsonl' else 'cli-list.jsonl')).read_text())
            pattern = allowed[name][-2]
            selected = {p:[n for n in names if re.search(pattern,n)] for p,names in listing.items()}
            if not any(selected.values()) or not invocation.get('real_systemd_enabled'):
                raise Failure('PR real-manager selection empty/disabled')
            verify_pr_stream((directory/name).read_text(), selected)
            verify_selected(proof['actual'], selected, real_manager=True)


def discover_from_log(text):
    names, ended = {}, set()
    for line in text.splitlines():
        item = json.loads(line)
        if item.get('Action') in ('fail', 'build-fail', 'run'):
            raise Failure('invalid PR compiled listing')
        if item.get('Action') in ('pass', 'skip') and not item.get('Test'):
            ended.add(item.get('Package'))
        if item.get('Action') == 'output':
            name = item.get('Output', '').strip()
            if re.fullmatch(r'(Test|Fuzz|Example)\w*', name) and name != 'TestMain':
                if name in names.setdefault(item['Package'], []):
                    raise Failure('duplicate PR compiled listing name')
                names[item['Package']].append(name)
    if not ended or set(names) - ended:
        raise Failure('PR compiled listing lacks package terminals')
    return {p:sorted(n) for p,n in names.items()}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--plan', action='store_true')
    parser.add_argument('--verify', action='store_true')
    parser.add_argument('--verify-audit', action='store_true')
    parser.add_argument('--own-plain-vet', action='store_true')
    parser.add_argument('--tier', choices=['pr','daily','full'])
    parser.add_argument('--suite', choices=['linux','macos','fuzz','systemd','extra','audit'], default='linux')
    parser.add_argument('--output', type=Path, required=True)
    parser.add_argument('--extra-plan', type=Path)
    parser.add_argument('--seconds', type=int, default=2400)
    args = parser.parse_args()
    if args.seconds <= 0:
        parser.error('--seconds must be positive')
    def interrupted(signum, frame):
        raise Failure('tier command cancelled')
    for sig in (signal.SIGTERM, signal.SIGINT):
        signal.signal(sig, interrupted)
    try:
        event = os.environ.get('GITHUB_EVENT_NAME')
        resolved = resolve(event,os.environ.get('GITHUB_REF',''),os.environ.get('CI_SCHEDULE',''),os.environ.get('CI_SUITE','all')) if event else None
        tier = args.tier or resolved
        if tier is None:
            raise Failure('local execution requires an explicit tier')
        if event in ('push','workflow_dispatch'):
            tier = 'full'  # exact candidates and manual suites cannot be downgraded
        if event == 'schedule' and args.tier and args.tier != resolved:
            raise Failure('explicit tier conflicts with scheduled cadence')
        if args.verify_audit:
            import run_go_shard
            context_hash = run_go_shard.digest(plain_vet_identity(plain_vet_context())) if args.own_plain_vet else None
            verify_source_audit(args.output, os.environ['GITHUB_SHA'],
                require_plain_vet=args.own_plain_vet, expected_vet_context_sha256=context_hash)
            return 0
        if args.verify:
            verify_full(args.output, json.loads(args.extra_plan.read_text()))
            return 0
        if args.plan:
            tier = resolved or tier  # event-owned planning cannot be downgraded by a flag
            catalog()
            source = git('rev-parse','HEAD')
            if source != os.environ.get('GITHUB_SHA',source):
                raise Failure('plan source mismatch')
            extra = pr_extras(json.loads(Path(os.environ['GITHUB_EVENT_PATH']).read_text()),source) if tier == 'pr' else {'packages':[],'native':False,'systemd':False,'fuzz':False,'purge':False}
            plan = dict(extra, source=source, tier=tier)
            args.output.parent.mkdir(parents=True,exist_ok=True)
            args.output.write_text(json.dumps(plan,indent=2)+'\n')
            if os.environ.get('GITHUB_OUTPUT'):
                with open(os.environ['GITHUB_OUTPUT'],'a') as handle:
                    for key,value in {'tier':tier,'native':extra['native'],'systemd':extra['systemd'],'purge':extra['purge'],'linux':bool(extra['packages'] or extra['fuzz'] or extra['systemd'])}.items():
                        handle.write(f'{key}={str(value).lower()}\n')
        else:
            extra = json.loads(args.extra_plan.read_text()) if args.extra_plan else None
            if extra and extra['source'] != git('rev-parse','HEAD'):
                raise Failure('extra plan source mismatch')
            if tier == 'pr' and args.suite not in ('extra', 'macos', 'audit'):
                raise Failure('PR ordinary coverage belongs to exhaustive shards')
            run_suite(tier,args.suite,args.output,extra,args.seconds,own_plain_vet=args.own_plain_vet)
    except (Failure,ValueError,KeyError,TypeError,OSError,subprocess.SubprocessError) as error:
        print('tier gate failed: '+str(error),file=sys.stderr)
        return 1
    return 0


if __name__ == '__main__':
    sys.exit(main())
