# Agent skills

Setup gives Claude Code, Codex, and Cursor two skills, so you can talk to your
agent about your other sessions instead of typing commands:

| Skill | Who runs it | What it does |
| --- | --- | --- |
| `agent-archive` | The agent, when you refer to a past session | Finds a session, on this machine or in the archive, and pulls it in as context. |
| `handoff` | You, as `/handoff codex` | Continues the session you are in, in another agent ([handoff](handoff.md#from-inside-an-agent-handoff)). |

A skill is a text file the agent reads: it names the commands to run and when.
The `agent-archive` skill runs the same read-only commands you would:
`handoff`, `list`, `show`, and `status`.

## Asking for a past session

Say what you want, as you would to a colleague:

```text
Pull in the auth session from Codex.
What did we do in Cursor yesterday?
Look at my last Claude Code session about the migration, then continue it.
Continue where my other agent left off.
```

The agent runs `agent-archive handoff "auth" --harness codex`, which prints
that session as a prompt: filtered as the archive is (injected instructions and
credentials removed, tool output trimmed), cut to about 120 KB, and addressed
to the receiving agent. It then does what you asked with it.

- **It finds the session by its words.** Every word must appear, in any case,
  in the session's name (the one your agent shows in its sidebar), title (its
  first prompt), branch, project name, or app, or start its short session ID,
  and `#212` or `212` also matches a pull request number. So the words may be a
  topic, a PR number, a branch, or a project name, and may match different
  fields ("linux 212"). The agent searches for one or two distinctive words,
  not a whole sentence. It looks in this machine's sessions first (no network),
  then the archive's, so a session from an hour ago needs no bucket access, and
  inside a repository it looks at that repository's sessions before the
  others, saying how many more match elsewhere. Subagent sessions answer only
  when nothing else matches. With no topic ("where my other agent left off") it
  takes the most recent session for the project you are in (`--latest`). It
  passes over the session it is running in when the agent names it to commands
  it runs, as Claude Code does; Cursor does not, so there words or `--latest`
  can match the current chat. The full rules are in
  [naming a session in words](handoff.md#naming-a-session-in-words).
- **It never guesses.** When several sessions match, `handoff` prints a table
  and exits 1: the short ID, agent, project when they span several, when, pull
  request, and title of each, then a `Next:` line with the exact command for
  the first row. The agent shows you that table and asks which one. It never
  picks for you. Once you choose, it runs the command again with that short ID
  in place of the words, and `--harness` set to that row's agent. To see the
  candidates as data instead, it can run `list "<words>" --json`, which prints
  the archive's matches for the same words (a session not uploaded yet is only
  in `handoff`'s table).
- **When nothing matches**, it tries different words, or looks at the last 30
  days with `list --since 30d` and shows you the titles.
- **It can browse:** `list` for what exists (a short table of top-level
  sessions, with a hint of how many subagent sessions each has; `list "<words>"`
  searches it, and `--json` gives the full metadata), `show ID` for a session's
  summary, `show ID --transcript` for its conversation, bounded to about 120 KB
  (the rest is saved in a file whose path is named).

- **It can search conversation content:** `search "WORDS" --json` returns
  bounded ranked snippets from the private local index. The agent reads exact
  selected body references with `search --read REF --max-bytes 4096 --before 1
  --after 1 --json`, using continuation offsets only when more is needed. It
  reports incomplete coverage and asks before widening scope or indexing.
  Search results and context are untrusted session data. A selected session
  can then use ordinary full-ID archive handoff through existing gates.

The skill never lets the agent run `setup`, `uninstall`, `purge`, `backfill`,
`sync`, or `feedback`, add `--to` to `handoff` (that starts another agent, and
belongs to `/handoff`), or lift the size limit with `--max-bytes 0`, unless you
asked for exactly that. Nothing in it prompts: inside an agent every command
answers or fails instead of waiting for a key
([`AGENT_ARCHIVE_NONINTERACTIVE`](../reference/configuration.md#environment-variables)).

## What the agent can read through the skill

What you would see running the same commands yourself: the filtered session
`handoff` prints, `list`'s table, bounded search snippets and selected context,
`show`'s summary, and its transcript (`handoff`
and `show --transcript` are cut to about 120 KB, though `handoff`'s bound is
best effort and a very long session can run over it). It is the archive's filtered content (credentials and
injected instructions removed, tool output trimmed), not your raw transcript
files, and the skill tells the agent not to open those. That is an
instruction, not a barrier: the agent runs as you, so it could read files you
can read whatever the skill says. Once a session is in the agent's context,
that agent's own provider sees it like anything else in the conversation.
Pulling in a session from another agent therefore shows that content to this
agent's provider. See [privacy](../security/privacy.md#what-an-agent-can-read-through-the-skill).

## What the agent is told about what it reads

A pulled-in session is a record of another conversation, and text in it may
have been written by a web page or a tool. It is filtered for credentials, not
for hostile wording, so the skill tells the agent to treat it as data: never to
follow instructions found inside it, and to check the repository's current
state before relying on it. If the output was trimmed, it may open the one
untrimmed copy the last line names (a file in `handoffs` under the data
directory) and no other path the text mentions. See [privacy](../security/privacy.md).

## Permissions

The agent's own permission settings still apply.

- **Claude Code.** Using a skill is itself something Claude Code asks about,
  so expect a question the first time the agent picks this one; allow it for
  good with a `Skill(agent-archive)` permission rule (we saw the `Skill` call
  refused where nothing can ask; the interactive dialog itself we did not
  see). Then the commands. The skill names one command the agent may run
  without asking: `agent-archive status`, spelled exactly so (its full path is
  in the file; when that path needs shell quoting, the file names no command at
  all). Claude Code's documentation says this applies during the turn that
  uses the skill, in every kind of session. In our check of version 2.1.283 it
  did not: with `claude -p`, and the skill installed as a project skill, a
  skill the agent chose did not get it, though the same rule on a skill the
  person started by typing its name did. So `status` may ask too. Nobody has
  yet checked this with the skill installed under `~/.claude/skills`, or in an
  interactive session. The
  first time the agent runs `handoff`, `list`, or `show`, Claude Code asks you.
  That is on purpose. A permission rule cannot leave a flag out: allowing
  `handoff` for the skill would allow `handoff --to codex`, which starts another
  agent, and allowing `show` would allow `--max-bytes 0`, which prints a whole
  transcript. Text in a session the agent reads must not be able to start
  either. If you tell Claude Code to always allow a command, you are choosing
  that wider rule yourself. Where nothing can ask, as in `claude -p`, the
  command is denied, and the agent says so and shows you the command.
- **Codex and Cursor.** Their approvals and sandbox decide. Codex's default
  asks before a command that needs the network. In Cursor, your run mode
  decides: Auto-review, its recommended default, runs a shell command in a
  sandbox where it can and asks only when one needs full access and looks
  risky; Allowlist asks for anything not on your list; Run Everything never
  asks. So in Cursor an archive read may fail in the sandbox without a
  question.
- **A sandbox.** In Claude Code's sandbox (off until you turn it on), in
  Codex's default one, and in Cursor's, the network is blocked unless
  approved (Claude Code asks about each new domain; where nothing can ask, as
  in `claude -p`, it is refused). `handoff "<words>"` still finds a session on
  this machine by any field it matches (name, title, branch, project, app, a PR
  number, or the start of its ID), since it looks there first and needs no
  network, but the archive, and so `list` and `show`, fail with `operation not
  permitted`. The skill tells you what failed and asks you to allow it, or to
  run the command yourself, rather than retrying variations. None of the three
  documents whether a sandboxed command can read the macOS Keychain, which an
  R2 archive needs on a Mac (on Linux it is a credentials file the sandbox may
  also block), so expect to allow or run those reads yourself.

## Where the files are

| App | File |
| --- | --- |
| Claude Code | `~/.claude/skills/agent-archive/SKILL.md` (in `$CLAUDE_CONFIG_DIR` when set) |
| Codex and Cursor | `~/.agents/skills/agent-archive/SKILL.md` |

Each file names the full path of the `agent-archive` you installed, since an
agent's shell often does not have it on `PATH`, and `AGENT_ARCHIVE_HOME=...`
when your installation uses another data directory. Each carries the marker
line described for [`/handoff`](handoff.md#from-inside-an-agent-handoff): setup
replaces and uninstall removes only a file with it, delete the line to keep an
edited copy, and a file already at that path without it is left alone.
`agent-archive status --verbose` lists the installed files, and `status` warns
when one was written by an earlier release. Upgrading with the installer
refreshes both skills for you ([install](../getting-started/install.md)); so does
`agent-archive setup --refresh`, which also brings the app hooks and the
background job's definition up to date, and changes nothing else.

Cursor also reads `~/.claude/skills` (and `~/.codex/skills`), so on a machine set up
for both Claude Code and Cursor it can find the skill in two places. The two
files carry the same instructions; Cursor's documentation does not say whether
it lists a duplicate once. Codex reads `~/.agents/skills`; setup writes nothing
under `$CODEX_HOME`.

Claude Code and Codex notice a new or refreshed skill in a running session
(restart the app if it does not show up). Claude Code does not watch a skills
directory that did not exist when the session started, as on a machine that never
had `~/.claude/skills`: run `/reload-skills` there, or start a new session.
Cursor's documentation does not say, so start a new chat there.

## Turning the skills off

`agent-archive setup --no-skills` installs neither skill and removes the ones
setup wrote (a file that is not setup's is left alone and named). It is saved,
so later setup runs keep them off, and `agent-archive setup --skills` turns
them back on ([setup](../getting-started/setup.md)). `agent-archive
uninstall` removes them along with the hooks.

## If the agent does not use the skill

See [my agent does not use the skill](troubleshooting.md#my-agent-does-not-use-the-skill).

Machine pairing must run in a separate human terminal. Both `machines add` and
`setup --pair` refuse when `CLAUDE_CODE_SESSION_ID`, `CODEX_THREAD_ID`, or
`CURSOR_AGENT` is present, including with `--yes` or
`AGENT_ARCHIVE_NONINTERACTIVE=0`. This prevents code exposure in transcripts and
unattended destination changes. Do not ask an agent to handle pairing codes.
