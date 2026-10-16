# Muse runtime presence

The runtime integration uses the injected environment only. A nonblank
`MUSE_THREAD_ID` or `MUSE_SESSION_ID` disables interactive prompts and pagers;
the thread key takes diagnostic precedence. Neither value becomes a current
session ID or a project-latest fallback. Launching a supported destination
agent removes exactly these two Muse session variables, retaining configuration,
provider and XDG variables.

Only the Runtime operation is registered. Muse cannot be selected for setup,
capture or destination launch. No native transcript is opened, native command
is run or installed version is probed by this detector. Native integrity and
original-creation gates in the [qualification record](README.md) remain open.

## Evidence

[The official changelog](https://dev.meta.ai/docs/muse-code/changelog), version
1.4.4, documents the two session variables for shell commands, background monitors
and hooks. The current-build observation below is a fresh root shell command;
hook environment is not substituted for shell evidence.

On 2026-10-15, macOS x86_64 Muse `1.4.9-R6297.1`, native binary SHA-256
`4c6ed04ccd5a453bd7b38799aeb9eb290b8635b32c95b5a35e6950a2f20446cb`, executed
one requested native bash command in a synthetic isolated workspace. A unique
HOME/XDG configuration and file credential backend were used. The parent
authorized one real-provider root turn; normal native approval and sandbox
remained enabled. The native judge approved the read-only command. No bypass
flags or delegated agent were used. An owned isolated authentication symlink
let Muse authenticate; its contents were never read or copied and the symlink
was removed afterward. No personal settings/hooks/session context were loaded.
The native process exited zero within its deadline, followed by the owned
process-group cleanup. Independent system process inventory was unavailable.

The fixed command, expressed as a JSON string to preserve literal escaping, was:

```json
"printf 'MUSE_RUNTIME_NATIVE_SENTINEL\\nMUSE_THREAD_ID=%s\\nMUSE_SESSION_ID=%s\\n' \"${MUSE_THREAD_ID-}\" \"${MUSE_SESSION_ID-}\""
```

The one native tool result exited zero and contained the sentinel and both IDs
matching the owning root, replacing deliberately stale synthetic host values:

```text
MUSE_RUNTIME_NATIVE_SENTINEL
MUSE_THREAD_ID=01a14091-1056-7a22-b2cf-c67f3351c873
MUSE_SESSION_ID=01a14091-1056-7a22-b2cf-c67f3351c873
```

The structured sanitized local evidence document had SHA-256
`eed0b0d69ffab579594d540a20bc951225996382260071a1b6fa2fe1053caa7c`. It records the exact command, native result,
expected ID comparisons and outcome; full native logs, provider contexts and
credentials are not published. The implementation tests use synthetic markers,
not the machine's environment or a native Muse process.

Earlier isolated loopback attempts did **not** qualify runtime environment: one
used a schema field the native bash dispatcher refused; the corrected command
reached the default approval judge and timed out without shell output. They
are failed attempts, distinct from the passing real-provider root turn.

Resume, child/subagent and monitor shell environments remain unqualified. This
root observation does not authorize exact current-thread or project-latest
selection, capture, native-file integrity, billing, platform support or version
probing. Further native qualification is opt-in and must keep isolated settings,
normal approval/sandbox and explicit provider authorization.
