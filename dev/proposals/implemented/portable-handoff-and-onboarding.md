# Portable handoff, lower-friction setup, and Linux: plan and specification

> **Implemented**, with the open items under [Outcome](#outcome-as-built). Part 1 (repo-key handoff matching) and Part 2 (guided bucket creation) shipped in PRs #157, #161, #167, #171 and #184; Part 3 (Linux) shipped through the series that [platform-abstraction.md](platform-abstraction.md) re-planned. Prepared 2026-09-29 from a read of the code at commit 5563546 (no builds or tests were run); the file and line references below are to that commit and the sections are kept as the design record, so where they differ from the code, the code and the Outcome section win.

Three changes that back the product's main claim, "switch computers and coding agents without losing your session":

1. **Repo-based handoff matching**: `handoff --latest` finds the right session on another machine without requiring the same checkout path.
2. **Lower-friction setup**: reach a first working handoff without creating a bucket, and make bucket creation guided when the user wants sync.
3. **Linux support**: full persistent capture, not only the ephemeral cloud mode in [cloud-capture.md](../cloud-capture.md).

They are independent except for a few shared seams, called out under [Sequencing](#sequencing).

## Outcome (as built)

Updated 2026-10-01. What shipped, and where it differs from the plan below.

| Package | PR | Notes |
| --- | --- | --- |
| Plan | #153 | This document. |
| R1: `repo_key` | #161 | Hash of the normalized `origin` remote, never the URL. Parser `0.16.0` (`0.14.0` and `0.15.0` went to the token fields and git activity first). |
| S1: setup trims | #157 | One capture question on a first run from a repository; import offer after the next steps; `storage.Probe` before `VerifyAccess`. A repository that is the home folder or a temporary folder is never pre-selected. |
| S3: guided S3 creation | #167 | After the profile, setup asks which profile archiving should use (the creation profile stays the default); no lifecycle rule; no explicit encryption call. |
| S2: guided R2 creation | #171 | Experimental: offered only with `AGENT_ARCHIVE_EXPERIMENTAL_R2_CREATE=1`. No lifecycle rule. A public r2.dev URL or custom domain stops setup with a menu. |
| R2: handoff matching | #184 | Path matches outrank repository matches; a repository-only match needs confirmation on a terminal and is refused, with no bucket-controlled text, without one. See the threat model in `docs/security/privacy.md`. |
| Linux | see [platform-abstraction.md](platform-abstraction.md) | The Linux series, with the scheduler and OS work re-planned there. |

**Differences from the plan.**

- **Lifecycle rules were dropped** for both providers: a per-object age rule contradicts the collector's retention (it can delete sessions the user meant to keep, or leave metadata pointing at expired sources). See 2a step 7 and 2b step 5.
- **Local-only mode and the folder backend were not built**, as decided in Part 2's scope.
- **Repository-only handoff matches are untrusted.** The plan matched by recency alone. Review found that a cloned repository controls its own `origin`, so path matches always rank first and a repository-only match is confirmed (see `dev/proposals/implemented/handoff.md` and `docs/security/privacy.md`).
- **S4 (acceptance record) was not done as a package.** Docs shipped with each PR; `dev/maintainers/open-source-acceptance.md` does not yet cover guided creation or repository matching.
- **The `AGENT_ARCHIVE_*` printout** from 2c is not done: it waits for cloud capture (see the 2c status).

**Open items.**

- Guided R2 remains gated. The 2026-10-05 live provider run verified default-jurisdiction key derivation, storage operations, bucket scope and independent revocation; it exposed management REST conflict code `10004`, now recognized alongside `10073`. Actual guided CLI acceptance, exact bootstrap permissions, jurisdictions and the remaining checklist in `dev/contributing/testing.md` are still open.
- Guided S3 has never run against real AWS (what `HeadBucket` answers for a free name without list permission, how `GetCallerIdentity` behaves through SSO and assume-role profiles, and the printed runtime policy, which is marked untested).
- Partly applied review fixes: after creating an S3 bucket, a Ctrl-C at a later prompt leaves an unreported empty bucket (the reminder covers only the archiving-profile question), and the R2 and S3 flows keep separate trackers of what they created.
- Sessions captured before `repo_key` have no key until a metadata refresh, so they match by path only.

## Decisions needed from the owner

| # | Decision | Recommendation |
| --- | --- | --- |
| D1 | Store a `repo_key` hash only, or also a readable normalized remote in metadata? | Hash only. Matches `dev/proposals/implemented/handoff.md` phase 2 and keeps `privacy.md`'s "hash, not the path" property. |
| D2 | On Linux, store R2 secrets in a 0600 file? | Yes, with a permission check, and recommend an S3 profile in docs. This changes the "R2 secrets live in Keychain" statement in the privacy docs. |
| D3 | Storage backends beyond S3 and R2 (folder, iCloud, Dropbox, NAS) and a local-only mode? | Decided: no. Keep S3 and R2 only; no local-only mode. |
| D4 | Linux scheduler: systemd `--user` timer first, with what fallback? | systemd first. Fallback for hosts without a user bus (WSL without systemd, containers): defer, but fail with an actionable message. |
| D5 | Unsigned Linux binaries: acceptable trust story? | Yes: mandatory `SHA256SUMS` check in `install.sh` plus a GitHub build attestation, documented. |
| D6 | Guided R2 creation from one pasted Cloudflare API token (bucket, bucket-scoped key)? | Yes: the docs support it end to end (Part 2a). Close the listed unconfirmed items with a live test on a scratch account before shipping. **Implemented (S2, #171) behind `AGENT_ARCHIVE_EXPERIMENTAL_R2_CREATE=1` until that live test is done; the lifecycle rule was dropped from it (see step 7).** |

---

## Part 1. Repo-based handoff matching

### Problem

`ProjectID` is `"project-" + sha256(filepath.Clean(root))[:16]` (`internal/archive/bundle.go:387`). It depends on the absolute checkout path, so a session archived on machine A matches `handoff --latest` on machine B only if the repository is at the identical path. `noMatch` (`internal/cli/handoff.go:~551`) says so. Nothing in the metadata sidecar records anything about git today, and nothing in the repo runs `git`.

### Design

Add an optional **`repo_key`**: a stable identifier for "the same repository", independent of where it is checked out.

**Derivation.** `repo_key = "repo-" + hex(sha256(NormalizeRemoteURL(origin)))[:16]`, computed from `git -C <project root> config --get remote.origin.url`.

`NormalizeRemoteURL` must, before hashing:

- strip userinfo (`user:token@`), so a credential never enters the hash input and the hash does not vary by credential;
- convert scp-style `git@host:owner/repo.git` to the same canonical form as `ssh://git@host/owner/repo.git` and `https://host/owner/repo.git`, so SSH and HTTPS clones of one repo match;
- lower-case the host, drop a trailing `.git` and trailing `/`, and drop the default port;
- return `""` when there is no `origin` (then no `repo_key` is recorded; the `ProjectID` path still works).

Only the hash is stored (D1). The URL itself is never written to the sidecar, registration, or bucket.

**Where it is computed.** Best-effort, short timeout, never fails capture:

1. At hook registration (`internal/capture/hook.go:~487`), store it on the registration as optional `RepoKey string json:"repo_key,omitempty"` (`internal/archive/types.go:~316`). The registration has no git fields today. The hook path must stay fast (2 s budget), so run `git` with a small timeout and skip on failure.
2. At publish and refresh, if the registration has none, compute it from `reg.ProjectRoot` as a fallback (this is how already-registered sessions gain it).

**Metadata.** Add `Metadata.RepoKey` (`json:"repo_key,omitempty"`) beside `ProjectName` (`types.go:~575`), an `ApplyRepoKey` beside `ApplyProjectName` (`internal/archive/metadata.go:451`), and call it in both places `ApplyProjectName` is called (`internal/collector/session.go:596`, `internal/collector/metadata.go:106`). Add it to `schemas/metadata.schema.json` (`additionalProperties: false`, so the schema must change in the same PR). Optional field: no `MetadataSchemaVersion` bump. Bump `DefaultParserVersion` to `0.16.0` (`0.14.0` and `0.15.0` went to the token fields and git activity first) (`internal/archive/adapters.go:67`) so existing sessions refresh and gain it.

Limit: refresh can only add `repo_key` on the machine that owns the registration and still has the repo. Sessions from other machines, or with the repo gone, stay on `ProjectID` matching. That is acceptable: the field matters most for sessions captured from now on.

**Matching.** In `resolveHandoffTarget` (`handoff.go:~148`), compute the current directory's repo key (walk to the git root). Then:

- `localCandidates` (`handoff.go:403`): accept `reg.RepoKey == key` in addition to `sameProject`.
- `archiveHandoffCandidates` (`handoff.go:479`): accept `m.RepoKey == key` in addition to `projectIDs[m.ProjectID]`. The signature changes, so the test at `handoff_test.go:23` changes.
- `ProjectID` matching stays as the fallback.
- **Subdirectory rule.** Today `--latest` matches the project root or a path inside it, not a parent (`sameProject`). Repo-key matching uses the git root of the current directory, so running inside `repo/pkg/x` matches `repo` sessions. Running from a parent directory of several repos does not match. Both preserve the existing "does not match child projects" test (`handoff_test.go:483`).
- **Same repo, different branch.** Rank candidates by recency as today, but print the source branch and the current branch on stderr when they differ ("session was on `feature/x`; you are on `main`"). Do not filter by branch: continuing on another branch is a real use.
- **Forks and multiple remotes.** Only `origin` is used. A fork's `origin` differs from upstream's, so they do not match. Note this in docs; no `upstream` fallback in this version.

**Workspace difference reporting.** Extend `HandoffWorkspace` (`internal/archive/handoff.go:110`, `handoff_render.go:90-97`) to include the source branch and, when known, tell the receiving agent that the recorded directory differs from its own checkout ("checked out elsewhere; check paths against the current tree"). Uncommitted changes still do not travel; that is `--worktree`/carry-changes work in handoff v2 and is out of scope here.

**`--to` with archived sessions.** `--to` currently forces `--source local` and rejects `--source archive` (`handoff_options.go:83-85`, `:118`). Cross-machine launch needs this relaxed; handoff v2 already plans to drop both restrictions (`dev/proposals/implemented/handoff.md:~575`). Do that in the same change as matching, or first.

**`noMatch` text.** Replace the "depends on the checkout path" explanation with one that says what was tried (repo key, then path), and keep listing the five most recent sessions.

### Privacy

- The hash is derived from the normalized remote. For public repos it is guessable (anyone can hash a known URL), which is acceptable; for private repos the URL is not public. Say in `privacy.md` that `repo_key` lets someone with bucket read access test whether a known repository URL was used.
- Update `docs/security/privacy.md` (lines ~64 and ~92, which say the sidecar holds only a path hash) and `dev/specs/archive.md` (~249, ~309).

### Tests

- Normalizer table: `https` with `user:token@`, scp-style, `ssh://`, `.git` suffix, mixed case, port, no remote, garbage.
- `RepoKey` of an SSH clone and an HTTPS clone of the same repo are equal.
- Candidate selection with two machines' project IDs but the same repo key.
- Collector refresh: an old sidecar gains `repo_key` after the parser bump; unchanged sidecars are not republished.
- Hook: `git` missing, `git` slow (timeout), not a repo. Capture still succeeds and registers with no key.
- Schema test.

### Docs

`dev/proposals/implemented/handoff.md` (selection, "Known limit", "Decisions: No git in v1"), `docs/guides/handoff.md`, `docs/guides/multiple-macs.md`, `docs/reference/cli.md`, `handoff_options.go` help text, `bucket-layout.md`, `schemas.md`, `dev/maintainers/versions.md`, `CHANGELOG.md`.

### Work: one PR, about 400 lines plus tests and docs

---

## Part 2. Lower-friction setup: guided bucket creation

### Scope

Storage stays S3 and R2 only. A local-only mode (setup with no bucket) and a folder backend (iCloud, Dropbox, NAS) were considered and are **not planned**: local-only adds little beyond what `handoff` already does on one machine, and cloud capture ([cloud-capture.md](../cloud-capture.md)) matters more. So this part is about removing the dashboard work from setup for both providers, and trimming setup's remaining steps.

### Problem

First value requires creating a bucket, creating scoped credentials, copying an account ID, then answering the setup steps. Bucket creation is entirely manual (`docs/getting-started/bucket.md`), and the repo has no bucket-creation code and no Cloudflare API client. The `help` text in setup (`setup.go:975-983`) repeats the manual steps.

### Principles

- **Admin credentials are bootstrap-only.** The user supplies a credential that can create resources; setup uses it in memory, then discards it. It is never written to disk, config, Keychain, or logs, and never sent to the collector environment. The stored credential is always the least-privilege runtime credential.
- **Object credentials never go to a management API** (existing rule, `internal/storage/privacy.go:50`). The bootstrap token and the runtime credentials are different values used against different endpoints.
- **`VerifyAccess` stays the authoritative gate** (`storage.VerifyAccess`, `storage.go:120`): write, read, list, delete, read-missing. Creation is only complete when the runtime credential passes it.
- **Opt-in.** Guided creation is a menu choice next to "use an existing bucket". Existing flows and `setup --yes` keep working unchanged.

### 2a. R2: create bucket and bucket-scoped key from one API token

Verified against Cloudflare's docs on 2026-09-29 (sources listed below). It is feasible; it is not a single call. The user pastes one **bootstrap API token** and setup does everything else.

**Bootstrap token.** The user creates it once in the Cloudflare dashboard (setup opens a deep link to the token page and prints the exact permissions):

- `Workers R2 Storage Write` (account scope): creates the bucket. (The plan also had it set a lifecycle rule for retention; that step was dropped, see step 7.)
- `Account API Tokens Write` (account scope): mints the runtime token.

An account-owned token is preferred over a user-owned token: it is a durable service principal, has a higher limit (500 per account versus 50 per user), and R2 is supported for account tokens. A user-owned token can mint tokens only with the `API Tokens Write` permission from the "Create additional tokens" template. Members can grant only a subset of their own permissions, so a member who is not a Super Administrator may be refused; surface that as an actionable error. Also accept `CLOUDFLARE_API_TOKEN` and `CLOUDFLARE_ACCOUNT_ID` from the environment, matching wrangler's convention, so users who already have them set skip pasting. Do not reuse `wrangler login` OAuth state: its file format is not a documented interface and its scopes are not known to include token creation.

**Steps** (all against `https://api.cloudflare.com/client/v4`, `Authorization: Bearer <bootstrap token>`):

1. **Account ID.** `GET /accounts`. The docs do not list the token permission this needs, so treat failure as "ask the user for the account ID" (32-hex, already parsed by `ParseR2Location`, `credentials.go:191`).
2. **Bucket.** `POST /accounts/{account}/r2/buckets` with `{"name": "<name>"}`. Optional `locationHint` (`apac`, `eeur`, `enam`, `weur`, `wnam`, `oc`) and jurisdiction through the `cf-r2-jurisdiction` header (`default`, `eu`, `us`, `fedramp`, `fedramp-high`). Names are lowercase letters, digits, and hyphens, 3 to 63 characters, no leading or trailing hyphen. Default name `agent-archive-<6 random hex>`; a name collision inside the account returns an error, so retry once with a new suffix, and if the user picked the name, ask for another.
3. **Permission group ID.** `GET /accounts/{account}/tokens/permission_groups?name=Workers%20R2%20Storage%20Bucket%20Item%20Write` (paginated; filter by name and check `is_selectable`). **Look the ID up at runtime, never hardcode it**: the docs say the name is cosmetic and the `id` is the stable key, and they publish an ID only for the read group.
4. **Runtime token.** `POST /accounts/{account}/tokens` with one policy: resource `com.cloudflare.edge.r2.bucket.<ACCOUNT>_<JURISDICTION>_<BUCKET>` mapped to `*` (`JURISDICTION` is `default` unless the bucket was created in one), permission group = the Bucket Item Write ID. Name it `agent-archive <bucket> <machine-short-id>`. Do not set `expires_on`, since expiry would silently kill capture; the doc does not confirm the default when omitted, so verify with a real call. This token can read, write, and list objects in that one bucket and nothing else. It cannot manage the bucket, and it cannot reach other buckets.
5. **Derive S3 keys.** Access Key ID = the token's `result.id`. Secret Access Key = SHA-256 of `result.value`. The value is returned only once (Cloudflare marks it show-once), so setup must save it immediately or roll the token back. **The hash encoding (lowercase hex of the UTF-8 bytes) is not spelled out in the docs**; the plan is to implement hex and confirm it with a live `VerifyAccess` in the acceptance test, treating a mismatch as a blocker.
6. **Store and verify.** Save the derived key pair through the credential store exactly as a pasted key pair is saved today (Keychain on macOS, see Part 3 for Linux), then run the existing storage check. Endpoint is `https://<ACCOUNT>.r2.cloudflarestorage.com` (jurisdictional buckets require `<ACCOUNT>.<jurisdiction>.r2.cloudflarestorage.com` and work only there, so the config must record it: `R2Endpoint` already exists).
7. **Retention. Dropped in the implementation (S2, #171): setup sets no lifecycle rule.** A per-object age rule contradicts the retention backstop in `docs/getting-started/uninstall.md` (it deletes sessions the person meant to keep after retention is raised, or on a second machine with longer retention, and can leave live metadata pointing at expired sources), and the S3 flow sets none either; the docs point at the uninstall guide's backstop instead. The original plan follows for the record. Set a lifecycle rule with the bootstrap token so the user's `retention_days` is enforced server-side as well as by the collector: `PUT /accounts/{account}/r2/buckets/{bucket}/lifecycle` with a delete-after-age rule (`maxAge` in seconds, empty prefix). The runtime token cannot do this. This is optional and additive; the collector's own retention sweep is unchanged. Note the PUT replaces all rules, which is safe on a bucket setup just created and must not be run against a pre-existing bucket.
8. **Privacy evidence.** R2 privacy is `not_verified` today because object credentials cannot inspect public-access state. With the bootstrap token, setup can check it: `GET /accounts/{account}/r2/buckets/{bucket}/domains/managed` returns `enabled` for the r2.dev public URL. A fresh bucket is private by default per the docs, so this is confirmation, not a fix. Custom domains are a separate public path; the docs read did not confirm the list endpoint, so say "r2.dev public access: off" rather than "private" unless the custom-domain check is also implemented. Record only what was actually checked. If the setup-time result is stored, mark it as observed-at-setup, not continuously verified.
9. **Discard.** Zero the bootstrap token from memory. Nothing about it is persisted, including in the setup draft (`setup-draft.json` is saved after each step, so keep the token out of the draft struct) and the setup journal.

**Failure handling.** Creation is not atomic across steps 2 through 6. Order the work so leftovers are minimal and report them:

- If the token is created but the key fails verification, delete the token (`DELETE /accounts/{account}/tokens/{id}`; endpoint to confirm) or tell the user which token name to revoke.
- If the bucket was created but the token step failed, keep the bucket, say so, and let the user retry from step 3 with the same bucket.
- A crash between "token created" and "key saved" loses the show-once value; the recovery is to revoke the orphan token by its recognizable name. Print that name before step 4.
- Respect the 1200-requests-per-5-minutes limit (the flow uses under ten) and honor `retry-after` on 429.

**Unconfirmed items to close before building** (each is a small live test against a scratch Cloudflare account, not a design question):

- the exact secret-derivation encoding (step 5);
- the token permission `GET /accounts` needs, and `GET .../domains/managed` needs;
- default expiry when `expires_on` is omitted, and the token-delete endpoint;
- whether the runtime token can be limited by key prefix (the docs describe bucket-level scope only; treat prefix scoping as unavailable);
- whether `GET /user/tokens/verify` works for account-owned tokens (avoid depending on it; a failed `GET /accounts` already validates the token).

### 2b. S3: create bucket in the user's own AWS account

Uses the AWS SDK v2 already linked in the binary and the profile picker that exists (`setup_aws.go:251`).

1. After the profile is chosen, if no suitable bucket exists, offer "Create a new private bucket" with a default name `agent-archive-<random suffix>` (S3 names are global, so collisions are likely without a suffix).
2. `CreateBucket` (with `LocationConstraint` outside `us-east-1`), then `PutPublicAccessBlock` with all four flags true, then optionally `PutBucketEncryption`. If `PutPublicAccessBlock` fails after the bucket was created, do not proceed to uploads: report the bucket and offer to retry or delete it (the bucket is empty).
3. Run the existing `InspectPrivacy` (`cli/privacy.go`, `storage/privacy.go`) so the result screen can show a verified "Block Public Access is on" row. This is stronger than R2's setup-time check.
4. **Permissions.** The published least-privilege runtime policy deliberately lacks `s3:CreateBucket` and `s3:PutBucketPublicAccessBlock`. So this needs a profile with those actions, used at setup time only. Setup then prints the runtime policy from [bucket permissions](../../../docs/security/bucket-permissions.md) for the user to attach to a runtime identity; it does **not** create IAM users or keys. If the chosen profile is itself the runtime profile and cannot create buckets, say so and fall back to the existing "pick an existing bucket" flow.
5. Retention (dropped, like the R2 lifecycle rule in 2a step 7: it contradicts the backstop docs): optionally set an S3 lifecycle expiration rule via `PutBucketLifecycleConfiguration` with the same caveat as R2 (only on the bucket setup just created; it replaces existing rules).

This differs from R2 in one important way: S3 setup does not mint a new scoped credential. It reuses the user's profile. That is simpler and safer to build, but the user's profile is usually broader than least privilege; document a recommendation to use a separate runtime profile.

### 2c. Trim the rest of setup

Independent of creation, these are sequencing changes over existing defaults:

- One confirmation for detected apps, current repository pre-selected (already true when run inside a git repo), retention defaulted to 90 days (already the default).
- Move the "import past sessions" offer (`offerSetupImport`) after the first successful capture check, not straight after commit.
- Replace the manual `help` text in `promptStorage` (`setup.go:975-983`) with the creation menu, and keep `docs/getting-started/bucket.md` as the manual path.
- Validate a pasted R2 key pair immediately with a cheap `ListObjectsV2` (max 1) before the full round trip, so a wrong account ID or key fails in seconds with the existing diagnosis (`storage/diagnose.go`).
- Fix the cloud-mode overlap early: cloud mode ([cloud-capture.md](../cloud-capture.md)) configures storage by environment variables and never runs `setup`. Guided creation produces exactly the values that mode needs (bucket, endpoint, key pair). After creation, print the `AGENT_ARCHIVE_*` variables for a cloud environment, next to the existing "set up another Mac" command (`printNextSteps`, `setup.go:783-839`), and offer to make a **separate** bucket-scoped token for cloud use rather than reuse the workstation's key (a cloud VM's environment variables are readable by anyone with access to the environment, per the cloud proposal). The R2 flow can mint that second token with the same bootstrap token in one extra call.

**Status (package S1, implemented):**

- Detected apps and the current repository are one question on a first setup run from a Git repository ("Archive Codex, Claude Code, and Cursor sessions in ~/src/app?"); a no falls back to the separate app and project questions, and the review step's "Edit a setting" still changes apps, projects, and retention. Retention was already silent (90 days), so it is unchanged. The answer says how many other known projects were found, and the review repeats the "Edit a setting" hint. A repository that is the home folder, holds it, or is one of backfill's temporary folders (`Env.backfillTempDirs`: `DefaultTempDirs` and `$TMPDIR`) or holds one (`broadFolder`, `currentProject`) is never pre-selected, in the combined question or the project list, and setup prints one line saying so. A repository inside a temporary folder stays an ordinary project, as backfill treats it: only the folder itself is too broad to take on one Enter.
- The past-sessions offer now follows the next steps ("Check progress with `agent-archive status`"), not the "Configuration saved." line. It stays skippable, and `setup --yes` is unchanged. It is not tied to a first successful capture check: at that moment no session has been captured yet, so nothing there could be checked.
- The manual storage help is two lines pointing at `docs/getting-started/bucket.md`; `guidedStorageOptions` (`setup.go`) is the slot where "Create a new bucket for me" goes (S2, S3).
- `storage.Probe` (one `ListObjectsV2` with max keys 1 under the `.setup-test/` folder, no writes) runs before `VerifyAccess` in `verifyStorage`, so it covers R2 keys, S3 profiles, and `setup --yes` alike. It runs at the storage check, straight after the key is entered, not inside the key prompt: the pasted secret is staged in the credential store first and the store reads it from there.
- **Not done: the `AGENT_ARCHIVE_*` printout.** Cloud mode is not implemented (`AGENT_ARCHIVE_CLOUD` appears nowhere in `internal/`), so printing those variables would describe a feature that does not exist. TODO when cloud mode ships: in `printAnotherMachine` (`setup.go`), print the variables named in [cloud-capture.md](../cloud-capture.md) "Cloud mode configuration" for the configured provider, bucket, prefix, endpoint, and region, with `<access key id>` and `<secret access key>` placeholders (never the values), and one line saying that a cloud environment's variables are readable by anyone with access to it, so it needs a separate bucket-scoped key.

### Interaction with `--yes`

`setup --yes` keeps its current contract (existing bucket and credentials supplied). Guided creation is interactive only in the first version. A non-interactive creation path (`--create-bucket` reading `CLOUDFLARE_API_TOKEN` or an AWS profile) can follow once the interactive flow has proven the steps, since a script that creates cloud resources deserves an explicit design of its own.

### Tests

- Cloudflare client behind an interface, tested against an `httptest` server: happy path, name collision, permission group not found or not selectable, token created but key derivation fails verification (rollback), 429 with `retry-after`, missing permissions (each step's 403 mapped to a message naming the permission to add), show-once value lost on crash (recovery text printed before step 4).
- Assertion that the bootstrap token never appears in the setup draft, journal, config, environment passed to the collector, logs, or diagnostics output (grep-style test over everything written to the temp home).
- S3: `CreateBucket` fake with `us-east-1` versus other regions, block-public-access failure after creation.
- A **live acceptance step** (documented in `dev/contributing/testing.md` and the open-source acceptance record) against a scratch Cloudflare account and a scratch AWS account, since fakes cannot confirm the unconfirmed items above.

### Docs

`docs/getting-started/bucket.md` (add the guided path, keep the manual one), `docs/getting-started/setup.md`, `docs/security/privacy.md` (what the bootstrap token can do and that it is discarded; what "checked at setup" means), `docs/security/bucket-permissions.md` (the create-time-only permissions), `docs/reference/cli.md`, `CHANGELOG.md`.

### Sources (Cloudflare, read 2026-09-29)

- R2 API tokens and S3 credential derivation: developers.cloudflare.com/r2/api/tokens/
- Create bucket API: developers.cloudflare.com/api/resources/r2/subresources/buckets/methods/create/
- Bucket naming and privacy defaults: developers.cloudflare.com/r2/buckets/create-buckets/ and .../public-buckets/
- Account token create and permission groups: developers.cloudflare.com/api/resources/accounts/subresources/tokens/
- Account versus user tokens and member limits: developers.cloudflare.com/fundamentals/api/get-started/account-owned-tokens/
- Managed (r2.dev) domain status: developers.cloudflare.com/api/resources/r2/subresources/buckets/subresources/domains/subresources/managed/methods/list/
- Lifecycle rules: developers.cloudflare.com/r2/buckets/object-lifecycles/
- Rate limits: developers.cloudflare.com/fundamentals/api/reference/limits/

### Work: about 4 PRs

1. Setup trims and immediate credential validation (2c), including the cloud-variables printout.
2. Cloudflare client plus the R2 guided flow (2a), behind an interface, with the live acceptance step.
3. S3 guided flow (2b).
4. Docs and the acceptance-record update.

---

## Part 3. Linux support (persistent capture)

> **Update 2026-09-30.** PRs 5 to 7 below (scheduler refactor, systemd backend, terminology) are re-planned around one scheduler port and one OS value in [platform-abstraction.md](platform-abstraction.md), which supersedes sections 3c to 3e and the PR list here. PRs 1 to 4 stand.

### Scope

Persistent capture on a Linux workstation or server: hooks, a scheduled collector, credentials, Cursor paths, install. This is distinct from [cloud-capture.md](../cloud-capture.md), which designs ephemeral cloud VMs with env-var configuration and, for Linux, makes `setup` an error. The two share the Linux build, checksums, installer, and credential abstraction; they diverge on scheduler, config source, and `setup`. Resolve the conflict by gating `setup` on "a working scheduler exists", not on `runtime.GOOS`, and on `AGENT_ARCHIVE_CLOUD` for cloud mode.

### What already works

The capture core is portable: hooks, collector, adapters, storage, the S3 client. `flock` (`internal/local/files.go:311`, `internal/cursorstore/snapshots.go`), `syscall.Kill(pid, 0)`, and pure-Go SQLite (`modernc.org/sqlite`) all work on Linux. The default data directory `~/.local/share/agent-archive` is already XDG-shaped. Claude Code (`~/.claude`), Codex (`~/.codex`), and Cursor hooks (`~/.cursor`) use the same paths as on macOS. Handoff launching (`exec.LookPath`) and the pager are portable.

### What is macOS-coupled

1. **Scheduler**: launchd and `launchctl` through `setup`, `status`, `uninstall`, and `setupjournal`.
2. **Credentials**: Keychain is the only R2 store.
3. **Paths**: Cursor's `state.vscdb` and `workspaceStorage`, plus a few macOS-desktop-app locations used by backfill.
4. **Release and install**: macOS-only signing, `install.sh`, and the build script.
5. **Wording**: "Mac" is pervasive in strings, docs, and golden files.

### 3a. Release artifacts and installer

- `scripts/build-release.sh`: add `GOOS=linux GOARCH={amd64,arm64} CGO_ENABLED=0` (the Keychain stub is used, so no cgo), and a portable checksum command (`shasum` is not on all Linux images). Artifact names `agent-archive-linux-{amd64,arm64}`.
- `.github/workflows/release.yml`: an `ubuntu-24.04` build job (native run of the amd64 binary's version check; arm64 checked with `strings`, as darwin/amd64 is today). In `publish`, include the Linux files in `SHA256SUMS`, the attestation subject list, the artifact upload, and the explicit `gh release create` asset list. Apple signing and notarization stay darwin-only.
- `install.sh`: detect `uname -s`; choose asset by OS and arch; use `sha256sum` when `shasum` is absent; skip the `codesign` check on Linux with a clear message that trust rests on the mandatory checksum plus the release attestation (`gh attestation verify`), and never fall back to running an unverified binary. `scripts/install-from-source.sh` needs the same OS awareness.
- `scripts/test_install.py`: replace `test_rejects_non_macos` with Linux success and unsupported-arch cases (the tests already use `uname` shims).
- CI: add a `GOOS=linux GOARCH=arm64` build and a `GOOS=darwin` vet so cross-platform breakage is caught. The lint job is macOS-only today; new Linux-tagged files need a Linux deadcode run or an exception list.
- Risks: the release workflow cannot be exercised without a tag, so use a dry run on a fork or `workflow_dispatch`; the attestation subject list must match the released assets exactly.

### 3b. Cursor paths and backfill

- `cursorstore.StateDatabase(home)` (`internal/cursorstore/cursorstore.go:29`) and `backfill.cursorWorkspaceStorage` (`internal/backfill/resolve.go:437`) switch on OS: macOS keeps `~/Library/Application Support/Cursor/...`; Linux uses `$XDG_CONFIG_HOME` (default `~/.config`)`/Cursor/User/...`. One `appSupportDir(home, getenv, goos)` helper, with `goos` injected so both branches are unit-tested on any OS (the pattern `userTempDir` already uses). **Confirm the Linux path on a real Cursor install**; it is the standard VS Code layout but was not verified.
- Gate macOS-only backfill inputs: `privacyProtectedFolders` (TCC), `~/Library/Application Support/Claude/scratch-workspaces`, `~/Documents/Codex`, and the `/Applications/*.app` capability probes.
- Backfill birth time: `birthtime_other.go` falls back to mtime. Accept that for now, or use `statx`; document the ordering difference.
- Do not set `PrivateTmp` on the collector unit: the Cursor snapshot root is under the user temp directory and the sweep and the collector must see the same one (`snapshots.go:50-58`).

### 3c. Credential store abstraction

- `credentials.CredentialStore` (`credentials.go:47`) is already an interface with `Save`, `Load`, `Delete`, injected through `Env.Keychain` (`cli.go:169`). Add a platform-neutral opener and rename `Env.Keychain`/`openKeychain` to a neutral name. Store-neutral wording is a prerequisite: `setup_preflight.go` (its "release build ... can open the Keychain" hint must not appear on Linux), `setup.go`, `setup_flags.go`, `uninstall.go:262,304`, `storage/diagnose.go:159-173`, and `credentials/keychain_errors.go`.
- Linux stores, in order: (1) **S3 with an AWS profile**: already works; the collector environment capture (`collector_env.go`) carries over to the unit; (2) **0600 file store** at `$AGENT_ARCHIVE_HOME/credentials/<ref>.json`, refusing group- or world-readable modes at load (D2); (3) **environment variables** (`AGENT_ARCHIVE_R2_ACCESS_KEY_ID`/`..._SECRET_ACCESS_KEY`, already used by `setup --yes`), read-only, natural for containers and `EnvironmentFile=`; (4) libsecret later. A secret-service store is a poor fit for a background job on headless hosts.
- Keep the darwin Keychain store byte-compatible: service name and JSON tags are pinned and existing items must stay readable.
- This is the security-sensitive change: it alters the privacy doc's "secrets not on disk" claim and needs the explicit decision D2.

### 3d. Scheduler abstraction and systemd backend

**Refactor first (no behavior change).** Introduce a `scheduler` interface in front of `env_defaults.go` (`launchctl` calls, job-state parsing), `install_paths.go` (label, plist path), `hooks.LaunchAgent*` (`internal/hooks/install.go:465-650`, which also parses the plist for program and environment that `status` reads), `setupjournal.Launchd` (`internal/setupjournal/launchd.go`, already an interface but with plist/label vocabulary), and the call sites in `setup_transaction.go`, `setup_preflight.go`, `status.go`, `uninstall.go`. The macOS backend wraps the existing code. About 12 production files and 9 tests are involved; the interface seam `Env.JobState`/`LoadLaunchAgent`/`UnloadLaunchAgent` exists but the plist-parsing helpers are outside it.

**Then the systemd user backend.**

- Pure function generating `agent-archive-collector-<12hex>.service` (`Type=oneshot`, `ExecStart=<exe> _collect`, `Environment=AGENT_ARCHIVE_HOME=...`, plus the same environment map `collector_env.go` builds, with systemd's own default PATH instead of `launchdPath`) and a `.timer` (`OnBootSec`, `OnUnitActiveSec=60s`, `AccuracySec=1s`, `Persistent`). Overlap is already handled by the collector flock.
- Log to `collector.log`/`collector-error.log` via `StandardOutput=append:` (systemd 240+) so `status` log paths stay valid.
- Ownership check analogous to the plist: unit file path via `systemctl --user show -p FragmentPath`, so "another installation" detection still works.
- Job state via `systemctl --user`. It needs a user session bus (`XDG_RUNTIME_DIR`); over SSH, `su`, containers, and WSL without systemd it fails. Then setup should stop with an actionable message (enable lingering with `loginctl enable-linger`, or use the fallback), not shell out blindly.
- Fallback for no user bus (cron with a marker comment, or a hook-triggered detached `_collect`): defer (D4). The hook-triggered option would be the first place hooks spawn a process and needs care to never block the agent.
- `setup` preflight's "Background job" check becomes scheduler-generic. Tests drive the backend through a fake; CI containers have no real systemd.
- This is the largest and riskiest PR. The setup journal's recovery, relabel, and legacy-migration logic must behave exactly as before on macOS. Consider splitting journal changes into their own PR.

### 3e. Terminology and docs

- Rename `docs/guides/multiple-macs.md` to `multiple-machines.md` and update every inbound link in the same PR (`internal/doclinks` fails on broken links; `TestCLIReferenceIsCurrent` fails on a stale `cli.md`).
- Replace "Mac" with "machine" where the content is not macOS-specific: about 67 occurrences in Go strings (`help.go`, `config.go`, `retention/clock.go`, `setup.go`, `status.go`, `purge.go`, and others), about 129 lines in docs, plus 43 golden files and 19 test files. Keep macOS-specific content (Keychain, Time Machine, Migration Assistant, TCC). This is not safe as a blind find-and-replace.
- `machine_id` is a random opaque ID, so no schema change is needed. New Linux hazard: cloning a VM or container image copies the data directory and therefore the ID, so two live machines believe they own the same sessions. Add a Linux equivalent of the Migration Assistant warning and consider a "regenerate machine ID" step.
- Update the FAQ ("Does it run on Linux? No"), `README.md`, `CONTRIBUTING.md`, and `dev/specs/archive.md` scope only after 3c and 3d have shipped, so the docs never advertise Linux capture before it works end to end.

### Linux PR list

1. Release artifacts and CI (3a, part 1)
2. `install.sh` and installer tests (3a, part 2)
3. Cursor paths and backfill gating (3b)
4. Credential store abstraction and neutral wording (3c)
5. Scheduler refactor, no behavior change (3d-i)
6. systemd backend (3d-ii)
7. Terminology and "Linux supported" docs (3e)

---

## Sequencing

Value order for the launch story: **Part 1**, then **Part 2** (guided creation), then **Part 3**, with the cloud-capture work ([cloud-capture.md](../cloud-capture.md)) able to start after Linux PRs 1-2.

Dependencies and shared seams:

- Part 1 is self-contained and unblocks the handoff demo. It also lays the `repo_key` that `cloud-capture.md` schema v2 already wants (`repo_key` in the cloud design), so implement it once and reuse it there.
- The cloud-capture Phase 1 needs Linux PRs 1-2 only; it does not need 3c-3e.
- 3c's store-neutral wording and 3e's terminology pass both edit `setup.go` and `setup_flags.go` strings. Do 3c's wording first, and land 3e quickly after agreement to limit rebase conflicts.
- Guided R2 creation (2a) mints a key pair that must be stored through the credential store, so on Linux it depends on the 3c credential abstraction. On macOS it works with the Keychain as is. Ship it on macOS first; enable it on Linux after 3c. It is experimental, offered only with `AGENT_ARCHIVE_EXPERIMENTAL_R2_CREATE=1`, until the live acceptance checklist in `dev/contributing/testing.md` is done; remove the gate then.
- Guided creation also produces the values cloud mode needs (bucket, endpoint, key), so it directly serves the cloud-capture work; see 2c.

## Risks and open questions

- Linux Cursor path and `cursor-agent` behavior are unverified; test on a real install before claiming support.
- Claude Code and Codex hook behavior on a persistent Linux desktop is unverified beyond the cloud probe (which ran as root on a web VM).
- Repo-key refresh only helps machines that own the registration and still have the repo; older cross-machine sessions keep path matching.
- Guided creation depends on the unconfirmed Cloudflare details listed in Part 2a (secret hash encoding, some permission requirements, token-delete endpoint, default expiry). A wrong assumption fails at `VerifyAccess`, not silently, but the flow must be tested live before release.
- The bootstrap token is powerful for the seconds it exists. The tests that assert it is never persisted or logged are release-blocking.
- Unsigned Linux binaries weaken the release-trust story relative to macOS; the checksum check must be mandatory.
- Snapshot directory mode for Cursor database copies on Linux `/tmp` was not checked; it should be 0700.

## Work packages and file ownership

Parts 1 and 2 are split into PRs with fixed names so parallel work does not collide. Handoff v2 PRs (#147 to #152) were in flight when this was written and edit `internal/cli/handoff*.go` and `internal/cli/setup*.go`; a package that touches those files must be based on the relevant branch or wait for it to merge, and must merge `main` into its branch before requesting review.

| Package | Scope | Owns |
| --- | --- | --- |
| R1 | `repo_key`: normalizer, hash, registration field, metadata field, schema, parser 0.16.0, refresh. No CLI matching change | `internal/archive` (bundle.go, metadata.go, types.go, adapters.go), `internal/capture/hook.go`, `internal/collector/{session,metadata}.go`, `schemas/metadata.schema.json`, docs for metadata |
| R2 | Handoff matching by `repo_key`, branch note, `--to` with archived sessions, `noMatch` text | `internal/cli/handoff*.go`, handoff render in `internal/archive`, handoff docs; depends on R1 |
| S1 | Setup trims, immediate credential validation, cloud-variables printout | `internal/cli/setup*.go`, `internal/cli/prompt.go`, setup docs |
| S2 | Cloudflare client and guided R2 creation | new `internal/cloudflare`, `internal/cli/setup_r2_create*.go`, minimal hooks into `setup.go`; depends on S1 landing first for `setup.go` |
| S3 | Guided S3 bucket creation | `internal/cli/setup_aws*.go`, `internal/storage` additions, docs; depends on S1 |
| S4 | Docs and acceptance-record updates | `docs/getting-started/*`, `docs/security/*`, `dev/maintainers/open-source-acceptance.md` |

Every package gets an independent implementer and an independent reviewer; a second review follows any fix round that touched correctness.
