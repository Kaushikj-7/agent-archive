# Set up capture

Before enabling capture, read [what leaves your machine](../security/privacy.md#what-is-uploaded).
Credential redaction is best effort, and there is no client-side encryption.
Fresh setup stores skill names and hashes by default; choosing skill bodies can
upload filtered user-level instructions even when you include only one project.

Included-project scope captures new sessions in explicitly included projects.
Codex also offers an explicit all-current-and-future-projects scope, subject to
exclusions. Claude Code and Cursor remain project scoped. Review
[Codex source and scope consent](#automatic-codex-discovery) before choosing it.
When it finishes, setup imports the sessions of the last 7 days in the
projects and apps you chose ([after setup](#after-setup)).
[Backfill](../guides/backfill.md) imports older sessions only when you choose it.

```sh
agent-archive setup
```

Setup has three steps: choose apps and projects, connect storage, then review
and start. You need a private Cloudflare R2 or Amazon S3 bucket: an existing
one ([create one](bucket.md); [bucket permissions](../security/bucket-permissions.md)
has the least access it needs), or, for S3, one setup creates for you in
your own AWS account. Run it from inside a project you want
archived, so setup can offer it, or from anywhere: setup then offers the
projects your apps have sessions in. Choose "Show setup instructions" at the
storage prompt for a link to the [bucket guide](bucket.md). Setup asks questions, so it needs a terminal: without one it
stops before asking anything and changes nothing. Its prompt sequence is not
a scripting API; to script it, use [`setup --yes`](#set-up-without-questions).

### Interrupted storage creation

If setup stops after requesting a bucket, rerun `agent-archive setup`. Private
resource receipts survive draft changes and record whether creation was confirmed
or its outcome remains unknown. Setup offers to connect the bucket as existing
storage, choose other storage, or inspect resource details. Connecting still asks
for credentials and verifies access; it does not establish ownership.

An unknown creation is never retried for the same bucket automatically. Inspect
the named account and bucket in the provider console. S3 receipts record the
creation profile, which can change accounts; a profile name is not ownership proof.
Bucket cleanup instructions remain pending because setup cannot verify ownership
or manual deletion. Check bucket contents and other machines before deleting it.

Unused guided R2 keys can be explicitly revoked after confirmation, using a fresh
management token. Setup verifies exact provider key lineage and permissions before
deleting, protects active and saved-draft credentials, and retains unsuccessful
cleanup for retry. Bootstrap and management tokens are not stored in receipts.

Setup groups each question, explanation, choices and shortcuts together. On a
supported terminal, the answered question becomes a short receipt. Yes/No receipts
retain the question subject so completed decisions remain identifiable.
`NO_COLOR` removes color while keeping the same structure and interaction;
dumb terminals and redirected streams retain the expanded question. R2 secret
fields hide input and show a fixed credential receipt.

## Before the first question

Setup first checks what starting to archive will need, and prints one line
for each check: a ✓, or a ✗ with the problem and how to fix it.

- **Hook files.** The file of each app setup found, or that your saved
  settings or an unfinished setup include, must be one setup can add its
  hooks to: plain JSON, without comments, trailing commas, or duplicate
  keys. A problem names the file, line, and column, such as
  `~/.claude/settings.json:2:3`. A file that doesn't exist yet is fine;
  setup creates it.
- **Background job.** The scheduler must say whether the collector's job is
  already loaded: `launchctl` on macOS, and on Linux `systemctl --user`, which
  needs a systemd user manager you can reach and systemd 240 or newer
  ([setup on Linux](#setup-on-linux)).
- **Keychain** (macOS) **or credentials file** (Linux). When your saved
  settings or an unfinished setup store in R2, the place where the R2 key is
  kept must open: the macOS Keychain, or on Linux the private credentials file
  folder ([where credentials are kept](../security/privacy.md#where-credentials-are-kept)).

A ✗ stops setup before it asks anything: nothing is changed, and an
unfinished setup is kept. Fix what is marked, then run `agent-archive setup`
again. To leave out an app whose file you don't want to change, choose the
apps with [`setup --yes --apps`](#set-up-without-questions). `setup --yes`
makes the same checks, for the apps it would include, and checks the
credential store (the Keychain, or on Linux the credentials folder) when it
stores in R2.

Setup captures **new** sessions within the scope you approve: included projects by default, or all non-excluded projects for Codex when explicitly chosen. It also
imports the last 7 days of sessions in the included projects
([after setup](#after-setup)). To import older conversations already on this
machine, run [`agent-archive backfill`](../guides/backfill.md) afterwards.

## 1. Apps and projects

Setup first offers detected apps together, then asks **Which projects?** in
one shared selector. Inside a repository, the current folder heads the list and
is labeled `this folder`; it still uses the same All versus Specific decision.
A home folder, a folder containing it, or a temporary root is too broad to offer
automatically. A repository inside a temporary folder remains an ordinary project.

**All N found projects** is choice 1 and the visible default. Enter includes every
validated candidate, including projects on later pages. Candidates combine the
current repository, saved project rules and eligible history from your chosen
apps. All applies to that found set; it does not approve future directories.
Codex's all-current-and-future-projects setting remains a separate explicit choice.

**Choose specific projects** starts with all candidates checked in fresh setup,
and your actual choices checked when editing an installed or saved selection.
Enter numbers or ranges to toggle each project once, `a` to select all pages,
`p` to add a path, then Enter to confirm. Lists show twelve projects per page,
with stable numbers and Next/Previous actions. Adding a path preserves the page,
selection and existing activation times. An unavailable selected directory must
be repaired or explicitly left out before confirmation.

Discovery is bounded. When it cannot finish, setup reports **search incomplete**,
explains why, and offers Retry or Add a path. Partial session counts are labeled
as observed lower bounds. Rendering and paging reuse the result. With no valid
candidates, setup opens Add a path rather than offering All 0 projects.

Apps and projects, All settings, and Projects in the review editor use this same
selector. Continuing a draft whose project step is complete keeps its choices;
storage-only and retention-only edits leave projects alone. Selecting All in an
edit can re-enable exclusions in the candidate set; setup reports the expanded
scope before Save. Specific retains explicit excluded roots and imported-project
exclusions, with the nearest explicit project rule still taking precedence.

In included-project mode, include each project explicitly; nothing outside
an included project is captured, and setup asks again if none are included.
Codex-only all-projects mode can have zero explicitly included projects and
covers current and future projects except configured exclusions. Review that
choice separately from whether automatic discovery is enabled; see
[automatic Codex discovery](#automatic-codex-discovery).

## 2. Storage

Within Step 2, **Account → Storage → Verification** shows the current work.
Account covers provider access; Storage covers the destination and archive
credentials; Verification checks connectivity and reports bucket privacy
separately. Back and Retry return to the stage being edited, preserving valid
existing-bucket answers within the run.

Setup suggests S3 when your shell sets `AWS_PROFILE` or your AWS settings
already have a profile with credentials, and R2 otherwise.

The menu has two numbered choices: **Cloudflare R2** and **Amazon S3**.
After choosing a provider, press Enter for **Create a new bucket**, or
choose **Use an existing bucket** before supplying creation credentials.

For R2 creation, supply a temporary setup token. Setup checks archive-key
permission access before showing a suggested name and automatic location.
For S3, select a creation profile; its region supplies the default, or setup
asks for a region if it has none. Both flows show a summary before creating
anything. Press Enter to create, **Customize** to change the settings, or
**Back** to return. Existing-bucket access remains available on the summary
and during error recovery. See the [bucket guide](bucket.md) for credentials
and permissions.

S3 setup enables and reads back all four Block Public Access settings. It
offers **View archive permissions** for a narrower runtime policy, but does
not create IAM identities or keys.
The creation profile remains the default archive profile; choose **Choose
another archive profile** to switch after attaching the policy yourself.

To connect an existing R2 bucket, enter its account ID, bucket and S3 keys.
Pasting `https://<account-id>.r2.cloudflarestorage.com/<bucket>` fills in both
account and bucket. Other R2 endpoints, including EU jurisdiction endpoints,
work too. Secret input is hidden on terminals and saved in the macOS Keychain
or a private credentials file on Linux.

For existing S3 storage, select an AWS profile and a bucket. Setup lists
buckets when allowed, looks up the bucket's region, or asks for these values
when your profile cannot read them. Profile discovery reads AWS configuration
only; it does not run credential processes or sign in.

When editing an installed configuration, **Keep current storage** is the
default. It still verifies the connection. Changing the destination opens the
provider flow. Saved incomplete setup drafts resume their existing step and
credentials; they do not automatically create a replacement bucket.

For scripted interactive answers, `r2-existing` and `s3-existing` jump directly
to the existing-bucket form. `r2-create` (when enabled) and `s3-new` jump to
creation. Bare `r2` and `s3` now open the provider flow. Noninteractive
`setup --yes` flags retain their existing behavior.

Creating a bucket needs `s3:CreateBucket` and
`s3:PutBucketPublicAccessBlock`, which the [runtime
policy](../security/bucket-permissions.md) deliberately does not grant, so
use a profile that has them for this step. If the profile is refused,
setup says which permissions are missing (an organization policy can also
forbid creation) and offers retry, profile replacement, or an existing bucket. If S3 gives no
clear answer to the request, setup says the bucket may exist and to check
the S3 console; it deletes nothing in that case. If the bucket was created
but Block Public Access could not be turned on, setup does not use it: it
offers to try again, to delete the empty bucket (after you type its name),
or to stop. When it succeeds, setup offers **View archive permissions** for the new
bucket (not for a folder name with characters other than letters, digits
and `. _ - /`; it says so and points at the guide) and asks which AWS
profile archiving should use. The profile that created the bucket is the
default, and it is usually far broader than archiving needs: attach the
policy to a separate IAM identity, save it as its own profile, and choose
that profile here to archive with less. The usual storage check then
verifies whichever you chose. If you choose storage again later in the same
run, setup offers the bucket it already created instead of making another,
and if setup ends without using a bucket it created, it says so and that
the bucket is empty, so you can delete it (or, when your saved setup draft
still names it, that running setup again resumes with it). Guided creation is interactive
only; `setup --yes` still takes an existing bucket.

Setup checks the connection in two steps. (After guided **Cloudflare R2** creation, setup has already made this check on the new key before storing
it, so a key that doesn't work is revoked at once; the check then runs again on
the stored key.) First it lists at most one object
under `.setup-test/`, which writes nothing, so a wrong account ID, key, or
profile fails within moments with an explanation. Then it writes one
temporary synthetic object (`.setup-test/<random>.json`), reads it back, and
deletes it. That proves the credentials work; it does not prove the bucket is private. Setup then
inspects the bucket's public-access settings read-only (S3 only; see
[privacy](../security/privacy.md#bucket-privacy-evidence)). R2 keys cannot
read those settings. For an existing R2 bucket, the review reminds you to
check public access in the Cloudflare dashboard. For a bucket setup just
created, it shows what the temporary Cloudflare token checked at setup.

Never pass secrets as command arguments; secret input fails rather than
falling back to visible keystrokes.

## 3. Review and start

The compact review shows apps, selected project paths or count, Codex scope,
full storage destination including its prefix, retention, and skill evidence
scope. Nondefault capture restrictions, exclusions and meaningful changes stay
visible, with old/new values for a reconfiguration. Storage is shown once.

```text
Step 3 of 3 · Review and start

  Apps       Codex, Claude Code, Cursor
  Projects   3 included projects (paths in Details)
  Codex      Included projects only
  Skills     Metadata, including user folders
  Storage    s3://team-archive/agent-archive/
  Keep for   90 days

  ✓ Storage connected
  ✓ Bucket is private
  ! Setup imports the last 7 days of included projects; older history is separate.
  ! Sensitive text may remain after filtering.
```

Choose **Full settings and privacy** (`d`) for app versions, source roots, exact
project paths and exceptions, discovery/copy behavior, hook status, credential
ownership, storage checks and privacy documentation. Longer details use the
pager and return to the same draft and decision. **Name this machine** (`m`) and
**Cancel; keep draft** (`q`) are separate shortcuts. Narrow displays wrap values
and warnings without dropping them. The default retention is 90 days; older
sessions are deleted from the bucket automatically.

The Skills row controls filesystem skill evidence. Fresh setup uses
`metadata` (names and filtered hashes, no body). Choose Edit a setting to
select `none` or `body`. User-level skill folders may be scanned outside
your included projects. If an older configuration did not record a mode,
setup displays `body (kept from previous setup)` and lets you change it.
Changing the mode affects future uploads; it does not remove earlier local
or bucket copies.

In the checklist, ✓ is fine, ! needs you, and ✗ needs fixing first. While
any row is ✗, setup does not offer to start: fix what it names, then choose
"Check again", which checks storage and the hook files again.


- **Storage connected**: the storage check wrote, read, listed and deleted a
  test file.
- **Bucket is private**: for S3, all bucket-level Block Public Access settings
  were observed on. For a newly created R2 bucket, `r2.dev` was off and no
  custom domains were enabled when setup checked. An existing R2 bucket, or
  one whose settings could not be read, is marked !. A public bucket is marked
  ✗ unless you chose **Continue anyway** during guided R2 creation; that
  choice remains visible as a warning in the review.
- **Hook files are valid**: setup can edit each app's hook file.
- **Codex discovery**: supported new tasks do not require hook approval when
  enabled. Hook-only setups instead show **Codex needs one step**: run `/hooks`
  in Codex and approve the archive hooks.

Any warnings about the change follow, such as a shorter retention period or
hooks moving to another file.

At "Start archiving?", enter the number for "Edit a setting" to adjust apps, projects, session scope, retention, storage,
the folder inside the bucket, or the AWS region. The folder inside the
bucket (the prefix) is `agent-archive/` unless you change it. Retention is
a whole number of days from 1 to 36,500, or `forever` to disable automatic whole-session expiry. Provider lifecycle rules and explicit deletion still apply. Storage changes are checked
again before starting. If the connection test fails, setup says why and
how to fix it; see [when setup's storage check
fails](../guides/troubleshooting.md#when-setups-storage-check-fails).

Run setup from the `agent-archive` you will keep using. Setup refuses a
binary in a temporary folder, including the one `go run` builds and deletes
when it exits, since every hook and the collector would run a path that is
gone; build one with `go build` or use the installer.

Setup saves non-secret choices after each completed step. If it is
interrupted, run it again to continue or start over. After a plain
`agent-archive uninstall`, setup sets up again with your saved answers as
the defaults. Reconfiguration lets you
edit capture, storage, or retention separately (its headings then drop
"Step n of 3"), or choose "Nothing, exit" to leave without changing
anything, and keeps the machine identity, existing project activation
times, paused state, and unrelated hooks.

## Set up without questions

For a second machine, or any scripted setup, pass the answers as flags with
`--yes`. Setup then asks nothing, runs the same
[checks](#before-the-first-question) and storage check, and saves;
if an answer is missing or the check fails, it says so and changes nothing.
A bucket that allows public access stops it the same way, marked ✗ with a
link on fixing it, as it blocks the review of interactive setup; a bucket
whose public-access settings could not be read is only marked !.
Every missing or wrong answer is listed at once, one per line with the flag
that fixes it, before any check runs.

```sh
export AGENT_ARCHIVE_R2_SECRET_ACCESS_KEY=...   # or pipe it on standard input
agent-archive setup --yes --provider r2 --r2-account ACCOUNT_ID --bucket BUCKET \
  --r2-access-key-id KEY_ID --project ~/code/app --project ~/code/api \
  --apps codex,claude --codex-discovery on --codex-capture-scope included-projects

agent-archive setup --yes --provider s3 --bucket BUCKET --aws-profile PROFILE \
  --project ~/code/app
```

- The R2 secret access key comes from `AGENT_ARCHIVE_R2_SECRET_ACCESS_KEY`,
  or else from standard input. It is never a flag. The access key ID can
  also come from `AGENT_ARCHIVE_R2_ACCESS_KEY_ID`. Setup removes both
  variables once read, so no command it runs inherits them. Without a new
  key, the one already stored is kept.
- If saving fails after the storage check, a new R2 key stays with the
  unfinished setup; run `agent-archive setup` to finish or discard it.
- `--r2-account` also takes the bucket URL, which names the bucket too.
- For S3, `--region` defaults to the profile's region, and must look like a
  region, such as `us-east-1`.
- `--apps` defaults to the apps already set up, else those found on this machine.
  It can add apps but never removes one: it must name every app already set
  up, and to remove an app (and its hooks) you run `agent-archive setup`.
- `--project` adds to the projects already set up; repeat it for several.
  `--project-repo REPO_KEY` matches a unique local clone by its origin's hash,
  independently of `--project`; ambiguous, excluded, or incomplete matches
  are skipped. See [repository matching](../guides/multiple-machines.md#repository-matching-in-setup-commands)
  for discovery limits.
- `--prefix PREFIX` sets the folder inside the bucket, including when changing
  only that setting on an existing installation.
- `--retention-days DAYS` sets retention from 1 to 36500 days.
- `--keep-forever` disables automatic whole-session expiry and conflicts with `--retention-days`. The default remains 90 days. Guided retention offers **90 days**, **Custom duration**, and **Keep forever**, starting from the saved preference. Custom duration also accepts `forever`. This preference applies to existing and future sessions owned by this installation; provider lifecycle rules and explicit deletion still apply.
- `--require-skill-use` captures only sessions that use skills;
  `--no-require-skill-use` captures sessions with or without skills. Omitted
  settings keep their saved values (fresh setup captures both).
- `--skill-evidence none|metadata|body` sets the skill evidence mode. A fresh
  setup defaults to `metadata`; an older configuration without the field
  retains `body` until changed.
- `--no-skills` installs no [agent skills](#what-setup-changes-on-your-machine)
  (such as `/handoff`) and removes the ones setup wrote earlier; a file that
  is not setup's is left alone and named. It works with or without `--yes`
  (without it, setup on a finished installation first asks what to change,
  so `agent-archive setup --yes --no-skills` changes only this). It is saved
  (`no_skills` in the [configuration](../reference/configuration.md)), and
  later setup runs keep the skills off. `--skills` turns them back on and
  installs them. Giving both is an error.
- Without storage flags, the storage already set up is kept, so
  `agent-archive setup --yes --project DIR` just adds a project.

[CLI reference](../reference/cli.md#agent-archive-setup) lists every flag.

## Refreshing after an upgrade

The hooks, the background job's definition (the LaunchAgent's plist, or the
systemd unit files), and the skill files name the
`agent-archive` binary and its flags, so a new binary needs them rewritten.
`agent-archive setup --refresh` does that, and nothing else: it asks
nothing, needs no terminal, and never touches storage, credentials,
projects, retention, or your saved answers. It prints `nothing to refresh`
(exit 0) when all is current, or one line saying what it refreshed
(`--verbose` lists the files). [The installer](install.md#install-with-the-script)
runs it for you when it finds a set-up machine.

- It uses the saved settings and the hook files setup recorded, and the
  executable you run it from. If the recorded executable differs (it moved,
  or was deleted, the case `status` calls "capture has stopped"), it points
  the hooks, the job's definition, and the skills at the running one and
  records it.
  It refuses a temporary build (`go run`) or a file that cannot run.
- Skills follow the same rules as in setup: a stale file of setup's is
  replaced, a file that is not setup's is left and named, and with
  `--no-skills` saved none are installed. It writes what setup would, so a
  skill or hook file you deleted by hand is written again (to keep the
  skills off, use `--no-skills`; to stop capture, use `agent-archive
  uninstall` or `pause`, not deleting the hooks).
- It changes the background job only when its definition itself changes (a
  moved binary) for a job that is loaded: that job is stopped and started
  again so it runs the new definition. A job that is not loaded stays that
  way, and an unchanged definition leaves the scheduler alone.
- All of it is one transaction with setup's journal, so a failure puts every
  file back, and a Ctrl-C or closed terminal while it writes does not stop
  it halfway. It waits up to ten seconds for a background collection pass
  that is running, then refuses and asks you to retry.
- It refuses, changing nothing and exiting 1 with one line on standard
  error, when setup never finished, an interrupted setup needs recovery, the
  archive was uninstalled, another installation's hooks are in a hook file
  it would write, another installation owns the background job, another
  setup is running, or it runs as root (`sudo`) in a home directory that
  belongs to another user, where it would leave root-owned files. Run it as
  yourself. Any other flag except `--verbose` is a usage error
  (exit 2).

## Setup on Linux

Setup is the same on Linux, with these differences ([what was tested, and
what was not](install.md#platforms)).

- **The background job is a systemd user timer.** Setup writes
  `agent-archive-collector.service` (a one-shot that runs `agent-archive
  _collect`, with its output appended to `collector.log` and
  `collector-error.log` in the data directory) and
  `agent-archive-collector.timer` (a minute after boot, so at once when setup
  starts it, then a minute after each start; in a container the first run can
  wait up to a minute) into `~/.config/systemd/user`, then runs
  `systemctl --user daemon-reload` and `enable --now` on the timer. It needs
  systemd 240 or newer (RHEL 8 and its rebuilds from 8.3); an older one is
  refused. There is no cron or other fallback: without a systemd user manager
  setup stops.
- **No user bus.** Over SSH without `pam_systemd`, in a container, or after
  `su`, `systemctl --user` cannot reach a user manager. Setup then stops at
  its first check, before it asks anything or changes anything:

  ```text
  Checking installed applications...
    ✓ Claude Code hooks: ~/.claude/settings.json
    ✗ Background job: the systemd user manager cannot be reached (this session has no user bus), so setup cannot tell whether the agent-archive-collector job is loaded, and loads it only when it can tell
      Run this from a login session that has a systemd user bus (a desktop or console login, or ssh with pam_systemd), or run `loginctl enable-linger` once so the user manager runs without a login, then check that `systemctl --user status` works, then run agent-archive setup again.
  Setup incomplete. Nothing was changed, and any unfinished setup is kept. Fix what is marked ✗ above, then run agent-archive setup again.
  ```

  After `loginctl enable-linger` the manager starts at boot, but a shell that
  was already open may still lack the bus: log in again, or `export
  XDG_RUNTIME_DIR=/run/user/$(id -u)`, and `systemctl --user status` should
  answer. Then run `agent-archive setup` again.
- **Lingering.** Without it, the user manager exists only while you are logged
  in, so the collector stops when you log out. Hooks keep recording sessions
  and the next pass after you log in uploads them, but nothing uploads while
  you are away. `status` and `status --json` (`background_warnings`) say so
  while the job is otherwise working; `loginctl enable-linger` fixes it. A
  server or a machine you reach over SSH wants lingering on.
- **Where the units go.** Always `~/.config/systemd/user`, whatever
  `XDG_CONFIG_HOME` your shell has. The user manager reads
  `$XDG_CONFIG_HOME/systemd/user` instead only when its own environment sets
  the variable (as `pam_env` or a `user@.service` drop-in can); a variable
  exported in your shell, the usual place, never reaches it. If the manager
  does look elsewhere, `enable --now` fails with "Unit file ... does not
  exist", setup puts everything back and the message names the directory the
  units were written to; `systemctl --user show --property=UnitPath` lists
  where the manager looks. Setup has no option to write the units elsewhere:
  unset the variable for the manager (look in `/etc/environment`, `pam_env`
  and any `user@.service` drop-in), log in again, and run setup again.
- **`XDG_CONFIG_HOME` and `XDG_CACHE_HOME` are forwarded to the job** when
  your shell sets them to an absolute path, so backfill from your shell and
  the scheduled collector find the same Cursor database and keep Cursor
  database copies in the same place (`~/.cache/agent-archive/cursor-snapshots`
  by default, never `/tmp`). A relative or empty value counts as unset.
  `status` warns when the shell's value later differs from the job's.
  **A limit to know about:** a systemd user manager can get these two
  variables from outside your shell (an `environment.d` file, a desktop
  session's `import-environment`). Setup records only what the shell has, so
  if the shell had neither variable when setup ran, the job uses whatever
  the manager has. `status` compares only your shell's values with the ones
  recorded and cannot see the manager's, so it cannot warn that the job and a
  backfill from your shell look in different places. The workaround is to export the same values in
  the shell as the manager has, and run `agent-archive setup` again, which
  records them.
- **The job's `PATH`** is your shell's usable `PATH` entries when setup ran,
  then systemd's own default (`/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin`),
  so a `credential_process` helper such as `aws-vault`, `op` or one in
  `~/.local/bin` is found. A directory that does not exist when setup runs
  is not recorded; run setup again after installing a helper there.
- **The job's working directory is `/`.** Use an absolute path or `~/` for
  a `credential_process` helper under your home. A path such as
  `./bin/helper` is resolved from `/`, as it is for the macOS collector.
  Existing installations get this setting when you run setup again.
- **Credentials.** There is no Keychain. An R2 key is kept in a private file
  that is not encrypted, and an S3 profile stores no secret of
  agent-archive's own; prefer S3 on Linux. In a container or a service, set
  `AGENT_ARCHIVE_R2_ACCESS_KEY_ID` and `AGENT_ARCHIVE_R2_SECRET_ACCESS_KEY`
  instead. See [where credentials are kept](../security/privacy.md#where-credentials-are-kept).
- **Cursor** is best effort, and setup cannot detect its version (it says
  "version not detected"); see [what is not verified](install.md#platforms).
- **Moving or copying.** A data directory copied to another machine, or a
  cloned VM or container image, makes `status` and `setup` warn that it was
  set up on a different machine; see [cloned machines on
  Linux](../guides/multiple-machines.md#cloned-machines-on-linux).
- **A network home.** If the data directory or `~/.config/systemd/user` is on
  a network filesystem (NFS, SMB/CIFS, sshfs and the like), setup refuses
  before its first question and changes nothing: machines sharing a home would
  share one machine ID, cannot rely on file locks and would each run the
  collector. Put the data directory on local disk with `AGENT_ARCHIVE_HOME`,
  or, for a home only one machine mounts, run `agent-archive setup
  --allow-network-home`, which is recorded in `config.json` and does not stop
  `status` warning. See [a home directory shared across
  machines](../guides/multiple-machines.md#a-home-directory-shared-across-machines).
- **Uninstalling without a user bus.** `agent-archive uninstall` refuses
  when the manager cannot say whether the job is loaded;
  `uninstall --skip-scheduler` removes the units, the hooks and the skill
  files anyway and prints the `systemctl --user stop` command to run from a
  session that has the bus ([uninstall](uninstall.md#when-the-background-scheduler-cannot-be-reached)).

## What setup changes on your machine

- **Hooks**, where each app reads them: Claude Code's
  `$CLAUDE_CONFIG_DIR/settings.json` and Codex's `$CODEX_HOME/hooks.json`
  when those variables are set in the shell you run setup from, otherwise
  `~/.claude/settings.json` and `~/.codex/hooks.json`; Cursor's is always
  `~/.cursor/hooks.json`. The paths are recorded (`hook_files` in the
  [configuration](../reference/configuration.md)), so `status` and
  `uninstall` find them from any shell. Only the `hooks` entry of each file
  is rewritten (and Cursor's `version`, when missing): every other setting
  keeps its exact text, key order, and numbers, and uninstall restores a file
  setup only added hooks to byte for byte. A file that removing the hooks
  leaves empty (`{}`, or Cursor's `version` alone), as when setup created
  it, is deleted. A file that is a symlink, as dotfile managers such
  as stow or chezmoi create, is updated at its target and the link is kept.
  Setup refuses a file it cannot edit safely and says where the problem is:
  anything that is not plain JSON (a comment, a trailing comma, a byte-order
  mark), or a key that appears twice inside `hooks`.
- **A background job** that runs the collector every 60 seconds: on macOS a
  LaunchAgent, `~/Library/LaunchAgents/com.agent-archive.collector.plist`; on
  Linux a systemd user timer and service,
  `~/.config/systemd/user/agent-archive-collector.timer` and `.service`, each
  mode 0600, plus the link `systemctl --user enable` makes in
  `timers.target.wants` ([setup on Linux](#setup-on-linux)). The scheduler
  gives it none of your shell's environment, so for S3 its definition (the
  plist, or the service's `Environment=` lines) also carries what setup's
  storage check ran with: `AWS_CONFIG_FILE`, `AWS_SHARED_CREDENTIALS_FILE`,
  `AWS_CA_BUNDLE`, the `AWS_ENDPOINT_URL` overrides, the proxy
  variables (a proxy URL with a password is left out), and aws-vault's and
  1Password's settings for where credentials live, when set, and your
  shell's `PATH` (without directories every account can write to), which a profile's `credential_process`
  (`aws-vault`, `op`, `granted`) is found through. Never your shell's AWS
  keys or tokens. If that command is not a program on your `PATH` (an alias
  or shell function), the review warns that background uploads will fail.
  Run setup again after moving your AWS files or the helper; `status` warns
  when they no longer match
  ([configuration](../reference/configuration.md#environment-variables)).
- **Agent skill files**, one folder per skill under each app's skills
  directory: `/handoff` is written to `$CLAUDE_CONFIG_DIR/skills/handoff/SKILL.md`
  (`~/.claude/skills/…` by default) for Claude Code and to
  `~/.agents/skills/handoff/SKILL.md` for Codex and Cursor
  ([handoff](../guides/handoff.md#from-inside-an-agent-handoff)), and
  `agent-archive`, which lets the agent find and pull in a past session, to
  `skills/agent-archive/SKILL.md` in the same two places
  ([agent skills](../guides/agent-skills.md)). Each file
  carries a marker line: setup replaces, `status` lists, and uninstall
  removes only a file with it (naming this installation's data directory),
  and leaves any other file at that path alone, saying so. After you upgrade
  `agent-archive`, `status` warns about a skill file written by an earlier
  release; [`agent-archive setup --refresh`](#refreshing-after-an-upgrade)
  refreshes it (the installer runs that). Setup installs them without
  asking and says how to opt out: `agent-archive setup --no-skills` removes
  the files it wrote and keeps them off in later runs (`status` says they are
  turned off), and `agent-archive setup --skills` turns them on again.
- **Local state** in `~/.local/share/agent-archive` (or `AGENT_ARCHIVE_HOME`;
  see [local state](../reference/local-state.md)), private to your account.
  It holds registrations, frozen uploads, and caches; transcripts are read in
  place, not copied, except a Cursor database copy that exists only while a
  read of it is in progress.
- **A Keychain item** (service `agent-archive`) for R2 credentials on macOS;
  on Linux, a credentials file (mode 0600, in a mode 0700 folder) in the data
  directory. S3 credentials stay in your AWS profile.

`agent-archive uninstall` removes the hooks, the skill files, and the
background job (the LaunchAgent, or the systemd units and their enable link);
`--delete-local-data` also removes the local state and the Keychain item
(or credentials file).
Neither touches the bucket ([uninstall](uninstall.md)).

Only the account's own default installation, in `~/.local/share/agent-archive`
under the home directory the system records for your account, uses the
job name `com.agent-archive.collector` (launchd) or `agent-archive-collector`
(systemd). Any other data directory, whether set
with `AGENT_ARCHIVE_HOME` or moved by a sandbox that overrides `HOME`, gets a
name of its own (`com.agent-archive.collector.<hash>`, or
`agent-archive-collector-<hash>` under systemd), and its hooks carry
the directory in their command, since apps run hooks without your shell's
environment. That directory is also how each installation recognizes its own
hooks: setup replaces and uninstall removes only handlers whose command runs
with this installation's data directory, and leaves another installation's
alone. If an app's hook file already holds another installation's hooks,
setup names that installation before its first question and installs
nothing beside it (two installations would each capture every session): if
you choose that app, setup stops right after the apps and projects step,
before asking about storage, and keeps your answers. `status` reports it too;
uninstall that installation first, or give the new one its own `HOME` (or
`CLAUDE_CONFIG_DIR` and `CODEX_HOME`) so the apps' hook files are separate
too. Before stopping a job, setup and uninstall check that the scheduler loaded
it from this installation's own definition (plist or unit file); a job loaded
from any other one is left running and reported. These keep a second or test
installation from stopping or replacing the default one. They do not stop it
from loading its own job into your real launchd or systemd user manager: stub
`launchctl` or `systemctl` in tests (see
[testing](../../dev/contributing/testing.md)).

## After setup

After "Setup complete", setup says, with one line per app, what it needs and
which sessions it captures from now on. A session already open in an
included project, started in the last 7 days, is not lost: setup imports it
([below](#sessions-from-the-last-7-days)), and the collector keeps it
current.

- **Codex:** automatic discovery captures supported tasks that start within the
  reviewed Codex scope: an included project in included-project mode, or any
  non-excluded project in all-projects mode. Interactive setup reviews
  automatic discovery and Codex scope; fresh scripts specify both choices
  explicitly. Hook capture remains available with `/hooks` approval, and is
  required when discovery is disabled or the native producer is unsupported.
- **Claude Code:** nothing to approve; hooks capture sessions that start from
  now on (or after `/clear`).
- **Cursor:** nothing to approve; hooks capture Agent chats that start from now on.

Setup's closing lines also name the agent skills it installed (`/handoff` and
`agent-archive`) and the opt-out, `--no-skills`. Unlike Codex's hooks, a skill
needs no approval; Claude Code and Codex notice it in a session that is
already open (Claude Code needs `/reload-skills` when it had no
`~/.claude/skills` folder at start), and Cursor may need a new chat. Then ask an agent in words, such
as "pull in the auth session from Codex" ([agent skills](../guides/agent-skills.md)).
Claude Code asks before it first uses the skill and before the commands it
runs.

Hooks and discovery need a provable fresh start, so they admit only sessions
that start after setup in an included project. In Codex and Claude Code,
`/clear` also starts one; in Cursor, only a new chat does. Sessions that
started before setup are covered by its import instead, within its limits
below. Setup finishes
without waiting for either. Check progress with:

```sh
agent-archive status
agent-archive status --json
```

What each status line means is in
[troubleshooting](../guides/troubleshooting.md#reading-status).

If you set up Codex discovery before setup imported the last 7 days, or a
session in those days was not imported, `status` says so under Codex, once
discovery has scanned it:

```text
! 1 Codex session from the 7 days before setup was not captured.
  Import it with agent-archive backfill --since 2026-09-28.
```

The date is the local day 7 days before discovery was first enabled for Codex.
The count is top-level Codex sessions that started from that day until
discovery was enabled, that discovery refused for starting before it, and that
are not archived; the line goes away once they are imported. Claude Code and
Cursor have no such line: their hooks see no session from before they were
installed, unless one runs a hook later, which status lists as a session
skipped for starting before the project's activation.

### Sessions from the last 7 days

Sessions that started before setup, the one you may be running setup from
among them, are imported instead. Right after "Setup complete", without a
question, setup imports the sessions of the last 7 days (the same days as
`agent-archive backfill --since 7d`) in the included projects, for the apps
you chose:

```text
✓ Setup complete · Automatic capture is on
Imported 3 sessions from the last 7 days (Codex 2, Claude Code 1). Uploading in the background.
211 older sessions: run agent-archive backfill to import them.
```

The import registers the sessions and leaves the upload to the background
collector, as `agent-archive backfill --yes --background` does. It is an
ordinary import: `agent-archive backfill history` lists it and
`agent-archive backfill undo ID` removes its sessions again (see
[backfill](../guides/backfill.md)). It never adds a project: a session in a
folder you did not include, a temporary folder, or a worktree that no longer
exists is left to `backfill`, which asks first. The second line appears only
when the same projects have older sessions that are not in the archive.

Setup says nothing about importing when there is nothing to import, and skips
it while capture is paused. `setup --yes` imports the same way. If the import
fails, or a collector pass is still running after about 20 seconds, setup is
still complete and says so:

```text
Recent sessions were not imported: <reason>. Run agent-archive backfill --since 7d to retry.
```

Setup's storage check and installed hooks establish configuration, not a captured session. After `agent-archive sync` or the next background pass, check that the app's Capture row says **archived, verified**, then confirm the session appears in `agent-archive list` and `agent-archive show SESSION_ID`. An overall `Ready` state alone does not establish that this app published a new session and had it read back. For a short route through the check, see [first successful capture](../README.md#first-successful-capture).

Completion reflects the configured capture mode. Codex-only hook capture is
reported as configured; its app approval still requires `/hooks`. Paused capture
stays paused.

After setup, a short hint points to machine pairing. Choose **Machine transfer details**
under More next steps for a command that sets up another machine with the same
storage, capture rules, agent skill installation policy, apps and projects
([without questions](#set-up-without-questions)),
ready to copy:

```text
To set up another machine with this storage, run there:
  agent-archive setup --yes --provider s3 --bucket BUCKET --aws-profile PROFILE --region us-east-1 --apps codex,claude --codex-discovery on --codex-capture-scope included-projects --prefix agent-archive/ --retention-days 90 --no-require-skill-use --skill-evidence metadata --skills --project ~/code/app
```

Whole repositories with a known origin use `--project-repo` for bounded
[repository matching](../guides/multiple-machines.md#repository-matching-in-setup-commands).
Other project paths in your home folder are written from `~`; adjust those
paths for the new machine. For R2 the command never
carries the key: set `AGENT_ARCHIVE_R2_ACCESS_KEY_ID` and
`AGENT_ARCHIVE_R2_SECRET_ACCESS_KEY` on the other machine first. The command
carries the saved folder with `--prefix`, retention, skill-use capture rule,
effective skill evidence mode, and whether agent skills are installed.
Omitting these flags from a scripted reconfiguration keeps the saved settings.

For another machine, encrypted pairing can transfer settings through
`setup --pair` or `setup --pair-file PATH`. Read [Multiple machines](../guides/multiple-machines.md)
for separate bundle/code delivery, destination consent, scope review, and the
shared R2 key's revocation limit. Pairing refuses to run inside a coding agent.

When your capture scope contains exclusions, the second-machine command uses
`--project-scope-file -` with a quoted heredoc to transfer all inclusion and exclusion rules together.
Repository-relative exclusions and reincluded subtrees follow a relocated
checkout. Missing excluded folders stay excluded if created later; a failed
repository match or unsafe subtree mapping refuses the transfer before
changing capture settings. Saved destination exclusions stay in force; conflicting
saved reinclusions refuse the transfer until you review the destination scope.
Review non-home absolute paths for the new machine.

Scope flags require a nonempty value. Include all desired capture rules in the
scope; combining it with `--project` or `--project-repo` is refused.
Scope transfer accepts at most 4,096 input rules and refuses saved or resulting
destination scopes above that size. Saved unresolved symlinks must be reviewed
before transfer; missing excluded directories remain supported.

## Automatic Codex discovery

Interactive setup offers Codex capture scope, defaulting to **Included projects
only**. Choosing **All current and future projects (Codex only)** approves supported
new Codex tasks in unlisted repositories and directories without a list selection; setup explains the scope and opens the exceptions editor. Claude Code and Cursor still need explicitly included projects. Codex-only
all-projects setup can have zero explicitly included projects.

Scope and automatic discovery are separate choices. Fresh scripted Codex setup
must pass both `--codex-discovery on|off` and
`--codex-capture-scope included-projects|all-projects`. For example:

```sh
agent-archive setup --yes --provider s3 --bucket BUCKET \
  --aws-profile PROFILE --region us-east-1 --apps codex \
  --codex-discovery on --codex-capture-scope all-projects
```

`all-projects` plus `--codex-discovery off` approves hook-only capture for that
Codex scope. Existing installations keep omitted choices; `--yes`, refresh,
pairing, destination edits and new projects never imply expanded permission.
Pairing reviews the receiving machine's local capture choice and starts its own
permission window; another machine's live authorization intervals are never imported.
Use the review’s **Edit a setting → Codex project exceptions** to exclude a
directory or explicitly include a child beneath an excluded parent. The nearest
explicit rule wins; lifting an exclusion admits only eligible future starts.
Project exception rules are shared. If Claude Code or Cursor is also selected,
setup asks for separate confirmation naming those apps before changing their
capture permission. The default is No; declining leaves the rule and capture
permissions unchanged. Codex-only setup does not need that additional choice.
The final review keeps scope and nondefault restrictions visible; Details separates
capture mechanism, exceptions, approved Codex homes, start boundary, history,
copies and optional hooks.

Consent uses the session's original native creation time, not its file modification
time, copy time, first prompt, or discovery time. Existing history and sessions
created while paused stay excluded. Recognizable imports, forks and spawned-agent
records stay unsupported. A recently copied native session that has indistinguishable
supported metadata may qualify; discovery does not attest where execution occurred.
Project exclusions, destination consent, deduplication and privacy filtering still
apply. Setup's [7-day import](#sessions-from-the-last-7-days) and deliberate
backfill are the ways to include earlier history.

Ordinary `agent-archive status` leads Codex with discovery state and scope;
absent optional hooks are healthy, while broken owned hooks remain actionable.
Mixed supported and unsupported observations show skipped counts and reasons.
Update agent-archive for unknown source formats; hooks and backfill can help only
where that particular format is supported. The shell CLI version does not prove
which format every desktop build writes. Status shows last attempt age and
separates discovery/reconciliation, identity recovery and queued uploads.
Manual sync advances bounded work; it does not guarantee exhaustive coverage or
an ETA. Large cold source trees can need many scheduled passes.
`agent-archive status --verbose` adds details separately from hook observation.
A task found locally is not an upload, and an upload is not verified until its
filtered archive has been read back. A complete scan is coverage information,
not proof that a task was captured.

Discovery recognizes the recorded JSONL format: legacy (including an absent
history-mode field) and native paginated history, with supported local source
tags and valid creation/first-task evidence. Compatible versions and prereleases,
including the `0.155` family, qualify without an exact version allowlist or a
minimum-version gate. Records lacking essential evidence and unsupported
history representations still reject independently of other sessions.

Status reports observed session producer versions and distinguishes `runtime
tested`, `source inspected`, and `compatible untested` evidence. Untested
compatibility does not block capture; these labels do not establish consent or
verified publication. An installed executable's version does not determine the
format of existing sessions. Structural checks cannot prove that an unknown
future producer preserves all current semantics, and these rules do not claim
desktop GUI acceptance. See the [format contract](../reference/local-discovery-scanner.md#codex-format-compatibility)
for supported profiles and limits.

### Interrupted resource creation

Guided storage creation records a private receipt before sending each bucket
creation request. Receipts survive removing a setup draft. A request without a
durably recorded response is uncertain: it does not prove that a bucket exists
or that setup owns it. Check your provider account before retrying or deleting
such a bucket. S3 receipts identify the creation profile, not a verified AWS
account identity. R2 archive keys use the existing issuance ledger; receipts and
ledger records contain identifiers and credential references, never setup tokens
or credential values. Old drafts remain readable and have no inferred receipts.
Setup withholds new bucket creation if existing receipts cannot be read or the
receipt limit is reached; preserve and reconcile that history before retrying.
