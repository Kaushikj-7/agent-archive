"""Execute every documented bucket-purge block against a disposable fake bucket."""
import argparse
import itertools
import json
import os
from pathlib import Path
import re
import shutil
import stat
import sys
import subprocess
import tempfile
import unittest

DOCS = Path(__file__).resolve().parent.parent / 'docs'
TIER = 'full'
CASE_EVIDENCE = []

RECIPE_PAGES = [DOCS / 'security' / 'archive-cleanup.md', DOCS / 'getting-started' / 'uninstall.md']

FAKE_AWS = r'''#!/bin/sh
cat > /dev/null
root=$FAKE_S3
bump() {
  counter="$FAKE_S3_STATE.$1"
  n=0; [ ! -f "$counter" ] || n=$(cat "$counter")
  n=$((n + 1)); printf '%s\n' "$n" > "$counter"
  COUNT=$n
}
object() { printf '%s/%s' "$root" "${1#s3://}"; }
case "$1 $2" in
  "s3api list-objects-v2")
    bump list
    [ "$COUNT" != "${FAKE_LIST_FAIL_AT:-0}" ] || { echo 'fake listing failure' >&2; exit 1; }
    shift 2
    while [ $# -gt 0 ]; do
      case "$1" in
        --bucket) bucket=$2; shift 2 ;;
        --prefix) prefix=$2; shift 2 ;;
        --query|--output) shift 2 ;;
        *) echo "unexpected aws option $1" >&2; exit 2 ;;
      esac
    done
    if [ "$COUNT" = "${FAKE_LIST_BAD_AT:-0}" ]; then
      echo '{"broken":true}'
    else
      (cd "$root/$bucket" && find . -type f ! -name '*.unreadable' | sed 's|^./||' |
        awk -v p="$prefix" 'index($0,p)==1' | LC_ALL=C sort) |
        jq -R -s 'split("\n")[:-1] | if length == 0 then null else . end'
    fi
    ;;
  "s3 cp")
    bump cp
    f=$(object "$3")
    [ "$COUNT" != "${FAKE_CP_FAIL_AT:-0}" ] && [ -f "$f" ] && [ ! -f "$f.unreadable" ] ||
      { echo "fake metadata read failure: $3" >&2; exit 1; }
    cat "$f"
    ;;
  "s3 rm")
    bump rm
    f=$(object "$3")
    printf '%s\n' "${3#s3://}" >> "$FAKE_S3_ATTEMPTS"
    [ "$COUNT" != "${FAKE_RM_FAIL_AT:-0}" ] || { echo "fake deletion failure: $3" >&2; exit 1; }
    rm -f "$f" || exit 1
    if [ "$COUNT" = "${FAKE_RM_CRASH_AT:-0}" ]; then
      kill -KILL "$PPID"
      exit 99
    fi
    printf '%s\n' "${3#s3://}" >> "$FAKE_S3_REMOVED"
    ;;
  *) echo "unexpected aws call: $*" >&2; exit 2 ;;
esac
'''
FAKE_AGENT_ARCHIVE = r'''#!/bin/sh
echo "$1" >> "$FAKE_AGENT_ARCHIVE_LOG"

if [ "$1" = pause ] && [ -n "${FAKE_INSTALL_STATE:-}" ]; then
  echo "fake installation $FAKE_INSTALL_STATE; pause refuses" >&2
  exit 1
fi
if [ "$1" = uninstall ]; then
  printf '%s\n' "$*" > "$FAKE_S3/../uninstall-args.log"
  [ "$#" = 2 ] && [ "$2" = --yes ] || { echo 'unsupported uninstall arguments' >&2; exit 2; }
  if [ -n "${FAKE_UNINSTALL_FAIL:-}" ]; then
    if [ -n "${FAKE_WRITER_KEY:-}" ]; then
      printf 'in-flight source\n' > "$FAKE_S3/my-archive-bucket/$FAKE_WRITER_KEY"
    fi
    echo "fake uninstall $FAKE_UNINSTALL_FAIL" >&2
    exit 1
  fi
fi
if [ "$(wc -l < "$FAKE_AGENT_ARCHIVE_LOG" | tr -d ' ')" = "${FAKE_PAUSE_FAIL_AT:-0}" ]; then
  if [ -n "${FAKE_WRITER_KEY:-}" ]; then
    printf 'in-flight source\n' > "$FAKE_S3/my-archive-bucket/$FAKE_WRITER_KEY"
  fi
  echo 'fake lock holder still uploading' >&2
  exit 1
fi
'''


def recipe_blocks():
    blocks = {}
    for page in RECIPE_PAGES:
        for match in re.finditer(r'<!-- purge-recipe:([a-z-]+)[^>]*-->\n```sh\n(.*?)```', page.read_text(), re.S):
            blocks[match.group(1)] = match.group(2)
    return blocks


def write_executable(path, body):
    path.write_text(body)
    path.chmod(path.stat().st_mode | stat.S_IXUSR)


def resume_cases(tier):
    """Only reviewed repetitions move cadence; every mode retains failure/restart."""
    full = [(prefix, mode, prepare, boundary)
            for prefix in ('agent-archive/', 'nested/archive/', '')
            for mode, prepare, count in [('unreferenced', 'unreferenced', 3),
                                        ('machine', 'machine "$machine"', 6),
                                        ('old', 'old 10', 4), ('all', 'all', 9)]
            for boundary in range(1, count + (1 if mode == 'all' and not prefix else 0) + 1)]
    if tier == 'full':
        return full, []
    selected = []
    for case in full:
        prefix, mode, prepare, boundary = case
        count = {'unreferenced': 3, 'machine': 6, 'old': 4, 'all': 9}[mode]
        maximum = count + (1 if mode == 'all' and not prefix else 0)
        boundaries = {1, maximum} if tier == 'pr' else {1, (maximum+1)//2, maximum}
        if boundary in boundaries and (tier == 'daily' or prefix == 'agent-archive/'):
            selected.append(case)
    return selected, [case for case in full if case not in selected]


class EvidenceResult(unittest.TextTestResult):
    def addSubTest(self, test, subtest, err):
        super().addSubTest(test, subtest, err)
        CASE_EVIDENCE.append({'test': test.id(), 'case': dict(subtest.params),
                              'status': 'pass' if err is None else 'fail'})

    def addSuccess(self, test):
        super().addSuccess(test)
        CASE_EVIDENCE.append({'test': test.id(), 'status': 'pass'})

    def addSkip(self, test, reason):
        super().addSkip(test, reason)
        CASE_EVIDENCE.append({'test': test.id(), 'status': 'skip', 'reason': reason})

    def addFailure(self, test, err):
        super().addFailure(test, err)
        CASE_EVIDENCE.append({'test': test.id(), 'status': 'fail'})

    def addError(self, test, err):
        super().addError(test, err)
        CASE_EVIDENCE.append({'test': test.id(), 'status': 'fail'})


class PurgeRecipeTest(unittest.TestCase):
    shells = [s for s in ('bash', 'zsh') if shutil.which(s)]

    def setUp(self):
        if not shutil.which('jq'):
            self.skipTest('jq is not installed')
        self.blocks = recipe_blocks()
        self.assertEqual(sorted(self.blocks), ['all', 'delete', 'list', 'machine', 'old-sessions'])
        self.root = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, self.root)

    def build(self, prefix='agent-archive/', sessions=None, extra=(), unreadable=()):
        sessions = self.sessions() if sessions is None else sessions
        bucket = self.root / 's3' / 'my-archive-bucket'
        if bucket.exists():
            shutil.rmtree(bucket)
        for directory, (current, others, version, *machine) in sessions.items():
            base = bucket / prefix / 'sessions' / directory
            base.mkdir(parents=True)
            for name in ([current] if current else []) + others:
                (base / name).write_bytes(b'gz')
            if current:
                metadata = {'schema_version': 1, 'filter_version': str(version), 'machine_id': (machine or ['m-other'])[0],
                            'source_bundle': {'key': f'sessions/{directory}/{current}'}}
                (base / 'metadata.json').write_text(json.dumps(metadata))
        for key in extra:
            path = bucket / key
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text('x')
        for key in unreadable:
            (bucket / (key + '.unreadable')).write_text('')
        return bucket

    def run_recipe(self, shell, prefix, names, between=None, env_extra=None, tail=None):
        for name in ('state.json', 'attempts.log', 'removed.log', 'agent-archive.log'):
            (self.root / name).unlink(missing_ok=True)
        for path in self.root.glob('state.json.*'):
            path.unlink()
        bin_dir = self.root / 'bin'
        bin_dir.mkdir(exist_ok=True)
        write_executable(bin_dir / 'aws', FAKE_AWS)
        write_executable(bin_dir / 'agent-archive', FAKE_AGENT_ARCHIVE)
        marker_rm = bin_dir / 'rm'
        if (env_extra or {}).get('FAKE_MARKER_RM_FAIL'):
            write_executable(marker_rm, '#!/bin/sh\ncase "$1" in */VALID) exit 1 ;; esac\nexec /bin/rm "$@"\n')
        else:
            marker_rm.unlink(missing_ok=True)
        work = self.root / 'work'
        if work.exists():
            shutil.rmtree(work)
        work.mkdir()
        isolated_home = self.root / 'home'
        isolated_home.mkdir(exist_ok=True)
        isolated_tmp = self.root / 'tmp'
        isolated_tmp.mkdir(exist_ok=True)
        script = '\n'.join(self.blocks[name] for name in names)
        if tail:
            script += '\n' + tail
        if between:
            script = script.replace('purge_apply\n', between + '\npurge_apply\n', 1)
        script = script.replace('prefix=agent-archive/ ', f'prefix={prefix} ')
        script = script.replace('machine=0123456789abcdef0123456789abcdef', 'machine=m-retired')
        helper = work / 'helpers.sh'
        helper.write_text(self.blocks['list'].rsplit('purge_prepare unreferenced', 1)[0])
        env = dict(os.environ,
                   PATH=f'{bin_dir}{os.pathsep}{os.environ["PATH"]}',
                   HOME=str(isolated_home), TMPDIR=str(isolated_tmp),
                   FAKE_HELPERS=str(helper), FAKE_SHELL=shutil.which(shell),
                   AWS_CONFIG_FILE=str(self.root / 'aws-config'),
                   AWS_SHARED_CREDENTIALS_FILE=str(self.root / 'no-credentials'),
                   FAKE_S3=str(self.root / 's3'),
                   FAKE_S3_STATE=str(self.root / 'state.json'),
                   FAKE_S3_ATTEMPTS=str(self.root / 'attempts.log'),
                   FAKE_S3_REMOVED=str(self.root / 'removed.log'),
                   FAKE_AGENT_ARCHIVE_LOG=str(self.root / 'agent-archive.log'))
        env.update(env_extra or {})
        return subprocess.run([shell, '-c', script], cwd=work, env=env, stdin=subprocess.DEVNULL,
                              capture_output=True, text=True)

    @staticmethod
    def keys(bucket):
        return sorted(str(p.relative_to(bucket)) for p in bucket.rglob('*') if p.is_file() and not p.name.endswith('.unreadable'))

    def attempts(self):
        path = self.root / 'attempts.log'
        return path.read_text().splitlines() if path.exists() else []

    def removed(self):
        path = self.root / 'removed.log'
        return path.read_text().splitlines() if path.exists() else []

    def sessions(self):
        return {
            'claude/aaaa': ('source.a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2.jsonl.gz', ['source.a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1.jsonl.gz', 'source.a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0.jsonl.gz'], 9, 'm-retired'),
            'codex/bbbb': ('source.b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1.jsonl.gz', [], 10, 'm-current'),
            'cursor/cccc': (None, ['source.c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1.jsonl.gz'], 0),
            'cursor/dddd': ('source.d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1.jsonl.gz', [], 10, 'm-retired'),
        }

    def history_metadata(self, bucket, prefix):
        path = bucket / prefix / 'sessions/codex/bbbb/metadata.json'
        metadata = json.loads(path.read_text())
        metadata['schema_version'] = 2
        metadata['history'] = {'current_revision': '11111111-1111-4111-8111-111111111111',
                               'preserved': []}
        current = metadata['source_bundle']
        current['sha256'] = Path(current['key']).name.split('.')[1]
        current['compressed_bytes'] = 2
        for index, hex_digit in enumerate(('e', 'f'), start=2):
            name = 'source.' + hex_digit * 64 + '.jsonl.gz'
            (path.parent / name).write_bytes(b'gz')
            metadata['history']['preserved'].append({
                'revision_id': str(index) * 8 + '-' + str(index) * 4 + '-4' + str(index) * 3 + '-8' + str(index) * 3 + '-' + str(index) * 12,
                'captured_at': '2024-02-29T12:00:00.123+05:30', 'filter_version': '9',
                'source': {'key': 'sessions/codex/bbbb/' + name,
                           'sha256': hex_digit * 64, 'compressed_bytes': 2}})
        path.write_text(json.dumps(metadata))
        return path, metadata

    def test_preserved_revisions_survive_unreferenced_cleanup(self):
        for shell in self.shells:
            for prefix in ('agent-archive/', 'nested/archive/', ''):
                with self.subTest(shell=shell, prefix=prefix):
                    bucket = self.build(prefix)
                    self.history_metadata(bucket, prefix)
                    result = self.run_recipe(shell, prefix, ['list', 'delete'])
                    self.assertEqual(result.returncode, 0, result.stderr)
                    for key in ('e', 'f'):
                        self.assertIn(prefix + 'sessions/codex/bbbb/source.' + key * 64 + '.jsonl.gz', self.keys(bucket))
                    self.assertEqual(len(self.attempts()), 3)

    def test_invalid_history_blocks_selective_cleanup(self):
        for shell in self.shells:
            for invalid in ('missing', 'cross-session', 'duplicate', 'revision', 'digest', 'schema', 'false-preserved', 'invalid-time', 'invalid-day', 'fractional-size', 'false-source-schema'):
                with self.subTest(shell=shell, invalid=invalid):
                    bucket = self.build()
                    path, metadata = self.history_metadata(bucket, 'agent-archive/')
                    refs = metadata['history']['preserved']
                    if invalid == 'missing':
                        (bucket / 'agent-archive' / refs[0]['source']['key']).unlink()
                    elif invalid == 'cross-session':
                        refs[0]['source']['key'] = refs[0]['source']['key'].replace('/bbbb/', '/aaaa/')
                    elif invalid == 'duplicate':
                        refs[1]['source'] = refs[0]['source']
                    elif invalid == 'revision':
                        refs[0]['revision_id'] = 'invalid'
                    elif invalid == 'digest':
                        refs[0]['source']['sha256'] = 'a' * 64
                    elif invalid == 'schema':
                        metadata['schema_version'] = 1
                    elif invalid == 'false-preserved':
                        metadata['history']['preserved'] = False
                    elif invalid == 'invalid-time':
                        refs[0]['captured_at'] = '2026-10-01Tinvalid'
                    elif invalid == 'invalid-day':
                        refs[0]['captured_at'] = '2026-02-30T12:00:00Z'
                    elif invalid == 'fractional-size':
                        refs[0]['source']['compressed_bytes'] = 2.5
                    elif invalid == 'false-source-schema':
                        refs[0]['source_schema_version'] = False
                    path.write_text(json.dumps(metadata))
                    before = self.keys(bucket)
                    result = self.run_recipe(shell, 'agent-archive/', ['list', 'delete'])
                    self.assertNotEqual(result.returncode, 0)
                    self.assertEqual(self.attempts(), [])
                    self.assertEqual(self.keys(bucket), before)

    def test_new_preserved_reference_invalidates_plan(self):
        for shell in self.shells:
            with self.subTest(shell=shell):
                bucket = self.build()
                path, metadata = self.history_metadata(bucket, 'agent-archive/')
                before = self.keys(bucket)
                source = 'sessions/codex/bbbb/source.' + 'a' * 64 + '.jsonl.gz'
                (bucket / 'agent-archive' / source).write_bytes(b'gz')
                before = self.keys(bucket)
                metadata['history']['preserved'].append({
                    'revision_id': '44444444-4444-4444-8444-444444444444',
                    'captured_at': '2026-10-01T12:00:00Z', 'filter_version': '8',
                    'source': {'key': source, 'sha256': 'a' * 64, 'compressed_bytes': 2}})
                replacement = self.root / 'replacement.json'
                replacement.write_text(json.dumps(metadata))
                result = self.run_recipe(shell, 'agent-archive/', ['list', 'delete'],
                    between='cp "' + str(replacement) + '" "' + str(path) + '"')
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(self.attempts(), [])
                self.assertEqual(self.keys(bucket), before)

    def test_unreferenced_sources_keep_current_and_scope(self):
        for shell in self.shells:
            for prefix in ('agent-archive/', 'nested/archive/', ''):
                with self.subTest(shell=shell, prefix=prefix):
                    bucket = self.build(prefix, extra=[f'{prefix}.setup-test/x.json',
                        'other/sessions/claude/aaaa/source.zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz.jsonl.gz', f'{prefix}sessions/claude/aaaa/notes.txt'])
                    result = self.run_recipe(shell, prefix, ['list', 'delete'])
                    self.assertEqual(result.returncode, 0, result.stderr)
                    remaining = self.keys(bucket)
                    for key in remaining:
                        self.assertNotIn(key, [f'{prefix}sessions/claude/aaaa/source.a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0.jsonl.gz',
                                               f'{prefix}sessions/claude/aaaa/source.a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1.jsonl.gz',
                                               f'{prefix}sessions/cursor/cccc/source.c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1.jsonl.gz'])
                    self.assertIn(f'{prefix}sessions/claude/aaaa/source.a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2.jsonl.gz', remaining)
                    self.assertIn('other/sessions/claude/aaaa/source.zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz.jsonl.gz', remaining)
                    self.assertTrue(all(k.startswith(f'my-archive-bucket/{prefix}sessions/') for k in self.removed()))

    def test_preflight_failures_delete_nothing(self):
        for shell in self.shells:
            for mode, names in [('source', ['list', 'delete']), ('old', ['list', 'old-sessions', 'delete']),
                                ('machine', ['list', 'machine', 'delete'])]:
                for failure in ('listing', 'bad-listing', 'unreadable', 'malformed',
                                'empty-metadata', 'multi-metadata', 'missing-source'):
                    with self.subTest(shell=shell, mode=mode, failure=failure):
                        bucket = self.build()
                        later = bucket / 'agent-archive/sessions/cursor/dddd/metadata.json'
                        env = {}
                        if failure == 'listing':
                            env['FAKE_LIST_FAIL_AT'] = '2' if mode != 'source' else '1'
                        elif failure == 'bad-listing':
                            env['FAKE_LIST_BAD_AT'] = '2' if mode != 'source' else '1'
                        elif failure == 'unreadable':
                            Path(str(later) + '.unreadable').write_text('')
                        elif failure == 'malformed':
                            later.write_text('{broken')
                        elif failure == 'empty-metadata':
                            later.write_text('')
                        elif failure == 'multi-metadata':
                            later.write_text(later.read_text() + '\n' + later.read_text())
                        else:
                            metadata = json.loads(later.read_text())
                            del metadata['source_bundle']['key']
                            later.write_text(json.dumps(metadata))
                        before = self.keys(bucket)
                        result = self.run_recipe(shell, 'agent-archive/', names, env_extra=env)
                        self.assertNotEqual(result.returncode, 0, result.stderr)
                        self.assertEqual(self.keys(bucket), before)
                        self.assertEqual(self.attempts(), [], result.stderr)

    def test_failed_pause_invalidates_previous_plan_and_blocks_every_mode(self):
        for shell in self.shells:
            for writer in (False, True):
                for mode in ('unreferenced', 'all', 'old 10', 'machine m-retired'):
                    with self.subTest(shell=shell, writer=writer, mode=mode):
                        bucket = self.build()
                        before = self.keys(bucket)
                        env = {'FAKE_PAUSE_FAIL_AT': '2'}
                        writer_key = 'agent-archive/sessions/codex/bbbb/source.' + 'e' * 64 + '.jsonl.gz'
                        if writer:
                            env['FAKE_WRITER_KEY'] = writer_key
                        # Reexecute preparation after an earlier successful plan in the same shell.
                        tail = 'purge_prepare ' + mode + '\npurge_apply'
                        result = self.run_recipe(shell, 'agent-archive/', ['list', 'list'],
                                                 env_extra=env, tail=tail)
                        self.assertNotEqual(result.returncode, 0, result.stderr)
                        self.assertEqual(self.attempts(), [])
                        self.assertEqual(self.keys(bucket), sorted(before + ([writer_key] if writer else [])))
                        self.assertIn('lock holder', result.stderr)
                        self.assertEqual(len(re.findall(r'Plan .*?: [0-9]+ exact keys', result.stdout)), 1)

    def test_damaged_archive_selective_refuses_full_prefix_deletes_metadata_first(self):
        for shell in self.shells:
            for prefix in ('agent-archive/', ''):
                for damage in ('unreadable', 'malformed', 'missing-reference', 'absent-source'):
                    with self.subTest(shell=shell, prefix=prefix, damage=damage):
                        bucket = self.build(prefix, extra=[prefix + 'unrelated', 'outside/keep'])
                        meta = bucket / (prefix + 'sessions/cursor/dddd/metadata.json')
                        data = json.loads(meta.read_text())
                        if damage == 'unreadable':
                            Path(str(meta) + '.unreadable').write_text('')
                        elif damage == 'malformed':
                            meta.write_text('{broken')
                        elif damage == 'missing-reference':
                            del data['source_bundle']['key']
                            meta.write_text(json.dumps(data))
                        else:
                            (bucket / (prefix + data['source_bundle']['key'])).unlink()
                        before = self.keys(bucket)
                        for selective in ('delete', 'old-sessions', 'machine'):
                            names = ['list', 'delete'] if selective == 'delete' else ['list', selective, 'delete']
                            refused = self.run_recipe(shell, prefix, names)
                            self.assertNotEqual(refused.returncode, 0, refused.stderr)
                            self.assertEqual(self.attempts(), [])
                            self.assertEqual(self.keys(bucket), before)
                        erased = self.run_recipe(shell, prefix, ['list', 'all', 'delete'])
                        self.assertEqual(erased.returncode, 0, erased.stderr)
                        expected = [k for k in before if not k.startswith(prefix)]
                        self.assertEqual(self.keys(bucket), expected)
                        targets = [k.removeprefix('my-archive-bucket/') for k in self.attempts()]
                        metadata = [k for k in before if k.startswith(prefix + 'sessions/') and k.endswith('/metadata.json')]
                        self.assertEqual(targets[:len(metadata)], sorted(metadata))
                        self.assertEqual(sorted(targets), sorted(k for k in before if k.startswith(prefix)))


    def test_full_prefix_removes_sources_without_metadata(self):
        for shell in self.shells:
            with self.subTest(shell=shell):
                bucket = self.build()
                for meta in bucket.rglob('metadata.json'):
                    meta.unlink()
                result = self.run_recipe(shell, 'agent-archive/', ['list', 'all', 'delete'])
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(self.keys(bucket), [])

    def test_full_prefix_recovery_preserves_metadata_deletion_history(self):
        for shell in self.shells:
            for observed_absence in (False, True):
                with self.subTest(shell=shell, observed_absence=observed_absence):
                    self.build()
                    tail = r'''
original=$purge_dir
saved_meta="$FAKE_S3/my-archive-bucket/agent-archive/sessions/claude/aaaa/metadata.json"
cp "$saved_meta" "$TMPDIR/saved-meta"
'''
                    if observed_absence:
                        tail += 'rm "$saved_meta"\n'
                    else:
                        tail += 'export FAKE_RM_FAIL_AT=2\n'
                    tail += r'''
purge_apply && exit 91
unset FAKE_RM_FAIL_AT
purge_resume "$original" || exit 92
recovery=$purge_dir
cp "$TMPDIR/saved-meta" "$saved_meta"
purge_resume "$recovery" && purge_apply && exit 93
exit 0
'''
                    result = self.run_recipe(shell, 'agent-archive/', ['list', 'all'], tail=tail)
                    self.assertEqual(result.returncode, 0, result.stderr)
                    self.assertEqual(len(self.attempts()), 0 if observed_absence else 2)
                    self.assertIn('Metadata reappeared', result.stderr)

    def test_full_prefix_never_reads_metadata_and_stops_at_each_object_class(self):
        for shell in self.shells:
            for failure in (1, 4, 5):
                with self.subTest(shell=shell, failure=failure):
                    bucket = self.build(extra=['agent-archive/.setup-test/x', 'outside/keep'])
                    before = self.keys(bucket)
                    result = self.run_recipe(shell, 'agent-archive/', ['list', 'all', 'delete'],
                                             env_extra={'FAKE_CP_FAIL_AT': '1', 'FAKE_RM_FAIL_AT': str(failure)})
                    self.assertNotEqual(result.returncode, 0, result.stderr)
                    self.assertEqual(len(self.attempts()), failure)
                    self.assertEqual(len(self.removed()), failure - 1)
                    self.assertIn('Already removed:', result.stderr)
                    self.assertIn('Not confirmed removed', result.stderr)
                    self.assertIn('outside/keep', self.keys(bucket))
                    self.assertEqual((self.root / 'state.json.cp').read_text().strip(), '1')
                    self.assertEqual(len(before) - len(self.keys(bucket)), failure - 1)

    def test_full_prefix_listing_failures_and_scope_changes_delete_nothing(self):
        for shell in self.shells:
            for intervention, env in (
                ('', {'FAKE_LIST_FAIL_AT': '2'}), ('', {'FAKE_LIST_BAD_AT': '2'}),
                ('', {'FAKE_LIST_FAIL_AT': '3'}), ('', {'FAKE_LIST_BAD_AT': '3'}),
                ('touch "$FAKE_S3/my-archive-bucket/agent-archive/new"', {}),
                ('prefix=', {}), ('bucket=other-bucket', {}),
                ('echo 1 > "$purge_dir/created"', {}), ('rm "$purge_dir/VALID"', {}),
                ('echo outside/keep >> "$purge_dir/targets"', {})):
                with self.subTest(shell=shell, intervention=intervention, env=env):
                    self.build(extra=['outside/keep'])
                    result = self.run_recipe(shell, 'agent-archive/', ['list', 'all', 'delete'],
                                             between=intervention, env_extra=env)
                    self.assertNotEqual(result.returncode, 0, result.stderr)
                    self.assertEqual(self.attempts(), [])

    def test_stale_and_changed_plans_delete_nothing(self):
        for shell in self.shells:
            for intervention in ('rm "$purge_dir/VALID"',
                                 'echo 1 > "$purge_dir/created"',
                                 'echo changed >> "$FAKE_S3/my-archive-bucket/agent-archive/sessions/codex/bbbb/metadata.json"',
                                 'touch "$FAKE_S3/my-archive-bucket/agent-archive/sessions/codex/bbbb/new.txt"',
                                 'bucket=other-bucket', 'mode=all'):
                with self.subTest(shell=shell, intervention=intervention):
                    bucket = self.build()
                    before = self.keys(bucket)
                    result = self.run_recipe(shell, 'agent-archive/', ['list', 'delete'], between=intervention)
                    self.assertNotEqual(result.returncode, 0, result.stderr)
                    self.assertEqual(self.attempts(), [], result.stderr)
                    if 'echo changed' not in intervention and 'touch' not in intervention:
                        self.assertEqual(self.keys(bucket), before)

    def test_invalid_current_source_identity_deletes_nothing(self):
        for shell in self.shells:
            for variant in ('nested', 'sibling', 'nonexistent'):
                with self.subTest(shell=shell, variant=variant):
                    bucket = self.build()
                    metadata_path = bucket / 'agent-archive/sessions/cursor/dddd/metadata.json'
                    metadata = json.loads(metadata_path.read_text())
                    original = metadata['source_bundle']['key']
                    name = original.rsplit('/', 1)[1]
                    metadata['source_bundle']['key'] = {
                        'nested': f'sessions/cursor/dddd/other/{name}',
                        'sibling': f'sessions/cursor/elsewhere/{name}',
                        'nonexistent': 'sessions/cursor/dddd/source.' + 'e' * 64 + '.jsonl.gz',
                    }[variant]
                    metadata_path.write_text(json.dumps(metadata))
                    before = self.keys(bucket)
                    result = self.run_recipe(shell, 'agent-archive/', ['list', 'delete'])
                    self.assertNotEqual(result.returncode, 0, result.stderr)
                    self.assertEqual(self.keys(bucket), before)
                    self.assertEqual(self.attempts(), [])

    def test_marker_removal_failure_deletes_nothing(self):
        for shell in self.shells:
            with self.subTest(shell=shell):
                bucket = self.build()
                before = self.keys(bucket)
                result = self.run_recipe(shell, 'agent-archive/', ['list', 'delete'],
                                         env_extra={'FAKE_MARKER_RM_FAIL': '1'})
                self.assertNotEqual(result.returncode, 0, result.stderr)
                self.assertEqual(self.keys(bucket), before)
                self.assertEqual(self.attempts(), [])

    def test_partial_delete_reports_progress_and_stops(self):
        for shell in self.shells:
            with self.subTest(shell=shell):
                self.build()
                result = self.run_recipe(shell, 'agent-archive/', ['list', 'delete'],
                                         env_extra={'FAKE_RM_FAIL_AT': '2'})
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(len(self.removed()), 1)
                self.assertEqual(len(self.attempts()), 2)
                self.assertIn('Already removed:', result.stderr)
                self.assertIn('Not confirmed removed', result.stderr)
                self.assertIn(self.removed()[0].removeprefix('my-archive-bucket/'), result.stderr)
                self.assertIn(self.attempts()[1].removeprefix('my-archive-bucket/'), result.stderr)

    def test_old_machine_and_all_exact_keys(self):
        for shell in self.shells:
            for prefix in ('agent-archive/', 'nested/archive/', ''):
                for mode, names in [('old', ['list', 'old-sessions', 'delete']),
                                    ('machine', ['list', 'machine', 'delete']),
                                    ('all', ['list', 'all', 'delete'])]:
                    with self.subTest(shell=shell, prefix=prefix, mode=mode):
                        bucket = self.build(prefix, extra=[f'{prefix}.setup-test/x', 'outside/keep'])
                        result = self.run_recipe(shell, prefix, names)
                        self.assertEqual(result.returncode, 0, result.stderr)
                        remaining = self.keys(bucket)
                        if mode != 'all' or prefix:
                            self.assertIn('outside/keep', remaining)
                        if mode == 'old':
                            self.assertFalse(any(f'{prefix}sessions/claude/aaaa/' in k for k in remaining))
                            self.assertIn(f'{prefix}sessions/codex/bbbb/source.b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1.jsonl.gz', remaining)
                        elif mode == 'machine':
                            self.assertFalse(any(f'{prefix}sessions/claude/aaaa/' in k or
                                                 f'{prefix}sessions/cursor/dddd/' in k for k in remaining))
                            self.assertIn(f'{prefix}sessions/codex/bbbb/source.b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1.jsonl.gz', remaining)
                        else:
                            self.assertFalse(any(k.startswith(prefix) for k in remaining))

    def test_resume_every_deletion_boundary_in_fresh_shell(self):
        selected, omitted = resume_cases(TIER)
        for shell in self.shells:
            for prefix, mode, prepare, boundary in omitted:
                CASE_EVIDENCE.append({'test': self.id(), 'case': {'shell': shell, 'prefix': prefix, 'mode': mode, 'boundary': boundary}, 'status': 'NOT RUN', 'reason': TIER+' representative repeated boundaries'})
            for prefix, mode, prepare, boundary in selected:
                with self.subTest(shell=shell, prefix=prefix, mode=mode, boundary=boundary):
                    bucket = self.build(prefix, extra=['outside/keep'])
                    before = self.keys(bucket)
                    contents = {k: (bucket / k).read_bytes() for k in before}
                    tail = r'''
original=$purge_dir
purge_apply && exit 91
unset FAKE_RM_FAIL_AT
"$FAKE_SHELL" -c '. "$FAKE_HELPERS"; bucket=my-archive-bucket; prefix=$2; purge_resume "$1" && purge_apply' recovery "$original" "$prefix"
'''
                    names = ['list'] if mode == 'unreferenced' else ['list', {'machine':'machine', 'old':'old-sessions', 'all':'all'}[mode]]
                    result = self.run_recipe(shell, prefix, names,
                        env_extra={'FAKE_RM_FAIL_AT': str(boundary)}, tail=tail)
                    self.assertEqual(result.returncode, 0, result.stderr)
                    remaining = self.keys(bucket)
                    if mode == 'unreferenced':
                        expected = [k for k in before if not (k.endswith('.jsonl.gz') and
                            ('/cursor/cccc/' in k or '/claude/aaaa/source.a0' in k or '/claude/aaaa/source.a1' in k))]
                    elif mode == 'all':
                        expected = [k for k in before if not k.startswith(prefix)]
                    else:
                        dirs = ['claude/aaaa'] + (['cursor/dddd'] if mode == 'machine' else [])
                        expected = [k for k in before if not any(k.startswith(f'{prefix}sessions/{d}/') for d in dirs)]
                    self.assertEqual(remaining, expected)
                    self.assertEqual({k: (bucket / k).read_bytes() for k in remaining},
                                     {k: contents[k] for k in expected})

    def test_expired_and_single_use_attempts_need_reviewed_resume(self):
        for shell in self.shells:
            with self.subTest(shell=shell):
                self.build()
                tail = r"""
original=$purge_dir
# Advance the clock rather than modify the sealed attempt.
expiry=$(($(cat "$purge_dir/created") + 301))
date() { printf '%s\n' "$expiry"; }
purge_apply && exit 91
purge_apply && exit 92
purge_resume "$original" || exit 93
purge_apply || exit 94
purge_apply && exit 95
exit 0
"""
                result = self.run_recipe(shell, 'agent-archive/', ['list', 'machine'], tail=tail)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(len(self.attempts()), 6)

    def test_unapplied_recovery_preserves_deleted_metadata_history(self):
        for shell in self.shells:
            for expired in (False, True):
                with self.subTest(shell=shell, expired=expired):
                    self.build()
                    tail = r"""
original=$purge_dir
purge_apply && exit 91
unset FAKE_RM_FAIL_AT
purge_resume "$original" || exit 92
recovery=$purge_dir
"""
                    if expired:
                        tail += r"""
expiry=$(($(cat "$purge_dir/created") + 301))
date() { printf '%s\n' "$expiry"; }
purge_apply && exit 93
unset -f date
"""
                    tail += r"""
cp "$original/meta.1" "$FAKE_S3/my-archive-bucket/agent-archive/sessions/claude/aaaa/metadata.json"
purge_resume "$recovery" && purge_apply && exit 94
exit 0
"""
                    result = self.run_recipe(shell, 'agent-archive/', ['list', 'machine'],
                        env_extra={'FAKE_RM_FAIL_AT': '2'}, tail=tail)
                    self.assertEqual(result.returncode, 0, result.stderr)
                    self.assertEqual(len(self.attempts()), 2, result.stderr)
                    self.assertIn('Metadata reappeared', result.stderr)

    def test_recovery_preserves_observed_metadata_absence(self):
        for shell in self.shells:
            for outcome in ('reappeared', 'absent', 'unselected'):
                with self.subTest(shell=shell, outcome=outcome):
                    bucket = self.build()
                    before = self.keys(bucket)
                    directory = 'codex/bbbb' if outcome == 'unselected' else 'claude/aaaa'
                    tail = r'''
original=$purge_dir
rm "$FAKE_S3/my-archive-bucket/agent-archive/sessions/''' + directory + r'''/metadata.json"
purge_apply && exit 91
'''
                    if outcome == 'unselected':
                        tail += 'purge_resume "$original" && exit 92\nexit 0\n'
                    else:
                        tail += 'purge_resume "$original" || exit 92\nrecovery=$purge_dir\n'
                        if outcome == 'reappeared':
                            tail += r'''
cp "$original/meta.1" "$FAKE_S3/my-archive-bucket/agent-archive/sessions/claude/aaaa/metadata.json"
purge_resume "$recovery" && purge_apply && exit 93
exit 0
'''
                        else:
                            tail += 'purge_apply\n'
                    result = self.run_recipe(shell, 'agent-archive/', ['list', 'machine'], tail=tail)
                    self.assertEqual(result.returncode, 0, result.stderr)
                    if outcome == 'absent':
                        self.assertEqual(len(self.attempts()), 5)
                        self.assertEqual(self.keys(bucket), [k for k in before if not any(
                            k.startswith('agent-archive/sessions/' + d + '/')
                            for d in ('claude/aaaa', 'cursor/dddd'))])
                    else:
                        self.assertEqual(self.attempts(), [])
                        expected = before if outcome == 'reappeared' else [k for k in before
                            if k != 'agent-archive/sessions/codex/bbbb/metadata.json']
                        self.assertEqual(self.keys(bucket), expected)
                        self.assertIn('Metadata reappeared' if outcome == 'reappeared'
                                      else 'Unselected metadata disappeared', result.stderr)

    def test_progress_failure_and_repeated_recovery(self):
        for shell in self.shells:
            with self.subTest(shell=shell):
                bucket = self.build(extra=['outside/keep'])
                before = self.keys(bucket)
                tail = r"""
mv() { case "$2" in */pending) return 1 ;; esac; command mv "$@"; }
purge_apply && exit 91
unset -f mv
original=$purge_dir
purge_resume "$original" || exit 92
# A second interruption must preserve the original exact scope.
export FAKE_RM_FAIL_AT=3
purge_apply && exit 93
unset FAKE_RM_FAIL_AT
original=$purge_dir
purge_resume "$original" && purge_apply
"""
                result = self.run_recipe(shell, 'agent-archive/', ['list', 'machine'], tail=tail)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(self.keys(bucket), [k for k in before if not any(
                    k.startswith('agent-archive/sessions/' + d + '/') for d in ('claude/aaaa', 'cursor/dddd'))])
            for blocked in ('inflight', 'removed', 'errors', 'pending'):
                with self.subTest(shell=shell, blocked=blocked):
                    bucket = self.build()
                    before = self.keys(bucket)
                    result = self.run_recipe(shell, 'agent-archive/', ['list', 'machine', 'delete'],
                        between='mkdir "$purge_dir/' + blocked + '"')
                    self.assertNotEqual(result.returncode, 0)
                    self.assertEqual(self.attempts(), [])
                    self.assertEqual(self.keys(bucket), before)

    def test_crash_after_remote_delete_before_progress(self):
        for shell in self.shells:
            for boundary in (1, 2, 5, 6):
                with self.subTest(shell=shell, boundary=boundary):
                    bucket = self.build(extra=['outside/keep'])
                    before = self.keys(bucket)
                    result = self.run_recipe(shell, 'agent-archive/', ['list', 'machine', 'delete'],
                        env_extra={'FAKE_RM_CRASH_AT': str(boundary)})
                    self.assertNotEqual(result.returncode, 0)
                    original = re.findall(r'Plan (.*?): [0-9]+ exact keys', result.stdout)[-1]
                    tail = '. "$FAKE_HELPERS"; bucket=my-archive-bucket; prefix=agent-archive/; purge_resume "' + original + '" && purge_apply'
                    resumed = self.run_recipe(shell, 'agent-archive/', [], tail=tail)
                    self.assertEqual(resumed.returncode, 0, resumed.stderr)
                    self.assertEqual(self.keys(bucket), [k for k in before if not any(
                        k.startswith('agent-archive/sessions/' + d + '/') for d in ('claude/aaaa', 'cursor/dddd'))])

    def test_resume_conflicts_delete_nothing(self):
        for shell in self.shells:
            for intervention in (
                'chmod 600 "$original/manifest.json"; echo broken > "$original/manifest.json"',
                'bucket=wrong', 'AWS_ENDPOINT_URL=https://wrong.example',
                'touch "$FAKE_S3/my-archive-bucket/agent-archive/sessions/claude/aaaa/new.txt"',
                'echo changed >> "$FAKE_S3/my-archive-bucket/agent-archive/sessions/cursor/dddd/metadata.json"',
                'cp "$original/meta.1" "$FAKE_S3/my-archive-bucket/agent-archive/sessions/claude/aaaa/metadata.json"',
                'export FAKE_LIST_BAD_AT=4', 'export FAKE_LIST_FAIL_AT=4'):
                with self.subTest(shell=shell, intervention=intervention):
                    self.build()
                    tail = r'''
original=$purge_dir
purge_apply && exit 91
unset FAKE_RM_FAIL_AT
''' + intervention + r'''
purge_resume "$original" && exit 92
'''
                    result = self.run_recipe(shell, 'agent-archive/', ['list', 'machine'],
                        env_extra={'FAKE_RM_FAIL_AT':'2'}, tail=tail)
                    self.assertEqual(result.returncode, 1, result.stderr)
                    self.assertEqual(len(self.attempts()), 2, result.stderr)

    def test_resume_rejects_corrupt_progress_and_reappeared_metadata(self):
        for shell in self.shells:
            for intervention in (
                ': > "$original/removed"',
                'echo bogus >> "$original/removed"',
                'rm "$original/removed"',
                'rm "$original/progress.json"',
                'chmod 600 "$original/progress.json"; echo broken > "$original/progress.json"',
                'echo bogus >> "$original/tombstones"'):
                with self.subTest(shell=shell, intervention=intervention):
                    self.build()
                    tail = r'''
original=$purge_dir
purge_apply && exit 91
unset FAKE_RM_FAIL_AT
cp "$original/meta.1" "$FAKE_S3/my-archive-bucket/agent-archive/sessions/claude/aaaa/metadata.json"
''' + intervention + r'''
purge_resume "$original" && purge_apply && exit 92
exit 0
'''
                    result = self.run_recipe(shell, 'agent-archive/', ['list', 'machine'],
                        env_extra={'FAKE_RM_FAIL_AT':'2'}, tail=tail)
                    self.assertEqual(result.returncode, 0, result.stderr)
                    self.assertEqual(len(self.attempts()), 2, result.stderr)
                    self.assertIn('progress', result.stderr)

    def test_crash_around_progress_capsule_rename_recovers(self):
        for shell in self.shells:
            for boundary in (2, 3):
                for phase in ('before', 'after'):
                    with self.subTest(shell=shell, boundary=boundary, phase=phase):
                        bucket = self.build()
                        before = self.keys(bucket)
                        between = r'''
progress_commits=0
mv() {
  case "$2" in
    */progress.json)
      progress_commits=$((progress_commits + 1))
      if [ "$progress_commits" = "$FAKE_PROGRESS_CRASH_AT" ] && [ "$FAKE_PROGRESS_CRASH_PHASE" = before ]; then kill -KILL $$; fi
      command mv "$@" || return 1
      if [ "$progress_commits" = "$FAKE_PROGRESS_CRASH_AT" ] && [ "$FAKE_PROGRESS_CRASH_PHASE" = after ]; then kill -KILL $$; fi ;;
    *) command mv "$@" ;;
  esac
}
'''
                        result = self.run_recipe(shell, 'agent-archive/', ['list', 'machine', 'delete'],
                            between=between, env_extra={'FAKE_PROGRESS_CRASH_AT': str(boundary),
                                'FAKE_PROGRESS_CRASH_PHASE': phase})
                        self.assertNotEqual(result.returncode, 0)
                        original = re.findall(r'Plan (.*?): [0-9]+ exact keys', result.stdout)[-1]
                        tail = '. "$FAKE_HELPERS"; bucket=my-archive-bucket; prefix=agent-archive/; purge_resume "' + original + '" && purge_apply'
                        resumed = self.run_recipe(shell, 'agent-archive/', [], tail=tail)
                        if boundary == 2 and phase == 'after':
                            # Write ahead committed, but delete invocation is uncertain.
                            self.assertNotEqual(resumed.returncode, 0, resumed.stderr)
                            self.assertEqual(self.attempts(), [])
                            self.assertEqual(self.keys(bucket), before)
                        else:
                            self.assertEqual(resumed.returncode, 0, resumed.stderr)
                            self.assertEqual(self.keys(bucket), [k for k in before if not any(
                                k.startswith('agent-archive/sessions/' + d + '/') for d in ('claude/aaaa', 'cursor/dddd'))])

    def test_progress_capsule_commit_failure_stops_before_deletion(self):
        for shell in self.shells:
            for blocked in ('progress.next', 'progress.json'):
                with self.subTest(shell=shell, blocked=blocked):
                    bucket = self.build()
                    before = self.keys(bucket)
                    result = self.run_recipe(shell, 'agent-archive/', ['list', 'machine', 'delete'],
                        between='mkdir "$purge_dir/' + blocked + '"')
                    self.assertNotEqual(result.returncode, 0, result.stderr)
                    self.assertEqual(self.attempts(), [])
                    self.assertEqual(self.keys(bucket), before)

    def test_empty_listing_is_a_valid_noop(self):
        for shell in self.shells:
            for prefix in ('agent-archive/', ''):
                with self.subTest(shell=shell, prefix=prefix):
                    bucket = self.build(prefix, sessions={})
                    result = self.run_recipe(shell, prefix, ['list', 'delete'])
                    self.assertEqual(result.returncode, 0, result.stderr)
                    self.assertEqual(self.keys(bucket), [])
                    self.assertEqual(self.attempts(), [])


    def test_stop_route_after_removed_installation(self):
        for shell in self.shells:
            for state in ('disabled', 'missing'):
                with self.subTest(shell=shell, state=state):
                    bucket = self.build(extra=['outside/keep'])
                    before = self.keys(bucket)
                    result = self.run_recipe(shell, 'agent-archive/', ['list', 'all', 'delete'],
                        env_extra={'FAKE_INSTALL_STATE': state})
                    self.assertEqual(result.returncode, 0, result.stderr)
                    self.assertEqual(self.keys(bucket), ['outside/keep'])
                    targets = [k.removeprefix('my-archive-bucket/') for k in self.attempts()]
                    metadata = sorted(k for k in before if k.endswith('/metadata.json'))
                    self.assertEqual(targets[:len(metadata)], metadata)
                    self.assertEqual(sorted(targets), sorted(k for k in before if k.startswith('agent-archive/')))
                    self.assertEqual((self.root / 'agent-archive.log').read_text().splitlines(), ['pause', 'uninstall'])
                    self.assertEqual((self.root / 'uninstall-args.log').read_text().strip(), 'uninstall --yes')

    def test_removed_installation_never_automatically_uninstalls(self):
        for shell in self.shells:
            for state in ('disabled', 'missing'):
                with self.subTest(shell=shell, state=state):
                    bucket = self.build()
                    before = self.keys(bucket)
                    result = self.run_recipe(shell, 'agent-archive/', ['list', 'delete'],
                        env_extra={'FAKE_INSTALL_STATE': state})
                    self.assertNotEqual(result.returncode, 0, result.stderr)
                    self.assertEqual(self.attempts(), [])
                    self.assertEqual(self.keys(bucket), before)
                    self.assertEqual((self.root / 'agent-archive.log').read_text().splitlines(), ['pause'])

    def test_failed_explicit_uninstall_invalidates_previous_plan(self):
        for shell in self.shells:
            for failure in ('lock', 'unknown-scheduler'):
                with self.subTest(shell=shell, failure=failure):
                    bucket = self.build()
                    before = self.keys(bucket)
                    writer_key = 'agent-archive/sessions/codex/bbbb/source.' + 'e' * 64 + '.jsonl.gz'
                    result = self.run_recipe(shell, 'agent-archive/', ['list'],
                        env_extra={'FAKE_UNINSTALL_FAIL': failure, 'FAKE_WRITER_KEY': writer_key},
                        tail='purge_stop_uploads uninstall\npurge_prepare all\npurge_apply')
                    self.assertNotEqual(result.returncode, 0, result.stderr)
                    self.assertEqual(self.attempts(), [])
                    self.assertEqual(self.keys(bucket), sorted(before + [writer_key]))
                    self.assertIn(failure, result.stderr)
                    self.assertEqual((self.root / 'uninstall-args.log').read_text().strip(), 'uninstall --yes')


if __name__ == '__main__':
    parser = argparse.ArgumentParser(add_help=False)
    parser.add_argument('--tier', choices=('pr', 'daily', 'full'), default='full')
    parser.add_argument('--evidence', type=Path)
    args, remaining = parser.parse_known_args()
    TIER = args.tier
    if os.environ.get('GITHUB_ACTIONS') == 'true' and (not shutil.which('jq') or not all(shutil.which(s) for s in ('bash','zsh'))):
        raise SystemExit('CI purge requires jq, bash and zsh; missing prerequisites are not passes')
    program = unittest.main(argv=[sys.argv[0], *remaining], exit=False,
                            testRunner=unittest.TextTestRunner(resultclass=EvidenceResult))
    if args.evidence:
        source = subprocess.check_output(['git','-C',str(DOCS.parent),'rev-parse','HEAD'],text=True).strip()
        args.evidence.parent.mkdir(parents=True,exist_ok=True)
        args.evidence.write_text(json.dumps({'schema':1, 'source':source, 'tier':TIER,
                                            'success':program.result.wasSuccessful(), 'cases':CASE_EVIDENCE},indent=2)+'\n')
    sys.exit(not program.result.wasSuccessful())
