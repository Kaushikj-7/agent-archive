"""Bound native CLI process lifetimes without reducing full race coverage."""
import json
import os
from pathlib import Path
import re

import run_ci_tier as tier
import run_go_shard as shard
import scope_native_verify as scope


def source_guard(source, out=None):
    """Require the commit's tracked bytes and modes, including hidden edits."""
    if tier.git('rev-parse', 'HEAD') != source or source != os.environ.get('GITHUB_SHA', source):
        raise tier.Failure('native source differs from tested commit')
    if scope.git(tier.ROOT, 'diff', '--cached', '--name-only', source, '--').strip():
        raise tier.Failure('native index differs from tested commit')
    # Ignored files can still alter Go builds (for example go.work or an
    # ignored directory containing Go packages), so inspect them as well.
    for raw_path in scope.records(scope.git(tier.ROOT, 'ls-files', '--others', '-z')):
        path = scope.repository_path(raw_path)
        item = tier.ROOT/path
        # Runner logs and Python's import cache are the only generated inputs
        # allowed in the checkout. In particular, added Go files must not alter
        # discovery while masquerading as the tested commit's source.
        import_cache = Path(path).parent == Path('scripts/__pycache__') and item.suffix == '.pyc'
        artifact = (out is not None and not item.is_symlink() and item.resolve().is_relative_to(Path(out).resolve())
                    and item.suffix in ('.json', '.jsonl'))
        if not import_cache and not artifact:
            raise tier.Failure('untracked native source input: '+path)
    for record in scope.records(scope.git(tier.ROOT, 'ls-tree', '-r', '-z', source)):
        metadata, raw_path = record.split(b'\t', 1)
        mode, kind, oid = metadata.split(b' ')
        path = scope.repository_path(raw_path)
        if (mode not in (b'100644', b'100755') or kind != b'blob'
                or not scope.working_input_matches(tier.ROOT, path, mode, oid)):
            raise tier.Failure('native tracked source differs from tested commit')


def listing(text, packages, require_cli=True):
    """Discover every runnable name and establish every package terminal."""
    shard.coverage(text, packages)
    names = {package: [] for package in packages}
    for event in shard.json_stream(text):
        if event.get('Test') or event['Action'] in ('run', 'pause', 'cont'):
            raise tier.Failure('native listing unexpectedly executed a body')
        if event['Action'] == 'output':
            name = event.get('Output', '').strip()
            if name.startswith(('Test', 'Fuzz', 'Example')):
                if (not re.fullmatch(r'(Test|Fuzz|Example)\w*', name)
                        or name == 'TestMain' or name in names[event['Package']]):
                    raise tier.Failure('invalid or duplicate native runnable name')
                names[event['Package']].append(name)
    if require_cli and len(names[shard.CLI]) < 2:
        raise tier.Failure('native CLI listing cannot form complete units')
    return {package: sorted(values) for package, values in names.items()}


def coverage(text, packages, selected):
    """Retain skips and reject missing, duplicate, or unassigned body events."""
    shard.coverage(text, packages)
    runs, ended = set(), set()
    for event in shard.json_stream(text):
        package, test, action = event['Package'], event.get('Test'), event['Action']
        if not test:
            continue
        if (not isinstance(test, str) or test.split('/')[0] not in selected[package]
                or action not in ('run', 'pause', 'cont', 'output', 'pass', 'skip')):
            raise tier.Failure('unassigned native body event')
        key = (package, test)
        if action == 'run':
            if key in runs:
                raise tier.Failure('duplicate native body run')
            runs.add(key)
        if action in ('pass', 'skip'):
            if key not in runs or key in ended:
                raise tier.Failure('missing run or duplicate native body terminal')
            ended.add(key)
    expected = {(package, name) for package, names in selected.items() for name in names}
    if runs != ended or {key for key in ended if '/' not in key[1]} != expected:
        raise tier.Failure('missing native body terminals')
    return tier.body_evidence(text, short=False)


def commands(catalog, cpus):
    common = ['go', 'test', '-race', '-count=1', '-json', '-timeout', '20m', '-p', '1', '-parallel', str(cpus)]
    ordinary = sorted(set(catalog['packages']) - {shard.CLI})
    result = [('native-ordinary', common + ordinary, ordinary)]
    for index, names in enumerate(catalog['assignments']):
        result.append((f'native-cli-{index}', common + ['-run', shard.selector(names), shard.CLI], [shard.CLI]))
    return result


def _verify(out, report, require_obligations):
    """Reconcile assignments with immutable discovery and actual unit logs."""
    native = report['native_full']
    catalog = native['catalog']
    source_guard(report['source'], out)
    if (catalog['source'] != report['source'] or native['catalog_sha256'] != shard.digest(catalog)
            or type(native['cpus']) is not int or not 1 <= native['cpus'] <= 4):
        raise tier.Failure('native catalog/source/settings mismatch')
    packages = shard.metadata((out/'native-packages.json').read_text())
    discovered = listing((out/'native-list.jsonl').read_text(), packages)
    performance_packages = [tier.MODULE+p[2:] for p in tier.FULL_PERFORMANCE_COMMAND if p.startswith('./')]
    performance_names = listing((out/'native-performance-list.jsonl').read_text(),
                                {p: packages[p] for p in performance_packages}, require_cli=False)
    if catalog['performance_names'] != performance_names:
        raise tier.Failure('native plain performance discovery mismatch')
    assignments = catalog['assignments']
    if (catalog['packages'] != packages or catalog['names'] != discovered or len(assignments) != 2
            or any(not unit or len(unit) != len(set(unit)) for unit in assignments)
            or set(assignments[0]) & set(assignments[1])
            or set(assignments[0]) | set(assignments[1]) != set(discovered[shard.CLI])):
        raise tier.Failure('native assignment is not disjoint and exhaustive')
    invocations = report['invocations']
    units = [item for item in invocations if item['log'].startswith('native-')]
    expected = commands(catalog, native['cpus'])
    if len(units) != len(expected):
        raise tier.Failure('missing or duplicate native unit invocation')
    raw = []
    for item, (name, command, assigned_packages) in zip(units, expected):
        if (item['command'] != command or item['log'] != name+'.jsonl'
                or item['performance_enabled'] or item['real_systemd_enabled']):
            raise tier.Failure('native unit command or environment weakened')
        selected = {package: discovered[package] for package in assigned_packages}
        if assigned_packages == [shard.CLI]:
            selected[shard.CLI] = assignments[int(name.rsplit('-', 1)[1])]
        text = (out/item['log']).read_text()
        if item['evidence'] != coverage(text, {p: packages[p] for p in assigned_packages}, selected):
            raise tier.Failure('native raw body evidence mismatch')
        raw.append(text)
    proof = tier.body_evidence('\n'.join(raw), short=False)
    required, children = {}, {}
    for entry in tier.catalog()['restorations']:
        package = tier.MODULE+entry['package']
        required.setdefault(package, []).append(entry['test'])
        children[(package, entry['test'])] = entry.get('full_children', [])
    tier.verify_selected(proof['actual'], required, children)
    if 'evidence' in native and native['evidence'] != proof:
        raise tier.Failure('native aggregate raw body evidence mismatch')
    native['evidence'] = proof
    other = [item for item in invocations if not item['log'].startswith('native-')]
    obligations = [('performance.jsonl', tier.FULL_PERFORMANCE_COMMAND, True),
                   ('published-writer.jsonl', ['bash', 'scripts/test_published_writer.sh'], False)] if require_obligations else []
    if len(other) != len(obligations):
        raise tier.Failure('missing or unexpected native full obligation')
    for item, (name, command, performance_enabled) in zip(other, obligations):
        if (item['log'] != name or item['command'] != command
                or item['performance_enabled'] is not performance_enabled or item['real_systemd_enabled']):
            raise tier.Failure('native full obligation command or environment weakened')
        text = (out/name).read_text()
        if command[0] == 'bash':
            text = '\n'.join(line for line in text.splitlines() if line.startswith('{'))
        if name == 'published-writer.jsonl':
            selected = {tier.MODULE+'internal/config': ['TestPublishedWriterRefusesProtectedConfiguration']}
        else:
            pattern = command[command.index('-run')+1]
            selected = performance_names
            if any(not re.search(pattern, test) for names in selected.values() for test in names):
                raise tier.Failure('native plain performance discovery contains unselected body')
            if not any(selected.values()):
                raise tier.Failure('native performance selection is empty')
        proof = coverage(text, {package: packages[package] for package in selected}, selected)
        if item['evidence'] != proof:
            raise tier.Failure('native full obligation raw body evidence mismatch')
        tier.verify_selected(proof['actual'], selected)


def _run(out, deadline, report, report_path):
    """Run ordinary packages and two CLI units sequentially under one deadline."""
    source_guard(report['source'], out)
    env = dict(os.environ, AGENT_ARCHIVE_REAL_SYSTEMD='0', AGENT_ARCHIVE_PERF='0')
    packages_text = tier.checked(['go', 'list', '-json', './...'], out/'native-packages.json', deadline, env)
    packages = shard.metadata(packages_text)
    text = tier.checked(['go', 'test', '-race', '-count=1', '-json', '-p', '1', '-timeout', '20m',
                         '-list', '^(Test|Fuzz|Example)', './...'], out/'native-list.jsonl', deadline, env)
    names = listing(text, packages)
    # Plain builds may expose different tests than race builds. Discover the
    # unchanged performance command in its own build/environment.
    performance_command = list(tier.FULL_PERFORMANCE_COMMAND)
    performance_command[performance_command.index('-run')] = '-list'
    performance_packages = [tier.MODULE+p[2:] for p in performance_command if p.startswith('./')]
    performance_text = tier.checked(performance_command, out/'native-performance-list.jsonl', deadline,
                                    dict(env, AGENT_ARCHIVE_PERF='1'))
    performance_names = listing(performance_text, {p: packages[p] for p in performance_packages}, require_cli=False)
    hints = json.loads((tier.ROOT/'scripts/go_shard_hints.json').read_text())['cli']
    catalog = {'source': report['source'], 'packages': packages, 'names': names,
               'assignments': shard.partition(names[shard.CLI], hints), 'performance_names': performance_names}
    cpus = max(1, min(4, os.cpu_count() or 1))
    env['GOMAXPROCS'] = str(cpus)
    report['native_full'] = {'catalog': catalog, 'catalog_sha256': shard.digest(catalog), 'cpus': cpus}
    report_path.write_text(json.dumps(report, indent=2)+'\n')
    source_guard(report['source'], out)
    for name, command, assigned_packages in commands(catalog, cpus):
        # No overlapping unit processes: each gets a fresh package lifetime,
        # while discovery and all work still consume the same suite budget.
        text = tier.checked(command, out/(name+'.jsonl'), deadline, env)
        selected = {package: names[package] for package in assigned_packages}
        if assigned_packages == [shard.CLI]:
            selected[shard.CLI] = catalog['assignments'][int(name.rsplit('-', 1)[1])]
        proof = coverage(text, {p: packages[p] for p in assigned_packages}, selected)
        report['invocations'].append({'command': command, 'log': name+'.jsonl', 'evidence': proof,
                                      'performance_enabled': False, 'real_systemd_enabled': False})
        report_path.write_text(json.dumps(report, indent=2)+'\n')
    verify(out, report, require_obligations=False)


def verify(out, report, require_obligations=True):
    try:
        _verify(out, report, require_obligations)
    except shard.Failure as error:
        raise tier.Failure(str(error)) from error


def run(out, deadline, report, report_path):
    try:
        _run(out, deadline, report, report_path)
    except shard.Failure as error:
        raise tier.Failure(str(error)) from error
