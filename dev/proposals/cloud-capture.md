# Cloud session capture: engineering specification

> **Proposed.** Not implemented or scheduled. See the [documentation index](../../docs/README.md) for what exists today.

Status: proposed plan for review; not scheduled for implementation. Prepared 2026-09-24; revised 2026-10-01. This document describes a target design, not the current implementation. Claude Code on the web facts marked *verified* come from live probes on 2026-09-23 and 2026-09-24; Codex CLI observations below come from a separate CLI launched inside a managed cloud worker on 2026-09-30. They do not verify the hosted conversation. Other vendor-product claims retain their original documentation dates and need rechecking before implementation.

## Purpose

The archive now supports configured macOS and Linux machines; Linux platform support has shipped. Ephemeral cloud capture remains proposed. The [product specification](../specs/archive.md#scope-and-non-goals) says: "Remote and cloud-hosted sessions require a collector in their execution environment and are not automatically covered by a Mac installation." Coding agents increasingly run in vendor-hosted VMs (Claude Code on the web, Codex cloud, Cursor Cloud Agents) or on CI runners. Those sessions are invisible to the archive today.

This specification adds cloud capture for the three harnesses the archive already supports, Claude Code, Codex and Cursor, wherever the vendors make it possible, without weakening the archive's existing guarantees: filtered native records, fresh-start eligibility, content-addressed sources, metadata written last, no secrets on disk or in output, and nothing on the agent's critical path that can block it.

## Why durable local capture does not reach ephemeral workers

| Local assumption | Cloud reality |
| --- | --- |
| Hooks in user-level app settings | A controlled cloud CLI can load runtime user settings. Hosted products must be probed separately; repository hooks verified for Claude web do not establish user-level support |
| A launchd or systemd collector retries later | The VM is reclaimed after the session; nothing runs after the last hook |
| Durable credential storage or an explicit AWS profile | Ephemeral workers need explicitly configured runtime authentication; hosted products may remove setup-phase secrets before agent execution |
| `machine_id` is a durable Mac identity; retention is swept by the registering machine | Each session gets a fresh VM; no machine ever returns to sweep |
| `project_id` hashes the local project root | The cloud checkout path (`/home/user/<repo>`) differs from the Mac path for the same repository |
| Linux builds and installer support have shipped | Select a pinned Linux artifact for the actual architecture; release download/provenance still needs a real-tag acceptance run |

## Capture strategies

Three strategies exist. They differ in fidelity and in where credentials live.

1. **In-environment push.** An environment bootstrap installs runtime hooks before the agent starts, without a repository commit or PR. Repository-committed hooks remain an explicit fallback where a hosted product requires them. The hook runs the `agent-archive` binary inside the cloud VM. At each stop event it filters the native transcript and uploads it synchronously. This preserves full native-record fidelity and reuses the existing adapters. It needs upload credentials inside the VM.
2. **CI-runner push.** The same as in-environment push, on a runner the user controls (GitHub Actions running `claude-code-action` or `codex-action`). Hooks and transcripts are documented there, and secrets are ordinary CI secrets. This is the easiest path and is included in every phase that ships in-environment push.
3. **After-the-fact pull.** The Mac fetches a finished session from a vendor API and archives the result. No credentials enter the VM, but vendor APIs return derived views, not native transcripts, so fidelity is lower and each API needs its own adapter.

Push is the primary strategy. Pull is a fallback only where push is impossible or unreliable.

**Exception: Claude Code on the web (2026-10-13).** Claude Code's session API lists every cloud session and returns the transcript records the VM wrote, so pull keeps native-record fidelity there and needs no hooks or VM credentials. Claude Code cloud capture now starts with pull; see [Claude Code cloud sessions: pull capture](claude-code-cloud-pull.md). Push remains the plan for CI runners and the other vendors.

## Support matrix

| Agent | Hooks run in cloud | Native transcript in VM | Upload path | Credential channel | Pull fallback | Plan |
| --- | --- | --- | --- | --- | --- | --- |
| Claude Code on the web | *Verified*: repo `.claude/settings.json`, single-repository sessions only | *Verified*: `/root/.claude/projects/<cwd-slug>/<session_id>.jsonl` | *Verified* reachable: S3 and R2 through the proxy's TLS tunnel (default Trusted level) | Environment variables, readable by anyone using the environment *Verified* list: `GET /v1/code/sessions`; transcript records through the API `--teleport` uses (undocumented). See [pull capture](claude-code-cloud-pull.md) | Pull first |
| Claude Code in CI (`claude-code-action`, `claude -p`) | *Documented*: repo settings and a `settings` input | *Documented*: `transcript_path` | Runner egress | CI secrets | Not needed | Phase 1 |
| Cursor Cloud Agents | *Documented*: repo `.cursor/hooks.json`; `sessionStart` and `sessionEnd` do not run; hooks skip early read-only turns | *Unknown*; `transcript_path` may be null ("if transcripts disabled") | *Documented*: all egress allowed by default | Runtime Secrets (redacted from transcripts) | *Documented*: v0 `GET /v0/agents/{id}/conversation` (messages only, no tool calls); v1 run stream (retention-limited) | Phase 0 probe, Phase 2 |
| Codex cloud | *Unknown*: hooks documented for the CLI only; project hooks require trust review | *Unknown* | *Documented*: agent-phase internet off by default; an optional GET/HEAD/OPTIONS-only mode would block uploads | Secrets are removed before the agent phase; only plain environment variables remain | *None*: `codex cloud` returns status and diffs, not transcripts | Phase 0 probe, Phase 3 if the probe passes |
| Codex CLI on a controlled cloud worker | *Observed*: user hooks in writable `CODEX_HOME`, CLI 0.159.0-alpha.3; start and prompt only | *Observed*: existing native rollout at start and prompt | Authenticated upload unverified | Explicit runtime credentials; not tested | Not needed | Phase 1 CLI acceptance |
| Codex in CI (`codex-action`) | *Documented*: normal CLI hooks | *Documented*: rollout files under a configurable `codex-home` | Runner egress | CI secrets | Not needed | Phase 1 CLI acceptance |

Other cloud agents, such as GitHub Copilot's cloud agent, Gemini CLI, Google Jules, Devin and Amp, are out of scope. Supporting any of them would mean a new harness adapter, not cloud capture for an existing one.

## Verified behavior: Claude Code on the web

Probes ran as one-off routines on the default Anthropic cloud environment, Claude Code 2.1.281.

- The VM is Ubuntu 24.04 x86_64, running as root with `HOME=/root` and `CLAUDE_CODE_REMOTE=true`. `agent-archive` builds there with Go 1.24 (`linux/amd64`) and `status` runs.
- Repository hooks fired for `SessionStart`, `UserPromptSubmit`, `SubagentStop` and `Stop`. `SessionEnd` could not be observed, because it runs after the last turn.
- `SessionStart` arrived with `source: "startup"` and a transcript that existed with 0 bytes, so the existing fresh-start proof in `provesFreshSessionStart` (`internal/capture`) passes unchanged.
- `transcript_path` was always absolute, and its basename equalled `session_id`. At `Stop` the transcript already contained the final assistant records.
- `SubagentStop` carried `agent_transcript_path` at `…/<session_id>/subagents/agent-<id>.jsonl`, and the file existed. That is the layout the Claude child capture already expects.
- The transcript contains record types the Claude adapter does not allow (`atis-latch`, `attachment`, `last-prompt`, `queue-operation`) and new top-level keys (`wireToolInputs`, `turnOrigin`, `atis`, `classifierBoundary`, `apiBlockIndex`, `queueSkipAttachments`, `rendered`, `sourceToolAssistantUUID`). Today these are dropped with `unknown_record_type` and `unknown_field_omitted` gaps, so nothing fails, but coverage is incomplete.
- The egress proxy passes `*.amazonaws.com` and `<account>.r2.cloudflarestorage.com` through a CONNECT tunnel, so the client sees the provider's own certificate and SigV4 signing works unchanged. An unauthenticated HEAD returns 405 from S3 and 400 from R2. A made-up R2 account hostname fails the TLS handshake everywhere, so a reachability check must use a real account ID.
- Anthropic's own `launcher-settings.json` installs a `Stop` hook; repository hooks run alongside it.

## Codex probe: CLI in a cloud worker, not hosted capture

On 2026-09-30, a managed Linux worker exposed Codex CLI
`0.159.0-alpha.3` with the `hooks` feature enabled. The hosted conversation's
`CODEX_HOME` was read-only and contained no `sessions` or `archived_sessions`.
Installing `hooks.json` there failed with `EROFS`. This is evidence about that
worker, not a universal limitation of Codex Cloud.

A separate CLI used a writable `CODEX_HOME` and a marker-only user hook.
`hooks/list` discovered the handlers and reported them as untrusted. After
explicitly allowing this inspected test hook for that invocation,
`SessionStart` fired with `source: startup`, followed by `UserPromptSubmit`.
Both named an existing native rollout file. No real model service was called;
the run used an intentionally unavailable local endpoint and was terminated.
The later mock-response test could not bind a local socket under the sandbox;
the additional-network request was canceled. `Stop`, `SessionEnd`, final-turn
contents, Agent Archive parsing, upload, and read-back remain unverified.

The probe establishes a viable CLI hook transport, not end-to-end capture.
Changing `CODEX_HOME` in a shell does not redirect a hosted harness that already
started elsewhere. A successful nested CLI must never certify its parent
hosted conversation. Record this distinction in [capability evidence](../../docs/reference/capture-capabilities.md).

## Design

### 1. Reuse shipped Linux support

Reuse the Linux builds, installer, platform locations, and credential support
recorded in [platform abstraction](implemented/platform-abstraction.md) and
[portable setup](implemented/portable-handoff-and-onboarding.md). Do not add a
second OS abstraction or reject ordinary Linux setup. Cloud setup is a separate
ephemeral lifecycle: it must not require a systemd user manager, install a
timer, or change an existing desktop installation. Verify pinned release
installation on both supported Linux architectures and cloud collection against
MinIO before shipping.

### 2. Cloud mode configuration

Cloud mode is selected only by an explicit environment variable, never inferred:

| Variable | Meaning |
| --- | --- |
| `AGENT_ARCHIVE_CLOUD=1` | Enables cloud mode. Without it, the cloud hook exits 0 immediately |
| `AGENT_ARCHIVE_PROVIDER` | `r2` or `s3` |
| `AGENT_ARCHIVE_BUCKET`, `AGENT_ARCHIVE_PREFIX` | Destination. The default prefix is `agent-archive/cloud/` |
| `AGENT_ARCHIVE_R2_ENDPOINT` | `https://<account>.r2.cloudflarestorage.com` |
| `AGENT_ARCHIVE_REGION` | S3 only |
| `AGENT_ARCHIVE_ACCESS_KEY_ID`, `AGENT_ARCHIVE_SECRET_ACCESS_KEY` | Static credentials for either provider |

In cloud mode the configuration is built in memory from these variables; no `config.json` or `setup` is involved. The Mac rule "do not silently use an unrelated environment credential" still holds: cloud mode reads only the `AGENT_ARCHIVE_*` names above, never ambient `AWS_*` credentials, and it never runs on a Mac install's configuration. An S3 user who wants web identity or OIDC (for example Cursor's documented OIDC token) can add it later as a separate, explicit variable.

The gate matters because repository hook files also run on every collaborator's laptop. With `AGENT_ARCHIVE_CLOUD` unset, the committed hook is a no-op, so it neither double-captures a session that the local install already captures nor fails on a machine without the binary.

### 3. Ephemeral capture command

Add an internal entry point, `agent-archive _hook --harness <h> --cloud`, that runtime-installed or explicitly committed hook files call.

- **Start events** register the session exactly as on the Mac (same fresh-start proof, same session index) under `AGENT_ARCHIVE_HOME`, which defaults to `$HOME/.local/share/agent-archive` inside the VM and lives as long as the VM.
- **Stop events** (`Stop`, `SubagentStop`, Cursor `stop` and `afterAgentResponse`, and Codex `Stop` if its probe passes) run a synchronous capture of that one session: filter, build the bundle, upload the source, read it back, then write metadata. This requires a new exported collector function, for example `collector.CaptureSession(ctx, local, store, registrationID, opts)`, wrapping the unexported `processSession` and the request and pending bookkeeping that `Run` does today. The urgent-request path already bypasses the 3-minute rate limit.
- **Retry within the VM.** A capture that fails leaves its `pending/` snapshot; the next stop event retries it first, exactly as the collector does. After the VM is gone there is no retry, so the last stop's success is the durability boundary.
- **Idempotence.** Each stop publishes a snapshot of the whole transcript so far. The source key is content-addressed, so an unchanged transcript uploads nothing new.
- **Never block the agent.** The command always exits 0, never returns a block decision, prints no transcript content or secrets, and writes one short status line to stderr on failure. The cloud hook entry sets a bounded timeout (30 seconds proposed, subject to vendor limits), versus 2 seconds for ordinary local hooks, because cloud orchestration uploads synchronously; start and prompt events stay local and fast. The probe reported a 3-second SessionEnd limit, so do not depend on SessionEnd for the upload drain.
- **No activation gate.** In ordinary local capture, a project must be configured and activated before sessions register. In cloud mode, explicitly selecting cloud capture, installing its hooks before startup, and configuring the destination is the opt-in, so every session in that repository with a proven fresh start is eligible. A resumed or teleported session still fails the fresh-start proof and is not captured.

### 4. Identity and metadata

The metadata schema has `additionalProperties: false`, so cloud fields require `schema_version: 2` of `metadata.schema.json`. Readers must accept versions 1 and 2.

- `execution`: `{ "kind": "local" | "cloud" | "ci", "provider": "claude_code_web" | "cursor_cloud" | "codex_cloud" | "github_actions" | …, "remote_session_id": "…" }`. `remote_session_id` comes from vendor variables such as `CLAUDE_CODE_REMOTE_SESSION_ID`, so a session can be linked back to the vendor UI.
- `machine_id`: in cloud mode, a stable pseudo-machine per provider and environment, for example `cloud-claude_code_web`, never the VM's random identity. This keeps `list` grouping meaningful and makes clear that the machine will not sweep its own sessions.
- `repo_key`: reuse the implemented normalized-origin identity described in the [handoff specification](../specs/handoff.md). Do not introduce a second hashing or normalization rule. Local and cloud sessions for the same origin can group together despite different checkout paths.

`list` and `status` show the execution kind and provider. `show` and `handoff` are unchanged.

### 5. Adapter coverage

- **Claude.** Review each cloud-only record type and key above against the [privacy rules](../../docs/security/privacy.md): allow what carries conversation structure (likely `attachment` with filtered contents), keep bookkeeping types as counted gaps, and bump the filter version. Add sanitized fixtures from a cloud transcript.
- **Cursor.** Reuse the desktop rules: register at the first `beforeSubmitPrompt` when `transcript_path` is null or empty, and adopt the path later. If the Phase 0 probe shows transcripts are disabled on cloud VMs, push capture cannot work and Cursor falls back to pull.
- **Codex.** The controlled CLI hook transport is observed, but parser compatibility with 0.159.0-alpha.3, stop capture, final-turn completeness, and read-back need acceptance tests. Hosted capture still depends on its own Phase 0 probe.

### 6. Credentials and privacy

Cloud credentials are the weakest point of this design and are handled as follows.

- **Separate destination.** Use a separate bucket (preferred for R2, where tokens cannot be narrower than a bucket) or a separate prefix, so a leaked cloud credential cannot read the Mac archive.
- **Least privilege.** The ephemeral path needs `PutObject` and `GetObject` on its prefix: `Get` is needed for the source read-back and the published-metadata check. It does not need `List` or `Delete`. S3 policies can enforce that; R2 Object Read & Write tokens scoped to one bucket are the closest equivalent.
- **Visibility.** Claude Code environment variables are readable by anyone using that environment, and the agent itself can read them, so a prompt-injected agent could misuse them. Documentation must say so plainly and recommend a per-user environment. Cursor Runtime Secrets are redacted from transcripts and are the preferred channel there. Codex removes secrets before the agent phase, so only plain environment variables work.
- **Redaction.** Before filtering, cloud mode replaces any exact occurrence of the `AGENT_ARCHIVE_ACCESS_KEY_ID` and `AGENT_ARCHIVE_SECRET_ACCESS_KEY` values in the transcript with a fixed marker, and it records a `credential_redacted` gap. The general filter remains best effort.
- **Shared environments.** A team environment with one owner's credentials would archive teammates' sessions into that owner's bucket. `cloud provision` (below) warns about this, and `status` on the Mac shows each cloud provider and environment that has published sessions.
- **Supply chain.** The environment bootstrap or optional committed setup script installs a pinned release version and verifies its SHA-256 before running it. It never uses `curl | sh` from a moving URL.

### 7. Retention without a returning machine

Two mechanisms, both driven from the Mac, which has `List` and `Delete`:

1. **Cloud sweep.** A configured Mac (opt-in with a new `cloud_retention` setting) lists sessions under the cloud prefix, reads remote metadata, deletes superseded sources that metadata no longer references once they are past the existing 24-hour grace period, and expires whole sessions after `RetentionDays`. It uses the same metadata-first deletion order as `retention.Sweep`. Remote metadata replaces the local registration as proof of ownership, limited to `execution.kind` of `cloud` or `ci` under the cloud prefix.
2. **Bucket lifecycle rule** as a backstop. Setup guidance recommends an object lifecycle rule on the cloud prefix at `RetentionDays` plus a margin. Both R2 and S3 support lifecycle rules.

Without either mechanism, superseded snapshots accumulate: one full source per stop event. The sweep is required before cloud mode is recommended for regular use.

### 8. Provisioning, environment setup and verification

Setup has two halves: storage provisioning and worker bootstrap. Neither requires a repository PR by default. Runtime-installed configuration is preferred; committed hooks are an opt-in fallback after probing the hosted product. Getting credentials into each vendor's cloud environment can only partly be automated, because most vendors document no API for it. The goal is that a user pastes one block of variables per vendor, once, and never touches it again until the credentials are rotated.

What vendor documentation (read 2026-09-24) says can be configured without a web UI:

| Target | Programmatic? | Mechanism |
| --- | --- | --- |
| R2 bucket | Yes | `wrangler r2 bucket create`, or `POST /accounts/{account_id}/r2/buckets` |
| R2 lifecycle rule | Yes | `wrangler r2 bucket lifecycle add <bucket> … --expire-days N`, or `PUT /accounts/{account_id}/r2/buckets/{bucket}/lifecycle` |
| R2 bucket-scoped credential | Yes, API only (no wrangler command) | `POST /accounts/{account_id}/tokens` with the "Workers R2 Storage Bucket Item Write" permission group on the one bucket; the S3 access key ID is the token `id`, and the secret access key is the SHA-256 of the token `value` |
| R2 short-lived credential | Yes | `POST /accounts/{account_id}/r2/temp-access-credentials`: bucket and optional prefix scope, at most 7 days |
| S3 bucket, IAM user or role | Yes | AWS CLI (`aws s3api`, `aws iam`); OIDC roles for GitHub Actions |
| GitHub Actions secrets | Yes | `gh secret set`, or the Actions secrets REST API |
| Cursor agents launched through the API or SDK | Per launch only | `POST /v1/agents` accepts `envVars` (session-scoped, deleted with the agent) |
| Claude Code hosted environment variables | No | Environment dialog on claude.ai/code; server-managed settings are admin-UI only |
| Cursor saved secrets (agents started from the app) | No | Dashboard only |
| Codex cloud environment variables | No | Codex environment settings only |

The routines API used for the live probes carries an undocumented `environment_variables` field. Anthropic states the routines API has no public token management, so this design does not rely on it.

Commands:

- **`agent-archive cloud provision [--harness claude,cursor,codex] [--ci github]`**, run on a configured macOS or Linux machine; a repository is required only when generating repository files. It is optional; every step can also be done by hand from the printed instructions.
  1. **Storage.** For R2, it asks once for a bootstrap Cloudflare API token with permission to create account API tokens (Account API Tokens: Edit), stores it in the platform credential store under its own reference, and uses it only for `provision` and `rotate`. It creates the cloud bucket if missing (default `agent-archive-cloud`), adds a lifecycle rule at the cloud retention period plus a margin, and mints a bucket-scoped object token. For S3 it creates a bucket-prefix-scoped IAM user and access key with `PutObject` and `GetObject` only, or, for GitHub Actions, an OIDC role with no stored key. It then runs the synthetic write, read-back and delete test.
  2. **Environment bootstrap (default).** Generate a pinned installation command and non-secret configuration for the vendor's environment setup field or a controlled runner. `cloud setup` (proposed below) installs runtime hooks before agent launch. It writes no tracked repository files by default. Report the required setup field and hook trust step, rather than promising one paste for an unverified vendor.
     **Repository fallback (explicit).** A proposed `--repo-hooks` option writes or merges only the selected harness's repository hook configuration plus a pinned bootstrap script. Use owned-entry merge/removal and preserve unrelated handlers. Generated files require review; nothing is committed automatically. Claude web runtime-generated project hooks need a fresh-worker probe before this can be recommended without a commit.
  3. **Environments.** With `--ci github`, it sets the repository secrets through `gh`. For each vendor without an API, it prints one block of `AGENT_ARCHIVE_*` variables in `.env` form and the settings location, and states who can read the values there (for example, anyone using a Claude Code environment, and the agent itself). The secret is printed once, to the terminal, never to a file.
- **`agent-archive cloud rotate`** mints a new scoped credential, updates every target it can update programmatically, prints the paste block for the rest, and deletes the old credential only after the user confirms that the paste is done. A `cloud verify` pass, run in a new cloud session after the paste, confirms that the environment has switched to the new credential.
- **`agent-archive cloud verify`**, run inside a cloud VM (or by the setup script), reports configuration, hook discovery/trust, transcript access, and storage readiness separately. A storage test writes and reads a unique object in a reserved verification prefix; it does not require List or Delete from the worker credential. A lifecycle rule or privileged maintenance client cleans up verification objects. Provisioning may perform a delete with its separate administrative credential. A successful storage test alone never means capture is verified. Errors contain no credential values.

The bootstrap token changes a rule in the [privacy document](../../docs/security/privacy.md): today the tool "does not request another token" beyond object credentials. `provision` asks for one only when the user opts into automated provisioning, keeps it in the platform credential store, never sends it to a cloud environment, and `cloud provision --forget-bootstrap` deletes it.

Short-lived R2 credentials do not remove the paste. Credentials stored in a vendor's environment settings must outlive the 7-day maximum, so those environments get a long-lived, bucket-scoped key. Short-lived credentials fit only where each launch supplies its own: Cursor launches through the API or SDK, CI jobs, and Claude Code self-hosted runners, whose wrapper script can mint per-session credentials.

### 8a. Controlled cloud CLI setup and launch contract

All commands and flags in this section are proposed, not available CLI features.
Default onboarding is one environment bootstrap plus destination authentication;
no repository PR, user-level scheduler, or hosted service is required.

1. Install a pinned, checksum-verified Linux binary before agent startup. Select
   architecture explicitly and preserve the worker's proxy and CA configuration.
2. `agent-archive cloud setup --harness codex --mode cli` validates explicit
   cloud configuration and prepares private run state. Choose a writable,
   absolute `AGENT_ARCHIVE_HOME`. Honor the intended `CODEX_HOME` or
   `CLAUDE_CONFIG_DIR`; do not silently replace a read-only vendor home with an
   empty directory that loses authentication, policy, skills, or other hooks.
   For a controlled CLI, the caller may explicitly supply a writable home and
   configure agent authentication there through its supported mechanism.
3. Plan and atomically merge only owned hook entries into the selected runtime
   configuration. Preserve other hooks and make repeated bootstrap idempotent.
   Report discovery, enabled state, and trust independently. Use the vendor's
   supported trust flow; do not default to `--dangerously-bypass-hook-trust`.
   An explicit automation-only exception needs inspected pinned hook sources
   and applies to that launch, not a hosted parent session.
4. `agent-archive cloud verify --harness codex` checks preflight readiness before
   launch. If configuration is read-only, credentials are absent at runtime,
   or hooks are untrusted, return a specific actionable result. Do not start
   an apparently archived run after a failed preflight.
5. `agent-archive cloud run --harness codex -- <CLI arguments>` launches the
   caller-selected supported CLI with the prepared environment and ordinary
   CLI authentication. Register only a proven fresh start. Hooks stage lifecycle
   evidence; cloud orchestration calls the shared collector for bounded stop
   snapshots. The wrapper never fabricates startup or transcript evidence.
6. On normal CLI exit, drain registered pending work while the worker is alive;
   on interruption, attempt a bounded best-effort drain. Preserve the CLI exit
   status and report upload outcome separately. A missing start registration
   is a capture failure, not permission to import an arbitrary transcript.
   VM termination cannot guarantee a final drain, so successful stop snapshots
   are the durability boundary. No retry is promised after worker destruction.

Verify end-to-end with a disposable real session: start, prompt, tool call,
final assistant response, stop, source upload/read-back, and metadata published
last. Test fresh versus resumed sessions, duplicate hook delivery, upload
failure/retry, timeout, final drain, and cleanup. A nested CLI is identified as
controlled CLI execution even if its host is a vendor cloud worker; it is never
labelled `codex_cloud` solely from its surroundings. Keep a native CLI session
ID separate from any independently proven vendor task ID.

### 8b. Hosted environments without repository changes

Try bootstrap in the provider's environment setup before the hosted agent
starts. Probe which generated settings it actually loads and when it resolves
trust. Claude web's committed project hooks are verified; runtime-generated
project settings and user-level settings are not. Codex hosted configuration
and transcript access remain unverified. Never overwrite vendor-managed policy
or credentials, or create a separate CLI and present it as hosted capture.

If the host requires committed hooks, say so and offer the explicit repository
fallback. If it exposes no native transcript, offer a clearly labelled export
or session report rather than full capture. MCP/skills can provide archive
lookup and report submission without repository edits, but do not by themselves
receive all messages or lifecycle events. An optional ingestion service can
reduce credential onboarding later; it is a separate storage/authentication
design and is not a prerequisite for this direct S3/R2 proposal.

### 9. After-the-fact pull (Phase 3)

`agent-archive import cursor-cloud` fetches finished Cloud Agent conversations with a Cursor API key stored in the platform credential store. Its output is a new adapter (`cursor-cloud-api`) with `source_kind: vendor_api`, so readers never mistake it for a native transcript. It maps the Cursor agent ID to `native_session_id` and deduplicates against pushed sessions only once the mapping between agent ID and conversation ID is verified. The v0 endpoint has no tool calls, and the v1 stream expires, so the import must run periodically from the Mac's collector. Claude Code uses the same pattern with its own design: [pull capture](claude-code-cloud-pull.md).

## Phases

| Phase | Scope | Exit criteria |
| --- | --- | --- |
| 0. Probes | Complete controlled Codex CLI stop/read-back testing; separately probe no-commit bootstrap and hosted hook loading, trust, transcripts, credential lifetime, and authenticated egress for each vendor | Record successes and explicit blockers per execution mode in capability evidence; do not require every hosted vendor to pass before shipping controlled CLI support |
| 1. Controlled CLI and Claude Code | Reuse Linux support; cloud setup/run without systemd or repo changes, cloud-mode configuration, `CaptureSession`, the `--cloud` hook, compatible metadata extension (`execution`, existing `repo_key`, cloud `machine_id`), Claude adapter review, credential redaction, `cloud provision` (R2 storage, environment bootstrap, optional repository files, vendor paste block, GitHub secrets), `cloud verify`, CI documentation for `claude-code-action` | Controlled Codex and Claude CLI runs publish final turns and read back; Claude web is enabled only after its own probe, with subagent verification |
| 2. Cursor, retention and rotation | Cursor push (if the probe passes), the Mac cloud sweep, `cloud rotate`, S3 provisioning | A Cursor Cloud Agent session reads back; superseded cloud sources are swept |
| 3. Codex and pull | Codex cloud push if the probe passes, `codex-action`, the Cursor pull importer | Codex CI reads back; Cursor import reads back with `source_kind: vendor_api` |

## Acceptance criteria

- A controlled cloud CLI needs no repository commit or PR and no systemd manager. Repeated bootstrap preserves foreign settings. Hosted onboarding states its actual setup and trust steps; one-paste support is claimed only after a fresh-worker acceptance run.
- A committed hook in a repository without `AGENT_ARCHIVE_CLOUD` does nothing on any machine, and a Mac with the local install captures each local session exactly once.
- A supported cloud session's final turn is read back with `show` from a configured reader. Probe-only CLI hooks never upgrade hosted capture or installed-version support to verified.
- A resumed or teleported session is not captured, and the diagnostic says why.
- A failed upload never delays the agent beyond the hook timeout, never blocks a stop, and is retried by the next stop in the same VM.
- The committed files, hook output and uploaded objects never contain the credential values.
- With the cloud sweep enabled, no unreferenced cloud source remains more than 24 hours past its supersession, and no cloud session remains past `RetentionDays`.
- Readers handle mixed schema v1 and v2 metadata.
- Worker verification succeeds without List/Delete privileges; its test objects expire through maintenance.
- Configuration readiness, hook execution, parser coverage, and upload/read-back verification remain separate outcomes.

## Open questions

1. Does Claude Code run `SessionEnd` in the cloud before reclaiming the VM, and with what time budget? Stop-based capture does not depend on it, but it would allow a final bookkeeping publish.
2. Can Claude Code's proxy-injected API credentials (Pro and Max) sign S3 or R2 requests? Only header credentials are documented; SigV4 needs the secret itself. If a future version supports it, credentials could leave the VM entirely.
3. Should cloud sessions share the Mac archive's retention period, or have their own?
4. Should `cloud provision` set up one shared cloud credential for all vendors (one token to rotate, several places to paste) or one per vendor (a leak is contained to one vendor)?
5. Is a per-session cloud upload key (for example a presigned URL issued by a small user-owned service) worth the extra infrastructure? It would remove long-lived credentials from the VM but breaks the "no hosted service" principle.

