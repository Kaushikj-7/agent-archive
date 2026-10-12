# Session start eligibility

A session is eligible only after its project has been included.

A new `SessionStart` must prove that it begins a conversation rather than resuming one that may predate activation. There are two proofs.

The first is the documented source. Codex and Claude Code provide `source: startup` or `source: clear` for a conversation that begins now; `resume` and `compact` do not establish the start of a previously unseen session, and neither does any other value. When a Codex or Claude payload carries a `source`, it decides, and the second proof cannot override it.

The second is the transcript itself: at the true start of a conversation the hook-provided `transcript_path` names a file that does not exist yet or holds no bytes, while a resumed conversation points at a transcript that already has content. It is the fallback when a Codex or Claude payload carries no `source` at all; for those, a start with no `transcript_path` proves nothing and stays declined, as does one whose path is not absolute or whose transcript cannot be stat'd. A version string is never proof of anything. The transcript is stat'd, never opened, and its path is never recorded in a diagnostic.

### Cursor

These rules rest on what the Cursor desktop app 3.21.13 was observed to do, recorded in [capture capabilities](capture-capabilities.md#cursor-32113-observations): a new Agent chat fires no `sessionStart`, its first `beforeSubmitPrompt` has a null `transcript_path`, later hooks name the transcript, and a resumed chat's first prompt names its existing, non-empty transcript.

So a never-seen Cursor conversation is registered at its first `beforeSubmitPrompt` (and still at `sessionStart`, should a version fire one) when `transcript_path` is null, absent, or names an absolute path to a missing or empty file. A transcript that already has bytes is a resume and is declined with the `session_start_unknown` diagnostic, for included projects only. `session_started_at` is the time of that first hook. A registration made without a path takes it from a later `beforeSubmitPrompt`, `afterAgentResponse`, or `stop`, but only a path that is absolute and whose file name is `<conversation_id>.jsonl`; a path, once set, is never replaced. Until the path arrives the collector treats the session as waiting, not failed. The same holds once the path names a file that exists but still holds no bytes: nothing has been captured, so the pass records no error, keeps any queued request, and reads the file again next time.

What was not observed on 3.21.13 (`sessionStart`, the subagent events, a version whose `session_id` differs from `conversation_id`) is listed with the observations, and each case fails closed.

A resumed chat is never mistaken for a new one, because its first prompt names its existing transcript. If transcripts are disabled, every hook's path is null: the chat registers but never receives a path, so nothing is read or uploaded. Its registration and queued hook evidence stay local until the session is older than the retention period, counted from its admission; retention then forgets it, queued evidence included, without any bucket call, since it never published. Codex and Claude Code are unchanged: a prompt from a session that never started registers nothing.

Project identity is matched by configured project, not by exact directory, for new registrations and continuations alike. A harness reports the session's working directory, which for a Claude Code worktree is `<project>/.claude/worktrees/<name>` and for a session started anywhere else in the tree is that subdirectory. The configured project that owns it — the nearest configured ancestor of the reported directory, comparing resolved paths — is the project the session registers under, and the registration stores that configured root spelling. Eligibility, the activation boundary, and the project ID all use it. The nearest ancestor wins, so a configured project nested inside another keeps its own inclusion decision rather than inheriting its parent's. A working directory that belongs to no configured project registers nothing and records nothing.

Already accepted registrations retain their original start time on resume. An absent transcript path does not erase the saved path. A different harness or project cannot replace the registration identity: a resume or compact from a subdirectory of the registered project is the same session, while a directory that belongs to a different configured project is a conflict.

For included projects, skipped starts (including a declined Cursor first prompt) produce a bounded local diagnostic with application, project, reason, and time. Reasons include a project that is not yet active, an unproven start, setup in progress, and a hook that could not acquire the capture lock before its deadline (`hook_busy`). Setup holds an open transaction: hooks register nothing then, so a start that lands in that window says so instead of disappearing. Writing diagnostics does not wait for setup's lock, so it is best effort. If both the capture and diagnostics locks are busy, the hook writes an explanation to stderr; `status` may have no record of that missed event. Diagnostic records contain no transcript, native session ID, or transcript path. Excluded project paths are not recorded, and a recorded diagnostic is dropped once its project is excluded: `status` reports only currently included projects, and saving a setup that excludes a project prunes its stored entries. When a start is declined for more than one reason, the diagnostic names the most specific one: a project that is not yet active is reported as such even if the start was also a resume. `status` and `status --json` show these reasons; hooks remain local and return successfully to the agent.

When a proven first-start event times out waiting for the capture lock, the hook writes a private admission intent. Cursor also keeps a minimal response/stop follow-up when it supplies the transcript path before the first prompt has registered or when the capture lock is busy. The intent contains the app, event name, native session ID, selected project, destination ID, transcript path, and observation time, but no conversation text or raw hook payload. A follow-up cannot admit a session by itself. The next collector pass rechecks the current configuration, destination, and project activation before replay. Setup removes intents for projects or destinations it changes, and pause clears pending intents. Intents are limited to 128 files and expire after 24 hours. `hook_busy` remains visible even if replay succeeds so the user can see that capture was delayed. A full queue or a second lock timeout is reported on hook stderr. Other missed lifecycle events are not replayed; a later collector pass still scans an admitted session's transcript.

## Imported sessions

`agent-archive backfill` registers sessions the hooks never saw, after you
confirm a plan ([backfill](../guides/backfill.md)). An imported session keeps
its true start time, from the transcript (or, for a Cursor file with no
timestamps, the file's creation time), and also records its admission
(`admitted_at`): when backfill took it over. The checks that decide whether
it is captured and kept count from the admission, not the start: whether its
project was included yet, which storage destination it belongs to, and its
age under retention before its first capture. So an old session imported
today is not refused as older than its project's inclusion, nor expired at
once. The [glossary](glossary.md) defines admission, destination, and
retention.

A hook that later resumes an imported session continues it. A session that
retention or `backfill undo` removed leaves a removal record, so a later
backfill does not import it again unless you pass `--include-removed`.

How admission is implemented, check by check, and the guard tests that keep
it so, are in [session admission](../../dev/contributing/session-admission.md).

## Automatically discovered Codex tasks

Discovery considers supported regular rollouts confined to approved local Codex
homes. Original native session creation must fall within the current Codex scope,
source and destination authorization and an unpaused interval. Included-project
scope requires an included project; all-projects scope permits unlisted current
and future Codex projects subject to explicit exceptions. Other apps retain their
included-project permissions. Native children and forks qualify under their own project, creation and current
permission checks. A child can be captured while running, even before its parent
archive link resolves. Copied context below a recorded own-history boundary does
not count as its first task or activity; an absent boundary excludes nothing.
Unknown execution/history formats and incomplete dependencies stay pending with
a specific reason. Partial first-own-task metadata is retried rather than guessed.

A native task can wait before its first prompt; that does not reset its original
creation time. Moving a known session to archived history or copying the same
native identity does not create another archive. An otherwise qualifying recent
copy with indistinguishable metadata can be admitted. File presence and rebuilt
Codex database rows are source facts, not proof of originating local execution.

A child first imported by a new backfill run belongs to that new import batch,
even if its parent came from an older batch. Undo removes only that batch’s
members, with nested children removed before their selected parents. Children
keep their own source, so removing a parent does not break child readback. Undo
keeps a project included while another batch has independently imported native
children there, including children whose parent link has resolved.

## Historical-only imports

A confirmed mapping-file import can admit complete supported evidence without a
live checkout or Git. Its separate historical project proof requires an enabled
import permission for the exact destination and selected imported app. Explicit
original/canonical cwd and required-history-source exclusions still win. Home,
temporary and above-home restrictions apply before assignment. The proof grants
no hooks, discovery, watchers or future-project inclusion.

Admission freezes filtered staging. First publication can recover after native
source deletion; later capture uses retained evidence only, preserving capture
age through privacy/parser/label maintenance. Disabling permission stops new
admission and publication, while finite retention and explicit undo/purge retain
their deletion authority. Re-enable applies only to the recorded destination.
An existing ordinary archive owner is never reassigned by a historical mapping.
