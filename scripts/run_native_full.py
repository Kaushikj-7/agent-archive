#!/usr/bin/env python3
"""Three independent Darwin workers over the existing exhaustive race evidence API."""
import argparse
import json
import os
from pathlib import Path
import platform
import re
import tempfile
import time

import full_race as full
import run_ci_tier as tier

ROOT = tier.ROOT
WORKERS = 3
RACE = full.COMMON + ['-p', '1', '-parallel', '2']
LIST = RACE + ['-list', '^(Test|Fuzz|Example)', './...']
Failure = full.shards.Failure


def context():
    return {key: os.environ.get(key, '') for key in
            ('GITHUB_EVENT_NAME', 'GITHUB_REF', 'CI_SCHEDULE', 'CI_SUITE',
             'GITHUB_RUN_ID', 'GITHUB_RUN_ATTEMPT')}


def native_environment():
    if platform.system() != 'Darwin':
        raise Failure('full native workers require Darwin')
    arch = {'arm64': 'arm64', 'x86_64': 'amd64'}.get(platform.machine())
    if arch is None:
        raise Failure('unsupported native architecture')
    return {'GOOS': 'darwin', 'GOARCH': arch, 'CGO_ENABLED': '1',
            'GOMAXPROCS': '2', 'GOFLAGS': '', 'GOEXPERIMENT': '', 'GOTOOLCHAIN': 'go1.27.2',
            'AGENT_ARCHIVE_REAL_SYSTEMD': '0'}


def proof():
    value = full.source_proof(ROOT)
    if value['source'] != os.environ.get('GITHUB_SHA', value['source']):
        raise Failure('native source differs from event SHA')
    return value


def save(out, name, value):
    (out / name).write_text(json.dumps(value, sort_keys=True, indent=2)+'\n')


def weights(names):
    profile = json.loads((ROOT/'scripts/native_full_hints.json').read_text())
    if profile.get('schema') != 1:
        raise Failure('unsupported native advisory weights')
    old = {(tier.MODULE+e['package'], e['test']): e for e in profile['restorations']}
    changed = {(tier.MODULE+e['package'], e['test']) for e in tier.catalog()['restorations']
               if old.get((tier.MODULE+e['package'], e['test'])) != e}
    units, unmeasured = {}, []
    for p, parents in names.items():
        selected = parents if p == full.CLI and parents else [None]
        for name in selected:
            unit = p+'::'+name if name is not None else p
            historical = profile['cli'].get(name) if name is not None else profile['packages'].get(p)
            changed_scale = (p, name) in changed if name is not None else any(pkg == p for pkg, _ in changed)
            if historical is None or changed_scale:
                # Unknown ordinary packages get one second per compiled parent;
                # unknown/changed CLI trees get one second. Positive and explicit,
                # but unmeasured until the exact-source candidate run qualifies them.
                units[unit] = 1000 if name is not None else max(1,len(parents))*1000
                unmeasured.append(unit)
            else:
                if type(historical) is not int or historical < 1:
                    raise Failure('invalid native advisory duration')
                units[unit] = historical
    return units, sorted(unmeasured), full.shards.digest(profile)


def schedule(package_map, names):
    # Historical full Darwin costs rank current compiled units only. Whole
    # ordinary packages stay together; CLI parents own every descendant.
    units, unmeasured, profile_sha = weights(names)
    if full.CLI in names and not names[full.CLI]:
        units[full.CLI] = 1
    assignments, commands = [], []
    for group in full.shards.partition(list(units), units, WORKERS):
        selected = {}
        for unit in group:
            if '::' in unit:
                p, name = unit.split('::')
                selected.setdefault(p, []).append(name)
            else:
                selected[unit] = names[unit]
        selected = {p: sorted(ns) for p, ns in sorted(selected.items())}
        ordinary = sorted(p for p in selected if p != full.CLI)
        invocations = []
        if ordinary:
            invocations.append({'command': RACE+ordinary,
                                'selected': {p: selected[p] for p in ordinary}})
        if full.CLI in selected:
            cli = selected[full.CLI]
            invocations.append({'command': RACE+(['-run', full.shards.selector(cli)] if cli else [])+[full.CLI],
                                'selected': {full.CLI: cli}})
        assignments.append(selected)
        commands.append(invocations)
    return {'workers': WORKERS, 'weight_basis': 'historical full Darwin logical costs; advisory, not current elapsed time',
            'profile_sha256': profile_sha, 'unmeasured_units': unmeasured,
            'unmeasured_policy': '1000ms per CLI tree; 1000ms per compiled ordinary parent, minimum one',
            'packages': package_map, 'names': names,
            'assignments': assignments, 'commands': commands}


def plan_value(out, source, environment):
    packages = full.packages((out/'packages.json').read_text())
    names = full.listing((out/'listing.jsonl').read_text(), packages)
    return {'schema': 1, 'source_proof': source, 'context': context(),
            'environment': environment, 'catalog_sha256': full.shards.digest(tier.catalog()),
            'discovery': [{'command': ['go', 'list', '-json', './...'], 'log': 'packages.json'},
                          {'command': LIST, 'log': 'listing.jsonl'}],
            'schedule': schedule(packages, names)}


def load_plan(directory):
    value = json.loads((directory/'plan.json').read_text())
    expected = plan_value(directory, proof(), native_environment())
    if value != expected:
        raise Failure('native plan source/context/environment/catalog/discovery mismatch')
    receipt = json.loads((directory/'receipt.json').read_text())
    if (receipt.get('schema') != 1 or receipt.get('role') != 'plan'
            or receipt.get('source_proof') != value['source_proof']
            or receipt.get('context') != value['context']
            or receipt.get('environment') != value['environment']
            or receipt.get('success') is not True or receipt.get('plan_sha256') != full.shards.digest(value)
            or receipt.get('invocations') != [dict(d, completed=True) for d in value['discovery']]):
        raise Failure('native plan discovery incomplete or changed')
    return value


def restorations(actual, plan):
    selected, children = {}, {}
    names = plan['schedule']['names']
    for entry in tier.catalog()['restorations']:
        p = tier.MODULE+entry['package']
        if p not in names or entry['test'] not in names[p]:
            raise Failure('restoration missing from native compiled discovery')
        selected.setdefault(p, []).append(entry['test'])
        children[(p, entry['test'])] = entry.get('full_children', [])
    tier.verify_selected(actual, selected, children)


def performance_names(plan):
    selected = {}
    pattern = re.compile(tier.PERFORMANCE[tier.PERFORMANCE.index('-run')+1])
    for relative in tier.PERFORMANCE[-4:]:
        p = tier.MODULE+relative[2:]
        names = [n for n in plan['schedule']['names'].get(p, []) if pattern.search(n)]
        if not names:
            raise Failure('plain performance package has no compiled selected bodies')
        selected[p] = names
    return selected


def body_coverage(text, packages, selected):
    evidence = full.coverage(text, packages, selected, tier.body_evidence)
    starts = []
    for record in full.shards.json_stream(text):
        if record.get('Test') and record.get('Action') == 'run':
            starts.append((record.get('Package'), record['Test']))
    terminals = {(r['package'], r['test']) for r in evidence['actual']}
    if len(starts) != len(set(starts)) or set(starts) != terminals:
        raise Failure('native body lacks exactly one start and terminal')
    return evidence


def invocation(out, report, command, name, deadline, env, selected=None, packages=None):
    record = {'command': command, 'log': name+'.jsonl', 'completed': False,
              'performance_enabled': env.get('AGENT_ARCHIVE_PERF') == '1',
              'real_systemd_enabled': env.get('AGENT_ARCHIVE_REAL_SYSTEMD') == '1'}
    report['invocations'].append(record)
    save(out, 'receipt.json', report)
    started = time.monotonic()
    try:
        text = tier.checked(command, out/record['log'], deadline, env)
        if command[0] == 'bash':
            text = '\n'.join(line for line in text.splitlines() if line.startswith('{'))
        if packages is not None:
            evidence = body_coverage(text, packages, selected)
        else:
            evidence = body_coverage(text, {p: True for p in selected}, selected)
            tier.verify_selected(evidence['actual'], selected)
        record.update(completed=True, evidence=evidence)
    finally:
        record['elapsed_seconds'] = round(time.monotonic()-started, 3)
        save(out, 'receipt.json', report)


def execute(mode, out, directory=None, index=None, seconds=2400):
    out.mkdir(parents=True, exist_ok=False)
    report = {'schema': 1, 'role': mode, 'success': False, 'invocations': []}
    save(out, 'receipt.json', report)
    deadline = time.monotonic()+seconds
    try:
        source, environment = proof(), native_environment()
        report.update(source_proof=source, context=context(), environment=environment)
        with tempfile.TemporaryDirectory(prefix='agent-archive-native-') as private:
            # Every job has disposable build/temp state; no artifact carries cache.
            temporary = Path(private)
            (temporary/'tmp').mkdir()
            env = dict(os.environ, **environment, GOCACHE=str(temporary/'build'),
                       TMPDIR=str(temporary/'tmp'))
            env.pop('AGENT_ARCHIVE_PERF', None)
            if mode == 'plan':
                for command, log in ((['go','list','-json','./...'], 'packages.json'), (LIST, 'listing.jsonl')):
                    record = dict(command=command, log=log, completed=False)
                    report['invocations'].append(record)
                    save(out, 'receipt.json', report)
                    tier.checked(command, out/log, deadline, env)
                    record['completed'] = True
                value = plan_value(out, source, environment)
                save(out, 'plan.json', value)
                report['plan_sha256'] = full.shards.digest(value)
            else:
                value = load_plan(directory)
                report['plan_sha256'] = full.shards.digest(value)
                if mode == 'worker':
                    if type(index) is not int or not 0 <= index < WORKERS:
                        raise Failure('invalid native worker index')
                    report['worker'] = index
                    for n, unit in enumerate(value['schedule']['commands'][index]):
                        subset = {p: value['schedule']['packages'][p] for p in unit['selected']}
                        invocation(out, report, unit['command'], f'race-{n}', deadline, env,
                                   unit['selected'], subset)
                elif mode == 'obligations':
                    selected = performance_names(value)
                    perf_env = dict(env, AGENT_ARCHIVE_PERF='1')
                    report['performance_compile'] = tier.prepare_performance(out, deadline, perf_env, source)
                    invocation(out, report, tier.PERFORMANCE, 'performance', deadline, perf_env, selected)
                    invocation(out, report, ['bash','scripts/test_published_writer.sh'], 'published-writer',
                               deadline, env, {tier.MODULE+'internal/config': ['TestPublishedWriterRefusesProtectedConfiguration']})
                else:
                    raise Failure('invalid native role')
            if proof() != source:
                raise Failure('native tracked source changed during invocation')
        report['success'] = True
    except BaseException as error:
        report['error'] = str(error)
        raise
    finally:
        save(out, 'receipt.json', report)


def verify_receipt(directory, plan, role):
    value = json.loads((directory/'receipt.json').read_text())
    if (value.get('schema') != 1 or value.get('success') is not True or value.get('role') != role
            or value.get('source_proof') != plan['source_proof']
            or value.get('context') != plan['context'] or value.get('environment') != plan['environment']
            or value.get('plan_sha256') != full.shards.digest(plan)):
        raise Failure('native receipt failed, cancelled, mixed source/context or missing')
    return value


def verify_invocation(directory, record, command, selected, packages=None, performance=False):
    if (record.get('command') != command or record.get('completed') is not True
            or record.get('performance_enabled') is not performance
            or record.get('real_systemd_enabled') is not False):
        raise Failure('native invocation missing or weakened')
    text = (directory/record['log']).read_text()
    if command[0] == 'bash':
        text = '\n'.join(line for line in text.splitlines() if line.startswith('{'))
    if packages is not None:
        evidence = body_coverage(text, packages, selected)
    else:
        evidence = body_coverage(text, {p: True for p in selected}, selected)
        tier.verify_selected(evidence['actual'], selected)
    if record.get('evidence') != evidence:
        raise Failure('native raw body evidence changed')
    return evidence['actual']


def reconcile(directory, needs):
    # GitHub job conclusions are mandatory as well as successful raw receipts.
    expected = {'macos-plan', 'macos-workers', 'macos-obligations'}
    if set(needs) != expected or any(needs[n].get('result') != 'success' for n in expected):
        raise Failure('missing, failed, cancelled or skipped native job')
    plan = load_plan(directory/'native-plan')
    required_artifacts = {'native-plan', 'native-obligations', *(f'native-worker-{i}' for i in range(WORKERS))}
    if {p.name for p in directory.glob('native-*')} != required_artifacts:
        raise Failure('missing or unexpected native artifact')
    actual = []
    for index in range(WORKERS):
        out = directory/f'native-worker-{index}'
        report = verify_receipt(out, plan, 'worker')
        units = plan['schedule']['commands'][index]
        if report.get('worker') != index or len(report['invocations']) != len(units):
            raise Failure('duplicate native worker or missing invocation')
        for n, (record, unit) in enumerate(zip(report['invocations'], units)):
            if record.get('log') != f'race-{n}.jsonl':
                raise Failure('native race log changed')
            subset = {p: plan['schedule']['packages'][p] for p in unit['selected']}
            actual.extend(verify_invocation(out, record, unit['command'], unit['selected'], subset))
    if len({(r['package'], r['test']) for r in actual}) != len(actual):
        raise Failure('duplicate native body across workers')
    restorations(actual, plan)
    out = directory/'native-obligations'
    report = verify_receipt(out, plan, 'obligations')
    compile_proof = dict(source_proof=plan['source_proof'], packages=tier.PERFORMANCE[-4:], parallel=2, completed=True)
    if report.get('performance_compile') != compile_proof or not (out/'performance-compile.log').is_file():
        raise Failure('plain performance compilation preflight missing')
    required = [('performance.jsonl', tier.PERFORMANCE, performance_names(plan), True),
                ('published-writer.jsonl', ['bash','scripts/test_published_writer.sh'],
                 {tier.MODULE+'internal/config': ['TestPublishedWriterRefusesProtectedConfiguration']}, False)]
    if len(report['invocations']) != len(required):
        raise Failure('native plain obligations missing or duplicated')
    for record, (log, command, selected, performance) in zip(report['invocations'], required):
        if record.get('log') != log:
            raise Failure('native obligation log changed')
        verify_invocation(out, record, command, selected, performance=performance)
    if proof() != plan['source_proof']:
        raise Failure('native source changed during reconciliation')
    return {'schema': 1, 'success': True, 'source_proof': plan['source_proof'],
            'plan_sha256': full.shards.digest(plan), 'workers': WORKERS,
            'actual': actual, 'obligations': ['plain performance', 'pinned published writer']}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('mode', choices=('plan','worker','obligations','reconcile'))
    parser.add_argument('--output', type=Path, required=True)
    parser.add_argument('--plan', type=Path)
    parser.add_argument('--worker', type=int)
    parser.add_argument('--seconds', type=int, default=2400)
    parser.add_argument('--needs', default='{}')
    args = parser.parse_args()
    if args.mode == 'reconcile':
        save(args.output, 'verdict.json', {'schema': 1, 'success': False})
        result = reconcile(args.output, json.loads(args.needs))
        save(args.output, 'verdict.json', result)
    else:
        execute(args.mode, args.output, args.plan, args.worker, args.seconds)


if __name__ == '__main__':
    main()
