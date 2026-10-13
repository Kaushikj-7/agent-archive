#!/usr/bin/env python3
"""Dedicated dispatch adapter. Install alongside current production helpers.
Render immutable approval literals only after root review; never run PR-only main.
"""
import argparse
from dataclasses import asdict
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import time

import go_cache_context as context
import go_cache_steps as steps

# Root replaces these literals in the dedicated adapter before freezing M.
APPROVED = {
    'ref': 'refs/heads/codex/ci-followup-cache-measure-20261013',
    'workflow': 'Representative cache measurement',
    'workflow_ref': 'wangjohn/agent-archive/.github/workflows/extended.yml@refs/heads/codex/ci-followup-cache-measure-20261013',
    'repository': 'wangjohn/agent-archive', 'batch': 'followup-20261013-0349',
    'runtime_digest': 'd168293d5d7b6482e65dc8b895fa26bab0cd774d534546bb8465bb98c5a3c359', 'runtime_source': '5854e2d1e38aa1c6a5d095adf0107f16d051a2ed',
}
# M can differ from reviewed S only at these two tracked files.
ADDITIONS = {'.github/workflows/extended.yml', 'scripts/measure_go_cache.py'}


# Read-only provenance of actual retained prior M1 worker evidence.
BASELINE = {'batch': 'followup-20261013-0349', 'ref': 'refs/heads/codex/ci-followup-cache-measure-20261013', 'measurement_sha': 'ba2957fdee5720c09ea1d9d2ddee26ad46a308a6', 'tree': '9b1c2d3b6a3bca3bd97cc5e8a73d695deec81ee0', 'context': '5dfeb260783627780c42d9b1deca5a1626dbec05ab869d4e280139fbc83f8ea8', 'dependency': '8d5d5416b378e560a654c806456c4e91f914b205655ced87cd80e94596d83183', 'catalog_sha256': '4d7ef9c41176db6c7aea55b72dc66e3920d69e866028a0f743ff3c82c22f67a5', 'catalog_membership_sha256': 'bfd81e5f50646bce1d96f5ce52ba196b8818625ec6138a9ed70fc31c8ee70d9e'}
# Added tests read from immutable integrated source; no test body is copied/executed.
REQUIRED_NEW_TESTS = {'github.com/wangjohn/agent-archive/internal/backfill': ['TestStagedBatchReconcileRetainsResultOwnerAcrossCopy', 'TestStagedUndoRefusesBeforeChangingAuthority'], 'github.com/wangjohn/agent-archive/internal/collector': ['TestStagedGroupUsesOnePrecommitReadbackTriplet', 'TestStagedLegacyResultCloseIsNoop', 'TestStagedMaterializationFitsOneValidatedStageView', 'TestStagedParentFirstRestartContinuesChildAfterNativeDeletion', 'TestStagedRunDiagnosticOwnerSurvivesAliasesFailuresAndRepeatedPasses', 'TestStagedRunFreshGroupAdmissionRespectsCapturedPressure', 'TestStagedRunFreshGroupAdmissionWithSufficientCapturedOwner', 'TestStagedRunScheduledReleaseRespectsCapturedPassBudget'], 'github.com/wangjohn/agent-archive/internal/config': ['TestStagedSelectionRawWriterFencePreservesOrdinary', 'TestStagedWriterFenceSurvivesActualSaveRollback'], 'github.com/wangjohn/agent-archive/internal/state': ['TestStagedAdmissionChildCannotEscapeCapturedPageBudget', 'TestStagedCancelAndForgetRefuseBeforeEffects', 'TestStagedChildPublishedAloneCannotReleaseRoster', 'TestStagedClosedProofCodecKeepsDigestAndCallerCancellation', 'TestStagedFactoryRejectsRedigestedMetadataIdentity', 'TestStagedFactoryRejectsRehashedNoncanonicalSource', 'TestStagedFactoryRejectsSameOwnerReplacementBundle', 'TestStagedFloorPreservesOrdinarySelection2', 'TestStagedGenericPendingRemovalRefusesCompletedProgress', 'TestStagedLexicalFactoryRechecksCoveredRequest', 'TestStagedLexicalSourceFactRefusesMutationAndKeepsFullDefault', 'TestStagedLexicalSourceFactStillReservesSecondScratch', 'TestStagedProofRejectsOversizeBeforeDecode', 'TestStagedReadablePendingFitsOneVerificationOwner', 'TestStagedReceiptOnlyPageBindsExactImportOwner', 'TestStagedReleaseRechecksPolicyAfterReadback', 'TestStagedReleaseRechecksTokenAfterVerifiedReadback', 'TestStagedReleaseRefusesCatalogBeforeAnyRead', 'TestStagedScheduledDiagnosticOwnerKeepsRawHoldThroughAliases', 'TestStagedScheduledPressureRefusesBeforeReleaseEffects', 'TestStagedSelectionExactReleaseAndRestart', 'TestStagedSelectionLegacyExactRemovalCannotBypassReceipt', 'TestStagedSelectionMandatoryCodecAndClosedAuthority', 'TestStagedSelectionReadbackAndNewerTokenRetainEvidence', 'TestStagedTypedDigestClassifiesOnlyLimitFailures', 'TestStagedTypedDigestPreservesCanonicalWire']}

def approval_guard(env, approved=APPROVED):
    if (env.get('GITHUB_EVENT_NAME') != 'workflow_dispatch'
            or env.get('GITHUB_SHA') != env.get('EXPECTED_SHA')
            or not context.SHA.fullmatch(env.get('EXPECTED_SHA', ''))
            or env.get('GITHUB_REF') != approved['ref']
            or env.get('GITHUB_WORKFLOW') != approved['workflow']
            or env.get('GITHUB_WORKFLOW_REF') != approved['workflow_ref']
            or env.get('GITHUB_WORKFLOW_SHA') != env.get('EXPECTED_SHA')
            or env.get('GITHUB_REPOSITORY') != approved['repository']
            or env.get('BATCH') != approved['batch']
            or env.get('BATCH') != BASELINE['batch']
            or env.get('GITHUB_REF') != BASELINE['ref']
            or not re.fullmatch(r'[a-z0-9]+(?:-[a-z0-9]+)*', approved['batch'])
            or env.get('MODE') not in ('prefix', 'warm')):
        raise context.InvalidContext('dispatch differs from frozen approval')


def runtime_digest(source, sha, git=context.scope.git):
    """Frame path/mode/blob identity of every runtime input; excludes only adapter/workflow."""
    h = hashlib.sha256()
    for record in context.scope.records(git(source, 'ls-tree', '-r', '-z', sha)):
        metadata, path = record.split(b'\t', 1)
        if context.scope.repository_path(path) in ADDITIONS:
            continue
        for field in (metadata, path):
            h.update(len(field).to_bytes(8, 'big')); h.update(field)
    return h.hexdigest()


def measurement_plan(worker, actual_context, source, runner_temp, batch):
    if not re.fullmatch(r'[a-z0-9]+(?:-[a-z0-9]+)*', batch):
        raise context.InvalidContext('unsafe measurement batch')
    # Shared production key primitive returns the production CachePlan dataclass.
    return context.owned_plan('aa-ci-measure-v1-'+batch, worker, actual_context, source, runner_temp)


def classification(mode, state):
    required = {'prefix': 'prefix', 'warm': 'exact'}
    if mode not in required or state != required[mode]:
        raise context.InvalidContext('spent allocation is not an approved mode observation')
    return state


def save_allowed(mode, state, successful):
    return successful is True and mode == 'prefix' and state == 'prefix'


def status_gate(needs):
    if set(needs) != {'worker0', 'worker1'} or any(
            not isinstance(n, dict) or n.get('result') != 'success' for n in needs.values()):
        raise context.InvalidContext('both actual workers must succeed')


def new_test_proof(directory, manifest, worker):
    """Require fresh JSON RUN then PASS for integrated tests in assigned packages.

    Current production reconcile remains the overall body/catalog authority.
    Catalog membership can be unchanged: ordinary catalogs enumerate packages,
    not ordinary test names. Never claim a new CLI name when none was added.
    """
    catalog = manifest['catalog']
    if (catalog['source'] == BASELINE['measurement_sha']
            or manifest['catalog_sha256'] != context.digest(catalog)
            or worker not in (0, 1)):
        raise context.InvalidContext('catalog is not current new-source evidence')
    assignments = catalog['assignments']
    if any(package not in catalog['packages']
           or sum(package in assignment['packages'] for assignment in assignments) != 1
           for package in REQUIRED_NEW_TESTS):
        raise context.InvalidContext('new required packages missing or duplicated')
    selected = {(package, name) for package, names in REQUIRED_NEW_TESTS.items()
                if package in assignments[worker]['packages'] for name in names}
    required = {(package, name) for package, names in REQUIRED_NEW_TESTS.items() for name in names}
    actions = {key: [] for key in selected}
    for name in ('ordinary.json', 'git-identity.json', 'cli.json'):
        path = Path(directory)/name
        if not path.exists():
            continue
        for line in path.read_text().splitlines():
            try:
                event = json.loads(line)
            except (ValueError, TypeError):
                continue
            if not isinstance(event, dict):
                continue
            key = (event.get('Package'), event.get('Test'))
            if (key in required and key not in selected
                    and event.get('Action') in ('run', 'pass', 'fail', 'skip')):
                raise context.InvalidContext('new required test executed outside assigned worker package')
            if key in actions and event.get('Action') in ('run', 'pass', 'fail', 'skip'):
                actions[key].append(event['Action'])
    if any(events != ['run', 'pass'] for events in actions.values()):
        raise context.InvalidContext('new required test did not freshly RUN and PASS exactly once')
    return {'source': catalog['source'], 'old_source': BASELINE['measurement_sha'],
            'catalog_sha256': manifest['catalog_sha256'], 'old_catalog_sha256': BASELINE['catalog_sha256'],
            'catalog_membership_changed': context.digest({k:v for k,v in catalog.items() if k != 'source'}) != BASELINE['catalog_membership_sha256'],
            'required_tests': [{'package': package, 'test': name, 'actions': actions[(package,name)]}
                               for package, name in sorted(selected)]}


def stamp(record, label):
    record.setdefault('timings', []).append({'phase': label, 'unix_ns': time.time_ns(),
                                             'monotonic_ns': time.monotonic_ns()})


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('phase', choices=('paths', 'prepare', 'verify', 'start-worker', 'complete', 'report', 'verdict'))
    args = parser.parse_args()
    env = os.environ
    record = {}
    stamp(record, args.phase+'-start')
    state_file = Path(env['RUNNER_TEMP'])/'measurement-state.json'
    if args.phase == 'report':
        # Evidence must survive source/module/worker/measurement failures.
        started = record['timings']
        record = json.loads(state_file.read_text()) if state_file.exists() else {}
        record.setdefault('timings', []).extend(started)
        try:
            approval_guard(env)
            current = context.source_identity(Path(env['GITHUB_WORKSPACE']).resolve(), env['GITHUB_SHA'])
            record['report_source'] = current
        except (context.InvalidContext, KeyError, TypeError, ValueError, OSError, subprocess.SubprocessError):
            record['report_guard'] = 'failed'
        record['service'] = {k: env.get(k, '') for k in
                             ('RESTORE_OUTCOME', 'MATCHED_KEY', 'EXACT_HIT', 'SAVE_OUTCOME', 'WORKER_OUTCOME')}
        record['logical_cache_bytes'] = {p: sum(f.stat().st_size for f in Path(p).rglob('*')
                                               if f.is_file() and not f.is_symlink())
                                         for p in (env.get('GOCACHE', '/nonexistent'), env.get('GOMODCACHE', '/nonexistent'))}
        stamp(record, 'report-end')
        (Path(env['RUNNER_TEMP'])/'measurement-evidence.json').write_text(json.dumps(record, sort_keys=True))
        return 0
    try:
        approval_guard(env)
        if args.phase == 'paths':
            # Pure owned-path validation/cleanup before setup-go or ANY Go tool.
            if env.get('SHARD') not in ('0', '1'):
                raise context.InvalidContext('invalid worker')
            paths = context.owned_paths(env['RUNNER_TEMP'])
            if (env.get('GOCACHE'), env.get('GOMODCACHE')) != paths:
                raise context.InvalidContext('worker paths differ')
            context.prepare_owned_paths(paths, clear=True)
            record['pre_setup_paths'] = {'paths': list(paths), 'sha': env['GITHUB_SHA'], 'worker': env['SHARD']}
            stamp(record, 'paths-end')
            state_file.write_text(json.dumps(record, sort_keys=True))
            return 0
        source = Path(env['GITHUB_WORKSPACE']).resolve()
        actual = context.source_identity(source, env['GITHUB_SHA'])
        stamp(record, 'source-identity-end')
        if runtime_digest(source, env['GITHUB_SHA']) != APPROVED['runtime_digest']:
            raise context.InvalidContext('runtime inputs differ from reviewed S')
        stamp(record, 'runtime-proof-end')
        if args.phase == 'verdict':
            status_gate(json.loads(env['NEEDS']))
            # Actual source/body/tier reconciliation remains authoritative.
            subprocess.run(['python3', '-B', 'scripts/run_go_shard.py', '--reconcile',
                            env['EVIDENCE_DIRECTORY']], check=True)
            proofs = [new_test_proof(path.parent, manifest, manifest['shard'])
                      for path in sorted(Path(env['EVIDENCE_DIRECTORY']).glob('**/manifest.json'))
                      for manifest in [json.loads(path.read_text())]]
            proven = {(entry['package'], entry['test']) for proof in proofs for entry in proof['required_tests']}
            expected = {(package, name) for package, names in REQUIRED_NEW_TESTS.items() for name in names}
            if len(proofs) != 2 or proven != expected:
                raise context.InvalidContext('integrated new test evidence is incomplete')
            (Path(env['RUNNER_TEMP'])/'measurement-verdict-evidence.json').write_text(json.dumps(proofs, sort_keys=True))
            print('Fresh integrated tests RUN/PASS:', len(proven), '; current catalog source:', env['GITHUB_SHA'])
            return 0
        if env.get('SHARD') not in ('0', '1'):
            raise context.InvalidContext('invalid worker')
        worker = int(env['SHARD'])
        prior = json.loads(state_file.read_text())
        if prior.get('pre_setup_paths') != {'paths': list(context.owned_paths(env['RUNNER_TEMP'])),
                                           'sha': env['GITHUB_SHA'], 'worker': env['SHARD']}:
            raise context.InvalidContext('pre-setup owned paths phase missing')
        runtime = context.runtime_context()
        stamp(record, 'runtime-context-end')
        plan = measurement_plan(worker, runtime, actual, env['RUNNER_TEMP'], env['BATCH'])
        if context.digest(runtime) != BASELINE['context'] or actual['dependency'] != BASELINE['dependency']:
            raise context.InvalidContext('actual context/dependencies no longer match old owned cache prefix')
        if (env.get('GOCACHE'), env.get('GOMODCACHE')) != plan.paths:
            raise context.InvalidContext('cache paths differ')
        identity = {'plan': json.loads(json.dumps(asdict(plan))), 'context': context.digest(runtime),
                    'mode': env['MODE'], 'approval': APPROVED}
        def guard(expected):
            approval_guard(env)
            if context.source_identity(source, env['GITHUB_SHA']) != expected:
                raise context.InvalidContext('source changed')
        if args.phase == 'prepare':
            initial = record['timings']
            record = json.loads(state_file.read_text())
            if record.get('pre_setup_paths') != {'paths': list(plan.paths), 'sha': env['GITHUB_SHA'], 'worker': env['SHARD']}:
                raise context.InvalidContext('pre-setup owned paths phase missing')
            record.setdefault('timings', []).extend(initial)
            record['runtime_binary_hashes'] = {key: runtime.get(key) for key in ('toolchain', 'cc', 'cxx')}
            context.prepare_paths(plan, clear=True)
            record['identity'] = identity
            record['event_payload_sha256'] = hashlib.sha256(Path(env['GITHUB_EVENT_PATH']).read_bytes()).hexdigest()
            record['actual'] = {k: env.get(k, '') for k in ('GITHUB_EVENT_NAME', 'GITHUB_SHA', 'GITHUB_REF',
                                'GITHUB_WORKFLOW', 'GITHUB_WORKFLOW_SHA', 'GITHUB_WORKFLOW_REF',
                                'GITHUB_RUN_ID', 'GITHUB_RUN_ATTEMPT', 'RUNNER_OS', 'RUNNER_ARCH',
                                'ImageOS', 'ImageVersion')}
            record['cpu'] = {'affinity': sorted(os.sched_getaffinity(0)),
                             'model': Path('/proc/cpuinfo').read_text(),
                             'quota': Path('/sys/fs/cgroup/cpu.max').read_text() if Path('/sys/fs/cgroup/cpu.max').exists() else 'unavailable'}
            context.outputs(plan)  # same safe output encoder as production
            steps.emit(context.outputs(plan), env['GITHUB_OUTPUT'])
        else:
            initial = record['timings']
            record = json.loads(state_file.read_text())
            record.setdefault('timings', []).extend(initial)
            if record.get('identity') != identity:
                raise context.InvalidContext('source/context differs from prepare')
            if args.phase == 'verify':
                outcome, matched, exact = env['RESTORE_OUTCOME'], env.get('MATCHED_KEY', ''), env.get('EXACT_HIT', '')
                # Retain production cleanup/download/verify/fatal-source behavior for all service states.
                def measured_command(argv):
                    label = 'module-'+argv[-1]
                    stamp(record, label+'-start')
                    try:
                        return steps.execute_modules(argv)
                    finally:
                        stamp(record, label+'-end')
                state = steps.verify_restored(plan, outcome, matched, exact, guard, command=measured_command)
                record['restore_state'] = state
                record['observation'] = classification(env['MODE'], state)
                if env['MODE'] == 'prefix' and matched != plan.prefix+BASELINE['tree']:
                    raise context.InvalidContext('prefix did not restore actual approved old M1 cache')
                record['matched_key'] = matched
                steps.emit({'restore_state': state}, env['GITHUB_OUTPUT'])
            elif args.phase == 'complete':
                classification(env['MODE'], record['restore_state'])
                if record.get('failed_phase'):
                    raise context.InvalidContext('prior phase failed')
                manifest = json.loads((source/'shard-results/manifest.json').read_text())
                steps.worker_success(plan, manifest, guard, worker)
                record['new_test_proof'] = new_test_proof(source/'shard-results', manifest, worker)
                allowed = save_allowed(env['MODE'], record['restore_state'], True)
                steps.emit({'save_ready': 'true' if allowed else 'false'}, env['GITHUB_OUTPUT'])
        stamp(record, args.phase+'-end')
        state_file.write_text(json.dumps(record, sort_keys=True))
        return 0
    except (context.InvalidContext, KeyError, TypeError, ValueError, OSError, subprocess.SubprocessError):
        # Preserve failed allocation phase even if mode classification/module verification fails.
        if record:
            # Source/runtime guards can fail before the normal state reload.
            # Preserve already-spent preparation evidence on that early path.
            try:
                retained = json.loads(state_file.read_text())
            except (OSError, TypeError, ValueError):
                retained = {}
            if retained and record.get('timings', [])[:1] != retained.get('timings', [])[:1]:
                record = {**retained, **record, 'timings': retained.get('timings', []) + record.get('timings', [])}
            record['failed_phase'] = args.phase
            stamp(record, args.phase+'-failed')
            state_file.write_text(json.dumps(record, sort_keys=True))
        print('Measurement source/context/mode/worker contract failed; retain spent allocation', file=sys.stderr)
        return 1


if __name__ == '__main__':
    sys.exit(main())
