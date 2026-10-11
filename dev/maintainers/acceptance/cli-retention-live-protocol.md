# Live retention and durable import acceptance extension

Use alongside the [published-release acceptance sheet](../open-source-acceptance.md),
which owns signed bytes, clean install, upgrade, actual-app capture and uninstall.
Create independent copies for **S3** and **R2**. Keep every result pending until
observed; no provider or architecture inherits another's pass. Record tester,
UTC time, release tag/SHA, documentation SHA, installed path, binary digest,
provider/private bucket/prefix, policy revision, OS/architecture and app versions.
Keep credentials, pairing bundles/codes and transcript content out of evidence.

## Prerequisites and isolation

A maintainer must supply the final signed release and installer evidence, a
private disposable bucket with the exact documented scoped policy, and two
disposable users/machines running compatible new binaries for pairing. Use a
synthetic repository and synthetic native sessions, including a Codex parent
with at least two referenced meaningful rollout revisions. For Linux, use a
disposable VM/container with real systemd and a supported user bus; never run
against your own login's manager. Use native supported macOS accounts to test
Keychain/launchd and actual apps. Enumerate each advertised platform separately;
missing signed Linux assets cannot be replaced by a source build in release
acceptance.

Keep the collector installed and observe its timer/launchd runs. Reserve separate
finite, forever, pairing, native-loss and upgrade fixtures/prefixes to avoid cross-deletion;
record which installation owns each session. Disable provider lifecycle rules
for the retention comparison and record that fact. A separate documented
lifecycle rule review must confirm that such rules can override forever. Stop
all writers before final destructive bucket cleanup.

Prepare old/new capture-age fixtures spanning a one-day cutoff, a preserved
revision with a newer meaningful capture, a newer active session, another
installation's session and paused/pending work. Use a controlled disposable
clock only if documented and trustworthy; record actual meaningful capture
ages and the method. Do not silently edit remote metadata or use mtime as the
capture age. Without reproducible cutoff fixtures the boundary cells stay
pending. Unit-test clocks establish synthetic behavior, not a live provider pass.

## Execute for each provider

The setup below edits an already configured disposable installation. Select the
provider/bucket/project with the release sheet first. Substitute only this run's
IDs/paths; review interactive output and record exit statuses and sanitized
observations. `status --json` retains numeric `retention_days: 0` for forever.

```sh
agent-archive setup --yes --keep-forever
agent-archive status --json
agent-archive setup --refresh
agent-archive status --verbose
agent-archive backfill --project PROJECT --dry-run
agent-archive backfill --project PROJECT
agent-archive backfill history
agent-archive show SESSION_ID --revisions --json
agent-archive show SESSION_ID --revision REVISION_ID
agent-archive handoff SESSION_ID --revision REVISION_ID
```

For native-loss publication, reserve a distinct synthetic project
(`NATIVE_LOSS_PROJECT`) and fresh native session/revision IDs. Keep their native
files outside every configured app/discovery directory until the collector is
stopped. They must not be exposed to setup's automatic import, the foreground
backfill above, hooks or earlier collector runs. Reusing the earlier project
and sessions cannot prove this check: foreground import publishes them and
backfill skips accepted registrations.

Stop only this disposable installation's scheduler while leaving capture
unpaused, so admission can run without a background upload.
On its disposable Linux user manager run:

```sh
systemctl --user stop agent-archive-collector.timer agent-archive-collector.service
```

On the disposable macOS account stop the owned LaunchAgent:

```sh
launchctl bootout "gui/$(id -u)" "$HOME/Library/LaunchAgents/com.agent-archive.collector.plist"
```

Confirm the owned job stopped; do not proceed while an in-flight collector or
other writer remains. Only now place the reserved synthetic native files in
the disposable app directories; do not launch an app or invoke a hook. Run the
dry-run below and verify that those fresh IDs are eligible new imports, not
already registered/archived. Confirm no corresponding remote session metadata
exists. If either check fails, reserve new IDs and repeat before proceeding.

Run flagged background backfill, which admits/stages without foreground
publication. Record the new import batch and its archive session/revision IDs
as `NATIVE_LOSS_SESSION_ID` and `NATIVE_LOSS_REVISION_ID`. Verify those sessions
were staged/admitted by this batch, have pending remote verification and still
have no published metadata. An empty import or a previously published session
does not satisfy this check. Then pause, move only the reserved synthetic native
files to a private backup, refresh the scheduler and resume:

```sh
agent-archive backfill --project NATIVE_LOSS_PROJECT --dry-run
agent-archive backfill --project NATIVE_LOSS_PROJECT --background
agent-archive backfill history
agent-archive pause
# Move this run's synthetic native files after admission; record the action.
agent-archive setup --refresh
```

Refresh preserves an unloaded scheduler's state; `resume` changes capture's
pause setting and does not load the job. While still paused, explicitly restart
only this installation's scheduler. On its disposable Linux user manager run:

```sh
systemctl --user start agent-archive-collector.timer
```

On the disposable macOS account load the owned LaunchAgent again:

```sh
launchctl bootstrap "gui/$(id -u)" "$HOME/Library/LaunchAgents/com.agent-archive.collector.plist"
```

The scheduler commands above use the default installation's names. If the
disposable fixture uses a separate data directory, use its recorded owned unit
name or LaunchAgent path for both stop and start; confirm ownership before
running either. Confirm the timer/LaunchAgent is loaded, then resume capture:

```sh
agent-archive resume
agent-archive status --verbose
agent-archive show NATIVE_LOSS_SESSION_ID --revisions --json
agent-archive show NATIVE_LOSS_SESSION_ID --revision NATIVE_LOSS_REVISION_ID
```

Never move real app history; for Cursor use a disposable synthetic database.
Confirm the timer/launchd run restarts and wait for its publication before the
read checks. If staging/admission before remote verification cannot be proved,
leave this cell pending. Do not infer native-loss durability from an import
already uploaded before files moved. Verify every active/preserved revision,
not merely source-object existence. A later `sync` can diagnose failures but
does not supply the background-run pass. For controlled crash/restart coverage,
use the existing checkpoint-injection tests; manually interrupting at an unknown
boundary is not proof of that boundary.

On the forever sender, run `agent-archive machines add`, follow its provider's
pairing procedure, and receive on the second disposable machine with
`agent-archive setup --pair-file PAIRING_FILE`. Keep the code private. Verify the
receiver's reviewed preference, saved zero and writer fence before its first
collector publication. Test interrupted/cancelled receive and retry in a fresh
receiver account, preserving the original bundle only within its validity.

On a separate finite fixture, configure one day. On the forever fixture perform
an explicit shortening only after recording survival. Run plain interactive
setup in a terminal, choose **How long sessions are kept** (`retention`), then
**Custom duration** (`custom`) and enter `1`. Inspect its deletion preview before
confirming; first decline and verify the saved preference is still forever,
then repeat and confirm:

```sh
agent-archive setup
agent-archive status --json
```

Wait for the background sweep
past the controlled boundary and read/list the session IDs. For scripted explicit
flags on a separate fixture, use `agent-archive setup --yes --retention-days 1`:
answer flags require `--yes`, which approves the reported effect without an
interactive confirmation. That scripted trial does not replace the reviewed
shortening trial above. Do not change
the sender's preference and expect the paired receiver to change remotely.

## Result ledger (one copy per provider)

| Check | Observable pass condition | Result / evidence |
| --- | --- | --- |
| Default and finite expiry | Fresh setup defaults to 90; one-day fixture expires only eligible owned old sessions and their references/local retention state. Newer, other-destination/owner and pending fixtures follow the documented sweeper rules. Record before/after IDs and boundary evidence. | pending — |
| Forever lifecycle | Saved zero survives repeated setup, guided editing, refresh and restart. Existing owned old sessions and active/preserved revisions remain readable after the same boundary; their local state remains. Capture stopped alone does not expire them. | pending — |
| History-aware shortening | Forever-to-finite review predicts eligible sessions using meaningful history capture age; no preview deletion. Confirmed background expiry matches prediction, including exactly-at-cutoff and preserved-newer cases. Paused/pending cases follow sweeper rules. | pending — |
| Pairing | S3 settings transfer / R2 dedicated-key path preserves zero and protection before first hook/collector run; cancelled/interrupted receive rolls back safely and retry preserves forever. Record sender/receiver versions separately. Later sender finite edit does not change receiver. | pending — |
| Revisions | Every referenced meaningful revision has stable inventory identity/provenance, and show/handoff selection returns the matching synthetic fixture. Unknown/unreferenced selectors fail; default remains active; selection is parent-only. Record IDs and comparisons without content. | pending — |
| Native-loss background publication | Use fresh IDs withheld from setup, hooks and earlier imports until writers stop. Prove new stage/admission and pending verification before upload and native removal; an empty/reused import cannot pass. Background run publishes and read-back verifies active plus preserved revisions with no native files. Restart/interruption retains recoverable ownership; prior published revisions remain readable. | pending — |
| Import/undo retention | Finite-to-forever import records the change; retry preserves it. Ordinary undo and `--yes` keep current forever. Separate explicit `--restore-retention` trial previews other eligible sessions and restores finite only when the saved policy was not independently changed. Explicit undo removes only its owned import. | pending — |
| Maintenance under forever | In isolated fixtures, unreferenced superseded cleanup and verified privacy replacement still run; all live history references survive. Explicit purge/undo remain available. For recipe apply stop every writer and follow the existing reviewed plan/retry protocol. | pending — |
| Compatibility | Representative published integer/protected decoders and actual old pairing receiver reject forever without writes; retention-only predecessor rejects staging protection. Use private temporary homes and fake scheduler/store for command probes, never the live accounts. Record binaries/digests and each named test execution or skip. | pending — |
| Platform/app coverage | List macOS Intel/Apple Silicon and each advertised Linux architecture, native scheduler, installer and app separately; run the parent release sheet on each supported combination. Missing hardware, app or asset remains pending. | pending — |

Run compatibility probes from a checkout of the recorded release SHA with Go
1.27.2. Supply absolute paths to the actual applicable predecessor binaries;
record their source/tag and digests separately. The published writer script is
native macOS only and validates the pinned v0.1.1 asset:

```sh
bash scripts/test_published_writer.sh
AGENT_ARCHIVE_OLD_CLI=/absolute/path/pre-forever-cli \
  go test -count=1 -v ./internal/pairing -run '^TestOldReceiverRejectsForeverBeforeSideEffects$'
AGENT_ARCHIVE_SOURCE_OLD_BINARY=/absolute/path/prior-scalar-schema-writer \
  go test -count=1 -v ./internal/config -run '^TestSourceBuiltWriterRefusesProtectedConfiguration$'
AGENT_ARCHIVE_PRE_FOREVER_BINARY=/absolute/path/pre-forever-protected-writer \
  go test -count=1 -v ./internal/config -run '^TestPriorRetentionWriterRefusesForeverBeforeMutation$'
AGENT_ARCHIVE_RETENTION_CLI=/absolute/path/retention-only-schema8-cli \
  go test -count=1 -v ./internal/cli -run '^TestStagedEvidenceRefusesRetentionOnlyWriterBeforeMutation$'
```

The source-built scalar probe requires a writer with an integer-only schema
decoder; it cannot test an already protected decoder. The separate protected
probe targets pre-forever source commit
`20f86e5f33fde4c720f3ca3554f4a234828bd7a3`, which supports fences through
version 7. Its finite and supported discovery controls must successfully pause
the intended configuration; forever, sticky finite-after-forever and composed
staging/forever fixtures must refuse with its actual unsupported-writer-fence
diagnostic and leave the entire disposable fixture tree unchanged. Record build
source and digest alongside the result; a source build does not replace the
published v0.1.1 probe.

A skip, missing fixture binary or command matching no test leaves that check
pending. These tests use isolated homes; their pass establishes command refusal,
not live receiver installation or provider acceptance.

Compatibility probes and synthetic crash/quota/policy-change coverage are
specified in [testing](../../contributing/testing.md) and the
[implementation evidence](cli-reliability-and-retention-2026-10-11.md).
The [Linux harness](../../../scripts/acceptance/linux/README.md) can supply
source-build MinIO/systemd evidence for its enumerated checks; it does not
exercise real S3/R2 policies, signed installers, this entire retention protocol,
or actual app capture. Preserve that distinction in the ledger.

Finish the parent sheet's uninstall, published-tag upgrade and full-prefix cleanup
for **both** provider records, including the paired installation and every fixture
prefix. The maintainer records go/no-go with every failed/pending cell, exact
artifact and platform limitations. This protocol authorizes no signing, tagging
or promotion by itself.
