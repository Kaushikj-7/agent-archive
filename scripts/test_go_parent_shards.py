"""Coverage and scheduling regressions for complete compiled parent trees."""
import copy
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest import mock

import go_parent_shards as p
import run_go_shard as r
import run_ci_tier
from test_go_shards import synthetic_vet_context

SOURCE = {'source':'a'*40,'tree':'b'*40}
CONFIG = {k:synthetic_vet_context()['go'][k] for k in p.CONFIG_FIELDS}


def rows(events):
    return '\n'.join(json.dumps(e) for e in events)


def event(package, action, **kw):
    return dict(Package=package,Action=action,Time='2026-01-01T00:00:00Z',Elapsed=0,**kw)


def listing(package, names):
    return rows([event(package,'start')]+[event(package,'output',Output=n+'\n') for n in names]+[event(package,'pass')])


def catalog(workers=8):
    packages = {r.CLI:True, **{n:True for n in p.HEAVY}, 'light':True, 'fileless':False}
    cli = sorted(['TestCLI','FuzzSeed','ExampleOutput'])
    parents = {package:['ExampleBody','FuzzCorpus','TestA','TestB','TestNewlyAdded'] for package in p.HEAVY}
    return p.build(SOURCE['source'],SOURCE,CONFIG,packages,cli,parents,{'packages':{},'cli':{}},workers)


def body(package, names):
    events = []
    for name in names:
        events.extend([event(package,'run',Test=name), event(package,'run',Test=name+'/runtime/child'),
                       event(package,'pass',Test=name+'/runtime/child'),event(package,'pass',Test=name)])
    events.append(event(package,'pass'))
    return rows(events)


def evidence(root, c):
    for i,a in enumerate(c['assignments']):
        d = root/str(i); d.mkdir()
        (d/'packages.json').write_text(rows([dict(ImportPath=name,Dir='/tmp',TestGoFiles=['test.go'] if tested else []) for name,tested in c['packages'].items()]))
        (d/'listing.json').write_text(listing(r.CLI,c['cli']))
        (d/'listing-parents.json').write_text('\n'.join(listing(n,ns) for n,ns in c['parents'].items()))
        (d/'listing-combined.json').write_text((d/'listing.json').read_text()+'\n'+(d/'listing-parents.json').read_text())
        (d/'discovery-config.json').write_text(json.dumps(CONFIG))
        for name,ns in a['parents'].items():
            (d/p.log_name(name)).write_text(body(name,ns))
        if a['cli']:
            (d/'cli.json').write_text(body(r.CLI,a['cli']))
        ordinary=[]
        for name in a['packages']:
            if c['packages'][name]:
                ordinary.append(body(name,['TestOrdinary']))
            else:
                ordinary.append(rows([event(name,'output',Output='? '+name+' [no test files]\n'),event(name,'skip')]))
        if ordinary:
            (d/'ordinary.json').write_text('\n'.join(ordinary))
        raw = p.verify_logs(d,c,a)
        m = {'catalog':c,'catalog_sha256':r.digest(c),'shard':i,'tier':'pr','success':True,
             'phases':{},'coverage':a,'settings':{'workers':c['workers'],'source_proof':SOURCE},
             'body_evidence':run_ci_tier.body_evidence(raw,True)}
        (d/'manifest.json').write_text(json.dumps(m))


class ParentScheduling(unittest.TestCase):
    def test_combined_listing_preserves_all_events_and_rejects_invalid_streams(self):
        heavy = p.HEAVY[:2]
        raw = listing(r.CLI, ['TestCLI']) + '\n' + '\n'.join(listing(n, ['TestA']) for n in heavy)
        cli, parents = p.split_listing(raw, heavy)
        self.assertEqual(r.listed_names(cli), ['TestCLI'])
        self.assertEqual(p.listing(parents, heavy), {n:['TestA'] for n in heavy})
        self.assertEqual(r.json_stream(cli) + r.json_stream(parents), r.json_stream(raw))
        for invalid in (raw + '\n' + listing('unexpected', ['TestX']),
                        raw + '\n' + listing(r.CLI, ['TestCLI']),
                        listing(r.CLI, ['TestCLI']),
                        raw + '\n' + rows([event(heavy[0], 'run', Test='TestA')]),
                        raw + '\n' + rows([event(heavy[0], 'start')])):
            with self.assertRaises(r.Failure):
                p.split_listing(invalid, heavy)

    def test_combined_primary_receipt_is_required_and_derivatives_cannot_diverge(self):
        c = catalog(4)
        with tempfile.TemporaryDirectory() as d:
            root = Path(d); evidence(root, c)
            worker = root/'0'; a = c['assignments'][0]
            primary = worker/'listing-combined.json'; original = primary.read_text()
            primary.unlink()
            with self.assertRaises(r.Failure): p.verify_logs(worker, c, a)
            primary.write_text(original)
            derivative = worker/'listing.json'; derivative.write_text(listing(r.CLI, ['TestOther']))
            with self.assertRaises(r.Failure): p.verify_logs(worker, c, a)

    def test_schema_three_requires_explicit_historical_import_authority(self):
        c = catalog(4); c['schema'] = 3
        with self.assertRaises(r.Failure): p.validate(c, 4)
        p.validate(c, 4, allow_legacy=True)
        with tempfile.TemporaryDirectory() as d, mock.patch.object(run_ci_tier.full_race, 'source_proof', return_value=SOURCE), mock.patch.dict(os.environ, {'GITHUB_SHA':SOURCE['source']}):
            root = Path(d); evidence(root, dict(c, schema=4))
            for path in root.glob('*/manifest.json'):
                m = json.loads(path.read_text()); m['catalog'] = c; m['catalog_sha256'] = r.digest(c)
                path.write_text(json.dumps(m)); (path.parent/'listing-combined.json').unlink()
            with self.assertRaises(r.Failure): r.reconcile(root, 4)
            r.reconcile(root, 4, historical_legacy=True)
            with self.assertRaises(r.Failure): p.verify_logs(root/'0', c, c['assignments'][0])
            p.verify_logs(root/'0', c, c['assignments'][0], allow_legacy=True)

    def test_unknown_and_stale_hint_diagnostics_never_remove_compiled_units(self):
        c = catalog(4)
        hints = {'packages': {'light': 7, 'removed': 99}, 'cli': {'TestCLI': 1, 'TestRemoved': 9},
                 'parents': {p.unit(p.HEAVY[0], 'TestRemoved'): 5}}
        report = p.hint_diagnostics(c, hints)
        self.assertIn(p.unit(r.CLI, 'FuzzSeed'), report['unknown_units'])
        self.assertIn(p.unit(p.HEAVY[0], 'TestNewlyAdded'), report['unknown_units'])
        self.assertEqual(report['stale_hints']['packages'], ['removed'])
        self.assertEqual(report['stale_hints']['cli'], ['TestRemoved'])
        self.assertEqual(set(report['unknown_units']), set(c['units']) - {'light', p.unit(r.CLI, 'TestCLI')})

    def test_lane_allocation_does_not_hide_ordinary_work_behind_cli_weights(self):
        packages = {r.CLI: True, **{name: True for name in ('a','b','c','d')}}
        hints = {'packages': {'a':100, 'b':90, 'c':80, 'd':70},
                 'cli': {'TestExpensive':10000}}
        c = p.build(SOURCE['source'], SOURCE, CONFIG, packages, ['TestExpensive'], {}, hints, 2)
        p.validate(c, 2)
        self.assertEqual(c['schema'], 4)
        self.assertEqual(c['allocation'], p.ALLOCATION)
        loads = [sum(hints['packages'][name] for name in a['packages']) for a in c['assignments']]
        self.assertEqual(loads, [170,170])
        self.assertEqual(sum(len(a['cli']) for a in c['assignments']), 1)
        mixed_weights = dict(hints['packages'], **{p.unit(r.CLI,'TestExpensive'):10000})
        mixed = r.partition(c['units'], mixed_weights, 2)
        self.assertEqual(max(sum(hints['packages'].get(name,0) for name in b) for b in mixed),340)
        # A huge stale/renamed hint cannot introduce a body or hide a new one.
        hints['cli']['TestRemoved'] = 99999999
        hints['packages']['not-discovered'] = 99999999
        changed = p.build(SOURCE['source'], SOURCE, CONFIG, packages, ['TestRenamed'], {}, hints, 2)
        p.validate(changed,2)
        self.assertEqual(set(changed['units']), set(packages)-{r.CLI} | {p.unit(r.CLI,'TestRenamed')})

    def test_collector_compiled_parents_split_but_reader_stays_whole(self):
        collector = p.MODULE + 'internal/collector'
        reader = p.MODULE + 'internal/reader'
        self.assertIn(collector,p.HEAVY)
        self.assertNotIn(reader,p.HEAVY)
        packages = {r.CLI:True, collector:True, reader:True}
        names = ['ExampleCollector','FuzzCollector','TestNewCollector','TestRenamedCollector']
        compiled = p.listing(listing(collector,names),[collector])
        c = p.build(SOURCE['source'],SOURCE,CONFIG,packages,['TestCLI'],compiled,
                    {'packages':{collector:9999999,reader:1000},'cli':{}},4)
        p.validate(c,4)
        self.assertIn(reader,c['units'])
        self.assertNotIn(collector,c['units'])
        self.assertEqual(set(c['units']), {reader,p.unit(r.CLI,'TestCLI')} |
                         {p.unit(collector,n) for n in names})
        with tempfile.TemporaryDirectory() as d:
            # All descendants remain in their selected complete parent trees.
            root=Path(d); evidence(root,c)
            for index,a in enumerate(c['assignments']):
                p.verify_logs(root/str(index),c,a)

    def test_sparse_lanes_preserve_nonempty_combined_workers_and_fixed_advice(self):
        c=catalog(8)
        fixed=[10000]+[0]*7
        updated=p.build(SOURCE['source'],SOURCE,CONFIG,c['packages'],c['cli'],c['parents'],
                        {'packages':{},'cli':{}},8,fixed)
        p.validate(updated,8)
        self.assertEqual(set(updated['units']),set(c['units']))
        self.assertEqual(len(updated['assignments'][0]['packages']) +
                         sum(map(len,updated['assignments'][0]['parents'].values())),0)
        self.assertTrue(updated['assignments'][0]['cli'])
        # Even absurd advisory fixed loads cannot strand workers when the
        # discovered union contains enough executable units.
        sparse=p.build(SOURCE['source'],SOURCE,CONFIG,
                       {r.CLI:True,**{name:True for name in ('a','b','c','d')}},
                       ['TestOnly'],{}, {'packages':{},'cli':{}},4,[1000000]*3+[0])
        p.validate(sparse,4)
        self.assertEqual(len(sparse['units']),5)
        self.assertTrue(all(a['packages'] or a['cli'] or a['parents'] for a in sparse['assignments']))
        with self.assertRaises(r.Failure):
            p.build(SOURCE['source'],SOURCE,CONFIG,{r.CLI:True},['TestOnly'],{},
                    {'packages':{},'cli':{}},8)
        with self.assertRaises(r.Failure):
            r.partition(['one'],{},2)
        self.assertEqual(r.partition(['one'],{},2,allow_empty=True),[['one'],[]])

    def test_four_and_eight_cover_new_fuzz_example_and_whole_packages(self):
        for workers in (4,8):
            c = catalog(workers);p.validate(c,workers)
            self.assertIn(p.unit(p.HEAVY[0],'TestNewlyAdded'),c['units'])
            self.assertIn('fileless',c['units'])
            self.assertNotIn(p.HEAVY[0],c['units'])
            with tempfile.TemporaryDirectory() as d, mock.patch.object(run_ci_tier.full_race,'source_proof',return_value=SOURCE), mock.patch.dict(os.environ,{'GITHUB_SHA':SOURCE['source']}):
                evidence(Path(d),c)
                r.reconcile(d,workers)
                # Truncated runtime descendant must fail even if the parent and
                # package retained successful terminals and manifest claims.
                selected=next((Path(d)/str(i)/p.log_name(n)) for i,a in enumerate(c['assignments']) for n in a['parents'])
                old=selected.read_text();records=r.json_stream(old)
                selected.write_text(rows([e for e in records if not (e.get('Action')=='pass' and '/' in e.get('Test',''))]))
                with self.assertRaises(r.Failure):r.reconcile(d,workers)
                selected.write_text(old)
                (Path(d)/'0'/'manifest.json').unlink()
                with self.assertRaises(r.Failure):r.reconcile(d,workers)

    def test_parent_and_descendant_lifecycles_reject_duplicate_or_unordered_events(self):
        c = catalog(4)
        with tempfile.TemporaryDirectory() as d:
            root = Path(d); evidence(root, c)
            index, package = next((i, n) for i, a in enumerate(c['assignments']) for n in a['parents'])
            worker = root/str(index); path = worker/p.log_name(package)
            original = path.read_text(); events = r.json_stream(original)
            for name in (events[0]['Test'], events[1]['Test']):
                for malformed in (['run', 'run', 'pass'], ['pass', 'run'],
                                  ['run', 'pause', 'pass'], ['run', 'cont', 'pass']):
                    bad = [e for e in events if e.get('Test') != name]
                    bad[0:0] = [event(package, action, Test=name) for action in malformed]
                    path.write_text(rows(bad))
                    with self.assertRaises(r.Failure):
                        p.verify_logs(worker, c, c['assignments'][index])
            path.write_text(original)
            p.verify_logs(worker, c, c['assignments'][index])

    def test_timing_report_includes_new_parent_logs_with_shared_lifecycle_semantics(self):
        import report_ci_timing
        c=catalog(4)
        with tempfile.TemporaryDirectory() as d:
            root=Path(d);evidence(root,c)
            index=next(i for i,a in enumerate(c['assignments']) if a['parents'])
            report=report_ci_timing.worker_evidence(root/str(index))
            actual=[row for row in report['complete_parent_trees'] if row['log'].startswith('parents-')]
            self.assertEqual(len(actual),sum(len(names) for names in c['assignments'][index]['parents'].values()))
            self.assertTrue(all(row['seconds']==0 and row['timing_issues']==[] for row in actual))

    def test_current_schema_hint_import_reconciles_actual_parent_logs(self):
        import generate_go_shard_hints as hints
        c=catalog(8)
        with tempfile.TemporaryDirectory() as d, mock.patch.object(run_ci_tier.full_race,'source_proof',return_value=SOURCE), mock.patch.dict(os.environ,{'GITHUB_SHA':SOURCE['source']}):
            root=Path(d);evidence(root,c)
            settings={str(i):json.loads((root/str(i)/'manifest.json').read_text())['settings'] for i in range(8)}
            result=hints.generate([('unit-current-schema',root)],SOURCE['source'],settings,list(c['packages']),c['cli'])
            self.assertEqual(len(result['parents']),5 * len(p.HEAVY))
            self.assertEqual(set(result['cli']),set(c['cli']))
            self.assertEqual(result['parents'][p.unit(p.HEAVY[0],'TestNewlyAdded')],1)
            bad=root/'0'/'manifest.json';value=json.loads(bad.read_text());value['settings']['workers']=4;bad.write_text(json.dumps(value))
            with self.assertRaises(r.Failure):hints.generate([('unit-current-schema',root)],SOURCE['source'],settings,list(c['packages']),c['cli'])

    def test_catalog_rejects_schema_source_configuration_and_unit_tampering(self):
        good=catalog()
        changes=[lambda c:c.update(schema=2),lambda c:c.update(allocation='mixed-lpt'),lambda c:c.update(schema=1),lambda c:c.update(schema=True),
                 lambda c:c['source_proof'].update(tree='broken'),
                 lambda c:c['configuration'].update(GOFLAGS='-mod=mod'),
                 lambda c:c['configuration'].update(GOVERSION='go1.99'),
                 lambda c:c['parents'].pop(p.HEAVY[0]),
                 lambda c:c['units'].append('invented'),
                 lambda c:c['assignments'][0]['packages'].append('fileless')]
        for change in changes:
            bad=copy.deepcopy(good);change(bad)
            with self.assertRaises(r.Failure):p.validate(bad,8)

    def test_discovery_listing_and_raw_source_config_are_authoritative(self):
        c=catalog()
        with tempfile.TemporaryDirectory() as d:
            root=Path(d);evidence(root,c)
            a=c['assignments'][0];worker=root/'0'
            for filename,content in [('listing-parents.json',listing(p.HEAVY[0],['TestRenamed'])),
                                     ('discovery-config.json','{}'),('packages.json','{}'),
                                     ('parents-unassigned.json',body(p.HEAVY[0],['TestA']))]:
                file=worker/filename;old=file.read_text() if file.exists() else None;file.write_text(content)
                with self.assertRaises((r.Failure,ValueError)):p.verify_logs(worker,c,a)
                if old is None:file.unlink()
                else:file.write_text(old)

    def test_hints_do_not_define_coverage_and_empty_testmain_stays_whole(self):
        c=catalog(4)
        packages=c['packages'];parents=copy.deepcopy(c['parents']);parents[p.HEAVY[0]]=[]
        updated=p.build(SOURCE['source'],SOURCE,CONFIG,packages,c['cli'],parents,
                        {'packages':{'stale':9999999},'cli':{'TestRemoved':9999999},'parents':{'not-discovered::Test':9999999}},4)
        p.validate(updated,4)
        self.assertIn(p.HEAVY[0],updated['units'])
        self.assertNotIn('stale',updated['units'])
        self.assertEqual(p.listing(listing(p.HEAVY[0],[]),[p.HEAVY[0]]),{p.HEAVY[0]:[]})
        no_files=rows([event(p.HEAVY[0],'output',Output='? package [no test files]'),event(p.HEAVY[0],'skip')])
        self.assertEqual(p.listing(no_files,[p.HEAVY[0]]),{p.HEAVY[0]:[]})

    def test_listing_duplicate_failed_executed_or_missing_packages_reject(self):
        package=p.HEAVY[0]
        for raw in (listing(package,['TestA','TestA']),listing(package,['TestA/child']),
                    listing(package,['TestMain']),rows([event(package,'fail')]),
                    rows([event(package,'run',Test='TestA'),event(package,'pass')]),''):
            with self.assertRaises(r.Failure):p.listing(raw,[package])

    def test_workflow_matrix_worker_count_cache_and_aggregate_are_consistent(self):
        workflow=(Path(__file__).resolve().parent.parent/'.github/workflows/test.yml').read_text()
        short=workflow
        self.assertIn('      - run: python3 -B scripts/test_go_parent_shards.py', workflow)
        self.assertIn('WORKERS: ${{ matrix.workers }}',short)
        self.assertIn("'{\"workers\":[8],\"shard\":[0,1,2,3,4,5,6,7]}'",short)
        self.assertIn("'{\"workers\":[4],\"shard\":[0,1,2,3]}'",short)
        self.assertEqual(short.count('run_go_shard.py --workers \"$WORKERS\"'),2)
        for n in (2,4,8):self.assertEqual(r.worker_count(n),n)
        self.assertIn('linux-short',workflow.split('  linux-race:',1)[1])


if __name__=='__main__':unittest.main()
