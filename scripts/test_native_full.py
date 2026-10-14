"""Native full workers fail closed over compiled discovery and raw body receipts."""
import copy
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import run_native_full as native

CLI = native.full.CLI
OTHER = native.tier.MODULE+'internal/archive'
EMPTY = native.tier.MODULE+'cmd/empty'
PROOF = {'source': 'a'*40, 'tree': 'b'*40}
ENV = {'GOOS': 'darwin', 'GOARCH': 'arm64', 'CGO_ENABLED': '1', 'GOMAXPROCS': '2',
       'GOFLAGS': '', 'AGENT_ARCHIVE_REAL_SYSTEMD': '0'}
CATALOG = {'schema': 1, 'restorations': [
    {'package': 'internal/archive', 'test': 'TestSame', 'restore': 'weekly'},
    {'package': 'internal/cli', 'test': 'TestFull', 'restore': 'weekly', 'full_children': ['large']}]}


def stream(items):
    return ''.join(json.dumps(e)+'\n' for e in items)


def bodies(selected, packages):
    events = []
    for pkg, names in selected.items():
        events.append(dict(Package=pkg, Action='start'))
        for name in names:
            events.append(dict(Package=pkg, Test=name, Action='run'))
            if name == 'TestFull':
                events.extend([dict(Package=pkg, Test=name+'/large', Action=a) for a in ('run','pass')])
            events.append(dict(Package=pkg, Test=name, Action='pass'))
        if not packages[pkg]:
            events.append(dict(Package=pkg, Action='output', Output='? '+pkg+' [no test files]\n'))
        events.append(dict(Package=pkg, Action='pass' if packages[pkg] else 'skip'))
    return stream(events)


class NativeFullTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.out = Path(self.temp.name)
        for target, value in [('proof', PROOF), ('native_environment', ENV), ('context', {'event':'qualification'})]:
            mock = patch.object(native, target, return_value=value)
            mock.start()
            self.addCleanup(mock.stop)
        mock = patch.object(native.tier, 'catalog', return_value=CATALOG)
        mock.start()
        self.addCleanup(mock.stop)
        self.packages = {OTHER: True, EMPTY: False, CLI: True}
        self.names = {OTHER: ['TestSame'], EMPTY: [], CLI: ['TestSame','TestFull','FuzzSeed','ExampleOutput']}
        plan_out = self.out/'native-plan'
        plan_out.mkdir()
        (plan_out/'packages.json').write_text(stream([
            dict(ImportPath=p, TestGoFiles=['x_test.go'] if t else []) for p,t in self.packages.items()]))
        events = []
        for p, names in self.names.items():
            events.append(dict(Package=p, Action='start'))
            events.extend(dict(Package=p, Action='output', Output=n+'\n') for n in names)
            if not self.packages[p]:
                events.append(dict(Package=p, Action='output', Output='? '+p+' [no test files]\n'))
            events.append(dict(Package=p, Action='pass' if self.packages[p] else 'skip'))
        (plan_out/'listing.jsonl').write_text(stream(events))
        self.plan = native.plan_value(plan_out, PROOF, ENV)
        native.save(plan_out, 'plan.json', self.plan)
        native.save(plan_out, 'receipt.json', dict(schema=1, role='plan', source_proof=PROOF, context=native.context(), environment=ENV,
                    success=True, plan_sha256=native.full.shards.digest(self.plan),
                    invocations=[dict(d, completed=True) for d in self.plan['discovery']]))
        self.needs = {n: {'result':'success'} for n in ('macos-plan','macos-workers','macos-obligations')}

    def receipt(self, role):
        return dict(schema=1, success=True, role=role, source_proof=PROOF, context=native.context(),
                    environment=ENV, plan_sha256=native.full.shards.digest(self.plan), invocations=[])

    def write_workers(self):
        for index, units in enumerate(self.plan['schedule']['commands']):
            out = self.out/f'native-worker-{index}'
            out.mkdir()
            report = self.receipt('worker')
            report['worker'] = index
            for n, unit in enumerate(units):
                text = bodies(unit['selected'], self.packages)
                log = f'race-{n}.jsonl'
                (out/log).write_text(text)
                subset = {p:self.packages[p] for p in unit['selected']}
                report['invocations'].append(dict(command=unit['command'], log=log, completed=True,
                    performance_enabled=False, real_systemd_enabled=False,
                    evidence=native.full.coverage(text, subset, unit['selected'], native.tier.body_evidence)))
            native.save(out, 'receipt.json', report)

    def write_obligations(self):
        # The compiled full catalog must contain every plain performance target.
        out = self.out/'native-obligations'
        out.mkdir()
        report = self.receipt('obligations')
        report['performance_compile'] = dict(source_proof=PROOF, packages=native.tier.PERFORMANCE[-4:], parallel=2, completed=True)
        (out/'performance-compile.log').write_text('compiled\n')
        perf = {OTHER: ['TestSame']}
        report['performance_scope'] = {'compiled_selection': perf, 'compile_only': sorted(native.PERFORMANCE_COMPILE_ONLY)}
        published = {native.tier.MODULE+'internal/config': ['TestPublishedWriterRefusesProtectedConfiguration']}
        for log, command, selected, enabled in (
                ('performance.jsonl', native.tier.PERFORMANCE, perf, True),
                ('published-writer.jsonl', ['bash','scripts/test_published_writer.sh'], published, False)):
            text = bodies(selected, {p:True for p in selected})
            (out/log).write_text(text)
            report['invocations'].append(dict(command=command, log=log, completed=True,
                performance_enabled=enabled, real_systemd_enabled=False, evidence=native.tier.body_evidence(text, False)))
        native.save(out, 'receipt.json', report)
        mock = patch.object(native, 'performance_names', return_value=perf)
        mock.start()
        self.addCleanup(mock.stop)

    def complete(self):
        self.write_workers()
        self.write_obligations()

    def edit_receipt(self, name, edit):
        path = self.out/name/'receipt.json'
        value = json.loads(path.read_text())
        edit(value)
        path.write_text(json.dumps(value))

    def test_schedule_exhaustive_disjoint_compiled_trees_and_name_collisions(self):
        assignments = self.plan['schedule']['assignments']
        actual = [(p,n) for group in assignments for p,names in group.items() for n in names]
        expected = [(p,n) for p,names in self.names.items() for n in names]
        self.assertCountEqual(actual, expected)
        self.assertEqual(len(actual), len(set(actual)))
        self.assertEqual(sum(EMPTY in a for a in assignments), 1)
        self.assertEqual(sum(OTHER in a for a in assignments), 1)
        self.assertTrue(all(group for group in assignments))
        for units in self.plan['schedule']['commands']:
            for unit in units:
                self.assertIn('-p', unit['command'])
                self.assertNotIn('-short', unit['command'])
                if OTHER in unit['selected']:
                    self.assertNotIn('-skip', unit['command'])

    def test_generic_partition_boundary_is_three_but_pr_topology_stays_two_or_four(self):
        self.assertEqual(len(native.full.shards.partition(['a','b','c'], {'a':1,'b':1,'c':1}, 3)), 3)
        with self.assertRaises(native.Failure):
            native.full.shards.worker_count(3)
        for count in (0,17,True):
            with self.assertRaises(native.Failure):
                native.full.shards.partition(['a','b','c'], {}, count)

    def test_complete_evidence_passes_and_retains_full_restoration_children(self):
        self.complete()
        verdict = native.reconcile(self.out, self.needs)
        self.assertTrue(verdict['success'])
        self.assertIn('TestFull/large', [r['test'] for r in verdict['actual']])

    def test_needs_missing_failed_cancelled_skipped_workers_fail(self):
        self.complete()
        for state in ('failure','cancelled','skipped'):
            bad = copy.deepcopy(self.needs)
            bad['macos-workers']['result'] = state
            with self.assertRaises(native.Failure):
                native.reconcile(self.out, bad)
        bad = copy.deepcopy(self.needs)
        del bad['macos-obligations']
        with self.assertRaises(native.Failure):
            native.reconcile(self.out, bad)

    def test_failed_duplicate_mixed_source_environment_receipts_fail(self):
        self.complete()
        original = (self.out/'native-worker-1'/'receipt.json').read_text()
        for edit in (lambda v:v.update(success=False), lambda v:v.update(worker=0),
                     lambda v:v.update(source_proof={'source':'c'*40,'tree':'b'*40}),
                     lambda v:v.update(context={'event':'different'}),
                     lambda v:v.update(environment=dict(ENV, CGO_ENABLED='0')),
                     lambda v:v['invocations'][0].update(performance_enabled=True),
                     lambda v:v['invocations'].clear()):
            self.edit_receipt('native-worker-1', edit)
            with self.assertRaises(native.Failure):
                native.reconcile(self.out, self.needs)
            (self.out/'native-worker-1'/'receipt.json').write_text(original)

    def test_missing_artifact_and_raw_log_fail(self):
        self.complete()
        path = self.out/'native-worker-1'/'receipt.json'
        old = path.read_text()
        path.unlink()
        with self.assertRaises(FileNotFoundError):
            native.reconcile(self.out, self.needs)
        path.write_text(old)
        (self.out/'native-worker-1'/'race-0.jsonl').unlink()
        with self.assertRaises(FileNotFoundError):
            native.reconcile(self.out, self.needs)

    def test_plan_tamper_catalog_source_context_or_compiled_name_fails(self):
        directory = self.out/'native-plan'
        original = (directory/'plan.json').read_text()
        for edit in (lambda v:v['schedule']['names'][CLI].pop(),
                     lambda v:v['source_proof'].update(source='c'*40),
                     lambda v:v.update(context={'event':'other'}),
                     lambda v:v.update(catalog_sha256='0'*64),
                     lambda v:v['schedule']['commands'][0][0]['command'].append('-short')):
            value = json.loads(original)
            edit(value)
            native.save(directory, 'plan.json', value)
            with self.assertRaises(native.Failure):
                native.load_plan(directory)
        (directory/'plan.json').write_text(original)
        with patch.object(native, 'proof', return_value={'source':'d'*40,'tree':'e'*40}):
            with self.assertRaises(native.Failure):
                native.load_plan(directory)

    def test_no_test_package_and_empty_selected_body_boundaries(self):
        for events in (
                [dict(Package=EMPTY,Action='skip')],
                [dict(Package=OTHER,Action='pass')],
                [dict(Package=OTHER,Action='output',Output='testing: warning: no tests to run\n'),dict(Package=OTHER,Action='pass')]):
            with self.assertRaises(native.Failure):
                native.full.coverage(stream(events), {OTHER:True} if events[-1]['Package']==OTHER else {EMPTY:False},
                    {OTHER:['TestSame']} if events[-1]['Package']==OTHER else {EMPTY:[]}, native.tier.body_evidence)
        self.assertEqual(native.full.coverage(bodies({EMPTY:[]},{EMPTY:False}), {EMPTY:False}, {EMPTY:[]}, native.tier.body_evidence)['actual'], [])

    def test_started_child_missing_terminal_duplicate_or_unassigned_body_fails(self):
        selected = {OTHER:['TestSame']}
        good = json.loads(json.dumps([json.loads(line) for line in bodies(selected,{OTHER:True}).splitlines()]))
        mutations = [good[:2]+[dict(Package=OTHER,Test='TestSame/child',Action='run')]+good[2:],
                     good[:2]+[dict(Package=OTHER,Test='TestSame',Action='pass')]+good[2:],
                     good[:2]+[dict(Package=OTHER,Test='TestOther/child',Action='run'),dict(Package=OTHER,Test='TestOther/child',Action='pass')]+good[2:]]
        for events in mutations:
            with self.assertRaises((native.Failure, native.tier.Failure)):
                native.full.coverage(stream(events), {OTHER:True}, selected, native.tier.body_evidence)

    def test_platform_skip_retained_but_restoration_child_skip_fails(self):
        selected = {CLI:['TestFull']}
        text = bodies(selected,{CLI:True}).replace('"Action": "pass", "Test": "TestFull/large"', '"Action": "skip", "Test": "TestFull/large"')
        # Mutation independent of JSON insertion order.
        events = [json.loads(line) for line in text.splitlines()]
        for e in events:
            if e.get('Test') == 'TestFull/large' and e['Action']=='pass':
                e['Action']='skip'
        evidence = native.full.coverage(stream(events),{CLI:True},selected,native.tier.body_evidence)
        self.assertEqual(next(r for r in evidence['actual'] if r['test']=='TestFull/large')['status'], 'skip')
        with self.assertRaises(native.tier.Failure):
            native.tier.verify_selected(evidence['actual'], selected, {(CLI,'TestFull'):['large']})

    def test_invocation_cancellation_persists_incomplete_receipt_and_clears_perf(self):
        report = self.receipt('worker')
        with patch.object(native.tier, 'checked', side_effect=native.tier.Failure('cancelled')):
            with self.assertRaises(native.tier.Failure):
                native.invocation(self.out,report,['go','test'],'race-0',100,dict(ENV),{OTHER:['TestSame']},{OTHER:True})
        saved = json.loads((self.out/'receipt.json').read_text())
        self.assertFalse(saved['invocations'][0]['completed'])
        self.assertFalse(saved['invocations'][0]['performance_enabled'])
        self.assertIn('elapsed_seconds',saved['invocations'][0])

    def test_body_terminal_without_start_and_duplicate_start_fail(self):
        selected = {OTHER:['TestSame']}
        events = [json.loads(line) for line in bodies(selected,{OTHER:True}).splitlines()]
        for bad in ([e for e in events if e['Action']!='run'], events[:2]+[events[1]]+events[2:]):
            with self.assertRaises(native.Failure):
                native.body_coverage(stream(bad), {OTHER:True}, selected)

    def test_unexpected_artifact_rejected(self):
        self.complete()
        (self.out/'native-worker-3').mkdir()
        with self.assertRaises(native.Failure):
            native.reconcile(self.out,self.needs)

    def test_changed_scale_and_unknown_weights_positive_unmeasured(self):
        profile = {'schema':1,'packages':{OTHER:90000},'cli':{'TestFull':80000,'TestSame':20000},
                   'restorations':[]}
        with patch.object(native.json,'loads',return_value=profile):
            costs, unknown, _ = native.weights(self.names)
        self.assertEqual(costs[OTHER],1000)
        self.assertEqual(costs[CLI+'::TestFull'],1000)
        self.assertEqual(costs[CLI+'::TestSame'],20000)
        self.assertIn(OTHER,unknown)
        self.assertIn(CLI+'::TestFull',unknown)
        self.assertIn(CLI+'::FuzzSeed',unknown)
        self.assertTrue(all(c>0 for c in costs.values()))

    def test_real_compiled_native_catalog_has_five_performance_bodies_and_compile_only_archive(self):
        # Regression P3-R1 uses the actual successful hosted compiled catalog,
        # whose archive package has 157 parents and zero regex matches.
        fixture = json.loads((Path(__file__).parent/'testdata/native_performance_catalog.json').read_text())
        selected = native.performance_names(fixture)
        archive = native.tier.MODULE+'internal/archive'
        self.assertEqual(len(fixture['schedule']['names'][archive]), 157)
        self.assertEqual(selected[archive], [])
        self.assertEqual(set(selected), {native.tier.MODULE+p[2:] for p in native.tier.PERFORMANCE[-4:]})
        self.assertEqual(sum(map(len, selected.values())), 5)
        for pkg, required in native.PERFORMANCE_REQUIRED.items():
            self.assertEqual(set(selected[pkg]), required)
        for pkg, required in native.PERFORMANCE_REQUIRED.items():
            for name in required:
                bad = copy.deepcopy(fixture)
                bad['schedule']['names'][pkg].remove(name)
                with self.assertRaises(native.Failure):
                    native.performance_names(bad)
        for pkg in native.PERFORMANCE_REQUIRED:
            bad = copy.deepcopy(fixture)
            bad['schedule']['names'][pkg].append('TestUnexpectedStayFast')
            with self.assertRaises(native.Failure):
                native.performance_names(bad)
        for pkg in selected:
            bad = copy.deepcopy(fixture)
            del bad['schedule']['names'][pkg]
            with self.assertRaises(native.Failure):
                native.performance_names(bad)
        bad = copy.deepcopy(fixture)
        bad['schedule']['names'][archive].append('TestUnexpectedStayFast')
        with self.assertRaises(native.Failure):
            native.performance_names(bad)

    def test_performance_raw_evidence_retains_compile_only_package_pass(self):
        fixture = json.loads((Path(__file__).parent/'testdata/native_performance_catalog.json').read_text())
        selected = native.performance_names(fixture)
        packages = {p:True for p in selected}
        text = bodies(selected, packages)
        out = self.out/'plain'
        out.mkdir()
        report = self.receipt('obligations')
        with patch.object(native.tier,'checked',return_value=text):
            native.invocation(out, report, native.tier.PERFORMANCE, 'performance', 100,
                              dict(ENV,AGENT_ARCHIVE_PERF='1'),selected)
        (out/'performance.jsonl').write_text(text)
        native.verify_invocation(out,report['invocations'][0],native.tier.PERFORMANCE,
                                 selected,performance=True)
        archive = native.tier.MODULE+'internal/archive'
        events = [json.loads(line) for line in text.splitlines()]
        missing_package = stream([e for e in events if e['Package']!=archive])
        with self.assertRaises(native.Failure):
            native.body_coverage(missing_package, packages, selected)
        skipped_package = stream([dict(e,Action='skip') if e['Package']==archive and e['Action']=='pass' else e for e in events])
        with self.assertRaises(native.Failure):
            native.body_coverage(skipped_package,packages,selected)

    def test_plain_performance_missing_package_and_body_fails(self):
        with self.assertRaises(native.Failure):
            native.performance_names(self.plan)
        self.complete()
        self.edit_receipt('native-obligations', lambda v:v.pop('performance_compile'))
        with self.assertRaises(native.Failure):
            native.reconcile(self.out,self.needs)


if __name__ == '__main__':
    unittest.main()
