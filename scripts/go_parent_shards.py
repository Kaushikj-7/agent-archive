"""Versioned complete-parent catalog helpers for the existing PR shard runner.

Compiled listings define coverage. Timing hints only choose an assignment.
"""
import json
from pathlib import Path
import re

SCHEMA = 4
LEGACY_SCHEMAS = (3,)
ALLOCATION = 'independent-lanes-lpt-v1'
MODULE = 'github.com/wangjohn/agent-archive/'
HEAVY = tuple(MODULE + 'internal/' + name for name in ('catalog', 'statshtml', 'state', 'collector'))
CONFIG_FIELDS = ('GOVERSION GOOS GOARCH GOHOSTOS GOHOSTARCH CGO_ENABLED GOFLAGS '
                 'GOTOOLCHAIN GOWORK GOEXPERIMENT GOAMD64').split()


def listing(text, expected):
    import run_go_shard as r
    result = {}
    records = r.json_stream(text)
    if any(e.get('Package') not in expected for e in records):
        raise r.Failure('unexpected parent listing package')
    for package in expected:
        selected = [e for e in records if e.get('Package') == package]
        names, terminal = [], []
        for e in selected:
            if e.get('Test') or e.get('Action') in ('fail', 'build-fail'):
                raise r.Failure('listing executed or failed a parent')
            if e.get('Action') in ('pass', 'skip'):
                terminal.append(e['Action'])
            if e.get('Action') == 'output':
                line = e.get('Output', '').strip()
                if line.startswith(('Test', 'Fuzz', 'Example')):
                    if (not re.fullmatch(r'(Test|Fuzz|Example)\w*', line)
                            or line == 'TestMain' or line in names):
                        raise r.Failure('invalid/duplicate compiled parent')
                    names.append(line)
        # A package with test files but no runnable names remains a whole-package
        # obligation, including its TestMain. It cannot disappear from coverage.
        no_files = any('[no test files]' in e.get('Output','') for e in selected if e.get('Action') == 'output')
        if terminal != ['pass'] and not (terminal == ['skip'] and no_files and not names):
            raise r.Failure('missing/failed parent listing terminal')
        result[package] = sorted(names)
    return result


def split_listing(text, heavy):
    """Partition one compiled-list command without dropping any Go event.

    The primary receipt retains the actual invocation. Derivative logs keep
    the established CLI/heavy parsing contracts, not fictitious invocations.
    """
    import run_go_shard as r
    groups = {r.CLI: [], **{package: [] for package in heavy}}
    for event in r.json_stream(text):
        if event.get('Package') not in groups:
            raise r.Failure('unexpected combined listing package')
        groups[event['Package']].append(event)
    for events in groups.values():
        actions = [event.get('Action') for event in events]
        if (not actions or actions[0] != 'start' or actions.count('start') != 1
                or actions[-1] not in ('pass', 'skip')
                or any(action not in ('start', 'output', 'pass', 'skip') for action in actions)):
            raise r.Failure('combined listing lacks one complete package lifetime')
    def encode(events):
        return ''.join(json.dumps(event, sort_keys=True) + '\n' for event in events)
    cli = encode(groups.pop(r.CLI))
    parents = encode([event for events in groups.values() for event in events])
    r.listed_names(cli)
    listing(parents, heavy)
    return cli, parents


def hint_diagnostics(catalog, hints):
    """Describe advisory gaps against compiled units, never filter coverage."""
    import run_go_shard as r
    split = {package for package, names in catalog['parents'].items() if names}
    eligible_packages = set(catalog['packages']) - {r.CLI} - split
    eligible_cli = set(catalog['cli'])
    eligible_parents = {unit(package, name) for package, names in catalog['parents'].items() for name in names}
    groups = [('packages', eligible_packages), ('cli', eligible_cli), ('parents', eligible_parents)]
    return {'schema': 1, 'default_ms': 1000,
            'unknown_units': sorted([unit(r.CLI, name) if key == 'cli' else name
                for key, eligible in groups for name in eligible - set(hints.get(key, {}))]),
            'stale_hints': {key: sorted(set(hints.get(key, {})) - (set(catalog['packages']) if key == 'packages' else eligible))
                            for key, eligible in groups},
            'inactive_package_hints': sorted(set(hints.get('packages', {})) & (split | {r.CLI})),
            'usage': 'advisory only; all compiled units remain assigned; refresh with verified comparable successful samples'}


def unit(package, parent=None):
    return package if parent is None else package + '::' + parent


def build(source, source_proof, config, packages, cli, parents, hints, workers, initial_loads=None):
    import run_go_shard as r
    split = {p: ns for p, ns in parents.items() if ns}
    whole = sorted(set(packages) - {r.CLI} - set(split))
    units = whole + [unit(p, n) for p, ns in {r.CLI: cli, **split}.items() for n in ns]
    weights = dict(hints['packages'])
    weights.update({unit(r.CLI, n): v for n, v in hints['cli'].items()})
    weights.update(hints.get('parents', {}))
    # CLI parents overlap a sequential ordinary lane. A single mixed sum can
    # hide an overloaded ordinary lane behind many overlapping CLI wall weights.
    # Rank each lane independently; pair high ordinary advice with low CLI
    # advice. These weights remain non-additive advice, never coverage authority.
    cli_units = [unit(r.CLI, name) for name in cli]
    ordinary_units = sorted(set(units) - set(cli_units))
    ordinary = r.partition(ordinary_units, {n: weights[n] for n in ordinary_units if n in weights},
                           workers, initial_loads, allow_empty=True)
    cli_bins = r.partition(cli_units, {n: weights[n] for n in cli_units if n in weights},
                           workers, allow_empty=True)
    def load(names):
        return sum(max(1, weights.get(name, 1000)) for name in names)
    fixed = [0] * workers if initial_loads is None else list(initial_loads)
    ordinary_order = sorted(range(workers), key=lambda i: (bool(ordinary[i]), fixed[i] + load(ordinary[i]), i))
    cli_order = sorted(range(workers), key=lambda i: (-load(cli_bins[i]), i))
    bins = [list(names) for names in ordinary]
    for ordinary_index, cli_index in zip(ordinary_order, cli_order):
        bins[ordinary_index].extend(cli_bins[cli_index])
    # Extreme advisory fixed costs or sparse lanes can leave a worker empty.
    # Hints cannot make executable units disappear or prevent a valid catalog.
    for index in [i for i, names in enumerate(bins) if not names]:
        donors = [i for i, names in enumerate(bins) if len(names) > 1]
        if not donors:
            raise r.Failure('not enough execution units for nonempty workers')
        donor = max(donors, key=lambda i: (fixed[i] + load(bins[i]), -i))
        moved = min(bins[donor], key=lambda name: (max(1, weights.get(name, 1000)), name))
        bins[donor].remove(moved)
        bins[index].append(moved)
    bins = [sorted(names) for names in bins]
    assignments = []
    for names in bins:
        assignment = {'packages': [], 'cli': [], 'parents': {}}
        for name in names:
            if '::' not in name:
                assignment['packages'].append(name)
            else:
                package, parent = name.split('::')
                if package == r.CLI:
                    assignment['cli'].append(parent)
                else:
                    assignment['parents'].setdefault(package, []).append(parent)
        assignments.append(assignment)
    return {'schema': SCHEMA, 'allocation': ALLOCATION, 'source': source, 'source_proof': source_proof,
            'configuration': config, 'workers': workers, 'packages': packages,
            'cli': cli, 'parents': parents, 'units': sorted(units), 'assignments': assignments}


def validate(catalog, workers, *, allow_legacy=False):
    import run_go_shard as r
    if (type(catalog.get('schema')) is not int or catalog['schema'] not in ((SCHEMA, *LEGACY_SCHEMAS) if allow_legacy else (SCHEMA,))
            or catalog.get('workers') != workers or catalog.get('allocation') != ALLOCATION
            or set(catalog) != {'schema','source','source_proof','configuration','workers',
                                'packages','cli','parents','units','assignments','allocation'}):
        raise r.Failure('unsupported parent catalog schema or worker count')
    config = catalog['configuration']
    if (not isinstance(config, dict) or set(config) != set(CONFIG_FIELDS)
            or any(type(v) is not str for v in config.values())
            or config['GOVERSION'] != 'go1.27.2' or config['GOOS'] != 'linux'
            or config['GOHOSTOS'] != 'linux' or config['GOARCH'] != config['GOHOSTARCH']
            or config['GOARCH'] not in ('amd64','arm64') or config['CGO_ENABLED'] != '1'
            or config['GOFLAGS'] != '-mod=readonly' or config['GOTOOLCHAIN'] != 'local'
            or config['GOWORK'] != 'off'):
        raise r.Failure('unsupported parent discovery configuration')
    if (not isinstance(catalog['source_proof'], dict)
            or set(catalog['source_proof']) != {'source','tree'}
            or catalog['source_proof']['source'] != catalog['source']
            or any(type(v) is not str or not re.fullmatch('[0-9a-f]{40}', v)
                   for v in catalog['source_proof'].values())):
        raise r.Failure('invalid catalog source proof')
    packages = catalog['packages']
    if not isinstance(packages, dict) or any(type(v) is not bool for v in packages.values()) or r.CLI not in packages:
        raise r.Failure('invalid package catalog')
    expected = set(HEAVY) & set(packages)
    if set(catalog['parents']) != expected:
        raise r.Failure('heavy parent discovery missing')
    for names in [catalog['cli'], *catalog['parents'].values()]:
        if (not isinstance(names, list) or names != sorted(set(names))
                or any(type(n) is not str or not re.fullmatch(r'(Test|Fuzz|Example)\w*', n)
                       or n == 'TestMain' for n in names)):
            raise r.Failure('invalid discovered parent names')
    whole = set(packages) - {r.CLI} - {p for p, ns in catalog['parents'].items() if ns}
    units = whole | {unit(p,n) for p,ns in {r.CLI:catalog['cli'], **catalog['parents']}.items() for n in ns}
    if catalog['units'] != sorted(units) or len(catalog['assignments']) != workers:
        raise r.Failure('catalog execution unit mismatch')
    assigned = []
    for a in catalog['assignments']:
        if not isinstance(a, dict) or set(a) != {'packages','cli','parents'}:
            raise r.Failure('invalid parent assignment schema')
        if not isinstance(a['parents'], dict) or any(p not in expected or not ns for p,ns in a['parents'].items()):
            raise r.Failure('invalid heavy parent assignment')
        names = a['packages'] + [unit(r.CLI,n) for n in a['cli']] + [unit(p,n) for p,ns in a['parents'].items() for n in ns]
        if not names:
            raise r.Failure('empty worker assignment')
        assigned.extend(names)
    if len(assigned) != len(set(assigned)) or set(assigned) != units:
        raise r.Failure('parent partition is not disjoint/exhaustive')


def log_name(package):
    import run_go_shard as r
    return 'parents-' + r.digest(package)[:16] + '.json'


def verify_lifecycle_containment(events):
    """Every observed descendant stays inside its top-level runtime body.

    Go JSON does not identify the immediate caller of t.Run. A slash-containing
    sibling name can resemble a nested parent, so do not infer virtual ancestry.
    Paused parallel bodies remain active until their actual terminal.
    """
    import run_go_shard as r
    active = set()
    for e in events:
        name, action, package = e.get('Test'), e.get('Action'), e.get('Package')
        if not name or action == 'output':
            continue
        key = (package, name)
        if '/' in name and (package, name.split('/')[0]) not in active:
            raise r.Failure('descendant lifecycle outside enclosing runtime parent')
        if action == 'run':
            active.add(key)
        elif action in ('pass', 'skip'):
            if key not in active or ('/' not in name and any(p == package and n.startswith(name + '/') for p, n in active)):
                raise r.Failure('parent terminal precedes descendant lifecycle completion')
            active.remove(key)


def verify_logs(directory, catalog, assignment, *, allow_legacy=False):
    import run_go_shard as r
    import run_ci_tier
    directory = Path(directory)
    validate(catalog, catalog['workers'], allow_legacy=allow_legacy)
    if catalog['schema'] == SCHEMA:
        try:
            cli, parents = split_listing((directory/'listing-combined.json').read_text(), catalog['parents'])
        except OSError as error:
            raise r.Failure('missing combined compiled listing receipt') from error
        if (r.json_stream(cli) != r.json_stream((directory/'listing.json').read_text())
                or r.json_stream(parents) != r.json_stream((directory/'listing-parents.json').read_text())):
            raise r.Failure('compiled listing derivatives differ from actual invocation')
    discovered = r.metadata((directory/'packages.json').read_text())
    cli = r.listed_names((directory/'listing.json').read_text())
    parents = listing((directory/'listing-parents.json').read_text(), catalog['parents'])
    if (discovered != catalog['packages'] or cli != catalog['cli'] or parents != catalog['parents']
            or json.loads((directory/'discovery-config.json').read_text()) != catalog['configuration']):
        raise r.Failure('actual compiled discovery/configuration differs from catalog')
    expected_logs = set()
    whole = assignment['packages']
    if r.GIT_IDENTITY in whole:
        expected_logs.add('git-identity.json')
    if set(whole) - {r.GIT_IDENTITY}:
        expected_logs.add('ordinary.json')
    if assignment['cli']:
        expected_logs.add('cli.json')
    expected_logs.update(log_name(p) for p in assignment['parents'])
    actual_logs = {p.name for p in directory.glob('parents-*.json')}
    actual_logs.update(n for n in ('ordinary.json','cli.json','git-identity.json') if (directory/n).exists())
    if actual_logs != expected_logs:
        raise r.Failure('missing/unexpected body log')
    raw_whole = '\n'.join((directory/n).read_text() for n in ('git-identity.json','ordinary.json') if n in expected_logs)
    r.coverage(raw_whole, {p:catalog['packages'][p] for p in whole})
    records = r.json_stream(raw_whole)
    verify_lifecycle_containment(records)
    for package in whole:
        if not catalog['packages'][package] and not any(e.get('Package') == package and not e.get('Test') and e.get('Action') == 'skip' for e in records):
            raise r.Failure('no-test package did not establish compilation terminal')
    for package, names in {r.CLI:assignment['cli'], **assignment['parents']}.items():
        if not names:
            continue
        raw = (directory/('cli.json' if package == r.CLI else log_name(package))).read_text()
        r.coverage(raw, {package: True}, names)
        # Coverage must contain actual run-to-terminal bodies, including every
        # dynamically generated descendant, rather than terminals alone.
        events = r.json_stream(raw)
        runs = {(e.get('Package'),e.get('Test')) for e in events if e.get('Action') == 'run' and e.get('Test')}
        terminals = {(e.get('Package'),e.get('Test')) for e in events if e.get('Action') in ('pass','skip') and e.get('Test')}
        if runs != terminals or not runs:
            raise r.Failure('missing/unexecuted parent or descendant terminal')
        lifecycles = {}
        for e in events:
            if e.get('Test') and e.get('Action') != 'output':
                lifecycles.setdefault(e['Test'], []).append(e['Action'])
        if any(actions not in (['run', 'pass'], ['run', 'skip'],
                               ['run', 'pause', 'cont', 'pass'],
                               ['run', 'pause', 'cont', 'skip'])
               for actions in lifecycles.values()):
            raise r.Failure('invalid parent or descendant lifecycle')
        verify_lifecycle_containment(events)
        if any(e.get('Test') and e['Test'].split('/')[0] not in names for e in events):
            raise r.Failure('unassigned descendant')
        run_ci_tier.terminals(raw)
    return '\n'.join((directory/n).read_text() for n in sorted(expected_logs))
