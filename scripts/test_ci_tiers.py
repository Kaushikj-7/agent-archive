"""Behavior tests for cadence, exact selection, source fallback and truthful evidence."""
import contextlib
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

    def test_dependency_map_and_unknown_changes_are_conservative(self):
        self.assertTrue(tier.extra_for_paths(['internal/scheduler/systemd/manage.go'])['systemd'])
        self.assertTrue(tier.extra_for_paths(['internal/config/consent.go'])['native'])
        archive=tier.extra_for_paths(['internal/archive/redact.go'])
        self.assertEqual(archive['packages'],['./...'])
        self.assertTrue(all(archive[k] for k in ('native','systemd','fuzz','purge')))
        self.assertTrue(tier.extra_for_paths(['docs/security/archive-cleanup.md'])['purge'])
        for path in ('internal/newfamily/x.go','internal/testutil/helper.go','go.mod','scripts/run_ci_tier.py','.github/workflows/test.yml','unknown.md'):
            result=tier.extra_for_paths([path]);self.assertEqual(result['packages'],['./...']);self.assertTrue(all(result[k] for k in ('native','systemd','fuzz','purge')))
        self.assertEqual(tier.extra_for_paths([])['packages'],[])
        self.assertEqual(tier.extra_for_paths(['CHANGELOG.md','CONTRIBUTING.md'])['packages'],[])

    def test_history_error_falls_back_full_not_empty(self):
        with mock.patch.object(tier,'git',side_effect=OSError('history unavailable')):
            result=tier.pr_extras({'pull_request':{'base':{'sha':'a'*40},'head':{'sha':'b'*40}}},'c'*40)
        self.assertEqual(result['packages'],['./...'])
        self.assertTrue(result['systemd'])

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


class SourceProofTest(unittest.TestCase):
    def test_verified_merge_includes_renamed_paths_and_hidden_working_edits_force_full(self):
        with tempfile.TemporaryDirectory() as directory:
            root=Path(directory)
            def git(*args):
                return subprocess.check_output(['git','-C',str(root),*args],stderr=subprocess.PIPE,text=True).strip()
            git('init','-q');git('config','user.name','CI fixture');git('config','user.email','ci@example.invalid')
            target=root/'internal/scheduler/old.go';target.parent.mkdir(parents=True);target.write_text('package scheduler\n')
            git('add','.');git('commit','-qm','base');base=git('rev-parse','HEAD');main=git('branch','--show-current')
            git('checkout','-qb','head');git('mv','internal/scheduler/old.go','internal/scheduler/new.go');git('commit','-qm','rename');head=git('rev-parse','HEAD')
            git('checkout',main);git('merge','--no-ff','-m','tested merge','head');tested=git('rev-parse','HEAD')
            event={'pull_request':{'base':{'sha':base},'head':{'sha':head}}}
            with mock.patch.object(tier,'ROOT',root):
                plan=tier.pr_extras(event,tested)
                self.assertEqual(plan['reason'],'verified base/head/tested merge')
                self.assertEqual(plan['paths'],['internal/scheduler/new.go','internal/scheduler/old.go'])
                self.assertTrue(plan['systemd']);self.assertNotEqual(plan['packages'],['./...'])
                git('update-index','--assume-unchanged','internal/scheduler/new.go')
                (root/'internal/scheduler/new.go').write_text('hidden edit\n')
                self.assertEqual(git('status','--porcelain'),'')
                self.assertEqual(tier.pr_extras(event,tested)['packages'],['./...'])


class ExecutionPlanTest(unittest.TestCase):
    def test_daily_executes_all_short_and_four_exact_full_bodies_with_expected_children(self):
        entries=[e for e in tier.catalog()['restorations'] if e['restore']=='daily']
        def command(args,output,deadline,env=None):
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
            text=events(*lines);output.write_text(text);return text
        with tempfile.TemporaryDirectory() as d,mock.patch.object(tier,'git',return_value='a'*40),mock.patch.object(tier,'checked',side_effect=command),mock.patch.dict(os.environ,{'GITHUB_SHA':'a'*40}):
            tier.run_suite('daily','linux',Path(d))
            report=json.loads((Path(d)/'tier.json').read_text())
            self.assertTrue(report['success']);self.assertEqual(len(report['invocations']),4)
            self.assertIn('-short',report['invocations'][0]['command'])
            self.assertTrue(all('-short' not in i['command'] for i in report['invocations'][1:]))
            restored={r['test'] for i in report['invocations'][1:] for r in i['evidence']['actual'] if '/' not in r['test']}
            self.assertEqual(restored,{e['test'] for e in entries})

    def test_real_manager_is_enabled_only_for_dedicated_bodies_not_race_perf_listing(self):
        calls=[]
        def checked(command,output,deadline,env=None):
            calls.append((command,env['AGENT_ARCHIVE_REAL_SYSTEMD']))
            package=tier.MODULE+'internal/scheduler/systemd'
            name='TestRealUserManagerConformance'
            if './internal/cli' in command:
                package=tier.MODULE+'internal/cli';name='TestRealSystemdSetup'
            if '-list' in command:
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
        with tempfile.TemporaryDirectory() as d,mock.patch.object(tier,'git',return_value='a'*40),mock.patch.object(tier,'checked',side_effect=checked),mock.patch.dict(os.environ,{'GITHUB_SHA':'a'*40,'AGENT_ARCHIVE_REAL_SYSTEMD':'1'}):
            tier.run_suite('full','extra',Path(d),extra)
            proof=json.loads((Path(d)/'tier.json').read_text());self.assertTrue(proof['success'])
            actual=next(i for i in proof['invocations'] if i['log']=='systemd-systemd.jsonl')['evidence']['actual']
            self.assertEqual(next(r for r in actual if r['test'].endswith('/UnreadableAndAbsentDefinitions'))['status'],'skip')
        for command,enabled in calls:
            dedicated='-run' in command and command[command.index('-run')+1] in ('^TestReal','^TestRealSystemd')
            self.assertEqual(enabled,'1' if dedicated else '0')
        self.assertEqual(sum(enabled=='1' for _,enabled in calls),2)

    def test_full_verdict_rejects_missing_artifact_source_and_incomplete_empty_extra(self):
        with tempfile.TemporaryDirectory() as d,mock.patch.dict(os.environ,{'GITHUB_SHA':'a'*40}):
            directory=Path(d);plan={'source':'a'*40,'packages':[],'native':False,'systemd':False,'fuzz':False,'purge':False}
            with self.assertRaises(FileNotFoundError):tier.verify_full(directory,plan)
            proof={'schema':1,'source':'a'*40,'tier':'full','suite':'extra','success':True,'extras':plan,'invocations':[]}
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


if __name__=='__main__':
    unittest.main()
