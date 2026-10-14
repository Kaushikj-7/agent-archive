"""Synthetic controls for optional same-source unrestricted coverage comparison."""
import copy
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest import mock

import run_pr_reference as ref
import run_ci_tier as tier
import run_go_shard as shards
from test_go_parent_shards import SOURCE, catalog, evidence, body, rows, event, listing
from test_go_shards import synthetic_worker_receipt


def snapshot(root,c):
    root.mkdir()
    names={package:([*c['cli']] if package==shards.CLI else c['parents'].get(package,['TestOrdinary'] if tested else [])) for package,tested in c['packages'].items()}
    (root/'packages.json').write_text(rows([dict(ImportPath=p,Dir='/tmp',TestGoFiles=['test.go'] if tested else []) for p,tested in c['packages'].items()]))
    listings=[];streams=[]
    for package,ns in names.items():
        if c['packages'][package]:
            listings.append(listing(package,ns));streams.append(body(package,ns))
        else:
            raw=rows([event(package,'output',Output='? package [no test files]\n'),event(package,'skip')])
            listings.append(raw);streams.append(raw)
    (root/'listing.json').write_text('\n'.join(listings));(root/'race.json').write_text('\n'.join(streams))
    m={'schema':1,'success':True,'source':SOURCE['source'],'source_proof':SOURCE,
       'command':copy.deepcopy(ref.COMMAND),'list_command':copy.deepcopy(ref.LIST_COMMAND),
       'policy':{'race':True,'short':True,'count':1,'package_p':1,'performance':False,
                 'real_systemd':False,'plain_audit_owned':True},
       'packages':c['packages'],'names':names,'settings':{'plain_vet_source_proof':SOURCE}}
    synthetic_worker_receipt(root,m)
    m['body_evidence']=tier.body_evidence((root/'race.json').read_text(),True)
    m['files']={n:ref.sha(root/n) for n in ref.LOGS}
    (root/'reference.json').write_text(json.dumps(m))
    return m


class ReferenceTests(unittest.TestCase):
    def test_execution_requires_explicit_manual_immutable_source(self):
        with tempfile.TemporaryDirectory() as d,mock.patch.object(shards,'supervise') as commands:
            for env,source in [({'GITHUB_EVENT_NAME':'push','CI_SUITE':'pr-reference'},SOURCE['source']),
                               ({'GITHUB_EVENT_NAME':'workflow_dispatch','CI_SUITE':'all'},SOURCE['source']),
                               ({'GITHUB_EVENT_NAME':'workflow_dispatch','CI_SUITE':'pr-reference'},'main')]:
                with mock.patch.dict(os.environ,env,clear=True),self.assertRaises(shards.Failure):ref.run(source,Path(d))
            commands.assert_not_called()
        self.assertNotIn('-run',ref.COMMAND);self.assertNotIn('-skip',ref.COMMAND)
        for flag in ('-race','-short','-count=1','-json'):self.assertIn(flag,ref.COMMAND)
        self.assertEqual(ref.COMMAND[ref.COMMAND.index('-timeout')+1],'20m')
        self.assertEqual(ref.COMMAND[-1],'./...')

    def test_actual_reference_receipt_source_flags_raw_and_lifecycle_tampering_fail(self):
        c=catalog(4)
        with tempfile.TemporaryDirectory() as d,mock.patch.object(tier,'verify_owned_audit_skip') as audit:
            root=Path(d)/'reference';good=snapshot(root,c);ref.verify(root,SOURCE)
            self.assertTrue(audit.called)
            for change in [lambda m:m.update(success=False),lambda m:m.update(schema=True),
                           lambda m:m['command'].extend(['-run','TestOnly']),
                           lambda m:m['source_proof'].update(tree='c'*40),
                           lambda m:m.update(body_evidence={})]:
                bad=copy.deepcopy(good);change(bad);(root/'reference.json').write_text(json.dumps(bad))
                with self.assertRaises(shards.Failure):ref.verify(root,SOURCE)
            (root/'reference.json').write_text(json.dumps(good))
            raw=(root/'race.json').read_text();(root/'race.json').write_text(raw+'\n')
            with self.assertRaises(shards.Failure):ref.verify(root,SOURCE)
            events=shards.json_stream(raw)
            for bad in ([e for e in events if not (e.get('Action')=='pass' and '/' in e.get('Test',''))],
                        [e for e in events if not (e.get('Action')=='run' and e.get('Test'))]):
                with self.assertRaises((shards.Failure,tier.Failure)):ref.bodies(rows(bad),good['packages'],good['names'])

    def test_comparison_requires_exact_union_compiler_and_skip_identities(self):
        c=catalog(8)
        with tempfile.TemporaryDirectory() as d,mock.patch.object(tier,'verify_owned_audit_skip'),mock.patch.object(tier,'verify_source_audit',return_value={'source_proof':SOURCE}),mock.patch.object(ref.hints,'measured_source',return_value=(SOURCE,json.dumps(tier.catalog()))):
            root=Path(d);workers=root/'workers';workers.mkdir();evidence(workers,c)
            for path in workers.glob('*/manifest.json'):
                m=json.loads(path.read_text());m['settings'].update(plain_vet_owned=True,plain_vet_source_proof=SOURCE)
                synthetic_worker_receipt(path.parent,m,image='unit-'+str(m['shard']));path.write_text(json.dumps(m))
            reference=root/'reference';snapshot(reference,c)
            result=ref.compare(reference,workers,root/'audit')
            self.assertTrue(result['success']);self.assertEqual(result['workers'],8)
            self.assertEqual(result['parents'], len(c['cli']) + sum(map(len,c['parents'].values())) + 1)
            # A newly skipped body cannot be hidden by successful parent/package terminals.
            path=next(workers.glob('*/parents-*.json'));events=shards.json_stream(path.read_text())
            target=next(e for e in events if e.get('Action')=='pass' and '/' in e.get('Test',''))
            target['Action']='skip';path.write_text(rows(events))
            manifest=path.parent/'manifest.json';m=json.loads(manifest.read_text())
            import go_parent_shards
            raw=go_parent_shards.verify_logs(path.parent,c,c['assignments'][m['shard']])
            m['body_evidence']=tier.body_evidence(raw,True);manifest.write_text(json.dumps(m))
            with self.assertRaisesRegex(shards.Failure,'skip'):ref.compare(reference,workers,root/'audit')

    def test_manual_workflow_is_exclusive_and_registered_test_is_required(self):
        from test_ci_workflow import jobs,job_key,condition_matches
        base=Path(__file__).resolve().parent.parent
        extended=(base/'.github/workflows/extended.yml').read_text();job=jobs(extended)['pr-reference']
        condition=job_key(job,'if')
        self.assertTrue(condition_matches(condition,'workflow_dispatch',suite='pr-reference'))
        for event,suite in [('pull_request','pr-reference'),('schedule','pr-reference'),('push','pr-reference'),('workflow_dispatch','all')]:
            self.assertFalse(condition_matches(condition,event,suite=suite))
        self.assertIn('ref: ${{ inputs.reference_source }}',job)
        self.assertIn('retention-days: 7',job)
        self.assertIn('persist-credentials: false',job)
        self.assertIn('cache: false',job)
        self.assertIn('python3 -B scripts/test_pr_reference.py',jobs((base/'.github/workflows/test.yml').read_text())['script-static'])


if __name__=='__main__':unittest.main()
