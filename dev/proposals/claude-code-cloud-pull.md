# Claude Code cloud sessions: pull capture

> **Blocked; do not build.** Kept as a research record. See the [documentation index](../../docs/README.md) for what exists today.

Status: blocked on Anthropic's terms, 2026-10-14. The Claude Code [legal and
compliance](https://code.claude.com/docs/en/legal-and-compliance) page says
"developers may not collect, store, or intermediate Claude.ai credentials or
session tokens — sign-in to a Claude account must complete through
Anthropic's own flow". Every design below reads the Claude Code login and calls
Anthropic's API with it, so agent-archive cannot ship it. Anthropic has enforced
this rule against other third-party tools without notice. Claude Code cloud
capture instead uses an account plugin whose hooks run inside the cloud session;
see [cloud capture](cloud-capture.md#verified-behavior-account-plugins). This
document stays useful if Anthropic offers a supported export, and its API facts
explain what the cloud service stores.

Prepared 2026-10-13 as the Claude Code part of [cloud capture](cloud-capture.md). Facts marked *verified* come
from probes of the owner's account on 2026-10-13 with Claude Code CLI 2.1.287.
Facts marked *from the CLI* were read from that CLI's bundled code. All seven
[pre-build checks](#pre-build-checks) are done.

## Purpose

Claude Code on the web (claude.ai/code, `claude --cloud`, routines) runs
sessions on Anthropic-hosted VMs. They never touch the user's Mac, so the local
collector never sees them. Push capture needs hooks committed to every
repository, or pasted environment variables, and storage credentials inside
the VM. That is too much onboarding for most users.

Pull capture fetches the sessions from the same API that `claude --teleport`
uses, from the machine that already runs agent-archive, with the Claude Code
login that machine already has. For a user who has signed in to the Claude
Code CLI, turning it on is one setup question.

## What the API offers

All endpoints are on `https://api.anthropic.com`, authenticated with the user's
claude.ai OAuth access token (`Authorization: Bearer`, `anthropic-version:
2023-06-01`). None are documented public APIs.

| Purpose | Request | Evidence |
| --- | --- | --- |
| List sessions | `GET /v1/code/sessions?limit=&cursor=` → `{data, next_cursor, resume_token}` | *Verified*: `limit` 1–200; 118 unique sessions in 2 pages at limit 100, newest `last_event_at` first, oldest created 2026-09-14. Works without `x-organization-uuid` |
| One session's metadata | `GET /v1/code/sessions/{id}` → `{response_shape: {…}}` | *Verified*: adds `metadata`, `session_url`, `post_turn_summary` and others to the list row |
| Full event log | `GET /v1/code/sessions/{id}/events?limit=200&sort_order=asc&cursor=` → `{data, next_cursor, resume_cursor}` | *Verified*: the whole session from creation, including subagents. Items are `{event_id, event_type, sequence_num, source, created_at, payload, …}`; payloads are Claude Code stream-JSON messages, not transcript records. One three-day session with 142 subagents was 77,619 events in 389 pages, 193 MB uncompressed, about 3 minutes to fetch |
| Current transcript | `GET /v1/code/sessions/{id}/teleport-events?limit=1000&cursor=` with `x-organization-uuid` | *Verified*: items `{event_id, event_type, created_at, is_compaction, payload}`, where `payload` is a transcript record (`uuid`, `parentUuid`, `isSidechain`, `sessionId`, `message`, …). **Only since the last compaction**, and no subagent records. What `--teleport` resumes from |
| Transcript (legacy) | `GET /v1/session_ingress/session/{id}` → `{loglines}` | *Verified* 404 for current sessions; not used |

List rows include `id`, `title`, `status`, `worker_status`, `environment_kind`,
`config.sources` (the git repository URL), `created_at`, `updated_at`,
`last_event_at`, `user_message_count`, `tags` and `participants`.

**Identifiers.** `cse_X` and `session_X` name the same session (both resolve).
The transcript and event payloads carry a separate UUID, `sessionId` /
`session_id` (for example `c28f5225-…`): the CLI's own session ID inside the
VM, stable across both endpoints and every event. A handful of client-sent
events carry `session_X` instead.

**Two formats.** `/events` is the complete, append-only event log in
stream-JSON form: `type` `user`, `assistant`, `system/<subtype>`, `result`,
`control_*` and others, with `session_id`, `parent_tool_use_id` and, for
subagents, `agent_id`, `subagent_type` and `task_description`. Main-thread
user and assistant payloads share `uuid`s with transcript records.
`teleport-events` is the transcript format agent-archive already parses, but
truncated at the last compaction, so it cannot be the archive source.
`environment_kind` is `anthropic_cloud` for hosted sessions and `bridge` for
local sessions served through Remote Control. The local collector already
captures bridge sessions from their transcript files, so pull capture skips them.

The organization UUID for `x-organization-uuid` is in `~/.claude.json` at
`oauthAccount.organizationUuid`.

`claude --teleport` lists only page one and filters to the current repository
on the client. The desktop app's session list contains only local sessions.

## Login

| User | Where the login is | Plan |
| --- | --- | --- |
| Signed in to the CLI on macOS | Keychain item `Claude Code-credentials`, JSON `claudeAiOauth.accessToken`, `refreshToken`, `expiresAt`, `refreshTokenExpiresAt`, `scopes` (includes `user:sessions:claude_code`) | Read it. *Verified* readable with `/usr/bin/security` from a terminal and from a launchd job, with no prompt |
| Signed in to the CLI on Linux | `~/.claude/.credentials.json` (or under `CLAUDE_CONFIG_DIR`), same JSON | Read it |
| Desktop app only | Encrypted in the app's `config.json` (`oauth:tokenCacheV2`), key in the keychain item `Claude Safe Storage` | Do not read. Setup offers the CLI the app bundles under `~/Library/Application Support/Claude/claude-code/<version>/` to sign in once |
| `claude setup-token` | Long-lived token | Does not work: *verified* 401 `oauth_scope_insufficient` (no `user:sessions:claude_code` scope) |

Rules:

- **Never use the refresh token directly.** If agent-archive refreshed the
  login itself, the new refresh token could replace the CLI's and sign the
  user out of Claude Code. Read the login fresh on every pass, and let the CLI
  refresh it (below).
- **Access tokens expire within hours.** A refreshed token was valid for 8
  hours. A user who works only on claude.ai/code and rarely runs the local CLI
  would otherwise usually have an expired token. `setup-token` cannot fill this
  gap (check 4).
- **Let the CLI refresh its own login.** *From the CLI*: every cloud-session
  call goes through `prepareApiRequest`, which first calls
  `checkAndRefreshOAuthTokenIfNeeded`. That refreshes when the access token is
  within 5 minutes of expiry or already expired, and saves the new login where
  it read it. So when the stored login is expired or about to expire,
  agent-archive runs `claude --teleport cse_00000000000000000000000000
  </dev/null` and reads the login again. *Verified*: that call returns
  "invalid session ID" from the server in about 1.6 seconds with exit status 1,
  shows no prompt, changes no files and uses no model quota. *Verified* live
  (check 7): a login expired for 5.5 hours came back with 8 hours of validity
  after one such call. `claude auth status` does not refresh; it only reads.
- **Refresh tokens expire too, on a fixed date.** *Verified* (check 7): a
  refresh did not extend `refreshTokenExpiresAt` (10.3 days left before and
  after; 10.7 days nine hours earlier). So the login lasts until that date
  however often it is refreshed, and the user must sign in to Claude Code again
  then. `status` warns from 2 days before, and setup records the date.
- **Which CLI to run.** The standalone `claude` on `PATH`, else the newest copy
  the desktop app bundles. The bundled copy reads and writes the same keychain
  item only after the user has signed in with it, which setup arranges.
- **Never store the login** in agent-archive's files, logs or archive.
- **Read without prompts in the background.** agent-archive's keychain reads
  never show UI (`kSecUseAuthenticationUIFail`). Reading with the system
  `security` tool, which is how Claude Code itself reads the item, avoids a
  per-binary keychain grant. Setup does one interactive read first, so any
  macOS prompt appears while the user is present.
- Never decrypt the desktop app's storage, and never run an OAuth flow under
  Claude Code's client identity.

## Design

### 1. Login reader (`internal/claudeauth`)

Returns the CLI login's access token, its expiry, the refresh token's expiry,
the organization UUID and which source it came from. Platform-specific reading
follows the `internal/credentials` layout. A token value never appears in errors
or diagnostics.

When the access token is expired or within 5 minutes of expiry, it runs the
CLI refresh call (bounded at 15 seconds, stdin closed, output discarded) and
reads the login again. It runs the call at most once per pass and backs off for
an hour after a refresh that did not help.

### 2. API client (`internal/claudecloud`)

A small HTTPS client modelled on `internal/cloudflare/client.go`: bounded
timeouts, paged list and transcript readers, typed errors for 401 (login
expired), 403 (forbidden, including device trust), 404 (session gone) and
5xx (retry next pass). Responses are decoded into narrow structs. Unknown
fields are ignored, not passed through.

### 3. Poll step in the collect pass

(The list poll is cheap: one or two pages. Event fetches are not; see
[incremental fetch](#5-source-provider-incremental-fetch-and-filter).)

The scheduled `_collect` pass (`internal/cli/collect.go`) gains a step between
`discovery.Run` and `collector.Run`:

1. If cloud capture is off for this machine, or no usable login is available
   after the refresh step, record the status and return.
2. Page `GET /v1/code/sessions` newest-first until a row's `last_event_at` is
   not newer than the stored high-water mark, with its own time budget (proposed
   20 seconds). Keep `environment_kind == "anthropic_cloud"`.
3. Apply the scope (below). For each admitted session whose `last_event_at`
   changed, create or update its registration.
4. Advance the high-water mark only after every row up to it was handled, so a
   failed pass is retried.

Network or login failures are recorded as status and never fail the pass or
delay local capture. The poll runs at most once per pass and, proposed, no more
often than every 5 minutes.

**Scope.** Cloud sessions run under `/home/user/<repo>`, which never matches an
included project root. Setup offers:

- all cloud sessions (default), the same precedent as Codex's all-projects scope;
- only sessions whose repository matches an included project's `repo_key`.

### 4. Registration

Follows the Cursor database pattern (`internal/backfill/cursordb.go`,
`internal/agents/cursor/source.go`):

| Field | Value |
| --- | --- |
| `SourceKind` | `claude/cloud-api` (accepted by `archive.validateSource` as provider-qualified) |
| `SourceKey` | the API session id (`cse_…`) |
| `SessionKey.NativeID` | the payload `session_id` UUID (the CLI's ID inside the VM), stable across the session |
| `ProjectRoot` | none; `ProjectID` hashes a synthetic `claude-cloud:<repository URL>` |
| `RepoKey` | from the repository URL using the existing normalized-origin rule, not by running git |
| `MachineID` | the polling machine, so read-back verification passes |
| `Origin` | `discovery` for polled sessions, `import` for setup and backfill imports |
| Mutation | `AppendOnly`: the event log only grows; compaction appends a `compact_boundary` event and removes nothing |
| Signature | `last_event_at` plus the stored event cursor and event count |

**One poller per account.** Two machines polling the same account would archive
each session twice under different archive IDs. Setup enables cloud capture on
one machine, and `status` shows which machine polls. A later version can
deduplicate on the API session ID across machines.

### 5. Source provider, incremental fetch and filter

**Source.** `/events` is the only complete source, so it is the archive source.
`teleport-events` is not used for capture.

**Incremental fetch.** A long session's log is hundreds of megabytes, so
re-reading it every pass is not possible. Per session, agent-archive stores the
last `next_cursor` (or `resume_cursor`, once its meaning is known) and fetches
only newer events, ascending. New events are filtered immediately and appended
to a local per-session staging file of filtered records. Raw events are never
written to disk. The collector publishes the staged filtered records as the
session's source snapshot, as it does for local transcripts. A session's first
fetch (imports, full backfill) runs under its own budget and resumes from the
stored cursor across passes. `sequence_num` is not strictly increasing in
ascending order (4 of 77,619 events were out of order), so ordering and
resumption use the cursor and `event_id`, never `sequence_num`.

**Codec and filter.** The payloads are stream-JSON, not transcript records, so
they need a new codec, `claude-cloud-events`, beside the transcript codec, with
its own allowlist and fixtures and a filter version bump:

- keep `user` and `assistant` messages (main thread and subagents), `result`,
  `system/init` (model and version only), `system/compact_boundary`;
- drop as counted gaps: `system/task_progress`, `background_tasks_changed`,
  `rate_limit_event`, `control_*`, `prompt_suggestion`, `env_manager_log`,
  hook events and other bookkeeping (together about a quarter of the volume);
- never keep `system/init` fields that describe the environment
  (`messaging_socket_path`, `scratchpad_path`, `mcp_servers`, `plugins`,
  `apiKeySource`) beyond what the transcript codec already allows.

Because main-thread payloads share `uuid`s with transcript records, readers can
render both formats the same way once the codec maps fields
(`session_id`→`sessionId`, `parent_tool_use_id`, `tool_use_result`).

**Subagents.** Events with `agent_id` belong to a subagent; `parent_tool_use_id`
links them to the parent's Agent tool call. The codec splits them into child
sessions keyed by `agent_id`, matching how local subagent transcripts become
child sessions.

**Size.** Measured on the largest session seen (three days, 142 subagents,
190.7 MB raw) with this section's keep and drop rules: 139.2 MB kept, of which
125.1 MB is subagent traffic. Compressed, the main thread is 3.4 MB, the largest
subagent 1.4 MB and the median subagent 0.23 MB (42.8 MB for all subagents).
With subagents as child sessions, no source needs splitting into parts.
Assistant messages arrive as one event per content block (29,981 events for
16,308 message IDs), the same granularity as transcript records, so no
deduplication is needed.

### 6. Setup and import

- **Setup.** In `chooseCapture`, after the Codex scope question: "Also archive
  your Claude Code cloud sessions?" Setup then reads the login once, showing any
  keychain prompt, and calls the list endpoint as a check. Without a login it
  offers the bundled CLI sign-in.
- **Recent import.** `importRecentSessions` gains a cloud planner for the last 7
  days, written as a normal import batch, so `backfill history` and
  `backfill undo` work.
- **Full history.** `agent-archive backfill --claude-cloud` imports everything the
  list returns.
- **Status.** `status` reports cloud capture state: off, polling machine, last
  successful poll, login expired, sign-in due (from 2 days before the refresh
  token's expiry), or forbidden.

### 7. Readers

`show`, `list` and archive handoff read metadata and bundles, so they work
unchanged. Handoff matches cloud sessions to a project by `repo_key`. Local
native handoff assumes transcript files and does not apply. Metadata gains no
new fields in this version; marking execution kind and provider waits for the
metadata schema 2 work in [cloud capture](cloud-capture.md#4-identity-and-metadata).

## Pre-build checks

Run with [`dev/probes/claude_cloud_checks.py`](../probes/claude_cloud_checks.py),
which doubles as a compatibility check when Claude Code changes. It writes raw
transcripts to `dev/probes/out/`, which git ignores; delete it after use.

| # | Question | Result |
| --- | --- | --- |
| 1 | Does `teleport-events` work with the CLI login, and are its records local-transcript records? | Works (4 of 4 sessions), transcript records, **but only since the last compaction**. `/events` is complete but stream-JSON. Design switched to `/events` |
| 2 | How do `cse_…`, `session_…` and the transcript `sessionId` relate? | `cse_X` = `session_X`; payload `session_id` is a separate stable UUID |
| 3 | Are subagent records in the same stream? | Not in `teleport-events` (0 sidechain records). In `/events`, as `agent_id` + `parent_tool_use_id` (142 subagents in one session) |
| 4 | Does a `claude setup-token` token work for list and transcript calls? | **No.** 401 `oauth_scope_insufficient`: the token lacks `user:sessions:claude_code`. Only a full CLI login works |
| 5 | Can a launchd job read the CLI login without a prompt? | Yes, through `/usr/bin/security` (parent `/sbin/launchd`, list call 200) |
| 6 | How far back does the list go, and which page sizes work? | `limit` 1–200; 118 sessions back to 2026-09-14 (76 cloud, 42 bridge). Whether that is account start or a cap is open |
| 7 | Does `claude --teleport <invalid id>` refresh an expired or nearly expired login, and does the refresh extend `refreshTokenExpiresAt`? | **Yes** for the access token: expired for 5.5 hours, then valid for 8 hours after one call. **No** for the refresh token: its expiry did not move (10.3 days left) |

## Implementation plan

| PR | Scope | Exit criteria |
| --- | --- | --- |
| 1 | This proposal and the probe script | Done: every check has a result |
| 2 | `claudeauth` (CLI login only) and `claudecloud` | Unit tests with recorded responses; no token in any output; expired, forbidden and wrong-scope logins mapped to statuses |
| 3 | Poll step, high-water mark, scope, registration, `repo_key` from URL | A fake API drives registrations; failure leaves the mark unchanged; local capture unaffected by API outage |
| 4 | `claude-cloud-events` codec and filter version, incremental fetch with stored cursors and filtered staging, subagent split | Sanitized recorded event logs filter and publish; a 200 MB log resumes across passes; no raw event reaches disk |
| 5 | Setup question, 7-day import, `backfill --claude-cloud`, `status` | Import batches undo cleanly; one-poller rule enforced |
| 6 | Live acceptance and documentation | On a real account: a new cloud session appears in `list` within one poll, `show` reads its final turn, and an expired login reports without failing capture |

## Risks

- **Terms of service.** Reusing the Claude Code login in another tool may break
  Anthropic's terms. Anthropic has blocked third-party tools that use
  subscription logins. Confirm before shipping, and prefer a supported export if
  Anthropic offers one.
- **Undocumented API.** Endpoints, headers and record formats can change with
  any Claude Code release. Decoding stays narrow, failures become statuses, and
  the probe script doubles as a compatibility check.
- **Device trust.** The CLI sends a trusted-device header on some calls. If the
  server starts requiring it for transcript reads, pull capture stops and
  `status` must say why.
- **Duplicates with push capture.** If push capture is also built, a session
  can arrive both ways. Deduplicate on the API session ID before both ship.

## Open questions

1. What are the list's `resume_token` and `/events`' `resume_cursor` for? Either
   could replace the stored high-water mark or event cursor.
2. Is the 2026-09-14 floor account history or a retention limit? If it is a
   limit, polling must run often enough never to miss a session.
3. Should cloud capture default to all cloud sessions or to included
   repositories only?
4. Would Anthropic accept a supported, non-interactive refresh command
   (`claude auth refresh`) instead of relying on a side effect of `--teleport`?
