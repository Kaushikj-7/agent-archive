"""Complete-method purge scheduling and source-bound evidence reconciliation."""
import argparse
import hashlib
import json
import math
from pathlib import Path
import sys
import unittest

ROOT = Path(__file__).resolve().parent.parent
WORKERS = 2


def digest(value):
    return hashlib.sha256(json.dumps(value, sort_keys=True, separators=(',', ':'),
                                     allow_nan=False).encode()).hexdigest()


def methods():
    import test_purge_recipe as purge
    return sorted(unittest.defaultTestLoader.getTestCaseNames(purge.PurgeRecipeTest))


def assignments(names, workers=WORKERS):
    if workers != WORKERS or not names or names != sorted(set(names)):
        raise ValueError('purge scheduling requires two workers and unique sorted methods')
    return [names[worker::workers] for worker in range(workers)]


def source_proof():
    # Reuse the existing byte/type/mode guard; a commit name alone is insufficient.
    import full_race
    return full_race.source_proof(ROOT)


def resume_inventory(tier):
    import test_purge_recipe as purge
    selected, omitted = purge.resume_cases(tier)
    def keys(cases):
        return {digest({'shell': shell, 'prefix': prefix, 'mode': mode, 'boundary': boundary})
                for shell in ('bash', 'zsh') for prefix, mode, _prepare, boundary in cases}
    return keys(selected), keys(omitted)


def verify(receipts, tier, proof):
    names = methods()
    bins = assignments(names)
    if len(receipts) != WORKERS or tier not in ('pr', 'daily', 'full'):
        raise ValueError('incomplete purge evidence or unsupported tier')
    seen_workers, all_cases, all_methods = set(), [], set()
    for receipt in receipts:
        if not isinstance(receipt, dict):
            raise ValueError('malformed purge worker')
        worker = receipt.get('worker')
        if (type(worker) is not int or worker not in range(WORKERS) or worker in seen_workers
                or receipt.get('schema') != 2 or type(receipt.get('workers')) is not int or receipt['workers'] != WORKERS
                or receipt.get('source') != proof['source']
                or receipt.get('source_proof') != proof or receipt.get('post_source_proof') != proof
                or receipt.get('tier') != tier or receipt.get('success') is not True
                or receipt.get('shells') != ['bash', 'zsh'] or receipt.get('catalog') != names
                or receipt.get('catalog_sha256') != digest(names)
                or receipt.get('assigned') != bins[worker]):
            raise ValueError('wrong/duplicate purge worker, source, catalog, assignment or verdict')
        seen_workers.add(worker)
        cases, terminals = receipt.get('cases'), receipt.get('method_timings')
        if not isinstance(cases, list) or not isinstance(terminals, list):
            raise ValueError('missing purge method/case stream')
        expected_ids = {f'__main__.PurgeRecipeTest.{name}' for name in bins[worker]}
        grouped = {name: [] for name in expected_ids}
        seen_cases = set()
        for case in cases:
            if (not isinstance(case, dict) or case.get('test') not in expected_ids
                    or case.get('status') not in ('pass', 'NOT RUN')
                    or ('case' in case and not isinstance(case['case'], dict))):
                raise ValueError('unexpected purge method/case/status')
            key = (case['test'], digest(case.get('case')))
            if key in seen_cases:
                raise ValueError('duplicate purge case')
            seen_cases.add(key)
            grouped[case['test']].append(case)
        seen_terminals = set()
        for terminal in terminals:
            if not isinstance(terminal, dict) or terminal.get('test') not in expected_ids:
                raise ValueError('unexpected purge terminal')
            name = terminal['test']
            elapsed = terminal.get('seconds')
            if (name in seen_terminals or type(elapsed) not in (int, float)
                    or not math.isfinite(elapsed) or elapsed < 0):
                raise ValueError('duplicate/malformed purge terminal')
            seen_terminals.add(name)
            children = grouped[name]
            # The independently retained parent terminal binds every emitted child.
            if (type(terminal.get('case_count')) is not int or terminal['case_count'] != len(children)
                    or terminal.get('cases_sha256') != digest(children)
                    or len([c for c in children if 'case' not in c]) != 1
                    or children[-1] != {'test': name, 'status': 'pass'}):
                raise ValueError('incomplete/truncated purge case stream or missing passing parent')
            child_cases = [c for c in children if 'case' in c]
            if name.endswith('.test_resume_every_deletion_boundary_in_fresh_shell'):
                required, omitted = resume_inventory(tier)
                actual = {digest(c['case']) for c in child_cases if c['status'] == 'pass'}
                not_run = {digest(c['case']) for c in child_cases if c['status'] == 'NOT RUN'}
                if actual != required or not_run != omitted or any(
                        c['status'] == 'NOT RUN' and c.get('reason') != tier+' representative repeated boundaries'
                        for c in child_cases):
                    raise ValueError('incomplete purge resume/cadence case union')
            elif any(c['status'] != 'pass' for c in child_cases):
                raise ValueError('unexpected purge cadence omission')
        if seen_terminals != expected_ids:
            raise ValueError('missing purge parent terminal')
        all_methods.update(seen_terminals)
        all_cases.extend(cases)
    if seen_workers != set(range(WORKERS)) or len(all_methods) != len(names):
        raise ValueError('incomplete purge worker/method union')
    return all_cases


def unique_object(pairs):
    value = dict(pairs)
    if len(value) != len(pairs):
        raise ValueError('duplicate JSON field in purge evidence')
    return value


def reconcile(directory, tier):
    paths = sorted(Path(directory).rglob('*'))
    files = [path for path in paths if path.is_file()]
    if (len(files) != WORKERS or any(path.name != 'purge-tier.json' or path.is_symlink() for path in files)
            or any(path.is_symlink() for path in paths)):
        raise ValueError('expected exactly two regular purge worker evidence files')
    proof = source_proof()
    cases = verify([json.loads(path.read_text(), object_pairs_hook=unique_object) for path in files], tier, proof)
    if source_proof() != proof:
        raise ValueError('purge verifier source changed')
    print(f'Reconciled {len(methods())} complete purge methods and {len(cases)} case/parent entries ({tier})')


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--evidence', type=Path, required=True)
    parser.add_argument('--tier', choices=('pr', 'daily', 'full'), required=True)
    args = parser.parse_args()
    try:
        reconcile(args.evidence, args.tier)
    except (ValueError, KeyError, TypeError, OSError) as error:
        raise SystemExit(str(error))
