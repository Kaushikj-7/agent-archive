# Finding a session — engineering plan

Status: implemented; planned 2026-09-30, [decisions](#decisions) confirmed the same day;
PRs 1 to 8 merged (the last on 2026-10-01); the owner's
[live check](#live-check-pr-8) is not recorded yet.
Where this plan and the code differ once packages merge, the code is the
reference and differences go under Deviations.

Goal: the session a person means is on the first screen of the handoff
picker or `list` without typing, and one or two words find it when it is
not. The same words find it when an agent (Codex, Claude Code, Cursor) runs
`agent-archive handoff "<words>"` or `agent-archive list "<words>"` for the
person.

Related: the handoff performance work (another session, 2026-09-30) is
making the archive listing faster in `internal/reader` and `internal/storage`,
and will later spec an index read. This plan owns the picker, `list`'s rows
and browser, the parser, and the filter; that work owns listing speed. The
index spec is to provide a subagent count per parent (from `LinkedSessions`),
one parent's children on demand, a repository-scoped window of recent
top-level sessions that falls back to `ProjectID`, and an unscoped one. Search
keeps the full scan or the local cache, so the matcher's fields must stay in
the sidecar.

## Decisions

Confirmed 2026-09-30:

1. **`list` hides subagents too.** Its table and browser show top-level
   sessions only, like the picker; bare `show` does too, since it shares the
   browser. A parent carries a `· N subagents` hint,
   and a subagent is reached by searching. `list --json` keeps every row
   ([§1](#1-top-level-sessions-only)).
2. **Default to this repository, with a fallback to all projects, and make
   the scope easy to change.** The browser (`show`, `list`, `handoff`) and title search all start
   from the working directory's repository. When nothing there matches, they
   fall back to all projects and say so. `a` toggles the scope on screen, and
   `--all-projects` and `--project` change it on the command line
   ([§3](#3-scope-this-repository-first)).
3. **`list` takes a search query, with the same UI as the picker.** `list
   "<words>"` and the picker share the matcher, the row formatter, the
   browser, the `/` filter, and the scope heading. The only difference is
   what Enter does: `list` shows the session, the picker hands it off
   ([§4](#4-one-matcher), [§5](#5-one-browser-filter-as-you-type)).
4. **Subagent descriptions come from `.meta.json`**
   ([§6](#6-subagent-descriptions)).
5. **The live marker `●` reuses `activeSourceWindow` (2 minutes)**, so the
   dot and handoff's shared-checkout question agree ([§7](#7-rows)).
6. **Every session chooser is one component.** Bare `show`, `list`, and
   `handoff`, and every place they ask the person to pick one session, use
   the same browser. That includes `show --json` with no ID, and an
   ambiguous `show "<words>"` or `handoff "<words>"`. Differences are
   parameters, not separate code ([§5](#5-one-browser-filter-as-you-type)).

## Problem

Measured on the owner's Mac on 2026-09-30 (662 registrations: 653 Claude,
8 Cursor, 1 Codex):

1. **Subagents crowd out sessions.** 558 of the 662 registrations are
   subagents; 104 are top-level. The picker means to offer top-level
   sessions only, and `rows` drops subagent *registrations*
   (`topLevelRegistration`), but the archived list from
   `loadSessionsForBrowse` is never passed through `topLevelSessions`
   (`internal/cli/handoff_select.go`, the `for _, m := range archived`
   loop). So every uploaded subagent is offered. In one 45-row screen, 34
   rows were subagents of four orchestrator sessions (45, 33, 24 and 17
   children). The footer said "Showing 50 of 659"; about 100 could be
   handed off. `TestHandoffPickerAndToSkipSubagents` covers only a local
   subagent registration. Title search already filters archived subagents
   (`handoff_title.go`, `archiveMatches`). `list` shows every subagent as a
   row of its own, with nothing that names its parent.
2. **Titles show the least distinctive text.** A title is the first
   filtered human prompt, cut to 72 runes. Prompts an orchestrator writes
   start with the same preamble ("You are the REVIEWER AND FIXER for
   https://github.com/wangj…", "In the agent-archive repo (Go CLI, …"), so
   10 rows can read alike. The harnesses already record a better name, and
   the archive drops it:
   - Claude Code writes `{"type":"custom-title","customTitle":"Fix flaky
     retention hook-request test"}` (the name in the app's sidebar, set
     automatically or by `/rename`; the last record wins). 92 of the 95
     top-level Claude sessions have one.
   - Claude Code writes `{"type":"pr-link","prNumber":"213","prRepository":
     "wangjohn/agent-archive","prUrl":…}` for a PR the session opened or
     linked. 64 of 95 have one.
   - Each Claude subagent transcript `subagents/agent-<id>.jsonl` has a
     sibling `agent-<id>.meta.json` whose `description` is the name the
     parent gave the task ("Review and fix PR #208 (5b-1b)"). 543 of 561
     have one.
   - Cursor's composer data has a chat `name`. Codex keeps a `thread_name`
     per thread in `~/.codex/session_index.jsonl`.

   The privacy filter drops `custom-title` and `pr-link` as
   `unknown_record_type` (`ClaudeAdapter.FilterJSONL`,
   `internal/archive/adapters.go`), and Cursor's `name` as an omitted chat
   key (`cursorComposerConsumed`). Nothing reads `.meta.json` or
   `session_index.jsonl`.
3. **Columns that never vary.** Run inside a repo, PROJECT and HARNESS are
   the same on every row. Grouping by project (`GroupByProject`) does
   nothing with one project. It also splits one repository's checkouts,
   because `ProjectID` hashes the checkout path.
4. **No way to type what you remember, and two choosers.** `list` and bare
   `show` run the alt-screen key browser (`runSessionBrowser`). The handoff
   picker, `show --json` with no ID, and an ambiguous `show "<words>"` or
   `handoff "<words>"` run the older line-mode chooser (`pickBrowseRow` and
   `pickBrowseSession`). The two look and behave differently: one is a
   full-screen table with keys, the other a printed table with a prompt.
   Neither filters by text; both take only a number or an ID. `list` cannot
   search at all; its help says to use `handoff` for that. `show "<words>"`
   and `handoff "<words>"` match through `matchSessionsByQuery`, but print
   ambiguous matches in different formats.
5. **Agents search one field.** `handoff "<words>"` matches the whole query
   as a substring of the title, or an ID prefix. "hand off my Linux support
   session" works only if the first prompt happens to contain "linux
   support". On several matches it prints a table and exits 1, and there is
   no structured way to see candidates.

## Goals

1. The browser (bare `show`, `list`, and `handoff`) shows only top-level
   sessions. Subagents are found
   by searching, and are shown under their parent.
2. Every row is labeled with the name the person saw in their agent, when
   the harness has one. The first prompt is the fallback.
3. Run inside a repository, the picker, `list`, and title search look at
   that repository's sessions first, across all its checkouts and worktrees.
   One key or one flag widens or moves the scope.
4. Choosing a session looks and works the same in `show`, `list`, and
   `handoff`: one browser, one set of keys. A person can type a few words, a
   PR number, or a branch name to narrow it as they type.
5. One matcher serves the browser's filter, `handoff "<words>"`, `show
   "<words>"`, and `list "<words>"`, so a person and an agent get the same
   answer from the same words.
6. Nothing added leaves the privacy filter's guarantees: every new field
   comes from filtered records.

## Non-goals

- Searching transcript content. It is slow over the archive and noisy (a
  word appears in hundreds of tool results). Fields are metadata only, as
  today.
- Rewriting prompts heuristically (stripping "You are the…" preambles). With
  native names on 97% of top-level Claude sessions, the fallback matters
  rarely, and a heuristic would mislabel the rest.
- An LLM-written title. Titles stay deterministic.
- Flags for what to match. Words carry the query (`handoff "#212"`,
  `list "linux support"`). The only new flag is `--all-projects`, which is
  about where to look.

## Design

### 1. Top-level sessions only

`handoffPicker.rows` drops archived metadata with a `ParentSessionID`, as it
drops subagent registrations. `total` and the footer count only what can be
offered.

`list`'s table and the browser (so bare `show` too) drop subagents the same
way, before `--limit` applies. `--limit 50` therefore means 50 top-level sessions. The footer adds
how many were hidden: `42 sessions (318 subagent sessions hidden; search to
find one)`.

`list --json` keeps every row, subagents included. Each row carries
`parent_session_id`, scripts may count subagents, and changing that would
change the script-facing document's row set. A query ([§4](#4-one-matcher))
narrows `--json` rows like table rows.

Subagents come back through search ([§5](#5-one-browser-filter-as-you-type)),
never as unfiltered rows.

### 2. Names the person recognizes

**Filter (version 13, adapter 0.13.0).**

- Claude: admit `custom-title` with the key `customTitle`, and `pr-link`
  with `prNumber`, `prRepository`, and `prUrl`. `customTitle` is text the
  model wrote from the person's prompt, or the person typed. It passes the
  same `sanitizeValue` redaction as prompt text. `prRepository` and
  `prNumber` must match `git_activity`'s patterns. `prUrl` is kept only
  when it is the GitHub URL rebuilt from those two, else dropped. The
  records' `sessionId` and `timestamp` stay as for other records.
  `agent-name` (a copy of the custom title) and `last-prompt` (derivable
  from the turns) stay dropped.
- Cursor: consume the chat-level `name` (redacted as prompt text) into a
  record the parser reads. It is no longer reported as an omitted key.
- Changelog section, `dev/specs/privacy-filter.md`,
  `docs/security/privacy.md` (a session's name is now archived), and
  regenerated goldens, per [versions](../maintainers/versions.md).

A filter bump makes the collector re-read and republish every session whose
transcript still exists. That is the only way to recover names the old
filter dropped. A session whose transcript is gone keeps its first-prompt
title.

**Parser (0.17.0).** Three optional metadata fields. Optional fields do not
bump the metadata schema version; `schemas/metadata.schema.json`
(`additionalProperties: false`) gains them in the same PR.

| Field | Type | Derived from |
|---|---|---|
| `name` | string, collapsed like `title` (72 runes) | Claude: the last `custom-title`. Cursor: the chat `name`. A Claude subagent: its description ([§6](#6-subagent-descriptions)). Omitted when there is none. |
| `branch` | string, `branchPattern` | The last record's `gitBranch` (as `recordedWorkspace` reads it for handoff). Omitted when empty or `HEAD`. |
| `pull_requests` | array of `{repository, number, url}`, at most 20, first-linked order, deduplicated | Claude `pr-link` records. |

`title` keeps its meaning (the first filtered prompt), so nothing that reads
it changes. Display and search read the new fields through two helpers:

```go
// DisplayTitle is what a row shows: the harness's name, else the title.
func DisplayTitle(m Metadata) string
// LatestPR is the last linked PR, else the last pr_created in git_activity.
func LatestPR(m Metadata) (PullRequestLink, bool)
```

The picker's local rows (`localMetadata`) build their fields with the same
code as `BuildMetadata`. `firstPrompt` gives way to one exported function
both paths call:

```go
// SessionLabels derives name, title, branch and PRs from a filtered bundle.
func SessionLabels(bundle SourceBundle) (Labels, bool)
```

This also removes the one difference between the two today: local titles are
cut by display width, and published ones by runes.

### 3. Scope: this repository first

The browser (`show`, `list`, `handoff`) and title search find a *scope*
from the working
directory:

1. The repository key of the directory (`gitremote.Resolver.Key`, the same
   hash hooks record). A session is in scope when its `RepoKey` equals it.
   This spans checkouts, worktrees, and machines.
2. With no key on either side (no origin remote, or an older session), a
   local session is in scope when `sameProject(reg.ProjectRoot, dir)`, and an
   archived one when its `ProjectID` is in `projectIDs(dir)`, as `--latest`
   already decides.
3. Outside any project, there is no scope: every session, grouped by project
   as today.

**Changing the scope.** Every way below is named on screen, so none has to
be remembered:

- **`a` on screen.** In the browser, whatever command opened it, `a` (typed alone,
  like `n`, `p` and `q`) toggles between the scope and all projects. The
  heading always names what is shown and the other choice: `agent-archive ·
  42 sessions · a all projects`, or `All projects · 104 sessions · a
  agent-archive`.
- **`--all-projects`** on `handoff` and `list`: no scope.
- **`--project DIR|NAME`** on `handoff` and `list`: the scope of that
  directory instead of the working one. A value that is not a directory is
  matched as a project name, case-insensitively and exactly, against
  `project_name` and configured project labels. `handoff --project` exists
  today for `--latest` only; it now applies to every selection. Given with
  `--all-projects`, it is a usage error.
- **Project names are a search field** ([§4](#4-one-matcher)), so `handoff
  "personal_website blog"` reaches another project without a flag.

**Fallback.** When the scope holds nothing to show, the browser opens on
all projects, and the heading says `Nothing in agent-archive ·
showing all projects`.

Title search, in `handoff "<words>"` and `list "<words>"`, looks in scope
first, then everywhere, as `handoff` already looks on this machine before the
archive. The tiers, each tried only when the one before has no match:

1. an exact session ID (unchanged)
2. top-level sessions in scope
3. top-level sessions anywhere
4. subagents in scope, then anywhere

When tier 2 answers, a note says how many more match outside the scope,
without listing them: `1 match in agent-archive (3 more in other projects:
--all-projects or a project name finds them)`. `handoff` prints the note on
stderr, and `list` prints it in its footer. `list --json` keeps the same
tiers, and gains an optional `scope` object (`{"label": "agent-archive",
"all_projects": false, "fell_back": false, "outside_matches": 3}`). The
field is additive, so `listSchemaVersion` stays 4.

**Compatibility.** Run in a project, `list` and `list --json` now return
that project's sessions where they returned all of them. Scripts that want
everything pass `--all-projects`. The CLI reference and the changelog say
so.

### 4. One matcher

```go
// sessionQuery is parsed once from what the person or agent typed.
type sessionQuery struct{ words []string; prs []int }
func parseSessionQuery(s string) sessionQuery
// matches reports whether every word appears in some field of the row.
func (q sessionQuery) matches(r sessionFields) bool
```

- The fields are name, title, branch, project name, harness, and short or
  full ID prefix. Every word must appear in at least one field,
  case-insensitively. Words may match different fields, so "linux 212"
  matches the Linux session's name and its PR.
- A word `#N`, or a bare number of 1–6 digits, also matches a PR number
  exactly. A bare number is still a row index when typed at a browser's
  number prompt (see §5).
- There is no score. Order within a tier is newest activity first, as today.
  An exact ID still wins outright.

Callers:

- the browser's `/` filter and the line-mode filter (§5), in both the picker
  and `list`
- `handoff "<words>"`: replaces `matchSessionsByQuery` inside
  `handoffQueryResolver`. The 50-session local scan bound stays, and a
  local session's fields come from `SessionLabels`.
- `list "<words>"`: new. `list` takes one optional positional query. It
  applies the §3 tiers, then `--harness`, `--since`, and the other filters,
  then `--limit`. Interactive, it opens the browser with the query already in
  the `/` filter, so the person can edit it. Piped, it prints the table.
  With `--json`, it prints the document.
- `show "<words>"`: `resolveShowQuery` replaces `matchSessionsByQuery` with
  the matcher and the §3 tiers. Its direct reads are unchanged: an exact
  full ID is still read without listing. Several matches open the browser
  on a terminal. Without one, they print the same candidate table as
  `handoff`, with `Next: agent-archive show <ID>`.

When `handoff "<words>"` matches several sessions without a terminal, the
table gains the columns an agent needs to tell rows apart, and ends with the
exact next command:

```text
agent-archive: handoff: "flaky" matches 3 sessions in agent-archive; pass one ID:
  d7a77938  claude  just now  #213  Fix flaky retention hook-request test
  36a7d5ee  claude  36m ago   #209  Fix flaky hook-lock timeout test bound
  76941c69  claude  12h ago   #183  Fix flaky TestBrowserKeysRestoreTheTerminal
Next: agent-archive handoff d7a77938 --harness claude
      (or: agent-archive list "flaky" --json)
```

A subagent row is labeled `subagent of <parent short ID>`.

### 5. One browser, filter as you type

Every place a person chooses a session runs one component. It is the
alt-screen key browser that `list` and bare `show` use today
(`runSessionBrowser`, `browserKeys`), with one line-mode fallback for when no
key terminal can be opened. It replaces `pickBrowseSession` and
`pickBrowseRow`, whose separate line-mode loop goes away except as that
fallback.

The callers differ only in three parameters: where rows come from, what
Enter does, and the heading's verb.

| Caller | Rows | Enter | Heading |
|---|---|---|---|
| `list` | archive | show the details, then back to the list (browse) | none |
| `show` (no ID) | archive | the same as `list` (browse) | none |
| `show --json` (no ID) | archive | return the session; print its JSON (pick one) | `Show ·` |
| `show "<words>"`, several matches | the matches | show the details (browse) | `"words" matches N ·` |
| `handoff` before setup | bounded filtered native previews, already checkout scoped; o explicitly loads older | return the opaque selected row to the native caller | `Hand off local ·` |
| `handoff` (no selector) | local and archive, merged as today | return the session to hand off (pick one) | `Hand off ·` |
| `handoff "<words>"`, several matches | the matches | the same as `handoff` (pick one) | `Hand off · "words" matches N ·` |

```go
// browserMode is what Enter does with the chosen row.
type browserMode int // browseSessions: show details and return; pickSession: return the row
type browserSpec struct { Mode browserMode; Verb string; Rows []listRow; Query string }
```

Everything else is the same code for every caller:

- the rows, their formatter (§7), and the subagent folding
- the scope heading and `a`
- `/`, the highlight, and the line-mode filter
- the matcher (§4)
- paging, scrolling, and fitting to the window (#149)

Rows keep their own sources: `list` and `show` read the archive, and
`handoff` also offers this machine's sessions that are not uploaded yet. Making
`list` and `show` offer local sessions is out of scope. `list`'s ID column
is a column rule (§7), not a browser difference.

The handoff picker, `show --json`, and the two ambiguous-query pickers move
from line mode to the key browser. Their dependency interfaces gain
`openKeyTerminal` and `interrupts`, which `sessionBrowserDependencies`
already has. Every prompt after the handoff picker (destination, active
source) keeps reading through the shared answers reader, as
`prepareLaunchDir` requires. The browser must hand back the terminal with no
typed-ahead input lost.

**Key mode.** `/` opens a filter line at the bottom, as in `less`. Rows
narrow as each character is typed. While filtering:

- The first row is highlighted (▸), and ↑/↓ move the highlight.
- Enter acts on the highlighted row, and Esc clears the filter.
- Row numbers keep their unfiltered values, so a number seen before
  filtering still works.

Outside the filter, keys behave as today: numbers, `n`, `p`, `q`, arrows,
plus `a` for scope.

**Line mode.** An answer that is not a number, an ID, or a command word (`n`,
`p`, `q`, `a`) is a filter. The table redraws with the matching rows and
says `3 sessions match "flaky" · a number, more words, or Enter for all`. An
empty answer clears the filter, and quits when there is none, as today.

**Subagents while filtering.** A matching subagent appears indented under its
parent (`↳ Review and fix PR #208 (5b-1b)`), and the parent is shown even
when it does not match. It can be picked like any row. Unfiltered, a parent
with subagents carries a dim hint where the skill hint goes: `· 45
subagents`. The count comes from the subagent rows already loaded; once the
listing index exists, it comes from the parent's `LinkedSessions`.

### 6. Subagent descriptions

A Claude subagent's `description` is not in its transcript. It sits in the
sibling `agent-<id>.meta.json`. The collector reads that file when it filters
a subagent transcript (it is beside `TranscriptPath`), and prepends one
synthetic record to the filtered records:
`{"type":"subagent-meta","description":…}`.

- The description is redacted as prompt text and length-bounded.
- Nothing else from the file is kept. `worktreePath` names a local path, and
  `agentType` is already local-only state.
- The parser takes `name` from it.
- This is part of the filter (version 14) because it changes filtered
  output.

The alternative was the `description` input of the parent's Agent tool call
in the parent's filtered transcript. It was rejected because joining that
call to the child needs the `toolUseId`, which only the local `.meta.json`
names.

### 7. Rows

For every browser caller (§5):

- **Title:** `DisplayTitle`.
- **PR column:** `#213`, from `LatestPR`. Shown only when some row on screen
  has one.
- **HARNESS and PROJECT:** hidden when every row shown has the same value.
  The heading names that value instead (`agent-archive`, `claude`).
- **Live marker:** a leading `●` on a session active within
  `activeSourceWindow` (2 minutes), as its local activity reports it. This
  is the same test that makes handoff ask about a shared checkout. A session
  waiting on the person for longer shows no dot.
- **ID:** kept in `list`, where people copy it. In the picker it is dim;
  numbers select, and a typed ID is still accepted.

Target, run in agent-archive:

```text
Hand off · agent-archive · 42 sessions                   / filter · a all projects

 #     TITLE                                          PR     WHEN
 1  ●  Fix flaky retention hook-request test          #213   just now
 2  ●  Stop non-hook fsyncs under the request lock           just now
 3  ●  Implement Linux support for agent-archive      #212   1m        · 45 subagents
 4     Investigate a lint for durable writes…                2m
 5     Agent-archive open source readiness            #167   4m        · 17 subagents
 6     Fix termlaunch tests under umask 002           #202   11m
 7     Insights command with agent-archive            #204   12m       · 33 subagents
 8     Fix gitremote os/exec architecture check       #214   24m
```

`/208`, typed in that screen:

```text
Hand off · agent-archive · "208" matches 1                        Esc clear

 #     TITLE                                          PR     WHEN
 3  ●  Implement Linux support for agent-archive      #212   1m
    ▸  ↳ Review and fix PR #208 (5b-1b)               #208   3h
```

## Shared names

Change these here first.

| Owner | Name |
|---|---|
| PR 3 `internal/archive` | `Metadata.Name string` (`json:"name,omitempty"`), `Metadata.Branch string` (`json:"branch,omitempty"`), `Metadata.PullRequests []PullRequestLink` (`json:"pull_requests,omitempty"`); `type PullRequestLink struct { Repository string; Number int; URL string }`; `type Labels struct { Name, Title, Branch string; PullRequests []PullRequestLink }`; `SessionLabels(bundle SourceBundle) (Labels, bool)`; `DisplayTitle(m Metadata) string`; `LatestPR(m Metadata) (PullRequestLink, bool)`; `MaxPullRequests = 20` |
| PR 4 `internal/cli` | `type sessionScope struct { Label string; RepoKey string; Dir string; ProjectIDs []string; All bool }`; `scopeFor(env scopeDependencies, project string, allProjects bool) (sessionScope, error)`; `(sessionScope).contains(m archive.Metadata, reg *archive.SessionRegistration) bool`; `listDocument.Scope *listScope` (`json:"scope,omitempty"`) |
| PR 5 `internal/cli` | `sessionQuery`, `parseSessionQuery`, `(sessionQuery).matches`, `type sessionFields struct { Name, Title, Branch, Project, Harness, SessionID string; PRs []int }`, `fieldsOf(m archive.Metadata, project string) sessionFields`; `listRow.Children int` |
| PR 6 `internal/cli` | `listRow.Depth int`, `listRow.Live bool`; `sessionPicker.filter string`; `type browserMode`, `type browserSpec` (§5); `runBrowser(env sessionBrowserDependencies, spec browserSpec, …) (listRow, bool, int)`, which replaces `pickBrowseSession` and `pickBrowseRow` and backs `runSessionBrowser` |

## PR breakdown

| PR | What | Base | Parallel with |
|---|---|---|---|
| 0 | this plan; pointer from [handoff.md](handoff.md#selection) | main | — |
| 1 | picker drops archived subagents | main | 2, 4 |
| 2 | filter 13: Claude `custom-title`, `pr-link`; Cursor `name` | main | 1, 4 |
| 3 | parser 0.17.0: `name`, `branch`, `pull_requests`; `SessionLabels`; rows show `DisplayTitle` | 2 | 4 |
| 4 | scope for picker, `list`, and title search; `a`, `--all-projects`, `--project`; rows (PR column, hidden constant columns, `●`) | 1 | 2, 3 |
| 5 | one matcher; search tiers; `list "<words>"`; `list` hides subagents with the `· N subagents` hint; candidate table; skill text | 3 + 4 | — |
| 6 | one browser for `show`, `list`, `handoff`, and every ambiguous-query chooser; `/` filter; line-mode filter; subagents under parents | 5 | 7 |
| 7 | filter 14: subagent descriptions from `.meta.json` | 3 | 6 |
| 8 | docs (handoff and list guides, CLI reference, agent-skills guide) and live check | all | — |

`list` hides subagents in PR 5, not PR 1, so that from the moment they leave
the table, `list "<words>"` can find them.

### PR 1 — top-level only in the picker (~20 lines + tests)

Drop archived rows with `ParentSessionID != ""` in `handoffPicker.rows`
before the merge. Tests in `handoff_select_test.go`:

- an archived subagent is not offered
- the footer's total excludes it
- a local subagent registration whose parent is archived is still skipped

### PR 2 — filter 13 (~250 lines + goldens)

`FilterJSONL`'s allowlist gains `custom-title` and `pr-link`, and
`allowedKeys` gains their keys. `pr-link` values are validated against
`git_activity`'s repository and number patterns, and the URL is rebuilt. The
Cursor composer filter consumes `name`. Tests:

- a filter test per record type: kept, redacted, and a malformed `pr-link`
  dropped with a gap
- goldens regenerated
- the changelog's version-13 section lists exactly what is newly kept

### PR 3 — parser 0.17.0 (~300 lines + tests)

`SessionLabels`, the three fields, the schema, `DisplayTitle` in
`formatSessionRows` and `show`'s summary (Name, Branch, and PR lines).
`localMetadata` uses `SessionLabels`, and `firstPrompt` stays only for
`hasPrompt`. Tests:

- the last `custom-title` wins
- PRs are deduplicated and capped
- a `HEAD` branch is omitted
- a local row and a published row of one fixture get identical labels
- `TestFixtureOutputMatchesPublishedSchemas` passes

### PR 4 — scope and rows (~450 lines + tests)

`scopeFor` resolves the directory's repository key once, with a short
timeout, and falls back to paths. It applies to the picker, `list` (table,
browser, `--json` with `scope`), and title search.

- `a` toggles the scope, and the heading names it.
- `--all-projects` is added to `handoff` and `list`.
- `--project DIR|NAME` is added to `list`, and `handoff`'s applies beyond
  `--latest`.
- When nothing is in scope, the view falls back to all projects.
- The row changes are those in §7.
- Help, `cli_reference_test`, and the changelog's compatibility note are
  updated.

Tests:

- two worktrees of one repository are one scope (fake resolver)
- no origin falls back to `sameProject`
- outside any project, every session shows
- `--project` takes a directory or a name, and is refused with
  `--all-projects`
- `a` toggles in the browser, whichever command opened it
- an empty scope falls back and says so
- `list --json` carries `scope`
- columns are hidden when constant
- `●` follows `activeSourceWindow`
- goldens `testdata/browse/handoff-scoped.txt` and `list-scoped.txt`

### PR 5 — one matcher (~550 lines + tests)

- `sessionQuery`; `handoffQueryResolver`, `resolveShowQuery`, and `list` use
  the tiers of §3, and `matchSessionsByQuery` is removed.
- `show "<words>"` without a terminal prints the shared candidate table.
- `list` accepts a positional query, with help and `cli_reference_test`
  updated.
- `list`'s table and browser drop subagents before `--limit`, show the
  hidden count in the footer, and give parents the `· N subagents` hint (the
  picker gets the hint too).
- `handoff`'s candidate table gains the PR column and the `Next:` line.
- `internal/agentskills/skills/agent-archive/SKILL.md.tmpl` is updated:
  - words may be a topic, a PR number, a branch, or a project name
  - to see candidates as data, run `list "<words>" --json`
  - the "Never pick for them" rule stays

Tests:

- AND across fields
- `#N` and a bare number match PRs
- an in-scope match shadows others and the note counts them
- subagents only as the last tier
- `list` hides subagents but `list --json` keeps them
- `--limit` counts top-level rows
- `list "<words>" --json` keeps the document shape
- skill template golden

### PR 6 — one browser, filter as you type (~600 lines + tests)

`runBrowser` takes a `browserSpec` and backs every caller in the §5 table.
`runSessionBrowser` becomes its browse mode; `pickBrowseSession` and
`pickBrowseRow` are removed, and their callers pass a spec. The PR adds:

- `/`, the highlight, and Esc
- the line-mode filter
- `list "<words>"` opening with the filter filled in
- subagents indented under matching parents

A test lists every caller in the §5 table and checks that each reaches
`runBrowser`, so a new chooser cannot quietly grow its own loop. Tests:

- each caller in the §5 table opens the key browser on a terminal and the
  line-mode fallback without one, with the heading and Enter action its row
  gives
- key-mode filter narrows and Enter acts on the highlight, in browse and
  pick-one modes
- numbers keep their unfiltered values
- line-mode words filter and an empty answer clears
- a subagent match shows its parent
- a PTY test that a handoff picked in key mode still reads the destination
  prompt's typed-ahead answer
- goldens `keys-handoff-filtered.txt` and `keys-list-filtered.txt`

### PR 7 — subagent descriptions (~200 lines + goldens)

The collector passes the `.meta.json` beside a Claude subagent transcript to
the filter, which emits one `subagent-meta` record. Backfill's
`claudeSubagents` does the same for imported subagents. Tests:

- description kept and redacted
- other keys never kept
- a missing or malformed file changes nothing and records no gap (the file
  is optional)
- goldens regenerated

### PR 8 — docs and live check

Update the handoff guide, `list` in the CLI reference, and the agent-skills
guide. Live check on the owner's Mac (the steps are under
[Live check (PR 8)](#live-check-pr-8)):

- bare `show`, `list`, and `handoff` from inside agent-archive open the same
  browser on this repository with native names
- an ambiguous `show "flaky"` and `handoff "flaky"` open that browser too
- `a` shows all projects, and `list --project personal_website` shows that
  project
- `/linux` finds the Linux session from `show`, `list`, and `handoff`
- `/208` finds the PR-208 reviewer under its parent
- from Codex, "pull in my flaky retention test session from Claude Code"
  resolves without asking
- a Claude Code CLI session (not the desktop app) is checked for whether it
  writes `custom-title` without `/rename`, and the guide says what was seen

## Live check (PR 8)

For the owner, on the Mac that holds the real archive. Automated tests use
fakes and never touch a real Mac, so this is the one place the whole feature
meets real sessions and a real terminal. It takes about 30 minutes. Tick each
box when what you see matches the line that starts `Expect`. A step that does not
match is a finding: write down the command, what you saw, and the screen
width, and file it.

### Before you start

- [ ] **Build and install `main`** (PRs 1 to 8 merged), then refresh setup, which
  brings the hooks, skills, and background job up to date for the new binary.
  `--replace-current` puts it where `agent-archive` already is on `PATH`, so
  the hooks and skills keep pointing at a real path:

  ```sh
  cd ~/path/to/agent-archive          # your checkout of this repository
  git checkout main && git pull
  ./scripts/install-from-source.sh --replace-current
  agent-archive --version             # dev-<commit>, the commit you just pulled
  agent-archive setup --refresh
  agent-archive status
  ```

  Expect: `setup --refresh` prints a one-line result (or `nothing to refresh`),
  and `status` shows no warning that a skill "is out of date". A source build is signed ad hoc on macOS, so the
  first command that reads the archive may ask again for Keychain access to the
  R2 key; allow it.
- [ ] **Let the collector re-read old sessions.** Filter 13 and 14 changed what
  is archived (names, PR links, subagent descriptions), and only a re-read
  recovers them for sessions already uploaded.

  ```sh
  agent-archive sync
  agent-archive status
  ```

  Expect: `sync` finishes without an error and `status` reports `Ready`. A
  session whose transcript is gone from this Mac keeps its old first-prompt
  title, which is fine. If native names are missing everywhere below, run
  `agent-archive sync` again and wait a minute before concluding anything.
- [ ] **Work in a real terminal** (iTerm2 or Terminal, 100 columns or more) and
  start from inside this repository: `cd ~/path/to/agent-archive`. Every step
  below starts there unless it says otherwise.

### 1. One browser, on this repository

- [ ] **Bare `show`, `list`, and `handoff` open the same browser.** Run each in
  turn, look, and press `q` (`handoff` quits without handing anything off):

  ```sh
  agent-archive show
  agent-archive list
  agent-archive handoff
  ```

  Expect, in all three:
  - a full-screen list that replaces the screen and is gone when you quit;
  - a heading that names this repository and the other choice, for example
    `agent-archive · 42 sessions · claude · a all projects` (`handoff` starts it
    with `Hand off ·`);
  - **native names** in the TITLE column, such as `Fix flaky retention
    hook-request test`, and not the long first prompts of an orchestrator ("You
    are the REVIEWER AND FIXER for…"), at least for the sessions Claude Code
    named;
  - no subagent rows. A parent that has some says so on its row (`· 45
    subagents`);
  - a PR column (`#213`) when any row has one, and no HARNESS or PROJECT
    columns when every row shares them (the heading says `claude` instead);
  - the same keys: a number and Enter, `/` to filter, `a`, `q`.

  Also expect, in `handoff` only: a `●` before a session you used in the last 2
  minutes (open one in another terminal to see it), and `· not yet uploaded` on a
  session the archive does not have yet.

### 2. An ambiguous query opens that browser

- [ ] **`show "flaky"`:**

  ```sh
  agent-archive show "flaky"
  ```

  Expect: the same browser, heading `"flaky" matches 3 · agent-archive · claude ·
  Esc clear` (the count is however many sessions match; `agent-archive` and
  `claude` are there because every match shares them, as in step 1, and there
  is no `a`, because the matches are already chosen), `/flaky` on the
  bottom line, and `▸` on the first match. Press Enter to see its summary, `b`
  to return, Esc to clear the words and list the matches, then `q`.
- [ ] **`handoff "flaky"`:**

  ```sh
  agent-archive handoff "flaky"
  ```

  Expect: the same screen with the heading `Hand off · "flaky" matches 3 ·
  agent-archive · claude · Esc clear`. Press Esc, then `q`, to leave without
  handing anything off (on the filter line `q` is typed text).
- [ ] **Without a terminal it prints candidates and a `Next:` line.**

  ```sh
  AGENT_ARCHIVE_NONINTERACTIVE=1 agent-archive handoff "flaky"; echo "exit $?"
  ```

  Expect: exit `1` and, on stderr, a table of the matching sessions
  (short ID, agent, age, PR, title) followed by `Next: agent-archive handoff
  <ID> --harness claude` and `(or: agent-archive list "flaky" --json)`.

### 3. Scope: `a` and `--project`

- [ ] **`a` toggles the scope.** In `agent-archive list`, press `a`.

  Expect: the heading becomes `All projects · N sessions · a agent-archive`, with
  more sessions than before and a PROJECT column. Press `a` again to return.
  The same key does the same in `show` and `handoff`.
- [ ] **`--project` names another project.**

  ```sh
  agent-archive list --project personal_website
  agent-archive list --all-projects
  agent-archive list --project personal_website --all-projects; echo "exit $?"
  ```

  Expect: the first lists `personal_website`'s sessions under a heading that
  names it (and says `Nothing in personal_website · showing all projects` if the
  archive holds none, which is a finding if you know it does). The second starts on
  all projects. The third prints a usage error and exits `2`.

### 4. `/linux` finds the Linux session

- [ ] **In each browser, filter for it.** Run `agent-archive show`, press `/`, and
  type `linux`. Repeat in `agent-archive list` and `agent-archive handoff`.

  Expect: in each, the session named like `Implement Linux support for
  agent-archive` (PR `#208`, the last pull request it linked) is listed. `▸` is on the first match, newest
  first, so if another session mentions Linux it starts there and ↓ reaches
  this one. Press Esc to clear. Press
  `q` to leave (in `handoff`, do not press Enter on it, unless you want to
  start a handoff).
- [ ] **The same words from the command line:**

  ```sh
  agent-archive list linux --no-pager | cat
  agent-archive list linux --json | jq '.sessions[].session_id, .scope'
  ```

  Expect: a table with the Linux session, a footer with how many more match
  elsewhere if any, and in the JSON the same session and a `scope` object
  (`"label": "agent-archive"`).

### 5. `/208` finds the reviewer under its parent

- [ ] **In the handoff picker, type `/208`.**

  ```sh
  agent-archive handoff
  ```

  Expect: the parent (`Implement Linux support for agent-archive`, `#208`) and,
  indented under it, `↳ Review and fix PR #208 (5b-1b)`. The parent linked
  PR 208 itself, so it matches on its own and `▸` starts on it (or on an
  earlier match, if a newer session also mentions 208); ↓ moves the mark to
  the reviewer. Esc, then `q`.

  If the reviewer is missing, or its row shows a short ID instead of a
  description, its `.meta.json` was not there when the collector read it
  (filter 14 reads it beside the transcript). Running `sync` again does not
  help: a `.meta.json` that appears beside an unchanged transcript is not
  noticed (PR 7 under Deviations). Note whether the transcript and an
  `agent-<id>.meta.json` beside it are still on this Mac (in
  `~/.claude/projects/*/<native ID>/subagents/`, where `<native ID>` is the
  parent's `native_session_id` from `agent-archive show <parent ID> --json`),
  and file it.

### 6. From Codex, the words resolve without asking

The case this is for: Claude Code has hit its rate limit, so you continue in
Codex and pull the Claude Code session in from there. Claude Code takes no
part.

- [ ] **Say it in Codex, started in this repository:**

  ```text
  pull in my flaky retention test session from Claude Code
  ```

  Expect: Codex may ask permission the first time it runs `agent-archive`
  (that is the permission prompt, not a question about which session). It
  runs `agent-archive handoff "<words>" --harness claude` (or without
  `--harness`) with words like `flaky retention`, names the one session it
  found (`Fix flaky retention hook-request test`, PR `#213`) **without asking
  which one you mean**, says in one line which session it pulled in, and
  continues with it as context. Codex's hooks need not be approved for this:
  they archive Codex's own sessions, and reading the archive does not use
  them.

  A miss is asking "which of these?" over several candidates when "flaky
  retention" matches exactly one, or `--harness codex` on any command it runs:
  Claude Code is the agent the session was in, so `--harness codex` searches
  Codex's sessions only and prints `no session matches "flaky retention" …
  for codex`. The skill never adds `--to` (that starts another agent and
  belongs to `/handoff`). If you see a miss, record the exact commands Codex
  ran and what they printed. Check what the words match:

  ```sh
  agent-archive list "flaky retention" --json | jq '.sessions | length'
  ```

  Expect: `1`.

### 7. Does the Claude Code CLI name a session without `/rename`?

The names were seen in the desktop app's sessions; this checks the CLI, and
the answer goes into the guide.

- [ ] **Run a throwaway CLI session and look for `custom-title`.** In a scratch
  directory, not a project you archive:

  ```sh
  mkdir -p /tmp/name-check && cd /tmp/name-check
  claude                      # type one short prompt, wait for the reply, then /exit
  f=$(ls -t ~/.claude/projects/*name-check*/*.jsonl | head -1)   # or $CLAUDE_CONFIG_DIR/projects
  grep -c '"type":"custom-title"' "$f"
  grep '"type":"custom-title"' "$f" | tail -1
  ```

  Expect: either a count of `0` (the CLI wrote no name), or a count above `0` and
  a last line holding `"customTitle":"…"`. Do not `/rename` in this session.
  Then resume it with `claude --continue` (still in `/tmp/name-check`), send
  three or four more prompts, `/exit`, and run the `f=$(ls -t …)` line and the
  two `grep` commands again (the `f=` line first, in case the resumed session
  wrote a new `.jsonl`), since a name may arrive only after several turns.
- [ ] **Record what you saw in the guide.** On a branch from `main`, in
  `docs/guides/list-and-show.md`, find the comment `OWNER, live check` under the
  paragraph about names, replace the sentence above it with what you saw
  (for example: "The Claude Code CLI wrote a `custom-title` after the second
  prompt, with no `/rename`" or "The CLI wrote none until `/rename`, so its
  sessions are listed by their first prompt"), and delete the comment. Commit
  it there for the pull request below.

### 8. What only a real terminal can show

- [ ] **Keys typed right after a pick are not lost.** Archived sessions take a
  moment to read after you pick them, which is the window to type into.

  ```sh
  agent-archive handoff
  ```

  Press `/`, type a word that matches an older session, move the mark with
  ↓ and ↑, and press Enter on it. As soon as you press Enter, without waiting
  for anything to appear, type `q` and Enter.

  Expect: the picker closes, `Continue in:` and its choices appear, and the
  question is answered at once by the `q` you typed before it appeared, so
  `handoff` exits without waiting for you. Lost input looks like the question
  staying up, waiting at `Enter 1-N, p, c, w, or q` (N being the number of
  agents listed), for an answer you have
  already typed. Repeat once with nothing typed ahead to check that the question
  and its default (`[1]`) still show and work (answer `q`).
- [ ] **The terminal is restored after `q`, Esc, and Ctrl-C.** For each, run the
  line, do what the second column says, and check the third:

  | Run | Do | Expect |
  | --- | --- | --- |
  | `b=$(stty -g); agent-archive list; [ "$(stty -g)" = "$b" ] && echo restored` | press `q` | `restored`; the screen before the command is back, scrollback intact |
  | the same | press Ctrl-C | `restored`, the shell prompt returns at once |
  | the same | press `/`, type `x`, press Esc, then `q` | Esc clears the filter and does not quit (the list is still there), then `q` quits and `restored` shows |

  Also check after each: your typing echoes, the arrow keys recall history,
  Ctrl-C at the prompt works, and the cursor is visible. Do the same with
  `agent-archive handoff` (pick nothing: `q`, then Ctrl-C).

### When you are done

- [ ] Every box above is ticked, or each miss is filed. The `OWNER` placeholder
  in `docs/guides/list-and-show.md` is replaced. On the same branch, change the
  Status line at the top of this plan to "PRs 1 to 8 merged and live-checked",
  then open a pull request with both changes and merge it.

## Later

- Codex `thread_name` from `~/.codex/session_index.jsonl`. It is a second
  file per Codex home, keyed by thread ID, so it needs the same "file beside
  the transcript" handling as PR 7. Codex sessions are rare on the owner's
  Mac today.
- A highlight cursor in the unfiltered browser, so → could open a parent's
  subagents without searching.
- Searching the last prompt as a field. Claude's `last-prompt` record would
  need admitting, or the parser would need to keep the last human prompt.
- A configured default scope (always all projects), if `a` and
  `--all-projects` prove not enough.

## Deviations

- PR 2: `custom-title` and `pr-link` are rebuilt from typed values
  (`internal/archive/filter_session_labels.go`, as `compact_boundary` is) and
  their keys are admitted for those two records only, instead of adding
  `customTitle`, `prNumber`, `prRepository`, and `prUrl` to `allowedKeys`.
  `allowedKeys` applies to every record, so adding the keys there would have
  kept an unvalidated `prUrl` on a `user` record, and could not check the URL
  against the repository and number.
- PR 2: `prNumber` is written as a JSON integer whether Claude Code wrote a
  string (`"213"`, what real data holds) or a number, so PR 3 reads one
  shape: an integer in `prNumber`, not a string.
- PR 2: a dropped `pr-link` (or a `custom-title` with no usable text) is
  recorded as `unsupported_value_omitted` with the detail `record omitted`,
  not `unknown_record_type`, which stays the code for record types the
  filter does not know. A `prUrl` dropped for not matching is named in the
  `unknown_field_omitted` gap, like any other dropped key.
- PR 2: Cursor's `name` is written as `name` on the `session` record, the
  first record of the chat. That record is part of what a later snapshot must
  extend, and Cursor names a chat after its first messages, so the collector's
  prefix check (`nativeEvidenceExtends`) compares a Cursor session record
  without its name (`archive.SameNativeRecord`). Otherwise nearly every
  Cursor chat would get a `cursor_chat_rewritten` gap ("Cursor changed
  messages") for being named. A chat named or renamed later is republished
  with its new name and no gap.
- PR 2: `splitRepository` and `parsePRNumber` in `git_activity.go` are the
  filter's reading of a repository (`repoPartPattern`, as `git_activity`
  requires) and a number (digits only, 1 to `maxPRNumber`, as a pull
  request URL has it). `pullRequestEvent` keeps its own number check, which
  also takes a leading `+` from an MCP call's argument, so `git_activity`'s
  output is unchanged.
- PR 4: bare `show` (its browser and `show --json`'s picker) also starts in
  the working directory's scope, with `a`, because it shares the browser and
  `findBrowseSessions`; it has no `--all-projects` or `--project` flag yet.
  `show "<words>"` is unchanged (PR 5 replaces its matcher).
- PR 4: title search keeps today's matcher (`matchSessionsByQuery`) and only
  gains the scope-first order: this machine's in-scope sessions, then the
  archive's in-scope, then everywhere (this machine's, then the archive's). The
  subagent tier is PR 5's. The "N more in other projects" count covers what
  was searched: a title answered on this machine does not read the archive, which
  needs the network, so its other matches are not counted.
- PR 4: a scope is applied before `--limit`, so a `list` that can narrow (a
  scope, or a terminal that can switch to one) reads the whole archive
  listing instead of the index's newest page. `list --all-projects` piped, or
  with `--json`, keeps the index fast path. The index spec's scoped window
  will replace this.
- PR 4: a scope exists only inside a project: a directory with a repository
  key, or inside a configured project root. Elsewhere the working directory
  has no scope (every session). A directory named with `--project` is always
  the scope, so one in no project holds nothing and falls back with the
  heading saying so. A scope made from a directory in a git checkout is named after the
  repository's main checkout (`repositoryName`: a worktree's `.git` file
  points into it, and a subdirectory walks up to it), not after the directory
  or the sessions a command happened to read, so every view, heading, title
  note, and `--json` document names it alike, with or without the archive.
- PR 4: `listScope.label` is the scope's name even when it is turned off
  (`all_projects` says so); `outside_matches` counts the sessions outside the
  scope that match the same filters. The object is omitted outside a project.
- PR 4: the PR column first read the last `pr_created` event of
  `git_activity` (`createdPRLabel`); PR 3 replaced that with `LatestPR`
  (the last linked pull request, else the last created). `listRow.Live` (owned by
  PR 6 in the shared-names table) is added here for the dot; local rows only
  (the archive says nothing about what is running here).
- PR 4: HARNESS and PROJECT are left out only for two or more rows (one row
  shares its value with nothing), and the verbose table keeps every column.
  With one project the table is not grouped, so a repository's checkouts are
  one list.
- PR 4: `handoff --project NAME` with `--latest` still reads its value as a
  directory (a project that no longer exists on disk is still searched by
  its ID). `--project` and `--all-projects` are usage errors with `--file`,
  and `--all-projects` with `--latest`; `--project` sets where a launched agent
  starts only with `--latest`, as before.
- PR 4: the picker's line-mode `a` takes precedence over a short ID prefix
  typed as `a`, as `n` and `p` do. In the key browser `a` pressed first
  switches the scope, so a short ID that starts with `a` (IDs are hex) is
  picked by its row number there; `n`, `p`, and `q` never met this, as they
  are not hex digits.
- PR 3: `SessionLabels` is the one place the four labels are derived, but
  `BuildMetadata` reaches it through `deriveLabels(bundle, view)`, which takes
  the normalized view it has already parsed, so a transcript is not parsed
  twice. `SessionLabels(bundle)` parses and calls the same `deriveLabels`; the
  picker's `localMetadata` calls it. Its `ok` is what `firstPrompt`'s was:
  whether the transcript holds a prompt (a Cursor text transcript counts).
- PR 3: `firstPrompt` is gone rather than kept: with the title coming from
  `SessionLabels`, `hasPrompt` is all that was left of it, so it reads the
  prompt test directly. The picker's local titles are now derived as
  published ones are (runes, and a Cursor text transcript now has a title
  where the local row had none).
- PR 3: `show`'s summary heading is `DisplayTitle`. Rather than a `Name` row
  that would repeat the heading, a session with a name gets a `Prompt` row
  holding its first prompt (which was the heading before), so `show` loses
  nothing; `Branch` and `PRs` rows are added, `PRs` being the linked pull
  requests as `owner/repo#n`, beside the existing `Git` row of the ones its
  commands opened or merged.
- PR 3: `PullRequestLink.URL` is `omitempty` (a `pr-link` may carry no
  `prUrl`), and the parser keeps a URL only when it is exactly the GitHub
  address rebuilt from the repository and number, as the filter does; it does
  not rebuild one the record lacked, since a GitHub Enterprise link is dropped
  by the filter and the host is not known. A `pr-link` whose repository or
  number is out of shape is skipped.
- PR 3: `branch` reads the last record's `gitBranch` through the same helper
  handoff uses (`recordedBranch`), then applies `validBranch`; a last value
  that is malformed or `HEAD` gives no branch, rather than falling back to an
  earlier record's.
- PR 3: `TestFilterV13NewRecordsDoNotChangeDerivedMetadata` (PR 2) asserted
  that the parser ignored the new records; it is now
  `TestFilterV13NewRecordsChangeOnlyNameAndPullRequests`, asserting that they
  change `name` and `pull_requests` and nothing else. The non-interactive
  ambiguous-`show` list also prints `DisplayTitle`.
- PR 5: the matcher's `prs` is parallel to `words` (`prs[i]` is word i read as
  a PR number, 0 when it is not one), so each word is checked on its own. A
  word `#N` matches a PR number exactly, or the text `#N` in a field when the
  next character is not a digit (so `#21` finds neither PR 213 nor "PR
  #213"); a bare number of 1 to 6 digits matches a PR exactly or as any other
  text does (a substring). `fieldsOf` collects every PR the session linked
  (`pull_requests`) and every `pr_created` in `git_activity`, deduplicated,
  not merged ones; `LatestPR` stays the PR column's.
- PR 5: an exact session ID is a single word equal to a full ID, or to an
  ID's first 8 characters (the short ID the table shows). It wins before the
  tiers, as §3's first tier, across every session the search reads: an exact
  ID of an out-of-scope session or a subagent outranks an in-scope title that
  mentions it, so a candidate table's `Next:` command takes the row it names.
  In `handoff` the exact full-ID reads (registered, then the archive) still
  come first, unchanged; then an exact short ID among this machine's sessions,
  and then, for a word of 8 hex characters only, the archive's (subagents
  too), so other words still need no network when this machine answers them.
- PR 5: `--harness`, `--since` and the other filters are applied when the
  archive is listed, before the tiers, rather than after them, so a filter can
  never empty a tier and hide a lower one. `--limit` applies last, as planned.
- PR 5: the tiers' shared code is `searchSessions` (archived metadata: `list`,
  `show`) and `matchPool` (`handoff`'s rows, which read this machine's sessions
  before the archive). Local subagent registrations are never searched (the
  picker never listed them; one is still handed off by its full ID), so
  `handoff`'s subagent tier is the archive's.
- PR 5: `list`'s table and browser always read every session's metadata (the
  cache applies) instead of the index's newest page, so subagents can be left
  out before `--limit` and counted for the footer; `list --json` without a
  query keeps the index fast path. The index spec's per-parent counts will
  bring the fast path back. A table with an index-limited count ("Showing 2
  or more") no longer exists.
- PR 5: with a query, `list` in a scope shows only the first tier's sessions
  in that scope, and an empty scope falls back to all projects as PR 4's
  fallback does, so `a` toggles between "in scope" and "everywhere" for the
  same words. The note about matches elsewhere is `list`'s footer line (in
  place of the count when nothing was cut, after it otherwise), and the
  browser, which names the toggle in its heading, prints the same line.
  `scope.outside_matches` counts the sessions of the answering tier outside
  the scope (top-level ones, or subagents when those answered).
- PR 5: the footer's hidden count reads as §1 has it, ending in a period:
  `42 sessions (318 subagent sessions hidden; search to find one).`; when
  `--limit` cut the list, the same parenthesis follows the existing `Showing
  N of M session(s)`. A query hides nothing: subagents are a tier, not hidden.
- PR 5: `show "<words>"` has no `--all-projects` or `--project`, so it always
  searches from the working directory's scope, and prints the "N more in other
  projects" note on stderr too, where the plan names only `handoff` and
  `list`. Its several-matches terminal chooser is still the line-mode
  `pickBrowseSession`; PR 6 replaces it with the browser.
- PR 5: the candidate table is `candidateList` (`candidates.go`), shared by
  `handoff` and `show`. It shows the PROJECT column only when the rows span
  projects and the PR column only when a row has a PR; it names the scope in
  its first line (`matches 3 sessions in agent-archive`) only when the scope
  answered. The `(or: agent-archive list "<words>" --json)` line repeats
  `--harness`, `--all-projects` and `--project` when `handoff` was given them,
  and is left out when the query was cut for display or holds control
  characters, since the command would not be the one searched. `show`'s table
  ends with the same two lines, the `Next:` one being `agent-archive show <ID>`.
- PR 5: a subagent is labelled `· subagent of <parent short ID>` after its
  title (the parent's first 8 characters, `listRow.Parent`), in every table
  that holds one, not only the candidate table; PR 6's indented `↳` row
  replaces it. A parent's hint is `· N subagents` (`· 1 subagent`), after the
  skill hint, counted from the sessions already loaded
  (`listFormatOptions.Children`).
- PR 5: `list "<words>"` takes one positional argument, before or after the
  flags; a blank one is a usage error, and a query nothing matches prints `No
  archived sessions match "<words>".` and exits 0, as an empty list does.
  Pre-filling the browser's `/` filter with the query is PR 6's; until then it
  opens over the matched rows.
- PR 5: `sessionFields` declares one field per line (`Name`, `Title`, `Branch`,
  `Project`, `Harness`, `SessionID`, then `PRs []int`), because the repository's
  lint (LV1003) rejects several names in one declaration; the names and types
  are those of the shared-names table.
- PR 5: "short or full ID prefix" (§4) is read per word with two limits. A
  word that is a PR number (`#N`, or a bare number of 1 to 6 digits) never
  matches the start of an ID, and any other word does only from 4 characters
  (`minIDPrefixWord`, git's shortest abbreviation). Session IDs are random
  hex, so without them `handoff 21` meant for PR 21 also matched about one
  session in 256 by its ID, a 3-digit PR one in 4096, and a short word such as
  "add" or "bed" one in 4096, making PR and word searches ambiguous in a large
  archive (and a test flaky when a fixture's random ID began with "212"). The
  old matcher took a prefix of any length, but only of the whole query, which
  had no PR numbers. An exact full ID, or exactly the 8-character short ID,
  still wins outright (`exactIDWins`); an ID that starts with digits only is
  found by more than 6 of its characters.
- PR 7: the `.meta.json` reaches the filter through
  `ClaudeAdapter.FilterSubagentJSONL(r io.Reader, metaJSON []byte)`, not a
  field on the `Adapter` interface, which stays a reader of one transcript. The
  collector's `filterReader` calls it when the adapter is Claude Code's, the
  registration has a `ParentSessionID`, and the transcript is named
  `agent-<id>.jsonl` (`archive.SubagentMetaPath`: the sibling is
  `agent-<id>.meta.json`). The file is read through the same non-blocking
  regular-file open as the transcript and at most `MaxSubagentMetaBytes`
  (16 KB) of it, after an `Lstat` that refuses a symbolic link (and a check
  that the file opened is the one found), so a link never makes it read a
  file elsewhere; anything else (missing, a link, a directory or pipe,
  oversized, not a JSON object, no non-blank string `description`) is as if
  absent, with no gap and no warning. Only `description` is read;
  `worktreePath` and `agentType` are never kept, and are not reported as
  omitted keys.
- PR 7: the description is cut to 512 bytes, after redaction (a cut before it
  could leave half a secret that no pattern matches) and then redacted again
  until stable, with a `content_truncated` gap. The plan said "a sane cap";
  512 is far under a prompt string's 64 KB, and the parser cuts `name` to 128
  runes anyway.
- PR 7: the `subagent-meta` record is written only when the transcript has
  records of its own. The collector reads an empty subagent transcript as
  "not written yet" (`subagentTranscriptEmpty`) and waits, and a transcript
  with no recognized record as unsafe; the record must change neither. It has
  no `sessionId`, `agentId`, or timestamp, so the provenance checks are
  unchanged. The filter also accepts a `subagent-meta` record in its own
  input, because a retained snapshot is filtered again when the filter
  version changes (`refilterBundle`), and keeps at most one (the file's wins,
  else the first; later ones are dropped with an `unsupported_value_omitted`
  gap). A top-level transcript that holds such a line would get a name from it,
  as it would from a `custom-title` line; both are the person's own file and
  pass the prompt rules.
- PR 7: the plan put a subagent's `name` in parser 0.17.0's field table. It is
  in parser 0.18.0 (`DefaultParserVersion` 0.17.1 to 0.18.0), because 0.17.0
  and 0.17.1 shipped without it: `deriveSessionName` also reads
  `subagent-meta`. The record is first, so a `custom-title` (which a subagent
  transcript does not normally hold) comes later and wins, as the last name
  always does. A subagent's own prompts are sidechain records, which are not
  the person's, so its `title` is empty and `name` is its only label.
- PR 7: backfill needs no code. `claudeSubagents` only lists the
  `agent-<id>.jsonl` files (and already ignores the `.meta.json` beside them);
  the plan's filter call there (`FilterTranscriptFile`, which has no
  registration) is a check that the collector would register the subagent and
  its output is not kept. An import saves a subagent candidate, and the
  collector filters it with the registration it assembles from the parent
  (`ParentSessionID` set, `TranscriptPath` the same file), which is where the
  description is read, so imported and hook-reported subagents are the same
  path (`TestImportedSubagentIsPublishedWithItsDescription`).
  `FilterTranscriptFile` (`handoff --file`, backfill's check) reads no
  `.meta.json`.
- PR 7: a changed or late `.meta.json` is not treated as a rewrite. The
  collector's check that a new snapshot extends the last one
  (`nativeEvidenceExtends`) leaves a leading `subagent-meta` record out of
  both sides (`archive.WithoutSubagentMeta`), as it does a Cursor chat's name;
  without that, a subagent captured before its file existed would be blocked
  with `transcript_rewritten` the next time its transcript grew. The scan
  signature that lets an unchanged session skip a read is the transcript's
  size and modification time only, so a `.meta.json` that appears or changes
  beside a transcript that does not change is not noticed until the transcript
  changes or a filter or adapter version changes (a parser version alone
  re-derives metadata from the retained snapshot, not the file). A subagent is
  registered when its `SubagentStop` fires, or when backfill finds it, so
  the file is normally there at the first read; one captured before it was
  written and never rescanned stays without a name.
- PR 7: `filter-golden.json` gains one entry, for the new
  `claude-subagent.jsonl` fixture, whose golden test filters it with the
  `claude-subagent.meta.json` beside it; every existing entry is unchanged,
  since a transcript filtered without a file is byte for byte what it was.
  Fixtures filtered before it that the golden file never listed were left
  unlisted. Two fuzz targets are added (`FuzzSubagentMeta`,
  `FuzzSubagentMetaDropsSecrets`) beside the description shapes in
  `fuzzSecretRecords`.
- PR 6: `browserSpec` carries `Choices *scopeChoices` (PR 4's rows for the
  scope and for all projects, which `a` switches between) in place of the
  plan's `Rows []listRow`, and also `Command`, `Store` and `NoPager`. A caller
  whose rows are already chosen (an ambiguous word's matches) wraps them with
  `rowChoices`, a scope-less choices. `runBrowser(ctx, env, p, stdout, stderr,
  spec) (row, picked, code)` takes the prompter, the writers and a context
  (the details read the archive, and the `show` resolver has one) besides the
  environment and spec. `runSessionBrowser` is folded into it, not kept as a
  wrapper: its browse mode is `Mode: browseSessions`. The sessionBrowser's
  details functions take the context as a parameter.
- PR 6: `Query` fills the filter for every caller. `list "<words>"` opens over
  the scope's top-level sessions with the words in the filter, and the filter
  searches every session of the scope (subagents too, and past `--limit`; row
  numbers continue past the last unfiltered row, and a subagent has none; the
  table's rows keep the table's numbers even when the search, read later,
  orders them otherwise, as the handoff picker's does when one of this
  machine's sessions was active in between (`withTableNumbers`); in
  the handoff picker a session not uploaded yet is searched only within the
  picker's 50 rows, as a title search reads them, since its title takes
  reading its transcript), so editing the words widens the search; Esc clears
  the filter to the plain list. PR 5's tiers still decide which scope the
  browser opens on (a scope with no match falls back to all projects, saying
  so while the filter holds the opening words; the scope's sessions are still
  one `a` away, since only the words missed them) and the note about matches
  elsewhere, which shows while the filter holds the opening words; the
  subagent tier's "last, only when nothing else matches" is replaced in the
  browser by nesting under the parent (§5). The ambiguous `show` and `handoff`
  choosers pass their matches as the rows and the words as `Query`, so their
  heading is `"words" matches N` and Esc lists the matches. Their interactive
  picker lists every match, not the first 20 (`handoffCandidateLimit` still
  bounds what a pipe or an agent is shown): it scrolls and filters, and a cap
  would have hidden what Esc and the filter are for.
- PR 6: `show "<words>"` with several matches on a terminal browses (Enter
  shows the details, as the table says) unless `--json` or `--transcript` is
  given, when it picks one session (`Show ·`) for the caller to print, since
  the browser cannot print JSON or a transcript for a session it also keeps
  open. `resolveShowQuery` takes `noPager` and `pickOne` for it.
- PR 6: while the filter line is open, `a`, `n`, `p` and `q` are typed text
  (§5 says they act outside it), so the other scope is one Esc away; digits
  are typed text too, so in key mode a row is chosen by its number after
  Esc, and in line mode at once. A row the filter finds past the table's limit is
  numbered on from the table's last row, and is chosen in the filter: with
  the highlight and Enter in key mode, and in line mode by its number while
  the page drawn shows it. Once the filter is gone (and in line mode on
  another page of it), a number past the table picks nothing (key mode
  refuses it, line mode takes it as words), so it never names a session the
  person has not seen; the table's own numbers always pick, as §5 asks; the
  heading while filtering names `Esc clear`, not `a`. Esc clears the words and
  closes the line in one press, and Backspace on an empty filter closes it. In
  line mode `a` still switches the scope with a filter on, and keeps it.
- PR 6: the `/ filter` hint is on the key-mode status line, not in the
  heading as §5's picture has it, so a `list` outside any project (which has
  no heading) names it too. The highlight starts on the first row the words
  match, not the first row shown, as §5's `/208` picture has it: a parent
  shown only for its subagent is not what Enter acts on until ↑ moves there. The heading while filtering is `<verb> · <scope>
  · "words" matches N · … · Esc clear` and the marked row has `▸` before its
  live dot (`listFormatOptions.Cursor`); a subagent is `  ↳ title`, indented
  two spaces, with no `· subagent of` hint, and the parent keeps its `· N
  subagents` hint while filtering. Columns a filter's rows no longer share
  come back (`filteredFormat`: PR, HARNESS, PROJECT, the live dot).
- PR 6: in line mode an unmatched number is words (a PR number, `208`), and
  so is a word shorter than `minIDPrefixWord` (4) that is not a number, even
  when a listed ID starts with it: once words filter, `db` or `add` must not
  pick the one session whose ID begins so (a typed ID needs 4 characters, as
  the matcher's ID prefix does). `n`
  and `p` always turn pages (`Everything is on this page.` when there is one),
  and more words add to the filter (`Enter for all` clears it). The question
  now says `words to filter`, which wraps one row more at 40 columns, so
  `list-page-2.txt` and two width-dependent tests moved a row. Typed text
  that is not a number, ID or command word no longer gets a "bad answer"
  message, so the tests pinning those messages were removed.
- PR 6: the picker, `show --json` and the ambiguous pickers now enter the
  alternate screen when stdout is a terminal, even where keys cannot be read
  (as `list` always did), so the line-mode fallback also leaves no table
  behind; the tests that read the picker's output strip the screen's control
  sequences.
- PR 6: handoff's answers are one `bufio.Reader` over a `handedBack` reader
  (`typedInput`), so the key browser can read the real terminal
  (`prompter.source`) and hand back what it read but did not use: text and
  Enter typed after the pick, with Backspace applied and other keys dropped,
  go to the front of the answers. Closing the key terminal then does not flush
  the input still waiting (it is read by the prompts as lines). Only a pick
  hands back; quitting still flushes, as before.
- PR 8: `list`'s help says `--json` lists every session "without WORDS"
  (with words it lists the tier that answers, as the table does) and that
  `--all-projects` is not for use with `--project`; no other help text changed.
- PR 8: the live check's Esc item is "Esc, then `q`": in the session browser
  and in `stats`, Esc clears a filter or typed text and never quits (so a split
  arrow key cannot close the screen), so the restore check presses Esc mid-use
  and quits with `q`. The words item is run from Codex, pulling in a Claude
  Code session, rather than from Claude Code handing off to Codex: the reason
  to switch agents is usually that Claude Code has hit its rate limit, when it
  cannot run anything. It expects the session to be found without a
  which-one question; the `agent-archive` skill never adds `--to`, which
  belongs to `/handoff`.

## Revision index update

The v3 session-addressed revision index, with v2 read compatibility, now supplies activity dates and per-parent counts from
freshly covered canonical headers. Noninteractive tables, including implicit repository scopes, can select
their top-level limit before metadata bodies are read. Interactive browsing,
named explicit project scopes and search retain exhaustive reads. See
[listing performance and revision protocol](../maintainers/list-performance.md).
