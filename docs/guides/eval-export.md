# Export sessions for evaluation

`agent-archive eval export` prints archived sessions in a documented,
versioned format for an evaluation tool to read: one JSON line per session,
built only from what agent-archive has already filtered. A tool that turns
your past sessions into a benchmark (pick the tasks, rebuild the repository
at the commit the session started on, replay the task with other agents,
grade the results) reads its input from here rather than parsing transcripts
itself.

```sh
# The cheap first pass: identity, commits, counts, tokens, tools, outcome.
# Reads only metadata sidecars, never a transcript.
agent-archive eval export --detail metadata 1f0c…e2 7a41…09

# The full record of the sessions worth building from: adds every human
# prompt in order, the final response, the edited files, and feedback.
agent-archive eval export 1f0c…e2
```

Session IDs come from `agent-archive list --json` (`sessions[].session_id`);
only full archive session IDs are accepted, never a title or a prefix, so an
export is always of exactly the sessions named.

## Without setup: transcripts on this machine

The same records can be made from the transcripts Claude Code, Codex, and
Cursor keep on this machine, with only the binary: no setup, no bucket, and
no data directory is created.

```sh
# Every transcript backfill would find here (the same filters as backfill).
agent-archive eval export --scan --detail metadata --since 30d

# One transcript file.
agent-archive eval export --file ~/.claude/projects/widget/1f0c.jsonl --harness claude
```

A local record has the transcript's path and its project folder, which never
leave this machine. It has no `git_head` (a transcript records no commit,
and none is guessed), no `replay` marker, and no feedback.

## Many sessions at once

```sh
# First pass: cheap metadata for everything; keep the IDs worth a closer look.
agent-archive eval export --scan --detail metadata > candidates.jsonl

# Second pass: full records for exactly those, in one process (kept.jsonl
# is the lines of candidates.jsonl the tool kept).
jq -r 'select(.record == "session") | .transcript_path // .session_id' kept.jsonl \
  | agent-archive eval export --ids-from - --detail full
```

`--ids-from -` reads archive session IDs and transcript paths from standard
input, one a line. Sessions are exported several at a time (`--workers`,
default the number of CPUs up to 8), and each record is written as its
session finishes, so match records on `session_id` or `transcript_path`
rather than by position.

## What a record holds

Every line is one JSON object with `"record": "session"` or
`"record": "error"`. The format is
[`schemas/eval-export.schema.json`](../../schemas/eval-export.schema.json),
and the [design record](../../dev/specs/eval-export.md) says what each field
means and how it may change. In short:

- **Identity and setup:** `session_id`, the app and its version (`harness`),
  `models` (with reasoning settings where the app records them), `project`
  (the project folder's name and the repository key, never a path),
  `branch`, and `git_head` (the commit the session started on, whether the
  tree was dirty, and the last commit a stop saw).
- **What happened:** `started_at`, `ended_at`, `state`, `turn_outcome`,
  `counts` (turns, tool calls, files touched, tokens, commits, …),
  `model_tokens`, `tools_used`, `mcp_calls`, `skills_used`, `git_activity`,
  and `capture_gaps`.
- **Replays:** a session a replay tool ran carries `replay`, so the tool can
  leave its own runs out.
- **At full detail only:** `prompts` (every human prompt, filtered, in order,
  whole), `final_response`, `files_edited` (in the order first edited), and
  `feedback` (what you attached with `agent-archive feedback`).

Unavailable tool, file and Git counts are omitted rather than reported as
measured zeros. Full detail omits `files_edited` when edited-file evidence is
unavailable; an observable session with no edits carries an empty list.
Muse retains qualified prompt/message counts. Nonempty enriched Muse sources
with qualified committed invocation evidence can also expose observed tool-call
counts. Zero-call or legacy-erased tool evidence remains unavailable, rather
than measured zero. Tool names, results/errors, files, Git and MCP attribution
remain unavailable; these finite counts do not establish approval-free read
session support or native capture. Cursor calls without recorded results keep
their invocation count, with the result count omitted.

## Size and errors

`--max-bytes N` (default 120000; `0` for none) bounds each record: the
longest texts are halved, never below 256 bytes and never dropped, then
edited files are dropped from the end, and `trimmed` says what was cut. A
session that cannot be exported (not found, its source missing or failing
verification, a parse failure) is an `error` record on its own line with a
`code` and a message; the other sessions are still printed, and the exit
code is 1. Invalid Git activity URLs in a sidecar produce `read_failed`
errors at both detail levels; the malformed URL is never copied into the
error message. A usage mistake exits 2 with nothing on standard output.

## Privacy

The export prints to standard output only, and uploads nothing. It holds
what `show --transcript` would show you (filtered prompts and replies), not
more: no tool results, no paths from the metadata, no credentials the filter
redacted. Treat it like the archive itself (see [privacy](../security/privacy.md)).
