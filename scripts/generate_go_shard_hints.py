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
from statistics import median

import contextlib
import run_go_shard as runner
import run_ci_tier


DEFAULT_MS = 1000


def json_stream(text):
    """Same helper object-stream contract without repeatedly copying the tail."""
    decoder, items, offset = json.JSONDecoder(), [], 0
    while True:
        offset = re.compile(r'\s*').match(text, offset).end()
        if offset == len(text):
            return items
        item, offset = decoder.raw_decode(text, offset)
        if not isinstance(item, dict):
            raise runner.Failure('JSON record is not an object')
        items.append(item)


@contextlib.contextmanager
def contract_parser(source):
    """Reconcile imported artifacts against their requested source, not CI's SHA."""
    original = runner.json_stream
    event_source = os.environ.get('GITHUB_SHA')
    runner.json_stream = json_stream
    os.environ['GITHUB_SHA'] = source
    try:
        yield
    finally:
        runner.json_stream = original
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
    try:
        value = event['Time']
        if not isinstance(value, str):
            raise ValueError('timestamp is not text')
        parsed = datetime.fromisoformat(value.replace('Z', '+00:00'))
        if parsed.tzinfo is None:
            raise ValueError('timestamp lacks timezone')
        return parsed.timestamp()
    except (KeyError, ValueError, TypeError) as error:
        raise runner.Failure('timed CLI evidence lacks valid timestamp') from error


def cli_durations(records, names):
    """Parent active envelope includes parallel descendants, never their sum."""
    result, groups = {}, {}
    for record in records:
        name = record.get('Test')
        if name:
            groups.setdefault(name.split('/', 1)[0], []).append(record)
    for name in names:
        subtree = groups.get(name, [])
        own = [r for r in subtree if r.get('Test') == name]
        terminal = [r for r in own if r['Action'] in ('pass', 'skip')]
        if len(terminal) != 1:
            raise runner.Failure('missing/duplicate CLI duration terminal')
        # The final cont excludes the initial pause; descendant cohort queueing
        # remains within the envelope. Do not sum sibling elapsed values.
        starts = [r for r in own if r['Action'] == 'cont'] or [r for r in own if r['Action'] == 'run']
        if not starts:
            raise runner.Failure('CLI duration lacks active start')
        start = stamp(starts[-1])
        ends = [stamp(r) for r in subtree if r['Action'] in ('pass', 'skip')
                and (r.get('Test') == name or r.get('Test', '').startswith(name + '/'))]
        envelope = max(ends) - start
        if envelope < 0:
            raise runner.Failure('backwards CLI clock')
        body = terminal[0].get('Elapsed')
        positive_ms(body)
        result[name] = max(float(body), envelope)
    return result


def _read_sample(directory, source, expected_settings):
    directory = Path(directory)
    paths = sorted(directory.glob('**/manifest.json'))
    if len(paths) != 2:
        raise runner.Failure('sample requires exactly two manifests')
    manifests = [json.loads(p.read_text()) for p in paths]
    if {m.get('shard') for m in manifests} != {0, 1}:
        raise runner.Failure('duplicate/missing shards')
    catalog = manifests[0]['catalog']
    if catalog.get('source') != source:
        raise runner.Failure('unexpected source')
    assignments = catalog['assignments']
    if len(assignments) != 2:
        raise runner.Failure('invalid assignment count')
    for kind in ('packages', 'cli'):
        a, b = [s[kind] for s in assignments]
        universe = set(catalog[kind]) - ({runner.CLI} if kind == 'packages' else set())
        if (len(a) != len(set(a)) or len(b) != len(set(b)) or not a or not b
                or set(a) & set(b) or set(a) | set(b) != universe):
            raise runner.Failure('incomplete/overlapping partition')
    package_values, cli_values, fingerprints, phases = {}, {}, [], {}
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
        cli_values.update(cli_durations(runner.json_stream(cli_raw), assignments[index]['cli']))
        # Auxiliary phases are provenance only, not arbitrary package weights.
        phases[str(index)] = m['phases']
        for f in sorted(p.glob('*.json')):
            fingerprints.append({'shard': index, 'file': f.name,
                                 'sha256': hashlib.sha256(f.read_bytes()).hexdigest()})
    return catalog, package_values, cli_values, {'files': fingerprints, 'phases': phases}


def read_sample(directory, source, expected_settings):
    with contract_parser(source):
        try:
            runner.reconcile(directory)
            return _read_sample(directory, source, expected_settings)
        except run_ci_tier.Failure as error:
            raise runner.Failure(str(error)) from error


def generate(samples, source, settings, discovered_packages, discovered_cli):
    if not samples:
        raise runner.Failure('no successful samples')
    if (len({sid for sid, _ in samples}) != len(samples)
            or len({str(Path(p).resolve()) for _, p in samples}) != len(samples)):
        raise runner.Failure('duplicate samples cannot count twice')
    package_samples, cli_samples, evidence = {}, {}, []
    universe = None
    for sample_id, directory in samples:
        catalog, packages, cli, proof = read_sample(directory, source, settings)
        current = (catalog['packages'], catalog['cli'])
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
    return {'packages': weights(discovered_packages, package_samples),
            'cli': weights(discovered_cli, cli_samples),
            'provenance': {'source': source, 'settings': settings, 'samples': evidence,
                           'runner_contract_sha256': hashlib.sha256(
                               Path(runner.__file__).read_bytes()).hexdigest(),
                           'policy': 'median seconds then half-up integer ms, minimum 1; '
                                     'package terminal elapsed; CLI max(parent elapsed, active subtree envelope); '
                                     'unknown discovered units 1000ms; auxiliary phases excluded',
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
    if len(manifests) != 2:
        raise runner.Failure('sample requires exactly two manifests')
    catalog = json.loads(manifests[0].read_text())['catalog']
    result = generate(samples, request['source'], request['settings'],
                      request.get('packages', sorted(set(catalog['packages']) - {runner.CLI})),
                      request.get('cli', catalog['cli']))
    result['provenance']['attribution'] = request.get('attribution', {})
    result['provenance']['generator_sha256'] = hashlib.sha256(Path(__file__).read_bytes()).hexdigest()
    Path(args.output).write_text(json.dumps(result, indent=2, sort_keys=True) + '\n')


if __name__ == '__main__':
    main()
