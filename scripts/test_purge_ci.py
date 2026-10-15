"""Purge workers retain whole parents and reconcile complete source-bound streams."""
import copy
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import purge_ci as ci
import test_purge_recipe as purge
from test_ci_workflow import ROOT, jobs, job_key, steps, step_key

PROOF = {'source': 'a' * 40, 'tree': 'b' * 40}


def receipts(tier='pr'):
    names = ci.methods()
    bins = ci.assignments(names)
    result = []
    for worker, assigned in enumerate(bins):
        cases, terminals = [], []
        for method in assigned:
            name = '__main__.PurgeRecipeTest.' + method
            children = []
            if method == 'test_resume_every_deletion_boundary_in_fresh_shell':
                selected, omitted = purge.resume_cases(tier)
                for shell in ('bash', 'zsh'):
                    for status, group in [('NOT RUN', omitted), ('pass', selected)]:
                        for prefix, mode, _prepare, boundary in group:
                            case = {'test': name, 'status': status,
                                    'case': dict(shell=shell, prefix=prefix, mode=mode, boundary=boundary)}
                            if status == 'NOT RUN':
                                case['reason'] = tier + ' representative repeated boundaries'
                            children.append(case)
            else:
                children = [{'test': name, 'status': 'pass', 'case': {'shell': shell, 'fixture': method}}
                            for shell in ('bash', 'zsh')]
            children.append({'test': name, 'status': 'pass'})
            terminals.append(dict(test=name, seconds=1.0, case_count=len(children),
                                  cases_sha256=ci.digest(children)))
            cases.extend(children)
        result.append(dict(schema=2, worker=worker, workers=2, source=PROOF['source'],
                           source_proof=PROOF, post_source_proof=PROOF, catalog=names,
                           catalog_sha256=ci.digest(names), assigned=assigned, shells=['bash', 'zsh'],
                           tier=tier, success=True, cases=cases, method_timings=terminals))
    return result


def rebind(receipt, name):
    """Update a receipt checksum to test independent semantic guards too."""
    children = [case for case in receipt['cases'] if case['test'] == name]
    terminal = next(item for item in receipt['method_timings'] if item['test'] == name)
    terminal.update(case_count=len(children), cases_sha256=ci.digest(children))


class PurgeUnionTest(unittest.TestCase):
    def test_discovery_assigns_every_current_method_once_including_new_names(self):
        names = ci.methods()
        split = ci.assignments(names)
        self.assertEqual(set(split[0]) | set(split[1]), set(names))
        self.assertFalse(set(split[0]) & set(split[1]))
        self.assertEqual(ci.assignments(sorted(names + ['test_new_method'])),
                         [sorted(names + ['test_new_method'])[n::2] for n in range(2)])
        for invalid in ([], names[::-1], sorted(names + [names[0]])):
            with self.assertRaises(ValueError):
                ci.assignments(invalid)

    def test_all_tiers_retain_complete_parent_and_resume_case_union(self):
        for tier in ('pr', 'daily', 'full'):
            actual = ci.verify(receipts(tier), tier, PROOF)
            self.assertEqual(len([case for case in actual if 'case' not in case]), len(ci.methods()))
            selected, omitted = purge.resume_cases(tier)
            self.assertEqual(len(selected) + len(omitted), len(purge.resume_cases('full')[0]))
            if tier == 'full':
                self.assertFalse(any(case['status'] != 'pass' for case in actual))

    def test_missing_duplicate_extra_and_failed_workers_are_rejected(self):
        good = receipts()
        for bad in (good[:1], good + good[:1], [good[0], good[0]]):
            with self.assertRaises(ValueError):
                ci.verify(bad, 'pr', PROOF)
        for key, value in [('worker', True), ('workers', 2.0), ('schema', 1), ('schema', 2.0), ('success', False),
                           ('tier', 'full'), ('source', 'x'), ('source_proof', {}),
                           ('post_source_proof', {}), ('catalog', []), ('assigned', []),
                           ('catalog_sha256', 'x'), ('shells', ['bash']), ('cases', None),
                           ('method_timings', [])]:
            bad = copy.deepcopy(good); bad[0][key] = value
            with self.subTest(key=key), self.assertRaises(ValueError):
                ci.verify(bad, 'pr', PROOF)

    def test_missing_duplicate_or_malformed_case_or_parent_is_rejected(self):
        for mutation in ('missing-child', 'duplicate-child', 'unknown-parent', 'failed-child',
                         'missing-parent', 'duplicate-parent', 'missing-terminal', 'duplicate-terminal',
                         'bad-time', 'bad-count'):
            bad = receipts(); worker = bad[0]; name = worker['cases'][0]['test']
            if mutation == 'missing-child':
                del worker['cases'][0]  # Parent digest still records that child.
            elif mutation == 'duplicate-child':
                worker['cases'].insert(0, dict(worker['cases'][0])); rebind(worker, name)
            elif mutation == 'unknown-parent':
                worker['cases'][0]['test'] = '__main__.PurgeRecipeTest.test_unknown'
            elif mutation == 'failed-child':
                worker['cases'][0]['status'] = 'fail'; rebind(worker, name)
            elif mutation == 'missing-parent':
                worker['cases'] = [c for c in worker['cases'] if c != {'test': name, 'status': 'pass'}]
                rebind(worker, name)
            elif mutation == 'duplicate-parent':
                worker['cases'].append({'test': name, 'status': 'pass'}); rebind(worker, name)
            elif mutation == 'missing-terminal':
                del worker['method_timings'][0]
            elif mutation == 'duplicate-terminal':
                worker['method_timings'].append(dict(worker['method_timings'][0]))
            elif mutation == 'bad-time':
                worker['method_timings'][0]['seconds'] = float('nan')
            elif mutation == 'bad-count':
                worker['method_timings'][0]['case_count'] = 0
            with self.subTest(mutation=mutation), self.assertRaises(ValueError):
                ci.verify(bad, 'pr', PROOF)

    def test_resume_case_omission_rejected_even_with_updated_parent_digest(self):
        for tier in ('pr', 'daily', 'full'):
            bad = receipts(tier)
            worker = next(r for r in bad if any('test_resume_every_deletion_boundary' in x for x in r['assigned']))
            case = next(c for c in worker['cases'] if 'test_resume_every_deletion_boundary' in c['test']
                        and 'case' in c and c['status'] == 'pass')
            worker['cases'].remove(case); rebind(worker, case['test'])
            with self.assertRaisesRegex(ValueError, 'resume/cadence'):
                ci.verify(bad, tier, PROOF)

    def test_unexpected_cadence_omission_rejected_even_with_updated_digest(self):
        bad = receipts(); worker = bad[0]; case = worker['cases'][0]
        case.update(status='NOT RUN', reason='pr representative repeated boundaries')
        rebind(worker, case['test'])
        with self.assertRaisesRegex(ValueError, 'unexpected purge cadence'):
            ci.verify(bad, 'pr', PROOF)

    def test_directory_rejects_missing_extra_truncated_duplicate_fields_and_symlinks(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder)
            for n, receipt in enumerate(receipts()):
                lane = root / str(n); lane.mkdir(); (lane/'purge-tier.json').write_text(json.dumps(receipt))
            with patch.object(ci, 'source_proof', return_value=PROOF):
                ci.reconcile(root, 'pr')
                (root/'extra.json').write_text('{}')
                with self.assertRaises(ValueError):
                    ci.reconcile(root, 'pr')
                (root/'extra.json').unlink()
                worker = root/'0/purge-tier.json'; original = worker.read_text()
                worker.write_text(original[:-1])
                with self.assertRaises(ValueError):
                    ci.reconcile(root, 'pr')
                worker.write_text(original[:-1] + ',"worker":0}')
                with self.assertRaisesRegex(ValueError, 'duplicate JSON'):
                    ci.reconcile(root, 'pr')
                worker.unlink(); worker.symlink_to(root/'1/purge-tier.json')
                with self.assertRaises(ValueError):
                    ci.reconcile(root, 'pr')

    def test_two_workers_and_reconciliation_are_mandatory_workflow_steps(self):
        found = jobs((ROOT/'.github/workflows/test.yml').read_text())
        lane, aggregate = found['script-purge'], found['linux-race']
        self.assertNotIn('script-tests', found)
        self.assertIn('fail-fast: false', lane)
        self.assertIn('shard: [0, 1]', lane)
        self.assertIn('--workers 2 --shard "$SHARD"', lane)
        self.assertIn('name: purge-tier-${{ matrix.shard }}', lane)
        self.assertIn('retention-days: 7', lane)
        self.assertIn('TMPDIR=%s/script-purge-tmp', lane)
        self.assertEqual(job_key(aggregate, 'if'), '${{ !cancelled() }}')
        self.assertNotIn('tier-plan', job_key(aggregate, 'needs'))
        self.assertIn('script-purge', job_key(aggregate, 'needs'))
        for job in (lane, aggregate):
            self.assertIn('fetch-depth: 0', job)
            self.assertIn('--plan --require-pr-proof', job)
            self.assertIn('steps.plan.outputs.purge', job)
        self.assertIn('pattern: purge-tier-*', aggregate)
        self.assertIn('scripts/purge_ci.py --tier "$tier"', aggregate)
        self.assertNotIn('continue-on-error:', aggregate)
        purge_step = next(step for step in steps(aggregate) if 'scripts/purge_ci.py' in step)
        download = next(step for step in steps(aggregate) if 'pattern: purge-tier-*' in step)
        self.assertIsNone(step_key(purge_step, 'if'))
        self.assertIsNone(step_key(download, 'if'))
        self.assertLess(aggregate.index('--verdict'), aggregate.index('pattern: purge-tier-*'))
        self.assertLess(aggregate.index('scripts/purge_ci.py'), aggregate.index('--reconcile'))


if __name__ == '__main__':
    unittest.main()
