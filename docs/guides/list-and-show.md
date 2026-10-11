# Inspect the archive: list, show, feedback

`list` and `show` are read-only: they show what is in the bucket without
touching local collector state. `feedback` attaches your own assessment to
a session.

Each remote object read and listing page has a two-minute budget, including
retries and response-body reads. A large archive scan can take longer because
each operation receives its own budget. Interrupting a read cancels it; time
spent choosing a session or scrolling in the pager has no read deadline.

```sh
# The most recently active archived sessions matching the filters (at most
# 50 by default), top-level sessions only, from this repository when you are
# in one: title (the name your agent gave the session, else a preview of the
# first filtered prompt), pull request, how long ago it was last active
# (WHEN: its latest record, so a session backfill imported is dated by when
# it ran, not the import), harness, project, and short ID. --json keeps the
# newest captured first. Metadata only, never full transcript text. On an
# interactive terminal, pick a numbered row to see that session's summary
# (see below).
# Otherwise the table is paged through $PAGER (or less; see Scrolling below);
# use --no-pager to print directly.
agent-archive list
agent-archive list --all-projects     # every project, not only this repository's
agent-archive list --project billing  # another project, by directory or name
agent-archive list --limit 0          # every match, not just the newest 50
agent-archive list --limit 200
agent-archive list --verbose          # full IDs, absolute times, origin, parser
agent-archive list "flaky retention"  # the sessions these words find (see Which sessions)

# Narrow it down. --since takes a date, an RFC 3339 time, or an age.
agent-archive list --harness claude --model claude-opus-5 --since 7d
agent-archive list --imported          # only sessions backfill imported
agent-archive list --hook-captured     # only sessions captured as they ran
agent-archive list --replays include   # also sessions a replay tool ran (hidden by default)
agent-archive list --replays only      # only those, marked [replay]
agent-archive list --skill review --skill-usage available
agent-archive list --skill review --skill-sha256 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
agent-archive list --complete          # complete parser coverage, no capture gaps

# For scripts: {"schema_version": 4, "sessions": [...], "limit", "returned",
# "total_matched_known"}, and "scope" inside a project. "total_matched" is
# exact when fresh headers prove index coverage. Never paged or
# interactive. Run inside a project it lists that repository's sessions, so a
# script that wants everything adds --all-projects.
agent-archive list --json
agent-archive list --json --limit 0
agent-archive list --rebuild-index  # migrate legacy entries and repair auxiliary hints

# One session's summary: title, when, app, models, activity, skills,
# subagents, and capture gaps. With no SESSION_ID on a terminal, the same
# session browser as list. Words also work, as for `list "<words>"`: every
# word must appear in the session's name, title, branch, project, app, or
# the start of its ID (4 characters or more), and #212 or 212 also matches a
# pull request number, never the start of an ID. This repository's sessions
# are searched first, subagent sessions last.
agent-archive show
agent-archive show SESSION_ID
agent-archive show "OAuth callback"
agent-archive show "OAuth callback" --harness codex  # search one app
agent-archive show SESSION_ID --json         # the metadata sidecar, for scripts

# The conversation itself, only when you ask for it.
agent-archive show SESSION_ID --transcript          # prompts, replies, tool calls
agent-archive show SESSION_ID --transcript --full   # also trimmed tool results
agent-archive show SESSION_ID --transcript --json   # the normalized view as JSON
agent-archive show SESSION_ID --transcript --max-bytes 40000  # a smaller limit
agent-archive show SESSION_ID --transcript --max-bytes 0      # no limit
```

A summary looks like this:

```text
Stabilize the OAuth tests
claude · agent-archive · 2h ago                                      ✓ completed

  Prompt    Fix flaky OAuth callback tests
  When      Sep 29, 10:14 → 10:58 (44m)
  Agent     Claude Code 2.4.1
  Model     claude-opus-5-5 (high reasoning) · 31 responses
            claude-haiku-4-5 · 4 responses
  Activity  35 turns · 212 messages · 148 tool calls · 3 shell commands ·
            1 compaction · 14 files edited
  Tools     Bash 42 · Edit 18 · Read 12 · Grep 9 ·
            mcp__github__create_pull_request 1
  Branch    fix-oauth
  Git       2 commits · 1 push · PR #155 opened · PR #155 merged
  PRs       wangjohn/agent-archive#155
  Skills    code-review, simplify
  Subagents 2 linked (1 available, 1 expired)

  ID 03e60c25f1a04b7c9d2e8f6a1b3c5d7e
     origin hook · parser 0.19.0 (partial) · filter 14

  Transcript: agent-archive show 03e60c25f1a04b7c9d2e8f6a1b3c5d7e --harness claude --transcript
  JSON:       agent-archive show 03e60c25f1a04b7c9d2e8f6a1b3c5d7e --harness claude --json
```

Rows the metadata has no data for are left out; a count that is unknown is
not shown as zero, and shell commands, compactions, and edited files are
listed only when there were some. A model's count is how many responses it
gave, and Tools lists the most-called tools (`tools_used`). Times are in
your local time zone. The session ends at `ended_at`, its latest record
timestamp. Metadata from before parser 0.13.0, or from an app whose records
carry no timestamps, has no end time, so the summary uses when the session
was last captured and labels the time since the start a span. A session
`backfill` imported with no end time shows only its start, since it was
captured when the import ran.

Capture gaps are the parser's notes on what the archived copy leaves out.
Most are expected: the privacy filter dropping injected instructions and
hidden fields, redacted secrets, long content cut to size, and fields or
records the parser does not recognize yet. The summary names those in one
dimmed Omitted row. Only gaps that may mean content is missing, such as an
unreadable record or a subagent whose transcript was never written, are
listed under a warning, "Incomplete capture": each code once, with how
often it occurs and its first detail. `show --json` has every gap.

## Which sessions `list` shows

**This repository first.** Run inside a git repository that has an `origin`
remote, or inside a project you configured, `list` shows that repository's
sessions: every checkout and worktree of it, and its sessions from your other
machines. The heading names it and the other choice:

```text
agent-archive · 42 sessions · claude · a all projects
```

`a` (typed alone) switches to all projects in the browser, and
`--all-projects` starts there. `--project DIR|NAME` looks at another project
instead: a directory (its repository), or a project name, matched exactly but
ignoring case against the project name archived with the session and the labels
in your configuration. The two flags together are a usage error. When the
repository has no archived session yet, `list` shows all projects and says so
(`Nothing in agent-archive · showing all projects`). Outside any repository or
configured project there is no scope: every session is listed, grouped by
project (projects with one name stay apart, labeled with their ID prefixes).
Within one project the table is not grouped, so a repository's checkouts are one
list.

**Top-level sessions only.** A subagent session is left out of the table and the
browser, before `--limit` counts, so `--limit 50` is 50 sessions you started. The
footer says how many were left out (`42 sessions (318 subagent sessions hidden;
search to find one).`) and a parent carries `· 45 subagents`. Words find a
subagent (see below). `list --json` keeps every row, subagents included; each has
`parent_session_id`.

**Columns.** TITLE, PR (the last pull request the session linked or created,
when some row has one), WHEN, HARNESS, PROJECT, and ID. A HARNESS or PROJECT that
every row shares is left out and named in the heading instead; `--verbose` keeps
every column. The leading `●` that marks a session active in the last 2
minutes is for the [handoff picker](handoff.md#the-picker): `list` reads the
archive, which says nothing about what is running here.

**Words.** `list "<words>"` shows only the sessions the words find, by the
rules `show` and `handoff` use. Every word must appear, in any case, in some
field of the session: its name, title, branch, project name, harness, or the
start of its ID (4 characters or more), and `#212` or `212` also matches a pull
request number exactly, never the start of an ID. Words may match different
fields: `list "linux 212"` finds the session named for Linux that opened PR 212.
The first of these that has a match answers:

1. top-level sessions in the scope;
2. top-level sessions in every project;
3. subagent sessions, in the scope and then everywhere.

A session ID, full or the 8-character short one, names that session first. When
the scope answers, the footer says how many more match elsewhere (`1 match in
agent-archive (3 more in other projects: --all-projects or a project name finds
them)`). `--harness`, `--since`, and the other filters apply before this, and
`--limit` last. A query nothing matches prints `No archived sessions match
"<words>".` and exits 0, as an empty list does. `list "<words>" --json` prints
the same sessions as data, which is what an agent reading candidates uses. On
a terminal, `list "<words>"` opens the [browser](#browsing-on-a-terminal) with
the words already in its filter, to edit.

**Scripts.** The `--json` document gains an optional `scope` object when `list`
runs inside a project or with `--project` (with `--all-projects` too, which it
records): `{"label": "agent-archive", "all_projects": false,
"fell_back": false, "outside_matches": 3}`. Because a `list` or `list --json`
run inside a project now returns only its sessions, a script that reads every
session passes `--all-projects`. See [JSON output](../reference/json-output.md).

## Browsing on a terminal

When stdin and stdout are both terminals, `list` and bare `show` open a
session browser on the terminal's alternate screen, so the list and a
session's summary replace each other instead of piling up:

- The browser reads each key as you press it; there is no need to press
  Enter after a command.
- Type a row number or short SESSION_ID and press Enter to see that
  session's summary. What you type shows at the prompt; Backspace edits it
  and Esc clears it. Row numbers are those of the whole list, so any listed
  number or short ID works wherever the list is scrolled (in the list
  grouped by project, the numbers on screen need not be consecutive).
- A list taller than the window scrolls: the mouse wheel and ↑ ↓ by a
  row, PgUp and PgDn (or space, `n`, and `p`) by a screen, Home and End to
  the top and bottom. A status line below it says where you are (`Top`, a
  percentage, `Bottom`, or `All` when the whole list fits), for example
  `Top · ↑↓ scroll · PgUp/PgDn page · / filter · type a number and Enter · q quit`. A
  project scrolled into keeps its heading at the top, marked
  `(continued)`. `a`, `n`, `p`, `q`, and `/` act only when nothing is typed.
- Press `/` to filter the rows as you type, as in `less`: a line at the
  bottom takes the words, and the rows narrow with each character. The words
  are the ones `list "<words>"` takes: every word must appear in a session's
  name, title, branch, project, app, or the start of its ID, and `#212` or
  `212` also matches a pull request number. The first session that matches
  is marked `▸`, ↑ and ↓ move the mark, and Enter shows the marked session
  (and `b` brings back the filtered list). Esc clears the filter and closes the line, and
  Backspace on an empty one does too. Rows keep the numbers they have
  unfiltered; a session the filter finds past the list's limit is numbered
  on from its last row, and is chosen with the mark (after Esc only the
  list's own numbers choose). A subagent session that matches is shown indented under its
  parent (`↳ Review and fix PR #208 (5b-1b)`), and the parent is shown even
  when it does not match (the mark starts on the subagent, not the parent);
  a subagent has no number, so choose it with the mark. `list "<words>"` and a `show` query that matches several sessions
  open the browser with the words already in the filter. Esc clears them and
  shows the plain list: for `list`, the sessions it lists without words; for a
  `show` query, the sessions it matched. On the filter line `a`, `n`, `p`,
  `q`, and digits are typed text: press Esc first to use them as keys.
- In the summary, `t` opens its transcript through the pager (quit the pager
  to come back), `b`, Enter, or Backspace return to the list, and `q`
  quits. `less` keeps even a one-screen transcript open until you press
  `q`; after another pager, press Enter to return to the summary.
- A summary taller than the window scrolls the same way, with a line such
  as `↑ 3 lines above · ↓ 12 more lines` below it; `m` opens the whole
  summary through the pager, as `t` does the transcript.
- `q` (or Enter with nothing typed at the list, or Ctrl-D) quits from
  anywhere but the filter line, and Ctrl-C quits at once. The last summary you viewed is printed
  to the normal screen as the browser closes, so its ID stays in your
  scrollback.
- Every place a session is picked from a list is this one browser: `list`,
  bare `show`, bare `show --json` (a heading `Show ·`, and Enter prints the
  chosen sidecar instead of opening its summary), a `show` query that
  matches more than one session (the heading says `"words" matches 3`), and
  the [handoff picker](handoff.md#the-picker).
- When the terminal's input cannot be read a key at a time, the browser reads
  lines. A list taller than the window is shown a page at a time, with a line
  such as `Page 2 of 3 · 50 sessions · [n] next  [p] previous`: `n` and `p`
  move between pages, and a project whose sessions started on the previous
  page is headed again, marked `(continued)`. Type a row number or short
  SESSION_ID to choose; any other answer is words to filter by, the table is
  drawn again with the sessions that match (`3 sessions match "flaky" · a
  number, more words, or Enter for all`), and more words narrow it further.
  An empty answer clears the filter, and quits when there is none. A summary
  taller than the window is cut with `… N more lines`, and without a pager
  `m` prints the lines left out.

Bare `show --transcript` is a usage error: pick a session with `show` and
press `t`, or give a SESSION_ID. Bare `show --json` picks one session in the
browser and prints its sidecar.

On an interactive terminal, bare `agent-archive` (no command) opens the
same session browser as `list` when capture is already set up.

None of this happens when a coding agent runs the command, even if its shell
is a pseudo-terminal. With `CLAUDE_CODE_SESSION_ID`, `CODEX_THREAD_ID`, or
`CURSOR_AGENT` in the environment, or `AGENT_ARCHIVE_NONINTERACTIVE=1`,
`list` prints its table without a pager or browser, bare `show` asks for a
SESSION_ID, and `show` words that match several sessions print the
candidates on stderr and exit 1, exactly as when piped.
`AGENT_ARCHIVE_NONINTERACTIVE=0` brings the browser back; see
[configuration](../reference/configuration.md#environment-variables) and
[troubleshooting](troubleshooting.md#no-picker-or-prompt-in-an-agents-terminal).

The JSON documents are described in [JSON output](../reference/json-output.md).

`list` reuses unchanged metadata from a local cache (`--no-cache` to skip
it). Once the cache knows a few hundred sessions, it also splits a full
listing into key ranges that are listed in parallel, so a large archive is
listed in a fraction of the time. A session whose metadata can't be read,
for example because a newer version wrote it, is left out with a warning on
stderr; the rest are listed.

`list` shows each session's `name` when metadata has one: the name your agent
gave the session (the one in Claude Code's sidebar, which `/rename` sets, or a
Cursor chat's name). Otherwise it shows the `title`, a short preview of the
first filtered human prompt, derived at publish time. Older sidecars without
either fall back to the short SESSION_ID in that column. `project_name`
in metadata labels the project when present; otherwise `list` uses this machine's
configured project basename. The ID column is a short prefix you can pass to
`show`; if multiple archived IDs share that prefix, use a longer ID from
`list --verbose` or add `--harness`. Projects with the same basename stay in
separate groups, labeled with their project ID prefixes.

Claude Code names come from typed native `custom-title` and `ai-title`
transcript records. The latest explicit `/rename` wins over generated titles;
otherwise the latest generated title wins. Names are redacted and shortened
for archive display. CLI/extension transcript-backed names are supported;
other desktop-only histories and every native picker fallback are outside
this capture contract. The separate `title` remains the first human prompt.

Codex currently uses the first-prompt preview because its sidebar title is
stored separately from the transcript. The table leaves subagent sessions
out: the footer says how many, and a parent carries a `· N subagents` hint.
Words find one, as for `list "<words>"`, and its row reads `· subagent of`
its parent's short ID; a Claude Code subagent's name is the description its
parent gave the task (see below). New and refreshed previews keep up to 128
characters from parser `0.17.1`; older metadata keeps its shorter saved
preview.

Metadata also says what a session did without downloading its transcript:
when it ended (`ended_at`), its most-called tools (`tools_used`), and how
many distinct files it edited (`counts.files_touched`). Only names and
counts are stored, never file paths. Sessions published by an older version
gain them when the collector next refreshes their metadata. See
[JSON output](../reference/json-output.md#show).

From parser `0.14.0` metadata also counts reasoning tokens and tool errors,
splits token counts by model, and counts MCP calls by server; these are for
scripts and are not shown by `show` yet.

From parser `0.15.0` metadata records the commits, pushes, and pull
requests the session created or merged, when its own tool calls confirmed
them; the `Git` row counts commits and pushes and names each pull request.
See [JSON output](../reference/json-output.md#show).

A session captured by this release's hooks in a git repository also records
the commit it started on, whether the working tree had uncommitted changes,
and the last commit a stop hook saw; the `Commit` row shows them
(`started on 3f9c2ab4d1e0 with uncommitted changes · last seen on
9e01d4c7a2b8`), and `--json` has the full names in `git_head`.
From parser `0.17.0` metadata also records the name your agent gave the
session (`name`), the last git branch it recorded (`branch`), and the pull
requests it was linked to (`pull_requests`). The summary's heading is the
name, with the first prompt as a `Prompt` row, and it gains `Branch` and `PRs`
rows; a session with no name keeps the first prompt as its heading. `PRs`
lists the pull requests the agent linked to the session, where `Git` counts
the ones its own commands opened or merged. A session published before the
name was kept (privacy filter 13) gets one the next time the collector
re-reads its transcript, if the transcript is still on the machine. A Claude
Code subagent's name is the description its parent gave the task (filter 14
and parser `0.18.0`), so a subagent archived before that gets one the same
way. From parser `0.19.0` a Cursor session's title is the prompt as you
typed it, without the `<timestamp>` line and `<user_query>` tags Cursor
wraps it in.

`show` prints conversation content only when asked, with `--transcript` or
the browser's `t`: it downloads the session's source bundle, verifies its
checksum and identity against the metadata, and prints each prompt, the
agent's replies, one line per tool call (`▸ Bash go test ./...`, marked ✗
when the call failed), the `!` shell commands (`$ make test`) and local
slash commands (`» /model`, with the app's reply) you ran, compaction
summaries, notices the app posted (such as a background task finishing,
with the agent's reply under it), and any final response a hook reported
that the transcript lacks. Your prompts are quoted with a `┃` gutter, and
each stretch of the agent's replies and tool calls starts with the app's
name (`Claude Code ›`), so you can tell who is speaking without color. Edit
bodies are never shown. `--full` adds each
tool result and shell command's output, trimmed to its first and last
lines; it does not combine with `--json`, which has every retained result. On a terminal the summary and
the transcript are paged like `list` (see [Scrolling](#scrolling));
`--no-pager` prints them directly. `--transcript --json`
prints the sidecar and then the normalized view (turns, tool calls, tool
results, and hook-reported final messages) as JSON. `--normalized`, its
former name, still works and prints a deprecation note on stderr. If the
same session ID was published under more than one harness, pass `--harness`
to pick one. The object layout is in
[bucket layout](../reference/bucket-layout.md).

### Size

`show SESSION_ID --transcript` prints at most 120,000 bytes (about 30k
tokens), as `handoff` does, so a script or an agent that runs it on a long
session gets an answer that fits. `--max-bytes N` changes the limit and `0`
removes it; it applies to `--full` and to `--json` too, and to a terminal
as much as to a pipe. The session browser's `t` key is not limited.

Over the limit, the text form trims the oldest exchanges first, and only as
far as needed, in this order: tool results and command output (with
`--full`) are dropped, runs of tool calls collapse to counts by tool
(`▸ 14 tool calls: Bash ×9, Read ×5`), agent messages are shortened to 300
bytes, and long prompts and long shell and slash commands you typed are cut
to 2,000. The last three exchanges (up to their last twenty steps) are kept
whole through those steps. If that is still too much, long final responses a
hook reported are cut to 2,000 bytes, the oldest of those are dropped (the
newest is kept), and the oldest exchanges are dropped (the newest is always
kept). When the newest exchange alone is too much, its long agent messages
are cut to 2,000 bytes, oldest first, and then its oldest steps go. What was
cut says so where it was cut (`…(shortened)`, `…(truncated)`), and the output
ends with what was omitted, as a log of the steps in the order they ran
(exchanges are counted from the session's first, so those it names may be
among the ones dropped), and where to read all of it.

`--json` output stays valid JSON: two documents, the sidecar and then the
normalized view, whose size together is limited. The sidecar is never
trimmed. In the normalized view the steps are: drop `tool_results` entries
(sizes only; a linked result's size is also on its call), drop `hook_finals`
entries (statuses only), drop each tool call's `input`, cut the `text` of turns other than the person's prompts to
300 bytes, cut prompts to 2,000, and finally drop the oldest turns, tool
calls, and tool results. Each goes to the oldest entries first and only as
far as needed. A cut text has `text_truncated: true`, and a `trimmed` object
at the end of the normalized view records the limit, what was omitted, and
where the full output is. `trimmed` is absent when nothing was trimmed.

When anything is trimmed, the untrimmed output is saved in the data
directory's `handoffs/` folder as `SESSION_ID.transcript.txt`
(`.transcript-full.txt` with `--full`, `.transcript.json` with `--json`),
mode 0600, replaced by the next run that trims the same session the same
way, removed after 7 days and by `uninstall --delete-local-data`. A limit too small for even the newest
step, the header, or the sidecar alone, prints a warning on stderr and as much
as it can. Nothing here prompts, and without a terminal nothing is paged.

Before setup has run, both commands print `Not set up.` to stderr and exit 1.

## Scrolling

On a terminal, anything longer than a screen goes through a pager: the
`list` table outside the browser, `show SESSION_ID`'s summary, a transcript,
`status`, and `purge plan`. Piped or redirected output, and `--json`, are
never paged; `--no-pager` prints directly on a terminal too.

With no pager set, or with the pager set to a bare `less` (as oh-my-zsh
does), Agent Archive runs `less` its own way: it quits at once when the text
fits on one screen and otherwise shows the keys on its last line:

```text
lines 1-48 of 1210 - arrows/space scroll, / search, q quit
```

- The mouse wheel, arrow keys, space and `b` (page down and up), and `g` and
  `G` (top and bottom) scroll; `/` searches, `n` finds the next match; `q`
  quits. From the session browser, `q` goes back to the summary.
- With `less` 551 or later (macOS ships a newer one), the wheel scrolls the
  text, three lines at a time, and the text stays on the screen after `q`.
  Because `less` then reads the mouse, hold Option while dragging to select
  text in iTerm2 (Shift in most other terminals; in Terminal, turn off View
  > Allow Mouse Reporting).
- `less` 530 to 550 shows the text on the terminal's alternate screen,
  where the terminal turns the wheel into arrow keys, and clears it on `q`.
- An older `less` keeps the text on the normal screen, where the wheel may
  scroll the terminal instead. A `less` that doesn't report its version
  (BusyBox's) runs as `less -FR`, without key hints.

To use another pager, set `AGENT_ARCHIVE_PAGER` (or `PAGER`), for example
`AGENT_ARCHIVE_PAGER='less -R'`. It runs as given, without the key hints or
mouse options, with two additions as git makes them: `LESS=FRX` when `LESS`
is not set, and `LV=-c` when `LV` is not set. From the session browser, a
plain `less` command also gets `-+F`, so a short transcript waits for `q`.
Set either variable to `cat`, or to nothing, to never page.

## Model and setting keys

Metadata model keys `gen_ai.provider.name`, `gen_ai.request.model`, and
`gen_ai.response.model` follow OpenTelemetry GenAI semantic conventions
v1.37.0 at commit `aec6e9d3e86754683dab7c707655d69d953b2768`.
`agent_archive.request.model_label`, `agent_archive.request.reasoning_level`,
and `agent_archive.request.setting.*` are archive-local extensions. The
archive is not an OTLP export. Metadata derived by parser 0.2.x may still
contain the former `gen_ai.request.reasoning.level`,
`gen_ai.request.model.label`, and `gen_ai.request.setting.*` keys;
regenerating metadata with a later parser moves them without changing the
source bundle.

## Linked subagent sessions

A Claude Code subagent (`SubagentStop`) becomes its own archived session with
`parent_session_id`, once the collector validates it against an already
accepted parent: matching parent and agent IDs, and a start after the
parent's start and the project's activation. The parent gets
`linked_sessions`; it does not embed the child's transcript or count its
messages. `agent-archive show PARENT` counts them on its Subagents row, and
`show PARENT --json` adds `linked_session_availability` (pending,
unavailable, or unavailable-or-expired per child) beside the sidecar's own
fields, so `show --json` output is not itself an instance of
`metadata.schema.json`; validate stored metadata objects, not command output.
A child whose transcript is still missing or empty 30 minutes after its
`SubagentStop` (Claude Code reports some background agents with a path it
never writes) is dropped, its parent's link becomes unavailable, and the
parent records a `subagent_transcript_never_written` capture gap.
A child's summary names its parent. Select a child with
`agent-archive show CHILD --transcript` for its verified content. Links do
not extend retention, and children are never downloaded
recursively. Codex and Cursor subagents are not captured yet.

## Skill evidence

During initial capture or new session activity, the background collector
checks known skill directories for the app. It records installed skills,
instruction hashes (SHA-256 of each SKILL.md with its credentials
redacted), filtered instruction copies, and observation
times. These observations do not prove that the app discovered a skill, made
it eligible, or used that exact version in an earlier turn. Oversized,
truncated, nested, unreadable, and uninspected content is marked as a
coverage gap rather than failing the session. Hooks never scan the
filesystem.

`--skill-sha256` filters metadata only and distinguishes sessions using
different bytes under the same skill name. No parser version records both a
complete eligible-skill set and complete use observation, so non-use is never
proven. `--skill-usage eligible_no_use` is unsupported and exits with a usage
error on stderr (code `2`) and no result on stdout. Use `used` or `available`
for supported skill queries. Sidecars from parsers before 0.4.0 (or with a
custom parser version) are never counted as observed non-use.

## Feedback

Feedback accepts a regular UTF-8 text file of at most 64 KiB; a symlink to
a regular file is allowed. Directories, devices and named pipes are rejected.

Write your assessment to a private UTF-8 text file, then attach it to a
session owned by this machine:

```sh
agent-archive feedback SESSION_ID --file /private/path/feedback.txt
agent-archive sync
```

Feedback is filtered before it enters the local upload queue. It records user
provenance and observation time; finishing a turn is never treated as
success. The command rejects excluded sessions and sessions retired by a
destination change. Feedback for a paused, still-included session waits for
resume.
