# Routine use and troubleshooting

Run `agent-archive` for a short command guide, or `agent-archive COMMAND
--help` (or `agent-archive help COMMAND`, including `help backfill undo`) for
options and examples. Help never activates hooks, reads credentials, or
changes state. Invalid flags fail before a command starts, with one line
naming the problem. Every command's options and the exit codes are in the
[CLI reference](../reference/cli.md#exit-codes). Commands that need setup and
find none say so without creating the data directory.

## Everyday commands

- `sync` collects and uploads once now, reporting results. It respects pause.
  A session is republished at most once per upload interval (3 minutes), so
  new activity soon after an upload waits; `sync` says so and when the first
  one is due:

  ```text
  Scanned 5 session(s): 0 published, 5 waiting for the upload interval (next at 14:27), 0 unchanged, 0 failed.
  ```

  Waiting work is saved locally and uploaded by the next pass after that
  time; `status` counts it under Pending.

  `sync` also counts Claude Code subagents it is still waiting on, and
  subagents it decided not to capture:

  ```text
  Scanned 2 session(s): 0 published, 2 unchanged, 0 failed; 1 subagent(s) waiting for transcripts; 1 subagent(s) not captured.
  ```

  `sync` also counts sessions whose scan it put off until a later pass,
  by reason, instead of listing them as failed:

  ```text
  Scanned 40 session(s): 1 published, 30 unchanged, 0 failed; 1 rechecking after the source changed while being read; 8 waiting for Codex history coverage.
  ```

  Codex history coverage is the work of finding every rollout file that
  belongs to a Codex thread; until a pass completes it, a session whose
  history spans several files can't be read in full. A source that changed
  while being read is read again. Neither is a failure: the next pass
  retries, the session stays pending, and `status` lists it as
  **waiting for Codex history coverage** or **rechecking after the source
  changed while being read** rather than as failed. A session still put
  off after 24 hours of passes is reported as failed, with "still not read
  after 24 hours of retries", on every pass until one reads it.

  None of these is a failure, and none changes the exit code. Claude Code
  reports some background agents with a transcript it never writes; such a
  subagent waits up to 30 minutes, then is not captured and its parent
  session records the link as unavailable, with a
  `subagent_transcript_never_written` capture gap. `status --verbose` shows
  how many are waiting, and how many of each type (as Claude Code reported
  it; the type stays on this machine) were dropped this way in the last 7
  days. A subagent lost for another reason (its transcript exists but
  can't be read, or doesn't match its parent session) is a failed session:
  `sync` names it and exits 1, once.
- `pause` persists until `resume`. If work is still running, the command
  reports that no settings changed, names the command holding the collector
  lock (and its process ID), and asks you to retry after it finishes. `sync`,
  `setup`, and `uninstall` name it the same way.
- A local file that cannot be read is named, with a way out. `status` reports
  around the ones it can do without (collector status, storage health,
  capture diagnostics, a single session's records) with a warning, and the
  collector or the next hook replaces them. `setup` offers to move a damaged
  or newer-version saved setup aside (renamed `*.corrupt`), and
  `setup --abandon-recovery` does the same for a damaged recovery record. A
  damaged `config.json` stops every command, which names it; restore it from
  a backup, or move it aside and run `setup` again.
- If your home folder is itself a Git checkout (a dotfiles repository, say),
  every command stops and names it: the archive's data never goes inside a
  checkout. Set `AGENT_ARCHIVE_HOME` to a directory outside it.
- Sessions registered before a pause catch up after `resume`, including
  activity written during the pause. New sessions begun while paused are not
  imported.
- Reducing retention shows how many locally owned sessions are affected and
  the cutoff before it asks. The collector applies the new policy.

Session identity recovery and older-version data-home restrictions are described
in [Local session identity and recovery](../reference/session-identity.md).

## Reading status

```sh
agent-archive status
agent-archive status claude
agent-archive status --verbose
agent-archive status --json
```

Status uses local evidence and a read-only check of the scheduler (launchd, or on Linux the systemd user manager); it never downloads
conversations. It leads with the overall state and, unless that is Ready,
the one thing to fix, with each command to run on its own line:

```text
Agent Archive  ● Needs attention

  ! The last sync failed: storage refused access
    Check storage access and run agent-archive sync.
    To change credentials, run agent-archive setup and choose storage.

Capture
  ✓ Claude Code 2.1.283       hooks installed   212 sessions (+40 subagents) · 200 archived (1 verified) · 48 imported · 3 uploading
      ~/agent-archive   started 11:12   waiting for its first upload
      ~/agent-archive   started 11:04
      ~/styleprofile    started 09:24
    · 17 sessions have capture gaps
  ✓ Cursor 3.21.13            hooks installed   14 sessions · 14 archived (14 verified) · 27 imported · 1 uploading
      ~/personal_website   started 11:40
  ! Codex 0.155.0-alpha.9.2   hooks installed   no sessions yet
    Approve the archive hooks with /hooks in Codex.

Storage
  ! r2://agent-archive/agent-archive/   refused access on the last pass · uploaded 2 minutes ago
  ✓ Background collector on             last scan 1 minute ago
  ! Bucket privacy not verified         R2 object credentials can't inspect it; see https://developers.cloudflare.com/r2/buckets/public-buckets/

More: agent-archive status --verbose · agent-archive status claude
```

**Capture** has a row per app with its version, capture method, and session
counts. For Codex with automatic discovery enabled, the main row says
**automatic capture** and names the selected project scope. Waiting or unknown
capture states remain visible. Optional hooks do not add a setup step to the
normal status screen.

Session counts distinguish sessions found from sessions **archived** (uploaded)
and **verified** (the uploaded publication was read back). Archived and verified
counts cover automatically captured sessions and include subagents where present;
imported sessions are counted separately. An archived session can still be
uploading newer changes or have capture gaps; the counts do not promise a complete
transcript. Imported sessions and sessions waiting to upload have separate counts.

Under each app are at most five uploading sessions (`status APP` lists all),
with the project, start time, and any failure. A session without its first
upload says **waiting for its first upload**; one whose last scan was put off
until the next pass says why (**waiting for Codex history coverage**, say)
and is not a failure. Capture gaps and Cursor chats
without transcripts are reported separately. Under Codex, **N Codex sessions
from the 7 days before setup were not captured** counts sessions that started
in the week before discovery was enabled and are not archived, with the
`backfill --since` command that imports them (see
[setup](../getting-started/setup.md#after-setup)).

**Hooks installed** means the hook configuration is present, not that the
hooks have run or been approved. `status --verbose` reports hook execution
as observed or not yet observed, separately from hook trust. Codex hook approval
instructions remain visible in normal status when discovery is disabled and
hook execution has not been observed. With discovery enabled, optional hook
information, scan counts, and source format diagnostics are available in
verbose output. Skipped scan observations still show a warning in normal status.

**Storage** has a row each for the
destination with the last upload (! when the last pass failed on storage),
the background collector, bucket privacy (with the provider's guidance
when this machine can't inspect the bucket),
and any error of the last sync that the line at the top doesn't already
state. ✓ is fine, ! needs you, ✗ is blocked, and · is information. Times are
relative to now and paths under your home folder start with `~`. The screen
stays the same length however many projects you include.

- **`status APP`** (`claude`, `codex` or `cursor`) shows one app in full:
  its row, every uploading session, and a table of its projects with the
  sessions each captured, imported and is uploading, and how far read-back
  has got, then the same Storage section as `status`.
- **`status --verbose`** adds what the short screen leaves out: each app's
  upload and read-back progress, a row per project, the included projects,
  skill evidence, the imports line, why bucket privacy couldn't be
  verified, when storage access was last checked, every error of the last
  pass, and the pending count. Then a **Details** section has what is
  behind each row: the state codes, exact times (UTC),
  full paths, each app's installed version and support, each project's
  verification state, read-back evidence and retries, the bucket privacy
  check's reason code and guidance, and each problem of the last pass as
  the collector recorded it, one per line. Include it when you report a
  problem.
- **`status --json`** is the same evidence as a versioned document for
  scripts ([JSON output](../reference/json-output.md#status---json)); it
  separates configured, hook-observed, captured, published, and
  read-back-verified evidence. `--verbose` doesn't change it.
- **Before setup**, status shows only that setup is needed (`--json`:
  background `missing`, authentication `not_configured`).
- **A failed sync** shows each problem the last pass recorded on a ✗ row of
  its own in the Storage section, unless the line at the top already says
  it: a lone storage failure is named there ("The last sync failed: storage
  refused access"), and `status --verbose` still shows its row. Its cause is
  named when the storage provider
  refused the request: **Storage refused access** (the credentials aren't
  allowed to list, read, write and delete in the bucket),
  **Storage didn't accept the credentials** (an expired or unknown key),
  **The bucket doesn't exist**, or **The bucket is in a different region**.
  Any other failure is shown as **Last error:** with the collector's own
  words. The provider's raw error is in `status --verbose`.
- **Sessions that failed in the last pass** are summarized on one
  **Last error:** row by kind, each with its count (sessions and subagents
  apart) and what to do: for example "2 sessions failed to reach storage
  (network or service unavailable) — check the network and the storage
  service, then run agent-archive sync (the next pass also retries)". A
  session over a transcript size limit keeps its last snapshot; the row
  names the limit, and `agent-archive status APP` lists each such session
  with its size and how to keep capturing the work (continue it in a new
  session; `agent-archive handoff` carries the context there).
  `agent-archive sync` prints each session's own error,
  and `status --json` has each session's kind as a code
  (`collector.session_issues`). When no kind is about storage, status
  leads with the most pressing one ("Some sessions could not be captured")
  and its next step instead of **The last sync failed**. When everything
  the last pass recorded needs nothing from you (a subagent that could not
  be captured, a transcript over the size limit), status doesn't lead with
  a failure at all, and shows those problems on · **Last pass:** rows
  instead of ✗ **Last error:** rows.
- **Background collector on** (`--json`: background `loaded`, or `running`
  while a pass is executing) means the scheduler (launchd, or the systemd
  user manager on Linux) knows the scheduled job. When it
  **belongs to another installation** (`another_installation`), the scheduler
  runs this installation's job from a different plist or unit file. That job
  belongs to another installation and is left alone: set `AGENT_ARCHIVE_HOME` to a data
  directory of this installation's own, or uninstall the other one.
- **agent-archive can't run from where setup installed it** means the hooks
  or background collector run an `agent-archive` executable that has since
  been moved, deleted, or made non-executable. Run
  `agent-archive setup --refresh` from the binary's new location: it points
  the hooks, the collector's job definition, and the skills at it, asks nothing, and
  changes no other setting. (A hooks row that says `hooks missing` is
  repaired the same way.)
- **The background collector can't load your AWS profile** (S3): the
  collector runs with the AWS files and `PATH` setup recorded in its
  job definition (LaunchAgent or systemd service), and one of them no longer
  works: an `AWS_CONFIG_FILE` that
  moved, or a `credential_process` helper that is no longer on that `PATH`
  (a LaunchAgent from an earlier build has only launchd's
  `/usr/bin:/bin:/usr/sbin:/sbin`). `agent-archive sync` from your shell
  may still work. Run `agent-archive setup` again from a shell where the
  profile works. A warning that your shell's AWS settings files differ from
  the collector's means `sync` here and the collector read different
  profiles.
- **The background collector couldn't get AWS credentials** (S3): the
  profile's `credential_process` helper ran but failed. It runs
  without most of your shell's environment: aws-vault's and 1Password's
  settings for where credentials live are passed on, but other settings
  aren't
  ([configuration](../reference/configuration.md#environment-variables)).
  Put a missing setting in the helper's own configuration. A helper that
  needs you to unlock it or sign in (a locked vault, an expired `op`
  session) fails until you do. Check with `agent-archive sync`, then run
  `agent-archive setup` again from a shell where it works.
- **Capture** rows say how far each app has got: waiting for its first
  session, session seen, captured but not uploaded, uploaded with read-back
  pending, or archived and verified. Configuration alone never establishes
  capture. An overall `Ready` state, installed hooks, or a reachable bucket
  is not proof that a particular app has published a new session and had it
  read back. Look for **archived, verified** on that app's row, then use
  `agent-archive list` and `agent-archive show SESSION_ID` to inspect the
  archived metadata.
- **Paused** means no new sessions are admitted. Run `agent-archive resume`,
  then start a new session in an included project; a session begun while
  paused is not added later.
- **The storage row** shows when the bucket was last found reachable with
  the configured credentials, and whether by setup's check or the collector
  (its access probe, or a pass that uploaded), or why the last check failed.
  The collector checks storage with one synthetic round trip for a new
  configuration, retries failed checks, and refreshes the check after four
  minutes. A check over ten minutes old is flagged, unless collection is
  paused, when the last check is shown with its time.
- **Capture gaps.** An app's row counts the sessions whose last capture
  recorded a gap; `status --json` lists them (`capture_gaps`,
  `sessions_with_capture_gaps`), as described in
  [JSON output](../reference/json-output.md#status---json).
- **Read-back.** Each publication is read back and compared. One that cannot
  be read back is retried with increasing delays (one minute up to a day), at
  most five per pass, oldest first; status says when it retries next and how
  many attempts it has made, or that what was read back doesn't match. It
  does not fail `sync`.
- **Collection stuck.** Every command that takes the collector lock records
  which command it is and when. When one has held it for over two hours
  (twice a pass's hard time limit), status says collection is stuck and names
  the command and its process ID.
- **Notes** count local state files a pass could not read and moved aside,
  and session summaries this version cannot refresh.
- **Authentication** in `status --verbose` and `status --json` says whether
  the storage check came from a manual `sync` or the background environment.
  App versions a hook doesn't report stay unknown.

## No Claude Code session

Check that setup includes **Claude Code** and the project where the session runs (`agent-archive status --verbose` shows the Capture and project rows). Start a new session in that project; an open session resumed after setup is ineligible, while `/clear` starts a fresh one. There is no hook approval step for Claude Code. Send a prompt, run `agent-archive sync`, and check the Claude Code row again. If it says **captured, not uploaded yet** or **read-back pending**, follow the Storage and retry details under [reading status](#reading-status). If it is still waiting for a first session, inspect the hook state and capture diagnostics in `status --verbose`; [session eligibility](../reference/session-eligibility.md) explains other exclusions.

## No Codex session

Check that setup includes **Codex** and the project where the session runs (`agent-archive status --verbose` shows both). In Codex, run `/hooks` and approve the archive hooks; installed hook files alone do not mean Codex runs them. Start a new session in the included project (`/clear` also starts one), send a prompt, then run `agent-archive sync` and check the Codex Capture row. If the row shows local capture but no verified archive, use the Storage and read-back details under [reading status](#reading-status). If no session was seen, check hook approval, the hook state and capture diagnostics in `status --verbose`, and [session eligibility](../reference/session-eligibility.md).

## No Cursor session

Check that setup includes **Cursor** and the project where the Agent chat runs (`agent-archive status --verbose` shows both). Start a **new Agent chat** in that project and send its first prompt; continuing an older chat does not establish a fresh start, and `/clear` is not the Cursor path. Run `agent-archive sync` and check the Cursor Capture row. If it shows local capture but no verified archive, use the Storage and read-back details under [reading status](#reading-status). If no session was seen, check the hook state and capture diagnostics in `status --verbose`. A chat with transcripts disabled can register but has no transcript to upload; see [session eligibility](../reference/session-eligibility.md#cursor).

## No picker or prompt in an agent's terminal

If `list` prints a table instead of opening the browser, bare `show` or
`handoff` says to name a session, or `setup`, `uninstall`, `backfill`, or `purge apply`
refuses with "Prompts are off because ...", agent-archive believes a coding
agent is running it: `AGENT_ARCHIVE_NONINTERACTIVE` is set, or
`CLAUDE_CODE_SESSION_ID`, `CODEX_THREAD_ID`, or `CURSOR_AGENT` is (a terminal
opened from inside an agent can inherit them). Nothing was changed. Check
with `env | grep -E 'AGENT_ARCHIVE_NONINTERACTIVE|CLAUDE_CODE_SESSION_ID|CODEX_THREAD_ID|CURSOR_AGENT'`.
To be asked anyway, run the command as
`AGENT_ARCHIVE_NONINTERACTIVE=0 agent-archive ...`, or `unset` the variable.
`--yes` skips a confirmation without it. `agent-archive: AGENT_ARCHIVE_NONINTERACTIVE="..."
is not a valid setting` means the value is not one of 1/true/yes/on or
0/false/no/off; fix or unset it.

## My agent does not use the skill

The `agent-archive` skill ([agent skills](agent-skills.md)) is what lets Claude
Code, Codex, or Cursor pull in a past session when you ask.

1. **Start a new session.** Claude Code and Codex pick up a new skill in a
   running session (restart the app if it does not appear; Claude Code needs
   `/reload-skills`, or a new session, when it had no `~/.claude/skills`
   folder when the session started), but Cursor's
   documentation does not say it does, so start a new chat there. Codex reads
   `~/.agents/skills`, and Claude Code `~/.claude/skills`
   (`$CLAUDE_CONFIG_DIR/skills` when set).
2. **Check that it is installed.** `agent-archive status --verbose` says when
   agent skills are turned off (`agent-archive setup --skills` turns them
   on), lists the skill files setup wrote, and warns about one written by an
   earlier release (run `agent-archive setup --refresh`; the installer does it
   when you upgrade). If setup said `Left ... as it
   is`, a file that is not setup's is at that path; move it aside and run
   setup again.
3. **Ask for it plainly**, naming what you want: "pull in my Codex session
   about the login bug". You can also name the skill: `/agent-archive` in
   Claude Code, `$agent-archive` in Codex (or pick it from `/skills`), and in
   Cursor "use the agent-archive skill".
4. **Expect a question the first time.** Claude Code asks before it uses the
   skill (a `Skill(agent-archive)` permission rule allows it for good) and
   before running `handoff`, `list`, or `show`, and Codex and Cursor apply
   their own approvals; the skill deliberately does not pre-approve them
   ([permissions](agent-skills.md#permissions)). Where nothing can ask, as in
   `claude -p`, the agent is refused instead and should say so; allow the command in that agent's settings, or run it yourself.
5. **A sandbox may block it.** If the agent says the network or the credential
   store (the Keychain, or on Linux the credentials file) was blocked,
   `handoff "<words>"` still finds a session on this machine by its name,
   title, branch, project, app, a PR number, or the start of its ID (it looks
   there first and needs no network). `list`, `show`, and sessions that exist
   only in the archive need the blocked access: allow the command, or run it
   yourself in a terminal.

## Linux and systemd

On Linux the background collector is a systemd user timer. These are the
things that go wrong there that do not on macOS; [what was tested, and what
was not](../getting-started/install.md#platforms). To look at the job
yourself (the unit is `agent-archive-collector` for the default
installation, and `agent-archive-collector-<hash>` for one with its own
`AGENT_ARCHIVE_HOME`):

```sh
systemctl --user status agent-archive-collector.timer
systemctl --user list-timers
```

The collector writes `collector.log` and `collector-error.log` in the data
directory, not to the journal.

- **"The systemd user manager cannot be reached (this session has no user
  bus)."** Setup and `uninstall` say this over SSH without `pam_systemd`, in a
  container, after `su`, and wherever nothing runs a user manager. `status`
  does not give the reason: it says "The background collector couldn't be
  checked" and shows the collector as *state unknown*, "systemctl couldn't
  say" (`--json`: background `unknown`). Run the command from a login session that has
  the bus, or run `loginctl enable-linger` once (as yourself, or with `sudo
  loginctl enable-linger "$USER"` where your system asks for it) so the user
  manager runs without a login; a shell that was already open may also need
  `export XDG_RUNTIME_DIR=/run/user/$(id -u)`. Check that `systemctl --user
  status` answers, then run `agent-archive setup` again. "This system was not
  booted with systemd" means systemd is not the init system (many containers,
  WSL without systemd enabled): there is no user manager to reach, and
  agent-archive has no cron or other fallback, so setup cannot install the
  background job there.
- **Lingering is off.** `status` has a note under Notes, and `status --json`
  has it in `background_warnings`: the collector runs while you are logged
  in and stops when you log out. Hooks still record sessions and the next
  pass after you log in uploads them. Run `loginctl enable-linger` to keep it
  running.
- **Setup fails with "Unit file agent-archive-collector.timer does not
  exist".** The units are always written to `~/.config/systemd/user`. The
  user manager searches `$XDG_CONFIG_HOME/systemd/user` instead when its own
  environment (not your shell's) sets `XDG_CONFIG_HOME`, as `pam_env` or a
  `user@.service` drop-in can. Setup rolls back and the error names the
  directory it wrote to and says to run `systemctl --user show
  --property=UnitPath`, which lists the directories the manager reads. Unset
  the variable for the manager, log in again, and run setup again.
- **A warning that the shell's `XDG_CONFIG_HOME` or `XDG_CACHE_HOME` differs
  from the collector's.** The collector uses what setup recorded, so a
  backfill from this shell and the scheduled pass would look for Cursor's
  database, or keep its temporary copies, in different places. If this
  shell's value is the right one, run `agent-archive setup` again from here.
  **Known limit:** a user manager can get either variable from outside your
  shell (an `environment.d` file, a desktop session's `import-environment`).
  Setup records only what its shell has, so when the shell had neither, the
  job uses the manager's value, and `status`, which compares only the
  shell's values with the recorded ones and cannot see the manager's, says
  nothing about a difference. If a Cursor database is not found by the collector while it is by
  a backfill from your shell, export the same values in the shell as the
  manager has and run setup again.
- **systemd older than 240.** Setup refuses, saying "this is systemd 237,
  older than 240, which the collector's logs need" (they use
  `StandardOutput=append:`); `status` shows the collector as *state unknown*.
  Upgrade systemd; on RHEL 8 or a rebuild of it, update to 8.3 or later.
- **A masked unit or a drop-in.** Setup and `uninstall` report a masked timer
  or service with the command that unmasks it (`systemctl --user unmask
  ...`); `status` shows the collector as *state unknown*. A drop-in that
  overrides the unit is a note under Notes: its settings differ from the unit
  file's, which is what setup and `setup --refresh` read.
- **"This data directory was set up on a different machine."** `status` and
  `setup` say it when the data directory's recorded host ID is not this
  machine's, which means it was copied (a cloned VM or container image).
  What to do about it is in [multiple
  machines](multiple-machines.md#cloned-machines-on-linux).
- **"Data directory ... is on a network filesystem"** (or the systemd unit
  directory). Setup and `setup --refresh` refuse, changing nothing, when the
  data directory or `~/.config/systemd/user` is on NFS, SMB/CIFS or another
  network filesystem, because machines that share a home would share one
  machine ID, cannot rely on file locks and would each run the collector.
  Set `AGENT_ARCHIVE_HOME` to a local path, or, if only one machine ever
  mounts this home, run `agent-archive setup --allow-network-home`. `status`
  warns about such a home either way. See [a home directory shared across
  machines](multiple-machines.md#a-home-directory-shared-across-machines).
- **Uninstalling without a user bus.** `agent-archive uninstall` refuses and
  prints the command that stops the job; `agent-archive uninstall
  --skip-scheduler` removes the units, hooks and skills anyway and prints
  that command to run from a session that has the bus ([uninstall](../getting-started/uninstall.md#when-the-background-scheduler-cannot-be-reached)).
- **Cursor captures nothing.** Cursor on Linux is unverified. A report on
  Cursor's forum says `cursor-agent` hooks on Linux may fail silently, so
  check `agent-archive status --verbose` for the hook state and diagnostics,
  that `~/.cursor/hooks.json` has the archive hooks, and start a new Agent
  chat. Setup cannot detect Cursor's version on Linux. Please report what
  you see, with `agent-archive --version` and the Cursor version.

## An interrupted setup

A failed setup restores the previous configuration, hooks, and scheduler. If
setup is interrupted before it can (a crash, a closed terminal), it leaves a
record in `setup-transaction.json` in the data directory; `status` says
recovery is needed, hooks record nothing, and `sync`, `pause`, `uninstall`,
and `backfill` refuse to start.

- Run `agent-archive setup` to recover: it restores the previous files and
  continues.
- Recovery never overwrites a file edited since the interrupted setup (an app
  may rewrite its own settings file). Setup then names the file and the
  record. To keep every file as it is now and discard the record, run
  `agent-archive setup --abandon-recovery`, then `agent-archive setup` to
  review your settings and reinstall anything missing.

## When setup's storage check fails

Setup checks the bucket before it saves anything: it writes a small test
file, reads it back, lists it, and deletes it. When that fails, setup says
why in a few lines, with the fix:

```text
Checking your storage connection…

  ✗ Can't sign in to AWS.
    No credentials were found for the storage profile.

    Fix: Add them with aws configure --profile work, or sign in with aws sso login --profile work for an SSO profile, then try again.
    Details: agent-archive setup --verbose

What next?
  1) Pick another profile
  2) Retry the check
  3) Change other settings
  4) Stop for now (your answers are kept)
```

The causes it tells apart are missing or expired credentials, access
refused, a bucket that doesn't exist, an S3 bucket in another region, and
no connection to the provider. The first choice, the default, goes to the
answer the cause points at (for a region, just the region). Stopping keeps
your answers: the next `agent-archive setup` offers to continue, and
continuing asks the storage questions again, with your answers as defaults,
rather than repeating the check. After a region failure it asks for the
region again unless S3 names a different one, and after R2 refused the access
key, it asks for the key.

The provider's own error is not shown, since it runs to several hundred
characters of SDK text. Run `agent-archive setup --verbose` (or add
`--verbose` to `setup --yes`) to see it under the diagnosis.

## Changing storage

A storage change is blocked while known work is pending; sync the current
destination first. A session that never received a transcript (a Cursor chat
with transcripts turned off) has nothing to publish, so it does not block the
change. Switching starts a new capture boundary: old sessions stay published
at their old destination, which this machine stops collecting into or cleaning
up. Their local evidence is kept until it ages past the retention period,
then removed from this machine only; nothing is deleted from either bucket on its
behalf.

## Upgrading from an earlier build

These notes are for builds from before the first release; a new install
can skip them.

- With S3 storage, run `agent-archive setup` again and save your settings
  unchanged; saving rewrites the collector's LaunchAgent. An
  earlier build's LaunchAgent gives the background collector none of your
  shell's AWS settings files, CA bundle, endpoints, proxy or `PATH`, so a
  profile outside `~/.aws` or one using `credential_process` (`aws-vault`,
  `op`, `granted`) uploads only when you run `sync`. `status` reports the
  cases it can see: a `credential_process` helper launchd's `PATH` doesn't
  find, or your shell's AWS files differing from the collector's.

- A source bundle written by an early build as a single JSON document
  (schema 1) is not read: `show --transcript` and `handoff` report
  "unsupported source schema version 1" for it.
- Setup retires the old `com.agent-skills.skill-runs-upload` job of the
  prototype this tool grew out of, only when its label and command match
  that prototype, and keeps the prototype's private records.
- With `AGENT_ARCHIVE_HOME` set to a non-default directory, hooks carry it in
  their command and the collector gets its own launchd label. Until you rerun
  `agent-archive setup`, `status` reports those hooks as `missing or
  incomplete`.
- Backfill keeps a local record of every session retention or undo removes,
  so a later backfill doesn't import it again. Sessions retention removed
  before that record existed can be imported once more if their transcripts
  are still on disk; check the plan, or use `--since`.
- Read-back evidence is keyed per session to the app, project root, and
  project activation time. The first run after upgrading from a build without
  that invalidates every `verification.json` and `storage-health.json` once:
  `status` reports authentication as `stale_configuration` and every
  publication as read-back pending until the next pass re-verifies it (capped
  per pass, so a large archive recovers over several passes; nothing is
  re-uploaded).
- A project that is removed and later re-added gets a fresh activation time.
  Sessions registered before that time are no longer accepted for that
  project; their earlier publications stay in the bucket until removed by
  hand.

## A slow list, show, or handoff

Run the command with `AGENT_ARCHIVE_TRACE=1` to see where its time goes:

```sh
AGENT_ARCHIVE_TRACE=1 agent-archive handoff
```

When the command finishes, it prints a timing tree on stderr: after you pick
or quit, for an interactive one, and just before the agent starts when
`handoff` launches one in this terminal. Interrupting with Ctrl-C prints
nothing. Each line gives a step's start (as an offset from the command's
start), how long it took, and counts such as keys listed, sidecars read from
the local cache or downloaded, and bytes per request. Repeated steps, such
as listing ranges or requests, fold into one line with how many there were,
the time they spanned from the first's start to the last's end (they often
run at once, so this is not their sum), and the longest. The `spinner (until
first draw)` line is how long the command was loading before it showed
anything, which for `handoff` is the wait before the picker appears. The
tree holds only step names, times and counts, never a session ID, title, key
or path, so it is safe to paste into an issue.

Most of a slow listing is usually `list objects`: listing a bucket costs
about 0.5–0.9 s per 1,000 objects on R2. A large archive is split into
ranges listed in parallel once the local metadata cache knows it, so the
first listing after clearing the cache (or with `--no-cache`) is the
slowest.

## Something else

Reporting a bug: use the issue templates. A capture gap (a session or field
that should have been archived and wasn't) has a template of its own.
Security problems go through [SECURITY.md](../../SECURITY.md), not public
issues. See also the [FAQ](faq.md).
