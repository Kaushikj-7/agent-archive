# Recover a blocked transcript

The preview groups the blocked transcript and recovery consequences before the command that confirms them. Read the complete consequences, including the permanent requirement for a generation-aware writer, before running with `--confirm`.


A `transcript_rewritten` capture gap means the current native transcript no
longer proves that it includes everything already archived. This can happen
when an app compacts its transcript, or when a filter upgrade changes how an
older record looks. The collector keeps the richer retained history. If it
can later prove extension, capture resumes automatically.

When the gap persists, preview an explicit recovery using the full archive
session ID shown by `list` or `status --json`:

```sh
agent-archive recover SESSION_ID
```

The preview reads and filters the current local transcript. It explains the
history being kept and the gap between the old and current records. It writes
nothing and accesses no storage or credentials. After reviewing it, confirm:

```sh
agent-archive recover SESSION_ID --confirm
agent-archive sync
```

Confirmation preserves the original archive ID, source history, feedback and
existing handoffs. It freezes further native capture into that generation and
queues the current filtered transcript under a new archive ID. Future hooks
route to that new generation. The metadata and source header record
`previous_generation_id`; this is separate from `parent_session_id`, which
continues to mean a subagent. Both generations remain ordinary visible sessions.
The new generation records a `recovered_generation` gap: absent records are
still absent from its transcript, and earlier retained history belongs to the
previous ID. Counts and analytics include both generations, so overlapping
records may be counted twice.

The command supports active, locally registered, top-level append-only
transcript files with published history and a rewrite gap. It refuses a
pending predecessor publication; settle that work with `sync` first. It also
refuses an excluded session, a changed storage destination, or starting or
resuming a recovery while collection is paused. Restore the appropriate capture permission or
destination before previewing again.
If the current file identifies a different native session, recovery refuses it;
restore the registered session's transcript or begin a fresh native session.
On an archive with publication composition protection, recovery also refuses a
session with retained history alternatives; that session keeps its current
publication. If the skill evidence, destination or capture settings change
while a confirmed recovery is interrupted, it stays pending until you restore
them and run `agent-archive sync`.

Direct subagent recovery is not supported in this first version. Start a
fresh parent session to capture new subagent activity. Existing subagents and
already recorded subagent candidates retain their identities and original
parent; newly admitted children after recovery use the active parent generation.
Replaceable Cursor database chats use their existing snapshot behavior and do
not need this append-only recovery path.

Repeating recovery with the original ID returns the same successor, including
after its content has expired. An interrupted confirmation resumes the same
journaled transition with the same capture time and ID. To recover again,
select the active successor after it develops its own rewrite gap. Recovery
never reconstructs missing original history or replaces it with a smaller
transcript.

Each generation expires independently under normal retention. Privacy and
parser maintenance may refresh a frozen generation from its retained evidence,
with its original capture time. Feedback submitted to an earlier ID stays with
that generation and does not renew its retention age. Linking does not extend that lifetime. Import
recovery preserves the original import batch; undoing that batch removes all
its remaining generations. A genuine new native start after the active
generation expires follows the normal admission rules and begins a separate
root; a continuation cannot revive an expired generation.

Recovery permanently upgrades the local configuration's writer protection.
Older binaries that cannot preserve frozen generations refuse this data
directory, even after generations expire. Keep using a generation-aware
release; setup changes, rollback and uninstall-preserved configuration retain
this protection. This operation does not change the privacy filter rules.
