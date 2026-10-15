# Session list UI — engineering plan

Status: in progress; planned 2026-10-14, [decisions](#decisions) confirmed the same day.
PRs 1–4 and 6 are merged (PR 6, remaining callers and `show`, in #560);
PR 5 (cursor, detail strip, footer) is in #562, so the live check below has
not run.
Where this plan and the code differ once PRs merge, the code is the reference
and differences go under Deviations.

Goal: anyone can read the session list at a glance. Each session takes **one
line**, sessions are **grouped by day**, titles read like names rather than
raw prompts, and the details (ID, extra projects, subagents) appear only
for the highlighted row. The same rendering serves every place that lists
sessions: `list`, bare `show`, `show <words>`, `handoff` and the `/` filter.

## Today's problems (from the 2026-10-14 screenshot)

1. **Every row wraps onto two lines.** `columnWriter`
   ([list_table.go](../../internal/cli/list_table.go)) pads each column to its
   widest cell, and titles are up to 128 runes plus the dim skill and
   subagent hints. Nothing fits rows to the terminal width (`oneRow` only
   cuts the heading, status and notice), so `HARNESS  ID` and every row's
   tail wrap.
2. **PR and WHEN sit far from the title** on a wide terminal, and WHEN is cut
   off ("2 minutes ag").
3. **Titles are raw first prompts** (`archive.DisplayTitle` → `Title`), such
   as "# Files pasted by the user: …" or "help me understand what …".
4. **"N days ago" repeats on every row**, and there are no landmarks in a
   50-row list.
5. **The footer takes three lines** of flag advice that only scripts need.
6. **There are two time formats**: `relativeAge` ("2 minutes ago") in lists
   and `compactAge` ("2h ago") in the `show` summary.

## Target

Interactive (key mode, alt screen):

```
Your sessions  86 · all projects                         / search   a this project

TODAY
› 1  ● Claude  Review historical project command        agent-archive   #505    2m
  2  ● Claude  Cloud session archive/retrieval options  built-in-browser #514  13h

EARLIER THIS WEEK
  3  ● Codex   Better loading indicator when running …  agent-archive          Mon
──────────────────────────────────────────────────────────────────────────────
Review historical project command
Claude · agent-archive + 1 more project · PR #505 · 4 subagents · last active 2 minutes ago · id dfb972ba
──────────────────────────────────────────────────────────────────────────────
↑↓ move   Enter open   / search   number + Enter jump   q quit       1–14 of 86
```

- **Columns:** `#`, agent (colored dot plus name), title, project, PR,
  short time. The ID leaves interactive rows.
- **Title column width:** `clamp(remaining width, 24, 56)`, so the project,
  PR and time stay next to the title.
- **Narrow terminals:** project goes first, then PR, then the agent name
  (the dot stays). A row never wraps.
- **Day groups:** Today, Yesterday, Earlier this week, Last week, Earlier
  this month, Older. The row's time is short: `2m`, `13h`, `Mon`, `Oct 9`,
  `2025-03-01`.
- **Detail strip:** shows the highlighted row's full display title, agent,
  every project, PR, subagents, full relative time and short ID.
- **Line mode** (no key-at-a-time input): the same one-line rows and day
  groups, with no cursor and no strip.
- **Piped / non-TTY:** one line per row, with no grouping and no color:
  `WHEN  AGENT  PROJECT  PR  ID  TITLE`. TITLE comes last so it never pushes
  other columns. The title is uncut, and the ID stays.

## Architecture

All new rendering goes in one shared place in `internal/cli`, used by every
caller listed above.

### `sessionLine` (new: `session_line.go`)

Pure functions over `listRow`, with no I/O:

- `agentLabel(harness) (dot, name string, color)` — Claude / Codex / Cursor /
  other, with color only when `textStyle.color` is set.
- `shortWhen(now, t)` and `longWhen(now, t)` — the single time format.
  `relativeAge` and `compactAge` both move onto it.
- `dayBucket(now, t, loc) bucket` — local-time day groups, with "this week"
  starting Monday.
- `listTitle(display string) string` — the readable list title 
  PR 2 below). The full `DisplayTitle` is kept for
  the strip, `show` and search.

### Layout (`list_table.go`)

- **`sessionLayout`** replaces `tableHeader`/`tableCells`/`columnWriter`
  for the non-verbose human table. It is computed once per screen from the
  rows and the terminal width (`terminalSize`; 0 means unknown = the plain
  table). It sets each column's width, which optional columns are dropped,
  and the title width.
- **`renderRow(row, layout, style)`** returns exactly one terminal line
  (`visibleWidth` ≤ width). `truncateVisible` cuts the title at a word
  boundary with `…`.
- **`withColumns`** (list_format.go) keeps its job of hiding constant
  harness/project columns and moving them into the heading. It feeds
  `sessionLayout`.
- **`sessionTableGroups`** gains day grouping 
  [decision 1](#decisions)). Groups keep `continued` for paging.
- **The verbose table** (`--verbose`) stays as it is. It is a debugging view.

### Browser (`list_browse.go`, `list_browse_keys.go`, `browser_filter.go`)

- **The cursor is always on in key mode**, not only while `/` filters.
  `Highlight` / `cursorCell` already exist for the filter; the unfiltered
  view starts using them too.
- **`screen()` / `fit`** budget the strip's lines (2 + rules) the same way
  they budget the footer.
- **Number + Enter and short ID + Enter still work** as they do today.

## PRs

Each PR is reviewable alone and leaves `main` shippable. The order is
1 → 3 → 4 → 5 → 6. PR 2 can land any time before 6.

### PR 1 — Shared formatting primitives

`session_line.go`: `agentLabel`, `shortWhen`/`longWhen`, `dayBucket`.

- `relativeAge` (handoff.go) and `compactAge` (show_summary.go) are rewritten
  on top of these. `relativeAge` keeps its output for now.
- Table tests cover the time boundaries (midnight, Monday, DST, year change)
  and unknown harnesses.
- No visible change except the `show` summary's age, which moves to
  `shortWhen`.

### PR 2 — Readable list titles

`listTitle`:

- Keep the first line and first sentence.
- Strip a leading markdown heading or quote marks.
- Upper-case the first letter.
- Collapse pasted blobs to a label: "Files pasted by the user: …" becomes
  "Pasted files: <first name>".
- Turn slash commands with long hashes into short forms:
  `/code-review d3e5…` becomes `Code review d3e5256…`.

Notes:

- Deterministic, with no model call.
- Applied only to the list title cell. The strip, `show`, the `/` filter and
  `--json` keep `DisplayTitle`.
- Table tests built from the real titles in the screenshot.

### PR 3 — Width-aware one-line rows (the core fix)

- Add `sessionLayout` and `renderRow`, plus the new column order and the
  agent column. Drop ID from interactive rows.
- Move the skill/subagent hints out of the title cell. The count shows as a
  dim `+4` after the title only when it fits; the full text goes to the
  strip in PR 5.
- Change the piped table to `WHEN AGENT PROJECT PR ID TITLE` 
  [decision 3](#decisions)).
- Update all 7 list goldens in `testdata/browse`.
- Add width-matrix tests: 60/80/100/140/220 columns, asserting no line
  exceeds the width.
- Update the text assertions in about 11 test files (handoff_select,
  inspect, list_format, list_browse, list_browse_keys, scope_rows,
  search_commands, pager, phase3, handoff_older, backfill_import).

### PR 4 — Day groups

- Day grouping in `sessionTableGroups`, with headings styled like today's
  project headings, in both line and key mode.
- `pageSessionGroups` keeps "(continued)" across pages.
- The project grouping (`GroupByProject: true` in inspect.go:172,
  handoff_select.go:136/178, show_resolve.go:135, handoff_title.go:507,
  list_browse.go:1031) is replaced by day grouping, with project as a
  column ([decision 1](#decisions)).
- The `/` filter's results are grouped by day too, keeping match order
  within a day.

### PR 5 — Cursor, detail strip, one-line footer

- In key mode, ↑↓ move a persistent highlight and Enter opens it. Scrolling
  follows the cursor.
- The detail strip under the list uses `longWhen`.
- The footer becomes one line: keys plus a `1–14 of 86` position.
- `printListFooter`'s flag advice stays for the piped table only. The
  "N subagent sessions hidden" count moves into the heading's dim text.
- PTY test (`list_browse_keys_pty_test.go`): arrow, Enter, resize, and the
  strip redraw.

### PR 6 — Remaining callers and `show`

- `candidateList.print` (candidates.go): the non-TTY ambiguous `show` /
  `handoff` matches use the piped table's columns and `listTitle`.
- Native handoff rows (`identityRow`, handoff_native.go) go through
  `listTitle` / `agentLabel`.
- The handoff "no session" fallback table (handoff.go:849) gets the same
  column order.
- The `show` summary's context line (`summaryContext`) uses `agentLabel`
  and `longWhen`, so the summary matches the strip.
- Docs: `docs/` command reference screenshots/text, and a CHANGELOG entry
  calling out the piped column change and pointing scripts at `--json`.

## Decisions

Confirmed by the owner 2026-10-14:

1. **Day groups replace project groups.** Project becomes a column. When
   every row shares a project, `withColumns` already hides the column and
   names the project in the heading. *Recommended.*
2. **The cursor is always on in key mode.** Enter with an empty prompt opens
   the highlighted row (today it does nothing). Number + Enter still jumps.
   *Recommended.*
3. **The piped column order changes.** Anything parsing `list | …` text
   breaks. `--json` is the stable contract, and the changelog says so.
   *Recommended.*
4. **Keep "subagents".** The strip and hints keep the existing word, which
   most people already know. *Owner decision, 2026-10-14.*

## Live check (after PR 5)

- Check iTerm2 and Terminal.app at 80, 120 and 200 columns, in light and
  dark themes.
- No row wraps. Resize mid-browse and confirm it redraws on one line.
- `list`, bare `show`, `show <words>`, `handoff`, `/` filter, and `a` scope
  toggle.
- `list | cat` and `list --json` unchanged in shape apart from decision 3.

## Deviations

- PR 4: day headings carry no row count ("TODAY", or "TODAY (continued)"
  on a page or screen that starts partway through the day), and a list
  whose sessions all fall on one day still shows that day's heading.
- PR 4: the verbose table is one flat table. It was grouped by project
  before; day groups add nothing to a debugging view with full timestamps.
- PR 5: the heading is the scope heading the code already draws
  (`agent-archive · 42 sessions · … · a all projects`), now with
  `· N subagents hidden`. A prompt line (`Number or ID to show:`) stays
  under the footer line so typed numbers echo, and a message takes the
  footer line's place until the next key. The `1–14 of 86` position counts
  the rows loaded (the `o older` batch), not the scope's total, which the
  heading names; `o older` is the last key a narrow footer drops but Enter.
  Esc out of the filter keeps the highlighted row when the unfiltered table
  has it, else the first row; `/` keeps the highlight until words are
  typed. The cursor mark is the existing `▸`. The strip has no
  "+ N more projects": a session records one project.
- PR 6: the non-interactive candidate table (`show` / `handoff` words with
  several matches, on stderr) shows the full display title, not
  `listTitle`, following decision 3's piped table: it is what an agent or a
  log reads. It uses the piped table's helpers (`tableHeader`,
  `tableCells`), so it gains a column header and AGENT is the lower-case
  harness name `--harness` takes. Titles are capped at 160 columns, above
  any captured title (129 runes), against a hostile sidecar.
- PR 6: before setup, `handoff`'s non-interactive local matches print the
  same candidate table, with the whole native ID in the ID column, in place
  of the `local <ID> (<app>): <title>` lines. The local picker groups by day
  and shows `listTitle` for a real title; a native ID that stands in for a
  missing title is shown as it is.
- PR 6: the `--latest` "no session" fallback keeps its own columns, now
  `WHEN AGENT MACHINE COMMAND` under a header, with `shortWhen` and the
  `agentLabel` name.
- PR 6: the `show` summary's context line is the `agentLabel` name (no
  dot; colored with `textStyle.agent`), the project, and
  "last active <longWhen>". `compactAge` is removed.
