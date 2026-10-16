# Worktree storage and cleanup

Git worktrees isolate source edits. They do not require a compiler cache for
each branch, reviewer, or agent. Ordinary local Go builds and reviews use the
account's configured shared build and module caches, including concurrent runs.
Do not create task-specific `GOCACHE` or `GOMODCACHE` overrides for routine work.

Inspect the current locations with:

```sh
go env GOCACHE GOMODCACHE
```

If a sandbox cannot write the normal cache, use the host's approved access
mechanism or an approved shared cache. Do not silently create another private
cache for each task or weaken sandbox permissions.

## When isolation is required

Preserve explicit fresh/isolated-cache requirements for hosted CI, qualification,
security checks, and cold-cache measurements. Those requirements apply to the
specified check; they are not a reason to isolate every local review or worktree.
Keep required test execution, flags, source proofs, and evidence checks unchanged.
Sharing compiler output does not authorize treating cached test results as fresh
validation; retain required `-count=1` invocations.

For an exceptional local isolated cache, prefer an existing runner that already
owns and cleans its temporary storage. Otherwise use a temporary directory with
cleanup on success, failure, and cancellation, such as Python's
`tempfile.TemporaryDirectory` or a shell cleanup trap. Stop child/background
processes before removing their files. A crash or force-kill can bypass cleanup;
record the directory's owner and purpose so leftovers can be reviewed later.
Do not introduce persistent PR/reviewer-specific cache paths under `/tmp`.

## Finish each task

- Retain small reports, logs, patches, and deliverables separately from disposable
  compiler output. Do not preserve gigabytes of build state with the evidence.
- Stop task-owned background processes and release disposable databases,
  simulator devices, and containers. During a large parallel run, record one
  owner and a cleanup action for each resource.
- Remove only task-owned scratch after checking it is no longer in use. A missing
  checkout or an old timestamp alone does not establish that storage is idle.
  Preserve shared caches, other tasks' resources, and unmerged/untracked work.
- Report retained large directories with their purpose, owner, and cleanup steps.
  Instructions guide new work; they do not automatically clean old `/tmp` folders.

## Worktree lifecycle

Inventory checkouts with `git worktree list --porcelain`. Use the host's archive
facility when available to preserve work before removing an unneeded checkout.
`git worktree prune --dry-run` previews stale Git administrative entries; it does
not reclaim active checkout directories or compiler caches. Review branches,
uncommitted changes, and untracked files before actual worktree removal.

New worktrees inherit these instructions after they reach the starting branch.
Existing worktrees and active agents must incorporate and reread the updated
instructions; updating one checkout does not update every other worktree.
