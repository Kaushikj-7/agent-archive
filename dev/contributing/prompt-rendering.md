# Guided prompt rendering

The shared renderer in `internal/cli/prompt_render.go` is an opt-in contract.
Setup's storage provider decision and R2 credential fields use it. Other flows
keep their existing output until their migration in the
[accepted CLI experience plan](../proposals/implemented/setup-and-cli-ux.md).

Build a `promptModel` and call `p.guidedChoice` or `p.guidedText`. Keep business
state, discovery, validation and consent in the command. The model holds the
question, helper lines, numbered primary `option` values, bracketed secondary
`actionOption` values, default semantic key and answer label. Numeric answers,
named keys, unambiguous key prefixes, secondary shortcuts and explicit aliases
resolve to semantic keys. Aliases belong only to choice fields, never text or
secret fields. A default is offered only when its key is an available choice.

`Numbers` optionally gives primary choices stable indices across display pages;
`Default` remains the semantic key. The caller owns the complete candidate set,
paging and selection. `ResolveReceipt` converts a resolved key or ordinary text
value to human text. `Receipt` is a label prefix for choices and ordinary text;
Enter on a text field resolves its default before its receipt. `Validate` runs
on the resolved semantic choice key or resolved text before completion and
retains the same field on errors. Validators must not
perform a transaction or imply consent to a larger flow.

Set `Secret` for credentials, clipboard-derived secrets and pairing input.
The renderer hides terminal echo before showing the cursor and prints only
`Credential received` or `Credential skipped`; it does not render a secret
value, default, length or caller-provided receipt. Secret validation errors use
a fixed message. Keep secret values out of questions/helpers and command logs.
The rendering state retains only row counts, cursor width, dimensions, output
generation and an ownership epoch. It never retains entered text.

`ReadAnswer` optionally accepts the **existing** `*bufio.Reader` and returns
one raw answer, including its newline and EOF, with its existing size limit.
For a pairing receiver, use a callback that calls
`boundedPairingAnswer(reader, limit)`, preserving its bound. The trimmed
`boundedPairingLine` adapter is for legacy consumers outside guided input. Do not open
another reader, read the file descriptor directly or turn a bounded payload into
an unbounded `ReadString`. The same buffer supplies legacy line prompts and
browser hand-back. Empty or whitespace-only EOF is an error and never chooses a default. A final
nonempty answer without a newline is accepted, with a conservative static
receipt on its own line.

`promptCapabilities` separates color, input/output terminals, redraw, ASCII,
width and height. Tests can implement `promptOutput` on a writer. `NO_COLOR`
disables color/emphasis while preserving supported live interaction. Dumb
terminals use `>` and `OK` in static output. Redirected or distinct terminals
get grouped static output without cursor controls or synthesized answer echo.
Synthetic capabilities must accurately model echo and stream ownership.

Only an owned active region can collapse. The renderer counts wrapped helpers,
choices and nonsecret echo using `visibleWidth`/`displayLines`. It appends a
receipt instead when dimensions change, the block/answer cannot fit the visible
viewport, typed-ahead input may already have echoed, a resume/resize arrives,
or the region's output generation/ownership changes. Control-character echo uses
static completion because canonical terminals may expand controls to caret
notation or move the cursor. Receipts discard control characters. Emoji
variation selectors, joined or modified emoji, keycaps, Hangul jamo and
spacing marks also use static completion because terminal shaping can make
their clusters occupy fewer cells than their individual runes. Control
characters in the rendered model likewise invalidate region ownership. Receipts stay in
ordinary scrollback. Setup never opens an alternate screen for prompts.

On a real input terminal, ordinary block emission temporarily suppresses only
echo, retaining canonical editing, EOF and signal processing. Echo is restored
before checking for a complete answer entered during the write; such an answer
gets a static receipt on its own line. The renderer preserves the original
ECHO and ECHONL settings. It counts ordinary answer wraps only when character
echo is enabled, and supplies the receipt newline when newline echo is disabled.
Canonical echo-off prompts still collapse when region ownership is proven;
static and final EOF receipts begin on their own row. Partial canonical input
cannot be probed without consuming it, so emission starts at column zero and completion assumes
its echoed extent is unknown. Short answers and defaults can still collapse;
an ordinary answer that could wrap from the cursor retains the question and
appends its receipt. The same buffer retains complete and partial typed-ahead
answers. An incomplete output write or a suspend during emission also prevents
erasure. Deferred restoration covers output errors and panics, and the terminal
guard restores echo before signals or suspension.

Use `release := p.suspendPrompts()` **before** a spinner, credential helper,
subprocess, browser, pairing alternate screen or pager takes the terminal;
call `release()` afterward. This invalidates the old region permanently and
allows a fresh block. If a key browser or its pager already handles the parent
process's job-control signals, call `p.suspendPrompts(true)` to delegate that
authority too; this prevents two guards from stopping the process on Ctrl-Z.
The default keeps canonical job control for spinners and credential helpers.
Writes through `p.out` are generation-tracked; writes to
an original stdout/stderr or subprocess-owned descriptor bypass that wrapper
and require explicit suspension. The renderer does not claim to detect such
writes automatically. Keep suspension on the same command goroutine.

The renderer's `block` owns blank lines for migrated headings and logical
blocks: use `p.renderer().block(text)` with a newline-terminated body rather
than embedding leading blank lines. Answer completion leaves one blank line.
Guided command callers use `guidedChoice`, `guidedText`, and their setup or
command adapters. Obsolete confirmation and menu helpers have been removed.
Low-level `line`, `promptText`, `secret`, and `labelText` retain their existing
browser and special-input contracts. The `guidedSpacing` field preserves
spacing for low-level input used during setup; it is not a reason to change
every browser `line` call.

Always `defer p.close()` for a command using guided hidden input. Its terminal
guard restores modes on completion, errors, EOF, SIGINT/SIGTERM/SIGHUP/SIGQUIT,
and before Ctrl-Z. On resume it reapplies hidden modes before reading continues.
Interrupt notification is limited to hidden reads, so storage checks and setup
transactions keep their existing interruption/rollback authority. The job-control
guard stays alive for the prompter's lifecycle so later canonical prompts
still handle Ctrl-Z after Go installs its signal handler. Existing shell exit
statuses remain 128 plus the signal number.

Tests live in `prompt_render_test.go` and `prompt_render_pty_test.go`. The latter
runs synthetic setup fields under a disposable PTY and interprets visible cells,
including resize, scroll, retries, secret modes, typed-ahead input and ownership
handoffs. Keep these cases in `scripts/test_macos_smoke.sh` when adding names.
