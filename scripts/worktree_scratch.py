#!/usr/bin/env python3
"""Run commands with owned scratch storage; inspect or prune abandoned runs.

Standalone POSIX helper, kept identical in agent-archive and family_books.
It manages only its own run directories, never Git worktrees or legacy /tmp data.
"""
import argparse
import contextlib
import fcntl
import json
import math
import os
from pathlib import Path
import re
import shutil
import signal
import stat
import subprocess
import sys
import tempfile
import time


FORMAT = 'worktree-scratch-v1'
RUN_NAME = re.compile(r'run-[a-z0-9_]{8}$')


def default_root():
    if sys.platform == 'darwin':
        return Path.home() / 'Library/Caches/agent-worktree-scratch'
    return Path(os.environ.get('XDG_CACHE_HOME', Path.home() / '.cache')) / 'agent-worktree-scratch'


def root_directory(path):
    path = Path(path).expanduser()
    if path.is_symlink():
        raise ValueError('scratch root must not be a symlink')
    path.mkdir(mode=0o700, parents=True, exist_ok=True)
    info = path.stat()
    if info.st_uid != os.getuid() or info.st_mode & 0o077:
        raise ValueError('scratch root must be owned by you with mode 0700')
    return path.resolve()


def save(path, metadata):
    temporary = path / 'metadata.new'
    temporary.write_text(json.dumps(metadata, indent=2) + '\n')
    temporary.replace(path / 'metadata.json')


def group_alive(pgid):
    if not isinstance(pgid, int) or pgid <= 1:
        return True  # Uncertain ownership must never permit removal.
    try:
        os.killpg(pgid, 0)
        return True
    except ProcessLookupError:
        return False
    except PermissionError:
        return True


def disk_bytes(path):
    total = 0
    for base, dirs, files in os.walk(path, followlinks=False):
        dirs[:] = [d for d in dirs if not (Path(base) / d).is_symlink()]
        for name in files:
            try:
                total += (Path(base) / name).lstat().st_blocks * 512
            except FileNotFoundError:
                pass
    return total


@contextlib.contextmanager
def owned_run(path):
    """Yield locked metadata, or None for active, malformed or unowned entries."""
    fd = None
    yielded = False
    try:
        if not RUN_NAME.fullmatch(path.name) or path.is_symlink() or not path.is_dir():
            yielded = True
            yield None
            return
        info = path.stat()
        if info.st_uid != os.getuid() or info.st_mode & 0o077:
            yielded = True
            yield None
            return
        for name in ('lease', 'metadata.json'):
            info = (path / name).lstat()
            if not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid() or info.st_nlink != 1:
                yielded = True
                yield None
                return
        fd = os.open(path / 'lease', os.O_RDWR | os.O_NOFOLLOW)
        try:
            fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            try:
                worktree = json.loads((path / 'metadata.json').read_text()).get('worktree', '?')
            except (ValueError, AttributeError):
                worktree = '?'
            yielded = True
            yield {'active': True, 'worktree': worktree}
            return
        try:
            metadata = json.loads((path / 'metadata.json').read_text())
            if not isinstance(metadata, dict):
                raise ValueError('metadata must be an object')
            timestamp = metadata.get('finished', metadata['created'])
            valid = (metadata.get('format') == FORMAT and metadata.get('id') == path.name
                     and isinstance(timestamp, (int, float)) and math.isfinite(timestamp)
                     and isinstance(metadata.get('worktree'), str))
        except (ValueError, KeyError, TypeError):
            valid = False
        if not valid:
            yielded = True
            yield None
            return
        metadata['active'] = group_alive(metadata.get('pgid'))
        yielded = True
        yield metadata
    except FileNotFoundError:
        if yielded:
            raise
        yield None
    finally:
        if fd is not None:
            os.close(fd)


def open_elsewhere(path):
    """A second guard for daemonized processes that left the original group."""
    tool = shutil.which('lsof')
    if tool is None:
        return True, 'lsof unavailable; cannot establish that this run is idle'
    try:
        result = subprocess.run([tool, '-nP', '-t', '+D', str(path)],
                                capture_output=True, text=True, timeout=15)
    except subprocess.TimeoutExpired:
        return True, 'open-file check timed out'
    if result.stderr.strip() or result.returncode not in (0, 1):
        return True, 'open-file check could not complete'
    pids = {int(p) for p in result.stdout.split() if p.isdigit()}
    return bool(pids - {os.getpid()}), 'open files held by another process'


def run(args, root):
    command = args.command
    if command[:1] == ['--']:
        command = command[1:]
    if not command:
        raise ValueError('run requires a command after --')
    if args.isolated_go_cache and not args.reason:
        raise ValueError('--isolated-go-cache requires --reason naming the isolation requirement')
    path = Path(tempfile.mkdtemp(prefix='run-', dir=root))
    lease = (path / 'lease').open('w')
    fcntl.flock(lease, fcntl.LOCK_EX)
    metadata = dict(format=FORMAT, id=path.name, created=time.time(),
                    worktree=str(Path.cwd().resolve()), owner_pid=os.getpid(),
                    isolated_go_cache=args.isolated_go_cache, reason=args.reason,
                    pgid=None)
    save(path, metadata)
    env = os.environ.copy()
    (path / 'tmp').mkdir()
    env['TMPDIR'] = str(path / 'tmp')
    env['GOTMPDIR'] = env['TMPDIR']
    env['AGENT_SCRATCH_DIR'] = str(path)
    # Ordinary work uses the user's Go default rather than a task-specific override.
    env.pop('GOCACHE', None)
    if args.isolated_go_cache:
        env['GOCACHE'] = str(path / 'go-build')
    print(f'Scratch: {path}', file=sys.stderr, flush=True)
    child = None
    handlers = {}
    received = []
    cancelled_at = []

    def forward(signum, _frame):
        received.append(signum)
        if not cancelled_at:
            cancelled_at.append(time.monotonic())
        if child is not None:
            with contextlib.suppress(ProcessLookupError):
                os.killpg(child.pid, signum)

    try:
        for signum in (signal.SIGINT, signal.SIGTERM, signal.SIGHUP):
            handlers[signum] = signal.signal(signum, forward)
        child = subprocess.Popen(command, env=env, start_new_session=True,
                                 pass_fds=(lease.fileno(),))
        metadata['pgid'] = child.pid
        save(path, metadata)
        for signum in received:
            with contextlib.suppress(ProcessLookupError):
                os.killpg(child.pid, signum)
        while True:
            try:
                result = child.wait(timeout=0.2)
                break
            except subprocess.TimeoutExpired:
                if cancelled_at and time.monotonic() - cancelled_at[0] > 5:
                    with contextlib.suppress(ProcessLookupError):
                        os.killpg(child.pid, signal.SIGKILL)
        return result if result >= 0 else 128 - result
    finally:
        for signum, handler in handlers.items():
            signal.signal(signum, handler)
        metadata['finished'] = time.time()
        save(path, metadata)
        # Background descendants retain their scratch; never delete beneath them.
        idle = child is None or not group_alive(child.pid)
        busy, reason = open_elsewhere(path) if idle and not args.keep else (True, '')
        if idle and not args.keep and not busy:
            shutil.rmtree(path)
        else:
            reason = reason if idle and not args.keep else ('requested --keep' if args.keep else 'live descendants')
            print(f'Retained scratch: {path} ({reason})', file=sys.stderr)
        lease.close()


def inspect(args, root):
    total = 0
    failures = 0
    for path in sorted(root.iterdir()):
        with owned_run(path) as metadata:
            if metadata is None:
                print(f'SKIP unrecognized entry: {path.name}')
                continue
            size = disk_bytes(path)
            total += size
            if metadata['active']:
                print(f'ACTIVE {size / 2**30:.3f} GiB {path.name} '
                      f"worktree={metadata.get('worktree', '?')}")
                continue
            age = (time.time() - metadata.get('finished', metadata['created'])) / 86400
            print(f'IDLE {size / 2**30:.3f} GiB {age:.1f} days {path.name} '
                  f"worktree={metadata['worktree']}")
            if args.action != 'prune' or age < args.older_than_days:
                continue
            if not args.apply:
                print(f'WOULD REMOVE {path.name} (apply rechecks open files)')
                continue
            busy, reason = open_elsewhere(path)
            if busy:
                print(f'SKIP {path.name}: {reason}')
                failures += 1
                continue
            if group_alive(metadata.get('pgid')):
                print(f'SKIP {path.name}: process group became active')
                failures += 1
                continue
            shutil.rmtree(path)
            print(f'REMOVED {path.name}')
    print(f'Managed scratch usage before cleanup: {total / 2**30:.3f} GiB')
    if args.action == 'status' and total > args.budget_gib * 2**30:
        print(f'OVER BUDGET: {args.budget_gib:g} GiB', file=sys.stderr)
        return 2
    return 1 if failures else 0


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--root', type=Path, default=default_root())
    commands = parser.add_subparsers(dest='action', required=True)
    execute = commands.add_parser('run', help='run with automatically cleaned scratch')
    execute.add_argument('--isolated-go-cache', action='store_true')
    execute.add_argument('--reason', default='')
    execute.add_argument('--keep', action='store_true', help='retain scratch for debugging')
    execute.add_argument('command', nargs=argparse.REMAINDER)
    status = commands.add_parser('status', help='report all worktrees sharing this root')
    status.add_argument('--budget-gib', type=float, default=20)
    prune = commands.add_parser('prune', help='preview idle runs older than seven days')
    prune.add_argument('--older-than-days', type=float, default=7)
    prune.add_argument('--apply', action='store_true')
    args = parser.parse_args()
    for name in ('budget_gib', 'older_than_days'):
        value = getattr(args, name, 0)
        if not math.isfinite(value) or value < 0:
            parser.error(f'{name.replace("_", "-")} must be finite and nonnegative')
    try:
        root = root_directory(args.root)
        return run(args, root) if args.action == 'run' else inspect(args, root)
    except (OSError, ValueError) as error:
        print(f'worktree-scratch: {error}', file=sys.stderr)
        return 1


if __name__ == '__main__':
    sys.exit(main())
