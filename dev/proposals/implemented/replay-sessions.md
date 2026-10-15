# Marking replay sessions

> **Status: implemented** (`internal/archive/replay.go`, `internal/capture`,
> `internal/reader`, `internal/cli`). This is the design record; the
> [metadata schema](../../../schemas/metadata.schema.json),
> [JSON output](../../../docs/reference/json-output.md), and
> [privacy](../../../docs/security/privacy.md#what-is-uploaded) are the contract.
> Written 2026-09-30 against `main` at `dc443c7`.

## Purpose

A separate tool (an eval runner built on Harbor) will replay a person's
archived tasks with other agents and models to build a personal benchmark. If
agent-archive's hooks are installed where those runs happen, the runs are
captured like any other session. That is useful: their tokens, tool calls, and
transcripts come for free, in the same shape as the originals. But they are
not the person's own work, and must not appear as such in `list`, `stats`, or
`handoff --latest`.

## Decisions

- **The marker is an environment variable.** The runner sets
  `AGENT_ARCHIVE_REPLAY=<run id>` in the environment of each agent it starts.
  The apps pass their environment to the hooks they run, so `agent-archive
  _hook` reads it (through `Env.LookupEnv`, so tests never see the real
  environment) and passes it to `capture` (`capture.WithReplay`). A variable
  needs nothing from the app: no flag, no payload field, no configuration
  file, and it reaches every hook the agent's process runs. The alternatives
  were worse: a marker file in the project would be visible to the agent and
  could be committed, and a registration made by the runner would race the
  hook.
- **It is set once, at registration.** Only the hook that registers a new
  session records it. A continuation neither adds nor removes it, so a replay
  runner resuming one of the person's own sessions cannot relabel it, and a
  person resuming a replay does not make it theirs. A start that found
  `hooks.lock` busy carries the marker in its queued admission intent, so the
  session the collector admits later is still a replay. A subagent carries its
  parent's marker.
- **Only an identifier is recorded.** The value is recorded as `run_id` only
  when it is 1 to 128 ASCII letters, digits, `.`, `_`, `:`, or `-`, starting
  with a letter or digit. Any other non-empty value still marks the session,
  without recording the value, so a runner cannot put free text into metadata
  by mistake. An empty value, or one that is only spaces, means unset.
- **A separate `replay` field, not a new `origin`.** `origin` answers "how did
  this session enter the archive" (a hook as it ran, or `backfill`), and
  existing readers already test `origin == "import"` and treat anything else
  as hook-captured (`list --hook-captured`, `stats --imported`). A replay is
  hook-captured; what differs is who drove it. As `origin: "replay"` it would
  drop out of `--hook-captured` and silently land on the "hook" side of every
  reader that checks only for `import`. The two are orthogonal: a runner
  that imports its runs with `backfill` instead would want both. So the
  sidecar gets an optional object, `replay: {"run_id": "…"}`, present only
  for a replay. An object leaves room for more fields (the original session a
  run replays, say) without another schema change.
- **Hidden by default where a person reads their history.** `list` (and its
  browser), `stats`, the bare `show` and `handoff` pickers, `show`'s and
  `handoff`'s title search, and `handoff --latest` (local registrations and the archive) leave
  replays out. `list` and `stats` take `--replays hide|include|only`, applied
  in `reader.Filter` so the indexed `list --limit N` still returns N matches.
  `reader.Filter`'s zero value includes replays, so a caller that does not ask
  (`show ID`, retention, the collector) sees every session. `show ID` and
  `handoff ID` open a replay by its ID as usual. `list --json` consumers that
  pass `--replays include` tell them apart by the `replay` key; the table
  marks them `[replay]` in the title, and `--verbose` keeps their real origin
  (`hook` or `imported`) in its ORIGIN column. `stats` names a
  `--replays include|only` filter on its screen and its `--html` page, as it
  names the others.
- **Retention treats them like any other session.** A replay is removed with
  the rest when it expires. A runner that wants its runs kept longer, or
  shorter, has to arrange that itself; a per-session retention is out of
  scope.
- **Status counts them.** A replay is a real capture by the app's hooks, so it
  counts in `status`'s `sessions` and verifies the app's hooks.
  `status --json` adds `replay_sessions` per app (absent when zero, so the
  pinned JSON of every existing status screen is unchanged), and the text
  says "3 sessions, 2 of them replays".
- **No parser bump.** Like `git_head`, the marker comes from the registration,
  and no existing session can gain one. The field is optional, so
  `MetadataSchemaVersion` stays 1.

## What the runner does

1. Install and set up agent-archive where the agents run (a container, say),
   with its own `AGENT_ARCHIVE_HOME`, and include the replay's checkout as a
   project. Capture follows the usual rules: only sessions that start fresh in
   an included, activated project are admitted
   ([session eligibility](../../../docs/reference/session-eligibility.md)).
2. Start each agent with `AGENT_ARCHIVE_REPLAY=<run id>` in its environment.
3. Read the runs back with `list --json --replays only`, and select a run's
   sessions by `replay.run_id`.

Whether a given app passes its environment to its hooks is recorded in
[capture capabilities](../../../docs/reference/capture-capabilities.md): Claude
Code and Codex run hooks as child processes of the agent. Cursor's desktop app
runs them from the app, whose environment is the one it was launched with, so a
runner driving Cursor must launch the app (or the Cursor agent CLI) with the
variable set.

## Privacy

The marker adds one opaque identifier the runner chose, or nothing, to the
metadata. It is listed in [privacy](../../../docs/security/privacy.md#what-is-uploaded).
Nothing else from the environment is read.

## Tests

- `internal/archive/replay_test.go` and `schema_test.go`: what is an
  identifier, and what reaches the sidecar.
- `internal/capture/hook_replay_test.go`: the marker at registration, never
  changed by a continuation either way, and kept through a queued admission.
- `internal/collector/replay_test.go`: the sidecar, and subagents.
- `internal/reader/matches_test.go` (`TestMatchesReplays`): the filter and its
  zero value.
- `internal/cli/replay_test.go`: `list` (with and without the indexed limit),
  the table's mark, `stats` and the filter it names, `show`'s title search, the hook command reading the variable,
  `handoff --latest` from the archive and from local registrations, and
  status's wording.
