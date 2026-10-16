# Worktree storage and cleanup

## Caches

- Share the configured Go build/module caches across ordinary worktrees and
  reviews. Inspect their locations with `go env GOCACHE GOMODCACHE`.
- Preserve isolation explicitly required by CI, qualification, security checks,
  or cold-cache measurements. Keep required flags, source proofs, fresh test
  execution (`-count=1`), and evidence checks unchanged.
- For exceptional local caches, use an existing self-cleaning runner or temporary
  directory with cleanup on success, failure, and cancellation. If sandbox access
  is blocked, use approved access or a shared cache instead of per-task overrides.

## Cleanup

- Stop owned background processes before removing task-owned scratch or releasing
  disposable databases, simulators, and containers. Verify resources are idle;
  age or a missing checkout alone is insufficient. Preserve other tasks' resources.
- Keep small reports, logs, and patches separately from compiler output. Report
  retained large directories with their owner, purpose, and cleanup steps.

## Worktrees

Use `git worktree list --porcelain` to inventory checkouts. Before removing one,
review its branch, uncommitted changes, and untracked files; use the host's archive
facility when available. `git worktree prune --dry-run` previews stale Git metadata,
not checkout directories or build caches.

New worktrees inherit instructions from their starting branch. Existing worktrees
and agents must incorporate and reread updates.
