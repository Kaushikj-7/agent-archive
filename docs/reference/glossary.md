# Glossary

Words agent-archive uses in its output and docs, in alphabetical order.

**Adapter.** The code that reads one app's transcript format (Claude Code
JSONL, Codex rollout JSONL, Cursor JSONL, text, or database chats) and hands
the filter records it understands. Its version is recorded with each capture
(`capture.adapter_version`); see [versions](../../dev/maintainers/versions.md).

**Admission.** The moment agent-archive took ownership of a session: a
hook's registration of a new session, or the import that backfill ran
(`admitted_at`). Project inclusion, storage destination, and retention age
before a first capture are all measured from it; see
[session eligibility](session-eligibility.md#imported-sessions).

**App, harness.** A coding agent agent-archive captures: Claude Code
(`claude`), Codex (`codex`), or Cursor (`cursor`). The docs say "app"; the
CLI's flags and output say `harness` (`--harness codex`, the `HARNESS`
column of `list`).

**Archive session ID.** The 32-hex-character ID agent-archive gives a
session, used in `list`, `show`, `handoff`, and object keys. The app's own
ID is kept as `native_session_id`.

**Batch (import batch).** One `backfill` run's set of imported sessions,
with an ID that `backfill history` lists and `backfill undo` takes; see
[backfill](../guides/backfill.md).

**Capture.** Reading a session's transcript, filtering it, and building its
source bundle and metadata on this machine. Capture is local; publication is the
upload.

**Capture gap.** A recorded, named piece of a session that is not in the
archive: a field the filter dropped, a redaction, a record over a size
limit, a transcript that could not be read. Gaps carry a code and names,
never the removed values. `list --complete` excludes sessions with any gap,
and `status --json` counts them per app. The codes are in
[`schemas/metadata.schema.json`](../../schemas/metadata.schema.json).

**Collector.** The background job (a LaunchAgent on macOS or a systemd user
timer on Linux, every 60 seconds; or `sync` on demand) that captures, publishes, reads back, and applies
retention.

**Destination.** The bucket and prefix sessions publish to. Each session
belongs to the destination that was current when it was admitted; changing
storage starts a new one and leaves earlier sessions where they are.

**Filter version.** The version of the privacy filter's rules
(`filter_version`). A new version refilters and republishes every session
whose transcript is still on the machine; see
[privacy](../security/privacy.md#after-a-filter-upgrade) and the
[filter changelog](../../dev/specs/privacy-filter-changelog.md).

**Hook.** A command an app runs at points in a session (start, prompt,
stop). agent-archive's hook records the session locally, typically in
tens of milliseconds, and never uploads anything itself.

**Included project.** A directory tree you chose in setup. Only sessions
whose working directory is inside an included project, admitted after the
project was included, are captured.

**Metadata (sidecar).** The small `metadata.json` object stored beside a
session's source: identity, app and versions, counts, models, skills, gaps,
and a pointer to the current source. `list` reads only these.

**Parser status.** How far the metadata's derived facts can be trusted:
`complete` (the adapter understands every retained record), `partial` (the
default: derived, but without that claim, so counts and models may be
incomplete), or `failed` (the source could not be parsed; only identity and
the source reference are filled in). Shown in the `PARSER` column of `list`.

**Publication.** Uploading a session: its source first, then its metadata
pointing at it. A session is republished whenever it changes, at most once
per upload interval (3 minutes).

**Read-back.** After publishing, the collector downloads what it uploaded
and checks its SHA-256 and identity against what it sent. An installed app
version is reported `verified_by_capture` (`installed_version_support` in
`status --json`) once a session from that version has been published and
read back on this machine; until then it is `unverified`.

**Refresh-skip.** A local note that this build cannot re-derive a session's
metadata from its uploaded source, or that the recorded source is gone, so
the collector stops retrying until the parser version or the source
changes. `status` counts these as unrefreshable summaries; see
[local state](local-state.md).

**Retention.** Automatic whole-session expiry: 90 days by default, 1 to 36,500
days for a finite policy, or **Keep forever** (`retention_days: 0`). Finite
retention ages from the last meaningful capture, including preserved revisions.
The owning machine applies its own policy; see
[multiple machines](../guides/multiple-machines.md). Forever still permits privacy
and superseded-source cleanup, explicit deletion and provider lifecycle expiry.

**Source bundle (source).** The gzip-compressed, filtered transcript of one
session, `source.<sha256>.jsonl.gz`, plus supplemental evidence (hook
observations and skill snapshots); see [bucket layout](bucket-layout.md).

**Supplemental evidence.** What the source carries besides the transcript:
hook observations (event names, turn IDs, models, a stop hook's final
message) and skill snapshots (installed `SKILL.md` names, hashes, and
filtered bodies).
