# Uninstall

The guided questions default to No. Removing integrations and deleting owned local data remain two separate confirmations. Each question follows the consequences it approves, with numbered Yes/No choices; named `yes` and `no` answers still work. Remote purge separately requires the exact displayed digest unless `--yes` is supplied.


```sh
agent-archive uninstall
```

After confirmation, uninstall stops the background collector, removes its
job (the LaunchAgent on macOS; on Linux the systemd timer and service units
and the link that enabled the timer), the hook entries setup added, and the agent skill files it
wrote (`/handoff` and the `agent-archive` skill; see
[agent skills](../guides/agent-skills.md#where-the-files-are)), and
disables capture. A skill file without setup's marker line is yours, and one
naming another data directory is another installation's; both are kept and
reported. It keeps
local evidence, settings, and credentials, so `agent-archive setup` can
reinstall. Unrelated hook handlers, the bucket, and the `agent-archive`
executable are always kept. Confirming needs a terminal; `--yes` skips the
confirmations and is required without one.

Only the collector of the current data directory (see
[`AGENT_ARCHIVE_HOME`](../reference/configuration.md#environment-variables))
is stopped, and only when launchd or systemd loaded it from this
installation's own plist or unit files. The hook files of the apps setup installed must be readable,
including any an earlier release left at `~/.claude/settings.json` or
`~/.codex/hooks.json`; any other file is only checked for leftover handlers,
so one that is not valid JSON is reported and left as it is rather than
blocking uninstall.

## When the background scheduler cannot be reached

Uninstall stops the background collector through the scheduler that runs it.
When that scheduler cannot say whether the job is loaded (it is not reachable
from this session, or a unit is masked), uninstall stops before it changes
anything, says what is wrong and what to do about it, and prints the command
that stops the job by hand from a session that can reach the scheduler.

```sh
agent-archive uninstall --skip-scheduler
```

goes on anyway. It tries to stop the job, then removes the job's definition,
the hooks and the skill files as usual, prints the same command for stopping
the job by hand, and ends by saying the collector was **not verified stopped**.
Deleting a definition does not stop a job that is already loaded, so run that
command from a session that can reach the scheduler if the job may still be
running. The job of another installation is left as it is either way.

On Linux the usual cause is a session with no systemd user bus (SSH without
`pam_systemd`, a container, `su`). The command it prints is
`systemctl --user stop agent-archive-collector.timer agent-archive-collector.service`
(with the installation's own unit name); run it from a login session that has
the bus, or after `loginctl enable-linger`. A stop, not `disable`, because
`systemctl` refuses to disable a unit whose file is already gone and a stop
still ends the timer the manager holds. The link that enabled the timer is
removed with the unit files, so nothing dangles.

## Delete local data too

```sh
agent-archive uninstall --delete-local-data
```

This also deletes agent-archive's own files in the data directory and stored
R2 credentials (the Keychain item on macOS, the `credentials` folder on
Linux). If the data directory is shared with other machines, which setup [refuses
on Linux unless you opt in](../guides/multiple-machines.md#a-home-directory-shared-across-machines),
this deletes it for all of them. It shows how many sessions are still waiting to upload and
asks a second time (`--yes` answers both). Unpublished evidence is lost. Only
files agent-archive creates are removed (see
[local state](../reference/local-state.md)); anything else in the directory
is kept and reported. The lock files go last, while uninstall still holds
them, so nothing can start work in a half-deleted directory.

Neither mode reads or deletes anything in the bucket; see
[delete the archive](#delete-the-archive-in-the-bucket).

## Remove the binary

Uninstall keeps the `agent-archive` executable, so you can still read the
archive or set it up again. To remove it too, after uninstalling:

```sh
rm "$(command -v agent-archive)"    # add sudo if it is in /usr/local/bin and not yours
```

## Delete the archive in the bucket

**Only the machine that captured a session ever deletes it**, through retention
or `backfill undo`, and only while that machine runs agent-archive and isn't
paused. The sessions of a machine you uninstall, wipe, retire, or pause for good
stay in the bucket indefinitely, and nothing else cleans them up.

To remove only unreferenced source snapshots from the currently configured
bucket, run `agent-archive purge plan`, review its exact keys, pause every machine
uploading to that prefix, then run `agent-archive purge apply PLAN` within five
minutes. Its report lists deleted and remaining keys. This does not delete
current sessions or empty an entire bucket. The full-prefix shell procedure
below is still needed for complete archive removal.

To delete everything under your prefix now, pause or uninstall **every** machine
that uploads there. Use the [cleanup preparation block](../security/privacy.md#after-a-filter-upgrade)
first in a bash or zsh shell. It defines `purge_prepare` and `purge_apply`,
attempts a strict unreferenced-source plan without deleting it. If that initial
plan refuses damaged metadata, the full-prefix plan below can still be prepared
after the explicit local stop command below succeeds. The AWS CLI and `jq`
must be installed, and your
credentials need list and delete access for full-prefix mode (selective modes
also need read access). In that same shell, set your
bucket and prefix and make a fresh full-prefix plan. This block explicitly
reruns ordinary uninstall for the local installation without prompting, even
when its integrations or local configuration are already removed. It refuses
active-lock or unknown-scheduler errors. Keep the binary available until cleanup
is done, stop every other uploader separately, and never use `--skip-scheduler`
to establish that uploads have stopped:

<!-- purge-recipe:all -->
```sh
bucket=my-archive-bucket
prefix=agent-archive/          # your prefix with its trailing slash, or empty for the whole bucket
if purge_stop_uploads uninstall; then
  purge_prepare all
fi
```

Review **every printed key**. An empty prefix plans the entire bucket. If the
plan contains exactly what you intend, run `purge_apply` within five minutes
in the same shell. Full-prefix mode validates the complete scoped listing and exact reviewed keys;
it does not require readable metadata or existing source references. Metadata
pointers are deleted before the remaining keys. A failed listing means zero deletions.
If preparation fails before printing a valid plan, fix the issue and rerun
`purge_prepare all`. After a valid plan has been printed, use `purge_resume`
with its retained directory for recovery. A delete failure reports the keys
already removed and those still pending. There is no rollback. External
writers can race these shell commands, so keep all uploading machines paused
throughout.

To delete only one machine's sessions, find its machine ID first (`jq -r .machine_id config.json`
in that machine's data directory, or the `machine_id` that `show --json` prints for one
of its sessions). Pause **every** uploading machine, run the linked preparation
block above in the same shell, and make a new machine plan:

<!-- purge-recipe:machine (scripts/test_purge_recipe.py runs this block) -->
```sh
bucket=my-archive-bucket
prefix=agent-archive/          # your prefix with its trailing slash, or empty
machine=0123456789abcdef0123456789abcdef
purge_prepare machine "$machine"
```

Review the exact keys, then run `purge_apply` in the same shell within five
minutes. It rechecks the complete listing and every metadata object before
the first delete; a changed object aborts the plan. On partial failure or interruption, retain the printed plan directory and run
`purge_resume /absolute/path/to/retained-plan` from the linked preparation
helpers. Review the remaining original keys, then apply the new single-use plan
within five minutes. Keep all writers paused through recovery. Deleting the
local manifest removes resumability; remove retained plan directories only after
success. On a versioned S3 bucket, ordinary
deletion hides current versions; remove noncurrent versions separately or
use a lifecycle rule.

### A lifecycle rule as a backstop

A bucket lifecycle rule deletes objects by age whatever happens to your
machines. With finite retention, make it comfortably longer than the longest
policy any machine uses (90 days by default). Object age and session capture
age differ, so even a longer rule can remove sources of a session still kept
by agent-archive. **Keep forever has no longer finite backstop:** remove or
exclude the archive prefix from expiration rules if you want that preference
to retain evidence indefinitely. Retired machines receive no cleanup worker;
delete their archives explicitly when wanted.

The following 180-day example intentionally expires objects, including those
owned by a Keep forever installation. It needs an administrator's credentials,
not the archive's:

```sh
# Replaces the bucket's whole lifecycle configuration: merge any rules it already has.
aws s3api put-bucket-lifecycle-configuration --bucket my-archive-bucket \
  --lifecycle-configuration '{"Rules": [{"ID": "agent-archive-backstop", "Status": "Enabled",
    "Filter": {"Prefix": "agent-archive/"}, "Expiration": {"Days": 180}}]}'
```

For R2, add a rule for the prefix under the bucket's **Settings → Object
lifecycle rules** in the Cloudflare dashboard, or see Cloudflare's
[object lifecycles](https://developers.cloudflare.com/r2/buckets/object-lifecycles/)
page for the API.

Objects expire one by one, by their own age. A session whose metadata was
refreshed later than its source (after a parser upgrade) can be left, for a
while, with metadata whose source is gone: `list` still shows it and `show
--transcript` reports the source missing, until the metadata expires too.

## When uninstall refuses

- If another operation is finishing, wait and retry.
- If the scheduler is unavailable (launchd cannot be reached, or on Linux the
  systemd user manager has no user bus), uninstall says what is wrong and
  refuses; fix it, or use [`--skip-scheduler`](#when-the-background-scheduler-cannot-be-reached).
  Likewise if a hook file was edited while uninstall ran: resolve the
  reported problem and run it again.
- After an interrupted setup, uninstall refuses until `agent-archive setup`
  recovers or `agent-archive setup --abandon-recovery` discards the record
  (see [troubleshooting](../guides/troubleshooting.md#an-interrupted-setup)).

Do not remove the data directory by hand while a collector is running.

## Downgrading

- **After choosing Keep forever**, older binaries refuse the saved configuration
  before writing. Returning to finite retention keeps this compatibility fence;
  retain a version that supports Keep forever for installed hooks and collectors.
- **After changing storage**, do not downgrade to a version that does not
  support `destination_since`. Older versions ignore that saved boundary and
  may upload earlier sessions to the new destination.
- **Pending metadata-only publications.** This version can queue a
  publication that carries only a reference to an uploaded source (when a
  parser upgrade meets a retained bundle it cannot rebuild). Before
  downgrading, run `agent-archive sync` until `status` shows nothing pending:
  an earlier version cannot publish such a file (under `pending/`, with
  `"metadata_only": true` and no `source_bytes`) and reports it as failing on
  every pass. If you already downgraded, deleting those files is safe; the
  metadata is refreshed on the session's next change.

Machine records under `machines/` and remote credential access remain after
uninstall. Remove access using your storage provider; the informational machine
list cannot prove or remove it. Include `machines/` when deleting the entire
archive. `--delete-local-data` also removes local machine registration retry state.

Uninstalling does not revoke provider access. Another authorized machine can use `agent-archive machines revoke` with an
independently verified binding; bucket records alone are insufficient. A
request or unknown provider result does not mean access was removed. Local data
removal includes this installation's revocation journals and own-key checkpoint,
without deleting unrelated files or other users' shared provider key.


Before deleting local data, retain independently verified nonsecret ownership
bindings from the retiring machine: its `machine_id`, chosen name, current
`machine_assignment`, same-destination `retired_machine_assignments`, and unused
spare assignments from its local issuance ledger. Keep them in a private operator
binding file as described in [machine revocation](../guides/multiple-machines.md#revocation-and-shared-key-migration).
Do not reconstruct this evidence from bucket records. From a healthy authorized
machine using the same destination, run:

```sh
agent-archive machines revoke --machine-id MACHINE_ID --binding-file /private/path/verified-binding.json
```

Use the verified immutable ID after rename. Review the provider-verified key set
and confirm interactively; `--yes` never bypasses ownership checks. For issuer
compromise, add `--include-issued` only after verifying the issuer ID out of band;
this can remove delivered descendants' access too. The original healthy issuer
can instead select its unique original issuance label, `--recipient-id` or
`--pairing-id`. A requested, partial, failed or unknown result leaves access removal
unconfirmed. Retain the operation journal and retry its `--operation-id` from the
same operator home. Shared or legacy access requires provider dashboard/IAM action;
replacing a shared key requires updating every user. Uninstall itself never deletes
provider keys, and downloaded data remains.
