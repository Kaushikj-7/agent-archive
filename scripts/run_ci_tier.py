#!/usr/bin/env python3
"""Explicit CI cadence, conservative PR backstops and actual execution evidence."""
import argparse
import json
import os
from pathlib import Path
import platform
import re
import signal
import subprocess
import sys
import time

ROOT = Path(__file__).resolve().parent.parent
MODULE = 'github.com/wangjohn/agent-archive/'


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


def extra_for_paths(paths):
    """Reviewed semantic dependency families; unmapped inputs require all backstops."""
    result = {'packages': [], 'native': False, 'systemd': False, 'fuzz': False, 'purge': False}
    families = {
        'catalog': {'reader', 'catalog', 'cli', 'storage', 'state', 'sourcefacts', 'collector', 'discovery'},
        'recovery': {'state', 'backfill', 'retention', 'revocation', 'setupjournal', 'collector', 'sourceio', 'discovery'},
        'archive': {'archive', 'agents', 'capture', 'nativecodec', 'transcriptio', 'sourcefacts'},
        'platform': {'scheduler', 'config', 'credentials', 'platform', 'setupresources', 'skillownership', 'termlaunch', 'terminal', 'sourcefacts', 'sourceio', 'discovery', 'capture'},
        'presentation': {'stats', 'statshtml', 'statsfmt', 'cli'},
        'purge': {'purge', 'retention', 'storage'},
    }
    groups = set()
    for path in paths:
        if path in {'CHANGELOG.md','CONTRIBUTING.md','CODE_OF_CONDUCT.md','LICENSE'}:
            continue  # ordinary doc/link and script gates cover these reviewed prose inputs
        if path.startswith('internal/archive/'):
            # Shared metadata/history/limits feed every product family.
            return dict(result, packages=['./...'], native=True, systemd=True, fuzz=True, purge=True)
        if path.startswith('internal/'):
            components = path.split('/')
            if len(components) < 3 or components[1] == 'testutil':
                return dict(result, packages=['./...'], native=True, systemd=True, fuzz=True, purge=True)
            found = {name for name, members in families.items() if components[1] in members}
            if not found:
                return dict(result, packages=['./...'], native=True, systemd=True, fuzz=True, purge=True)
            groups.update(found)
            if components[1] == 'cli':
                groups.update(families)  # setup, consent, purge and presentation share this facade
        elif path in ('docs/security/archive-cleanup.md', 'docs/getting-started/uninstall.md'):
            groups.add('purge')
        else:
            # Even unknown docs/config/test changes may affect declarative contracts.
            return dict(result, packages=['./...'], native=True, systemd=True, fuzz=True, purge=True)
    packages = set()
    for group in groups:
        for member in families[group]:
            if (ROOT / 'internal' / member).is_dir():
                packages.add('./internal/' + member + '/...')
    result.update(packages=sorted(packages), native='platform' in groups,
                  systemd='platform' in groups, fuzz='archive' in groups, purge='purge' in groups)
    return result


def pr_extras(event, tested):
    """Require exact base/head/tested-merge proof; uncertainty selects full."""
    full = {'packages': ['./...'], 'native': True, 'systemd': True, 'fuzz': True, 'purge': True}
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
        return dict(full, reason='full fallback: source proof unavailable')


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
    text = checked(['go', 'test', *(['-race'] if race else []), '-count=1', '-json', '-list', '^(Test|Fuzz|Example)', *packages], out, deadline, dict(os.environ, AGENT_ARCHIVE_REAL_SYSTEMD='0'))
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


def run_suite(tier, suite, out, extras=None, seconds=2400):
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
    def execute(command, name, short=False, selected=None, children=None, env=None, real_manager=False):
        if env is None:
            env = dict(os.environ, AGENT_ARCHIVE_REAL_SYSTEMD='0')
        text = checked(command, out/(name+'.jsonl'), deadline, env)
        if command[0] == 'bash':
            text = '\n'.join(line for line in text.splitlines() if line.startswith('{'))
        proof = body_evidence(text, short)
        if selected:
            verify_selected(proof['actual'], selected, children, real_manager=real_manager)
        report['invocations'].append({'command': command, 'log': name+'.jsonl', 'evidence': proof,
                                      'performance_enabled': env.get('AGENT_ARCHIVE_PERF') == '1',
                                      'real_systemd_enabled': env.get('AGENT_ARCHIVE_REAL_SYSTEMD') == '1'})
        path.write_text(json.dumps(report, indent=2)+'\n')
    common = ['go', 'test', '-race', '-count=1', '-json', '-timeout', '20m']
    if suite == 'macos' and tier == 'daily':
        execute(['bash', 'scripts/test_macos_smoke.sh'], 'native-smoke', short=True)
        if not report['invocations'][-1]['evidence']['actual']:
            raise Failure('native smoke executed no bodies')
    if suite in ('linux', 'extra') or suite == 'macos' and tier == 'full':
        packages = extras['packages'] if suite == 'extra' else ['./...']
        if packages:
            execute(common + (['-short'] if tier == 'daily' else []) + packages, 'race', tier == 'daily')
            if tier == 'full':
                required = {}
                children = {}
                for entry in catalog()['restorations']:
                    relative = './'+entry['package']
                    included = any(p == './...' or p == relative or p.endswith('/...') and (relative == p[:-4] or relative.startswith(p[:-3])) for p in packages)
                    if included:
                        pkg = MODULE+entry['package']
                        required.setdefault(pkg, []).append(entry['test'])
                        children[(pkg,entry['test'])] = entry.get('full_children', [])
                verify_selected(report['invocations'][-1]['evidence']['actual'], required, children)
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
            execute(['go', 'test', '-v', '-json', '-p', '1', '-count=1', '-run', 'StayFast|FiveMegabyte|TestCursorOverlappingHooksRegisterOnce', './internal/collector', './internal/archive', './internal/agents/nativecodec', './internal/capture'], 'performance', env=dict(os.environ, AGENT_ARCHIVE_PERF='1', AGENT_ARCHIVE_REAL_SYSTEMD='0'))
    if suite == 'macos':
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
    report['success'] = True
    path.write_text(json.dumps(report,indent=2)+'\n')


def verify_full(directory, plan):
    """Required verdict demands exact-source successful full/empty-extra proof."""
    path = directory / 'tier.json'
    report = json.loads(path.read_text())
    if plan.get('source') != os.environ['GITHUB_SHA']:
        raise Failure('plan source mismatch')
    if (report.get('schema') != 1 or report.get('success') is not True
            or report.get('source') != os.environ['GITHUB_SHA'] or report.get('tier') != 'full'):
        raise Failure('full tier source/status mismatch')
    extra = report.get('suite') == 'extra'
    if extra and report.get('extras') != plan:
        raise Failure('extra coverage differs from exact-source plan')
    if not extra and report.get('suite') != 'linux':
        raise Failure('unexpected full verdict suite')
    expected_packages = plan['packages'] if extra else ['./...']
    race = [i for i in report['invocations'] if i['log'] == 'race.jsonl']
    if bool(race) != bool(expected_packages) or len(race) > 1:
        raise Failure('missing or unexpected full race invocation')
    if race and race[0]['command'] != ['go','test','-race','-count=1','-json','-timeout','20m',*expected_packages]:
        raise Failure('full race command weakened')
    for invocation in report['invocations']:
        name = invocation['log']
        if not re.fullmatch(r'[\w-]+\.jsonl', name):
            raise Failure('unsafe evidence path')
        text = (directory/name).read_text()
        if invocation['command'][0] == 'bash':
            text = '\n'.join(line for line in text.splitlines() if line.startswith('{'))
        if invocation['evidence'] != body_evidence(text, short=False):
            raise Failure('raw body evidence mismatch')
    if expected_packages and not any(i['log']=='performance.jsonl' and i.get('performance_enabled') is True for i in report['invocations']):
        raise Failure('full plain performance evidence missing')
    if extra and plan['systemd'] and not all(any(i['log']=='systemd-'+name+'.jsonl' for i in report['invocations']) for name in ('systemd','cli')):
        raise Failure('required real manager body evidence missing')
    if extra and plan['fuzz'] and (len(report.get('fuzz_discovered',[])) < 24 or report.get('fuzz_completed') != report.get('fuzz_discovered') or report.get('fuzz_seconds_per_target') != '30s'):
        raise Failure('required full fuzz campaign missing')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--plan', action='store_true')
    parser.add_argument('--verify', action='store_true')
    parser.add_argument('--tier', choices=['pr','daily','full'])
    parser.add_argument('--suite', choices=['linux','macos','fuzz','systemd','extra'], default='linux')
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
            if tier == 'pr' and args.suite != 'extra':
                raise Failure('PR ordinary coverage belongs to exhaustive shards')
            run_suite('full' if args.suite == 'extra' else tier,args.suite,args.output,extra,args.seconds)
    except (Failure,ValueError,KeyError,TypeError,OSError,subprocess.SubprocessError) as error:
        print('tier gate failed: '+str(error),file=sys.stderr)
        return 1
    return 0


if __name__ == '__main__':
    sys.exit(main())
