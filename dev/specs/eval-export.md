# Eval export: sessions as evaluation task candidates

> **Status: implemented** (`internal/archive/eval_export.go`,
> `internal/cli/eval_export.go`): the format and schema, archived export,
> local mode (`--file`, `--scan`), and bulk selection (`--ids-from -`,
> `--workers`). Written 2026-09-30.
> The contract is [`schemas/eval-export.schema.json`](../../schemas/eval-export.schema.json)
> and the goldens in `internal/archive/testdata/eval-export/`.

## Purpose

A separate tool (agent-backtest, Python, built on Harbor) turns a person's
past sessions into a personal benchmark: pick sessions, rebuild the repository
at the commit the session started on, replay the task with other agents and
models, and grade the results against what the person asked for and how they
corrected the agent. It must not parse native transcripts itself: agent-archive
already has an adapter per app, tracks their format changes, and applies the
privacy filter. So agent-archive exports what the tool needs, in one
documented, versioned format, from two sources:

| | Archive | Local |
| --- | --- | --- |
| Reads | The bucket: metadata sidecars, and verified source bundles for full detail | Transcript files on this machine, filtered as the collector filters them |
| Needs | agent-archive set up | Only the binary: no setup, and the data directory is never created |
| `git_head` | As the hooks recorded it ([the commit a session started on](git-head.md)) | Absent: a transcript file records no commit, and nothing is guessed |
| `replay` | As the hook recorded it ([replay sessions](replay-sessions.md)) | Absent: only a hook records it |
| `feedback` | Yes | Absent: feedback is attached to archived sessions |
| `project.root`, `transcript_path` | Absent: sidecars hold no paths | Present: the paths never leave this machine, and the tool needs them to exclude its own sessions by working directory and to ask for a transcript again |

## Command

```text
agent-archive eval export SESSION_ID... [--detail metadata|full]
    [--harness codex|claude|cursor] [--max-bytes N] [--workers N]
agent-archive eval export --ids-from - [--detail …] [--workers N]
agent-archive eval export --file PATH --harness NAME [--detail …]
agent-archive eval export --scan [--harness NAME] [--project DIR]
    [--since DATE] [--until DATE] [--detail …] [--workers N]
```

The four ways of naming sessions are mutually exclusive; `--project`,
`--since`, and `--until` apply only to `--scan`.

- The name follows the CLI's `purge plan`/`purge apply` grouping: `eval` is
  the group, `export` its one action, which leaves room for later actions
  without claiming top-level names. It is not in the agent skill's command
  list ([agent skills](agent-skill.md)): the skill is for pulling a session
  into a conversation, and a full export of many sessions is exactly the
  context flood the skill avoids.
- **Read-only and headless.** It never prompts, pages, opens a picker, or
  writes a file; it writes records to standard output and problems to
  standard error. It accepts only exact identifiers (a full archive session
  ID, or a transcript path), never a title or a prefix, so the same command
  always exports the same sessions.
- **JSON Lines, always.** One record per line, even for one session, so a
  reader has one code path. Records are written as each session finishes,
  so with more than one worker the order is not the input order (with
  `--workers 1` it is); every record carries its `session_id` (and, for local
  ones, `transcript_path`, and for errors, `input`) to be matched on.
- **Two detail levels.** `--detail metadata` is the cheap first pass:
  identity, commits, counts, tokens, tools, outcome, and no conversation text.
  For the archive it reads metadata sidecars only, as `list` does, and never
  downloads a source bundle. `--detail full` (the default) adds `prompts`,
  `final_response`, `files_edited`, and `feedback`, from the source bundle,
  verified against the sidecar's checksum as `show --transcript` verifies it.
- **Size bound.** `--max-bytes N` bounds each record, as `handoff --max-bytes`
  bounds a handoff (default 120000; `0` for no limit). The longest of the
  prompts, the final response, and the feedback notes is halved, repeatedly,
  never below 256 bytes, each cut text marked `"truncated": true`; then edited
  files are dropped from the end. Prompts are never dropped: the tool derives
  the task and its rubric from them. A record that still does not fit is
  written anyway with `trimmed.exceeds_max_bytes`. The bound applies per
  record, not to the whole output: a bulk export's size is the tool's to
  manage.
- **Errors are records.** A session that cannot be exported (not found, its
  source missing or failing verification, a sidecar that decodes but breaks
  the schema's invariants, such as a negative count or an unknown enum, a
  parse failure) becomes
  `{"record": "error", "input": …, "error": {"code", "message"}}` on its own
  line, and the export goes on. The exit code is 0 when every input produced a
  session record, 1 when any produced an error record (or the archive could
  not be opened at all, with nothing written), and 2 for a usage error, with
  nothing on standard output.

### Local mode

- `--file PATH --harness NAME` exports one transcript, the way
  `handoff --file` renders one: `collector.FilterTranscriptFile`, then a
  source bundle, then metadata derived with the current parser
  (`archive.BuildLocalEvalExportWithAnalysis`). No configuration is read, and the data
  directory is never read or created: only an archive input opens the
  archive.
- A transcript path given on `--ids-from` names its app by the folder it is
  in (Claude Code's and Codex's session folders, including those
  `CLAUDE_CONFIG_DIR` and `CODEX_HOME` name, or a Cursor `agent-transcripts`
  folder), or by `--harness`. A path in none of them without `--harness` is
  an `unknown_harness` error record.
- `--scan` finds transcripts with backfill's discovery (`backfill.BuildPlan`
  over the apps' stores, with an archive state that knows no sessions and an
  empty configuration), with backfill's `--harness`, `--project`, `--since`,
  and `--until` filters and its defaults for the home directory and
  temporary folders (skipped). Sessions backfill would skip for a reason
  other than "already archived" are skipped here too. Cursor chats found only
  in Cursor's database are not exported by `--scan` in this version (no file
  path to name them by).
- A local record's `session_id` is the app's own session ID (as discovery
  found it, or as the transcript's records carry it, or for a Codex rollout
  named directly the UUID its file name ends with); `transcript_path` is the
  file's absolute path and `project.root` the project folder backfill
  resolved, or for `--file` the first working directory the transcript
  records. `started_at` uses the original native start, including Codex
  session metadata, or the start backfill's discovery found (a Cursor
  transcript's creation time), and is otherwise absent: the export time is
  never used as a start. `git_head`, `replay`, and `feedback` are absent (see
  the table above). A local session is filtered with the running build's filter, so its
  `filter_version` is always the current one.
- An archive ID selects the archive record; `--file`, `--scan`, and absolute
  paths select the supplied local transcript, even if it is also archived.
  The command never merges evidence between these sources.

### Bulk

- `--ids-from -` reads one input per line from standard input: an archive
  session ID, or an absolute transcript path. Blank lines are skipped. It
  refuses a terminal as standard input (exit 2): the list is piped in, and
  the command never waits for a person.
  This lets the second (full) pass export exactly the sessions the first
  (metadata) pass kept, in one process.
- `--workers N` (default: the number of CPUs, at most 8) exports that many
  sessions at once. It applies to every way of naming sessions. Parsing and filtering are the cost, not process start-up,
  so the pool is inside agent-archive. Each record is written whole, under a
  lock, as its session finishes.
- A failure in one session never stops the others.

## The record

`schemas/eval-export.schema.json` is the contract; it reuses the metadata
schema's definitions by reference, so a field shared with the sidecar means
exactly what it means there. A session record:

```json
{"schema_version": 1, "record": "session", "source": "archive", "detail": "full",
 "session_id": "…", "native_session_id": "…", "machine_id": "…",
 "harness": {"name": "claude", "version": "2.1.280"},
 "project": {"name": "widget", "repo_key": "repo-19d704750fcde678"},
 "branch": "fix/widget-test",
 "git_head": {"start": {"sha": "…", "dirty": true, "observed_at": "…"}, "last": {"sha": "…", "observed_at": "…"}},
 "replay": {"run_id": "run-1"},
 "started_at": "…", "ended_at": "…", "captured_at": "…",
 "state": "idle", "turn_outcome": "completed",
 "parser": {"name": "claude", "version": "0.16.0", "status": "partial"}, "filter_version": "12",
 "models": […], "counts": {…}, "model_tokens": […], "tools_used": […], "mcp_calls": […],
 "skills_used": […], "git_activity": […], "capture_gaps": […],
 "prompts": [{"text": "Why does the widget test fail?", "timestamp": "…"}, …],
 "final_response": {"text": "…", "source": "transcript"},
 "files_edited": ["widget/size.go"],
 "feedback": [{"text": "…", "observed_at": "…"}]}
```

- `prompts` are the human prompts the parser counts as `counts.turns`, in
  order: harness-written records (expanded skills, command output,
  notifications, compaction summaries) are not prompts. A prompt is shown as
  the person typed it: Cursor's `<timestamp>`/`<user_query>` wrapper is
  removed and a Claude Code slash command is its command line, as `handoff`
  shows them. Unlike `handoff`, a prompt is not cut short (only
  `--max-bytes` cuts). The mid-session prompts are the corrections the tool
  derives a rubric from.
- `final_response` is the last assistant text in the transcript, or, when
  the transcript has none, the last final message a stop hook reported
  (`source: "hook"`), leaving out a subagent's (one carrying `agent_id` in a
  parent's bundle). It is the last reply, not necessarily the answer to the
  last prompt.
- `files_edited` lists the files the editing calls named (Edit, Write,
  apply_patch, …), in the order first edited, relative to the session's first
  working directory when inside it; `counts.files_touched` counts the same
  set. It is empty for a Cursor text transcript, which records no calls.
- `branch` is the first branch the transcript records (Claude Code's
  `gitBranch`), the branch the session started on; Codex and Cursor record
  none. It is absent when that first branch is `HEAD` (a detached checkout)
  or not a valid branch name, as the metadata's branch is. An archived session's metadata record has none either: the sidecar
  does not hold it, and the metadata pass reads nothing else.
- One parser: at full detail, when the sidecar was written by another parser
  version, the parser-derived fields (`parser`, `counts`, `models`, tokens,
  tools, skills, `git_activity`, `capture_gaps`, `state`, `turn_outcome`,
  `ended_at`) are re-derived from the same parse as `prompts` and
  `files_edited`, so the two halves agree. What the hooks recorded
  (identity, project, `git_head`, `replay`) and admission gaps for imported
  or discovered sessions stay the sidecar's. A metadata
  record is the sidecar's alone, whatever parser wrote it.
- Paths: an archived record carries no path from the metadata, but
  `files_edited`, like the prompts, is text from the filtered transcript,
  which holds the working directories (see [privacy](../../docs/security/privacy.md#what-is-uploaded)).

## Stability

The external tool pins a supported range of `schema_version` and runs contract
tests against a pinned agent-archive release, so:

- Within a `schema_version`, fields are only added. A field keeps its name,
  type, and meaning; an enum may gain values (readers accept unknown ones, as
  the schema says for error codes, gap codes, and git event kinds).
- Removing or renaming a field, changing its type or meaning, or making an
  optional field required bumps `schema_version` (`archive.EvalExportSchemaVersion`),
  with a CHANGELOG entry. The previous version is not written alongside.
- What a field's value is derived from can improve without a bump when the
  parser version records it (`parser.version`): more accurate counts, a new
  prompt shape recognized. `filter_version` says which filter produced the
  text.
- The goldens in `internal/archive/testdata/eval-export/` pin the exact
  output for one session per app at both details; every change to them is
  reviewed like a schema change. The tool's own contract tests should
  validate against the schema, not the goldens' exact bytes.

## Privacy

Nothing new is read or uploaded. An archived record is built from the sidecar
and the verified source bundle, both already filtered; a local record from a
transcript run through the same filter the collector applies before upload.
The export adds no field that is not already in one of those, except the two
local paths, which exist only in local mode and are printed on this machine.
It prints to standard output and writes nothing; the text it prints is
filtered, not safe: it can hold anything the transcript held that the filter
does not recognize as a secret, like `show --transcript`.

## Not here

The eval runner, Harbor integration, containers, grading, and the inference of
a start commit for sessions without one (see
[the commit a session started on](git-head.md#not-done-here-inferring-a-start-for-older-sessions))
belong to the external tool.

## Tests

- `internal/archive/eval_export_test.go` also pins a local record
  (`local-claude.jsonl`) and that a local record never takes the export
  time as its start.
- `internal/cli/eval_export_local_test.go`: `--file` with no setup and no
  data directory created; `--scan` exporting what backfill would import and
  skipping what it skips, with its filters and several workers; `--ids-from -`
  with a known app's transcript and a stray one; an archive ID without
  setup; usage errors; and several workers writing each record exactly once.

- `internal/archive/eval_export_test.go`: goldens per app at both details,
  each line validated against the schema; every prompt in order and whole; no
  conversation text at metadata detail; the hook's final message as a
  fallback, never a subagent's; a validated starting branch; one parser for
  both halves of a full record; `ValidateEvalExport` rejecting what the schema
  rejects; a Cursor text transcript; `FitEvalExport`'s cuts, its floor, and
  that it never drops a prompt or changes its argument; the error record.
- `internal/cli/eval_export_test.go`: end to end from a session the hooks
  captured and `sync` published; the metadata pass working without the source
  bundle while the full pass reports it; one error record per missing input
  without stopping the rest; an invalid sidecar as an error record at both
  details; the bound; usage errors; not set up.

### Explicit native revision selection

`agent-archive eval export SESSION_ID --revision REVISION_ID --detail full`
selects one exact parent rollout UUID from validated metadata history. Selection
requires one explicit archive ID; metadata-only, batch/stdin and local discovery
inputs are usage errors. The record carries optional `revision_id`, and its
capture time, filter and harness provenance come from verified selected bytes.
Derived fields are rebuilt from that revision rather than copied from active
metadata. Current children are neither resolved nor included. Unknown IDs or
invalid history produce an error record and an operational failure without an
active fallback. Ordinary exports omit `revision_id` and retain their contract.
