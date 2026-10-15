"""Behavior checks for discovery, complete coverage and bounded process cleanup."""
import copy
import contextlib
import errno
import copy
import io
import json
import os
import re
from pathlib import Path
import signal
import subprocess
import sys
import tempfile
import time
import unittest
from unittest import mock

import run_go_shard as runner

CLI = runner.CLI


def records(*items):
    return '\n'.join(json.dumps(item) for item in items)


def event(action, package=CLI, **kw):
    return dict(Action=action, Package=package, **kw)


class JSONStreamTest(unittest.TestCase):
    def test_concatenated_multiline_objects_and_all_strip_whitespace(self):
        items = [{'ImportPath': 'synthetic/pkg', 'Nested': {'a': [1, True, None]}},
                 event('output', Output='quoted braces {} and newline\n'), {}]
        for separator in ('', '\n', ' \t\r\n', '\v\f\u0085\u00a0\u2003'):
            with self.subTest(separator=separator):
                text = separator + separator.join(json.dumps(item, indent=2) for item in items) + separator
                self.assertEqual(runner.json_stream(text), items)
        for text in ('', ' \t\r\n\v\f\u0085\u00a0\u2003'):
            self.assertEqual(runner.json_stream(text), [])

    def test_malformed_trailing_input_and_nonobject_records_still_fail(self):
        for text in ('{', '{} bad-json', '{} {"incomplete":', '{}\n{"x":1,}', '{}\x00'):
            with self.subTest(text=text), self.assertRaises(json.JSONDecodeError):
                runner.json_stream(text)
        for value in ([], 'text', 1, True, None):
            with self.subTest(value=value), self.assertRaisesRegex(runner.Failure, 'JSON record is not an object'):
                runner.json_stream('{}\n' + json.dumps(value))

    def test_malformed_error_retains_record_relative_document_and_position(self):
        malformed = '{\n  "x": 1,\n}'
        with self.assertRaises(json.JSONDecodeError) as expected:
            json.JSONDecoder().raw_decode(malformed)
        with self.assertRaises(json.JSONDecodeError) as actual:
            runner.json_stream('{} \n\u2003' + malformed)
        for field in ('msg', 'doc', 'pos', 'lineno', 'colno'):
            self.assertEqual(getattr(actual.exception, field), getattr(expected.exception, field))
        self.assertEqual(str(actual.exception), str(expected.exception))

    def test_large_stream_decodes_original_input_at_advancing_indices_without_suffix_copies(self):
        class ImmutableInput(str):
            def strip(self, *args):
                raise AssertionError('whole-stream strip scans the remaining suffix')

            def lstrip(self, *args):
                raise AssertionError('whole-stream lstrip copies the remaining suffix')

            def __getitem__(self, key):
                if isinstance(key, slice):
                    raise AssertionError('JSON stream copied a suffix')
                return super().__getitem__(key)

        count = 4096
        row = event('output', Output='synthetic ' * 128)
        text = ImmutableInput('\n' + (json.dumps(row) + '\n') * count)
        decoder = json.JSONDecoder()
        indices = []

        def decode(source, index):
            self.assertIs(source, text)
            self.assertGreater(index, indices[-1] if indices else -1)
            indices.append(index)
            return decoder.raw_decode(source, index)

        with mock.patch.object(runner.json, 'JSONDecoder', return_value=mock.Mock(raw_decode=decode)):
            actual = runner.json_stream(text)
        self.assertEqual(actual, [row] * count)
        self.assertEqual(len(indices), count)
        self.assertEqual(indices[0], 1)


def synthetic_vet_context(image='unit1'):
    """Synthetic unit identity; never evidence of hosted/native execution."""
    import go_cache_context
    go = dict.fromkeys(go_cache_context.GO_FIELDS, '')
    go.update(GOVERSION='go1.27.2', GOOS='linux', GOHOSTOS='linux', GOARCH='amd64',
              GOHOSTARCH='amd64', CGO_ENABLED='1', GOTOOLCHAIN='local', GOWORK='off',
              GOFLAGS='-mod=readonly')
    return {'go': go, 'version': 'go version go1.27.2 linux/amd64', 'runner_os': 'Linux',
            'runner_arch': 'X64', 'image': {'ImageOS': 'ubuntu24', 'ImageVersion': image},
            'os_release': 'a'*64, 'cc': {'binary': 'b'*64, 'version': 'c'*64},
            'cxx': {'binary': 'd'*64, 'version': 'e'*64},
            'toolchain': dict.fromkeys(('go','compile','asm','link','cgo','vet'), 'f'*64),
            'race': False, 'tags': [], 'schema': go_cache_context.SCHEMA,
            'native_environment': 'd'*64, 'pinned_tools': go_cache_context.TOOL_PINS,
            'compilation_modes': go_cache_context.COMPILATION_MODES}


def synthetic_worker_receipt(directory, manifest, image='unit1'):
    """Construct canonical-format UNIT fixture, not successful actual vet proof."""
    context = synthetic_vet_context(image)
    stdout, stderr = b'unit stdout\n', b'unit stderr\n'
    receipt = {'schema': 1, 'identity_schema': 'full-v1', 'command': ['go','vet','./...'],
        'log': 'plain-vet.txt', 'stderr_log': 'plain-vet.stderr', 'exit_code': 0,
        'success': True, 'performance_enabled': False, 'real_systemd_enabled': False,
        'pre_source_proof': manifest['settings']['plain_vet_source_proof'],
        'post_source_proof': manifest['settings']['plain_vet_source_proof'],
        'context': context, 'post_context': copy.deepcopy(context), 'invocation_seconds': .01,
        'log_sha256': __import__('hashlib').sha256(stdout).hexdigest(),
        'stderr_sha256': __import__('hashlib').sha256(stderr).hexdigest()}
    manifest['settings'].pop('plain_vet_identity_schema', None)
    manifest['settings'].update(plain_vet_receipt_schema=1, plain_vet_context=context,
                                plain_vet_context_sha256=runner.digest(context))
    manifest['plain_vet'] = receipt
    (directory/'plain-vet.txt').write_bytes(stdout)
    (directory/'plain-vet.stderr').write_bytes(stderr)
    (directory/'plain-vet-receipt.json').write_text(json.dumps(receipt))
    return receipt


class CatalogTest(unittest.TestCase):
    def test_object_stream_offset_parser_preserves_concatenation_and_errors(self):
        objects=[{'Action':'run','nested':{'value':'} inside string'}},{'Action':'pass'}]
        self.assertEqual(runner.json_stream(' \n\t'+''.join(json.dumps(o) for o in objects)+' \n'),objects)
        self.assertEqual(runner.json_stream(' \t\n'),[])
        for raw in ('{} trailing','{} {','[]','{} 1','null','{"x": NaN} bad'):
            with self.assertRaises((ValueError,runner.Failure)):runner.json_stream(raw)

    def test_metadata_keeps_fileless_packages_and_rejects_errors(self):
        items = [dict(ImportPath=n, Dir='/tmp', TestGoFiles=['x_test.go'] if n == CLI else [])
                 for n in (CLI, 'ordinary/a', 'ordinary/b')]
        self.assertEqual(runner.metadata(records(*items)), {CLI: True, 'ordinary/a': False, 'ordinary/b': False})
        for bad in (items + [items[0]], [], [dict(items[0], Error={'Err': 'bad'})] + items[1:]):
            with self.assertRaises(runner.Failure):
                runner.metadata(records(*bad))
        with self.assertRaises(ValueError):
            runner.metadata('{} bad-json')

    def test_listing_includes_fuzz_examples_but_never_duplicates_or_testmain(self):
        listing = [event('output', Output=n+'\n') for n in ('TestA', 'FuzzB', 'ExampleC')]
        self.assertEqual(runner.listed_names(records(*listing, event('pass'))), ['ExampleC', 'FuzzB', 'TestA'])
        for bad in (listing + [listing[0]], [event('output', Output='TestMain\n')],
                    [event('output', Output='TestA/child\n')], []):
            with self.assertRaises(runner.Failure):
                runner.listed_names(records(*bad, event('pass')))
        with self.assertRaises(runner.Failure):
            runner.listed_names(records(*listing, event('skip')))

    def test_integer_partition_unknown_stale_zero_and_ties(self):
        self.assertEqual(runner.partition(['b', 'a', 'c'], {'a': 10, 'b': 10, 'stale': 200}), [['c'], ['a', 'b']])
        self.assertEqual(runner.partition(['b', 'a'], {'a': 0, 'b': 0}), [['a'], ['b']])
        for units, hints in ((['a', 'a'], {}), (['a'], {}), (['a', 'b'], {'a': 1.5}), (['a', 'b'], {'a': -1})):
            with self.assertRaises(runner.Failure):
                runner.partition(units, hints)

    def test_exact_selector_prefix_escaping_and_limits(self):
        pattern = runner.selector(['TestA', 'TestAA', 'ExampleB'])
        import re
        self.assertTrue(re.fullmatch(pattern, 'TestAA'))
        self.assertFalse(re.fullmatch(pattern, 'TestAAA'))
        self.assertIn(r'TestA\.', runner.selector(['TestA.']))
        for names in ([], ['TestA', 'TestA'], ['TestA/child']):
            with self.assertRaises(runner.Failure):
                runner.selector(names)
        with self.assertRaises(runner.Failure):
            runner.selector(['TestLong'], limit=3)

    def test_coverage_nested_fuzz_examples_and_no_test_terminals(self):
        es = [event('pass', Test=n) for n in ('TestA/child', 'TestA', 'FuzzB/seed', 'FuzzB', 'ExampleC')]
        es += [event('skip', Test='TestSkipped'), event('pass')]
        selection = ['TestA', 'FuzzB', 'ExampleC', 'TestSkipped']
        self.assertEqual(runner.coverage(records(*es), {CLI: True}, selection)['cli'], sorted(selection))
        for bad in (es[:-1], es[:-2] + [event('pass')], es+[event('pass')],
                    es+[event('pass', Test='TestOther')], es+[event('pass', Test='TestA')],
                    [event('output', Output='warning: no tests to run\n')]+es):
            with self.assertRaises(runner.Failure):
                runner.coverage(records(*bad), {CLI: True}, selection)
        no_tests = records(event('output', 'ordinary/a', Output='? ordinary/a [no test files]\n'), event('skip', 'ordinary/a'))
        runner.coverage(no_tests, {'ordinary/a': False})
        for text, has_tests in ((records(event('skip', 'ordinary/a')), False), (no_tests, True)):
            with self.assertRaises(runner.Failure):
                runner.coverage(text, {'ordinary/a': has_tests})


class WorkerCountTest(unittest.TestCase):
    def test_shared_one_two_three_four_bins_are_deterministic_exhaustive_disjoint(self):
        units = [f'Test{i}' for i in range(12)]
        for workers in (1, 2, 3, 4):
            bins = runner.partition(units, {}, workers)
            self.assertEqual(len(bins), workers)
            self.assertEqual(bins, runner.partition(list(reversed(units)), {}, workers))
            flattened = [n for b in bins for n in b]
            self.assertEqual(set(flattened), set(units))
            self.assertEqual(len(flattened), len(set(flattened)))
        for invalid in (0, 17, True, '4'):
            with self.assertRaises(runner.Failure): runner.partition(units, {}, invalid)
        for invalid in (0, 1, 3, 5, True, '4'):
            with self.assertRaises(runner.Failure): runner.worker_count(invalid)
        with self.assertRaises(runner.Failure): runner.partition(['a', 'b'], {}, 4)

    def test_fixed_overhead_offsets_rebalance_without_changing_union(self):
        units=['a','b','c','d']
        self.assertEqual(runner.partition(units,dict.fromkeys(units,10),2,[20,0]),[['c'],['a','b','d']])
        for bad in ([0], [0,-1], [0,True], [0,'1']):
            with self.assertRaises(runner.Failure): runner.partition(units,{},2,bad)

    def test_four_worker_reconciliation_requires_every_terminal_and_assignment(self):
        import run_ci_tier
        with tempfile.TemporaryDirectory() as directory:
            catalog = {'source': os.environ.get('GITHUB_SHA', 'abc'), 'workers': 4,
                       'packages': {CLI: True, **{f'p{i}': True for i in range(4)}},
                       'cli': [f'Test{i}' for i in range(4)],
                       'assignments': [{'packages': [f'p{i}'], 'cli': [f'Test{i}']} for i in range(4)]}
            for i in range(4):
                path = Path(directory)/str(i); path.mkdir()
                ordinary = records(event('pass', f'p{i}', Test='TestOrdinary'), event('pass', f'p{i}'))
                cli = records(event('pass', Test=f'Test{i}'), event('pass'))
                (path/'ordinary.json').write_text(ordinary); (path/'cli.json').write_text(cli)
                (path/'manifest.json').write_text(json.dumps({'shard': i, 'tier': 'pr', 'success': True,
                    'catalog': catalog, 'catalog_sha256': runner.digest(catalog), 'coverage': catalog['assignments'][i], 'settings': {'workers': 4},
                    'body_evidence': run_ci_tier.body_evidence(ordinary+'\n'+cli, True)}))
            runner.reconcile(directory, 4, historical_legacy=True)
            for i in range(4):
                path=Path(directory)/str(i)/'manifest.json'; value=json.loads(path.read_text())
                value['settings']['plain_audit_owned']=True; path.write_text(json.dumps(value))
            with self.assertRaises(runner.Failure): runner.reconcile(directory,4,historical_legacy=True)
            with mock.patch.object(run_ci_tier,'verify_source_audit',return_value={}) as audit:
                runner.reconcile(directory,4,Path('/audit'),historical_legacy=True)
                audit.assert_called_once_with(Path('/audit'),catalog['source'])
            with mock.patch.object(run_ci_tier,'verify_source_audit',side_effect=run_ci_tier.Failure('missing body')):
                with self.assertRaises(runner.Failure): runner.reconcile(directory,4,Path('/audit'),historical_legacy=True)
            with mock.patch.object(run_ci_tier,'verify_source_audit',return_value={}),self.assertRaises(runner.Failure):
                runner.reconcile(directory,4,Path('/audit'),historical_legacy=True,require_plain_vet=True)
            for i in range(4):
                path=Path(directory)/str(i)/'manifest.json';value=json.loads(path.read_text())
                value['settings'].update(plain_vet_owned=True,plain_vet_identity_schema=run_ci_tier.PLAIN_VET_IDENTITY_SCHEMA,plain_vet_context_sha256='d'*64,plain_vet_source_proof={'source':catalog['source'],'tree':'b'*40})
                path.write_text(json.dumps(value))
            proof={'source':catalog['source'],'tree':'b'*40}
            # Projected compatibility without own execution is an acceptance gap.
            with mock.patch.object(run_ci_tier,'verify_source_audit',return_value={'source_proof':proof}), self.assertRaises(runner.Failure):
                runner.reconcile(directory,4,Path('/audit'),historical_legacy=True,require_plain_vet=True)
            for i in range(4):
                path=Path(directory)/str(i)/'manifest.json';value=json.loads(path.read_text())
                synthetic_worker_receipt(path.parent,value,'unit'+str(i))
                path.write_text(json.dumps(value))
            with mock.patch.object(run_ci_tier,'verify_source_audit',return_value={'source_proof':proof}) as audit:
                runner.reconcile(directory,4,Path('/audit'),historical_legacy=True,require_plain_vet=True)
                self.assertNotIn('require_plain_vet',audit.call_args.kwargs)
            path=Path(directory)/'0'/'manifest.json';value=json.loads(path.read_text())
            for update in (dict(plain_vet_owned=False),dict(plain_vet_context_sha256='e'*64),dict(plain_vet_receipt_schema=None)):
                bad=copy.deepcopy(value);bad['settings'].update(update);path.write_text(json.dumps(bad))
                with mock.patch.object(run_ci_tier,'verify_source_audit',return_value={}),self.assertRaises(runner.Failure):runner.reconcile(directory,4,Path('/audit'),historical_legacy=True)
            path.write_text(json.dumps(value))
            for i in range(4):
                path=Path(directory)/str(i)/'manifest.json';value=json.loads(path.read_text())
                value.pop('plain_vet');value['settings'].pop('plain_vet_receipt_schema');value['settings'].pop('plain_vet_context');value['settings'].pop('plain_vet_owned');value['settings'].pop('plain_vet_context_sha256');value['settings'].pop('plain_vet_source_proof')
                path.write_text(json.dumps(value))
            for i in range(4):
                path=Path(directory)/str(i)/'manifest.json'; value=json.loads(path.read_text())
                value['settings']['plain_audit_owned']=False; path.write_text(json.dumps(value))
            with self.assertRaises(runner.Failure): runner.reconcile(directory, 2, historical_legacy=True)
            path = Path(directory)/'3'/'manifest.json'; original = path.read_text()
            for change in ({'success': False}, {'shard': 2}, {'coverage': {'packages': [], 'cli': []}}):
                m = json.loads(original); m.update(change); path.write_text(json.dumps(m))
                with self.assertRaises(runner.Failure): runner.reconcile(directory, 4, historical_legacy=True)
            path.unlink()
            with self.assertRaises(runner.Failure): runner.reconcile(directory, 4, historical_legacy=True)

    def test_worker_default_and_owned_vet_use_canonical_command_and_shared_deadline(self):
        import run_ci_tier
        with tempfile.TemporaryDirectory() as directory:
            root=Path(directory);(root/'scripts').mkdir()
            hints={'packages':{'p0':10,'p1':10},'cli':{'TestA':10,'TestB':10},
                   'auxiliary_costs_ms':{'vet':5,'git_invocation':0}}
            (root/'scripts/go_shard_hints.json').write_text(json.dumps(hints))
            calls=[]; deadlines=[]
            def supervise(commands,deadline,*,stderr_path=None,terminal=None,after=None):
                deadlines.append(deadline)
                if stderr_path is not None: stderr_path.write_bytes(b'unit stderr\n')
                if terminal is not None: terminal[:] = [0]
                for command,path,env in commands:
                    calls.append(command)
                    if command[1]=='list':
                        path.write_text(records(*[{'ImportPath':p,'Dir':'/tmp','TestGoFiles':['test.go']} for p in ['p0','p1',CLI]]))
                    elif '-list' in command:
                        path.write_text(records(event('output',Output='TestA\n'),event('output',Output='TestB\n'),event('pass')))
                    elif command[1]=='vet':path.write_text('')
                    elif CLI in command:
                        pattern=command[command.index('-run')+1]
                        path.write_text(records(*[event(action,Test=n) for n in ['TestA','TestB'] if re.fullmatch(pattern,n) for action in ('run','pass')],event('pass')))
                    else:path.write_text(records(*[event('pass',p) for p in ['p0','p1'] if p in command]))
                return [0.01]*len(commands)
            with mock.patch.object(runner,'ROOT',root),mock.patch.object(runner,'supervise',side_effect=supervise),mock.patch.object(runner.os,'sched_getaffinity',return_value={0,1,2,3},create=True),mock.patch.object(runner.subprocess,'check_output',side_effect=lambda command,**kw: json.dumps({k:synthetic_vet_context()['go'][k] for k in __import__('go_parent_shards').CONFIG_FIELDS}) if command[:2]==['go','env'] else 'a'*40+'\n'),mock.patch.object(run_ci_tier,'plain_vet_context',return_value=synthetic_vet_context()),mock.patch.object(run_ci_tier.full_race,'source_proof',return_value={'source':'a'*40,'tree':'b'*40}),mock.patch.dict(os.environ,{'GITHUB_SHA':'a'*40}),contextlib.redirect_stdout(io.StringIO()):
                runner.run(0,root/'default',30)
                self.assertEqual(sum(c==['go','vet','./...'] for c in calls),1)
                calls.clear();deadlines.clear();runner.run(0,root/'owned',30,plain_vet_owned=True)
                self.assertEqual(sum(c==['go','vet','./...'] for c in calls),1)
                self.assertEqual(len(set(deadlines)),1)
                self.assertTrue((root/'owned/plain-vet.stderr').exists())
                self.assertTrue((root/'owned/plain-vet-receipt.json').exists())
            default=json.loads((root/'default/manifest.json').read_text());owned=json.loads((root/'owned/manifest.json').read_text())
            self.assertEqual(default['scheduling']['fixed_vet_ms'],[5,0])
            self.assertEqual(owned['scheduling']['fixed_vet_ms'],[0,0])
            self.assertTrue(owned['settings']['plain_vet_owned'])
            self.assertIn('vet_seconds',default['phases']);self.assertNotIn('vet_seconds',owned['phases'])



class WorkerVetReceiptTest(unittest.TestCase):
    """Synthetic unit controls; real hosted positives remain unimplemented."""
    def test_raw_context_source_command_and_terminal_tampering_reject(self):
        import run_ci_tier
        with tempfile.TemporaryDirectory() as directory:
            path=Path(directory); proof={'source':'a'*40,'tree':'b'*40}
            original={'settings':{'plain_vet_owned':True,'plain_vet_source_proof':proof}}
            receipt=synthetic_worker_receipt(path,original)
            runner.verify_worker_vet(path,original,proof)
            for field,value in [('exit_code',1),('exit_code',True),('success',False),
                    ('identity_schema','compatible-v1'),('command',['go','vet','-race','./...']),
                    ('pre_source_proof',{}),('post_source_proof',{}),('post_context',{})]:
                with self.subTest(field=field,value=value):
                    manifest=copy.deepcopy(original);bad=copy.deepcopy(receipt);bad[field]=value
                    manifest['plain_vet']=bad;(path/'plain-vet-receipt.json').write_text(json.dumps(bad))
                    with self.assertRaises((runner.Failure,run_ci_tier.Failure)):
                        runner.verify_worker_vet(path,manifest,proof)
            (path/'plain-vet-receipt.json').write_text(json.dumps(receipt))
            for name in ('plain-vet.txt','plain-vet.stderr'):
                raw=(path/name).read_bytes();(path/name).write_bytes(raw+b'changed')
                with self.assertRaises((runner.Failure,run_ci_tier.Failure)):
                    runner.verify_worker_vet(path,original,proof)
                (path/name).write_bytes(raw)
            bad=copy.deepcopy(receipt);bad['context']['image']['ImageVersion']='other'
            bad['post_context']=copy.deepcopy(bad['context']);manifest=copy.deepcopy(original)
            manifest['plain_vet']=bad;(path/'plain-vet-receipt.json').write_text(json.dumps(bad))
            with self.assertRaises(runner.Failure):runner.verify_worker_vet(path,manifest,proof)

    def test_failed_cancelled_nonzero_and_context_drift_leave_rejected_receipt(self):
        import run_ci_tier
        proof={'source':'a'*40,'tree':'b'*40};context=synthetic_vet_context()
        settings={'plain_vet_source_proof':proof,'plain_vet_context':context,
                  'plain_vet_context_sha256':runner.digest(context)}
        for mode in ('failure','cancel','nonzero','drift'):
            with self.subTest(mode=mode),tempfile.TemporaryDirectory() as directory:
                path=Path(directory)
                def supervise(commands,deadline,*,stderr_path=None,terminal=None,after=None):
                    self.assertEqual(deadline,12345)
                    self.assertEqual(commands[0][0],['go','vet','./...'])
                    commands[0][1].write_bytes(b'raw stdout');stderr_path.write_bytes(b'raw stderr')
                    terminal[:]=[7 if mode=='nonzero' else 0]
                    if mode=='failure':raise runner.Failure('unit child failure')
                    if mode=='cancel':raise KeyboardInterrupt('unit cancellation')
                changed=copy.deepcopy(context);changed['image']['ImageVersion']='drift'
                contexts=[context,changed] if mode=='drift' else [context,context,context]
                with mock.patch.object(runner,'supervise',side_effect=supervise),mock.patch.object(run_ci_tier.full_race,'source_proof',return_value=proof),mock.patch.object(run_ci_tier,'plain_vet_context',side_effect=contexts):
                    with self.assertRaises((runner.Failure,run_ci_tier.Failure,KeyboardInterrupt)):
                        runner.execute_worker_vet(path,12345,settings)
                receipt=json.loads((path/'plain-vet-receipt.json').read_text())
                self.assertFalse(receipt['success']);self.assertIn('error',receipt)
                self.assertEqual(receipt['exit_code'],7 if mode=='nonzero' else 0)
                self.assertEqual((path/'plain-vet.txt').read_bytes(),b'raw stdout')
                self.assertEqual((path/'plain-vet.stderr').read_bytes(),b'raw stderr')


class GateTest(unittest.TestCase):
    def test_event_verdict_fail_closed(self):
        pr = {j: {'result': s} for j, s in [('linux-short', 'success'), ('linux-full', 'success'), ('script-tests', 'success'), ('macos-smoke','success'), ('tier-plan','success'), ('source-audit','success')]}
        runner.verdict('pull_request', 'refs/pull/1/merge', pr)
        candidate = copy.deepcopy(pr)
        candidate['linux-short']['result'], candidate['linux-full']['result'], candidate['source-audit']['result'] = 'skipped', 'success', 'skipped'
        runner.verdict('push', 'refs/heads/release-candidate/x', candidate)
        for event_name, ref, needs in [('push', 'refs/heads/main', pr), ('schedule', '', pr), ('pull_request', '', {}),
                                       ('push', 'refs/heads/release-candidate/', candidate)]:
            with self.assertRaises(runner.Failure):
                runner.verdict(event_name, ref, needs)
        for base, event_name, ref in ((pr, 'pull_request', ''), (candidate, 'push', 'refs/heads/release-candidate/x')):
            for job in base:
                for result in ('failure', 'cancelled', None, 'unexpected'):
                    bad = copy.deepcopy(base);bad[job]['result'] = result
                    with self.assertRaises(runner.Failure):
                        runner.verdict(event_name, ref, bad)

    def test_manifest_reconciliation_requires_both_matching_complete_shards(self):
        catalog = {'source': os.environ.get('GITHUB_SHA', 'abc'), 'packages': {CLI: True, 'a': False, 'b': True},
                   'cli': ['FuzzA', 'ExampleB'], 'assignments': [{'packages': ['a'], 'cli': ['FuzzA']},
                                                              {'packages': ['b'], 'cli': ['ExampleB']}]}
        with tempfile.TemporaryDirectory() as directory:
            paths = []
            for i in range(2):
                path = Path(directory)/str(i)/'manifest.json';path.parent.mkdir();paths.append(path)
                cli_text = records(*[event('pass', Test=name) for name in catalog['assignments'][i]['cli']], event('pass'))
                (path.parent/'cli.json').write_text(cli_text)
                ordinary_text = records(event('pass','b',Test='TestOrdinary'),event('pass','b')) if i == 1 else records(event('output','a',Output='? a [no test files]\n'),event('skip','a'))
                (path.parent/'ordinary.json').write_text(ordinary_text)
                import run_ci_tier
                proof = run_ci_tier.body_evidence(ordinary_text+'\n'+cli_text,True)
                path.write_text(json.dumps({'shard': i, 'catalog': catalog, 'catalog_sha256': runner.digest(catalog),
                                            'success': True, 'tier': 'pr', 'body_evidence': proof, 'coverage': catalog['assignments'][i]}))
            runner.reconcile(directory,historical_legacy=True)
            good = paths[1].read_text()
            for key, value in [('success', False), ('shard', 0), ('coverage', {'packages': [], 'cli': []}),
                               ('catalog_sha256', 'different'), ('body_evidence', {})]:
                item = json.loads(good);item[key] = value;paths[1].write_text(json.dumps(item))
                with self.assertRaises(runner.Failure):
                    runner.reconcile(directory,historical_legacy=True)
            paths[1].unlink()
            with self.assertRaises(runner.Failure):
                runner.reconcile(directory,historical_legacy=True)


@unittest.skipUnless(sys.platform == 'linux', 'Linux worker uses procfs and child subreaping')
class RunPlanTest(unittest.TestCase):
    def test_complete_runner_executes_each_package_once_and_reconciles_both_bins(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root/'scripts').mkdir()
            (root/'scripts/go_shard_hints.json').write_text(json.dumps({'packages': {runner.GIT_IDENTITY: 10000}, 'cli': {}}))
            bindir = root/'bin';bindir.mkdir()
            command_log = root/'commands.jsonl'
            fake = bindir/'go'
            fake.write_text('#!'+sys.executable+'\n'+r'''import json,os,re,sys
args=sys.argv[1:]
cli='github.com/wangjohn/agent-archive/internal/cli'
git='github.com/wangjohn/agent-archive/internal/gitremote'
packages={cli:True,git:True,'ordinary/a':False,'ordinary/b':False}
with open(os.environ['TASK_COMMAND_LOG'],'a') as f:f.write(json.dumps({'args':args,'cpu':os.environ.get('GOMAXPROCS')})+'\n')
def emit(action,package,**kw):print(json.dumps(dict(Action=action,Package=package,**kw)),flush=True)
if args[0]=='list':
 for name,has_tests in packages.items():print(json.dumps(dict(ImportPath=name,Dir='/fixture',TestGoFiles=['x_test.go'] if has_tests else [])))
elif '-list' in args:
 for name in ['TestA','FuzzB','Example']:emit('output',cli,Output=name+'\n')
 emit('pass',cli)
elif args[0]=='vet':print('vet complete')
elif '-run' in args:
 pattern=args[args.index('-run')+1]
 for name in ['TestA','FuzzB','Example']:
  if re.fullmatch(pattern,name):
   emit('run',cli,Test=name)
   if name!='Example':emit('run',cli,Test=name+'/child');emit('pass',cli,Test=name+'/child')
   emit('pass',cli,Test=name)
 emit('pass',cli)
else:
 for package in [arg for arg in args if arg in packages]:
  if not packages[package]:
   emit('output',package,Output='? '+package+' [no test files]\n');emit('skip',package)
  else:emit('pass',package,Test='TestGit');emit('pass',package)
''')
            fake.chmod(0o755)
            with mock.patch.object(runner, 'ROOT', root), mock.patch('run_ci_tier.full_race.source_proof',return_value={'source':'a'*40,'tree':'b'*40}), mock.patch.object(runner.subprocess, 'check_output', side_effect=lambda command,**kw: json.dumps({k:synthetic_vet_context()['go'][k] for k in __import__('go_parent_shards').CONFIG_FIELDS}) if command[:2]==['go','env'] else 'a'*40+'\n'), mock.patch.dict(os.environ, {'PATH': str(bindir)+os.pathsep+os.environ['PATH'], 'GITHUB_SHA': 'a'*40, 'TASK_COMMAND_LOG': str(command_log)}), contextlib.redirect_stdout(io.StringIO()):
                for index in (0, 1):runner.run(index, root/str(index), 30)
                runner.reconcile(root)
            calls = [json.loads(line) for line in command_log.read_text().splitlines()]
            self.assertTrue(all('-race' in call['args'] for call in calls if call['args'][0]=='list'))
            listings = [call for call in calls if '-list' in call['args']]
            self.assertEqual(len(listings),2)
            self.assertTrue(all('-count=1' in call['args'] and '-race' in call['args'] and '-short' in call['args'] for call in listings))
            executed = [call for call in calls if call['args'][0]=='test' and '-list' not in call['args']]
            for package in (runner.GIT_IDENTITY, 'ordinary/a', 'ordinary/b'):
                self.assertEqual(sum(package in call['args'] for call in executed), 1)
            git_call = next(call for call in executed if runner.GIT_IDENTITY in call['args'])
            self.assertNotIn('-run', git_call['args'])
            self.assertEqual(sum(runner.CLI in call['args'] for call in executed), 1)
            first = json.loads((root/'0/manifest.json').read_text())
            self.assertEqual(first['settings']['isolated_packages'], [runner.GIT_IDENTITY])
            self.assertIn('git_identity_invocation_seconds', first['phases'])
            self.assertIn('vet_seconds', first['phases'])
            self.assertFalse((root/'0/ordinary.json').exists(), 'empty batch must not default to testing the module root')


class CleanupErrorTest(unittest.TestCase):
    def test_signal_and_wait_errors_do_not_abandon_later_owned_groups(self):
        # A terminal leader does not prove its private group has no descendants.
        first, sibling = mock.Mock(pid=123), mock.Mock(pid=456)
        first.poll.return_value = sibling.poll.return_value = 0
        first.wait.side_effect = subprocess.TimeoutExpired('synthetic command', 5)
        def rejected(pgid, sig):
            if pgid == first.pid:
                raise PermissionError(errno.EPERM, 'injected')
            raise ProcessLookupError(errno.ESRCH, 'already gone')
        with mock.patch.object(runner.os, 'killpg', side_effect=rejected) as signals:
            with self.assertRaises(runner.CleanupFailure) as raised:
                runner.stop_processes([first, sibling], None)
        self.assertEqual(signals.call_args_list, [
            mock.call(p.pid, sig) for sig in (signal.SIGTERM, signal.SIGKILL)
            for p in (first, sibling)])
        first.wait.assert_called_once()
        sibling.wait.assert_called_once()
        self.assertIn('wait pid=123 cleanup deadline', str(raised.exception))
        self.assertIn('signal=SIGKILL', str(raised.exception))

    def test_poll_error_preserves_primary_and_attempts_every_cleanup_phase(self):
        first, sibling = mock.Mock(pid=123), mock.Mock(pid=456)
        sibling.poll.return_value = 0
        commands = [(['synthetic'], mock.Mock(), None)] * 2
        primary = runner.Failure('original execution failure')
        polls = 0
        def rejected():
            nonlocal polls
            polls += 1
            if polls == 1:
                raise primary
            raise PermissionError(errno.EPERM, 'injected poll rejection')
        first.poll.side_effect = rejected
        with mock.patch.object(runner.sys, 'platform', 'darwin'), \
                mock.patch('builtins.open', mock.mock_open()), \
                mock.patch.object(runner.subprocess, 'Popen', side_effect=[first, sibling]), \
                mock.patch.object(runner.os, 'killpg') as signals:
            with self.assertRaises(runner.CleanupFailure) as raised:
                runner.supervise(commands, time.monotonic()+5)
        self.assertIs(raised.exception.primary, primary)
        self.assertIs(raised.exception.__cause__, primary)
        self.assertIn('poll pid=123: PermissionError errno=1', str(raised.exception))
        self.assertIn('original execution failure', str(raised.exception))
        self.assertEqual(signals.call_args_list, [
            mock.call(p.pid, sig) for sig in (signal.SIGTERM, signal.SIGKILL)
            for p in (first, sibling)])
        first.wait.assert_called_once()
        sibling.wait.assert_called_once()

    def test_adopted_wait_error_still_attempts_signal_and_later_child(self):
        with mock.patch.object(runner, 'child_pids', side_effect=[{123, 456}, set()]), \
                mock.patch.object(runner.os, 'waitpid', side_effect=PermissionError(errno.EPERM, 'injected')) as waits, \
                mock.patch.object(runner.os, 'kill') as signals:
            with self.assertRaises(runner.CleanupFailure) as raised:
                runner.stop_processes([], set())
        self.assertCountEqual(waits.call_args_list,
                              [mock.call(pid, os.WNOHANG) for pid in (123, 456)])
        self.assertCountEqual(signals.call_args_list,
                              [mock.call(pid, signal.SIGTERM) for pid in (123, 456)])
        self.assertIn('waitpid pid=123: PermissionError errno=1', str(raised.exception))

    def test_adopted_child_signal_error_does_not_abandon_other_children(self):
        def rejected(pid, sig):
            if pid == 123:
                raise PermissionError(errno.EPERM, 'injected')
        with mock.patch.object(runner, 'child_pids', side_effect=[{123, 456}, set()]), \
                mock.patch.object(runner.os, 'waitpid', return_value=(0, 0)) as waits, \
                mock.patch.object(runner.os, 'kill', side_effect=rejected) as signals:
            with self.assertRaises(runner.CleanupFailure) as raised:
                runner.stop_processes([], set())
        self.assertCountEqual(waits.call_args_list,
                              [mock.call(pid, os.WNOHANG) for pid in (123, 456)])
        self.assertCountEqual(signals.call_args_list,
                              [mock.call(pid, signal.SIGTERM) for pid in (123, 456)])
        self.assertIn('kill pid=123 signal=SIGTERM: PermissionError errno=1', str(raised.exception))


@unittest.skipUnless(sys.platform in ('linux', 'darwin'), 'supported process-group platforms')
class PortableBatchProcessTest(unittest.TestCase):
    def commands(self, directory, *codes):
        return [([sys.executable, '-B', '-c', code], Path(directory)/f'out-{i}', os.environ.copy())
                for i, code in enumerate(codes)]

    def run_batch(self, commands, seconds):
        import run_ci_tier
        children = []
        popen = subprocess.Popen
        def launch(*args, **kwargs):
            child = popen(*args, **kwargs)
            children.append(child)
            return child
        try:
            with mock.patch.object(runner.subprocess, 'Popen', side_effect=launch):
                run_ci_tier.checked_many(commands, time.monotonic()+seconds)
        finally:
            # poll/wait in the supervisor must have joined every direct child,
            # on success and every exceptional exit, leaving no zombies.
            for child in children:
                self.assertIsNotNone(child.returncode)
                with self.assertRaises(ProcessLookupError):
                    os.kill(child.pid, 0)

    def test_sequential_lane_preserves_overlap_and_stops_before_successor_on_failure(self):
        with tempfile.TemporaryDirectory() as d:
            marker=Path(d)/'first-completed'
            commands=self.commands(d,
                "import time;from pathlib import Path;time.sleep(.15);Path("+repr(str(marker))+").write_text('ready')",
                "from pathlib import Path;assert Path("+repr(str(marker))+").read_text()=='ready';print('second')",
                "import time;time.sleep(.1);print('overlap')")
            durations=runner.supervise(commands,time.monotonic()+3,after={1:0})
            self.assertEqual(commands[1][1].read_text().strip(),'second')
            self.assertEqual(commands[2][1].read_text().strip(),'overlap')
            self.assertEqual(len(durations),3)
            marker.unlink()
            failed=self.commands(d,'raise SystemExit(7)',"from pathlib import Path;Path("+repr(str(marker))+").write_text('must not launch')")
            with self.assertRaises(runner.Failure):runner.supervise(failed,time.monotonic()+3,after={1:0})
            self.assertFalse(marker.exists())
            for invalid in ({0:0},{0:1},{1:2},{2:0}):
                with self.assertRaises(runner.Failure):runner.supervise(failed,time.monotonic()+3,after=invalid)

    def test_cleanup_failure_preserves_primary_with_unlaunched_successor(self):
        with tempfile.TemporaryDirectory() as d:
            commands = self.commands(d, 'raise SystemExit(7)',
                                     "raise AssertionError('successor must not launch')")
            terminal = []
            with mock.patch.object(runner.os, 'killpg', side_effect=PermissionError(errno.EPERM, 'injected')):
                with self.assertRaises(runner.CleanupFailure) as raised:
                    runner.supervise(commands, time.monotonic()+5, after={1:0}, terminal=terminal)
            self.assertIsInstance(raised.exception.primary, runner.Failure)
            self.assertIn('child command failed: [7, None]', str(raised.exception.primary))
            self.assertEqual(terminal, [7, None])
            self.assertFalse(commands[1][1].exists())

    def test_expired_shared_deadline_launches_nothing(self):
        import run_ci_tier
        with tempfile.TemporaryDirectory() as d, mock.patch.object(runner.subprocess,'Popen') as launch:
            with self.assertRaisesRegex(run_ci_tier.Failure,'already exceeded'):
                run_ci_tier.checked_many(self.commands(d,'raise SystemExit(0)'),time.monotonic()-1)
            launch.assert_not_called()

    def test_two_private_processes_must_overlap_and_complete_their_own_streams(self):
        with tempfile.TemporaryDirectory() as d:
            codes = []
            for i in range(2):
                own, other = Path(d)/f'private-{i}', Path(d)/f'private-{1-i}'
                # Independent ephemeral ports/data, with a readiness barrier that
                # cannot finish if the second command is only launched later.
                codes.append("import socket,time;from pathlib import Path;s=socket.socket();s.bind(('127.0.0.1',0));p=Path("+repr(str(own))+");p.write_text(str(s.getsockname()[1]));q=Path("+repr(str(other))+");\nwhile not q.exists():time.sleep(.01)\nprint(p.read_text(),flush=True);s.close()")
            commands = self.commands(d, *codes)
            self.run_batch(commands, 5)
            ports = [int(output.read_text()) for _, output, _ in commands]
            self.assertEqual(len(set(ports)), 2)
            self.assertEqual(ports, [int((Path(d)/f'private-{i}').read_text()) for i in range(2)])

    def test_partial_failure_timeout_and_live_failure_join_every_child(self):
        import run_ci_tier
        for reason in ('exit', 'timeout', 'live'):
            with self.subTest(reason=reason), tempfile.TemporaryDirectory() as d:
                if reason == 'exit':
                    codes = ['print("completed",flush=True)', 'import time;time.sleep(.1);raise SystemExit(7)']
                elif reason == 'timeout':
                    codes = ['print("completed",flush=True)', 'import time;time.sleep(60)']
                else:
                    codes = ['import time;time.sleep(60)', 'import json,time;time.sleep(.1);print(json.dumps({"Action":"fail","Package":"p","Test":"TestA"}),flush=True);time.sleep(60)']
                commands = self.commands(d, *codes)
                if reason == 'live':
                    commands[1][0].extend(['test', '-json'])
                started = time.monotonic()
                with self.assertRaises(run_ci_tier.Failure):
                    self.run_batch(commands, .5 if reason == 'timeout' else 5)
                self.assertLess(time.monotonic()-started, 3)
                if reason in ('exit', 'timeout'):
                    self.assertEqual(commands[0][1].read_text(), 'completed\n')

    def test_signal_error_preserves_timeout_and_still_joins_both_commands(self):
        with tempfile.TemporaryDirectory() as d:
            children, waits, signals = [], [], []
            popen, killpg = subprocess.Popen, os.killpg
            def launch(*args, **kwargs):
                child = popen(*args, **kwargs)
                children.append(child)
                wait = child.wait
                def joined(*args, **kwargs):
                    waits.append(child.pid)
                    return wait(*args, **kwargs)
                child.wait = joined
                return child
            def signal_group(pgid, sig):
                signals.append((pgid, sig))
                if pgid == children[0].pid and sig == signal.SIGTERM:
                    raise PermissionError(errno.EPERM, 'injected signal rejection')
                return killpg(pgid, sig)
            terminal = []
            commands = self.commands(d, *['import time;time.sleep(60)']*2)
            started = time.monotonic()
            try:
                with mock.patch.object(runner.subprocess, 'Popen', side_effect=launch), \
                        mock.patch.object(runner.os, 'killpg', side_effect=signal_group):
                    with self.assertRaises(runner.CleanupFailure) as raised:
                        runner.supervise(commands, started+.2, terminal=terminal)
                error = raised.exception
                self.assertIsInstance(error.primary, runner.Failure)
                self.assertIs(error.__cause__, error.primary)
                for text in ('shared discovery/test deadline exceeded',
                             f'pgid={children[0].pid}', 'signal=SIGTERM', 'errno=1'):
                    self.assertIn(text, str(error))
                self.assertIn((children[1].pid, signal.SIGTERM), signals)
                self.assertIn((children[0].pid, signal.SIGKILL), signals)
                self.assertEqual(set(waits), {p.pid for p in children})
                self.assertTrue(all(status is not None and status != 0 for status in terminal))
                self.assertLess(time.monotonic()-started, 3)
                for child in children:
                    with self.assertRaises(ProcessLookupError):
                        os.kill(child.pid, 0)
            finally:
                for child in children:
                    if child.poll() is None:
                        child.kill()
                    child.wait(timeout=3)

    def test_cleanup_signal_error_rejects_success_with_terminal_receipt(self):
        import run_ci_tier
        with tempfile.TemporaryDirectory() as d:
            commands = self.commands(d, 'print("completed",flush=True)')
            with mock.patch.object(runner.os, 'killpg', side_effect=PermissionError(errno.EPERM, 'injected')):
                with self.assertRaisesRegex(run_ci_tier.Failure, 'process cleanup failed.*signal=SIGTERM.*errno=1'):
                    run_ci_tier.checked_many(commands, time.monotonic()+5)
            terminal = []
            with mock.patch.object(runner.os, 'killpg', side_effect=PermissionError(errno.EPERM, 'injected')):
                with self.assertRaises(runner.CleanupFailure) as raised:
                    runner.supervise(commands, time.monotonic()+5, terminal=terminal)
            self.assertIsNone(raised.exception.primary)
            self.assertEqual(terminal, [0])
            self.assertEqual(commands[0][1].read_text(), 'completed\n')

    def test_launch_failure_stops_and_reaps_the_already_started_sibling(self):
        with tempfile.TemporaryDirectory() as d:
            commands = self.commands(d, 'import time;time.sleep(60)')
            commands.append(([str(Path(d)/'missing-command')], Path(d)/'missing.out', os.environ.copy()))
            with self.assertRaises(FileNotFoundError):
                self.run_batch(commands, 5)

    def test_group_cleanup_stops_and_reaps_an_owned_descendant(self):
        import run_ci_tier
        with tempfile.TemporaryDirectory() as d:
            pidfile=Path(d)/'descendant'
            # A cooperative parent joins its child on group termination; this
            # exercises native group signals as well as Linux subreaping.
            code="import subprocess,signal,sys,time;from pathlib import Path;child=subprocess.Popen([sys.executable,'-c','import time;time.sleep(60)']);signal.signal(signal.SIGTERM,lambda *a:(child.wait(timeout=2),sys.exit(0)));Path("+repr(str(pidfile))+").write_text(str(child.pid));time.sleep(60)"
            with self.assertRaises(run_ci_tier.Failure):
                self.run_batch(self.commands(d,code,'import time;time.sleep(60)'),.5)
            self.assertTrue(pidfile.exists())
            with self.assertRaises(ProcessLookupError):
                os.kill(int(pidfile.read_text()),0)

    def test_batch_cleanup_preserves_a_preexisting_unrelated_child(self):
        import run_ci_tier
        unrelated = subprocess.Popen([sys.executable, '-c', 'import time;time.sleep(60)'], start_new_session=True)
        try:
            with tempfile.TemporaryDirectory() as d, self.assertRaises(run_ci_tier.Failure):
                self.run_batch(self.commands(d, 'import time;time.sleep(60)', 'import time;time.sleep(60)'), .2)
            self.assertIsNone(unrelated.poll())
        finally:
            unrelated.terminate(); unrelated.wait(timeout=3)

    def test_cancellation_joins_both_recorded_private_children(self):
        with tempfile.TemporaryDirectory() as d:
            paths = [Path(d)/f'pid-{i}' for i in range(2)]
            commands = self.commands(d, *["import os,time;from pathlib import Path;Path("+repr(str(p))+").write_text(str(os.getpid()));time.sleep(60)" for p in paths])
            # Run the signal owner in a separate process so cancellation cannot
            # interrupt unittest or signal unrelated test workers.
            arguments = [(command, str(output), None) for command, output, _ in commands]
            code = "import sys,time;from pathlib import Path;sys.path.insert(0,"+repr(str(Path(runner.__file__).parent))+");import run_ci_tier as t;t.checked_many([(c,Path(p),e) for c,p,e in "+repr(arguments)+"],time.monotonic()+60)"
            owner = subprocess.Popen([sys.executable, '-B', '-c', code], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            try:
                end = time.monotonic()+5
                while not all(p.exists() for p in paths) and time.monotonic()<end:
                    time.sleep(.02)
                self.assertTrue(all(p.exists() for p in paths))
                owner.send_signal(signal.SIGTERM)
                self.assertNotEqual(owner.wait(timeout=5), 0)
                for path in paths:
                    with self.assertRaises(ProcessLookupError):
                        os.kill(int(path.read_text()), 0)
            finally:
                if owner.poll() is None:
                    owner.kill(); owner.wait()


@unittest.skipUnless(sys.platform == 'linux', 'Linux process groups and child subreaping')
class ProcessTest(unittest.TestCase):
    def sleeper(self, directory, escaped=False):
        pidfile = Path(directory)/'pid'
        code = "import subprocess,time; from pathlib import Path; p=subprocess.Popen(['"+sys.executable+"','-c','import time;time.sleep(60)']); Path("+repr(str(pidfile))+").write_text(str(p.pid));time.sleep(60)"
        if escaped:
            code = code.replace(']); Path(', '], start_new_session=True); Path(')
        return [sys.executable, '-c', code], pidfile

    def assert_stopped(self, pidfile):
        if pidfile.exists():
            pid = int(pidfile.read_text())
            self.assertFalse(Path(f'/proc/{pid}').exists(), 'test grandchild must be reaped, including zombies')

    def test_separate_raw_streams_and_nonzero_terminal_cleanup(self):
        with tempfile.TemporaryDirectory() as d:
            terminal=[];out=Path(d)/'stdout';err=Path(d)/'stderr'
            code="import sys;print('raw stdout');print('raw stderr',file=sys.stderr);raise SystemExit(7)"
            with self.assertRaises(runner.Failure):
                runner.supervise([([sys.executable,'-c',code],out,os.environ.copy())],
                    time.monotonic()+3,stderr_path=err,terminal=terminal)
            self.assertEqual(terminal,[7])
            self.assertEqual(out.read_bytes(),b'raw stdout\n')
            self.assertEqual(err.read_bytes(),b'raw stderr\n')

    def test_failed_child_and_shared_timeout_stop_sibling_descendants(self):
        for failure in (False, True):
            with tempfile.TemporaryDirectory() as d:
                sleeper, pidfile = self.sleeper(d)
                commands = [(sleeper, Path(d)/'sleep.out', os.environ.copy())]
                if failure:
                    commands.append(([sys.executable, '-c', 'import time;time.sleep(.2);raise SystemExit(7)'], Path(d)/'fail.out', os.environ.copy()))
                start = time.monotonic()
                with self.assertRaises(runner.Failure):
                    runner.supervise(commands, start + .4)
                self.assertLess(time.monotonic()-start, 3)
                self.assert_stopped(pidfile)

    def test_cleanup_preserves_a_preexisting_unrelated_child(self):
        unrelated = subprocess.Popen([sys.executable, '-c', 'import time;time.sleep(60)'], start_new_session=True)
        try:
            with tempfile.TemporaryDirectory() as d:
                sleeper, pidfile = self.sleeper(d, escaped=True)
                with self.assertRaises(runner.Failure):
                    runner.supervise([(sleeper, Path(d)/'out', os.environ.copy())], time.monotonic()+.3)
                self.assert_stopped(pidfile)
                self.assertIsNone(unrelated.poll(), 'cleanup must not signal preexisting children')
        finally:
            unrelated.terminate();unrelated.wait(timeout=3)

    def test_timeout_reaps_grandchild_that_created_another_session(self):
        with tempfile.TemporaryDirectory() as d:
            sleeper, pidfile = self.sleeper(d, escaped=True)
            with self.assertRaises(runner.Failure):
                runner.supervise([(sleeper, Path(d)/'out', os.environ.copy())], time.monotonic()+.3)
            self.assert_stopped(pidfile)

    def test_live_json_failure_stops_commands_that_have_not_exited(self):
        with tempfile.TemporaryDirectory() as d:
            sleeper, pidfile = self.sleeper(d)
            code = "import json,time;time.sleep(.2);print(json.dumps({'Action':'fail','Package':'p','Test':'TestA'}),flush=True);time.sleep(60)"
            commands = [(sleeper, Path(d)/'sleep.out', os.environ.copy()),
                        ([sys.executable, '-c', code, 'test', '-json'], Path(d)/'json.out', os.environ.copy())]
            start = time.monotonic()
            with self.assertRaisesRegex(runner.Failure, 'live Go failure'):
                runner.supervise(commands, start+10)
            self.assertLess(time.monotonic()-start, 3)
            self.assert_stopped(pidfile)

    def test_cancellation_stops_process_group(self):
        with tempfile.TemporaryDirectory() as d:
            sleeper, pidfile = self.sleeper(d)
            code = "import sys,signal,time;sys.path.insert(0,"+repr(str(Path(runner.__file__).parent))+");import run_go_shard as r;signal.signal(signal.SIGTERM,lambda *args: (_ for _ in ()).throw(r.Failure('cancelled')));r.supervise([("+repr(sleeper)+","+repr(str(Path(d)/'out'))+",None)],time.monotonic()+60)"
            process = subprocess.Popen([sys.executable, '-B', '-c', code], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            try:
                end = time.monotonic()+3
                while not pidfile.exists() and time.monotonic()<end:
                    time.sleep(.02)
                self.assertTrue(pidfile.exists())
                process.send_signal(signal.SIGTERM)
                self.assertNotEqual(process.wait(timeout=3), 0)
                self.assert_stopped(pidfile)
            finally:
                if process.poll() is None:
                    process.kill();process.wait()


if __name__ == '__main__':
    unittest.main()
