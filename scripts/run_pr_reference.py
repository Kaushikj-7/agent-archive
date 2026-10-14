#!/usr/bin/env python3
"""Manual, source-bound unrestricted race-short reference and shard comparison.

This diagnostic qualification never replaces the required PR verdict or adds a
recurring campaign. It uses no test-name selector and no scheduling hints.
"""
import argparse
from collections import Counter
import hashlib
import json
import os
from pathlib import Path
import re
import time

import full_race
import generate_go_shard_hints as hints
import run_ci_tier as tier
import run_go_shard as shards

ROOT = Path(__file__).resolve().parent.parent
COMMAND = ['go','test','-race','-short','-count=1','-json','-timeout','20m','-p','1','./...']
LIST_COMMAND = ['go','test','-race','-short','-count=1','-json','-p','1','-list','^(Test|Fuzz|Example)','./...']
LOGS = ('packages.json','listing.json','race.json')


def sha(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def bodies(raw, packages, names):
    shards.coverage(raw,packages)
    actual = tier.body_evidence(raw,short=True)
    tops = Counter((row['package'],row['test']) for row in actual['actual'] if '/' not in row['test'])
    expected = Counter((package,name) for package,ns in names.items() for name in ns)
    if tops != expected:
        raise shards.Failure('unrestricted reference differs from compiled parent inventory')
    lifecycles = {}
    for e in shards.json_stream(raw):
        if e.get('Test'):
            if e['Test'].split('/')[0] not in names.get(e.get('Package'),[]):
                raise shards.Failure('unlisted unrestricted reference body')
            if e.get('Action') != 'output':
                lifecycles.setdefault((e['Package'],e['Test']),[]).append(e['Action'])
    if any(actions not in (['run','pass'],['run','skip'],['run','pause','cont','pass'],['run','pause','cont','skip'])
           for actions in lifecycles.values()):
        raise shards.Failure('invalid unrestricted reference body lifecycle')
    tier.verify_owned_audit_skip(raw)
    return actual


def run(source,out,seconds=1800):
    if os.environ.get('GITHUB_EVENT_NAME') != 'workflow_dispatch' or os.environ.get('CI_SUITE') != 'pr-reference':
        raise shards.Failure('reference execution requires explicit manual pr-reference selection')
    if not re.fullmatch('[0-9a-f]{40}',source):
        raise shards.Failure('reference source must be an immutable full SHA')
    out.mkdir(parents=True,exist_ok=True)
    path=out/'reference.json';path.unlink(missing_ok=True)
    report={'schema':1,'success':False,'source':source,'command':COMMAND,'list_command':LIST_COMMAND,
            'workflow_source':os.environ.get('GITHUB_SHA'),'settings':{},
            'policy':{'race':True,'short':True,'count':1,'package_p':1,'performance':False,
                      'real_systemd':False,'plain_audit_owned':True},'phases':{}}
    started=time.monotonic();deadline=started+seconds
    env=dict(os.environ,GITHUB_SHA=source,AGENT_ARCHIVE_REAL_SYSTEMD='0',AGENT_ARCHIVE_PLAIN_SOURCE_AUDIT_OWNED='1')
    env.pop('AGENT_ARCHIVE_PERF',None)
    try:
        proof=full_race.source_proof(ROOT)
        if proof['source'] != source:
            raise shards.Failure('reference checkout differs from immutable selected source')
        context=tier.plain_vet_context()
        report['source_proof']=proof
        report['settings']={'plain_vet_receipt_schema':1,'plain_vet_source_proof':proof,
                            'plain_vet_context':context,'plain_vet_context_sha256':shards.digest(context)}
        for command,filename,label in [(['go','list','-race','-json','./...'],'packages.json','package_discovery_seconds'),
                                       (LIST_COMMAND,'listing.json','compiled_listing_seconds'),
                                       (COMMAND,'race.json','unrestricted_invocation_seconds')]:
            report['phases'][label]=shards.supervise([(command,out/filename,env)],deadline)[0]
        packages=shards.metadata((out/'packages.json').read_text())
        names=full_race.listing((out/'listing.json').read_text(),packages)
        actual=bodies((out/'race.json').read_text(),packages,names)
        if full_race.source_proof(ROOT) != proof or tier.plain_vet_context() != context:
            raise shards.Failure('reference source/compiler context changed during bodies')
        report.update(packages=packages,names=names,body_evidence=actual)
        report['plain_vet']=shards.execute_worker_vet(out,deadline,report['settings'])
        report['files']={filename:sha(out/filename) for filename in LOGS}
        report['success']=True
    except BaseException as error:
        report['error']={'type':type(error).__name__,'message':str(error)}
        raise
    finally:
        report['phases']['reference_total_seconds']=time.monotonic()-started
        path.write_text(json.dumps(report,indent=2,sort_keys=True)+'\n')


def verify(directory,expected):
    directory=Path(directory);report=json.loads((directory/'reference.json').read_text())
    if (type(report.get('schema')) is not int or report['schema'] != 1 or report.get('success') is not True
            or report.get('error') is not None or report.get('source') != expected['source']
            or report.get('source_proof') != expected or report.get('command') != COMMAND
            or report.get('list_command') != LIST_COMMAND
            or report.get('policy') != {'race':True,'short':True,'count':1,'package_p':1,
                                      'performance':False,'real_systemd':False,'plain_audit_owned':True}
            or report.get('files') != {name:sha(directory/name) for name in LOGS}):
        raise shards.Failure('reference source/command/raw-evidence receipt mismatch')
    packages=shards.metadata((directory/'packages.json').read_text())
    names=full_race.listing((directory/'listing.json').read_text(),packages)
    actual=bodies((directory/'race.json').read_text(),packages,names)
    if report.get('packages') != packages or report.get('names') != names or report.get('body_evidence') != actual:
        raise shards.Failure('reference actual compiled discovery/body mismatch')
    if report.get('settings',{}).get('plain_vet_source_proof') != expected:
        raise shards.Failure('reference own-vet source mismatch')
    shards.verify_worker_vet(directory,report,expected)
    return report


def compare(reference,workers,audit):
    report=json.loads((Path(reference)/'reference.json').read_text())
    # Resolve expected source/tree and body policy from immutable Git objects,
    # rather than trusting a workflow ref or a mutable artifact's claimed tree.
    expected,catalog_raw=hints.measured_source(report['source'])
    with hints.contract_parser(report['source'],json.loads(catalog_raw)):
        reference_report=verify(reference,expected)
        paths=sorted(Path(workers).glob('**/manifest.json'))
        count=shards.worker_count(len(paths))
        shards.reconcile(workers,count,audit,expected_source_proof=expected,require_plain_vet=True)
        manifests=[json.loads(path.read_text()) for path in paths]
        catalog=manifests[0]['catalog']
        # Light packages are unrestricted in the actual worker stream, so their
        # observed top-level terminals supply the independently compared set.
        actual=[]
        for manifest in manifests:
            actual.extend(manifest['body_evidence']['actual'])
            if tier.plain_vet_identity(manifest['settings']['plain_vet_context']) != tier.plain_vet_identity(reference_report['settings']['plain_vet_context']):
                raise shards.Failure('reference/shard compiler context differs')
        tops=Counter((row['package'],row['test']) for row in actual if '/' not in row['test'])
        expected_tops=Counter((package,name) for package,ns in reference_report['names'].items() for name in ns)
        if catalog['packages'] != reference_report['packages'] or tops != expected_tops:
            raise shards.Failure('shard parent/package union differs from same-source unrestricted reference')
        def skips(records):
            return Counter((row['package'],row['test']) for row in records if row['status']=='skip')
        observed_skips=skips(actual);reference_skips=skips(reference_report['body_evidence']['actual'])
        if observed_skips != reference_skips:
            raise shards.Failure('unexpected/missing short or platform skip compared with unrestricted reference')
        return {'schema':1,'success':True,'source_proof':expected,'workers':count,
                'packages':len(catalog['packages']),'parents':sum(tops.values()),
                'matching_short_platform_skips':[{'package':p,'test':t,'count':n} for (p,t),n in sorted(observed_skips.items())],
                'note':'Actual same-source unrestricted parent/package union and skip identities match; plain audit is independently verified. Full-scale restorations and fuzz mutation campaigns remain separate.'}


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    sub=parser.add_subparsers(dest='action',required=True)
    execute=sub.add_parser('run');execute.add_argument('--source',required=True);execute.add_argument('--output',type=Path,required=True)
    check=sub.add_parser('compare');check.add_argument('--reference',type=Path,required=True);check.add_argument('--workers',type=Path,required=True);check.add_argument('--audit',type=Path,required=True);check.add_argument('--output',type=Path,required=True)
    args=parser.parse_args()
    try:
        if args.action=='run':run(args.source,args.output)
        else:args.output.write_text(json.dumps(compare(args.reference,args.workers,args.audit),indent=2,sort_keys=True)+'\n')
    except (shards.Failure,tier.Failure,ValueError,KeyError,TypeError,OSError) as error:
        parser.exit(1,'PR reference qualification failed: '+str(error)+'\n')


if __name__=='__main__':main()
