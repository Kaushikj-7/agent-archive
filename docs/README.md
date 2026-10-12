# agent-archive documentation

Start with the [README](../README.md) for what agent-archive is. These docs
are for people using it; specs and contributor guides are in
[`dev/`](../dev/README.md).

## First local handoff

After installing v0.2.0, run `agent-archive handoff` inside a project to choose an
existing Claude Code or Codex conversation before configuring storage. See
[scope, bounded previews and cleanup](guides/handoff.md#before-setup-native-local-sessions).

## First successful capture

1. Read [what is uploaded](security/privacy.md#what-is-uploaded) before setup. Filtering and credential redaction are best effort, there is no client-side encryption, and visible user-level `SKILL.md` text can be uploaded even when you include only one project.
2. [Install](getting-started/install.md) a release for macOS or Linux (the [platforms](getting-started/install.md#platforms) section says what each needs and what is verified), then run `agent-archive --version` to confirm the binary starts.
3. [Create a private bucket and configure storage credentials](getting-started/bucket.md), using [narrow bucket permissions](security/bucket-permissions.md). You need a Cloudflare or AWS account for storage; there is no agent-archive account.
4. From a project you want archived, run `agent-archive setup`. Include that project and your app, connect the bucket, and review the [setup choices](getting-started/setup.md). Setup imports the last 7 days of sessions in the included projects ([after setup](getting-started/setup.md#sessions-from-the-last-7-days)); for this new-session check, look for the new session, not an imported one. Older history is available with [backfill](guides/backfill.md).
5. For Codex hook capture, run `/hooks` and approve the archive hooks; supported automatic discovery does not require hook approval. Review [source and scope consent](getting-started/setup.md#automatic-codex-discovery). Start a **new** Codex or Claude Code session in the included project, or a new Cursor Agent chat there, and send a prompt. An already open session does not qualify; [setup explains fresh starts](getting-started/setup.md#after-setup).
6. Allow the background collector to run, or run `agent-archive sync`. Then run `agent-archive status`. A connected bucket, installed hooks, or an overall `Ready` state shows configuration or health; look for your app's **archived, verified** capture row to establish publication and read-back. Use `agent-archive status --verbose` for the per-project evidence. Finally, run `agent-archive list`, copy the new session ID, and run `agent-archive show SESSION_ID` to read its archived metadata. See [reading status](guides/troubleshooting.md#reading-status) and [list/show](guides/list-and-show.md) for detail.

If your app still has no captured session, use its specific steps: [Claude Code](guides/troubleshooting.md#no-claude-code-session), [Codex](guides/troubleshooting.md#no-codex-session), or [Cursor](guides/troubleshooting.md#no-cursor-session).

## Using agent-archive

| Doc | For |
| --- | --- |
| [Install](getting-started/install.md) | Installing a release, or building from source; macOS and Linux requirements and what was tested. |
| [Create a bucket](getting-started/bucket.md) | A private R2 or S3 bucket and an access key, step by step. |
| [Setup](getting-started/setup.md) | Choosing apps and projects, connecting a bucket, and what setup changes. |
| [Uninstall](getting-started/uninstall.md) | Removing hooks, the collector, local data, and the binary; deleting the archive in the bucket; downgrading. |
| [Backfill](guides/backfill.md) | Importing sessions already on your machine, and undoing an import. |
| [Handoff](guides/handoff.md) | Continuing a session in another agent, on this machine or another. |
| [Agent skills](guides/agent-skills.md) | Asking Claude Code, Codex, or Cursor to pull in a past session; what setup installs and what the agent may run. |
| [List, show, and feedback](guides/list-and-show.md) | Inspecting the archive; skill evidence; subagent sessions. |
| [Stats](guides/stats.md) | Usage over time: tokens, estimated cost, agents, models, projects. |
| [Eval export](guides/eval-export.md) | Sessions as JSON Lines for an evaluation tool: commits, counts, prompts in order. |
| [Multiple machines](guides/multiple-machines.md) | Several machines, macOS and Linux, sharing one bucket; Migration Assistant, Time Machine, and cloned Linux machines. |
| [Troubleshooting](guides/troubleshooting.md) | Reading `status`, systemd and Linux problems, recovering an interrupted setup, changing storage, upgrading. |
| [FAQ](guides/faq.md) | Short answers: cost, deleting everything, which versions and platforms work, transcript format changes. |

## Security and privacy

| Doc | For |
| --- | --- |
| [Privacy](security/privacy.md) | Threat model, what is and isn't uploaded, cleaning up after a filter upgrade, what changes on your machine. |
| [Archive cleanup](security/archive-cleanup.md) | Removing older filtered copies and recovering interrupted shell cleanup plans. |
| [Bucket permissions](security/bucket-permissions.md) | Least-privilege S3 policy and R2 token. |
| [SECURITY.md](../SECURITY.md) | Reporting a vulnerability. |

## Reference

| Doc | For |
| --- | --- |
| [CLI reference](reference/cli.md) | Every command, its help, its flags, and the exit codes (generated from the CLI). |
| [Configuration](reference/configuration.md) | `config.json` fields and environment variables. |
| [Bucket layout](reference/bucket-layout.md) | Object keys and how objects change. |
| [Local state](reference/local-state.md) | Every file in the data directory. |
| [Capture capabilities](reference/capture-capabilities.md) | What each app's hooks provide, and the versions observed. |
| [Session eligibility](reference/session-eligibility.md) | Which sessions are captured, and why. |
| [JSON output](reference/json-output.md) | `list --json`, `show --json`, `stats --json`, and `status --json`: the contract for scripts. |
| [Glossary](reference/glossary.md) | The terms agent-archive uses: harness, capture gap, sidecar, admission, parser status, read-back, and more. |
| [JSON schemas](reference/schemas.md) | `metadata.schema.json` and `source-bundle.schema.json`, their $ids, and the tests that validate them. |
