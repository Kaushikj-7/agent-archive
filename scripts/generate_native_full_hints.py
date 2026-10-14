#!/usr/bin/env python3
"""Import one successful historical Darwin full artifact as advisory costs."""
import argparse
import ast
import inspect
import re
import stat
from types import SimpleNamespace
import hashlib
import json
from pathlib import Path
import subprocess
import zipfile

import full_race as full
import generate_go_shard_hints as durations
import run_ci_tier as tier


# The historical two-bin LPT implementation and this reviewed generic helper
# have identical default-two weights, tie breaks and exhaustive/nonempty bins.
# Reject any unreviewed helper pair instead of guessing historical semantics.
PARTITION_COMPATIBILITY = (
    '17b43d85469c16593937d1cb489bef1e452dff545af986fe3aaacdb30377d52a',
    'e12c462f7276e49f40283885f00371cac79f2853643d2cd45bd88e7455de8baf',
)


def definition(module, name, kind):
    nodes = [n for n in module.body if isinstance(n, kind) and
             (n.name == name if isinstance(n, ast.FunctionDef)
              else any(isinstance(t, ast.Name) and t.id == name for t in n.targets))]
    if len(nodes) != 1:
        raise full.shards.Failure('historical scheduling definition missing or ambiguous')
    return nodes[0]


def ast_structure(value):
    # Python 3.12 adds empty type_params to ordinary function definitions.
    # Ignore only that representation change; generic definitions are unsupported.
    if isinstance(value, ast.AST):
        fields = []
        for key, item in ast.iter_fields(value):
            if key == 'type_params':
                if item != []:
                    raise full.shards.Failure('generic historical definitions unsupported')
                continue
            fields.append([key, ast_structure(item)])
        return [type(value).__name__, fields]
    if isinstance(value, list):
        return [ast_structure(item) for item in value]
    return value


def ast_hash(node):
    encoded = json.dumps(ast_structure(node), separators=(',', ':')).encode()
    return hashlib.sha256(encoded).hexdigest()


def historical_schedule(original):
    """Reuse only source-bound pure scheduling definitions, never module actions."""
    module = ast.parse(original('scripts/full_race.py'))
    helpers = ast.parse(original('scripts/run_go_shard.py'))
    cli = definition(module, 'CLI', ast.Assign)
    common = definition(module, 'COMMON', ast.Assign)
    literal_cli = definition(helpers, 'CLI', ast.Assign)
    if (any(len(n.targets) != 1 or not isinstance(n.targets[0], ast.Name) for n in (cli,common,literal_cli))
            or ast.dump(cli.value) != ast.dump(ast.parse('shards.CLI', mode='eval').body)
            or ast.literal_eval(literal_cli.value) != full.CLI
            or ast.literal_eval(common.value) != full.COMMON):
        raise full.shards.Failure('historical scheduling constants differ')
    old_partition = definition(helpers, 'partition', ast.FunctionDef)
    old_selector = definition(helpers, 'selector', ast.FunctionDef)
    path = Path(full.shards.__file__)
    if not stat.S_ISREG(path.lstat().st_mode):
        raise full.shards.Failure('current scheduling helpers are not regular source')
    current_partition = ast.parse(inspect.getsource(full.shards.partition)).body[0]
    current_selector = ast.parse(inspect.getsource(full.shards.selector)).body[0]
    if ((ast_hash(old_partition), ast_hash(current_partition)) != PARTITION_COMPATIBILITY
            or ast_structure(old_selector) != ast_structure(current_selector)):
        raise full.shards.Failure('unsupported historical/current scheduling helper contract')
    schedule = definition(module, 'schedule', ast.FunctionDef)
    if (schedule.decorator_list or schedule.returns is not None
            or schedule.args.defaults or schedule.args.kw_defaults or schedule.args.kwonlyargs
            or schedule.args.posonlyargs or schedule.args.vararg or schedule.args.kwarg
            or [a.arg for a in schedule.args.args] != ['package_map','names','patterns','hints']
            or any(a.annotation is not None for a in schedule.args.args)):
        raise full.shards.Failure('historical schedule signature unsupported')
    allowed = {ast.FunctionDef, ast.arguments, ast.arg, ast.Assign, ast.Expr, ast.If, ast.Return,
               ast.List, ast.Tuple, ast.Dict, ast.Name, ast.Load, ast.Store, ast.Constant, ast.Attribute,
               ast.Call, ast.keyword, ast.Starred, ast.Subscript, ast.ListComp, ast.DictComp,
               ast.GeneratorExp, ast.comprehension, ast.Compare, ast.GtE, ast.NotEq, ast.NotIn,
               ast.BoolOp, ast.Or, ast.BinOp, ast.Add, ast.Sub, ast.IfExp, ast.In}
    local = {n.id for n in ast.walk(schedule) if isinstance(n, ast.Name) and isinstance(n.ctx, ast.Store)}
    available = local | {'package_map','names','patterns','hints','CLI','COMMON','shards','sorted','set','len'}
    attributes = {('names','get'),('names','items'),('commands','append'),('assignments','append'),
                  ('shards','partition'),('shards','selector')}
    for node in ast.walk(schedule):
        if type(node) not in allowed or isinstance(node, ast.Name) and node.id not in available:
            raise full.shards.Failure('historical schedule contains unsupported operations')
        if isinstance(node, ast.Attribute):
            union = (node.attr == 'union' and isinstance(node.value, ast.Call)
                     and isinstance(node.value.func, ast.Name) and node.value.func.id == 'set'
                     and not node.value.args and not node.value.keywords)
            if not union and (not isinstance(node.value, ast.Name) or (node.value.id,node.attr) not in attributes):
                raise full.shards.Failure('historical schedule attribute unsupported')
        if isinstance(node, ast.Call) and (not isinstance(node.func, (ast.Name, ast.Attribute))
                or isinstance(node.func, ast.Name) and node.func.id not in {'set','len','sorted'}):
            raise full.shards.Failure('historical schedule callable unsupported')
        if (isinstance(node, ast.Assign) and any(not isinstance(t, ast.Name) for t in node.targets)
                or isinstance(node, ast.Name) and isinstance(node.ctx, ast.Store)
                and node.id in {'CLI','COMMON','shards','sorted','set','len'}):
            raise full.shards.Failure('historical schedule mutates its contract')
    namespace = {'__builtins__': {'set':set,'len':len,'sorted':sorted},
                 'CLI':full.CLI,'COMMON':list(full.COMMON),
                 'shards':SimpleNamespace(partition=full.shards.partition, selector=full.shards.selector)}
    exec(compile(ast.Module(body=[schedule], type_ignores=[]), '<historical-pure-schedule>', 'exec'), namespace)
    return namespace['schedule']


def generate(archive, expected_sha256, source, root):
    archive = Path(archive)
    if hashlib.sha256(archive.read_bytes()).hexdigest() != expected_sha256:
        raise full.shards.Failure('historical native artifact fingerprint differs')
    def git(*args):
        return subprocess.check_output(['git','-C',str(root),*args], timeout=20)
    if not re.fullmatch('[0-9a-f]{40}', source) or git('rev-parse','--verify',source+'^{commit}').decode().strip() != source:
        raise full.shards.Failure('historical source must resolve to exact commit')
    source_proof = {'source':source, 'tree':git('rev-parse',source+'^{tree}').decode().strip()}
    def original(path):
        records = git('ls-tree','-z',source,'--',path).split(b'\0')
        if len(records) != 2 or records[-1] or b'\t' not in records[0]:
            raise full.shards.Failure('historical input unavailable or ambiguous')
        metadata, actual_path = records[0].split(b'\t')
        mode, kind, oid = metadata.split()
        if mode not in (b'100644',b'100755') or kind != b'blob' or actual_path.decode() != path:
            raise full.shards.Failure('historical input is not a regular Git blob')
        return git('cat-file','blob',oid.decode())
    catalog_bytes = original('scripts/ci_tiers.json')
    catalog = json.loads(catalog_bytes)
    with zipfile.ZipFile(archive) as bundle:
        files = {name:bundle.read(name) for name in bundle.namelist()}
    report = json.loads(files['tier.json'])
    schedule = report['full_schedule']
    if (report.get('success') is not True or report.get('source') != source
            or report.get('platform') != 'Darwin' or report.get('tier') != 'full'
            or report.get('suite') != 'macos' or schedule['source_proof'] != source_proof
            or report.get('full_schedule_sha256') != full.shards.digest(schedule)):
        raise full.shards.Failure('historical native report not successful matched full Darwin')
    packages = full.packages(files['full-packages.json'].decode())
    names = full.listing(files['full-list.jsonl'].decode(), packages)
    old_hints = json.loads(original('scripts/go_shard_hints.json'))['cli']
    expected = historical_schedule(original)(packages,names,['./...'],old_hints)
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
