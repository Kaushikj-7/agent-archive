# Worktree storage and cleanup

Git worktrees isolate source edits. They do not require a compiler cache for
each branch, reviewer, or agent. Ordinary local Go builds use the account's
default build and module caches, including when work happens concurrently.
Do not set a new `GOCACHE` for every task or use an ad hoc `/tmp/*-go-cache`.
If the sandbox cannot write the normal cache, use the host's approved access
mechanism or one approved shared cache; do not silently multiply private caches.

Required CI, qualification, security, and cold-cache measurements retain their
existing isolated/fresh-cache requirements. A shared build cache does not permit
reusing test results as current validation: keep required `-count=1`, source
proofs, flags, and evidence checks.

## Run a local command with managed scratch

From the intended worktree, use Python 3.9+ on macOS or Linux:

```sh
python3 -B scripts/worktree_scratch.py run -- go test -race -count=1 ./...
```

For a service in a subdirectory, run from that directory and adjust the script
path. The helper preserves your working directory and command exit status.
It gives the command unique `TMPDIR`, `GOTMPDIR`, and `AGENT_SCRATCH_DIR`
directories, and removes an inherited task-specific `GOCACHE` override so Go uses
the user's configured default. It preserves `GOMODCACHE` and other settings.
Cache locations explicitly configured in Go's own environment file still apply;
check `go env GOCACHE GOMODCACHE` when diagnosing unexpected paths.

For a check that actually requires an isolated build cache:

```sh
python3 -B scripts/worktree_scratch.py run \
  --isolated-go-cache --reason 'required cold-cache qualification' -- \
  go test -race -count=1 ./...
```

The isolated Go cache lives inside that run's scratch directory. Do not override
its path from inside the command. Existing qualification runners that already
own and clean temporary caches can keep doing so; this helper does not replace
their validation contracts.

Scratch is removed on success or failure. Interrupt/termination signals are
forwarded to the command's process group; after five seconds a command that
ignores cancellation is killed. Live descendants retain their scratch. Avoid
daemonizing from a scratch run. Long-lived development databases, servers,
simulators, and container volumes need their own explicit owner and stop/delete
instructions; this tool does not stop or remove them.

Use `run --keep -- COMMAND` only when debugging needs the files. Store small
reports, logs, patches, and deliverables outside managed scratch before returning;
do not retain a whole compiler cache merely to preserve a report. The helper
records the worktree path, creation/finish times, process group and isolation
reason, but never saves the command's environment or full arguments.

## Inspect storage across worktrees

Both repositories use the same per-user registry and compatible helper:

- macOS: `~/Library/Caches/agent-worktree-scratch`
- Linux: `${XDG_CACHE_HOME:-~/.cache}/agent-worktree-scratch`

This is independent of branch and worktree count. To use another location, pass
`--root /absolute/private/path` before the subcommand consistently in every
worktree. It must be owned by your user with mode `0700` and not be a symlink.

```sh
python3 -B scripts/worktree_scratch.py status --budget-gib 20
python3 -B scripts/worktree_scratch.py prune --older-than-days 7
python3 -B scripts/worktree_scratch.py prune --older-than-days 7 --apply
```

`status` reports allocated file storage, worktree ownership and active/idle state;
it exits 2 when usage exceeds the budget. The budget is a reporting threshold,
not permission to interrupt active runs or an automatic disk quota. File-system
sharing can make the reported sizes differ from space actually recovered.

`prune` previews by default. Applying requires a valid tool-owned manifest,
an unlocked run lease, no live recorded process group, sufficient age, and a
successful `lsof` open-file check. Missing `lsof`, an inspection error, or an
active process preserves the run. `lsof` is also required for automatic cleanup;
without it the command still runs, but scratch is retained. On Linux install it
through the host's normal tooling before relying on cleanup. macOS includes it.
The wrapper holds a lease while the command runs; both repo copies cooperate
through those same leases. Crashes or force-killed wrappers can leave storage for
the next explicit prune. Unknown directories, symlinks, and malformed manifests
are skipped. No task is removed based solely on an old timestamp or a missing
worktree directory.

The tool never scans/deletes arbitrary `/private/tmp`, old agent session folders,
Git worktrees, default shared Go caches, databases, or simulator devices. Existing
unmanaged leftovers need a separate content and activity review. Do not point the
registry at an existing broad cache or temporary directory.

## Worktree lifecycle

At task completion, stop owned background processes, release disposable databases
and simulator devices, retain small evidence, and check managed scratch usage.
For a large parallel run, record one owner for each long-lived resource and a
cleanup action in the run plan. Never stop another worktree's services or delete
unmerged work in order to reclaim space.

Inventory Git worktrees with `git worktree list --porcelain`. Use a host's archive
facility when available to preserve work before removing an unneeded checkout.
`git worktree prune --dry-run` only previews stale Git administrative entries; it
does not reclaim active checkout directories or compiler caches. Review branches,
uncommitted changes and untracked files before any actual worktree removal.

Instruction updates reach new worktrees after they are committed and incorporated
into the starting branch. Existing worktrees and active agents must incorporate
and reread the updated instructions before continuing. Updating one checkout does
not update every existing worktree automatically.

## Verify helper changes

```sh
python3 -B scripts/test_worktree_scratch.py
```

The tests use synthetic temporary roots and check cleanup on success, failure and
cancellation, explicit isolation, previews/age limits, leases, open files, unsafe
paths and metadata, and budget reporting. They do not touch the account registry.
The standalone helper and its tests are intentionally mirrored in `agent-archive`
and `family_books`; keep behavioral fixes and the format version compatible in
both repositories.
