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
Archive and configuration changes require answering `y`.

```sh
agent-archive backfill --dry-run     # see the plan; no archive/configuration changes
agent-archive backfill --dry-run --json
agent-archive backfill               # import, after confirming
agent-archive backfill history       # past imports
agent-archive backfill undo          # remove the latest import from the bucket
agent-archive backfill undo ID --project DIR
```

While planning on a live terminal, backfill shows the current step and progress
counts. Discovery shows a running count of sessions found; once transcript checking
starts, it shows completed/total sessions and a percentage.
These counts include sessions examined before date and project filters are applied;
the finished plan shows how many will actually be imported.

Planning, including `--dry-run`, can update the disposable private
`backfill-inspection-cache` directory inside the local archive data directory.
It holds successful intrinsic inspection facts for ordinary Codex rollouts,
qualified by verified content digests and inspection versions. Current source
opening, content checks, project permissions and archive decisions still run.
The cache stores at most 10,000 entries and 16 MiB of serialized records
(filesystem overhead is additional); a bounded command writer
can drop observations under load. A missing, invalid, busy or evicted cache
simply causes full inspection. Delete this directory to discard it. Neither
planning nor this cache registers sessions, changes configuration or uploads data.

In `backfill history`, an import's UPLOAD column counts as **waiting** only
the sessions not in the archive yet. Sessions already uploaded are counted
after it: **rechecking** when only a re-scan is still owed (a Codex session
whose rollout changed while it was read, say), **updating** when newer work is still
to upload, as in `uploaded · 2 rechecking` or `3 waiting · 1 updating`.

Ctrl-C while the plan is being made stops it without archive or configuration changes. Ctrl-C
during registration or upload stops after the session in flight; run
`backfill` again with the same options to finish the import, or let the
background collector upload what is registered. Each says at once that it is
stopping. A second Ctrl-C, or closing the terminal (SIGHUP), SIGTERM or SIGQUIT,
quits at once, after removing the private copy of Cursor's database backfill
may have made; a copy a killed backfill leaves is removed by the next
`backfill` command of any kind.

When local session identity recovery is pending, the plan still prints.
Affected sessions appear as **waiting for local session index recovery**, with
`session_index_pending` in JSON and a counted `session_index_recovery`
diagnostic. They are not selected for import. The dry-run does not repair or
change the index; the background collector owns recovery. Retry after it
completes. Identity conflicts and other archive read errors still stop the plan.

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
`logical_history_pending` remains true while comprehensive dependency/session/
revision accounting is unfinished. The flag describes remaining comprehensive inventory reconciliation, not a
blanket refusal of supported complete histories. Complete native children, forks
and selected physical revisions use the validated shared history reader and
durable staging; missing or ambiguous dependencies remain unresolved.
Summaries contain no transcript paths, native IDs, remote URLs, or content.

Foreground planning and dry runs continue native preparation and inspection
across bounded lookup scopes, including a supported history requiring several
scopes. No timeout flag is needed. Each native I/O operation remains bounded;
source byte, record, graph and shared resource limits still apply. A dry-run
plan does not establish confirmed staging, admission or publication completion.
Those later phases retain their existing work allowances.

Local Git origins use bounded, read-only chain resolution for both proposed and
configured projects. Absolute/relative paths and local `file://` URLs (empty or
ASCII `localhost` host, case-insensitive) can lead to a portable repository
identity through another checkout or bare repository.
Filesystem names may contain colons when a slash precedes the first colon, as in
`/work/mirror:backup.git` or `./mirror:backup.git`. Git still treats an unprefixed
`host:path` as a remote, even if a similarly named local directory exists.
Paths follow filesystem symlink and parent-directory semantics, and their
dependencies are checked again before admission. Every origin value must
resolve consistently. Two physical clones of the same
repository remain competitors, even when one points at the other. Excluded
projects remain part of automatic recovery evidence.

Cycles, missing or unreadable targets, conflicting identities, unsupported
references, keyless chain terminals and traversal limits keep recovery pending
with a specific `project_origin_*` diagnostic. `status` directs pending automatic
recovery to `backfill --dry-run` for these reasons and reviewed assignments.
The observer compares at most 16
repository directories and 128 dependencies for one chain graph. Git is never
asked to fetch, and configuration is unchanged. Reviewed exact mappings or a
mapping file can resolve supported assignments independently of unrelated
uncertain clones. Mappings still respect exclusions and target identity; native
history must pass its own checks.

Discovery resumes partial repository sweeps and retries earlier incomplete origin
observations within its existing lookup allowance. Persistent failures do not
restart enumeration or prevent later roots from being observed. Restored access
can require several bounded discovery passes before automatic recovery completes.

The [large-backfill completion plan](../../dev/proposals/large-backfill-completion.md)
tracks remaining confirmed-import continuation, larger inventories and recovery
assistance. Planning continuation and bounded local-origin evidence are available;
broader completion and recovery assistance remain planned behavior. Existing
supported mappings and historical destinations remain available.

For Codex history refusals, `diagnostics` preserves fixed source reason codes
when available: a missing or duplicated rollout a thread continues from, no
single current rollout, missing dependencies or ownership boundaries, missing or invalid first-own-task evidence, and specific resource
limits. Text plans show the same counted breakdown. Unknown source reasons
use a generic unavailable or resource-limit diagnostic; raw errors are never
printed. These details explain the existing skip and do not change eligibility.

`lookup_allowance_spent` with action `retry` means this run used its time or
header-read allowance for finding Codex rollouts. Affected sessions remain
unchecked under `related_history_pending`; this is not evidence of an oversized
or unsupported source. Codex foreground preparation and inspection renew this
scheduling allowance automatically. Confirmed import and background collection
retain separate allowances. `lookup_io_timeout` identifies an actual failed
bounded I/O operation; check source access before retrying. `lookup_no_progress`
means inventory could not advance its directory cursor or finish another source;
preparation stops once and reports an incomplete-inventory warning. JSON includes
the optional `native_lookup_incomplete` diagnostic with this detail and `retry`
action. Independently verified sessions remain eligible; a session still missing
required native facts keeps its own refusal. No candidate or unreadable-folder
count is invented for unknown inventory. Repair source access before retrying.
Failed I/O is not retried automatically.
Actual source byte and record limits remain
`source_size_limit` and `source_record_limit`; history graph, aggregate byte and
record limits keep their specific reasons. Missing native evidence remains a
separate refusal.

For a content-free planning baseline, use `AGENT_ARCHIVE_TRACE=1` with a dry run.
The timing tree includes native preparation separately from inventory/recovery
and inspection. Fixed aggregate counters report `codex_lookup_scopes` (initial
and explicitly renewed allowances), `backfill_native_preparation_attempts`
(deduplicated requested thread lookups), `backfill_native_inspection_attempts` (child or
related-history inspections), and `backfill_lookup_unfinished_candidates`
(final candidates left pending specifically by a spent allowance). These count
attempts and final decisions, not complete histories or published sessions;
zero counters are omitted. They share the existing bounded trace aggregates and
retain no individual source records.

File and database-only candidates receive the same diagnostic details. For
`unsafe_format`, the `source_inspection_unavailable` detail asks you to review
source access and format support; the skip alone cannot prove which is missing.

## Private project recovery review

Use a terminal to review the missing original working directories observed by
the current planner and export exact assignments:

```sh
agent-archive backfill review-projects --harness codex --output reviewed-projects.json
agent-archive backfill --dry-run --mapping-file reviewed-projects.json
agent-archive backfill --mapping-file reviewed-projects.json
```

The review shows original cwds, normalized recorded repository keys, affected
counts, current decisions, known remaining refusals and physical checkout
evidence. These are private local metadata. Transcript content, native session
IDs and transcript locators are omitted. Terminal input and output are required;
JSON and piped review are unavailable. Ordinary previews do not expose this
private evidence unless you separately supply an explicit mapping file.
Database-only Cursor chats with a usable original folder join the same bounded
groups as transcript files. Shared configured-target observations are reused
across recorded keys; an incompatible key remains refused without repeatedly
spending the lookup allowance needed by later compatible groups.
Choose a historical destination for database-only Cursor chats. A configured
choice at a mixed cwd can recover supported transcript files while leaving those
database-only chats unresolved; the review reports their count explicitly.

Choose a displayed compatible configured destination, an import-only historical
project, `new` to define an import-only historical identity, or `skip`. Even a
single compatible choice requires selection. Multiple physical clones remain
distinct; unresolved chains, excluded sources/targets and conflicting usable
keys are shown without relaxing existing mapping policy. Existing disabled
historical decisions cannot be enabled by export. A historical identity with no
current-destination permission receives it only through a later confirmed import.

All observed repository keys at one exact cwd share one destination decision.
An assignment covers every supported source at that cwd, including identities
outside this incomplete review; repository keys are evidence rather than mapping
selectors. `skip` skips the whole cwd. A configured choice may recover only some
observed identities; incompatible/unresolved evidence is shown for each key.
The later import still checks each source's restrictions, identity and native
evidence. Paths and labels are safely quoted in the review.

Review the exact JSON and type `export` to create the named file with mode 0600.
An existing file, including a symlink, is refused. EOF, `cancel`, rejected export
or an invalid answer writes no assignment file. Review/export never changes
configuration, capture policy, admission, stages or archive; export is not an
import or upload. The later import renews all ordinary policy and evidence checks.
Assignments cover only exact original cwds and that import's supported scope;
they do not authorize future capture. Keep the file for canonical resume/undo:
reordering assignments or changing its filename does not create another batch.

The initial assistance keeps one deterministic original spelling and shows the
exact normalized assignment spelling when it differs. Multiple original
spellings sharing that assignment key retain all observed source restrictions.
Originals containing `..` path components require separate
manual review: the assistance refuses to qualify their traversal from a cleaned
spelling. Benign cleanups such as `.` retain the existing assignment behavior.
Existing exact mapping/import policy and its per-source checks remain
unchanged.

This initial review has **bounded, incomplete coverage**: at most 128 observed
cwd/key groups are retained; observed candidate records outside those groups
are counted, with the distinct omitted group count unknown. At most 128
configured targets, 128 existing historical choices and 128 automatic checkout
evidence rows are shown; unlisted rows are counted. Unlisted choices require a
separately hand-reviewed exact mapping file. Existing source and witness
inventory windows remain in force. No
partial window establishes uniqueness or promises a complete recovery inventory.
Counts are candidates for project recovery, not promises of importability:
native history, independent own-task evidence, excluded ancestors and source
limits may still prevent import after assignment. Complete reviews across larger
paged inventories and oversized review partitioning require later integration.
Existing inline (128) and mapping-file (4 MiB, 16,384 assignments/definitions)
limits are unchanged. Hand-reviewed exact files remain available independently
of this assistance.

## Historical-only projects

When the repository is gone, assign complete supported sessions to an explicit
historical project. This imports reviewed evidence without adding a live project,
installing hooks, or including future chats. Storage must already be configured;
its live project list may be empty. Git and an existing checkout are not required.

Create a stable random 128-bit ID once, keep it with your mapping file, and reuse
it across imports and paired machines. For example, `openssl rand -hex 16` prints
an ID. Labels are display names and do not determine ownership. A mapping file
looks like this (replace the example ID and paths):

```json
{
  "schema_version": 1,
  "historical_projects": [
    {"id": "11111111111111111111111111111111", "label": "Retired service"}
  ],
  "mappings": [
    {"cwd": "/old/service", "historical_project_id": "11111111111111111111111111111111"},
    {"cwd": "/old/worktree", "configured_root": "/current/repository"}
  ]
}
```

Each mapping has one exact absolute original cwd and exactly one target. A
`configured_root` must already be an existing included live project, just like
`--map-project`; its existing live ownership rules still apply. Historical IDs
must be 32 lowercase hexadecimal characters. Definitions can introduce an absent
identity or permission for the current destination. Existing labels and disabled
permissions are preserved. Definitions for projects with no admitted sessions do
not create permissions.

```sh
agent-archive backfill --mapping-file mappings.json --dry-run --json
agent-archive backfill --mapping-file mappings.json
agent-archive backfill projects list
agent-archive backfill projects rename 11111111111111111111111111111111 "Retired API"
agent-archive backfill projects disable 11111111111111111111111111111111
agent-archive backfill projects enable 11111111111111111111111111111111
```

Review the assignments, historical IDs, labels and counts before confirming.
The text preview shows up to 128 assignments; JSON shows all assignments. The
file is limited to 4 MiB, 16,384 mappings and 16,384 project definitions. Unknown
fields, duplicate JSON fields, conflicting cleaned cwds, invalid labels and
multiple targets are refused before discovery. Inline `--map-project` keeps its
128-mapping limit and merges with the file under the same conflict rule.
Field names must use the exact lowercase schema spelling. Supplying both target
fields is invalid even when one is an empty string.

Rerun with the same semantic assignments to resume an interrupted batch. Ordering,
filename and display labels do not change its canonical ownership digest. Changing
a target starts a different batch; changing configuration while confirmation is
pending requires a new preview. Mapping-file paths and private source cwds are
not published or stored in the batch journal. `--project DIR` remains a live
project output filter and does not select historical project IDs.

Explicit exclusions and original source location restrictions also apply to required
physical ancestors. A complete history requiring an excluded or restricted dependency is refused as
`excluded_history`; it is not flattened into a partial session. Home, temporary
and above-home restrictions still apply to the original cwd.
Both original and canonical paths must pass these restrictions, so a symlink
from a temporary directory still requires `--include-temp`.
Missing dependencies or ambiguous histories remain unresolved; fragment import
is still unavailable.

After confirmation, filtered staging survives native source deletion before the
first upload or after a restart. Subsequent publication, privacy filtering,
parser upgrades and local handoff reads use retained evidence only. A transcript
that later grows, or a repository that reappears, cannot extend the historical
import. Rename changes the displayed project name without changing the ID or
capture age. Disable stops admission and publication in the current destination;
re-enable restores only that exact permission. Switching storage never republishes
these imports into the new destination.

Pairing carries historical IDs, labels and the source destination's import
permission, without original cwds or mapping assignments. The recipient preserves
its own existing labels and enabled/disabled decisions for the same destination.
It must supply its own reviewed source mappings. These settings do not authorize
live capture on either machine.

Retention uses the import's capture/admission age. Keep forever disables automatic
whole-session expiry; disabled permissions still expire under finite retention.
Exact batch undo and purge remove the owned sessions and staged/remote evidence.
**Undo retains historical project identities and destination permissions as
explicit configuration**, including identities shared by another import. Use
`backfill projects disable ID` to stop their retained maintenance. Undo does not
claim sessions owned by another batch or change live capture configuration.

The first committed historical permission installs the permanent
`historical-projects-v10` writer fence. Upgrade installed hooks and collectors
before using it; earlier writers refuse rather than discard import-only ownership.
The fence remains after disable, undo, expiry or setup rollback.


Interrupted whole-session undo keeps its exact removal journal until cleanup
finishes. Run undo again to resume it. A hook arriving during retention can keep
its session alive by restoring the exact retained archive baseline, including
preserved revisions, even if the native files have disappeared. The new hook's
evidence remains queued for its normal capture or capture-gap decision. Explicit
undo grants no such restoration: it continues removing the selected session.
If recovery reports changed authority or damaged local evidence, preserve that
evidence and repair the reported condition before retrying.
