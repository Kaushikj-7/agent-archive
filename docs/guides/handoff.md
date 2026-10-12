# Continue a session in another agent (handoff)

`handoff` continues a session in another coding agent. Ran out of Claude Code
usage halfway through a task, or want Codex to take a look? Hand the session
off, and the other agent starts with it as context. You don't copy or paste
anything.

Handoff transfers filtered conversation context. Prepare the receiving checkout
separately: files, uncommitted edits, and running processes do not move.

Start with [local sessions before setup](#before-setup-native-local-sessions),
[handoff inside an agent](#from-inside-an-agent-handoff), or
[handoff from a terminal](#from-a-terminal). Advanced details cover
[separate checkouts](#working-in-a-separate-checkout---worktree),
[session search](#naming-a-session-in-words), and
[what the receiving agent sees](#what-the-receiving-agent-is-told).

There are two ways to do it:

- **Inside an agent.** In Claude Code, type `/handoff codex`. In Codex, ask
  for `$handoff`. The other agent opens in a new terminal tab (a new window
  in Terminal) with this session as its context.
- **From a terminal.** Run `agent-archive handoff`, pick a session (or name one
  in words: `agent-archive handoff "flaky retention"`), and press Enter at
  `Continue in`. The other agent starts in this terminal.

## Before setup: native local sessions

After installing, run `agent-archive handoff` inside a checkout to browse
existing Claude Code and Codex conversations. No bucket, capture hooks,
credentials, registrations, or background job are created. Cursor automatic
discovery is deferred; `--file PATH --harness cursor` remains available.

```sh
agent-archive handoff
agent-archive handoff --latest --harness claude --to codex
agent-archive handoff "OAuth" --harness codex --to claude
agent-archive handoff NATIVE_ID --harness claude --source local
```

The scope is this directory and its descendants, using the transcript's
recorded working directory and canonical paths. `--project DIR` requires an
existing directory before setup. `--all-projects` explicitly searches other
projects; an empty checkout or word search never broadens automatically.
Default app homes and `CLAUDE_CONFIG_DIR` / `CODEX_HOME` overrides are searched.
These selectable IDs are native local IDs, qualified by harness. Full IDs or
unique prefixes (at least four characters) work beyond the preview window.
Conflicting identities require the explicit-file path.

The picker initially inspects filtered labels for the newest 50 candidates.
Use **o · Load older sessions** to inspect another 50; clear a key-mode `/`
filter with Esc first. Redrawing, scrolling and filtering inspect no additional
transcripts. Word queries search only loaded previews and report their bounded
coverage. A partial label falls back to the first filtered prompt or native ID.
Discovery inspects at most 10,000 files, with a cumulative 64 MiB discovery and
preview read budget per invocation. After header inspection finishes, unused
header reservations become available for previews. If the budget is exhausted,
the picker keeps remaining verified native IDs selectable, marks their labels
uninspected, and disables older preview loading. Searching these rows can match
IDs and checkout facts, but cannot match uninspected conversation text.

Local `--latest` uses transcript file modification time. Copying or restoring
files can change that ordering. It requires complete identity/checkout discovery
and skips the exactly identified calling session. When discovery is incomplete,
it refuses automatic selection and offers the terminal picker for an explicit
choice. Scripts receive up to 20 known candidates with copyable, harness-qualified
local handoff commands. These candidates are not a claim about the newest
session; even `--latest --to` requires an explicit choice after refusal.
`--to` without a selector
uses an exact available native identity, offers the terminal picker when one
cannot be identified, or requires explicit selection inside an agent; it never
substitutes latest. Automatic current identity depends on the app exposing a
matching session variable. Disposable live Claude/Codex checks on both supported
platforms remain unverified; synthetic home/launcher tests cover the command
flow. Setup installs agent skills only when explicitly requested later.

Rendering creates no archive. Launch files are private (directory 0700, file
0600) under `agent-archive-local-handoffs-<uid>` in the temporary directory and survive
asynchronous launch. Each user has a separate namespace; reuse and cleanup refuse
directories owned by another user, public directories, and symlinks. Subsequent
local handoffs remove owned directories older
than seven days, best effort; files can remain until another invocation or OS
cleanup. Trimmed output has no automatic saved full copy before setup: use
`--max-bytes 0` or explicit `--output`. `--source archive` requires setup.

Once configured, the existing registration and archive eligibility rules apply;
this utility does not make other native history eligible. Run `setup` later for
backup and cross-machine use, with explicit capture and backfill choices.

## From inside an agent: /handoff

```text
/handoff codex
```

Setup installs a `handoff` skill for the apps it sets up. In Claude Code,
`/handoff codex` hands off the session you are in; `/handoff claude` and
`/handoff cursor` work too. In Codex, ask for `$handoff` (or pick it from
`/skills`) and name the agent. In Cursor, ask the agent to hand off; it
reads the same skill as Codex. With no agent named, the skill picks another
agent than the one you are in: Codex from Claude Code, Claude Code from Codex
or Cursor.

The skill runs `agent-archive handoff --to <agent>`, which opens the agent in
a new tab or window (see [what happens](#what-happens)) and returns at once.
Inside an agent `handoff` never prompts, even when the agent's shell is a
pseudo-terminal, so it cannot hang there. The agent you are in reports where
it opened:

```text
handoff: opened codex in a new iTerm2 tab
```

Both agents can then edit the same checkout; see
[working in a separate checkout](#working-in-a-separate-checkout---worktree)
to give the new one its own.

The skill files are `~/.claude/skills/handoff/SKILL.md` (in
`$CLAUDE_CONFIG_DIR` when set) for Claude Code, and
`~/.agents/skills/handoff/SKILL.md` for Codex and Cursor. Each carries a
marker line: setup replaces and uninstall removes only a file with it, so
delete that line to keep an edited copy. A file already at that path without
it is left alone, and setup says so. An installation with
`AGENT_ARCHIVE_HOME` set runs the command with it, and leaves the skill of
another installation sharing the same home folder alone.
`agent-archive status --verbose` lists the installed files, and `status`
warns when one was written by an earlier release (after you upgrade
`agent-archive`); `agent-archive setup --refresh` (which the installer runs)
refreshes it. To bring a session *into* the agent you are in, without opening
another one, ask it in words: the `agent-archive` skill runs
`handoff "<words>"` for you ([agent skills](agent-skills.md)).

## From a terminal

```sh
agent-archive handoff
```

Pick a session in [the picker](#the-picker) (a number, or `/` and a few
words to find one), and `handoff` asks where to continue:

```text
? Continue in

  1) Codex (default)
  2) Claude Code

  [p] print
  [c] copy to the clipboard
  [w] write to a file
  [q] quit

› Choose [1]:
```

Only agents whose CLI is on `PATH` are numbered. Enter takes the default:
`handoff.default_to` for the session's harness in `config.json` (see
[configuration](../reference/configuration.md)) when that agent is installed,
else Codex for a Claude Code session and Claude Code for a Codex or Cursor
one, else the first listed. A number (or an agent's name) starts that agent
in this terminal, and `handoff` returns when it exits.

The other answers don't start an agent. `p` prints the handoff, through the
pager when it is longer than the screen. `c` copies it with `pbcopy` on macOS,
`wl-copy` on Wayland, or `xclip`/`xsel` on X11. The copy choice appears only when
a provider is installed and, on Linux, a display is configured. In a headless
session, use `w` to write a file instead. `w`
asks for a file name (default `handoff-<short id>.md` in the directory an
agent would start in: the current one, or `--project DIR` with `--latest` or,
before setup, with any selection; `~/` is your home directory), writes it with
mode 0600,
and asks before replacing a file. A write that fails is reported and asked
again; Enter or `q` then gives up. With no agent installed, Enter prints.

To skip the question, name the agent with `--to`:

```sh
agent-archive handoff --to codex                       # pick the session
agent-archive handoff --latest --harness claude --to codex
agent-archive handoff SESSION_ID --to claude
agent-archive handoff SESSION_ID --to cursor
agent-archive handoff "fix the auth bug" --to claude   # words, as for show
```

`handoff` asks where to continue only on a terminal, and not with `--to`,
`--output`, `--format json`, or `--no-preamble`. Inside Claude Code, Codex,
or Cursor (`CLAUDE_CODE_SESSION_ID`, `CODEX_THREAD_ID`, or `CURSOR_AGENT` is
set), or with `AGENT_ARCHIVE_NONINTERACTIVE=1`, it never asks anything, even
on a terminal; `AGENT_ARCHIVE_NONINTERACTIVE=0` turns asking back on. See
[scripting and other outputs](#scripting-and-other-outputs) and
[configuration](../reference/configuration.md#environment-variables).

## What happens

1. **The session is read and filtered.** Everything the agent gets has passed
   the same [privacy filter](../security/privacy.md) as the archive. A
   session on this machine is read from its transcript as it is now, so nothing
   needs to sync first.
2. **The record goes to a private file.** It is written to
   `handoffs/launch-<session>-<time>-<random>/handoff.md` in the data
   directory: a folder of its own, file mode 0600. The file is kept so a
   resumed session can read it again, and removed after 7 days like other
   saved handoffs. With `--file` before setup there is no data directory, so
   it goes to a private folder in the system's temporary directory, kept for
   the system to clear.
3. **The agent is started with a short prompt** asking it to read that file.
   The file begins with a preamble telling the agent what it is reading and
   how to get more context (see
   [what the receiving agent is told](#what-the-receiving-agent-is-told)).
4. **It opens here or in a new tab.** On a terminal, the agent runs there and
   `handoff` returns when it exits. Anywhere else, including inside an agent
   (or with `AGENT_ARCHIVE_NONINTERACTIVE=1`) even when its shell is a
   pseudo-terminal, as when an agent runs `handoff --to` for you, it opens
   without asking in a new tmux window when `$TMUX` is set, else a new
   iTerm2 or Ghostty tab (by `$TERM_PROGRAM`), else a new Terminal window,
   and `handoff` returns at once. `--here` forces this terminal (and fails
   without one, or inside an agent); `--new-window` opens a new window or
   tab even from a terminal. When the agent exits with an error there, the
   window waits for Enter so you can read it. When no window can be opened
   (not macOS and not inside tmux), `handoff` exits 1 and prints the command
   to paste into a terminal; it never runs the agent without one.

`--to` starts the installed `claude`, `codex`, or Cursor `agent` (else
`cursor-agent`) CLI in the current directory (or `--project DIR` with
`--latest`, and before setup with any native selection). Codex gets that directory with `--cd` and Cursor with
`--workspace`. Claude Code runs in it and is given only the handoff's folder
with `--add-dir`, since it reads outside the project only from an added
directory. The launched agent does not inherit the calling agent's session
variables (such as `CLAUDE_CODE_SESSION_ID` or `CODEX_THREAD_ID`); your
settings, such as `CLAUDE_CODE_USE_BEDROCK`, pass through. It never starts a
remote agent.

### Passing arguments to the agent

Arguments after `--` go to the agent, after any set for it in `config.json`'s
`handoff.args` (see [configuration](../reference/configuration.md)). A second
`--` is refused, since the prompt follows one:

```sh
agent-archive handoff SESSION_ID --to codex -- --model o3
```

Give every option its value (`--model o3` or `--model=o3`). The prompt comes
right after these arguments, so an option left without its value at the end
would take the launcher's `--` (Claude Code, Codex) or the prompt itself
(Cursor) as its value.

## Working in a separate checkout (`--worktree`)

Two agents editing one checkout get in each other's way. `--worktree` gives
the launched agent a checkout of its own:

```sh
agent-archive handoff SESSION_ID --to codex --worktree
agent-archive handoff --worktree   # pick the session and agent on a terminal
agent-archive handoff SESSION_ID --to claude --worktree --branch try-codex-fix
```

It runs `git worktree add -b handoff/<id> <repo>-handoff-<id> HEAD`, where
`<id>` is the first 8 characters of the session ID and `<repo>-handoff-<id>`
is a directory beside the checkout; `--branch NAME` names the branch instead.
Your uncommitted changes and untracked files come along: the changes are
recorded with `git stash create` and applied in the new worktree, which
leaves your checkout and your stash list untouched, and untracked files are
copied with their permissions (symlinks as symlinks). Changes you had staged
arrive unstaged, except new files, which arrive staged. Ignored files, such
as `.env` or `node_modules`, are not copied, and submodules are neither
checked out nor carried (run `git submodule update --init` in the
worktree). A checkout in the middle of a merge, rebase, cherry-pick, or
revert is refused: finish or abort it first. Launched from a subdirectory,
the agent starts in the same subdirectory of the worktree. The worktree's
path and branch are printed:

```text
handoff: created worktree /Users/me/src/app-handoff-3f2a9c1e on branch handoff/3f2a9c1e (carried 2 changed and 1 untracked files)
```

Without `--to`, the worktree is made only once you pick an agent at
`Continue in`; print, copy, and write make none.

An existing branch or directory of that name is an error, never reused;
pick another branch with `--branch`, or remove the old worktree with
`git worktree remove`. If copying your changes fails after the worktree was
made, the worktree is left in place and the error says where. When you are
done, merge or cherry-pick the branch and run `git worktree remove` on the
directory.

Without `--worktree`, if the session being handed off is on this machine, was
active in the last 2 minutes, and belongs to the checkout the agent would
start in, a terminal asks first:

```text
? The source session was active just now; where should the new agent continue?

  In the same checkout, both agents can edit its files until one stops.

  1) Continue in the same checkout
  2) Cancel (default)
  3) Continue in a new git worktree

› Choose [2]:
```

`y` continues, `N` (the default) cancels with nothing launched, and `w`
creates a worktree as `--worktree` does. Without a terminal, or inside an
agent (or with `AGENT_ARCHIVE_NONINTERACTIVE=1`), `handoff` prints a warning
and continues. When an agent hands off its own session with
`--to`, as `/handoff` does, it is active by definition; a note is printed
and nothing is asked.

## Naming a session in words

The argument is a session ID or words; quote several words as one argument:

```sh
agent-archive handoff "fix the auth bug" --harness codex
agent-archive handoff "fix the auth bug" --to claude --worktree
agent-archive handoff "#212"                    # a pull request number
agent-archive handoff "personal_website blog"   # a project, then a topic
```

Use one or two distinctive words: a topic, a PR number, a branch, or a project
name. `handoff` matches them the way `list "<words>"` and `show "<words>"` do.

This section describes `handoff` after setup. [Before
setup](#before-setup-native-local-sessions) there is no archive: words search
only the loaded previews of Claude Code and Codex sessions in this directory
and below (the newest 50 at first), match a name, title, branch, project, app,
or the start of a native ID but not a pull request number, can match a session
with no prompt yet, and never widen to other projects unless you pass
`--all-projects`. Several matches open the picker on a
terminal; without one, the first 20 are printed as `local <ID> (<app>): <title>`
and the command exits 1, saying how many matched, with no `Next:` line.

**What a word matches.** Every word must appear, case-insensitively, in some
field of the session: its name (the one your agent shows in its sidebar), its
title (the first prompt), its branch, its project name, or its app
(`claude`, `codex`, `cursor`). Words may match different fields, so "linux 212"
finds the session named for Linux that opened PR 212. Two more ways to match:

- **A pull request number.** `#212` or `212` (a number of 1 to 6 digits) matches
  a pull request the session linked or created. It matches the number exactly,
  so `#21` does not find PR 212, and it never matches the start of a session ID.
- **The start of a session ID.** Any other word of 4 characters or more also
  matches a session whose ID starts with it, as git's shortest abbreviation
  does. Shorter words don't, since IDs are random hexadecimal and a 3-character
  word would match a session by chance. A whole session ID, or the 8-character
  short one a table shows, names that session outright, even when another
  title mentions it, and even when it has no prompt, which words never offer.

Only metadata is matched, never the rest of the conversation. For a session
on this machine the name, title, branch, and linked pull requests are read
from its transcript file, so a session that was never uploaded is found too.

**Where it looks, in order.** Each step is tried only when the one before found
nothing, and the first that has a match answers:

1. An exact session ID, full or short.
2. Top-level sessions in the current scope: this repository's (every checkout
   and worktree of it, and its sessions from other Macs), when you are in one.
   This machine's sessions come before the archive's. `--project DIR|NAME`
   looks in that project instead, and `--all-projects` skips this step.
3. Top-level sessions in every project, this machine's first, then the
   archive's.
4. Subagent sessions ([below](#subagent-sessions)), in the scope and then
   everywhere.

When step 2 answers, a note on stderr says how many more match elsewhere:
`1 match in agent-archive (3 more in other projects: --all-projects or a
project name finds them)`. A project's name also works as a word
(`"personal_website blog"`), so you need no flag to reach another project.
Outside any repository or configured project there is no scope, and steps 2
and 3 are one.

The search looks at this machine's sessions first, which needs no network and
no upload, then the archive's when none of this machine's match. An archive
that cannot be reached does not fail words this machine can answer. A single
word of 8 hexadecimal characters that is not the short ID of one of this
machine's sessions is also looked up in the archive as a short ID first.
`--source local` or `--source archive` limits the search to one. Only the 50
most recently active sessions on this machine that have a prompt are searched
by words; an older one that was uploaded is found in the archive.
`--harness` narrows the search. When the command runs inside a Claude Code or
Codex session, that session is not offered for words (as `--latest` passes over
it), unless `--to` is set, which hands off a session the caller names.

**One match** is handed off, and then everything else applies to it: `--to`,
`--worktree`, the `Continue in` question on a terminal. This holds even when the
archive holds other matches, so name an ID when in doubt.

**Several matches** are never guessed between.

- On a terminal, the [picker](#the-picker) opens on just those sessions, with
  the words already in its filter (heading `Hand off · "flaky" matches 3`).
  `▸` marks the first match and Enter hands it off (↑ and ↓ move it). On the
  filter line digits and `q` are typed text, so press Esc first to pick by
  number or to quit with `q`; Esc lists the same matches without the filter.
  Every match is listed, however many.
- Without a terminal, and inside a coding agent (where nothing is asked), they
  are printed to standard error and the command exits with code 1, so the
  caller can ask which one and run it again with an ID:

  ```text
  agent-archive: handoff: "flaky" matches 3 sessions in agent-archive; pass one ID:
    d7a77938  claude  just now        #213  Fix flaky retention hook-request test
    36a7d5ee  claude  36 minutes ago  #209  Fix flaky hook-lock timeout test bound
    76941c69  claude  12 hours ago    #183  Fix flaky TestBrowserKeysRestoreTheTerminal
  Next: agent-archive handoff d7a77938 --harness claude
        (or: agent-archive list "flaky" --json)
  ```

  Each row has its short ID, app, project (when the rows span several
  projects), age, pull request (when a row has one), and title. The first line
  names the scope when the scope answered. `Next:` is the exact command for the
  first row; edit the ID for another. The second line is the `list` command that
  prints the archive's matches for the same words as data (a session not
  uploaded yet is only in this table; it repeats `--harness`, `--project`, and
  `--all-projects` when you gave them, and is left out when the words were cut
  to fit or hold control characters). At most the 20 newest are listed, with a
  count of the rest; add words, a PR number, or `--harness` to narrow.

With none, the message points to `agent-archive list`.

### Subagent sessions

A session an agent started for a task (a Claude Code subagent) is its own
archived session, but the picker, `list`, and the candidate table leave it out:
it would crowd the sessions you mean. Words find one only when no top-level
session matches (step 4), and a subagent is named by what its parent asked of it
(for example "Review and fix PR #208 (5b-1b)"), so a PR number or a word from
that task finds it. A candidate table labels it `subagent of <parent short ID>`,
and in the picker, while you filter, it appears indented under its parent
(`↳ Review and fix PR #208 (5b-1b)`). Handing one off works like any session,
by its ID or by picking it. A parent shows how many subagents it has
(`· 45 subagents`), and `list --json` without words keeps every row,
subagents included. Only the archive's subagents are searched: one that is
registered on this machine is handed off by its full ID.

## Where the session comes from

With no session selector, `handoff` opens the session picker when stdin and
stdout are terminals. In a script or pipeline, pass a session ID or words,
`--latest`, or `--file`. The picker never opens when a coding agent runs the
command, even in a pseudo-terminal: `CLAUDE_CODE_SESSION_ID`,
`CODEX_THREAD_ID`, or `CURSOR_AGENT` in the environment (or
`AGENT_ARCHIVE_NONINTERACTIVE=1`) turns prompts off, and `handoff` without a
selector is a usage error (exit 2) instead. `AGENT_ARCHIVE_NONINTERACTIVE=0`
turns them back on; see
[configuration](../reference/configuration.md#environment-variables).

With `--to` and no selector, run from inside an agent, `handoff` hands off
the session it is running in without asking: the one Claude Code names in
`CLAUDE_CODE_SESSION_ID` or Codex in `CODEX_THREAD_ID`. Cursor names no
session, so inside Cursor (`CURSOR_AGENT` is set) it takes the newest Cursor
session for the current directory, as `--latest --harness cursor` would.
That is not a prompt, so it works while prompts are off. Otherwise a terminal
gets the picker, and anything else is told to name a session.

A session registered on this machine is read from its transcript as it is now, so
a handoff right after you stop needs no sync and works while collection is
paused; nothing is uploaded. Otherwise the session is downloaded from the
archive, which is how a second machine hands off a session from the first.
`--source local|archive` forces one or the other; with neither, a local
transcript that cannot be read falls back to the archive's copy. Either kind
can be launched with `--to`.

`--latest` names its choice on stderr, passes over sessions with no prompt
yet, and, when run by an agent that names its own session (Claude Code does,
through `CLAUDE_CODE_SESSION_ID`), skips that session unless `--to` is used.
With `--to`, the calling session is eligible because it is the source being
handed off. `--latest` matches the current directory's project, not projects
beneath it.

### The picker

The picker is the one session browser that `list` and bare `show` open, and
that an ambiguous `show "<words>"` or `handoff "<words>"` opens too (see
[browsing on a terminal](list-and-show.md#browsing-on-a-terminal)). It has the
same keys everywhere; only what Enter does differs. Here Enter hands the
session off, and the heading starts with `Hand off ·`. This section describes
the picker after setup; [before setup](#before-setup-native-local-sessions) its
heading starts with `Hand off local ·`, it lists only this machine's Claude
Code and Codex sessions in this directory and below, including ones with no
prompt yet (an unnamed one is titled by its native ID; no PR column, `●`,
`· not yet uploaded`, or `a`), and `o` loads 50 older ones.

```text
Hand off · agent-archive · 5 sessions · claude · a all projects
#  TITLE                                                            PR    WHEN            ID
1  ● Fix the retry budget in the upload queue                       #213  just now        d7a77938
2    Stop rereading the config on every hook call                         4 minutes ago   36a7d5ee
3    Add a status line for paused collection · 1 subagent           #212  38 minutes ago  76941c69
4    Investigate slow listings on a large archive                         5 hours ago     5b0c1e22
5    Rename the export flag and update the docs · not yet uploaded  #204  2 days ago      9f3a64d0
5 session(s).
All · / filter · type a number and Enter · q quit

Enter number (or unique short SESSION_ID) to hand off, or q to quit:
```

*What it lists.* Top-level sessions, newest activity first: this machine's,
including ones not uploaded yet, together with the archive's. A session that is
both appears once. Subagent sessions and sessions with no prompt yet, on this
machine or in the archive, are left out (one uploaded before its first prompt
is listed once its transcript here has one), and `--harness` narrows the list
(`--source local` or `--source archive` limits where it looks). At most 50 rows
are listed; the filter below searches the rest. When the archive cannot be read (offline, say), the picker lists this
machine's sessions and says why archived ones are missing.

*What a row shows.*

- **The title is the name your agent gave the session** (the one in Claude
  Code's sidebar, or a Cursor chat's name), else a preview of its first prompt.
- **PR** is the last pull request the session linked or created, and appears
  when some row has one.
- **`●`** marks a session on this machine that was active in the last 2
  minutes, the same test that makes `handoff` ask about a shared checkout (see
  [working in a separate checkout](#working-in-a-separate-checkout---worktree)).
  The archive says nothing about what is running here, so only this machine's
  sessions get one.
- **`· not yet uploaded`** marks a session on this machine that the archive does
  not have yet (shown once the archive was read, so a failed read marks nothing).
  You can hand it off; it is read from its transcript as it is now.
- **`· 45 subagents`** says how many subagent sessions a session has. They are
  not rows; search for one.
- The **ID** is dim: numbers choose rows, and a typed short ID works too.
  HARNESS and PROJECT columns appear when rows differ in them. When every row
  shares one value, the heading names it (`claude`) instead.

*Which sessions: the scope.* In a git repository with an `origin` remote, or
inside a project you configured, the picker starts on that repository's
sessions, across all its checkouts and worktrees and from your other Macs, and
the heading says so. Elsewhere it lists every session, grouped by project. If the
repository has none yet, it opens on all projects with `Nothing in agent-archive ·
showing all projects` in the heading.

- Press `a` (typed alone) to switch between the repository and all projects. The
  heading names the other choice: `a all projects`, or `a agent-archive`.
- `--all-projects` starts on all projects, and `--project DIR|NAME` on another
  project: a directory (its repository), or a project name matched to the
  archived project name or a configured label, exactly but ignoring case. The two
  together are a usage error. Neither applies to `--file`, and `--all-projects`
  does not apply to `--latest`, which names the current directory's project
  (`--project DIR` moves it).

*Keys.* Type a row number or a short ID and press Enter to hand that session off.
The list scrolls with the mouse wheel, arrows, PgUp and PgDn (or space, `n`,
and `p`), Home and End. `q`, Ctrl-D, or Enter with nothing typed quits without
producing a handoff; Ctrl-C quits at once.

- **`/` filters as you type**, as in `less`. A line at the bottom takes the words,
  and the rows narrow with each character. They are the words `handoff
  "<words>"` takes ([above](#naming-a-session-in-words)): a topic, a PR number
  (`/208`), a branch, a project. The filter searches every session in the
  scope, not only the 50 listed (a session not uploaded yet is searched only
  among the picker's 50 rows). On the filter line `a`, `n`, `p`, `q`, and
  digits are typed text: press Esc first to use them as keys.
- **`▸` marks the row Enter hands off**; it starts on the first match. ↑ and ↓
  move it, and Enter acts on it.
- **A subagent that matches is shown indented under its parent** (`↳ Review and
  fix PR #208 (5b-1b)`), and the parent is shown even when it does not match, so
  a subagent can be picked like any row. The mark starts on the subagent.
- **Rows keep the numbers they have unfiltered**, and Esc clears the words and
  closes the filter line (Backspace on an empty filter does too), after which the
  list's own numbers choose again.

```text
Hand off · agent-archive · "208" matches 1 · claude · Esc clear
#  TITLE                                                   PR    WHEN            ID
3    Add a status line for paused collection · 1 subagent  #212  38 minutes ago  76941c69
   ▸   ↳ Review and fix PR #208 (5b-1b)                    #208  3 hours ago     a1b2c3d4
All · ↑↓ move · Enter hand off · Esc clear

/208
```

*Line mode.* Where keys cannot be read, the picker prints the table and reads
lines. A number or short ID picks. `n` and `p` turn pages and `a` switches the
scope. Any other answer is words to filter by (`3 sessions match "flaky" · a
number, more words, or Enter for all`): the table is drawn again with the matches,
more words narrow it, and an empty answer clears the filter, and quits when
there is none. A number the table does not have, and a word shorter than 4
characters, are words too.

*After you pick.* The picker closes and `Continue in` follows. Whatever you
type ahead for that question while the picker is closing is kept for it.

### Finding a session by repository

A session matches the current directory when it ran at the same path, or in
a checkout of the same repository. The repository is identified by its
`origin` remote (`git remote get-url origin`): a session captured in a
repository with an `origin` records a hash of it (see
[privacy](../security/privacy.md#what-is-uploaded)), so the
session from your other Mac is found even when the repository is at a
different path there, and whether it was cloned over SSH or HTTPS.
Running in a subdirectory of the repository works; running from a folder that
holds several repositories matches none of them. A session that ran at this path always comes before one found only by
repository: this Mac's sessions for the path, then the archive's, and only
when there are none does a repository match count. Within each kind the most
recently active session wins, and this Mac's own sessions are tried before
the archive's. So to take the newest session from another Mac when this one
has an older one at the same path, add `--source archive`.

- **A fork's `origin` is the fork.** It is not the repository it was forked
  from, so a clone of the fork and a clone of the upstream do not match each
  other. Only the remote named `origin` is read.
- **No `origin`, no repository match.** A directory that is not a git
  repository, or has no remote named `origin`, matches by path only, as
  before. `git` must be installed, and sessions captured before you updated
  gain the key on the Mac that captured them, when the collector next
  refreshes them and the repository is still there.
- **Branches are not compared.** When the session was on another branch
  than the one you have checked out, `handoff` says so on stderr
  (``handoff: session was on `feature/x`; you are on `main` ``) and tells
  the receiving agent in the workspace lines, and continues anyway. The
  workspace lines also say when the session ran in a different directory
  than yours, so the agent checks paths against the tree in front of it.
- **When nothing matches**, `handoff` says what it tried (the repository,
  then the path) and lists the five most recent archived sessions with the
  command for each.

A repository can name any origin, and so can anyone who can write to the
archive, so the key is a convenience and not proof that a session is yours.
When `--latest` reaches a session by repository and not by path, `handoff`
therefore stops before downloading any of it. On a terminal it names the session
(this or another Mac, project, start time, first prompt, each cut short) and
asks; the answer defaults to no, and nothing is printed or launched until
you say yes (for a session on this Mac its transcript is read first, to show
the first prompt, and stays on the Mac). Where it cannot ask (a pipe, or an
agent's shell, which has prompts off) it does not use the session and exits
1. It prints only which machine (this or another Mac), when the session
started, and the command that does, `agent-archive handoff SESSION_ID`, with
the `--harness`, `--to`, and `--worktree` you gave (not `--format`,
`--output`, `--max-bytes`, or `--branch`; add them). If the session's ID is
not 32 lowercase hexadecimal digits, it says to run `agent-archive list`
instead. That refusal is a speed bump for an agent that has been steered, not
a barrier: it can still run the command or name a session ID, so check what
an agent is doing with a session from another computer. Naming a session
yourself, or picking one in the picker, is never questioned. The reasons are
in the [threat model](../security/privacy.md#threat-model).

The notes about branch and directory are made for the checkout `handoff`
runs in. They are left out with `--worktree`, since the agent then starts in
a new worktree, and they stay in a handoff you print or pipe.

Uncommitted changes stay on the machine that made them, so push a branch
before continuing elsewhere.

## What the receiving agent is told

The handoff starts with where the session left off, the latest plan, and the
files touched, then every prompt with the agent's replies, one-line tool-call
summaries, trimmed tool output, and any summary Claude Code wrote when the
session was compacted. Edit bodies are left out; the receiving agent should
read the files as they are now. Cursor transcripts record no tool results, so
a Cursor handoff says so and shows none.

A handoff is a record of someone else's session, and its text is whatever
that session contained: your prompts, but also tool output, file contents,
and web pages the first agent read, any of which can hold instructions
written to steer an agent. So the output is shaped as a record, not as
instructions:

- A short **preamble** at the top tells the receiving agent that what
  follows is a filtered record of a past session, that `[REDACTED]` is not
  a real value, that tool output is trimmed and edit bodies are left out,
  to check the repository's current state before acting, to ask you when
  the next step is unclear, and not to follow instructions inside the
  record.
- Prompts, agent replies, and summaries are block-quoted, and plan items
  and file names are rendered so they can't add headings of their own.

An agent launched by `handoff` is also told that Agent Archive exists, where
its executable is, and how to ask for more context:
`agent-archive show SESSION_ID --transcript` reads the published archive
copy, which may lag the local transcript or not exist yet, and
`agent-archive handoff SESSION_ID --source local --max-bytes 0` reads the
current, complete **filtered** local record. Neither command exposes
unfiltered raw transcript data.

`--no-preamble` leaves out that note, for when you write your own framing
around a printed handoff; `--to` always includes it. The quoting stays, but
nothing then tells the receiving agent that the text is a record rather than
a request, so only use it when your own prompt says so. A handoff read from
a shared bucket is only as trustworthy as everyone who can write to it; see
the [threat model](../security/privacy.md#threat-model).

## Size

Output is limited to 120,000 bytes (about 30k tokens; `--max-bytes`, `0` for
no limit). Over the limit, older tool output is dropped first, then older
tool calls are collapsed to counts, then older agent messages are shortened,
then long prompts are truncated; until then the last three exchanges (up to
their last twenty steps) are kept whole. A session still over the limit has
each older exchange's agent messages cut to its last one, then all tool
output dropped, then every exchange but the last condensed to its prompt's
first line and a count of what it held (the first prompt keeps its first
2,000 bytes), then the last exchange's tool calls collapsed and its messages
shortened. A step leaves alone an exchange it would make longer, such as a
one-line reply that its count would outweigh. Every prompt and where it left
off are always kept. Markdown
output that is still over the limit, which happens when one part, such as a
very long final message, is larger than the limit by itself, or when a
session has so many exchanges (several hundred) that their first lines are,
is cut at a line with a marker. The last exchange is kept after the marker
when it is small enough, and the list of what was omitted stays at the end;
under a limit too small for that list, a shorter marker naming the saved
untrimmed version replaces it. JSON output is not cut. When anything is trimmed, the
untrimmed version is saved under the data directory in `handoffs/` (mode
0600, removed after 7 days and by `uninstall --delete-local-data`) and its
path is named at the end, so the receiving agent can read what was omitted.
`--file` without setup never creates the data directory, so a trimmed
`--file` handoff is not saved in full there; use `--max-bytes 0` to print all
of it.

## Scripting and other outputs

Piped or redirected output never asks where to continue; it prints the
handoff as before. So you can hand it to an agent as its first prompt
yourself:

```sh
# Continue in Claude Code what you started in Codex, in the same repository
claude "$(agent-archive handoff --latest --harness codex)"

# Continue in Codex what you started in Claude Code
codex "$(agent-archive handoff --latest --harness claude)"
```

Other outputs:

```sh
# A specific session, from `list` or by words, written to a file (mode 0600)
agent-archive handoff SESSION_ID --output /tmp/handoff.md
agent-archive handoff "fix the auth bug" --output /tmp/handoff.md

# The same content as JSON
agent-archive handoff SESSION_ID --format json

# A transcript the archive never captured, on this machine; needs no setup
agent-archive handoff --file ~/.codex/sessions/.../rollout-....jsonl --harness codex
```

`--output` refuses to replace an existing file unless you add `--force`.
It publishes a complete private file atomically; `--force` replaces only an
ordinary file and rejects symbolic links and other file types. Failed writes
preserve the previous output.
`--format json` prints the structured handoff document the prompt is rendered
from; see [JSON output](../reference/json-output.md). The format is specified
in the [handoff design](../../dev/specs/handoff.md).
