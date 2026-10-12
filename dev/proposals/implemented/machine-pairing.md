# Adding a machine: pairing, per-machine keys, and revocation

> **Implemented as an experimental draft; general availability remains disabled.**
> Original design reviewed 2026-10-01 against `4de2d07`; the non-live reconciliation
> started from `7e804f80` on 2026-10-05. Live Cloudflare and representative platform
> acceptance remain separate gates. Code wins where historical references differ.

Current implementation retains retired committed dedicated assignments atomically
with setup, accepts healthy issuer labels or independent operator files for remote
revocation, verifies unused spare claims separately from delivered descendants,
and reports bounded local and untrusted remote operation progress. Portable
standalone exclusions, safe interactive mappings, direct-first discovery, partial
candidate review/manual recovery, post-commit success, management-token fallback,
clipboard recovery and missing-app recheck are implemented. See the
[multiple-machine guide](../../../docs/guides/multiple-machines.md) for the exact current
experimental flow and [uninstall instructions](../../../docs/getting-started/uninstall.md)
for safe revocation before deleting local ownership evidence.

Provider token names intentionally contain only recipient, issuer and slot IDs;
chosen display names are local/bucket labels. Default machine names use random IDs,
never hostnames. Rename does not change authority: an original issuer label remains
trusted lineage, while a renamed remote label needs independently verified operator
evidence. Ordinary revocation covers local retired assignments and independently
listed unused spares; delivered descendants require `--include-issued` and a trusted
immutable issuer ID. Records remain informational, including provider-confirmed claims.

The operation snapshot bound is 64 KiB; ordinary registry records stay 16 KiB.
Listings share five seconds, 1,000 pages/objects and four fetch workers. Retired
record hints are limited to sixteen entries with an explicit partial flag; committed
local history is limited to 128 without silently dropping earlier keys. The wire
format/version and cryptography remain unchanged. Older receivers reject standalone
exclusions with an empty affected set; upgrade both peers for this scope fix and
retain operator bindings before downgrading or removing local data.

The remaining design below is historical. Its command examples describe the intended
GA experience, rather than overriding current experimental gates. Timing targets and
live acceptance requirements below remain unverified until separately recorded.

Non-live verification on 2026-10-05 (macOS arm64, Go 1.27.1): the repository-wide
`go test -race -timeout 20m ./...` passed, as did the ten relevant package race
suites plus doc-link checks. After complexity-only helper extraction, the affected
pairing, machine, revocation, project, setup and schema race regressions passed.
`go vet ./...`, pinned golangci-lint 2.14.0's blocking pass, and changed-code doc
comment checks passed. Combined fake acceptance covers source issuer names,
third-machine revocation after rename, retired re-pairing keys, unused/delivered
spares, issuer compromise, competing adds for one spare, and partial deletion/retry.
These results establish local contracts only; no real Cloudflare mutation, live
permission/visibility/propagation check, or representative Linux/platform timing
acceptance was performed in this implementation session.

Goal: add a computer in under a minute when apps and credentials are ready, without dashboard work, retyping settings, or exposing secrets. Commands:

1. **`agent-archive machines add`** on a machine that is already set up. It writes an encrypted **pairing bundle** and prints a **pairing code**.
2. **`agent-archive setup --pair`** on the new machine. It takes the bundle and the code and sets the machine up with the same settings.
3. **`agent-archive machines`** and **`agent-archive machines revoke NAME`**. They list the machines that use the bucket and cut one off.
4. **Per-machine R2 keys.** Each machine gets its own key limited to the bucket, so one machine can be revoked without touching the others.

No Cloudflare API token with account-wide permissions is ever stored.

## Contents

- [Why](#why)
- [Decisions](#decisions)
- [User experience](#user-experience)
- [Part 1: what a pairing carries](#part-1-what-a-pairing-carries)
- [Part 2: the pairing bundle and code](#part-2-the-pairing-bundle-and-code)
- [Part 3: the machine list in the bucket](#part-3-the-machine-list-in-the-bucket)
- [Part 4: per-machine R2 keys](#part-4-per-machine-r2-keys)
- [Part 5: revocation](#part-5-revocation)
- [S3](#s3)
- [Security analysis](#security-analysis)
- [Compatibility](#compatibility)
- [Phases and pull requests](#phases-and-pull-requests)
- [Testing](#testing)
- [Documentation](#documentation)
- [Open questions and things to verify](#open-questions-and-things-to-verify)
- [Alternatives considered](#alternatives-considered)
- [Review changes](#review-changes)

## Why

Today, repeating `setup` requires manual settings and credential entry. Its printed `setup --yes` command (`printAnotherMachine`, `internal/cli/setup.go:975`; `anotherMachineCommand`, `setup.go:993`):

- Appears only once and cannot be recalled.
- Omits prefix (`setup.go:984`), `retention_days`, `require_skill_use`, `skill_evidence`, `no_skills`, and `handoff`.
- Uses project paths under `~` (`homeRelative`, `setup.go:1023`), so relocated checkouts fail to match. Handoff already uses `repo_key` (`internal/archive/repo_key.go`).
- Shares the first machine's R2 key, preventing independent revocation.

Copying the data directory also copies `machine_id`, causing competing session owners and overwrites ([multiple machines](../../../docs/guides/multiple-machines.md)).

## Decisions

| # | Question | Decision |
| --- | --- | --- |
| D1 | Command names | `machines add` on the source; `setup --pair` on the destination; `machines` and `machines revoke NAME` anywhere. “Invite” was rejected because it suggests referring a friend. |
| D2 | How settings and the key travel | Encrypted bundle copied through any channel; generated code typed separately. |
| D3 | Pairing code | Six `crypto/rand` words from EFF short wordlist 2: 1,296 entries, ~62 bits, unique three-letter prefixes. |
| D4 | Where R2 keys come from | Fresh key with an available Cloudflare token, otherwise a spare; otherwise prompt for a token, explicit sharing, or cancellation. |
| D5 | Storing a key-creating Cloudflare token | Never persist the management token. Pre-create spares while it is available; reacquire from environment, password manager, or prompt. |
| D6 | The machine list | Informational records under `<prefix>/machines/`; every machine can edit them. `machines --verify` checks provider state explicitly. Records never authorize deletion. |
| D7 | Revocation | Delete independently verified keys; track per-key results. A request alone means “access not removed”. See [revocation](#part-5-revocation). |
| D8 | Shared-key fallback | Explicitly allowed and labelled throughout; removing shared access requires replacing the key on its users. |
| D9 | Project matching | Repository key, then eligible home-relative path; ambiguous clones need selection. Preserve exclusions and skip unresolved scope under `--yes`. |
| D10 | Cryptography | Argon2id + XChaCha20-Poly1305 from `golang.org/x/crypto`, maintained alongside existing `golang.org/x/sys`, `x/term`, and `x/text` in `go.mod`. See [alternatives](#open-questions-and-things-to-verify). |
| D11 | Coding agents | Refuse pairing commands when `CLAUDE_CODE_SESSION_ID`, `CODEX_THREAD_ID`, or `CURSOR_AGENT` is set, regardless of `--yes` or `AGENT_ARCHIVE_NONINTERACTIVE`: prevent transcript leaks and injected destination changes. |
| D12 | Where the bundle and code are shown | Bundle to clipboard or `--print`; code on a cleared alternate screen, redisplayable during the command. Recording/sharing/logging can still capture it. |
| D13 | Credential identity | Immutable random `recipient_id`, `issuer_id`, and slot IDs identify assignments. Names are labels; existing-machine pairing keeps `machine_id`. |
| D14 | Pairing recovery | Secret-free ledger, atomic spare reservation, five lifecycle states; uncertain delivery remains reserved. See [recovery](#pairing-lifecycle-and-recovery). |
| D15 | Performance | Bounded discovery and bucket reads; partial results. Provider/password-manager work requires explicit `--verify`. |

## User experience

### On the machine already set up

```text
$ agent-archive machines add
Name for the new machine: work-laptop
✓ Storage check passed
✓ Gave work-laptop its own key for my-bucket (from a spare; 1 spare left)
✓ Pairing bundle copied to the clipboard. It expires in 15 minutes.

On work-laptop, run:  agent-archive setup --pair
and paste the bundle. Press Enter to see the pairing code.
```

Pressing Enter switches to the alternate screen:

```text
Pairing code for work-laptop (type it there; don't send it with the bundle):

    tusk  cove  amble  rotor  gizmo  plank

Typing the first three letters of each word is enough.
Press Enter to hide the code. You can show it again while this command is open.
```

The normal screen retains only “✓ Code shown” and offers “Show code again”, “Done”, or “Cancel pairing”. Redisplay uses the alternate screen. Closing the command discards the code; another command creates a new pairing. Recording, sharing, or terminal logging can retain the display.

- Clipboard: `pbcopy` on macOS; `wl-copy` or `xclip` on Linux. Apple Universal Clipboard supports cross-Mac paste. Without a clipboard, or with `--print`, print the bundle. Clear the clipboard on command exit only if it still contains the bundle—not when hiding the code. Clipboard-manager history may retain it, still encrypted.
- `--file PATH` writes the bundle to a file (mode 0600, refusing to overwrite one) for AirDrop or a USB stick.
- `--expires DURATION` sets the expiry, from 5 minutes to 24 hours.
- `machines add --yes --name NAME` prints both pieces without prompts for scripts that deliver them separately.
- Check source storage before creating anything.
- It refuses to run inside a coding agent ([D11](#decisions)).

### On the new machine

```text
$ agent-archive setup --pair
Paste the pairing bundle: aa-pair1:eJyrVkrOz0vOz0kpTVayUlAqSi0u...
Pairing code: tus cov amb rot giz pla

✓ Bundle unlocked from mac-studio. Proposed machine name: "work-laptop".

Sessions from this machine will be uploaded to:
Storage   R2 my-bucket (account 1a2b…9f), folder agent-archive/   from mac-studio
Apps      Claude Code, Codex                          from mac-studio
Projects  ~/src/app        matched by repository
          ~/work/api       matched by repository
          docs-site        not found on this machine (skipped)
Retention 90 days · skill evidence: metadata          from mac-studio

Save? [Y/n]
✓ Storage check passed · key saved to Keychain · hooks installed · background collector running
✓ Paired with mac-studio. This machine is "work-laptop".
```

- At general availability, fresh interactive `agent-archive setup` offers pairing or setup from scratch; `--pair` remains a shortcut.
- Reuse `internal/cli/setup_review.go`: editable settings, destination first (provider/account/bucket/prefix). Reconfiguration shows the prior destination and requires explicit approval of a change, without a default yes.
- Show `handoff.args` in full because they become agent command-line arguments.
- Show “Paired” after credentials, configuration, hooks, and scheduler commit; list outstanding app approvals separately. Offer import afterward. Machine-record publication failure leaves setup committed and displays “machine registration pending”; the collector retries. Failures explain existing capture status and [retry steps](#pairing-lifecycle-and-recovery).
- `--pair-file PATH` reads the bundle from a file, and `--pair-file -` from standard input.
- Read the code from the terminal, or `AGENT_ARCHIVE_PAIRING_CODE` under `--yes`; remove the variable after reading. Never accept a code flag (shell history/`ps`).
- Accept full words or three-letter prefixes, case-insensitively, separated by spaces or hyphens. Validate prefixes before derivation, with hints (“*tns* … did you mean *tus*?”). Check the outer checksum before asking for the code; distinguish damaged paste from wrong code.
- It refuses to run inside a coding agent ([D11](#decisions)).

### Anywhere

```text
$ agent-archive machines
NAME          PLATFORM       KEY                  PAIRED       HEARTBEAT
mac-studio    macOS arm64    own key              2026-08-02   today         (this machine)
work-laptop   macOS arm64    own key              2026-10-01   today
build-box     Linux x86-64   shared (mac-studio)  2026-09-14   3 days ago
old-mbp       macOS x86-64   revocation requested—access not removed

Not checked against Cloudflare. Use agent-archive machines --verify.
Heartbeat is updated at most daily; it does not indicate current activity.

$ agent-archive machines --verify
✓ Checked against Cloudflare just now

$ agent-archive machines revoke work-laptop
This deletes work-laptop's own key and its 0 unused spare keys.
Recipient: <immutable recipient ID>; issuer: <immutable machine ID>
Provider key: "agent-archive r=<recipient-id> i=<issuer-id> k=<slot-id>"
Its sessions stay in the bucket.
Revoke? [y/N] y
✓ Key deletion confirmed · access removed for the verified key set
```

Missing tokens or partial deletion produce per-key active/unverified status and dashboard instructions; bucket records cannot prove access removal.

## Part 1: what a pairing carries

### Carried

| Setting | Source | Notes |
| --- | --- | --- |
| Provider, bucket, prefix | `storage.Provider`, `Bucket`, `Prefix` | The prefix gap in today's `setup --yes` is closed (see [Phase 1](#phases-and-pull-requests)). |
| R2 account or endpoint | `storage.R2AccountID`, `R2Endpoint` | |
| AWS profile and region | `storage.AWSProfile`, `Region` | The profile name only; see [S3](#s3). |
| Apps | `harnesses` | Only apps found on the new machine are installed; the rest are listed and offered. |
| Projects and exclusions | `archive.projects[]` | Inclusions carry `repo_key`, display name, and home-relative path. Exclusions carry repository-relative or home-relative mappings where possible, plus a warning when they cannot be mapped. See [project matching](#project-matching). |
| Retention | `retention_days` | |
| Capture rules | `require_skill_use`, `skill_evidence`, `no_skills` | |
| Handoff preferences | `handoff.args`, `handoff.default_to` | |
| R2 key | Part 4 | The key for this machine, or the shared key, labeled as such. |
| Pairing details | | The new machine's name, random `pairing_id` and immutable `recipient_id`, the sender's `machine_id` (`issuer_id`) and name, creation and expiry times. Spare assignments reuse the recipient ID allocated at spare creation. |

### Never carried

`machine_id`, `host_id`, `allow_network_home`, `R2CredentialRef`, `retired_credential_refs`, `retired_machine_assignments`, `declined_harnesses`, `imported_harnesses`, `hook_files`, `installed_executable`, `background_backend`, `destination_since`, `previous_destinations`, `storage_verified_at`, `bucket_privacy`, `paused`, local state, and anything from the AWS or proxy environment (paths and endpoints differ per machine). The new machine gets its own identity, and its own checks run fresh.

Never install source identity or absolute paths; carry unmappable exclusions as scope warnings without source absolute paths.

### Project matching

Candidates: existing home-relative paths, current directory, then bounded app-history roots (`backfill.KnownProject`) for unresolved projects. Use the bundle’s repository key/display name/path; no home crawl or transcript-body scan.

Deduplicate canonical paths and cache repository keys. Initial limits: **5 seconds total**, including history; **128 roots**; **four concurrent Git processes**; **250 ms per lookup** within the shared deadline. Validate these defaults on macOS/Linux. `knownProjectsOnce` currently scans headers for ten seconds and discards partial results: add a bounded API returning partial results, with cancellation during enumeration and header reads.

Match `repo_key` when present; a known mismatch cannot fall back to path. With no `origin`, allow home-relative path matching labelled “matched by path”. Multiple clones require selection, including explicit consent to include both; `--yes` skips ambiguity. Distinguish missing projects from timeout/cap results, offer manual paths, and set `activated_at` to destination save time.

Map exclusions relative to matched roots, rejecting absolute paths, traversal, and symlink escapes; validate other home-relative mappings. Unmappable exclusions block affected inclusions until interactive mapping or explicit scope approval; `--yes` skips those roots. Preserve existing local exclusions unless edited. Review exact roots, effective exclusions, skips, and unresolved warnings.

## Part 2: the pairing bundle and code

### Format

```text
aa-pair1:<base64url, no padding>

header (authenticated, not encrypted)
  version        1 byte   = 1
  kdf params     Argon2id time, memory, threads (so they can be raised later)
  salt           16 bytes
  nonce          24 bytes
  pairing_id     16 bytes
  expires_at     8 bytes  (Unix seconds)
ciphertext       XChaCha20-Poly1305(gzip(payload JSON)), header as associated data
checksum         4 bytes  CRC-32 of everything before it
```

- CRC diagnoses typos/truncation; only the AEAD tag authenticates.
- Compress before encryption; the payload has no attacker-chosen content in this model.
- Before authentication or derivation, reject bundles over **64 KB**, unknown versions, and Argon2id parameters outside exact supported sets. Cap decompression at **256 KB**. Unauthenticated parameters must not trigger excessive allocation.
- Estimated text size: **1–1.5 KB** for ten projects plus an R2 key. Copy the bundle; type the code.

### Key derivation and encryption

- Embed EFF short wordlist 2 (~10 KB): six independently generated words, 1,296 choices (~10.3 bits/word; ~62 total). Test unique three-letter prefixes rather than trusting the list description.
- Derive 32 bytes with `Argon2id(code, salt, t=3, m=64 MiB, p=4)` (RFC 9106’s second recommended parameters); expected subsecond laptop latency needs measurement. Header parameters allow future upgrades.
- **Guessing rationale:** each offline guess costs 64 MiB Argon2id. Even assuming 10,000 guesses/second/GPU, half of 2^62 takes millions of GPU-years. For a revocable bucket credential, the large list’s 77.5 bits do not justify roughly twice the typing.
- XChaCha20-Poly1305 uses a random 24-byte nonce and authenticated header, protecting expiry and `pairing_id`.
- Expand prefixes, lowercase words, join with single hyphens before derivation.

### Expiry

`expires_at`: 15 minutes by default; accept five minutes of clock skew. Expiry errors state elapsed time by the destination clock. Expiry binds honest clients only: another program can decrypt with both pieces afterward. Encryption protects the key; [unused-pairing reporting](#unused-pairings) makes lingering credentials visible.

### What is never written

- Never persist the code in config, `setup-draft.json`, journals, logs, or `status`. Redisplay only in the active process’s alternate screen ([D12](#decisions)).
- The bundle goes to the clipboard, or is printed, or written to the `--file` path. The new machine never saves it.
- Redact `aa-pair1:` via `internal/archive/credential_shapes.go` as `[REDACTED]` before upload; bump `FilterVersion` and update its changelog. Ordinary-word codes cannot be recognized reliably; the agent refusal prevents automatic transcript exposure.
- Keep decrypted payload in memory; stage R2 secrets only in the credential store. Drafts/journals contain references and non-secret retry metadata, never code or bundle. Audit existing setup staging paths for this requirement.

## Part 3: the machine list in the bucket

### Objects

```text
<prefix>/
  machines/
    <machine_id>.json            one per machine, written only by that machine
    revocations/<operation_id>.json  one progress record per revocation operation
```

`<machine_id>.json`:

```json
{
  "schema_version": 1,
  "machine_id": "…",
  "name": "work-laptop",
  "platform": "darwin/arm64",
  "agent_archive_version": "0.2.0",
  "paired_at": "2026-10-01T17:02:11Z",
  "paired_from": "<machine_id of mac-studio>",
  "pairing_id": "…",
  "credential": { "kind": "r2_own", "access_key_id": "…", "recipient_id": "…", "issuer_id": "…" },
  "spare_access_key_ids": ["…"],
  "heartbeat_at": "2026-10-01T17:05:00Z"
}
```

- `credential.kind` is `r2_own`, `r2_shared` (with the `machine_id` whose key it shares), or `aws_profile`.
- `access_key_id` is the non-secret R2 token/Access Key ID (`cloudflare.DeriveS3Credentials`, `internal/cloudflare/r2.go:129`); `DeleteToken` uses it.
- Writers: each machine’s own record; each revoker’s distinct operation object. Serialize local operation retries; provider deletion is idempotent. Every bucket key can modify all records—these conventions are not access controls.
- Choose the name in `machines add` or interactive first setup (suggest short hostname): lowercase letters, digits, hyphens, ≤40 characters. Refuse observed duplicates, but concurrent creation can race; ambiguous names require IDs. `machines rename NAME` changes labels/local mappings, never provider identity.
- `heartbeat_at` is refreshed by the collector at most once a day, which is one small PUT per machine per day.
- No paths, sessions, or transcript content; names/platforms are the new personal data, documented in privacy guidance.

### When records are written

- `setup` (any form) writes or refreshes this machine's record after a successful save.
- Pairing publishes `pairing_id`, `recipient_id`, and `paired_from` after commit. Claims are informational, not exclusive-use proof; retry publication independently.
- Collector: daily heartbeat and missing-record backfill. Older machines use `unnamed-<first 4 hex of machine_id>` to avoid leaking personal hostnames. Only interactive setup or `machines rename` asks for a name; `status`/listing never prompt.
- `uninstall` writes nothing. It prints how to run `machines revoke` for this machine from another one, since uninstalling does not delete the key.

### Reading

`machines` reads bucket records only—no token acquisition, password-manager call, account-token listing, or session scan. `--verify` opts into Part 4’s token sources and reports provider-check time, missing/unclaimed keys, ownership mismatches, and incomplete results. Timeout/permission errors never count as successful verification.

Paginate bucket/provider listings. Validate bucket schemas/paths; initial caps: **16 KiB/record**, **four fetches**, **5 seconds total**, **1,000 records**. Name omitted/unreadable records. Provider verification has a separate cancellable **20-second** budget and reports partial pagination. Validate defaults before shipping.

Say “Not checked against Cloudflare” unless this invocation verified it. Label daily `heartbeat_at` as “Heartbeat”, using date/“today”, not presence or activity. Tolerate newer fields within limits; unsupported semantics remain unknown. Records neither authorize deletion nor prove access removal.

### The issued-key ledger

Keep a secret-free **0600** ledger in `<data dir>/issued/`: pairing/recipient/issuer IDs, label history, provider key ID/name, credential reference, fresh/spare/shared origin, delivery/expiry times, claims, and cleanup outcomes. Record slots before use. Trust immutable IDs and verified local bindings, never bucket claims overwriting them. Reconfiguration keeps `machine_id` and binds it to the assignment’s distinct `recipient_id`.

Retain issuance lineage after claim/local-secret removal: the issuer could have copied the key. `uninstall --delete-local-data` removes the ledger; provider issuer IDs still identify issued keys, but bindings/labels may need verification elsewhere.

### Pairing lifecycle and recovery

| State | Durable evidence | Recovery |
| --- | --- | --- |
| Prepared | Pairing/recipient IDs, reserved key slot, expiry, delivery intent | Before exposure, release an untouched spare or delete a freshly created key. Record failed cleanup for retry. |
| Delivered | Delivery was attempted or confirmed; uncertainty is recorded | Keep the key reserved and valid. Do not revoke automatically after a clipboard, file, print, or terminal failure that may have exposed the bundle. |
| Claimed | A post-commit bucket record matches pairing, recipient, and key IDs | Record the observed destination binding; report mismatches. This is informational and cannot detect two clients sharing the key. |
| Expired | No matching claim observed before expiry | Warn that the key may still work; offer explicit revocation. Never recycle a delivered spare. |
| Cancelled | Person explicitly cancels; per-key cleanup result recorded | Delete a dedicated key when a management token is available, otherwise show cleanup pending. Shared credentials are never deleted automatically. |

Reserve spares with an inter-process lock and atomic ledger replacement. Persist creation intent before the API call; reconcile lost responses by unique provider slot before retrying. Persist delivery intent before exposure; crashes/Ctrl-C leave uncertain slots reserved. Remove delivered spares from the pool and delete issuer-local secrets after the active command no longer needs them; retain lineage.

Keep code/bundle in process memory for redisplay and delivery retries. After exit, generate new pieces; keep previously delivered keys tracked until explicit revocation. Reuse a pre-delivery spare only when the journal establishes no exposure; uncertainty counts as delivered.

Use credential-store staging and the existing setup transaction. Before commit, failure preserves prior configuration and records non-secret retry metadata; reuse matching staged references. Expired, uncommitted bundles require new pairing. After commit, retry only record publication, without requiring the expired bundle or repeating setup.

### Unused pairings

Issuer `status`/`machines` report expired or uncertain ledger entries as “claim not observed”, not proven unused. Dedicated keys remain valid until deletion; shared-key cleanup needs replacement. No automatic management action/token acquisition in listing. Use pairing/recipient ID for explicit revoke when no record exists.

A pairing used twice cannot reliably be distinguished. If both pieces may have been seen, revoke the dedicated credential and pair again; for a shared key, replace that key everywhere it remains in use.

## Part 4: per-machine R2 keys

### Spare keys

Spares are bucket-scoped R2 keys pre-created in the credential store, avoiding a later dashboard trip without retaining a management token.

- **When spares are created:**
  - during guided R2 setup (`internal/cli/setup_r2_create.go`), right after `mintKey`, while the token is still in memory;
  - during `machines add` when a token is available, refilling to the target count. `machines revoke` never creates or refills keys.
- **How many:** two by default. `machines add --spares N` (0 to 5) changes the target and saves it as `spare_keys` in `config.json`. `0` turns spares off.
- **Where:** the same credential store as the machine's own key (Keychain on macOS; `credentials/<reference>.json`, 0600, on Linux), under references listed in a new `spare_credential_refs` config field, and in the [issued-key ledger](#the-issued-key-ledger).
- **Provider name limit:** official [Token Details](https://developers.cloudflare.com/api/resources/accounts/subresources/tokens/methods/get/) documents a 120-character maximum. The original bucket-bearing proposal exceeds that limit even for a one-character bucket. The canonical name omits the bucket; only the exact provider policy and configured account/jurisdiction/bucket establish destination scope. Names alone never authorize management.
- **Identity:** allocate random 128-bit `recipient_id` and slot ID before the destination is known. Fresh/spare provider names: `agent-archive r=<recipient-id> i=<issuer-id> k=<slot-id>` (118 bytes with three lowercase 32-hex IDs). Carry the spare’s recipient ID into its bundle; labels stay in local/bucket records. Issuer identity persists after delivery and does not imply “unused spare”.
- **Risk:** spares outlive deletion of the holder’s main key. Distinguish unused from delivered keys through verified bindings. [Issuer-compromise recovery](#issuer-compromise) includes potentially copied delivered keys; recipients obtain replacements from a healthy issuer.

### Where a token comes from

Creating/deleting/listing keys needs a Cloudflare token with “Account API Tokens Write”. Sources, in order:

1. `CLOUDFLARE_API_TOKEN`, as wrangler uses. Removed from the environment once read, like the R2 variables.
2. `cloudflare_token_command` in config, an argv array, e.g. `["op", "read", "op://Private/Cloudflare/agent-archive"]`: interactive commands only, never collector. Allow unlock/Touch ID; never log stdout/token. Suppress stderr except generic failure and exit status, matching S3 `credential_process`.
3. A prompt, with the same deep link and permission list guided setup prints (`printR2BootstrapInstructions`, `setup_r2_create.go:286`).

Keep tokens in memory for one command. Never send management tokens to object storage or object credentials to the management API.

### `machines add` for R2

1. If a token is available without asking (1 or 2 above), allocate an immutable recipient ID and key slot, create a fresh key using the provider naming scheme above, refill spares, and use the fresh key. Refill failure is reported independently and does not invalidate a delivered pairing.
2. Otherwise, if a spare is left, use it and say how many remain.
3. Otherwise, ask:
   - "Paste a Cloudflare token to create a key for work-laptop" (and refill spares);
   - "Share this machine's key" (labeled: work-laptop cannot then be revoked on its own);
   - "Cancel".

`--yes` takes 1 or 2 when it can, and otherwise fails, naming `CLOUDFLARE_API_TOKEN` and `--share-key`. Sharing never happens without the person choosing it.

Reuse `internal/cloudflare`’s `SelectPermissionGroup`, `BucketResource`, `CreateToken`, `DeriveS3Credentials`, and `checkKey` propagation wait (`setup_r2_create.go:708`). Verify before bundle delivery; apply [lifecycle recovery](#pairing-lifecycle-and-recovery): clean up fresh keys before exposure, retain keys after attempted delivery until explicit cancellation, journal cleanup failures and ambiguous API outcomes.

Manually configured machines have no spares or known management permissions; they can still create keys with a token or explicitly share without one.

## Part 5: revocation

`agent-archive machines revoke NAME` resolves a unique target, verifies bindings, and confirms exact keys/affected machines. Mutually exclusive `--machine-id ID`, `--recipient-id ID`, or `--pairing-id ID` replace NAME for ambiguous/unclaimed targets. `--yes` cannot bypass unresolved ownership. Never mint/refill keys here.

### Which keys revoke deletes

Provider names establish immutable recipient/issuer/slot IDs and issuance—not labels or bucket-asserted machine bindings. Connect assignments to targets via trusted local ledger/committed credentials; rename changes no deletion authority.

- Verify token ID, bucket scope, issuer, recipient, and slot against provider data and independently trusted bindings before deletion. Legacy manually entered keys have no such naming guarantee and require explicit verification.
- Bucket records are hints. Without an independent binding, show exact provider IDs and require healthy-issuer ledger or explicit operator verification. Stop on mismatch; `--yes` fails rather than trusting bucket mappings.
- Ordinary revocation deletes the target's verified active and retired dedicated keys and its verified unused spares. Delivered keys for other recipients are not included merely because their issuer is the target. Incomplete spare/recipient bindings are reported, not guessed.
- Without a token, provider state cannot be checked. Print claimed IDs as unverified, dashboard instructions, and any trusted local bindings. Recording a request is not access removal.

### Revocation progress and concurrency

Persist a random `operation_id` in a secret-free local journal and publish `machines/revocations/<operation_id>.json` before key deletion. Store target/recipient/key IDs, requester, time, and per-key pending/confirmed/failed-or-unknown states. Each revoker owns its record; retries resume it. No token: “revocation requested—access not removed”. Partial deletion lists remaining keys; complete verified deletion: “access removed for the verified key set”. Label incomplete inventory explicitly.

Provider-confirmed absence is idempotent success, including concurrent deletions. Merge informational per-key results without replacing confirmed deletion with later pending observations; reverify untrusted bucket evidence for sensitive actions. Retry deletion and record publication independently.

**Self-revocation:** publish pending progress and persist locally while the key works; delete the final object credential last. Final bucket publication may fail. Report provider-confirmed success locally and bucket progress as pending; another authorized machine reconciles via `machines --verify`. Separate bucket-write failure from deletion outcome.

**Shared key:** list known affected machines and inventory gaps. Move remaining users to `machines own-key` before deletion. A bucket request cannot isolate one shared-key user; `machines rotate-key` is deferred.

### Issuer compromise

An issuer may have copied every minted/delivered secret. `machines revoke NAME --include-issued` enumerates all keys with the verified provider issuer ID, lists recipients/unknown bindings, and confirms the broader impact. Never use a compromised ledger to exclude delivered keys. Replace from a healthy issuer before deletion where practical; otherwise explain recipient outages. Separately verify legacy/shared credentials and label incomplete recovery.

### What revocation does not do

Revocation leaves sessions and previously downloaded data intact. [Uninstall’s lifecycle backstop](../../../docs/getting-started/uninstall.md#a-lifecycle-rule-as-a-backstop) covers retention after the owner stops sweeping, with deletion instructions. It does not modify the remote machine; subsequent uploads fail and `status` reports credential rejection.
### Moving a machine off the shared key

Beta/shared-key users run `agent-archive machines own-key` with a management token:

1. creates a dedicated key with immutable recipient/issuer/slot IDs and checks that it works;
2. commits the replacement through the setup credential transaction, preserving rollback until commit;
3. removes the old shared secret and reference from this machine after the replacement commits, unless another local destination explicitly still needs it; any retained access is shown as still present;
4. updates this machine's record to `r2_own`, retrying record publication separately if needed.

Do not delete the provider’s shared key; others may still use it. Listing counts observed users (including manually entered matching keys), labelling a singleton “one known user”, not exclusive ownership.

## S3

S3 carries only profile name and region—usually SSO with short-lived credentials, never credential values.

- `setup --pair` checks that the profile exists on the new machine. If it does not, it prints `aws configure sso --profile <name>` (or `aws configure --profile <name>`) and stops before saving. Run `setup --pair` again after signing in; the same bundle works until it expires.
- The machine list records `credential.kind: aws_profile`. `machines revoke` records a revocation request and states "AWS access not removed; change IAM permissions". A profile name does not identify a unique AWS principal. No bucket marker is presented as proof of access removal.
- Keep the same encrypted/code flow: bucket names and repository hashes are private, despite no secret credential. See [open questions](#open-questions-and-things-to-verify).

## Security analysis

| Threat | Guarantee or limit |
| --- | --- |
| Bundle alone | ~62-bit code plus 64 MiB Argon2id; see [guessing rationale](#key-derivation-and-encryption). |
| Code alone | Insufficient without bundle; no code persistence. |
| Screen/history capture | Alternate screen reduces scrollback, not recording/sharing/logging. `--yes` prints both pieces; scripts deliver separately. |
| Both pieces stolen or replayed | Bucket access; double use cannot be detected reliably. Revoke dedicated key and re-pair, or replace a shared key everywhere. Claim absence is not proof of non-use. |
| Coding agent runs add | Binary refusal ([D11](#decisions)); manually pasted bundles are redacted before upload. |
| Injected agent runs `setup --pair --yes` | Same refusal; skill guidance alone is insufficient to prevent destination hijacking. |
| Person accepts attacker bundle | Destination-first review and explicit reconfiguration approval; cannot protect deliberate approval. |
| Forged bucket ownership/revocation/list | Records are untrusted; independently verify immutable IDs before deletion. `--verify` flags missing/unclaimed/mismatched provider keys but a matching claim does not establish ownership. |
| Oversized KDF/decompression request | Known parameter sets and size caps, enforced before derivation/allocation. |
| Issuer compromise | Own keys, spares, and copied delivered secrets may leak. Normal revoke covers verified own/unused keys; `--include-issued` extends to issued recipients. No persisted management token. Legacy/shared gaps remain explicit. |
| Recipient compromise | Revoke its dedicated key; other recipient keys remain active. |
| Bundle tampering | AEAD rejects modification, including expiry and pairing ID. |
| Modified client ignores expiry | Possible; expiration is not credential revocation. Ledger keeps the assignment visible. |
| Management-token leak | Out of scope. Never persist; recommend an account-owned token limited to guided setup’s two documented permissions. |

## Compatibility

- **Config:** add optional `machine_name`, credential recipient/issuer bindings, `spare_keys`, `spare_credential_refs`, `cloudflare_token_command`. `config.Load` (`internal/config/config.go:201`) tolerates unknown fields, but older setup drops them while provider/stored keys remain. Upgrade all machines before spares; `machines --verify` finds unclaimed keys for cleanup.
- **Bucket:** new `machines/` is ignored by existing retention/purge/list paths, which scan `sessions/`/`listing/` (`internal/purge/purge.go:87`, `internal/reader/list_index.go:131`). No record retention deletion; include the folder in archive-removal instructions/layout docs.
- **Bucket permissions:** a key limited to the prefix ([bucket permissions](../../../docs/security/bucket-permissions.md)) already covers `machines/`.
- **`setup --yes`:** unchanged for existing scripts. New flags are added only (Phase 1).
- **Schemas:** session metadata remains unchanged and joins on `machine_id`; specify/validate new machine, operation, and ledger schemas separately.
- **Privacy filter:** the `aa-pair1:` shape bumps `FilterVersion`, so existing sessions are republished under the new filter as any filter bump does (see [versions](../../maintainers/versions.md)).
- **Local state:** the issued-key ledger is a new folder in the data directory, ignored by older binaries.

## Phases and pull requests

### Phase 1: close the `setup --yes` gaps (no new concepts)

Useful on its own, and the base the pairing payload is applied through.

- **P1a.** Add `setup --yes` flags for what it cannot set today: `--prefix`, `--retention-days`, `--require-skill-use` / `--no-require-skill-use`. Teach `anotherMachineCommand` to emit them, plus `--skill-evidence` and `--no-skills`, and remove the "then run agent-archive setup there" fallback (`setup.go:984`).
- **P1b.** Match `--project` by repository: accept `--project-repo REPO_KEY` alongside paths, resolved as in [project matching](#project-matching), so the printed command works when checkouts are at different paths. Refactor matching into a function the pairing path reuses.

### Phase 2: explicitly labelled shared-key beta

Ship only as an opt-in beta for R2, requiring explicit `--share-key` consent and warning that this machine cannot be revoked independently. Do not advertise isolated revocation or make this the default second-machine path. S3 carries settings and profile names only. The general pairing flow waits for Phase 4's per-machine keys and working revocation.

- **P2a.** `internal/pairing`: the bundle format, the embedded wordlist with its prefix test, Argon2id and XChaCha20-Poly1305, the limits on header parameters and decompression, prefix expansion and typo hints. Pure functions with no filesystem or network access, like `internal/archive`. Adds `golang.org/x/crypto`.
- **P2b.** The `aa-pair1:` credential shape in the privacy filter, with its `FilterVersion` bump, changelog entry and regenerated golden files. It ships before `machines add` so no released version can print a bundle that the filter would upload.
- **P2c.** Machine records: `machines/` objects in `internal/storage` terms, writing this machine's record from setup and the collector (`unnamed-…` until named), and `agent-archive machines` (list only, no Cloudflare check yet). `machines rename`, with immutable IDs and ambiguity handling.
- **P2d.** `machines add`, using the shared key for R2 (labeled) and the profile for S3: the storage check first, the clipboard, the code on the alternate screen, the coding-agent refusal, the durable lifecycle ledger, atomic reservations, delivery recovery, and pending-pairing warning in `status`.
- **P2e.** `setup --pair`, `--pair-file`, `AGENT_ARCHIVE_PAIRING_CODE`, the coding-agent refusal, the destination-first review screen, portable exclusions, bounded project discovery, transactional success/retry behavior, the import offer, and the opt-in beta entry point. The first-run pairing question becomes general availability only after Phase 4.

### Phase 3: live acceptance of guided R2 creation (gate)

Run every box of "Live acceptance: guided R2 creation" in `dev/contributing/testing.md` against a real Cloudflare account, plus the new items under [things to verify](#open-questions-and-things-to-verify). Nothing in Phase 4 ships before this passes. This is the open item already recorded in [the portable handoff plan](portable-handoff-and-onboarding.md#outcome-as-built).

### Phase 4: per-machine keys and revocation

- **P4a.** Token sources (`CLOUDFLARE_API_TOKEN`, `cloudflare_token_command`, prompt) as one function both commands use.
- **P4b.** Spare keys: immutable recipient/issuer/slot identities, provider naming, atomic reservation and issuance lineage; created in guided setup and refilled by `machines add` only. `spare_keys`, `spare_credential_refs`, `--spares`.
- **P4c.** `machines add` creates a fresh key when a token is available, falls back to spares, and asks before sharing.
- **P4d.** `machines revoke`, `--include-issued`, independently verified ownership, per-key operation progress, self-revocation and concurrency recovery; `machines --verify` for explicit bounded provider checks.
- **P4e.** `machines own-key`, including removal of obsolete shared secrets after commit, so beta machines can move off the shared key.
- **P4f.** General availability only after paired setup, revocation, rename, delivered-spare ownership, recovery, exclusions, and performance acceptance pass together. Enable the first-run pairing question and update the public quickstart then.

### Later, not in this plan

- `machines rotate-key` to replace a shared key on every machine that uses it.
- A pairing flow where the new machine shows a public key and the old machine encrypts to it (see [Alternatives](#alternatives-considered)).

## Testing

- **Pairing format:** round trip; wrong code; each header field tampered (AEAD failure); truncated and mistyped bundles (CRC failure, distinct message); expired bundle, and one within the five-minute clock allowance; future `version` refused with an upgrade message; unknown Argon2id parameters and oversized bundles refused before any derivation (assert no large allocation); decompression stopped at its cap; code normalization (case, spaces, hyphens, three-letter prefixes, full words, a mix); a prefix that matches no word, caught before derivation; fuzzing of the decoder.
- **Wordlist:** every word has a unique three-letter prefix, the list has 1,296 entries, and its hash matches the published list.
- **Secrets never written:** after `machines add` and `setup --pair` in a temporary home, a grep over every file written (config, draft, journal, logs, the issued-key ledger, `status --json`) finds neither the code, the bundle, the R2 secret (outside the credential store), nor a Cloudflare token. Same shape as the existing assertion for guided setup's bootstrap token. The interactive path asserts that the code is written only between the alternate-screen enter and exit sequences.
- **Coding agents:** with each of `CLAUDE_CODE_SESSION_ID`, `CODEX_THREAD_ID` and `CURSOR_AGENT` set, `machines add` and `setup --pair` refuse, also with `--yes` and with `AGENT_ARCHIVE_NONINTERACTIVE=0`, and change nothing.
- **Redaction:** a transcript containing an `aa-pair1:` bundle is uploaded with it replaced by `[REDACTED]`, and the capture gap is recorded.
- **Revocation key selection:** rename then revoke selects the same immutable owner; forged bucket mappings are refused, also under `--yes`; delivered spares are distinguished from unused spares; third-machine revocation requires independent binding verification; no token deletes nothing and reports access not removed; `--include-issued` covers delivered descendants and unknown bindings; legacy keys never inherit trust from a matching label.
- **Project matching:** same path; different path same remote; no remote with same path; two clones of one remote (interactive selects, `--yes` skips); not found; a remote with credentials in its URL (the `repo_key` normalizer already strips them; assert nothing else carries them).
- **Machine records:** two machines in one `storagetest` store; revoke from a third; a record written by a newer schema version; a missing record backfilled by the collector; `heartbeat_at` written at most daily, never described as live activity.
- **Cloudflare:** against `internal/cloudflare/cloudflaretest`: key creation for a named machine, spare refill, `DeleteToken` for key and spares, the cross-check flagging an unknown key and a missing one, explicit verification without default token acquisition, a token without "Account API Tokens Write" (each 403 names the permission), rollback when verification fails.
- **Compatibility:** an older-format `config.json` without the new fields; a bucket with `machines/` read by the current `list`, retention and purge (they ignore it).
- **Lifecycle and concurrency:** interrupt/crash before and after each credential, ledger, delivery, and setup commit boundary; ambiguous delivery remains reserved; lost API responses reconcile by slot ID; two concurrent adds never issue the same spare; delivered bundles remain valid after later failures; pre-delivery cleanup failures remain visible; retries do not duplicate staged secrets; post-commit publication resumes after bundle expiry.
- **Revocation progress:** no token, per-key partial deletion, provider timeout and already-absent tokens, two concurrent revokers, self-revocation after its object credential stops working, and independent bucket-write failure. Assert truthful pending/partial/confirmed output and no spare creation during revoke.
- **Capture scope:** portable nested exclusions, unmappable exclusions, symlink/traversal rejection, local exclusions preserved on reconfiguration, and broad roots withheld under `--yes` when exclusion scope is unresolved.
- **Performance:** large app histories and slow Git/directory reads return partial results within one shared budget; candidate caps, path deduplication and Git concurrency limits; paginated machine/provider records, oversized/malformed JSON, bounded fetches, incomplete verification, and ordinary `machines` performing no session scan or password-manager call.
- **Migration:** `machines own-key` removes obsolete shared secrets only after replacement commits, retains rollback on failure, and reports explicitly any shared access retained for another local destination.
- **End to end:** the sandbox recipe in `dev/contributing/testing.md` with two `AGENT_ARCHIVE_HOME`s and two `HOME`s against an in-memory or MinIO store: add, pair, list, revoke.

## Documentation

- `docs/guides/multiple-machines.md`: keep settings transfer first until general availability; label the shared-key beta and its access limitations. After Phase 4 acceptance, lead with `machines add` and `setup --pair`; keep `setup --yes` for scripts.
- `docs/getting-started/setup.md`: the pairing question, `--pair`, `--pair-file`, `AGENT_ARCHIVE_PAIRING_CODE`.
- `docs/reference/cli.md` (regenerated) and `help machines`.
- `docs/reference/configuration.md`: the new fields and environment variables.
- `docs/reference/bucket-layout.md`: the `machines/` folder.
- `docs/security/privacy.md`: what a pairing bundle holds, how it is protected, that the code and bundle are never stored, that bundles are redacted from transcripts, that the machine list stores names and platforms, and where spare keys and the issued-key ledger are kept. What to do if a code may have been seen: revoke and add again.
- `dev/specs/privacy-filter.md` and `dev/specs/privacy-filter-changelog.md`: the `aa-pair1:` shape.
- `docs/guides/agent-skills.md`: that pairing refuses to run inside a coding agent, and why.
- `docs/security/bucket-permissions.md`: per-machine keys, and the token permissions `machines add` and `machines revoke` need.
- `docs/getting-started/uninstall.md`: revoke the machine's key from another machine; deleting the archive includes `machines/`.
- `README.md` quickstart line about a second machine.
- `CHANGELOG.md` per phase.

## Open questions and things to verify

**Design questions**

1. **Cryptography dependency.** `golang.org/x/crypto` for Argon2id and XChaCha20-Poly1305 (recommended), or the standard library only: PBKDF2-SHA256 (`crypto/pbkdf2`) with a high iteration count and AES-256-GCM. With the 62-bit code, the choice matters more than it did at 77 bits. PBKDF2 needs no memory, so GPUs guess it hundreds of times faster than Argon2id with 64 MiB: tens of thousands of GPU-years instead of millions. That is still out of reach, but with a much smaller margin. Argon2id is worth its one well-maintained dependency, and choosing PBKDF2 should mean going back to a longer code.
2. **S3 bundles without a code.** S3 bundles hold no secret. Skipping the code for S3 would make S3 pairing one paste, at the cost of the bucket name and repository hashes travelling in the clear and two flows to explain. This plan keeps one flow.
3. **Default spare count.** Two is enough for a laptop and a desktop added later. More means more dormant keys to track.
4. **Heartbeat and activity.** Decided: daily `heartbeat_at` is informational, labelled with its cadence. Session activity is a separate future field; ordinary machine listing never scans sessions.
5. **Pairing a machine that is already set up.** Treat it as a reconfiguration (keep `machine_id`, show changes on the review screen), or refuse and point at `setup`. This plan proposes reconfiguration, matching what `setup` does today.

**To verify against Cloudflare (Phase 3)**

- That tokens created without `expires_on` never expire (already listed for guided setup).
- `DELETE /accounts/{account}/tokens/{id}` for account-owned tokens (`Client.DeleteToken` exists; confirm live).
- Listing account-owned tokens (`GET /accounts/{account}/tokens`), its pagination, and the permission it needs, for the cross-check.
- Whether a token with "Account API Tokens Write" can be limited to creating only tokens for R2, or for one bucket. If it can, storing such a token becomes worth revisiting, since it would remove the spare-key machinery; until then, [D5](#decisions) stands.
- The rate of token creation Cloudflare allows, for refilling spares.
- Provider name length/character limits for full immutable IDs, uniqueness lookup after an ambiguous creation response, and whether listing exposes enough resource-scope data to verify bucket ownership.
- Deletion propagation and already-absent-token responses; do not promise immediate cutoff until verified.

**General-availability acceptance gate**

Rename then revoke, handed-out spare ownership, issuer-compromise recovery, interrupted and concurrent setup, per-key partial/self/concurrent revocation, capture exclusions, and the total discovery/listing budgets must pass together before advertising per-machine pairing and revocation. Record timings on representative macOS and Linux hosts; the under-a-minute goal is measured end to end, with manual AWS sign-in and external permission approval reported separately.

## Alternatives considered

- **Copy the data directory or `config.json`.** Carries `machine_id`; two machines would claim the same sessions. Already warned against in [multiple machines](../../../docs/guides/multiple-machines.md).
- **An unencrypted, secret-free profile with the key entered by hand.** Simpler, and still possible: `setup --yes` after Phase 1 is exactly that. Pairing exists to move the key without the person handling it.
- **A passphrase the person chooses.** People choose guessable passphrases, and the bundle can be attacked offline. A generated code is the same effort to type.
- **Six words from the EFF large wordlist (77.5 bits).** The first draft of this plan. It means twice the typing and no prefix shortcut, to protect a revocable key that 62 bits already puts beyond guessing.
- **The new machine shows a public key; the old machine encrypts to it.** Nothing could be guessed offline at all. But the person types a roughly 50-character string into the old machine (or scans a QR code) in the opposite direction from the bundle. With a 62-bit generated code behind Argon2id, the gain does not justify the friction. Kept as a later option.
- **A relay service (as Magic Wormhole uses) so only a short code is needed.** agent-archive has no server, and adding one is out of proportion.
- **Use the bucket as the mailbox for the bundle.** The new machine cannot read the bucket until it has a key, which is what the bundle delivers. A presigned URL could bridge that, but the URL is as long as the bundle and must be kept as private as one.
- **Store a key-creating Cloudflare token in the Keychain.** The smoothest experience, but the machine then holds a credential that can create tokens across the whole Cloudflare account. Spare keys give nearly the same experience with no such credential.
- **R2 temporary credentials.** Each machine would need a stored Cloudflare token to keep renewing them, which is the problem this plan avoids. Not verified against Cloudflare's current API.
- **A Cloudflare Worker that brokers bucket access per machine.** The cleanest per-machine control, but it replaces the S3 storage path and asks every user to deploy something.

## Review changes

Both reviews (2026-10-01) are incorporated above. First-review rationale:

| Earlier design | Adjustment and reason |
| --- | --- |
| Bucket records chose deletion keys | All machine keys can forge records; use independent provider/local ownership evidence. |
| Bundle/code printed together | Clipboard bundle + cleared alternate-screen code reduce retained exposure. |
| Agents could run pairing | Binary refusal prevents transcript leakage/injected upload destinations; redact pasted bundles and lead review with destination. |
| Header chose arbitrary KDF memory | Known parameter sets prevent unauthenticated memory exhaustion. |
| 77.5-bit large wordlist | ~62-bit short list permits three-letter prefixes with adequate guessing margin. |
| Shared-key users stayed shared | Add `machines own-key`. |
| Smaller UX gaps | Storage precheck, five-minute clock allowance, import offer, neutral legacy names, and explicit replay limits. |

### Second design review: ownership, recovery, and performance

Second review added:

- [Immutable ownership and compromise recovery](#which-keys-revoke-deletes): rename-safe IDs, delivered-spare distinction, issuer lineage.
- [Lifecycle](#pairing-lifecycle-and-recovery): atomic reservations, conservative delivery, interruption recovery.
- [Revocation progress](#revocation-progress-and-concurrency): per-key outcomes, self/concurrent requests, verified access status.
- [Capture scope](#project-matching): portable exclusions, clone selection, bounded discovery and partial results.
- [Listing](#reading): explicit provider verification, bounded reads, heartbeat distinct from activity.
- [UX](#user-experience): success after commit, active-command code redisplay, screen-capture limits.
- [Rollout](#phases-and-pull-requests): settings first, labelled shared-key beta, dedicated-key/revocation gate, no refill during revoke.
