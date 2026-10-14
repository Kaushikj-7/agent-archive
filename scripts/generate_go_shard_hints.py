#!/usr/bin/env python3
"""Regenerate advisory shard weights from successful matched Go JSON artifacts."""
import argparse
from datetime import datetime
from decimal import Decimal, ROUND_HALF_UP
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
from statistics import median

import contextlib
import run_go_shard as runner
import run_ci_tier


DEFAULT_MS = 1000


# Historical import and live execution share the same object-stream parser.
json_stream = runner.json_stream


@contextlib.contextmanager
def contract_parser(source, body_catalog=None):
    """Reconcile imported artifacts against their requested source, not CI's SHA."""
    original = runner.json_stream
    original_catalog = run_ci_tier.catalog
    event_source = os.environ.get('GITHUB_SHA')
    runner.json_stream = json_stream
    if body_catalog is not None:
        run_ci_tier.catalog = lambda: body_catalog
    os.environ['GITHUB_SHA'] = source
    try:
        yield
    finally:
        runner.json_stream = original
        run_ci_tier.catalog = original_catalog
        if event_source is None:
            os.environ.pop('GITHUB_SHA', None)
        else:
            os.environ['GITHUB_SHA'] = event_source


def positive_ms(seconds):
    if isinstance(seconds, bool) or seconds is None:
        raise runner.Failure('invalid duration type')
    try:
        value = Decimal(str(seconds))
    except Exception as error:
        raise runner.Failure('invalid duration') from error
    if not value.is_finite() or value < 0:
        raise runner.Failure('invalid duration')
    return max(1, int((value * 1000).quantize(Decimal('1'), rounding=ROUND_HALF_UP)))


def stamp(event):
    """Parse Go RFC3339 nanoseconds exactly, including on Python 3.9."""
    value = event.get('Time')
    match = re.fullmatch(r'(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2})(?:\.(\d{1,9}))?(Z|[+-]\d{2}:\d{2})', value) if isinstance(value, str) else None
    try:
        if not match:
            raise ValueError('invalid RFC3339 timestamp')
        # fromisoformat normalizes offsets such as +00:60 instead of rejecting
        # them. RFC3339 requires an hour from 00 through 23 and minute 00..59.
        if match[3] != 'Z' and (int(match[3][1:3]) > 23 or int(match[3][4:6]) > 59):
            raise ValueError('invalid RFC3339 timezone offset')
        parsed = datetime.fromisoformat(match[1] + match[3].replace('Z', '+00:00'))
        return Decimal(str(parsed.timestamp())) + Decimal('0.' + (match[2] or '0'))
    except (ValueError, TypeError) as error:
        raise runner.Failure('timed CLI evidence lacks valid timestamp') from error


def cli_durations(records, names):
    """Account logical test body cost without parent waits or subtree queueing.

    Go's parent Elapsed includes synchronous child execution, but excludes its
    final wait for parallel children. Subtract each direct child's synchronous
    run-to-pause (parallel) or run-to-terminal (sequential) interval before adding
    that child's recursively accounted cost. Thus parallel preludes and ordinary
    descendants count once, and cohort queueing does not become a body weight.
    These logical costs are neither CPU time nor worker wall-time predictions.
    """
    trees = {}
    for record in records:
        name, action = record.get('Test'), record.get('Action')
        if not name or name.split('/', 1)[0] not in names or action == 'output':
            continue
        if action not in ('run', 'pause', 'cont', 'pass', 'skip'):
            raise runner.Failure('invalid CLI timing action')
        trees.setdefault(name, []).append(record)
    nodes, children_by_parent = {}, {}
    for name, events in trees.items():
        actions = [r['Action'] for r in events]
        if actions not in (['run', 'pass'], ['run', 'skip'],
                           ['run', 'pause', 'cont', 'pass'], ['run', 'pause', 'cont', 'skip']):
            raise runner.Failure('invalid CLI timing lifecycle')
        times = [stamp(r) for r in events]
        if any(b < a for a, b in zip(times, times[1:])):
            raise runner.Failure('backwards CLI clock')
        elapsed = events[-1].get('Elapsed')
        positive_ms(elapsed)
        nodes[name] = (Decimal(str(elapsed)), times[1] - times[0], times)
    # A single t.Run name can contain slashes. A matching path component is
    # an executed parent only if its lifecycle contains the child and it was
    # active when the child started; otherwise it is a selector-only prefix.
    for name, (_, _, times) in nodes.items():
        parent = name.rsplit('/', 1)[0] if '/' in name else None
        while parent is not None:
            candidate = nodes.get(parent)
            if candidate is not None:
                parent_times = candidate[2]
                active = (len(parent_times) == 2 or times[0] < parent_times[1]
                          or times[0] >= parent_times[2])
                if active and parent_times[0] <= times[0] and parent_times[-1] >= times[-1]:
                    children_by_parent.setdefault(parent, []).append(name)
                    break
            if '/' not in parent:
                raise runner.Failure('CLI timing descendant lacks active enclosing parent')
            parent = parent.rsplit('/', 1)[0]
    def cost(name):
        if name not in nodes:
            raise runner.Failure('CLI duration lacks active start or terminal')
        elapsed, _, times = nodes[name]
        children = children_by_parent.get(name, [])
        for child in children:
            child_times = nodes[child][2]
            if child_times[0] < times[0] or child_times[-1] > times[-1]:
                raise runner.Failure('CLI timing child outside parent lifecycle')
        synchronous = sum((nodes[n][1] for n in children), Decimal(0))
        return max(Decimal(0), elapsed - synchronous) + sum((cost(n) for n in children), Decimal(0))
    return {name: cost(name) for name in names}


def complete_parent_durations(records, names):
    """Use the shared lifecycle measure; timing errors cannot become weights."""
    from report_ci_timing import parent_tree_timings
    selected = [e for e in records if e.get('Test', '').split('/',1)[0] in names]
    # Preserve exact RFC3339Nano validation before consuming diagnostic rows.
    for e in selected:
        if e.get('Action') != 'output':
            stamp(e)
    rows = parent_tree_timings(selected)
    if (len(rows) != len(names) or {row['parent'] for row in rows} != set(names)
            or any(row['timing_issues'] or row['seconds'] is None
                   or row['status'] not in ('pass','skip') for row in rows)):
        raise runner.Failure('invalid complete-parent timing lifecycle')
    return {row['parent']:Decimal(str(row['seconds'])) for row in rows}


def _read_parent_sample(directory, source, expected_settings, package_cost_exclusions):
    import go_parent_shards as parents
    if package_cost_exclusions:
        raise runner.Failure('parent catalog cannot import legacy package exclusions')
    workers = runner.worker_count(len(expected_settings))
    if set(expected_settings) != {str(i) for i in range(workers)}:
        raise runner.Failure('invalid parent settings worker count')
    paths = sorted(Path(directory).glob('**/manifest.json'))
    catalog = json.loads(paths[0].read_text())['catalog']
    packages, cli, parent_values, phases, fingerprints = {}, {}, {}, {}, []
    for path in paths:
        m = json.loads(path.read_text()); index = str(m['shard'])
        if m['settings'] != expected_settings[index] or catalog['source'] != source:
            raise runner.Failure('parent sample settings/source mismatch')
        assignment = catalog['assignments'][m['shard']]
        raw = parents.verify_logs(path.parent, catalog, assignment)
        records = runner.json_stream(raw)
        for e in records:
            if not e.get('Test') and e.get('Action') in ('pass','skip') and e.get('Package') in assignment['packages']:
                packages[e['Package']] = e['Elapsed']
        for package, names in {runner.CLI:assignment['cli'], **assignment['parents']}.items():
            timed = complete_parent_durations([e for e in records if e.get('Package') == package], names)
            if package == runner.CLI:
                cli.update(timed)
            else:
                parent_values.update({parents.unit(package,n):v for n,v in timed.items()})
        phases[index] = m['phases']
        for file in sorted(path.parent.glob('*.json')):
            fingerprints.append({'shard':m['shard'],'file':file.name,'sha256':hashlib.sha256(file.read_bytes()).hexdigest()})
    return catalog, packages, cli, {'files':fingerprints,'phases':phases,'package_cost_exclusions':[],
                                   'parent_values':{n:str(v) for n,v in parent_values.items()}}


def _read_sample(directory, source, expected_settings, package_cost_exclusions=None):
    directory = Path(directory)
    paths = sorted(directory.glob('**/manifest.json'))
    if paths and 'schema' in json.loads(paths[0].read_text())['catalog']:
        return _read_parent_sample(directory, source, expected_settings, package_cost_exclusions)
    workers = runner.worker_count(len(expected_settings))
    if set(expected_settings) != {str(i) for i in range(workers)}:
        raise runner.Failure('invalid settings worker count')
    paths = sorted(directory.glob('**/manifest.json'))
    if len(paths) != workers:
        raise runner.Failure('sample requires complete worker manifests')
    manifests = [json.loads(p.read_text()) for p in paths]
    if {m.get('shard') for m in manifests} != set(range(workers)):
        raise runner.Failure('duplicate/missing shards')
    catalog = manifests[0]['catalog']
    if catalog.get('source') != source:
        raise runner.Failure('unexpected source')
    assignments = catalog['assignments']
    if len(assignments) != workers or catalog.get('workers', 2) != workers:
        raise runner.Failure('invalid assignment count')
    for kind in ('packages', 'cli'):
        bins = [s[kind] for s in assignments]
        flattened = [n for b in bins for n in b]
        universe = set(catalog[kind]) - ({runner.CLI} if kind == 'packages' else set())
        if (len(flattened) != len(set(flattened)) or not all(bins)
                or set(flattened) != universe):
            raise runner.Failure('incomplete/overlapping partition')
    package_values, cli_values, fingerprints, phases, exclusions, parent_values = {}, {}, [], {}, [], {}
    for path, m in zip(paths, manifests):
        index = m['shard']
        if (m.get('success') is not True or m['catalog'] != catalog
                or m.get('catalog_sha256') != runner.digest(catalog)
                or m.get('settings') != expected_settings[str(index)]
                or m.get('coverage') != assignments[index]):
            raise runner.Failure('failed/mixed/incomplete shard evidence')
        p = path.parent
        metadata = runner.metadata((p / 'packages.json').read_text())
        names = runner.listed_names((p / 'listing.json').read_text())
        if metadata != catalog['packages'] or names != catalog['cli']:
            raise runner.Failure('actual discovery differs from catalog')
        isolated = m['settings'].get('isolated_packages', [])
        if any(n != runner.GIT_IDENTITY or n not in assignments[index]['packages'] for n in isolated):
            raise runner.Failure('unexpected isolated package')
        raw = '\n'.join((p / f).read_text() for f in
                        (['git-identity.json'] if isolated else []) +
                        (['ordinary.json'] if set(assignments[index]['packages']) - set(isolated) else []))
        expected_packages = {n: metadata[n] for n in assignments[index]['packages']}
        ordinary = runner.coverage(raw, expected_packages)
        cli_raw = (p / 'cli.json').read_text()
        cli = runner.coverage(cli_raw, {runner.CLI: True}, assignments[index]['cli'])
        if ordinary['packages'] != assignments[index]['packages'] or cli['cli'] != assignments[index]['cli']:
            raise runner.Failure('actual coverage mismatch')
        for r in runner.json_stream(raw):
            if not r.get('Test') and r['Action'] in ('pass', 'skip'):
                positive_ms(r.get('Elapsed'))
                package_values[r['Package']] = r['Elapsed']
        for package, test in (package_cost_exclusions or {}).items():
            if package not in assignments[index]['packages']:
                continue
            body = [r for r in runner.json_stream(raw) if r.get('Package') == package
                    and r.get('Test') == test and r.get('Action') != 'output']
            if ([r.get('Action') for r in body] != ['run', 'pass']
                    or any(r.get('Package') == package and r.get('Test', '').startswith(test + '/')
                           for r in runner.json_stream(raw))):
                raise runner.Failure('excluded package cost needs one sequential passing body')
            if 'Time' in body[0] and stamp(body[-1]) < stamp(body[0]):
                raise runner.Failure('excluded package body clock reversed')
            elapsed = body[-1].get('Elapsed')
            positive_ms(elapsed)
            remaining = Decimal(str(package_values[package])) - Decimal(str(elapsed))
            if remaining < 0:
                raise runner.Failure('excluded body cost exceeds package elapsed')
            exclusions.append({'package': package, 'test': test, 'observed_body_seconds': elapsed,
                               'observed_package_seconds': package_values[package],
                               'derived_remaining_seconds': str(remaining)})
            package_values[package] = remaining
        import go_parent_shards as parent_shards
        for package in set(assignments[index]['packages']) & set(parent_shards.HEAVY):
            events = [e for e in runner.json_stream(raw) if e.get('Package') == package]
            top_names = sorted({e['Test'] for e in events if e.get('Test') and '/' not in e['Test']})
            parent_values.update({parent_shards.unit(package,n):str(v) for n,v in complete_parent_durations(events,top_names).items()})
        cli_values.update(cli_durations(runner.json_stream(cli_raw), assignments[index]['cli']))
        # Auxiliary phases are provenance only, not arbitrary package weights.
        phases[str(index)] = m['phases']
        for f in sorted(p.glob('*.json')):
            fingerprints.append({'shard': index, 'file': f.name,
                                 'sha256': hashlib.sha256(f.read_bytes()).hexdigest()})
    if set(package_cost_exclusions or {}) != {e['package'] for e in exclusions}:
        raise runner.Failure('excluded package body absent from successful sample')
    return catalog, package_values, cli_values, {'files': fingerprints, 'phases': phases, 'package_cost_exclusions': exclusions, 'parent_values': parent_values}


def measured_source(source, body_catalog_json=None):
    """Resolve historical proof and regular catalog bytes from immutable Git objects."""
    if not isinstance(source, str) or not re.fullmatch('[0-9a-f]{40}', source):
        raise runner.Failure('measured source must be a full commit SHA')
    def git(*args):
        return subprocess.check_output(['git', '-C', str(runner.ROOT), *args],
                                       stderr=subprocess.PIPE, timeout=20)
    try:
        commit = git('rev-parse', '--verify', source + '^{commit}').decode().strip()
        if commit != source:
            raise runner.Failure('measured source must name the commit itself')
        tree = git('rev-parse', source + '^{tree}').decode().strip()
        entry = git('ls-tree', source, '--', 'scripts/ci_tiers.json').decode().strip()
        mode, kind, blob_path = entry.split(None, 2)
        blob, path = blob_path.split('\t')
        if mode != '100644' or kind != 'blob' or path != 'scripts/ci_tiers.json':
            raise runner.Failure('measured source body catalog must be a regular tracked file')
        raw = git('cat-file', 'blob', blob).decode()
        if body_catalog_json is not None and raw != body_catalog_json:
            raise runner.Failure('pinned body catalog differs from measured Git bytes')
        return {'source': commit, 'tree': tree}, raw
    except (subprocess.SubprocessError, OSError, ValueError) as error:
        raise runner.Failure('fetch the exact measured source commit and its regular body catalog before importing') from error


def read_sample(directory, source, expected_settings, body_catalog=None, package_cost_exclusions=None, *, audit_directory=None):
    expected = None
    if audit_directory is not None:
        expected, catalog_raw = measured_source(source)
        if body_catalog != json.loads(catalog_raw):
            raise runner.Failure('historical audit requires the measured Git body catalog')
    with contract_parser(source, body_catalog):
        try:
            runner.reconcile(directory, len(expected_settings), audit_directory, expected_source_proof=expected, historical_legacy=True)
            catalog, packages, cli, proof = _read_sample(directory, source, expected_settings, package_cost_exclusions)
            if audit_directory is not None:
                if not all(v.get('plain_audit_owned') is True for v in expected_settings.values()):
                    raise runner.Failure('imported plain audit requires owned short sample settings')
                audit_directory = Path(audit_directory)
                rows = json_stream((audit_directory / 'source-audit.jsonl').read_text())
                terminal = [r for r in rows if r.get('Test') == run_ci_tier.SOURCE_AUDIT_TEST and r.get('Action') == 'pass'][0]
                positive_ms(terminal.get('Elapsed'))
                proof['plain_audit'] = {'expected_source_proof': expected,
                    'observed_plain_body_seconds': terminal['Elapsed'],
                    'files': [{'file': name, 'sha256': hashlib.sha256((audit_directory / name).read_bytes()).hexdigest()}
                              for name in (['tier.json', 'source-audit.jsonl'] +
                                  (['plain-vet.txt'] if any(v.get('plain_vet_owned') for v in expected_settings.values()) else []))]}
            return catalog, packages, cli, proof
        except run_ci_tier.Failure as error:
            raise runner.Failure(str(error)) from error


def generate(samples, source, settings, discovered_packages, discovered_cli, body_catalog=None, package_cost_exclusions=None, *, audit_directories=None, discovered_parents=None):
    if not samples:
        raise runner.Failure('no successful samples')
    if (len({sid for sid, _ in samples}) != len(samples)
            or len({str(Path(p).resolve()) for _, p in samples}) != len(samples)):
        raise runner.Failure('duplicate samples cannot count twice')
    if set(audit_directories or {}) - {sid for sid, _ in samples}:
        raise runner.Failure('audit directory names an unknown sample')
    package_samples, cli_samples, parent_samples, evidence = {}, {}, {}, []
    universe = None
    for sample_id, directory in samples:
        catalog, packages, cli, proof = read_sample(directory, source, settings, body_catalog, package_cost_exclusions, audit_directory=(audit_directories or {}).get(sample_id))
        current = (catalog['packages'], catalog['cli'], catalog.get('parents'))
        for name,value in proof.get('parent_values', {}).items():
            parent_samples.setdefault(name, []).append(Decimal(value))
        if universe is not None and current != universe:
            raise runner.Failure('mixed discovery catalogs')
        universe = current
        evidence.append({'sample_id': sample_id, **proof})
        for values, destination in ((packages, package_samples), (cli, cli_samples)):
            for name, value in values.items():
                destination.setdefault(name, []).append(Decimal(str(value)))
    def weights(discovered, values):
        if len(discovered) != len(set(discovered)):
            raise runner.Failure('duplicate discovered names')
        return {n: positive_ms(median(values[n])) if n in values else DEFAULT_MS
                for n in sorted(discovered)}
    excluded_samples = {}
    for sample in evidence:
        for excluded in sample['package_cost_exclusions']:
            excluded_samples.setdefault(excluded['package'], []).append(Decimal(str(excluded['observed_body_seconds'])))
    for sample in evidence:
        if 'plain_audit' in sample:
            if package_cost_exclusions:
                raise runner.Failure('owned audit samples must not subtract the skipped race audit again')
            excluded_samples.setdefault(run_ci_tier.SOURCE_AUDIT_PACKAGE, []).append(
                Decimal(str(sample['plain_audit']['observed_plain_body_seconds'])))
    return {'parents': weights(discovered_parents if discovered_parents is not None else list(parent_samples),parent_samples),
            'owned_plain_audit_exclusions_ms': {p: positive_ms(median(values)) for p, values in sorted(excluded_samples.items())},
            'packages': weights(discovered_packages, package_samples),
            'cli': weights(discovered_cli, cli_samples),
            'provenance': {'source': source, 'settings': settings, 'samples': evidence,
                           'runner_contract_sha256': hashlib.sha256(
                               Path(runner.__file__).read_bytes()).hexdigest(),
                           'body_catalog_sha256': runner.digest(body_catalog) if body_catalog is not None else None,
                           'policy': 'median seconds then half-up integer ms, minimum 1; '
                                     'legacy: package terminal elapsed and CLI recursive logical cost; schema2: complete parent active lifecycle wall cost including descendant waits; '
                                     'unknown discovered units 1000ms; auxiliary phases excluded; '
                                     'owned audit plain body cost only restores advisory unowned local ranking',
                           'usage': 'advisory ranking only; dynamic discovery and current CI are authoritative'}}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--request', required=True, help='JSON source/settings/discovery/samples request')
    parser.add_argument('--output', required=True)
    parser.add_argument('--artifacts-root', type=Path, default=Path('.'),
                        help='Root containing each request sample directory')
    args = parser.parse_args()
    request = json.loads(Path(args.request).read_text())
    samples = [(s['id'], args.artifacts_root / s['directory']) for s in request['samples']]
    if not samples:
        raise runner.Failure('no successful samples')
    manifests = sorted(samples[0][1].glob('**/manifest.json'))
    if len(manifests) != runner.worker_count(len(request['settings'])):
        raise runner.Failure('sample requires complete worker manifests')
    catalog = json.loads(manifests[0].read_text())['catalog']
    body_catalog_json = request.get('body_catalog_json')
    body_catalog = None
    if body_catalog_json is not None:
        if (not isinstance(body_catalog_json, str) or hashlib.sha256(body_catalog_json.encode()).hexdigest()
                != request.get('body_catalog_sha256')):
            raise runner.Failure('invalid pinned source body catalog bytes')
        measured_source(request['source'], body_catalog_json)
        body_catalog = json.loads(body_catalog_json)
        if (not isinstance(body_catalog, dict) or body_catalog.get('schema') != 1
                or not isinstance(body_catalog.get('restorations'), list)):
            raise runner.Failure('invalid pinned source body catalog')
    result = generate(samples, request['source'], request['settings'],
                      request.get('packages', sorted(set(catalog['packages']) - {runner.CLI})),
                      request.get('cli', catalog['cli']), body_catalog, request.get('package_cost_exclusions'),
                      audit_directories={s['id']: args.artifacts_root / s['audit_directory']
                                         for s in request['samples'] if 'audit_directory' in s}, discovered_parents=request.get('parents'))
    if request.get('auxiliary_costs_ms') is not None:
        auxiliary = request['auxiliary_costs_ms']
        if set(auxiliary) != {'vet', 'git_invocation'} or any(type(v) is not int or v < 0 for v in auxiliary.values()):
            raise runner.Failure('invalid auxiliary cost hints')
        phases = [p for sample in result['provenance']['samples'] for p in sample['phases'].values()]
        expected_auxiliary = {name: positive_ms(median([p[field] for p in phases if field in p]))
                              for name, field in [('vet', 'vet_seconds'), ('git_invocation', 'git_identity_invocation_seconds')]}
        if auxiliary != expected_auxiliary:
            raise runner.Failure('auxiliary hints differ from observed invocation medians')
        result['auxiliary_costs_ms'] = auxiliary
    result['provenance']['attribution'] = request.get('attribution', {})
    result['provenance']['generator_sha256'] = hashlib.sha256(Path(__file__).read_bytes()).hexdigest()
    Path(args.output).write_text(json.dumps(result, indent=2, sort_keys=True) + '\n')


if __name__ == '__main__':
    main()
