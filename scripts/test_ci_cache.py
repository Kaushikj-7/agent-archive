#!/usr/bin/env python3
"""Contracts for accelerators, not a second set of coverage/verdict receipts."""
from dataclasses import replace
import io
import json
import os
from pathlib import Path
import subprocess
import tarfile
import tempfile
import unittest
from unittest import mock

import ci_cache as cache
import go_cache_context as keys
from go_cache_steps import verify_restored, restore_result
import pinned_git
import verified_tools
import warm_shared_tools
from test_go_cache_context import metadata
from test_ci_workflow import jobs, ROOT


class CompilationTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name).resolve()
        self.context = {'compiler': 'same', 'flags': '-race'}
        self.source = {'tested': 'a'*40, 'tree': 'b'*40, 'dependency': 'c'*64}
        self.plan = cache.general_plan(self.context, self.source, self.root)
        keys.prepare_paths(self.plan)
        (Path(self.plan.paths[0])/'object').write_bytes(b'compiled source')
        (Path(self.plan.paths[1])/'cache').mkdir()
        (Path(self.plan.paths[1])/'cache/module').write_bytes(b'verified dependency')

    def call(self, matched=None, outcome='success'):
        calls = []
        state = cache.restore_compilation(self.plan, self.context, outcome,
                                         self.plan.seed if matched is None else matched,
                                         lambda source: calls.append(('guard', source)),
                                         lambda argv: calls.append(argv))
        self.assertEqual(calls, [('guard', self.source), ['go', 'mod', 'download'],
                                 ['go', 'mod', 'verify'], ('guard', self.source)])
        return state

    def test_cold_hit_and_corruption_rebuild_without_previous_verdict(self):
        cache.publish_manifest(self.plan, self.context)
        self.assertEqual(self.call(), 'trusted')
        self.assertTrue((Path(self.plan.paths[0])/'object').exists())
        (Path(self.plan.paths[0])/'object').write_bytes(b'corrupt')
        self.assertEqual(self.call(), 'miss')
        self.assertFalse((Path(self.plan.paths[0])/'object').exists())
        self.assertEqual(self.call(matched=''), 'miss')
        self.assertEqual(self.call(outcome='failure'), 'miss')

    def test_manifest_symlink_is_rejected_without_reading_or_writing_target(self):
        target = self.root/'unrelated'
        target.write_text('keep source bytes')
        (Path(self.plan.paths[0])/cache.MANIFEST).symlink_to(target)
        with self.assertRaises(keys.InvalidContext):
            cache.publish_manifest(self.plan,self.context)
        with self.assertRaises(keys.InvalidContext):
            cache.validate_manifest(self.plan,self.context)
        self.assertEqual(target.read_text(),'keep source bytes')

    def test_context_and_module_invalidation_and_source_reuse(self):
        cache.publish_manifest(self.plan, self.context)
        new_source = dict(self.source, tested='d'*40, tree='e'*40)
        changed = cache.general_plan(self.context, new_source, self.root)
        # Content-addressed Go compilation may be reused across source trees;
        # current source is checked independently and bodies always run fresh.
        self.assertEqual(changed.seed, self.plan.seed)
        cache.validate_manifest(changed, self.context)
        for change in ({'compiler': 'different'}, {'flags': '-race -tags=special'}):
            context = dict(self.context, **change)
            other = cache.general_plan(context, self.source, self.root)
            self.assertNotEqual(other.seed, self.plan.seed)
            with self.assertRaises(keys.InvalidContext):
                cache.validate_manifest(other, context)
        other = cache.general_plan(self.context, dict(self.source, dependency='d'*64), self.root)
        self.assertNotEqual(other.seed, self.plan.seed)
        with self.assertRaises(keys.InvalidContext):
            cache.validate_manifest(other, self.context)

    def test_restored_module_verification_retries_cold_once(self):
        cache.publish_manifest(self.plan, self.context)
        calls = []
        def command(argv):
            calls.append(argv)
            if len(calls) == 2:
                raise subprocess.CalledProcessError(1, argv)
        self.assertEqual(cache.restore_compilation(self.plan, self.context, 'success', self.plan.seed,
                                                  lambda source: None, command), 'corrupt-rebuilt')
        self.assertEqual(calls, [['go', 'mod', 'download'], ['go', 'mod', 'verify']]*2)
        self.assertFalse((Path(self.plan.paths[0])/'object').exists())

    def test_malformed_manifest_links_and_oversize_cannot_publish(self):
        manifest = Path(self.plan.paths[0])/cache.MANIFEST
        manifest.write_text('{broken')
        self.assertEqual(self.call(), 'miss')
        outside = self.root/'outside'
        outside.write_bytes(b'do not remove')
        (Path(self.plan.paths[0])/'link').symlink_to(outside)
        with self.assertRaises(keys.InvalidContext):
            cache.publish_manifest(self.plan, self.context)
        self.assertEqual(self.call(), 'miss')
        self.assertEqual(outside.read_bytes(), b'do not remove')
        (Path(self.plan.paths[0])/'big').write_bytes(b'12345')
        with mock.patch.object(cache, 'MAX_BYTES', 4), self.assertRaises(keys.InvalidContext):
            cache.publish_manifest(self.plan, self.context)

    def test_storage_bound_prunes_only_recoverable_go_objects(self):
        build = Path(self.plan.paths[0])
        ordinary = build/'ab'
        ordinary.mkdir()
        discard = ordinary/('a'*64+'-d')
        discard.write_bytes(b'x'*200)
        tools = build/'aa-pinned-tools'
        tools.mkdir()
        tool = tools/'deadcode'
        tool.write_bytes(b'pinned tool')
        with mock.patch.object(cache, 'MAX_BYTES', 100):
            removed = cache.bound_compilation(self.plan)
        self.assertEqual(removed, 200)
        self.assertFalse(discard.exists())
        self.assertEqual(tool.read_bytes(), b'pinned tool')
        self.assertEqual((Path(self.plan.paths[1])/'cache/module').read_bytes(), b'verified dependency')

    def test_snapshot_kinds_separate_modules_from_pr_compilation(self):
        cache.publish_manifest(self.plan, self.context, kind='compilation')
        (Path(self.plan.paths[1])/'cache/module').write_bytes(b'fresh modules are verified separately')
        cache.validate_manifest(self.plan, self.context, kind='compilation')
        manifest = json.loads((Path(self.plan.paths[0])/cache.MANIFEST).read_text())
        self.assertEqual(manifest['kind'], 'compilation')
        self.assertTrue(all(key.startswith('0/') for key in manifest['files']))
        cache.publish_manifest(self.plan, self.context, kind='seed')
        manifest = json.loads((Path(self.plan.paths[0])/cache.MANIFEST).read_text())
        self.assertTrue(any(key.startswith('1/') for key in manifest['files']))
        with self.assertRaises(keys.InvalidContext):
            cache.snapshot_paths(self.plan, 'unreviewed')

    def test_pr_worker_trusted_seed_hit_and_corrupt_fallback(self):
        cache.publish_manifest(self.plan, self.context)
        plan = replace(self.plan, primary='pr-key', prefix='pr-prefix-')
        self.assertEqual(restore_result(plan, 'success', self.plan.seed, 'false').state, 'trusted')
        calls = []
        self.assertEqual(verify_restored(plan, 'success', self.plan.seed, 'false',
                                         lambda source: None, lambda argv: calls.append(argv),
                                         self.context), 'trusted')
        self.assertEqual(calls, [['go', 'mod', 'download'], ['go', 'mod', 'verify']])
        (Path(self.plan.paths[1])/'cache/module').write_bytes(b'changed')
        self.assertEqual(verify_restored(plan, 'success', self.plan.seed, 'false',
                                         lambda source: None, lambda argv: None,
                                         self.context), 'corrupt-rebuilt')

    def test_one_canonical_schema_rejects_unknown_missing_policy_or_go_fields(self):
        from test_go_shards import synthetic_vet_context
        context = synthetic_vet_context()
        self.assertEqual(keys.validate_context(context), context)
        for changed in (dict(context, unknown='unreviewed'),
                        {k:v for k,v in context.items() if k != 'pinned_tools'},
                        dict(context, pinned_tools={}),
                        dict(context, compilation_modes={}),
                        dict(context, schema='aa-pr-go-v1'),
                        dict(context, go=dict(context['go'], UNREVIEWED='yes'))):
            with self.assertRaises(keys.InvalidContext):
                keys.validate_context(changed)

    def test_trusted_writes_require_real_push_main_never_fork_or_pr_target(self):
        env = {'GITHUB_EVENT_NAME': 'push', 'GITHUB_REF': 'refs/heads/main',
               'GITHUB_SHA': 'a'*40, 'GITHUB_REPOSITORY': 'wangjohn/agent-archive'}
        event = {'ref': env['GITHUB_REF'], 'after': env['GITHUB_SHA'], 'deleted': False,
                 'repository': {'full_name': env['GITHUB_REPOSITORY']}}
        self.assertTrue(cache.trusted_main(env, event))
        for changes in ({'GITHUB_EVENT_NAME': 'pull_request'},
                        {'GITHUB_EVENT_NAME': 'pull_request_target'},
                        {'GITHUB_EVENT_NAME': 'workflow_dispatch'},
                        {'GITHUB_REF': 'refs/heads/fork'}, {'GITHUB_SHA': 'b'*40}):
            self.assertFalse(cache.trusted_main(dict(env, **changes), event))
        self.assertFalse(cache.trusted_main(env, dict(event, repository={'full_name': 'attacker/fork'})))


class NativeIdentityTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name).resolve()
        self.release = self.root/'os-release'
        self.release.write_text('ID=ubuntu\nVERSION_ID="24.04"\n')
        self.go = metadata()
        self.env = {'RUNNER_OS':'Linux','RUNNER_ARCH':'X64',
                    'ImageOS':'ubuntu24','ImageVersion':'20261001.1.0'}

    def test_sdk_xcode_and_compiler_bytes_are_native_identity(self):
        self.go.update(GOOS='darwin', GOHOSTOS='darwin', GOARCH='arm64', GOHOSTARCH='arm64')
        self.env.update(RUNNER_OS='macOS', RUNNER_ARCH='ARM64', ImageOS='macos14')
        sdk = [b'1']
        def command(argv):
            if argv == ['go', 'version']:
                return b'go version go1.27.2 darwin/arm64\n'
            if argv[:3] == ['go', 'env', '-json']:
                return json.dumps(self.go).encode()
            return ' '.join(argv).encode()+sdk[0]
        def context():
            return keys.runtime_context(command, self.env, self.release,
                                        compiler=lambda name: {'binary': name},
                                        toolchain=lambda go: {'compiler': 'bytes'}, native=True)
        first = context()
        self.assertEqual(set(first['sdk']), {'xcode', 'developer', 'sdk_path', 'sdk_version', 'sdk_build', 'overrides'})
        sdk[0] = b'2'
        self.assertNotEqual(keys.digest(first), keys.digest(context()))
        # Legacy Linux receipt callers do not silently accept Darwin.
        with self.assertRaises(keys.InvalidContext):
            keys.runtime_context(command, self.env, self.release)


class ToolTests(unittest.TestCase):
    def test_buildinfo_and_recorded_bytes_reject_wrong_tool(self):
        with tempfile.TemporaryDirectory() as tmp:
            binary = Path(tmp)/'deadcode'
            binary.write_bytes(b'one')
            binary.chmod(0o755)
            metadata = [b'BINARY: go1.27.2\n\tpath\tgolang.org/x/tools/cmd/deadcode\n\tmod\tgolang.org/x/tools\tv0.50.0\th1:abc\n\tbuild\tGOOS=linux\n\tbuild\tGOARCH=amd64\n\tbuild\tCGO_ENABLED=1\n']
            def command(argv, **kwargs):
                if argv[1:3] == ['version', '-m']:
                    return metadata[0]
                return json.dumps({'GOOS': 'linux', 'GOARCH': 'amd64', 'CGO_ENABLED': '1'}).encode()
            first = verified_tools.inspect(binary, 'deadcode', command)
            binary.write_bytes(b'two')
            self.assertNotEqual(first['sha256'], verified_tools.inspect(binary, 'deadcode', command)['sha256'])
            for change in (b'go1.27.3', b'v0.49.0', b'other/tools', b'CGO_ENABLED=0'):
                old = metadata[0]
                needle = (b'go1.27.2' if change == b'go1.27.3' else b'v0.50.0' if change == b'v0.49.0'
                          else b'golang.org/x/tools' if change == b'other/tools' else b'CGO_ENABLED=1')
                metadata[0] = old.replace(needle, change)
                with self.assertRaises(keys.InvalidContext):
                    verified_tools.inspect(binary, 'deadcode', command)
                metadata[0] = old

    def test_pinned_tool_hit_avoids_install_and_corrupt_binary_rebuilds(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp).resolve()
            installs = []
            def command(argv, **kwargs):
                if argv[:2] == ['go', 'install']:
                    installs.append(argv)
                    binary = Path(kwargs['env']['GOBIN'])/'deadcode'
                    binary.write_bytes(b'built')
                    binary.chmod(0o755)
                    return b''
                if argv[:3] == ['go', 'version', '-m']:
                    return b'BINARY: go1.27.2\n\tpath\tgolang.org/x/tools/cmd/deadcode\n\tmod\tgolang.org/x/tools\tv0.50.0\th1:abc\n\tbuild\tGOOS=linux\n\tbuild\tGOARCH=amd64\n\tbuild\tCGO_ENABLED=1\n'
                return b'{"GOOS":"linux","GOARCH":"amd64","CGO_ENABLED":"1"}'
            binary = verified_tools.ensure('deadcode', root, command)
            self.assertEqual(len(installs), 1)
            self.assertEqual(verified_tools.ensure('deadcode', root, command), binary)
            self.assertEqual(len(installs), 1)
            binary.write_bytes(b'wrong executable bytes')
            verified_tools.ensure('deadcode', root, command)
            self.assertEqual(len(installs), 2)
            receipt = json.loads((root/'deadcode.json').read_text())
            self.assertEqual(receipt['version'], 'v0.50.0')

    def test_shared_module_graphs_and_builds_are_exact_and_never_run_checks(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            calls = []
            def command(argv, **kwargs):
                calls.append((argv, kwargs))
            proof = {'tested': keys.TOOL_PINS['shared']}
            with mock.patch.object(warm_shared_tools, 'shared_identity', return_value=proof):
                warm_shared_tools.warm(root, root/'outside-tool', command)
            builds = [(argv, options) for argv, options in calls if argv[:2] == ['go','build']]
            self.assertEqual(len(builds),4)
            self.assertEqual([options['cwd'] for _,options in builds],
                             [root/directory for directory,_ in warm_shared_tools.MODULES])
            self.assertEqual(sum(argv == ['go','mod','verify'] for argv,_ in calls),4)
            self.assertTrue(all('verify' not in argv[0] for argv,_ in calls))
            self.assertEqual(builds[0][1]['env']['CGO_ENABLED'],'0')
            self.assertEqual(builds[0][1]['env']['GOFLAGS'],'-mod=readonly -buildvcs=false')

    def test_git_cache_hit_validates_actual_executable_and_checksum(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            cache_dir = root/'cache'
            cache_dir.mkdir()
            installation = root/'initial'
            (installation/'bin').mkdir(parents=True)
            git = installation/'bin/git'
            git.write_text('#!/bin/sh\necho "git version 2.39.2"\n')
            git.chmod(0o755)
            archive = cache_dir/'upstream-2.39.2.tar.gz'
            with tarfile.open(archive,'w:gz') as handle:
                handle.add(installation/'bin',arcname='bin')
            tag,commit,expected = pinned_git.PINS['upstream-2.39.2']
            import hashlib
            receipt = {'tag':tag,'commit':commit,'expected':expected,'flags':pinned_git.FLAGS,
                       'sha256':hashlib.sha256(archive.read_bytes()).hexdigest()}
            (cache_dir/'upstream-2.39.2.json').write_text(json.dumps(receipt))
            binary_dir = pinned_git.ensure('upstream-2.39.2', cache_dir, root/'install')
            self.assertEqual(subprocess.check_output([str(binary_dir/'git'),'--version']).decode().strip(),expected)
            self.assertEqual(binary_dir,root/'install/bin')

    def test_git_archive_rejects_traversal_devices_and_symlink_children(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            for name, kind, link in [('../escape', tarfile.REGTYPE, ''),
                                      ('bad', tarfile.CHRTYPE, ''),
                                      ('link', tarfile.SYMTYPE, '../escape')]:
                archive = root/'bad.tar.gz'
                with tarfile.open(archive, 'w:gz') as handle:
                    member = tarfile.TarInfo(name)
                    member.type, member.linkname = kind, link
                    handle.addfile(member)
                with self.assertRaises(keys.InvalidContext):
                    pinned_git.extract(archive, root/'install')


class WorkflowTests(unittest.TestCase):
    def test_seed_writes_are_main_only_and_only_two_standard_jobs(self):
        text = (ROOT/'.github/workflows/compilation-seeds.yml').read_text()
        self.assertIn('branches: [main]', text)
        self.assertNotIn('pull_request:', text)
        self.assertNotIn('pull_request_target:', text)
        self.assertNotIn('schedule:', text)
        self.assertEqual(text.count('          - os:'), 3)
        self.assertIn('profile: shared', text)
        self.assertIn('profile: product', text)
        self.assertIn('scripts/ci_cache.py complete', text)
        self.assertIn("steps.complete.outputs.save_ready == 'true'", text)
        self.assertNotIn('retention-days:', text)  # Caches use default repo 10 GB LRU.

    def test_integrated_cross_lanes_static_registration_and_reference_context(self):
        workflow = jobs((ROOT/'.github/workflows/test.yml').read_text())
        for name in ('cross-linux-arm64','cross-darwin-nocgo','cross-linux-release'):
            lane = workflow[name]
            self.assertIn('uses: ./.github/actions/verified-go-cache',lane)
            for setting in ('GOTOOLCHAIN: local',"GOWORK: 'off'","GOFLAGS: '-mod=readonly'"):
                self.assertIn(setting,lane)
        static = workflow['script-static']
        for script in ('test_go_cache_context.py','test_go_cache_steps.py','test_ci_cache.py',
                       'test_go_parent_shards.py','test_pr_reference.py'):
            self.assertIn('scripts/'+script,static)
        reference = jobs((ROOT/'.github/workflows/extended.yml').read_text())['pr-reference']
        for setting in ("GOENV: 'off'",'GOTOOLCHAIN: local',"GOWORK: 'off'","GOFLAGS: '-mod=readonly'"):
            self.assertIn(setting,reference)

    def test_release_scanner_uses_product_toolchain_before_strict_cache(self):
        import re
        from test_release_assets import steps
        lane = jobs((ROOT/'.github/workflows/levenshtein.yml').read_text())['native-release-vulnerabilities']
        compiler = re.findall(r'^toolchain go(\S+)$', (ROOT/'go.mod').read_text(), re.M)
        self.assertEqual(compiler, ['1.27.2'])
        setup = next(step for step in steps(lane) if 'uses: actions/setup-go@' in step)
        self.assertIn("go-version: '"+compiler[0]+"'", setup)
        self.assertNotIn('go-version-file:', setup)
        self.assertLess(lane.index('uses: actions/setup-go@'), lane.index('uses: ./app/.github/actions/verified-go-cache'))
        self.assertIn('scripts/stage_release_vulnerability_scan.py --source ./app', lane)
        self.assertIn('--destination "$RUNNER_TEMP/release-vuln-source" --shared ./levenshtein', lane)
        self.assertIn('./levenshtein/verify native-release-vulnerabilities', lane)
        self.assertIn('--source "$RUNNER_TEMP/release-vuln-source" --jobs 1', lane)
        self.assertIn('--cache-dir "$RUNNER_TEMP/release-vuln-verification"', lane)

    def test_composite_leaves_environment_ownership_to_every_caller(self):
        from test_release_assets import steps
        composite = (ROOT/'.github/actions/verified-go-cache/action.yml').read_text()
        self.assertNotIn('GITHUB_ENV', composite)
        consumers = 0
        for filename in ('test.yml', 'levenshtein.yml', 'compilation-seeds.yml'):
            workflow = jobs((ROOT/'.github/workflows'/filename).read_text())
            for lane in workflow.values():
                found = steps(lane)
                for index, step in enumerate(found):
                    if '/.github/actions/verified-go-cache' not in step:
                        continue
                    consumers += 1
                    previous = found[index-1]
                    self.assertIn('Set controlled compilation/module paths', previous)
                    self.assertIn('GOCACHE=%s/pr-go-build', previous)
                    self.assertIn('GOMODCACHE=%s/pr-go-modules', previous)
                    self.assertIn('"$RUNNER_TEMP" "$RUNNER_TEMP" >> "$GITHUB_ENV"', previous)
        self.assertEqual(consumers, 11)

    def test_every_hit_still_executes_current_checks_and_forks_never_save(self):
        workflow = jobs((ROOT/'.github/workflows/test.yml').read_text())
        short = workflow['linux-short']
        self.assertIn('matrix.shard == 0', short)
        self.assertIn('github.event.pull_request.head.repo.full_name == github.repository', short)
        self.assertIn('steps.go-cache-key.outputs.seed', short)
        lint = workflow['lint']
        self.assertIn('scripts/verified_tools.py golangci-lint', lint)
        self.assertIn('run --disable=revive', lint)
        self.assertIn('run --enable-only=revive', lint)
        self.assertIn('check macos "$shared"', lint)
        self.assertIn('GOOS=linux CGO_ENABLED=0 check linux', lint)
        shared = (ROOT/'.github/workflows/levenshtein.yml').read_text()
        self.assertIn('uses: ./app/.github/actions/verified-go-cache', shared)
        self.assertIn('--cache-dir "$RUNNER_TEMP/verification"', shared)
        self.assertIn('--cache-dir "$RUNNER_TEMP/release-vuln-verification"', shared)
        composite = (ROOT/'.github/actions/verified-go-cache/action.yml').read_text()
        self.assertNotIn('actions/cache/save', composite)
        self.assertNotIn('XDG', '\n'.join(line for line in composite.splitlines() if 'path:' in line))


if __name__ == '__main__':
    unittest.main()
