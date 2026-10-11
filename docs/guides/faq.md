# FAQ

**Is there a hosted service or an account?**
There is no agent-archive account, hosted service, or telemetry. You do need a Cloudflare or AWS account to provide the private bucket and credentials that agent-archive uses for storage. See [create a bucket](../getting-started/bucket.md).

**What exactly is uploaded?**
Prompts, assistant text, tool inputs and outputs, commands, file paths,
working directories (which usually contain your username), Git branch names,
model names, and token counts, after a best-effort filter that drops hidden
reasoning, injected instructions, images, and unknown fields, and redacts
recognizable credentials. Fresh setup records skill names and hashes of
filtered skill text, with no skill bodies. This includes user-level skill
folders (such as `~/.claude/skills`), whatever the project, and the project's
skill folder. Older configurations retain filtered skill bodies until you
change their `skill_evidence` setting; you can choose `none`, `metadata`, or
`body`. There is no client-side encryption. See [privacy](../security/privacy.md#what-is-uploaded).

**Can it capture sessions I had before installing it?**
Yes. Setup imports the sessions of the last 7 days in the projects you
include ([details](../getting-started/setup.md#sessions-from-the-last-7-days)),
and [`agent-archive backfill`](backfill.md) imports older ones, or ones in
other folders, when you run it.

**Does it capture every project on my machine?**
By default, capture is limited to projects you include in setup (or that a
backfill adds, which it tells you first). Codex also offers an explicit
all-current-and-future-projects scope; it applies only to Codex and does not
change Claude Code or Cursor scope. Existing installations must opt in. See
[automatic Codex discovery](../getting-started/setup.md#automatic-codex-discovery).
Sessions run from your home directory or a temporary directory are skipped
by default.

**What does it cost?**
Your provider's storage, request, retrieval, and transfer charges, as
applicable. Check the current [Cloudflare R2 pricing](https://developers.cloudflare.com/r2/pricing/)
or [Amazon S3 pricing](https://aws.amazon.com/s3/pricing/) for the storage
class and region you choose. Sources are gzip-compressed filtered
transcripts (tool results are capped at 64 KB per string), and a session is
uploaded again only when it changed; retention deletes old snapshots and
expired sessions. There is no typical figure to quote, since it depends on
how long and how many your sessions are. To measure current stored bytes, run
`aws s3 ls s3://<bucket>/<prefix>sessions/ --recursive --summarize` for the
same bucket and prefix configured in agent-archive (for R2, also pass its
`--endpoint-url`). Divide **Total Size**, not the object count, by the readable
session count from `agent-archive list --all-projects --json --limit 0`, with
no other filters. Its `total_matched` is exact when `total_matched_known` is
true; `returned` and `sessions.length` also give the full readable count in
this unlimited result. Default text and JSON listings return at most 50
sessions, and capped JSON may omit `total_matched`
([JSON contract](../reference/json-output.md#list---json)).

For example, 200 MiB across 200 readable sessions is approximately
1 MiB/session, even if the default listing shows only 50. With zero sessions,
do not divide; report the stored bytes and zero readable sessions instead.
Unreadable or skipped metadata makes the denominator incomplete: investigate
stderr warnings before using the estimate. This is approximate current storage
per readable session, not exact transcript size or provider billing; the
session prefix can include superseded objects, and versioned storage charges
can include noncurrent objects. Your provider's billing page shows request
counts and charges.

**How do I stop it for a while?**
`agent-archive pause`, then `agent-archive resume`. Pause persists across
restarts.

**How do I delete everything?**
`agent-archive uninstall --delete-local-data` removes the hooks, the
background job (the LaunchAgent on macOS, the systemd units on Linux),
agent-archive's local files, and stored R2 credentials; then
remove the binary. It never touches the bucket, and no machine deletes another
machine's sessions: see
[delete the archive](../getting-started/uninstall.md#delete-the-archive-in-the-bucket)
for the commands, and for a lifecycle rule that cleans up after a machine you
retire.

**How long are sessions kept?**
90 days by default; setup offers 1 to 36,500 days or **Keep forever**
(`agent-archive setup --yes --keep-forever`). Finite retention counts from the
last meaningful capture, including preserved revisions; importing old native
sessions starts a new retention window rather than expiring them immediately.
Keep forever disables automatic whole-session expiry for existing and future
sessions owned by this machine. It keeps the active source and bounded meaningful
revision history, not every redundant snapshot. Superseded-source cleanup,
[verified privacy replacement](../security/privacy.md#after-a-filter-upgrade),
explicit purge and import undo still run. Provider lifecycle rules and manual
bucket deletion can still remove evidence; deleted sessions are not restored.
The preference is per machine, and older binaries refuse its sticky compatibility
fence even after you return to finite retention.

**Can I use it on several machines?**
Yes, macOS and Linux in any mix; see [multiple machines](multiple-machines.md).
If you move to a new Mac with Migration Assistant, or restore from Time
Machine, read [that section](multiple-machines.md#migration-assistant-and-time-machine-macos)
first; on Linux, read [the one on clones](multiple-machines.md#cloned-machines-on-linux):
a copy keeps the old machine's identity.

**Can my coding agent see my archive?**
Through the `agent-archive` skill setup installs (`agent-archive setup --no-skills` turns it off), it reads what `handoff`, `list`, and `show` print: the same filtered content you would see running them, cut to a size bound. That is what the skill tells it to run, not a barrier: an agent with a shell runs as you, and can run `agent-archive` or read your files directly. The agent treats it as data, not instructions, and Claude Code asks before it first uses the skill and before it runs the commands. See [agent skills](agent-skills.md) and [privacy](../security/privacy.md#what-an-agent-can-read-through-the-skill).

**Does it run on Linux?**
Yes, for persistent capture on a machine with a systemd user manager (systemd
240 or newer; RHEL 8 and its rebuilds from 8.3). The background collector is a
systemd user timer, so a headless machine or an SSH session needs `loginctl
enable-linger`; there is no cron fallback. An R2 key is kept in a private file
(not encrypted), so use an S3 profile where you can. Release binaries are
unsigned (the installer checks them against `SHA256SUMS`, and `gh attestation
verify` confirms where they came from). The live synthetic-session acceptance
run passed 91 checks on Ubuntu 24.04 with systemd 255 on both arm64 and amd64,
using a throwaway MinIO bucket. What is **not** verified on Linux:
the real Cursor app and `cursor-agent` hooks (a Cursor forum report says
they may fail silently there, so Cursor capture is best effort), the real
Claude Code and Codex apps, distributions and systemd versions other than
Ubuntu 24.04 with systemd 255, real R2 and Amazon S3, a real
logout with lingering off, a desktop login, and WSL. A
home directory shared by several Linux machines (NFS, say) is refused by
setup unless you opt in for a home only one machine mounts
([details](multiple-machines.md#a-home-directory-shared-across-machines)).
See [platforms](../getting-started/install.md#platforms), [setup on
Linux](../getting-started/setup.md#setup-on-linux) and [troubleshooting](troubleshooting.md#linux-and-systemd).

**Does it run on Windows?**
No.

**What happens when an app changes its transcript format?**
Nothing is guessed. A field the filter doesn't know is dropped and named in
a capture gap, a record type it doesn't know is dropped whole, and a source
the parser can't read gets parser status `failed` rather than wrong counts.
`status` reports a new app version as `unverified` until one of its sessions
is published and read back. If sessions from a new version look wrong, file
a capture gap issue; a fix comes as a new adapter or filter version, and
sessions whose transcripts are still on your machine are refiltered then.

**Which app versions work?**
See the [tested app versions](../reference/capture-capabilities.md#tested-app-versions). A version
agent-archive hasn't seen is reported as `unverified` rather than assumed to
work.

**Is my R2 bucket verified private?**
An existing or manually configured R2 bucket remains `not_verified`: its
S3-compatible credentials can't inspect public-bucket settings. When guided
setup creates a bucket, its temporary Cloudflare token can check that
`r2.dev` is off and no custom domains are enabled. If both checks succeed,
status shows **R2 public access off at setup** with `verified_private`
evidence. This is a setup-time snapshot, not a continuing guarantee: the
collector's next privacy refresh returns to `not_verified`, and without a
refresh the observation becomes stale after 24 hours. Check public access
in the Cloudflare dashboard; see [bucket privacy evidence](../security/privacy.md#bucket-privacy-evidence).
For S3, setup and the collector inspect Block Public Access, the bucket
policy, and the ACL read-only.

**Why didn't a session show up?**
Check `agent-archive status` first. A connected bucket or `Ready` state does not by itself prove this app captured and read back a session. Follow the app-specific steps for [Claude Code](troubleshooting.md#no-claude-code-session), [Codex](troubleshooting.md#no-codex-session), or [Cursor](troubleshooting.md#no-cursor-session).
