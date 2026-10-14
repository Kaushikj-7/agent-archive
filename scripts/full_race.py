"""Source-bound exhaustive full race scheduling, separate from PR short shards."""
import json
import os
import re

import run_go_shard as shards
import scope_native_verify as source_guard

CLI = shards.CLI
COMMON = ['go', 'test', '-race', '-count=1', '-json', '-timeout', '20m']
LIST_FLAGS = COMMON + ['-list', '^(Test|Fuzz|Example)']


def source_proof(root):
    """Check tracked bytes/types/modes even when index flags hide working edits."""
    source = source_guard.git(root, 'rev-parse', 'HEAD').decode().strip()
    tree = source_guard.git(root, 'rev-parse', 'HEAD^{tree}').decode().strip()
    for record in source_guard.records(source_guard.git(root, 'ls-tree', '-r', '-z', source)):
        metadata, path = record.split(b'\t', 1)
        mode, kind, oid = metadata.split(b' ')
        path = source_guard.repository_path(path)
        if (mode not in (b'100644', b'100755') or kind != b'blob'
                or not source_guard.working_input_matches(root, path, mode, oid)):
            raise shards.Failure('full discovery source bytes/types/modes changed')
    for path in source_guard.records(source_guard.git(root, 'ls-files', '--others', '-z')):
        name = path.decode()
        if name.endswith(('.go', '.py', '.sh', '.mod', '.sum')) or name.rsplit('/', 1)[-1] in ('go.work', 'go.work.sum'):
            raise shards.Failure('untracked full discovery source input')
    return {'source': source, 'tree': tree}


def packages(text):
    result = {}
    for item in shards.json_stream(text):
        name = item.get('ImportPath')
        if (not isinstance(name, str) or not re.fullmatch(r'[A-Za-z0-9._/-]+', name)
                or name.startswith('-') or name in result or item.get('Error') or item.get('DepsErrors')):
            raise shards.Failure('invalid full package discovery')
        for key in ('TestGoFiles', 'XTestGoFiles'):
            if not isinstance(item.get(key, []), list):
                raise shards.Failure('invalid full test-file metadata')
        result[name] = bool(item.get('TestGoFiles') or item.get('XTestGoFiles'))
    if not result:
        raise shards.Failure('empty full package discovery')
    return dict(sorted(result.items()))


def listing(text, package_map):
    names = {p: [] for p in package_map}
    ended, no_tests = {}, set()
    for event in shards.json_stream(text):
        pkg, action = event.get('Package'), event.get('Action')
        if pkg not in names or event.get('Test') or action not in ('start', 'output', 'pass', 'skip'):
            raise shards.Failure('full listing executed bodies or failed')
        if action in ('pass', 'skip'):
            if pkg in ended:
                raise shards.Failure('duplicate full listing package')
            ended[pkg] = action
        if action == 'output':
            line = event.get('Output', '').strip()
            if '[no test files]' in line:
                no_tests.add(pkg)
            if re.fullmatch(r'(Test|Fuzz|Example)\w*', line):
                if line == 'TestMain' or line in names[pkg]:
                    raise shards.Failure('duplicate or invalid full runnable name')
                names[pkg].append(line)
    if set(ended) != set(package_map):
        raise shards.Failure('full listing lacks terminal packages')
    if any(a == 'skip' and (package_map[p] or p not in no_tests) for p, a in ended.items()):
        raise shards.Failure('full listing skipped a test-bearing package')
    return {p: sorted(n) for p, n in names.items()}


def schedule(package_map, names, patterns, hints):
    """Only names exclusive to CLI may be globally excluded from the first run."""
    second = []
    cli = names.get(CLI, [])
    collisions = set().union(*(set(n) for p, n in names.items() if p != CLI))
    exclusive = sorted(set(cli) - collisions)
    if len(exclusive) >= 2:
        # Existing hints only rank the compiled universe. New tests, fuzz seeds
        # and executable examples are assigned with the ordinary positive default.
        second = shards.partition(exclusive, {n: hints[n] for n in exclusive if n in hints})[1]
    assignments = [{p: [n for n in ns if p != CLI or n not in second] for p, ns in names.items()}]
    commands = [COMMON + (['-skip', shards.selector(second)] if second else []) + patterns]
    if second:
        assignments.append({CLI: second})
        commands.append(COMMON + ['-run', shards.selector(second), CLI])
    return {'schema': 1, 'packages': package_map, 'names': names,
            'assignments': assignments, 'commands': commands}


def coverage(text, package_map, selected, body_evidence):
    """All compiled parents and started children require exactly one terminal."""
    shards.coverage(text, package_map)
    actual = body_evidence(text, False)
    tops = {(r['package'], r['test']) for r in actual['actual'] if '/' not in r['test']}
    expected = {(p, n) for p, names in selected.items() for n in names}
    if tops != expected:
        raise shards.Failure('full compiled parent coverage missing or unassigned')
    for event in shards.json_stream(text):
        if event.get('Test') and event['Test'].split('/')[0] not in selected.get(event.get('Package'), []):
            raise shards.Failure('full child belongs to unassigned parent')
    return actual


def verify(directory, report, patterns, root, body_evidence, verify_selected, restorations):
    """Recompute discovery, canonical argv, exhaustive assignment and raw coverage."""
    value = report.get('full_schedule')
    if not isinstance(value, dict):
        raise shards.Failure('source-bound full schedule missing')
    if value.get('source_proof') != source_proof(root) or value['source_proof']['source'] != report['source']:
        raise shards.Failure('full schedule source mismatch')
    package_map = packages((directory / 'full-packages.json').read_text())
    names = listing((directory / 'full-list.jsonl').read_text(), package_map)
    hints = json.loads((root / 'scripts/go_shard_hints.json').read_text())['cli']
    expected = schedule(package_map, names, patterns, hints)
    expected['source_proof'] = value['source_proof']
    expected['discovery'] = [{'command': ['go', 'list', '-json', *patterns], 'log': 'full-packages.json'},
                             {'command': LIST_FLAGS + patterns, 'log': 'full-list.jsonl'}]
    if value != expected:
        raise shards.Failure('full discovery/assignment/command mismatch')
    if report.get('full_schedule_sha256') != shards.digest(value):
        raise shards.Failure('full schedule digest mismatch')
    if report.get('full_discovery') != [dict(d, completed=True) for d in expected['discovery']]:
        raise shards.Failure('full discovery incomplete or weakened')
    invocations = [i for i in report['invocations'] if i['log'].startswith('race')]
    if len(invocations) != len(expected['commands']):
        raise shards.Failure('missing or duplicate exhaustive race invocation')
    all_actual = []
    for index, (invocation, command, assignment) in enumerate(zip(invocations, expected['commands'], expected['assignments'])):
        if (invocation['command'] != command or invocation['log'] != f'race-{index}.jsonl'
                or invocation.get('performance_enabled') is not False
                or invocation.get('real_systemd_enabled') is not False):
            raise shards.Failure('full race invocation weakened or reordered')
        subset = {p: package_map[p] for p in assignment}
        evidence = coverage((directory / invocation['log']).read_text(), subset, assignment, body_evidence)
        if invocation['evidence'] != evidence:
            raise shards.Failure('raw full race evidence mismatch')
        all_actual.extend(evidence['actual'])
    if len({(r['package'], r['test']) for r in all_actual}) != len(all_actual):
        raise shards.Failure('duplicate full body across invocations')
    required, children = {}, {}
    for entry in restorations:
        pkg = 'github.com/wangjohn/agent-archive/' + entry['package']
        if pkg in package_map:
            if entry['test'] not in names[pkg]:
                raise shards.Failure('required restoration absent from compiled discovery')
            required.setdefault(pkg, []).append(entry['test'])
            children[(pkg, entry['test'])] = entry.get('full_children', [])
    verify_selected(all_actual, required, children)


def run(patterns, out, deadline, report, persist, checked, execute, root, body_evidence, verify_selected, restorations):
    proof = source_proof(root)
    if proof['source'] != report['source']:
        raise shards.Failure('full discovery starts from another source')
    discovery = [{'command': ['go', 'list', '-json', *patterns], 'log': 'full-packages.json'},
                 {'command': LIST_FLAGS + patterns, 'log': 'full-list.jsonl'}]
    report['full_discovery'] = [dict(d, completed=False) for d in discovery]
    persist()
    env = dict(os.environ, AGENT_ARCHIVE_REAL_SYSTEMD='0')
    env.pop('AGENT_ARCHIVE_PERF', None)
    checked(['go', 'list', '-json', *patterns], out / 'full-packages.json', deadline, env)
    package_map = packages((out / 'full-packages.json').read_text())
    report['full_discovery'][0]['completed'] = True
    persist()
    checked(LIST_FLAGS + patterns, out / 'full-list.jsonl', deadline, env)
    names = listing((out / 'full-list.jsonl').read_text(), package_map)
    report['full_discovery'][1]['completed'] = True
    hints = json.loads((root / 'scripts/go_shard_hints.json').read_text())['cli']
    report['full_schedule'] = dict(schedule(package_map, names, patterns, hints), source_proof=proof)
    report['full_schedule']['discovery'] = discovery
    report['full_schedule_sha256'] = shards.digest(report['full_schedule'])
    if source_proof(root) != proof:
        raise shards.Failure('full discovery source changed')
    persist()
    for index, command in enumerate(report['full_schedule']['commands']):
        execute(command, f'race-{index}')
    verify(out, report, patterns, root, body_evidence, verify_selected, restorations)
