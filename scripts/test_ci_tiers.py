"""Behavior tests for cadence, exact selection, source fallback and truthful evidence."""
import contextlib
import copy
import io
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest import mock

import run_ci_tier as tier
import test_purge_recipe as purge


def events(*items):
    return '\n'.join(json.dumps(i) for i in items)


class PolicyTest(unittest.TestCase):
    def test_schedule_deduplicates_weekly_and_full_overrides_calendar(self):
        config = tier.catalog()
        self.assertEqual(config['daily_cron'], '17 8 * * 1-6')
        self.assertEqual(config['weekly_cron'], '17 8 * * 0')
        self.assertEqual(tier.resolve('schedule', schedule=config['daily_cron']), 'daily')
        self.assertEqual(tier.resolve('schedule', schedule=config['weekly_cron']), 'full')
        self.assertEqual(tier.resolve('pull_request'), 'pr')
        self.assertEqual(tier.resolve('push', 'refs/tags/v0.2.0'), 'full')
        with self.assertRaises(tier.Failure):
            tier.resolve('pull_request', 'refs/tags/v0.2.0')
        for ref in ('refs/tags/v0.2.0-rc1', 'refs/tags/v0.2', 'refs/tags/0.2.0', 'refs/tags/v0.2.0/extra'):
            with self.assertRaises(tier.Failure):
                tier.resolve('push', ref)
        for suite in ('all','linux','macos','fuzz','systemd'):
            self.assertEqual(tier.resolve('workflow_dispatch', schedule=config['daily_cron'], suite=suite), 'full')
        self.assertEqual(tier.resolve('push','refs/heads/release-candidate/final',config['daily_cron']), 'full')
        for event,ref,schedule,suite in [('schedule','','bad','all'),('push','refs/heads/main','','all'),('workflow_dispatch','','','bad')]:
            with self.assertRaises(tier.Failure):
                tier.resolve(event,ref,schedule,suite)

    def test_daily_exact_restorations_include_physical_boundary_without_weekly_scale(self):
        selected = {(e['package'],e['test']) for e in tier.catalog()['restorations'] if e['restore']=='daily'}
        self.assertEqual(selected, {('internal/cli','TestRealCLICatalogDefaultList50Bounded'),('internal/cli','TestRealCLICatalogListNonMultiplePageLimit'),('internal/reader','TestRemoteRootChildrenReusesOnlyCompleteSameRootSQL'),('internal/agents/codex','TestRelatedMillionRecordLimit')})
        physical = next(e for e in tier.catalog()['restorations'] if e['test']=='TestRelatedMillionRecordLimit')
        self.assertEqual(physical['counterpart'], 'TestRelatedCachedRecordCountLimitIsTypedAndReleasesBudget')
        boundary = {e['test'] for e in tier.catalog()['restorations'] if e['package']=='internal/backfill'}
        self.assertIn('TestImportStagingAccountsForEncodedTextAndRefilterScratch',boundary)

    def test_explicit_pr_routes_keep_sensitive_bodies_and_defer_scale(self):
        for path, packages, native, systemd in (
                ('internal/cli/list.go', './internal/cli/...', True, False),
                ('internal/catalog/limits.go', './internal/catalog/...', True, False),
                ('internal/archive/history.go', './internal/archive/...', True, False),
                ('internal/config/consent.go', './internal/config/...', True, False),
                ('internal/scheduler/systemd/manage.go', './internal/scheduler/...', True, True)):
            result = tier.extra_for_paths([path])
            self.assertTrue(result['ordinary_coverage']); self.assertTrue(result['regressions'] or systemd or native)
            self.assertEqual(result['native'], native)
            self.assertEqual(result['systemd'], systemd)
            self.assertFalse(result['fuzz']); self.assertFalse(result['purge'])
            self.assertNotEqual(result['packages'], ['./...'])
        for path in ('internal/newfamily/x.go', 'internal/testutil/helper.go', 'go.mod',
                     'scripts/run_ci_tier.py', '.github/workflows/test.yml'):
            result = tier.extra_for_paths([path])
            self.assertTrue(result['regressions']); self.assertEqual(result['ordinary_coverage'], 'required-linux-short-shards')
            self.assertFalse(any(result[k] for k in ('systemd', 'fuzz', 'purge')))
        for path in ('docs/security/archive-cleanup.md', 'unknown.md', 'CHANGELOG.md', 'CONTRIBUTING.md'):
            self.assertEqual(tier.extra_for_paths([path])['packages'], [])
        self.assertEqual(tier.extra_for_paths([])['packages'], [])

    def test_history_uncertainty_requires_complete_representative_coverage(self):
        with mock.patch.object(tier,'git',side_effect=OSError('history unavailable')):
            result=tier.pr_extras({'pull_request':{'base':{'sha':'a'*40},'head':{'sha':'b'*40}}},'c'*40)
        self.assertTrue(result['regressions'])
        self.assertEqual(result['reason'], 'representative fallback: source proof unavailable')
        self.assertFalse(result['fuzz']); self.assertFalse(result['purge'])

    def test_bad_catalog_fails_instead_of_empty_selection(self):
        with tempfile.TemporaryDirectory() as directory:
            root=Path(directory);(root/'scripts').mkdir();(root/'scripts/ci_tiers.json').write_text('{"schema":1,"restorations":[]}')
            with mock.patch.object(tier,'ROOT',root),self.assertRaises(tier.Failure):tier.catalog()


class EvidenceTest(unittest.TestCase):
    def test_dedicated_manager_allows_only_exact_unrepresentable_child_and_retains_skip(self):
        package=tier.MODULE+'internal/scheduler/systemd'
        parent='TestRealUserManagerConformance'
        child=parent+'/UnreadableAndAbsentDefinitions'
        reason='a real manager cannot be put in a loaded state over damaged unit files'
        text=events({'Action':'run','Package':package,'Test':child},
                    {'Action':'output','Package':package,'Test':child,'Output':'=== RUN   '+child+'\n'},
                    {'Action':'output','Package':package,'Test':child,'Output':'    conformance.go:119: '+reason+'\n'},
                    {'Action':'output','Package':package,'Test':child,'Output':'--- SKIP: '+child+' (0.00s)\n'},
                    {'Action':'skip','Package':package,'Test':child},
                    {'Action':'pass','Package':package,'Test':parent+'/Installed'},
                    {'Action':'pass','Package':package,'Test':parent})
        actual=tier.body_evidence(text,False)['actual']
        selected={package:[parent]}
        tier.verify_selected(actual,selected,real_manager=True)
        skipped=next(r for r in actual if r['test']==child)
        self.assertEqual(skipped['status'],'skip');self.assertIn(reason,skipped['reason'])
        with self.assertRaises(tier.Failure):tier.verify_selected(actual,selected)
        for changed in (dict(skipped,reason=skipped['reason'].replace(reason,'changed reason')),
                        dict(skipped,test=parent+'/Unexpected'),
                        dict(skipped,package=tier.MODULE+'internal/cli')):
            with self.subTest(change=changed),self.assertRaises(tier.Failure):
                tier.verify_selected([dict(r,**changed) if r['test']==child else r for r in actual],
                                     {changed['package']:[parent]} if changed['package']!=package else selected,
                                     real_manager=True)
        for broken in ([r for r in actual if r['test']!=parent],
                       [dict(r,status='skip') if r['test']==parent else r for r in actual],
                       actual+[{'package':package,'test':parent+'/NewSkip','status':'skip','reason':reason}]):
            with self.assertRaises(tier.Failure):tier.verify_selected(broken,selected,real_manager=True)
        with self.assertRaises(tier.Failure):
            tier.verify_selected(actual,selected,{(package,parent):['Missing']},real_manager=True)

    def test_nested_skip_and_scaled_pass_are_distinct_from_full_body(self):
        pkg=tier.MODULE+'internal/cli';name='TestRealCLICatalogDefaultList50Bounded'
        text=events({'Action':'run','Package':pkg,'Test':name},{'Action':'pass','Package':pkg,'Test':name+'/128'},{'Action':'pass','Package':pkg,'Test':name},{'Action':'skip','Package':pkg,'Test':'TestOther/child'})
        proof=tier.body_evidence(text,True)
        self.assertEqual(proof['full_restorations'][0]['full_body'],'NOT RUN')
        self.assertIn('skip',[r['status'] for r in proof['actual']])
        with self.assertRaises(tier.Failure):tier.verify_selected(proof['actual'],{pkg:[name]},{(pkg,name):['128','512']})

    def test_selected_skip_missing_run_or_duplicate_terminal_fails(self):
        pkg='p';name='TestA'
        with self.assertRaises(tier.Failure):tier.verify_selected([{'package':pkg,'test':name,'status':'skip'}],{pkg:[name]})
        for text in (events({'Package':pkg,'Test':name,'Action':'run'}),events(*[{'Package':pkg,'Test':name,'Action':'pass'}]*2),events({'Action':'fail','Package':pkg})):
            with self.assertRaises(tier.Failure):tier.terminals(text)

    def test_unknown_ordinary_name_remains_actual_and_catalog_does_not_filter_it(self):
        actual=tier.body_evidence(events({'Package':'p','Test':'TestBrandNew','Action':'pass'}),True)
        self.assertEqual(actual['actual'][0]['test'],'TestBrandNew')
        self.assertEqual(actual['full_restorations'],[])

    def test_discovery_rejects_failed_and_duplicate_listings(self):
        with tempfile.TemporaryDirectory() as d:
            out=Path(d)/'list'
            for text in (events({'Package':'p','Action':'fail'}),events(*[{'Package':'p','Action':'output','Output':'FuzzA\n'}]*2,{'Package':'p','Action':'pass'})):
                with mock.patch.object(tier,'checked',return_value=text),self.assertRaises(tier.Failure):tier.discover(['./p'],out,1)

    def test_empty_extra_plan_is_explicit_success_without_go_invocation(self):
        with tempfile.TemporaryDirectory() as d,mock.patch.object(tier,'git',return_value='a'*40),mock.patch.object(tier,'checked') as command:
            extra={'packages':[],'native':False,'systemd':False,'fuzz':False,'purge':False}
            with mock.patch.dict(os.environ,{'GITHUB_SHA':'a'*40}):tier.run_suite('full','extra',Path(d),extra)
            self.assertFalse(command.called)
            proof=json.loads((Path(d)/'tier.json').read_text());self.assertTrue(proof['success']);self.assertEqual(proof['invocations'],[])


class FocusedPRTest(unittest.TestCase):
    def test_source_bound_focused_body_verdict_refuses_missing_and_weakened_evidence(self):
        package = tier.MODULE+'internal/catalog'
        name = 'TestMissingCorruptAndOversizedNodesFailClosed'
        plan = dict(tier.representative_fallback(), source='a'*40, tier='pr',
                    packages=['./internal/catalog'], regressions={'internal/catalog':[name]})
        source = {'source':'a'*40, 'tree':'b'*40}
        def checked(command, output, deadline, env=None):
            self.assertEqual(env['AGENT_ARCHIVE_REAL_SYSTEMD'], '0')
            self.assertNotIn('AGENT_ARCHIVE_PERF', env)
            self.assertIn('-race', command)
            if '-list' in command:
                text=events(dict(Action='output',Package=package,Output=name+'\n'),dict(Action='pass',Package=package))
            else:
                self.assertIn('-short',command)
                text=events(dict(Action='run',Package=package,Test=name),dict(Action='pass',Package=package,Test=name),dict(Action='pass',Package=package))
            output.write_text(text); return text
        with tempfile.TemporaryDirectory() as d, mock.patch.object(tier,'git',return_value='a'*40), mock.patch.object(tier.full_race,'source_proof',return_value=source), mock.patch.object(tier,'checked',side_effect=checked), mock.patch.dict(os.environ,{'GITHUB_SHA':'a'*40,'GITHUB_EVENT_NAME':'pull_request','GITHUB_REF':'refs/pull/1/merge','CI_SCHEDULE':'','AGENT_ARCHIVE_PERF':'1'}):
            out=Path(d); tier.run_suite('pr','extra',out,plan)
            original=json.loads((out/'tier.json').read_text())
            self.assertEqual(original['tier'],'pr'); self.assertNotIn('full_schedule',original)
            tier.verify_full(out,plan)
            for mutate in (lambda r:r['invocations'].clear(),
                           lambda r:r['invocations'][0]['command'].remove('-short'),
                           lambda r:r['source_proof'].update(tree='c'*40),
                           lambda r:r['invocations'][0]['evidence']['actual'].clear(),
                           lambda r:r['extras'].update(ordinary_coverage='omitted')):
                changed=copy.deepcopy(original);mutate(changed);(out/'tier.json').write_text(json.dumps(changed))
                with self.assertRaises(tier.Failure):tier.verify_full(out,plan)
            # A truncated stream can retain every selected body PASS and the
            # same body evidence while losing the package's final verdict.
            (out/'tier.json').write_text(json.dumps(original))
            log = out/original['invocations'][0]['log']
            complete = log.read_text()
            log.write_text(events(dict(Action='run',Package=package,Test=name),
                                  dict(Action='pass',Package=package,Test=name)))
            self.assertEqual(tier.body_evidence(log.read_text(), True), original['invocations'][0]['evidence'])
            with self.assertRaises(tier.Failure):tier.verify_full(out,plan)
            log.write_text(complete)
            (out/'representative-list.jsonl').write_text(events(dict(Action='pass',Package=package)))
            with self.assertRaises(tier.Failure):tier.verify_full(out,plan)

    def test_cli_setup_deletion_and_rename_old_path_keep_manager_integration(self):
        for paths in (['internal/cli/setup_linux.go'], ['internal/cli/uninstall.go'],
                      ['internal/cli/setup.go','internal/cli/new_flow.go'],
                      ['internal/scheduler/old.go','internal/newfamily/new.go']):
            result=tier.extra_for_paths(paths)
            self.assertTrue(result['systemd'])
            self.assertFalse(result['fuzz']);self.assertFalse(result['purge'])
        result=tier.extra_for_paths(['internal/cli/list.go'])
        self.assertFalse(result['systemd'])
        self.assertEqual(set(result['regressions']), {'internal/cli'})

    def test_shared_setup_identity_dispatch_and_recovery_keep_manager_integration(self):
        for path in ('internal/cli/cli.go', 'internal/cli/install_paths.go',
                     'internal/cli/status.go', 'internal/setupjournal/recovery.go',
                     'internal/setupresources/resource.go'):
            with self.subTest(path=path):
                self.assertTrue(tier.extra_for_paths([path])['systemd'])
        self.assertFalse(tier.extra_for_paths(['internal/cli/list.go'])['systemd'])

    def test_pr_native_smoke_does_not_download_the_scheduled_published_writer(self):
        package = tier.MODULE+'internal/credentials'
        name = 'TestNativeSmoke'
        def checked(command, output, deadline, env=None):
            self.assertEqual(command, ['bash', 'scripts/test_macos_smoke.sh'])
            text = events(dict(Action='run', Package=package, Test=name),
                          dict(Action='pass', Package=package, Test=name),
                          dict(Action='pass', Package=package))
            output.write_text(text)
            return text
        with tempfile.TemporaryDirectory() as d, mock.patch.dict(os.environ, {'GITHUB_SHA': 'a'*40}), mock.patch.object(tier, 'git', return_value='a'*40), mock.patch.object(tier.full_race, 'source_proof', return_value={'source':'a'*40, 'tree':'b'*40}), mock.patch.object(tier, 'checked', side_effect=checked):
            tier.run_suite('pr', 'macos', Path(d), tier.representative_fallback())
            report = json.loads((Path(d)/'tier.json').read_text())
            self.assertTrue(report['success'])
            self.assertEqual([i['log'] for i in report['invocations']], ['native-smoke.jsonl'])

    def test_short_scale_counterparts_restore_explicit_weekly_children(self):
        entries={e['test']:e for e in tier.catalog()['restorations']}
        self.assertEqual(entries['TestPickerFirstPageWorkDoesNotScaleWithUnrelatedArchive']['full_children'],['10000','100000'])
        self.assertEqual(entries['TestPickerQualificationRealCLIPhases']['full_children'],['1400','10000','100000'])
        self.assertTrue(all(entries[n]['restore']=='weekly' for n in ('TestPickerFirstPageWorkDoesNotScaleWithUnrelatedArchive','TestPickerQualificationRealCLIPhases')))


class PerformanceCompileTest(unittest.TestCase):
    proof = {'source': 'a'*40, 'tree': 'b'*40}

    def test_compile_only_preparation_never_executes_binaries_and_cleans_before_return(self):
        with tempfile.TemporaryDirectory() as d:
            out = Path(d); marker = out/'executed'; directories = []
            env = {'AGENT_ARCHIVE_PERF': '1', 'AGENT_ARCHIVE_REAL_SYSTEMD': '0'}
            packages = ['./internal/collector', './internal/archive', './internal/agents/nativecodec', './internal/capture']
            self.assertEqual(tier.PERFORMANCE, ['go', 'test', '-v', '-json', '-p', '1', '-count=1', '-run', 'StayFast|FiveMegabyte|TestCursorOverlappingHooksRegisterOnce', *packages])
            def compile_only(command, output, deadline, actual_env):
                self.assertEqual(command[:6], ['go', 'test', '-c', '-p', '2', '-o'])
                self.assertEqual(command[7:], packages)
                self.assertEqual(deadline, 137); self.assertIs(actual_env, env)
                directory = Path(command[6]); directories.append(directory)
                self.assertNotEqual(directory.parent, tier.ROOT)
                for package in command[7:]:
                    binary = directory/(package.rsplit('/', 1)[1]+'.test')
                    binary.write_text('#!/bin/sh\ntouch '+str(marker)+'\n')
                    binary.chmod(0o700)
                output.write_text(''); return ''
            with mock.patch.object(tier.full_race, 'source_proof', return_value=self.proof), mock.patch.object(tier, 'checked', side_effect=compile_only):
                result = tier.prepare_performance(out, 137, env, self.proof)
            self.assertFalse(marker.exists())
            self.assertFalse(directories[0].exists())
            self.assertEqual(result, dict(source_proof=self.proof, packages=tier.PERFORMANCE[-4:], parallel=2, completed=True))

    def test_failure_missing_binary_and_changed_source_prevent_canonical_execution(self):
        for failure in ('compile', 'cancel', 'missing', 'symlink', 'source-before', 'source-after'):
            with self.subTest(failure=failure), tempfile.TemporaryDirectory() as d:
                out = Path(d); calls = []; directories = []
                changed = dict(self.proof, tree='c'*40)
                proofs = [changed] if failure == 'source-before' else [self.proof, changed if failure == 'source-after' else self.proof]
                def compile_only(command, output, deadline, env):
                    calls.append(command)
                    self.assertIn('-c', command)
                    directory = Path(command[command.index('-o')+1]); directories.append(directory)
                    for package in command[7:]:
                        if failure != 'missing' or package != command[-1]:
                            binary = directory/(package.rsplit('/', 1)[1]+'.test')
                            if failure == 'symlink' and package == command[-1]:
                                binary.symlink_to(directory/'collector.test')
                            else:
                                binary.write_text('not executable')
                    if failure in ('compile', 'cancel'):
                        raise tier.Failure('compile failed' if failure == 'compile' else 'tier command cancelled')
                    output.write_text(''); return ''
                def full_run(packages, directory, deadline, report, *args):
                    report['full_schedule'] = {'source_proof': self.proof}
                with mock.patch.object(tier, 'git', return_value='a'*40), mock.patch.object(tier.full_race, 'run', side_effect=full_run), mock.patch.object(tier.full_race, 'source_proof', side_effect=proofs), mock.patch.object(tier, 'checked', side_effect=compile_only), mock.patch.dict(os.environ, {'GITHUB_SHA': 'a'*40}), self.assertRaises(tier.Failure):
                    tier.run_suite('full', 'linux', out, seconds=37)
                self.assertTrue(all('-c' in command for command in calls))
                self.assertTrue(all(not directory.exists() for directory in directories))
                report = json.loads((out/'tier.json').read_text())
                self.assertFalse(report['success']); self.assertEqual(report['invocations'], [])


class SourceProofTest(unittest.TestCase):
    def test_full_source_proof_rejects_hidden_modes_symlinks_and_ignored_go_inputs(self):
        with tempfile.TemporaryDirectory() as d:
            root=Path(d)
            def git(*args):return subprocess.check_output(['git','-C',str(root),*args],stderr=subprocess.PIPE,text=True).strip()
            git('init','-q');git('config','user.name','CI fixture');git('config','user.email','ci@example.invalid')
            path=root/'input.go';path.write_text('package fixture\n');(root/'.gitignore').write_text('ignored.go\n')
            git('add','.');git('commit','-qm','source')
            proof=tier.full_race.source_proof(root);self.assertEqual(proof['source'],git('rev-parse','HEAD'))
            git('update-index','--assume-unchanged','input.go');path.write_text('package changed\n')
            self.assertEqual(git('status','--porcelain'),'')
            with self.assertRaises(tier.full_race.shards.Failure):tier.full_race.source_proof(root)
            path.write_text('package fixture\n');path.chmod(0o755)
            with self.assertRaises(tier.full_race.shards.Failure):tier.full_race.source_proof(root)
            path.chmod(0o644);(root/'ignored.go').write_text('package injected\n')
            with self.assertRaises(tier.full_race.shards.Failure):tier.full_race.source_proof(root)
            (root/'ignored.go').unlink();path.unlink();path.symlink_to('.gitignore')
            with self.assertRaises(tier.full_race.shards.Failure):tier.full_race.source_proof(root)

    def test_verified_merge_includes_renamed_paths_and_hidden_working_edits_force_full(self):
        with tempfile.TemporaryDirectory() as directory:
            root=Path(directory).resolve()
            def git(*args):
                return subprocess.check_output(['git','-C',str(root),*args],stderr=subprocess.PIPE,text=True).strip()
            git('init','-q');git('config','user.name','CI fixture');git('config','user.email','ci@example.invalid')
            target=root/'internal/scheduler/old.go';target.parent.mkdir(parents=True);target.write_text('package scheduler\n')
            git('add','.');git('commit','-qm','base');base=git('rev-parse','HEAD');main=git('branch','--show-current')
            git('checkout','-qb','fixture-change');git('mv','internal/scheduler/old.go','internal/scheduler/new.go');git('commit','-qm','rename');head=git('rev-parse','HEAD')
            git('checkout',main);git('merge','--no-ff','-m','tested merge',head);tested=git('rev-parse','HEAD')
            self.assertEqual(git('rev-list','--parents','-n','1',tested).split(),[tested,base,head])
            event={'pull_request':{'base':{'sha':base},'head':{'sha':head}}}
            with mock.patch.object(tier,'ROOT',root):
                plan=tier.pr_extras(event,tested)
                self.assertEqual(plan['reason'],'verified base/head/tested merge')
                self.assertEqual(plan['paths'],['internal/scheduler/new.go','internal/scheduler/old.go'])
                self.assertTrue(plan['systemd']);self.assertEqual(plan['packages'],[])
                git('update-index','--assume-unchanged','internal/scheduler/new.go')
                (root/'internal/scheduler/new.go').write_text('hidden edit\n')
                self.assertEqual(git('status','--porcelain'),'')
                self.assertTrue(tier.pr_extras(event,tested)['regressions'])


class PlainVetTest(unittest.TestCase):
    proof={'source':'a'*40,'tree':'b'*40}
    context={'version':'go version go1.27.2 linux/amd64','runner_os':'Linux','runner_arch':'X64',
             'race':False,'tags':[],'go':{'GOVERSION':'go1.27.2','GOOS':'linux','GOHOSTOS':'linux',
             'GOARCH':'amd64','GOHOSTARCH':'amd64','CGO_ENABLED':'1','GOTOOLCHAIN':'local',
             'GOWORK':'off','GOFLAGS':'-mod=readonly'}}

    import go_cache_context as context_keys
    context.update(schema=context_keys.SCHEMA,os_release='d'*64,
                   cc={'binary':'d'*64,'version':'d'*64},cxx={'binary':'d'*64,'version':'d'*64},
                   toolchain={name:'d'*64 for name in ('go','compile','asm','link','cgo','vet')},
                   image={'ImageOS':'ubuntu24','ImageVersion':'20261001.1.0'})
    context['go']={**dict.fromkeys(context_keys.GO_FIELDS,''),**context['go']}

    def test_actual_checked_vet_receipt_and_failures_bind_source_and_context(self):
        import run_go_shard
        with tempfile.TemporaryDirectory() as d, mock.patch.object(tier.full_race,'source_proof',return_value=self.proof), mock.patch.object(tier,'plain_vet_context',return_value=self.context):
            out=Path(d)
            def checked(command,path,deadline,env):
                self.assertEqual(command,['go','vet','./...'])
                self.assertNotIn('AGENT_ARCHIVE_PERF',env);self.assertEqual(env['AGENT_ARCHIVE_REAL_SYSTEMD'],'0')
                path.write_text('');return ''
            with mock.patch.object(tier,'checked',side_effect=checked):receipt=tier.execute_plain_vet(out,123)
            report={'plain_vet':receipt};fingerprint=run_go_shard.digest(tier.plain_vet_identity(self.context))
            tier.verify_plain_vet(out,report,self.proof,fingerprint)
            # A nonzero/failed supervisor can never produce a successful receipt.
            with mock.patch.object(tier,'checked',side_effect=tier.Failure('exit1')):
                with self.assertRaises(tier.Failure):tier.execute_plain_vet(out,123)
            for update in (lambda r:r.update(exit_code=1),lambda r:r.update(exit_code=True),
                           lambda r:r.update(command=['go','test','./...']),
                           lambda r:r['post_source_proof'].update(tree='c'*40),
                           lambda r:r['pre_source_proof'].update(source='c'*40),
                           lambda r:r['context']['go'].update(GOVERSION='go1.27.1'),
                           lambda r:r['context']['go'].update(GOARCH='arm64'),
                           lambda r:r['context']['go'].update(GOFLAGS='-tags=other'),
                           lambda r:r.update(log_sha256='c'*64),lambda r:r['context'].pop('toolchain')):
                bad=copy.deepcopy(receipt);update(bad)
                with self.assertRaises(tier.Failure):tier.verify_plain_vet(out,{'plain_vet':bad},self.proof,fingerprint)
            for key,value in (('GOVERSION','go1.27.1'),('GOARCH','arm64'),('GOOS','darwin'),('CGO_ENABLED','0')):
                bad=copy.deepcopy(receipt);bad['context']['go'][key]=value
                forged_digest=run_go_shard.digest(bad['context'])
                with self.assertRaises(tier.Failure):
                    tier.verify_plain_vet(out,{'plain_vet':bad},self.proof,forged_digest,historical=True)
            with mock.patch.object(tier,'plain_vet_context',return_value=dict(self.context,image={'ImageVersion':'different'})):
                with self.assertRaises(tier.Failure):tier.verify_plain_vet(out,report,self.proof,fingerprint)
            tier.verify_plain_vet(out,report,self.proof,fingerprint,historical=True)
            old=copy.deepcopy(receipt);old.pop('identity_schema')
            tier.verify_plain_vet(out,{'plain_vet':old},self.proof,run_go_shard.digest(self.context),historical=True)
            with self.assertRaises(tier.Failure):tier.verify_plain_vet(out,{'plain_vet':old},self.proof,run_go_shard.digest(self.context))
            with self.assertRaises(tier.Failure):tier.verify_plain_vet(out,{},self.proof,fingerprint)
            (out/'plain-vet.txt').unlink()
            with self.assertRaises(tier.Failure):tier.verify_plain_vet(out,report,self.proof,fingerprint)
            with mock.patch.object(tier.full_race,'source_proof',side_effect=[self.proof,{'source':'a'*40,'tree':'c'*40}]),mock.patch.object(tier,'checked',side_effect=checked):
                with self.assertRaises(tier.Failure):tier.execute_plain_vet(out,123)
            with mock.patch.object(tier,'plain_vet_context',side_effect=[self.context,dict(self.context,version='changed')]),mock.patch.object(tier,'checked',side_effect=checked):
                with self.assertRaises(tier.Failure):tier.execute_plain_vet(out,123)

    def test_cross_job_identity_ignores_only_image_release_and_never_downgrades_live(self):
        import run_go_shard
        old=copy.deepcopy(self.context);old['image']['ImageVersion']='20261004.327.1'
        new=copy.deepcopy(old);new['image']['ImageVersion']='20261012.334.1'
        self.assertNotEqual(run_go_shard.digest(old),run_go_shard.digest(new))
        self.assertEqual(tier.plain_vet_identity(old),tier.plain_vet_identity(new))
        import go_cache_context
        with tempfile.TemporaryDirectory() as d:
            source={'tested':'a'*40,'tree':'b'*40,'dependency':'c'*64}
            root=Path(d).resolve()
            self.assertNotEqual(go_cache_context.cache_plan(42,0,old,source,root).prefix,
                                go_cache_context.cache_plan(42,0,new,source,root).prefix)
        with tempfile.TemporaryDirectory() as d,mock.patch.object(tier.full_race,'source_proof',return_value=self.proof),mock.patch.object(tier,'plain_vet_context',return_value=old):
            out=Path(d)
            def checked(command,path,deadline,env):path.write_text('');return ''
            with mock.patch.object(tier,'checked',side_effect=checked):receipt=tier.execute_plain_vet(out,123)
            fingerprint=run_go_shard.digest(tier.plain_vet_identity(new))
            with mock.patch.object(tier,'plain_vet_context',return_value=new):
                tier.verify_plain_vet(out,{'plain_vet':receipt},self.proof,fingerprint)
            self.assertEqual(receipt['context']['image']['ImageVersion'],'20261004.327.1')
            with mock.patch.object(tier,'plain_vet_context',side_effect=[old,new]),mock.patch.object(tier,'checked',side_effect=checked):
                with self.assertRaises(tier.Failure):tier.execute_plain_vet(out,123)
            for path,value in ((('cc','binary'),'e'*64),(('cxx','version'),'e'*64),
                               (('toolchain','vet'),'e'*64),(('go','CGO_CFLAGS'),'-different'),
                               (('go','GOAMD64'),'v3'),(('os_release',),'e'*64),
                               (('image','ImageOS'),'ubuntuOther')):
                bad=copy.deepcopy(receipt);target=bad['context']
                for component in path[:-1]:target=target[component]
                target[path[-1]]=value
                recomputed=run_go_shard.digest(tier.plain_vet_identity(bad['context']))
                with self.assertRaises(tier.Failure):tier.verify_plain_vet(out,{'plain_vet':bad},self.proof,recomputed)
            for schema in (None,'unsupported'):
                bad=copy.deepcopy(receipt);bad['identity_schema']=schema
                with self.assertRaises(tier.Failure):tier.verify_plain_vet(out,{'plain_vet':bad},self.proof,fingerprint)
            bad=copy.deepcopy(receipt);bad['context']['image'].pop('ImageVersion')
            with self.assertRaises(tier.Failure):tier.verify_plain_vet(out,{'plain_vet':bad},self.proof,fingerprint)
            for image in ({'ImageOS':'ubuntu24','ImageVersion':'unsafe\nlabel'},
                          {'ImageOS':'ubuntu24','ImageVersion':'valid','extra':'unreviewed'}):
                bad=copy.deepcopy(receipt);bad['context']['image']=image
                with self.assertRaises(tier.Failure):tier.verify_plain_vet(out,{'plain_vet':bad},self.proof,fingerprint)

    def test_audit_body_and_vet_are_independent_required_proofs(self):
        import run_go_shard
        with tempfile.TemporaryDirectory() as d, mock.patch.object(tier,'git',return_value='a'*40), mock.patch.object(tier.full_race,'source_proof',return_value=self.proof), mock.patch.object(tier,'plain_vet_context',return_value=self.context), mock.patch.dict(os.environ,{'GITHUB_SHA':'a'*40}):
            out=Path(d);calls=[]
            def checked(command,path,deadline,env):
                calls.append(command)
                raw=SourceAuditTest().raw() if command==tier.SOURCE_AUDIT_COMMAND else ''
                path.write_text(raw);return raw
            with mock.patch.object(tier,'checked',side_effect=checked):tier.run_suite('pr','audit',out,own_plain_vet=True)
            self.assertEqual(calls,[tier.SOURCE_AUDIT_COMMAND,tier.PLAIN_VET_COMMAND])
            fingerprint=run_go_shard.digest(tier.plain_vet_identity(self.context))
            report=tier.verify_source_audit(out,'a'*40,require_plain_vet=True,expected_vet_context_sha256=fingerprint)
            saved=copy.deepcopy(report);report.pop('plain_vet');(out/'tier.json').write_text(json.dumps(report))
            tier.verify_source_audit(out,'a'*40) # Historical unowned evidence remains valid.
            with self.assertRaises(tier.Failure):tier.verify_source_audit(out,'a'*40,require_plain_vet=True,expected_vet_context_sha256=fingerprint)
            (out/'tier.json').write_text(json.dumps(saved));(out/'source-audit.jsonl').write_text(events({'Package':tier.SOURCE_AUDIT_PACKAGE,'Action':'pass'}))
            with self.assertRaises(tier.Failure):tier.verify_source_audit(out,'a'*40,require_plain_vet=True,expected_vet_context_sha256=fingerprint)
            with self.assertRaises(tier.Failure):tier.run_suite('daily','linux',out,own_plain_vet=True)


class SourceAuditTest(unittest.TestCase):
    proof = {'source': 'a'*40, 'tree': 'b'*40}

    def raw(self):
        return events({'Action': 'run', 'Package': tier.SOURCE_AUDIT_PACKAGE, 'Test': tier.SOURCE_AUDIT_TEST},
                      {'Action': 'pass', 'Package': tier.SOURCE_AUDIT_PACKAGE, 'Test': tier.SOURCE_AUDIT_TEST},
                      {'Action': 'pass', 'Package': tier.SOURCE_AUDIT_PACKAGE})

    def test_plain_gate_requires_exact_source_command_body_and_package_pass(self):
        import copy
        with tempfile.TemporaryDirectory() as d, mock.patch.object(tier, 'git', return_value='a'*40), mock.patch.object(tier.full_race, 'source_proof', return_value=self.proof), mock.patch.dict(os.environ, {'GITHUB_SHA': 'a'*40}):
            out = Path(d)
            def checked(command, output, deadline, env):
                self.assertEqual(command, tier.SOURCE_AUDIT_COMMAND)
                self.assertNotIn('-race', command); self.assertNotIn('-short', command)
                self.assertEqual(command[command.index('-p')+1], '1')
                output.write_text(self.raw()); return self.raw()
            with mock.patch.object(tier, 'checked', side_effect=checked): tier.run_suite('pr', 'audit', out)
            report = tier.verify_source_audit(out, 'a'*40)
            self.assertGreaterEqual(report['audit_plain_seconds'], 0)
            for update in (lambda r: r.update(success=False), lambda r: r['source_proof'].update(tree='c'*40),
                           lambda r: r.update(source='c'*40), lambda r: r['invocations'][0]['command'].append('-race'),
                           lambda r: r['invocations'][0].update(evidence={}), lambda r: r['invocations'].append(r['invocations'][0])):
                bad = copy.deepcopy(report); update(bad); (out/'tier.json').write_text(json.dumps(bad))
                with self.assertRaises(tier.Failure): tier.verify_source_audit(out, 'a'*40)
            (out/'tier.json').write_text(json.dumps(report))
            rows = [json.loads(line) for line in self.raw().splitlines()]
            for bad in (rows[1:], rows[:-1], rows + [rows[-1]],
                        [dict(r, Action='skip') if r.get('Test') and r['Action']=='pass' else r for r in rows],
                        rows + [{'Action': 'run', 'Package': tier.SOURCE_AUDIT_PACKAGE, 'Test': 'TestOther'}]):
                (out/'source-audit.jsonl').write_text(events(*bad))
                with self.assertRaises(tier.Failure): tier.verify_source_audit(out, 'a'*40)

    def test_owned_race_skip_is_explicit_and_plain_pass_cannot_replace_it(self):
        raw = events({'Action':'run','Package':tier.SOURCE_AUDIT_PACKAGE,'Test':tier.SOURCE_AUDIT_TEST},
                     {'Action':'output','Package':tier.SOURCE_AUDIT_PACKAGE,'Test':tier.SOURCE_AUDIT_TEST,'Output':tier.SOURCE_AUDIT_SKIP},
                     {'Action':'skip','Package':tier.SOURCE_AUDIT_PACKAGE,'Test':tier.SOURCE_AUDIT_TEST})
        tier.verify_owned_audit_skip(raw)
        for bad in ('', self.raw(), raw.replace(tier.SOURCE_AUDIT_SKIP, 'unrelated skip')):
            with self.assertRaises(tier.Failure): tier.verify_owned_audit_skip(bad)


class ExecutionPlanTest(unittest.TestCase):
    def test_daily_executes_all_short_and_four_exact_full_bodies_with_expected_children(self):
        entries=[e for e in tier.catalog()['restorations'] if e['restore']=='daily']
        def command(args,output,deadline,env=None):
            if args == tier.SOURCE_AUDIT_COMMAND:
                text = SourceAuditTest().raw(); output.write_text(text); return text
            lines=[]
            for pkg in sorted({tier.MODULE+e['package'] for e in entries}):
                selected=[e for e in entries if tier.MODULE+e['package']==pkg]
                if '-list' in args:
                    self.assertIn('-count=1',args);self.assertIn('-race',args)
                    lines += [{'Action':'output','Package':pkg,'Output':e['test']+'\n'} for e in selected]
                    lines.append({'Action':'pass','Package':pkg});continue
                pattern=args[args.index('-run')+1] if '-run' in args else None
                if pattern and './'+pkg[len(tier.MODULE):] not in args:continue
                for e in selected:
                    if pattern and not __import__('re').fullmatch(pattern,e['test']):continue
                    for child in (['128'] if '-short' in args and e.get('full_children')==['128','512'] else e.get('full_children',[])):
                        lines.append({'Action':'pass','Package':pkg,'Test':e['test']+'/'+child})
                    lines.append({'Action':'pass','Package':pkg,'Test':e['test']})
                lines.append({'Action':'pass','Package':pkg})
            if '-short' in args:
                self.assertEqual(env[tier.SOURCE_AUDIT_ENV], '1')
                lines += [{'Action':'run','Package':tier.SOURCE_AUDIT_PACKAGE,'Test':tier.SOURCE_AUDIT_TEST}, {'Action':'output','Package':tier.SOURCE_AUDIT_PACKAGE,'Test':tier.SOURCE_AUDIT_TEST,'Output':tier.SOURCE_AUDIT_SKIP}, {'Action':'skip','Package':tier.SOURCE_AUDIT_PACKAGE,'Test':tier.SOURCE_AUDIT_TEST}, {'Action':'pass','Package':tier.SOURCE_AUDIT_PACKAGE}]
            text=events(*lines);output.write_text(text);return text
        with tempfile.TemporaryDirectory() as d,mock.patch.object(tier.full_race,'source_proof',return_value=SourceAuditTest.proof),mock.patch.object(tier,'git',return_value='a'*40),mock.patch.object(tier,'checked',side_effect=command),mock.patch.dict(os.environ,{'GITHUB_SHA':'a'*40}):
            tier.run_suite('daily','linux',Path(d))
            report=json.loads((Path(d)/'tier.json').read_text())
            self.assertTrue(report['success']);self.assertEqual(len(report['invocations']),5)
            self.assertEqual(report['invocations'][0]['command'], tier.SOURCE_AUDIT_COMMAND)
            self.assertIn('-short',report['invocations'][1]['command'])
            self.assertTrue(all('-short' not in i['command'] for i in report['invocations'][2:]))
            restored={r['test'] for i in report['invocations'][2:] for r in i['evidence']['actual'] if '/' not in r['test']}
            self.assertEqual(restored,{e['test'] for e in entries})

    def test_real_manager_is_enabled_only_for_dedicated_bodies_not_race_perf_listing(self):
        calls=[]
        def checked(command,output,deadline,env=None):
            calls.append((command,env['AGENT_ARCHIVE_REAL_SYSTEMD']))
            package=tier.MODULE+'internal/scheduler/systemd'
            name='TestRealUserManagerConformance'
            if command[:2] == ['go', 'list']:
                text=json.dumps({'ImportPath':package,'TestGoFiles':['test.go']})
                output.write_text(text);return text
            if '-c' in command:
                temporary = Path(command[command.index('-o')+1])
                for selected_package in command[7:]:
                    (temporary/(selected_package.rsplit('/', 1)[1]+'.test')).write_text('compile only')
                output.write_text(''); return ''
            if './internal/cli' in command:
                package=tier.MODULE+'internal/cli';name='TestRealSystemdSetup'
            if '-list' in command:
                if './internal/scheduler/...' in command:name='TestOrdinary'
                text=events({'Action':'output','Package':package,'Output':name+'\n'},{'Action':'pass','Package':package})
            else:
                if '-race' in command or command[-1]=='./internal/capture':name='TestOrdinary'
                records=[{'Action':'pass','Package':package,'Test':name},{'Action':'pass','Package':package}]
                if name=='TestRealUserManagerConformance':
                    child=name+'/UnreadableAndAbsentDefinitions'
                    records.extend([{'Action':'output','Package':package,'Test':child,'Output':'    conformance.go:119: a real manager cannot be put in a loaded state over damaged unit files\n'},
                                    {'Action':'skip','Package':package,'Test':child}])
                text=events(*records)
            output.write_text(text);return text
        extra={'packages':['./internal/scheduler/...'],'native':False,'systemd':True,'fuzz':False,'purge':False}
        with tempfile.TemporaryDirectory() as d,mock.patch.object(tier,'git',return_value='a'*40),mock.patch.object(tier.full_race,'source_proof',return_value={'source':'a'*40,'tree':'b'*40}),mock.patch.object(tier,'checked',side_effect=checked),mock.patch.dict(os.environ,{'GITHUB_SHA':'a'*40,'AGENT_ARCHIVE_REAL_SYSTEMD':'1'}):
            tier.run_suite('full','extra',Path(d),extra)
            proof=json.loads((Path(d)/'tier.json').read_text());self.assertTrue(proof['success'])
            actual=next(i for i in proof['invocations'] if i['log']=='systemd-systemd.jsonl')['evidence']['actual']
            self.assertEqual(next(r for r in actual if r['test'].endswith('/UnreadableAndAbsentDefinitions'))['status'],'skip')
        for command,enabled in calls:
            dedicated='-run' in command and command[command.index('-run')+1] in ('^TestReal','^TestRealSystemd')
            self.assertEqual(enabled,'1' if dedicated else '0')
        self.assertEqual(sum(enabled=='1' for _,enabled in calls),2)

    def test_full_verdict_rejects_missing_artifact_source_and_incomplete_empty_extra(self):
        with tempfile.TemporaryDirectory() as d,mock.patch.dict(os.environ,{'GITHUB_SHA':'a'*40,'GITHUB_EVENT_NAME':'local','GITHUB_REF':'','CI_SCHEDULE':''}):
            directory=Path(d);plan={'source':'a'*40,'packages':[],'native':False,'systemd':False,'fuzz':False,'purge':False}
            with self.assertRaises(FileNotFoundError):tier.verify_full(directory,plan)
            proof={'schema':1,'source':'a'*40,'tier':'full','suite':'extra','success':True,'extras':plan,'invocations':[], 'event':'local','ref':'','schedule':''}
            (directory/'tier.json').write_text(json.dumps(proof));tier.verify_full(directory,plan)
            for key,value in [('source','b'*40),('success',False),('tier','daily')]:
                bad=dict(proof,**{key:value});(directory/'tier.json').write_text(json.dumps(bad))
                with self.assertRaises(tier.Failure):tier.verify_full(directory,plan)

    def test_every_fuzz_campaign_uses_cadence_duration_and_failure_cannot_complete(self):
        package=tier.MODULE+'internal/archive'
        names=['FuzzCase'+str(i) for i in range(24)]
        for cadence,duration in [('daily','5s'),('full','30s')]:
            calls=[]
            def checked(command,output,deadline,env=None):
                self.assertEqual(env['AGENT_ARCHIVE_REAL_SYSTEMD'],'0')
                if '-list' in command:
                    self.assertIn('-count=1',command)
                    text=events(*[{'Action':'output','Package':package,'Output':name+'\n'} for name in names],{'Action':'pass','Package':package})
                else:
                    calls.append(command)
                    text=events({'Action':'output','Package':package,'Output':'fuzz: elapsed: campaign completed\n'},{'Action':'pass','Package':package})
                output.write_text(text);return text
            with tempfile.TemporaryDirectory() as d,mock.patch.object(tier,'git',return_value='a'*40),mock.patch.object(tier,'checked',side_effect=checked),mock.patch.dict(os.environ,{'GITHUB_SHA':'a'*40,'AGENT_ARCHIVE_REAL_SYSTEMD':'1'}):
                tier.run_suite(cadence,'fuzz',Path(d))
                proof=json.loads((Path(d)/'tier.json').read_text())
                self.assertTrue(proof['success']);self.assertEqual(proof['fuzz_discovered'],proof['fuzz_completed'])
            self.assertEqual(len(calls),24)
            self.assertTrue(all(c[c.index('-fuzztime')+1]==duration and '-count=1' in c for c in calls))
        with tempfile.TemporaryDirectory() as d,mock.patch.object(tier,'git',return_value='a'*40),mock.patch.object(tier,'discover',return_value={package:names}),mock.patch.object(tier,'checked',side_effect=tier.Failure('campaign cancelled')),mock.patch.dict(os.environ,{'GITHUB_SHA':'a'*40}):
            with self.assertRaises(tier.Failure):tier.run_suite('full','fuzz',Path(d))
            self.assertFalse(json.loads((Path(d)/'tier.json').read_text())['success'])

    def test_candidate_and_manual_all_cannot_be_downgraded_by_explicit_flag(self):
        for event,ref in [('push','refs/heads/release-candidate/final'),('workflow_dispatch','refs/heads/main')]:
            with tempfile.TemporaryDirectory() as d,mock.patch.object(sys,'argv',['run_ci_tier','--tier','daily','--suite','linux','--output',d]),mock.patch.dict(os.environ,{'GITHUB_EVENT_NAME':event,'GITHUB_REF':ref,'CI_SUITE':'all'}),mock.patch.object(tier,'run_suite') as run:
                self.assertEqual(tier.main(),0);self.assertEqual(run.call_args.args[0],'full')

    def test_release_tag_parser_selects_full_on_both_platforms_and_refuses_downgrades(self):
        for suite in ('linux', 'macos'):
            with tempfile.TemporaryDirectory() as d, mock.patch.dict(os.environ, {'GITHUB_EVENT_NAME':'push', 'GITHUB_REF':'refs/tags/v0.2.0', 'GITHUB_SHA':'a'*40}), mock.patch.object(tier, 'git', return_value='a'*40):
                output=Path(d)/'plan.json'
                with mock.patch.object(sys,'argv',['run_ci_tier','--plan','--tier','full','--suite',suite,'--output',str(output)]):
                    self.assertEqual(tier.main(),0)
                plan=json.loads(output.read_text())
                self.assertEqual(plan['tier'],'full');self.assertEqual(plan['source'],'a'*40)
                with mock.patch.object(tier,'run_suite') as run, mock.patch.object(sys,'argv',['run_ci_tier','--tier','full','--suite',suite,'--output',d]):
                    self.assertEqual(tier.main(),0)
                    self.assertEqual(run.call_args.args[:2],('full',suite))
                    self.assertEqual(run.call_args.args[4],2400)
                with mock.patch.object(tier,'verify_full') as verify, mock.patch.object(sys,'argv',['run_ci_tier','--verify','--tier','full','--suite',suite,'--extra-plan',str(output),'--output',d]):
                    self.assertEqual(tier.main(),0)
                    self.assertEqual(verify.call_args.kwargs,{'expected_suite':suite})
                for weaker in ('pr','daily'):
                    with mock.patch.object(tier,'run_suite') as run, mock.patch.object(sys,'argv',['run_ci_tier','--tier',weaker,'--suite',suite,'--output',d]), contextlib.redirect_stderr(io.StringIO()):
                        self.assertEqual(tier.main(),1);run.assert_not_called()


class PurgeSelectionTest(unittest.TestCase):
    def test_default_full_matrix_matches_original_cartesian_boundaries(self):
        selected,omitted=purge.resume_cases('full')
        self.assertFalse(omitted);self.assertEqual(len(selected),67)
        self.assertIn(('', 'all','all',10),selected)

    def test_representative_tiers_restore_all_modes_failures_and_weekly_completeness(self):
        full=set(purge.resume_cases('full')[0])
        pr,pr_omitted=purge.resume_cases('pr');daily,daily_omitted=purge.resume_cases('daily')
        self.assertLess(len(pr),len(daily));self.assertLess(len(daily),len(full))
        self.assertEqual(set(pr)|set(pr_omitted),full);self.assertEqual(set(daily)|set(daily_omitted),full)
        self.assertEqual({x[1] for x in pr},{'unreferenced','machine','old','all'})
        self.assertEqual({x[0] for x in daily},{'agent-archive/','nested/archive/',''})
        self.assertTrue(set(pr)<=set(daily))
        # Uncertain capsule boundary/phase cases remain in separate unchanged test.
        source=Path(purge.__file__).read_text()
        self.assertIn("if boundary == 2 and phase == 'after':",source)
        self.assertIn('self.assertEqual(self.attempts(), [])',source)


class FullRaceTest(unittest.TestCase):
    def fixture(self, directory, isolated=False):
        full=tier.full_race;cli=full.CLI;ordinary=tier.MODULE+'internal/other';empty=tier.MODULE+'internal/empty'
        packages={cli:True,ordinary:True,empty:False}
        names={cli:['ExampleWorks','FuzzSeed','TestCollision','TestNew','TestRestore'],ordinary:['TestCollision'],empty:[]}
        if isolated:names[cli]=sorted(names[cli]+['TestPickerQualificationRealCLIPhases'])
        (directory/'full-packages.json').write_text(events(*[
            dict(ImportPath=p,TestGoFiles=['fixture_test.go'] if tested else []) for p,tested in packages.items()]))
        records=[]
        for pkg, tests in names.items():
            records += [dict(Action='output',Package=pkg,Output=n+'\n') for n in tests]
            if not packages[pkg]:records.append(dict(Action='output',Package=pkg,Output='[no test files]\n'))
            records.append(dict(Action='pass' if packages[pkg] else 'skip',Package=pkg))
        (directory/'full-list.jsonl').write_text(events(*records))
        hints=json.loads((tier.ROOT/'scripts/go_shard_hints.json').read_text())['cli']
        with contextlib.redirect_stdout(io.StringIO()):schedule=full.schedule(packages,names,['./...'],hints)
        schedule['source_proof']={'source':'a'*40,'tree':'b'*40}
        schedule['discovery']=[dict(command=['go','list','-json','./...'],log='full-packages.json'),dict(command=full.LIST_FLAGS+['./...'],log='full-list.jsonl')]
        invocations=[]
        for index, (command,assignment) in enumerate(zip(schedule['commands'],schedule['assignments'])):
            rows=[]
            for pkg, tests in assignment.items():
                for name in tests:
                    rows.append(dict(Action='run',Package=pkg,Test=name))
                    if name=='TestRestore':rows += [dict(Action='run',Package=pkg,Test=name+'/512'),dict(Action='pass',Package=pkg,Test=name+'/512')]
                    if name=='TestPickerQualificationRealCLIPhases':
                        for child in ('1400','10000','100000'):
                            rows += [dict(Action='run',Package=pkg,Test=name+'/'+child),dict(Action='pass',Package=pkg,Test=name+'/'+child)]
                    rows.append(dict(Action='pass',Package=pkg,Test=name))
                if not packages[pkg]:rows.append(dict(Action='output',Package=pkg,Output='[no test files]\n'))
                rows.append(dict(Action='pass' if packages[pkg] else 'skip',Package=pkg))
            text=events(*rows);log=f'race-{index}.jsonl';(directory/log).write_text(text)
            invocations.append(dict(command=command,log=log,evidence=tier.body_evidence(text,False),performance_enabled=False,real_systemd_enabled=False))
        perf=events(dict(Action='pass',Package=tier.MODULE+'internal/collector',Test='TestStayFast'))
        (directory/'performance.jsonl').write_text(perf)
        invocations.append(dict(command=tier.PERFORMANCE,log='performance.jsonl',evidence=tier.body_evidence(perf,False),performance_enabled=True,real_systemd_enabled=False))
        report=dict(schema=1,source='a'*40,tier='full',suite='extra',success=True,
                    event='local',ref='',schedule='',
                    extras=dict(source='a'*40,packages=['./...'],native=False,systemd=False,fuzz=False,purge=False),
                    invocations=invocations,full_schedule=schedule,
                    full_schedule_sha256=full.shards.digest(schedule),
                    full_discovery=[dict(d,completed=True) for d in schedule['discovery']])
        report['performance_compile'] = dict(source_proof=schedule['source_proof'], packages=tier.PERFORMANCE[-4:], parallel=2, completed=True)
        return report

    def verify(self, directory, report, restorations=None):
        (directory/'tier.json').write_text(json.dumps(report))
        with mock.patch.dict(os.environ,{'GITHUB_SHA':'a'*40,'GITHUB_EVENT_NAME':'local','GITHUB_REF':'','CI_SCHEDULE':''}),mock.patch.object(tier.full_race,'source_proof',return_value={'source':'a'*40,'tree':'b'*40}),mock.patch.object(tier,'catalog',return_value={'restorations':restorations or []}),contextlib.redirect_stdout(io.StringIO()):
            tier.verify_full(directory,report['extras'])

    def test_verdict_fixture_isolates_hosted_workflow_identity(self):
        hosted={'GITHUB_SHA':'c'*40,'GITHUB_EVENT_NAME':'pull_request','GITHUB_REF':'refs/pull/493/merge','CI_SCHEDULE':'17 8 * * 0'}
        with tempfile.TemporaryDirectory() as d,mock.patch.dict(os.environ,hosted):
            directory=Path(d);report=self.fixture(directory)
            self.verify(directory,report)
            for key,value in hosted.items():self.assertEqual(os.environ[key],value)
            # Fixture isolation must not allow a forged report identity.
            for key,value in [('event','push'),('ref','refs/heads/foreign'),('schedule','foreign')]:
                bad=copy.deepcopy(report);bad[key]=value
                with self.subTest(key=key),self.assertRaises(tier.Failure):self.verify(directory,bad)

    def test_tag_full_verdict_rejects_forged_source_coverage_and_platform_obligations(self):
        for suite in ('linux', 'macos'):
            with tempfile.TemporaryDirectory() as d:
                directory=Path(d)
                with mock.patch.object(tier,'catalog',return_value={'restorations':[]}):
                    report=self.fixture(directory,isolated=True)
                report.update(suite=suite,event='push',ref='refs/tags/v0.2.0')
                if suite == 'macos':
                    raw=events(dict(Action='run',Package=tier.MODULE+'internal/config',Test='TestPublishedWriterRefusesProtectedConfiguration'),dict(Action='pass',Package=tier.MODULE+'internal/config',Test='TestPublishedWriterRefusesProtectedConfiguration'),dict(Action='pass',Package=tier.MODULE+'internal/config'))
                    (directory/'published-writer.jsonl').write_text(raw)
                    report['invocations'].append(dict(command=['bash','scripts/test_published_writer.sh'],log='published-writer.jsonl',evidence=tier.body_evidence(raw,False),performance_enabled=False,real_systemd_enabled=False))
                def verify(value, restorations=None):
                    (directory/'tier.json').write_text(json.dumps(value))
                    with mock.patch.dict(os.environ,{'GITHUB_SHA':'a'*40,'GITHUB_EVENT_NAME':'push','GITHUB_REF':'refs/tags/v0.2.0','CI_SCHEDULE':''}),mock.patch.object(tier.full_race,'source_proof',return_value={'source':'a'*40,'tree':'b'*40}),mock.patch.object(tier,'catalog',return_value={'restorations':restorations or []}),contextlib.redirect_stdout(io.StringIO()):
                        tier.verify_full(directory,report['extras'],expected_suite=suite)
                verify(report)
                changes=[lambda r:r.update(success=False),lambda r:r.update(source='c'*40),lambda r:r.update(ref='refs/tags/v0.2.1'),lambda r:r.update(suite='linux' if suite=='macos' else 'macos'),lambda r:r['full_schedule']['source_proof'].update(tree='c'*40),lambda r:r['invocations'][0]['command'].remove('-count=1')]
                if suite == 'macos':
                    changes += [lambda r:r['invocations'].pop(),lambda r:r['invocations'][-1].update(command=['bash','scripts/test_macos_smoke.sh']),lambda r:r['invocations'][-1].update(real_systemd_enabled=True),lambda r:r['invocations'][-1].update(performance_enabled=True)]
                for change in changes:
                    bad=copy.deepcopy(report);change(bad)
                    with self.assertRaises((tier.Failure,tier.full_race.shards.Failure)):
                        verify(bad)
                # A claimed-success receipt must still reconcile every raw descendant.
                target=directory/'race-2.jsonl';original=target.read_text()
                target.write_text(events(*[row for row in tier.full_race.shards.json_stream(original) if not row.get('Test','').endswith('/100000')]))
                bad=copy.deepcopy(report);bad['invocations'][2]['evidence']=tier.body_evidence(target.read_text(),False)
                # Catalogued children enforce restored scale, independently of parent PASS.
                entry=dict(package='internal/cli',test='TestPickerQualificationRealCLIPhases',restore='weekly',full_children=['100000'])
                with self.assertRaises((tier.Failure,tier.full_race.shards.Failure)):
                    verify(bad,[entry])

    def test_partition_preserves_colliding_package_names_and_new_fuzz_examples(self):
        with tempfile.TemporaryDirectory() as d:
            directory=Path(d);report=self.fixture(directory);self.verify(directory,report)
            assignments=report['full_schedule']['assignments'];cli=tier.full_race.CLI
            self.assertEqual(len(assignments),2)
            self.assertIn('TestCollision',assignments[0][cli]);self.assertNotIn('TestCollision',assignments[1][cli])
            self.assertEqual(set(assignments[0][cli])|set(assignments[1][cli]),set(report['full_schedule']['names'][cli]))
            self.assertFalse(set(assignments[0][cli])&set(assignments[1][cli]))
            self.assertIn('FuzzSeed',report['full_schedule']['names'][cli]);self.assertIn('ExampleWorks',report['full_schedule']['names'][cli])

    def test_measured_full_parent_has_its_own_package_lifetime(self):
        full=tier.full_race;cli=full.CLI;other=tier.MODULE+'internal/other'
        isolated='TestPickerQualificationRealCLIPhases'
        names={cli:['FuzzNew','TestA','TestB',isolated],other:['TestOther']}
        with contextlib.redirect_stdout(io.StringIO()):
            schedule=full.schedule({cli:True,other:True},names,['./...'],{})
        self.assertEqual(len(schedule['commands']),3)
        self.assertEqual(schedule['batches'],[[0],[1,2]])
        self.assertEqual(schedule['assignments'][2],{cli:[isolated]})
        self.assertEqual(schedule['commands'][2],full.COMMON+['-run','^('+isolated+')$',cli])
        assigned=[n for a in schedule['assignments'] for n in a.get(cli,[])]
        self.assertCountEqual(assigned,names[cli])
        self.assertEqual(len(assigned),len(set(assigned)))
        self.assertTrue(all('-short' not in c and c[6]=='20m' for c in schedule['commands']))
        # A global -skip would also suppress this name in another package.
        names[other].append(isolated)
        with contextlib.redirect_stdout(io.StringIO()):
            collision=full.schedule({cli:True,other:True},names,['./...'],{})
        self.assertEqual(len(collision['commands']),2)
        self.assertIn(isolated,collision['assignments'][0][cli])
        self.assertIn(isolated,collision['assignments'][0][other])

    def test_isolated_full_parent_verdict_rejects_assignment_and_raw_evidence_tampering(self):
        with tempfile.TemporaryDirectory() as d:
            directory=Path(d);original=self.fixture(directory,isolated=True)
            required=[e for e in tier.catalog()['restorations'] if e['test']=='TestPickerQualificationRealCLIPhases']
            self.verify(directory,original,required)
            self.assertEqual(len(original['full_schedule']['assignments']),3)
            for change in (lambda r:r['full_schedule'].pop('batches'),
                           lambda r:r['full_schedule'].__setitem__('batches',[[0,1],[2]]),
                           lambda r:r['full_schedule'].__setitem__('batches',[[0],[1],[2]]),
                           lambda r:r['full_schedule'].__setitem__('batches',[[0],[1,1,2]]),
                           lambda r:r['invocations'].pop(2),
                           lambda r:r['full_schedule']['assignments'][2][tier.full_race.CLI].clear(),
                           lambda r:r['invocations'][2]['command'].__setitem__(8,'.*')):
                bad=copy.deepcopy(original);change(bad)
                with self.assertRaises((tier.Failure,tier.full_race.shards.Failure)):
                    self.verify(directory,bad,required)
            path=directory/'race-2.jsonl'
            raw=path.read_text()
            rows=[json.loads(l) for l in raw.splitlines()]
            # Even matching claimed evidence cannot replace a missing raw parent.
            changed=events(*[r for r in rows if not r.get('Test')])
            path.write_text(changed);bad=copy.deepcopy(original)
            bad['invocations'][2]['evidence']=tier.body_evidence(changed,False)
            with self.assertRaises((tier.Failure,tier.full_race.shards.Failure)):
                self.verify(directory,bad,required)

            path.write_text(raw)
            # Isolating the parent cannot waive any required full-scale child.
            for child in ('1400','10000','100000'):
                changed=events(*[r for r in rows if r.get('Test')!='TestPickerQualificationRealCLIPhases/'+child])
                path.write_text(changed);bad=copy.deepcopy(original)
                bad['invocations'][2]['evidence']=tier.body_evidence(changed,False)
                with self.assertRaises((tier.Failure,tier.full_race.shards.Failure)):
                    self.verify(directory,bad,required)

    def test_verdict_rejects_omitted_overlapping_names_source_commands_and_unknown_invocations(self):
        with tempfile.TemporaryDirectory() as d:
            directory=Path(d);original=self.fixture(directory)
            changes=[lambda r:r['full_schedule']['names'][tier.full_race.CLI].remove('FuzzSeed'),
                     lambda r:r.pop('performance_compile'),
                     lambda r:r['performance_compile'].update(parallel=4),
                     lambda r:r['performance_compile'].update(completed=False),
                     lambda r:r['performance_compile']['packages'].pop(),
                     lambda r:r['performance_compile']['source_proof'].update(tree='c'*40),
                     lambda r:r.update(event='push'),
                     lambda r:r.update(ref='refs/heads/unrelated'),
                     lambda r:r['full_schedule']['assignments'][1][tier.full_race.CLI].append('TestCollision'),
                     lambda r:r['full_schedule']['source_proof'].update(source='c'*40),
                     lambda r:r['full_schedule']['discovery'][1]['command'].insert(2,'-short'),
                     lambda r:r['full_discovery'][1].update(completed=False),
                     lambda r:r['invocations'][0]['command'].insert(2,'-short'),
                     lambda r:r['invocations'][0]['command'].__setitem__(6,'21m'),
                     lambda r:r['invocations'][1]['command'].__setitem__(8,'.*'),
                     lambda r:r['invocations'].pop(1),
                     lambda r:r['invocations'].append(copy.deepcopy(r['invocations'][0])),
                     lambda r:r['invocations'].append(dict(r['invocations'][-1],log='extra.jsonl'))]
            (directory/'extra.jsonl').write_text((directory/'performance.jsonl').read_text())
            for index, change in enumerate(changes):
                bad=copy.deepcopy(original);change(bad)
                with self.subTest(index=index),self.assertRaises((tier.Failure,tier.full_race.shards.Failure)):
                    self.verify(directory,bad)

    def test_raw_logs_refuse_missing_parents_children_package_terminals_and_forged_evidence(self):
        with tempfile.TemporaryDirectory() as d:
            directory=Path(d);original=self.fixture(directory)
            invocation=original['invocations'][0];path=directory/invocation['log'];raw=path.read_text();rows=[json.loads(l) for l in raw.splitlines()]
            top=next(r['Test'] for r in rows if r.get('Test') and '/' not in r['Test'])
            variants=[lambda rs:[r for r in rs if r.get('Test')!=top],
                      lambda rs:[r for r in rs if r.get('Test')!=top or r['Action']!='pass'],
                      lambda rs:[r for r in rs if r.get('Test') or r['Action'] not in ('pass','skip')],
                      lambda rs:rs+[dict(Action='pass',Package='unexpected/package',Test='TestExtra')],
                      lambda rs:rs+[dict(Action='pass',Package=tier.full_race.CLI,Test='TestNotCompiled')],
                      lambda rs:rs+[dict(Action='run',Package=tier.full_race.CLI,Test=top+'/unended')],
                      lambda rs:rs+[next(r for r in rs if r.get('Test') and r['Action']=='pass')]]
            for index, mutate in enumerate(variants):
                changed=events(*mutate(rows));path.write_text(changed);bad=copy.deepcopy(original)
                try:bad['invocations'][0]['evidence']=tier.body_evidence(changed,False)
                except tier.Failure:pass
                with self.subTest(index=index),self.assertRaises((tier.Failure,tier.full_race.shards.Failure)):
                    self.verify(directory,bad)
            path.write_text(raw);bad=copy.deepcopy(original);bad['invocations'][0]['evidence']['actual']=[]
            with self.assertRaises(tier.Failure):self.verify(directory,bad)

    def test_required_restored_child_missing_or_skipped_fails_even_with_parent_pass(self):
        with tempfile.TemporaryDirectory() as d:
            directory=Path(d);report=self.fixture(directory)
            entry=dict(package='internal/cli',test='TestRestore',restore='weekly',full_children=['512'])
            with mock.patch.object(tier,'catalog',return_value={'restorations':[entry]}):
                # Recompute the scale evidence for this synthetic required parent.
                for inv in report['invocations']:inv['evidence']=tier.body_evidence((directory/inv['log']).read_text(),False)
                self.verify(directory,report,[entry])
                inv=next(i for i in report['invocations'] if any(r['test']=='TestRestore' for r in i['evidence']['actual']))
                path=directory/inv['log'];raw=path.read_text();rows=[json.loads(l) for l in raw.splitlines()]
                for skipped in (False,True):
                    altered=[dict(r,Action='skip') if r.get('Test')=='TestRestore/512' and r['Action']=='pass' else r for r in rows] if skipped else [r for r in rows if r.get('Test')!='TestRestore/512']
                    text=events(*altered);path.write_text(text);inv['evidence']=tier.body_evidence(text,False)
                    with self.assertRaises(tier.Failure):self.verify(directory,report,[entry])

    def test_discovery_rejects_duplicate_failed_or_body_executing_lists(self):
        full=tier.full_race;pkg=full.CLI
        for raw in (events(dict(Action='fail',Package=pkg)),events(dict(Action='run',Package=pkg,Test='TestA')),
                    events(dict(Action='output',Package=pkg,Output='TestA\n'),dict(Action='output',Package=pkg,Output='TestA\n'),dict(Action='pass',Package=pkg)),
                    events(dict(Action='pass',Package='unknown'))):
            with self.subTest(raw=raw),self.assertRaises(full.shards.Failure):full.listing(raw,{pkg:True})

    def test_full_batch_failure_retains_both_pending_entries_and_never_claims_success(self):
        for suite in ('linux','macos'):
            with self.subTest(suite=suite), tempfile.TemporaryDirectory() as d:
                directory=Path(d);original=self.fixture(directory,isolated=True)
                raw={p.name:p.read_text() for p in directory.iterdir()}
                commands=[]
                def checked(command,output,deadline,env=None):
                    commands.append(command)
                    if command[:2]==['go','list']:text=raw['full-packages.json']
                    elif '-list' in command:text=raw['full-list.jsonl']
                    else:text=raw[output.name]
                    output.write_text(text);return text
                def interrupted(batch,deadline):
                    pending=json.loads((directory/'tier.json').read_text())['invocations'][-2:]
                    self.assertEqual([i['log'] for i in pending],['race-1.jsonl','race-2.jsonl'])
                    batch[0][1].write_text(raw['race-1.jsonl'])
                    batch[1][1].write_text('')
                    raise tier.Failure('partial batch cancelled')
                with mock.patch.object(tier,'git',return_value='a'*40),mock.patch.object(tier.full_race,'source_proof',return_value={'source':'a'*40,'tree':'b'*40}),mock.patch.object(tier,'checked',side_effect=checked),mock.patch.object(tier,'checked_many',side_effect=interrupted),mock.patch.object(tier,'catalog',return_value={'restorations':[]}),mock.patch.dict(os.environ,{'GITHUB_SHA':'a'*40}),contextlib.redirect_stdout(io.StringIO()),self.assertRaisesRegex(tier.Failure,'partial batch'):
                    tier.run_suite('full',suite,directory,seconds=37)
                result=json.loads((directory/'tier.json').read_text())
                self.assertFalse(result['success'])
                self.assertEqual(len(result['invocations']),3)
                self.assertTrue(result['invocations'][0]['evidence']['actual'])
                self.assertTrue(all(i['evidence'] is None for i in result['invocations'][1:]))
                self.assertNotIn('performance_compile',result)
                self.assertEqual((directory/'race-1.jsonl').read_text(),raw['race-1.jsonl'])
                self.assertFalse(any('-c' in command for command in commands))

    def test_native_full_and_linux_enforce_the_same_compiled_schedule(self):
        for suite in ('linux','macos'):
            with tempfile.TemporaryDirectory() as d:
                directory=Path(d);report=self.fixture(directory,isolated=True);raw={p.name:p.read_text() for p in directory.iterdir()}
                calls=[]; batches=[]
                def checked(command,output,deadline,env=None):
                    calls.append((command,deadline,env['AGENT_ARCHIVE_REAL_SYSTEMD']))
                    if command[:2]==['go','list']:text=raw['full-packages.json']
                    elif '-c' in command:
                        temporary = Path(command[command.index('-o')+1])
                        for package in command[7:]:
                            (temporary/(package.rsplit('/', 1)[1]+'.test')).write_text('compile only')
                        text = ''
                    elif command == tier.PERFORMANCE:
                        self.assertTrue(all(not Path(c[6]).exists() for c, _, _ in calls if '-c' in c))
                        text = raw[output.name]
                    elif '-list' in command:text=raw['full-list.jsonl']
                    elif command[0]=='bash':text=events(dict(Action='run',Package=tier.MODULE+'internal/config',Test='TestPublishedWriterRefusesProtectedConfiguration'),dict(Action='pass',Package=tier.MODULE+'internal/config',Test='TestPublishedWriterRefusesProtectedConfiguration'),dict(Action='pass',Package=tier.MODULE+'internal/config'))
                    else:text=raw[output.name]
                    output.write_text(text);return text
                def checked_many(commands, deadline):
                    pending=json.loads((directory/'tier.json').read_text())['invocations'][-2:]
                    self.assertEqual([i['log'] for i in pending],['race-1.jsonl','race-2.jsonl'])
                    self.assertTrue(all(i['evidence'] is None for i in pending))
                    self.assertEqual([i['command'] for i in pending],[c for c,_,_ in commands])
                    batches.append([output.name for _,output,_ in commands])
                    for command,output,env in commands:
                        checked(command,output,deadline,env)
                with mock.patch.object(tier,'git',return_value='a'*40),mock.patch.object(tier.full_race,'source_proof',return_value={'source':'a'*40,'tree':'b'*40}),mock.patch.object(tier,'checked',side_effect=checked),mock.patch.object(tier,'checked_many',side_effect=checked_many),mock.patch.object(tier,'catalog',return_value={'restorations':[]}),mock.patch.dict(os.environ,{'GITHUB_SHA':'a'*40}),contextlib.redirect_stdout(io.StringIO()):
                    tier.run_suite('full',suite,directory,seconds=37)
                    result=json.loads((directory/'tier.json').read_text())
                    self.verify(directory, dict(result, event='local', ref='', schedule='', extras=report['extras']))
                    if suite == 'macos':
                        original=(directory/'published-writer.jsonl').read_text()
                        for bad in ('', events(dict(Action='skip',Package=tier.MODULE+'internal/config',Test='TestPublishedWriterRefusesProtectedConfiguration'),dict(Action='pass',Package=tier.MODULE+'internal/config'))):
                            (directory/'published-writer.jsonl').write_text(bad)
                            altered=json.loads((directory/'tier.json').read_text())
                            altered['invocations'][-1]['evidence']=tier.body_evidence(bad,False)
                            with self.assertRaises((tier.Failure,tier.full_race.shards.Failure)):
                                self.verify(directory,altered)
                        (directory/'published-writer.jsonl').write_text(original)
                        self.verify(directory,dict(result,event='local',ref='',schedule='',extras=report['extras']))
                result=json.loads((directory/'tier.json').read_text());self.assertTrue(result['success'])
                self.assertEqual(len([i for i in result['invocations'] if i['log'].startswith('race')]),3)
                self.assertEqual(batches,[['race-1.jsonl','race-2.jsonl']])
                self.assertEqual(result['full_schedule']['batches'],[[0],[1,2]])
                self.assertEqual(len({deadline for _,deadline,_ in calls}),1)
                self.assertTrue(all(enabled=='0' for _,_,enabled in calls))


if __name__=='__main__':
    unittest.main()
