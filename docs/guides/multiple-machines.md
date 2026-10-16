# Using more than one machine

Pairing and key-management questions use `?` for the active question and `›` for the answer. Numbered choices are the main decisions; bracketed shortcuts are secondary actions. Completed answers become short receipts where safe terminal redraw is available. Pairing codes remain confined to their cleared alternate screen, and hidden token and bundle receipts never reproduce private input. Human machine listings have labeled fields when the terminal is too narrow for aligned columns. JSON output retains its existing structure.

Before saving paired settings, use `[d] Full settings and privacy` to review exact project paths and native capture roots. Leaving the pager returns to the same settings and consent question. Storage is checked after you confirm these settings.


Several machines, macOS and Linux in any mix, can archive into the same
bucket and prefix. Each machine runs its own setup, with its own machine ID,
hooks, collector, and local state.

- **Ownership is per machine.** A session belongs to the machine that
  registered it. Only that machine uploads it, republishes it, and deletes it
  when retention expires or `backfill undo` removes it. Local state is never
  shared or synced between machines. So the sessions of a machine you retire
  or wipe stay in the bucket until you delete them; see
  [delete the archive](../getting-started/uninstall.md#delete-the-archive-in-the-bucket),
  which also shows a lifecycle rule that cleans up after a machine that is
  gone.
- **Reading is shared.** `list`, `show`, and `handoff` read the bucket, so
  they see every machine's sessions. Each session's metadata records the
  `machine_id` that captured it.
- **Handoff across machines.** `agent-archive handoff --latest` on another
  machine downloads the session from the archive. It finds it by the
  repository's `origin` remote, so the repository can be at a different path
  on each machine (a directory with no `origin` still needs the same path, or
  a session ID from `list`). Because a repository chooses its own remote,
  `handoff` names a session it found this way and asks before using it. Push
  your branch first: uncommitted changes stay on the machine that made them.
  See [handoff](handoff.md#finding-a-session-by-repository).
- **Retention is per machine too.** Each machine's retention setting applies
  to the sessions it owns, including Keep forever. Pairing copies the preference
  once; later changes do not propagate. Set the same value on every machine if
  you want one policy. Upgrade a receiving machine before pairing a Keep forever
  installation: older receivers reject that bundle. Provider lifecycle rules
  apply to the shared bucket independently and can override any machine's policy.
- **Credentials.** Each machine needs its own access to the bucket: an AWS
  profile, or R2 credentials entered in its own setup (stored in that
  machine's Keychain on macOS, or in a private file under its data directory
  on Linux; see [where credentials are kept](../security/privacy.md#where-credentials-are-kept)).
  [`setup --yes`](../getting-started/setup.md#set-up-without-questions)
  sets up another machine from a script. The command printed after setup
  carries the bucket folder, retention, skill-use capture rule, skill evidence,
  and agent skill installation policy. Whole repositories with a known origin
  use [repository matching](#repository-matching-in-setup-commands); adjust
  other project paths for the new machine. A key limited to one prefix (see
  [bucket permissions](../security/bucket-permissions.md)) works for several
  machines sharing that prefix.

Don't copy the data directory from one machine to another: two machines that
believe they own the same sessions would publish over each other. The rest of
this page is about the ways that happens by accident.

## Migration Assistant and Time Machine (macOS)

Migration Assistant and a Time Machine restore copy
`~/.local/share/agent-archive` like any other folder, machine ID included.

- **Moving to a new Mac, retiring the old one.** Nothing to do: the new Mac
  carries on as the same machine and owns the old sessions. Rerun
  `agent-archive setup` if `status` reports hooks or background as broken
  (the binary moved).
- **Both Macs stay in use.** On the new one, run `agent-archive uninstall
  --delete-local-data` and then `agent-archive setup`, which gives it a new
  machine ID. Sessions captured before the move stay owned by the old Mac.
- **Restoring an older backup on the same Mac.** The data directory goes
  back to the backup's state: sessions registered after the backup are
  forgotten locally, so this Mac never updates or deletes their copies in
  the bucket. A [lifecycle rule](../getting-started/uninstall.md#a-lifecycle-rule-as-a-backstop)
  eventually removes them.

## Cloned machines on Linux

A cloned VM, a container image with a set-up home inside it, a restored
backup, or a copied home directory carries the data directory and so the
machine ID. The same advice applies as on macOS:

- **The original is retired, and the copy takes over.** Nothing to do for the
  sessions: the copy carries on as the same machine and owns the old
  sessions. The systemd units live in `~/.config/systemd/user`, outside the
  data directory, so a copy of the data directory alone has no background
  job: run `agent-archive setup` if `status` says the background collector
  is not running (`setup --refresh` when it reports it as broken because the
  binary moved).
- **Both stay in use.** On the copy, run `agent-archive uninstall
  --delete-local-data`, then `agent-archive setup`, which gives it a new
  machine ID. Sessions captured before the clone stay owned by the original.

Linux has a cheap signal, so there is a warning. The first setup records a
digest of the machine's own ID (`/etc/machine-id`, else
`/var/lib/dbus/machine-id`) in `config.json` as `host_id`
([configuration](../reference/configuration.md)); it is never uploaded.
`status` and `setup` warn when the data directory was set up on a machine
whose ID is not this one's, which is what a clone whose machine ID was
regenerated looks like (cloud images, `sysprep` and most VM templates do
that, and it is the usual case). The warning names the two ways out: on a
copy whose original is still in use, `uninstall --delete-local-data` and
`setup` as above; when the original is retired, or this is the same machine
with its operating system reinstalled over the same home, remove the
`host_id` entry from `config.json` and the warning stops (setup records the
current machine's the next time it runs).

A data directory copied from a Mac to a Linux machine carries no `host_id`
(macOS records none), so nothing warns; treat it as a clone and set up again.

The warning is best effort, so set up a cloned machine again whether or not
it fires:

- It cannot see a clone that kept its machine ID (a disk copied as it is, a
  container committed with `/etc/machine-id` baked in).
- It says nothing when this machine has no usable ID: an image's empty
  `/etc/machine-id`, `uninitialized`, a container with none, a system whose
  `/etc` is read-only and gets a new ID at every boot, or a container that
  bind-mounts the host's file.
- It is a false positive after an operating system reinstall over the same
  home, which is why removing `host_id` is the way out.

### A home directory shared across machines

A home directory that several Linux machines mount at once (NFS, say) breaks
what this program assumes: each machine would find the others' hooks,
`config.json`, local state and ownership records in the one data directory and
claim the same sessions under one machine ID; file locks (`flock`) are not
reliable between machines, so two collectors can corrupt local state; and
every machine's systemd user manager loads the same unit from
`~/.config/systemd/user`, so every machine runs the collector against the same
files.

So on Linux, `agent-archive setup` and `setup --refresh` refuse, before any
question and changing nothing, when the data directory or the systemd unit
directory is on a network filesystem. It looks at the filesystem type the
kernel reports (`/proc/self/mountinfo`) for each directory, or for the
nearest directory above it that exists, since setup has not made them yet. The
types it treats as network are `nfs`, `nfs4`, `cifs`, `smb3`, `smbfs`, `ceph`,
`glusterfs`, `afs`, `lustre`, `gpfs`, `beegfs`, `pvfs2` (OrangeFS), the
cluster filesystems `gfs2` and `ocfs2`, and the FUSE ones that name a remote
(`fuse.sshfs`, `fuse.rclone`, `fuse.s3fs`, `fuse.gcsfuse`, `fuse.glusterfs`,
`fuse.ceph-fuse`, `fuse.mfs` (MooseFS), `fuse.juicefs`). It does not treat as
network a local disk, `tmpfs`, an `overlay`, an encrypted view of local files
(`ecryptfs`, `fuse.gocryptfs`), a plain `fuse`, or `virtiofs` and `9p`, which
share a host's folders with one virtual machine and are what WSL mounts
Windows drives with. A home that autofs mounts on demand is looked at once it
is mounted (the check mounts it, as setup would). It is a guard, not proof: a
network filesystem under another name, or a system whose
`/proc/self/mountinfo` cannot be read, is not noticed. macOS is never checked,
and there `--allow-network-home` is accepted and does nothing, so one setup
command can serve both systems.

The refusal names the directory, the filesystem and why it matters, and gives
two ways out:

- Put the data directory on local disk: set `AGENT_ARCHIVE_HOME` (for the shell
  that runs setup, and so for its hooks and background job) to a path there,
  and give each machine a home of its own for the apps' hook files and
  the unit directory. `AGENT_ARCHIVE_HOME` does not move the unit directory,
  which is under the home directory, so a home that is shared stays refused
  because of it.
- If the home is only ever mounted on **one** machine (a home that is network
  storage for a single computer, or a diskless workstation), run
  `agent-archive setup --allow-network-home`. Setup then goes ahead, says on its
  checklist that it was allowed, and records `allow_network_home` in
  `config.json` (only while a directory is on a network filesystem), so
  `setup --refresh` and later runs do not refuse again. `status` keeps warning
  that the home is on a network filesystem, in its warnings and in `status
  --json`'s `warnings`, so the risk stays visible.

An installation made before the check, on a network home, is not changed:
`status` warns, and `setup --refresh` stops, changing nothing, until you run
`agent-archive setup --allow-network-home` (or move the data directory to local
disk).

If you opt in on a home that several machines do mount, expect this:

- The clone warning fires on every machine but the first, since only the
  first one's ID is recorded in the shared `config.json`.
- `agent-archive uninstall --delete-local-data` on any one of them deletes
  the shared data directory, and with it the state of every machine that
  uses it.
- Each machine runs its own background collector and hooks against the same
  directory, which agent-archive's ownership rules and file locks were not
  written for and which nothing here tests.

Silencing the warning by removing `host_id` does not make this safe.

### Repository matching in setup commands

Setup's printed command uses `--project-repo REPO_KEY` for an included
repository root with an origin remote, so another checkout may live at a
different path. A configured subdirectory keeps its path to preserve scope. Repository keys are hashes; remote URLs and credentials are never
printed. Projects without a key retain their `--project DIR` argument.

When exclusions are configured, the printed command instead carries all capture
rules together through `--project-scope-file -` and a quoted shell heredoc.
The JSON travels on stdin instead of argv, so large scopes do not exceed
operating-system argument limits. Large lists without exclusions also use
this transport once the assembled arguments exceed 64 KiB; smaller lists
keep their ordinary `--project`/`--project-repo` command. This changes only
the transport, not which projects may be configured.
`--project-scope JSON` remains available
for small manual inputs; `--project-scope-file PATH` reads a saved JSON file. Each rule has `path` and `included`;
repository-relative rules also have `repo_key`. Exclusions and reincluded
subtrees follow the matched checkout, even if its destination path changes.
When a checkout lies inside a configured path-based ancestor, its rules stay
relative to that ancestor instead of relocating independently.
Distinct configured clones of the same repository keep their paths so their
separate exclusions cannot collapse onto one checkout.
Paths outside a configured repository use the destination home for `~/`.
Missing excluded folders remain excluded if created later. Setup refuses the
whole transfer when a repository is missing, ambiguous, blocked by saved
scope, or incompletely discovered, or a subtree resolves outside its checkout.
Saved destination exclusions stay in force. If a saved reinclusion would
defeat a transferred exclusion, or a transferred inclusion would override a
saved exclusion, setup refuses before changing any rule. Review the destination
capture scope before retrying; explicit transferred reinclusions remain intact.
Symlink aliases share the same capture decision and repository anchor.
No inclusion is applied without its associated exclusions. Inspect these
rules before running the command; update non-home absolute paths for the
new machine if necessary.
Scope flags require a nonempty value and cannot be combined with `--project`
or `--project-repo`; put every inclusion and exclusion in the scope instead.
Pairing receives scope from its encrypted bundle and refuses both scope flags.
A scope on stdin owns that stream; provide an R2 secret in
`AGENT_ARCHIVE_R2_SECRET_ACCESS_KEY` or read the scope from a file.
File/stdin scope input is bounded to 128 MiB.
Scope arrays contain at most 4,096 rules. Scope transfer also refuses a saved
destination scope or resulting combined scope larger than 4,096 rules, before
applying any rule. This limit applies to scope transfer, including legacy
configurations without a Codex scope policy; larger source scopes are preserved
in the printed input and refused rather than truncated. Review those scopes
before transferring. Saved dangling or looping symlinks also refuse the transfer;
ordinary missing excluded directories remain supported.
Equal-decision aliases coalesce;
conflicting alias decisions are refused atomically, so review those source
rules before transferring. Reapplying a compatible keyed scope is supported;
saved destination conflicts still refuse the entire transfer.

`setup --yes --project-repo REPO_KEY` includes only a unique, eligible clone.
It establishes each keyed candidate’s full checkout root with Git, so running
from a subdirectory cannot narrow the requested repository. It checks the
current directory, saved project roots, and the first record
of app history files, with a shared five-second budget, 128 distinct roots,
four Git processes, and 250 milliseconds per Git lookup. Discovery returns
partial results and reports timeout, cap, or unreadable files; incomplete or
ambiguous discovery skips repository selection. Existing exclusions also
block implicit inclusion of an overlapping root; saved inclusions block a
wider ancestor. Failed or nonportable origin lookups make discovery incomplete. Use an explicit
`--project DIR` to select a path independently. There is no home-directory
crawl or transcript-body search. Filesystem cancellation is cooperative:
a native filesystem call already in progress can outlast the budget.

## Machine records and names

`agent-archive machines` lists informational records from this destination.
Interactive setup and `setup --yes` publish a record after they commit, and the collector retries failed
publication independently of capture and retention. A successful heartbeat
is refreshed at most daily; it is not a signal of current activity. Paused
machines need not send heartbeats. A missing record is recreated on the next
daily publication. Older installations appear as `unnamed-` followed by four
characters of their machine ID, without reading a hostname.

Interactive first setup offers **Name this machine** at the final review.
Leave the default to use a neutral name without reading your hostname. Chosen
names are checked against bounded bucket observations before saving; concurrent
choices can still race. The list shows pairing dates when known and shared-key
source names with their immutable IDs, or the ID alone if its record is absent.
These are unverified claims; a shared key cannot be revoked independently.

Run `agent-archive machines rename work-laptop` on the machine being named.
Names contain 1 to 40 lowercase letters, digits, or hyphens and start with a
letter or digit. Observed duplicates are refused; concurrent naming can race.
When selecting an existing name, `agent-archive machines rename MACHINE_ID NEW_NAME`
uses the full immutable ID to distinguish duplicates. Renaming changes a label,
never machine or credential identity. A failed upload keeps the local name
and reports registration pending; the collector retries.

`agent-archive machines --json` reports records, omitted objects and whether
results are partial. Listing reads only machine records, at most 1000 pages or objects
and 16 KiB per record, with four concurrent reads and a shared five-second
budget. Oversized continuation tokens also stop listing. Unsupported schemas,
malformed records and read failures are reported
as omitted objects; incomplete results exit with code 1.

Every bucket credential can forge these records. They are not provider-verified,
never prove exclusive key ownership, and never authorize removing access.
Manual R2 credentials are labelled `r2_unknown`; one observed user does not
make a key exclusively owned. S3 records carry `aws_profile` as their kind.
Provider verification, pairing and revocation are not part of these commands.

`setup --refresh` keeps its existing no-storage/no-credentials promise. It does
not publish a record or write registration state, including when nothing needs
refreshing. The next collector pass publishes a changed application version
through its normal fingerprint check, without waiting for the daily heartbeat.

### Provider observations

Run `agent-archive machines --verify` to request provider verification. This reads Cloudflare account token metadata
for the configured R2 destination; it does not create or revoke keys. Arbitrary
S3-compatible endpoints are refused.

The management token comes from `CLOUDFLARE_API_TOKEN` first, which is read and
removed before provider work. Otherwise an interactive invocation can use
`cloudflare_token_command` in config, an argv array such as
`["op", "read", "op://Private/Cloudflare/agent-archive"]`, or a hidden prompt.
The command runs without a shell, has a 20-second budget and a 4 KiB output
limit, and receives an environment without token, secret, object-access or
pairing variables. Its stderr and failure output are suppressed. The token
is held in memory and is never saved. Ordinary listing and collection never
run this command. `--yes` and `--json` require the environment token and never
prompt or run the configured command.

Use Account API Tokens Read or Write. A successful list may expose only tokens
created by the caller (`list_self`), so completed pagination is not evidence of
account-wide completeness. Missing metadata means missing **or not visible**,
not revoked. Checks read at most 20 pages / 1,000 tokens and 16 supplemental
details within one 20-second provider budget; partial observations remain
available when requests fail.

Canonical issued names are `agent-archive r=<32hex> i=<32hex> k=<32hex>` (118
bytes, within Cloudflare's 120-character limit). Bucket-bearing names from the
original proposal are not accepted. Exact provider policy must independently
match the configured account, jurisdiction, bucket and verified permission
group. Unsupported policy fields or inconsistent pagination evidence remain
unknown. A matching name describes issuance, not machine ownership. Only the
current destination-bound local assignment establishes a committed local
binding; other bucket records remain untrusted claims. Legacy/manual keys
remain unknown. “Claim not observed” keys are candidates for inspection, never
proof that a key is unused or safe to revoke.

## Encrypted pairing

First setup asks whether to import a pairing from another machine. On a
configured source, run `agent-archive machines add --name laptop` to create a
dedicated R2 pairing or transfer S3 settings. Use `--share-key` explicitly
when sharing the active R2 key is intended. The source checks storage before
creating a pairing, then offers **Save a file** (recommended) or **Copy to clipboard**.
The default file is named `agent-archive-pairing-laptop.txt` in Downloads, or in
your home directory when Downloads does not exist. Existing files are never overwritten.

Transfer the file to the other machine, for example using AirDrop. The source
prints a copyable command using the actual filename and the receiving machine's
home directory. Save the file in Downloads there, then run that command:

```sh
agent-archive setup --pair-file "$HOME"/Downloads/agent-archive-pairing-laptop.txt
```

If you save the file elsewhere, use that path with `--pair-file`. Alternatively,
run `agent-archive setup --pair` and enter the file path or paste copied pairing
text. Clipboard transfer requires pasting the text into a file or message and
transferring it before closing the source command; its clipboard is cleared on
exit if it still contains that text.

After you confirm that the transfer has arrived, the source shows the six-word
code automatically on a temporary screen. Enter it privately on the receiver;
deliver the code separately from the pairing file. The first three characters of each word
are sufficient; use `yo-` for `yo-yo`, including the hyphen.
Review the destination before capture settings: an existing destination change
requires explicit consent. Source apps that are absent here are skipped.
The source shows **Transfer prepared**, **File delivered locally**, and **Code
available**. These describe delivery, not successful receiver setup. Choose
**Close transfer instructions** when finished; receiver completion remains
unconfirmed on the source. The receiver progresses through **Review settings**,
**Check storage**, and **Save configuration**, then says **Paired successfully**
only after its configuration commits. There is no automatic acknowledgement
back to the source. Preparing spare keys for future
machines happens afterward and does not invalidate this pairing if it fails.

Cancelling on the source records local cancellation but does not delete transferred
files or invalidate their encrypted contents. Dedicated R2 cancellation attempts
key deletion and reports confirmed deletion or pending cleanup; shared R2 access
requires replacing the key everywhere. S3 pairing issues no AWS credential and
cannot revoke the receiver’s local profile. Bundle expiry prevents a new open
after the authenticated expiry tolerance, but never revokes storage access.

If receiver storage verification or configuration commit fails, existing capture
remains unchanged. Retry the same unexpired pairing, or finish/discard saved setup
where offered. Reusing the same pairing reuses its staged credential; it does not
issue another provider key. An expired bundle requires a new pairing from the
source. Delete file copies when no longer needed.

Explicit shared-key pairing shares the active R2 key. Cancelling a pairing, expiry, and deleting a
machine record do not remove access. Replace the shared key on every machine to
revoke it. S3 bundles contain a profile name and settings, without AWS credentials;
configure that local profile with `aws configure --profile NAME` or
`aws configure sso --profile NAME` before receiving. No provider management token
is requested. Registry claims remain informational and untrusted.

Use `--print` for encrypted bundle output or `--file PATH` for an exclusive private
0600 file. Interactive source delivery requires terminal input and output;
codes appear only on a cleared alternate screen and the
clipboard is cleared on normal exit only when it still contains that exact bundle.
A crash or interruption leaves delivery uncertain; check `status` or `machines`.
Do not record or screen-share code display. Delete explicitly saved bundle files
when no longer needed. The default expiry is 15 minutes; `--expires` accepts 5m
through 24h, with five minutes of authenticated clock-skew tolerance.

For deliberately scripted use, `machines add --yes --name laptop` prints both
pieces (or prints the code with `--file`). Receive with `setup --pair-file PATH`
or `setup --pair-file -`, plus `--yes` and `AGENT_ARCHIVE_PAIRING_CODE`; the receiver
reads and removes that variable before setup. There is no code command-line flag.
`--yes` refuses to replace an existing destination. Any presence of
`CLAUDE_CODE_SESSION_ID`, `CODEX_THREAD_ID`, or `CURSOR_AGENT` refuses pairing before
home, credential, storage, trace, or output changes, regardless of other overrides.

Repository matching is bounded by one five-second deadline, 128 canonical roots,
four Git workers, and 250ms per Git lookup. It does not crawl home directories or
search conversation bodies. Native filesystem calls cannot always be cancelled;
the deadline prevents new work once they return. Relative subtree paths retain
inclusion scope after a repository moves. Known origin mismatches never fall back
to a home path. Ambiguous, incomplete, and unresolved exclusions are skipped under
`--yes`; local exclusions remain. Choose skipped scopes deliberately with ordinary
`setup --project DIR`. Unmapped source exclusions withhold affected inclusions.

The receiver keeps its immutable machine ID and commits through ordinary setup's
credential staging, hook, and scheduler transaction. Interrupted staging retries
reuse a matching opaque credential reference. Codes, bundles and decrypted payloads
are never saved in drafts or ledgers. After commit, machine publication failure
remains pending without disabling capture; the collector can reconstruct paired
assignment metadata and publish after the bundle expires. App approval and history
imports remain separate steps.

### Dedicated key issuance

Dedicated R2 issuance is the default for `machines add` without
`--share-key`. The configured destination must be an exact Cloudflare R2 endpoint,
not a compatible third-party endpoint.

A `CLOUDFLARE_API_TOKEN` or interactive `cloudflare_token_command` creates a fresh
bucket-scoped key. The environment token is removed from the command's environment;
your shell may still retain it. The management token stays in memory and is discarded
at exit. Without a token, an eligible spare is reserved; without a spare, interactive
use offers a hidden token prompt, explicit sharing, or cancellation. `--yes` never
shares implicitly and never runs a token command; supply the environment token or
use an existing spare. `--share-key` deliberately retains the shared-key behavior.

`machines add --spares N` saves a target from zero through five; the default is two.
Zero disables spare use/refill. Lowering the target does not silently delete existing
provider keys. Guided R2 creation also prepares spares while its management token
is available. Listing, collector activity, and revocation never create or refill keys.
Spare refill failure is separate from pairing delivery: the delivered bundle remains
valid even when refill or an advisory config index update fails.

The private `issued/slot-<slot-id>.json` ledger owns eligibility. Its durable records
precede creation, credential staging, reservation, and bundle exposure. Interrupted
creation is reconciled using the exact immutable provider name and exact bucket policy;
lost one-time token values cannot be recovered. Guided retries require confirmed cleanup
of the previous slot. A guided setup key stays staged until configuration commits;
discarding its draft retains provider cleanup lineage. Uncertain cleanup stays pending and
requires explicit management access. Never edit the ledger or reuse config spare
references as proof of eligibility. Corrupt records withhold spare use.

A file, clipboard, or print error may have exposed the bundle, so its key stays reserved.
Expiry is not key revocation. Delivered issuer-local secrets are removed at command
exit; immutable issuance lineage remains for later verification. Local cancellation
needs management access to delete a dedicated token; otherwise access may remain.
Provider deletion does not guarantee immediate access cutoff; verify propagation.

### Revocation and shared-key migration

Pairing is available during first setup and through explicit commands. Read-only verification and complete pagination never prove ownership
or account-wide visibility.

Use
`agent-archive machines revoke NAME` or select exactly one `--machine-id`,
`--recipient-id`, `--pairing-id`, or retry `--operation-id`. A bucket name or
machine record is a hint, never deletion authority. A current destination-bound
local assignment supports self-revocation; a healthy issuer's ledger supports
recipient/pairing selection and unique original issuance labels, independently of bucket names. After rename, use the original trusted issuer label or recipient/pairing ID; for the new name or immutable machine ID, provide a private
`--binding-file` independently established from the machine's committed local
assignment and an out-of-band check. Never copy bucket claims into this file.
Its strict JSON shape is `{"machine_id":"<32hex>","name":"portable",
"independently_verified":true,"assignment":{...},"retired_assignments":[],
"unused_spares":[]}`, with the destination-bound assignment fields from
`machine-assignment.schema.json`. The declaration records operator evidence;
the software still verifies exact provider token ID, scope and immutable name.
It cannot establish that an operator's assertion is honest. Unknown ownership
refuses even with `--yes`. `name` is independently checked operator evidence, not
a label taken from storage. After a rename, use `--machine-id` with the existing
verified file, or recheck the new name out of band. Include all same-destination
retired dedicated assignments from the target
configuration and only independently checked, undelivered spare bindings from
its local issuance ledger in the two optional arrays. Each uses the assignment
shape above; an unused spare must have that machine as issuer and no pairing ID.
The file is a private regular file (mode 0600), limited to 64 KiB and 128 extra
bindings. Do not derive spare eligibility from provider names or bucket claims.

Retired bindings already deleted by this operator's exact local journal confirmation
are retained for history but excluded from a new selection. Destination, provider
scope, requester and every immutable key binding must match. Unknown, pending,
foreign or bucket-only outcomes cannot omit keys; missing provider metadata alone
remains unresolved. This lets revoke → re-pair → revoke work without weakening
absence checks.

Normal revocation selects verified dedicated/retired keys and unused spares;
it excludes keys delivered to other recipients merely by the target issuer.
`--include-issued` requires an independently bound issuer machine ID and checks
all visible provider descendants, including delivered recipients and potentially
copied credentials, without trusting a compromised ledger to exclude keys.
Creator-only visibility and legacy/shared gaps remain explicit. Replace keys
from a healthy issuer before compromise recovery where practical.

Each random operation is persisted privately in `revocations/`, then published
as `machines/revocations/<operation-id>.json`. At most 128 verified keys fit one
operation. Selection and execution each have a 20-second provider budget;
initial and final publication each have a separate five-second budget. Retries use exactly the
local selected set and never regress a confirmed result. Different revokers
publish distinct objects. Setup and issuance locks preserve the selection
through deletion; selected local slots are withheld from delivery even when
provider outcomes are unknown. Confirmed retries finish local slot retirement. Only a successful provider delete confirms a key;
404 or missing metadata remains unknown until live absence semantics are
verified. Self's active object key is last. Final bucket publication can fail
while local deletion results remain confirmed. No-token and AWS-profile requests
say access was not removed and preserve the requested name or immutable ID as
explicitly unverified operation metadata. A named self-revocation requires the
locally committed name (or explicit immutable ID); a forged bucket label cannot
redirect the command to this machine's trusted key. Revocation never creates or refills keys, never
promises immediate cutoff, and leaves sessions/downloaded data intact.

Run
`agent-archive machines own-key` to migrate shared/legacy R2 access. The exact
slot is checkpointed before minting, verified/staged privately, and committed
through setup's rollback-capable transaction. The existing machine ID is its
immutable recipient. Retry resumes the exact staged slot only while its original
pre-migration credential reference remains active. A later setup that replaces
that reference blocks resume before token/provider or setup work, even if the
new reference loads the same shared key. The staged key remains recorded; use
safe `own-key --cancel` before starting a new migration. Recovery after the
exact staged slot has already committed still finishes its cleanup. An interruption
before slot allocation leaves no provider key: a validated checkpoint with no
remaining issuance or cleanup obligation can be retired locally on retry or
cancel, including after setup changes destination. This cancellation needs no
management token. Uncertain issuance and pending secret removal stay recorded. After confirmed
provider deletion, retry or cancellation can finish exact staged local secret
removal when the credential store recovers. Active or retained aliases, unknown
identity, and further cleanup failures keep the obligation recorded.
Only after commit
can the obsolete shared local secret be deleted; another local destination's
reference, a matching retired alias, an unreadable binding, or a cleanup failure
keeps access explicitly present. Completed cleanup retires its checkpoint so a
later supported setup/migration can create a new own key; incomplete cleanup
and staged keys remain recorded. Earlier dedicated keys retain their issuance
lineage and are never deleted by checkpoint retirement. The shared
provider key is never deleted. Registration failure is separate from commit.
Both commands use the explicit token sources described above; `--yes` and JSON
revocation do not prompt or run a configured token command.

For a staged own-key operation that has not committed, run
`agent-archive machines own-key --cancel --yes` with an environment management
token. Cancellation reloads the committed config and active stored credential
and all retained local destination credentials
under locks before deleting the exact staged dedicated key. It refuses if the
key is active, the binding is unknown, or setup recovery is pending. A committed
own-key operation cannot be cancelled this way; use verified revocation.

The fake combined acceptance recipe is
`go test ./internal/cli -run 'TestMachine(TwoHome|Remote)FakeAcceptance' -count=1`.
It uses isolated source, recipient and operator homes, an in-memory credential store and object store,
and a loopback fake provider. It exercises own-key, dedicated add, pair setup,
list with provider verification, rename preserving identity, and receiver
revocation, source issuer-label selection, third-machine revocation after rename,
re-pairing with retired assignments, explicit unused-spare ownership, delivered
descendant exclusion, compromised issuer lineage, and partial deletion/retry.
The focused revocation tests cover forged mappings, delivered-key
exclusion and issuer inclusion, scope mismatch, unknown 404 outcomes, and
publication failure. This establishes local contracts only. Live provider
permission, inventory visibility, absence semantics and revocation propagation
coverage remains tracked separately from fake tests. The maintainer approved
general availability after additional acceptance testing; a full separate-machine
release test remains planned.


Ordinary `machines` listing reads records and revocation operations together within
one five-second budget, at most 1,000 pages or objects and four concurrent reads.
Records are limited to 16 KiB and operation snapshots to 64 KiB. Operation results
are explicitly **untrusted bucket claims**, distinguishing requested, partial,
failed/unknown, and claimed provider-confirmed selected sets. They never prove
removal, identity, or account-wide completeness. `status` reports bounded local
operation snapshots as local provider results. Machine records include up to five
unused spare bindings and up to sixteen retired credential hints; longer retired
history is explicitly incomplete. The full committed history stays local and is
used for revocation regardless of these listing hints.

Pairing preserves standalone home-relative exclusions as well as nested and ancestor
restrictions. Unmappable source paths remain secret-free warnings; affected roots
are withheld under `--yes`. Interactive setup offers safe home-relative exclusion
mapping before any explicit scope override. Recovering one clone preserves every
successful exclusion mapping; other unmapped clones are withheld individually.
Manual project paths accept `~` and `~/` against the receiving home, while ordinary
relative paths remain relative to that home. Discovery checks direct candidates
first; app-history headers are read only for unresolved requests. Partial candidates
remain visible with an explicit warning that additional clones may exist; unattended
setup skips incomplete evidence. Interactive recovery can select a manual directory,
checks repository identity and subtree boundaries, and shows repository/path provenance.

Clipboard failure offers a retry, a new private file, deliberate terminal bundle
printing, or a stop that retains uncertain delivery. A password-manager failure can
be retried once or replaced with hidden token input during an explicit interactive
invocation. Missing apps offer a recheck after installation or a deliberate skip.
Pairing success is printed once setup commits, before housekeeping and any import
offer; failed draft removal is reported as pending cleanup of committed setup.
