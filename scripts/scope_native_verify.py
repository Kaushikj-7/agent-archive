"""Conservatively omit unchanged workflow audits from a staged PR run."""
import copy
import hashlib
from pathlib import Path, PurePosixPath
import re
import stat
import subprocess

GO_CHECKS = ['lint', 'vet', 'modules', 'vulnerabilities']
WORKFLOW_CHECKS = ['workflow-lint', 'workflow-security']
KINDS = ['go-lint', 'go-vet', 'go-mod', 'go-vuln', *WORKFLOW_CHECKS]
SCOPED_RUN = 'pr-go'
ROOT_INPUTS = {'levenshtein.json', '.gitmodules', '.go-version', '.golangci.yml',
               'action.yml', 'action.yaml', 'actionlint.yml', 'actionlint.yaml',
               '.actionlint.yml', '.actionlint.yaml', 'zizmor.yml', 'zizmor.yaml'}


def relevant(path):
    return path in ROOT_INPUTS or any(path == root or path.startswith(root + '/')
                                      for root in ('.github', 'scripts'))


def compatible(config):
    """Unknown valid configurations retain their original complete run."""
    try:
        if config.get('baseline') or SCOPED_RUN in config['runs']:
            return False
        if config['runs']['pre-merge']['checks'] != GO_CHECKS + WORKFLOW_CHECKS:
            return False
        for name, kind in zip(GO_CHECKS + WORKFLOW_CHECKS, KINDS):
            check = config['checks'][name]
            if check['kind'] != kind or 'targets' in check:
                return False
            if name in WORKFLOW_CHECKS:
                target = config['targets'][check['target']]
                if target != {'dir': '.', 'workspace': '.', 'inputs': ['.github']}:
                    return False
        return True
    except (KeyError, TypeError):
        return False


def git(source, *args):
    return subprocess.check_output(['git', '-C', str(source), *args],
                                   stderr=subprocess.PIPE, timeout=15)


def records(data):
    if data and not data.endswith(b'\0'):
        raise ValueError('unterminated Git records')
    result = data[:-1].split(b'\0') if data else []
    if any(not record for record in result):
        raise ValueError('empty Git record')
    return result


def repository_path(raw):
    value = raw.decode()
    path = PurePosixPath(value)
    if not value or path.is_absolute() or '..' in path.parts or str(path) != value:
        raise ValueError('malformed repository path')
    return value


def working_input_matches(source, path, mode, oid):
    """Inspect staged bytes directly; Git status can hide index-marked edits."""
    source = Path(source).resolve()
    item = source / path
    for component in [item, *item.parents]:
        if component == source:
            break
        if component.is_symlink():
            return False
    metadata = item.stat()
    if not stat.S_ISREG(metadata.st_mode):
        return False
    executable = bool(metadata.st_mode & 0o111)
    if executable != (mode == b'100755'):
        return False
    content = item.read_bytes()
    blob = b'blob ' + str(len(content)).encode() + b'\0' + content
    return hashlib.sha1(blob, usedforsecurity=False).hexdigest().encode() == oid


def decision(source, config, event, tested):
    """Return a run and safe reason; any proof failure keeps full coverage."""
    base = head = ''
    try:
        if not compatible(config):
            raise ValueError('configuration is outside the workflow-scoping contract')
        pr = event['pull_request']
        base, head = pr['base']['sha'], pr['head']['sha']
        for sha in (base, head, tested):
            if not isinstance(sha, str) or not re.fullmatch('[0-9a-f]{40}', sha):
                raise ValueError('missing or invalid event commit')
            if git(source, 'rev-parse', '--verify', sha + '^{commit}').decode().strip() != sha:
                raise ValueError('event commit is unavailable')
        if git(source, 'rev-parse', '--is-shallow-repository') != b'false\n':
            raise ValueError('history is shallow or uncertain')
        if git(source, 'rev-parse', 'HEAD').decode().strip() != tested:
            raise ValueError('checkout differs from event commit')
        parents = git(source, 'show', '-s', '--format=%P', tested).decode().strip().split()
        if parents != [base, head]:
            raise ValueError('checkout is not the expected event merge')
        # A merge-base proves connected histories; it is not the diff endpoint.
        if not re.fullmatch(b'[0-9a-f]{40}\n', git(source, 'merge-base', base, head)):
            raise ValueError('event ancestry is uncertain')
        for sha in (base, head):
            git(source, 'merge-base', '--is-ancestor', sha, tested)
        # Check current workspace too. No generated files/import caches may be
        # created in source before this proof. Dirty unrelated files also fall back.
        if git(source, 'status', '--porcelain=v1', '-z', '--untracked-files=all'):
            raise ValueError('checkout has modified or untracked files')
        for sha in (base, head, tested):
            for record in records(git(source, 'ls-tree', '-r', '-z', sha)):
                metadata, path = record.split(b'\t', 1)
                mode, kind, oid = metadata.split(b' ')
                path = repository_path(path)
                if not re.fullmatch(b'[0-9a-f]{40}', oid):
                    raise ValueError('malformed tree object')
                if relevant(path) and (mode not in (b'100644', b'100755') or kind != b'blob'):
                    raise ValueError('relevant input is a symlink or unusual object')
                if sha == tested and relevant(path) and not working_input_matches(source, path, mode, oid):
                    raise ValueError('relevant working input differs from tested tree')
        for endpoint in (head, tested):
            changed = records(git(source, 'diff', '--no-ext-diff', '--no-textconv',
                                  '--no-renames', '--name-only', '-z', base, endpoint, '--'))
            if any(relevant(repository_path(path)) for path in changed):
                raise ValueError('workflow, configuration, tooling or scripts changed')
        return SCOPED_RUN, f'relevant inputs unchanged: base {base}, PR head {head}, tested merge {tested}'
    except (KeyError, TypeError, ValueError, UnicodeError, OSError, subprocess.SubprocessError):
        # Do not include raw subprocess diagnostics or PR-controlled text in a
        # summary. An unavailable/uncertain proof is not a successful skip.
        identifiers = ', '.join(f'{name} {sha}' for name, sha in
                                (('base', base), ('PR head', head), ('tested merge', tested))
                                if isinstance(sha, str) and re.fullmatch('[0-9a-f]{40}', sha))
        return 'pre-merge', ('full run: relevant inputs changed or unchanged-input proof unavailable'
                             + (f' ({identifiers})' if identifiers else ''))


def add_run(config):
    generated = copy.deepcopy(config['runs']['pre-merge'])
    generated['checks'] = GO_CHECKS.copy()
    config['runs'][SCOPED_RUN] = generated


def summarize(run, reason):
    if run == SCOPED_RUN:
        return ('Selected lint, vet, modules, vulnerabilities.\n'
                'workflow-lint and workflow-security NOT RUN: ' + reason + '\n')
    return 'Selected original complete pre-merge run. ' + reason + '\n'
