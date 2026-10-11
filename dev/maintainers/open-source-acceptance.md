# Published-release acceptance record

Use this sheet for a **new signed tag built from the final documentation and
code commit**. The [release runbook](releasing.md) owns tagging, GitHub
settings, signing, and publication; this sheet tests the bytes a new user
downloads. A local `scripts/build-release.sh` binary does not qualify.
`v0.1.1` is an existing published release, not evidence for a later tag.

Run capture commands only as a disposable macOS user or on an isolated test
Mac, with a disposable **private** bucket and synthetic project, app session,
and user-level skills. Never point this run at a real archive or development
account. Keep the bucket until the deletion checks finish. Use one dedicated
prefix in an **unversioned** bucket, record its exact spelling, and make sure
no other Mac writes to it. The narrow S3 policy cannot list or remove
noncurrent object versions; if versioning is enabled, use separate administrator
credentials to inspect and remove them and record that cleanup separately.
Do not paste credentials, transcript text, `show --normalized` output, or
`handoff` output into the record.

## Test identity and prerequisites

**Provider for this record:** S3 / R2 (select one). **Bucket and main/upgrade
prefixes:** ______ / ______ / ______. Make a second, independent record for
the other provider; fill every result cell again.

Record: tester and UTC date; tag, release commit SHA, documentation commit SHA,
release workflow URL; Mac architecture and macOS version; Claude Code, Codex,
and Cursor versions actually used; provider, bucket region, dedicated prefix,
and policy revision. For S3, use the policy **exactly as printed** in
[bucket permissions](../../docs/security/bucket-permissions.md#amazon-s3),
replacing only bucket and prefix names. Do not widen `s3:ListBucket` after a
failure: record the error, then investigate it. Record S3 Block Public
Access evidence, or R2 public-access dashboard evidence, separately from
the setup connection test. For R2, use a token scoped to only this bucket.
Test each provider separately; S3 results do not establish R2 behavior.
Make a separate copy of this record for S3 and R2, with its own bucket,
prefixes, commands, and evidence; never carry a pass from one provider into
the other's record.
Before installing, record that this disposable user's `PATH` has no
`agent-archive`, and inventory the existing app hook files, agent-archive
data directory, `com.agent-archive.collector` LaunchAgent, and R2 Keychain
item (presence only, never values). A pre-existing item makes the clean-user
check pending until a fresh disposable user is available. Unrelated app
configuration is fine if it has no agent-archive hook entries; preserve it
for the uninstall comparison. Do not delete someone else's state to make the
account look clean.

Have the AWS CLI and `jq` available for the final purge check. Use a
dedicated AWS profile for S3. For R2, configure the CLI with a disposable
bucket-scoped token and the R2 endpoint shown in the
[purge recipe](../../docs/security/privacy.md#after-a-filter-upgrade);
do not put secrets in this record. Prepare a
synthetic repository with a distinct marker in its prompts and no private
files. Create one eligible synthetic historical session before setup and
decline setup's offer to import it, reserving it for the backfill check.
Begin hook capture with a fresh app session after setup; an old open session
does not test it.

## Execute and record

Set `tag` to the new published release, then install the pinned script and
asset into the disposable account:

```sh
tag=vX.Y.Z
curl -fsSL "https://raw.githubusercontent.com/wangjohn/agent-archive/${tag}/install.sh" |
  AGENT_ARCHIVE_VERSION="$tag" AGENT_ARCHIVE_INSTALL_DIR="$HOME/bin" sh
export PATH="$HOME/bin:$PATH"
"$HOME/bin/agent-archive" --version
uname -m
sw_vers -productVersion
```

For S3, use the **same narrow profile** setup uses to check the missing-key
response. Keep `prefix` identical to the setup prefix, including its trailing
slash (or leave it empty for a root prefix):

```sh
bucket=DISPOSABLE_BUCKET
prefix=agent-archive/
profile=DISPOSABLE_PROFILE
missing_key="${prefix}.setup-test/acceptance-missing-$(date +%s)-$$.json"
head_status=0
aws s3api head-object --bucket "$bucket" --key "$missing_key" --profile "$profile" || head_status=$?
printf 'head exit: %s\n' "$head_status"
aws s3api list-objects-v2 --bucket "$bucket" --prefix "$missing_key" \
  --profile "$profile" --query 'Contents[].Key' --output json
```

Replace `SESSION_ID`, `IMPORT_ID`, and `PROJECT` below with the values for
this run. A command passing is only the named check; record its observed
result and a sanitized log or screenshot reference in the table. If a command
fails, preserve its exit code and redacted error, mark **fail**, and do not
substitute a broader policy or a local build.

| Check | Action and pass condition | Result / evidence for this provider |
| --- | --- | --- |
| Release provenance | In [releasing](releasing.md#repository-settings-and-release-evidence), record live `main` protection, required checks, protected `release` environment, private reporting, labels, and the successful build/publish workflow for `TAG`. Verify both downloaded architecture assets against `SHA256SUMS`, their pinned Developer ID signatures, accepted notarization in the workflow, and `gh attestation verify` for both. Tag must point to the tested final commit. | pending — |
| Clean-user baseline | Before the first install, record `command -v agent-archive`, the presence or absence of the three app hook files, `~/.local/share/agent-archive`, `~/Library/LaunchAgents/com.agent-archive.collector.plist`, and the `agent-archive` Keychain service. Pass when no installation or capture state or agent-archive hook entry exists; record unrelated hook-file contents for later comparison. | pending — |
| Clean install | Run the commands above, recording the installed path. Version must equal `tag`; installer output must report checksum and pinned-team signature verification. If using the installer's default directory instead, follow its printed `PATH` guidance or invoke the printed path directly. Repeat on Intel and Apple Silicon where available; mark missing architecture pending. | pending — |
| Bucket privacy and narrow access | Confirm private access in the provider dashboard or S3 public-access API using an administrator identity. Run `agent-archive setup` with the dedicated bucket, exact policy, chosen app(s), and `PROJECT`. Its temporary object write/read/list/delete check must pass. Record whether setup reports verified private, not verified, or public. | pending — |
| S3 missing-key fallback | Run the exact commands above with the setup profile. Record `head-object` exit status and sanitized HTTP error (403 or 404), and `list-objects-v2` exit status plus empty result. A recorded 403 and successful exact-key list, together with setup's deleted-test-object read succeeding, exercise the fallback. If S3 returns 404, leave the 403 path pending. Keep the [untested-on-AWS caveat](../../docs/security/bucket-permissions.md#amazon-s3) until the exact policy and path have real evidence. Mark this row not applicable in the R2 record. | pending — |
| App-by-app first publication | For **each** of Claude Code, Codex, and Cursor available on the test Mac, confirm setup included that app and `PROJECT`, start a **new** session there, and send a synthetic prompt. In Codex approve hooks with `/hooks` first. Wait for the collector or run `agent-archive sync`, then `agent-archive status --verbose`. Pass that app only when its row says **archived, verified**; `Ready`, hooks installed, and storage connected alone are insufficient. Record each app separately and mark unavailable apps pending. If it stalls, use the [Claude Code](../../docs/guides/troubleshooting.md#no-claude-code-session), [Codex](../../docs/guides/troubleshooting.md#no-codex-session), or [Cursor](../../docs/guides/troubleshooting.md#no-cursor-session) path. | Claude Code: pending —; Codex: pending —; Cursor: pending — |
| Read path | Run `agent-archive list --no-pager`, `agent-archive show SESSION_ID`, `agent-archive show SESSION_ID --normalized`, and `agent-archive handoff SESSION_ID`. Confirm the synthetic marker and session identity in the verified archive, with no command error. Record IDs and checks only, not content. | pending — |
| Pause and resume | Run `agent-archive pause`; confirm `sync` does not upload while paused. Run `agent-archive resume` and `agent-archive sync`; confirm a registered synthetic change catches up and becomes read-back verified. A session begun only while paused is not a valid catch-up fixture. | pending — |
| Backfill and undo | Use the reserved **pre-setup** session. Run `agent-archive backfill --project PROJECT --dry-run`, review scope, then `agent-archive backfill --project PROJECT` and `agent-archive backfill history`. Confirm the imported session is verified; record `IMPORT_ID`. Run `agent-archive backfill undo IMPORT_ID --project PROJECT`, inspect its plan, and confirm only that import's objects disappear while the hook-captured session remains. | pending — |
| Retention | On this disposable installation only, choose a one-day retention in setup and use a controlled eligible old synthetic session or test clock fixture. Run `sync` after the retention boundary; confirm expiration removes its metadata and source while a newer session remains. Record how time was controlled. If a trustworthy boundary test is unavailable, leave pending; absence of an old object alone is not proof. | pending — |
| Uninstall and reinstall | Run `agent-archive uninstall`, confirm this installation's hooks and LaunchAgent are removed and the bucket remains. Run setup again with saved answers, then verify a fresh synthetic session. Run `agent-archive uninstall --delete-local-data` only after all pending uploads are resolved; confirm its owned local data and R2 Keychain item are removed, unrelated hooks/files stay, and the bucket remains. | pending — |
| Published-tag upgrade | In a **second** disposable user/prefix, install and set up published `v0.1.1` with synthetic data, then run the pinned installer for the new `tag` over it. Confirm `--version` changes to `tag`, saved settings/hooks/LaunchAgent still point at the installed binary, and a new synthetic session publishes and reads back. Record both installer logs and versions; do not use a local build as the source. | pending — |
| Safe bucket purge | Stop every writer to this disposable bucket, including the upgrade test user. For **each** test prefix, follow the [full-prefix preparation and apply recipe](../../docs/getting-started/uninstall.md#delete-the-archive-in-the-bucket) in one bash or zsh shell. Review **every planned key**, apply within five minutes, then list each prefix and confirm it is empty. Record key counts, both cleanup results, and any partial failure; never use a production prefix. For a versioned exception, record a separate administrator-led inspection and cleanup of noncurrent versions and delete markers for both prefixes. | main: pending —; upgrade: pending — |
| Final evidence | Attach passing pull-request checks from [Test](../../.github/workflows/test.yml) and [Levenshtein](../../.github/workflows/levenshtein.yml), and the final commit's full Linux and macOS race, performance, and script checks from [Extended](../../.github/workflows/extended.yml). Record the latest green nightly or manually dispatched fuzz and real-systemd runs on that commit; if their SHA differs, mark them pending until rerun. Also attach the release workflow, both asset digests, signature/notary evidence, and two attestation results. Record a separate pass/fail/pending for S3, R2, each Mac architecture, each app, and the fresh-user walkthrough. | pending — |

## Decision

For every row, replace `pending —` with **pass**, **fail**, or **pending**,
followed by UTC time, sanitized evidence location, and the tester. A failure
needs a linked issue or fix commit and a repeat on a **new signed tag** if it
changes release-bound code or docs. A pending live check is not a pass. The
test shows only the provider, policy, app versions, architecture, and account
configuration recorded above; preserve the AWS/R2 caveat for any path not
actually exercised. The primary maintainer records a final go/no-go with
every unresolved failure and coverage gap named.

## Deferred native name comparisons

Real Codex sidebar and Claude CLI picker comparisons against archive list/show
remain **PENDING**. Use the [manual comparison protocol](native-session-name-comparisons.md)
and its [evidence template](acceptance/native-session-name-comparison-template.md);
record UUID-matched observations separately for each producer, storage form and
lookup mode. The follow-up package and disposable lookup timing probes do not
establish real title parity or desktop coexistence.

## CLI reliability and retention release follow-up

The [CLI reliability and keep forever implementation plan](../proposals/cli-reliability-and-retention.md)
is being delivered in separate implementation and review PRs. This work does not
fill the pending provider, signed-release or actual-app results above. On
2026-10-11, the latest promoted release remains v0.1.1 with macOS assets only;
current-checkout and source-build checks do not establish acceptance for a new
published release.

Before promoting a release containing these changes, extend each disposable
provider record to cover both finite and forever retention, paired-machine
preference preservation, and an explicit forever-to-finite change with its
expiration preview. Include active and preserved history in the cleanup and
read checks, and recovery after native import files disappear. Record supported
macOS/Linux architecture coverage separately. Keep the existing results pending
until the final release bytes and that provider's behavior have actually been
exercised; do not carry focused unit-test results into these live result cells.
