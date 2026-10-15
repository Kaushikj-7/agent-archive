# The commit a session started on

> **Status: implemented** (`internal/gitremote/head.go`,
> `internal/capture/hook.go`, `internal/archive/git_head.go`). This is the
> design record; the [metadata schema](../../../schemas/metadata.schema.json),
> [JSON output](../../../docs/reference/json-output.md#show), and
> [privacy](../../../docs/security/privacy.md#what-is-uploaded) are the
> contract. Written 2026-09-30 against `main` at `dc443c7`.

## Purpose

A session's metadata says which repository it ran in (`repo_key`) and, through
the retained transcript, which branch, but not which commit. A tool that wants
to rebuild the repository as the session found it (to replay the task with
another agent, say, and compare the results) needs the exact starting commit,
and whether the working tree already had changes the commit does not hold.
The commit at the end is useful too: it bounds what the session itself
committed.

Nothing in the transcripts carries HEAD reliably. Claude Code records
`gitBranch`; Codex and Cursor record neither. The one place that sees the
working directory at the moment a session starts is the hook.

## Decisions

- **The hook asks git, at registration.** A hook that registers a new session
  (a `SessionStart` for Claude Code and Codex; Cursor's first
  `beforeSubmitPrompt`, since its desktop app fires no start) runs
  `git rev-parse --verify --quiet HEAD^{commit}` and
  `git status --porcelain --untracked-files=normal --ignore-submodules=dirty`
  in the working directory the payload reports (`cwd`, or Cursor's first
  `workspace_roots` entry), and records the answers on the registration as
  `start_head`. It uses the same machinery as the repository key: the lookup
  is passed in by the command line (`capture` runs no program), runs only for
  a start that will be admitted in an included project, runs before
  `hooks.lock` is taken, and is abandoned after 600 ms. The two git calls run
  at the same time as each other and as the repository-key lookup, so the
  hook's worst case is unchanged: whatever the lookups use still comes off the
  lock wait (`lockWaitAfter`).
- **Every stop hook asks for HEAD again, and only HEAD.** A stop (`Stop`,
  `StopFailure`, `Interrupt`, `SessionEnd`, Cursor's `stop` and `sessionEnd`)
  of a registered session in an included project runs `rev-parse` in the
  reported directory, before the lock and under the same bound, and records
  the answer as `last_head` when it names a different commit from the one
  recorded. A stop at the same commit preserves its published first-seen
  time and advances a local observation watermark, so an older delayed stop
  cannot replace a newer observation. The common case adds one `git rev-parse`
  (a few milliseconds) and one registration update.
  `git status` is not run at stops: it can take far longer in a large
  repository, and it would run on every turn.
- **The working directory, not the project root.** The repository key is
  asked of the configured project root; the commit is asked of the directory
  the hook reports. The difference matters for Claude Code worktrees
  (`<project>/.claude/worktrees/<name>`), whose HEAD is their own. The cost is
  that a session started inside a submodule or a nested repository records
  that repository's commit; the schema says so.
- **Full object names only.** A value is kept only when it is 40 (SHA-1) or
  64 (SHA-256) lowercase hex digits. A branch with no commits, a directory
  outside any repository, git not installed (or the macOS stub without the
  developer tools, which `gitremote` refuses to run), a timeout, and a panic
  all record nothing. A dirty flag without a commit is dropped: it is
  relative to nothing.
- **Unknown is absent, never guessed.** The collector copies what the
  registration holds into the sidecar's `git_head` at every publication. It
  never runs git for a commit itself: HEAD at publish time is not HEAD at the
  start, and a sidecar that looked authoritative would be worse than none.
  Sessions registered before this change or imported by `backfill` have no
  starting observation. Later live stops can record `last`. Subagents have
  neither observation.
- **A moved HEAD is published even when nothing else moved.** A stop whose
  transcript did not grow still changes `last`. The collector publishes it as
  a metadata-only update over the retained source (re-uploading that source
  if it has gone from storage), preserving capture time and parser output.
  This does not depend on the stop's request: the scan signature records
  which `last` observation (commit and first-seen time) the published
  metadata holds, and a session whose registration holds another is scanned
  again however unchanged its transcript is, even past a remembered read
  failure, since the update reads no source. It is published before any
  parser refresh, which it does not need. A publication whose metadata is
  not cached locally counts as unknown until its sidecar has been read. A stop the hook records after a pass has listed
  registrations, or one whose transcript has since been deleted, is
  therefore published by a later pass instead of being lost.
- **No parser bump.** `git_head` comes from the registration, not from the
  source bundle, and nothing can derive it for a session that was published
  before the hooks recorded it. Bumping `DefaultParserVersion` would make every
  Mac refresh every sidecar for no change. A session registered by the new
  hooks gets `git_head` with its first publication; a session registered
  earlier that is still running gets `last` at its next publication after a
  stop. The field is optional, so `MetadataSchemaVersion` stays 1.

## What is recorded

On the registration (`registrations/<id>.json`, local only):

```json
"start_head": {"sha": "3f9c…40 hex…", "dirty": true, "observed_at": "2026-09-30T09:00:00Z"},
"last_head":  {"sha": "9e01…40 hex…", "observed_at": "2026-09-30T09:41:12Z"}
```

In the sidecar:

```json
"git_head": {
  "start": {"sha": "3f9c…", "dirty": true, "observed_at": "2026-09-30T09:00:00Z"},
  "last":  {"sha": "9e01…", "observed_at": "2026-09-30T09:41:12Z"}
}
```

- `start.dirty` is `true` when `git status --porcelain` printed anything:
  a staged or unstaged change to a tracked file, or an untracked file that is
  not ignored. A submodule counts when its checked-out commit differs from
  the one the superproject records, not for changes inside it
  (`--ignore-submodules=dirty`), which would mean walking every submodule.
  It is absent when the status call did not finish in time. What `git status`
  printed (file names) is read for emptiness only and never kept.
- `last.observed_at` is the first stop that saw HEAD at that commit. A later
  stop at the same commit does not move it, and a stop whose lookup fails
  keeps what was recorded. The registration's local-only `last_head_seen_at`
  tracks the most recent successful stop, including repeated commits, and
  fences older delayed observations.
- `show` prints a `Commit` row (`started on 3f9c2ab4d1e0 with uncommitted
  changes · last seen on 9e01d4c7a2b8`); `show --json` and `list --json`
  carry `git_head` as stored.

## Edge cases

| Case | Result |
| --- | --- |
| Not a git repository | No `git_head` (both lookups fail). |
| git not installed, or the macOS stub without developer tools | No `git_head`; the stub's install prompt is never opened. |
| A branch with no commits yet | No `start`; a later stop after the first commit records `last`. |
| Detached HEAD | Recorded like any other: the commit is what matters. |
| Linked worktree (Claude Code's `.claude/worktrees/<name>`) | The worktree's own HEAD. |
| Session started in a subdirectory | The repository's HEAD (`rev-parse` answers for any subdirectory). |
| Session started inside a submodule | The submodule's HEAD. A consumer can tell by checking the commit against the repository `repo_key` names; it will not be there. |
| A large or slow repository | `git status` is abandoned at 500 ms (`gitremote.Timeout`) and `dirty` is absent; `rev-parse` normally still answers. The hook waits at most 600 ms for both. |
| A hung mount or a git that never exits | Killed at the timeout; the hook's own 600 ms bound covers a stall before the process starts. |
| `GIT_DIR` and friends in the hook's environment | Stripped before git runs (`gitremote.environment`), as for the repository key, so git answers for the reported directory and not another repository. |
| A continuation (resume, `/clear`, compact) | `start_head` is never replaced; the stop that follows updates `last_head`. |
| A start that found `hooks.lock` busy and was admitted later from its queued intent | No `start`: the collector admits it after the fact, when HEAD may have moved. Its stops still record `last`. |
| A stop that found `hooks.lock` busy and was queued (Cursor's stop with a transcript path) | The commit the hook saw before queueing is kept on the queued intent (`last_head`) and recorded when it is replayed, under the same delayed-observation rules. |
| A subagent | No `git_head`: its start is not its parent's, and no hook reports one. |
| An imported session | No `start`; later live stops can record `last`. See the heuristic below. |

## Privacy

A commit name, a boolean, and two times. No branch, remote URL, path, file
name, diff, or content of uncommitted changes is read into the archive: the
status output is tested for emptiness and discarded in `gitremote`. A commit
name is not secret from someone who can read the repository, but it does let
anyone who can read the bucket match a session to a commit in a repository
they can also read, which is spelled out in
[privacy](../../../docs/security/privacy.md#what-is-uploaded). The commit may
exist only on the capturing Mac (never pushed, or later rewritten); the
metadata makes no claim that it is reachable anywhere else.

## Not done here: inferring a start for older sessions

Sessions captured before this change, and every imported session, have no
recorded starting commit. A consumer that wants one anyway can infer a candidate
outside agent-archive, and must label it as inferred:

1. Take the session's branch from its retained transcript (Claude Code's
   `gitBranch` on the first record) or, failing that, the repository's
   default branch.
2. In a clone of the repository `repo_key` identifies, take the last commit
   on that branch whose committer date is at or before the session's
   `started_at`: `git rev-list -1 --before=<started_at> <branch>` (with the
   branch's reflog, `git rev-list -1 --before=… -g <branch>`, when the clone is
   the capturing machine's own, since a rebase or force push rewrites what
   the branch held).
3. Treat the result as a guess: the working tree may have been dirty, the
   session may have started on a local branch that was never pushed, and a
   commit made in the minutes before the session is ambiguous with one made
   by the session itself (compare `git_activity`).

agent-archive does not do this itself: it would mean running git against
history at publish time, in a checkout that may have moved, to produce a
value that looks exactly like a recorded one. If it is ever added, it belongs
in a separate, clearly named field (for example `git_head.start_inferred`),
never in `start`.

## Tests

- `internal/gitremote/head_test.go`: the exact git arguments, what counts as a
  commit and as dirty, the output cap, both calls at once, a hung git, and
  the real git (when installed) against a clean tree, an untracked file, an
  ignored file, an edited file, a detached HEAD, a linked worktree, a
  subdirectory, an empty repository, and a directory outside any repository.
- `internal/capture/hook_git_head_test.go`: the commit and dirty flag on a new
  registration (including a worktree and Cursor's first prompt), nothing for a
  declined, resumed, or already registered start, the stop's HEAD-only lookup
  and its first-seen timestamp and delayed-observation rules, a contended stop
  replayed from its queued intent, nothing at the stop of a session that is
  not archived, the lookups running together and before `hooks.lock`, and a
  hung lookup.
- `internal/archive/git_head_test.go` and `schema_test.go`: what
  `ApplyGitHead` publishes and drops, and a schema that accepts only full
  commit names and no other keys.
- `internal/collector/git_head_test.go`, `internal/cli/hook_git_head_test.go`,
  and `TestSummaryCommit`: the sidecar (including a stop-only commit change on
  a publication without cached metadata, with new hook evidence, with its
  source missing from storage, after its transcript was deleted, with no
  request left to carry it, seen again after another commit, past an
  underivable refresh, behind an unreadable legacy sidecar, and behind a
  remembered read failure), the command wiring, and `show`'s row.
