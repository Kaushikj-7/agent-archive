# `agent-archive backfill` — engineering spec

Status: implemented. Phase 1 merged in #17–#23 and #28 (destination IDs), and phase 2
(Cursor database chats) in #29–#31; Cursor subagent chats are not imported yet.
Written 2026-09-23 against `main` at `50087bf` (filter 6, adapter 0.6.0,
parser 0.9.0, source bundle schema 2). How to use it:
[backfill guide](../../docs/guides/backfill.md). The native stores were
probed on a working Mac for their layout and field names only, never their
content. Phase 1 imports transcript files. Phase 2 adds Cursor chats that exist
only in Cursor's SQLite database.

## Problem

The archive captures only sessions that start after their project is activated
([session eligibility](../../docs/reference/session-eligibility.md)). Earlier
sessions are invisible to `list`, `show`, `handoff`, and retros. So are
sessions the hooks declined: resumed old conversations, projects that weren't
included yet, and apps without hooks. That history is still on disk, for now.

The probe Mac held 31 Claude Code sessions (plus 101 subagent transcripts), 1
Codex session, 3 Cursor transcript files, and 7 Cursor database chats: the same
3, plus 1 draft and 3 empty ones. None existed only in the database there, but
the database holds timestamps and tool results that the 3 files lack. The 24
Claude project folders turned out to be 6 repositories, 11 of their worktrees,
5 desktop scratch chats, and 2 temporary directories. So getting each session
to the right project is central to this work.

**Before this ships:** by default, Claude Code deletes transcripts older than
30 days, every day. Anyone who wants more history must set
`"cleanupPeriodDays": 365` in `~/.claude/settings.json` now. The archive never
changes app settings.

## Goals

1. `agent-archive backfill` with no arguments imports every session on this
   machine. It says so plainly, and first shows each project with its session
   count per app. Nothing is written until the person confirms.
2. Imported sessions go through the same filter, bundle, metadata, retention,
   `list`, `show`, and `handoff` code as hook-captured ones.
3. Correct project attribution. Worktrees fold into their repository, and the
   home directory never silently becomes a catch-all project.
4. Idempotent, crash-safe, and reversible with one command.
5. Existing guarantees hold. Nothing old is uploaded without an explicit
   decision. An import never counts as proof that hooks work. Retention never
   orphans objects.
6. Readers can tell an imported session from a captured one, and can see what
   evidence the import lacks.

**Non-goals:**
- Re-importing sessions this machine already registered (see
  [Re-admission](#re-admission)).
- Deduplicating content that Claude Code copies into forked sessions.
- Linking Codex sub-threads to their parents.
- Importing from other machines, from backups, or from store paths other than
  the defaults and those `CLAUDE_CONFIG_DIR`, `CODEX_HOME`, and setup's
  recorded hook files name (see [Discovery](#discovery)).
- Installing hooks or changing app settings.
- A "never delete" retention setting. Setup currently maps `0` days to 90.

## Command

```
agent-archive backfill [flags]    import sessions
agent-archive backfill history    list past imports
agent-archive backfill undo [ID]  remove the latest import, or import ID
```

| Flag | Meaning |
|---|---|
| `--harness NAME`, `--project DIR` | Limit to `claude`, `codex`, or `cursor`, or to one project. Both can be repeated. The project directory does not need to exist. |
| `--since DATE`, `--until DATE` | Filter on session start, as local `YYYY-MM-DD`, inclusive. |
| `--include-home`, `--include-temp` | Import sessions from the home directory or from temporary directories, which are skipped by default ([Project resolution](#project-resolution)). |
| `--include-removed` | Import sessions that retention or `undo` removed earlier. |
| `--dry-run [--json]` | Print the plan and exit. Nothing is written, locally or remotely. |
| `--yes` | Skip the confirmation. Required when stdin is not a terminal. |
| `--background` | Register the sessions and exit. The scheduled collector uploads them. |

Backfill requires a completed setup with archiving enabled. It refuses to run
while collection is paused or a setup transaction is pending; `--dry-run` works
in both cases. It exits `0` when the import completes, when there is nothing to
import, or when the person declines; `2` on a usage error (an unknown flag or
app, a bad date, `--json` without `--dry-run`); and `1` on any other failure.
Ctrl-C during registration exits `1`, because the import is incomplete until a
rerun; Ctrl-C during upload exits `0`, because the collector finishes it. The
first Ctrl-C, in planning, registration, or upload, prints that it is
stopping. A second Ctrl-C, or SIGTERM, SIGHUP or SIGQUIT at any point, exits at once
with the shell's status for the signal (130, 143, 129, 131), after
`cursorstore.RemoveOwnSnapshots` removes the database copies this process's
Readers hold (the one-step backup can't be interrupted, and those Readers
are never closed).

The storage check (step 2 of [Registration and
concurrency](#registration-and-concurrency)) writes, reads, and deletes one
synthetic test object, as setup's check does. It never touches a session, so
"nothing is written until the person confirms" holds for everything the import
itself writes.

### The default run

Counts are illustrative.

```
$ agent-archive backfill
Looking for Claude Code, Codex, and Cursor sessions on this machine… 35 found.
Checking storage… ready.

Backfill imports every session found on this machine into
s3 / personal-agent-archive / agent-archive/. Nothing has been uploaded yet.

PROJECT                               CLAUDE  CODEX  CURSOR  TOTAL
~/agent-archive                            9      1       2     12  already included
~/levenshtein                              5      –       –      5  will be added
~/agent-skills                             4      –       –      4  will be added
~/GoodProse                                2      –       –      2  will be added
~/personal-website                         –      –       1      1  will be added

Not a repository:
Claude desktop scratch chats               5      –       –      5  will be added
  Chats started without a folder. New ones will be captured too.

Total: 29 sessions (plus 101 subagent transcripts), 25 MB,
       started 2026-09-17 to 2026-09-23.

Not imported:
   3  already in the archive
   2  run from temporary directories            add --include-temp
   1  Cursor chat whose project could not be determined
   2  Cursor subagent chats are not imported yet

If you continue:
  • 5 projects are added, and new Claude Code sessions in them are captured.
    New Codex and Cursor sessions need those apps added in setup.
  • Retention is 90 days, so these sessions are deleted on 2026-12-22.
    Choose `edit` to keep them longer.
  • Undo any time with `agent-archive backfill undo`.

Import 29 sessions from 6 projects? [y/N/edit]
```

- **Scope sentence.** It names the bucket and says "every session found on
  this machine". When filters are set, it says "sessions matching" and lists
  them instead.
- **Answers.** The default is No. `edit` asks for a longer retention
  period, which applies to the whole archive, then shows the plan again. It
  only raises retention: a shorter period, or turning retention on while it
  is off, would delete sessions already archived (hook-captured ones too),
  so it keeps the current value and says to change it in setup.
  `ApplyToConfig` refuses a shorter one as well. The batch records the value
  it raised from (`retention`), and undo offers to put it back while
  retention is still what the import set, showing first how many sessions
  the shorter period then deletes (hook-captured and other imports' too;
  the confirmation question names the count). `undo --yes` leaves retention
  alone (`UndoPlan.WithRetentionKept`) unless `--restore-retention` is given,
  and says how to restore it; the batch is not marked restored, so a later
  `undo ID --restore-retention` still can. A `--project` undo leaves
  retention alone.
- **Deletion date.** All imports are captured at about the same time, so
  they expire together, and the plan shows that date.
- **Rows.** Rows are sorted by total and then by path. Paths are never
  truncated. A missing folder is marked `(folder no longer exists)`.
- **Not imported.** Every non-zero [skip reason](#skip-reasons) is listed,
  with the flag that overrides it, if there is one.
- **Privacy.** No transcript path, native ID, or content is ever printed.

After confirming, the command prints `Added 5 projects. Registered 29 sessions
and 101 subagent transcripts as import 2026-09-23-1.` It then shows an upload
progress bar and ends with a summary and `list --imported`. Ctrl-C is safe:
registered sessions persist locally, and the next collector pass uploads them.
`--background` stops after registering.

`--dry-run --json` prints the same plan with these top-level keys:
`destination`, `filters`, `projects` (each has `root`, `kind`, `status`,
`exists`, per-app `sessions`, `subagents`, `bytes`, first and last start,
`captures_subfolders`, `kept_out`, the folders inside it the import adds as
excluded projects, `kept_out_unchecked`, those among them kept out whole
without being looked in because macOS protects them, and
`kept_out_complete`),
`skipped` (a count for each reason), `apps_without_hooks`, `retention_days`,
`expires_on`, and `storage_checked` (always `false` in a dry run, which
writes nothing; the storage check writes a test object), then
`cursor_database_checked`,
`cursor_database_unchecked_reason` (when not checked: `locked`,
`unreadable`, `unknown_format`, `changed_during_read`, or
`transcripts_unreadable`),
`cursor_database_newer_format`, `cursor_subagents_not_imported`,
`subagents_skipped`, `unreadable_folders`, and `unreadable_stores`.
Plans whose recovery evidence was incomplete also include optional
`recovery_evidence_gaps` (see [Recorded project recovery](#recorded-project-recovery)).

Plans with diagnostic details also include optional `diagnostics` (bounded
aggregate records with the winning primary `skip`, typed `detail`, `action`,
and `count`) and `inventory`. Existing skip codes and precedence are unchanged.
Text renders the same content-free explanation and actionable recovery advice.
No diagnostic includes native locators, IDs, Git command output or content.
File and database-only candidates use the same diagnostic vocabulary. The broad
`unsafe_format` skip has `source_inspection_unavailable` detail and `review_source`
action: check access and format support, without assuming a format incompatibility.
Recovery outcomes remain distinct even when their primary skip is
`worktree_unresolved`; history stays `related_history_pending` with
`history_lookup_pending` until a shared selection/dependency lookup is available.

`inventory.unique_candidate_files` counts distinct candidate transcript paths;
`database_candidates` counts database-only candidates separately. Each file has
one disposition (`planned_import`, `already_archived`, `duplicate_candidate`,
`excluded`, or `unresolved`), whose counts reconcile to unique candidate files.
Recorded recovery exclusions count as excluded while keeping `worktree_unresolved`
as their primary skip. Repeated observations of a selected file do not count that
file as a discarded duplicate; planned import or archived selection wins, and
conflicting remaining dispositions are unresolved.
This foundation does not infer identical bytes, physical dependency roles,
logical histories or retained revisions. `logical_history_pending` is true.
Unenumerated stores and unreadable folders retain their separate existing fields.

### `history` and `undo`

`history` lists each import with its ID, date, session count, projects added,
and upload state.

`undo [ID] [--project DIR] [--yes]` removes the most recent import, or the one
with the given ID from `history`. Once the most recent import is undone, a
plain `undo` says it has nothing left rather than moving on to an older
import; an older one needs its ID. It first shows what it will do. For example:
delete 28 sessions and 101 subagents from the bucket, including 2 that were
resumed since and have newer content; exclude the 5 projects the import
added; and leave hook-captured sessions and the apps' own files alone. It then
asks `[y/N]`. On a yes:

- **Refusals and checks.** Undo refuses while paused or while a setup
  transaction is pending. When anything will be deleted from the bucket, it
  checks storage before asking, as import does. With nothing left to undo it
  says so before asking for confirmation.
- **Locks.** It holds `setup.lock` from planning to exit. It takes
  `collector.lock` only after the person confirms, so the prompt never stalls
  the collector, then reloads the configuration, checks its fingerprint, and
  rebuilds the selection. If the selection gained sessions, projects, or apps,
  it aborts and asks for a rerun. `collector.lock` is then held through
  deletion, so nothing can be republished meanwhile.
- **Order.** It first records `undone_at` on the batch, then excludes the
  projects the batch added (`Included: false`, which setup can reverse;
  projects already included, or already excluded by an earlier undo, stay as
  they were), then records them in `projects_excluded`. Only then does it
  remove sessions. An interrupted undo therefore stops republishing at once,
  a batch with `undone_at` is never continued by a later import, and a rerun
  of undo finishes the job.
- **Removal.** Each session is deleted as whole-session retention deletes
  one, metadata first, then sources, through `collector.DeleteWholeSession`,
  which both now call. A session whose objects are in a previous destination
  is only forgotten locally. Subagents go before their parents. Each session
  is forgotten with a removal record with reason `undo`
  ([Removal records](#removal-records)). A failure leaves that session
  registered and undo continues with the rest.
- **Resumed sessions** are counted in the plan: those whose transcript was
  modified after the import (nothing in the archive writes to a transcript, so
  only the app can have), or that carry hook evidence since the import. The
  first covers apps without hooks. A republish alone is not a resume: parser
  upgrades and subagent links republish too.
- `--project` limits the undo to one project.
- **Shared projects.** A project the import added stays included while
  another import still has sessions there; the batch records it in
  `projects_kept`, and the imports it was kept for in `projects_kept_for`.
  Only the undo of one of those imports takes it over, and the plan lists a
  taken-over project apart from the ones the import added. A project any
  undo has excluded (it is in some batch's `projects_excluded`) is never
  excluded again by any undo: if it is included now, setup included it.
- **Interrupted undo.** The batch is marked undone before the configuration
  is saved, and what the save excluded and restored is recorded after it. A
  run that stops in between leaves those unrecorded; its rerun (with
  `undone_at` set) records a project of the import that is excluded now,
  and a retention that is back at `from`, as done (`UndoPlan.Settled`),
  even when nothing else is left to undo.

## Admission model

### The problem

`SessionRegistration.SessionStartedAt` does two jobs:

1. It records when the conversation began: `metadata.started_at`, subagent
   ordering, handoff times, and the Cursor text first event.
2. It decides which boundary the session falls on. `Config.AcceptSession`
   rejects a session that starts before its project's `ActivatedAt` or before
   `DestinationSince` ([config.go:103](../../internal/config/config.go)).
   Retention compares it with `DestinationSince` to decide whether the
   session's objects are in the current bucket
   ([collect.go:190](../../internal/cli/collect.go)).

For a hook session the two times are equal. For an import they are months
apart.

### Fields

```go
// AdmittedAt is when this machine took ownership of the session: the
// boundary for project activation and storage destination. Hooks set it at
// registration and backfill sets it to the import time. Empty on older
// registrations.
AdmittedAt time.Time `json:"admitted_at,omitempty"`

// Origin is how the session entered the archive. Set once.
Origin SessionOrigin `json:"origin,omitempty"`

// StartedAtSource says where SessionStartedAt came from.
StartedAtSource StartedAtSource `json:"started_at_source,omitempty"`

// ImportBatch is the backfill run that registered the session.
ImportBatch string `json:"import_batch,omitempty"`
```

`SessionOrigin` and `StartedAtSource` are typed strings, following
`MetadataState`. An empty value means the
hook value, so older registrations decode unchanged:

```go
type SessionOrigin string

const (
	SessionOriginHook   SessionOrigin = "hook"
	SessionOriginImport SessionOrigin = "import"
)

type StartedAtSource string

const (
	StartedAtSourceHook           StartedAtSource = "hook"            // when the hook fired
	StartedAtSourceTranscript     StartedAtSource = "transcript"      // earliest native record
	StartedAtSourceFileCreated    StartedAtSource = "file_created"    // file birth time; format has no timestamps
	StartedAtSourceCursorComposer StartedAtSource = "cursor_composer" // phase 2: composerData.createdAt
)
```

Every boundary check goes through one accessor:

```go
// Admitted is the boundary time. Registrations older than AdmittedAt were
// all hook-registered, so their start is their admission.
func (r SessionRegistration) Admitted() time.Time {
	if !r.AdmittedAt.IsZero() {
		return r.AdmittedAt
	}
	return r.SessionStartedAt
}
```

`SessionStartedAt` keeps only its first job, and for imports it holds the true
start. Old registrations need no migration, because the fallback reproduces
today's behaviour. Hooks set `AdmittedAt = SessionStartedAt = now`, so hook
behaviour is unchanged.

| Site | After |
|---|---|
| `AcceptSession`, project activation | `Admitted()` |
| `AcceptSession` destination check, and retention's `CurrentDestination` (both `InCurrentDestination`) | The registration's `DestinationID` ([B1b](#destination-id-b1b)); `Admitted()` when it has none |
| `AcceptSession` app check | `Harnesses`, plus `ImportedHarnesses` for imports ([Apps without hooks](#apps-without-hooks)) |
| Retention age before a first capture, and with no transcript path (`retention.go:133`, `:149`) | `Admitted()`. Without this, an import with a two-year-old start whose first upload fails is expired and pruned at once. |
| `Eligible` (hook fresh start), `BuildMetadata`, Cursor text first event, subagent ordering, handoff | Unchanged. These want the true start. |
| Status `HookObserved`, app verification, skill inventory observer | `SessionOriginHook` only |

A guard test fails if non-test code compares `SessionStartedAt` with
`ActivatedAt` or `DestinationSince` anywhere outside `Admitted()`. This
matters because a missed site fails silently: the collector skips the session,
or retention leaves its objects behind.

### Destination ID (B1b)

Deciding which bucket a session belongs to by comparing times is a guess, and a
clock change can make it wrong. B1b, now implemented, records the bucket
directly: `config.DestinationID` is a hash of provider, endpoint, bucket, and
prefix, never of credentials. (It lives in `config`, not `archive`, which
imports no other internal package.) A test pins its value: changing the hash
needs a migration of every stored ID. Hooks set the registration's
`DestinationID` on a new session, backfill on an import, and a subagent copies
its parent's; a continuation never changes it. `InCurrentDestination`, which
`AcceptSession` and retention use, compares it when it is set, and falls back
to comparing times when it is empty. No migration is needed. A session
admitted into bucket A is not accepted while the destination is B. Switching
back to A resumes A's sessions that are still registered: they publish and
expire in A again, where their objects are. What happened while B was
configured is not recovered: retention forgot A sessions that expired then
without deleting their objects, A's pending subagent candidates were
rejected, and hooks dropped A sessions' lifecycle evidence. Project
activation stays a time comparison, because it really is a question of time.

### Alternatives considered

| Option | Why not |
|---|---|
| `SessionStartedAt = now` for imports | Needs no code changes, but the data is wrong: `started_at` is false, `list` sorts old sessions as new, handoff times are false, and every imported subagent is rejected because it appears to start before its parent. |
| Move `ActivatedAt` or `DestinationSince` back in time | Widens the boundary for every session, not just the confirmed ones. It resets app verification, and claims sessions that were published to an earlier bucket. |
| `ImportedAt` alone | Works through a fallback, but one field then means both "was imported" and "when admitted". Re-admission would need a second field anyway. |

### Remaining downsides

- **Two times per session.** Future code must pick the right one. The
  accessor, the guard test, and the table above (which moves into the
  eligibility doc) contain this.
- **Before/after comparisons.** The archive no longer begins at activation,
  so comparing before and after a skill was adopted could silently include
  imports. Metadata `origin` and `list --imported` let readers exclude them.
- **Imports expire together.** Retention ages a session from its capture time
  ([retention.go:134](../../internal/retention/retention.go)). The plan shows the
  date and offers `edit`.
- **Less exact start times.** An import's start is its earliest record, or
  for a Cursor file its birth time, which a copy or restore resets.
  `started_at_source` says which applies.
- **Clock dependence.** `AdmittedAt` is stamped under `setup.lock`. Backfill
  aborts if `AdmittedAt` is earlier than `DestinationSince` or than any target
  project's `ActivatedAt`. A session whose start is later than the import is
  skipped with `start_in_future`.

### Re-admission

A registration that exists but is no longer accepted stays that way. This
covers sessions behind an earlier bucket, and sessions in a project that was
excluded and then re-included. Backfill reports these as
`registered_not_admitted`. Admitting one again would mean re-stamping
`AdmittedAt` and `DestinationID` and resetting the published cache and the
superseded-source ledger. Otherwise the collector sees nothing new to publish,
and retention deletes keys that belong to another bucket. A later
`--readmit` could do this; the fields above leave room for it.

### Apps without hooks

`Config.Harnesses` lists the apps that have hooks installed, and
`AcceptSession` rejects every other app. A new `ImportedHarnesses` list admits
imports only, for apps that were imported without hooks. Backfill adds apps to
it, and the plan says so. Setup shows the list, and removing an app there stops
publication of that app's imports. Installing an app's hooks moves it to
`Harnesses`.

## Import batches

Imported sessions are stored exactly like captured ones: the same
`sessions/<harness>/<archive id>/source.<sha>.jsonl.gz` bundles and
`metadata.json` in the same bucket, read by the same `list`, `show`, and
retention code. Only metadata sets them apart: `origin`, `imported_at`, and
`started_at_source` ([Evidence imports lack](#evidence-imports-lack)).

The import batch is local bookkeeping, and nothing about it is uploaded. Each
confirmed run writes `imports/<date>-<n>.json` under the archive home, with:

- start and completion times
- the filters and `--include-*` flags used
- `destination_id`
- the projects and apps it added
- its archive session IDs

The file never holds native IDs or paths; added projects are stored as
project IDs. Registrations point back through `ImportBatch`, and they are the
source of truth: a crash can leave registrations the batch file doesn't list
yet, so a rerun rebuilds the batch's session list from them before marking it
complete, and undo selects sessions by `ImportBatch`. Only an import
registration (`origin: import`) carrying the batch's ID belongs to it
(`backfill.InBatch`, the one place that compares the field). A batch file
whose `id` is missing or malformed (not `<date>-<n>`), differs from its file
name, or that has no start time is reported as unreadable, like one that
isn't JSON, so undo stops rather than select by an empty ID. A rerun with the same
filters and destination continues an unfinished batch rather than starting a
new one. A relative `--since` or `--until` (an age such as `30d`) is recorded
as typed too (`since_arg`, `until_arg`), and matches the same value on any
later day; other values match by the local day they name. When the latest
batch is interrupted and the run's options would start a new one, the plan
says so and prints the command, with the batch's options, that finishes
it. `history`, `undo`, and `status` read these files, and
`uninstall --delete-local-data` removes them.

## Removal records

Retention's `ForgetSession` deletes the session's entry in the native session
index ([internal/state/lineage.go](../../internal/state/lineage.go)). Without some other
record, the next backfill would import the session again. Codex keeps its
files forever, so the result would be a 90-day cycle of import and delete.

When retention or `undo` forgets a session, it writes
`forgotten/<sha256(app + "\x00" + native ID)>.json` containing
`{app, reason, at}`. Backfill skips a session with such a record, as
`removed_by_retention` or `removed_by_undo`, unless `--include-removed` is
set. Hooks are unaffected. Sessions removed by retention before this change
have no record and can be imported once more; the release notes say so.

## Project resolution

Each session resolves to one project root or to a skip reason. The first
matching rule wins.

1. **Working directory.** Claude Code: the `cwd` of the first record that
   has one. Codex: `session_meta.payload.cwd`. Cursor: see
   [Discovery](#discovery).
2. **Configured project.** If a configured project owns the directory, use
   it. "Owns" means the nearest configured ancestor on resolved paths, the
   same rule hooks use (`capture.ConfiguredProjectActivationFor`). If that project is
   excluded, skip with `excluded_project`. An exclusion always beats the
   default.
3. **Worktree.** If walking up from the directory finds a `.git` file, follow
   `gitdir:` and `commondir` to the main repository. If the directory no
   longer exists, `<repo>/.claude/worktrees/<name>` still maps to `<repo>` by
   its path. A missing Codex or Cursor worktree cannot be mapped by its path alone.
   Recorded repository identity or a reviewed exact configured mapping may
   recover its destination; otherwise it stays `worktree_unresolved`. Recovery
   uses bounded read-only Git observations outside admission locks to establish
   repository identity and renew its evidence.
   Recorded repository recovery additionally considers the bounded union of all
   configured roots (including excluded and unavailable roots) and live
   repository roots resolved from the completed source/header inventory. Live
   ownership is resolved before recorded recovery and before output filters,
   including validated Cursor workspace and database evidence. The database
   evidence pass inspects at most 1,024 catalog records, 65,536 metadata/raw
   SQL rows and 128 MiB of aggregate payload before filters. The shared ledger
   includes catalog keys/values, scalar schema checks, numeric metadata projections,
   source header observations, bounded workspace metadata and composer/bubble keys/values before
   allocation. This bounds
   payload read into the process, not SQLite page I/O. Recovery reads only
   settled in-place transactions, with no backup or failure-signature reads;
   a live WAL with frames in `-wal`, unavailable capability and exhausted
   budgets keep it pending. A WAL database whose `-wal` is empty (with or
   without `-shm`, as Cursor can leave both after quitting) is settled: it is
   read with `immutable=1` and rejected as `changed_during_read` unless the
   file, its header and its side files (`-wal` still empty, `-shm` still
   present or absent as before, no `-journal`) are unchanged afterwards.
   Frames in `-wal` are never read for recovery: `immutable=1` would ignore
   them, and nothing checkpoints or rewrites Cursor's side files.
   Native recovery requires a non-partial unique binary index on the exact key
   column with the native default binary comparisons; explicit collations and
   unknown schemas cannot establish payload length bounds.
   Settled database, side-file and workspace observations
   renew without rereading chats; changed observations require a new plan.
   Unsettled sources can renew at most eight bounded evidence epochs per plan;
   exhausted renewal leaves admission pending with the instruction to rerun.
   File membership/header observations renew through at most 65,536 observed
   native paths per slice, including directory and absent-store stamps; larger
   inventories, and removed, replaced or rewritten observed paths, require a new
   plan and keep automatic recovery pending. Membership that only grew (a
   directory stamp moved, an absent path appeared) is judged by the transcripts
   added (see [Recorded project recovery](#recorded-project-recovery)).
   The inventory checks membership, not content: added, removed or replaced
   paths (file identity), mode changes, truncation and same-size rewrites change
   it, but a transcript growing in place does not when a planned session owns
   that file (its header came from that same file). Running agents append to
   their transcripts, and header facts come from complete leading records that
   an append cannot change; a header no complete record decided already keeps
   recovery unavailable. The owning session's own source observation still
   skips it as `source_changed` and still governs its witness renewal, but
   that check only runs for imported sessions' sources and for the witness a
   recovery relies on: a same-inode rewrite that grows any other owned
   transcript is not detected. Agents append rather than rewrite in place, so
   this limit is accepted. Paths no session owns keep the full size and
   modification-time comparison.
   Known plain-folder and absent-cwd ownership is also renewed, so a newly
   created checkout cannot evade clone evidence. This is an observation boundary,
   not an atomic filesystem snapshot.
   Selected file witnesses pass the full native filter/import inspector even
   when archive state or output filters hide their sessions.
   Pending or oversized sources cannot
   alone propose destinations; malformed or incomplete evidence cannot certify
   uniqueness. One exception applies to proposed (not yet configured) roots
   only: when Git reports a checkout top level and prints an origin that yields
   no repository key (for example a clone whose origin is a local path to a
   deleted worktree), that root cannot own any recorded key and is non-owning
   rather than unknown, so it does not make recovery unavailable for other
   sessions. It must still be a keyless checkout at the same top level when
   evidence is renewed. A proposed root that Git could not observe, whose
   origin read failed, or whose observation exhausted its budget, still blocks, and every configured root
   keeps the strict rule ([Recorded project recovery](#recorded-project-recovery)).
   Exact mappings continue to require configured targets.
   A Cursor database chat with no folder evidence at all (no folder, no
   workspace reference, no message folder; for example a subagent composer)
   is a non-witness, not incomplete evidence: it names no root, so it cannot
   hide a clone. A chat whose workspace reference cannot be read, whose
   `workspaceIdentifier` is present in any shape this release cannot resolve
   to a local folder (a remote URI, an unknown shape), or whose messages name
   several folders, still makes the inventory incomplete.
   The plan displays selected proposed roots as projects it will add through the
   ordinary batch/config transaction. Hidden roots remain recovery evidence
   without becoming new capture roots. Evidence Context/digest binds the union;
   PolicyContext binds the exact prospective committed configuration, separately.
   Outside-lock confirmation preflight and renewed admission validation reject
   stale evidence. Neither planning nor automatic discovery uses uncommitted
   proposed roots as capture authorization.
4. **Repository.** If walking up finds a `.git` directory, use its parent.
   The walk stops at home. A repository inside a temporary directory (rule
   6), reached directly or through one of its worktrees, is not a project:
   the session is temporary under rule 6. Such repositories are mostly
   throwaway clones an agent made, say to review a pull request, and adding
   one would capture every later session there. A configured project still
   owns it (rule 2), and a worktree in a temporary directory of a repository
   elsewhere still folds into that repository (rule 3). A temporary
   directory that holds home (a container or sandbox whose `HOME` is under
   `/tmp`) does not make the repositories in home temporary.
5. **Desktop app workspaces.** Anything under
   `~/Library/Application Support/Claude/scratch-workspaces/` (Claude desktop
   scratch chats) or `~/Documents/Codex/` (Codex desktop's dated workspaces,
   `<date>/<name>`) becomes that one folder as a project, unless rule 3 or 4
   already found a repository inside it. Because the nearest ancestor wins,
   future chats there are captured too, and the plan says so. macOS asks
   before an app looks inside `~/Documents`, so the plan matches
   `~/Documents/Codex/` as spelled (under home as given and with its
   symlinks resolved) and doesn't look inside Documents for it: a plan with
   no session and no configured project in Documents never touches it, and
   a terminal without Documents access gets no prompt. Only when a
   configured project is already in Documents is the folder's own symlink
   resolved, as before.
6. **Temporary directories.** `/tmp`, `/private/tmp`, `/var/folders`, and
   `$TMPDIR` are skipped with `temporary_directory`, under both their given
   and symlink-resolved spellings (`/tmp` is `/private/tmp` on macOS), and so
   is a repository in one (rule 4). With `--include-temp`, each session's
   directory becomes its own project, except that a repository in one
   becomes one project at its root, which its worktrees join wherever they
   are. These sessions are mostly tool runs whose folders are gone.
7. **Home and above.** Home is skipped with `home_directory`. With
   `--include-home`, it becomes a project, and unless home is already
   included, the plan warns that it will then capture every future session
   under home that isn't in a nearer project. `/`, `/Users`, and anything else
   above home are skipped with `above_home`, which no flag overrides.
8. **Anything else** becomes its own project, whether or not it still exists.

A plain folder (rule 8), or a temporary directory with `--include-temp`,
that the import adds owns every folder under it that no nearer project
owns: hooks capture a new session in the nearest configured project that
contains it. So the plan says, under the row, that every future session
under it that isn't in a nearer project is captured too, and it looks
inside the folder (at most 5,000 folders listed; symlinks, `.git`,
`node_modules`, and virtual environments are not entered) for what must stay
as it is: nested repositories and linked worktrees (a `.git` folder or
file), and the desktop apps' workspace folders and temporary directories.
It never looks inside a folder macOS protects with a privacy prompt (the
home folder's Desktop, Documents, Downloads, and Library; iCloud Drive and
other apps' containers inside Library; `/Volumes` and the other paths to
removable and network volumes) unless the added folder is itself inside that
one, where running a session already needed the access; the 5,000-folder
limit applies either way. A protected folder it does not look in is kept out
whole (`kept_out_unchecked` in the dry-run JSON), without reading it, so
nothing in it is captured that was not before.
The import adds each as an excluded project (`projects_kept_out` in the
batch), so the nearest configured project for anything in them is excluded
and hooks keep ignoring them, as before the import. The plan lists them, and
says when not every folder could be checked: a repository not found is then
captured too, and the plan says how to stop that (exclude the folder in
setup, or import only chosen projects with `--project`). Undoing the import removes
each such entry again once nothing included contains it (removing it then
changes no capture); an import still in place keeps its entries, even while
setup has the folder excluded, so including the folder again keeps them out.
Home (rule 7) is not looked in: `--include-home` is the explicit
choice to capture everything under home, and the plan warns about it. Nor
is a folder whose nearest project above it is included and not itself
looked in (home added by the same import, or a configured project): what is
under it is captured with or without the folder, so keeping anything out
would stop capture rather than keep it as it was. The look stops when the
plan is cancelled. A
later backfill skips a kept-out repository's sessions as
`excluded_project`, until setup includes it.

When rules 3 or 4 map a directory to a repository, rule 2 runs again on that
repository, so a worktree outside its repository still honours the
repository's inclusion or exclusion, with its configured spelling. A worktree
whose `gitdir` target no longer exists is `worktree_unresolved`, unless the
`.claude/worktrees` path rule applies.

Existing paths have their symlinks resolved, as hooks do. A missing path
resolves its longest existing ancestor and keeps the rest, so its spelling
matches what hooks would record. The project ID is
`archive.ProjectID(root)`, so imports and later hook sessions share it. A new
project is added with `Included: true`, and its `ActivatedAt` is the import
time. From then on it behaves like any project included in setup. Setup checks
that a project's folder exists only when the project is newly included, so an
imported project whose folder is gone doesn't block later setup runs, and it
asks once whether to keep all projects backfill added rather than once per
project.

## Discovery

Discovery is read-only. It never follows a path found inside a transcript. The
plan runs each app's existing adapter over the whole transcript, as the
collector will, so the plan's counts are what gets imported. Transcripts that
would become permanently blocked registrations are skipped instead: those the
adapter refuses, those over `archive.MaxRecordBytes`, and those with no
conversation (no retained user, assistant, message, response, or tool record,
and no text). A Claude Code or Codex transcript with no record timestamp is
`start_unknown`: only Cursor, whose files have no timestamps at all, falls
back to the file's birth time; a chat read from Cursor's database starts at
its `createdAt`, and without one is `start_unknown`. Only counts, times, and
sizes are kept.
Symlinked transcript files are skipped. A folder in an app's store that can't
be listed is skipped and counted on one "Not imported" line
(`unreadable_folders` in JSON), without printing its path. If an app's whole
session folder can't be listed (`~/.claude/projects`, `~/.codex/sessions`,
`~/.codex/archived_sessions`, or `~/.cursor/projects`), the plan names the app
instead and says none of its sessions are included (`unreadable_stores` in
JSON); the other apps still import. Header reads (a Claude `cwd`, a Codex `session_meta`) are
capped at 1 MiB per line. The same session found more than once, for example a
Claude file under two project folders, is imported once. The winner is, in
order: a copy that would import over one that wouldn't, the best identity
match, a Codex file in `sessions/` over `archived_sessions/`, the larger file,
then the lexically first path. The rest are `duplicate_session`. Subagent
transcripts get the size check and the same acceptance checks the collector
applies when it registers a child (`collector.CheckImportedSubagent`:
non-empty, complete native timestamps, matching agent and parent IDs, not
starting before the parent). Those that fail are left out of the subagent
count and reported on one "Not imported" line. The collector rejects an
imported subagent whose transcript is empty rather than waiting for it, since a
historical transcript won't grow; a hook-reported one still waits.

The plan uses `min(8, max(2, runtime.NumCPU()/2))` workers. Filtering is
CPU-bound and scales almost linearly with workers. On an 18-core Mac, the
Claude adapter processed 519 MB of real transcripts at these rates:

| Workers | Throughput | Peak heap |
|---|---|---|
| 1 | 30 MB/s | 15 MB |
| 4 | 104 MB/s | 21 MB |
| 8 | 183 MB/s | 32 MB |
| 18 | 316 MB/s | 56 MB |

Eight workers read 5 GB of history in about 30 seconds, and using every core
would cost the person's machine more than it saves. Upload, which is sequential
and network-bound, is the slower phase either way. Memory stays small because
the adapter streams. The only guard needed stops several large files from
being read at once: workers share a 128 MiB cap on transcript bytes being
read at the same time, so a file that doesn't fit waits for room, and one
larger than the cap (a raw transcript may be up to 256 MiB, below) runs
alone.

| | Claude Code | Codex | Cursor |
|---|---|---|---|
| Files | `~/.claude/projects/*/*.jsonl` | `~/.codex/sessions/**/rollout-*.jsonl` and `~/.codex/archived_sessions/rollout-*.jsonl`; `sessions/` wins if a file is in both | `~/.cursor/projects/<slug>/agent-transcripts/<id>/<id>.jsonl`, plus the text form |
| Native ID | File stem. Must be among the records' `sessionId` values; a forked file also carries its parent's ID. A conversation with no `sessionId` at all is `identity_mismatch`. | `session_meta.payload.id`. Must equal `session_id` (when present) and the UUID in the file name. | `<id>`, which is what hooks register |
| Start | Earliest record (`transcript`) | `session_meta` timestamp, else earliest record (`transcript`) | File birth time (`file_created`). Records have no timestamps. |
| Project | `cwd` | `payload.cwd` | Slug match, below |

Claude Code's and Codex's folders are found as setup finds their hook files:
besides `~/.claude` and `~/.codex`, backfill looks in the folders
`CLAUDE_CONFIG_DIR` and `CODEX_HOME` name in its environment, and in the
folders of the hook files setup recorded (`hook_files`), so a shell without
the variables still finds them. A session in two of these folders is a
`duplicate_session`. Cursor has no such variable.

- **Identity.** An ID mismatch is skipped with `identity_mismatch`.
  Acceptance must confirm that hooks register Codex sessions under
  `payload.id`, because deduplication against hook registrations depends on
  it.
- **Codex sources.** Every source is imported (`cli`, `vscode`, `exec`, and so
  on). Like hooks, backfill records no Codex harness observation.
- **Claude subagents.** For each imported parent, backfill reads
  `<slug>/<session>/subagents/agent-<id>.jsonl`. It writes a
  `SubagentCandidate` with `ObservedAt` set to the import time and
  `Origin: SessionOriginImport`, plus the pending linked-session evidence that
  `handleSubagentStop` writes. The existing materialize step validates and
  registers the child, with two changes: the child copies the parent's
  `AdmittedAt`, `Origin`, and `ImportBatch`, and an imported child gets no
  `subagentstop` lifecycle evidence, because no hook fired.
- **Cursor slugs.** The folder slug is the path with every character other
  than an ASCII letter or digit replaced by `-` (so `personal_website` becomes
  `personal-website`), which can't be reversed reliably. Instead, each candidate path is converted
  with Cursor's rule and compared with the slug. Candidates come from
  configured roots, roots resolved from Claude Code and Codex sessions, the
  `folder` in Cursor's `workspaceStorage/*/workspace.json`, and finally a walk
  of the file system that tries each `-` as `/`, bounded to 4,096
  directories. Exactly one match is required; otherwise the session is
  `project_unknown`.
- **Cursor database chats.** The plan reads `state.vscdb` through
  `modernc.org/sqlite` (package `internal/cursorstore`), and never writes to
  it or next to it. The path is resolved through symlinks first, because
  SQLite keeps its side files next to the real file. Chats only the database
  holds are candidates like file sessions ([phase 2](#phase-2-cursor-database-chats)).
  - **What is listed:** `composerData:*` rows (read through the key index,
    not a table scan) that have messages in `fullConversationHeadersOnly` or,
    in older chats, an inline `conversation`; are not drafts; are not listed
    in any row's `subagentComposerIds`; and have no transcript on disk (the
    file wins). Each goes through the archive-state check, so
    `already_archived` and `removed_*` win, and `--since` and `--until` use
    `createdAt` (a chat without it fails them). A second row for the same
    chat is a `duplicate_session` (the row kept is one whose key is the
    chat's ID), and a row whose `composerId` differs from its key is
    `identity_mismatch`, since the collector reads a chat by its ID. Rows with a newer `_v` than tested are still listed when the fields
    that decide the listing decode; their number is reported in JSON
    (`cursor_database_newer_format`), and the filter then refuses them as
    `unsafe_format`.
  - **What is read.** Every chat still importable is read whole (its
    composerData row and its message rows) through one `cursorstore.Reader`
    for the whole plan, so at most one snapshot of the database (below), and
    filtered with the collector's own code
    (`collector.FilterCursorChat`), so `empty`, `unsafe_format`, and
    `too_large` (its rows together over 256 MiB before filtering, what the
    filter keeps of them over 64 MiB, or one row over the 64 MiB record
    limit) are what capture would decide. Its size is its rows' bytes. Its
    project is the folder `workspaceIdentifier.uri` names, then the
    `folder` of `workspaceStorage/<workspaceIdentifier.id>/workspace.json`,
    then the one folder its messages' `workspaceUris` agree on; that folder
    goes through the [project rules](#project-resolution) like a working
    directory (so `--project` and the home, temporary, and excluded rules
    apply), and a chat with none is `project_unknown`. A chat Cursor
    deleted between the listing and the read is not counted, and one whose
    own values don't decode is `unsafe_format`. If a read fails because of
    the database itself (Cursor held a lock past the timeout, the copy
    failed, the file changed), the database counts as not checked, with
    that reason, and none of its chats is included.
  - **Subagent chats** of the Cursor chats the plan imports are not imported
    yet; the plan says how many ("N Cursor subagent chats are not imported
    yet", `cursor_subagents_not_imported` in JSON).
  - **How it lists.** With Cursor running (`-wal` and `-shm` present), in
    place with `mode=ro`, `readonly_shm`, and `query_only`: a read-only shared
    memory file still lets the reader take shared locks, so Cursor never
    overwrites a page mid-read. With Cursor closed (no side files), in place
    with `immutable=1`, which opens no side file and takes no lock; it is safe
    only because the reader confirms afterwards that the file's size,
    modification time, inode, and 100-byte header are unchanged and that no
    side file appeared, and otherwise reports "not checked". Opening a closed
    WAL database any other way would create `-wal` or `-shm` next to it.
  - **Not checked,** with a reason in JSON
    (`cursor_database_unchecked_reason`): `locked` (a `-journal` exists: an
    unfinished write that only Cursor may roll back), `unreadable` (one side
    file without the other, or an open failure), `unknown_format` (missing
    table or fields that don't decode), `changed_during_read`, and
    `transcripts_unreadable` (part of Cursor's transcript store couldn't be
    listed, so chats with transcripts can't be told apart; the database isn't
    opened). A missing
    database counts as checked with none. `--harness` without Cursor skips the
    read entirely.
  - If Cursor quits in the instant between the side-file check and the open,
    SQLite can leave an empty `-wal` behind. That is harmless (an empty WAL
    has nothing to replay), and the reader never deletes files next to
    Cursor's database. Such a database is listed as closed. One with an empty
    `-wal` and a `-shm` is listed as running (in place, `readonly_shm`), and
    the recovery evidence pass reads it as closed (see recorded-repository
    recovery above), so leftover side files don't lock recovery.

## Skip reasons

Every session found is either imported or has exactly one reason. When
several reasons apply, the first in this list wins.

| Code | Override |
|---|---|
| `already_archived` | — |
| `duplicate_session` | — |
| `registered_not_admitted` | future `--readmit` |
| `removed_by_undo`, `removed_by_retention` | `--include-removed` |
| `filtered_out` (shown only when filters are set) | — |
| `excluded_project` | `setup` |
| `home_directory` | `--include-home` |
| `above_home` | — |
| `temporary_directory` | `--include-temp` |
| `project_unknown`, `worktree_unresolved`, `identity_mismatch` | — |
| `empty`, `unsafe_format`, `too_large` (see below) | — |
| `start_unknown` (no record timestamp for Claude Code and Codex; no `createdAt` for a Cursor database chat) | — |
| `start_in_future` | — |

`too_large` uses capture's limits, so an import never skips a session the
collector would capture, or the reverse. The 64 MiB limit applies to what
the privacy filter keeps, not to the raw transcript, whose bulk is tool
output the filter drops. A transcript is too large when the raw file is over
256 MiB (four times the limit; such a file is not read at all), when what
the filter keeps of it is over 64 MiB, or when one record in it is over the
64 MiB record limit. A Cursor database chat is measured the same way, its
rows standing in for the file.

`cursor_database_only`, phase 1's placeholder for chats only Cursor's
database holds, is retired: those chats are now imported (phase 2). When the
database is not checked they are simply not found, and the plan says why.

## Evidence imports lack

Hook-captured sessions also carry lifecycle events (`Stop`, `SessionEnd`), the
final-response text, and the skill inventory observed while the session ran.
Imports get none of these:

- **Lifecycle.** None is created. `metadata.state` comes from the transcript
  (`nativeTurnEnd`).
- **Skills.** The skill inventory observer does not run. Today's skills
  attached to an old session would be false evidence. `skills_available` is
  empty.
- **Capture gap.** Metadata records `imported_without_hook_evidence`.

Three optional metadata fields are added, both in `Metadata` and in
`schemas/metadata.schema.json`: `origin`, `imported_at` (the `AdmittedAt` of
an import), and `started_at_source`. All three are omitted for hook sessions,
so existing metadata is byte-identical and the parser version stays the same.
The source bundle doesn't change either: a transcript hashes the same whether
a hook or backfill registered it.

## Registration and concurrency

The lock order stays `setup.lock` → `collector.lock` → `hooks.lock`. A hook
waits at most one second for `hooks.lock`, then drops its event
([capture/hook.go:128](../../internal/capture/hook.go)). So backfill holds `hooks.lock` for
only a few milliseconds at a time.

1. **Plan.** No locks. Record a fingerprint of the configuration: a hash of
   the parsed configuration without its bucket-privacy evidence, which the
   collector refreshes in the background, as setup's own check ignores it.
2. **Check storage.** Run `storage.VerifyAccess`. On failure, stop before the
   prompt and print `credentials.RecoveryAction`. `--dry-run` skips this.
3. **Confirm.**
4. **Commit the configuration.** Take `setup.lock` and hold it until exit.
   Reload the configuration. Abort if its fingerprint changed ("run backfill
   again"), if it is paused, or if a setup transaction is pending. Stamp
   `AdmittedAt` and check the clock: admission must not be earlier than
   `DestinationSince`, any target project's `ActivatedAt`, or the time the plan
   was made. `collector.lock` is taken before the
   reload and held through registration (step 5), so no collector pass runs
   while candidates are half-written, and `pause`, which also takes it, waits
   until registration ends. Take `hooks.lock` briefly to write the batch file,
   then the new projects, `ImportedHarnesses`, and any retention edit. The
   batch file records every configuration change the import makes, so undo
   can reverse it (a guard test diffs every configuration field).
5. **Register** in batches of at most 50 sessions or 100 ms:
   - Take `hooks.lock` and reload the configuration. Skip anything no longer
     admitted. A session whose start is after `AdmittedAt` is skipped as
     `start_in_future`, a backstop for the clock check in step 4.
   - For each session, re-stat the transcript and skip it if it's gone. Call
     `RegisterNewSession`. If the native index already has the session, a
     hook or another run got there first, so count it `already_archived`.
     Otherwise save a request with reason `backfill`.
   - Release `hooks.lock`.
   - Subagent candidates are written just before their parent is
     registered. A crash in between leaves a candidate with no parent, which
     the collector discards, and a rerun writes it again with the same
     archive ID.
6. **Upload.** Unless `--background` is set, run `runOnePass` repeatedly,
   exactly as `sync` does, until the batch has no pending work or a pass makes
   no progress. Progress is reported through a new
   `collector.Options.Progress` callback.

Other properties:

- **Ordering.** `collector.Run` handles pending sessions oldest-start first,
  so the transcripts Claude Code will delete next are uploaded first.
- **Crash safety.** A crash after step 4 leaves projects added and nothing
  registered; a rerun finishes the job. A crash during step 5 leaves some
  sessions registered, each with a request, so the collector uploads those and
  a rerun registers the rest. A crash during step 6 loses nothing.
- **Plan freeze.** The import is exactly the confirmed plan, keyed by native
  ID. Sessions that appear after the plan are not included.
- **Scale.** An unchanged session costs one stat per pass. Retention calls the
  bucket only when something can be deleted. Thousands of imports therefore
  add only local work to the 60-second pass, though a parser upgrade re-reads
  them all.

## Status, list, and privacy

- **`status`** adds an "Imported" line: sessions imported, sessions waiting to
  upload, and the last import. `status --json` adds `imported_sessions`,
  `imported_pending`, and `last_import`. Imports never count toward an app's
  `HookObserved`, its verification, or "waiting for first session".
- **Verification.** Read-back verification still runs on imports, but it
  never promotes an app to `verified_by_capture`.
- **`list` and `show`.** `list` marks imports and gains `--imported` and
  `--hook-captured`. `show` prints `origin`, `imported_at`, and
  `started_at_source`.
- **Privacy.** The plan shows only project roots and counts. Paths, native
  IDs, and content are never printed, logged, or written to diagnostics, and
  skips produce no diagnostics. Native stores are opened read-only. Setup
  exclusions always win.

## Phase 2: Cursor database chats

Status: implemented (P1 composer adapter, P2 source readers, P3 import
integration). Support stays `unverified` until the live check in decision 10.

Cursor's `globalStorage/state.vscdb` (table `cursorDiskKV`) holds:

- **`composerData:<id>`, one per chat.** Fields include `createdAt`,
  `lastUpdatedAt`, `workspaceIdentifier` (empty on 4 of the 7 probed chats),
  and ordered message headers in `fullConversationHeadersOnly`. Also
  `subagentComposerIds`, `modelConfig`, `usageData`, `isDraft`, and `_v`.
- **`bubbleId:<chat>:<message>`, one per message.** Fields include `type`,
  `text`, `toolResults`, `tokenCount`, `modelInfo`, timestamps, and large
  context fields.
- **`agentKv:blob:<hash>`, content blobs.** These may be encrypted, since
  chats carry a `blobEncryptionKey`.

For chats that also have a file on disk, the database holds timestamps and tool
results the file lacks. Phase 2 still imports only chats that have no file:

1. **One source per session.** Registration fields `SourceKind` (a typed
   string: `SourceKindFile`, the empty value, or `SourceKindCursorSQLite`,
   `cursor-sqlite`) and `SourceKey` (the chat ID, which must equal the native
   ID) are fixed at registration. A `SourceKindCursorSQLite` session never
   adopts a hook's `transcript_path`. Otherwise a resumed chat would switch
   formats and fail `nativeEvidenceExtends`.
2. **The file wins when both exist.** It is what hooks capture live.
   Upgrading file sessions to the richer database source is a separate
   decision.
3. **Snapshot reads** (`internal/cursorstore`). With Cursor running, the
   database is copied with SQLite's online backup API, from a read-only
   connection, in one uninterruptible step (a busy source is retried until
   the read's 30-second deadline, then the read fails rather than reading
   partially), into a `0600` file in a new `0700` directory under
   `filepath.Join(<temp>, "agent-archive-cursor-<uid>")`, where `<temp>` is
   on macOS the per-user `DARWIN_USER_TEMP_DIR` whatever `$TMPDIR` says (the
   folder Time Machine excludes, and the one launchd's collector, which gets
   only `AGENT_ARCHIVE_HOME`, and hook runs agree on), falling back to
   `$TMPDIR` and then `os.TempDir()` only if it can't be read; never the
   archive home, which may be backed up or synced. That root must be a real
   directory of this user's with mode `0700`, or no copy is made. A
   `cursorstore.Reader` takes at most one snapshot however many chats it
   reads: a collector pass holds one Reader, and so does a backfill plan,
   and each removes its copy when it ends; a copy that can't be removed
   fails the pass or the plan. Each snapshot directory holds an `flock`
   while its Reader uses it; directories older than an hour whose lock is
   free (a killed process's) are swept at the start of a pass, on a
   Reader's first read, and at the start of every `backfill` command
   (`history`, `undo`, and `--dry-run` included), through
   `cursorstore.RemoveStaleSnapshots`. An unlocked directory whose lock file
   is over a minute old is a killed process's and goes at once. With
   Cursor closed, the backup API would create `-wal` next to the source, so
   a chat is read in place as the listing is: `immutable=1`, then size,
   modification time, inode, header, and side files must be unchanged, and
   otherwise the read is discarded (`changed_during_read`). A missing
   database or chat is a missing source; anything else unsafe is "not
   checked" with a reason, and is retried on the next pass.
4. **`modernc.org/sqlite`, a pure-Go driver,** already added for the phase-1
   count (v1.46.1, the newest release that supports the `go 1.24.0` floor;
   builds use `toolchain go1.27.2`). It keeps CI and tests free of
   cgo. `mattn/go-sqlite3` would also work, because release builds already
   use cgo. Shelling out to `/usr/bin/sqlite3` is rejected because its output
   and version are uncontrolled.
5. **`CursorComposerAdapter`, with a new allowlist.** It keeps role, text,
   tool calls and results (through the existing tool filter), model, token
   counts, and timestamps. It drops context payloads such as
   `codebaseContextChunks`, `attachedCodeChunks`, `originalFileStates`,
   `diffHistories`, `images`, `consoleLogs`, and `recentlyViewedFiles`.
   Source format `cursor-composer`, a filter version bump, and golden tests
   from synthetic chats. Implemented as `CursorAdapter.FilterComposer` in
   `internal/archive/cursor_composer.go`, with filter version 8. The
   collector applies the transcript size limits first: the chat's rows
   together are its size, and each row is one record.
6. **Fail closed.** An unknown `_v` is `unsafe_format`. Missing message rows
   add a `cursor_bubble_missing` gap with a count; the probe had 432 headers
   and only 415 message rows, all in chats that also have a file. Blobs are not read, and add
   `cursor_blob_content_unavailable`.
7. **Change detection.** The scan signature is `(lastUpdatedAt, header
   count, last message ID, listed messages with a row, a hash of the last
   message's row)`, read in place in one transaction from a few indexed rows
   (the row count from the key index alone), never a copy. The database file
   itself changes constantly, so its stat is useless here. A read that
   failed for a reason reading again can't fix (a filter refusal, a size
   limit) is remembered at that signature, the versions, and the size limits:
   later passes report it, or keep its gap, without reading, even when a
   hook request arrives. A refusal leaves the request queued (its evidence is
   the only copy); a size-limit gap completes it, as blocking does. A chat
   in the database but not yet in the pass's older snapshot is a transient
   `changed_during_read`, not a deleted chat.
8. **Project and start.** The project comes from `workspaceIdentifier.uri`,
   then the `folder` of `workspaceStorage/<workspaceIdentifier.id>/workspace.json`,
   then the one folder the messages' `workspaceUris` agree on, and then the
   usual [project rules](#project-resolution); otherwise the chat is
   `project_unknown`. The start is `createdAt`
   (`started_at_source = cursor_composer`). See
   [Discovery](#discovery) for how the plan lists, reads, and classifies the
   chats.
9. **Subagents.** Not imported yet. Linking `subagentComposerIds` as linked
   sessions is not a small change: the collector's child registration and
   its checks on every scan (`CheckImportedSubagent`,
   `validateSubagentTranscript`) are built on transcript files whose records
   name the parent session and the agent, and a composer chat carries
   neither. The plan counts the subagent chats of the Cursor chats it
   imports and says they are not imported yet.
10. **Unverified until checked live.** Support stays `unverified` until one
    database-only chat is published and read back on a live Cursor, with its
    version recorded.

**Rewritten chats.** Cursor rewrites finished messages in its database (a
token count filled in late, an edited prompt, a checkpoint restore), so a
cursor-sqlite chat whose new records no longer extend what was published is
not blocked as `transcript_rewritten`, as a transcript file is. The new
snapshot is published as a replacement, and the chat carries one
`cursor_chat_rewritten` capture gap (collector evidence, provenance
`collector:cursor-rewrite`, no content) whose detail counts the rewrites and
whose observation time is the last one's, so a chat Cursor rewrites often
(late token counts are routine) does not grow a gap per rewrite. A change to
the chat's name alone, which filter 13 keeps on the chat's first record, is
not a rewrite: the chat is republished with its new name and no gap.

**Everywhere else a transcript file was assumed.** The collector reads each
registration through a small `sourceReader` interface (`Signature`,
`Filter`), with the file code as its first implementation and the
cursor-sqlite reader as its second. Metadata regeneration's change check and
`handoff --source local` (including its last-activity time, the chat's
`lastUpdatedAt`) go through it. Registration checks in place that the chat
still exists and counts it gone otherwise. Undo counts a database chat as
resumed when its `lastUpdatedAt` is after `AdmittedAt`, and says how many
chats it could not check when the database can't be read. A storage change
counts an un-uploaded database chat as pending, not as waiting for a
transcript.

## Code layout

| Area | Change |
|---|---|
| `internal/archive/types.go` | New fields and `Admitted()`. B1b adds `DestinationID`; phase 2 adds `SourceKind` and `SourceKey`. |
| `internal/config`, `internal/capture/hook.go` | `ImportedHarnesses` and `AcceptSession` via `Admitted()`. Hooks set `AdmittedAt` and `Origin`. |
| `internal/retention` | `Admitted()`, removal records, and a shared `deleteWholeSession` |
| `internal/collector` | Origin-aware skill observer and subagent lifecycle, `Progress`, oldest-first ordering. Phase 2: `sourceReader` (file and cursor-sqlite), remembered read failures, rewritten Cursor chats republished with a gap, `FilterCursorChat` for the plan |
| `internal/cursorstore` (phase 2) | Read-only access to Cursor's `state.vscdb`: the in-place listing, one-snapshot `Reader`, `ReadSignature`, and the snapshot directory |
| `internal/archive/views.go`, `schemas/` | Metadata fields and the capture gap |
| `internal/backfill` (new) | Discovery, resolution, plan, commit, batches, and undo. Discovery and resolution run over an injected file system and clock; the adapter pass, registration re-checks, and batch files use the real file system. |
| `internal/cli` | `backfill.go` (new), plus origin-aware status, verification, list, show, and setup |
| docs | The eligibility doc gains an "Imported sessions" section and the use-site table. The main spec and install.md point to this command where they say history is not imported. |

## Tests

- **Admission.** Cover every combination of origin, activation, destination,
  and app list. A legacy registration behaves exactly as today. Include the
  guard test.
- **Retention.**
  - An import with an old start and a failed first upload is not expired
    early.
  - After a bucket change, imports belong to the previous bucket, by
    `DestinationID` (B1b), or by time for a registration without one.
  - Removal records are honored, and overridden by `--include-removed`.
- **Hook resume of an import.** It is accepted, keeps `SessionStartedAt`,
  `AdmittedAt`, and `Origin`, and updates the path.
- **Resolution.** Table tests over a fake file system cover:
  - worktrees, present and missing
  - the nearest ancestor, and excluded projects
  - home, `/`, and temporary directories, with and without their flags
  - scratch chats
  - Cursor slugs with no match, one match, two matches (`a-b/c` against
    `a/b-c`), and a match found only through `workspace.json`
  - identity mismatches
- **Command.** Golden output for the plan, a filtered plan, `--json`, every
  skip reason, `history`, and `undo`. Also:
  - No terminal and no `--yes` means a refusal.
  - Declining changes nothing, locally or in the bucket.
  - `edit` updates the deletion date.
  - A failed storage check stops before the prompt.
  - A second run is a no-op.
  - A configuration change during the prompt aborts, and pause refuses.
  - A hook fired mid-registration succeeds within its one-second wait (real
    flock, as in `internal/capture/diagnostics_test.go`).
  - Crashes injected between steps converge on rerun.
  - Subagents inherit the import fields and carry no hook lifecycle evidence.
  - Hook metadata is byte-identical to before.
- **Undo.** It deletes metadata before sources, then forgets, writes removal
  records, and excludes only the projects the import added. `--project`
  limits its scope. A partial failure finishes on rerun. It cannot race a
  collector pass.
- **End to end,** using the local MinIO recipe with copies of real
  transcripts:
  - plan counts match the native ground truth
  - `list --imported` matches the plan
  - read-back verifies
  - no app is promoted by imports
  - `undo` restores the bucket

## Sequencing

| PR | Scope |
|---|---|
| B1 | Admission model: fields, `Admitted()`, `ImportedHarnesses`, use sites, removal records, origin-aware status and verification, metadata. No command. |
| B1b | `DestinationID`. Optional, but required before `--readmit`. |
| B2 | Discovery and `backfill --dry-run [--json]`. Read-only and useful on its own. |
| B3 | Import: storage check, retention `edit`, commit, batches, registration, subagents, ordering, progress, `--background`, `history`, `list --imported` |
| B4 | Undo. Ships in the same release as B3. |
| B5 | Docs and live acceptance on the probe Mac |
| B6–B9 | Phase 2: snapshot reader and driver; composer adapter; `sourceReader` and source fields; discovery and live Cursor acceptance |

## Open questions

1. **Home and temporary directories.** They are skipped by default, the only
   gap in "everything". Adding home as a project would silently capture
   everything under it. Recommendation: keep this default; one flag reverses
   it.
2. **Added projects capture new sessions.** The alternative is an import-only
   project state, which would need a third inclusion state and a matching hook
   rule. Recommendation: capture, as specified. The plan states it.
3. **Keeping sessions forever.** A "never delete" retention setting is more
   useful once imports exist. Recommendation: propose it separately, after
   B3.

## Recorded project recovery

The optional bounded Codex `git.repository_url` supplies a normalized repository
key through native metadata; malformed optional Git evidence does not invalidate
thread identity. Shared `sourcefacts.RecoveryResolver` evaluates all configured
roots, including exclusions, through an injected bounded local Git identity port.
SSH and HTTPS spellings use the existing archive normalizer. Different physical
clones remain ambiguous; aliases of the same checkout coalesce only with equal
scope decisions. Inherited repository identity does not identify a missing
subtree. Descendant exclusions therefore require an exact reviewed import
mapping. Live filesystem ownership and nearest configured rules take precedence.

The discovery catalog checkpoints an incomplete configured-root sweep. Each
pass allows at most 128 identity lookups; completed sweeps refresh next pass,
and cached prefix metadata is validated before a resumed sweep. Unavailable
entries cannot certify uniqueness. Backfill's proposed roots are the only
exception: a proposed root whose completed, within-budget lookup found a
checkout top level and an origin value that yields no repository key is
non-owning; a failed origin read is unknown, not keyless. Every
`remote.origin.url` value is read (`git config --get` reports only the last):
the checkout is keyless only when no value yields a key; a value that yields
one is the checkout's key wherever it appears; values that yield different
keys leave the identity unknown, neither keyless nor matched. It cannot match a
recorded key, so a match elsewhere stays unique among the identifiable roots.
It has no dependency stamps, so every admission slice observes it again
(sharing the coalesced semantic sweep) and requires the same keyless top level
and observer scope; gaining a key or becoming unobservable invalidates the proof.
Unobserved or budget-exhausted proposed roots, and all configured roots, remain
uncertainty. A keyless origin that is a `url.insteadOf` shorthand for the same
repository is the accepted residual: it is never normalized, so it could not
match the recorded key even if configured. Bounded Git config origin names identify
metadata dependencies; no partial Git configuration parser interprets remotes.
Source and Git reads run outside admission locks. Under-lock scope digests and
permission generations reject configuration changes.

Backfill's uniqueness proof is over the clone roots its evidence names: the
configured roots plus the live repository roots of every observed session, from
every app, before output filters. Evidence that could name a root but was not
observed (an unreadable store or folder, a session whose folder is unknown or
unreadable, a native inventory that changed other than by added transcripts,
an added transcript whose folder cannot be told, an unavailable or unsettled Cursor
database, an exhausted budget, more than 1,024 distinct witness roots) is a
recorded gap. Evidence that names no root (a Cursor chat with no folder,
workspace reference or message folder) cannot be a clone and is not a gap. Gaps are kept as a finite set of (app, cause)
values and isolated by app: a gap blocks recorded-key recovery only for the
sessions of the app whose evidence is missing, and a gap that cannot be
attributed to one app (a native inventory that changed other than by added
transcripts, the 1,024-root bound, an unreadable folder of unknown app) blocks
every app. A gap in another app's
evidence does not block. This holds with or without `--harness`: an app the
filter leaves out still contributes the witnesses it observed (they can make a
recovery ambiguous or name its destination), and its gaps block only its own
sessions, which the filter hides anyway.

Admitting past another app's gap is safe within the documented residuals. The
proof was never a census of every clone on disk: a clone no agent ever ran in
is already invisible, and another app's gap widens that set only by clones
known solely to that app's sessions. Configured roots, including exclusions,
are enumerated by repository lookups independent of every app store, so no gap
can hide capture policy, and a clone nested in an excluded root resolves to
that root and is never a witness. The only wrong outcome is recovering a
session into one included checkout of its own repository, configured or
proposed, when it actually ran in a second, unconfigured clone known only to
the app with the gap, instead of leaving it unresolved. It is never an
excluded root, a root inside one, or a root no witness could propose; it is
the same residual as a keyless clone and a
`url.insteadOf` origin. The plan reports it: `recovery_evidence_gaps` in JSON
(each `app`, `cause`, and the `blocked` and `recovered_without` session counts
by app) and a "Project recovery evidence" note in text. An app's own gaps
still block its sessions as defense in depth. Witnesses from an incomplete
Cursor database epoch remain clone evidence but cannot propose a destination,
because they cannot be renewed; such an epoch needs no renewal at admission.
A complete epoch that changes between the plan and confirmation (its settled
stamps differ, or a renewed read of an unsettled database differs) is isolated
the same way at admission. It blocks Cursor's own recoveries, recoveries of
unknown app, and any recovery whose destination rests on Cursor database
evidence: an unconfigured destination with no current witness from another
source that could propose it on its own. It does not block another app's
recovery into a configured root or into a destination that current evidence
from another source can propose. Such a proof used Cursor's chats only as
possible clones, and a change can at most remove one (the proof stays
unique) or add one (a second checkout known only to Cursor, the residual
above).
A native transcript that was only appended to after its header was read (same
file, same mode, larger size) keeps its own `source_changed` skip but stays
clone evidence that cannot propose a destination, and further appends renew
it; a truncation, replacement or other rewrite is still a gap. Header facts
come from leading complete records, so an append cannot change them. An
in-place rewrite that grows the file is indistinguishable from an append by
file metadata; that is the accepted residual of the rule. The candidate
diagnostic names the highest-priority blocking cause instead of
`project_inventory_unavailable`: `project_budget_exhausted`,
`project_witness_limit` (retrying cannot help; an exact `--map-project`
can), `native_store_unreadable`, `cursor_database_unavailable`,
`native_inventory_changed`, `cursor_chat_folder_unavailable`, then
`session_folder_unknown`. Exact mappings and configured-path ownership do not
depend on the witness inventory.

Running agents keep creating transcripts while a plan is made and confirmed
(a review agent in a temporary clone starts a rollout every minute), and a new
transcript can only add evidence: it cannot take away a clone another
transcript named. So a native inventory whose only change is added membership
does not fail every recorded recovery. Every app's store is listed again with
the app's own discovery, and the headers of transcripts the plan did not
observe are read with discovery's header readers, at most 64 transcripts and
16 MiB together; past either bound (including a header the byte bound would
cut short) the change is unattributed and blocks every app, as before. Each
added transcript is judged by the root its folder resolves to, with committed
configuration only:

- A folder that is not an existing, unconfigured repository root (a configured
  project, a plain or temporary folder, a deleted folder) names no clone that
  could compete with a recorded key and is ignored, as the witness inventory
  ignores it.
- A root the witness inventory already holds adds nothing; a new Cursor chat in
  a project folder a planned chat was found in names that folder's root.
- A new root found while planning joins the inventory as evidence that cannot
  propose a destination, so the recovery resolver reads its key and a clone of
  the same repository makes the recovery ambiguous; its renewal accepts
  appends. Found after planning (confirmation or an admission slice), the new
  root's key is read and every recorded-key recovery of that key fails; a
  keyless root (as a proposed root above) or a repository without a key
  matches none.
- An added transcript whose folder cannot be told (no folder, a header that is
  not understood or whose IDs disagree, a Cursor project folder no planned chat
  had, a root whose key cannot be read), and a store that can no longer be
  listed in full, is a `native_inventory_changed` gap of that app, isolated
  like any other per-app gap. After planning it blocks that app's
  recorded-key recoveries and recoveries of unknown app.

Removed, replaced or rewritten observed transcripts and directories stay
strict for every app. An entry removed from a listed directory that discovery
never observed (not a transcript of the store's layout) was never evidence.

Confirmation admits recovered sessions one by one. A session whose project or
source evidence is no longer current when the import is confirmed (its own
transcript changed, its proof's evidence changed, or a new clone of its
repository appeared) is left out as `source_changed`, as planning leaves out a
transcript that changed while it was read, and the rest of the confirmed plan
is imported: a strict subset of what was confirmed. The configuration change
is recomputed for what is left: a proposed project only the left-out sessions
needed is not added, and every remaining proof is bound to that configuration
(`PolicyContext`). The import says how many were left out and to run backfill
again (setup names its own retry command). Confirmation still refuses, with
nothing changed, when no session would be left or when the check ran out of
time or was cancelled, since a session that read as changed then may only have
run out of time. It also refuses when a dropped proposed project nests with a
plain folder the import still adds (one inside the other): the plan decided
what to keep out of that folder with the dropped project as a project of its
own, so without it the folder would capture what the plan never checked.
Admission slices already skip a stale session per session.

`--map-project OLD_CWD=CONFIGURED_ROOT` is exact and invocation-local, with at
most 128 mappings and 4,096 bytes per absolute path. The first equals sign is
the delimiter. Its canonical sorted digest participates in `BatchFilters`
equality. Local registrations retain original cwd, method, normalized key and
policy/inventory context and bounded canonical cwd evidence, independently of
timestamps and destination. Canonical cwd evidence travels with each proof; the
4096-entry decision cache does not limit the number of distinct import candidates.
No raw repository URL enters durable evidence. A mapping targets an existing included
configured root and never overrides configured or live ownership.

Recovery evidence is rechecked before admitting a new owner. Git dependency
stamps include HEAD, worktree/common config paths, empty or absent included
files, all candidate global/system config paths reported by Git, and scratch
ancestors with no Git metadata and absent nearer checkout markers for configured
Git subdirectories. A documented no-value result establishes that no config path
applies. Optional `git var GIT_CONFIG_GLOBAL`/`GIT_CONFIG_SYSTEM` usage errors
select semantic validation instead of making otherwise known identity unknown.
The capability cache and retained root observations belong to one pass and are
invalidated by executable metadata or effective environment changes; version
strings are never authority. A content-free observer-scope digest binds temporary
inventory evidence to that executable and environment. Exact mappings share a
single planned identity per destination root; a later candidate cannot overwrite
the observation on which an earlier proof depends.
Name/origin enumeration has a 256 KiB output cap; URLs and short queries retain
the 4 KiB cap. Each command has the existing 500 ms timeout and dependency
collection has a shared 500 ms deadline, at most 128 paths and 64 ancestors.

A semantic epoch binds the full sorted configured inventory (including excluded
roots), canonical configured paths, policy/mapping context, and each root's
known/unknown state, canonical checkout top level and normalized repository key.
The planning sweep and second admission sweep must agree on every root, even
scratch or excluded roots that did not match the selected key. Unknown, changed
or cancelled evidence stays pending. The second sweep is coalesced once per
bounded admission slice and costs at most 1024 root observations independently
of its number of imported sessions. The initial discovery sweep retains its
128-lookup continuation cursor; its partial entries never establish uniqueness.
Each lookup verifies its root/key again after collecting bounded metadata.
Semantic sweeps cannot stamp unknown absent system/global paths and are not an
atomic filesystem snapshot: a change after the last observation, or a change
and reversal between sweeps, can escape observation. Enumerated local stamps,
canonical original cwd and native source observation checks remain required.
Git reads occur before the admission lock, which rechecks configuration,
destination and consent. `validation_method=semantic` is retained privately as
ownership provenance. Validation caches and observation contexts are temporary;
retained admitted ownership does not acquire a continuing live-Git requirement.
New discovery checks the original source observation and original cwd again; imports validate each source and
cwd and coalesce the common inventory check for each short registration hold.
Import and recovered discovery attribution retain the file identity, size and modification time sampled
before bounded native header inspection. It checks that same observation after
header inspection and repository resolution. Imports also check after full filtering, and before each
registration hold for recovered attribution. A rewrite or replacement during those stages reports
`source_changed`; a settled retry resolves current cwd/repository facts. Automatic
discovery retains file identity only in memory; persisted scheduling hints must
reprobe the header whenever its cwd is missing, including configured descendants
without a recorded repository key, before admitting new ownership.
These checks run outside hooks.lock; current policy and consent still run under
it. Already retained ownership continues without rerunning recovery evidence.

Repository metadata validation has a separate finite allowance from physical
project resolution: at most `max(1024, 2 * configured_roots * 129)` dependency
rechecks and canonical-path resolver invocations per discovery pass (at most
1024 roots). Two maximum-sized inventories leave room to validate a resumed
prefix and admit a recovered source. Further candidates remain retryable.
Planning validates the inventory prefix once per allowance, not once per
candidate: later candidates reuse entries already validated or observed in
the same pass, so the prefix costs at most `128 * configured_roots` rechecks
however many sessions are planned. A failed freshness check, or a new
allowance, makes the next candidate revalidate the prefix; admission still
rechecks every entry. A recovery limit that remains is reported without a
promise that retrying clears it.
`repository_metadata_operations` reports those invocation units; a canonical
path invocation may perform multiple filesystem probes. Import resets this
allowance per short hold, with one common inventory check for its candidates.
Initial Git identity lookup limits remain unchanged; semantic admission has the
separate 1024-root bound above. No-context programmatic import callers get a
30-second validation deadline per slice. Filesystem proof checks cannot
prevent a change after their final observation; the next admission slice or
pass refreshes the evidence.
