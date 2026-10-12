# Local state

agent-archive keeps its local state in one data directory:
`~/.local/share/agent-archive`, or the directory `AGENT_ARCHIVE_HOME` names
(see [configuration](configuration.md#environment-variables)). The directory
must not be inside a Git checkout. Nothing here is uploaded as a file; what is
uploaded is built from it and described in [bucket layout](bucket-layout.md).
Files are private to your account and written atomically (temporary file,
fsync, rename, directory fsync).

On macOS, R2 credentials are not here: they are in the Keychain under the
service `agent-archive`. On Linux, which has no Keychain, they are in `credentials/<reference>.json` in this directory,
each file mode 0600 in a folder mode 0700 that agent-archive refuses to read
if it is open to other users (see
[where credentials are kept](../security/privacy.md#where-credentials-are-kept)).
S3 credentials stay in your AWS profile.

## Who owns what

- **`internal/state`** owns the session files: registrations, requests,
  published and pending publications, change detection, the native-session
  index, subagent candidates, removal records, refresh-skips, and
  `status.json`. Its `Store` holds the rules for each file and the
  per-session locks (`request-locks/`) that keep a hook and the collector
  from interleaving. A hook waits only a second for one, so writers sync to
  disk outside it: a writer syncs its temporary file first and holds the lock
  only to check and rename. A hook that another writer overtook meanwhile
  holds it for its next attempt; any other writer retries later instead.
  Subagent candidates are written the same way under their own locks. `state.OwnedEntries()` lists every
  top-level entry it can create. It never reads transcripts or talks to storage.
- **`internal/config`** owns `config.json`; **`internal/cli`** owns setup's
  files, logs, diagnostics, the collector-lock record, and `handoffs/`;
  **`internal/backfill`** owns `imports/`; **`internal/reader`** owns
  `cache/`.

## Configuration and setup

| Entry | What it is |
| --- | --- |
| `config.json` | The configuration: storage, projects, apps, retention, pause, and on Linux a digest of the machine ID setup first ran on (`host_id`, never uploaded), which lets `status` and `setup` notice a copied data directory. See [configuration](configuration.md). |
| `setup-draft.json` | Setup's saved progress, so an interrupted setup can continue. Holds no secrets. |
| `setup-transaction.json` | Present only while setup is changing files, or after it was interrupted; see [troubleshooting](../guides/troubleshooting.md#an-interrupted-setup). |
| `application-versions.json` | Installed app versions setup found. |
| `purge-plans/<id>.json` | Private 0600, five-minute remote deletion plan with exact keys, sizes, destination, and digest. |
| `purge-plans/<id>.json.report` | Private 0600 progress report written after each deletion, so a partial apply can resume before plan expiry. |

## Sessions (`internal/state`)

| Entry | What it is |
| --- | --- |
| `registrations/<id>.json` | One per captured session: archive session ID, native ID, project, repository key (a hash of its git origin, when it had one), the commit its working directory started on (with whether the tree was dirty) and the last commit a stop hook saw, app, transcript path or Cursor chat ID, start and admission times, origin (hook or import), destination, and, for a session a replay tool ran, its replay marker. |
| `requests/<id>.json`, `request-locks/` | Hook evidence waiting for the next collector pass, and the per-session locks around it. |
| `sessions/<hash>.json` | Index from a native session ID (hashed, since it is app-controlled input) to its archive session ID. |
| `sessions/<id>/` | Per-session evidence, such as `verification.json` (the last read-back check). |
| `subagent-candidates/` | Subagent transcripts a hook reported, waiting for the collector to validate them. |
| `pending-scans/`, `scan-signatures/` | Transcript reads in progress, and what each transcript looked like at its last read (and which last commit its published metadata names), so an unchanged one isn't read again. |
| `pending/<id>.json` | A publication frozen before its first upload: the exact source and metadata bytes, so retries are byte-identical. |
| `published/<id>.json` | The last bundle built and the last one published, with the uploaded source's key, SHA-256, and size, and its metadata. |
| `superseded/<id>.json` | Earlier source objects of the session, oldest first. Filter-version predecessors carry a privacy-sensitive marker; retention removes them after verified republish and a 24-hour reader grace interval, retrying failures. |
| `refresh-skips/<id>.json` | A session whose metadata this build can't refresh (`metadata_underivable`, or `source_unavailable` when the recorded source is gone), with the parser version and source key it applies to. It is not retried until either changes; `status` counts these as unrefreshable summaries. |
| `forgotten/<hash>.json` | A record of each session retention or undo removed, so backfill doesn't import it again. |
| `status.json` | The collector's last pass: scan and publish times, counts, errors. |

**Pending sessions.** A session is *pending* when these files say the
bucket doesn't have all of it yet: a request is queued, a scan was started
and never finished (`pending-scans/`), a publication is built and not yet
accepted by storage, in flight or waiting for the upload interval
(`pending/`), the last candidate built is waiting for that interval
(`published/` says `rate_limited`), or nothing was ever captured for it.
A recorded capture gap or a policy decline is not pending: nothing more can
be done until the transcript changes. Every count uses this one definition
(`state.Store.Outstanding`): `status`'s pending count and its imported
"waiting to upload", `backfill`'s upload progress and history, the
destination check in `setup`, the warning in `uninstall`, and the collector.
Retention asks the same files a narrower question: it doesn't expire a
session with a queued request or a publication storage hasn't accepted
(unless it's a chat that never received its transcript, or one the
configuration no longer publishes). An unfinished scan or a rate-limited
cache doesn't hold expiry back, because a scan that fails on every pass
leaves both in place for good, and a session never captured ages from when
it was registered. A session only waiting for its
transcript is pending but doesn't hold `setup` at the current destination,
since no sync can publish it.

**Quarantine.** A session file a pass can't parse at all (truncated, empty,
not JSON) is re-read under its lock, then renamed
`<name>.json.<UTC time>.corrupt` and reported for that session only, so it
stops failing every pass. It is kept for you to inspect; `status --json`
lists it under `quarantined_files` until you delete it. A file of valid JSON
but an unexpected shape (for example one a newer version wrote) is reported
and left in place.

## Everything else

| Entry | Owner | What it is |
| --- | --- | --- |
| `imports/<id>.json` | backfill | One per import. See [backfill](../guides/backfill.md#import-files). |
| `storage-health.json` | cli | The last storage access check. |
| `capture-diagnostics.json`, `diagnostics.lock` | cli | Content-free records of sessions a hook declined, for `status`. |
| `admission-intents/`, `admission-intents.lock` | cli | Private, bounded retry records for proven first starts and Cursor response/stop transcript paths. The collector revalidates and replays them. They include a native session ID, destination ID, and transcript path, but no conversation text or raw hook payload. A Cursor follow-up alone cannot admit a session. |
| `collector.log`, `collector-error.log` | cli | The background collector's output. The error log is trimmed in place to its most recent part once it grows past a limit. |
| `collector-lock.json` | cli | Which command holds `collector.lock`, its process ID, and since when; `status` uses it to report stuck collection. |
| `cache/` | reader | A disposable cache of metadata sidecars for `list`, `show`, `stats`, and `handoff`, which also sets how a large listing is split into parallel ranges. Safe to delete. |
| `handoffs/` | cli | Untrimmed handoffs, and untrimmed `show --transcript` output (`*.transcript.txt`, `*.transcript.json`), saved when output was trimmed, and the copies `handoff --to` launches agents with (one `launch-*/` folder each); removed after 7 days. |
| `collector.lock`, `hooks.lock`, `setup.lock` | local | File locks coordinating processes. |

## What uninstall removes

`agent-archive uninstall --delete-local-data` removes every entry above,
the three lock files last: it holds them while deleting everything else, so
no process can start work in a half-deleted directory, then unlinks them
before releasing them. It also removes leftover temporary files from
interrupted writes. Anything else in the directory is kept and reported, and
the directory itself stays if anything is left. A test keeps uninstall's
list in step with `state.OwnedEntries()`.

## Outside the data directory

- On macOS, `~/Library/LaunchAgents/com.agent-archive.collector.plist` (or
  `com.agent-archive.collector.<hash>.plist` for a non-default data
  directory).
- On Linux, `~/.config/systemd/user/agent-archive-collector.service` and
  `agent-archive-collector.timer` (or `agent-archive-collector-<hash>.*` for
  a non-default data directory), each mode 0600, and the link that enables
  the timer, `~/.config/systemd/user/timers.target.wants/agent-archive-collector.timer`.
  The collector's logs stay in the data directory (`collector.log` and
  `collector-error.log`).
- The hook entries in each app's hook file, recorded in `hook_files` in
  `config.json`.
- A Cursor database copy, only while a read of it is in progress with Cursor
  running. On macOS: `agent-archive-cursor-<uid>/cursor-snapshot-*` in your
  per-user temporary folder (`getconf DARWIN_USER_TEMP_DIR`). On Linux:
  `cursor-snapshots/` (mode 0700) in `agent-archive/` under `$XDG_CACHE_HOME`
  (else `~/.cache`, the account's home), beside a `CACHEDIR.TAG` that tells
  backup tools to skip it; never `/tmp` and never the data directory, which
  may be backed up or synced. The copy is removed when the read ends, and an
  abandoned one is swept by the next read.

## Discovery state

Discovery keeps a bounded source catalog and enumeration cursor, plus a
content-free health snapshot. These are scheduling hints, not identity or consent
authority. Namespaced native identities and their registration journal prevent
duplicate logical archives across hooks, discovery and deliberate imports.
Deleting a scan cache may cause another bounded scan; it does not grant permission
to capture old history. `discovery-refused-starts.json` lists the top-level
Codex sessions discovery refused for starting in the 8 days before its start
floor (at most 256, each with its native ID, start time and rollout path, no
conversation text) so `status` can point at `backfill` for them; the scan
drops a session once it is archived, removed, excluded or its rollout is gone,
and the file once it is empty. Preserve protected configuration and identity state when
disabling discovery; an older binary refuses protected configuration.

## Archive generations

`generation-heads/` holds one small qualified-native-identity head, naming its
unique active archive ID, a retirement marker, or an interrupted transition.
Hooks read this bounded head and the selected registration, never enumerate
or decode the complete chain. They also read at most 4097 bytes from each of
the selected generation's own and predecessor receipts; unfinished journals
fail closed before their publication body is decoded. `generation-nodes/` retains immutable identity
nodes, and `generation-recovery/` retains transition journals which become
small immutable successor receipts after completion. These identity records
contain no conversation content after a transition completes. They survive
content retention so index reconstruction cannot route to a frozen ancestor.
Unknown or damaged lineage fails closed.

Settled frozen generations use a retained-maintenance scan signature, separate
from native file stats. A pass checks bounded lineage authority, derivation and
privacy versions, recorded HEAD and outstanding work before skipping them.
Unchanged frozen history costs no source decode or per-session scan journal;
feedback, policy or version changes and interrupted publications still trigger
maintenance from retained evidence.

History publication also freezes the consumed native scan signature in its
existing pending journal. Only exact acknowledged publication installs that
signature; a newer source still fails its equality check. Retained privacy
successors clear the native proof because they did not consume current native
input under their new policy. This keeps both same-pass and interrupted
publication cheap after restart without acknowledging newer work.

A collector pass shares one 256 MiB logical data ledger across native/index
reads, retained compressed and decoded inputs, filtered output, comparison and
encoding scratch, and local pending/published input. It charges bytes held in
memory, never the length of a transcript read through its descriptor: a
rollout is streamed a record at a time, so its charge follows its longest
record and what the filter keeps. Each session's scan may hold at most
160 MiB of it; the other 96 MiB holds what the pass keeps between sessions (cached rollout headers and prefixes, Codex coverage and catalog), so a session within its share always fits and one large session cannot leave the others without room. A
session that needs more than that is recorded once as a `transcript_too_large`
gap naming the per-session read limit; one refused only because other owners
held the ledger is deferred (`read_budget`) to a later pass. Reservations precede the
bounded reads or output accumulation and belong to the data's consumer until
release. Resource refusal leaves valid journals and requests pending; it is not
corruption. This ledger bounds charged data, not Go map/allocator overhead or
process RSS. Those are measured separately. Source framing preflights add
bounded reads to measure the actual largest line before scanner allocation;
they do not persist native content. Small retained refilters grow their output
charge per safe row instead of reserving the maximum output ceiling.


Recovery holds `collector.lock`, then `hooks.lock`. Registration, request and
index commits use the existing staging helpers: disk syncs occur outside
request locks, and membership commits retain request-before-membership order.
The journal precedes the routing fence, immutable nodes, old capture freeze,
successor registration, fixed pending publication, fixed request token, index
redirect, active head and completed receipt. A collector resumes interrupted
transitions before its qualified index census and native scans. Every retry
uses the same successor ID, capture time and frozen publication bytes.

Retention stages active-tip retirement before its request lock, commits that
head before removing registration/index, and syncs after releasing the lock.
Earlier frozen generations expire independently. A genuine fresh start can
create an unrelated root after tip retirement; immutable prior-root membership
lets the collector validate remaining historical registrations while keeping
that separate from published transcript continuity. Imports keep their original
batch, admission and destination, so batch undo continues to select all of its
generations.

The packed native-identity census validates generation lineage before selecting
the active tip; linked generations are not unrelated duplicate owners. Queued
children may still name a retained frozen parent. A journal or validated lineage
census can replace an obsolete packed predecessor through a fenced per-key
override, while ordinary hook lookups retain their fail-closed checks. Active-tip
expiry retires generation routing before removing content, then prunes packed
identities after request locks release. Retired generations use an explicit
absence override rather than redirecting native hooks to a retained ancestor.

The scheduled collector primes qualified history requests from accepted existing registrations before opening native snapshots. The CLI advances one shared inventory observation and validation slice with the same lookup deadline and read budget. That slice cannot admit owners; scheduled IDs never replace provider identity, confinement, project or consent checks. Preview planning remains read-only.

Explicit generation recovery previews use the same trusted Codex homes and
lookup as capture. Preview cleanup discards newly requested coverage and leaves
the capture catalog unchanged. Filtered preview and returned publication data
retain their shared lease through confirmation; cleanup returns it on refusal,
cancellation, error or completion. Confirmation charges full recovery-journal
reads and encoding, and validates canonical source bytes by streaming their
checksum rather than retaining another compressed source.

Scheduled startup replays generation journals under the shared logical ledger,
using a bounded local fallback before a native lookup exists. Each journal's
initial view ends before its locked reread, and completed receipts release their
input before the next journal. Resource refusal preserves that journal and
reports pending recovery while smaller journals and unrelated captures continue.
Malformed schemas and caller cancellation still stop recovery. Lookup revision
digests reserve both encoded owners; a refused digest leaves the requested
coverage attempt undelivered for retry.
