#!/usr/bin/env python3
"""Bound final Muse campaigns to reviewed inputs; never enable capture or release.

This orchestrates source-pinned assertion checkers. It does not decode Muse,
replace shared Analysis, authenticate imported claims, or authorize native runs.
Use only an independently reviewed packet and disposable isolated configuration.
"""

import argparse
import hashlib
import json
import math
import os
import pathlib
import platform
import re
import sys
import time

import full_race
import muse_qualification as probe

VERSION = 1
MAX_JSON = 8 * 1024 * 1024
MAX_CASES = 1024
MAX_SECONDS = 1800
BUDGETS = {'pass_read_bytes': 256 << 20, 'pass_cache_reserve_bytes': 96 << 20,
           'session_read_bytes': 160 << 20}
HEX = re.compile(r"[0-9a-f]{64}\Z")
HARNESS_HELPERS = ('muse_final_acceptance.py', 'muse_qualification.py', 'full_race.py',
                   'run_go_shard.py', 'scope_native_verify.py')
BUDGET_PATHS = ('internal/agentapi/read_budget.go', 'internal/collector/collector.go',
                'internal/archive/adapters.go', 'internal/archive/source_jsonl.go')
ROOTS = ('home', 'archive', 'config', 'data', 'cache', 'state', 'tmp', 'storage')
FACTS = {
    'privacy.nested_sentinels': {'remote_secret_sentinels': 0, 'remote_reasoning_dumps': 0},
    'privacy.exclusion_dependencies': {'excluded_references_published': 0},
    'storage.frozen_retry_source_gone': {'native_source_removed': True, 'frozen_bytes_preserved': True,
                                        'destination_preserved': True, 'ownership_preserved': True,
                                        'new_native_bytes_substituted': False},
    'storage.protected_stages': {'pending_stages_deleted': 0, 'dangling_acknowledgements': 0},
    'storage.failure_boundaries': {when + '_' + stage: True
                                   for when in ('before', 'after')
                                   for stage in ('local_state', 'staging', 'source_put', 'metadata_put',
                                                 'verification', 'ack', 'cleanup')},
    'storage.root_expiry': {'expired_root_objects': 0, 'dangling_links': 0},
    'storage.child_expiry_links': {'expired_child_objects': 0, 'dangling_links': 0,
                                   'unresolved_links_preserved': True},
    'import.exact_undo_live_overlap': {'later_live_ownership_preserved': True,
                                      'acknowledged_data_preserved': True, 'native_files_preserved': True,
                                      'undo_restart_verified': True},
    'import.readonly_pre_setup': {'capture_state_mutations': 0},
    'resource.cold_inventory': {'larger_than_pass_budget': True, 'complete_scoped_census': True,
                                 'selected_id_currentness': True, 'valid_beside_retry_progress': True},
    'resource.four_agent_pressure': {'codex': True, 'cursor': True, 'claude': True, 'muse': True,
                                      'ordinary_session_headroom': True},
}
PUBLICATION = ('source_put', 'metadata_put', 'checksum', 'readback', 'ack', 'status')
METRICS = ('operations', 'alloc_bytes', 'disk_read_bytes', 'record_visits',
           'elapsed_seconds', 'peak_rss_bytes', 'ledger_peak_bytes', 'cache_peak_bytes')


class Failure(ValueError):
    """The campaign cannot certify its requested evidence."""


def catalog():
    """Canonical union, independent of the submitted packet's selected cases."""
    result = {}

    def add(name, kind='deterministic', scope='full'):
        result[name] = {'kind': kind, 'scope': scope, 'assertions': [name], 'facts': FACTS.get(name, {})}

    for name in ('privacy.nested_sentinels', 'privacy.exclusion_dependencies',
                 'admission.no_mutation_before_proof', 'admission.revocation_races',
                 'storage.frozen_retry_source_gone', 'storage.protected_stages',
                 'storage.failure_boundaries', 'storage.root_expiry',
                 'compat.current_reader_writer', 'compat.old_reader', 'compat.old_writer',
                 'compat.producer_upgrade', 'compat.future_format'):
        add(name, scope='root')
    for name in ('children.independent_authority', 'children.graph_conflicts',
                 'children.late_parent_links', 'children.own_totals', 'storage.child_expiry_links',
                 'import.exact_undo_live_overlap', 'import.readonly_pre_setup',
                 'reader.text_json_html_eval', 'skills.installed_eligible_invoked_read',
                 'handoff.bidirectional_safe_argv', 'replay.exact_forwarding',
                 'docs.schemas_capabilities_versions'):
        add(name)
    for workload in ('1', '128', '512', 'full_raw_limit', 'full_record_limit',
                     'full_source_limit', 'cold_inventory', 'warm_unchanged',
                     'child_graph', 'four_agent_pressure'):
        add('resource.' + workload, scope='root' if workload != 'child_graph' else 'full')
    for role in ('root', 'direct', 'workflow', 'observer', 'grandchild'):
        add('publication.' + role, scope='root' if role == 'root' else 'full')
    # A cross-build is deliberately absent: it cannot stand in for native execution.
    for target in ('darwin-amd64', 'darwin-arm64', 'linux-amd64', 'linux-arm64'):
        for mode in ('tui', 'exec', 'serve'):
            for action in ('fresh', 'second_turn', 'known_resume', 'unknown_resume',
                           'failure', 'orderly_exit', 'sigint', 'sigterm', 'hard_kill_late_write',
                           'fork', 'rewind', 'side', 'recovery', 'compaction_manual',
                           'compaction_default', 'compaction_periodic_resume',
                           'logging_disabled', 'ephemeral'):
                add('native.' + target + '.' + mode + '.' + action, 'native', 'root')
    add('provider.real_usage_accounting', 'provider', 'root')
    add('provider.disposable_storage_readback', 'provider', 'root')
    for name in ('test', 'levenshtein', 'extended', 'combined_review', 'postmerge'):
        add('candidate.' + name, 'candidate', 'root')
    return dict(sorted(result.items()))


def sha(data):
    return hashlib.sha256(data).hexdigest()


def encode(value):
    return (json.dumps(value, sort_keys=True, indent=2, allow_nan=False) + '\n').encode()


def unique(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise Failure('duplicate JSON key: ' + key)
        result[key] = value
    return result


def load(path):
    data = probe.read_limited(pathlib.Path(path), MAX_JSON)
    return json.loads(data, object_pairs_hook=unique,
                      parse_constant=lambda value: (_ for _ in ()).throw(Failure('nonfinite JSON')))


def require(condition, message):
    if not condition:
        raise Failure(message)


def regular(path):
    path = pathlib.Path(path)
    require(path.is_absolute() and not path.is_symlink() and path.is_file(), 'expected absolute regular file')
    require(path.resolve() == path, 'file path traverses symlink or dot components')
    return path


def pin_file(pin):
    require(isinstance(pin, dict) and isinstance(pin.get('sha256'), str) and HEX.fullmatch(pin['sha256']),
            'invalid file pin')
    path = regular(pin['path'])
    require(probe.file_digest(path) == pin['sha256'], 'file hash differs: ' + str(path))
    return path


def source_proof(root, expected):
    proof = full_race.source_proof(pathlib.Path(root))
    require(proof == expected, 'combined source/tree differs')
    return proof


def packet_check(packet, repository):
    require(type(packet.get('version')) is int and packet['version'] == VERSION, 'unsupported packet version')
    require(set(packet['source']) == {'source', 'tree'}, 'source must pin commit and tree')
    source_proof(repository, packet['source'])
    require(packet.get('scope') in ('root', 'full'), 'scope must be root or full')
    require(packet.get('catalog_sha256') == sha(encode(catalog())), 'canonical catalog differs')
    require(packet.get('budget_files') and packet.get('schema_files'), 'pin current budget and schema inputs')
    require(packet.get('budget_limits') == BUDGETS, 'current pass/cache/session caps must not increase')
    require(set(packet.get('configuration', {}).get('roots', {})) == set(ROOTS), 'pin all isolated roots')
    require(packet['configuration']['roots'] == {name: name for name in ROOTS}, 'roots must be owned case-relative directories')
    require(packet['configuration'].get('files'), 'pin isolated configuration bytes')
    for pin in packet['configuration']['files']:
        pin_file(pin)
    require({str(pathlib.Path(repository).resolve() / path) for path in BUDGET_PATHS} <=
            {pin.get('path') for pin in packet['budget_files']}, 'authoritative budget input omitted')
    for pin in packet['budget_files'] + packet['schema_files']:
        path = pin_file(pin)
        require(path.is_relative_to(pathlib.Path(repository).resolve()), 'policy input outside combined source')
    require(set(packet.get('binaries', {})) >= {'archive', 'producer', 'old_reader', 'old_writer'},
            'pin archive, producer and actual old reader/writer binaries')
    for pin in packet['binaries'].values():
        pin_file(pin)
        require(set(pin.get('build_source', {})) == {'source', 'tree'} and pin.get('version'),
                'binary lacks build source/tree/version provenance')
    require(packet['binaries']['archive']['build_source'] == packet['source'], 'archive binary source differs')
    for role in ('old_reader', 'old_writer'):
        require(packet['binaries'][role]['sha256'] != packet['binaries']['archive']['sha256'] and
                packet['binaries'][role]['build_source'] != packet['source'], 'old binary must be an actual distinct prior build')
    require(isinstance(packet.get('helpers'), dict) and packet['helpers'], 'pin checker helper closure')
    for pin in packet['helpers'].values():
        pin_file(pin)
    require({str(pathlib.Path(__file__).resolve().parent / name) for name in HARNESS_HELPERS} <=
            {pin['path'] for pin in packet['helpers'].values()}, 'harness helper closure omitted')
    cases = packet.get('cases')
    require(isinstance(cases, list) and 0 < len(cases) <= MAX_CASES, 'invalid case count')
    seen, total = set(), 0
    for case in cases:
        name = case['id']
        require(name in catalog() and name not in seen, 'unknown or duplicate case')
        seen.add(name)
        require(case.get('kind') == catalog()[name]['kind'], 'synthetic/cross-build cannot replace native/provider evidence')
        seconds = case.get('seconds')
        require(type(seconds) in (int, float) and math.isfinite(seconds) and 0 < seconds <= 120,
                'case deadline must be bounded at 120 seconds')
        total += seconds
        argv = case.get('argv')
        require(isinstance(argv, list) and argv and all(isinstance(a, str) and '\x00' not in a for a in argv),
                'invalid exact argv')
        require(argv[0] in [p['path'] for p in packet['helpers'].values()], 'executable must be a pinned helper')
        require(case.get('assertions') == catalog()[name]['assertions'], 'canonical assertion union differs')
        require(all(isinstance(a, str) and a for a in case['assertions']), 'invalid assertions')
    require(total <= MAX_SECONDS, 'campaign exceeds shared deadline')
    require(packet.get('configuration', {}).get('isolated') is True, 'isolated configuration required')
    require(packet['configuration'].get('provider_authorized') is True or
            all(c['kind'] != 'provider' for c in cases), 'provider campaign needs separate explicit authorization')
    return packet


def artifact(root, item):
    require(isinstance(item, dict) and set(item) == {'path', 'sha256'}, 'invalid artifact reference')
    relative = pathlib.PurePosixPath(item['path'])
    require(not relative.is_absolute() and relative.parts and all(p not in ('.', '..') for p in relative.parts),
            'artifact escapes case directory')
    path = regular(pathlib.Path(root) / relative)
    require(path.is_relative_to(pathlib.Path(root).resolve()), 'artifact outside case directory')
    require(isinstance(item['sha256'], str) and HEX.fullmatch(item['sha256']) and
            probe.file_digest(path) == item['sha256'], 'artifact checksum differs')
    return path


def observations(case, value, directory, packet):
    """Validate checker protocol, never treat exit zero as acceptance."""
    require(type(value.get('version')) is int and value['version'] == VERSION and value.get('case') == case['id'] and
            value.get('kind') == case['kind'] and value.get('source') == packet['source'],
            'checker identity/source/kind differs')
    require(value.get('assertions') == {a: 'PASS' for a in case['assertions']}, 'missing, skipped or failed assertion')
    require(value.get('facts', {}) == catalog()[case['id']]['facts'], 'required lifecycle/privacy/resource facts differ')
    artifacts = value.get('artifacts')
    require(isinstance(artifacts, list) and artifacts, 'checker must retain independently inspectable artifacts')
    paths = [artifact(directory, item) for item in artifacts]
    require(len(paths) == len(set(paths)), 'duplicate artifacts')
    if case['kind'] == 'native':
        require(value.get('execution') == 'native', 'cross-build/synthetic is not native execution')
        require(value.get('target') == case['id'].split('.')[1], 'native target differs')
        require(value.get('machine') == platform.node() and value.get('platform') == platform.system().lower() + '-' +
                {'x86_64': 'amd64', 'aarch64': 'arm64'}.get(platform.machine(), platform.machine()), 'native machine differs')
        require(value['target'] == value['platform'], 'native execution on another platform')
    if case['id'].startswith('publication.'):
        entities = value.get('entities')
        require(isinstance(entities, list) and entities, 'publication lacks independently checked entities')
        require(len({e['archive_id'] for e in entities}) == len(entities), 'duplicate archive entity')
        for entity in entities:
            require(isinstance(entity.get('archive_id'), str) and entity['archive_id'], 'empty archive entity')
            require(entity.get('role') == case['id'].split('.')[1], 'publication role differs')
            require(set(entity.get('stages', {})) == set(PUBLICATION), 'publication lacks PUT/checksum/readback/ack/status')
            stages = entity['stages']
            for name in ('source_put', 'metadata_put'):
                require(stages[name].get('key'), 'publication object key missing')
                require(stages[name].get('body') in artifacts, 'PUT body lacks hashed evidence')
            require(stages['source_put']['key'] != stages['metadata_put']['key'], 'source/metadata object keys collide')
            for name, put in (('source', 'source_put'), ('metadata', 'metadata_put')):
                body = stages[put]['body']
                readback = stages['readback'].get(name)
                require(readback in artifacts and readback['sha256'] == body['sha256'] and
                        stages['checksum'].get(name + '_sha256') == body['sha256'], 'PUT/checksum/readback disagree')
            for name in ('ack', 'status'):
                require(stages[name] in artifacts, 'ack/status lacks hashed evidence')
    if case['id'].startswith('compat.old_'):
        role = case['id'].split('.')[1]
        require(value.get('binary_sha256') == packet['binaries'][role]['sha256'], 'actual old binary differs')
        require(value.get('schemas') == packet['schema_files'], 'compatibility schema inputs differ')
    if case['id'].startswith('resource.'):
        measurements = value.get('measurements', {})
        require(set(measurements) >= set(METRICS), 'resource measurement missing')
        require(all(type(measurements[k]) in (int, float) and math.isfinite(measurements[k]) and
                    measurements[k] >= 0 for k in METRICS), 'invalid resource measurement')
        require(value.get('machine') == platform.node() and value.get('budget_files') == packet['budget_files'],
                'resource machine/current budget evidence differs')
        require(value.get('full_scale') is True, 'short or reduced resource case cannot pass')
        require(measurements['ledger_peak_bytes'] <= BUDGETS['pass_read_bytes'] and
                measurements['cache_peak_bytes'] <= BUDGETS['pass_cache_reserve_bytes'], 'resource caps exceeded')
        workload = case['id'].split('.')[1]
        if workload in ('1', '128', '512'):
            require(value.get('sessions') == int(workload), 'resource cohort reduced')
        require(value.get('limits_reached') is True if workload.startswith('full_') else True,
                'full-limit boundary not exercised')
        if case['id'] == 'resource.warm_unchanged':
            require(value.get('unchanged') == {'full_filter': 0, 'metadata_put': 0, 'store_walk': 0},
                    'unchanged tick performed full work')
        if case['id'] == 'resource.child_graph':
            require(value.get('parent_analyses') == 1 and value.get('full_parent_reads') == 1,
                    'children reread parent')
    return True


def execute(packet, repository, output):
    """Run a reviewed finite packet, retaining all failed receipts and streams."""
    packet_check(packet, repository)
    output = pathlib.Path(output).absolute()
    require(not output.exists() and output.parent.resolve() == output.parent, 'output must be new under a resolved parent')
    output.mkdir(mode=0o700)
    (output / 'packet.json').write_bytes(encode(packet))
    report = {'version': VERSION, 'packet_sha256': sha(encode(packet)), 'source': packet['source'],
              'scope': packet['scope'], 'cases': {}, 'release_authorized': False, 'capture_unlock_authorized': False}
    start = time.monotonic()
    try:
        with probe.termination_cleanup():
            for index, case in enumerate(packet['cases']):
                directory = output / ('case-%04d' % index)
                directory.mkdir(mode=0o700)
                env = probe.isolated_env(directory)
                (directory / 'storage').mkdir(mode=0o700)
                env.pop('META_API_KEY', None)
                env['MUSE_FINAL_CASE'] = case['id']
                env['MUSE_FINAL_SOURCE'] = json.dumps(packet['source'], sort_keys=True)
                item_roots = {name: str(directory / name) for name in ROOTS}
                argv = [a.replace('{case_root}', str(directory)) for a in case['argv']]
                item = {'status': 'FAIL', 'directory': directory.name, 'argv': argv, 'roots': item_roots}
                report['cases'][case['id']] = item
                try:
                    packet_check(packet, repository)
                    item['pre_source'] = source_proof(repository, packet['source'])
                    require(time.monotonic() - start + case['seconds'] <= MAX_SECONDS, 'campaign deadline exhausted')
                    result = probe.run_process(argv, env=env, cwd=directory, timeout=case['seconds'],
                                               group_limit=True, evidence=directory)
                    require(not result['timeout'] and result['exit'] == 0, 'checker failed or timed out')
                    receipt = load(directory / 'process.json')
                    require(receipt['cleanup'].get('leader_reaped') is True and
                            receipt['cleanup'].get('group_absent') is True and not receipt['cleanup'].get('inspection_errors'),
                            'cleanup incomplete')
                    observations(case, load(directory / 'stdout.bin'), directory, packet)
                    packet_check(packet, repository)
                    item['post_source'] = source_proof(repository, packet['source'])
                    item['status'] = 'PROTOCOL_VERIFIED'
                except Exception as error:  # Preserve failure and continue independent cases.
                    item['error'] = type(error).__name__ + ': ' + str(error)
                finally:
                    item['files'] = {name: probe.file_digest(directory / name) for name in
                                     ('stdout.bin', 'stderr.bin', 'process.json') if (directory / name).is_file()}
                    (output / 'report.json').write_bytes(encode(report))
    finally:
        report['duration_seconds'] = time.monotonic() - start
        report['protocol_matrix'] = matrix(packet, report)
        report['protocol_complete'] = all(s in ('PROTOCOL_VERIFIED', 'UNCLAIMED') for s in report['protocol_matrix'].values())
        report['matrix'] = qualification_matrix(report['protocol_matrix'])
        report['qualification_complete'] = False
        (output / 'report.json').write_bytes(encode(report))
    return report


def matrix(packet, report):
    result = {}
    for name, requirement in catalog().items():
        if packet['scope'] == 'root' and requirement['scope'] != 'root':
            result[name] = 'UNCLAIMED'
        else:
            result[name] = report['cases'].get(name, {}).get('status', 'UNVERIFIED')
    return result


def qualification_matrix(protocol):
    # Only independent product acceptance can promote capability qualification.
    return {name: ('UNVERIFIED' if status == 'PROTOCOL_VERIFIED' else status)
            for name, status in protocol.items()}


def verify(packet, repository, output):
    """Recompute evidence; a caller-edited summary never supplies a verdict."""
    packet_check(packet, repository)
    output = pathlib.Path(output).resolve()
    require(load(output / 'packet.json') == packet, 'recorded packet differs')
    report = load(output / 'report.json')
    require(report.get('packet_sha256') == sha(encode(packet)) and report.get('source') == packet['source'], 'report packet/source differs')
    require(set(report['cases']) <= {c['id'] for c in packet['cases']}, 'unexpected report case')
    statuses = {}
    for index, case in enumerate(packet['cases']):
        item = report['cases'].get(case['id'], {})
        statuses[case['id']] = {'status': 'UNVERIFIED'}
        if item.get('status') != 'PROTOCOL_VERIFIED':
            statuses[case['id']]['status'] = 'FAIL' if item.get('status') == 'FAIL' else 'UNVERIFIED'
            continue
        directory = output / ('case-%04d' % index)
        require(item.get('directory') == directory.name, 'case directory differs')
        require(item.get('roots') == {name: str(directory / name) for name in ROOTS}, 'isolated roots differ')
        require(item.get('pre_source') == packet['source'] == item.get('post_source'), 'case source proof differs')
        argv = [a.replace('{case_root}', str(directory)) for a in case['argv']]
        receipt = load(directory / 'process.json')
        require(receipt.get('argv') == argv == item.get('argv') and receipt.get('exit') == 0 and
                receipt.get('timeout') is False and receipt.get('finished') is True, 'process terminal differs')
        require(receipt['cleanup'].get('leader_reaped') is True and receipt['cleanup'].get('group_absent') is True and
                not receipt['cleanup'].get('inspection_errors'), 'process cleanup unverified')
        for name in ('stdout.bin', 'stderr.bin', 'process.json'):
            require(probe.file_digest(regular(directory / name)) == item.get('files', {}).get(name), 'process evidence changed')
        for name in ('stdout', 'stderr'):
            require(probe.file_digest(directory / (name + '.bin')) == receipt.get(name + '_sha256'), 'stream differs')
        observations(case, load(directory / 'stdout.bin'), directory, packet)
        statuses[case['id']]['status'] = 'PROTOCOL_VERIFIED'
    result = matrix(packet, {'cases': statuses})
    return {'version': VERSION, 'source': packet['source'], 'protocol_matrix': result, 'matrix': qualification_matrix(result),
            'protocol_complete': all(s in ('PROTOCOL_VERIFIED', 'UNCLAIMED') for s in result.values()),
            'qualification_complete': False,
            'release_authorized': False, 'capture_unlock_authorized': False,
            'trust': 'source-pinned checker protocol; requires independent checker and raw-artifact review'}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action', choices=('catalog', 'run', 'verify'))
    parser.add_argument('--packet', type=pathlib.Path)
    parser.add_argument('--repository', type=pathlib.Path)
    parser.add_argument('--output', type=pathlib.Path)
    args = parser.parse_args()
    try:
        if args.action == 'catalog':
            value = {'version': VERSION, 'catalog_sha256': sha(encode(catalog())), 'cases': catalog()}
        else:
            require(args.packet and args.repository and args.output, 'packet, repository and output are required')
            value = (execute if args.action == 'run' else verify)(load(args.packet), args.repository, args.output)
        sys.stdout.buffer.write(encode(value))
        return 0 if args.action == 'catalog' or value.get('protocol_complete') is True else 1
    except (Failure, ValueError, OSError, KeyError, TypeError, full_race.shards.Failure) as error:
        print('Muse final acceptance refused: ' + str(error), file=sys.stderr)
        return 1


if __name__ == '__main__':
    sys.exit(main())
