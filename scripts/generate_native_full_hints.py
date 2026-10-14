#!/usr/bin/env python3
"""Import one successful historical Darwin full artifact as advisory costs."""
import argparse
import hashlib
import json
from pathlib import Path
import subprocess
import zipfile

import full_race as full
import generate_go_shard_hints as durations
import run_ci_tier as tier


def generate(archive, expected_sha256, source, root):
    archive = Path(archive)
    if hashlib.sha256(archive.read_bytes()).hexdigest() != expected_sha256:
        raise full.shards.Failure('historical native artifact fingerprint differs')
    def original(path):
        return subprocess.check_output(['git','-C',str(root),'show',source+':'+path], timeout=20)
    catalog_bytes = original('scripts/ci_tiers.json')
    catalog = json.loads(catalog_bytes)
    with zipfile.ZipFile(archive) as bundle:
        files = {name:bundle.read(name) for name in bundle.namelist()}
    report = json.loads(files['tier.json'])
    schedule = report['full_schedule']
    if (report.get('success') is not True or report.get('source') != source
            or report.get('platform') != 'Darwin' or report.get('tier') != 'full'
            or report.get('suite') != 'macos' or schedule['source_proof']['source'] != source
            or report.get('full_schedule_sha256') != full.shards.digest(schedule)):
        raise full.shards.Failure('historical native report not successful matched full Darwin')
    packages = full.packages(files['full-packages.json'].decode())
    names = full.listing(files['full-list.jsonl'].decode(), packages)
    old_hints = json.loads(original('scripts/go_shard_hints.json'))['cli']
    expected = full.schedule(packages,names,['./...'],old_hints)
    expected.update(source_proof=schedule['source_proof'], discovery=[
        {'command':['go','list','-json','./...'],'log':'full-packages.json'},
        {'command':full.LIST_FLAGS+['./...'],'log':'full-list.jsonl'}])
    if schedule != expected or report.get('full_discovery') != [dict(d,completed=True) for d in expected['discovery']]:
        raise full.shards.Failure('historical full assignment/discovery mismatch')
    package_costs, cli_costs, actual = {}, {}, []
    race = [i for i in report['invocations'] if i['log'].startswith('race')]
    if len(race) != len(schedule['commands']):
        raise full.shards.Failure('historical native race invocation missing')
    with durations.contract_parser(source,catalog):
        for index,(record,command,assignment) in enumerate(zip(race,schedule['commands'],schedule['assignments'])):
            if (record['command'] != command or record['log'] != f'race-{index}.jsonl'
                    or record.get('performance_enabled') is not False or record.get('real_systemd_enabled') is not False):
                raise full.shards.Failure('historical native command changed')
            text = files[record['log']].decode()
            evidence = full.coverage(text,{p:packages[p] for p in assignment},assignment,tier.body_evidence)
            if evidence != record['evidence']:
                raise full.shards.Failure('historical native raw bodies changed')
            actual.extend(evidence['actual'])
            records = durations.json_stream(text)
            for event in records:
                if not event.get('Test') and event.get('Action') in ('pass','skip') and event['Package'] != full.CLI:
                    package_costs[event['Package']] = durations.positive_ms(event.get('Elapsed',0))
            cli_records = [r for r in records if r.get('Package')==full.CLI]
            cli_costs.update({n:durations.positive_ms(v) for n,v in durations.cli_durations(cli_records,assignment.get(full.CLI,[])).items()})
        if len(actual) != len({(r['package'],r['test']) for r in actual}):
            raise full.shards.Failure('historical native duplicated body')
        required,children = {},{}
        for entry in catalog['restorations']:
            p=tier.MODULE+entry['package']
            if entry['test'] not in names.get(p,[]):
                raise full.shards.Failure('historical native restoration undiscovered')
            required.setdefault(p,[]).append(entry['test'])
            children[(p,entry['test'])]=entry.get('full_children',[])
        tier.verify_selected(actual,required,children)
    if set(package_costs) != set(packages)-{full.CLI} or set(cli_costs)!=set(names[full.CLI]):
        raise full.shards.Failure('historical native timing units incomplete')
    return {'schema':1,'packages':package_costs,'cli':cli_costs,'restorations':catalog['restorations'],
            'provenance': {'source':source,'source_proof':schedule['source_proof'],
                'artifact_sha256':expected_sha256,
                'files':{n:hashlib.sha256(b).hexdigest() for n,b in sorted(files.items())},
                'package_source_trees':{p:hashlib.sha256(subprocess.check_output(
                    ['git','-C',str(root),'ls-tree','-r',source,'--',p.removeprefix(tier.MODULE)],timeout=20)).hexdigest()
                    for p in packages},
                'source_files':{n:hashlib.sha256(original(n)).hexdigest() for n in
                    ('go.mod','go.sum','scripts/ci_tiers.json','scripts/full_race.py','scripts/run_ci_tier.py')},
                'settings':{'platform':'Darwin','race':True,'short':False,'count':1,
                    'commands':schedule['commands'],'worker_topology':'historical serial full commands'},
                'policy':'package terminal elapsed; CLI recursive logical body cost via generate_go_shard_hints; positive half-up ms; historical advisory only',
                'limitations':'One older source and serial hosted run. Current source/dependencies/scale and three-worker CPU bounds differ. These weights do not predict current wall time. Changed restoration catalog and unknown CLI bodies use positive unmeasured defaults.'}}


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--archive',required=True)
    parser.add_argument('--sha256',required=True)
    parser.add_argument('--source',required=True)
    parser.add_argument('--output',type=Path,required=True)
    args=parser.parse_args()
    value=generate(args.archive,args.sha256,args.source,tier.ROOT)
    args.output.write_text(json.dumps(value,sort_keys=True,indent=2)+'\n')


if __name__=='__main__':
    main()
