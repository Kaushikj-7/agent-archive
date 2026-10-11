# Import existing sessions (backfill)

Interactive import separates the plan from the current question. Enter skips a standalone import; choose the numbered import action or answer `yes` to proceed. `[e] Edit retention` returns to the same plan after changing the retention period. Undo also defaults to No. Completed answers use short receipts where the terminal can safely redraw them. Explicit noninteractive flags remain the supported route for automation.


`agent-archive backfill` imports the Claude Code, Codex, and Cursor sessions
already on this machine. Run it after [setup](../getting-started/setup.md), which
itself imports the last 7 days of sessions in the projects you chose (an
import like `backfill --since 7d --project DIR`, listed in `backfill history`
and undone with `backfill undo`; see [setup](../getting-started/setup.md#sessions-from-the-last-7-days));
run `backfill` for older sessions and anything else. With
no options it imports every session it finds, but first it shows each project
with its session count per app, what it will skip and why, and an estimated
finite-retention expiry date or **no automatic expiry** for Keep forever.
Nothing changes until you answer
`y`.

```sh
agent-archive backfill --dry-run     # see the plan; changes nothing
agent-archive backfill --dry-run --json
agent-archive backfill               # import, after confirming
agent-archive backfill history       # past imports
agent-archive backfill undo          # remove the latest import from the bucket
agent-archive backfill undo ID --project DIR
```

In `backfill history`, an import's UPLOAD column counts as **waiting** only
the sessions not in the archive yet. Sessions already uploaded are counted
after it: **rechecking** when only a re-scan is still owed (a Codex session
waiting for its history coverage, say), **updating** when newer work is still
to upload, as in `uploaded · 2 rechecking` or `3 waiting · 1 updating`.

Ctrl-C while the plan is being made stops it and changes nothing. Ctrl-C
during registration or upload stops after the session in flight; run
`backfill` again with the same options to finish the import, or let the
background collector upload what is registered. Each says at once that it is
stopping. A second Ctrl-C, or closing the terminal (SIGHUP), SIGTERM or SIGQUIT,
quits at once, after removing the private copy of Cursor's database backfill
may have made; a copy a killed backfill leaves is removed by the next
`backfill` command of any kind.

## Durable import and recovery

After confirmation, backfill stores the selected privacy-filtered evidence in
private local staging before admitting each session. Setup's recent import and
`--background` use the same protection. Once staged, an import can finish even
if the apps delete their native files. A source that disappears before staging
finishes is reported for retry; it is not counted as admitted.

Staging and pending publication share a 1 GiB local quota. Foreground imports
upload completed groups to free capacity; setup and background imports preserve
completed work and report a capacity error so you can retry after the collector
makes room. Each selected revision set and its encoded staging journal has a
32 MiB upper bound; each child has a separate journal under the same quota.
Encoded expansion and recovery work for large records can lower the practical
input limit. A source that exceeds these bounds is rejected before admission;
completed groups stay recoverable. `backfill history` distinguishes staged/admitted sessions from
remotely verified uploads. Uploaded means the collector has read back the
selected remote sources and metadata and committed its local completion state.

Pausing keeps referenced evidence for retry. Updated privacy rules are applied
again before staged evidence is published. Canceling before admission removes
unreferenced evidence, with a cleanup journal if interrupted; canceling after
admission keeps evidence for recovery. Undo removes that import's owned staging
along with its sessions. Dry-run writes no staging or admission state.

## What gets imported

- Claude Code and Codex sessions are read from `~/.claude` and `~/.codex`,
  and from wherever `CLAUDE_CONFIG_DIR` or `CODEX_HOME` points, now or when
  you ran setup.
- Projects the import adds are captured from then on, like projects you
  include in setup. Setup asks once whether to keep them.
- Codex and Cursor sessions import even without their hooks installed. New
  sessions from those apps are captured only after you add them in setup.
- Cursor chats that exist only in Cursor's own database (older chats, and
  those without a transcript file) are imported too. Backfill reads the
  database without changing it; while Cursor is running it reads a private
  copy (macOS: in your per-user temporary folder; Linux: under
  `~/.cache/agent-archive`, or `$XDG_CACHE_HOME`) and deletes the copy when
  it is done. The collector keeps capturing such a chat from the database
  afterwards. Their subagent chats are not imported yet; the plan says how
  many there are.
- Sessions run from your home directory or a temporary directory are skipped
  unless you pass `--include-home` or `--include-temp`. A Git repository in a
  temporary directory, such as a throwaway clone an agent made to review a
  pull request, counts as temporary too.
- A plain folder that isn't a repository (say `~/code`, where you once ran an
  agent) becomes a project that captures new sessions anywhere under it
  that no nearer project owns; the plan says so under its row. Repositories
  and app folders inside it are added to setup as excluded projects, so they
  stay out of capture as before, and the plan lists them. Include one in
  setup to capture it; undoing the import removes them again. Backfill does
  not look, on macOS, inside folders it asks about before an app reads them
  (Desktop, Documents, Downloads, Library, iCloud Drive, other volumes;
  Linux has no such prompts, so nothing is kept out there)
  unless the plain folder is itself inside one; such a folder is kept out
  whole. When not every folder inside could be checked (it stops after
  5,000), the plan says so: a repository it did not find is captured too.
  To stop that, exclude the folder in setup, or import only the projects
  you want with `--project`.
- On macOS, chats Codex desktop started in its own workspaces
  (`~/Documents/Codex`) are one project. Recognizing them doesn't look inside Documents, so a
  terminal without access to Documents is not asked for it unless a session
  you import actually ran there.
- `--since` and `--until` take a date (`2026-09-01`), an RFC 3339 time, or an
  age (`30d`, `12h`). Backfill selects whole local days, so a time or an age
  selects from the start of the day it falls on. An interrupted import run
  with an age is finished by the same age on a later day too. When the
  latest import was interrupted and a run with other options would start a
  new one, the plan says so and prints the command that finishes it.
- Finite retention starts a new window for imported old sessions rather than
  aging them from their native start date. The plan estimates expiry from the
  import day; later meaningful captures, including preserved revisions, can
  extend it, and pending publication can postpone deletion. Choose `edit` to
  keep sessions longer or select `forever` for no automatic whole-session expiry.
  This changes the owning machine's policy for all its sessions, so `edit` only
  raises retention; shorten it in setup. Provider lifecycle rules, privacy and
  superseded-source cleanup, and explicit deletion still apply. Undoing the
  import can put the earlier retention back; see [undo](#undo).
- Claude Code deletes its own transcripts after 30 days by default. To keep
  more history for backfill, set `"cleanupPeriodDays"` in
  `~/.claude/settings.json`.

The rules for which sessions are eligible are in
[session eligibility](../reference/session-eligibility.md); the full design
is in the [backfill design](../../dev/specs/backfill.md).

## Undo

`backfill undo [ID]` deletes an import's sessions from the bucket, forgets
them on this machine, and keeps later backfills from importing them again unless
you pass `--include-removed`. With `--project DIR` it undoes only that
project's sessions. The plan shows exactly what will be deleted, including
sessions resumed since the import, before it asks.

Undo also excludes the projects the import added, with two exceptions the
plan names:

- A project another import still has sessions in stays included, so that
  import's sessions keep updating. The undo of the last of those imports
  excludes it; the plan lists such a project apart from the ones the import
  added.
- A project setup included again after any undo excluded it is left
  included, whichever import added it.

Sessions captured by hooks in an excluded project stop uploading; they are not
deleted.

If the import raised retention (`edit` at the prompt), undo offers to put the
shorter retention back. That applies to all sessions owned by this machine,
so later collector passes may delete other eligible old sessions, including
hook-captured sessions and other imports. The undo plan says how many, and the question
you answer names them. With `--yes` undo leaves retention as it is and prints
the command that restores it: `agent-archive backfill undo ID --yes
--restore-retention`. A `--project` undo never changes retention, and neither
does undo when retention was changed after the import. This includes an import
that raised finite retention to Keep forever: default `--yes` undo keeps forever.
Only explicit `--restore-retention` restores the earlier finite policy; its
preview counts other eligible sessions using meaningful capture age and pending
work, and the compatibility fence remains installed.

Undo refuses, and changes nothing, when it cannot tell its sessions from
another import's (an earlier import whose file is missing had the same ID).

## Import files

Each import is recorded in `imports/<id>.json` in the data directory
([local state](../reference/local-state.md)). If one becomes unreadable,
backfill and undo stop and name it. Repair the file. Moving it out of the
folder lets backfill run again, but that import's sessions stay archived and
`backfill undo` can no longer remove them.

An imported session whose transcript disappears before its first upload
blocks a storage destination change until retention removes it, as a
hook-captured session does.

### Deleted Codex worktrees

A removed Codex checkout can be recovered from its recorded repository identity
when exactly one existing configured checkout owns that identity. The match
includes excluded clones, and never uses a folder name. Missing or unreadable
Git evidence, multiple clones, and uncertain nested exclusions leave the session
unresolved. Existing configured rules and verified live worktree ownership win.

On a first import, backfill resolves live repository sessions before deleted
worktree recovery. It can propose those live repositories as destinations for
sessions with matching recorded identity. Review the project rows: confirmation
adds the selected destinations through the ordinary import configuration change,
so new sessions there will be captured too. Dry-run changes no configuration.
Supported readable Cursor database chats also provide live evidence, even when
only Codex output is selected. Unavailable or malformed database evidence keeps
automatic recovery pending. Output date, app, and project filters do not remove observed live clones from the
recovery evidence. Hidden clones remain evidence without being silently added to
capture; exclusions, unknown roots and incomplete source inventories keep
recovery pending. Automatic discovery uses committed projects only.

For an ambiguous historical import, review an exact mapping with:

```sh
agent-archive backfill --dry-run --map-project /old/cwd=/configured/repository
agent-archive backfill --map-project /old/cwd=/configured/repository
```

Repeat `--map-project` for each original cwd. Both sides must be absolute; the
first equals sign separates them, so an equals sign can appear in the target
but not the original cwd. The target must be an existing, included configured
root. A mapping cannot override an exclusion or contradict usable repository
evidence. It applies only to that exact cwd in this import; it does not grant
future capture permission or change configuration. Repeat the same mappings to
continue an interrupted batch. `--project` remains an output/import filter and
cannot hide other clones to make a repository match unique.

Automatic discovery uses the same unique-repository fallback, while preserving
the native original creation time and current capture authorization. Future
ambiguous chats remain pending. Imported ownership and its original cwd proof
are preserved when a chat continues.

Backfill preserves the `worktree_unresolved` skip code and adds specific recovery
reasons: unavailable repository evidence, resource limits, excluded or ambiguous
repositories, mapping conflicts, and unproved subtree ownership. Retry reasons
ask you to check access or Git and run the plan again; review reasons ask you to
review configured ownership or an exact mapping. A mapping never overrides an
exclusion or conflicting recorded identity.

Dry-run JSON includes optional `diagnostics` summaries with `skip`, `detail`,
`action`, and `count`. When diagnostics are present, `inventory` reconciles
unique candidate transcript files into one disposition each and reports
database-only candidates separately. These counts cover enumerated candidates,
not unreadable stores. Duplicate candidates are not proof of identical bytes.
A repeated observation of a selected file does not make that file a discarded duplicate. Known recovery
exclusions count as excluded while preserving the primary skip code.
`logical_history_pending` is true: dependencies, logical sessions, and retained
revisions have not been reconciled. Related histories keep their existing pending
skip; backfill has not inspected their current selection or missing dependencies.
Summaries contain no transcript paths, native IDs, remote URLs, or content.

File and database-only candidates receive the same diagnostic details. For
`unsafe_format`, the `source_inspection_unavailable` detail asks you to review
source access and format support; the skip alone cannot prove which is missing.
